"""把任意音频/视频文件解码成单声道 float32 PCM（缺省 16 kHz），经 ffmpeg 管道、
不落中间文件。

返回 numpy 一维 `float32` 数组，取值约在 [-1, 1]，下标 i 对应时间 i / sr 秒，
与文件自己的时间轴同起点（第 0 个采样就是文件的第 0 秒）。
"""
from __future__ import annotations

import os
import threading

import numpy as np

from ..media._proc import MediaError, resolve_tool, run_bytes

DEFAULT_SR = 16000


def decode_pcm(
    path: str | os.PathLike,
    *,
    sr: int = DEFAULT_SR,
    ffmpeg: str | None = None,
    timeout: float = 900,
    cancel_event: "threading.Event | None" = None,
) -> np.ndarray:
    """解码 `path` 的第一条音频流，混成单声道、重采样到 `sr`，返回 float32 数组。

    视频文件（webm/mp4 等）只取音轨；没有音轨时 ffmpeg 报错，抛 `MediaError`。
    内存：16 kHz 下每小时约 230 MB。`timeout` 秒内没解完整组杀掉，抛
    `MediaTimeoutError`；`cancel_event` 置位抛 `MediaCancelled`。

    不补时间戳空洞：容器里音频时间戳有断档时，PCM 是按包顺序接起来的。
    """
    if sr <= 0:
        raise ValueError("sr 必须为正")
    tool = resolve_tool(ffmpeg, "ffmpeg")
    cmd = [
        tool, "-nostdin", "-v", "error",
        "-i", str(path),
        "-map", "0:a:0", "-vn", "-sn", "-dn",
        "-ac", "1", "-ar", str(int(sr)),
        "-f", "f32le", "-acodec", "pcm_f32le", "pipe:1",
    ]
    returncode, data, stderr = run_bytes(cmd, timeout=timeout, cancel_event=cancel_event)
    if returncode != 0:
        detail = stderr.strip()[-2000:] or f"exit code {returncode}"
        raise MediaError(f"解码音频失败：{detail}")
    usable = len(data) - len(data) % 4
    if usable != len(data):
        del data[usable:]
    return np.frombuffer(data, dtype="<f4").astype(np.float32, copy=False)
