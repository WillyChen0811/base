"""字幕/文本片段的本地检索：关键词（SQLite FTS5）+ 语义（向量余弦）混合，RRF 融合。

零第三方依赖，见 `index.py` 顶部注释里的方案取舍（trigram tokenizer + 短查询 LIKE
兜底、暴力余弦不引入 sqlite-vec）。
"""
from __future__ import annotations

from .chunking import segments_from_cues
from .index import SearchIndex
from .models import Hit, Segment

__all__ = [
    "SearchIndex",
    "Segment",
    "Hit",
    "segments_from_cues",
]
