"""回溯校验：几条引文是不是原文里连着的一段，原文里哪些小句没被任何引文碰到。

判定只有一条：**引文（归一后）是原文（归一后）里连着的一段**才算通过——先比原样（``exact``），
再比归一后（``normalized``，见 ``normal.py``），都不是就是 ``not_found``。**不接受只对上开头一小段的部分匹配**
（Google LangExtract issue #289 的教训）。

不是原文的引文另给「原文里最像的一句」和分数（0–100），**只作提示**（写进回给模型的错误话），不参与判定。
近似比对用 RapidFuzz（可选依赖 ``pip install "pybase[textsplit]"``；``fuzz.partial_ratio``，引文不长于那一句时用，
引文更长时用 ``fuzz.ratio``）；没装退回标准库 ``difflib.SequenceMatcher``（``autojunk=False``，部分匹配照
fuzzywuzzy 的老做法：按匹配块对齐窗口取最高）。两边分数不保证一样，只保证「明显最像的那句」选得一致。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Sequence

from .normal import Normalized, normalize
from .sentence import Span, count_hanzi, sentences

EXACT = "exact"
NORMALIZED = "normalized"
NOT_FOUND = "not_found"


@dataclass(frozen=True)
class QuoteTrace:
    """一条引文的结果。

    - ``status``：``exact`` / ``normalized`` / ``not_found``；``ok`` 是前两种。
    - ``spans``：引文在原文里**所有**出现的位置 ``(start, end)``（原文下标；归一后才对上的已经指回原文）。
    - ``span``：选定的那一处（缺省第一处；``trace(ordered=True)`` 时取不早于上一条选定位置的第一处），没找到为 None。
    - ``best`` / ``score``：没找到时原文里最像的一句和分数（0–100），只作提示；找到了都是 None。
    """

    quote: str
    status: str
    spans: tuple[tuple[int, int], ...]
    span: tuple[int, int] | None
    best: Span | None = None
    score: float | None = None

    @property
    def ok(self) -> bool:
        return self.status != NOT_FOUND


@dataclass(frozen=True)
class TraceReport:
    """``results`` 与输入的引文一一对应；``uncovered`` 是没被任何找到的引文碰到的小句（按出现先后）；
    ``fuzzy`` 是这次近似比对用的后端（``rapidfuzz`` / ``difflib``；没用上是 None）。"""

    results: tuple[QuoteTrace, ...]
    uncovered: tuple[Span, ...]
    fuzzy: str | None = None


# ---------------------------------------------------------------- 近似比对（只作提示）
def _difflib_ratio(a: str, b: str) -> float:
    from difflib import SequenceMatcher

    return SequenceMatcher(None, a, b, autojunk=False).ratio() * 100


def _difflib_partial(short: str, long: str) -> float:
    from difflib import SequenceMatcher

    if len(short) > len(long):
        short, long = long, short
    if not short:
        return 0.0
    best = 0.0
    for a, b, _size in SequenceMatcher(None, short, long, autojunk=False).get_matching_blocks():
        start = max(0, b - a)
        window = long[start:start + len(short)]
        best = max(best, _difflib_ratio(short, window))
        if best >= 100:
            break
    return best


def fuzzy_backend() -> str:
    """当前能用的近似比对后端：装了 RapidFuzz 是 ``rapidfuzz``，否则 ``difflib``（每次调用现查）。"""
    try:
        from rapidfuzz import fuzz  # noqa: F401
    except ImportError:
        return "difflib"
    return "rapidfuzz"


def _scorer(backend: str):
    if backend == "rapidfuzz":
        from rapidfuzz import fuzz

        return fuzz.partial_ratio, fuzz.ratio
    return _difflib_partial, _difflib_ratio


def similarity(quote: str, sentence: str, *, backend: str | None = None) -> float:
    """引文和一句的相似度 0–100（两边先归一）：引文不长于那一句用部分匹配，更长用整体比。只作提示。"""
    q, s = normalize(quote).text, normalize(sentence).text
    if not q or not s:
        return 0.0
    partial, ratio = _scorer(backend or fuzzy_backend())
    return float(partial(q, s) if len(q) <= len(s) else ratio(q, s))


# ---------------------------------------------------------------- 判定
def _all_find(hay: str, needle: str) -> list[int]:
    out, i = [], hay.find(needle)
    while i >= 0:
        out.append(i)
        i = hay.find(needle, i + 1)
    return out


def _locate(original: str, norm: Normalized, quote: str) -> tuple[str, list[tuple[int, int]]]:
    nq = normalize(quote).text
    if not nq:                                            # 空串、全是空白：不算原文
        return NOT_FOUND, []
    exact = _all_find(original, quote)
    if exact:
        return EXACT, [(i, i + len(quote)) for i in exact]
    hits = _all_find(norm.text, nq)
    if hits:
        return NORMALIZED, [norm.to_source(i, i + len(nq)) for i in hits]
    return NOT_FOUND, []


def _intersects(a: tuple[int, int], b: tuple[int, int]) -> bool:
    return a[0] < b[1] and b[0] < a[1]


def uncovered_clauses(original: str, spans: Iterable[tuple[int, int]], *, min_hanzi: int = 0) -> list[Span]:
    """原文里和 ``spans`` 都不相交的小句（汉字不少于 ``min_hanzi`` 个的才列）。"""
    spans = list(spans)
    out = []
    for s in sentences(original):
        for c in s.clauses:
            if count_hanzi(c.text) < min_hanzi:
                continue
            if not any(_intersects((c.start, c.end), sp) for sp in spans):
                out.append(c)
    return out


def trace(original: str, quotes: Sequence[str], *, min_hanzi: int = 0, ordered: bool = False,
          hint: bool = True) -> TraceReport:
    """原文 + 若干引文 → ``TraceReport``。

    ``ordered``：引文本来就按原文先后排（如拆需求的 quote），同一子串出现多次时 ``span`` 取不早于上一条选定位置的那处。
    ``min_hanzi``：没碰到的小句里，汉字少于这个数的不列（缺省全列）。「碰到」= 和某条找到的引文的**任一**出现位置相交。
    ``hint``：没找到的引文要不要算「最像的一句」（只作提示；关了更快）。
    """
    if not isinstance(original, str):
        raise TypeError(f"original 要是字符串：{type(original).__name__}")
    quotes = list(quotes)
    for q in quotes:
        if not isinstance(q, str):
            raise TypeError(f"引文要是字符串：{q!r}")
    norm = normalize(original)
    sents = None
    backend = None
    results = []
    covered: list[tuple[int, int]] = []
    last = 0
    for q in quotes:
        status, spans = _locate(original, norm, q)
        span = None
        best = score = None
        if spans:
            covered.extend(spans)
            span = next((sp for sp in spans if sp[0] >= last), spans[0]) if ordered else spans[0]
            last = span[0]
        elif hint:
            if sents is None:
                sents = sentences(original)
                backend = fuzzy_backend()
            scored = [(similarity(q, s.text, backend=backend), k) for k, s in enumerate(sents)]
            if scored:
                score, k = max(scored, key=lambda x: (x[0], -x[1]))
                s = sents[k]
                best = Span(s.start, s.end, s.text)
        results.append(QuoteTrace(q, status, tuple(spans), span, best, score))
    return TraceReport(tuple(results), tuple(uncovered_clauses(original, covered, min_hanzi=min_hanzi)), backend)
