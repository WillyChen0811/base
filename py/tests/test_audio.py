"""pybase.audio 的纯逻辑测试：信号全是 numpy 现场合成的（tests/audio_synth.py），
不碰 ffmpeg、不碰任何真实录音，本机就能跑。

碰 ffmpeg 的（decode_pcm、端到端）在 test_audio_ffmpeg.py。两小时长轨的对齐耗时
只在设了 PYBASE_LONG_AUDIO_TEST=1 时跑（Studio 的 py/testenv.json 里设了）。
"""
from __future__ import annotations

import os
import time

import pytest

np = pytest.importorskip("numpy")

from audio_synth import (  # noqa: E402
    G_SELF_MIC, G_SELF_MIX, REMOTE, SELF, SR, TRUTH, coverage, make_long_meeting,
    make_meeting, report,
)
from pybase.audio import (  # noqa: E402
    AlignError, align_detail, align_offset, attribute, auto_threshold_db,
    frame_energy_db, speech_segments,
)
from pybase.audio.vad import grid_power  # noqa: E402


def _merge(segs, gap=200):
    out = []
    for s, e in sorted(segs):
        if out and s - out[-1][1] < gap:
            out[-1][1] = max(out[-1][1], e)
        else:
            out.append([s, e])
    return [tuple(x) for x in out]


def _assert_segments(got, want, tol=60):
    assert len(got) == len(want), (got, want)
    for (gs, ge), (ws, we) in zip(got, want):
        assert abs(gs - ws) <= tol and abs(ge - we) <= tol, ((gs, ge), (ws, we))


# ---------------------------------------------------------------- VAD

@pytest.mark.parametrize("seed", [7, 1, 2])
def test_vad_mix_matches_truth(seed):
    m = make_meeting(-40, seed=seed)
    want = _merge(SELF + REMOTE)
    _assert_segments(speech_segments(m.mix, m.sr), want)


@pytest.mark.parametrize("seed", [7, 1, 2])
def test_vad_headphone_mic_is_self_only(seed):
    m = make_meeting(-40, seed=seed)
    want = [(s - m.offset_ms, e - m.offset_ms) for s, e in SELF]
    _assert_segments(speech_segments(m.mic, m.sr), want)


def test_vad_silence_and_stationary_noise_give_nothing():
    rng = np.random.default_rng(0)
    assert speech_segments(np.zeros(SR * 5, dtype=np.float32), SR) == []
    noise = (0.01 * rng.standard_normal(SR * 5)).astype(np.float32)
    assert speech_segments(noise, SR) == []
    assert auto_threshold_db(frame_energy_db(noise, SR)) is None


def test_vad_merges_short_gaps_and_drops_short_bursts():
    t = np.arange(int(SR * 6)) / SR
    x = np.zeros_like(t)
    tone = 0.3 * np.sin(2 * np.pi * 440 * t)
    on = ((t >= 1.0) & (t < 2.0)) | ((t >= 2.15) & (t < 3.0)) | ((t >= 4.0) & (t < 4.1))
    x[on] = tone[on]
    x += 1e-4 * np.random.default_rng(1).standard_normal(len(t))
    segs = speech_segments(x.astype(np.float32), SR)
    # 150 ms 空隙并成一段；100 ms 的短声丢掉
    _assert_segments(segs, [(1000, 3000)], tol=30)
    # 空隙放宽到 100 ms 以下才并：两段分开
    _assert_segments(speech_segments(x, SR, min_gap_ms=100), [(1000, 2000), (2150, 3000)], tol=30)
    # 显式阈值（dBFS）：比正弦电平还高，什么都切不出来
    assert speech_segments(x, SR, threshold=0.0) == []


def test_vad_end_never_exceeds_duration():
    x = 0.2 * np.random.default_rng(2).standard_normal(int(SR * 1.01))
    x[: SR // 2] *= 1e-4
    segs = speech_segments(x.astype(np.float32), SR)
    assert segs and segs[-1][1] <= 1010


def test_grid_power_marks_frames_outside_signal_and_handles_fractional_frames():
    x = np.ones(1000, dtype=np.float32)
    p = grid_power(x, 1000, -2.0, 1.0, 1004)  # 1 kHz、1 ms 一帧，从 -2 ms 起
    assert np.isnan(p[:2]).all() and np.isnan(p[1002:]).all()
    assert np.allclose(p[2:1002], 1.0)
    # 44.1 kHz、5 ms 一帧（220.5 个采样）：边界按四舍五入取，不累计误差
    y = np.ones(44100, dtype=np.float32)
    q = grid_power(y, 44100, 0.0, 5.0, 200)
    assert np.allclose(q, 1.0)


# ---------------------------------------------------------------- 对齐

@pytest.mark.parametrize("echo_db", [-20, -40])
@pytest.mark.parametrize("seed", [7, 1, 2, 3])
def test_align_offset_within_20ms(echo_db, seed):
    m = make_meeting(echo_db, seed=seed)
    assert m.offset_ms == 1234
    assert abs(align_offset(m.mic, m.mix, m.sr) - m.offset_ms) <= 20


def test_align_negative_offset():
    # 混音比麦克风晚开录 3 秒：offset = 1234 - 4234 = -3000
    m = make_meeting(-20, mix_start_ms=4234)
    assert m.offset_ms == -3000
    assert abs(align_offset(m.mic, m.mix, m.sr) - m.offset_ms) <= 20


def test_align_max_shift_limits_search():
    m = make_meeting(-20)
    assert abs(align_offset(m.mic, m.mix, m.sr, max_shift_ms=2000) - 1234) <= 20
    try:
        got = align_offset(m.mic, m.mix, m.sr, max_shift_ms=500)
    except AlignError:
        return
    assert abs(got) <= 600


def test_align_detail_shape():
    m = make_meeting(-20)
    d = align_detail(m.mic, m.mix, m.sr, chunk_ms=10000)
    assert set(d) == {"offset_ms", "chunks", "used", "total", "spread_ms"}
    assert d["total"] >= d["used"] >= 3
    for ch in d["chunks"]:
        assert set(ch) == {"mic_ms", "coarse_ms", "offset_ms", "r"}
    agreed = [ch["offset_ms"] for ch in d["chunks"] if ch["offset_ms"] is not None]
    assert len(agreed) == d["used"]
    assert all(abs(o - 1234) <= 30 for o in agreed)


def test_align_silent_track_raises():
    m = make_meeting(-20)
    with pytest.raises(AlignError):
        align_offset(np.zeros_like(m.mic), m.mix, m.sr)
    with pytest.raises(AlignError):
        align_offset(m.mic[: SR], m.mix, m.sr)  # 1 秒太短


@pytest.mark.skipif(not os.environ.get("PYBASE_LONG_AUDIO_TEST"),
                    reason="两小时合成轨要 1 GB 内存，只在 Studio（PYBASE_LONG_AUDIO_TEST=1）跑")
def test_align_two_hour_tracks_under_30s():
    t0 = time.perf_counter()
    m = make_long_meeting(2.0)
    made = time.perf_counter() - t0
    t0 = time.perf_counter()
    d = align_detail(m.mic, m.mix, m.sr)
    took = time.perf_counter() - t0
    t0 = time.perf_counter()
    mic_segs, mix_segs = speech_segments(m.mic, m.sr), speech_segments(m.mix, m.sr)
    vad = time.perf_counter() - t0
    t0 = time.perf_counter()
    attribute(mic_segs, mix_segs, d["offset_ms"], m.mic, m.mix, m.sr)
    attr = time.perf_counter() - t0
    report(f"两小时轨（16 kHz，两条）：对齐 {took:.2f} s，误差 {d['offset_ms'] - m.offset_ms:+d} ms，"
           f"块 {d['used']}/{d['total']}，极差 {d['spread_ms']} ms；两条 VAD {vad:.2f} s；"
           f"归属 {attr:.2f} s（合成信号 {made:.1f} s 不算）")
    assert abs(d["offset_ms"] - m.offset_ms) <= 20
    assert took < 30


# ---------------------------------------------------------------- 归属

def _attribute(m, **kw):
    mic_segs = speech_segments(m.mic, m.sr)
    mix_segs = speech_segments(m.mix, m.sr)
    off = align_offset(m.mic, m.mix, m.sr)
    return mix_segs, attribute(mic_segs, mix_segs, off, m.mic, m.mix, m.sr, **kw)


def _majority(pieces, s, e, shrink=200):
    lo, hi = s + shrink, e - shrink
    dur: dict[str, int] = {}
    for p in pieces:
        d = min(hi, p["end_ms"]) - max(lo, p["start_ms"])
        if d > 0:
            dur[p["who"]] = dur.get(p["who"], 0) + d
    return max(dur, key=dur.get) if dur else None


@pytest.mark.parametrize("echo_db", [-20, -40])
@pytest.mark.parametrize("seed", [7, 1, 2, 3])
def test_attribute_all_correct(echo_db, seed):
    m = make_meeting(echo_db, seed=seed)
    _, pieces = _attribute(m)
    for s, e, who in TRUTH:
        assert _majority(pieces, s, e) == who, (s, e, who, pieces)
        assert coverage(pieces, s, e, who) >= 0.8, (s, e, who, pieces)
    # 同时说话那 2 秒标 both；独白里不冒 both
    assert coverage(pieces, 18000, 20000, "both") >= 0.8
    for s, e, who in TRUTH:
        if who in ("self", "remote"):
            assert coverage(pieces, s, e, "both") <= 0.1, (s, e, pieces)


def test_attribute_ratio_db_levels():
    m = make_meeting(-20)
    _, pieces = _attribute(m)
    selfs = [p["ratio_db"] for p in pieces if p["who"] == "self"]
    remotes = [p["ratio_db"] for p in pieces if p["who"] == "remote"]
    assert selfs and all(abs(r) <= 2 for r in selfs)
    assert remotes and all(-26 <= r <= -16 for r in remotes)
    assert all(p["ratio_db"] is None for p in pieces if p["who"] == "unknown")


def test_attribute_pieces_partition_mix_segments():
    m = make_meeting(-20)
    mix_segs, pieces = _attribute(m)
    i = 0
    for s, e in mix_segs:
        assert pieces[i]["start_ms"] == s
        while pieces[i]["end_ms"] != e:
            assert pieces[i + 1]["start_ms"] == pieces[i]["end_ms"]
            assert pieces[i + 1]["who"] != pieces[i]["who"]
            i += 1
        i += 1
    assert i == len(pieces)


def test_attribute_self_ratio_override_matches_auto():
    m = make_meeting(-20)
    _, auto = _attribute(m)
    true_ratio = 20 * np.log10(G_SELF_MIC / G_SELF_MIX)
    _, given = _attribute(m, self_ratio_db=true_ratio)
    assert [p["who"] for p in auto] == [p["who"] for p in given]


def test_attribute_silent_mic_means_remote():
    m = make_meeting(-20)
    quiet = (1e-5 * np.random.default_rng(3).standard_normal(len(m.mic))).astype(np.float32)
    mix_segs = speech_segments(m.mix, m.sr)
    pieces = attribute([], mix_segs, m.offset_ms, quiet, m.mix, m.sr)
    assert {p["who"] for p in pieces} == {"remote", "unknown"}
    # 麦克风没覆盖到的 200–1000 是 unknown
    assert _majority(pieces, 200, 1000) == "unknown"


def test_attribute_empty_inputs():
    m = make_meeting(-20)
    assert attribute([], [], 0, m.mic, m.mix, m.sr) == []
    assert attribute([(0, 100)], [(0, 100)], 0, m.mic, np.zeros(0, np.float32), m.sr) == []
