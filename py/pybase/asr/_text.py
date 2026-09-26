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
# 句内停顿标点：max_duration_ms 硬切时优先在这些词后面切，读起来比硬切在词中间自然
_PAUSE_END = ("，", "、", "；", "：", ",", ";", ":")
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


def _pick_cut(current: list[Word], origin: int, max_duration_ms: int) -> int:
    """在 `current`（已经超出 max_duration_ms 的一串词）里选切点，返回切点下标
    （前半段 = current[:cut]，`cut` 至少为 1）。

    优先切在最后一个"没超限且词尾是停顿标点"的位置；找不到就切在离上限
    （origin + max_duration_ms）最近的词边界——不论是刚好没超还是刚好超一点。
    """
    limit = origin + max_duration_ms
    pause_cut: int | None = None
    nearest_cut = 1
    nearest_diff: int | None = None
    for i, word in enumerate(current, start=1):
        diff = abs(word.end - limit)
        if nearest_diff is None or diff < nearest_diff:
            nearest_diff = diff
            nearest_cut = i
        if word.end <= limit and word.text.strip().endswith(_PAUSE_END):
            pause_cut = i  # 靠后的覆盖靠前的：尽量切得晚、少切几段
    return pause_cut if pause_cut is not None else nearest_cut


def _split_by_max_duration(words: list[Word], max_duration_ms: int) -> list[list[Word]]:
    """一组词按 max_duration_ms 再切；借鉴 ai-browser emitSegments 的"找不到停顿就
    硬切"思路，但切点算法不同（这里认词边界，不认字符数/静音间隔）。

    词缺时间或 max_duration_ms 不合法就不切，原样返回——没有可靠时间没法判断超没超限。
    """
    if not words or max_duration_ms <= 0 or any(w.start is None or w.end is None for w in words):
        return [words]
    groups: list[list[Word]] = []
    remaining = words
    while remaining:
        origin = remaining[0].start
        over_at = next(
            (i for i, w in enumerate(remaining) if w.end - origin > max_duration_ms), None
        )
        if over_at is None:
            groups.append(remaining)
            break
        cut = _pick_cut(remaining[: over_at + 1], origin, max_duration_ms)
        groups.append(remaining[:cut])
        remaining = remaining[cut:]
    return groups


def segment_cues(
    text: str,
    start: int | None,
    end: int | None,
    words: Sequence[Word] = (),
    max_duration_ms: int | None = None,
) -> list[Cue]:
    """一个引擎分段变成字幕 Cue：有逐词时间就按句尾标点切开，否则整段一条。

    句子里的词缺时间时，用句内能取到的最早/最晚时间，再不行退回分段自身的时间；
    仍然没有时间的句子丢弃（Cue 必须有时间）。

    `max_duration_ms` 给了值时，切完的句子如果还超这个时长，在词边界再切一次
    （见 `_split_by_max_duration`）；不给（None）就不做这层限制——多长算"太长"是
    调用方的业务判断，本函数不内置默认值。
    """
    if not words:
        text = clean(text)
        return [Cue(start, end, text)] if text and start is not None and end is not None else []

    groups: list[list[Word]] = [[]]
    for word in words:
        groups[-1].append(word)
        if word.text.strip().endswith(_SENTENCE_END):
            groups.append([])

    if max_duration_ms is not None:
        split_groups: list[list[Word]] = []
        for group in groups:
            split_groups += _split_by_max_duration(group, max_duration_ms)
        groups = split_groups

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
