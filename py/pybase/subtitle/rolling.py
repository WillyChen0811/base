"""YouTube 自动字幕的"滚动式"折叠。

YouTube 自动生成（语音识别）的 VTT 是两行滚动上屏，实测结构是交替的两种 cue：

    04.400 --> 06.869   "This is a three. It's sloppily written"          ← 正文
    06.869 --> 06.879   "This is a three. It's sloppily written"          ← 10ms 过渡
    06.879 --> 08.549   "This is a three. It's sloppily written\\nand rendered at ..."
                         ↑ 上一句被顶到第一行          ↑ 这一条真正新增的内容

相邻 cue 首尾相接、**时间上并不重叠**，所以不能拿"时间重叠"当信号。

算法照 srt_fix（https://github.com/bindestriche/srt_fix，`dedupe_yt_srt`，
Unlicense）：极短且文本已含在上一条里的过渡 cue 并进上一条；本条首行等于上一条
末行时丢掉首行。只有 YouTube 自动字幕需要这一步，网站自带/人工字幕没有这个问题。
"""
from __future__ import annotations

from dataclasses import replace
from typing import Iterable

from .models import Cue

# 过渡 cue 实测 10ms；srt_fix 用 150ms 做阈值，留足余量
_TRANSITION_MS = 150


def _is_transition(prev: Cue, cur: Cue) -> bool:
    return cur.end - cur.start < _TRANSITION_MS and cur.text in prev.text


def _carries_over(prev: Cue, cur: Cue) -> bool:
    """本条首行是不是上一条末行被顶上来的。"""
    return cur.text.split("\n")[0] == prev.text.split("\n")[-1]


def looks_rolling(cues: Iterable[Cue]) -> bool:
    """像不像 YouTube 自动字幕：过半相邻对是"过渡 cue"或"首行承接上一条末行"。

    正常字幕偶尔也会有一句台词原样重复，但不会过半；少于 3 条不判。
    """
    values = [c for c in cues if c.text]
    if len(values) < 3:
        return False
    hits = sum(
        1 for prev, cur in zip(values, values[1:])
        if _is_transition(prev, cur) or _carries_over(prev, cur)
    )
    return hits * 2 >= len(values) - 1


def collapse_rolling(cues: Iterable[Cue]) -> list[Cue]:
    """把滚动式字幕折叠成每条只含新内容的普通字幕。

    `looks_rolling` 判 False 时原样返回——普通字幕绝不能被这一步改动。
    """
    values = sorted((c for c in cues if c.text), key=lambda c: c.start)
    if not looks_rolling(values):
        return values

    result: list[Cue] = []
    prev: Cue | None = None  # 上一条原始 cue（比较用原文，不用折叠后的）
    for cue in values:
        if prev is not None and result and _is_transition(prev, cue):
            last = result[-1]
            result[-1] = replace(last, end=cue.end, original_end=cue.original_end,
                                 user_edited=last.user_edited or cue.user_edited)
            continue  # prev 不前移：过渡 cue 的文本是上一条的子集
        text = cue.text
        if prev is not None and _carries_over(prev, cue):
            text = "\n".join(cue.text.split("\n")[1:])
        prev = cue
        if text.strip():
            # 折叠后的文本就是这条的"原文"：修正锚点建立在折叠之后
            result.append(replace(cue, text=text, original_text=text))
        elif result:  # 整条都是承接下来的旧行：只延长上一条
            last = result[-1]
            result[-1] = replace(last, end=cue.end, original_end=cue.original_end,
                                 user_edited=last.user_edited or cue.user_edited)
    return result
