"""pybase.net.monitor.diff：两份快照的差异对比。夹具参考 test_net_monitor.py
里已有的真实（脱敏）格式，手写简化版覆盖各段增删改与噪声忽略。
"""
from __future__ import annotations

from pybase.net.monitor.diff import Change, diff_snapshots, summarize_changes


def _ok(**data) -> dict:
    return {"error": None, **data}


def _iface(name, status="active", mtu=1500, ipv4=None, ipv6=None):
    return {
        "name": name,
        "status": status,
        "mtu": mtu,
        "ipv4": [{"address": a} for a in (ipv4 or [])],
        "ipv6": [{"address": a} for a in (ipv6 or [])],
    }


# ---------------------------------------------------------------------------
# 空/无变化
# ---------------------------------------------------------------------------


def test_no_changes_returns_empty_list():
    snap = {
        "interfaces": _ok(interfaces=[_iface("en0", ipv4=["172.20.10.3"])]),
        "scenario": _ok(key="direct", label="直连"),
    }
    assert diff_snapshots(snap, snap) == []
    assert summarize_changes([]) == "两份快照没有差异"


# ---------------------------------------------------------------------------
# interfaces
# ---------------------------------------------------------------------------


def test_interfaces_added_removed_status_mtu_addresses():
    before = _ok(
        interfaces=[
            _iface("en0", status="active", mtu=1500, ipv4=["172.20.10.3"]),
            _iface("utun6", status="active", mtu=9000, ipv4=["198.18.0.1"]),
        ]
    )
    after = _ok(
        interfaces=[
            _iface("en0", status="inactive", mtu=1400, ipv4=["172.20.10.3", "172.20.10.4"]),
            _iface("utun7", status="active", mtu=1500, ipv6=["fe80::1"]),
        ]
    )
    changes = diff_snapshots({"interfaces": before}, {"interfaces": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["en0.status"].kind == "changed"
    assert by_subject["en0.status"].before == "active"
    assert by_subject["en0.status"].after == "inactive"
    assert by_subject["en0.mtu"].before == 1500 and by_subject["en0.mtu"].after == 1400
    assert sorted(by_subject["en0.ipv4"].after) == ["172.20.10.3", "172.20.10.4"]
    assert by_subject["utun6"].kind == "removed"
    assert by_subject["utun7"].kind == "added"


def test_interfaces_no_change_when_identical():
    payload = _ok(interfaces=[_iface("en0", ipv4=["172.20.10.3"])])
    assert diff_snapshots({"interfaces": payload}, {"interfaces": payload}) == []


# ---------------------------------------------------------------------------
# routes：增删/网关变化，忽略 expire 与链路本地 %scope 噪声
# ---------------------------------------------------------------------------


def test_routes_gateway_change_and_add_remove():
    before = _ok(
        ipv4=[
            {"destination": "default", "gateway": "172.20.10.1", "interface": "en0", "expire": None},
            {"destination": "10.0.0.0/8", "gateway": "10.0.0.1", "interface": "utun6", "expire": None},
        ],
        ipv6=[],
    )
    after = _ok(
        ipv4=[
            {"destination": "default", "gateway": "172.20.10.99", "interface": "en0", "expire": "1200"},
            {"destination": "192.168.1.0/24", "gateway": None, "interface": "en0", "expire": None},
        ],
        ipv6=[],
    )
    changes = diff_snapshots({"routes": before}, {"routes": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["ipv4:default"].kind == "changed"
    assert by_subject["ipv4:default"].before == ("172.20.10.1", "en0")
    assert by_subject["ipv4:default"].after == ("172.20.10.99", "en0")
    assert by_subject["ipv4:10.0.0.0/8"].kind == "removed"
    assert by_subject["ipv4:192.168.1.0/24"].kind == "added"


def test_routes_ignore_expire_only_change():
    before = _ok(ipv4=[{"destination": "default", "gateway": "1.1.1.1", "interface": "en0", "expire": None}], ipv6=[])
    after = _ok(ipv4=[{"destination": "default", "gateway": "1.1.1.1", "interface": "en0", "expire": "800"}], ipv6=[])
    assert diff_snapshots({"routes": before}, {"routes": after}) == []


def test_routes_ignore_link_local_scoped_ipv6_noise():
    before = _ok(ipv4=[], ipv6=[{"destination": "fe80::%en0/64", "gateway": None, "interface": "en0"}])
    after = _ok(
        ipv4=[],
        ipv6=[
            {"destination": "fe80::%en0/64", "gateway": None, "interface": "utun6"},
            {"destination": "fe80::%utun7/64", "gateway": None, "interface": "utun7"},
        ],
    )
    assert diff_snapshots({"routes": before}, {"routes": after}) == []


# ---------------------------------------------------------------------------
# dns
# ---------------------------------------------------------------------------


def test_dns_global_nameservers_search_and_scoped_resolver():
    before = _ok(
        resolvers=[
            {"scoped": False, "nameservers": ["8.8.8.8"], "search": ["local"], "if_name": None},
            {"scoped": True, "nameservers": ["10.0.0.1"], "search": [], "if_name": "en0"},
        ]
    )
    after = _ok(
        resolvers=[
            {"scoped": False, "nameservers": ["1.1.1.1"], "search": ["local", "corp"], "if_name": None},
            {"scoped": True, "nameservers": ["10.0.0.2"], "search": [], "if_name": "en0"},
        ]
    )
    changes = diff_snapshots({"dns": before}, {"dns": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["global.nameservers"].after == ["1.1.1.1"]
    assert by_subject["global.search"].after == ["corp", "local"]
    assert by_subject["interface:en0"].before == ["10.0.0.1"]
    assert by_subject["interface:en0"].after == ["10.0.0.2"]


def test_dns_scoped_resolver_added_removed():
    before = _ok(resolvers=[{"scoped": True, "nameservers": ["10.0.0.1"], "search": [], "if_name": "en0"}])
    after = _ok(resolvers=[{"scoped": True, "nameservers": ["10.0.0.1"], "search": [], "if_name": "utun6"}])
    changes = diff_snapshots({"dns": before}, {"dns": after})
    kinds = {c.subject: c.kind for c in changes}
    assert kinds["interface:en0"] == "removed"
    assert kinds["interface:utun6"] == "added"


# ---------------------------------------------------------------------------
# proxies
# ---------------------------------------------------------------------------


def test_proxies_system_fields_exceptions_and_env():
    before = _ok(
        system={"HTTPEnable": "0", "ExceptionsList": ["*.local", "169.254/16"]},
        env={"http_proxy": None},
    )
    after = _ok(
        system={"HTTPEnable": "1", "HTTPProxy": "127.0.0.1", "ExceptionsList": ["*.local", "10.0.0.0/8"]},
        env={"http_proxy": "http://127.0.0.1:7890"},
    )
    changes = diff_snapshots({"proxies": before}, {"proxies": after})
    subjects = {(c.section, c.kind, c.subject) for c in changes}
    assert ("proxies", "changed", "system.HTTPEnable") in subjects
    assert ("proxies", "changed", "system.HTTPProxy") in subjects  # 新出现的键，before 为 None
    assert ("proxies", "removed", "system.ExceptionsList") in subjects  # 169.254/16 被移除
    assert ("proxies", "added", "system.ExceptionsList") in subjects  # 10.0.0.0/8 新增
    assert ("proxies", "changed", "env.http_proxy") in subjects


# ---------------------------------------------------------------------------
# vpn_services
# ---------------------------------------------------------------------------


def test_vpn_services_status_change_and_add_remove():
    before = _ok(services=[{"name": "office", "uuid": "u1", "status": "Disconnected", "enabled": True}])
    after = _ok(
        services=[
            {"name": "office", "uuid": "u1", "status": "Connected", "enabled": True},
            {"name": "home", "uuid": "u2", "status": "Connected", "enabled": True},
        ]
    )
    changes = diff_snapshots({"vpn_services": before}, {"vpn_services": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["office"].kind == "changed"
    assert by_subject["home"].kind == "added"


# ---------------------------------------------------------------------------
# netbird
# ---------------------------------------------------------------------------


def test_netbird_connection_ip_and_peer_count():
    before = _ok(available=True, connected=False, daemon_status="Disconnected", netbird_ip="100.90.1.2/16", peers_total=3, peers_connected=1)
    after = _ok(available=True, connected=True, daemon_status="Connected", netbird_ip="100.90.1.3/16", peers_total=3, peers_connected=3)
    changes = diff_snapshots({"netbird": before}, {"netbird": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["connected"].after is True
    assert by_subject["netbird_ip"].after == "100.90.1.3/16"
    assert by_subject["peers_connected"].after == 3


def test_netbird_not_available_on_either_side_skips_field_comparison():
    before = _ok(available=False)
    after = _ok(available=True, connected=True, netbird_ip="100.90.1.2/16")
    changes = diff_snapshots({"netbird": before}, {"netbird": after})
    subjects = [c.subject for c in changes]
    assert subjects == ["available"]  # 只报可用性本身的变化，不比较 available=False 时没有的字段


# ---------------------------------------------------------------------------
# mihomo：不比连接数/流量
# ---------------------------------------------------------------------------


def test_mihomo_mode_tun_version_but_not_traffic():
    before = _ok(version="1.18.0", mode="rule", tun_enabled=False, tun_device=None, connections_count=5, total_upload=100)
    after = _ok(version="1.19.0", mode="global", tun_enabled=True, tun_device="utun6", connections_count=42, total_upload=999999)
    changes = diff_snapshots({"mihomo": before}, {"mihomo": after})
    subjects = {c.subject for c in changes}
    assert subjects == {"mode", "tun_enabled", "tun_device", "version"}


# ---------------------------------------------------------------------------
# owners
# ---------------------------------------------------------------------------


def test_owners_changed_ignores_evidence_text():
    before = _ok(owners={"utun6": {"owner_kind": "mihomo_tun", "owner_name": "mihomo", "evidence": "a"}})
    after = _ok(owners={"utun6": {"owner_kind": "mihomo_tun", "owner_name": "mihomo", "evidence": "b different wording"}})
    assert diff_snapshots({"owners": before}, {"owners": after}) == []


def test_owners_real_change_and_add_remove():
    before = _ok(owners={"utun6": {"owner_kind": "mihomo_tun", "owner_name": "mihomo", "evidence": "x"}})
    after = _ok(
        owners={
            "utun6": {"owner_kind": "unknown", "owner_name": None, "evidence": "y"},
            "utun7": {"owner_kind": "netbird", "owner_name": "netbird", "evidence": "z"},
        }
    )
    changes = diff_snapshots({"owners": before}, {"owners": after})
    kinds = {c.subject: c.kind for c in changes}
    assert kinds["utun6"] == "changed"
    assert kinds["utun7"] == "added"


# ---------------------------------------------------------------------------
# scenario
# ---------------------------------------------------------------------------


def test_scenario_key_and_label_change():
    before = _ok(key="direct", label="直连")
    after = _ok(key="wireguard:office", label="WireGuard：office")
    changes = diff_snapshots({"scenario": before}, {"scenario": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["key"].after == "wireguard:office"
    assert by_subject["label"].after == "WireGuard：office"


# ---------------------------------------------------------------------------
# error / 缺失
# ---------------------------------------------------------------------------


def test_section_error_on_either_side_is_unavailable():
    before = {"error": "ifconfig 退出码 1"}
    after = _ok(interfaces=[_iface("en0")])
    changes = diff_snapshots({"interfaces": before}, {"interfaces": after})
    assert len(changes) == 1
    assert changes[0].kind == "unavailable"
    assert changes[0].section == "interfaces"


def test_section_missing_from_one_side_is_unavailable():
    after = _ok(interfaces=[_iface("en0")])
    changes = diff_snapshots({}, {"interfaces": after})
    assert changes == [Change("interfaces", "unavailable", "interfaces", "missing", None)]


def test_section_missing_from_both_sides_is_skipped():
    assert diff_snapshots({"scenario": _ok(key="a", label="A")}, {"scenario": _ok(key="a", label="A")}) == []


def test_unknown_section_is_ignored():
    before = {"custom_thing": {"foo": 1}}
    after = {"custom_thing": {"foo": 2}}
    assert diff_snapshots(before, after) == []


# ---------------------------------------------------------------------------
# summarize_changes
# ---------------------------------------------------------------------------


def test_summarize_changes_one_line_per_section():
    before = {
        "interfaces": _ok(interfaces=[_iface("en0", status="active")]),
        "netbird": _ok(available=False),
    }
    after = {
        "interfaces": _ok(interfaces=[_iface("en0", status="inactive")]),
        "netbird": {"error": "netbird 退出码 1", "available": True},
    }
    text = summarize_changes(diff_snapshots(before, after))
    lines = text.splitlines()
    assert any(line.startswith("interfaces：") and "en0.status：active→inactive" in line for line in lines)
    assert any(line.startswith("netbird：无法比较") for line in lines)


def test_summarize_changes_spells_out_dict_field_changes():
    change = Change("vpn_services", "changed", "WG-B",
                    {"status": "Disconnected", "enabled": True},
                    {"status": "Connected", "enabled": True})
    assert "WG-B.status：Disconnected→Connected" in summarize_changes([change])
