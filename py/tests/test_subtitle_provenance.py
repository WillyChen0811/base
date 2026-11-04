"""覆盖 pybase.subtitle 的来源（provenance）与用户修正层。"""
from __future__ import annotations

import json

import pytest

from pybase.subtitle import Cue, reset_timing, shift
from pybase.subtitle.provenance import (
    Anchor,
    Correction,
    TrackMetadata,
    apply_corrections,
    detect_origin,
    export,
    load_meta,
    meta_path,
    record_origin,
    remove_correction,
    save_meta,
)


# ---------------------------------------------------------------------------
# 来源历史：优先级 + 历史保留
# ---------------------------------------------------------------------------


def test_effective_origin_none_without_history():
    assert TrackMetadata().effective_origin is None


def test_effective_origin_prefers_user_over_acquired_and_detected():
    track = TrackMetadata()
    track = record_origin(track, "platform_asr", "detected", at="t1")
    track = record_origin(track, "uploaded", "acquired", at="t2")
    track = record_origin(track, "local_asr", "user", at="t3")
    assert track.effective_origin == "local_asr"
    # 历史全保留，不覆盖
    assert len(track.history) == 3
    assert track.history[0].origin == "platform_asr"


def test_effective_origin_detected_evidence_beats_platform_claim():
    """X 平台把识别结果当普通字幕给：下载器记 uploaded，内容里有识别特征，信证据。"""
    track = TrackMetadata()
    track = record_origin(track, "uploaded", "acquired", at="t1")
    track = record_origin(track, "platform_asr", "detected", at="t2")
    assert track.effective_origin == "platform_asr"


def test_effective_origin_user_wins_even_if_recorded_before_detected():
    """user 判断即便记录在前，后来又来一条 detected，也不该被盖过去。"""
    track = TrackMetadata()
    track = record_origin(track, "uploaded", "user", at="t1")
    track = record_origin(track, "platform_asr", "detected", at="t2")
    assert track.effective_origin == "uploaded"


def test_effective_origin_same_priority_takes_latest():
    track = TrackMetadata()
    track = record_origin(track, "uploaded", "user", at="t1")
    track = record_origin(track, "local_asr", "user", at="t2")
    assert track.effective_origin == "local_asr"


# ---------------------------------------------------------------------------
# detect_origin
# ---------------------------------------------------------------------------

_YOUTUBE_SAMPLE = """WEBVTT

00:00:00.400 --> 00:00:02.869
<00:00:00.400><c> this</c><00:00:00.700><c> is</c><00:00:01.100><c> a</c> test

00:00:02.869 --> 00:00:02.879
this is a test

00:00:02.879 --> 00:00:04.549
this is a test
<00:00:02.879><c> and</c><00:00:03.200><c> more</c>
"""

_X_SAMPLE = """WEBVTT

00:00:00.000 --> 00:00:02.000
Hello<00:00:00.500><X-word-ms 500> there

00:00:02.000 --> 00:00:04.000
General Kenobi
"""

_PLAIN_SRT_SAMPLE = """1
00:00:00,000 --> 00:00:02,000
Hello there.

2
00:00:02,500 --> 00:00:05,000
Nice to meet you.

3
00:00:05,500 --> 00:00:08,000
Goodbye now.
"""


def test_detect_origin_youtube_word_timestamp():
    evidence, origin = detect_origin(_YOUTUBE_SAMPLE)
    assert origin == "platform_asr"
    assert evidence and evidence[0].signal in ("youtube_word_timestamp", "rolling_structure")


def test_detect_origin_x_word_ms_tag():
    evidence, origin = detect_origin(_X_SAMPLE)
    assert origin == "platform_asr"
    assert evidence[0].signal == "x_word_ms_tag"


def test_detect_origin_plain_srt_returns_none():
    evidence, origin = detect_origin(_PLAIN_SRT_SAMPLE)
    assert origin is None
    assert evidence == []


# ---------------------------------------------------------------------------
# 修正层
# ---------------------------------------------------------------------------


def _cues() -> list[Cue]:
    return [
        Cue(start=0, end=2000, text="Hello, this is QuadCode."),
        Cue(start=2000, end=4000, text="Nice to meet you."),
        Cue(start=4000, end=6000, text="Goodbye now."),
    ]


def test_apply_corrections_fixes_text_and_marks_user_edited():
    cues = _cues()
    correction = Correction(
        anchor=Anchor(original_start=0, original_end=2000, text="Hello, this is QuadCode."),
        at="t1",
        new_text="Hello, this is Claude Code.",
    )
    result, orphans = apply_corrections(cues, [correction])
    assert orphans == []
    assert result[0].text == "Hello, this is Claude Code."
    assert result[0].user_edited is True
    assert result[0].start == 0 and result[0].end == 2000
    # 没被改的 cue 原样保留，也不会被标记
    assert result[1] == cues[1]
    assert result[1].user_edited is False


def test_apply_corrections_can_adjust_timing():
    cues = _cues()
    correction = Correction(
        anchor=Anchor(original_start=2000, original_end=4000, text="Nice to meet you."),
        at="t1",
        new_start=2100,
        new_end=3900,
    )
    result, orphans = apply_corrections(cues, [correction])
    assert orphans == []
    assert result[1].start == 2100
    assert result[1].end == 3900
    assert result[1].text == "Nice to meet you."


def test_apply_corrections_orphan_when_version_changed():
    cues = _cues()
    correction = Correction(
        anchor=Anchor(original_start=0, original_end=2000, text="这句在新版本里已经不存在了"),
        at="t1",
        new_text="不会被用到",
    )
    result, orphans = apply_corrections(cues, [correction])
    assert orphans == [correction]
    assert result == cues  # 没有任何改动


def test_apply_corrections_same_anchor_keeps_latest_only():
    cues = _cues()
    anchor = Anchor(original_start=0, original_end=2000, text="Hello, this is QuadCode.")
    first = Correction(anchor=anchor, at="t1", new_text="Hello, this is Cloud Code.")
    second = Correction(anchor=anchor, at="t2", new_text="Hello, this is Claude Code.")
    result, orphans = apply_corrections(cues, [first, second])
    assert orphans == []
    assert result[0].text == "Hello, this is Claude Code."


def test_remove_correction_undoes_by_id():
    anchor = Anchor(original_start=0, original_end=2000, text="Hello, this is QuadCode.")
    correction = Correction(anchor=anchor, at="t1", new_text="Hello, this is Claude Code.")
    corrections = [correction]
    remaining = remove_correction(corrections, correction.id)
    assert remaining == []
    # 撤销之后应用，字幕恢复原样
    result, orphans = apply_corrections(_cues(), remaining)
    assert result == _cues()
    assert orphans == []


def test_corrections_still_anchor_after_shift():
    """shift 之后 start/end 变了，但 original_* 不变，修正照样能定位到。"""
    cues = shift(_cues(), 5000)
    correction = Correction(
        anchor=Anchor(original_start=0, original_end=2000, text="Hello, this is QuadCode."),
        at="t1",
        new_text="Hello, this is Claude Code.",
    )
    result, orphans = apply_corrections(cues, [correction])
    assert orphans == []
    fixed = result[0]
    assert fixed.text == "Hello, this is Claude Code."
    assert fixed.start == 5000  # shift 后的时间没被修正动到
    assert fixed.original_start == 0
    # reset_timing 之后仍然是修正过的文本，original_* 保持
    restored = reset_timing(result)
    assert restored[0].text == "Hello, this is Claude Code."
    assert restored[0].start == 0


# ---------------------------------------------------------------------------
# 旁车文件
# ---------------------------------------------------------------------------


def test_meta_path_appends_suffix(tmp_path):
    srt = tmp_path / "video.srt"
    assert meta_path(srt) == tmp_path / "video.srt.meta.json"


def test_load_meta_missing_file_returns_empty(tmp_path):
    meta = load_meta(tmp_path / "nope.srt")
    assert meta.track.effective_origin is None
    assert meta.corrections == ()


def test_save_and_load_meta_roundtrip(tmp_path):
    srt = tmp_path / "video.srt"
    srt.write_text(_PLAIN_SRT_SAMPLE, encoding="utf-8")

    track = TrackMetadata(language="en", platform="youtube", created_at="2026-01-01")
    track = record_origin(track, "platform_asr", "acquired", at="2026-01-01", note="来自下载器元数据")
    correction = Correction(
        anchor=Anchor(original_start=0, original_end=2000, text="Hello there."),
        at="2026-01-02",
        new_text="Hello, there!",
    )
    from pybase.subtitle.provenance import Meta

    meta = Meta(track=track, corrections=(correction,))
    path = save_meta(srt, meta)
    assert path == meta_path(srt)

    loaded = load_meta(srt)
    assert loaded.track.language == "en"
    assert loaded.track.effective_origin == "platform_asr"
    assert loaded.track.history[0].note == "来自下载器元数据"
    assert len(loaded.corrections) == 1
    assert loaded.corrections[0].new_text == "Hello, there!"
    assert loaded.corrections[0].id == correction.id

    # 原字幕文件内容不受影响
    assert srt.read_text(encoding="utf-8") == _PLAIN_SRT_SAMPLE


def test_meta_offset_ms_roundtrips_and_defaults_to_zero(tmp_path):
    from pybase.subtitle.provenance import Meta

    srt = tmp_path / "video.srt"
    assert load_meta(srt).offset_ms == 0  # 没有旁车文件时的默认值

    save_meta(srt, Meta(offset_ms=-350))
    assert load_meta(srt).offset_ms == -350


def test_load_meta_missing_offset_field_defaults_to_zero(tmp_path):
    # 旧版本写的旁车文件没有 offset_ms 字段，读回来不能报错，也不能当成有偏移
    srt = tmp_path / "video.srt"
    meta_path(srt).write_text('{"schema_version": 1, "track": {}, "corrections": []}', encoding="utf-8")
    assert load_meta(srt).offset_ms == 0


def test_save_meta_is_atomic_no_leftover_tmp_file(tmp_path):
    srt = tmp_path / "video.srt"
    save_meta(srt, load_meta(srt))
    leftovers = [p for p in tmp_path.iterdir() if ".tmp-" in p.name]
    assert leftovers == []
    assert meta_path(srt).exists()


def test_meta_json_is_plain_readable_json(tmp_path):
    srt = tmp_path / "video.srt"
    track = record_origin(TrackMetadata(language="en"), "uploaded", "user", at="t1")
    save_meta(srt, load_meta(srt).__class__(track=track))
    raw = json.loads(meta_path(srt).read_text(encoding="utf-8"))
    assert raw["schema_version"] == 1
    assert raw["track"]["language"] == "en"
    assert raw["track"]["history"][0]["by"] == "user"


# ---------------------------------------------------------------------------
# export
# ---------------------------------------------------------------------------


def test_export_saves_corrected_copy_without_touching_original(tmp_path):
    src = tmp_path / "video.srt"
    src.write_text(_PLAIN_SRT_SAMPLE, encoding="utf-8")
    out = tmp_path / "video.fixed.srt"

    correction = Correction(
        anchor=Anchor(original_start=0, original_end=2000, text="Hello there."),
        at="t1",
        new_text="Hi there!",
    )
    saved_path, orphans = export(_cues_from_plain_srt(), [correction], out)
    assert orphans == []
    assert saved_path == out
    content = out.read_text(encoding="utf-8")
    assert "Hi there!" in content
    # 原文件没被动过
    assert src.read_text(encoding="utf-8") == _PLAIN_SRT_SAMPLE


def _cues_from_plain_srt() -> list[Cue]:
    return [
        Cue(start=0, end=2000, text="Hello there."),
        Cue(start=2500, end=5000, text="Nice to meet you."),
        Cue(start=5500, end=8000, text="Goodbye now."),
    ]


def test_second_edit_of_same_cue_still_anchors_to_original_text():
    from pybase.subtitle import anchor_of

    raw = [Cue(start=1000, end=3000, text="Hello, this is QuadCode.")]
    first = Correction(anchor=anchor_of(raw[0]), at="t1", new_text="Hello, this is Claude Code.")
    once, _ = apply_corrections(raw, [first])
    # 界面拿到的是改过的 cue，再改一次：锚点仍须是原文
    second = Correction(anchor=anchor_of(once[0]), at="t2", new_text="Hello, this is Claude Code!")
    twice, orphans = apply_corrections(raw, [first, second])
    assert orphans == []
    assert twice[0].text == "Hello, this is Claude Code!"
    assert twice[0].original_text == "Hello, this is QuadCode."


def test_load_meta_refuses_newer_schema(tmp_path):
    sub = tmp_path / "a.srt"
    meta_path(sub).write_text('{"schema_version": 99}', encoding="utf-8")
    with pytest.raises(ValueError):
        load_meta(sub)


def test_meta_dict_roundtrip_and_newer_schema():
    from pybase.subtitle import Meta, meta_from_dict, meta_to_dict
    meta = Meta(offset_ms=1200)
    assert meta_from_dict(meta_to_dict(meta)) == meta
    with pytest.raises(ValueError):
        meta_from_dict({"schema_version": 99})
