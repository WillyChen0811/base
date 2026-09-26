"""按时间查字幕的索引结构。

接口语义参考 asbplayer 的 SubtitleCollection——"给一个时间点，答出正在显示什么、
上一条是什么、下一条是什么"。asbplayer 是 AGPL-3.0，这里只借语义，独立实现。

只用标准库 `bisect`：允许 cue 重叠（双语/多轨字幕会有多条同时显示），不引入
第三方区间树——量级是几千条字幕，线性扫一小段窗口足够快。
"""
from __future__ import annotations

from bisect import bisect_left, bisect_right
from dataclasses import dataclass
from typing import Iterable

from .models import Cue


@dataclass(frozen=True)
class Slice:
    """某个时间点上字幕的状态。"""

    showing: list[Cue]  # 这一刻正在显示的所有 cue（可能有多条，重叠时）
    last_shown: Cue | None  # 这一刻之前最后结束的一条（没有则 None）
    next_to_show: Cue | None  # 这一刻之后最先开始的一条（没有则 None）


class CueIndex:
    """对一份 cue 列表建索引，支持按时间点/区间查询。"""

    def __init__(self, cues: Iterable[Cue]):
        by_start = sorted(cues, key=lambda c: c.start)
        self._by_start = by_start
        self._starts = [c.start for c in by_start]
        by_end = sorted(by_start, key=lambda c: c.end)
        self._by_end = by_end
        self._ends = [c.end for c in by_end]
        # showing 窗口扫描的边界：任何在 t 时刻显示中的 cue，start 必然满足
        # t - start < 它自己的时长 <= max_duration，超过这个窗口的 cue 不可能
        # 还在显示，不用继续往前扫。
        self._max_duration = max((c.end - c.start for c in by_start), default=0)

    def at(self, t: int) -> Slice:
        """查时间点 t（毫秒）的状态。"""
        idx = bisect_right(self._starts, t)

        showing: list[Cue] = []
        i = idx - 1
        while i >= 0 and t - self._starts[i] < self._max_duration:
            cue = self._by_start[i]
            if cue.start <= t < cue.end:
                showing.append(cue)
            i -= 1
        showing.reverse()

        idx_end = bisect_right(self._ends, t)
        last_shown = self._by_end[idx_end - 1] if idx_end > 0 else None

        next_to_show = self._by_start[idx] if idx < len(self._by_start) else None

        return Slice(showing=showing, last_shown=last_shown, next_to_show=next_to_show)

    def between(self, a: int, b: int) -> list[Cue]:
        """[a, b) 区间内有交集的所有 cue，按 start 排序。"""
        idx = bisect_left(self._starts, b)
        return sorted(
            (c for c in self._by_start[:idx] if c.end > a),
            key=lambda c: c.start,
        )

    def gaps(self, min_ms: int) -> list[tuple[int, int]]:
        """字幕之间没有任何 cue 覆盖、且长度 >= min_ms 的空档，供"跳过空白"用。

        不含开头（第一条 cue 之前）和结尾（最后一条 cue 之后）——那两段有没有
        内容取决于视频总时长，这个信息 CueIndex 不掌握，留给调用方自己算。
        """
        if not self._by_start:
            return []
        merged: list[list[int]] = []
        for cue in self._by_start:
            if merged and cue.start <= merged[-1][1]:
                merged[-1][1] = max(merged[-1][1], cue.end)
            else:
                merged.append([cue.start, cue.end])
        return [
            (merged[i][1], merged[i + 1][0])
            for i in range(len(merged) - 1)
            if merged[i + 1][0] - merged[i][1] >= min_ms
        ]

    def surrounding(self, cue: Cue, before: int, after: int) -> list[Cue]:
        """取 cue 前后各若干条上下文（含 cue 本身），按 start 排序，做卡片用。

        cue 的定位按 (start, end, text) 值匹配（Cue 是不可变 dataclass，值相等
        即视为同一条），找不到时只返回空列表。
        """
        idx = bisect_left(self._starts, cue.start)
        pos = None
        i = idx
        while i < len(self._by_start) and self._by_start[i].start == cue.start:
            if self._by_start[i] == cue:
                pos = i
                break
            i += 1
        if pos is None:
            return []
        lo = max(0, pos - before)
        hi = min(len(self._by_start), pos + after + 1)
        return self._by_start[lo:hi]
