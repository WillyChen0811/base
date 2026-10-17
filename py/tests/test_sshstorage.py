"""pybase.sshstorage：测试进程内起一个只听 127.0.0.1 的 SFTP 服务，不碰任何真实主机。"""

import os
import socket
import sys
import threading

import pytest

paramiko = pytest.importorskip("paramiko")

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

from pybase.fileops import copy_file  # noqa: E402
from pybase.sshstorage import SSHStorage  # noqa: E402
from pybase.storage import LocalStorage  # noqa: E402


class _Handle(paramiko.SFTPHandle):
    def stat(self):
        try:
            return paramiko.SFTPAttributes.from_stat(os.fstat(self.readfile.fileno()))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)

    def chattr(self, attr):
        return paramiko.SFTP_OK


class _SFTP(paramiko.SFTPServerInterface):
    ROOT = ""

    def _r(self, path):
        return os.path.join(self.ROOT, path.lstrip("/"))

    def canonicalize(self, path):
        return "/" + os.path.relpath(self._r(path), self.ROOT).replace("\\", "/").lstrip("./") if path not in ("", "/", ".") else "/"

    def list_folder(self, path):
        try:
            out = []
            for n in os.listdir(self._r(path)):
                a = paramiko.SFTPAttributes.from_stat(os.lstat(os.path.join(self._r(path), n)))
                a.filename = n
                out.append(a)
            return out
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)

    def stat(self, path):
        try:
            return paramiko.SFTPAttributes.from_stat(os.stat(self._r(path)))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)

    def lstat(self, path):
        try:
            return paramiko.SFTPAttributes.from_stat(os.lstat(self._r(path)))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)

    def open(self, path, flags, attr):
        try:
            fd = os.open(self._r(path), flags | getattr(os, "O_BINARY", 0), 0o666)
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)
        mode = "wb" if flags & os.O_WRONLY else ("r+b" if flags & os.O_RDWR else "rb")
        f = os.fdopen(fd, mode)
        h = _Handle(flags)
        h.filename = self._r(path)
        h.readfile = h.writefile = f
        return h

    def remove(self, path):
        try:
            os.remove(self._r(path))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)
        return paramiko.SFTP_OK

    def rename(self, old, new):          # 像真服务器一样：目标已存在就失败
        if os.path.lexists(self._r(new)):
            return paramiko.SFTP_FAILURE
        try:
            os.rename(self._r(old), self._r(new))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)
        return paramiko.SFTP_OK

    def posix_rename(self, old, new):
        try:
            os.replace(self._r(old), self._r(new))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)
        return paramiko.SFTP_OK

    def mkdir(self, path, attr):
        try:
            os.mkdir(self._r(path))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)
        return paramiko.SFTP_OK

    def rmdir(self, path):
        try:
            os.rmdir(self._r(path))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)
        return paramiko.SFTP_OK

    def chattr(self, path, attr):
        try:
            if attr.st_mtime is not None:
                os.utime(self._r(path), (attr.st_atime or attr.st_mtime, attr.st_mtime))
        except OSError as e:
            return paramiko.SFTPServer.convert_errno(e.errno)
        return paramiko.SFTP_OK


class _Auth(paramiko.ServerInterface):
    def check_auth_publickey(self, username, key):
        return paramiko.AUTH_SUCCESSFUL

    def get_allowed_auths(self, username):
        return "publickey"

    def check_channel_request(self, kind, chanid):
        return paramiko.OPEN_SUCCEEDED


class Server:
    def __init__(self, root):
        self.host_key = paramiko.RSAKey.generate(2048)
        self.sock = socket.socket()
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(5)
        self.port = self.sock.getsockname()[1]
        self.transports = []
        sftp_cls = type("S", (_SFTP,), {"ROOT": str(root)})
        self.sftp_cls = sftp_cls
        threading.Thread(target=self._accept, daemon=True).start()

    def _accept(self):
        while True:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            t = paramiko.Transport(conn)
            t.add_server_key(self.host_key)
            t.set_subsystem_handler("sftp", paramiko.SFTPServer, self.sftp_cls)
            t.start_server(server=_Auth())
            self.transports.append(t)

    def drop_clients(self):
        for t in self.transports:
            t.close()
        self.transports.clear()

    def stop(self):
        self.drop_clients()
        try:
            self.sock.shutdown(socket.SHUT_RDWR)   # 唤醒阻塞在 accept 的线程，监听才真正停
        except OSError:
            pass
        self.sock.close()


@pytest.fixture
def env(tmp_path, monkeypatch):
    home = tmp_path / "home"
    (home / ".ssh").mkdir(parents=True)
    monkeypatch.setenv("HOME", str(home))          # known_hosts 为空，不碰真实 ~/.ssh
    monkeypatch.delenv("SSH_AUTH_SOCK", raising=False)
    key = paramiko.RSAKey.generate(2048)
    keyfile = tmp_path / "id_rsa"
    key.write_private_key_file(str(keyfile))
    remote = tmp_path / "remote"
    remote.mkdir()
    srv = Server(remote)
    made = []

    def make(root="/", **kw):
        s = SSHStorage("127.0.0.1", root, port=srv.port, key_filename=str(keyfile),
                       trust_unknown=True, connect_timeout=5, **kw)
        made.append(s)
        return s
    yield make, remote, srv, tmp_path
    for s in made:
        s.close()
    srv.stop()


def test_读写往返_自动建上级目录(env):
    make, remote, _, _ = env
    st = make()
    with st.open_write("a/b/x.txt") as f:
        f.write(b"hello")
    assert (remote / "a/b/x.txt").read_bytes() == b"hello"
    with st.open_read("a/b/x.txt") as f:
        assert f.read() == b"hello"


def test_stat与listdir(env):
    make, remote, _, _ = env
    st = make()
    (remote / "d").mkdir()
    (remote / "d" / "f.bin").write_bytes(b"12345")
    os.utime(remote / "d/f.bin", (1_600_000_000, 1_600_000_000))
    e = st.stat("d/f.bin")
    assert (e.name, e.path, e.is_dir, e.size, e.mtime) == ("f.bin", "d/f.bin", False, 5, 1_600_000_000)
    assert st.stat("d").is_dir
    assert st.stat("").is_dir
    names = {x.path: x for x in st.listdir("d")}
    assert set(names) == {"d/f.bin"} and names["d/f.bin"].size == 5
    assert {x.path for x in st.listdir("")} == {"d"}


def test_不存在返回None(env):
    make, *_ = env
    st = make()
    assert st.stat("nope") is None
    assert st.stat("nope/deeper/x") is None
    with pytest.raises(OSError):
        st.listdir("nope")


def test_root在子目录下_路径换算(env):
    make, remote, _, _ = env
    (remote / "work").mkdir()
    st = make("/work")
    with st.open_write("a.txt") as f:
        f.write(b"x")
    assert (remote / "work/a.txt").exists()
    assert st.stat("a.txt").size == 1


@pytest.mark.parametrize("bad", ["../x", "a/../../x", "/etc/passwd"])
def test_越出root被拒(env, bad):
    make, *_ = env
    st = make("/")
    for call in (st.stat, st.listdir, st.open_read, st.open_write, st.remove, st.rmdir):
        with pytest.raises(ValueError):
            call(bad)
    with pytest.raises(ValueError):
        st.replace("ok", bad)
    with pytest.raises(ValueError):
        st.set_mtime(bad, 1.0)


def test_root必须绝对路径():
    with pytest.raises(ValueError):
        SSHStorage("h", "relative/dir")


def test_replace覆盖目标(env):
    make, remote, _, _ = env
    st = make()
    (remote / "src").write_bytes(b"new")
    (remote / "dst").write_bytes(b"old-old")
    st.replace("src", "dst")
    assert (remote / "dst").read_bytes() == b"new" and not (remote / "src").exists()


def test_replace没有posix_rename扩展时先删再改(env, monkeypatch):
    make, remote, _, _ = env
    st = make()
    st._connect()
    monkeypatch.setattr(st._sftp, "posix_rename", lambda a, b: (_ for _ in ()).throw(IOError("unsupported")))
    (remote / "src").write_bytes(b"new")
    (remote / "dst").write_bytes(b"old")
    st.replace("src", "dst")
    assert (remote / "dst").read_bytes() == b"new"
    with pytest.raises(OSError):                     # 源不存在：不能误删目标
        st.replace("ghost", "dst")
    assert (remote / "dst").read_bytes() == b"new"


def test_remove与rmdir(env):
    make, remote, _, _ = env
    st = make()
    (remote / "d").mkdir()
    (remote / "d" / "f").write_bytes(b"1")
    with pytest.raises(OSError):
        st.rmdir("d")                                # 非空
    st.remove("d/f")
    st.rmdir("d")
    assert not (remote / "d").exists()
    with pytest.raises(OSError):
        st.remove("d/f")


def test_set_mtime(env):
    make, remote, _, _ = env
    st = make()
    (remote / "f").write_bytes(b"1")
    st.set_mtime("f", 1_500_000_000)
    assert int(os.stat(remote / "f").st_mtime) == 1_500_000_000


def test_断线后available为False_下次操作自动重连(env):
    make, remote, srv, _ = env
    st = make()
    assert not st.available()                        # 还没连过
    (remote / "f").write_bytes(b"abc")
    assert st.stat("f").size == 3
    assert st.available()
    srv.drop_clients()
    for _ in range(100):
        if not st.available():
            break
        threading.Event().wait(0.05)
    assert not st.available()
    assert st.stat("f").size == 3                    # 自动重连
    assert st.available()


def test_服务停了_操作抛OSError_available不抛(env):
    make, remote, srv, _ = env
    st = make()
    st.stat("")
    srv.stop()
    for _ in range(100):
        if not st.available():
            break
        threading.Event().wait(0.05)
    assert not st.available()
    with pytest.raises(OSError):
        st.stat("f")                                 # 连不上不能当"不存在"
    assert not st.available()


def test_未知主机默认拒绝(env):
    make, _, srv, tmp = env
    st = SSHStorage("127.0.0.1", "/", port=srv.port, key_filename=str(tmp / "id_rsa"), connect_timeout=5)
    with pytest.raises(OSError):
        st.stat("")
    assert not st.available()


def test_上下文管理器关闭(env):
    make, *_ = env
    with make() as st:
        st.stat("")
        assert st.available()
    assert not st.available()


def test_copy_file_本地与SSH互拷_校验(env):
    make, remote, _, tmp = env
    st = make()
    (tmp / "loc").mkdir()
    local = LocalStorage(tmp / "loc")
    data = os.urandom(300_000)
    (tmp / "loc" / "a.bin").write_bytes(data)
    os.utime(tmp / "loc/a.bin", (1_600_000_000, 1_600_000_000))

    r = copy_file(local, "a.bin", st, "up/a.bin", verify=True)
    assert r.status == "copied" and r.verified
    assert (remote / "up/a.bin").read_bytes() == data
    assert int(os.stat(remote / "up/a.bin").st_mtime) == 1_600_000_000
    assert not (remote / "up/a.bin.part").exists()
    assert copy_file(local, "a.bin", st, "up/a.bin", verify=True).status == "already-present"

    r = copy_file(st, "up/a.bin", local, "back/a.bin", verify=True)
    assert r.status == "copied"
    assert (tmp / "loc/back/a.bin").read_bytes() == data
