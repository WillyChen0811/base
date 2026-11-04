"""pybase.media.frames_at 的测试：用 ffmpeg lavfi testsrc 现场合成短视频，
不用任何真实录像。本机没有 ffmpeg 时整体跳过（Ubuntu 不装 ffmpeg，这些只在 Studio 跑）。
"""
from __future__ import annotations

import shutil
import subprocess

import pytest

from pybase.media import MediaError, frames_at, probe

FFMPEG = shutil.which("ffmpeg")
FFPROBE = shutil.which("ffprobe")
requires_ffmpeg = pytest.mark.skipif(not (FFMPEG and FFPROBE), reason="本机没有 ffmpeg/ffprobe")


def _encoders() -> str:
    return subprocess.run([FFMPEG, "-hide_banner", "-encoders"], capture_output=True,
                          text=True, timeout=30).stdout


def _vp9_args() -> list[str]:
    if "libvpx-vp9" not in _encoders():
        pytest.skip("这个 ffmpeg 没有 libvpx-vp9 编码器")
    return ["-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-b:v", "300k"]


def _make_webm(path, *, duration=5, pipe=False):
    """录制端那样的画面：VP9、8 fps、没有声音。pipe=True 时写管道（不回写时长，像 MediaRecorder）。"""
    cmd = [FFMPEG, "-v", "error", "-y", "-f", "lavfi",
           "-i", f"testsrc=size=640x360:rate=8:duration={duration}",
           *_vp9_args(), "-pix_fmt", "yuv420p", "-an"]
    if pipe:
        with open(path, "wb") as fh:
            subprocess.run([*cmd, "-f", "webm", "pipe:1"], stdout=fh, check=True, timeout=60)
    else:
        subprocess.run([*cmd, str(path)], check=True, timeout=60)
    return path


def _make_mp4(path, *, duration=4):
    """腾讯会议转码那样的结构：一条视频 + 一条 AAC 单声道 32 kHz（视频用自带的 mpeg4 编码器）。"""
    subprocess.run(
        [FFMPEG, "-v", "error", "-y",
         "-f", "lavfi", "-i", f"testsrc=size=640x360:rate=15:duration={duration}",
         "-f", "lavfi", "-i", f"sine=frequency=440:duration={duration}:sample_rate=32000",
         "-c:v", "mpeg4", "-q:v", "5", "-pix_fmt", "yuv420p",
         "-c:a", "aac", "-ac", "1", "-ar", "32000", "-shortest", str(path)],
        check=True, timeout=60)
    return path


def test_frames_at_rejects_negative_and_empty(tmp_path):
    with pytest.raises(ValueError):
        frames_at(tmp_path / "x.webm", [100, -1], tmp_path / "out")
    assert frames_at(tmp_path / "x.webm", [], tmp_path / "out") == []
    assert not (tmp_path / "out").exists()


@requires_ffmpeg
def test_frames_at_webm_points_order_dedupe_and_beyond_end(tmp_path):
    src = _make_webm(tmp_path / "record.webm")
    out = tmp_path / "frames"
    res = frames_at(src, [1000, 0, 2500, 1000, 4800, 6000], out)
    assert [r.t_ms for r in res] == [1000, 0, 2500, 4800, 6000]
    for r in res[:4]:
        assert r.skipped is None and r.path == out / f"{r.t_ms}.jpg" and r.path.is_file()
        info = probe(r.path)
        assert info["streams"][0]["codec_name"] == "mjpeg"
        assert info["streams"][0]["width"] == 640  # 不比 1280 宽，不放大
    assert res[4].path is None and "超出时长" in res[4].skipped
    assert not (out / "6000.jpg").exists()
    # 不同时间点的画面不一样（testsrc 上有走动的计数）
    assert (out / "0.jpg").read_bytes() != (out / "2500.jpg").read_bytes()
    # 没留临时文件
    assert sorted(p.name for p in out.iterdir()) == ["0.jpg", "1000.jpg", "2500.jpg", "4800.jpg"]


@requires_ffmpeg
def test_frames_at_width(tmp_path):
    src = _make_webm(tmp_path / "record.webm", duration=2)
    small = frames_at(src, [500], tmp_path / "small", width=320)[0]
    assert probe(small.path)["streams"][0]["width"] == 320
    full = frames_at(src, [500], tmp_path / "full", width=None)[0]
    assert probe(full.path)["streams"][0]["width"] == 640


@requires_ffmpeg
def test_frames_at_webm_without_duration(tmp_path):
    src = _make_webm(tmp_path / "piped.webm", duration=3, pipe=True)
    res = frames_at(src, [1000, 9000], tmp_path / "out")
    assert res[0].path is not None and res[0].path.is_file()
    assert res[1].path is None and res[1].skipped
    assert not (tmp_path / "out" / "9000.jpg").exists()


@requires_ffmpeg
def test_frames_at_mp4_like_meeting_recording(tmp_path):
    src = _make_mp4(tmp_path / "meeting.mp4")
    res = frames_at(src, [0, 1999, 3000], tmp_path / "out", width=1280)
    assert all(r.path is not None for r in res)


@requires_ffmpeg
def test_frames_at_audio_only_raises(tmp_path):
    src = tmp_path / "a.m4a"
    subprocess.run([FFMPEG, "-v", "error", "-y", "-f", "lavfi", "-i", "sine=duration=2",
                    "-c:a", "aac", str(src)], check=True, timeout=30)
    with pytest.raises(MediaError):
        frames_at(src, [500], tmp_path / "out")
