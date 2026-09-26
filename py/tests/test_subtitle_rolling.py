"""覆盖 pybase.subtitle 的滚动字幕（YouTube 自动生成 VTT）识别与折叠。"""
from __future__ import annotations

from pybase.subtitle import Cue, collapse_rolling, looks_rolling


def _rolling_sample() -> list[Cue]:
    """手造一份典型的 YouTube 自动字幕滚动模式：2 行窗口，旧行上移、新行下探，
    相邻 cue 大量时间重叠、文本行互相重叠。"""
    return [
        Cue(start=0, end=2000, text="hello there"),
        Cue(start=1800, end=4000, text="hello there\nwelcome back"),
        Cue(start=3800, end=6000, text="welcome back\nto the show"),
        Cue(start=5800, end=7000, text="to the show"),
    ]


def _normal_sample() -> list[Cue]:
    """普通字幕：互不重叠、文本各不相同。"""
    return [
        Cue(start=0, end=1000, text="First line."),
        Cue(start=1200, end=2200, text="Second line."),
        Cue(start=2400, end=3400, text="Third line."),
    ]


def _bilingual_overlap_sample() -> list[Cue]:
    """双语轨：时间大量重叠，但文本完全不同——不该被判成滚动字幕。"""
    return [
        Cue(start=0, end=2000, text="Hello there"),
        Cue(start=0, end=2000, text="你好"),
        Cue(start=2000, end=4000, text="Welcome back"),
        Cue(start=2000, end=4000, text="欢迎回来"),
    ]


def test_looks_rolling_true_for_youtube_style_sample():
    assert looks_rolling(_rolling_sample()) is True


def test_looks_rolling_false_for_normal_subtitle():
    assert looks_rolling(_normal_sample()) is False


def test_looks_rolling_false_for_overlapping_bilingual_tracks():
    assert looks_rolling(_bilingual_overlap_sample()) is False


def test_looks_rolling_false_for_too_few_cues():
    assert looks_rolling([Cue(start=0, end=1000, text="a")]) is False


def test_collapse_rolling_leaves_normal_subtitle_untouched():
    cues = _normal_sample()
    assert collapse_rolling(cues) == cues


def test_collapse_rolling_leaves_bilingual_overlap_untouched():
    cues = _bilingual_overlap_sample()
    assert collapse_rolling(cues) == cues


def test_collapse_rolling_collapses_youtube_style_sample():
    collapsed = collapse_rolling(_rolling_sample())
    texts = [c.text for c in collapsed]
    # 4 条滚动 cue 折叠成 3 条互不重复的行，每行只出现一次
    assert texts == ["hello there", "welcome back", "to the show"]
    assert len(texts) == len(set(texts))
    # 每条折叠后的时间跨度覆盖了它在原始滚动字幕里出现的完整区间
    by_text = {c.text: c for c in collapsed}
    assert by_text["hello there"].start == 0
    assert by_text["to the show"].end == 7000


def test_collapse_rolling_empty_input():
    assert collapse_rolling([]) == []


# 结构照 yt-dlp 下载的 YouTube 自动字幕原样构造（逐词 <c> 标签、只有空格的行、
# align 属性、10ms 过渡 cue、首尾相接不重叠），文本是自编的。
_REAL_STRUCTURE_VTT = """WEBVTT
Kind: captions
Language: en

00:00:00.000 --> 00:00:02.390 align:start position:0%
 
[Music]

00:00:02.390 --> 00:00:02.400 align:start position:0%
 
 

00:00:02.400 --> 00:00:04.869 align:start position:0%
 
Today<00:00:02.799><c> we</c><00:00:02.960><c> look</c><00:00:03.200><c> at</c><00:00:03.920><c> trees</c>

00:00:04.869 --> 00:00:04.879 align:start position:0%
Today we look at trees
 

00:00:04.879 --> 00:00:06.549 align:start position:0%
Today we look at trees
and<00:00:05.120><c> how</c><00:00:05.440><c> they</c><00:00:05.680><c> grow</c>

00:00:06.549 --> 00:00:06.559 align:start position:0%
and how they grow
 

00:00:06.559 --> 00:00:08.430 align:start position:0%
and how they grow
over<00:00:07.040><c> many</c><00:00:07.360><c> years</c>
"""


def test_collapse_rolling_real_youtube_structure():
    from pybase.subtitle import load

    cues = load(_REAL_STRUCTURE_VTT, format="vtt")
    assert looks_rolling(cues)
    assert [(c.start, c.end, c.text) for c in collapse_rolling(cues)] == [
        (0, 2390, "[Music]"),
        (2400, 4879, "Today we look at trees"),
        (4879, 6559, "and how they grow"),
        (6559, 8430, "over many years"),
    ]
