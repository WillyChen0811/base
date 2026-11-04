"""定点抽帧：给一串时间点，每个点用 `ffmpeg -ss` 抽一张 jpg，不扫整片。

给"模型点名要看哪一秒的画面"这类场景用：几小时的录像只抽那几张，
每张一个子进程、`-ss` 放在 `-i` 前做快速 seek（见 clip.snapshot 的注释）。
时间单位整数毫秒；文件名固定 `<t_ms>.jpg`，调用方据此按时间点找回图。
"""
from __future__ import annotations

import os
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

from ._proc import MediaError, discard, resolve_tool, run, tmp_path_for
from .clip import _seconds
from .probe import probe


@dataclass(frozen=True)
class FrameFile:
    """一个时间点的抽帧结果。`skipped` 为 None 表示抽到了、图在 `path`；
    否则 `path` 为 None，`skipped` 是没抽的原因（超出时长、那一点没有画面）。"""

    t_ms: int
    path: Path | None
    skipped: str | None = None


def frames_at(
    video: str | os.PathLike,
    times_ms: Iterable[int],
    out_dir: str | os.PathLike,
    *,
    width: int | None = 1280,
    ffmpeg: str | None = None,
    ffprobe: str | None = None,
    timeout: float = 30,
    cancel_event: "threading.Event | None" = None,
) -> list[FrameFile]:
    """在 `times_ms` 的每个时间点抽一帧，写成 `out_dir/<t_ms>.jpg`。

    返回与去重后的时间点一一对应（保持调用方给的顺序）的 `FrameFile` 列表：
    抽不到的点不报错，`path=None`、`skipped` 写明原因——
    - 探测得到时长时，`t_ms >= duration_ms` 直接跳过，不起 ffmpeg；
    - 探测不到时长（浏览器 MediaRecorder 录的 webm 常不写时长）或在时长内但
      ffmpeg 正常退出却没出图（末尾几十毫秒没有帧），按"那一点没有画面"跳过。
    ffmpeg 返回非 0（文件坏、不是视频）照常抛 `MediaError`，不当跳过吞掉。

    `width`：等比缩到不超过这个宽度（不放大）；None 保持原尺寸。
    同名文件直接覆盖（先写临时文件再 `replace`，不留半截图）。
    `timeout` 是每个时间点的上限。时间点为负抛 `ValueError`。
    """
    times = list(dict.fromkeys(int(t) for t in times_ms))
    if any(t < 0 for t in times):
        raise ValueError("时间点不能为负")
    if not times:
        return []
    tool = resolve_tool(ffmpeg, "ffmpeg")
    out_root = Path(out_dir)
    out_root.mkdir(parents=True, exist_ok=True)

    duration_ms = probe(
        video, ffprobe=ffprobe, timeout=timeout, cancel_event=cancel_event
    )["duration_ms"]

    results: list[FrameFile] = []
    for t in times:
        if duration_ms is not None and t >= duration_ms:
            results.append(FrameFile(t, None, f"超出时长（{duration_ms} ms）"))
            continue
        out = out_root / f"{t}.jpg"
        tmp = tmp_path_for(out)
        cmd = [
            tool, "-nostdin", "-v", "error", "-y",
            "-ss", _seconds(t), "-i", str(video),
            "-map", "0:v:0", "-frames:v", "1", "-q:v", "2",
        ]
        if width:
            cmd += ["-vf", f"scale='min({int(width)},iw)':-2"]
        cmd += [str(tmp)]
        try:
            result = run(cmd, timeout=timeout, cancel_event=cancel_event)
        except MediaError:
            discard(tmp)
            raise
        if result.returncode != 0:
            discard(tmp)
            detail = (result.stderr or "").strip()[-2000:] or f"exit code {result.returncode}"
            raise MediaError(f"抽帧失败（{t} ms）：{detail}")
        if not tmp.exists() or tmp.stat().st_size == 0:
            discard(tmp)
            note = "这一点没有画面（超出视频末尾或该处无帧）"
            if duration_ms is None:
                note += "；探测不到时长"
            results.append(FrameFile(t, None, note))
            continue
        os.replace(tmp, out)
        results.append(FrameFile(t, out))
    return results
