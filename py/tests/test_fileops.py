"""pybase.storage / fileops / retry：拷贝（默认不校验 / 校验开关）、不覆盖冲突、断线续传、有前提的删除、空目录收拾。"""

import os
import sys

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

from pybase.fileops import copy_file, delete_confirmed, hash_file, prune_empty_dirs  # noqa: E402
from pybase.retry import retry_call, wait_until  # noqa: E402
from pybase.storage import LocalStorage  # noqa: E402


@pytest.fixture
def stores(tmp_path):
    (tmp_path / "src" / "a").mkdir(parents=True)
    (tmp_path / "src" / "a" / "x.bin").write_bytes(b"hello" * 1000)
    (tmp_path / "dst").mkdir()
    return LocalStorage(tmp_path / "src"), LocalStorage(tmp_path / "dst"), tmp_path


def test_默认不校验_不算哈希(stores, monkeypatch):
    src, dst, tmp = stores
    import pybase.fileops as fo
    monkeypatch.setattr(fo, "new_hasher", lambda *a: (_ for _ in ()).throw(AssertionError("不该算哈希")))
    r = copy_file(src, "a/x.bin", dst, "y.bin")
    assert r.status == "copied" and r.digest is None and r.verified is False
    assert (tmp / "dst/y.bin").read_bytes() == b"hello" * 1000
    assert copy_file(src, "a/x.bin", dst, "y.bin").status == "already-present"   # 只比大小
    (tmp / "dst/z.bin").write_bytes(b"short")
    assert copy_file(src, "a/x.bin", dst, "z.bin").status == "conflict"


def test_校验开_三方哈希一致_保留修改时间(stores):
    src, dst, tmp = stores
    os.utime(tmp / "src/a/x.bin", (1_600_000_000, 1_600_000_000))
    r = copy_file(src, "a/x.bin", dst, "b/y.bin", verify=True)
    assert r.status == "copied" and r.ok and r.verified
    assert (tmp / "dst/b/y.bin").read_bytes() == b"hello" * 1000
    assert r.digest == hash_file(dst, "b/y.bin")
    assert int((tmp / "dst/b/y.bin").stat().st_mtime) == 1_600_000_000
    assert not (tmp / "dst/b/y.bin.part").exists()


def test_再拷一次算已存在_不重写(stores):
    src, dst, tmp = stores
    copy_file(src, "a/x.bin", dst, "y.bin", verify=True)
    before = (tmp / "dst/y.bin").stat().st_mtime_ns
    r = copy_file(src, "a/x.bin", dst, "y.bin", verify=True)
    assert r.status == "already-present"
    assert (tmp / "dst/y.bin").stat().st_mtime_ns == before


def test_目标处是同大小的另一版_校验时不覆盖(stores):
    src, dst, tmp = stores
    (tmp / "dst/y.bin").write_bytes(b"world" * 1000)
    r = copy_file(src, "a/x.bin", dst, "y.bin", verify=True)
    assert r.status == "conflict" and not r.ok
    assert (tmp / "dst/y.bin").read_bytes() == b"world" * 1000


def test_源与期望哈希不符_不落正式文件(stores):
    src, dst, tmp = stores
    r = copy_file(src, "a/x.bin", dst, "y.bin", expected_hash="0" * 64)
    assert r.status == "source-hash-differs"
    assert list((tmp / "dst").iterdir()) == []


def test_blake2b_256(stores):
    src, dst, _ = stores
    r = copy_file(src, "a/x.bin", dst, "y.bin", verify=True, algorithm="blake2b-256")
    assert r.ok and len(r.digest) == 64


class FlakyStorage(LocalStorage):
    """写到一半掉线一次，掉线期间 available() 为假，之后恢复。"""

    def __init__(self, root):
        super().__init__(root)
        self.broken = False
        self.failures_left = 1

    def available(self):
        return not self.broken and super().available()

    def open_write(self, path):
        f = super().open_write(path)
        storage = self

        class Writer:
            def __enter__(self):
                return self

            def __exit__(self, *a):
                f.close()

            def write(self, data):
                if storage.failures_left:
                    f.write(data[:10])
                    storage.failures_left -= 1
                    storage.broken = True
                    raise OSError(5, "Input/output error")
                return f.write(data)

        return Writer()


def test_中途断线_等恢复后整份重拷_不计重试次数(stores):
    src, _, tmp = stores
    dst = FlakyStorage(tmp / "dst")
    sleeps = []

    def sleep(seconds):
        sleeps.append(seconds)
        dst.broken = False  # 等待期间"重新连上"

    r = retry_call(lambda: copy_file(src, "a/x.bin", dst, "y.bin", verify=True, chunk=1024),
                   attempts=1, delay_seconds=60, available=dst.available, poll_seconds=1200, sleep=sleep)
    assert r.status == "copied"
    assert (tmp / "dst/y.bin").read_bytes() == b"hello" * 1000
    assert sleeps == [1200]          # 只等了恢复，没有消耗唯一的一次重试
    assert not (tmp / "dst/y.bin.part").exists()


def test_资源在但一直失败_用完次数后抛出():
    calls = []

    def boom():
        calls.append(1)
        raise OSError("nope")

    with pytest.raises(OSError):
        retry_call(boom, attempts=3, delay_seconds=0, sleep=lambda s: None)
    assert len(calls) == 3


def test_wait_until_超时返回假():
    t = [0.0]
    ok = wait_until(lambda: False, 10, timeout=25, sleep=lambda s: t.__setitem__(0, t[0] + s), clock=lambda: t[0])
    assert ok is False and t[0] >= 25


def test_删除要经核对_收拾空目录但不动非空(stores):
    src, _, tmp = stores
    (tmp / "src/a/keep.txt").write_text("k")
    (tmp / "src/a/deep/er").mkdir(parents=True)
    (tmp / "src/a/deep/er/z.bin").write_bytes(b"z")
    assert delete_confirmed(src, "a/x.bin", lambda p: "server-missing") == "server-missing"
    assert (tmp / "src/a/x.bin").exists()
    assert delete_confirmed(src, "a/deep/er/z.bin", lambda p: True) == "deleted"
    assert delete_confirmed(src, "a/deep/er/z.bin", lambda p: True) == "missing"
    assert prune_empty_dirs(src, ["a/deep/er"]) == 2
    assert (tmp / "src/a").is_dir() and not (tmp / "src/a/deep").exists()


def test_挂载点不在算不可用(tmp_path):
    s = LocalStorage(tmp_path, mount_point=tmp_path / "not-a-mount")
    assert s.available() is False
    assert LocalStorage(tmp_path).available() is True


def test_相对路径不许越界(tmp_path):
    with pytest.raises(ValueError):
        LocalStorage(tmp_path).stat("../etc/passwd")
