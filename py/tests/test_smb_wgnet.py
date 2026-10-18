"""SMB 传输层经 wgnet.tcp：配了代理时三个入口（smbprotocol 会话 / pysmb 列共享 / 端口探测 / HTTP 取文件）
对桩 SOCKS5 报的都是真实目的地址；没配时照旧系统直连。"""

import os
import socket
import sys
import threading

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, REPO)
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from pybase import smbstorage, wgnet          # noqa: E402
from pybase.smb import http_fetch             # noqa: E402
from pybase.smb.client import SMBClient, list_shares   # noqa: E402
from pybase.smb.transport import WgConnection, WgTcp, ensure_connection   # noqa: E402
from test_socks5 import Stub                  # noqa: E402


@pytest.fixture(autouse=True)
def _reset(monkeypatch):
    monkeypatch.delenv(wgnet.config.ENV_PROXY, raising=False)
    monkeypatch.delenv(wgnet.config.ENV_STRICT, raising=False)
    wgnet.config._proxy = wgnet.config._strict = None
    yield
    wgnet.config._proxy = wgnet.config._strict = None


@pytest.fixture
def refusing_proxy():
    """代理一律拒绝连接：只为看它收到什么目的地址；若有任何路径绕过代理走系统直连，10.x 会超时而非被拒。"""
    s = Stub(reply=2)
    wgnet.configure(s.addr, strict=True)
    yield s
    s.stop()


def _hosts(stub):
    return [(h, p) for _, h, p in stub.seen]


def test_smb会话经代理_收到真实IP(refusing_proxy):
    c = SMBClient("10.116.7.250", "share", "u", "p", port=445)
    with pytest.raises(Exception):
        c.connect()
    assert _hosts(refusing_proxy)[0] == ("10.116.7.250", 445)


def test_smb会话经代理_域名原样(refusing_proxy):
    with pytest.raises(Exception):
        SMBClient("nas.corp.example", "share", port=1445).connect()
    assert _hosts(refusing_proxy)[0] == ("nas.corp.example", 1445)


def test_缓存里的连接是WgConnection(refusing_proxy):
    cache = {}
    with pytest.raises(ValueError, match="Failed to connect"):
        ensure_connection(cache, "10.1.2.3", 445, 3)
    assert not cache and _hosts(refusing_proxy) == [("10.1.2.3", 445)]
    conn = WgConnection(b"0" * 16, "10.1.2.3", 445)
    assert isinstance(conn.transport, type(None))
    from smbprotocol.transport import Tcp
    conn.transport = Tcp("10.1.2.3", 445, 3)          # 库内部这样赋值
    assert isinstance(conn.transport, WgTcp)


def test_pysmb列共享经代理(refusing_proxy):
    with pytest.raises(Exception):
        list_shares("10.116.7.250", "u", "p", port=445)
    assert _hosts(refusing_proxy)[0] == ("10.116.7.250", 445)


def test_端口探测经代理(refusing_proxy):
    assert smbstorage._tcp_probe("10.116.7.250", 445, 3) is False
    assert _hosts(refusing_proxy) == [("10.116.7.250", 445)]


def test_http取文件经代理(refusing_proxy):
    with pytest.raises(Exception):
        http_fetch.fetch_webdav({"address": "nas.corp.example", "share": "s", "webdav_port": 5006}, "a.docx")
    assert _hosts(refusing_proxy)[0] == ("nas.corp.example", 5006)


def test_没配代理照旧系统直连():
    srv = socket.socket()
    srv.bind(("127.0.0.1", 0))
    srv.listen(1)
    port = srv.getsockname()[1]
    t = WgTcp("127.0.0.1", port, 3)
    t.connect()
    assert t.connected
    t.close()
    assert smbstorage._tcp_probe("127.0.0.1", port, 3) is True
    srv.close()
    assert smbstorage._tcp_probe("127.0.0.1", port, 1) is False


def test_非strict无代理的远端也走系统直连(monkeypatch):
    calls = []
    monkeypatch.setattr(socket, "create_connection", lambda addr, timeout=None: calls.append(addr) or (_ for _ in ()).throw(OSError("x")))
    assert smbstorage._tcp_probe("10.116.7.250", 445, 1) is False
    assert calls == [("10.116.7.250", 445)]
