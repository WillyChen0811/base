"""给大模型用的字幕格式化：带时间戳的纯文本、按字符预算切块。"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

from .models import Cue


def _format_timestamp(ms: int) -> str:
    total_seconds = ms // 1000
    hours, rem = divmod(total_seconds, 3600)
    minutes, seconds = divmod(rem, 60)
    if hours:
        return f"{hours}:{minutes:02d}:{seconds:02d}"
    return f"{minutes:02d}:{seconds:02d}"


def to_timestamped_text(cues: Iterable[Cue]) -> str:
    """把 cues 转成 `[MM:SS] 文本` 每行一条的纯文本，超过 1 小时用 `[H:MM:SS]`。

    多行 cue 的换行折成空格——这是给模型读的摘要输入，不是要还原排版。
    """
    lines = [f"[{_format_timestamp(c.start)}] {c.text.replace(chr(10), ' ')}" for c in cues]
    return "\n".join(lines)


@dataclass(frozen=True)
class Chunk:
    """一个用于 map-reduce 总结的字幕块。"""

    start: int
    end: int
    cues: list[Cue]


def chunk(cues: Iterable[Cue], max_chars: int) -> list[Chunk]:
    """按字符预算（`to_timestamped_text` 口径）切块，不拆开单条 cue。

    单条 cue 自己的格式化文本就超过 max_chars 时，单独成一块（没法再拆），
    不报错也不截断——截断字幕内容是调用方该决定的事，不是切块该做的事。
    """
    values = list(cues)
    if not values:
        return []

    chunks: list[Chunk] = []
    current: list[Cue] = []
    current_len = 0

    def flush() -> None:
        if current:
            # 重叠 cue 里最后一条不一定最晚结束
            chunks.append(Chunk(start=current[0].start, end=max(c.end for c in current), cues=list(current)))

    for cue in values:
        line_len = len(f"[{_format_timestamp(cue.start)}] {cue.text}") + 1
        if current and current_len + line_len > max_chars:
            flush()
            current = []
            current_len = 0
        current.append(cue)
        current_len += line_len

    flush()
    return chunks
