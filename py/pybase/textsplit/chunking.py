"""容量切分：带编号的行 + 预算 → 若干块，每块有主体和「只作参考」的前文。

- **主体不重叠**：每一行（被切开的超长行是它的每一片）恰好属于一块的主体，按原顺序。
  （Google LangExtract「主体 + 前文窗口」印证的切法；不做 LangChain 那种主体重叠。）
- **预算**可组合：``max_chars``（主体字数，按 ``measure`` 算，缺省每片 ``len(text)``）、``max_lines``（主体行数，
  被切开的行在每块里算一行）、``max_ms``（主体的时间窗：最晚的结束 - 最早的开始；没有结束时间的行按开始算）。
  加上下一片会超任一预算就另起一块；一块至少放一片，所以单独一片超了预算（只能是时间窗或 ``measure`` 带的固定开销）
  也自成一块，不报错。
- **超长单行**（只在给了 ``max_chars`` 且这一行超了时）：先按句、再按小句（``sentence.py`` 的规则），最后硬切；
  切出的片首尾相接正好拼回整行（片之间的空白归前一片），装块时同一行连着的片在一块里并回一片。
  片的时间沿用整行的起止。
- **前文** ``context``：每块主体之前的那几片（整行算一片，被切开的行每片算一片；跨过前面几块也照取），只作参考，
  不算进预算。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Callable, Iterable, Mapping, Sequence

from .sentence import LINE_BREAKS, Span, sentences


@dataclass(frozen=True)
class Line:
    """一行：调用方给的编号 ``no``（任意可比较、可哈希的值，如稳定行号）、文字、可选的起止毫秒。"""

    no: Any
    text: str
    start_ms: int | None = None
    end_ms: int | None = None


@dataclass(frozen=True)
class Piece:
    """块里的一片：第 ``no`` 行文字的 ``[start, end)``（整行时是 ``0, len(行文字)``）。"""

    no: Any
    text: str
    start: int
    end: int
    line_len: int
    start_ms: int | None = None
    end_ms: int | None = None

    @property
    def whole(self) -> bool:
        """是不是整行。"""
        return self.start == 0 and self.end == self.line_len


@dataclass(frozen=True)
class Chunk:
    """一块：``index`` 从 0 起；``body`` 是主体（不和别的块重叠）；``context`` 是只作参考的前文。"""

    index: int
    body: tuple[Piece, ...]
    context: tuple[Piece, ...]

    @property
    def first(self) -> Any:
        """主体第一行的编号。"""
        return self.body[0].no

    @property
    def last(self) -> Any:
        """主体最后一行的编号。"""
        return self.body[-1].no

    @property
    def lines(self) -> tuple:
        """主体涉及的行号（去重，按顺序）。"""
        out: list = []
        for p in self.body:
            if not out or out[-1] != p.no:
                out.append(p.no)
        return tuple(out)


def as_line(item: Any) -> Line:
    """``Line``、``(no, text)``、``(no, text, start_ms, end_ms)``、带 no/text[/start_ms/end_ms] 的 dict → ``Line``。"""
    if isinstance(item, Line):
        line = item
    elif isinstance(item, Mapping):
        line = Line(item["no"], item["text"], item.get("start_ms"), item.get("end_ms"))
    elif isinstance(item, (tuple, list)) and len(item) in (2, 4):
        line = Line(*item)
    else:
        raise TypeError(f"行要是 Line、(no, text)、(no, text, start_ms, end_ms) 或 dict：{item!r}")
    if not isinstance(line.text, str):
        raise TypeError(f"第 {line.no!r} 行的文字要是字符串：{line.text!r}")
    return line


def _piece(line: Line, a: int, b: int) -> Piece:
    return Piece(line.no, line.text[a:b], a, b, len(line.text), line.start_ms, line.end_ms)


def _fit_end(line: Line, a: int, b: int, limit: int, measure: Callable[[Piece], int]) -> int:
    """从 a 起、不超过 b，``measure`` 不超 limit 的最远终点；连一个字都放不下也至少给一个字。"""
    lo, hi = a + 1, b
    if measure(_piece(line, a, lo)) > limit:
        return lo
    while lo < hi:                                  # 找最大的 e 使 measure([a, e)) <= limit（假定随长度不减）
        mid = (lo + hi + 1) // 2
        if measure(_piece(line, a, mid)) <= limit:
            lo = mid
        else:
            hi = mid - 1
    return lo


def split_line(line: Line, limit: int, measure: Callable[[Piece], int] | None = None) -> list[Piece]:
    """把一行切成每片 ``measure`` 不超 limit 的若干片：先按句、再按小句、最后硬切；片首尾相接拼回整行。"""
    measure = measure or (lambda p: len(p.text))
    text = line.text
    n = len(text)
    if n == 0 or measure(_piece(line, 0, n)) <= limit:
        return [_piece(line, 0, n)]

    def tiles(starts: list[int], a: int, b: int) -> list[tuple[int, int]]:
        cuts = sorted({a, b, *[s for s in starts if a < s < b]})
        return list(zip(cuts, cuts[1:]))

    out: list[Piece] = []
    sents = sentences(text)
    for a, b in tiles([s.start for s in sents], 0, n):
        if measure(_piece(line, a, b)) <= limit:
            out.append(_piece(line, a, b))
            continue
        cls = [c.start for s in sents if s.start < b and s.end > a for c in s.clauses]
        for ca, cb in tiles(cls, a, b):
            if measure(_piece(line, ca, cb)) <= limit:
                out.append(_piece(line, ca, cb))
                continue
            x = ca
            while x < cb:                           # 硬切
                y = _fit_end(line, x, cb, limit, measure)
                out.append(_piece(line, x, y))
                x = y
    return out


def _coalesce(pieces: Sequence[Piece]) -> tuple[Piece, ...]:
    out: list[Piece] = []
    for p in pieces:
        if out and out[-1].no == p.no and out[-1].end == p.start and out[-1].line_len == p.line_len:
            q = out[-1]
            out[-1] = Piece(q.no, q.text + p.text, q.start, p.end, q.line_len, q.start_ms, q.end_ms)
        else:
            out.append(p)
    return tuple(out)


def chunk(lines: Iterable[Any], *, max_chars: int | None = None, max_lines: int | None = None,
          max_ms: int | None = None, context: int = 0,
          measure: Callable[[Piece], int] | None = None) -> list[Chunk]:
    """带编号的行 → 块的列表（规则见模块说明）。至少给一种预算；行可以是 ``Line``、元组或 dict（见 ``as_line``）。"""
    for name, v in (("max_chars", max_chars), ("max_lines", max_lines), ("max_ms", max_ms)):
        if v is not None and (not isinstance(v, int) or isinstance(v, bool) or v <= 0):
            raise ValueError(f"{name} 要是正整数：{v!r}")
    if max_chars is None and max_lines is None and max_ms is None:
        raise ValueError("至少给一种预算：max_chars、max_lines、max_ms")
    if not isinstance(context, int) or isinstance(context, bool) or context < 0:
        raise ValueError(f"context 要是不小于 0 的整数：{context!r}")
    measure = measure or (lambda p: len(p.text))
    items = [as_line(x) for x in lines]
    if max_ms is not None:
        for ln in items:
            if ln.start_ms is None:
                raise ValueError(f"给了 max_ms，第 {ln.no!r} 行却没有 start_ms")

    units: list[Piece] = []
    for ln in items:
        if max_chars is not None:
            units.extend(split_line(ln, max_chars, measure))
        else:
            units.append(_piece(ln, 0, len(ln.text)))

    groups: list[tuple[int, int]] = []
    i, n = 0, len(units)
    while i < n:
        j = i + 1
        chars = measure(units[i])
        nos = {units[i].no}
        lo = units[i].start_ms
        hi = units[i].end_ms if units[i].end_ms is not None else units[i].start_ms
        while j < n:
            u = units[j]
            c2 = chars + measure(u)
            nos2 = nos | {u.no}
            lo2 = hi2 = None
            if max_ms is not None:
                lo2 = min(lo, u.start_ms)
                hi2 = max(hi, u.end_ms if u.end_ms is not None else u.start_ms)
            if ((max_chars is not None and c2 > max_chars) or (max_lines is not None and len(nos2) > max_lines)
                    or (max_ms is not None and hi2 - lo2 > max_ms)):
                break
            chars, nos, lo, hi = c2, nos2, lo2, hi2
            j += 1
        groups.append((i, j))
        i = j

    return [Chunk(k, _coalesce(units[a:b]), _coalesce(units[max(0, a - context):a]))
            for k, (a, b) in enumerate(groups)]


def chunk_text(text: str, max_chars: int) -> list[Span]:
    """一整段文字（可以有换行）→ 首尾相接的几段，每段约不超过 ``max_chars`` 字：按物理行装块，超长的行先按句、
    再按小句、最后硬切（同 ``chunk``）；换行算一个字。每段是原文里连着的一段（``text == 原文[start:end]``，
    去掉两头空白）；各段之间只隔着空白，合起来盖住原文所有不是空白的字。全是空白 → []。"""
    if not isinstance(text, str):
        raise TypeError(f"text 要是字符串：{type(text).__name__}")
    spans, start = [], 0
    for i, c in enumerate(text):
        if c in LINE_BREAKS:
            spans.append((start, i))
            start = i + 1
    spans.append((start, len(text)))
    lines = [Line(k, text[a:b]) for k, (a, b) in enumerate(spans) if text[a:b].strip()]
    if not lines:
        return []
    out = []
    for ch in chunk(lines, max_chars=max_chars, measure=lambda p: len(p.text) + 1):
        a = spans[ch.body[0].no][0] + ch.body[0].start
        b = spans[ch.body[-1].no][0] + ch.body[-1].end
        while a < b and text[a].isspace():
            a += 1
        while b > a and text[b - 1].isspace():
            b -= 1
        if a < b:
            out.append(Span(a, b, text[a:b]))
    return out
