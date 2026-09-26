""""能不能直接给网页 `<video>` 播"的判断（`playback_plan`）与转封装（`remux`）。

Chromium 系播放器（Chrome/Electron）支持 mp4/webm 容器，H.264/VP8/VP9/AV1
视频编码，AAC/Opus/Vorbis/MP3/FLAC 音频编码；mkv 部分支持（seek/时长不可靠，
不建议直接播）；HEVC 取决于构建与硬件，Electron 默认不开。这些"支持什么"
因播放端（Chrome/Electron/Safari）而异，本模块**不写死任何默认集合**——
`playback_plan` 要求调用方显式传入。

判断结果分三档：
- `"direct"`：容器和编码播放端都认，原文件直接喂给 `<video>`。
- `"remux"`：编码播放端都认，但容器不认，或者首条可播的音/视频轨不是文件
  里的第一条——只需要 `-c copy` 转封装（快），不用重新编码。
- `"transcode"`：找不到播放端认识的视频/音频编码，必须重新编码（本模块
  暂不提供，太慢、参数太多，等真实需求再做）。
"""
from __future__ import annotations

import os
import threading
from pathlib import Path

from ._proc import resolve_tool, run_to_file

# mp4 系 muxer 才认 -movflags；其余容器传这个选项 ffmpeg 会直接报错退出。
_FASTSTART_EXTS = {".mp4", ".m4v", ".mov", ".m4a"}

# mkv 和 webm 在 ffprobe 里都报 "matroska,webm"。WebM 规范只允许这些编码，
# 编码全在里面才按 webm 算，否则就是 mkv——不能因为调用方写了 webm 就放行 h264 的 mkv
_WEBM_CODECS = {"vp8", "vp9", "av1", "vorbis", "opus"}


def playback_plan(
    probe_result: dict,
    *,
    video_codecs: "set[str] | frozenset[str]",
    audio_codecs: "set[str] | frozenset[str]",
    containers: "set[str] | frozenset[str]",
) -> dict:
    """给 `probe()` 的结果配上播放建议。

    `probe_result` 必须是 `pybase.media.probe` 的返回值（要有 `streams`
    和 `container` 两个字段；旧代码如果自己拼 dict，记得把 `probe()` 升级
    到带 `container` 字段的版本）。

    `video_codecs`/`audio_codecs`/`containers` 是播放端支持的编码/容器集合
    （大小写不敏感），由调用方给，不同播放端不一样。Chromium 常见取值示例：

        video_codecs={"h264", "vp8", "vp9", "av1"}
        audio_codecs={"aac", "opus", "vorbis", "mp3", "flac"}
        containers={"mp4", "webm"}

    封面图（`attached_pic`）不算视频流。ffprobe 分不清 mkv/webm，这里按 WebM
    规范的编码白名单区分：编码全是 VP8/VP9/AV1/Vorbis/Opus 才算 webm。

    多条音轨/视频轨时的选轨规则：每种类型（视频/音频）取**第一条编码在
    支持集合里的流**，其 `index` 分别放进返回值的 `video_stream`/
    `audio_stream`（没有该类型的流，或都不支持，给 `None`）。如果选中的
    流不是文件里那一类型的第一条（比如第 0 条视频轨是 HEVC，第 1 条才是
    H.264），即便编码本身能播，浏览器默认也只会挑第一条，所以仍然判
    `"remux"`，靠 `-map` 把不要的轨去掉。

    返回 `{"action": "direct"|"remux"|"transcode", "reasons": [...],
    "video_stream": idx|None, "audio_stream": idx|None}`。`reasons` 在
    `"direct"` 时是空列表，其余两档至少有一条，说明具体卡在哪。
    """
    video_codecs = {c.lower() for c in video_codecs}
    audio_codecs = {c.lower() for c in audio_codecs}
    containers = {c.lower() for c in containers}

    streams = probe_result.get("streams") or []
    container_tokens = {tok.lower() for tok in (probe_result.get("container") or ())}

    video_streams = [
        s for s in streams if s.get("codec_type") == "video" and not s.get("attached_pic")
    ]
    audio_streams = [s for s in streams if s.get("codec_type") == "audio"]

    video_stream = _first_playable_index(video_streams, video_codecs)
    audio_stream = _first_playable_index(audio_streams, audio_codecs)

    reasons: list[str] = []
    if video_streams and video_stream is None:
        names = _codec_names(video_streams)
        reasons.append(f"视频编码不受支持：{names}")
    if audio_streams and audio_stream is None:
        names = _codec_names(audio_streams)
        reasons.append(f"音频编码不受支持：{names}")

    if reasons:
        return {
            "action": "transcode",
            "reasons": reasons,
            "video_stream": video_stream,
            "audio_stream": audio_stream,
        }

    if {"matroska", "webm"} <= container_tokens:
        chosen = {
            (s.get("codec_name") or "").lower()
            for s in streams
            if s.get("index") in (video_stream, audio_stream)
        }
        container_tokens = {"webm"} if chosen <= _WEBM_CODECS else {"matroska"}
    if not (container_tokens & containers):
        reasons.append(
            f"容器不受支持：{', '.join(sorted(container_tokens)) or '未知'}"
        )
    if video_streams and video_stream != video_streams[0]["index"]:
        reasons.append("首条视频轨编码不受支持，需改用后面可播的视频轨（丢弃其余视频轨）")
    if audio_streams and audio_stream != audio_streams[0]["index"]:
        reasons.append("首条音轨编码不受支持，需改用后面可播的音轨（丢弃其余音轨）")

    action = "remux" if reasons else "direct"
    return {
        "action": action,
        "reasons": reasons,
        "video_stream": video_stream,
        "audio_stream": audio_stream,
    }


def _first_playable_index(streams: list[dict], codecs: set) -> "int | None":
    for s in streams:
        name = (s.get("codec_name") or "").lower()
        if name in codecs:
            return s.get("index")
    return None


def _codec_names(streams: list[dict]) -> str:
    names = sorted({s.get("codec_name") for s in streams if s.get("codec_name")})
    return ", ".join(names) if names else "未知"


def remux(
    src: str | os.PathLike,
    out: str | os.PathLike,
    *,
    video_stream: "int | None" = None,
    audio_stream: "int | None" = None,
    ffmpeg: str | None = None,
    timeout: float = 60,
    cancel_event: "threading.Event | None" = None,
) -> None:
    """`-c copy` 转封装（不重新编码，快），只映射选中的流，丢掉字幕轨
    （网页播放器走外挂字幕，容器里带字幕反而多余）。

    `video_stream`/`audio_stream` 一般直接用 `playback_plan` 返回的同名
    字段：某个是 `None` 就不映射那一类型（比如纯音频文件不给
    `video_stream`）。两个都是 `None` 时不加 `-map`，退化成 ffmpeg 默认
    行为（不推荐，正常应该先跑 `playback_plan` 拿到明确的流下标）。

    输出格式按 `out` 扩展名交给 ffmpeg 猜，和 `clip.py` 一致；只有落在
    `.mp4`/`.m4v`/`.mov`/`.m4a` 时才会加 `-movflags +faststart`——这是
    mov/mp4 muxer 私有的选项，塞给别的容器 ffmpeg 会直接报错退出。
    """
    tool = resolve_tool(ffmpeg, "ffmpeg")
    args = [tool, "-v", "error", "-y", "-i", str(src)]
    if video_stream is not None:
        args += ["-map", f"0:{video_stream}"]
    if audio_stream is not None:
        args += ["-map", f"0:{audio_stream}"]
    args += ["-c", "copy"]
    if Path(out).suffix.lower() in _FASTSTART_EXTS:
        args += ["-movflags", "+faststart"]

    def cmd_for(tmp: Path) -> list[str]:
        return [*args, str(tmp)]

    run_to_file(cmd_for, out, timeout=timeout, cancel_event=cancel_event)
