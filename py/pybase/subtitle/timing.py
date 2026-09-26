"""字幕时间校准：整体平移、还原。"""
from __future__ import annotations

from typing import Iterable

from .models import Cue


def shift(cues: Iterable[Cue], offset_ms: int) -> list[Cue]:
    """整体平移所有 cue 的 start/end，`original_start`/`original_end` 保持不变。

    严格按 `offset_ms` 平移，允许出现负时间：校准值是用户明确给的，悄悄改小会让
    "先 -5s 再 +5s"回不到原位。负时间只在写文件时处理（见 `io_.dumps`）。
    """
    return [
        Cue(
            start=c.start + offset_ms,
            end=c.end + offset_ms,
            text=c.text,
            original_start=c.original_start,
            original_end=c.original_end,
        )
        for c in cues
    ]


def reset_timing(cues: Iterable[Cue]) -> list[Cue]:
    """把 start/end 还原成 original_start/original_end，撤销之前所有的 shift。"""
    return [
        Cue(
            start=c.original_start,
            end=c.original_end,
            text=c.text,
            original_start=c.original_start,
            original_end=c.original_end,
        )
        for c in cues
    ]
