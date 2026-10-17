"""pybase.fileops.copy_file_resumable 与 Storage.open_resume：块级续传、回退、源被改、不覆盖。"""

import os
import sys

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

from pybase.fileops import ResumeState, copy_file_resumable  # noqa: E402
from pybase.storage import LocalStorage  # noqa: E402

CH = 8
DATA = bytes(range(256)) * 1 + b"tail"           # 260 字节，33 块，最后一块 4 字节
PART = "d/x.bin.part"


class Boom(Exception):
    pass


@pytest.fixture
def env(tmp_path):
    (tmp_path / "s").mkdir()
    (tmp_path / "d").mkdir()
    (tmp_path / "s" / "x.bin").write_bytes(DATA)
    return LocalStorage(tmp_path / "s"), LocalStorage(tmp_path / "d"), tmp_path


def _interrupt_at(n):
    seen = []

    def cb(done, idx, sha):
        seen.append((done, idx, sha))
        if idx == n:
            raise Boom()
    return cb, seen


def _run(src, dst, state, progress=None, **kw):
    return copy_file_resumable(src, "x.bin", dst, "d/x.bin", part_path=PART, chunk=CH,
                               progress=progress, state=state, **kw)


def test_open_resume_本地(tmp_path):
    st = LocalStorage(tmp_path)
    with st.open_resume("a/b/f", 0) as f:                # 不存在且 offset=0：新建并建上级目录
        f.write(b"0123456789")
    with st.open_resume("a/b/f", 4) as f:                # 截到 4 再接着写
        f.write(b"XY")
    assert (tmp_path / "a/b/f").read_bytes() == b"0123XY"
    with pytest.raises(FileNotFoundError):
        st.open_resume("nope", 3)
    with pytest.raises(ValueError):
        st.open_resume("a/b/f", 99)
    with pytest.raises(ValueError):
        st.open_resume("a/b/f", -1)


def test_中途断开后续传_从断点读源_逐字节一致(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    cb, seen = _interrupt_at(10)
    with pytest.raises(Boom):
        _run(src, dst, state, cb)
    assert len(state.chunks) == 11 and (tmp / "d/d/x.bin.part").exists() and not (tmp / "d/d/x.bin").exists()

    seeks = []
    real = src.open_read

    def spy(path):
        f = real(path)
        orig = f.seek
        f.seek = lambda *a: (seeks.append(a[0]), orig(*a))[1]
        return f
    src.open_read = spy
    cb2, seen2 = _interrupt_at(10 ** 9)
    r = _run(src, dst, state, cb2)
    assert r.status == "copied" and r.ok
    assert (tmp / "d/d/x.bin").read_bytes() == DATA
    assert not (tmp / "d/d/x.bin.part").exists()
    assert seen2[0][1] == 11 and seen2[0][0] == 12 * CH      # 从第 11 块接着传，没重传前面的
    assert 11 * CH in seeks


def test_重新建state也能续_靠调用方持久化的块表(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    cb, _ = _interrupt_at(5)
    with pytest.raises(Boom):
        _run(src, dst, state, cb)
    saved = ResumeState(size=state.size, mtime=state.mtime, chunk=CH, chunks=list(state.chunks))
    assert _run(src, dst, saved).status == "copied"
    assert (tmp / "d/d/x.bin").read_bytes() == DATA


def test_最后一块损坏_回退重传(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    cb, _ = _interrupt_at(9)
    with pytest.raises(Boom):
        _run(src, dst, state, cb)
    part = tmp / "d/d/x.bin.part"
    raw = bytearray(part.read_bytes())
    raw[9 * CH + 1] ^= 0xFF                                  # 第 9 块（最后确认的一块）坏了
    raw[8 * CH + 1] ^= 0xFF                                  # 第 8 块也坏 -> 连退两块
    part.write_bytes(bytes(raw))
    cb2, seen = _interrupt_at(10 ** 9)
    r = _run(src, dst, state, cb2)
    assert r.status == "copied" and (tmp / "d/d/x.bin").read_bytes() == DATA
    assert seen[0][1] == 8                                   # 第 8 块起重传


def test_临时文件比记录短_退到对得上为止(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    cb, _ = _interrupt_at(6)
    with pytest.raises(Boom):
        _run(src, dst, state, cb)
    (tmp / "d/d/x.bin.part").write_bytes(DATA[:3 * CH + 2])  # 只剩 3 块多一点
    cb2, seen = _interrupt_at(10 ** 9)
    assert _run(src, dst, state, cb2).status == "copied"
    assert seen[0][1] == 3 and (tmp / "d/d/x.bin").read_bytes() == DATA


def test_临时文件丢了_从头来(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    cb, _ = _interrupt_at(6)
    with pytest.raises(Boom):
        _run(src, dst, state, cb)
    (tmp / "d/d/x.bin.part").unlink()
    cb2, seen = _interrupt_at(10 ** 9)
    assert _run(src, dst, state, cb2).status == "copied"
    assert seen[0][1] == 0 and (tmp / "d/d/x.bin").read_bytes() == DATA


def test_源被改后从头重来(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    cb, _ = _interrupt_at(10)
    with pytest.raises(Boom):
        _run(src, dst, state, cb)
    new = bytes(reversed(DATA))
    (tmp / "s/x.bin").write_bytes(new)                       # 大小不变，只变内容和 mtime
    os.utime(tmp / "s/x.bin", (1_700_000_000, 1_700_000_000))
    cb2, seen = _interrupt_at(10 ** 9)
    r = _run(src, dst, state, cb2)
    assert r.status == "copied" and seen[0][1] == 0
    assert (tmp / "d/d/x.bin").read_bytes() == new


def test_源在拷贝过程中被改_自动重来一遍(env):
    src, dst, tmp = env
    hit = []

    def cb(done, idx, sha):
        if idx == 3 and not hit:
            hit.append(1)
            (tmp / "s/x.bin").write_bytes(DATA + b"more")
            os.utime(tmp / "s/x.bin", (1_700_000_001, 1_700_000_001))
    r = _run(src, dst, ResumeState(chunk=CH), cb)
    assert r.status == "copied"
    assert (tmp / "d/d/x.bin").read_bytes() == DATA + b"more"


def test_目标已存在且不同_不覆盖(env):
    src, dst, tmp = env
    (tmp / "d/d").mkdir()
    other = bytes(len(DATA))
    (tmp / "d/d/x.bin").write_bytes(other)
    assert _run(src, dst, None).status == "conflict"
    assert (tmp / "d/d/x.bin").read_bytes() == other
    (tmp / "d/d/x.bin").write_bytes(b"short")
    assert _run(src, dst, None).status == "conflict"
    assert (tmp / "d/d/x.bin").read_bytes() == b"short"


def test_目标已存在且相同_算已存在_清掉残留临时文件(env):
    src, dst, tmp = env
    (tmp / "d/d").mkdir()
    (tmp / "d/d/x.bin").write_bytes(DATA)
    (tmp / "d/d/x.bin.part").write_bytes(b"junk")
    assert _run(src, dst, None).status == "already-present"
    assert not (tmp / "d/d/x.bin.part").exists()


def test_源不存在_空文件_保留mtime(env):
    src, dst, tmp = env
    assert copy_file_resumable(src, "nope", dst, "n", part_path="n.part", chunk=CH).status == "source-missing"
    (tmp / "s/empty").write_bytes(b"")
    os.utime(tmp / "s/empty", (1_600_000_000, 1_600_000_000))
    r = copy_file_resumable(src, "empty", dst, "e/empty", part_path="e/empty.part", chunk=CH)
    assert r.status == "copied" and (tmp / "d/e/empty").read_bytes() == b""
    os.utime(tmp / "s/x.bin", (1_600_000_000, 1_600_000_000))
    _run(src, dst, None)
    assert int((tmp / "d/d/x.bin").stat().st_mtime) == 1_600_000_000


def test_块大小变了_清零重来(env):
    src, dst, tmp = env
    state = ResumeState(chunk=CH)
    cb, _ = _interrupt_at(5)
    with pytest.raises(Boom):
        _run(src, dst, state, cb)
    r = copy_file_resumable(src, "x.bin", dst, "d/x.bin", part_path=PART, chunk=16, state=state)
    assert r.status == "copied" and (tmp / "d/d/x.bin").read_bytes() == DATA
