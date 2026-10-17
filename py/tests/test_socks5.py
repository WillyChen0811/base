"""pybase.socks5 与 SSHStorage 经代理：桩 SOCKS5 服务断言收到的是域名、拒绝/连不上不回落直连。"""

import os
import socket
import sys
import threading

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, REPO)

from pybase import socks5  # noqa: E402

pytest.importorskip("paramiko")
from test_sshstorage import env  # noqa: E402,F401
from pybase.sshstorage import SSHStorage  # noqa: E402


class Stub:
    """只听 127.0.0.1 的桩 SOCKS5：记下请求，按 reply 回复；成功时把连接转到 forward。"""

    def __init__(self, reply=0, forward=None):
        self.reply, self.forward, self.seen = reply, forward, []
        self.sock = socket.socket()
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(5)
        self.addr = "127.0.0.1:%d" % self.sock.getsockname()[1]
        threading.Thread(target=self._loop, daemon=True).start()

    def _loop(self):
        while True:
            try:
                c, _ = self.sock.accept()
            except OSError:
                return
            threading.Thread(target=self._serve, args=(c,), daemon=True).start()

    def stop(self):
        self.sock.close()


def _serve_conn(stub, c):
    def rd(n):
        b = b""
        while len(b) < n:
            x = c.recv(n - len(b))
            if not x:
                raise EOFError
            b += x
        return b
    try:
        ver, nm = rd(2)
        rd(nm)
        c.sendall(b"\x05\x00")
        ver, cmd, _, atyp = rd(4)
        if atyp == 3:
            host = rd(rd(1)[0]).decode()
        elif atyp == 1:
            host = socket.inet_ntoa(rd(4))
        else:
            host = socket.inet_ntop(socket.AF_INET6, rd(16))
        port = int.from_bytes(rd(2), "big")
        stub.seen.append((atyp, host, port))
        if stub.reply:
            c.sendall(bytes([5, stub.reply, 0, 1]) + b"\0" * 6)
            c.close()
            return
        up = socket.create_connection(stub.forward) if stub.forward else None
        c.sendall(b"\x05\x00\x00\x01" + b"\0" * 6)
        if up is None:
            c.close()
            return

        def pipe(a, b):
            try:
                while True:
                    d = a.recv(65536)
                    if not d:
                        break
                    b.sendall(d)
            except OSError:
                pass
            finally:
                for s in (a, b):
                    try:
                        s.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass
        threading.Thread(target=pipe, args=(c, up), daemon=True).start()
        pipe(up, c)
    except EOFError:
        pass


Stub._serve = lambda self, c: _serve_conn(self, c)


@pytest.fixture(autouse=True)
def _reset():
    socks5.set_proxy(None)
    yield
    socks5.set_proxy(None)


def test_设置与取消():
    assert socks5.get_proxy() is None
    socks5.set_proxy("127.0.0.1:11080")
    assert socks5.get_proxy() == "127.0.0.1:11080"
    socks5.set_proxy("")
    assert socks5.get_proxy() is None
    with pytest.raises(ValueError):
        socks5.set_proxy("nonsense")


def test_没设代理返回None():
    assert socks5.connect_via_proxy("example.invalid", 22) is None


def test_域名原样发给代理_不是IP():
    stub = Stub()
    try:
        socks5.set_proxy(stub.addr)
        s = socks5.connect_via_proxy("ubuntu1st", 22, 3)
        s.close()
    finally:
        stub.stop()
    assert stub.seen == [(3, "ubuntu1st", 22)]


def test_代理拒绝_报错():
    stub = Stub(reply=2)
    try:
        socks5.set_proxy(stub.addr)
        with pytest.raises(OSError, match="拒绝"):
            socks5.connect_via_proxy("ubuntu1st", 22, 3)
    finally:
        stub.stop()


def test_ssh经代理读写_收到的是域名(env):  # noqa: F811
    make, remote, srv, tmp = env
    stub = Stub(forward=("127.0.0.1", srv.port))
    try:
        socks5.set_proxy(stub.addr)
        st = SSHStorage("ubuntu1st", "/", port=22, key_filename=str(tmp / "id_rsa"),
                        trust_unknown=True, connect_timeout=5)
        assert st.available()
        st.close()
    finally:
        stub.stop()
    assert stub.seen and stub.seen[0] == (3, "ubuntu1st", 22)


def test_ssh代理拒绝_不回落直连(env):
    make, remote, srv, tmp = env
    stub = Stub(reply=5)
    try:
        socks5.set_proxy(stub.addr)
        # 目标端口就是真能直连的 SFTP 服务：若回落直连就会连上
        st = SSHStorage("127.0.0.1", "/", port=srv.port, key_filename=str(tmp / "id_rsa"),
                        trust_unknown=True, connect_timeout=5)
        assert st.available() is False
        with pytest.raises(OSError, match="拒绝"):
            st.listdir("")
    finally:
        stub.stop()
    assert stub.seen


def test_ssh代理连不上_不回落直连(env):
    make, remote, srv, tmp = env
    dead = socket.socket()
    dead.bind(("127.0.0.1", 0))
    port = dead.getsockname()[1]
    dead.close()
    socks5.set_proxy("127.0.0.1:%d" % port)
    st = SSHStorage("127.0.0.1", "/", port=srv.port, key_filename=str(tmp / "id_rsa"),
                    trust_unknown=True, connect_timeout=5)
    assert st.available() is False
    with pytest.raises(OSError, match="代理"):
        st.listdir("")
