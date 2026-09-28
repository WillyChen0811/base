"""pybase.net 测试：diag 的地址分类/连通性探测/路由解析，monitor 的采集器解析
（真实系统命令样例，脱敏后固化成夹具）、失败隔离、Recorder 的去重存储/变化
检测/查询/prune、Monitor.tick。
"""
from __future__ import annotations

import json
import socket
import subprocess
import tempfile
import threading
import time

import pytest

import pybase.net.diag as diag
from pybase.net.monitor import Monitor, Recorder, collectors


# ---------------------------------------------------------------------------
# diag: classify_address / looks_proxied
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "ip,expected",
    [
        ("127.0.0.1", "loopback"),
        ("::1", "loopback"),
        ("169.254.1.1", "link_local"),
        ("fe80::1", "link_local"),
        ("198.18.0.1", "fake_ip"),
        ("198.19.255.254", "fake_ip"),
        ("100.64.0.1", "cgnat"),
        ("100.127.255.254", "cgnat"),
        ("192.168.1.1", "private"),
        ("10.0.0.1", "private"),
        ("fc00::1", "private"),
        ("8.8.8.8", "global"),
        ("2409:8934:1ed3:17c9::1", "global"),
    ],
)
def test_classify_address(ip, expected):
    assert diag.classify_address(ip) == expected


def test_classify_address_invalid_returns_none():
    assert diag.classify_address("not-an-ip") is None


def test_looks_proxied_true_when_gateway_is_fake_ip():
    route = diag.RouteInfo(
        host="1.1.1.1", destination="1.0.0.0", gateway="198.18.0.1",
        interface="utun6", source=None, flags=None, raw="",
    )
    assert diag.looks_proxied(route) is True


def test_looks_proxied_true_when_source_is_fake_ip():
    route = diag.RouteInfo(
        host="1.1.1.1", destination="1.0.0.0", gateway="10.0.0.1",
        interface="eth0", source="198.18.5.5", flags=None, raw="",
    )
    assert diag.looks_proxied(route) is True


def test_looks_proxied_false_for_normal_route():
    route = diag.RouteInfo(
        host="8.8.8.8", destination="default", gateway="192.168.1.1",
        interface="en0", source=None, flags=None, raw="",
    )
    assert diag.looks_proxied(route) is False


def test_looks_proxied_false_for_none_route():
    assert diag.looks_proxied(None) is False


# ---------------------------------------------------------------------------
# diag: tcp_probe（真实本地 socket，不 mock）
# ---------------------------------------------------------------------------


def test_tcp_probe_ok_against_local_listener():
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.bind(("127.0.0.1", 0))
    server.listen(1)
    port = server.getsockname()[1]
    try:
        result = diag.tcp_probe("127.0.0.1", port, timeout=2)
        assert result.status == "ok"
        assert result.elapsed_ms >= 0
    finally:
        server.close()


def test_tcp_probe_refused_when_nothing_listening():
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.bind(("127.0.0.1", 0))
    port = server.getsockname()[1]
    server.close()  # 绑定后立刻关闭：这个端口大概率没人监听
    result = diag.tcp_probe("127.0.0.1", port, timeout=2)
    assert result.status == "refused"


def test_tcp_probe_unreachable_on_os_error(monkeypatch):
    """`unreachable` 覆盖 DNS 解析失败/EHOSTUNREACH 等所有其他 `OSError`；
    直接 mock `socket.create_connection`，不依赖本机真实网络环境（有的机器
    装了全局透明代理，任何域名都会先被 fake-ip 接住，"解析不到"反而连不出来）。
    """

    def raise_gaierror(*a, **kw):
        raise socket.gaierror("nodename nor servname provided")

    monkeypatch.setattr(diag.socket, "create_connection", raise_gaierror)
    result = diag.tcp_probe("host.invalid.does-not-exist", 80, timeout=2)
    assert result.status == "unreachable"


# ---------------------------------------------------------------------------
# diag: route_of（macOS / Linux 输出样例）
# ---------------------------------------------------------------------------

_MACOS_ROUTE_GET = """   route to: 1.1.1.1
destination: 1.0.0.0
       mask: 255.0.0.0
    gateway: 198.18.0.1
  interface: utun6
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      9000         0
"""

_LINUX_ROUTE_GET = "1.1.1.1 via 192.168.1.1 dev eth0 src 192.168.1.5 uid 1000\n    cache\n"

_LINUX_ROUTE_GET_LOCAL = "192.168.1.5 dev eth0 src 192.168.1.5 uid 1000\n"


def _fake_run(stdout, returncode=0):
    def run(cmd, *, timeout):
        return subprocess.CompletedProcess(cmd, returncode, stdout, "")

    return run


def test_route_of_macos(monkeypatch):
    monkeypatch.setattr(diag, "resolve_tool", lambda explicit, name: "/sbin/route")
    monkeypatch.setattr(diag, "run", _fake_run(_MACOS_ROUTE_GET))
    monkeypatch.setattr(diag.platform, "system", lambda: "Darwin")
    route = diag.route_of("1.1.1.1")
    assert route.destination == "1.0.0.0"
    assert route.gateway == "198.18.0.1"
    assert route.interface == "utun6"
    assert route.source is None


def test_route_of_linux_with_gateway(monkeypatch):
    monkeypatch.setattr(diag, "resolve_tool", lambda explicit, name: "/sbin/ip")
    monkeypatch.setattr(diag, "run", _fake_run(_LINUX_ROUTE_GET))
    monkeypatch.setattr(diag.platform, "system", lambda: "Linux")
    route = diag.route_of("1.1.1.1")
    assert route.gateway == "192.168.1.1"
    assert route.interface == "eth0"
    assert route.source == "192.168.1.5"


def test_route_of_linux_directly_connected_no_gateway(monkeypatch):
    monkeypatch.setattr(diag, "resolve_tool", lambda explicit, name: "/sbin/ip")
    monkeypatch.setattr(diag, "run", _fake_run(_LINUX_ROUTE_GET_LOCAL))
    monkeypatch.setattr(diag.platform, "system", lambda: "Linux")
    route = diag.route_of("192.168.1.5")
    assert route.gateway is None
    assert route.interface == "eth0"
    assert route.source == "192.168.1.5"


def test_route_of_invalid_host_returns_none():
    assert diag.route_of("not-an-ip") is None


def test_route_of_command_failure_returns_none(monkeypatch):
    monkeypatch.setattr(diag, "resolve_tool", lambda explicit, name: "/sbin/route")
    monkeypatch.setattr(diag, "run", _fake_run("no route", returncode=1))
    monkeypatch.setattr(diag.platform, "system", lambda: "Darwin")
    assert diag.route_of("1.1.1.1") is None


def test_env_proxies_reads_both_cases(monkeypatch):
    monkeypatch.delenv("http_proxy", raising=False)
    monkeypatch.delenv("HTTP_PROXY", raising=False)
    monkeypatch.setenv("HTTP_PROXY", "http://127.0.0.1:7893")
    result = diag.env_proxies()
    assert result["http_proxy"] == "http://127.0.0.1:7893"


# ---------------------------------------------------------------------------
# monitor.collectors：真实系统输出样例（脱敏），失败隔离
# ---------------------------------------------------------------------------

_IFCONFIG_SAMPLE = """lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384
\tinet 127.0.0.1 netmask 0xff000000
\tinet6 ::1 prefixlen 128
\tinet6 fe80::1%lo0 prefixlen 64 scopeid 0x1
en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
\tether aa:bb:cc:dd:ee:ff
\tinet6 fe80::1111:2222:3333:4444%en0 prefixlen 64 secured scopeid 0xb
\tinet 172.20.10.3 netmask 0xfffffff0 broadcast 172.20.10.15
\tmedia: autoselect
\tstatus: active
utun6: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 9000
\tinet 198.18.0.1 --> 198.18.0.1 netmask 0xffff0000
"""


def test_collect_interfaces_macos_parsing(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/sbin/ifconfig")
    monkeypatch.setattr(collectors, "run", _fake_run(_IFCONFIG_SAMPLE))
    monkeypatch.setattr(collectors.platform, "system", lambda: "Darwin")
    result = collectors.collect_interfaces()
    assert result["error"] is None
    by_name = {i["name"]: i for i in result["interfaces"]}
    assert by_name["lo0"]["mtu"] == 16384
    assert by_name["lo0"]["ipv4"][0]["address"] == "127.0.0.1"
    assert by_name["en0"]["mac"] == "aa:bb:cc:dd:ee:ff"
    assert by_name["en0"]["status"] == "active"
    assert by_name["en0"]["ipv4"][0]["broadcast"] == "172.20.10.15"
    assert by_name["utun6"]["ipv4"][0]["address"] == "198.18.0.1"


_IP_ADDR_JSON_SAMPLE = json.dumps(
    [
        {
            "ifindex": 2,
            "ifname": "eth0",
            "flags": ["BROADCAST", "MULTICAST", "UP", "LOWER_UP"],
            "mtu": 1500,
            "operstate": "UP",
            "address": "aa:bb:cc:dd:ee:ff",
            "addr_info": [
                {"family": "inet", "local": "192.168.1.5", "prefixlen": 24, "broadcast": "192.168.1.255"},
                {"family": "inet6", "local": "fe80::1", "prefixlen": 64, "scope": "link"},
            ],
        }
    ]
)


def test_collect_interfaces_linux_parsing(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/sbin/ip")
    monkeypatch.setattr(collectors, "run", _fake_run(_IP_ADDR_JSON_SAMPLE))
    monkeypatch.setattr(collectors.platform, "system", lambda: "Linux")
    result = collectors.collect_interfaces()
    assert result["error"] is None
    eth0 = result["interfaces"][0]
    assert eth0["name"] == "eth0"
    assert eth0["ipv4"][0]["address"] == "192.168.1.5"
    assert eth0["ipv6"][0]["address"] == "fe80::1"
    assert eth0["status"] == "UP"


_NETSTAT_INET_SAMPLE = """Routing tables

Internet:
Destination        Gateway            Flags               Netif Expire
default            172.20.10.1        UGScg                 en0
1                  198.18.0.1         UGSc                utun6
127.0.0.1          127.0.0.1          UH                    lo0
"""


def test_collect_routes_macos_parsing(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/usr/sbin/netstat")
    monkeypatch.setattr(collectors, "run", _fake_run(_NETSTAT_INET_SAMPLE))
    monkeypatch.setattr(collectors.platform, "system", lambda: "Darwin")
    result = collectors.collect_routes()
    assert result["error"] is None
    default = next(r for r in result["ipv4"] if r["destination"] == "default")
    assert default["gateway"] == "172.20.10.1"
    assert default["interface"] == "en0"


_IP_ROUTE_JSON_SAMPLE = json.dumps(
    [
        {"dst": "default", "gateway": "192.168.1.1", "dev": "eth0", "protocol": "dhcp", "metric": 100, "flags": []},
        {
            "dst": "192.168.1.0/24", "dev": "eth0", "protocol": "kernel", "scope": "link",
            "prefsrc": "192.168.1.5", "flags": [],
        },
    ]
)


def test_collect_routes_linux_parsing(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/sbin/ip")
    monkeypatch.setattr(collectors, "run", _fake_run(_IP_ROUTE_JSON_SAMPLE))
    monkeypatch.setattr(collectors.platform, "system", lambda: "Linux")
    result = collectors.collect_routes()
    assert result["error"] is None
    default = next(r for r in result["ipv4"] if r["destination"] == "default")
    assert default["gateway"] == "192.168.1.1"
    other = next(r for r in result["ipv4"] if r["destination"] == "192.168.1.0/24")
    assert other["source"] == "192.168.1.5"


_SCUTIL_DNS_SAMPLE = """DNS configuration

resolver #1
  nameserver[0] : 8.8.8.8
  flags    : Request A records, Request AAAA records
  reach    : 0x00000002 (Reachable)

resolver #2
  domain   : local
  options  : mdns
  order    : 300000
  reach    : 0x00000000 (Not Reachable)

DNS configuration (for scoped queries)

resolver #1
  nameserver[0] : 8.8.8.8
  if_index : 11 (en0)
  flags    : Scoped, Request A records, Request AAAA records
  reach    : 0x00000002 (Reachable)
"""


def test_collect_dns_parsing(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(collectors, "run", _fake_run(_SCUTIL_DNS_SAMPLE))
    result = collectors.collect_dns()
    assert result["error"] is None
    resolvers = result["resolvers"]
    assert resolvers[0]["nameservers"] == ["8.8.8.8"]
    assert resolvers[0]["scoped"] is False
    scoped = next(r for r in resolvers if r["scoped"])
    assert scoped["if_index"] == 11
    assert scoped["if_name"] == "en0"


_SCUTIL_PROXY_SAMPLE = """<dictionary> {
  ExceptionsList : <array> {
    0 : 192.168.0.0/16
    1 : 10.0.0.0/8
  }
  HTTPEnable : 1
  HTTPPort : 7893
  HTTPProxy : 127.0.0.1
}
"""


def test_collect_proxies_parsing(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(collectors, "run", _fake_run(_SCUTIL_PROXY_SAMPLE))
    monkeypatch.delenv("http_proxy", raising=False)
    monkeypatch.delenv("HTTP_PROXY", raising=False)
    result = collectors.collect_proxies()
    assert result["error"] is None
    assert result["system"]["HTTPPort"] == "7893"
    assert result["system"]["ExceptionsList"] == ["192.168.0.0/16", "10.0.0.0/8"]


_SCUTIL_NC_SAMPLE = (
    '* (Disconnected)   CF2436EE-C240-45AF-80C0-5CCA22CD2088 VPN (com.wireguard.macos) '
    '"my-wg-tunnel"             [VPN:com.wireguard.macos]\n'
    '  (Connected)      A21543F4-FF84-45F4-919F-6398A962AAED PPP --> Fake Modem "Fake Modem"'
    '            [PPP:Modem]\n'
)


def test_collect_vpn_services_parsing(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(collectors, "run", _fake_run(_SCUTIL_NC_SAMPLE))
    result = collectors.collect_vpn_services()
    assert result["error"] is None
    services = result["services"]
    wg = next(s for s in services if s["name"] == "my-wg-tunnel")
    assert wg["enabled"] is True
    assert wg["status"] == "Disconnected"
    assert wg["subtype"] == "VPN:com.wireguard.macos"
    modem = next(s for s in services if s["name"] == "Fake Modem")
    assert modem["enabled"] is False
    assert modem["status"] == "Connected"


_NETBIRD_JSON_SAMPLE = json.dumps(
    {
        "peers": {"total": 2, "connected": 1, "details": None},
        "cliVersion": "0.76.3",
        "daemonStatus": "Connected",
        "netbirdIp": "100.90.1.2/16",
        "management": {"connected": True},
    }
)


def test_collect_netbird_parsing(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/usr/local/bin/netbird")
    monkeypatch.setattr(collectors, "run", _fake_run(_NETBIRD_JSON_SAMPLE))
    result = collectors.collect_netbird()
    assert result["error"] is None
    assert result["available"] is True
    assert result["peers_total"] == 2
    assert result["daemon_status"] == "Connected"


def test_collect_netbird_unavailable_when_cli_missing(monkeypatch):
    from pybase.media._proc import ToolNotFoundError

    def raise_not_found(explicit, name):
        raise ToolNotFoundError("not found")

    monkeypatch.setattr(collectors, "resolve_tool", raise_not_found)
    result = collectors.collect_netbird()
    assert result["error"] is None
    assert result["available"] is False


# ---------------------------------------------------------------------------
# 采集器失败隔离：工具报错/命令失败都不抛异常
# ---------------------------------------------------------------------------


def test_collect_dns_tool_missing_returns_error_not_raise(monkeypatch):
    from pybase.media._proc import ToolNotFoundError

    def raise_not_found(explicit, name):
        raise ToolNotFoundError("scutil not found")

    monkeypatch.setattr(collectors, "resolve_tool", raise_not_found)
    result = collectors.collect_dns()
    assert result["error"] is not None
    assert "resolvers" not in result


def test_collect_interfaces_command_failure_returns_error(monkeypatch):
    monkeypatch.setattr(collectors, "resolve_tool", lambda explicit, name: "/sbin/ifconfig")
    monkeypatch.setattr(collectors, "run", _fake_run("boom", returncode=1))
    monkeypatch.setattr(collectors.platform, "system", lambda: "Darwin")
    result = collectors.collect_interfaces()
    assert result["error"] is not None


def test_collect_targets_isolates_per_target_failure():
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.bind(("127.0.0.1", 0))
    server.listen(1)
    port = server.getsockname()[1]
    try:
        targets = [
            {"name": "ok", "host": "127.0.0.1", "port": port},
            {"name": "bad", "host": object(), "port": 1},  # 类型不对，触发内部异常，不该拖垮整批
        ]
        result = collectors.collect_targets(targets, route_timeout=1, probe_timeout=1)
        assert result["error"] is None
        by_name = {t["name"]: t for t in result["targets"]}
        assert by_name["ok"]["error"] is None
        assert by_name["ok"]["probe"]["status"] == "ok"
        assert by_name["bad"]["error"] is not None
    finally:
        server.close()


def test_collect_mihomo_against_fake_server():
    import http.server

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            routes = {
                "/version": {"version": "1.0.0"},
                "/configs": {"mode": "rule", "tun": {"enable": True}},
                "/connections": {
                    "connections": [
                        {"upload": 10, "download": 20, "chains": ["DIRECT"], "rule": "MATCH"},
                    ]
                },
            }
            body = json.dumps(routes.get(self.path, {})).encode()
            self.send_response(200 if self.path in routes else 404)
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a):
            pass

    server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        result = collectors.collect_mihomo(f"http://127.0.0.1:{port}", timeout=2)
        assert result["error"] is None
        assert result["mode"] == "rule"
        assert result["tun_enabled"] is True
        assert result["connections_count"] == 1
        assert result["total_upload"] == 10
    finally:
        server.shutdown()


def test_collect_mihomo_connection_refused_returns_error():
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.bind(("127.0.0.1", 0))
    port = server.getsockname()[1]
    server.close()
    result = collectors.collect_mihomo(f"http://127.0.0.1:{port}", timeout=1)
    assert result["error"] is not None


# ---------------------------------------------------------------------------
# Recorder：去重存储、变化检测、查询、prune
# ---------------------------------------------------------------------------


@pytest.fixture
def recorder():
    rec = Recorder(":memory:")
    yield rec
    rec.close()


def test_recorder_first_tick_has_no_events(recorder):
    events = recorder.record({"interfaces": {"error": None, "interfaces": []}}, at=1000.0)
    assert events == []
    assert recorder.latest()["interfaces"]["interfaces"] == []


def test_recorder_dedup_unchanged_payload_stores_ref():
    rec = Recorder(":memory:")
    payload = {"error": None, "interfaces": [{"name": "en0", "status": "active", "ipv4": [], "ipv6": []}]}
    rec.record({"interfaces": payload}, at=1.0)
    rec.record({"interfaces": payload}, at=2.0)
    rows = rec._con.execute(
        "SELECT stored FROM snapshot_items WHERE collector = 'interfaces' ORDER BY snapshot_id"
    ).fetchall()
    assert [r[0] for r in rows] == ["full", "ref"]
    rec.close()


def test_recorder_interface_up_down_and_address_change_events(recorder):
    before = {
        "error": None,
        "interfaces": [{"name": "en0", "status": "inactive", "ipv4": [], "ipv6": []}],
    }
    after = {
        "error": None,
        "interfaces": [
            {"name": "en0", "status": "active", "ipv4": [{"address": "192.168.1.5"}], "ipv6": []}
        ],
    }
    recorder.record({"interfaces": before}, at=1.0)
    events = recorder.record({"interfaces": after}, at=2.0)
    kinds = {e["kind"] for e in events}
    assert "interface_up" in kinds
    assert "interface_address_changed" in kinds


def test_recorder_default_route_changed_event(recorder):
    before = {"error": None, "ipv4": [{"destination": "default", "gateway": "1.1.1.1", "interface": "en0"}], "ipv6": []}
    after = {"error": None, "ipv4": [{"destination": "default", "gateway": "2.2.2.2", "interface": "en1"}], "ipv6": []}
    recorder.record({"routes": before}, at=1.0)
    events = recorder.record({"routes": after}, at=2.0)
    assert any(e["kind"] == "default_route_changed" and e["subject"] == "ipv4" for e in events)


def test_recorder_dns_resolvers_changed_event(recorder):
    before = {"error": None, "resolvers": [{"scoped": False, "nameservers": ["8.8.8.8"]}]}
    after = {"error": None, "resolvers": [{"scoped": False, "nameservers": ["1.1.1.1"]}]}
    recorder.record({"dns": before}, at=1.0)
    events = recorder.record({"dns": after}, at=2.0)
    assert any(e["kind"] == "dns_resolvers_changed" for e in events)


def test_recorder_system_proxy_changed_event(recorder):
    before = {"error": None, "system": {"HTTPEnable": "0"}}
    after = {"error": None, "system": {"HTTPEnable": "1", "HTTPPort": "7893"}}
    recorder.record({"proxies": before}, at=1.0)
    events = recorder.record({"proxies": after}, at=2.0)
    assert any(e["kind"] == "system_proxy_changed" for e in events)


def test_recorder_vpn_service_status_changed_event(recorder):
    before = {"error": None, "services": [{"uuid": "u1", "name": "wg", "status": "Disconnected"}]}
    after = {"error": None, "services": [{"uuid": "u1", "name": "wg", "status": "Connected"}]}
    recorder.record({"vpn_services": before}, at=1.0)
    events = recorder.record({"vpn_services": after}, at=2.0)
    assert any(
        e["kind"] == "vpn_service_status_changed" and e["subject"] == "wg" for e in events
    )


def test_recorder_netbird_status_changed_event(recorder):
    before = {"error": None, "available": True, "daemon_status": "Connecting", "connected": False}
    after = {"error": None, "available": True, "daemon_status": "Connected", "connected": True}
    recorder.record({"netbird": before}, at=1.0)
    events = recorder.record({"netbird": after}, at=2.0)
    kinds = {e["kind"] for e in events}
    assert kinds == {"netbird_status_changed"}
    subjects = {e["subject"] for e in events}
    assert subjects == {"daemon_status", "management_connected"}


def test_recorder_target_events(recorder):
    before = {
        "error": None,
        "targets": [
            {
                "name": "wg-a", "error": None, "proxied": False,
                "route": {"interface": "en0"}, "probe": {"status": "ok"},
            }
        ],
    }
    after = {
        "error": None,
        "targets": [
            {
                "name": "wg-a", "error": None, "proxied": True,
                "route": {"interface": "utun6"}, "probe": {"status": "timeout"},
            }
        ],
    }
    recorder.record({"targets": before}, at=1.0)
    events = recorder.record({"targets": after}, at=2.0)
    kinds = {e["kind"] for e in events}
    assert kinds == {
        "target_interface_changed", "target_proxied_changed", "target_connectivity_changed",
    }


def test_recorder_error_snapshot_suppresses_diff(recorder):
    before = {"error": None, "interfaces": [{"name": "en0", "status": "active", "ipv4": [], "ipv6": []}]}
    broken = {"error": "ifconfig timed out"}
    recorder.record({"interfaces": before}, at=1.0)
    events = recorder.record({"interfaces": broken}, at=2.0)
    assert events == []


def test_recorder_probes_query_backfills_from_last_full(recorder):
    payload = {
        "error": None,
        "targets": [
            {"name": "cloudflare", "error": None, "proxied": True,
             "route": {"interface": "utun6"}, "probe": {"status": "ok", "elapsed_ms": 5.0}}
        ],
    }
    recorder.record({"targets": payload}, at=100.0)
    recorder.record({"targets": payload}, at=101.0)  # 完全相同，走 ref
    series = recorder.probes("cloudflare", 0, 1000)
    assert len(series) == 2
    assert all(p["status"] == "ok" for p in series)
    assert [p["at"] for p in series] == [100.0, 101.0]


def test_recorder_events_query_filters_by_kind_and_time(recorder):
    recorder.record({"interfaces": {"error": None, "interfaces": [{"name": "en0", "status": "inactive", "ipv4": [], "ipv6": []}]}}, at=1.0)
    recorder.record({"interfaces": {"error": None, "interfaces": [{"name": "en0", "status": "active", "ipv4": [], "ipv6": []}]}}, at=2.0)
    recorder.record({"interfaces": {"error": None, "interfaces": [{"name": "en0", "status": "inactive", "ipv4": [], "ipv6": []}]}}, at=3.0)
    all_events = recorder.events(0, 1000)
    assert len(all_events) == 2
    up_only = recorder.events(0, 1000, kinds=["interface_up"])
    assert len(up_only) == 1
    assert up_only[0]["kind"] == "interface_up"
    windowed = recorder.events(2.5, 1000)
    assert len(windowed) == 1


def test_recorder_prune_removes_old_snapshots_but_keeps_latest(recorder):
    recorder.record({"interfaces": {"error": None, "interfaces": []}}, at=1.0)
    recorder.record({"interfaces": {"error": None, "interfaces": [{"name": "en0", "status": "active", "ipv4": [], "ipv6": []}]}}, at=2.0)
    recorder.prune(1.5)
    remaining = recorder._con.execute("SELECT COUNT(*) FROM snapshots").fetchone()[0]
    assert remaining == 1
    assert recorder.latest()["interfaces"]["interfaces"][0]["name"] == "en0"


def test_recorder_persists_across_reopen(tmp_path):
    path = str(tmp_path / "monitor.sqlite")
    rec1 = Recorder(path)
    rec1.record({"interfaces": {"error": None, "interfaces": []}}, at=1.0)
    rec1.close()
    rec2 = Recorder(path)
    assert rec2.latest()["interfaces"]["interfaces"] == []
    # 重开后去重基准仍然生效：同样的 payload 应该只存引用
    rec2.record({"interfaces": {"error": None, "interfaces": []}}, at=2.0)
    rows = rec2._con.execute(
        "SELECT stored FROM snapshot_items WHERE collector='interfaces' ORDER BY snapshot_id"
    ).fetchall()
    assert rows[-1][0] == "ref"
    rec2.close()


# ---------------------------------------------------------------------------
# Monitor.tick
# ---------------------------------------------------------------------------


def test_monitor_tick_collects_and_records_and_returns_events():
    calls = {"n": 0}

    def flaky_collector():
        calls["n"] += 1
        status = "active" if calls["n"] > 1 else "inactive"
        return {"error": None, "interfaces": [{"name": "en0", "status": status, "ipv4": [], "ipv6": []}]}

    rec = Recorder(":memory:")
    monitor = Monitor({"interfaces": flaky_collector}, rec, interval=999)
    events1 = monitor.tick(at=1.0)
    assert events1 == []
    events2 = monitor.tick(at=2.0)
    assert any(e["kind"] == "interface_up" for e in events2)
    rec.close()


def test_monitor_run_stops_on_event():
    rec = Recorder(":memory:")
    ticks = {"n": 0}

    def counting_collector():
        ticks["n"] += 1
        return {"error": None, "interfaces": []}

    monitor = Monitor({"interfaces": counting_collector}, rec, interval=0.01)
    stop_event = threading.Event()

    def stopper():
        time.sleep(0.05)
        stop_event.set()

    t = threading.Thread(target=stopper)
    t.start()
    monitor.run(stop_event)
    t.join()
    assert ticks["n"] >= 1
    rec.close()
