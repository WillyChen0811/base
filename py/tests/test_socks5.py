"""pybase.socks5（SOCKS5 客户端）：桩 SOCKS5 服务断言收到的是域名、拒绝/连不上不回落直连。"""

import os
import socket
import sys
import threading

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, REPO)

from pybase import socks5  # noqa: E402



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


def test_子进程经环境变量继承代理():
    import subprocess
    stub = Stub()
    try:
        socks5.set_proxy(stub.addr)
        assert os.environ[socks5.ENV] == stub.addr
        code = "from pybase import socks5; s = socks5.connect_via_proxy('ubuntu1st', 22, 3); print(s is not None)"
        env = dict(os.environ, PYTHONPATH=os.path.join(os.path.dirname(__file__), ".."))
        out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, timeout=20, env=env)
        assert out.stdout.strip() == "True", out.stderr
    finally:
        socks5.set_proxy(None)
        stub.stop()
    assert stub.seen == [(3, "ubuntu1st", 22)]
    assert socks5.ENV not in os.environ
