"""pybase.asr 的测试。

引擎本身全部用替身：Apple 辅助程序的协议用假子进程/假脚本模拟，mlx-audio 用假模型。
只有最后的集成用例会在 macOS 26+ 上真编译、真识别（`say` 合成的语音），其他平台跳过。
"""
from __future__ import annotations

import json
import platform
import shutil
import subprocess
import sys
import textwrap
import threading
from types import SimpleNamespace

import pytest

import pybase.asr.apple as apple_module
import pybase.asr.mlx as mlx_module
from pybase.asr import AppleSpeech, AsrError, Hypothesis, MlxAudio, Word
from pybase.asr._text import clean, join, segment_cues


# ---------------------------------------------------------------------------
# 文本整理
# ---------------------------------------------------------------------------


def test_clean_removes_space_before_fullwidth_punctuation():
    assert clean(" 今天天气很好 ，我们走吧 。") == "今天天气很好，我们走吧。"
    assert clean(" Hello, world. ") == "Hello, world."


def test_join_adds_space_only_between_non_cjk():
    assert join(["今天", "天气"]) == "今天天气"
    assert join(["Hello.", "World"]) == "Hello. World"
    assert join(["中文", "English"]) == "中文 English"
    assert join(["", " a ", ""]) == "a"


def test_segment_cues_splits_on_sentence_end_by_word_times():
    words = [Word("你", 0, 100), Word("好", 100, 200), Word("。", 200, 300),
             Word("再", 1000, 1100), Word("见", 1100, 1200)]
    cues = segment_cues("你好。再见", 0, 1200, words)
    assert [(c.start, c.end, c.text) for c in cues] == [(0, 300, "你好。"), (1000, 1200, "再见")]


def test_segment_cues_keeps_english_word_spacing():
    words = [Word("Hi", 0, 100), Word(" there.", 100, 400), Word(" Bye", 500, 700), Word(" now!", 700, 900)]
    cues = segment_cues("", 0, 900, words)
    assert [c.text for c in cues] == ["Hi there.", "Bye now!"]


def test_segment_cues_falls_back_to_segment_times():
    cues = segment_cues("整段", 500, 900)
    assert [(c.start, c.end, c.text) for c in cues] == [(500, 900, "整段")]
    # 词都没时间就用分段时间；分段也没时间的丢弃
    assert segment_cues("x", 10, 20, [Word("x", None, None)])[0].start == 10
    assert segment_cues("x", None, None) == []
    assert segment_cues("   ", 0, 10) == []


# ---------------------------------------------------------------------------
# Apple：辅助程序协议
# ---------------------------------------------------------------------------


def _lines(*events) -> str:
    return "".join(json.dumps(e, ensure_ascii=False) + "\n" for e in events)


def test_apple_transcribe_parses_final_results(monkeypatch):
    stdout = _lines(
        {"status": "installing"},
        {"text": "今天 ，好。明天", "start": 0, "end": 2000, "final": True, "words": [
            {"text": "今", "start": 0, "end": 100}, {"text": "天", "start": 100, "end": 200},
            {"text": " ，", "start": 200, "end": 300}, {"text": "好", "start": 300, "end": 400},
            {"text": "。", "start": 400, "end": 500}, {"text": "明", "start": 1500, "end": 1700},
            {"text": "天", "start": 1700, "end": 2000},
        ]},
        {"text": " Next.", "start": 2000, "end": 3000, "final": True},
    )
    calls = []

    def fake_run(cmd, *, timeout, cancel_event=None):
        calls.append(cmd)
        return subprocess.CompletedProcess(cmd, 0, stdout, "")

    monkeypatch.setattr(apple_module, "run", fake_run)
    result = AppleSpeech("zh-CN", helper="/fake/helper").transcribe("a.wav")
    assert calls == [["/fake/helper", "file", "zh-CN", "a.wav"]]
    assert result.engine == "apple-speech:zh-CN"
    assert result.language == "zh-CN"
    assert result.text == "今天，好。明天 Next."
    assert [(c.start, c.end, c.text) for c in result.cues] == [
        (0, 500, "今天，好。"), (1500, 2000, "明天"), (2000, 3000, "Next."),
    ]


def test_apple_transcribe_raises_helper_error(monkeypatch):
    def fake_run(cmd, *, timeout, cancel_event=None):
        return subprocess.CompletedProcess(cmd, 1, _lines({"error": "不支持的 locale：xx"}), "")

    monkeypatch.setattr(apple_module, "run", fake_run)
    with pytest.raises(AsrError, match="不支持的 locale"):
        AppleSpeech("xx", helper="/fake/helper").transcribe("a.wav")


def _fake_helper(tmp_path, body: str) -> str:
    """造一个假辅助程序：stream 模式读完 stdin 再按 body 输出。"""
    script = tmp_path / "fake-helper"
    script.write_text(f"#!{sys.executable}\n" + textwrap.dedent(body))
    script.chmod(0o755)
    return str(script)


def test_apple_stream_protocol(tmp_path):
    helper = _fake_helper(tmp_path, """
        import json, sys
        assert sys.argv[1:] == ["stream", "en-US", "8000", "fast"], sys.argv
        data = sys.stdin.buffer.read()
        out = lambda e: print(json.dumps(e), flush=True)
        out({"text": "hel", "start": 0, "end": 100, "final": False})
        out({"text": f"bytes {len(data)}", "start": 0, "end": 200, "final": True,
             "words": [{"text": "bytes", "start": 0, "end": 200}]})
    """)
    engine = AppleSpeech("en-US", helper=helper, fast_results=True)
    with engine.open_stream(sample_rate=8000) as stream:
        assert stream.sample_rate == 8000
        stream.feed(b"\x00\x01" * 10)
        stream.feed(b"\x00")
    got = stream.results()
    assert got == [
        Hypothesis("hel", False, 0, 100),
        Hypothesis("bytes 21", True, 0, 200, (Word("bytes", 0, 200),)),
    ]
    with pytest.raises(AsrError, match="已结束"):
        stream.feed(b"\x00\x00")


def test_apple_stream_reports_helper_failure(tmp_path):
    helper = _fake_helper(tmp_path, """
        import json, sys
        sys.stdin.buffer.read()
        print(json.dumps({"error": "boom"}), flush=True)
        sys.exit(1)
    """)
    stream = AppleSpeech("en-US", helper=helper).open_stream()
    with pytest.raises(AsrError, match="boom"):
        stream.close()


def test_apple_stream_abort_kills_helper(tmp_path):
    helper = _fake_helper(tmp_path, """
        import time
        time.sleep(60)
    """)
    stream = AppleSpeech("en-US", helper=helper).open_stream()
    with pytest.raises(RuntimeError, match="caller"):
        with stream:
            raise RuntimeError("caller")
    assert stream._proc.returncode is not None


@pytest.mark.skipif(sys.platform != "darwin", reason="只在 macOS 上编译")
def test_build_helper_failure_leaves_nothing(tmp_path, monkeypatch):
    monkeypatch.setattr(apple_module, "_check_platform", lambda: None)
    monkeypatch.setattr(apple_module, "resolve_tool", lambda explicit, name: "/fake/swiftc")
    monkeypatch.setattr(
        apple_module, "run",
        lambda cmd, *, timeout: subprocess.CompletedProcess(cmd, 1, "", "error: nope"),
    )
    out = tmp_path / "helper"
    with pytest.raises(AsrError, match="nope"):
        apple_module.build_helper(out)
    assert list(tmp_path.iterdir()) == []


def test_build_helper_rejects_non_mac(monkeypatch):
    monkeypatch.setattr(apple_module.sys, "platform", "linux")
    with pytest.raises(AsrError, match="macOS"):
        apple_module.build_helper()


# ---------------------------------------------------------------------------
# mlx-audio：假模型
# ---------------------------------------------------------------------------


class _FakeModel:
    def __init__(self, output, session=None):
        self.output = output
        self.session = session
        self.threads = set()
        self.calls = []

    def generate(self, audio, **kwargs):
        self.threads.add(threading.get_ident())
        self.calls.append((audio, kwargs))
        return self.output


def _use_model(monkeypatch, model):
    loaded = []

    def fake_load(name):
        loaded.append(name)
        model.threads.add(threading.get_ident())
        return model

    monkeypatch.setattr(mlx_module, "_load", fake_load)
    return loaded


def test_mlx_transcribe_sentences_output(monkeypatch):
    output = SimpleNamespace(text=" Hi there. Bye. ", sentences=[
        SimpleNamespace(text=" Hi there.", start=0.0, end=1.25),
        SimpleNamespace(text=" Bye.", start=2.0, end=2.5),
    ])
    model = _FakeModel(output)
    loaded = _use_model(monkeypatch, model)
    engine = MlxAudio("some/parakeet", generate_kwargs={"chunk_duration": 30})
    result = engine.transcribe("a.wav")
    engine.transcribe("b.wav")
    engine.close()
    assert loaded == ["some/parakeet"]  # 只加载一次
    assert model.calls[0] == ("a.wav", {"chunk_duration": 30})
    assert len(model.threads) == 1 and threading.get_ident() not in model.threads
    assert result.engine == "mlx-audio:some/parakeet"
    assert result.text == "Hi there. Bye."
    assert [(c.start, c.end, c.text) for c in result.cues] == [(0, 1250, "Hi there."), (2000, 2500, "Bye.")]


def test_mlx_transcribe_segments_output(monkeypatch):
    output = SimpleNamespace(text="你好。再见", language=["Chinese", "Chinese"], segments=[
        {"text": "你好。再见", "start": 0.0, "end": 3.0, "words": [
            {"word": "你好。", "start": 0.0, "end": 0.8}, {"word": "再见", "start": 2.0, "end": 3.0},
        ]},
        {"text": "整块没有逐词时间", "start": 3.0, "end": 9.0},
    ])
    _use_model(monkeypatch, _FakeModel(output))
    result = MlxAudio("m").transcribe("a.wav")
    assert result.language == "Chinese"
    assert [(c.start, c.end, c.text) for c in result.cues] == [
        (0, 800, "你好。"), (2000, 3000, "再见"), (3000, 9000, "整块没有逐词时间"),
    ]


def test_mlx_transcribe_text_only_output(monkeypatch):
    _use_model(monkeypatch, _FakeModel(SimpleNamespace(text="just text", segments=None)))
    result = MlxAudio("m").transcribe("a.wav")
    assert result.text == "just text" and result.cues == ()


def test_mlx_open_stream_requires_streaming_model(monkeypatch):
    model = _FakeModel(None)
    _use_model(monkeypatch, model)
    with pytest.raises(AsrError, match="不支持实时识别"):
        MlxAudio("m").open_stream()


class _FakeSession:
    """按 mlx-audio StreamingSession 协议：feed 排队，step 出增量，close 后 step 到 done。"""

    input_sample_rate = 16000

    def __init__(self, threads):
        self.threads = threads
        self.fed = []
        self.closed = False
        self._emitted = 0
        self.done = False

    def feed(self, samples):
        self.fed.append(samples)

    def close(self):
        self.closed = True

    def step(self, *, max_decode_tokens=4):
        self.threads.add(threading.get_ident())
        if self._emitted < len(self.fed):
            self._emitted += 1
            return [f"w{self._emitted} "]
        if self.closed:
            self.done = True
        return []


def test_mlx_stream_decodes_on_model_thread(monkeypatch):
    np = pytest.importorskip("numpy")
    model = _FakeModel(None)
    session = _FakeSession(model.threads)
    model.create_streaming_session = lambda **kw: (model.threads.add(threading.get_ident()), session)[1]
    _use_model(monkeypatch, model)
    engine = MlxAudio("m")
    with engine.open_stream() as stream:
        assert stream.sample_rate == 16000
        stream.feed(b"\x00\x40\x00")  # 奇数字节：留一个到下一块
        stream.feed(b"\x80")
    got = stream.results()
    engine.close()
    assert got[-1] == Hypothesis("w1 w2", True)
    assert all(not h.final for h in got[:-1])
    assert np.allclose(session.fed[0], [0.5]) and np.allclose(session.fed[1], [-1.0])
    assert len(model.threads) == 1 and threading.get_ident() not in model.threads


# ---------------------------------------------------------------------------
# 集成：真的 SpeechAnalyzer
# ---------------------------------------------------------------------------


def _macos_major() -> int:
    major = platform.mac_ver()[0].split(".")[0]
    return int(major) if major.isdigit() else 0


@pytest.mark.skipif(
    sys.platform != "darwin" or _macos_major() < 26 or not shutil.which("swiftc") or not shutil.which("say"),
    reason="需要 macOS 26+、swiftc 和 say",
)
def test_apple_real_transcribe(tmp_path):
    audio = tmp_path / "en.aiff"
    subprocess.run(
        ["say", "-o", str(audio), "Speech recognition works. [[slnc 800]] This is the second sentence."],
        check=True,
    )
    result = AppleSpeech("en-US").transcribe(audio)
    assert "speech recognition" in result.text.lower()
    assert len(result.cues) == 2
    assert result.cues[0].end <= result.cues[1].start
