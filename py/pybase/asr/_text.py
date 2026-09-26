"""各引擎共用的文本整理：清掉引擎输出的多余空白、按句切分成字幕 Cue。"""
from __future__ import annotations

import re
from typing import Sequence

from ..subtitle.models import Cue
from .models import Word

# Apple 中文结果会在全角标点前多一个空格（"很好 ，"）
_SPACE_BEFORE_FULLWIDTH = re.compile(r"\s+(?=[，。！？；：、）」』》”’])")
# 只看词尾：英文 "Mr." 也会断开，基础切分不做缩写词表
_SENTENCE_END = ("。", "！", "？", "!", "?", ".", "…")
_CJK = re.compile(r"[぀-ヿ㐀-鿿가-힯　-〿＀-￯]")


def clean(text: str) -> str:
    return _SPACE_BEFORE_FULLWIDTH.sub("", text).strip()


def join(parts: Sequence[str]) -> str:
    """拼接多段文本：两侧都是中日韩字符时不加空格，否则加一个。"""
    out = ""
    for part in (p.strip() for p in parts):
        if not part:
            continue
        if out and not (_CJK.match(out[-1]) and _CJK.match(part[0])):
            out += " "
        out += part
    return out


def segment_cues(
    text: str, start: int | None, end: int | None, words: Sequence[Word] = ()
) -> list[Cue]:
    """一个引擎分段变成字幕 Cue：有逐词时间就按句尾标点切开，否则整段一条。

    句子里的词缺时间时，用句内能取到的最早/最晚时间，再不行退回分段自身的时间；
    仍然没有时间的句子丢弃（Cue 必须有时间）。
    """
    if not words:
        text = clean(text)
        return [Cue(start, end, text)] if text and start is not None and end is not None else []

    groups: list[list[Word]] = [[]]
    for word in words:
        groups[-1].append(word)
        if word.text.strip().endswith(_SENTENCE_END):
            groups.append([])

    cues = []
    for group in groups:
        sentence = clean("".join(w.text for w in group))
        starts = [w.start for w in group if w.start is not None]
        ends = [w.end for w in group if w.end is not None]
        s = min(starts) if starts else start
        e = max(ends) if ends else end
        if sentence and s is not None and e is not None:
            cues.append(Cue(s, max(s, e), sentence))
    return cues


def ms(seconds: float | None) -> int | None:
    return None if seconds is None else int(round(seconds * 1000))
