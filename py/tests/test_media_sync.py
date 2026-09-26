"""pybase.media.sync_subtitles 的测试：全部用 mock 子进程，不强求本机装了
ffsubsync（它是可选外部工具，装不装不影响这里的命令拼装/输出解析逻辑）。
"""
from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

import pybase.media._proc as proc_module
from pybase.media import MediaCancelled, MediaError, MediaTimeoutError, ToolNotFoundError
from pybase.media._proc import tmp_path_for
from pybase.media.sync import _parse_offset, sync_subtitles


@pytest.fixture()
def fake_ffsubsync(tmp_path) -> str:
    """resolve_tool 会校验显式传入的路径确实存在且可执行，这里造一个假的，
    真正的行为完全靠 monkeypatch 换掉 `run`，这个文件本身永远不会被执行。
    """
    binary = tmp_path / "fake-ffsubsync"
    binary.write_text("#!/bin/sh\nexit 0\n")
    binary.chmod(0o755)
    return str(binary)


def _fake_run(returncode: int, stdout: str = "", stderr: str = "", write_tmp: bool = True):
    """造一个替身 `run`：按 `-o` 后面的参数把假输出写到临时路径，
    模拟 ffsubsync 真的往 `-o` 指定的路径写了文件。
    """

    def run(cmd, *, timeout, cancel_event=None):
        if write_tmp:
            tmp_path = Path(cmd[cmd.index("-o") + 1])
            tmp_path.write_text("subtitle")
        return subprocess.CompletedProcess(cmd, returncode, stdout, stderr)

    return run


def test_sync_subtitles_parses_offset_and_replaces_output(tmp_path, monkeypatch, fake_ffsubsync):
    monkeypatch.setattr(
        proc_module, "run",
        _fake_run(0, stderr="INFO:ffsubsync.ffsubsync:offset seconds: -1.250"),
    )
    out = tmp_path / "out.srt"
    offset = sync_subtitles(
        tmp_path / "ref.mp4", tmp_path / "in.srt", out, ffsubsync=fake_ffsubsync,
    )
    assert offset == -1.25
    assert out.read_text() == "subtitle"
    assert not tmp_path_for(out).exists()


def test_sync_subtitles_offset_unparseable_returns_none(tmp_path, monkeypatch, fake_ffsubsync):
    monkeypatch.setattr(proc_module, "run", _fake_run(0, stdout="all good, no numbers here"))
    out = tmp_path / "out.srt"
    offset = sync_subtitles(
        tmp_path / "ref.mp4", tmp_path / "in.srt", out, ffsubsync=fake_ffsubsync,
    )
    assert offset is None
    assert out.is_file()


def test_sync_subtitles_failure_raises_and_discards_tmp(tmp_path, monkeypatch, fake_ffsubsync):
    monkeypatch.setattr(
        proc_module, "run", _fake_run(1, stderr="alignment failed", write_tmp=False)
    )
    out = tmp_path / "out.srt"
    with pytest.raises(MediaError):
        sync_subtitles(tmp_path / "ref.mp4", tmp_path / "in.srt", out, ffsubsync=fake_ffsubsync)
    assert not out.exists()
    assert not tmp_path_for(out).exists()


def test_sync_subtitles_timeout_propagates_and_discards_tmp(tmp_path, monkeypatch, fake_ffsubsync):
    def run(cmd, *, timeout, cancel_event=None):
        tmp_path_out = Path(cmd[cmd.index("-o") + 1])
        tmp_path_out.write_text("partial")
        raise MediaTimeoutError("timed out")

    monkeypatch.setattr(proc_module, "run", run)
    out = tmp_path / "out.srt"
    with pytest.raises(MediaTimeoutError):
        sync_subtitles(tmp_path / "ref.mp4", tmp_path / "in.srt", out, ffsubsync=fake_ffsubsync)
    assert not out.exists()
    assert not tmp_path_for(out).exists()


def test_sync_subtitles_cancel_propagates(tmp_path, monkeypatch, fake_ffsubsync):
    def run(cmd, *, timeout, cancel_event=None):
        raise MediaCancelled("cancelled")

    monkeypatch.setattr(proc_module, "run", run)
    out = tmp_path / "out.srt"
    with pytest.raises(MediaCancelled):
        sync_subtitles(tmp_path / "ref.mp4", tmp_path / "in.srt", out, ffsubsync=fake_ffsubsync)
    assert not out.exists()


def test_sync_subtitles_missing_tool_raises():
    with pytest.raises(ToolNotFoundError):
        sync_subtitles("ref.mp4", "in.srt", "out.srt", ffsubsync="/no/such/ffsubsync-binary")


@pytest.mark.parametrize(
    "text, expected",
    [
        ("INFO:ffsubsync.ffsubsync:offset seconds: -1.020", -1.02),
        ("offset seconds: 0.500", 0.5),
        # 质量告警里的数不是偏移，不能被当成偏移
        ("|offset| 12.0s > 60.0s", None),
        ("no useful information here", None),
        ("", None),
    ],
)
def test_parse_offset(text, expected):
    assert _parse_offset(text) == expected
