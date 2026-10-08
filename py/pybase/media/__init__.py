"""音视频基础能力：ffmpeg/ffprobe/ffsubsync 的子进程封装。

给"视频字幕学习"类业务提供 subs2srs/mpvacious/asbplayer 式的标准操作：
探测媒体信息（`probe`）、按字幕时间段截音频/截图（`extract_audio`/
`snapshot`，含前后 padding）、调 ffsubsync 做字幕自动对齐
（`sync_subtitles`）、判断能否直接喂给网页 `<video>`（`playback_plan`）
以及只转封装不重新编码（`remux`）。

需要自己拼命令的调用方可以直接用 `run` 跑子进程、`resolve_tool` 找可执行
文件，超时/取消语义与封装函数一致：命中时整个进程组被杀掉，分别抛
`MediaTimeoutError`/`MediaCancelled`，找不到工具抛 `ToolNotFoundError`。

全部通过**子进程**调用外部工具，模块顶层不引入任何第三方 Python 依赖。
可执行文件路径由调用方传入，不传就用 `shutil.which` 在 PATH 里找，找不到
抛 `ToolNotFoundError`。时间单位统一整数毫秒，与 pysubs2/字幕模块一致。
"""
from __future__ import annotations

from ._proc import (
    MediaCancelled,
    MediaError,
    MediaTimeoutError,
    ToolNotFoundError,
    resolve_tool,
    run,
)
from .clip import extract_audio, snapshot
from .playback import playback_plan, remux
from .probe import probe
from .sync import sync_subtitles

__all__ = [
    "MediaCancelled",
    "MediaError",
    "MediaTimeoutError",
    "ToolNotFoundError",
    "extract_audio",
    "playback_plan",
    "probe",
    "remux",
    "resolve_tool",
    "run",
    "snapshot",
    "sync_subtitles",
]
