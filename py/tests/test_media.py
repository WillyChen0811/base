"""pybase.media 的测试：用 ffmpeg lavfi 现场生成测试媒体，覆盖 probe/
extract_audio/snapshot 的正常路径，以及子进程超时/取消的收尾行为。

本机没有 ffmpeg 的环境整体跳过 ffmpeg 相关用例；超时/取消的机制测试不依赖
真实 ffmpeg，用可控的假子进程（当前 Python 解释器）验证，避免真实 ffmpeg
快慢不定引入计时不稳定。
"""
from __future__ import annotations

import shutil
import subprocess
import sys
import threading
from pathlib import Path

import pytest

from pybase.media import (
    MediaCancelled,
    MediaTimeoutError,
    ToolNotFoundError,
    extract_audio,
    probe,
    snapshot,
)
from pybase.media._proc import run_to_file, tmp_path_for

FFMPEG = shutil.which("ffmpeg")
FFPROBE = shutil.which("ffprobe")
requires_ffmpeg = pytest.mark.skipif(
    not (FFMPEG and FFPROBE), reason="本机没有 ffmpeg/ffprobe"
)


def _make_audio(path: Path, duration: float = 3.0) -> Path:
    subprocess.run(
        [FFMPEG, "-v", "error", "-y", "-f", "lavfi",
         "-i", f"sine=frequency=440:duration={duration}", str(path)],
        check=True, timeout=30,
    )
    return path


def _make_video(path: Path, duration: float = 3.0) -> Path:
    subprocess.run(
        [FFMPEG, "-v", "error", "-y",
         "-f", "lavfi", "-i", f"testsrc=size=320x240:rate=10:duration={duration}",
         "-f", "lavfi", "-i", f"sine=frequency=440:duration={duration}",
         "-shortest", "-pix_fmt", "yuv420p", str(path)],
        check=True, timeout=30,
    )
    return path


@requires_ffmpeg
def test_probe_audio(tmp_path):
    src = _make_audio(tmp_path / "a.wav", duration=3.0)
    info = probe(src)
    assert 2900 <= info["duration_ms"] <= 3100
    assert len(info["streams"]) == 1
    assert info["streams"][0]["codec_type"] == "audio"
    assert info["streams"][0]["sample_rate"]


@requires_ffmpeg
def test_probe_video(tmp_path):
    src = _make_video(tmp_path / "v.mp4", duration=2.0)
    info = probe(src)
    assert 1900 <= info["duration_ms"] <= 2200
    kinds = {s["codec_type"] for s in info["streams"]}
    assert kinds == {"video", "audio"}
    video = next(s for s in info["streams"] if s["codec_type"] == "video")
    assert video["width"] == 320
    assert video["height"] == 240


@requires_ffmpeg
def test_probe_missing_tool_raises(tmp_path):
    with pytest.raises(ToolNotFoundError):
        probe(tmp_path / "x.wav", ffprobe="/no/such/ffprobe-binary")


@requires_ffmpeg
def test_extract_audio_clip_duration(tmp_path):
    src = _make_audio(tmp_path / "a.wav", duration=5.0)
    out = tmp_path / "clip.mp3"
    extract_audio(src, out, start_ms=1000, end_ms=2000)
    assert out.is_file()
    info = probe(out)
    assert 900 <= info["duration_ms"] <= 1150


@requires_ffmpeg
def test_extract_audio_with_padding(tmp_path):
    src = _make_audio(tmp_path / "a.wav", duration=5.0)
    out = tmp_path / "clip.mp3"
    extract_audio(src, out, start_ms=1000, end_ms=2000, pad_start_ms=200, pad_end_ms=300)
    info = probe(out)
    # 1000-200=800ms 到 2000+300=2300ms，约 1500ms
    assert 1400 <= info["duration_ms"] <= 1650


@requires_ffmpeg
def test_extract_audio_padding_clamped_to_bounds(tmp_path):
    src = _make_audio(tmp_path / "a.wav", duration=2.0)
    out = tmp_path / "clip.wav"
    # padding 远超媒体首尾，应该被夹到 [0, duration]
    extract_audio(src, out, start_ms=100, end_ms=1900, pad_start_ms=10_000, pad_end_ms=10_000)
    info = probe(out)
    assert 1800 <= info["duration_ms"] <= 2100


@requires_ffmpeg
def test_extract_audio_full_length_conversion(tmp_path):
    src = _make_audio(tmp_path / "a.wav", duration=2.0)
    out = tmp_path / "16k.wav"
    extract_audio(src, out, sample_rate=16000, mono=True)
    info = probe(out)
    assert info["streams"][0]["sample_rate"] == 16000
    assert info["streams"][0]["channels"] == 1
    assert 1900 <= info["duration_ms"] <= 2100


@requires_ffmpeg
def test_extract_audio_start_end_must_be_paired(tmp_path):
    src = _make_audio(tmp_path / "a.wav", duration=1.0)
    with pytest.raises(ValueError):
        extract_audio(src, tmp_path / "clip.wav", start_ms=100)


@requires_ffmpeg
def test_snapshot_produces_sized_image(tmp_path):
    src = _make_video(tmp_path / "v.mp4", duration=2.0)
    out = tmp_path / "frame.jpg"
    snapshot(src, out, at_ms=500)
    assert out.is_file()
    info = probe(out)
    video = next(s for s in info["streams"] if s["codec_type"] == "video")
    assert video["width"] == 320
    assert video["height"] == 240


@requires_ffmpeg
def test_snapshot_max_width_scales_down(tmp_path):
    src = _make_video(tmp_path / "v.mp4", duration=2.0)
    out = tmp_path / "frame.jpg"
    snapshot(src, out, at_ms=500, max_width=160)
    info = probe(out)
    video = next(s for s in info["streams"] if s["codec_type"] == "video")
    assert video["width"] == 160
    assert video["height"] == 120


def _sleepy_script(tmp_path: Path) -> Path:
    """假子进程：先写一份"半截"输出，睡一会儿，再补完 + 留个完成标记。

    用来验证超时/取消发生时——即便临时文件已经有部分内容落盘——也会被
    整个丢弃，且进程真的没跑到"补完"那一步（marker 不存在）。
    """
    script = tmp_path / "slow_tool.py"
    script.write_text(
        "import sys, time\n"
        "tmp_path, marker, delay = sys.argv[1], sys.argv[2], float(sys.argv[3])\n"
        "with open(tmp_path, 'w') as f:\n"
        "    f.write('partial')\n"
        "time.sleep(delay)\n"
        "with open(tmp_path, 'a') as f:\n"
        "    f.write('-done')\n"
        "with open(marker, 'w') as f:\n"
        "    f.write('marker')\n"
    )
    return script


def test_run_to_file_timeout_kills_process_and_leaves_no_output(tmp_path):
    script = _sleepy_script(tmp_path)
    marker = tmp_path / "marker.txt"
    out = tmp_path / "out.bin"

    def cmd_for(tmp: Path) -> list[str]:
        return [sys.executable, str(script), str(tmp), str(marker), "2.0"]

    with pytest.raises(MediaTimeoutError):
        run_to_file(cmd_for, out, timeout=0.3)

    assert not out.exists()
    assert not tmp_path_for(out).exists()
    assert not marker.exists()


def test_run_to_file_cancel_kills_process_and_leaves_no_output(tmp_path):
    script = _sleepy_script(tmp_path)
    marker = tmp_path / "marker.txt"
    out = tmp_path / "out.bin"
    cancel_event = threading.Event()
    threading.Timer(0.2, cancel_event.set).start()

    def cmd_for(tmp: Path) -> list[str]:
        return [sys.executable, str(script), str(tmp), str(marker), "2.0"]

    with pytest.raises(MediaCancelled):
        run_to_file(cmd_for, out, timeout=10, cancel_event=cancel_event)

    assert not out.exists()
    assert not tmp_path_for(out).exists()
    assert not marker.exists()


def test_run_to_file_success_replaces_atomically(tmp_path):
    script = _sleepy_script(tmp_path)
    marker = tmp_path / "marker.txt"
    out = tmp_path / "out.bin"

    def cmd_for(tmp: Path) -> list[str]:
        return [sys.executable, str(script), str(tmp), str(marker), "0.1"]

    run_to_file(cmd_for, out, timeout=5)
    assert out.read_text() == "partial-done"
    assert marker.is_file()
    assert not tmp_path_for(out).exists()


@pytest.mark.skipif(sys.platform == "win32", reason="用 os.kill(pid, 0) 探活，仅 POSIX")
def test_timeout_kills_grandchild_processes(tmp_path):
    """ffsubsync 自己会起 ffmpeg：超时只杀父进程会留下孤儿，必须整组杀。"""
    import os
    import time

    from pybase.media._proc import run

    pid_file = tmp_path / "child.pid"
    parent = tmp_path / "parent.py"
    parent.write_text(
        "import subprocess, sys, time\n"
        "p = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'])\n"
        f"open({str(pid_file)!r}, 'w').write(str(p.pid))\n"
        "time.sleep(30)\n"
    )
    with pytest.raises(MediaTimeoutError):
        run([sys.executable, str(parent)], timeout=1.0)

    child_pid = int(pid_file.read_text())
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        try:
            os.kill(child_pid, 0)
        except ProcessLookupError:
            return
        time.sleep(0.1)
    os.kill(child_pid, 9)
    pytest.fail("孙进程在父进程被杀后仍然存活")


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="需要 ffmpeg")
def test_probe_marks_cover_art(tmp_path):
    src = tmp_path / "cover.mp3"
    subprocess.run([
        "ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=d=1",
        "-f", "lavfi", "-i", "color=c=red:s=64x64:d=1", "-map", "0", "-map", "1",
        "-frames:v", "1", "-c:v", "mjpeg", "-disposition:v", "attached_pic", str(src),
    ], check=True)
    video = [s for s in probe(src)["streams"] if s["codec_type"] == "video"]
    assert video and video[0].get("attached_pic") is True
