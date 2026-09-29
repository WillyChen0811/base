"""pybase.net.monitor.apps：进程匹配、出口归属、system/router/clash 三条探测
路径的解析逻辑、`collect_apps` 的装配，以及 `events._diff_apps` 的事件生成。

网络/子进程相关的底层函数（`route_of`/`resolve_tool`/`run`/`socket`/`ssl`）全部
monkeypatch 掉，不依赖真实网络环境。
"""
from __future__ import annotations

import socket
import ssl
import subprocess

import pytest

from pybase.net.monitor import apps, events


def _at() -> float:
    return 1700000000.0


# ---------------------------------------------------------------------------
# 进程匹配
# ---------------------------------------------------------------------------


def test_process_matches_prefix():
    assert apps._process_matches(["/Applications/WeChat.app/"], "/Applications/WeChat.app/Contents/MacOS/WeChat")


def test_process_matches_regex():
    assert apps._process_matches([r"claude/versions/\d+\.\d+\.\d+$"], "/Users/x/.local/share/claude/versions/2.1.284")


def test_process_matches_invalid_regex_does_not_raise():
    assert apps._process_matches(["("], "/some/path") is False


def test_match_processes_running_with_multiple_helpers():
    processes = [
        {"pid": 1, "path": "/Applications/WeChat.app/Contents/MacOS/WeChat"},
        {"pid": 2, "path": "/Applications/WeChat.app/Contents/Frameworks/Helper (Renderer).app/Helper"},
        {"pid": 3, "path": "/Applications/Safari.app/Contents/MacOS/Safari"},
    ]
    apps_cfg = [
        {"name": "微信", "process": ["/Applications/WeChat.app/"]},
        {"name": "钉钉", "process": ["/Applications/DingTalk.app/"]},
    ]
    result = apps.match_processes(apps_cfg, processes)
    assert result["微信"]["running"] is True
    assert sorted(result["微信"]["pids"]) == [1, 2]
    assert result["钉钉"]["running"] is False
    assert result["钉钉"]["pids"] == []


def test_list_processes_non_darwin_returns_empty(monkeypatch):
    monkeypatch.setattr(apps.platform, "system", lambda: "Linux")
    assert apps.list_processes() == []


# ---------------------------------------------------------------------------
# 已建立连接
# ---------------------------------------------------------------------------

_LSOF_SAMPLE = "p123\nn1.2.3.4:51000->198.18.1.5:443\nn*:7893\np456\nn10.0.0.1:60000->192.168.1.1:22\n"


def _fake_run(stdout: str, returncode: int = 0):
    def runner(cmd, *, timeout):
        return subprocess.CompletedProcess(cmd, returncode, stdout=stdout, stderr="")
    return runner


def test_list_established_connections_parses_fpn(monkeypatch):
    monkeypatch.setattr(apps, "resolve_tool", lambda explicit, name: "/usr/sbin/lsof")
    monkeypatch.setattr(apps, "run", _fake_run(_LSOF_SAMPLE))
    conns = apps.list_established_connections()
    assert conns[123] == [("198.18.1.5", 443)]
    assert conns[456] == [("192.168.1.1", 22)]


def test_list_established_connections_tool_missing_returns_empty(monkeypatch):
    def raise_not_found(explicit, name):
        raise apps.ToolNotFoundError("no lsof")
    monkeypatch.setattr(apps, "resolve_tool", raise_not_found)
    assert apps.list_established_connections() == {}


def test_list_established_connections_no_matches_returncode_1_not_error(monkeypatch):
    monkeypatch.setattr(apps, "resolve_tool", lambda explicit, name: "/usr/sbin/lsof")
    monkeypatch.setattr(apps, "run", _fake_run("", returncode=1))
    assert apps.list_established_connections() == {}


# ---------------------------------------------------------------------------
# 出口归属
# ---------------------------------------------------------------------------


def test_exit_label_clash_proxy():
    assert apps._exit_label("127.0.0.1", 7893, "direct:en0", (7893,)) == "clash_proxy"


def test_exit_label_router_fakeip():
    assert apps._exit_label("198.18.1.5", 443, "direct:en0", ()) == "router_fakeip"


def test_exit_label_passthrough_via():
    assert apps._exit_label("8.8.8.8", 443, "wireguard:Deguo", ()) == "wireguard:Deguo"


def test_app_exits_counts_by_label(monkeypatch):
    class FakeRoute:
        def __init__(self, interface):
            self.interface = interface

    def fake_route_of(host, *, timeout):
        return FakeRoute("en0") if host != "10.0.0.5" else FakeRoute("utun6")

    monkeypatch.setattr(apps, "route_of", fake_route_of)
    # owners 是 collect_interface_owners 结果里已解包的 "owners" 字段（调用方约定，
    # 见 monitor_app.build_primary_collectors 里同样的解包方式），不是整个信封
    owners = {"utun6": {"owner_kind": "mihomo_tun", "owner_name": "mihomo"}}
    connections = {
        1: [("198.18.1.5", 443), ("10.0.0.5", 443)],
        2: [("127.0.0.1", 7893)],
    }
    counts = apps.app_exits([1, 2], connections, owners, (7893,))
    assert counts == {"router_fakeip": 1, "clash_tun": 1, "clash_proxy": 1}


def test_app_exits_shares_route_cache_across_calls(monkeypatch):
    calls = []

    def fake_route_of(host, *, timeout):
        calls.append(host)
        return None

    monkeypatch.setattr(apps, "route_of", fake_route_of)
    connections = {1: [("8.8.8.8", 443)], 2: [("8.8.8.8", 443)]}
    cache: dict = {}
    apps.app_exits([1], connections, None, (), route_cache=cache)
    apps.app_exits([2], connections, None, (), route_cache=cache)
    assert calls == ["8.8.8.8"]  # 第二次命中缓存，没有再查一次路由


# ---------------------------------------------------------------------------
# DNS 报文解析
# ---------------------------------------------------------------------------


def _build_dns_response(txid: int, ips: list[str]) -> bytes:
    import struct as _struct
    header = _struct.pack(">HHHHHH", txid, 0x8180, 1, len(ips), 0, 0)
    question = b"\x03api\x09anthropic\x03com\x00" + _struct.pack(">HH", 1, 1)
    answers = b""
    for ip in ips:
        answers += b"\xc0\x0c" + _struct.pack(">HHIH", 1, 1, 60, 4) + socket.inet_aton(ip)
    return header + question + answers


def test_parse_dns_a_response_single_answer():
    data = _build_dns_response(0x1234, ["198.18.1.5"])
    assert apps._parse_dns_a_response(data, expected_txid=0x1234) == ["198.18.1.5"]


def test_parse_dns_a_response_multiple_answers():
    data = _build_dns_response(0x1234, ["198.18.1.5", "198.18.2.6"])
    assert apps._parse_dns_a_response(data, expected_txid=0x1234) == ["198.18.1.5", "198.18.2.6"]


def test_parse_dns_a_response_txid_mismatch_returns_empty():
    data = _build_dns_response(0x1234, ["198.18.1.5"])
    assert apps._parse_dns_a_response(data, expected_txid=0x9999) == []


def test_parse_dns_a_response_truncated_returns_empty():
    assert apps._parse_dns_a_response(b"\x00\x01", expected_txid=1) == []


# ---------------------------------------------------------------------------
# TLS/HEAD（用假 socket，不碰真实网络）
# ---------------------------------------------------------------------------


class _FakePlainSocket:
    """占位的"已连接的裸 socket"：只需要 `settimeout` 不炸，真正的收发发生在
    TLS 包一层之后的 `_FakeTlsSocket` 上。"""

    def settimeout(self, t):
        pass


class _FakeTlsSocket:
    def __init__(self, response: bytes):
        self._response = response
        self.sent = None

    def settimeout(self, t):
        pass

    def sendall(self, data):
        self.sent = data

    def recv(self, n):
        return self._response

    def close(self):
        pass


class _RaisingCtx:
    def wrap_socket(self, sock, server_hostname):
        raise ssl.SSLError("handshake failed")


class _OkCtx:
    def __init__(self, response: bytes):
        self._response = response

    def wrap_socket(self, sock, server_hostname):
        return _FakeTlsSocket(self._response)


def test_finish_tls_head_ok(monkeypatch):
    monkeypatch.setattr(apps.ssl, "create_default_context", lambda: _OkCtx(b"HTTP/1.1 200 OK\r\n"))
    result = apps._finish_tls_head(_FakePlainSocket(), "example.com", 0.0, 5.0, 1.0, 3.0, ip="1.2.3.4")
    assert result["status"] == "ok"
    assert result["http_status"] == 200
    assert result["ip"] == "1.2.3.4"


def test_finish_tls_head_accepts_non_2xx_status_as_ok(monkeypatch):
    """探测只看"收到状态码"，403/404 也算通。"""
    monkeypatch.setattr(apps.ssl, "create_default_context", lambda: _OkCtx(b"HTTP/1.1 403 Forbidden\r\n"))
    result = apps._finish_tls_head(_FakePlainSocket(), "example.com", 0.0, 5.0, 1.0, 3.0)
    assert result["status"] == "ok"
    assert result["http_status"] == 403


def test_finish_tls_head_tls_fail(monkeypatch):
    monkeypatch.setattr(apps.ssl, "create_default_context", lambda: _RaisingCtx())
    result = apps._finish_tls_head(_FakePlainSocket(), "example.com", 0.0, 5.0, 1.0, 3.0)
    assert result["status"] == "tls_fail"


def test_safe_probe_catches_exceptions():
    def boom(*a, **k):
        raise RuntimeError("boom")
    result = apps._safe_probe(boom)
    assert result["status"] == "error"


# ---------------------------------------------------------------------------
# clash 路径：命中规则/节点链查询
# ---------------------------------------------------------------------------


def test_lookup_clash_connection_finds_matching_host(monkeypatch):
    def fake_mihomo_get(base_url, path, secret, timeout):
        assert path == "/connections"
        return {"connections": [
            {"metadata": {"host": "other.com"}, "rule": "Match", "rulePayload": "", "chains": ["DIRECT"]},
            {"metadata": {"host": "platform.claude.com"}, "rule": "Match", "rulePayload": "漏网之鱼",
             "chains": ["DIRECT"]},
        ]}
    monkeypatch.setattr(apps, "_mihomo_get", fake_mihomo_get)
    info = apps._lookup_clash_connection(("http://127.0.0.1:9090", None), "platform.claude.com", 3.0)
    assert info == {"rule": "Match", "rule_payload": "漏网之鱼", "chains": ["DIRECT"]}


def test_lookup_clash_connection_no_match_returns_none(monkeypatch):
    monkeypatch.setattr(apps, "_mihomo_get", lambda *a, **k: {"connections": []})
    assert apps._lookup_clash_connection(("http://127.0.0.1:9090", None), "x.com", 3.0) is None


def test_lookup_clash_connection_api_error_returns_none(monkeypatch):
    def raise_err(*a, **k):
        raise OSError("connection refused")
    monkeypatch.setattr(apps, "_mihomo_get", raise_err)
    assert apps._lookup_clash_connection(("http://127.0.0.1:9090", None), "x.com", 3.0) is None


def test_probe_clash_includes_rule_info_when_clash_api_given(monkeypatch):
    class _FakeConnectSocket(_FakePlainSocket):
        def __init__(self):
            self.sent = None
        def recv(self, n):
            return b"HTTP/1.1 200 Connection Established\r\n\r\n"
        def sendall(self, data):
            self.sent = data
        def close(self):
            pass

    monkeypatch.setattr(apps.socket, "create_connection", lambda addr, timeout: _FakeConnectSocket())
    monkeypatch.setattr(apps, "_lookup_clash_connection", lambda api, host, timeout: {
        "rule": "Match", "rule_payload": "漏网之鱼", "chains": ["DIRECT"],
    })
    monkeypatch.setattr(apps, "_finish_tls_head", lambda *a, **k: {"status": "tls_fail", "error": "boom"})

    result = apps.probe_clash(
        "platform.claude.com", proxy_host="127.0.0.1", proxy_port=7893,
        clash_api=("http://127.0.0.1:9090", None),
    )
    assert result["status"] == "tls_fail"
    assert result["rule"] == "Match"
    assert result["chains"] == ["DIRECT"]


def test_probe_clash_skips_rule_lookup_without_clash_api(monkeypatch):
    class _FakeConnectSocket(_FakePlainSocket):
        def recv(self, n):
            return b"HTTP/1.1 200 Connection Established\r\n\r\n"
        def sendall(self, data):
            pass
        def close(self):
            pass

    monkeypatch.setattr(apps.socket, "create_connection", lambda addr, timeout: _FakeConnectSocket())
    called = []
    monkeypatch.setattr(apps, "_lookup_clash_connection", lambda *a, **k: called.append(1))
    monkeypatch.setattr(apps, "_finish_tls_head", lambda *a, **k: {"status": "ok"})

    result = apps.probe_clash("x.com", proxy_host="127.0.0.1", proxy_port=7893)
    assert "rule" not in result
    assert called == []


# ---------------------------------------------------------------------------
# collect_apps 装配
# ---------------------------------------------------------------------------


def _fake_details(entries: dict[int, tuple[int, str, str]]) -> dict[int, dict]:
    """`{pid: (ppid, tty, command)}` -> `list_process_details` 的返回形状。"""
    return {pid: {"ppid": ppid, "tty": tty, "command": command} for pid, (ppid, tty, command) in entries.items()}


def test_collect_apps_end_to_end(monkeypatch):
    apps_cfg = [
        {"name": "Claude Code", "process": ["/claude/versions/"], "domains": ["api.anthropic.com"]},
        {"name": "Chrome", "process": ["/Google Chrome.app/"], "domains": ["api.anthropic.com", "www.google.com"]},
    ]

    def fake_process_lister():
        return [{"pid": 100, "path": "/x/claude/versions/2.1.284"}]

    def fake_connection_lister():
        return {100: [("1.2.3.4", 443)]}

    details = _fake_details({100: (50, "ttys001", "/x/claude/versions/2.1.284")})

    probe_calls = []

    def fake_probe_system(host, *, timeout):
        probe_calls.append(host)
        return {"status": "ok", "http_status": 200}

    monkeypatch.setattr(apps, "route_of", lambda host, *, timeout: None)
    monkeypatch.setattr(apps, "probe_system", fake_probe_system)

    result = apps.collect_apps(
        apps_cfg, process_lister=fake_process_lister, connection_lister=fake_connection_lister,
        process_detail_lister=lambda: details, cwd_lister=lambda pids: {},
    )
    assert result["error"] is None
    by_name = {a["name"]: a for a in result["apps"]}
    cc = by_name["Claude Code"]
    assert cc["running"] is True
    assert cc["pid_count"] == 1
    assert len(cc["instances"]) == 1
    inst = cc["instances"][0]
    assert inst["pid"] == 100
    assert inst["tty"] == "ttys001"
    assert inst["exits"] == {"unknown": 1}  # route_of 被 mock 成 None，via_of(None, ...) -> "unknown"
    assert by_name["Chrome"]["running"] is False
    assert by_name["Chrome"]["instances"] == []
    # api.anthropic.com 被两个应用共用，只探测一次
    assert probe_calls.count("api.anthropic.com") == 1
    assert cc["domains"]["api.anthropic.com"]["system"]["status"] == "ok"
    assert by_name["Chrome"]["domains"]["www.google.com"]["system"]["status"] == "ok"
    # 没传 router_iface/clash_proxy，这两条路径不出现
    assert "router" not in by_name["Chrome"]["domains"]["www.google.com"]
    assert "clash" not in by_name["Chrome"]["domains"]["www.google.com"]


def test_collect_apps_process_lister_failure_returns_error():
    def boom():
        raise RuntimeError("libproc 挂了")
    result = apps.collect_apps([], process_lister=boom)
    assert result["error"] is not None


def test_collect_apps_probes_router_and_clash_when_configured(monkeypatch):
    apps_cfg = [{"name": "x", "process": ["/x"], "domains": ["a.com"]}]
    monkeypatch.setattr(apps, "probe_system", lambda host, *, timeout: {"status": "ok"})
    monkeypatch.setattr(apps, "probe_router", lambda host, **kw: {"status": "ok", "ip": "1.2.3.4"})
    monkeypatch.setattr(apps, "probe_clash", lambda host, **kw: {"status": "ok"})
    result = apps.collect_apps(
        apps_cfg, process_lister=lambda: [], connection_lister=lambda: {},
        process_detail_lister=lambda: {}, cwd_lister=lambda pids: {},
        router_iface="en0", router_dns="192.168.1.1", clash_proxy=("127.0.0.1", 7893),
    )
    domains = result["apps"][0]["domains"]["a.com"]
    assert set(domains) == {"system", "router", "clash"}


def test_collect_apps_merges_background_processes_into_one_instance(monkeypatch):
    """图形界面应用（tty 全是 ??）不管起了几个进程/Helper，也不管是不是互为
    父子，一律合并成唯一一个 "background" 实例——不按进程树拆分。"""
    apps_cfg = [{"name": "微信", "process": ["/Applications/WeChat.app/"], "domains": []}]

    def fake_process_lister():
        return [
            {"pid": 10, "path": "/Applications/WeChat.app/Contents/MacOS/WeChat"},
            {"pid": 11, "path": "/Applications/WeChat.app/Contents/Frameworks/Helper.app/Helper"},
            {"pid": 20, "path": "/Applications/WeChat.app/Contents/MacOS/WeChat"},  # 不是任何人的子进程
        ]

    details = _fake_details({
        10: (1, "??", "/Applications/WeChat.app/Contents/MacOS/WeChat"),
        11: (10, "??", "/Applications/WeChat.app/Contents/Frameworks/Helper.app/Helper"),
        20: (1, "??", "/Applications/WeChat.app/Contents/MacOS/WeChat"),
    })
    monkeypatch.setattr(apps, "route_of", lambda host, *, timeout: None)
    result = apps.collect_apps(
        apps_cfg, process_lister=fake_process_lister, connection_lister=lambda: {},
        process_detail_lister=lambda: details, cwd_lister=lambda pids: {},
    )
    instances = result["apps"][0]["instances"]
    assert len(instances) == 1
    inst = instances[0]
    assert inst["id"] == "background"
    assert inst["tty"] is None
    assert inst["member_pids"] == [10, 11, 20]
    # 代表 pid 优先选主进程（Contents/MacOS/WeChat），不是随便一个
    assert inst["pid"] in (10, 20)


def test_collect_apps_terminal_instances_split_by_tty(monkeypatch):
    """终端里的应用（tty 不是 ??）按 tty 分组——两个不同终端窗口各起一份
    Claude Code，应该是两个独立实例。"""
    apps_cfg = [{"name": "Claude Code", "process": [r"/claude/versions/"], "domains": []}]

    def fake_process_lister():
        return [
            {"pid": 100, "path": "/x/claude/versions/2.1.284"},
            {"pid": 200, "path": "/x/claude/versions/2.1.284"},
        ]

    details = _fake_details({
        100: (1, "ttys000", "claude"),
        200: (1, "ttys001", "claude --resume abc"),
    })
    monkeypatch.setattr(apps, "route_of", lambda host, *, timeout: None)
    result = apps.collect_apps(
        apps_cfg, process_lister=fake_process_lister, connection_lister=lambda: {},
        process_detail_lister=lambda: details, cwd_lister=lambda pids: {},
    )
    instances = result["apps"][0]["instances"]
    assert {i["id"] for i in instances} == {"ttys000", "ttys001"}
    assert {i["tty"] for i in instances} == {"ttys000", "ttys001"}


def test_collect_apps_main_process_required_for_running(monkeypatch):
    """只有 Helper 在跑（比如 Chrome 扩展的辅助进程），主进程没跑，不算应用
    在运行——复现"Claude 桌面版误判为在运行"的真实 bug。"""
    apps_cfg = [{"name": "Claude", "process": ["/Applications/Claude.app/"], "domains": []}]

    def fake_process_lister():
        return [{"pid": 5, "path": "/Applications/Claude.app/Contents/Helpers/chrome-native-host"}]

    details = _fake_details({5: (1, "??", "/Applications/Claude.app/Contents/Helpers/chrome-native-host")})
    result = apps.collect_apps(
        apps_cfg, process_lister=fake_process_lister, connection_lister=lambda: {},
        process_detail_lister=lambda: details, cwd_lister=lambda pids: {},
    )
    app = result["apps"][0]
    assert app["running"] is False
    assert app["pid_count"] == 1  # Helper 还是算在 pid_count 里，只是不算"运行中"


def test_collect_apps_running_true_when_main_process_present(monkeypatch):
    apps_cfg = [{"name": "Claude", "process": ["/Applications/Claude.app/"], "domains": []}]

    def fake_process_lister():
        return [
            {"pid": 5, "path": "/Applications/Claude.app/Contents/Helpers/chrome-native-host"},
            {"pid": 6, "path": "/Applications/Claude.app/Contents/MacOS/Claude"},
        ]

    details = _fake_details({
        5: (1, "??", "helper"),
        6: (1, "??", "Claude"),
    })
    result = apps.collect_apps(
        apps_cfg, process_lister=fake_process_lister, connection_lister=lambda: {},
        process_detail_lister=lambda: details, cwd_lister=lambda pids: {},
    )
    assert result["apps"][0]["running"] is True


def test_is_main_process_nested_helper_bundle_excluded():
    prefix = "/Applications/Foo.app/"
    nested = prefix + "Contents/Frameworks/Foo Helper.app/Contents/MacOS/Foo Helper"
    assert apps._is_main_process(prefix, nested) is False
    assert apps._is_main_process(prefix, prefix + "Contents/MacOS/Foo") is True


def test_is_main_process_non_bundle_pattern_always_main():
    assert apps._is_main_process(r"/\.local/share/claude/versions/", "/x/.local/share/claude/versions/2.1.284")


def test_collect_apps_attributes_child_process_connections(monkeypatch):
    """Claude Code 调用的 git 自己没匹配到任何应用规则，但父进程是 Claude Code
    的实例根，连接应该归到该实例下、标 child:git。"""
    apps_cfg = [{"name": "Claude Code", "process": ["/claude/versions/"], "domains": []}]

    def fake_process_lister():
        return [{"pid": 100, "path": "/x/claude/versions/2.1.284"}]

    details = _fake_details({
        100: (50, "ttys001", "/x/claude/versions/2.1.284"),
        200: (100, "ttys001", "/usr/bin/git fetch"),
    })

    def fake_connection_lister():
        return {200: [("140.82.112.3", 443)]}

    monkeypatch.setattr(apps, "route_of", lambda host, *, timeout: None)
    result = apps.collect_apps(
        apps_cfg, process_lister=fake_process_lister, connection_lister=fake_connection_lister,
        process_detail_lister=lambda: details, cwd_lister=lambda pids: {},
    )
    inst = result["apps"][0]["instances"][0]
    assert inst["pid"] == 100
    assert any(k == "child:git" for k in inst["exits"])
    assert inst["exits"]["child:git"] == 1


def test_collect_apps_terminal_other_bucket(monkeypatch):
    """挂在终端上、联网、但不属于任何应用/子进程的进程归入 terminal_other。"""
    details = _fake_details({300: (999, "ttys002", "/usr/bin/curl https://example.com")})

    def fake_connection_lister():
        return {300: [("93.184.216.34", 443)]}

    monkeypatch.setattr(apps, "route_of", lambda host, *, timeout: None)
    result = apps.collect_apps(
        [], process_lister=lambda: [], connection_lister=fake_connection_lister,
        process_detail_lister=lambda: details, cwd_lister=lambda pids: {},
    )
    assert "curl" in result["terminal_other"]
    assert result["terminal_other"]["curl"]["count"] == 1
    assert result["terminal_other"]["curl"]["instances"][0]["tty"] == "ttys002"


def test_collect_apps_background_unmatched_process_not_in_terminal_other(monkeypatch):
    """tty 为 ?? 的后台进程即使联网也不算"终端其他"（那条规则只针对终端里的）。"""
    details = _fake_details({400: (1, "??", "/usr/libexec/some-daemon")})

    def fake_connection_lister():
        return {400: [("1.2.3.4", 443)]}

    monkeypatch.setattr(apps, "route_of", lambda host, *, timeout: None)
    result = apps.collect_apps(
        [], process_lister=lambda: [], connection_lister=fake_connection_lister,
        process_detail_lister=lambda: details, cwd_lister=lambda pids: {},
    )
    assert result["terminal_other"] == {}


# ---------------------------------------------------------------------------
# 事件：events._diff_apps
# ---------------------------------------------------------------------------


def _instance(pid, *, tty="ttys001", exits=None, id_=None):
    return {
        "id": id_ if id_ is not None else (tty or "background"),
        "pid": pid, "tty": tty, "cwd": None, "command": None, "member_pids": [pid], "exits": exits or {},
    }


def _app(name, *, running=True, instances=None, domains=None):
    return {"name": name, "running": running, "instances": instances or [], "domains": domains or {}}


def _envelope(*apps_list):
    return {"error": None, "apps": list(apps_list)}


def test_diff_apps_start_stop():
    before = _envelope(_app("微信", running=False))
    after = _envelope(_app("微信", running=True))
    evs = events.diff("apps", before, after, _at())
    assert len(evs) == 1
    assert evs[0]["kind"] == "app_started"


def test_diff_apps_instance_started_and_stopped():
    before = _envelope(_app("Claude Code", instances=[_instance(100, tty="ttys000")]))
    after = _envelope(_app("Claude Code", instances=[_instance(200, tty="ttys001")]))
    evs = events.diff("apps", before, after, _at())
    kinds = {e["kind"] for e in evs}
    assert kinds == {"app_instance_stopped", "app_instance_started"}


def test_diff_apps_background_instance_pid_churn_is_not_a_restart():
    """background 实例（图形界面应用）底下的代表 pid 换了（比如原来选中的
    Helper 退出，换另一个 pid 代表），只要 id 还是 "background"，不该被当成
    实例重启——这是按 tty/background 而不是按 pid 对齐实例的意义所在。"""
    before = _envelope(_app("微信", instances=[_instance(10, tty=None, id_="background")]))
    after = _envelope(_app("微信", instances=[_instance(20, tty=None, id_="background")]))
    evs = events.diff("apps", before, after, _at())
    assert not any(e["kind"] in ("app_instance_started", "app_instance_stopped") for e in evs)


def test_diff_apps_exit_changed():
    before = _envelope(_app("Claude Code", instances=[_instance(100, exits={"direct:en0": 3})]))
    after = _envelope(_app("Claude Code", instances=[_instance(100, exits={"clash_tun": 3})]))
    evs = events.diff("apps", before, after, _at())
    kinds = [e["kind"] for e in evs]
    assert "app_exit_changed" in kinds


def test_diff_apps_domain_status_ok_to_fail():
    before = _envelope(_app("Claude Code", domains={"api.anthropic.com": {"system": {"status": "ok"}}}))
    after = _envelope(_app("Claude Code", domains={"api.anthropic.com": {"system": {"status": "connect_timeout"}}}))
    evs = events.diff("apps", before, after, _at())
    assert any(e["kind"] == "app_domain_status_changed" for e in evs)


def test_diff_apps_no_event_when_both_ok_despite_latency_change():
    before = _envelope(_app("x", domains={"a.com": {"system": {"status": "ok", "total_ms": 10}}}))
    after = _envelope(_app("x", domains={"a.com": {"system": {"status": "ok", "total_ms": 900}}}))
    evs = events.diff("apps", before, after, _at())
    assert evs == []


def test_diff_apps_router_fakeip_reset_needs_two_domains():
    before = _envelope(_app(
        "x", domains={
            "a.com": {"router": {"status": "ok", "ip": "198.18.1.1"}},
            "b.com": {"router": {"status": "ok", "ip": "198.18.1.2"}},
        },
    ))
    after = _envelope(_app(
        "x", domains={
            "a.com": {"router": {"status": "ok", "ip": "198.18.0.1"}},
            "b.com": {"router": {"status": "ok", "ip": "198.18.0.2"}},
        },
    ))
    evs = events.diff("apps", before, after, _at())
    reset = [e for e in evs if e["kind"] == "router_fakeip_reset"]
    assert len(reset) == 1
    assert reset[0]["after"] == ["a.com", "b.com"]


def test_diff_apps_single_domain_ip_change_is_not_reset():
    before = _envelope(_app("x", domains={"a.com": {"router": {"status": "ok", "ip": "198.18.1.1"}}}))
    after = _envelope(_app("x", domains={"a.com": {"router": {"status": "ok", "ip": "198.18.0.1"}}}))
    evs = events.diff("apps", before, after, _at())
    assert not any(e["kind"] == "router_fakeip_reset" for e in evs)


def test_diff_apps_before_none_no_events():
    assert events.diff("apps", None, _envelope(_app("x")), _at()) == []
