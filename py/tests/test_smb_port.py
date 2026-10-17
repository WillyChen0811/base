"""非默认端口（如经 127.0.0.1:1445 转发）时，所有 smbclient 文件调用都必须带上该端口。

smbclient 查会话时 port 默认 445：只在 register_session 带端口、文件操作不带，会话就对不上，
文件操作去连 127.0.0.1:445。用记录型替身逐调用断言 port。
"""

import os
import sys
import types

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
pytest.importorskip("smbclient")

from pybase.smb import client as client_mod, download, listing, stream, upload  # noqa: E402
from pybase.smb.client import SMBClient  # noqa: E402

HEAD = "\\\\srv\\share"


class Rec(types.SimpleNamespace):
    """每个 smbclient.* 调用记下 (函数名, port)，再落到本地临时目录。"""

    def __init__(self, base):
        super().__init__()
        self.base, self.seen = base, []

    def _l(self, name, unc, kw):
        self.seen.append((name, kw.get("port", 445)))
        return os.path.join(self.base, unc[len(HEAD):].replace("\\", "/").lstrip("/"))

    def lstat(self, unc, **kw):
        return os.lstat(self._l("lstat", unc, kw))

    def stat(self, unc, **kw):
        return os.stat(self._l("stat", unc, kw))

    def open_file(self, unc, mode="rb", **kw):
        return open(self._l("open_file", unc, kw), mode)

    def remove(self, unc, **kw):
        os.remove(self._l("remove", unc, kw))

    def rmdir(self, unc, **kw):
        os.rmdir(self._l("rmdir", unc, kw))

    def replace(self, a, b, **kw):
        os.replace(self._l("replace", a, kw), self._l("replace", b, kw))

    def rename(self, a, b, **kw):
        os.rename(self._l("rename", a, kw), self._l("rename", b, kw))

    def utime(self, unc, times=None, **kw):
        os.utime(self._l("utime", unc, kw), times)

    def makedirs(self, unc, exist_ok=False, **kw):
        os.makedirs(self._l("makedirs", unc, kw), exist_ok=exist_ok)

    def scandir(self, unc, **kw):
        self._l("scandir", unc, kw)
        return iter(())

    def exists(self, unc, **kw):
        return os.path.exists(self._l("exists", unc, kw))

    def set_basic(self, unc, **kw):
        self._l("set_basic_information", unc, kw)


@pytest.fixture(params=[1445, 445])
def c(request, tmp_path, monkeypatch):
    fake = Rec(str(tmp_path))
    for mod in (stream, upload, listing, download, client_mod):
        monkeypatch.setattr(mod, "smbclient", fake)
    monkeypatch.setattr(listing, "smbpath", types.SimpleNamespace(exists=fake.exists))
    monkeypatch.setattr(upload, "_set_basic_information", fake.set_basic)
    cl = SMBClient("srv", "share", "u", "p", port=request.param)
    cl._connected = True
    cl.fake, cl.base, cl.port_expected = fake, tmp_path, request.param
    return cl


def _ports(c, name):
    got = [p for n, p in c.fake.seen if n == name]
    assert got, f"没有调用 {name}"
    return got


def test_stream_各操作带端口(c, tmp_path):
    (tmp_path / "f").write_bytes(b"abc")
    c.stat("f")
    with c.open_read("f"):
        pass
    with c.open_write("w"):
        pass
    with c.open_resume("f", 3):
        pass
    c.replace("w", "w2")
    c.set_mtime("w2", 1_500_000_000)
    c.remove("w2")
    (tmp_path / "d").mkdir()
    c.rmdir("d")
    for n in ("lstat", "open_file", "replace", "utime", "remove", "rmdir"):
        assert set(_ports(c, n)) == {c.port_expected}, n


def test_listing_带端口(c):
    c.list_dir("")
    c.exists("")
    assert set(_ports(c, "scandir")) == {c.port_expected}
    assert set(_ports(c, "exists")) == {c.port_expected}


def test_upload_带端口(c, tmp_path):
    src = tmp_path / "local.bin"
    src.write_bytes(b"x" * 10)
    c._ensured_dirs.add("sub")
    os.makedirs(tmp_path / "sub")
    r = c.upload_file(str(src), "sub/a.bin")
    assert r["success"], r
    c.mkdir("m1")
    c.rename("sub/a.bin", "sub/b.bin")
    for n in ("open_file", "stat", "replace", "set_basic_information", "makedirs", "rename"):
        assert set(_ports(c, n)) == {c.port_expected}, n


def test_download_带端口(c, tmp_path):
    (tmp_path / "f").write_bytes(b"hello")
    assert c.download_file("f") == b"hello"
    c.hash_file("f")
    assert set(_ports(c, "open_file")) == {c.port_expected}
    assert len(_ports(c, "open_file")) == 2


def test_safe_delete_带端口(c, tmp_path):
    (tmp_path / "f").write_bytes(b"1")
    c._safe_delete(c._unc("f"))
    assert _ports(c, "remove") == [c.port_expected]
