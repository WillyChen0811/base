"""ffsubsync 命令行包装：把字幕自动对齐到参考（视频/音频，或另一份已对齐字幕）。

对齐算法（VAD/关键词匹配）完全交给 ffsubsync 做（MIT）——它是成熟的可选外部
工具，不是 Python 依赖，找不到就抛 `ToolNotFoundError`；本模块只管拼命令行、
跑子进程、解析它自己报的偏移量。
"""
from __future__ import annotations

import os
import re
import threading
from pathlib import Path

from ._proc import resolve_tool, run_to_file

# ffsubsync 的原文是 `logger.info("offset seconds: %.3f", ...)`（ffsubsync.py）。
# 只认这一种：它还会打 "|offset| 12.0s > 60.0s" 这类质量告警，宽松匹配会拿错数。
_OFFSET_PATTERN = re.compile(r"offset seconds:\s*(-?\d+(?:\.\d+)?)")


def sync_subtitles(
    reference: str | os.PathLike,
    subtitle: str | os.PathLike,
    out: str | os.PathLike,
    *,
    ffsubsync: str | None = None,
    extra_args: list[str] | None = None,
    timeout: float = 300,
    cancel_event: "threading.Event | None" = None,
) -> float | None:
    """调 `ffsubsync reference -i subtitle -o out`，返回它报告的偏移秒数。

    `reference` 可以是视频/音频文件，也可以是另一份已经对齐好的字幕。
    返回值只在能从 ffsubsync 输出里解析出偏移时才给；解析不到是 `None`，
    不代表对齐失败——`out` 是否写出来才是对齐是否成功的判据。
    输出同样先落临时文件再 `os.replace`，超时/取消不留半截字幕文件。
    """
    tool = resolve_tool(ffsubsync, "ffsubsync")

    def cmd_for(tmp: Path) -> list[str]:
        return [tool, str(reference), "-i", str(subtitle), "-o", str(tmp), *(extra_args or [])]

    result = run_to_file(cmd_for, out, timeout=timeout, cancel_event=cancel_event)
    return _parse_offset((result.stdout or "") + "\n" + (result.stderr or ""))


def _parse_offset(text: str) -> float | None:
    match = _OFFSET_PATTERN.search(text)
    return float(match.group(1)) if match else None
