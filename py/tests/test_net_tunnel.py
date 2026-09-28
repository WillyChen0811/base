"""pybase.net 隧道层测试：wireguard.py（conf 解析/生成、密钥、UAPI）、engine.py
（wireguard-go 进程管理，用假二进制模拟）、routes.py/tunnel.py（用假 runner/假引擎
断言命令序列与回滚，不碰真实网络设备——本机没有 root，不能建 utun/改路由）。
"""
from __future__ import annotations

import base64
import os
import shutil
import socket
import stat
import subprocess
import tempfile
import threading
import time
from pathlib import Path

import pytest

from pybase.net import routes
from pybase.net.engine import BinaryNotFoundError, EngineStartError, WireGuardGo
from pybase.net.tunnel import Tunnel, TunnelError
from pybase.net.wireguard import (
    DeviceStatus,
    Interface,
    Peer,
    UapiError,
    WireGuardConfig,
    WireGuardConfigError,
    connect_uapi,
    dump_conf,
    generate_preshared_key,
    parse_conf,
    uapi_get,
    uapi_set,
)

try:
    import cryptography  # noqa: F401
    HAS_CRYPTOGRAPHY = True
except ImportError:
    HAS_CRYPTOGRAPHY = False

requires_cryptography = pytest.mark.skipif(not HAS_CRYPTOGRAPHY, reason="本机没装 cryptography")


@pytest.fixture
def sock_dir():
    """unix socket 路径有长度限制（macOS ~104 字节），pytest 的 tmp_path 经常超；
    这里另开一个短路径的临时目录，专给 socket 用。
    """
    d = tempfile.mkdtemp(prefix="wgt-")
    try:
        yield Path(d)
    finally:
        shutil.rmtree(d, ignore_errors=True)


# ---------- conf 解析/生成 ----------

# 仿 wg-peers 的 wg-quick 导出格式（见 ../wg-peers/wgpeer.py 的 TMPL）
WG_PEERS_STYLE_CONF = """[Interface]
PrivateKey = wA1r4G+nD9y7uH8bO3xVvQe0mJk5tYcXqNzP2sL6h1E=
Address = 10.10.0.100/32
DNS = 10.10.0.1

[Peer]
PublicKey = 3P8xK1mQeR9tYvC7bN2dJ5wL0oZ4hU6sA8fG1iX9pT0=
PresharedKey = f4Z9rT2mK8xQ1nB6cV3sL7pE0yU5wA2dJ9oR4hG8iX0=
Endpoint = router.example.com:51820
AllowedIPs = 10.10.0.0/24, 192.168.50.0/24
PersistentKeepalive = 25
"""


def test_parse_conf_wg_peers_style_roundtrip():
    cfg = parse_conf(WG_PEERS_STYLE_CONF)
    assert cfg.interface.private_key == "wA1r4G+nD9y7uH8bO3xVvQe0mJk5tYcXqNzP2sL6h1E="
    assert cfg.interface.addresses == ("10.10.0.100/32",)
    assert cfg.interface.dns == ("10.10.0.1",)
    assert len(cfg.peers) == 1
    peer = cfg.peers[0]
    assert peer.endpoint == "router.example.com:51820"
    assert peer.allowed_ips == ("10.10.0.0/24", "192.168.50.0/24")
    assert peer.persistent_keepalive == 25
    assert peer.preshared_key == "f4Z9rT2mK8xQ1nB6cV3sL7pE0yU5wA2dJ9oR4hG8iX0="

    dumped = dump_conf(cfg)
    assert parse_conf(dumped) == cfg  # 语义往返稳定，不要求字节相同


def test_parse_conf_ipv6_multi_peer_and_extras():
    text = """
    ; 整行注释
    [Interface]
    PrivateKey = wA1r4G+nD9y7uH8bO3xVvQe0mJk5tYcXqNzP2sL6h1E=
    Address = 10.10.0.2/32, fd00::2/128
    ListenPort = 51821
    MTU = 1380
    PostUp = echo hello
    Table = off

    [Peer]
    PublicKey = 3P8xK1mQeR9tYvC7bN2dJ5wL0oZ4hU6sA8fG1iX9pT0=
    Endpoint = [2001:db8::1]:51820
    AllowedIPs = fd00::/64

    [Peer]
    PublicKey = 9nQ2rF6xM1oL8vD5zA0kJ7hY4tW3sE0bC9uG2iX6pR1=
    Endpoint = 203.0.113.5:51820
    AllowedIPs = 0.0.0.0/0, ::/0
    """
    cfg = parse_conf(text)
    assert cfg.interface.addresses == ("10.10.0.2/32", "fd00::2/128")
    assert cfg.interface.listen_port == 51821
    assert cfg.interface.mtu == 1380
    # wg-quick 专有键：保留但不解释
    assert cfg.interface.extras == {"PostUp": ("echo hello",), "Table": ("off",)}
    assert len(cfg.peers) == 2
    assert cfg.peers[0].endpoint == "[2001:db8::1]:51820"
    assert cfg.peers[1].allowed_ips == ("0.0.0.0/0", "::/0")

    dumped = dump_conf(cfg)
    assert parse_conf(dumped) == cfg
    assert "PostUp = echo hello" in dumped
    assert "Table = off" in dumped


def test_parse_conf_private_key_not_base64():
    with pytest.raises(WireGuardConfigError, match="base64"):
        parse_conf("[Interface]\nPrivateKey = not-base64!!\n")


def test_parse_conf_private_key_wrong_length():
    short_key = base64.b64encode(b"short").decode()
    with pytest.raises(WireGuardConfigError, match="32 字节"):
        parse_conf(f"[Interface]\nPrivateKey = {short_key}\n")


def test_parse_conf_bad_address():
    priv = base64.b64encode(os.urandom(32)).decode()
    with pytest.raises(WireGuardConfigError, match="Address"):
        parse_conf(f"[Interface]\nPrivateKey = {priv}\nAddress = not-an-address\n")


def test_parse_conf_missing_private_key():
    with pytest.raises(WireGuardConfigError, match="PrivateKey"):
        parse_conf("[Interface]\nAddress = 10.0.0.1/32\n")


def test_parse_conf_missing_public_key():
    priv = base64.b64encode(os.urandom(32)).decode()
    with pytest.raises(WireGuardConfigError, match="PublicKey"):
        parse_conf(f"[Interface]\nPrivateKey = {priv}\n\n[Peer]\nAllowedIPs = 10.0.0.0/24\n")


def test_parse_conf_bad_endpoint():
    priv = base64.b64encode(os.urandom(32)).decode()
    pub = base64.b64encode(os.urandom(32)).decode()
    with pytest.raises(WireGuardConfigError, match="Endpoint"):
        parse_conf(
            f"[Interface]\nPrivateKey = {priv}\n\n[Peer]\nPublicKey = {pub}\nEndpoint = no-port-here\n"
        )


def test_parse_conf_bad_allowed_ips():
    priv = base64.b64encode(os.urandom(32)).decode()
    pub = base64.b64encode(os.urandom(32)).decode()
    with pytest.raises(WireGuardConfigError, match="AllowedIPs"):
        parse_conf(
            f"[Interface]\nPrivateKey = {priv}\n\n[Peer]\nPublicKey = {pub}\nAllowedIPs = not-a-network\n"
        )


def test_parse_conf_unknown_section():
    with pytest.raises(WireGuardConfigError, match="未知节"):
        parse_conf("[Bogus]\nFoo = bar\n")


def test_generate_preshared_key_is_valid_key_material():
    psk = generate_preshared_key()
    raw = base64.b64decode(psk)
    assert len(raw) == 32


# ---------- 密钥生成（需要 cryptography） ----------

@requires_cryptography
def test_generate_private_key_and_public_key_of():
    from pybase.net.wireguard import generate_private_key, public_key_of

    priv = generate_private_key()
    pub1 = public_key_of(priv)
    pub2 = public_key_of(priv)
    assert pub1 == pub2  # 确定性推导
    assert base64.b64decode(priv).__len__() == 32
    assert base64.b64decode(pub1).__len__() == 32
    assert pub1 != priv


# ---------- UAPI ----------

class _FakeUapiServer:
    """本地 unix socket 假 wireguard-go UAPI 服务端：记录收到的请求文本，
    对 `set=` 回 errno=0，对 `get=` 回预设的响应文本。
    """

    def __init__(self, path: str, get_response: bytes = b"errno=0\n\n"):
        self.path = path
        self.get_response = get_response
        self.requests: list[str] = []
        self._srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._srv.bind(path)
        self._srv.listen(1)
        self._srv.settimeout(0.2)
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()

    def _serve(self) -> None:
        while not self._stop.is_set():
            try:
                conn, _ = self._srv.accept()
            except socket.timeout:
                continue
            with conn:
                buf = b""
                while b"\n\n" not in buf:
                    chunk = conn.recv(4096)
                    if not chunk:
                        break
                    buf += chunk
                text = buf.decode()
                self.requests.append(text)
                if text.startswith("set="):
                    conn.sendall(b"errno=0\n\n")
                elif text.startswith("get="):
                    conn.sendall(self.get_response)

    def close(self) -> None:
        self._stop.set()
        self._thread.join(timeout=2)
        self._srv.close()
        try:
            os.unlink(self.path)
        except OSError:
            pass


@pytest.fixture
def fake_uapi(sock_dir: Path):
    server = _FakeUapiServer(str(sock_dir / "uapi.sock"))
    yield server
    server.close()


def _sample_cfg() -> WireGuardConfig:
    priv = base64.b64encode(os.urandom(32)).decode()
    pub = base64.b64encode(os.urandom(32)).decode()
    psk = base64.b64encode(os.urandom(32)).decode()
    return WireGuardConfig(
        interface=Interface(private_key=priv, listen_port=51820),
        peers=(
            Peer(
                public_key=pub,
                allowed_ips=("10.10.0.0/24", "0.0.0.0/0"),
                endpoint="203.0.113.5:51820",
                preshared_key=psk,
                persistent_keepalive=25,
            ),
        ),
    )


def test_uapi_set_wire_format(fake_uapi):
    cfg = _sample_cfg()
    sock = connect_uapi(fake_uapi.path)
    try:
        uapi_set(sock, cfg)
    finally:
        sock.close()
    time.sleep(0.05)
    assert len(fake_uapi.requests) == 1
    text = fake_uapi.requests[0]
    assert text.startswith("set=1\n")
    assert f"private_key={base64.b64decode(cfg.interface.private_key).hex()}" in text
    assert "listen_port=51820" in text
    assert "replace_peers=true" in text
    assert f"public_key={base64.b64decode(cfg.peers[0].public_key).hex()}" in text
    assert f"preshared_key={base64.b64decode(cfg.peers[0].preshared_key).hex()}" in text
    assert "endpoint=203.0.113.5:51820" in text
    assert "persistent_keepalive_interval=25" in text
    assert "replace_allowed_ips=true" in text
    assert "allowed_ip=10.10.0.0/24" in text
    assert "allowed_ip=0.0.0.0/0" in text


def test_uapi_set_raises_on_nonzero_errno(sock_dir):
    class BadServer(_FakeUapiServer):
        def _serve(self):
            while not self._stop.is_set():
                try:
                    conn, _ = self._srv.accept()
                except socket.timeout:
                    continue
                with conn:
                    buf = b""
                    while b"\n\n" not in buf:
                        chunk = conn.recv(4096)
                        if not chunk:
                            break
                        buf += chunk
                    conn.sendall(b"errno=22\n\n")

    server = BadServer(str(sock_dir / "bad.sock"))
    try:
        sock = connect_uapi(server.path)
        try:
            with pytest.raises(UapiError, match="errno=22"):
                uapi_set(sock, _sample_cfg())
        finally:
            sock.close()
    finally:
        server.close()


def test_uapi_get_parses_device_status(sock_dir):
    pub_a = os.urandom(32)
    pub_b = os.urandom(32)
    response = (
        "private_key=" + os.urandom(32).hex() + "\n"
        "listen_port=51820\n"
        "fwmark=0\n"
        f"public_key={pub_a.hex()}\n"
        "endpoint=203.0.113.5:51820\n"
        "last_handshake_time_sec=1700000000\n"
        "last_handshake_time_nsec=123\n"
        "tx_bytes=100\n"
        "rx_bytes=200\n"
        "persistent_keepalive_interval=25\n"
        "allowed_ip=10.10.0.0/24\n"
        "allowed_ip=0.0.0.0/0\n"
        f"public_key={pub_b.hex()}\n"
        "last_handshake_time_sec=0\n"
        "last_handshake_time_nsec=0\n"
        "tx_bytes=0\n"
        "rx_bytes=0\n"
        "persistent_keepalive_interval=0\n"
        "allowed_ip=fd00::/64\n"
        "errno=0\n\n"
    ).encode()
    server = _FakeUapiServer(str(sock_dir / "get.sock"), get_response=response)
    try:
        sock = connect_uapi(server.path)
        try:
            status = uapi_get(sock)
        finally:
            sock.close()
    finally:
        server.close()

    assert isinstance(status, DeviceStatus)
    assert status.listen_port == 51820
    assert len(status.peers) == 2
    peer_a, peer_b = status.peers
    assert peer_a.public_key == base64.b64encode(pub_a).decode()
    assert peer_a.endpoint == "203.0.113.5:51820"
    assert peer_a.last_handshake_sec == 1700000000
    assert peer_a.rx_bytes == 200 and peer_a.tx_bytes == 100
    assert peer_a.persistent_keepalive == 25
    assert peer_a.allowed_ips == ("10.10.0.0/24", "0.0.0.0/0")
    # 从未握手：sec=0 要变成 None，不是字面 0
    assert peer_b.last_handshake_sec is None
    assert peer_b.persistent_keepalive is None


# ---------- engine.py：用假 wireguard-go 二进制 ----------

_FAKE_WG_GO = """#!/usr/bin/env python3
import os, socket, signal, sys

def main():
    ifname_hint = sys.argv[2] if len(sys.argv) > 2 else "utun"
    name = ifname_hint + "9"
    name_file = os.environ.get("WG_TUN_NAME_FILE")
    if name_file:
        with open(name_file, "w") as f:
            f.write(name + "\\n")
    uapi_dir = os.environ.get("WG_UAPI_DIR") or "/var/run/wireguard"
    os.makedirs(uapi_dir, exist_ok=True)
    sock_path = os.path.join(uapi_dir, name + ".sock")
    try:
        os.unlink(sock_path)
    except OSError:
        pass
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(sock_path)
    srv.listen(1)
    srv.settimeout(0.2)

    stopping = {"v": False}

    def handle(signum, frame):
        stopping["v"] = True

    signal.signal(signal.SIGTERM, handle)
    while not stopping["v"]:
        try:
            conn, _ = srv.accept()
            conn.close()
        except socket.timeout:
            continue
    try:
        os.unlink(sock_path)
    except OSError:
        pass

if __name__ == "__main__":
    main()
"""

_FAKE_WG_GO_DIES_IMMEDIATELY = """#!/usr/bin/env python3
import sys
sys.exit(1)
"""


def _write_fake_binary(tmp_path: Path, content: str) -> Path:
    path = tmp_path / "fake-wireguard-go"
    path.write_text(content)
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return path


def test_engine_start_stop_lifecycle(tmp_path, sock_dir):
    binary = _write_fake_binary(tmp_path, _FAKE_WG_GO)
    uapi_dir = sock_dir / "uapi"
    engine = WireGuardGo(binary=str(binary), uapi_dir=str(uapi_dir), start_timeout=5, stop_timeout=2)

    assert not engine.alive
    ifname = engine.start(ifname_hint="utun")
    assert ifname == "utun9"
    assert engine.ifname == "utun9"
    assert engine.alive
    assert engine.sock_path == str(uapi_dir / "utun9.sock")
    assert os.path.exists(engine.sock_path)

    engine.stop()
    assert not engine.alive
    assert not os.path.exists(uapi_dir / "utun9.sock")

    engine.stop()  # 幂等，不报错
    assert not engine.alive


def test_engine_binary_not_found():
    engine = WireGuardGo(binary="/definitely/not/a/real/binary-xyz")
    with pytest.raises(BinaryNotFoundError, match="brew install"):
        engine.start()


def test_engine_start_error_when_process_dies(tmp_path, sock_dir):
    binary = _write_fake_binary(tmp_path, _FAKE_WG_GO_DIES_IMMEDIATELY)
    engine = WireGuardGo(binary=str(binary), uapi_dir=str(sock_dir / "uapi"), start_timeout=2)
    with pytest.raises(EngineStartError):
        engine.start()
    assert not engine.alive


# ---------- routes.py：假 runner 断言命令序列 ----------

class _FakeRunner:
    def __init__(self, responses: dict | None = None):
        self.calls: list[list[str]] = []
        self._responses = responses or {}

    def __call__(self, cmd):
        cmd = list(cmd)
        self.calls.append(cmd)
        key = tuple(cmd)
        for pattern, (rc, stdout) in self._responses.items():
            if tuple(cmd[: len(pattern)]) == pattern:
                return subprocess.CompletedProcess(cmd, rc, stdout=stdout, stderr="" if rc == 0 else "boom")
        return subprocess.CompletedProcess(cmd, 0, stdout="", stderr="")


def test_set_addresses_darwin():
    runner = _FakeRunner()
    routes.set_addresses(
        "utun9", ["10.10.0.2/32", "fd00::2/64"], mtu=1380, runner=runner, platform="darwin"
    )
    assert runner.calls == [
        ["ifconfig", "utun9", "inet", "10.10.0.2", "10.10.0.2", "alias"],
        ["ifconfig", "utun9", "inet6", "fd00::2", "prefixlen", "64"],
        ["ifconfig", "utun9", "mtu", "1380"],
    ]


def test_set_addresses_linux():
    runner = _FakeRunner()
    routes.set_addresses("wg0", ["10.10.0.2/32"], mtu=1380, runner=runner, platform="linux")
    assert runner.calls == [
        ["ip", "addr", "add", "10.10.0.2/32", "dev", "wg0"],
        ["ip", "link", "set", "dev", "wg0", "mtu", "1380"],
    ]


def test_add_remove_route_darwin_and_linux():
    runner = _FakeRunner()
    routes.add_route("10.10.0.0/24", "utun9", runner=runner, platform="darwin")
    routes.remove_route("10.10.0.0/24", "utun9", runner=runner, platform="darwin")
    routes.add_route("::/0", "utun9", runner=runner, platform="linux")
    assert runner.calls == [
        ["route", "-q", "-n", "add", "-inet", "10.10.0.0/24", "-interface", "utun9"],
        ["route", "-q", "-n", "delete", "-inet", "10.10.0.0/24", "-interface", "utun9"],
        ["ip", "route", "add", "::/0", "dev", "utun9"],
    ]


def test_route_error_on_nonzero_returncode():
    runner = _FakeRunner({("ifconfig",): (1, "")})
    with pytest.raises(routes.RouteError):
        routes.bring_up("utun9", runner=runner, platform="darwin")


def test_default_gateway_darwin_and_linux():
    darwin_runner = _FakeRunner({("route", "-n", "get"): (0, "   gateway: 192.168.1.1\n")})
    assert routes.default_gateway(runner=darwin_runner, platform="darwin") == "192.168.1.1"

    linux_runner = _FakeRunner({("ip", "route", "show", "default"): (0, "default via 10.0.0.1 dev eth0\n")})
    assert routes.default_gateway(runner=linux_runner, platform="linux") == "10.0.0.1"

    missing_runner = _FakeRunner({("ip", "route", "show", "default"): (1, "")})
    assert routes.default_gateway(runner=missing_runner, platform="linux") is None


def test_needs_endpoint_protection():
    assert routes.needs_endpoint_protection("203.0.113.5", ["0.0.0.0/0"])
    assert not routes.needs_endpoint_protection("203.0.113.5", ["10.10.0.0/24"])
    assert routes.needs_endpoint_protection("2001:db8::1", ["::/0"])


def test_endpoint_host_route_add_remove():
    runner = _FakeRunner()
    routes.add_endpoint_host_route("203.0.113.5", "192.168.1.1", runner=runner, platform="darwin")
    routes.remove_endpoint_host_route("203.0.113.5", runner=runner, platform="darwin")
    assert runner.calls == [
        ["route", "-q", "-n", "add", "-inet", "203.0.113.5/32", "192.168.1.1"],
        ["route", "-q", "-n", "delete", "-inet", "203.0.113.5/32"],
    ]


# ---------- tunnel.py：假引擎 + 假 runner ----------

class _FakeEngine:
    """duck-typed 假引擎：不真正起进程，只在 start() 里拉起一个假 UAPI 服务器。"""

    def __init__(self, tmp_path: Path, get_response: bytes = b"errno=0\n\n"):
        self._tmp_path = tmp_path
        self._get_response = get_response
        self._alive = False
        self._ifname = None
        self._server = None
        self.stop_calls = 0

    @property
    def alive(self):
        return self._alive

    @property
    def ifname(self):
        return self._ifname

    @property
    def sock_path(self):
        return str(self._tmp_path / "fake.sock") if self._ifname else None

    def start(self, *, ifname_hint="utun"):
        self._ifname = "utun9"
        self._server = _FakeUapiServer(self.sock_path, get_response=self._get_response)
        self._alive = True
        return self._ifname

    def stop(self):
        self.stop_calls += 1
        if self._server is not None:
            self._server.close()
            self._server = None
        self._alive = False


def _tunnel_cfg() -> WireGuardConfig:
    priv = base64.b64encode(os.urandom(32)).decode()
    pub = base64.b64encode(os.urandom(32)).decode()
    return WireGuardConfig(
        interface=Interface(private_key=priv, addresses=("10.10.0.2/32",)),
        peers=(
            Peer(
                public_key=pub,
                allowed_ips=("10.10.0.0/24", "0.0.0.0/0"),
                endpoint="203.0.113.5:51820",
                persistent_keepalive=25,
            ),
        ),
    )


def test_tunnel_up_down_command_sequence_and_endpoint_protection(sock_dir):
    engine = _FakeEngine(sock_dir)
    runner = _FakeRunner({("ip", "route", "show", "default"): (0, "default via 10.0.0.1 dev eth0\n")})
    tun = Tunnel(_tunnel_cfg(), runner=runner, engine=engine, platform="linux")

    tun.up()
    assert engine.alive
    assert runner.calls == [
        ["ip", "addr", "add", "10.10.0.2/32", "dev", "utun9"],
        ["ip", "link", "set", "dev", "utun9", "up"],
        ["ip", "route", "add", "10.10.0.0/24", "dev", "utun9"],
        ["ip", "route", "add", "0.0.0.0/0", "dev", "utun9"],
        ["ip", "route", "show", "default"],
        ["ip", "route", "add", "203.0.113.5/32", "via", "10.0.0.1"],
    ]

    status = tun.status()
    assert status.ifname == "utun9"
    assert status.engine_alive is True
    assert status.routes == ("10.10.0.0/24", "0.0.0.0/0")
    assert status.endpoint_routes == ("203.0.113.5",)
    assert status.dns_applied is False

    tun.up()  # 幂等：不重复执行
    assert len(runner.calls) == 6

    runner.calls.clear()
    tun.down()
    assert runner.calls == [
        ["ip", "route", "del", "203.0.113.5/32"],
        ["ip", "route", "del", "0.0.0.0/0", "dev", "utun9"],
        ["ip", "route", "del", "10.10.0.0/24", "dev", "utun9"],
    ]
    assert engine.stop_calls == 1
    assert not engine.alive

    runner.calls.clear()
    tun.down()  # 幂等：引擎已经不在跑，不再重复清理
    assert runner.calls == []
    assert engine.stop_calls == 1


def test_tunnel_up_rolls_back_on_route_failure(sock_dir):
    engine = _FakeEngine(sock_dir)

    def failing_runner(cmd):
        cmd = list(cmd)
        if cmd[:3] == ["ip", "route", "add"] and cmd[3] == "0.0.0.0/0":
            return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="boom")
        return subprocess.CompletedProcess(cmd, 0, stdout="", stderr="")

    calls = []

    def recording_runner(cmd):
        calls.append(list(cmd))
        return failing_runner(cmd)

    tun = Tunnel(_tunnel_cfg(), runner=recording_runner, engine=engine, platform="linux")
    with pytest.raises(TunnelError, match="阶段：routes"):
        tun.up()

    # 第一条路由（10.10.0.0/24）成功了，第二条（0.0.0.0/0）失败——应该把第一条删掉，
    # 并且停掉引擎；不应该把已加的路由算作"生效"
    assert ["ip", "route", "del", "10.10.0.0/24", "dev", "utun9"] in calls
    assert engine.stop_calls == 1
    assert not engine.alive

    status = tun.status()
    assert status.routes == ()
    assert status.endpoint_routes == ()
