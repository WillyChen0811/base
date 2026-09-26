"""pybase.media.playback 的测试：`playback_plan` 用构造的 probe 字典覆盖各
分支（纯函数，不需要 ffmpeg）；`remux` 用 ffmpeg lavfi 现场生成一个 h264+aac
的 mkv，验证转成 mp4 后容器/编码/时长正确、faststart 生效。本机没有 ffmpeg
的用例整体跳过。
"""
from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

from pybase.media import playback_plan, probe, remux

FFMPEG = shutil.which("ffmpeg")
FFPROBE = shutil.which("ffprobe")
requires_ffmpeg = pytest.mark.skipif(
    not (FFMPEG and FFPROBE), reason="本机没有 ffmpeg/ffprobe"
)

CHROME_VIDEO = {"h264", "vp8", "vp9", "av1"}
CHROME_AUDIO = {"aac", "opus", "vorbis", "mp3", "flac"}
CHROME_CONTAINERS = {"mp4", "webm"}


def _probe_result(*, container, streams):
    return {"duration_ms": 1000, "streams": streams, "container": container}


def _video(index, codec_name):
    return {"index": index, "codec_type": "video", "codec_name": codec_name}


def _audio(index, codec_name):
    return {"index": index, "codec_type": "audio", "codec_name": codec_name}


def test_direct_when_container_and_codecs_all_supported():
    result = _probe_result(
        container=("mov", "mp4", "m4a"),
        streams=[_video(0, "h264"), _audio(1, "aac")],
    )
    plan = playback_plan(
        result, video_codecs=CHROME_VIDEO, audio_codecs=CHROME_AUDIO,
        containers=CHROME_CONTAINERS,
    )
    assert plan == {
        "action": "direct", "reasons": [], "video_stream": 0, "audio_stream": 1,
    }


def test_remux_when_container_unsupported_but_codecs_ok():
    # 典型场景：h264+aac 装在 mkv 里，编码能播，容器不认，转封装就够了。
    result = _probe_result(
        container=("matroska", "webm"),
        streams=[_video(0, "h264"), _audio(1, "aac")],
    )
    plan = playback_plan(
        result, video_codecs=CHROME_VIDEO, audio_codecs=CHROME_AUDIO,
        containers={"mp4"},
    )
    assert plan["action"] == "remux"
    assert plan["video_stream"] == 0
    assert plan["audio_stream"] == 1
    assert any("容器" in r for r in plan["reasons"])


def test_transcode_when_video_codec_unsupported():
    result = _probe_result(
        container=("mov", "mp4"),
        streams=[_video(0, "hevc"), _audio(1, "aac")],
    )
    plan = playback_plan(
        result, video_codecs=CHROME_VIDEO, audio_codecs=CHROME_AUDIO,
        containers=CHROME_CONTAINERS,
    )
    assert plan["action"] == "transcode"
    assert plan["video_stream"] is None
    assert plan["audio_stream"] == 1
    assert any("视频编码" in r for r in plan["reasons"])


def test_transcode_when_audio_codec_unsupported():
    result = _probe_result(
        container=("mov", "mp4"),
        streams=[_video(0, "h264"), _audio(1, "ac3")],
    )
    plan = playback_plan(
        result, video_codecs=CHROME_VIDEO, audio_codecs=CHROME_AUDIO,
        containers=CHROME_CONTAINERS,
    )
    assert plan["action"] == "transcode"
    assert plan["audio_stream"] is None
    assert any("音频编码" in r for r in plan["reasons"])


def test_remux_when_first_video_track_unplayable_but_second_is():
    # 第 0 条是 HEVC 播不了，第 1 条 H.264 能播：选中第 1 条，但因为不是
    # 首条，仍然判 remux（浏览器默认只挑第一条）。
    result = _probe_result(
        container=("mov", "mp4"),
        streams=[_video(0, "hevc"), _video(1, "h264"), _audio(2, "aac")],
    )
    plan = playback_plan(
        result, video_codecs=CHROME_VIDEO, audio_codecs=CHROME_AUDIO,
        containers=CHROME_CONTAINERS,
    )
    assert plan["action"] == "remux"
    assert plan["video_stream"] == 1
    assert plan["audio_stream"] == 2
    assert any("首条视频轨" in r for r in plan["reasons"])


def test_direct_audio_only_file():
    result = _probe_result(container=("mp3",), streams=[_audio(0, "mp3")])
    plan = playback_plan(
        result, video_codecs=CHROME_VIDEO, audio_codecs=CHROME_AUDIO,
        containers={"mp3", "mp4", "webm"},
    )
    assert plan == {
        "action": "direct", "reasons": [], "video_stream": None, "audio_stream": 0,
    }


def test_case_insensitive_codec_and_container_matching():
    result = _probe_result(
        container=("MOV", "MP4"),
        streams=[_video(0, "H264"), _audio(1, "AAC")],
    )
    plan = playback_plan(
        result, video_codecs={"H264"}, audio_codecs={"AAC"}, containers={"MP4"},
    )
    assert plan["action"] == "direct"


def _make_mkv(path: Path, duration: float = 2.0) -> Path:
    subprocess.run(
        [FFMPEG, "-v", "error", "-y",
         "-f", "lavfi", "-i", f"testsrc=size=320x240:rate=10:duration={duration}",
         "-f", "lavfi", "-i", f"sine=frequency=440:duration={duration}",
         "-shortest", "-pix_fmt", "yuv420p",
         "-c:v", "libx264", "-c:a", "aac", str(path)],
        check=True, timeout=30,
    )
    return path


def _moov_before_mdat(path: Path) -> bool:
    data = path.read_bytes()
    moov_at = data.find(b"moov")
    mdat_at = data.find(b"mdat")
    assert moov_at != -1 and mdat_at != -1
    return moov_at < mdat_at


@requires_ffmpeg
def test_remux_mkv_to_mp4_faststart(tmp_path):
    src = _make_mkv(tmp_path / "v.mkv", duration=2.0)
    src_info = probe(src)
    assert set(src_info["container"]) & {"matroska", "webm"}

    plan = playback_plan(
        src_info, video_codecs=CHROME_VIDEO, audio_codecs=CHROME_AUDIO,
        containers={"mp4"},
    )
    assert plan["action"] == "remux"

    out = tmp_path / "v.mp4"
    remux(src, out, video_stream=plan["video_stream"], audio_stream=plan["audio_stream"])

    assert out.is_file()
    info = probe(out)
    assert "mp4" in info["container"]
    kinds = {s["codec_type"]: s["codec_name"] for s in info["streams"]}
    assert kinds["video"] == "h264"
    assert kinds["audio"] == "aac"
    assert 1900 <= info["duration_ms"] <= 2200
    assert _moov_before_mdat(out)


@requires_ffmpeg
def test_remux_video_only_omits_audio_map(tmp_path):
    src = tmp_path / "v.mkv"
    subprocess.run(
        [FFMPEG, "-v", "error", "-y", "-f", "lavfi",
         "-i", "testsrc=size=320x240:rate=10:duration=1",
         "-pix_fmt", "yuv420p", "-c:v", "libx264", "-an", str(src)],
        check=True, timeout=30,
    )
    out = tmp_path / "v.mp4"
    remux(src, out, video_stream=0, audio_stream=None)
    info = probe(out)
    assert len(info["streams"]) == 1
    assert info["streams"][0]["codec_type"] == "video"


_CHROMIUM = dict(
    video_codecs={"h264", "vp9"}, audio_codecs={"aac", "mp3", "opus"},
    containers={"mp4", "webm", "mp3"},
)


def test_plan_h264_mkv_is_not_treated_as_webm():
    probe_result = {"container": ("matroska", "webm"), "streams": [
        {"index": 0, "codec_type": "video", "codec_name": "h264"},
        {"index": 1, "codec_type": "audio", "codec_name": "aac"},
    ]}
    assert playback_plan(probe_result, **_CHROMIUM)["action"] == "remux"


def test_plan_vp9_opus_matroska_counts_as_webm():
    probe_result = {"container": ("matroska", "webm"), "streams": [
        {"index": 0, "codec_type": "video", "codec_name": "vp9"},
        {"index": 1, "codec_type": "audio", "codec_name": "opus"},
    ]}
    assert playback_plan(probe_result, **_CHROMIUM)["action"] == "direct"


def test_plan_ignores_cover_art_stream():
    probe_result = {"container": ("mp3",), "streams": [
        {"index": 0, "codec_type": "audio", "codec_name": "mp3"},
        {"index": 1, "codec_type": "video", "codec_name": "mjpeg", "attached_pic": True},
    ]}
    plan = playback_plan(probe_result, **_CHROMIUM)
    assert plan["action"] == "direct" and plan["video_stream"] is None
