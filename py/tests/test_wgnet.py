"""pybase.wgnet：桩 SOCKS5（TCP+UDP）+ 桩目标；域名不在本机解析、本机直连、strict 不回落、HTTP/HTTPS/UDP 往返、子进程继承。"""

import os
import socket
import ssl
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, REPO)
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from pybase import socks5, wgnet  # noqa: E402
from test_socks5 import Stub      # noqa: E402


@pytest.fixture(autouse=True)
def _reset(monkeypatch):
    monkeypatch.delenv(wgnet.config.ENV_PROXY, raising=False)
    monkeypatch.delenv(wgnet.config.ENV_STRICT, raising=False)
    wgnet.config._proxy = wgnet.config._strict = None
    yield
    wgnet.config._proxy = wgnet.config._strict = None


@pytest.fixture
def no_resolve(monkeypatch):
    calls = []
    real = socket.getaddrinfo

    def boom(host, *a, **k):
        if host not in ("127.0.0.1", "localhost"):      # 连代理口自己（IP 字面量）不算；其余域名不许在本机解析
            calls.append(host)
            raise AssertionError("本机不该解析域名")
        return real(host, *a, **k)
    monkeypatch.setattr(socket, "getaddrinfo", boom)
    return calls


class UdpStub:
    """SOCKS5 控制口（只应答 UDP ASSOCIATE）+ UDP 中继（回显，目的地址原样写进回包头）。"""

    def __init__(self):
        self.seen = []
        self.relay = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.relay.bind(("127.0.0.1", 0))
        self.ctrl = socket.socket()
        self.ctrl.bind(("127.0.0.1", 0))
        self.ctrl.listen(5)
        self.addr = "127.0.0.1:%d" % self.ctrl.getsockname()[1]
        threading.Thread(target=self._accept, daemon=True).start()
        threading.Thread(target=self._relay, daemon=True).start()

    def _accept(self):
        while True:
            try:
                c, _ = self.ctrl.accept()
            except OSError:
                return
            c.recv(3)
            c.sendall(b"\x05\x00")
            c.recv(10)
            c.sendall(b"\x05\x00\x00\x01\x00\x00\x00\x00" + self.relay.getsockname()[1].to_bytes(2, "big"))

    def _relay(self):
        while True:
            try:
                pkt, src = self.relay.recvfrom(65535)
            except OSError:
                return
            host, port, off = socks5.decode_addr(pkt, 3)
            self.seen.append((host, port))
            self.relay.sendto(pkt[:off] + b"echo:" + pkt[off:], src)

    def stop(self):
        self.ctrl.close()
        self.relay.close()


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self._r(b"hello " + self.path.encode())

    def do_POST(self):
        self._r(b"got " + self.rfile.read(int(self.headers["Content-Length"])))

    def _r(self, b):
        self.send_response(200)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def log_message(self, *a):
        pass


def _http_server(tls=None):
    srv = HTTPServer(("127.0.0.1", 0), H)
    if tls:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(*tls)
        srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def test_tcp_域名原样给代理_本机不解析(no_resolve):
    t = socket.socket(); t.bind(("127.0.0.1", 0)); t.listen(1)
    stub = Stub(forward=("127.0.0.1", t.getsockname()[1]))
    try:
        wgnet.configure(stub.addr)
        s = wgnet.tcp("ubuntu1st", 22, 3)
        c, _ = t.accept()
        s.sendall(b"hi")
        assert c.recv(2) == b"hi"
        s.close()
    finally:
        stub.stop()
    assert stub.seen == [(3, "ubuntu1st", 22)] and no_resolve == []


def test_本机目的不经代理():
    t = socket.socket(); t.bind(("127.0.0.1", 0)); t.listen(1)
    stub = Stub()
    try:
        wgnet.configure(stub.addr)
        for h in ("127.0.0.1", "localhost"):
            wgnet.tcp(h, t.getsockname()[1], 3).close()
    finally:
        stub.stop()
    assert stub.seen == []


def test_缺省非strict():
    assert not wgnet.is_strict()


def test_strict_没配置报错不直连():
    wgnet.configure(None, strict=True)
    with pytest.raises(OSError, match="没有配置"):
        wgnet.tcp("example.invalid", 80, 1)


def test_非strict_没配置才用系统直连():
    t = socket.socket(); t.bind(("127.0.0.1", 0)); t.listen(1)
    wgnet.configure(None, strict=False)
    wgnet.tcp("127.0.0.1", t.getsockname()[1], 1).close()
    # 非本机地址在非 strict 下走系统：用 127.0.0.2 代替（环回网段内但不算「本机」字面量之外的例子无意义，仅断言不抛 strict 错）
    wgnet.configure(None, strict=False)
    assert wgnet.config.route("example.invalid") is None


def test_代理连不上_报错不回落():
    t = socket.socket(); t.bind(("127.0.0.1", 0)); t.listen(1)
    dead = socket.socket(); dead.bind(("127.0.0.1", 0)); port = dead.getsockname()[1]; dead.close()
    wgnet.configure(f"127.0.0.1:{port}", strict=False)     # 即使 strict=False，配了代理就必须走代理
    with pytest.raises(OSError, match="连不上本机代理"):
        wgnet.tcp("ubuntu1st", t.getsockname()[1], 1)


def test_代理拒绝_报错():
    stub = Stub(reply=2)
    try:
        wgnet.configure(stub.addr)
        with pytest.raises(OSError, match="拒绝"):
            wgnet.tcp("ubuntu1st", 22, 3)
    finally:
        stub.stop()


def test_http_往返_域名给代理(no_resolve):
    srv = _http_server()
    stub = Stub(forward=("127.0.0.1", srv.server_port))
    try:
        wgnet.configure(stub.addr)
        r = wgnet.http.get("http://omlx.lan:8080/gw/status?x=1", timeout=3)
        assert (r.status, r.body) == (200, b"hello /gw/status?x=1")
        r = wgnet.http.post("http://omlx.lan:8080/p", b"abc", timeout=3)
        assert r.body == b"got abc"
        with wgnet.http.get("http://omlx.lan:8080/s", stream=True) as r:
            assert r.read(5) == b"hello"
    finally:
        stub.stop(); srv.shutdown()
    assert {x[1] for x in stub.seen} == {"omlx.lan"} and no_resolve == []


@pytest.fixture
def cert(tmp_path):
    crt, key = str(tmp_path / "c.pem"), str(tmp_path / "k.pem")
    r = subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", crt,
                        "-days", "2", "-subj", "/CN=secure.lan", "-addext", "subjectAltName=DNS:secure.lan"],
                       capture_output=True)
    if r.returncode:
        pytest.skip("没有 openssl")
    return crt, key


def test_https_往返_SNI用原主机名(cert):
    srv = _http_server(tls=cert)
    stub = Stub(forward=("127.0.0.1", srv.server_port))
    ctx = ssl.create_default_context(cafile=cert[0])        # 校验的是原主机名 secure.lan，不是 IP
    try:
        wgnet.configure(stub.addr)
        r = wgnet.http.get("https://secure.lan/x", context=ctx, timeout=3)
        assert (r.status, r.body) == (200, b"hello /x")
    finally:
        stub.stop(); srv.shutdown()
    assert stub.seen == [(3, "secure.lan", 443)]


def test_udp_往返_域名给代理(no_resolve):
    stub = UdpStub()
    try:
        wgnet.configure(stub.addr)
        u = wgnet.udp()
        u.settimeout(3)
        u.sendto(b"ping", ("dns.lan", 53))
        data, addr = u.recvfrom()
        u.close()
    finally:
        stub.stop()
    assert data == b"echo:ping" and addr == ("dns.lan", 53)
    assert stub.seen == [("dns.lan", 53)] and no_resolve == []


def test_udp_本机目的直发():
    stub = UdpStub()
    echo = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); echo.bind(("127.0.0.1", 0))
    try:
        wgnet.configure(stub.addr)
        u = wgnet.udp(); u.settimeout(3)
        u.sendto(b"x", ("127.0.0.1", echo.getsockname()[1]))
        pkt, src = echo.recvfrom(10)
        echo.sendto(b"back", src)
        assert u.recvfrom()[0] == b"back"
        u.close()
    finally:
        stub.stop()
    assert stub.seen == []


def test_udp_strict没配置报错():
    wgnet.configure(None, strict=True)
    with pytest.raises(OSError, match="没有配置"):
        wgnet.udp().sendto(b"x", ("dns.lan", 53))


def test_子进程经环境变量继承():
    stub = Stub()
    try:
        wgnet.configure(stub.addr)
        code = "from pybase import wgnet; wgnet.tcp('ubuntu1st', 22, 3).close(); print('ok')"
        out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, timeout=20,
                             env=dict(os.environ, PYTHONPATH=REPO))
        assert out.stdout.strip() == "ok", out.stderr
        wgnet.configure(None, strict=True)        # 父进程 strict 但没有代理口：子进程同样拒绝直连
        out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, timeout=20,
                             env=dict(os.environ, PYTHONPATH=REPO))
        assert "没有配置" in out.stderr
    finally:
        stub.stop()
    assert stub.seen == [(3, "ubuntu1st", 22)]


def _redirect_server():
    import http.server
    import threading as _t

    class H(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def _go(self):
            n = int(self.headers.get("Content-Length") or 0)
            if n:
                self.rfile.read(n)
            if self.path == "/a":
                self.send_response(302); self.send_header("Location", "/b"); self.send_header("Content-Length", "0"); self.end_headers()
            elif self.path == "/p":
                self.send_response(303); self.send_header("Location", "/b"); self.send_header("Content-Length", "0"); self.end_headers()
            elif self.path == "/loop":
                self.send_response(302); self.send_header("Location", "/loop"); self.send_header("Content-Length", "0"); self.end_headers()
            else:
                b = f"{self.command} {self.path}".encode()
                self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)

        do_GET = do_POST = _go

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
    _t.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def test_http_跟随重定向():
    srv = _redirect_server()
    base = f"http://127.0.0.1:{srv.server_address[1]}"
    try:
        assert wgnet.http.get(base + "/a").body == b"GET /b"
        assert wgnet.http.post(base + "/p", b"x").body == b"GET /b"      # 303 改 GET
        with pytest.raises(OSError, match="重定向超过"):
            wgnet.http.get(base + "/loop")
    finally:
        srv.shutdown()
