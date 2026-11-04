"""pybase.asr.remote 的测试：本进程起 http.server 当假的 /v1/audio/transcriptions。

音频全是程序合成的 16k 单声道 WAV（随机噪声当「说话」、低幅噪声当静音），走纯 Python 路，
**不调 ffmpeg**（autouse 夹具把 ffmpeg 入口换成直接报错）。假服务器把上传的 WAV 解出来、
在源 PCM 里找到它的位置，所以能精确断言每段/每句截的是哪一段、多少个样本。

碰 ffmpeg 的（非 WAV 输入真转码）标 `requires_ffmpeg`，本机没有 ffmpeg 自动跳过，在
Studio 测试机上跑。
"""
from __future__ import annotations

import array
import io
import json
import os
import random
import shutil
import signal
import subprocess
import sys
import threading
import time
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

from pybase.asr import AsrError, Cue, TranscriptionCancelled, refine_cues, transcribe_remote
from pybase.asr import remote

RATE = 16_000
PY_DIR = Path(__file__).resolve().parents[1]
requires_ffmpeg = pytest.mark.skipif(
    not (shutil.which("ffmpeg") and shutil.which("ffprobe")), reason="本机没有 ffmpeg/ffprobe"
)


# ---------------------------------------------------------------------------
# 合成音频
# ---------------------------------------------------------------------------


def _pcm(parts: list[tuple[str, int]], seed: int = 0) -> bytes:
    """(类型, 毫秒) 拼成 s16le PCM：voice 是满幅随机噪声（约 -5 dBFS），quiet 是 ±32 的
    随机噪声（约 -65 dBFS，算静音但每段都不一样，假服务器能唯一定位）。"""
    rng = random.Random(seed)
    out = bytearray()
    for kind, ms in parts:
        samples = array.array("h")
        samples.frombytes(rng.randbytes(ms * RATE // 1000 * 2))
        if kind == "quiet":
            samples = array.array("h", (x >> 10 for x in samples))
        elif kind != "voice":
            raise ValueError(kind)
        if sys.byteorder == "big":
            samples.byteswap()
        out += samples.tobytes()
    return bytes(out)


def _write_wav(path: Path, pcm: bytes, *, rate: int = RATE) -> Path:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(pcm)
    return path


# ---------------------------------------------------------------------------
# 假服务器
# ---------------------------------------------------------------------------


def _parse_multipart(content_type: str, body: bytes) -> tuple[dict, dict]:
    boundary = content_type.split("boundary=", 1)[1].encode()
    fields, file_part = {}, {}
    for chunk in body.split(b"--" + boundary)[1:]:
        if chunk.startswith(b"--"):
            break
        head, _, content = chunk[2:].partition(b"\r\n\r\n")
        content = content[:-2]  # 去掉段尾的 \r\n
        headers = dict(
            line.split(": ", 1) for line in head.decode("utf-8").split("\r\n") if ": " in line
        )
        disposition = headers["Content-Disposition"]
        name = disposition.split('name="', 1)[1].split('"', 1)[0]
        if 'filename="' in disposition:
            file_part = {"filename": disposition.split('filename="', 1)[1].split('"', 1)[0],
                         "content_type": headers.get("Content-Type"), "data": content}
        else:
            fields[name] = content.decode("utf-8")
    return fields, file_part


class FakeServer:
    """`script`：[(绝对起 ms, 绝对止 ms, 文本模型回的文字)]，timing_model 回落在上传音频
    里的那几句（时间换成相对上传音频开头的秒），text_model 回和小样重叠最多那句的文字。

    `hook(record)` 可以返回 None（正常回）、`("status", 码, 正文)`、`("drop",)`（不回直接
    断开）、`("sleep", 秒)`（先睡再正常回）、`("wait", Event)`（等事件再正常回）。
    """

    def __init__(self, pcm: bytes = b"", script=(), *, text_delay: float = 0.0):
        self.pcm = pcm
        self.script = list(script)
        self.text_delay = text_delay
        self.hook = None
        self.records: list[dict] = []
        self.lock = threading.Lock()
        self.inflight = 0
        self.max_inflight = 0
        server = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                server._handle(self)

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.httpd.daemon_threads = True
        self.httpd.handle_error = lambda request, address: None  # 客户端被杀/超时断开不刷屏
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        self.thread = threading.Thread(target=self.httpd.serve_forever, args=(0.02,), daemon=True)
        self.thread.start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()

    # 记录 ---------------------------------------------------------------

    def of(self, model: str) -> list[dict]:
        return [r for r in self.records if r["model"] == model]

    def _locate(self, frames: bytes) -> int | None:
        index = self.pcm.find(frames) if frames else -1
        while index >= 0 and index % 2:
            index = self.pcm.find(frames, index + 1)
        if index < 0:
            return None
        sample = index // 2
        assert sample * 1000 % RATE == 0, "截取点不在整毫秒上"
        return sample * 1000 // RATE

    def _handle(self, h: BaseHTTPRequestHandler) -> None:
        body = h.rfile.read(int(h.headers["Content-Length"]))
        fields, part = _parse_multipart(h.headers["Content-Type"], body)
        with wave.open(io.BytesIO(part["data"]), "rb") as w:
            params = (w.getnchannels(), w.getframerate(), w.getsampwidth())
            nframes = w.getnframes()
            frames = w.readframes(nframes)
        start_ms = self._locate(frames)
        record = {
            "model": fields["model"], "response_format": fields["response_format"],
            "language": fields.get("language"), "filename": part["filename"],
            "file_content_type": part["content_type"], "params": params,
            "nframes": nframes, "start_ms": start_ms,
            "end_ms": None if start_ms is None else start_ms + nframes * 1000 // RATE,
            "client": h.headers.get("X-Xxyi-Client"), "auth": h.headers.get("Authorization"),
        }
        with self.lock:
            self.records.append(record)
            self.inflight += 1
            self.max_inflight = max(self.max_inflight, self.inflight)
        try:
            action = self.hook(record) if self.hook else None
            if action and action[0] == "drop":
                h.close_connection = True
                return
            if action and action[0] == "sleep":
                time.sleep(action[1])
            if action and action[0] == "wait":
                action[1].wait(30)
            if action and action[0] == "status":
                self._send(h, action[1], action[2])
                return
            if record["response_format"] == "verbose_json":
                payload = {"text": "whisper", "segments": self._timing(record)}
            else:
                time.sleep(self.text_delay)
                payload = {"text": self._text(record), "language": "chinese", "segments": []}
            self._send(h, 200, json.dumps(payload, ensure_ascii=False))
        finally:
            with self.lock:
                self.inflight -= 1

    @staticmethod
    def _send(h, status: int, body: str) -> None:
        data = body.encode("utf-8")
        h.send_response(status)
        h.send_header("Content-Type", "application/json")
        h.send_header("Content-Length", str(len(data)))
        h.end_headers()
        h.wfile.write(data)

    def _timing(self, record: dict) -> list[dict]:
        lo, hi = record["start_ms"], record["end_ms"]
        return [
            {"start": max(0, s - lo) / 1000, "end": (min(hi, e) - lo) / 1000, "text": f"w{n}"}
            for n, (s, e, _) in enumerate(self.script) if s < hi and e > lo
        ]

    def _text(self, record: dict) -> str:
        lo, hi = record["start_ms"], record["end_ms"]
        best, text = 0, ""
        for s, e, words in self.script:
            overlap = min(hi, e) - max(lo, s)
            if overlap > best:
                best, text = overlap, words
        return text


@pytest.fixture
def make_server():
    servers: list[FakeServer] = []

    def make(*args, **kwargs) -> FakeServer:
        servers.append(FakeServer(*args, **kwargs))
        return servers[-1]

    yield make
    for server in servers:
        server.close()


@pytest.fixture(autouse=True)
def no_waits_no_ffmpeg(monkeypatch):
    """重试等待抹成 0；纯 Python 路绝不该碰 ffmpeg：转码入口一律换成报错（要测转码的用例自己再换）。"""
    monkeypatch.setattr(remote, "_RETRY_WAITS_SECONDS", (0, 0))

    def forbidden(*args, **kwargs):
        raise AssertionError("16k 单声道 WAV 输入不许调 ffmpeg")

    monkeypatch.setattr(remote, "extract_audio", forbidden)


def _kw(server: FakeServer, tmp_path: Path, **extra) -> dict:
    return {"base_url": server.url, "api_key": "secret", "timing_model": "whisper-turbo",
            "text_model": "qwen-asr", "checkpoint_dir": tmp_path / "ckpt", **extra}


def _cues(result) -> list[tuple[int, int, str]]:
    return [(c.start_ms, c.end_ms, c.text) for c in result.cues]


# 9 秒：说话 2s、静音 1s、说话 6s（最长静音 1s，远不到长静音门槛，整条都送识别）。
NINE_S = [("voice", 2_000), ("quiet", 1_000), ("voice", 6_000)]
SCRIPT_9S = [
    (0, 2_400, "第一句。"),
    (2_500, 3_200, "嗯。"),               # 纯语气词 → 丢
    (3_400, 4_400, "好。"),
    (4_450, 5_800, "重叠句。"),           # 起点在段 0 的地盘、中点在段 1 的：按中点归段 1
    (6_000, 8_200, "第二句。"),
]


@pytest.fixture
def nine(tmp_path):
    src = tmp_path / "src"
    src.mkdir()
    pcm = _pcm(NINE_S)
    return _write_wav(src / "talk.wav", pcm), pcm


# ---------------------------------------------------------------------------
# 分段、重叠、截句（纯 Python 路，样本数精确）
# ---------------------------------------------------------------------------


def test_segments_overlap_and_sentence_clips_are_exact(nine, make_server, tmp_path):
    wav, pcm = nine
    server = make_server(pcm, SCRIPT_9S)
    progress = []
    result = transcribe_remote(
        wav, **_kw(server, tmp_path), language="zh-CN", segment_ms=5_000, overlap_ms=1_000,
        on_progress=lambda stage, fraction: progress.append((stage, fraction)),
    )

    assert _cues(result) == [
        (0, 2_400, "第一句。"), (3_400, 4_400, "好。"),
        (4_450, 5_800, "重叠句。"), (6_000, 8_200, "第二句。"),
    ]
    assert all(c.speaker == "" for c in result.cues)
    assert result.engine == "omlx:whisper-turbo+qwen-asr"
    assert progress == [("transcribing", 0.5), ("transcribing", 1.0)]

    # 两段：[0,5000) 与 [4000,9000)，每段 80000 个样本，切点就在源 PCM 的那个位置
    timing = server.of("whisper-turbo")
    assert [(r["start_ms"], r["end_ms"], r["nframes"]) for r in timing] == [
        (0, 5_000, 80_000), (4_000, 9_000, 80_000)]
    assert [r["filename"] for r in timing] == ["seg-0000.wav", "seg-0001.wav"]

    # 句子小样：余量 ≤150ms、不超过与相邻句空隙的一半、不出这一段。
    # 段 0 句序 0..3（3 是 4450–5000 的半句：起点 < 4500 但中点 4725 ≥ 4500，归段 1，不发）；
    # 段 1 句序 0..2：0 是 4000–4400 的碎片（中点 4200 < 4500 → 不发），1 = 重叠句，2 = 第二句。
    text = {r["filename"]: (r["start_ms"], r["end_ms"], r["nframes"]) for r in server.of("qwen-asr")}
    assert text == {
        "seg-0000-s0000.wav": (0, 2_450, 2_450 * 16),        # 段起点夹住前余量；后空隙 100 → 50
        "seg-0000-s0001.wav": (2_450, 3_300, 850 * 16),       # 前后空隙 100/200 → 50/100
        "seg-0000-s0002.wav": (3_300, 4_425, 1_125 * 16),     # 前 200 → 100；后 50 → 25
        "seg-0001-s0001.wav": (4_425, 5_900, 1_475 * 16),     # 前 50 → 25；后 200 → 100
        "seg-0001-s0002.wav": (5_900, 8_350, 2_450 * 16),     # 末句后面给满 150
    }
    for r in server.records:
        assert r["params"] == (1, RATE, 2)
        assert r["file_content_type"] == "audio/wav"
        assert r["client"] == "pybase-asr" and r["auth"] == "Bearer secret"
        assert r["language"] == "zh"                          # zh-CN → zh
    assert {r["response_format"] for r in timing} == {"verbose_json"}
    assert {r["response_format"] for r in server.of("qwen-asr")} == {"json"}


def test_sentence_clip_never_leaves_its_segment(nine, make_server, tmp_path):
    """段尾那句的后余量不伸出这一段：timing_model 听到的就是这一段。"""
    wav, pcm = nine
    server = make_server(pcm, [(4_000, 4_980, "跨段尾。")])
    result = transcribe_remote(wav, **_kw(server, tmp_path), segment_ms=5_000, overlap_ms=1_000)
    assert _cues(result) == [(4_000, 4_980, "跨段尾。")]       # 中点 4490 归段 0
    (clip,) = server.of("qwen-asr")
    assert (clip["filename"], clip["start_ms"], clip["end_ms"]) == ("seg-0000-s0000.wav", 3_850, 5_000)


def test_single_segment_keeps_whisper_start_and_never_writes_next_to_source(
        nine, make_server, tmp_path):
    wav, pcm = nine
    server = make_server(pcm, SCRIPT_9S)
    result = transcribe_remote(wav, **_kw(server, tmp_path))
    assert _cues(result)[0] == (0, 2_400, "第一句。")       # 句首不按音量挪
    assert len(server.of("whisper-turbo")) == 1             # 9 秒 < 10 分钟：一段
    assert all(r["language"] is None for r in server.records)  # 没给 language 就不带
    assert sorted(p.name for p in wav.parent.iterdir()) == ["talk.wav"]  # 源目录不多任何东西
    assert not (tmp_path / "ckpt").exists()                # 成功后断点目录（空了）删掉


def test_text_response_shapes_and_fillers(nine, make_server, tmp_path):
    wav, pcm = nine
    script = [(0, 1_000, ""), (1_200, 1_800, "x"), (3_000, 4_000, "哦哦……"), (5_000, 6_000, "y")]
    server = make_server(pcm, script)

    def hook(record):
        if record["filename"].endswith("s0001.wav"):
            return ("status", 200, "纯文本响应。")           # 不是 JSON → 用原文
        if record["filename"].endswith("s0003.wav"):
            return ("status", 200, json.dumps({"text": "正文在 json 里。", "segments": []}))
        return None

    server.hook = hook
    result = transcribe_remote(wav, **_kw(server, tmp_path))
    # 空正文、纯语气词丢掉；整坨 JSON 不会被当成文字
    assert [c.text for c in result.cues] == ["纯文本响应。", "正文在 json 里。"]


# ---------------------------------------------------------------------------
# 长静音（纯 Python 能量法）
# ---------------------------------------------------------------------------


def test_long_silence_middle_is_skipped(make_server, tmp_path, monkeypatch):
    monkeypatch.setattr(remote, "_LONG_SILENCE_MIN_S", 2)
    pcm = _pcm([("voice", 2_000), ("quiet", 3_000), ("voice", 2_000)])
    wav = _write_wav(tmp_path / "a.wav", pcm)
    server = make_server(pcm, [(200, 1_000, "静音前。"), (5_300, 6_200, "静音后。")])
    result = transcribe_remote(wav, **_kw(server, tmp_path))

    # 30ms 一帧：1980–2010、4980–5010 两帧一半是声音 → 不算静音；长静音 = [2010, 4980)
    assert [(r["start_ms"], r["end_ms"]) for r in server.of("whisper-turbo")] == [
        (0, 2_010), (4_980, 7_000)]
    assert _cues(result) == [(200, 1_000, "静音前。"), (5_300, 6_200, "静音后。")]
    assert all(not (2_010 < r["start_ms"] < 4_980) for r in server.records)


def test_leading_and_trailing_long_silence_skipped(make_server, tmp_path, monkeypatch):
    monkeypatch.setattr(remote, "_LONG_SILENCE_MIN_S", 2)
    pcm = _pcm([("quiet", 3_000), ("voice", 2_000), ("quiet", 3_000)])
    wav = _write_wav(tmp_path / "a.wav", pcm)
    server = make_server(pcm, [(3_200, 4_000, "中间有声音。")])
    result = transcribe_remote(wav, **_kw(server, tmp_path))
    assert [(r["start_ms"], r["end_ms"]) for r in server.of("whisper-turbo")] == [(3_000, 5_010)]
    assert _cues(result) == [(3_200, 4_000, "中间有声音。")]


def test_all_silence_and_empty_wav_send_nothing(make_server, tmp_path, monkeypatch):
    monkeypatch.setattr(remote, "_LONG_SILENCE_MIN_S", 2)
    server = make_server()
    quiet = _write_wav(tmp_path / "quiet.wav", _pcm([("quiet", 3_000)]))
    empty = _write_wav(tmp_path / "empty.wav", b"")
    for wav in (quiet, empty):
        progress = []
        result = transcribe_remote(wav, **_kw(server, tmp_path),
                                   on_progress=lambda s, f: progress.append((s, f)))
        assert result.cues == () and progress == [("transcribing", 1.0)]
    assert server.records == []


def test_long_silence_default_threshold_180s(tmp_path):
    """缺省口径：-35 dBFS、≥180 秒。用全零样本造长音频（快），只测检测本身。"""
    def square(amplitude: int, ms: int) -> bytes:
        samples = array.array("h", [amplitude, -amplitude] * (ms * RATE // 2000))
        if sys.byteorder == "big":
            samples.byteswap()
        return samples.tobytes()

    # 边界都落在 30ms 帧格上：990 = 33 帧、182010 = 6067 帧
    pcm = square(8000, 990) + bytes(181_020 * 32) + square(8000, 990) + bytes(179_010 * 32)
    wav = _write_wav(tmp_path / "long.wav", pcm)
    silences = remote._long_silences(wav, cancel=threading.Event())
    assert silences == [(990, 182_010)]                     # 179 秒那段不够 3 分钟，不算

    # -35 dBFS ≈ 幅度 583：方波 RMS 就是幅度。700（-33.4 dB）不算静音，450（-37.2 dB）算。
    pcm = square(700, 180_990) + square(450, 181_020)
    wav = _write_wav(tmp_path / "levels.wav", pcm)
    assert remote._long_silences(wav, cancel=threading.Event()) == [(180_990, 362_010)]
    cancel = threading.Event()
    cancel.set()
    with pytest.raises(TranscriptionCancelled):
        remote._long_silences(wav, cancel=cancel)


# ---------------------------------------------------------------------------
# 并发、重试、取消
# ---------------------------------------------------------------------------

SIX_SENTENCES = [(i * 1_000, i * 1_000 + 600, f"第{i}句。") for i in range(6)]


@pytest.mark.parametrize("concurrency, expected", [(None, 2), (1, 1), (3, 3)])
def test_concurrency_limits_inflight_sentence_requests(concurrency, expected, make_server, tmp_path):
    pcm = _pcm([("voice", 6_000)])
    wav = _write_wav(tmp_path / "a.wav", pcm)
    server = make_server(pcm, SIX_SENTENCES, text_delay=0.2)
    kwargs = {} if concurrency is None else {"concurrency": concurrency}
    result = transcribe_remote(wav, **_kw(server, tmp_path), **kwargs)
    assert len(result.cues) == 6
    assert server.max_inflight == expected                  # 缺省 2


def _one_sentence(tmp_path, make_server):
    pcm = _pcm([("voice", 3_000)])
    wav = _write_wav(tmp_path / "a.wav", pcm)
    return wav, make_server(pcm, [(2_000, 2_600, "那一句。")])


def test_5xx_retried_twice_then_fails_with_reason(tmp_path, make_server):
    wav, server = _one_sentence(tmp_path, make_server)
    server.hook = lambda r: ("status", 503, "server is busy") if r["model"] == "qwen-asr" else None
    with pytest.raises(AsrError, match="重试 2 次") as excinfo:
        transcribe_remote(wav, **_kw(server, tmp_path))
    assert len(server.of("qwen-asr")) == 3
    message = str(excinfo.value)
    assert "qwen-asr" in message and "503" in message and "共请求 3 次" in message


def test_4xx_not_retried(tmp_path, make_server):
    wav, server = _one_sentence(tmp_path, make_server)
    server.hook = lambda r: ("status", 400, "bad language") if r["model"] == "qwen-asr" else None
    with pytest.raises(AsrError, match="HTTP 400"):
        transcribe_remote(wav, **_kw(server, tmp_path))
    assert len(server.of("qwen-asr")) == 1


def test_dropped_connection_and_timeout_are_retried(tmp_path, make_server, monkeypatch):
    monkeypatch.setattr(remote, "_TEXT_HTTP_TIMEOUT_SECONDS", 0.3)
    wav, server = _one_sentence(tmp_path, make_server)
    actions = iter([("drop",), ("sleep", 1.0)])           # 第一次断开、第二次读超时、第三次成功
    server.hook = lambda r: next(actions, None) if r["model"] == "qwen-asr" else None
    result = transcribe_remote(wav, **_kw(server, tmp_path))
    assert [c.text for c in result.cues] == ["那一句。"]
    assert len(server.of("qwen-asr")) == 3


def test_timing_request_retried_on_5xx(tmp_path, make_server):
    wav, server = _one_sentence(tmp_path, make_server)
    calls = []

    def hook(r):
        if r["model"] == "whisper-turbo":
            calls.append(r)
            return ("status", 502, "bad gateway") if len(calls) == 1 else None
        return None

    server.hook = hook
    result = transcribe_remote(wav, **_kw(server, tmp_path))
    assert len(calls) == 2 and [c.text for c in result.cues] == ["那一句。"]


def test_timing_shape_error_names_model_and_segment(tmp_path, make_server):
    wav, server = _one_sentence(tmp_path, make_server)
    server.hook = lambda r: ("status", 200, json.dumps({"text": "没有 segments"}))
    with pytest.raises(AsrError, match="whisper-turbo 处理第 1 段.*segments"):
        transcribe_remote(wav, **_kw(server, tmp_path))


def test_cancel_stops_further_sentence_requests(tmp_path, make_server):
    pcm = _pcm([("voice", 6_000)])
    wav = _write_wav(tmp_path / "a.wav", pcm)
    server = make_server(pcm, SIX_SENTENCES)
    cancel = threading.Event()

    def hook(record):
        if record["model"] == "qwen-asr":
            cancel.set()                                    # 第一句请求到达时用户按了取消
        return None

    server.hook = hook
    with pytest.raises(TranscriptionCancelled):
        transcribe_remote(wav, **_kw(server, tmp_path), concurrency=1, cancel_event=cancel)
    assert len(server.of("qwen-asr")) == 1
    assert not (tmp_path / "ckpt" / "seg-0000.json").exists()  # 半截段不落盘


def test_cancel_during_retry_wait_returns_at_once(tmp_path, make_server, monkeypatch):
    monkeypatch.setattr(remote, "_RETRY_WAITS_SECONDS", (30, 30))
    wav, server = _one_sentence(tmp_path, make_server)
    cancel = threading.Event()

    def hook(record):
        if record["model"] == "qwen-asr":
            cancel.set()
            return ("status", 503, "busy")
        return None

    server.hook = hook
    started = time.monotonic()
    with pytest.raises(TranscriptionCancelled):
        transcribe_remote(wav, **_kw(server, tmp_path), cancel_event=cancel)
    assert time.monotonic() - started < 5
    assert len(server.of("qwen-asr")) == 1


# ---------------------------------------------------------------------------
# 断点续跑
# ---------------------------------------------------------------------------

TWO_SEG = {"segment_ms": 5_000, "overlap_ms": 1_000}


def test_checkpoint_resumes_after_failure_and_keeps_callers_files(nine, make_server, tmp_path):
    wav, pcm = nine
    server = make_server(pcm, SCRIPT_9S)
    ckpt = tmp_path / "ckpt"
    ckpt.mkdir()
    (ckpt / "keep.txt").write_text("调用方自己的文件", encoding="utf-8")
    asleep = {"on": True}
    server.hook = lambda r: (("status", 503, "asleep")
                             if asleep["on"] and r["filename"] == "seg-0001-s0002.wav" else None)
    with pytest.raises(AsrError, match="重试 2 次"):
        transcribe_remote(wav, **_kw(server, tmp_path), **TWO_SEG)

    payload = json.loads((ckpt / "seg-0000.json").read_text(encoding="utf-8"))
    assert not (ckpt / "seg-0001.json").exists()            # 失败的段不落盘
    assert payload["cues"] == [
        {"start_ms": 0, "end_ms": 2_400, "text": "第一句。", "speaker": ""},
        {"start_ms": 3_400, "end_ms": 4_400, "text": "好。", "speaker": ""},
    ]
    fp = payload["fingerprint"]
    assert fp["source"] == str(wav.resolve()) and fp["text_model"] == "qwen-asr"
    assert (fp["long_silence_noise_db"], fp["long_silence_min_s"]) == (-35.0, 180)

    asleep["on"] = False
    server.records.clear()
    progress = []
    result = transcribe_remote(wav, **_kw(server, tmp_path), **TWO_SEG,
                               on_progress=lambda s, f: progress.append(f))
    assert not any(r["filename"].startswith("seg-0000") for r in server.records)
    assert [r["filename"] for r in server.of("whisper-turbo")] == ["seg-0001.wav"]
    assert progress == [0.5, 1.0]
    assert sorted(p.name for p in ckpt.iterdir()) == ["keep.txt"]   # 只删自己的

    baseline = transcribe_remote(wav, **_kw(server, tmp_path / "b"), **TWO_SEG)
    assert _cues(result) == _cues(baseline)


def test_checkpoint_resumes_after_process_killed(nine, make_server, tmp_path):
    """真杀进程：子进程跑到第 2 段时 SIGKILL，再跑一遍不重发第 1 段。"""
    wav, pcm = nine
    server = make_server(pcm, SCRIPT_9S)
    release = threading.Event()
    server.hook = lambda r: ("wait", release) if r["filename"].startswith("seg-0001") else None
    ckpt = tmp_path / "ckpt"
    child = subprocess.Popen([sys.executable, "-c", (
        "import sys; sys.path.insert(0, sys.argv[1]);"
        "from pybase.asr import transcribe_remote;"
        "transcribe_remote(sys.argv[2], base_url=sys.argv[3], api_key='', timing_model='whisper-turbo',"
        " text_model='qwen-asr', checkpoint_dir=sys.argv[4], segment_ms=5000, overlap_ms=1000)"
    ), str(PY_DIR), str(wav), server.url, str(ckpt)])
    try:
        deadline = time.monotonic() + 60
        while not any(r["filename"] == "seg-0001.wav" for r in server.records):
            assert child.poll() is None, "子进程提前退出"
            assert time.monotonic() < deadline, "子进程没跑到第 2 段"
            time.sleep(0.05)
        os.kill(child.pid, signal.SIGKILL)
        child.wait(10)
    finally:
        if child.poll() is None:
            child.kill()
        release.set()
    assert (ckpt / "seg-0000.json").is_file()
    assert sorted(p.name for p in ckpt.iterdir()) == ["seg-0000.json"]  # 没有半截的 .tmp

    server.hook = None
    server.records.clear()
    result = transcribe_remote(wav, **_kw(server, tmp_path), **TWO_SEG)
    assert not any(r["filename"].startswith("seg-0000") for r in server.records)
    assert [r["filename"] for r in server.of("whisper-turbo")] == ["seg-0001.wav"]
    assert [c.text for c in result.cues] == ["第一句。", "好。", "重叠句。", "第二句。"]
    assert not ckpt.exists()


def test_checkpoint_not_reused_when_model_changes_or_file_corrupted(nine, make_server, tmp_path):
    wav, pcm = nine
    server = make_server(pcm, SCRIPT_9S)
    ckpt = tmp_path / "ckpt"

    def fail_once_more():
        server.hook = lambda r: ("status", 400, "x") if r["filename"].startswith("seg-0001") else None
        with pytest.raises(AsrError):
            transcribe_remote(wav, **_kw(server, tmp_path), **TWO_SEG)
        assert (ckpt / "seg-0000.json").is_file()
        server.hook = None
        server.records.clear()

    fail_once_more()
    result = transcribe_remote(wav, **_kw(server, tmp_path, text_model="qwen-asr-v2"), **TWO_SEG)
    assert [r["filename"] for r in server.of("whisper-turbo")] == ["seg-0000.wav", "seg-0001.wav"]
    assert result.engine == "omlx:whisper-turbo+qwen-asr-v2"

    fail_once_more()
    (ckpt / "seg-0000.json").write_text('{"fingerprint": {"so', encoding="utf-8")
    transcribe_remote(wav, **_kw(server, tmp_path), **TWO_SEG)
    assert [r["filename"] for r in server.of("whisper-turbo")] == ["seg-0000.wav", "seg-0001.wav"]


# ---------------------------------------------------------------------------
# 非 16k 单声道 WAV：转一次放 checkpoint_dir（本机用假转码器，真 ffmpeg 见文件末尾）
# ---------------------------------------------------------------------------


def _fake_converter(calls: list):
    """把 8k WAV 每个样本重复一遍「升采样」成 16k，写到 extract_audio 给的输出路径。"""
    def convert(src, out, *, sample_rate, mono, **kwargs):
        calls.append((Path(src), Path(out), sample_rate, mono))
        with wave.open(str(src), "rb") as w:
            frames = w.readframes(w.getnframes())
        samples = array.array("h")
        samples.frombytes(frames)
        doubled = array.array("h", (x for x in samples for _ in (0, 1)))
        _write_wav(Path(out), doubled.tobytes())
    return convert


def test_non_16k_input_converted_once_into_checkpoint_dir(make_server, tmp_path, monkeypatch):
    src_dir = tmp_path / "src"
    src_dir.mkdir()
    pcm8k = _pcm([("voice", 9_000)])[: 9_000 * 8 * 2]       # 9 秒 8k
    source = _write_wav(src_dir / "talk-8k.wav", pcm8k, rate=8_000)
    samples = array.array("h")
    samples.frombytes(pcm8k)
    pcm16k = array.array("h", (x for x in samples for _ in (0, 1))).tobytes()
    server = make_server(pcm16k, SCRIPT_9S)
    calls: list = []
    monkeypatch.setattr(remote, "extract_audio", _fake_converter(calls))
    ckpt = tmp_path / "ckpt"

    server.hook = lambda r: ("status", 400, "x") if r["filename"].startswith("seg-0001") else None
    progress = []
    with pytest.raises(AsrError):
        transcribe_remote(source, **_kw(server, tmp_path), **TWO_SEG,
                          on_progress=lambda s, f: progress.append(s))
    assert calls == [(source, ckpt / "audio-16k.wav", 16_000, True)]
    assert progress[0] == "converting"
    assert (ckpt / "audio-16k.wav").is_file() and (ckpt / "audio-16k.json").is_file()
    # 发出去的段就是转出的 16k WAV 里的那一段
    assert [(r["start_ms"], r["nframes"]) for r in server.of("whisper-turbo")] == [
        (0, 80_000), (4_000, 80_000)]

    server.hook = None
    result = transcribe_remote(source, **_kw(server, tmp_path), **TWO_SEG)
    assert len(calls) == 1                                  # 续跑直接用转好的，不再转
    assert [c.text for c in result.cues] == ["第一句。", "好。", "重叠句。", "第二句。"]
    assert not ckpt.exists()                                # 成功后转出的 WAV 也删掉
    assert sorted(p.name for p in src_dir.iterdir()) == ["talk-8k.wav"]


def test_cleanup_never_deletes_the_source_even_if_named_like_ours(make_server, tmp_path):
    ckpt = tmp_path / "ckpt"
    ckpt.mkdir()
    pcm = _pcm([("voice", 3_000)])
    source = _write_wav(ckpt / "audio-16k.wav", pcm)        # 调用方的源文件碰巧放这儿、叫这个名
    server = make_server(pcm, [(2_000, 2_600, "那一句。")])
    result = transcribe_remote(source, **_kw(server, tmp_path))
    assert [c.text for c in result.cues] == ["那一句。"]
    assert sorted(p.name for p in ckpt.iterdir()) == ["audio-16k.wav"]


# ---------------------------------------------------------------------------
# refine_cues：只换文字，不动时间轴和说话人
# ---------------------------------------------------------------------------

REFINE_SCRIPT = [
    (0, 1_000, "千问第一句。"),
    (1_200, 2_000, "千问第二句。"),
    (2_200, 3_000, "对对对对对对对对"),       # 对着噪音的重复字 → 保留原文
    (3_200, 4_000, "原文一样"),               # 去标点后与原文相同 → kept
    (4_200, 5_000, "千问第五句。"),
]
APPLE_CUES = [
    Cue(0, 1_000, "苹果第一句", "测试员"),
    Cue(1_200, 2_000, "苹果第二句", "客户"),
    Cue(2_200, 3_000, "苹果第三句", "测试员"),
    Cue(3_200, 4_000, "原文一样。", "客户"),
    Cue(4_200, 5_000, "苹果第五句", ""),
]


@pytest.fixture
def refine_audio(tmp_path):
    pcm = _pcm([("voice", 6_000)], seed=3)
    return _write_wav(tmp_path / "mic.wav", pcm), pcm


def _refine_kw(server, tmp_path, **extra):
    return {"base_url": server.url, "api_key": "k", "text_model": "qwen-asr",
            "checkpoint_dir": tmp_path / "ckpt", **extra}


def test_refine_replaces_text_only(refine_audio, make_server, tmp_path):
    wav, pcm = refine_audio
    server = make_server(pcm, REFINE_SCRIPT)
    progress = []
    cues, stats = refine_cues(APPLE_CUES, wav, **_refine_kw(server, tmp_path), skip={1},
                              language="zh-CN", on_progress=lambda s, f: progress.append((s, f)))

    assert [(c.start_ms, c.end_ms, c.speaker) for c in cues] == [
        (c.start_ms, c.end_ms, c.speaker) for c in APPLE_CUES]
    assert [c.text for c in cues] == ["千问第一句。", "苹果第二句", "苹果第三句", "原文一样。", "千问第五句。"]
    assert stats == {"changed": 2, "kept": 2, "failed": 0, "skipped": 1, "resumed": 0}
    assert sorted(r["filename"] for r in server.records) == [
        "cue-00000.wav", "cue-00002.wav", "cue-00003.wav", "cue-00004.wav"]   # skip 的不发
    clips = {r["filename"]: (r["start_ms"], r["end_ms"], r["nframes"]) for r in server.records}
    assert clips["cue-00000.wav"] == (0, 1_100, 1_100 * 16)        # 夹在 0；后空隙 200 → 100
    assert clips["cue-00004.wav"] == (4_100, 5_150, 1_050 * 16)    # 末句后面给满 150
    assert {r["language"] for r in server.records} == {"zh"}
    assert len(progress) == 5 and {s for s, _ in progress} == {"refining"}
    assert max(f for _, f in progress) == 1.0
    assert not (tmp_path / "ckpt").exists()


def test_refine_failed_sentence_kept_then_resumed(refine_audio, make_server, tmp_path):
    wav, pcm = refine_audio
    server = make_server(pcm, REFINE_SCRIPT)
    server.hook = lambda r: ("status", 400, "x") if r["filename"] == "cue-00004.wav" else None
    cues, stats = refine_cues(APPLE_CUES, wav, **_refine_kw(server, tmp_path))
    assert cues[4] == APPLE_CUES[4] and stats["failed"] == 1
    saved = json.loads((tmp_path / "ckpt" / "refine.json").read_text(encoding="utf-8"))
    assert sorted(saved["done"]) == ["0", "1", "2", "3"]   # 失败的不记，下次重试

    server.hook = None
    server.records.clear()
    cues, stats = refine_cues(APPLE_CUES, wav, **_refine_kw(server, tmp_path))
    assert [r["filename"] for r in server.records] == ["cue-00004.wav"]
    assert stats["resumed"] == 4 and stats["failed"] == 0
    assert cues[4].text == "千问第五句。" and cues[0].text == "千问第一句。"
    assert not (tmp_path / "ckpt").exists()


def test_refine_all_failed_or_server_down_raises(refine_audio, make_server, tmp_path):
    wav, pcm = refine_audio
    server = make_server(pcm, REFINE_SCRIPT)
    server.hook = lambda r: ("status", 400, "x")
    with pytest.raises(AsrError, match="每一句都校正失败"):
        refine_cues(APPLE_CUES, wav, **_refine_kw(server, tmp_path))

    many = [Cue(i * 100, i * 100 + 80, "原文") for i in range(20)]
    server.records.clear()
    with pytest.raises(AsrError, match="连续 6 句校正失败"):
        refine_cues(many, wav, **_refine_kw(server, tmp_path / "x"), concurrency=1)
    assert len(server.records) == 6


def test_refine_cancel(refine_audio, make_server, tmp_path):
    wav, pcm = refine_audio
    server = make_server(pcm, REFINE_SCRIPT)
    cancel = threading.Event()

    def hook(record):
        cancel.set()
        return None

    server.hook = hook
    with pytest.raises(TranscriptionCancelled):
        refine_cues(APPLE_CUES, wav, **_refine_kw(server, tmp_path), concurrency=1,
                    cancel_event=cancel)
    assert len(server.records) == 1


def test_refine_rejects_subtitle_cues(refine_audio, make_server, tmp_path):
    from pybase.subtitle.models import Cue as SubtitleCue

    wav, _ = refine_audio
    server = make_server()
    with pytest.raises(TypeError, match="from_subtitle"):
        refine_cues([SubtitleCue(0, 100, "x")], wav, **_refine_kw(server, tmp_path))


# ---------------------------------------------------------------------------
# 入参
# ---------------------------------------------------------------------------


def test_arguments_are_checked(nine, make_server, tmp_path):
    wav, _ = nine
    server = make_server()
    kw = _kw(server, tmp_path)
    with pytest.raises(FileNotFoundError):
        transcribe_remote(tmp_path / "nope.wav", **kw)
    with pytest.raises(ValueError, match="base_url"):
        transcribe_remote(wav, **{**kw, "base_url": " "})
    with pytest.raises(ValueError, match="checkpoint_dir"):
        transcribe_remote(wav, **{**kw, "checkpoint_dir": None})
    with pytest.raises(TypeError):
        transcribe_remote(wav, **{k: v for k, v in kw.items() if k != "checkpoint_dir"})
    with pytest.raises(ValueError, match="language"):
        transcribe_remote(wav, **kw, language="日语")
    with pytest.raises(ValueError, match="concurrency"):
        transcribe_remote(wav, **kw, concurrency=0)
    with pytest.raises(ValueError, match="overlap_ms"):
        transcribe_remote(wav, **kw, segment_ms=1_000, overlap_ms=1_000)
    with pytest.raises(ValueError, match="checkpoint_dir"):
        refine_cues([], wav, base_url=server.url, api_key="", text_model="m", checkpoint_dir="")
    assert server.records == []


# ---------------------------------------------------------------------------
# 纯函数
# ---------------------------------------------------------------------------


def test_voiced_intervals():
    voiced = remote._voiced_intervals
    assert voiced(10_000, []) == [(0, 10_000)]
    assert voiced(10_000, [(2_000, 5_000)]) == [(0, 2_000), (5_000, 10_000)]
    assert voiced(10_000, [(0, 3_000), (7_000, 10_000)]) == [(3_000, 7_000)]
    assert voiced(10_000, [(0, 10_000)]) == []
    assert voiced(10_000, [(1_000, 4_000), (3_000, 6_000)]) == [(0, 1_000), (6_000, 10_000)]
    assert voiced(10_000, [(4_000, 4_000)]) == [(0, 10_000)]
    assert voiced(10_000, [(9_000, 20_000)]) == [(0, 9_000)]


def test_plan_segments():
    spans = [(s.start_ms, s.end_ms) for s in remote._plan_segments(
        1_500_000, segment_ms=600_000, overlap_ms=5_000)]
    assert spans == [(0, 600_000), (595_000, 1_195_000), (1_190_000, 1_500_000)]
    assert [(s.start_ms, s.end_ms) for s in remote._plan_segments(
        600_000, segment_ms=600_000, overlap_ms=5_000)] == [(0, 600_000)]


def test_sentence_pads_never_eat_the_neighbours():
    pads = remote._sentence_pads
    assert pads(1_000, 2_000, None, None) == (150, 150)
    assert pads(1_000, 2_000, 2_000, 2_000) == (0, 0)
    assert pads(1_000, 2_000, 900, 2_200) == (50, 100)
    assert pads(1_000, 2_000, 950, 2_099) == (25, 49)
    assert pads(1_000, 2_000, 500, 6_000) == (150, 150)


def test_drop_contained_duplicates():
    cues = [
        Cue(591_200, 594_500, "你查到的数据，数据库里面有没有？"),
        Cue(591_200, 593_200, "你查到的数据。"),          # 碎片 → 丢
        Cue(595_000, 596_000, "你查到的数据。"),            # 时间不含 → 留
        Cue(597_000, 598_000, "另一件事。"),
        Cue(597_200, 597_600, "另一件"),                    # 碎片 → 丢
    ]
    cues.sort(key=lambda c: (c.start_ms, -c.end_ms))
    kept = [(c.start_ms, c.end_ms) for c in remote.drop_contained_duplicates(cues)]
    assert kept == [(591_200, 594_500), (595_000, 596_000), (597_000, 598_000)]


def test_normalize_language():
    norm = remote._normalize_language
    assert norm("zh-TW") == "zh" and norm("zh_Hans") == "zh" and norm("EN") == "en"
    assert norm(None) is None and norm("") is None and norm("  ") is None
    with pytest.raises(ValueError):
        norm("日语")


def test_wav_detection_only_accepts_16k_mono_16bit(tmp_path):
    assert remote._wav_frames(_write_wav(tmp_path / "ok.wav", b"\x00\x00" * 160)) == 160
    assert remote._wav_frames(_write_wav(tmp_path / "8k.wav", b"\x00\x00" * 80, rate=8_000)) is None
    with wave.open(str(tmp_path / "stereo.wav"), "wb") as w:
        w.setnchannels(2)
        w.setsampwidth(2)
        w.setframerate(RATE)
        w.writeframes(b"\x00" * 64)
    assert remote._wav_frames(tmp_path / "stereo.wav") is None
    (tmp_path / "x.webm").write_bytes(b"\x1aE\xdf\xa3 not a wav")
    assert remote._wav_frames(tmp_path / "x.webm") is None


# ---------------------------------------------------------------------------
# 真 ffmpeg（Studio）：非 WAV 输入转码一次后走纯 Python 路
# ---------------------------------------------------------------------------


def _ffmpeg_audio(tmp_path: Path, name: str, args: list[str]) -> Path:
    out = tmp_path / "src" / name
    out.parent.mkdir(exist_ok=True)
    subprocess.run([shutil.which("ffmpeg"), "-hide_banner", "-loglevel", "error", "-y",
                    "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=9",
                    *args, str(out)], check=True, capture_output=True)
    return out


@requires_ffmpeg
def test_real_ffmpeg_conversion_then_pure_python(make_server, tmp_path, monkeypatch):
    from pybase.media import extract_audio

    monkeypatch.setattr(remote, "extract_audio", extract_audio)
    source = _ffmpeg_audio(tmp_path, "talk.m4a", ["-ac", "2", "-c:a", "aac"])
    server = make_server()                                  # 转码过的音频定位不了，只看形状
    seen = []

    def hook(record):
        if record["response_format"] == "verbose_json":
            seen.append((record["params"], record["nframes"]))
            return ("status", 200, json.dumps({"segments": [
                {"start": 0.5, "end": 1.5, "text": "w"}]}))
        return ("status", 200, json.dumps({"text": "转码后的文字。"}))

    server.hook = hook
    result = transcribe_remote(source, **_kw(server, tmp_path), **TWO_SEG)
    assert [(c.start_ms, c.end_ms) for c in result.cues] == [(500, 1_500), (4_500, 5_500)]
    assert [p for p, _ in seen] == [(1, RATE, 2)] * 2
    assert abs(seen[0][1] - 80_000) <= 16 and seen[1][1] > 70_000
    assert not (tmp_path / "ckpt").exists()
    assert sorted(p.name for p in source.parent.iterdir()) == ["talk.m4a"]


@requires_ffmpeg
def test_real_ffmpeg_refine_from_flac(make_server, tmp_path, monkeypatch):
    from pybase.media import extract_audio

    monkeypatch.setattr(remote, "extract_audio", extract_audio)
    source = _ffmpeg_audio(tmp_path, "mic.flac", ["-ac", "2", "-ar", "48000"])
    server = make_server()
    server.hook = lambda r: ("status", 200, json.dumps({"text": f"重听{r['nframes'] // 16}毫秒"}))
    cues, stats = refine_cues([Cue(1_000, 2_000, "原文", "测试员")], source,
                              **_refine_kw(server, tmp_path))
    assert cues == [Cue(1_000, 2_000, "重听1300毫秒", "测试员")]   # 前后各 150ms 余量
    assert stats["changed"] == 1
    assert not (tmp_path / "ckpt").exists()
