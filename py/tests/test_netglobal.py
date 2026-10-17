"""pybase.netglobal：桩 SOCKS5 + 桩目标，验证域名不在本机解析、本机直连、没代理不回落、paramiko/子进程同样经代理。"""

import http.server
import os
import socket
import subprocess
import sys
import threading
import urllib.request

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, REPO)
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from pybase import netglobal, socks5  # noqa: E402
pytest.importorskip("paramiko")
from test_socks5 import Stub  # noqa: E402
from test_sshstorage import env  # noqa: E402,F401
from pybase.sshstorage import SSHStorage  # noqa: E402


class _H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"hello")

    def log_message(self, *a):
        pass


@pytest.fixture
def web():
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield srv.server_address[1]
    srv.shutdown()
    srv.server_close()


@pytest.fixture
def resolved(monkeypatch):
    """装之前把 getaddrinfo 换成记录器：装好后它就是「本机解析」，记下谁被拿去本机解析。"""
    calls = []
    real = socket.getaddrinfo

    def rec(host, *a, **k):
        calls.append(host)
        return real(host, *a, **k)
    monkeypatch.setattr(socket, "getaddrinfo", rec)
    socks5.set_proxy(None)
    netglobal.install()
    yield calls
    netglobal.uninstall()
    socks5.set_proxy(None)


def _get(url):
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    return op.open(url, timeout=5).read()


def test_域名不在本机解析_代理收到域名(web, resolved):
    stub = Stub(forward=("127.0.0.1", web))
    try:
        socks5.set_proxy(stub.addr)
        assert _get("http://svc.example.invalid:%d/" % web) == b"hello"
    finally:
        stub.stop()
    assert stub.seen == [(3, "svc.example.invalid", web)]
    assert "svc.example.invalid" not in resolved


def test_IP目的收到IP(web, resolved):
    stub = Stub(forward=("127.0.0.1", web))
    try:
        socks5.set_proxy(stub.addr)
        s = socket.socket()
        s.settimeout(5)
        s.connect(("203.0.113.9", 4433))
        s.close()
    finally:
        stub.stop()
    assert stub.seen == [(1, "203.0.113.9", 4433)]


def test_本机不经代理(web, resolved):
    stub = Stub()
    try:
        socks5.set_proxy(stub.addr)
        assert _get("http://127.0.0.1:%d/" % web) == b"hello"
        assert _get("http://localhost:%d/" % web) == b"hello"
        s = socket.socket()
        assert s.connect_ex(("127.0.0.1", web)) == 0
        s.close()
    finally:
        stub.stop()
    assert stub.seen == []


def test_没有代理地址_报错不直连(web, resolved):
    with pytest.raises(OSError, match="拒绝直连"):
        socket.create_connection(("203.0.113.9", web), timeout=2)
    with pytest.raises(OSError, match="拒绝直连"):
        socket.create_connection(("svc.example.invalid", web), timeout=2)
    s = socket.socket()
    assert s.connect_ex(("203.0.113.9", web)) != 0
    s.close()


def test_代理连不上_报错不直连(web, resolved):
    dead = socket.socket()
    dead.bind(("127.0.0.1", 0))
    port = dead.getsockname()[1]
    dead.close()
    socks5.set_proxy("127.0.0.1:%d" % port)
    # 目的就是能直连的桩 web：若回落直连就会成功
    with pytest.raises(OSError, match="代理"):
        socket.create_connection(("svc.example.invalid", web), timeout=2)


def test_代理拒绝_报错(resolved):
    stub = Stub(reply=2)
    try:
        socks5.set_proxy(stub.addr)
        with pytest.raises(OSError, match="拒绝"):
            _get("http://svc.example.invalid/")
    finally:
        stub.stop()


def test_uninstall还原():
    orig = socket.socket.connect
    netglobal.install()
    assert socket.socket.connect is not orig
    netglobal.uninstall()
    assert socket.socket.connect is orig


def test_paramiko经代理_收到域名(env, resolved):  # noqa: F811
    make, remote, srv, tmp = env
    (remote / "a.txt").write_bytes(b"x")
    stub = Stub(forward=("127.0.0.1", srv.port))
    try:
        socks5.set_proxy(stub.addr)
        st = SSHStorage("ubuntu1st", "/", port=22, key_filename=str(tmp / "id_rsa"),
                        trust_unknown=True, connect_timeout=5)
        assert st.available()
        assert "a.txt" in [e.name if hasattr(e, "name") else e for e in st.listdir("")]
        st.close()
    finally:
        stub.stop()
    assert stub.seen and stub.seen[0] == (3, "ubuntu1st", 22)
    assert "ubuntu1st" not in resolved


def test_子进程经环境变量继承(web):
    stub = Stub(forward=("127.0.0.1", web))
    try:
        code = ("from pybase import netglobal; netglobal.install(); import urllib.request as u;"
                "o=u.build_opener(u.ProxyHandler({}));"
                "print(o.open('http://svc.example.invalid:%d/', timeout=5).read().decode())" % web)
        env = dict(os.environ, PYTHONPATH=REPO, **{socks5.ENV: stub.addr})
        out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, timeout=30, env=env)
        assert out.stdout.strip() == "hello", out.stderr
        env.pop(socks5.ENV)       # 子进程没有代理地址：报错
        out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, timeout=30, env=env)
        assert out.stdout.strip() == "" and "拒绝直连" in out.stderr
    finally:
        stub.stop()
    assert stub.seen == [(3, "svc.example.invalid", web)]
