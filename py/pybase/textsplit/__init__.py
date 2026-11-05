"""文本切分的共同底座：分句、切块、重切、回溯校验、合并，和语义拆分的配方。

不带业务、不调模型、只用标准库（近似比对装了 RapidFuzz 就用，``pip install "pybase[textsplit]"``）。
「一个大类下的两种」：**容量切分** = ``chunk`` + ``merge``（程序定边界，为了读得下，合回原形状）；
**语义拆分** = ``atomize`` 配方（模型定边界，产出新单位，靠 ``trace`` 指回原文并查漏）。

- ``sentences(text)`` → ``[Sentence(start, end, text, clauses)]``：句和小句，带原文起止位置
- ``chunk(lines, max_chars=, max_lines=, max_ms=, context=)`` → ``[Chunk(index, body, context)]``：主体不重叠
- ``chunk_text(text, max_chars)`` → ``[Span]``：一整段文字按句切成首尾相接的几段
- ``resegment(text, start_ms, end_ms, max_chars=, max_ms=, words=)`` → ``[Segment]``：只切不并、带起止毫秒；
  ``resegment_many(utterances, ...)`` 逐段喂
- ``normalize(text)`` → ``Normalized(text, origin)``：逐字 NFKC + 补充表 + 去空白，带偏移表
- ``trace(original, quotes, min_hanzi=, ordered=)`` → ``TraceReport(results, uncovered)``：精确 / 归一后精确 / 不是原文
- ``merge(outputs, key=, span=, same=, combine=)`` → 列表：按编号去重、跨块相接的段并成一段
- ``p_atom(limits=, messages=)`` → ``Recipe``：拆需求（P-ATOM）的配方，交给 xxyi 的 ``taskmodel.run`` 去跑

各件的输入输出详见同目录 README.md 与各模块说明。
"""
from __future__ import annotations

from .atomize import DEFAULT_MESSAGES, Atom, AtomizeResult, AtomLimits, Recipe, p_atom
from .chunking import Chunk, Line, Piece, as_line, chunk, chunk_text, split_line
from .merging import merge
from .normal import Normalized, normalize
from .reseg import Segment, Word, resegment, resegment_many
from .sentence import Sentence, Span, clauses, count_hanzi, sentences
from .tracing import EXACT, NORMALIZED, NOT_FOUND, QuoteTrace, TraceReport, fuzzy_backend, similarity, trace, \
    uncovered_clauses

__all__ = [
    "Atom", "AtomizeResult", "AtomLimits", "DEFAULT_MESSAGES", "Recipe", "p_atom",
    "Chunk", "Line", "Piece", "as_line", "chunk", "chunk_text", "split_line",
    "merge",
    "Normalized", "normalize",
    "Segment", "Word", "resegment", "resegment_many",
    "Sentence", "Span", "clauses", "count_hanzi", "sentences",
    "EXACT", "NORMALIZED", "NOT_FOUND", "QuoteTrace", "TraceReport", "fuzzy_backend", "similarity", "trace",
    "uncovered_clauses",
]
