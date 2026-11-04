"""pybase.audio 碰 ffmpeg 的测试：decode_pcm，以及"合成会议 → 按录制端/腾讯会议的
格式编码 → 解码 → 对齐 → 归属"整条链路。音频全是程序合成的（lavfi sine、
tests/audio_synth.py），不用任何真实录音。本机没有 ffmpeg 时整体跳过（Ubuntu
不装 ffmpeg，这些只在 Studio 跑）。
"""
from __future__ import annotations

import shutil
import subprocess

import pytest

np = pytest.importorskip("numpy")

from audio_synth import TRUTH, coverage, make_meeting  # noqa: E402
from pybase.audio import (  # noqa: E402
    align_detail, attribute, decode_pcm, speech_segments,
)
from pybase.media import MediaError, ToolNotFoundError  # noqa: E402

FFMPEG = shutil.which("ffmpeg")
requires_ffmpeg = pytest.mark.skipif(FFMPEG is None, reason="本机没有 ffmpeg")


def _ff(*args, **kw):
    subprocess.run([FFMPEG, "-v", "error", "-y", *map(str, args)], check=True, timeout=120, **kw)


def _encoders() -> str:
    return subprocess.run([FFMPEG, "-hide_banner", "-encoders"], capture_output=True,
                          text=True, timeout=30).stdout


def _opus_args() -> list[str]:
    enc = _encoders()
    if "libopus" in enc:
        return ["-c:a", "libopus", "-b:a", "48k"]
    if " opus " in enc:
        return ["-c:a", "opus", "-strict", "-2", "-b:a", "48k"]
    pytest.skip("这个 ffmpeg 没有 opus 编码器")


def _peak_hz(x: np.ndarray, sr: int) -> float:
    spec = np.abs(np.fft.rfft(x * np.hanning(len(x))))
    return float(np.fft.rfftfreq(len(x), 1 / sr)[int(np.argmax(spec))])


def test_decode_pcm_missing_ffmpeg(tmp_path):
    with pytest.raises(ToolNotFoundError):
        decode_pcm(tmp_path / "x.wav", ffmpeg=str(tmp_path / "no-such-ffmpeg"))
    with pytest.raises(ValueError):
        decode_pcm(tmp_path / "x.wav", sr=0)


@requires_ffmpeg
def test_decode_pcm_m4a_aac_32k(tmp_path):
    src = tmp_path / "mix.m4a"
    _ff("-f", "lavfi", "-i", "sine=frequency=440:duration=2:sample_rate=32000",
        "-c:a", "aac", "-ac", "1", "-ar", "32000", src)
    pcm = decode_pcm(src)
    assert pcm.dtype == np.float32 and pcm.ndim == 1
    assert abs(len(pcm) - 32000) <= 320
    assert abs(_peak_hz(pcm, 16000) - 440) < 5
    rms = float(np.sqrt(np.mean(pcm[1600:-1600] ** 2)))
    assert abs(rms - 0.125 / np.sqrt(2)) < 0.01  # lavfi sine 缺省振幅 1/8
    low = decode_pcm(src, sr=8000)
    assert abs(len(low) - 16000) <= 160


@requires_ffmpeg
def test_decode_pcm_takes_audio_from_video(tmp_path):
    src = tmp_path / "meeting.mp4"
    _ff("-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=3",
        "-f", "lavfi", "-i", "sine=frequency=1000:duration=3:sample_rate=32000",
        "-c:v", "mpeg4", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ac", "1", "-ar", "32000",
        "-shortest", src)
    pcm = decode_pcm(src)
    assert abs(len(pcm) - 48000) <= 480
    assert abs(_peak_hz(pcm, 16000) - 1000) < 5


@requires_ffmpeg
def test_decode_pcm_webm_opus_stereo_48k(tmp_path):
    src = tmp_path / "record-mic.webm"
    _ff("-f", "lavfi", "-i", "sine=frequency=300:duration=2:sample_rate=48000",
        "-ac", "2", *_opus_args(), src)
    pcm = decode_pcm(src)
    assert abs(len(pcm) - 32000) <= 400
    assert abs(_peak_hz(pcm, 16000) - 300) < 5


@requires_ffmpeg
def test_decode_pcm_without_audio_raises(tmp_path):
    src = tmp_path / "silent.mp4"
    _ff("-f", "lavfi", "-i", "testsrc=size=160x120:rate=8:duration=1", "-c:v", "mpeg4", src)
    with pytest.raises(MediaError):
        decode_pcm(src)


@requires_ffmpeg
@pytest.mark.parametrize("echo_db", [-20, -40])
def test_end_to_end_encoded_meeting(tmp_path, echo_db):
    """麦克风按录制端的格式（webm/opus 48k）、混音按腾讯会议的（m4a AAC 单声道 32k）编码，
    解码回来再对齐、归属，结果和直接用合成数组的一样准。"""
    m = make_meeting(echo_db)
    raw_mic, raw_mix = tmp_path / "mic.f32", tmp_path / "mix.f32"
    raw_mic.write_bytes(m.mic.astype("<f4").tobytes())
    raw_mix.write_bytes(m.mix.astype("<f4").tobytes())
    mic_file, mix_file = tmp_path / "record-mic.webm", tmp_path / "meeting.m4a"
    _ff("-f", "f32le", "-ar", m.sr, "-ac", 1, "-i", raw_mic, "-ar", 48000, *_opus_args(), mic_file)
    _ff("-f", "f32le", "-ar", m.sr, "-ac", 1, "-i", raw_mix, "-ar", 32000, "-ac", 1,
        "-c:a", "aac", "-b:a", "64k", mix_file)

    mic, mix = decode_pcm(mic_file), decode_pcm(mix_file)
    d = align_detail(mic, mix, 16000)
    print(f"\n编码后对齐（回声 {echo_db} dB）：{d['offset_ms']} ms，真值 {m.offset_ms}，"
          f"误差 {d['offset_ms'] - m.offset_ms:+d} ms")
    assert abs(d["offset_ms"] - m.offset_ms) <= 20

    pieces = attribute(speech_segments(mic, 16000), speech_segments(mix, 16000),
                       d["offset_ms"], mic, mix, 16000)
    for s, e, who in TRUTH:
        assert coverage(pieces, s, e, who) >= 0.8, (s, e, who, pieces)
