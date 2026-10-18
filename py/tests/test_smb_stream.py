"""pybase.smb.stream（SMBClient 的 stat/流式读写/续写/replace/remove/rmdir/set_mtime）与 is_alive。

用替身 smbclient 模块（UNC -> 本地临时目录）覆盖，不连任何服务器。
"""

import errno
import os
import sys
import types

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
pytest.importorskip("smbclient")

from pybase.smb import client as client_mod, stream  # noqa: E402
from pybase.smb.client import SMBClient  # noqa: E402

HEAD = "\\\\srv\\share"


class FakeSmbclient(types.SimpleNamespace):
    def __init__(self, base):
        super().__init__()
        self.base, self.calls = base, []

    def _l(self, unc):
        assert unc.startswith(HEAD), unc
        self.calls.append(unc)
        return os.path.join(self.base, unc[len(HEAD):].replace("\\", "/").lstrip("/"))

    def lstat(self, unc, **kw):
        return os.lstat(self._l(unc))

    def open_file(self, unc, mode="rb", **kw):
        self.last_kw = kw
        return open(self._l(unc), mode)

    def remove(self, unc, **kw):
        os.remove(self._l(unc))

    def rmdir(self, unc, **kw):
        os.rmdir(self._l(unc))

    def replace(self, a, b, **kw):
        os.replace(self._l(a), self._l(b))

    def utime(self, unc, times=None, **kw):
        os.utime(self._l(unc), times)

    def makedirs(self, unc, exist_ok=False, **kw):
        os.makedirs(self._l(unc), exist_ok=exist_ok)

    def scandir(self, unc, **kw):
        raise AssertionError("list_dir 在这里被替换")


@pytest.fixture
def c(tmp_path, monkeypatch):
    fake = FakeSmbclient(str(tmp_path))
    monkeypatch.setattr(stream, "smbclient", fake)
    from pybase.smb import upload
    monkeypatch.setattr(upload, "smbclient", fake)
    cl = SMBClient("srv", "share", "u", "p")
    cl._connected = True                     # 不真连
    cl.fake, cl.base = fake, tmp_path

    def list_dir(path=""):
        return [{"name": n} for n in os.listdir(tmp_path / path)]
    cl.list_dir = list_dir
    return cl


def test_stat_存在与不存在(c):
    (c.base / "d").mkdir()
    (c.base / "d/f").write_bytes(b"abc")
    os.utime(c.base / "d/f", (1_500_000_000.5, 1_500_000_000.5))
    d = c.stat("d/f")
    assert (d["name"], d["path"], d["is_dir"], d["size"]) == ("f", "d/f", False, 3)
    assert d["mtime"] == 1_500_000_000.5 and d["modified_at"]
    assert c.stat("d")["is_dir"] and c.stat("d")["size"] == 0
    assert c.stat("nope/x") is None


def test_stat_其它错误照抛(c, monkeypatch):
    def boom(unc, **kw):
        raise PermissionError(errno.EACCES, "denied")
    monkeypatch.setattr(c.fake, "lstat", boom)
    with pytest.raises(PermissionError):
        c.stat("x")


def test_open_read用最小读权限与共享模式(c):
    (c.base / "f").write_bytes(b"hi")
    with c.open_read("f") as f:
        assert f.read() == b"hi"
    assert c.fake.last_kw["share_access"] == "rwd"
    assert c.fake.last_kw["desired_access"] == stream._READ_ACCESS


def test_open_write自动建上级并复用目录缓存(c):
    with c.open_write("a/b/c.txt") as f:
        f.write(b"x")
    assert (c.base / "a/b/c.txt").read_bytes() == b"x"
    assert "a/b" in c._ensured_dirs
    with c.open_write("a/b/d.txt") as f:
        f.write(b"y")


def test_open_resume偏移语义(c):
    with c.open_resume("d/f", 0) as f:
        f.write(b"0123456789")
    with c.open_resume("d/f", 4) as f:
        assert f.tell() == 4
        f.write(b"XY")
    assert (c.base / "d/f").read_bytes() == b"0123XY"
    with c.open_resume("d/f", 6) as f:
        f.write(b"!")
    assert (c.base / "d/f").read_bytes() == b"0123XY!"
    with pytest.raises(ValueError):
        c.open_resume("d/f", 100)
    assert (c.base / "d/f").read_bytes() == b"0123XY!"
    with pytest.raises(FileNotFoundError):
        c.open_resume("none", 3)
    with pytest.raises(ValueError):
        c.open_resume("d/f", -1)


def test_replace覆盖_remove_set_mtime(c):
    (c.base / "a").write_bytes(b"1")
    (c.base / "b").write_bytes(b"2")
    c.replace("a", "b")
    assert (c.base / "b").read_bytes() == b"1" and not (c.base / "a").exists()
    c.set_mtime("b", 1_500_000_000)
    assert int(os.stat(c.base / "b").st_mtime) == 1_500_000_000
    c.remove("b")
    assert c.stat("b") is None


def test_rmdir只删空目录并清目录缓存(c):
    c.mkdir("d/e")
    assert "d/e" in c._ensured_dirs
    (c.base / "d/e/x").write_bytes(b"")
    with pytest.raises(OSError) as ei:
        c.rmdir("d/e")
    assert ei.value.errno == errno.ENOTEMPTY
    c.remove("d/e/x")
    c.rmdir("d/e")
    assert not (c.base / "d/e").exists()
    assert "d/e" not in c._ensured_dirs and "d" in c._ensured_dirs


def test_is_alive():
    cl = SMBClient("srv", "share")
    assert not cl.is_alive()
    cl._connected = True
    assert cl.is_alive()                                  # 无会话缓存：只看标志

    class Conn:
        class transport:
            connected = True
    cl._cache["srv:445"] = Conn()
    assert cl.is_alive()
    Conn.transport.connected = False
    assert not cl.is_alive()


def test_connect固定ntlm与建连超时(monkeypatch):
    seen = []
    monkeypatch.setattr(client_mod.smbclient, "register_session", lambda addr, **kw: seen.append(kw))
    monkeypatch.setattr(client_mod, "ensure_connection", lambda *a, **k: None)   # 传输层另有 test_smb_wgnet 覆盖
    cl = SMBClient("srv", "share", "u", "p")
    cl.connect()
    assert seen[0]["auth_protocol"] == "ntlm" and seen[0]["connection_timeout"] == client_mod.CONNECT_TIMEOUT
