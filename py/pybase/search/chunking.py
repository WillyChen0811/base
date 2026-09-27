"""把字幕 cue 序列合并成检索用的片段（Segment）。

不依赖 `pybase.subtitle`：`cues` 只要是带 `start`/`end`/`text` 属性的对象序列即可
（`pybase.subtitle.Cue` 天然满足），检索模块不应该反过来依赖字幕模块。
"""
from __future__ import annotations

from typing import Sequence

from .models import Segment


def segments_from_cues(cues: Sequence, *, max_chars: int, overlap: int) -> list[Segment]:
    """按字符预算把连续 cue 合并成片段，片段之间按 `overlap` 个字符重叠。

    不拆开单条 cue：单条 cue 本身的文本长度就超过 `max_chars`，也单独成一段，
    不截断（跟 `pybase.subtitle.llm.chunk` 的取舍一致）。`overlap` 按整条 cue
    回退（不会把某条 cue 从中间切开），实际重叠的字符数可能略多于 `overlap`。
    """
    if max_chars <= 0:
        raise ValueError("max_chars 必须为正数")
    if overlap < 0:
        raise ValueError("overlap 不能为负数")

    items = list(cues)
    if not items:
        return []

    segments: list[Segment] = []
    n = len(items)
    start_idx = 0

    while start_idx < n:
        idx = start_idx
        length = 0
        included = []
        while idx < n:
            piece = items[idx].text
            add_len = len(piece) + (1 if included else 0)  # 拼接时中间补一个空格
            if included and length + add_len > max_chars:
                break
            included.append(items[idx])
            length += add_len
            idx += 1
        if not included:
            # 单条 cue 自己就超过 max_chars，没法再拆，单独成段
            included = [items[idx]]
            idx += 1

        text = " ".join(c.text.replace("\n", " ") for c in included)
        segments.append(Segment(start=included[0].start, end=included[-1].end, text=text))

        if idx >= n:
            break

        if overlap == 0:
            start_idx = idx
            continue

        # 从 idx 往回数，凑够 overlap 个字符就是下一段的起点（整条 cue 回退，不切开）
        back = idx - 1
        overlap_len = 0
        while back > start_idx and overlap_len < overlap:
            overlap_len += len(items[back].text) + 1
            back -= 1
        start_idx = back + 1  # back 停在 start_idx 处不会再被计入重叠，+1 保证严格推进

    return segments
