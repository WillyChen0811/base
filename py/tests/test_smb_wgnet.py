"""SMB 库已整库拷进 pybase._vendor 且外连换成 wgnet：配了代理时各入口（smbclient 会话与断线重连 /
pysmb 列共享 / NetBIOS UDP / 端口探测 / HTTP 取文件）对桩 SOCKS5 报的都是真实目的地址；没配时照旧系统直连。"""

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
from pybase._vendor import smbclient               # noqa: E402
from pybase._vendor.smbprotocol.transport import Tcp   # noqa: E402
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


def test_断线后smbclient自己重连也经代理(refusing_proxy):
    """缓存里是条已断开的连接：register_session 会自己 new Connection 重连，这条路径也必须走 wgnet。"""
    class Dead:
        class transport:
            connected = False
    cache = {"10.1.2.3:445": Dead()}
    with pytest.raises(ValueError, match="Failed to connect"):
        smbclient.register_session("10.1.2.3", username="u", password="p", port=445,
                                   connection_cache=cache, connection_timeout=3)
    assert _hosts(refusing_proxy) == [("10.1.2.3", 445)]


def test_Tcp直接建连经代理(refusing_proxy):
    t = Tcp("nas.corp.example", 445, 3)
    with pytest.raises(ValueError, match="Failed to connect"):
        t.connect()
    assert _hosts(refusing_proxy) == [("nas.corp.example", 445)]


def test_NetBIOS的UDP经wgnet():
    from pybase._vendor.nmb.NetBIOS import NetBIOS
    srv = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    srv.bind(("127.0.0.1", 0))
    srv.settimeout(3)
    nb = NetBIOS(broadcast=False)
    try:
        assert nb.queryIPForName("127.0.0.1", port=srv.getsockname()[1], timeout=0.3) is None
        data, _ = srv.recvfrom(1024)
        assert len(data) > 12
    finally:
        nb.close()
        srv.close()


def test_导入pybase_smb不加载site_packages里的原版():
    import subprocess
    code = ("import sys, pybase.smb, pybase.smb.client, pybase.smbstorage;"
            "bad=[m for m in sys.modules if m.split('.')[0] in ('smbprotocol','smbclient','smb','nmb')];"
            "print(bad); sys.exit(1 if bad else 0)")
    r = subprocess.run([sys.executable, "-c", code], cwd=REPO, capture_output=True, text=True)
    assert r.returncode == 0, r.stdout + r.stderr


def test_vendor拷贝里没有外连用的socket():
    import re
    root = os.path.join(REPO, "pybase", "_vendor")
    pat = re.compile(r"socket\.create_connection|socket\.socket\(")
    bad = []
    for d, _, fs in os.walk(root):
        for f in fs:
            if f.endswith(".py"):
                for i, line in enumerate(open(os.path.join(d, f), encoding="utf-8"), 1):
                    if pat.search(line) and not line.lstrip().startswith("#"):
                        bad.append(f"{f}:{i}")
    assert not bad, bad


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
    t = Tcp("127.0.0.1", port, 3)
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
