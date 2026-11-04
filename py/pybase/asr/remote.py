"""远程识别：服务器上「Whisper 定时间、Qwen 出文字」（从 ai-browser core/remote_asr.py 搬来）。

服务器提供 OpenAI 标准的 `POST {base_url}/v1/audio/transcriptions`（multipart：
file/model/language/response_format），两个模型各有一半短板：

* timing_model（如 whisper-*-turbo）：`verbose_json` 每段带 start/end（秒、相对上传
  音频开头），句尾时间准；但没标点、有错别字，开头有静音时第一段 start 会被算成 0。
* text_model（如 Qwen3-ASR）：文字准、有标点，但只回一整段、没有分句时间；而且请求
  `response_format="text"` 也照样回 JSON，所以直接要 `json` 再取 `text`。

所以合着用：timing_model 只管「什么时候说了话」，text_model 只管「说的是什么」——每句
按 timing_model 的时间从音频里截出来单独发给 text_model。请求数是「每句一次」，靠服务器
并发消化（线程池，最多 `concurrency` 个同时在飞）。

**两条路**（`transcribe_remote`、`refine_cues` 都一样）：

* 输入是 `wave` 能读的 16 kHz 单声道 16 位 PCM WAV：全程不调 ffmpeg。时长从 WAV 头算，
  长静音用纯 Python 能量法找（30ms 一帧算 RMS，低于 -35 dBFS 算静音帧，连续 ≥180 秒算
  长静音），切段、切句都按帧位置从 WAV 里读出来、在内存里包成小 WAV 发出去。
* 其他输入（视频、webm、m4a、别的采样率/声道/位深的 WAV……）：先用 ffmpeg 转一次成
  16k 单声道 WAV，放在 `checkpoint_dir/audio-16k.wav`（旁边 `audio-16k.json` 记源文件
  指纹，续跑时源文件没变就直接用），之后走上面那条纯 Python 路。这是唯一调 ffmpeg 的地方。

`transcribe_remote` 的流程：长静音检测 → 有声区间 = [0, 时长) 减去长静音 → 每个有声
区间各自切段（缺省 10 分钟、重叠 5 秒，任何一段都不跨过长静音）→ 每段发 timing_model 拿
分句时间 → 每句截出来并发发 text_model（前后余量最多 150ms，且不超过和相邻句之间空隙的
一半，也不超出这一段）→ 按「每段只负责中间一块」去重重叠部分（句子中点落在
[段起点+重叠/2, 段终点−重叠/2) 才归这段；每个有声区间的首尾段那一侧不缩）→ 语气词/空
文本丢掉 → 排序后清掉「时间被前一句包住、文字也是前一句一部分」的重复句。

短停顿交给识别模型自己判断，程序**不按音量挪句首**（实测 -30dB 的句首校正会把轻声的字
当静音吞掉）。

`refine_cues` 是另一条用法：沿用已有时间轴（通常来自 Apple Speech），每句截出来交
text_model 重听，只换文字、不动时间和说话人。

瞬时故障（连不上、超时、HTTP 5xx）自动重试 2 次（等 2 秒、5 秒，取消可打断）；HTTP 4xx、
响应形状不对不重试。HTTP 经 `pybase.wgnet`（标准库实现，走 wgtund 的线路规则），每个
请求带 `X-Xxyi-Client: pybase-asr`（omlxgw 网关日志里的客户端名）。

断点：`checkpoint_dir` 必须由调用方给，本模块绝不往源文件旁边写东西。`transcribe_remote`
每段成功后写 `seg-NNNN.json`，`refine_cues` 每 10 句写一次 `refine.json`；指纹（源文件、
模型、语言、切段与长静音参数）对得上才复用。全部成功后只删本模块自己写的文件（含转出的
WAV），目录删空了才删目录，调用方放在里面的其他文件不碰。一个 `checkpoint_dir` 同一时间
只给一次识别用。

时间一律整数毫秒；结果类型见 `pybase.asr.types`。
"""
from __future__ import annotations

import array
import hashlib
import http.client
import io
import json
import math
import operator
import os
import re
import sys
import threading
import unicodedata
import uuid
import wave
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Callable, Iterable

from .. import wgnet
from ..media import MediaCancelled, MediaError, MediaTimeoutError, extract_audio
from .models import AsrError, TranscriptionCancelled
from .types import Cue, TranscriptionResult, as_cues

ProgressCallback = Callable[[str, "float | None"], None]

# 每个请求都带的头：omlxgw 网关按它记是哪个客户端发的。
_CLIENT_HEADERS = {"X-Xxyi-Client": "pybase-asr"}

# 纯 Python 路认的 WAV：16 kHz、单声道、16 位 PCM。
_RATE = 16_000
_SAMPLE_WIDTH = 2

# 每句截给 text_model 时的余量上限：timing_model 的时间偏紧，贴着字头字尾截会吃掉辅音的
# 尾巴。但两句紧挨着时还照留 150ms，就会把上一句的最后一个字截进来（字幕开头多出「了相关
# 的问题」这种货），所以实际余量取 min(这个上限, 和相邻句之间空隙的一半)。
_PAD_MS = 150

# HTTP 超时按「这次上传的音频有多长」分两档：整段（10 分钟音频）600 秒是 Whisper 的正常
# 工作量，压短会误杀；单句只有几秒，60 秒等不来就快速失败交给重试（实测一句卡满 600 秒
# 读超时，整部片子的识别全丢了，服务器紧接着又能访问）。
_TIMING_HTTP_TIMEOUT_SECONDS = 600
_TEXT_HTTP_TIMEOUT_SECONDS = 60

# 非 16k 单声道 WAV 输入转一次 WAV 的 ffmpeg 超时（几小时的录像整段解码）。
_CONVERT_TIMEOUT_SECONDS = 1800.0

# 瞬时故障重试：最多再试 _MAX_RETRIES 次，之前分别等 _RETRY_WAITS_SECONDS。等待用
# cancel.wait，取消立刻停。
_MAX_RETRIES = 2
_RETRY_WAITS_SECONDS = (2, 5)

# 长静音：只有超过 3 分钟的空档才由程序跳过（不截、不发请求）。-35 dBFS 比「听得出没声音」
# 宽，把底噪也算静音；180 秒确保正常句间停顿不会被误伤。30ms 一帧算 RMS。
_LONG_SILENCE_NOISE_DB = -35.0
_LONG_SILENCE_MIN_S = 180
_SILENCE_FRAME_MS = 30

# 纯语气词句子（「嗯。」「呃……啊」）没有信息量，整句剥到一个字不剩才丢。
_FILLER_CHARS = frozenset("嗯呃啊哦噢喔诶唉呐")

# refine：连续失败这么多句就判定服务器不行了，整个中止；每完成这么多句落一次盘。
_REFINE_MAX_CONSECUTIVE_FAILURES = 6
_REFINE_CHECKPOINT_EVERY = 10

# 断点文件的格式标识，进指纹：格式变了旧文件自然不复用。
_CHECKPOINT_FORMAT = "pybase.asr.remote/1"
_SEGMENT_CHECKPOINT_RE = re.compile(r"seg-\d{4}\.json(\.tmp)?")
_REFINE_CHECKPOINT_NAME = "refine.json"
_CONVERTED_WAV_NAME = "audio-16k.wav"
_CONVERTED_META_NAME = "audio-16k.json"
_CONVERTED_NAMES = frozenset({
    _CONVERTED_WAV_NAME, _CONVERTED_META_NAME, _CONVERTED_META_NAME + ".tmp",
    "audio-16k.part.wav",  # pybase.media 转码时的临时名，转一半被杀会留下
})

_sumprod = getattr(math, "sumprod", None) or (lambda a, b: sum(map(operator.mul, a, b)))


@dataclass(frozen=True)
class _Segment:
    index: int
    start_ms: int
    end_ms: int


def _plan_segments(duration_ms: int, *, segment_ms: int, overlap_ms: int) -> list[_Segment]:
    """把 [0, duration_ms) 切成相邻重叠 `overlap_ms` 的段，最后一段截到 duration_ms。"""
    if duration_ms <= 0:
        raise ValueError("duration_ms 必须为正")
    segments: list[_Segment] = []
    start = 0
    while start < duration_ms:
        end = min(start + segment_ms, duration_ms)
        segments.append(_Segment(len(segments), start, end))
        if end == duration_ms:
            break
        start = end - overlap_ms
    return segments


# ---------------------------------------------------------------------------
# WAV（纯 Python 路）
# ---------------------------------------------------------------------------


def _wav_frames(path: Path) -> int | None:
    """`path` 是 16 kHz 单声道 16 位 PCM WAV 就返回帧数，否则（不是 WAV、读不了、参数不对）None。"""
    try:
        with wave.open(str(path), "rb") as w:
            if (w.getnchannels(), w.getframerate(), w.getsampwidth(), w.getcomptype()) != (
                    1, _RATE, _SAMPLE_WIDTH, "NONE"):
                return None
            return w.getnframes()
    except (wave.Error, EOFError, OSError):
        return None


def _ms_to_frame(ms: int) -> int:
    return ms * _RATE // 1000


def _frame_to_ms(frame: int) -> int:
    return frame * 1000 // _RATE


def _wav_bytes(path: Path, start_ms: int, end_ms: int) -> bytes:
    """从 16k WAV 里读出 [start_ms, end_ms) 的帧，包成一个完整的小 WAV（内存里）。"""
    with wave.open(str(path), "rb") as w:
        total = w.getnframes()
        first = min(max(0, _ms_to_frame(start_ms)), total)
        last = min(max(first, _ms_to_frame(end_ms)), total)
        w.setpos(first)
        frames = w.readframes(last - first)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as out:
        out.setnchannels(1)
        out.setsampwidth(_SAMPLE_WIDTH)
        out.setframerate(_RATE)
        out.writeframes(frames)
    return buf.getvalue()


def _long_silences(path: Path, *, cancel: threading.Event) -> list[tuple[int, int]]:
    """纯 Python 能量法：30ms 一帧算 RMS，低于 `_LONG_SILENCE_NOISE_DB` dBFS 的帧算静音，
    连续静音 ≥ `_LONG_SILENCE_MIN_S` 秒的返回成 (start_ms, end_ms)；末尾的静音按音频结束收口。"""
    frame_len = _RATE * _SILENCE_FRAME_MS // 1000
    amplitude = 32768.0 * 10 ** (_LONG_SILENCE_NOISE_DB / 20)
    limit = amplitude * amplitude          # 每个样本的均方门槛：sumsq < limit * n 即 RMS 低于门槛
    min_frames = _LONG_SILENCE_MIN_S * _RATE
    silences: list[tuple[int, int]] = []
    run_start: int | None = None
    pos = 0

    def close(end: int) -> None:
        if run_start is not None and end - run_start >= min_frames:
            silences.append((_frame_to_ms(run_start), _frame_to_ms(end)))

    with wave.open(str(path), "rb") as w:
        while True:
            _check_cancel(cancel)
            data = w.readframes(frame_len * 2000)   # 一次读 60 秒
            if not data:
                break
            samples = array.array("h")
            samples.frombytes(data[: len(data) // 2 * 2])
            if sys.byteorder == "big":
                samples.byteswap()
            for offset in range(0, len(samples), frame_len):
                chunk = samples[offset:offset + frame_len]
                if _sumprod(chunk, chunk) < limit * len(chunk):
                    if run_start is None:
                        run_start = pos
                else:
                    close(pos)
                    run_start = None
                pos += len(chunk)
    close(pos)
    return silences


def _converted_wav(
    source: Path, ckpt_dir: Path, *, identity: dict, cancel: threading.Event,
    on_progress: "ProgressCallback | None", ffmpeg: str | None, ffprobe: str | None,
) -> Path:
    """非 16k 单声道 WAV 的输入：ffmpeg 转一次放 `ckpt_dir/audio-16k.wav`；旁边的
    `audio-16k.json` 记源文件指纹，对得上（续跑）就直接用，不再转。"""
    wav = ckpt_dir / _CONVERTED_WAV_NAME
    meta = ckpt_dir / _CONVERTED_META_NAME
    if wav.resolve() == source.resolve():
        raise AsrError(f"源文件和转码输出是同一个路径（{wav}），换一个 checkpoint_dir")
    stamp = {"format": _CHECKPOINT_FORMAT, **identity}
    try:
        if json.loads(meta.read_text(encoding="utf-8")) == stamp and _wav_frames(wav) is not None:
            return wav
    except (OSError, ValueError):
        pass
    meta.unlink(missing_ok=True)
    if on_progress is not None:
        on_progress("converting", None)
    ckpt_dir.mkdir(parents=True, exist_ok=True)
    _media_call(
        extract_audio, "转成 16k WAV", source, wav,
        sample_rate=_RATE, mono=True, ffmpeg=ffmpeg, ffprobe=ffprobe,
        timeout=_CONVERT_TIMEOUT_SECONDS, cancel_event=cancel,
    )
    if _wav_frames(wav) is None:
        raise AsrError(f"ffmpeg 转出的不是 16k 单声道 16 位 WAV：{wav}")
    _write_json_atomic(meta, stamp)
    return wav


def _media_call(fn, where: str, *args, **kwargs):
    """跑一个 pybase.media 函数，把它的异常换成本模块的：取消 → TranscriptionCancelled，
    超时/失败/找不到 ffmpeg → AsrError。"""
    try:
        return fn(*args, **kwargs)
    except MediaCancelled as exc:
        raise TranscriptionCancelled("识别已取消") from exc
    except MediaTimeoutError as exc:
        raise AsrError(f"{where}超时：{exc}") from exc
    except MediaError as exc:
        raise AsrError(f"{where}失败：{exc}") from exc


def _prepare_audio(
    source: Path, ckpt_dir: Path, *, identity: dict, cancel: threading.Event,
    on_progress: "ProgressCallback | None", ffmpeg: str | None, ffprobe: str | None,
) -> tuple[Path, int]:
    """返回 (要读的 16k WAV, 时长毫秒)：源文件本身就是 16k 单声道 WAV 就直接用它。"""
    frames = _wav_frames(source)
    wav = source
    if frames is None:
        wav = _converted_wav(source, ckpt_dir, identity=identity, cancel=cancel,
                             on_progress=on_progress, ffmpeg=ffmpeg, ffprobe=ffprobe)
        frames = _wav_frames(wav)
    duration_ms = _frame_to_ms(frames or 0)
    return wav, duration_ms


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------


def _multipart(fields: dict[str, str], filename: str, data: bytes) -> tuple[bytes, str]:
    boundary = f"pybase-asr-{uuid.uuid4().hex}"
    parts: list[bytes] = []
    for name, value in fields.items():
        parts.append(
            f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"\r\n\r\n{value}\r\n'
            .encode("utf-8")
        )
    parts.append(
        f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\n'
        f"Content-Type: audio/wav\r\n\r\n".encode("utf-8")
    )
    parts.append(data)
    parts.append(f"\r\n--{boundary}--\r\n".encode("utf-8"))
    return b"".join(parts), f"multipart/form-data; boundary={boundary}"


def _post_transcription(
    *,
    base_url: str,
    api_key: str,
    model: str,
    audio: bytes,
    filename: str,
    response_format: str,
    language: str | None,
    timeout: float,
) -> "wgnet.http.Response":
    """发**一次** multipart 转写请求（`audio` 是完整的 WAV 文件内容），原样返回响应（正文已读完）。

    只管发一次：连接错误/超时原样抛出，由 `_post_with_retry` 决定重不重来；状态码和
    响应形状留给调用方校验（只有调用方知道是第几段、该拿到什么形状）。
    """
    fields = {"model": model, "response_format": response_format}
    if language:
        fields["language"] = language
    body, content_type = _multipart(fields, filename, audio)
    headers = {"Content-Type": content_type, **_CLIENT_HEADERS}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    return wgnet.http.post(
        f"{base_url.rstrip('/')}/v1/audio/transcriptions", body, headers, timeout=timeout,
    )


def _post_with_retry(
    *,
    base_url: str,
    api_key: str,
    model: str,
    audio: bytes,
    filename: str,
    response_format: str,
    language: str | None,
    timeout: float,
    cancel: threading.Event,
    where: str,
) -> "wgnet.http.Response":
    """发一次转写请求，瞬时故障（`OSError`——含连接被拒/重置、超时——、HTTP 层协议错误、
    HTTP 5xx）自动重试；HTTP < 500 的响应原样返回给调用方。重试用完抛 `AsrError`，消息里
    说清重试了几次、最后一次为什么失败。"""
    attempts = _MAX_RETRIES + 1
    failure = ""
    for attempt in range(1, attempts + 1):
        _check_cancel(cancel)
        try:
            response = _post_transcription(
                base_url=base_url, api_key=api_key, model=model, audio=audio,
                filename=filename, response_format=response_format,
                language=language, timeout=timeout,
            )
        except (OSError, http.client.HTTPException) as exc:
            failure = f"{type(exc).__name__}: {exc}"
        except ValueError as exc:
            # URL 不合法这类：再发多少次都是同一个错
            raise AsrError(f"{where}失败：{exc}") from exc
        else:
            if response.status < 500:
                return response
            failure = f"HTTP {response.status} {response.text[:200]}"
        if attempt < attempts:
            wait = _RETRY_WAITS_SECONDS[min(attempt - 1, len(_RETRY_WAITS_SECONDS) - 1)]
            if cancel.wait(wait):
                raise TranscriptionCancelled("识别已取消")
    raise AsrError(f"{where}失败：重试 {attempts - 1} 次（共请求 {attempts} 次）仍失败：{failure}")


def _text_from_response(response) -> str:
    """text_model 的正文：先当 JSON 取 `text` 字段，不是 JSON 才退回原始文本。"""
    body = response.text or ""
    try:
        payload = json.loads(body)
    except ValueError:
        return body.strip()
    if isinstance(payload, dict) and isinstance(payload.get("text"), str):
        return payload["text"].strip()
    return body.strip()


def _parse_timing_response(response, *, model: str, seg_index: int) -> list[dict]:
    """校验并解析 timing_model 的 verbose_json，返回段内相对毫秒的句子列表。"""
    where = f"识别模型 {model} 处理第 {seg_index + 1} 段"
    if response.status != 200:
        raise AsrError(f"{where}失败：HTTP {response.status} {response.text[:200]}")
    try:
        payload = json.loads(response.text)
    except ValueError as exc:
        raise AsrError(f"{where}返回的不是合法 JSON：{exc}") from exc
    raw = payload.get("segments") if isinstance(payload, dict) else None
    if not isinstance(raw, list):
        raise AsrError(f"{where}返回的 verbose_json 里没有 segments 数组")
    sentences: list[dict] = []
    for number, item in enumerate(raw, 1):
        if (
            not isinstance(item, dict)
            or isinstance(item.get("start"), bool)
            or isinstance(item.get("end"), bool)
            or not isinstance(item.get("start"), (int, float))
            or not isinstance(item.get("end"), (int, float))
            or not isinstance(item.get("text"), str)
        ):
            raise AsrError(f"{where}返回的 segments 第 {number} 条缺少合法的 start/end/text")
        sentences.append({
            "start_ms": max(0, round(float(item["start"]) * 1000)),
            "end_ms": max(0, round(float(item["end"]) * 1000)),
            "text": item["text"],
        })
    return sentences


# ---------------------------------------------------------------------------
# 纯逻辑
# ---------------------------------------------------------------------------


def _normalize_language(language: str | None) -> str | None:
    """语言 → 发给服务器的两三字母小写主语言码：`zh-TW`/`zh_Hans`/`zh-CN` → `zh`，`EN` → `en`。

    None/空串 → None（请求里不带 language，交给模型自己判断；本库不内置语种默认值）；
    主语言码不是两三个字母抛 `ValueError`。
    """
    text = str(language or "").strip()
    if not text:
        return None
    primary = re.split(r"[-_]", text, maxsplit=1)[0].lower()
    if not re.fullmatch(r"[a-z]{2,3}", primary):
        raise ValueError(f"language 不是合法的语言码：{language!r}")
    return primary


def _check_cancel(cancel: threading.Event) -> None:
    if cancel.is_set():
        raise TranscriptionCancelled("识别已取消")


def _voiced_intervals(duration_ms: int, silences: list[tuple[int, int]]) -> list[tuple[int, int]]:
    """[0, duration_ms) 挖掉所有长静音后剩下的有声区间（升序，可能为空）。"""
    intervals: list[tuple[int, int]] = []
    cursor = 0
    for silence_start, silence_end in sorted(silences):
        silence_start = min(max(silence_start, 0), duration_ms)
        silence_end = min(max(silence_end, 0), duration_ms)
        if silence_end <= silence_start or silence_end <= cursor:
            continue
        if silence_start > cursor:
            intervals.append((cursor, silence_start))
        cursor = silence_end
    if cursor < duration_ms:
        intervals.append((cursor, duration_ms))
    return intervals


def _has_content(text: str) -> bool:
    """去掉标点/空白/语气词后还剩字就算有内容。"""
    return any(
        ch not in _FILLER_CHARS and unicodedata.category(ch)[0] not in ("P", "S", "Z", "C")
        for ch in text
    )


def _strip_for_compare(text: str) -> str:
    return "".join(ch for ch in text if unicodedata.category(ch)[0] not in ("P", "S", "Z", "C"))


def _is_degenerate(text: str) -> bool:
    """text_model 对着噪音会一直输出同一个字（「嗯嗯嗯嗯嗯嗯」）：一个字占了六成以上且不短。"""
    chars = [ch for ch in text if unicodedata.category(ch)[0] not in ("P", "S", "Z", "C")]
    if len(chars) < 6:
        return False
    top = max(chars.count(ch) for ch in set(chars))
    return top / len(chars) > 0.6


def _sentence_pads(
    start_ms: int, end_ms: int, prev_end_ms: int | None, next_start_ms: int | None,
) -> tuple[int, int]:
    """这一句截音频时前后各留多少毫秒：上限 `_PAD_MS`，且不伸过和相邻句空隙的一半；
    没有上一句/下一句（传 None）的那一侧只受上限约束。"""
    gap_before = _PAD_MS * 2 if prev_end_ms is None else max(0, start_ms - prev_end_ms)
    gap_after = _PAD_MS * 2 if next_start_ms is None else max(0, next_start_ms - end_ms)
    return min(_PAD_MS, gap_before // 2), min(_PAD_MS, gap_after // 2)


def drop_contained_duplicates(cues: Iterable[Cue]) -> list[Cue]:
    """清掉「时间被前一句完全包住、文字（去标点）也是前一句一部分」的重复句。

    timing_model 偶发同一句报两遍（整句 + 只有一半时间的碎片）。两个条件同时成立才丢：
    只比时间会误杀同一时间点接着说的两句短话，只比文字会误杀重复强调的同一句话。
    输入要按 (start_ms, -end_ms) 排好，只和紧挨着的前一句比。
    """
    kept: list[Cue] = []
    for cue in cues:
        if kept:
            prev = kept[-1]
            if (
                prev.start_ms <= cue.start_ms and cue.end_ms <= prev.end_ms
                and (text := _strip_for_compare(cue.text))
                and text in _strip_for_compare(prev.text)
            ):
                continue
        kept.append(cue)
    return kept


# ---------------------------------------------------------------------------
# 断点
# ---------------------------------------------------------------------------


def _write_json_atomic(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, ensure_ascii=False)
    os.replace(tmp, path)


def _source_identity(source: Path) -> dict:
    st = source.stat()
    return {"source": str(source.resolve()), "size_bytes": st.st_size, "mtime_ns": st.st_mtime_ns}


def _load_segment_checkpoint(path: Path, *, fingerprint: dict) -> list[Cue] | None:
    """文件不存在/坏/指纹对不上都返回 None（当没有）。"""
    try:
        with open(path, encoding="utf-8") as fh:
            payload = json.load(fh)
        if not isinstance(payload, dict) or payload.get("fingerprint") != fingerprint:
            return None
        raw = payload.get("cues")
        if not isinstance(raw, list):
            return None
        return [Cue.from_dict(item) for item in raw]
    except (OSError, ValueError):
        return None


def _cleanup_checkpoints(directory: Path, names: Callable[[str], bool], *, keep: Path) -> None:
    """只删本模块写的文件；目录因此空了才删目录，调用方放的其他文件不碰。
    `keep`（源文件）就算碰巧叫这些名字也不删。"""
    try:
        entries = list(directory.iterdir())
    except OSError:
        return
    keep = keep.resolve()
    for entry in entries:
        if entry.is_file() and names(entry.name) and entry.resolve() != keep:
            entry.unlink(missing_ok=True)
    try:
        directory.rmdir()
    except OSError:
        pass  # 不空（有调用方的文件）或已不存在


def _require_checkpoint_dir(checkpoint_dir) -> Path:
    if checkpoint_dir is None or not str(checkpoint_dir).strip():
        raise ValueError("checkpoint_dir 必须给（断点目录由调用方定，本模块不往源文件旁边写东西）")
    return Path(checkpoint_dir)


def _check_server_args(base_url: str, models: dict[str, str], concurrency: int) -> str:
    base_url = str(base_url or "").strip()
    if not base_url:
        raise ValueError("base_url 为空：远程识别需要服务器地址")
    for name, value in models.items():
        if not str(value or "").strip():
            raise ValueError(f"{name} 不能为空")
    if not isinstance(concurrency, int) or isinstance(concurrency, bool) or concurrency < 1:
        raise ValueError("concurrency 至少为 1")
    return base_url


def _transcribe_sentence(
    *,
    wav: Path,
    clip_start_ms: int,
    clip_end_ms: int,
    filename: str,
    where: str,
    base_url: str,
    api_key: str,
    text_model: str,
    language: str | None,
    cancel: threading.Event,
) -> str | None:
    """一句：从 16k WAV 截 [clip_start_ms, clip_end_ms)（已含余量）发 text_model，返回文字；
    空/纯语气词返回 None。HTTP 非 200 才算失败（`AsrError`）。"""
    _check_cancel(cancel)
    response = _post_with_retry(
        base_url=base_url, api_key=api_key, model=text_model,
        audio=_wav_bytes(wav, clip_start_ms, clip_end_ms), filename=filename,
        response_format="json", language=language,
        timeout=_TEXT_HTTP_TIMEOUT_SECONDS, cancel=cancel, where=where,
    )
    if response.status != 200:
        raise AsrError(f"{where}失败：HTTP {response.status} {response.text[:200]}")
    text = _text_from_response(response)
    if not text or not _has_content(text):
        return None  # 这句没听出东西 / 纯语气词：丢掉这一句，不让整次识别失败
    return text


# ---------------------------------------------------------------------------
# 对外
# ---------------------------------------------------------------------------


def transcribe_remote(
    path: str | os.PathLike,
    *,
    base_url: str,
    api_key: str,
    timing_model: str,
    text_model: str,
    language: str | None = None,
    on_progress: ProgressCallback | None = None,
    cancel_event: threading.Event | None = None,
    concurrency: int = 2,
    segment_ms: int = 600_000,
    overlap_ms: int = 5_000,
    checkpoint_dir: str | os.PathLike,
    ffmpeg: str | None = None,
    ffprobe: str | None = None,
) -> TranscriptionResult:
    """对一个音频/视频文件跑「timing_model 定时间、text_model 出文字」的远程识别。

    返回 `TranscriptionResult`：engine 为 `omlx:<timing_model>+<text_model>`，cue 时间
    是相对源文件开头的绝对毫秒、按时间排序，`speaker` 为空；长句原样保留不拆。

    `path` 是 16k 单声道 16 位 WAV 时全程不调 ffmpeg；否则先用 ffmpeg 转一次放
    `checkpoint_dir/audio-16k.wav`（`ffmpeg`/`ffprobe` 只用在这一步，不给就在 PATH 里找）。
    `language`：`zh`/`zh-CN`/`en-US` 这类，发给服务器前取主语言码（`zh`）；None 就不带，
    交给模型判断。`concurrency`：同时在飞的句子请求数（整段请求是串行的）。
    `on_progress(stage, fraction)`：转码前回调一次 `("converting", None)`（只有要转时）；
    每段完成（含断点复用）回调一次 `("transcribing", 0..1)`。

    `checkpoint_dir` 必须给：每段成功后写 `seg-NNNN.json`，下次指纹一致直接复用、不发
    请求；全部成功后删掉本模块写的文件（目录删空才删目录）；失败/取消保留，留给下次续传。

    取消抛 `TranscriptionCancelled`（包括在重试等待里时）。重试完仍失败、HTTP 4xx、响应
    形状不对、转码失败一律 `AsrError`，消息带模型名和段号。并发中某一句失败时会把
    `cancel_event` 置位让其余句子停下，所以失败后再发起识别请换新的事件对象。
    """
    source = Path(path)
    if not source.is_file():
        raise FileNotFoundError(f"音频文件不存在：{source}")
    base_url = _check_server_args(
        base_url, {"timing_model": timing_model, "text_model": text_model}, concurrency)
    if segment_ms <= 0 or overlap_ms < 0 or overlap_ms >= segment_ms:
        raise ValueError("要求 segment_ms > 0、0 <= overlap_ms < segment_ms")
    ckpt_dir = _require_checkpoint_dir(checkpoint_dir)

    cancel = cancel_event if cancel_event is not None else threading.Event()
    engine = f"omlx:{timing_model}+{text_model}"
    speech_language = _normalize_language(language)

    def _percent(done: int, total: int) -> None:
        if on_progress is not None:
            on_progress("transcribing", done / total if total else 1.0)

    identity = _source_identity(source)
    wav, duration_ms = _prepare_audio(source, ckpt_dir, identity=identity, cancel=cancel,
                                      on_progress=on_progress, ffmpeg=ffmpeg, ffprobe=ffprobe)

    # 按有声区间切段：段号全局连续；重叠去重的「首尾段不缩」按每个有声区间的首尾段算。
    segments: list[_Segment] = []
    segment_bounds: list[tuple[bool, bool]] = []
    voiced = _voiced_intervals(duration_ms, _long_silences(wav, cancel=cancel)) if duration_ms else []
    for interval_start, interval_end in voiced:
        planned = _plan_segments(interval_end - interval_start,
                                 segment_ms=segment_ms, overlap_ms=overlap_ms)
        for position, seg in enumerate(planned):
            segments.append(_Segment(len(segments), interval_start + seg.start_ms,
                                     interval_start + seg.end_ms))
            segment_bounds.append((position == 0, position == len(planned) - 1))
    halves = overlap_ms // 2

    def _fingerprint(segment: _Segment) -> dict:
        return {
            "format": _CHECKPOINT_FORMAT, **identity, "duration_ms": duration_ms,
            "segment_start_ms": segment.start_ms, "segment_end_ms": segment.end_ms,
            "timing_model": timing_model, "text_model": text_model,
            "language": speech_language, "segment_ms": segment_ms, "overlap_ms": overlap_ms,
            "long_silence_noise_db": _LONG_SILENCE_NOISE_DB,
            "long_silence_min_s": _LONG_SILENCE_MIN_S,
            "silence_frame_ms": _SILENCE_FRAME_MS,
        }

    cues: list[Cue] = []
    if not segments:
        _percent(1, 1)  # 空音频或整段都是长静音：空结果
    # 一个线程池贯穿所有段：段内各句并发发 text_model，段与段之间接力。
    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        for position, segment in enumerate(segments):
            _check_cancel(cancel)
            first, last = segment_bounds[position]

            ckpt_file = ckpt_dir / f"seg-{segment.index:04d}.json"
            cached = _load_segment_checkpoint(ckpt_file, fingerprint=_fingerprint(segment))
            if cached is not None:
                cues.extend(cached)
                _percent(position + 1, len(segments))
                continue

            sentences = _parse_timing_response(
                _post_with_retry(
                    base_url=base_url, api_key=api_key, model=timing_model,
                    audio=_wav_bytes(wav, segment.start_ms, segment.end_ms),
                    filename=f"seg-{segment.index:04d}.wav", response_format="verbose_json",
                    language=speech_language, timeout=_TIMING_HTTP_TIMEOUT_SECONDS,
                    cancel=cancel, where=f"识别模型 {timing_model} 处理第 {segment.index + 1} 段",
                ),
                model=timing_model, seg_index=segment.index,
            )

            # 重叠去重：本段只负责中点落在 [start+重叠/2, end−重叠/2) 的句子。
            own_lo = segment.start_ms + (0 if first else halves)
            own_hi = segment.end_ms - (0 if last else halves)
            futures = []
            for number, sentence in enumerate(sentences):
                start_ms = segment.start_ms + sentence["start_ms"]
                end_ms = segment.start_ms + sentence["end_ms"]
                if not own_lo <= (start_ms + end_ms) // 2 < own_hi or start_ms >= end_ms:
                    continue
                pad_start, pad_end = _sentence_pads(
                    sentence["start_ms"], sentence["end_ms"],
                    sentences[number - 1]["end_ms"] if number > 0 else None,
                    sentences[number + 1]["start_ms"] if number + 1 < len(sentences) else None,
                )
                # 小样不出这一段：timing_model 听到的就是这一段，余量也只在段内取。
                futures.append((start_ms, end_ms, pool.submit(
                    _transcribe_sentence, wav=wav,
                    clip_start_ms=max(segment.start_ms, start_ms - pad_start),
                    clip_end_ms=min(segment.end_ms, end_ms + pad_end),
                    filename=f"seg-{segment.index:04d}-s{number:04d}.wav",
                    where=f"文本模型 {text_model} 处理第 {segment.index + 1} 段第 {number + 1} 句",
                    base_url=base_url, api_key=api_key, text_model=text_model,
                    language=speech_language, cancel=cancel,
                )))
            seg_cues: list[Cue] = []
            for start_ms, end_ms, future in futures:
                try:
                    text = future.result()
                except TranscriptionCancelled:
                    raise
                except BaseException:
                    # 某一句失败：置位 cancel，让排在后面还没开始的句子直接退出。
                    cancel.set()
                    raise
                if text is not None:
                    seg_cues.append(Cue(start_ms, end_ms, text))
            cues.extend(seg_cues)
            # 这一段彻底过了才落盘：失败的段没有断点文件，下次整段重做。
            _write_json_atomic(ckpt_file, {
                "fingerprint": _fingerprint(segment),
                "cues": [cue.to_dict() for cue in seg_cues],
            })
            _percent(position + 1, len(segments))

    # 同一个 start 上让长的那句排前面，碎片才会「被前一句包住」；清完再正常排序。
    cues.sort(key=lambda cue: (cue.start_ms, -cue.end_ms))
    cues = drop_contained_duplicates(cues)
    cues.sort(key=lambda cue: (cue.start_ms, cue.end_ms))
    # 结果到手才清断点：中途任何失败/取消都到不了这里。
    _cleanup_checkpoints(ckpt_dir, lambda name: bool(_SEGMENT_CHECKPOINT_RE.fullmatch(name))
                         or name in _CONVERTED_NAMES, keep=source)
    return TranscriptionResult(tuple(cues), engine)


def _refine_fingerprint(source_identity: dict, cues: list[Cue], text_model: str,
                        language: str | None) -> str:
    # 只认时间轴不认文字：校正写回后文字变了，但切句没变，断点仍然有效。
    body = "\n".join(f"{c.start_ms}|{c.end_ms}" for c in cues)
    head = json.dumps({"format": _CHECKPOINT_FORMAT, **source_identity,
                       "text_model": text_model, "language": language}, sort_keys=True)
    return hashlib.sha1(f"{head}\n{body}".encode("utf-8")).hexdigest()


def _load_refine_checkpoint(path: Path, fingerprint: str) -> dict[int, str | None]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        if data.get("fingerprint") != fingerprint:
            return {}
        return {int(k): v for k, v in data["done"].items() if v is None or isinstance(v, str)}
    except (OSError, ValueError, KeyError, AttributeError, TypeError):
        return {}


def refine_cues(
    cues: Iterable[Cue],
    path: str | os.PathLike,
    *,
    base_url: str,
    api_key: str,
    text_model: str,
    language: str | None = None,
    skip: Iterable[int] = (),
    on_progress: ProgressCallback | None = None,
    cancel_event: threading.Event | None = None,
    concurrency: int = 2,
    checkpoint_dir: str | os.PathLike,
    ffmpeg: str | None = None,
    ffprobe: str | None = None,
) -> tuple[list[Cue], dict]:
    """沿用 `cues` 的时间轴，每一句的文字交给 `text_model` 重听一遍：只换文字，
    `start_ms`/`end_ms`/`speaker` 原样保留，条数和顺序不变。

    每句按原起止时间（余量规则同 transcribe_remote，夹在 [0, 时长] 内）从 `path` 截小样
    发给模型，**只发音频、不带原文字**，所以原来的错字不影响这一步。`path` 不是 16k 单声道
    WAV 时同 transcribe_remote 先转一次。

    保留原文字的情况：下标在 `skip` 里（如用户改过的）；模型没听出内容；模型输出是对着噪音
    的重复字；去掉标点后和原文一样；这一句单独请求失败（重试后仍失败）。连续
    `_REFINE_MAX_CONSECUTIVE_FAILURES` 句失败就中止、抛 `AsrError`（服务器多半没开）；
    每一句都失败也抛 `AsrError`，免得悄悄返回一份什么都没校正的结果。取消抛
    `TranscriptionCancelled`。`on_progress` 每句回调一次 `("refining", 0..1)`。

    `checkpoint_dir` 必须给：每完成 10 句把已校正的结果写进 `refine.json`（失败的句子不记，
    下次重试），指纹（源文件、时间轴、模型、语言）一致就复用；没有失败才删掉本模块写的文件。

    返回 `(新 cues, 统计)`，统计键：changed/kept/failed/skipped/resumed。
    """
    cues = as_cues(cues)
    source = Path(path)
    if not source.is_file():
        raise FileNotFoundError(f"音频文件不存在：{source}")
    base_url = _check_server_args(base_url, {"text_model": text_model}, concurrency)
    ckpt_dir = _require_checkpoint_dir(checkpoint_dir)
    checkpoint = ckpt_dir / _REFINE_CHECKPOINT_NAME
    skip = frozenset(skip)

    cancel = cancel_event if cancel_event is not None else threading.Event()
    speech_language = _normalize_language(language)
    identity = _source_identity(source)
    total = len(cues)
    lock = threading.Lock()
    done = [0]
    stats = {"changed": 0, "kept": 0, "failed": 0, "skipped": 0, "resumed": 0}
    result: list[Cue] = list(cues)
    fingerprint = _refine_fingerprint(identity, cues, text_model, speech_language)
    finished = _load_refine_checkpoint(checkpoint, fingerprint)  # 下标 → 新文字或 None(保留)
    unsaved = [0]
    first_error: list[BaseException] = []
    streak = [0]
    aborted = threading.Event()

    needs_audio = any(i not in skip and i not in finished for i in range(total))
    wav, duration_ms = (
        _prepare_audio(source, ckpt_dir, identity=identity, cancel=cancel,
                       on_progress=on_progress, ffmpeg=ffmpeg, ffprobe=ffprobe)
        if needs_audio else (source, 0)
    )

    def save() -> None:
        _write_json_atomic(checkpoint, {"fingerprint": fingerprint,
                                        "done": {str(k): v for k, v in finished.items()}})

    def tick() -> None:
        with lock:
            done[0] += 1
            fraction = done[0] / total if total else 1.0
        if on_progress is not None:
            on_progress("refining", fraction)

    def work(i: int) -> None:
        cue = cues[i]
        if aborted.is_set():
            return
        if i in skip:
            with lock:
                stats["skipped"] += 1
            tick()
            return
        if i in finished:
            with lock:
                if finished[i] is not None:
                    result[i] = replace(cue, text=finished[i])
                    stats["changed"] += 1
                else:
                    stats["kept"] += 1
                stats["resumed"] += 1
            tick()
            return
        _check_cancel(cancel)
        pad_start, pad_end = _sentence_pads(
            cue.start_ms, cue.end_ms,
            cues[i - 1].end_ms if i > 0 else None,
            cues[i + 1].start_ms if i + 1 < total else None,
        )
        try:
            fresh = None
            if cue.start_ms < min(cue.end_ms, duration_ms):
                fresh = _transcribe_sentence(
                    wav=wav, clip_start_ms=max(0, cue.start_ms - pad_start),
                    clip_end_ms=min(duration_ms, cue.end_ms + pad_end),
                    filename=f"cue-{i:05d}.wav", where=f"文本模型 {text_model} 校正第 {i + 1} 句",
                    base_url=base_url, api_key=api_key, text_model=text_model,
                    language=speech_language, cancel=cancel,
                )
        except TranscriptionCancelled:
            raise
        except Exception as exc:  # noqa: BLE001  单句失败保留原文
            with lock:
                stats["failed"] += 1
                streak[0] += 1
                if not first_error:
                    first_error.append(exc)
                if streak[0] >= _REFINE_MAX_CONSECUTIVE_FAILURES:
                    aborted.set()
            tick()
            return
        with lock:
            streak[0] = 0
            if (fresh is None or _is_degenerate(fresh)
                    or _strip_for_compare(fresh) == _strip_for_compare(cue.text)):
                stats["kept"] += 1
                finished[i] = None
            else:
                result[i] = replace(cue, text=fresh)
                stats["changed"] += 1
                finished[i] = fresh
            unsaved[0] += 1
            if unsaved[0] >= _REFINE_CHECKPOINT_EVERY:
                unsaved[0] = 0
                save()
        tick()

    try:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(work, i) for i in range(total)]
            try:
                for future in futures:
                    future.result()
            except BaseException:
                # 池子关闭会等排队的任务跑完：先通知其余句子停下。
                cancel.set()
                raise
    finally:
        if finished:
            with lock:
                save()

    if aborted.is_set():
        raise AsrError(
            f"连续 {_REFINE_MAX_CONSECUTIVE_FAILURES} 句校正失败，已中止（服务器可能没开或"
            f"已掉线）：{first_error[0]}")
    attempted = total - stats["skipped"]
    if attempted and stats["failed"] == attempted:
        raise AsrError(f"每一句都校正失败，服务器可能没开：{first_error[0]}")
    if stats["failed"] == 0:
        _cleanup_checkpoints(ckpt_dir, lambda name: name in (
            _REFINE_CHECKPOINT_NAME, _REFINE_CHECKPOINT_NAME + ".tmp") or name in _CONVERTED_NAMES,
            keep=source)
    return result, stats
