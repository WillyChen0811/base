"""分句：一段文字 → 句和小句，各带在原文里的字符起止位置。

规则（依据国标 GB/T 15834-2011《标点符号用法》的句末点号与句内点号；UAX #29 也是按标点属性断句、只到句子一级）：

- **句**在句末点号（。？！… 及半角 . ? !）和换行处断；连着的几个句末点号算一个（「？！」「……」「...」），
  紧跟在后面的右引号、右括号归前一句。
- **小句**在句内点号 ，；：（及半角 , ; :）处再断；**顿号不断**。
- **成对的引号、括号里面不拆**（“”‘’「」『』（）()【】[]《》〈〉〔〕｛｝{}﹁﹂﹃﹄〖〗［］，半角双引号按出现次序两两配对）；
  只认同一行里配上对的，落单的引号括号当普通字，不会让后面整段都不断。引号括号里以句末点号结尾、
  配对的右引号右括号后面又不是点号时，在右引号右括号后面断（「提示“保存成功。”系统返回列表。」断成两句）。
- 半角点号的例外：. 前面是数字（「1.5」「1.支持查询」）、或 . ? ! 两边都是英文字母数字（「config.json」）时不算句末；
  , 两边都是数字（「1,000」）、: 两边都是英文字母数字或后面是 /（「12:30」「http://」）时不算句内点号。
- 起止位置是 ``[start, end)``，``text == 原文[start:end]``；句与小句都去掉两头的空白，空的不出。
"""
from __future__ import annotations

import re
from dataclasses import dataclass

LINE_BREAKS = frozenset("\n\r\v\f\x1c\x1d\x1e\x85  ")
FULL_STOPS = frozenset("。？！…⋯﹒﹖﹗")          # 全角句末点号：总是断
HALF_STOPS = frozenset(".?!．")                  # 半角（及全角句点 ．）：有例外，见模块说明
CLAUSE_FULL = frozenset("，；：﹐﹔﹕")
CLAUSE_HALF = frozenset(",;:")
_PAIRS = {"“": "”", "‘": "’", "「": "」", "『": "』", "（": "）", "(": ")", "【": "】", "[": "]", "《": "》",
          "〈": "〉", "〔": "〕", "｛": "｝", "{": "}", "﹁": "﹂", "﹃": "﹄", "〖": "〗", "［": "］"}
_CLOSE_OF = {v: k for k, v in _PAIRS.items()}
_SYMMETRIC = '"'
CLOSERS = frozenset(_CLOSE_OF) | {_SYMMETRIC}
_STOPS = FULL_STOPS | HALF_STOPS
_CLAUSE = CLAUSE_FULL | CLAUSE_HALF
_FOLLOW_NO_BREAK = _STOPS | _CLAUSE | frozenset("、")   # 右引号后面跟着这些就不在右引号处断
_HANZI = re.compile(r"[㐀-䶿一-鿿豈-﫿\U00020000-\U0002ebef\U00030000-\U0003134f]")


@dataclass(frozen=True)
class Span:
    """原文里的一段：``text == 原文[start:end]``。"""

    start: int
    end: int
    text: str


@dataclass(frozen=True)
class Sentence:
    """一句：起止位置、文字，和它的小句（至少一个；没有句内点号时就是整句）。"""

    start: int
    end: int
    text: str
    clauses: tuple[Span, ...]


def count_hanzi(text: str) -> int:
    """汉字个数（CJK 统一表意文字及扩展区、兼容区）。"""
    return len(_HANZI.findall(text or ""))


def _ascii_alnum(c: str) -> bool:
    return c.isascii() and c.isalnum()


def _inside_mask(text: str) -> tuple[list[bool], set[int]]:
    """→ (inside, closers)：``inside[i]`` 为真表示第 i 个字在某对配上的引号括号里面（不含引号括号本身）；
    ``closers`` 是配上对的右引号右括号的位置。只在同一行里配对，落单的不算。"""
    n = len(text)
    diff = [0] * (n + 1)
    closers: set[int] = set()
    stack: list[tuple[str, int]] = []
    for i, c in enumerate(text):
        if c in LINE_BREAKS:
            stack = []
            continue
        if c == _SYMMETRIC:
            k = next((j for j in range(len(stack) - 1, -1, -1) if stack[j][0] == _SYMMETRIC), -1)
            if k >= 0:
                diff[stack[k][1] + 1] += 1
                diff[i] -= 1
                closers.add(i)
                del stack[k:]
            else:
                stack.append((c, i))
        elif c in _PAIRS:
            stack.append((c, i))
        elif c in _CLOSE_OF:
            want = _CLOSE_OF[c]
            k = next((j for j in range(len(stack) - 1, -1, -1) if stack[j][0] == want), -1)
            if k >= 0:
                diff[stack[k][1] + 1] += 1
                diff[i] -= 1
                closers.add(i)
                del stack[k:]
    inside, depth = [False] * n, 0
    for i in range(n):
        depth += diff[i]
        inside[i] = depth > 0
    return inside, closers


def _half_stop_exempt(text: str, i: int) -> bool:
    c = text[i]
    prev = text[i - 1] if i > 0 else ""
    nxt = text[i + 1] if i + 1 < len(text) else ""
    if c in ".．" and prev and prev.isdecimal():
        return True
    return bool(prev and nxt and _ascii_alnum(prev) and _ascii_alnum(nxt))


def _clause_exempt(text: str, i: int) -> bool:
    c = text[i]
    prev = text[i - 1] if i > 0 else ""
    nxt = text[i + 1] if i + 1 < len(text) else ""
    if c == ",":
        return bool(prev and nxt and prev.isdecimal() and nxt.isdecimal())
    if c == ":":
        return nxt == "/" or bool(prev and nxt and _ascii_alnum(prev) and _ascii_alnum(nxt))
    return False


def _strip(text: str, a: int, b: int) -> tuple[int, int]:
    while a < b and text[a].isspace():
        a += 1
    while b > a and text[b - 1].isspace():
        b -= 1
    return a, b


def _sentence_cuts(text: str, inside: list[bool], closers: set[int]) -> list[int]:
    """句的切点（每个切点是下一句开头的位置；换行处切在换行字前后各一次，由去空白收掉）。"""
    n = len(text)
    cuts: list[int] = []
    i = 0
    while i < n:
        c = text[i]
        if c in LINE_BREAKS:
            cuts.append(i)
            cuts.append(i + 1)
            i += 1
            continue
        if inside[i]:
            i += 1
            continue
        if c in _STOPS:
            j = i
            while j < n and text[j] in _STOPS and not inside[j]:
                j += 1
            if j - i == 1 and c in HALF_STOPS and _half_stop_exempt(text, i):
                i += 1
                continue
            # 句末点号后面落单的右引号右括号归前一句（半角双引号分不出开合，只在配上对时算右引号，这里不收）
            while j < n and text[j] in _CLOSE_OF:
                j += 1
            cuts.append(j)
            i = j
            continue
        if i in closers:
            # 配对的右引号右括号回到最外层：里面以句末点号结尾、后面又不是点号，就在它后面断
            j = i
            while j + 1 < n and (j + 1) in closers and not inside[j + 1]:
                j += 1
            p = i - 1
            while p >= 0 and text[p] in CLOSERS:
                p -= 1
            if p >= 0 and text[p] in _STOPS and not (text[p] in HALF_STOPS and _half_stop_exempt(text, p)):
                nxt = j + 1
                while nxt < n and text[nxt].isspace() and text[nxt] not in LINE_BREAKS:
                    nxt += 1
                if nxt < n and text[nxt] not in _FOLLOW_NO_BREAK and text[nxt] not in CLOSERS:
                    cuts.append(j + 1)
            i = j + 1
            continue
        i += 1
    return cuts


def _clause_spans(text: str, a: int, b: int, inside: list[bool]) -> tuple[Span, ...]:
    out: list[Span] = []
    start = a
    i = a
    while i < b:
        c = text[i]
        if not inside[i] and c in _CLAUSE and not _clause_exempt(text, i):
            j = i
            while j < b and text[j] in _CLAUSE and not inside[j]:
                j += 1
            s, e = _strip(text, start, j)
            if s < e:
                out.append(Span(s, e, text[s:e]))
            start = i = j
            continue
        i += 1
    s, e = _strip(text, start, b)
    if s < e:
        out.append(Span(s, e, text[s:e]))
    return tuple(out)


def sentences(text: str) -> list[Sentence]:
    """一段文字 → 句的列表（按出现先后），每句带小句。规则见模块说明。空串、全是空白 → []。"""
    if not isinstance(text, str):
        raise TypeError(f"text 要是字符串：{type(text).__name__}")
    if not text:
        return []
    inside, closers = _inside_mask(text)
    bounds = [0] + _sentence_cuts(text, inside, closers) + [len(text)]
    out: list[Sentence] = []
    for a, b in zip(bounds, bounds[1:]):
        s, e = _strip(text, a, b)
        if s < e:
            out.append(Sentence(s, e, text[s:e], _clause_spans(text, s, e, inside)))
    return out


def clauses(text: str) -> list[Span]:
    """一段文字 → 所有小句（按出现先后，跨句接起来）。"""
    return [c for s in sentences(text) for c in s.clauses]
