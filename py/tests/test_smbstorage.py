"""pybase.smbstorage：用替身 SMBClient / smbclient 操作层（映射到本地临时目录）覆盖，不连任何真实服务器。

真实 SMB 服务集成：本机没有 smbd / smbprotocol 时 skip（见最后一个用例）。
"""

import os
import shutil
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from pybase import smbstorage  # noqa: E402
from pybase.fileops import copy_file_resumable  # noqa: E402
from pybase.smbstorage import SMBStorage, set_credential_provider  # noqa: E402
from pybase.storage import LocalStorage  # noqa: E402



class FakeConn:
    class transport:
        connected = True


class FakeClient:
    """像 SMBClient：`_unc` / `_cache` / connect / disconnect / mkdir / list_dir。"""
    instances = []

    def __init__(self, address, share, username="", password="", port=445, *, base=None, state=None):
        self.address, self.share, self.username, self.password, self.port = address, share, username, password, port
        self.base, self.state = base, state
        self._cache, self._connected = {}, False
        self.mkdirs = []
        FakeClient.instances.append(self)

    def _unc(self, path=""):
        rel = (path or "").replace("/", "\\").strip("\\")
        return f"\\\\{self.address}\\{self.share}" + (f"\\{rel}" if rel else "")

    def connect(self):
        if not self.state["up"]:
            raise ValueError("Failed to connect")
        self._cache[f"{self.address}:{self.port}"] = FakeConn()
        self._connected = True
        self.state["connects"] += 1
        return True

    def disconnect(self):
        self._cache.clear()
        self._connected = False

    def mkdir(self, path):
        self.mkdirs.append(path)
        os.makedirs(os.path.join(self.base, path), exist_ok=True)
        return True

    def is_alive(self):
        return self._connected and all(v.transport.connected for v in self._cache.values())

    def _l(self, path):
        return os.path.join(self.base, path)

    def stat(self, path):
        try:
            st = os.lstat(self._l(path))
        except FileNotFoundError:
            return None
        d = os.path.isdir(self._l(path))
        return {"is_dir": d, "size": 0 if d else st.st_size, "mtime": st.st_mtime}

    def open_read(self, path):
        return open(self._l(path), "rb")

    def open_write(self, path):
        os.makedirs(os.path.dirname(self._l(path)), exist_ok=True)
        return open(self._l(path), "wb")

    def open_resume(self, path, offset):
        # 偏移语义由真实 SMBClient 的测试（test_smb_stream）覆盖；这里只要能续写
        if not os.path.exists(self._l(path)):
            if offset:
                raise FileNotFoundError(path)
            return self.open_write(path)
        f = open(self._l(path), "r+b")
        size = f.seek(0, 2)
        if offset > size:
            f.close()
            raise ValueError("补洞")
        f.truncate(offset)
        f.seek(offset)
        return f

    def replace(self, a, b):
        os.replace(self._l(a), self._l(b))

    def remove(self, path):
        os.remove(self._l(path))

    def rmdir(self, path):
        os.rmdir(self._l(path))

    def set_mtime(self, path, mtime):
        os.utime(self._l(path), (mtime, mtime))

    def list_dir(self, path=""):
        import datetime
        out = []
        for n in os.listdir(os.path.join(self.base, path)):
            st = os.lstat(os.path.join(self.base, path, n))
            d = os.path.isdir(os.path.join(self.base, path, n))
            out.append({"name": n, "path": f"{path}/{n}".lstrip("/"), "is_dir": d, "size": 0 if d else st.st_size,
                        "modified_at": datetime.datetime.fromtimestamp(st.st_mtime).isoformat()})
        return out


@pytest.fixture
def env(tmp_path):
    base = tmp_path / "share"
    base.mkdir()
    state = {"up": True, "connects": 0}
    made = []

    def make(root="", **kw):
        s = SMBStorage("srv", "share", root, user="u", password="pw", client_factory=lambda h, sh, u, p, port: FakeClient(
            h, sh, u, p, port, base=str(base), state=state), probe=lambda h, p, t: state["up"], **kw)
        made.append(s)
        return s
    yield make, base, state
    for s in made:
        s.close()


def test_读写往返_自动建上级目录(env):
    make, base, _ = env
    st = make("proj")
    with st.open_write("a/b/c.txt") as f:
        f.write(b"hello")
    assert (base / "proj/a/b/c.txt").read_bytes() == b"hello"       # 路径换算到 root 下
    with st.open_read("a/b/c.txt") as f:
        assert f.read() == b"hello"
    e = st.stat("a/b/c.txt")
    assert (e.name, e.path, e.is_dir, e.size) == ("c.txt", "a/b/c.txt", False, 5)
    assert st.stat("a/b").is_dir
    assert st.stat("nope") is None
    assert st.stat("") .is_dir                                          # root 本身


def test_listdir相对root(env):
    make, base, _ = env
    st = make("r")
    (base / "r/d").mkdir(parents=True)
    (base / "r/d/f").write_bytes(b"12")
    (base / "r/g").write_bytes(b"1")
    top = {e.name: e for e in st.listdir()}
    assert top["d"].is_dir and top["d"].path == "d" and not top["g"].is_dir and top["g"].size == 1
    sub = st.listdir("d")
    assert [(e.name, e.path, e.size) for e in sub] == [("f", "d/f", 2)]
    assert isinstance(sub[0].mtime, float)


@pytest.mark.parametrize("bad", ["../x", "a/../../x", "/etc/passwd", ".."])
def test_越出root被拒(env, bad):
    make, *_ = env
    st = make("r")
    for fn in (st.stat, st.open_read, st.open_write, st.listdir, st.remove, st.rmdir):
        with pytest.raises(ValueError):
            fn(bad)
    with pytest.raises(ValueError):
        st.replace("ok", bad)
    with pytest.raises(ValueError):
        st.open_resume(bad, 0)


def test_root含点点被拒():
    with pytest.raises(ValueError):
        SMBStorage("h", "s", "a/../b")


def test_open_resume偏移语义(env):
    make, base, _ = env
    st = make("r")
    # 不存在 + offset=0：新建，自动建目录
    with st.open_resume("d/f", 0) as f:
        f.write(b"0123456789")
    assert (base / "r/d/f").read_bytes() == b"0123456789"
    # 已存在：截断到 offset 再续写
    with st.open_resume("d/f", 4) as f:
        assert f.tell() == 4
        f.write(b"XY")
    assert (base / "r/d/f").read_bytes() == b"0123XY"
    # offset == 文件长度：追加
    with st.open_resume("d/f", 6) as f:
        f.write(b"!")
    assert (base / "r/d/f").read_bytes() == b"0123XY!"
    # offset 超过文件长度：不能补洞
    with pytest.raises(ValueError):
        st.open_resume("d/f", 100)
    assert (base / "r/d/f").read_bytes() == b"0123XY!"
    # 不存在 + offset>0
    with pytest.raises(FileNotFoundError):
        st.open_resume("none", 3)
    with pytest.raises(ValueError):
        st.open_resume("d/f", -1)


def test_replace_remove_rmdir_set_mtime(env):
    make, base, _ = env
    st = make("r")
    for n, c in (("a", b"1"), ("b", b"2")):
        with st.open_write(n) as f:
            f.write(c)
    st.replace("a", "b")
    assert (base / "r/b").read_bytes() == b"1" and not (base / "r/a").exists()
    st.set_mtime("b", 1_500_000_000)
    assert int(os.stat(base / "r/b").st_mtime) == 1_500_000_000
    st.remove("b")
    assert st.stat("b") is None
    (base / "r/d").mkdir()
    (base / "r/d/x").write_bytes(b"")
    with pytest.raises(OSError):
        st.rmdir("d")
    st.remove("d/x")
    st.rmdir("d")
    assert not (base / "r/d").exists()


def test_available_从未连接主动连一次并复用(env):
    make, _, state = env
    st = make()
    assert st._client is None
    assert st.available()
    assert state["connects"] == 1
    assert st.available() and state["connects"] == 1      # 复用


def test_available_连不上为False不抛异常(env):
    make, _, state = env
    st = make()
    state["up"] = False
    assert not st.available()
    state["up"] = True
    assert st.available()


def test_available_连接已断会重连(env):
    make, _, state = env
    st = make()
    assert st.available()
    st._client._cache["srv:445"].transport.connected = False      # 模拟 TCP 断了
    assert st._alive() is False
    assert st.available() and state["connects"] == 2
    state["up"] = False
    st._client._cache["srv:445"].transport.connected = False
    assert not st.available()


def test_连不上操作抛OSError_不当作不存在(env):
    make, _, state = env
    st = make()
    state["up"] = False
    with pytest.raises(OSError):
        st.stat("x")


def test_规格往返_凭据不进规格():
    s = SMBStorage("10.0.0.5", "共享", "a b/c", user="DOM\\张三", password="secret", port=4450)
    spec = smbstorage.smb_spec(s)
    assert "secret" not in spec and spec.startswith("smb://")
    s2 = smbstorage.smb_from_spec(spec)
    assert (s2.host, s2.share, s2.root, s2.user, s2.port) == ("10.0.0.5", "共享", "a b/c", "DOM\\张三", 4450)
    assert smbstorage.smb_spec(s2) == spec
    plain = SMBStorage("h", "s")
    assert smbstorage.smb_spec(plain) == "smb://h/s"
    assert smbstorage.smb_from_spec("smb://h/s").root == ""
    with pytest.raises(ValueError):
        smbstorage.smb_from_spec("smb://h")


def test_transfer_规格接线():
    from pybase.transfer import storage_from_spec, storage_to_spec
    s = SMBStorage("h", "s", "r", user="u", password="pw")
    spec = storage_to_spec(s)
    assert spec == "smb://u@h/s/r" and "pw" not in spec
    back = storage_from_spec(spec)
    assert isinstance(back, SMBStorage) and (back.host, back.share, back.root, back.user) == ("h", "s", "r", "u")


def test_密码来源_参数优先_其次回调():
    calls = []
    set_credential_provider(lambda h, sh, u, p: calls.append((h, sh, u, p)) or "from-provider")
    try:
        s = SMBStorage("h", "s", user="u")
        assert s._pw() == "from-provider" and calls == [("h", "s", "u", 445)]
        assert SMBStorage("h", "s", user="u", password="explicit")._pw() == "explicit"
    finally:
        set_credential_provider(None)
    assert SMBStorage("h", "s")._pw() == ""


def test_真实SMBClient拿到的凭据():
    pytest.importorskip("smbclient")
    set_credential_provider(lambda *a: "pw")
    try:
        c = SMBStorage("h", "s", user="u")._make_client()
        assert (c.address, c.share, c.username, c.password, c.port) == ("h", "s", "u", "pw", 445)
    finally:
        set_credential_provider(None)


def test_copy_file_resumable_本地到SMB_断点续传(env, tmp_path):
    make, base, _ = env
    src = LocalStorage(tmp_path / "src")
    (tmp_path / "src").mkdir()
    data = os.urandom(100)
    (tmp_path / "src/f.bin").write_bytes(data)
    dst = make("out")
    seen = []

    class Stop(Exception):
        pass

    def stop_at_3(done, idx, sha):
        seen.append(done)
        if idx == 2:
            raise Stop()
    from pybase.fileops import ResumeState
    state = ResumeState(chunk=10)
    with pytest.raises(Stop):
        copy_file_resumable(src, "f.bin", dst, "f.bin", part_path="f.bin.part", chunk=10, progress=stop_at_3, state=state)
    res = copy_file_resumable(src, "f.bin", dst, "f.bin", part_path="f.bin.part", chunk=10, state=state)
    assert res.ok
    assert (base / "out/f.bin").read_bytes() == data
    assert not (base / "out/f.bin.part").exists()


@pytest.mark.skipif(not (shutil.which("smbd") and os.environ.get("PYBASE_SMBD_TEST")),
                    reason="集成测试需要本机 smbd 与 smbprotocol，并设 PYBASE_SMBD_TEST=1；本机未满足，已由替身覆盖")
def test_集成_本机smbd():
    pytest.importorskip("smbclient")
    pytest.skip("需自行配置 smbd 共享后补全地址与凭据")
