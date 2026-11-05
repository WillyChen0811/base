"""归一化（回溯校验用）：逐字 NFKC + 一张补充表 + 去掉空白，同时留偏移表，能把归一后的位置指回原文位置。

- **逐字** NFKC：每个字单独做（不做跨字的组合），原文和引文用同一套规则，两边一致即可。
  NFKC 会把全角的 ，；：？！（）、全角字母数字、全角空格转成半角；``……`` 会变成六个点（长度变了，所以要偏移表）。
- **补充表**（NFKC 不动或会变长的几类）：句号 。→ ``.``、顿号 、→ ``,``；省略号 … ⋯ ︙ 各算一个 ``…``
  （不让 NFKC 把它拉成三个点）；各种引号（“”‘’„‟〝〞〟「」『』﹁﹂﹃﹄ 及半角 ' ）→ ``"``；
  各种破折号连字符（— – ― ‒ ‐ ‑ ─ − 等）→ ``-``。半角的 ``...`` 不和 ``…`` 相等。
- **去掉空白**：``str.isspace()`` 为真的字，加上零宽字符（U+200B/200C/200D/2060/FEFF、软连字符 U+00AD）。
- 不改大小写。
"""
from __future__ import annotations

import unicodedata
from dataclasses import dataclass

_QUOTES = "“”‘’‚‛„‟〝〞〟「」『』﹁﹂﹃﹄'＂＇"
_DASHES = "‐‑‒–—―─━−﹘﹣－"
_ELLIPSES = "…⋯︙"
_TABLE: dict[str, str] = {
    "。": ".", "、": ",",
    **{c: "…" for c in _ELLIPSES},
    **{c: '"' for c in _QUOTES},
    **{c: "-" for c in _DASHES},
}
_INVISIBLE = frozenset("​‌‍⁠﻿­")


def norm_char(c: str) -> str:
    """一个字归一后的样子（可能是空串、一个字或几个字）。"""
    if c in _TABLE:
        out = _TABLE[c]
    else:
        out = "".join(_TABLE.get(x, x) for x in unicodedata.normalize("NFKC", c))
    return "".join(x for x in out if not x.isspace() and x not in _INVISIBLE)


@dataclass(frozen=True)
class Normalized:
    """归一后的文字和偏移表：``origin[i]`` 是归一后第 i 个字出自原文的第几个字。"""

    text: str
    origin: tuple[int, ...]
    source_len: int

    def to_source(self, start: int, end: int) -> tuple[int, int]:
        """归一后的 ``[start, end)`` → 原文的 ``[s, e)``（从产出第一个字的原文字到产出最后一个字的原文字，
        中间被去掉的空白也在里面）。空区间指向它后面那个字的位置。"""
        n = len(self.text)
        if not (0 <= start <= end <= n):
            raise ValueError(f"区间越界：[{start}, {end})，归一后长 {n}")
        if start == end:
            pos = self.origin[start] if start < n else self.source_len
            return pos, pos
        return self.origin[start], self.origin[end - 1] + 1


def normalize(text: str) -> Normalized:
    """一段文字 → ``Normalized``（规则见模块说明）。"""
    if not isinstance(text, str):
        raise TypeError(f"text 要是字符串：{type(text).__name__}")
    chars: list[str] = []
    origin: list[int] = []
    for i, c in enumerate(text):
        for x in norm_char(c):
            chars.append(x)
            origin.append(i)
    return Normalized("".join(chars), tuple(origin), len(text))
