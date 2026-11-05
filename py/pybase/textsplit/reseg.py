"""重切：一段带起止毫秒的话 → 若干行（各带起止毫秒），给证据行用。

- **只切不并**：先在句末点号（及换行）处切开；某一句超过上限（``max_chars`` 字数、``max_ms`` 时长，任一）时
  在句内点号处切——从前往后尽量多装几个小句，装不下才切；某个小句自己还超，就硬切（有字词级时间时切在词的边界上）。
  不同的话之间、不同的句之间从不并到一起。
- **时间**：给了字词级时间 ``words``（同一时间轴的毫秒），按词定：一行的开始 = 落在这行里的第一个词的开始，
  结束 = 最后一个词的结束；词对不上文字、或这一行里一个词都没有时，按字数比例估，并标 ``estimated``。
  没给 ``words`` 时全按字数比例估（只数字母、数字、汉字，标点和空白不占时间），都标 ``estimated``。
  结果都夹在这段话的起止之内。
- **确定、可重入**：同样的输入同样的输出，没有全局状态；``resegment_many`` 是逐段调 ``resegment`` 的生成器，
  一段一段喂和一次喂结果一样。行的身份是 ``(seq, part)``：``seq`` 是调用方给的这段话的序号，``part`` 是切出的第几段。
- 每行的文字去掉两头空白；``start``/``end`` 是它在这段话文字里的位置。空的话（全是空白）不出行。

词和文字怎么对上：两边都逐字归一（``normal.py``）、只留字母数字汉字，按顺序找——每个词在上一个词之后、
8 个字以内找到就算对上，找不到就跳过这个词（不让一个对不上的词把后面全带偏）。
"""
from __future__ import annotations

import unicodedata
from bisect import bisect_left
from dataclasses import dataclass
from typing import Any, Iterable, Iterator, Mapping, Sequence

from .normal import norm_char
from .sentence import sentences

_LOOKAHEAD = 8


@dataclass(frozen=True)
class Word:
    """字词级时间：一个词（或字）和它的起止毫秒。"""

    text: str
    start_ms: int
    end_ms: int


@dataclass(frozen=True)
class Segment:
    """重切出的一行。``(seq, part)`` 是它的身份；``estimated`` 为真表示时间是按字数估的。"""

    seq: Any
    part: int
    start_ms: int
    end_ms: int
    text: str
    estimated: bool
    start: int
    end: int
    speaker: str = ""


def _is_ms(v: Any) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def _as_word(w: Any) -> Word:
    if isinstance(w, Word):
        return w
    if isinstance(w, Mapping):
        return Word(w.get("text", w.get("word", "")), w["start_ms"], w["end_ms"])
    if isinstance(w, (tuple, list)) and len(w) == 3:
        return Word(*w)
    if all(hasattr(w, k) for k in ("text", "start_ms", "end_ms")):
        return Word(w.text, w.start_ms, w.end_ms)
    raise TypeError(f"字词级时间要是 Word、(text, start_ms, end_ms) 或带这三样的 dict：{w!r}")


def _spoken(c: str) -> bool:
    return unicodedata.category(c)[0] in "LN"


class _Timing:
    """这段话里任一字符位置的时间，和一段 ``[a, b)`` 的起止。"""

    def __init__(self, text: str, start_ms: int, end_ms: int, words: Sequence[Word] | None):
        self.text, self.t0, self.t1 = text, start_ms, end_ms
        weights = [1 if _spoken(c) else 0 for c in text]
        if not any(weights):
            weights = [1] * len(text)
        self.cum = [0]
        for w in weights:
            self.cum.append(self.cum[-1] + w)
        self.words: list[tuple[int, int, int, int]] = []        # (原文起, 原文止, 开始毫秒, 结束毫秒)，按原文起排
        if words:
            self._align(words)
        self.word_starts = [w[0] for w in self.words]
        self.word_bounds = sorted({p for a, b, _s, _e in self.words for p in (a, b)})
        # 估时间用的锚点（按字数权重的位置 → 毫秒）：这段话的起止，加上对上的每个词的起止；毫秒不往回走
        pts = sorted([(0, start_ms), (self.cum[-1], end_ms)]
                     + [(self.cum[a], s) for a, _b, s, _e in self.words]
                     + [(self.cum[b], e) for _a, b, _s, e in self.words])
        self.anchor_w: list[int] = []
        self.anchor_ms: list[int] = []
        top = start_ms
        for w, ms in pts:
            top = max(top, self._clamp(ms))
            self.anchor_w.append(w)
            self.anchor_ms.append(top)

    def _align(self, words: Sequence[Word]) -> None:
        keys: list[str] = []
        origin: list[int] = []
        for i, c in enumerate(self.text):
            for x in norm_char(c).casefold():
                if _spoken(x):
                    keys.append(x)
                    origin.append(i)
        hay = "".join(keys)
        cur = 0
        for w in words:
            k = "".join(x for x in "".join(norm_char(c) for c in w.text).casefold() if _spoken(x))
            if not k:
                continue
            pos = hay.find(k, cur, cur + _LOOKAHEAD + len(k))
            if pos < 0:
                continue
            a, b = origin[pos], origin[pos + len(k) - 1] + 1
            s, e = sorted((w.start_ms, w.end_ms))
            self.words.append((a, b, s, e))
            cur = pos + len(k)

    def _clamp(self, t: int) -> int:
        return max(self.t0, min(self.t1, t))

    def at(self, pos: int) -> int:
        """按字数比例估的时间（有对上的词时在相邻的词之间插值）。"""
        w = self.cum[pos]
        k = bisect_left(self.anchor_w, w)
        if k < len(self.anchor_w) and self.anchor_w[k] == w:
            return self.anchor_ms[k]
        if k == 0:
            return self.anchor_ms[0]
        if k >= len(self.anchor_w):
            return self.anchor_ms[-1]
        w0, w1 = self.anchor_w[k - 1], self.anchor_w[k]
        m0, m1 = self.anchor_ms[k - 1], self.anchor_ms[k]
        return self._clamp(m0 + round((m1 - m0) * (w - w0) / (w1 - w0)))

    def span(self, a: int, b: int) -> tuple[int, int, bool]:
        """``[a, b)`` → (开始, 结束, 是不是估的)。词按开头落在哪一行算给那一行。"""
        i, j = bisect_left(self.word_starts, a), bisect_left(self.word_starts, b)
        if i < j:
            inside = self.words[i:j]
            s = self._clamp(min(x[2] for x in inside))
            e = self._clamp(max(x[3] for x in inside))
            return s, max(s, e), False
        s, e = self.at(a), self.at(b)
        return s, max(s, e), True

    def duration(self, a: int, b: int) -> int:
        s, e, _ = self.span(a, b)
        return e - s


def _strip(text: str, a: int, b: int) -> tuple[int, int]:
    while a < b and text[a].isspace():
        a += 1
    while b > a and text[b - 1].isspace():
        b -= 1
    return a, b


def resegment(text: str, start_ms: int, end_ms: int, *, max_chars: int | None = None, max_ms: int | None = None,
              words: Iterable[Any] | None = None, seq: Any = None, speaker: str = "") -> list[Segment]:
    """一段话 → 若干行（规则见模块说明）。``max_chars``/``max_ms`` 都不给就只在句末切。"""
    if not isinstance(text, str):
        raise TypeError(f"text 要是字符串：{type(text).__name__}")
    if not _is_ms(start_ms) or not _is_ms(end_ms) or start_ms < 0 or end_ms < start_ms:
        raise ValueError(f"起止要是整数毫秒且 0 <= start_ms <= end_ms：{start_ms!r}, {end_ms!r}")
    for name, v in (("max_chars", max_chars), ("max_ms", max_ms)):
        if v is not None and (not _is_ms(v) or v <= 0):
            raise ValueError(f"{name} 要是正整数：{v!r}")
    ws = [_as_word(w) for w in words] if words is not None else None
    for w in ws or []:
        if not _is_ms(w.start_ms) or not _is_ms(w.end_ms):
            raise ValueError(f"字词级时间要是整数毫秒：{w!r}")
    timing = _Timing(text, start_ms, end_ms, ws)

    def fits(a: int, b: int) -> bool:
        s, e = _strip(text, a, b)
        if max_chars is not None and e - s > max_chars:
            return False
        return max_ms is None or timing.duration(s, e) <= max_ms

    def chars_fit(a: int, b: int) -> bool:
        s, e = _strip(text, a, b)
        return max_chars is None or e - s <= max_chars

    def hard(a: int, b: int) -> list[tuple[int, int]]:
        """硬切 ``[a, b)``：候选切点是词的边界（有对上的词时）或每个字；从前往后尽量长。
        一个词按时长装不下就整个词成一行（词里面没有时间可切）；按字数装不下才在词里按字切。"""
        if timing.words:
            cands = [p for p in timing.word_bounds if a < p < b] + [b]
        else:
            cands = list(range(a + 1, b + 1))
        out, x, k = [], a, 0
        while x < b:
            while cands[k] <= x:
                k += 1
            y, j = None, k
            while j < len(cands) and fits(x, cands[j]):
                y = cands[j]
                j += 1
            if y is None:
                nxt = cands[k]
                if chars_fit(x, nxt):
                    y = nxt
                else:
                    y = next((p for p in range(nxt - 1, x, -1) if chars_fit(x, p)), x + 1)
            out.append((x, y))
            x = y
        return out

    pieces: list[tuple[int, int]] = []
    for sent in sentences(text):
        if fits(sent.start, sent.end):
            pieces.append((sent.start, sent.end))
            continue
        cur: tuple[int, int] | None = None
        for c in sent.clauses:
            if cur is not None and fits(cur[0], c.end):
                cur = (cur[0], c.end)
                continue
            if cur is not None:
                pieces.append(cur)
                cur = None
            if fits(c.start, c.end):
                cur = (c.start, c.end)
            else:
                pieces.extend(hard(c.start, c.end))
        if cur is not None:
            pieces.append(cur)

    out: list[Segment] = []
    for a, b in pieces:
        a, b = _strip(text, a, b)
        if a >= b:
            continue
        s, e, est = timing.span(a, b)
        out.append(Segment(seq, len(out), s, e, text[a:b], est, a, b, speaker))
    return out


def resegment_many(utterances: Iterable[Any], *, max_chars: int | None = None,
                   max_ms: int | None = None) -> Iterator[Segment]:
    """逐段重切（生成器，可以接实时来的话）。每段是带 text/start_ms/end_ms（可选 words、seq、speaker）的对象或 dict，
    比如 ``pybase.asr.types.Cue``。"""
    for u in utterances:
        if isinstance(u, Mapping):
            get = u.get
        else:
            def get(k, d=None, _u=u):
                return getattr(_u, k, d)
        yield from resegment(get("text"), get("start_ms"), get("end_ms"), max_chars=max_chars, max_ms=max_ms,
                             words=get("words"), seq=get("seq"), speaker=get("speaker") or "")
