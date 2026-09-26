"""覆盖 pybase.subtitle 的模型/读写/时间校准/索引/大模型格式化。

滚动字幕折叠单独放在 test_subtitle_rolling.py。
"""
from __future__ import annotations

import sys

import pytest

from pybase.subtitle import Cue, CueIndex, chunk, dumps, load, reset_timing, save, shift, to_timestamped_text
from pybase.subtitle import io_ as subtitle_io


# ---------------------------------------------------------------------------
# models
# ---------------------------------------------------------------------------


def test_cue_defaults_original_to_start_end():
    cue = Cue(start=1000, end=2000, text="hi")
    assert cue.original_start == 1000
    assert cue.original_end == 2000


def test_cue_explicit_original_kept():
    cue = Cue(start=1500, end=2500, text="hi", original_start=1000, original_end=2000)
    assert cue.original_start == 1000
    assert cue.original_end == 2000


def test_cue_is_immutable():
    cue = Cue(start=0, end=1000, text="hi")
    with pytest.raises(Exception):
        cue.text = "bye"  # type: ignore[misc]


# ---------------------------------------------------------------------------
# io_: load / dumps / save
# ---------------------------------------------------------------------------

_SRT = """1
00:00:01,000 --> 00:00:02,000
<i>Hello</i> world

2
00:00:03,000 --> 00:00:02,500
This end-before-start line must be dropped

3
00:00:04,000 --> 00:00:05,000


4
00:00:06,000 --> 00:00:07,000
Duplicate line

5
00:00:06,000 --> 00:00:07,000
Duplicate line

6
00:00:01,000 --> 00:00:02,000
Hello world
"""


def test_load_srt_text_cleans_and_sorts():
    cues = load(_SRT, format="srt")
    # 空文本(块3)、end<=start(块2) 被丢弃；块1和块6文本相同但不同来源条目——
    # 巧的是 start/end 也相同，属于"完全相同的重复条目"，只留一条。
    assert [c.text for c in cues] == ["Hello world", "Duplicate line"]
    assert cues[0].start == 1000 and cues[0].end == 2000
    # 按 start 排好序
    assert [c.start for c in cues] == sorted(c.start for c in cues)


def test_load_strips_style_tags_from_srt():
    cues = load(_SRT, format="srt")
    assert "<i>" not in cues[0].text and "</i>" not in cues[0].text


_VTT = """WEBVTT

00:00:01.000 --> 00:00:02.000
hey<00:00:01.500><c> everyone</c> welcome

00:00:03.000 --> 00:00:04.000
<v Speaker>second line</v>

"""


def test_load_vtt_strips_inline_cue_tags():
    cues = load(_VTT, format="vtt")
    assert cues[0].text == "hey everyone welcome"
    assert cues[1].text == "second line"


def test_load_from_file_path(tmp_path):
    path = tmp_path / "sample.srt"
    path.write_text(_SRT, encoding="utf-8")
    cues = load(path)
    assert [c.text for c in cues] == ["Hello world", "Duplicate line"]


def test_load_format_autodetect_from_extension(tmp_path):
    path = tmp_path / "sample.vtt"
    path.write_text(_VTT, encoding="utf-8")
    cues = load(path)  # 不传 format，靠扩展名推断
    assert cues[0].text == "hey everyone welcome"


def test_dumps_and_load_roundtrip():
    cues = [Cue(start=0, end=1000, text="line one"), Cue(start=1200, end=2000, text="line two")]
    text = dumps(cues, format="srt")
    assert "line one" in text and "line two" in text
    reloaded = load(text, format="srt")
    assert [c.text for c in reloaded] == ["line one", "line two"]
    assert [c.start for c in reloaded] == [0, 1200]


def test_save_infers_format_from_suffix(tmp_path):
    cues = [Cue(start=0, end=1000, text="hello")]
    out = save(cues, tmp_path / "out.vtt")
    assert out.exists()
    assert "WEBVTT" in out.read_text(encoding="utf-8")


def test_save_unknown_suffix_without_format_raises(tmp_path):
    with pytest.raises(ValueError):
        save([Cue(start=0, end=1000, text="hello")], tmp_path / "out.weird")


def test_missing_pysubs2_raises_clear_error(monkeypatch):
    monkeypatch.setitem(sys.modules, "pysubs2", None)
    with pytest.raises(ImportError, match="pybase\\[subtitle\\]"):
        subtitle_io._pysubs2()


def test_load_empty_text_returns_empty_list():
    assert load("", format="srt") == []


# ---------------------------------------------------------------------------
# io_: strict timestamp validation
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "content",
    [
        # 缺时分：SRT 必须有 HH:MM:SS
        "1\n00:01,000 --> 00:02,000\nx\n",
        # 用了点号而不是逗号
        "1\n00:00:01.000 --> 00:00:02.000\nx\n",
        # 秒 >= 60
        "1\n00:00:99,000 --> 00:01:40,000\nx\n",
        # 分钟 >= 60
        "1\n00:99:00,000 --> 01:40:00,000\nx\n",
        # 毫秒不是三位
        "1\n00:00:01,00 --> 00:00:02,000\nx\n",
    ],
)
def test_load_srt_rejects_malformed_or_out_of_range_timestamps(content):
    with pytest.raises(ValueError, match="Invalid subtitle timestamp"):
        load(content, format="srt")


def test_load_vtt_accepts_minute_form_but_rejects_out_of_range():
    good = "WEBVTT\n\n00:01.000 --> 00:02.000\nx\n"
    assert [c.text for c in load(good, format="vtt")] == ["x"]

    bad = "WEBVTT\n\n00:99.000 --> 01:40.000\nx\n"
    with pytest.raises(ValueError, match="Invalid subtitle timestamp"):
        load(bad, format="vtt")


@pytest.mark.parametrize(
    "content",
    [
        # 用了逗号而不是点号
        "WEBVTT\n\n00:00:01,000 --> 00:00:02,000\nx\n",
        # 秒 >= 60
        "WEBVTT\n\n00:00:99.000 --> 00:01:40.000\nx\n",
        # 毫秒不是三位
        "WEBVTT\n\n00:00:01.00 --> 00:00:02.000\nx\n",
    ],
)
def test_load_vtt_rejects_malformed_or_out_of_range_timestamps(content):
    with pytest.raises(ValueError, match="Invalid subtitle timestamp"):
        load(content, format="vtt")


def test_load_strict_false_falls_back_to_lenient_pysubs2_behaviour():
    # 严格模式下拒绝，但 strict=False 时按 pysubs2 原本的宽松规则放行（不校验范围）
    bad = "1\n00:99:00,000 --> 01:40:00,000\nx\n"
    cues = load(bad, format="srt", strict=False)
    assert len(cues) == 1


def test_load_vtt_cue_settings_after_timestamp_still_pass_strict_validation():
    """VTT 时间行后面允许跟 cue settings（如 align/position），不能被当成格式错误。"""
    content = (
        "WEBVTT\n\n"
        "00:00:00.000 --> 00:00:02.390 align:start position:0%\nhello\n"
    )
    assert [c.text for c in load(content, format="vtt")] == ["hello"]


def test_load_srt_hour_over_99_is_still_accepted():
    # 超长视频小时数可以超过两位，不该被拒绝
    content = "1\n100:00:00,000 --> 100:00:01,000\nx\n"
    assert [c.text for c in load(content, format="srt")] == ["x"]


# ---------------------------------------------------------------------------
# timing: shift / reset_timing
# ---------------------------------------------------------------------------


def test_shift_moves_start_end_keeps_original():
    cues = [Cue(start=1000, end=2000, text="a")]
    shifted = shift(cues, 500)
    assert shifted[0].start == 1500 and shifted[0].end == 2500
    assert shifted[0].original_start == 1000
    assert shifted[0].original_end == 2000


def test_shift_negative_within_bounds():
    cues = [Cue(start=1000, end=2000, text="a")]
    shifted = shift(cues, -500)
    assert shifted[0].start == 500 and shifted[0].end == 1500


def test_shift_negative_is_exact_and_round_trips():
    cues = [Cue(start=200, end=1000, text="a"), Cue(start=1500, end=2000, text="b")]
    shifted = shift(cues, -500)
    assert [(c.start, c.end) for c in shifted] == [(-300, 500), (1000, 1500)]
    assert shift(shifted, 500) == cues


def test_dumps_drops_and_clamps_negative_times():
    cues = [Cue(start=-900, end=-100, text="gone"), Cue(start=-300, end=500, text="cut"),
            Cue(start=1000, end=1500, text="kept")]
    assert [(c.start, c.end, c.text) for c in load(dumps(cues, "srt"), format="srt")] == [
        (0, 500, "cut"), (1000, 1500, "kept"),
    ]


def test_load_missing_subtitle_path_raises(tmp_path):
    with pytest.raises(FileNotFoundError):
        load(str(tmp_path / "missing.srt"))


def test_chunk_end_covers_overlapping_cues():
    cues = [Cue(start=0, end=9000, text="long"), Cue(start=1000, end=2000, text="short")]
    assert chunk(cues, max_chars=1000)[0].end == 9000


def test_shift_empty_list():
    assert shift([], 100) == []


def test_reset_timing_restores_original():
    cues = [Cue(start=1000, end=2000, text="a")]
    twice_shifted = shift(shift(cues, 500), 300)
    restored = reset_timing(twice_shifted)
    assert restored[0].start == 1000 and restored[0].end == 2000
    assert restored[0].original_start == 1000 and restored[0].original_end == 2000


# ---------------------------------------------------------------------------
# CueIndex
# ---------------------------------------------------------------------------


def _sample_cues():
    return [
        Cue(start=0, end=1000, text="a"),
        Cue(start=1000, end=2000, text="b"),
        Cue(start=5000, end=6000, text="c"),
        # 双语场景：跟 c 时间重叠的第二轨
        Cue(start=5200, end=5800, text="c-translation"),
    ]


def test_cue_index_at_showing_overlap():
    idx = CueIndex(_sample_cues())
    slice_ = idx.at(5300)
    assert {c.text for c in slice_.showing} == {"c", "c-translation"}


def test_cue_index_at_gap_last_and_next():
    idx = CueIndex(_sample_cues())
    slice_ = idx.at(3000)  # 落在 b 结束和 c 开始之间的空档
    assert slice_.showing == []
    assert slice_.last_shown.text == "b"
    assert slice_.next_to_show.text == "c"


def test_cue_index_at_before_first_cue():
    idx = CueIndex(_sample_cues())
    slice_ = idx.at(-100)
    assert slice_.showing == []
    assert slice_.last_shown is None
    assert slice_.next_to_show.text == "a"


def test_cue_index_at_after_last_cue():
    idx = CueIndex(_sample_cues())
    slice_ = idx.at(999_999)
    assert slice_.showing == []
    assert slice_.next_to_show is None
    assert slice_.last_shown.text in ("c", "c-translation")


def test_cue_index_between():
    idx = CueIndex(_sample_cues())
    result = idx.between(900, 5100)
    assert {c.text for c in result} == {"a", "b", "c"}


def test_cue_index_gaps_respects_min_ms():
    idx = CueIndex(_sample_cues())
    assert idx.gaps(1000) == [(2000, 5000)]
    assert idx.gaps(10_000) == []


def test_cue_index_gaps_empty_for_no_cues():
    assert CueIndex([]).gaps(100) == []


def test_cue_index_surrounding():
    cues = _sample_cues()
    idx = CueIndex(cues)
    target = cues[2]  # "c"
    ctx = idx.surrounding(target, before=1, after=1)
    assert [c.text for c in ctx] == ["b", "c", "c-translation"]


def test_cue_index_surrounding_clamped_at_edges():
    cues = _sample_cues()
    idx = CueIndex(cues)
    ctx = idx.surrounding(cues[0], before=5, after=0)
    assert [c.text for c in ctx] == ["a"]


def test_cue_index_surrounding_unknown_cue_returns_empty():
    idx = CueIndex(_sample_cues())
    unknown = Cue(start=999_000, end=999_500, text="not indexed")
    assert idx.surrounding(unknown, 1, 1) == []


# ---------------------------------------------------------------------------
# llm: to_timestamped_text / chunk
# ---------------------------------------------------------------------------


def test_to_timestamped_text_minutes_seconds():
    cues = [Cue(start=65_000, end=66_000, text="hi")]
    assert to_timestamped_text(cues) == "[01:05] hi"


def test_to_timestamped_text_hours():
    cues = [Cue(start=3_661_000, end=3_662_000, text="hi")]
    assert to_timestamped_text(cues) == "[1:01:01] hi"


def test_to_timestamped_text_multiline_folds_to_space():
    cues = [Cue(start=0, end=1000, text="line1\nline2")]
    assert to_timestamped_text(cues) == "[00:00] line1 line2"


def test_chunk_empty_input():
    assert chunk([], max_chars=100) == []


def test_chunk_packs_within_budget():
    cues = [Cue(start=i * 1000, end=i * 1000 + 500, text=f"cue{i}") for i in range(5)]
    chunks = chunk(cues, max_chars=40)
    assert sum(len(c.cues) for c in chunks) == 5
    for part in chunks:
        assert part.start == part.cues[0].start
        assert part.end == part.cues[-1].end


def test_chunk_does_not_split_single_long_cue():
    long_text = "x" * 500
    cues = [Cue(start=0, end=1000, text="short"), Cue(start=1000, end=2000, text=long_text)]
    chunks = chunk(cues, max_chars=50)
    # 长 cue 单独成块，不会被拆开、也不报错
    assert any(len(c.cues) == 1 and c.cues[0].text == long_text for c in chunks)
