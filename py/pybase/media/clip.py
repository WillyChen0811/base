"""按时间段截音频/截图，subs2srs、mpvacious、asbplayer 做卡片的标准做法。

时间单位统一整数毫秒。输出格式一律按 `out` 的扩展名交给 ffmpeg 猜。
"""
from __future__ import annotations

import os
import threading
from pathlib import Path

from ._proc import resolve_tool, run_to_file


def _seconds(ms: int) -> str:
    return f"{ms / 1000:.3f}"


def extract_audio(
    src: str | os.PathLike,
    out: str | os.PathLike,
    *,
    start_ms: int | None = None,
    end_ms: int | None = None,
    pad_start_ms: int = 0,
    pad_end_ms: int = 0,
    sample_rate: int | None = None,
    mono: bool = False,
    ffmpeg: str | None = None,
    ffprobe: str | None = None,
    timeout: float = 60,
    cancel_event: "threading.Event | None" = None,
) -> None:
    """截一段音频（一句字幕的时间段 + 前后 padding）。

    `start_ms`/`end_ms` 都省略表示整段转码——识别用的 16k 单声道 wav 也走
    这条路径。`pad_start_ms`/`pad_end_ms` 只在给了 `start_ms`/`end_ms` 时
    生效，会被截到 `[0, 媒体时长]` 内：越界的 padding 不报错，安静夹紧。
    `sample_rate`/`mono` 分别对应 `-ar`/`-ac 1`，识别场景常配 16000 + True。
    """
    if (start_ms is None) != (end_ms is None):
        raise ValueError("start_ms 和 end_ms 必须同时给出或同时省略")
    if start_ms is not None and (start_ms < 0 or end_ms < start_ms):
        raise ValueError("start_ms/end_ms 不合法")
    if pad_start_ms < 0 or pad_end_ms < 0:
        raise ValueError("pad_start_ms/pad_end_ms 不能为负")

    tool = resolve_tool(ffmpeg, "ffmpeg")
    args = [tool, "-v", "error", "-y"]

    clip_ms = None
    if start_ms is not None:
        from .probe import probe  # 延迟到用得上时才探测时长，避免全长转码多一次子进程

        duration_ms = probe(
            src, ffprobe=ffprobe, timeout=timeout, cancel_event=cancel_event
        )["duration_ms"]
        actual_start_ms = max(0, start_ms - pad_start_ms)
        actual_end_ms = end_ms + pad_end_ms
        if duration_ms is not None:
            actual_end_ms = min(duration_ms, actual_end_ms)
        clip_ms = max(0, actual_end_ms - actual_start_ms)
        # -ss 放在 -i 前做快速 seek（跳过前面不用解码的部分，比放在 -i 后全程
        # 解码快得多），音频没有关键帧对齐的顾虑，精度足够。
        args += ["-ss", _seconds(actual_start_ms)]

    args += ["-i", str(src)]

    if clip_ms is not None:
        args += ["-t", _seconds(clip_ms)]
    if mono:
        args += ["-ac", "1"]
    if sample_rate:
        args += ["-ar", str(sample_rate)]
    args += ["-vn"]

    def cmd_for(tmp: Path) -> list[str]:
        return [*args, str(tmp)]

    run_to_file(cmd_for, out, timeout=timeout, cancel_event=cancel_event)


def snapshot(
    src: str | os.PathLike,
    out: str | os.PathLike,
    *,
    at_ms: int,
    ffmpeg: str | None = None,
    max_width: int | None = None,
    timeout: float = 30,
    cancel_event: "threading.Event | None" = None,
) -> None:
    """在 `at_ms` 处截一帧图，输出格式按 `out` 扩展名交给 ffmpeg 猜
    （jpg/png/webp 等）。`max_width` 给了就等比缩放到不超过它（不放大）。
    """
    if at_ms < 0:
        raise ValueError("at_ms 不能为负")
    tool = resolve_tool(ffmpeg, "ffmpeg")
    # -ss 放在 -i 前做快速 seek，见 extract_audio 同款注释；截图场景更明显：
    # 长视频整段解码到目标帧会慢得多。
    args = [
        tool, "-v", "error", "-y",
        "-ss", _seconds(at_ms), "-i", str(src), "-frames:v", "1",
    ]
    if max_width:
        args += ["-vf", f"scale='min({max_width},iw)':-2"]

    def cmd_for(tmp: Path) -> list[str]:
        return [*args, str(tmp)]

    run_to_file(cmd_for, out, timeout=timeout, cancel_event=cancel_event)
