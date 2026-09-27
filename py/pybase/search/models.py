"""检索的基础数据模型。

只放数据结构，不放索引/查询逻辑（那是 `index.py` 的事）。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Literal


@dataclass(frozen=True)
class Segment:
    """一条待索引的检索片段。

    时间单位与 `pybase.subtitle.Cue` 一致，整数毫秒；`start`/`end` 是这个片段
    覆盖的时间范围（多条 cue 合并而成时取首条 start、末条 end）。
    """

    start: int
    end: int
    text: str


@dataclass(frozen=True)
class Hit:
    """一条检索结果。

    `score` 越大越相关：纯关键词路时是 bm25 取负后的值，混合路时是 RRF 融合分数——
    两条路的分数量纲不同，只在各自的排序里有意义，不要跨查询比较绝对值。
    `matched_by` 标记这条命中是关键词、语义还是两路都命中。
    """

    doc_id: str
    start: int
    end: int
    text: str
    score: float
    snippet: str
    matched_by: Literal["keyword", "semantic", "both"]
