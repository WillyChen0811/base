"""临时名降级：tmp_names 规则、名字被拒判定、续传临时名降级/找回/清理。"""

import errno
import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from pybase.fileops import ResumeState, copy_file_resumable  # noqa: E402
from pybase.resumepart import (drop_parts, is_resume_part, resume_part_paths)  # noqa: E402
from pybase.storage import LocalStorage  # noqa: E402
from pybase.tmpname import is_name_rejected, tmp_names, tmp_paths  # noqa: E402

CH = 8
DATA = bytes(range(200))
LONG = "长" * 82 + ".docx"       # 82*3+5 = 251 字节：追加 .xferq.part 必超 255，换成 .xfq 不超


class Boom(Exception):
    pass


def test_候选规则():
    assert tmp_names("a.docx", ".wsu") == ["a.docx.wsu", "a.wsu"]
    assert tmp_names("a.docx", ".xferq.part", ".xfq") == ["a.docx.xferq.part", "a.xfq"]
    assert tmp_names("Makefile", ".wsu") == ["Makefile.wsu"]          # 无扩展名：没有降级余地
    assert tmp_names(".gitignore", ".wsu") == [".gitignore.wsu"]
    assert tmp_paths("d/e/a.bin", ".xferq.part", ".xfq") == ["d/e/a.bin.xferq.part", "d/e/a.xfq"]
    assert tmp_paths("a.bin", ".p", ".q") == ["a.bin.p", "a.q"]
    assert resume_part_paths("x/a.bin") == resume_part_paths("x/a.bin")   # 确定性


def test_smb_tmp_names_沿用同一规则():
    pytest.importorskip("smbprotocol")
    from pybase.smb.common import _tmp_names
    assert _tmp_names("a.docx") == ["a.docx.wsu", "a.wsu"]


def test_被拒判定很窄():
    class NT(OSError):
        ntstatus = 0xC0000033

    class Denied(OSError):
        ntstatus = 0xC0000022          # ACCESS_DENIED

    assert is_name_rejected(OSError(errno.ENAMETOOLONG, "x"))
    assert is_name_rejected(NT())
    assert not is_name_rejected(Denied())
    assert not is_name_rejected(PermissionError(errno.EACCES, "x"))
    assert not is_name_rejected(ConnectionError("断线"))
    assert not is_name_rejected(OSError(errno.ENOSPC, "满"))
    assert not is_name_rejected(ValueError("x"))


def test_清理识别():
    assert is_resume_part("a.docx.xferq.part") and is_resume_part("a.xfq")
    assert not is_resume_part("a.docx")


@pytest.fixture
def env(tmp_path):
    for d in ("s", "d"):
        (tmp_path / d).mkdir()
    (tmp_path / "s" / LONG).write_bytes(DATA)
    (tmp_path / "s" / "n.bin").write_bytes(DATA)
    return LocalStorage(tmp_path / "s"), LocalStorage(tmp_path / "d"), tmp_path


def _copy(src, dst, name, state, progress=None):
    parts = resume_part_paths(name)
    return copy_file_resumable(src, name, dst, name, part_path=parts[0], part_fallbacks=parts[1:],
                               chunk=CH, progress=progress, state=state)


def _stop_at(n):
    def cb(done, idx, sha):
        if idx == n:
            raise Boom()
    return cb


def test_长名降级_续传找回_转正后无残留(env):
    src, dst, tmp = env
    short = LONG[:-5] + ".xfq"
    state = ResumeState(chunk=CH)
    with pytest.raises(Boom):
        _copy(src, dst, LONG, state, _stop_at(5))
    assert os.listdir(tmp / "d") == [short]
    assert len(state.chunks) == 6

    # 续传：必须从降级名接着写（若重新从头写，进度回调的首个 idx 会是 0）
    idxs = []
    r = _copy(src, dst, LONG, state, lambda d, i, s: idxs.append(i))
    assert r.status == "copied" and idxs[0] == 6
    assert (tmp / "d" / LONG).read_bytes() == DATA
    assert sorted(os.listdir(tmp / "d")) == [LONG]


def test_降级名被清理识别(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    with pytest.raises(Boom):
        _copy(src, dst, LONG, state, _stop_at(2))
    left = os.listdir(tmp / "d")
    assert len(left) == 1 and is_resume_part(left[0])
    drop_parts(dst, resume_part_paths(LONG))
    assert os.listdir(tmp / "d") == []


def test_普通名不受影响(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    with pytest.raises(Boom):
        _copy(src, dst, "n.bin", state, _stop_at(3))
    assert os.listdir(tmp / "d") == ["n.bin.xferq.part"]
    assert _copy(src, dst, "n.bin", state).status == "copied"
    assert os.listdir(tmp / "d") == ["n.bin"]


def test_非名字类错误照常抛_不降级(env):
    src, dst, tmp = env

    class Denied(LocalStorage):
        def open_resume(self, path, offset):
            raise PermissionError(errno.EACCES, "拒绝", path)

    with pytest.raises(PermissionError):
        _copy(src, Denied(tmp / "d"), LONG, ResumeState(chunk=CH))
    assert os.listdir(tmp / "d") == []


def test_所有候选都被拒_抛出(env):
    src, dst, tmp = env
    name = "长" * 84            # 无扩展名：只有一个候选，且放不下
    (tmp / "s" / name).write_bytes(DATA)
    with pytest.raises(OSError) as ei:
        _copy(src, dst, name, ResumeState(chunk=CH))
    assert is_name_rejected(ei.value)


def test_最后一块不足整块且已写完_续传直接转正(env):
    """旧实现 offset=n*chunk 会越过文件末尾（如 10 字节、块 8：两块都在时算成 16），open_resume 抛 ValueError。"""
    src, dst, tmp = env
    data = DATA[:2 * CH + 3]                      # 最后一块只有 3 字节
    (tmp / "s" / "n.bin").write_bytes(data)
    state = ResumeState(chunk=CH)
    with pytest.raises(Boom):
        _copy(src, dst, "n.bin", state, _stop_at(2))   # 第 2 块（最后一块）写完并记录哈希后中断
    assert len(state.chunks) == 3
    part = [f for f in os.listdir(tmp / "d")]
    assert len(part) == 1 and (tmp / "d" / part[0]).stat().st_size == len(data)

    idxs = []
    r = _copy(src, dst, "n.bin", state, lambda d, i, s: idxs.append(i))
    assert r.status == "copied" and idxs == []          # 没有再写任何块
    assert (tmp / "d" / "n.bin").read_bytes() == data
    assert os.listdir(tmp / "d") == ["n.bin"]
