"""pybase.net 测试：diag 的地址分类/连通性探测/路由解析，monitor 的采集器解析
（真实系统命令样例，脱敏后固化成夹具）、失败隔离、Recorder 的去重存储/变化
检测/查询/prune、Monitor.tick。
"""
from __future__ import annotations

import json
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time

import pytest

import pybase.net.diag as diag
from pybase.net.monitor import Monitor, Recorder, Scenario, collectors, scenario_envelope, summarize_scenario


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


# ---------------------------------------------------------------------------
# collect_interface_owners / via_of：接口归属判定
# ---------------------------------------------------------------------------

_OWNERS_IFACES = {
    "error": None,
    "interfaces": [
        {"name": "utun6", "status": "active", "ipv4": [{"address": "198.18.0.1"}], "ipv6": []},
        {"name": "utun1", "status": "active", "ipv4": [{"address": "192.168.65.8"}], "ipv6": []},
        {"name": "utun2", "status": "active", "ipv4": [{"address": "100.90.1.2"}], "ipv6": []},
        {"name": "utun3", "status": "active", "ipv4": [], "ipv6": [{"address": "fe80::1"}]},
        {"name": "en0", "status": "active", "ipv4": [{"address": "172.20.10.3"}], "ipv6": []},
    ],
}
_OWNERS_MIHOMO = {"error": None, "mode": "rule", "tun_enabled": True, "tun_device": "utun6"}
_OWNERS_VPN = {
    "error": None,
    "services": [
        {"uuid": "u1", "name": "MacBookAirChenwyM3", "enabled": True, "status": "Connected",
         "subtype": "VPN:com.wireguard.macos"},
        {"uuid": "u2", "name": "Deguo", "enabled": True, "status": "Disconnected",
         "subtype": "VPN:com.wireguard.macos"},
    ],
}
_OWNERS_NETBIRD = {
    "error": None, "available": True, "connected": True,
    "daemon_status": "Connected", "netbird_ip": "100.90.1.2/16",
}


def test_collect_interface_owners_mihomo_tun_by_device_name():
    result = collectors.collect_interface_owners(
        _OWNERS_IFACES, _OWNERS_VPN, _OWNERS_NETBIRD, _OWNERS_MIHOMO,
        vpn_service_addresses={"MacBookAirChenwyM3": ["192.168.65.8"]},
    )
    assert result["error"] is None
    assert result["owners"]["utun6"] == {
        "owner_kind": "mihomo_tun", "owner_name": "mihomo",
        "evidence": "mihomo /configs tun.device=utun6",
    }


def test_collect_interface_owners_wireguard_by_known_address():
    result = collectors.collect_interface_owners(
        _OWNERS_IFACES, _OWNERS_VPN, _OWNERS_NETBIRD, _OWNERS_MIHOMO,
        vpn_service_addresses={"MacBookAirChenwyM3": ["192.168.65.8"]},
    )
    owner = result["owners"]["utun1"]
    assert owner["owner_kind"] == "wireguard"
    assert owner["owner_name"] == "MacBookAirChenwyM3"


def test_collect_interface_owners_wireguard_not_matched_when_service_disconnected():
    # Deguo 是 Disconnected，即使给了地址也不该被匹配上
    result = collectors.collect_interface_owners(
        _OWNERS_IFACES, _OWNERS_VPN, _OWNERS_NETBIRD, _OWNERS_MIHOMO,
        vpn_service_addresses={"Deguo": ["100.90.1.2"]},
    )
    # utun2 命中的是 netbird_ip，不是 Deguo（netbird 判断在前）
    assert result["owners"]["utun2"]["owner_kind"] == "netbird"


def test_collect_interface_owners_netbird_by_ip():
    result = collectors.collect_interface_owners(
        _OWNERS_IFACES, _OWNERS_VPN, _OWNERS_NETBIRD, _OWNERS_MIHOMO,
    )
    assert result["owners"]["utun2"] == {
        "owner_kind": "netbird", "owner_name": "netbird",
        "evidence": "接口地址命中 netbird status 的 netbirdIp=100.90.1.2",
    }


def test_collect_interface_owners_self_tunnel_mapping():
    result = collectors.collect_interface_owners(
        _OWNERS_IFACES, _OWNERS_VPN, _OWNERS_NETBIRD, _OWNERS_MIHOMO,
        self_tunnels={"utun3": "test-peer"},
    )
    assert result["owners"]["utun3"] == {
        "owner_kind": "self_tunnel", "owner_name": "test-peer",
        "evidence": "调用方给出的自建隧道接口映射（tunnel_state.json）",
    }


def test_collect_interface_owners_unknown_when_no_evidence():
    result = collectors.collect_interface_owners(_OWNERS_IFACES, _OWNERS_VPN, _OWNERS_NETBIRD, _OWNERS_MIHOMO)
    owner = result["owners"]["utun1"]  # 没给 vpn_service_addresses，utun1 判不出
    assert owner["owner_kind"] == "unknown"
    assert owner["owner_name"] is None
    # 物理接口 en0 不在虚拟接口正则里，根本不登记
    assert "en0" not in result["owners"]


def test_collect_interface_owners_interfaces_error_fails():
    result = collectors.collect_interface_owners({"error": "boom"})
    assert result["error"] is not None
    assert "owners" not in result


def test_via_of_mapping():
    owners = {
        "utun6": {"owner_kind": "mihomo_tun", "owner_name": "mihomo"},
        "utun1": {"owner_kind": "wireguard", "owner_name": "MacBookAirChenwyM3"},
        "utun2": {"owner_kind": "netbird", "owner_name": "netbird"},
        "utun3": {"owner_kind": "self_tunnel", "owner_name": "test-peer"},
        "utun4": {"owner_kind": "unknown", "owner_name": None},
    }
    assert collectors.via_of("utun6", owners) == "clash_tun"
    assert collectors.via_of("utun1", owners) == "wireguard:MacBookAirChenwyM3"
    assert collectors.via_of("utun2", owners) == "netbird"
    assert collectors.via_of("utun3", owners) == "self_tunnel:test-peer"
    assert collectors.via_of("utun4", owners) == "unknown"
    assert collectors.via_of("en0", owners) == "direct:en0"
    assert collectors.via_of(None, owners) == "unknown"


def test_collect_targets_fills_via_from_owners(monkeypatch):
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.bind(("127.0.0.1", 0))
    server.listen(1)
    port = server.getsockname()[1]
    try:
        fake_route = diag.RouteInfo(
            host="127.0.0.1", destination="127.0.0.1", gateway=None,
            interface="utun1", source=None, flags=None, raw="",
        )
        monkeypatch.setattr(collectors, "route_of", lambda host, timeout: fake_route)
        owners = {"utun1": {"owner_kind": "wireguard", "owner_name": "MacBookAirChenwyM3"}}
        result = collectors.collect_targets(
            [{"name": "wg-a", "host": "127.0.0.1", "port": port}],
            owners=owners, route_timeout=1, probe_timeout=1,
        )
        assert result["targets"][0]["via"] == "wireguard:MacBookAirChenwyM3"
    finally:
        server.close()


def test_collect_targets_via_unknown_without_owners():
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.bind(("127.0.0.1", 0))
    server.listen(1)
    port = server.getsockname()[1]
    try:
        result = collectors.collect_targets(
            [{"name": "x", "host": "127.0.0.1", "port": port}], route_timeout=1, probe_timeout=1,
        )
        # 没传 owners：查得到路由就是 direct:<iface>，查不到就是 unknown，两种都可接受
        assert result["targets"][0]["via"] in (None, "unknown") or result["targets"][0]["via"].startswith("direct:")
    finally:
        server.close()


# ---------------------------------------------------------------------------
# summarize_scenario：多种组合下的 key/label
# ---------------------------------------------------------------------------


def _scenario_snapshot(*, wg=(), netbird_available=True, netbird_connected=False, netbird_status=None,
                        clash_mode=None, clash_tun=False, system_proxy=False,
                        default_iface=None, owners=None):
    services = [
        {"uuid": f"u{i}", "name": name, "enabled": True, "status": "Connected"}
        for i, name in enumerate(wg)
    ]
    return {
        "vpn_services": {"error": None, "services": services},
        "netbird": {
            "error": None, "available": netbird_available,
            "connected": netbird_connected, "daemon_status": netbird_status,
        },
        "mihomo": {"error": None, "mode": clash_mode, "tun_enabled": clash_tun, "listen_ports": [7893]},
        "proxies": {"error": None, "system": (
            {"HTTPEnable": "1", "HTTPProxy": "127.0.0.1", "HTTPPort": "7893"} if system_proxy
            else {"FTPPassive": "1"})},
        "routes": {
            "error": None,
            "ipv4": [{"destination": "default", "interface": default_iface}] if default_iface else [],
            "ipv6": [],
        },
        "owners": {"error": None, "owners": owners or {}},
    }


def test_summarize_scenario_only_wg_a():
    snap = _scenario_snapshot(wg=["WG-A"], default_iface="utun1",
                               owners={"utun1": {"owner_kind": "wireguard", "owner_name": "WG-A"}})
    sc = summarize_scenario(snap)
    assert isinstance(sc, Scenario)
    assert sc.wireguard == ("WG-A",)
    assert sc.default_via == "wireguard:WG-A"
    assert "WG-A" in sc.label


def test_summarize_scenario_wg_a_and_b_same_key_as_itself():
    snap1 = _scenario_snapshot(wg=["WG-A", "WG-B"], default_iface="utun1",
                                owners={"utun1": {"owner_kind": "wireguard", "owner_name": "WG-A"}})
    snap2 = _scenario_snapshot(wg=["WG-B", "WG-A"], default_iface="utun1",
                                owners={"utun1": {"owner_kind": "wireguard", "owner_name": "WG-A"}})
    sc1, sc2 = summarize_scenario(snap1), summarize_scenario(snap2)
    assert sc1.key == sc2.key  # 顺序无关，排序后应一致
    assert sc1.wireguard == ("WG-A", "WG-B")


def test_summarize_scenario_two_wg_plus_clash_tun_and_system_proxy():
    snap = _scenario_snapshot(
        wg=["WG-A", "WG-B"], clash_mode="rule", clash_tun=True, system_proxy=True,
        default_iface="utun6", owners={"utun6": {"owner_kind": "mihomo_tun", "owner_name": "mihomo"}},
    )
    sc = summarize_scenario(snap)
    assert sc.clash_mode == "rule"
    assert sc.clash_tun is True
    assert sc.system_proxy is True
    assert sc.default_via == "clash_tun"
    assert "Clash" in sc.label and "WireGuard" in sc.label


def test_summarize_scenario_only_netbird():
    snap = _scenario_snapshot(netbird_connected=True, netbird_status="Connected",
                               default_iface="utun2",
                               owners={"utun2": {"owner_kind": "netbird", "owner_name": "netbird"}})
    sc = summarize_scenario(snap)
    assert sc.netbird_connected is True
    assert sc.wireguard == ()
    assert sc.default_via == "netbird"
    assert "NetBird" in sc.label


def test_summarize_scenario_all_off_direct():
    snap = _scenario_snapshot(default_iface="en0", netbird_available=False)
    sc = summarize_scenario(snap)
    assert sc.wireguard == ()
    assert sc.netbird_connected is False
    assert sc.clash_mode is None
    assert sc.default_via == "direct:en0"
    assert "直连" in sc.label


def test_summarize_scenario_different_combos_have_different_keys():
    only_wg = summarize_scenario(_scenario_snapshot(wg=["WG-A"], default_iface="utun1"))
    only_nb = summarize_scenario(_scenario_snapshot(netbird_connected=True, netbird_status="Connected"))
    all_off = summarize_scenario(_scenario_snapshot())
    assert len({only_wg.key, only_nb.key, all_off.key}) == 3


def test_scenario_envelope_wraps_as_collector_style_dict():
    sc = summarize_scenario(_scenario_snapshot())
    env = scenario_envelope(sc)
    assert env["error"] is None
    assert env["key"] == sc.key
    assert env["label"] == sc.label


# ---------------------------------------------------------------------------
# Recorder：scenario 事件/分段聚合、手动备注、source 维度
# ---------------------------------------------------------------------------


def test_recorder_scenario_changed_event(recorder):
    before = scenario_envelope(summarize_scenario(_scenario_snapshot()))
    after = scenario_envelope(summarize_scenario(_scenario_snapshot(wg=["WG-A"], default_iface="utun1")))
    recorder.record({"scenario": before}, at=1.0)
    events = recorder.record({"scenario": after}, at=2.0)
    assert any(e["kind"] == "scenario_changed" and e["subject"] == "scenario" for e in events)


def test_recorder_scenarios_segments(recorder):
    s_off = scenario_envelope(summarize_scenario(_scenario_snapshot()))
    s_wg = scenario_envelope(summarize_scenario(_scenario_snapshot(wg=["WG-A"], default_iface="utun1")))
    recorder.record({"scenario": s_off}, at=0.0)
    recorder.record({"scenario": s_off}, at=5.0)  # 相同，走 ref，不产生新分段
    recorder.record({"scenario": s_wg}, at=10.0)
    segments = recorder.scenarios(0.0, 20.0)
    assert len(segments) == 2
    assert segments[0]["key"] == s_off["key"]
    assert segments[0]["start"] == 0.0
    assert segments[0]["end"] == 10.0
    assert segments[1]["key"] == s_wg["key"]
    assert segments[1]["end"] == 20.0


def test_recorder_probes_by_scenario_aggregates_success_rate_and_via(recorder):
    s_off = scenario_envelope(summarize_scenario(_scenario_snapshot()))
    s_wg = scenario_envelope(summarize_scenario(_scenario_snapshot(wg=["WG-A"], default_iface="utun1")))

    def target_payload(via, status, ms):
        return {
            "error": None,
            "targets": [{"name": "t1", "error": None, "proxied": False, "via": via,
                         "route": {"interface": None}, "probe": {"status": status, "elapsed_ms": ms}}],
        }

    recorder.record({"scenario": s_off, "targets": target_payload("direct:en0", "ok", 10.0)}, at=0.0)
    recorder.record({"scenario": s_off, "targets": target_payload("direct:en0", "timeout", None)}, at=1.0)
    recorder.record({"scenario": s_wg, "targets": target_payload("wireguard:WG-A", "ok", 50.0)}, at=10.0)
    recorder.record({"scenario": s_wg, "targets": target_payload("wireguard:WG-A", "ok", 60.0)}, at=11.0)

    agg = recorder.probes_by_scenario("t1", 0.0, 20.0)
    assert len(agg) == 2
    off_seg = next(a for a in agg if a["key"] == s_off["key"])
    assert off_seg["total"] == 2
    assert off_seg["success_rate"] == 0.5
    assert off_seg["via"] == {"direct:en0": 2}
    wg_seg = next(a for a in agg if a["key"] == s_wg["key"])
    assert wg_seg["success_rate"] == 1.0
    assert wg_seg["latency_median_ms"] == 55.0
    assert wg_seg["via"] == {"wireguard:WG-A": 2}


def test_recorder_annotate_and_query(recorder):
    recorder.annotate(1.0, "在公司，开了 WG-B")
    recorder.annotate(2.0, "回家了")
    notes = recorder.annotations(0.0, 10.0)
    assert [n["text"] for n in notes] == ["在公司，开了 WG-B", "回家了"]
    assert notes[0]["source"] == "local"
    windowed = recorder.annotations(1.5, 10.0)
    assert [n["text"] for n in windowed] == ["回家了"]


def test_recorder_record_and_events_carry_source(recorder):
    recorder.record({"interfaces": {"error": None, "interfaces": []}}, at=1.0, source="router-a")
    events_local = recorder.events(0, 10, source="local")
    events_router = recorder.events(0, 10, source="router-a")
    assert events_local == []
    row = recorder._con.execute("SELECT source FROM snapshots").fetchone()
    assert row[0] == "router-a"


# ---------------------------------------------------------------------------
# Monitor：derived 阶段（owners/scenario 需要同一轮其他采集器的新鲜结果）
# ---------------------------------------------------------------------------


def test_monitor_tick_runs_derived_after_primary_collectors():
    rec = Recorder(":memory:")

    def owners_derived(snapshot):
        return collectors.collect_interface_owners(
            snapshot["interfaces"], snapshot["vpn_services"], snapshot["netbird"], snapshot["mihomo"],
        )

    def scenario_derived(snapshot):
        return scenario_envelope(summarize_scenario(snapshot))

    collectors_map = {
        "interfaces": lambda: _OWNERS_IFACES,
        "vpn_services": lambda: _OWNERS_VPN,
        "netbird": lambda: _OWNERS_NETBIRD,
        "mihomo": lambda: _OWNERS_MIHOMO,
        "routes": lambda: {"error": None, "ipv4": [{"destination": "default", "interface": "utun6"}], "ipv6": []},
        "proxies": lambda: {"error": None, "system": {}},
    }
    monitor = Monitor(collectors_map, rec, interval=999, derived={"owners": owners_derived, "scenario": scenario_derived})
    monitor.tick(at=1.0)
    latest = rec.latest()
    assert latest["owners"]["owners"]["utun6"]["owner_kind"] == "mihomo_tun"
    assert latest["scenario"]["default_via"] == "clash_tun"
    rec.close()


# ---------------------------------------------------------------------------
# Recorder：用户命名场景（scene），跟自动识别的 scenario 平行、互不影响
# ---------------------------------------------------------------------------


def test_create_scene_and_capture_more_snapshots(recorder):
    scene_id = recorder.create_scene("公司", snapshot={"wifi": "corp-5g"}, at=1.0)
    snap_id = recorder.capture_scene(scene_id, snapshot={"wifi": "corp-5g-v2"}, at=2.0)
    snaps = recorder.scene_snapshots(scene_id)
    assert len(snaps) == 2
    assert snaps[-1]["id"] == snap_id
    assert [s["at"] for s in snaps] == [1.0, 2.0]
    assert "data" not in snaps[0]  # 默认摘要，不带完整内容
    full = recorder.scene_snapshots(scene_id, include_data=True)
    assert full[0]["data"] == {"wifi": "corp-5g"}
    assert recorder.scene_snapshot(snap_id)["data"] == {"wifi": "corp-5g-v2"}


def test_create_scene_duplicate_name_raises(recorder):
    recorder.create_scene("公司", snapshot={}, at=1.0)
    with pytest.raises(ValueError):
        recorder.create_scene("  公司  ", snapshot={}, at=2.0)  # 首尾空白规范化后同名
    with pytest.raises(ValueError):
        recorder.create_scene("公司", snapshot={}, at=3.0)


def test_create_scene_same_name_different_source_allowed(recorder):
    a = recorder.create_scene("公司", snapshot={}, at=1.0, source="local")
    b = recorder.create_scene("公司", snapshot={}, at=1.0, source="router-a")
    assert a != b


def test_enter_scene_auto_ends_previous(recorder):
    home = recorder.create_scene("家", snapshot={}, at=0.0)
    office = recorder.create_scene("公司", snapshot={}, at=0.0)
    recorder.enter_scene(home, at=1.0)
    recorder.enter_scene(office, at=5.0)
    periods = recorder.scene_periods(0.0, 10.0)
    assert len(periods) == 2
    home_period = next(p for p in periods if p["scene_id"] == home)
    office_period = next(p for p in periods if p["scene_id"] == office)
    assert home_period["start"] == 1.0 and home_period["end"] == 5.0
    assert office_period["start"] == 5.0 and office_period["end"] == 10.0  # 仍打开，用 until 收尾
    assert recorder.current_scene()["scene_id"] == office


def test_current_scene_none_when_never_entered(recorder):
    assert recorder.current_scene() is None
    recorder.create_scene("家", snapshot={}, at=0.0)
    assert recorder.current_scene() is None


def test_leave_scene_clears_current(recorder):
    home = recorder.create_scene("家", snapshot={}, at=0.0)
    recorder.enter_scene(home, at=1.0)
    recorder.leave_scene(at=3.0)
    assert recorder.current_scene() is None
    periods = recorder.scene_periods(0.0, 10.0)
    assert periods[0]["end"] == 3.0


def test_current_scene_persists_across_reopen(tmp_path):
    path = str(tmp_path / "scene.sqlite")
    rec1 = Recorder(path)
    scene_id = rec1.create_scene("家", snapshot={"x": 1}, at=0.0)
    rec1.enter_scene(scene_id, at=1.0)
    rec1.close()
    rec2 = Recorder(path)
    current = rec2.current_scene()
    assert current is not None
    assert current["scene_id"] == scene_id
    assert current["name"] == "家"
    rec2.close()


def test_record_tags_observations_with_current_scene(recorder):
    home = recorder.create_scene("家", snapshot={}, at=0.0)
    recorder.record({"interfaces": {"error": None, "interfaces": []}}, at=1.0)  # 还没进场景
    recorder.enter_scene(home, at=2.0)
    recorder.record({"interfaces": {"error": None, "interfaces": [{"name": "en0", "status": "active", "ipv4": [], "ipv6": []}]}}, at=3.0)
    rows = recorder._con.execute("SELECT at, scene_id FROM snapshots ORDER BY at").fetchall()
    assert rows[0] == (1.0, None)
    assert rows[1] == (3.0, home)


def test_probes_scene_id_filter(recorder):
    home = recorder.create_scene("家", snapshot={}, at=0.0)
    office = recorder.create_scene("公司", snapshot={}, at=0.0)

    def payload(status):
        return {
            "error": None,
            "targets": [{"name": "t1", "error": None, "proxied": False,
                         "route": {"interface": None}, "probe": {"status": status}}],
        }

    recorder.enter_scene(home, at=0.0)
    recorder.record({"targets": payload("ok")}, at=1.0)
    recorder.enter_scene(office, at=2.0)
    recorder.record({"targets": payload("timeout")}, at=3.0)

    home_probes = recorder.probes("t1", 0.0, 10.0, scene_id=home)
    office_probes = recorder.probes("t1", 0.0, 10.0, scene_id=office)
    assert [p["status"] for p in home_probes] == ["ok"]
    assert [p["status"] for p in office_probes] == ["timeout"]
    assert len(recorder.probes("t1", 0.0, 10.0)) == 2


def test_scenes_lists_counts_and_duration(recorder):
    home = recorder.create_scene("家", note="有猫", snapshot={"a": 1}, at=0.0)
    recorder.capture_scene(home, snapshot={"a": 2}, at=5.0)
    recorder.enter_scene(home, at=0.0)
    recorder.leave_scene(at=10.0)
    scenes = recorder.scenes(until=10.0)
    assert len(scenes) == 1
    s = scenes[0]
    assert s["name"] == "家" and s["note"] == "有猫"
    assert s["snapshot_count"] == 2
    assert s["first_snapshot_at"] == 0.0 and s["last_snapshot_at"] == 5.0
    assert s["duration"] == 10.0


def test_rename_scene(recorder):
    home = recorder.create_scene("家", snapshot={}, at=0.0)
    recorder.rename_scene(home, "我家")
    assert recorder.scenes()[0]["name"] == "我家"


def test_rename_scene_duplicate_raises(recorder):
    recorder.create_scene("家", snapshot={}, at=0.0)
    office = recorder.create_scene("公司", snapshot={}, at=0.0)
    with pytest.raises(ValueError):
        recorder.rename_scene(office, "家")


def test_delete_scene_soft_deletes_and_frees_name(recorder):
    home = recorder.create_scene("家", snapshot={}, at=0.0)
    recorder.enter_scene(home, at=1.0)
    recorder.delete_scene(home, at=5.0)
    # 默认列表不再出现，但可以带 include_deleted 看到，标记为已删除
    assert recorder.scenes() == []
    all_scenes = recorder.scenes(include_deleted=True)
    assert all_scenes[0]["deleted"] is True
    # 名字空出来，可以重新创建同名场景
    new_home = recorder.create_scene("家", snapshot={}, at=6.0)
    assert new_home != home
    # 已打开的区间被关闭，不再是 current
    assert recorder.current_scene() is None
    periods = recorder.scene_periods(0.0, 10.0)
    assert any(p["scene_id"] == home and p["end"] == 5.0 for p in periods)
    # 历史观测里的 scene_id 保留，不置空/断链
    recorder.record({"interfaces": {"error": None, "interfaces": []}}, at=2.0)


def test_scene_capture_and_enter_require_existing_scene(recorder):
    with pytest.raises(ValueError):
        recorder.capture_scene(999, snapshot={}, at=1.0)
    with pytest.raises(ValueError):
        recorder.enter_scene(999, at=1.0)


def test_scene_schema_migration_from_legacy_db(tmp_path):
    """旧库（没有 scene 相关表/列）打开后自动补全，不丢旧数据。"""
    path = str(tmp_path / "legacy.sqlite")
    con = sqlite3.connect(path)
    con.executescript(
        """
        CREATE TABLE snapshots (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            at REAL NOT NULL,
            source TEXT NOT NULL DEFAULT 'local'
        );
        CREATE TABLE snapshot_items (
            snapshot_id INTEGER NOT NULL,
            collector TEXT NOT NULL,
            stored TEXT NOT NULL,
            payload TEXT,
            hash TEXT NOT NULL,
            PRIMARY KEY (snapshot_id, collector)
        );
        CREATE TABLE latest (
            collector TEXT PRIMARY KEY,
            payload TEXT NOT NULL,
            hash TEXT NOT NULL,
            at REAL NOT NULL
        );
        CREATE TABLE events (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            at REAL NOT NULL,
            kind TEXT NOT NULL,
            subject TEXT NOT NULL,
            before TEXT,
            after TEXT,
            source TEXT NOT NULL DEFAULT 'local'
        );
        CREATE TABLE annotations (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            at REAL NOT NULL,
            source TEXT NOT NULL DEFAULT 'local',
            text TEXT NOT NULL
        );
        """
    )
    con.execute(
        "INSERT INTO snapshot_items(snapshot_id, collector, stored, payload, hash) "
        "VALUES (1, 'interfaces', 'full', '{\"interfaces\": []}', 'h')"
    )
    con.execute("INSERT INTO snapshots(id, at, source) VALUES (1, 1.0, 'local')")
    con.execute(
        "INSERT INTO latest(collector, payload, hash, at) VALUES "
        "('interfaces', '{\"interfaces\": []}', 'h', 1.0)"
    )
    con.commit()
    con.close()

    rec = Recorder(path)
    # 旧数据没丢
    assert rec.latest()["interfaces"]["interfaces"] == []
    # 新表/新列可用
    scene_id = rec.create_scene("家", snapshot={"x": 1}, at=2.0)
    rec.enter_scene(scene_id, at=2.0)
    rec.record({"interfaces": {"error": None, "interfaces": []}}, at=3.0)
    row = rec._con.execute("SELECT scene_id FROM snapshots WHERE at = 3.0").fetchone()
    assert row[0] == scene_id
    rec.close()


def test_default_interface_follows_clash_split_routes_not_literal_default():
    from pybase.net.monitor.scenario import _default_interface

    def route(dest, iface, flags="UGSc"):
        return {"destination": dest, "gateway": "x", "flags": flags, "interface": iface}

    # mihomo TUN：default 仍指向 en0，但 1/8、2/7…128/1 这组拆分路由更具体
    split = [route("default", "en0", "UGScg")] + [
        route(d, "utun6") for d in ("1", "2/7", "4/6", "8/5", "16/4", "32/3", "64/2", "128.0/1")]
    assert _default_interface({"ipv4": split}, "ipv4") == "utun6"
    assert _default_interface({"ipv4": [route("default", "en0")]}, "ipv4") == "en0"
    # macOS 的 I 标志（按接口限定）不参与全局选路
    v6 = [route("default", "utun3", "UGcIg"), route("default", "en0", "UGcg")]
    assert _default_interface({"ipv6": v6}, "ipv6") == "en0"
    assert _default_interface({"error": "boom"}, "ipv4") is None


def test_set_scene_note_updates_and_rejects_missing():
    from pybase.net.monitor.recorder import Recorder

    r = Recorder(":memory:")
    sid = r.create_scene("家", note="旧", snapshot={}, at=1)
    r.set_scene_note(sid, "开了 WG-A")
    assert r.scenes(until=2)[0]["note"] == "开了 WG-A"
    with pytest.raises(ValueError):
        r.set_scene_note(999, "x")


def test_scene_settings_roundtrip_and_replace(tmp_path):
    from pybase.net.monitor.recorder import Recorder

    path = str(tmp_path / "m.db")
    r = Recorder(path)
    sid = r.create_scene("公司", snapshot={}, at=1)
    assert r.scene_settings(sid) == {}
    r.set_scene_settings(sid, {"targets": [{"name": "nas", "host": "10.0.0.2", "port": 445}]}, at=2)
    r.set_scene_settings(sid, {"targets": []}, at=3)  # 整体替换
    r.close()
    assert Recorder(path).scene_settings(sid) == {"targets": []}
    with pytest.raises(ValueError):
        Recorder(path).set_scene_settings(999, {}, at=4)


def test_summarize_scenario_system_proxy_judged_by_enable_flags_not_dict():
    # 全关时 scutil --proxy 只剩 FTPPassive：不能判成"系统代理开着"，内核在跑也不算接管
    sc = summarize_scenario(_scenario_snapshot(clash_mode="rule", default_iface="en0"))
    assert sc.system_proxy is False and sc.system_proxy_to_clash is False
    assert "未接管" in sc.label and "nosysproxy" in sc.key

    sc = summarize_scenario(_scenario_snapshot(clash_mode="rule", system_proxy=True, default_iface="en0"))
    assert sc.system_proxy_to_clash is True and "Clash 接管（系统代理" in sc.label

    snap = _scenario_snapshot(default_iface="en0")
    snap["proxies"] = {"error": None, "system": {"SOCKSEnable": "1", "SOCKSProxy": "10.0.0.9", "SOCKSPort": "1080"}}
    sc = summarize_scenario(snap)
    assert sc.system_proxy is True and sc.system_proxy_to_clash is False
    assert "系统代理→10.0.0.9:1080" in sc.label and "sysproxy-other" in sc.key
