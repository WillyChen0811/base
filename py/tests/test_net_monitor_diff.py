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


# ---------------------------------------------------------------------------
# dynamic_store：字段级下钻、服务 UUID 翻译、噪声忽略（依据见 diff.py 里的
# _NOISE_DYNAMIC_STORE_*_RE 注释：本机实测 LinkQuality/AutoJoinTimestamp/
# CachedScanRecord/Lease 时间戳几秒内自己变，跟配置无关）
# ---------------------------------------------------------------------------

_UUID = "AAAA1111-2222-3333-4444-555555555555"


def test_dynamic_store_field_drill_and_uuid_translation():
    before = _ok(keys={
        f"Setup:/Network/Service/{_UUID}": {"UserDefinedName": "My Ethernet"},
        f"Setup:/Network/Service/{_UUID}/DNS": {"ServerAddresses": ["8.8.8.8"]},
        "State:/Network/Global/IPv4": {"PrimaryInterface": "en0", "Router": "192.0.2.1"},
    }, count=3)
    after = _ok(keys={
        f"Setup:/Network/Service/{_UUID}": {"UserDefinedName": "My Ethernet"},
        f"Setup:/Network/Service/{_UUID}/DNS": {"ServerAddresses": ["1.1.1.1"]},
        "State:/Network/Global/IPv4": {"PrimaryInterface": "utun6", "Router": "192.0.2.1"},
    }, count=3)
    changes = diff_snapshots({"dynamic_store": before}, {"dynamic_store": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["State:/Network/Global/IPv4.PrimaryInterface"].after == "utun6"
    assert "State:/Network/Global/IPv4.Router" not in by_subject  # Router 没变，不出现
    dns_subject = f"Setup:/Network/Service/My Ethernet({_UUID[:8]})/DNS.ServerAddresses"
    assert by_subject[dns_subject].after == ["1.1.1.1"]
    # UserDefinedName 没变，服务本身这一条不产生变化
    assert f"Setup:/Network/Service/My Ethernet({_UUID[:8]})" not in by_subject


def test_dynamic_store_ignores_noise_link_quality_and_timestamps():
    before = _ok(keys={
        "State:/Network/Interface/en0/LinkQuality": {"LinkQuality": "100"},
        "State:/Network/Interface/en0/AirPort": {
            "AutoJoinTimestamp": "09/28/2026 10:00:00", "CachedScanRecord": "0xaa", "CHANNEL": "149",
        },
        "State:/Network/Service/X/DHCP": {"LeaseStartTime": "a", "LeaseExpirationTime": "b"},
        "State:/Network/Interface/en0/NAT64": {
            "PLATDiscoveryStartTime": "t1", "PLATDiscoveryCompletionTime": "t2",
        },
    }, count=4)
    after = _ok(keys={
        "State:/Network/Interface/en0/LinkQuality": {"LinkQuality": "42"},
        "State:/Network/Interface/en0/AirPort": {
            "AutoJoinTimestamp": "09/28/2026 12:00:00", "CachedScanRecord": "0xbb", "CHANNEL": "149",
        },
        "State:/Network/Service/X/DHCP": {"LeaseStartTime": "c", "LeaseExpirationTime": "d"},
        "State:/Network/Interface/en0/NAT64": {
            "PLATDiscoveryStartTime": "t3", "PLATDiscoveryCompletionTime": "t4",
        },
    }, count=4)
    assert diff_snapshots({"dynamic_store": before}, {"dynamic_store": after}) == []


def test_dynamic_store_added_removed_key():
    before = _ok(keys={"State:/A": "1"}, count=1)
    after = _ok(keys={"State:/A": "1", "State:/B": "2"}, count=2)
    changes = diff_snapshots({"dynamic_store": before}, {"dynamic_store": after})
    assert changes == [Change("dynamic_store", "added", "State:/B", None, "2")]


def test_dynamic_store_error_is_unavailable():
    before = {"error": "scutil 超时（5.0s）"}
    after = _ok(keys={}, count=0)
    changes = diff_snapshots({"dynamic_store": before}, {"dynamic_store": after})
    assert changes == [Change("dynamic_store", "unavailable", "dynamic_store", "scutil 超时（5.0s）", None)]


# ---------------------------------------------------------------------------
# nwi：接口顺序变化（主网卡切换）、增删、flags/address/VPN server
# ---------------------------------------------------------------------------


def _nwi_iface(name, flags="0x5", address=None, vpn_server=None):
    return {"name": name, "flags": {"value": flags, "names": []}, "address": address,
            "vpn_server": vpn_server, "reach": None}


def test_nwi_order_change_and_vpn_server_field_change():
    before = _ok(
        ipv4={"interfaces": [_nwi_iface("en0", address="203.0.113.5"),
                              _nwi_iface("utun6", address="198.51.100.2", vpn_server="198.51.100.1")],
              "reach": None},
        ipv6={"interfaces": [], "reach": None},
        interfaces=["en0", "utun6"],
    )
    after = _ok(
        ipv4={"interfaces": [_nwi_iface("utun6", address="198.51.100.2", vpn_server="198.51.100.9"),
                              _nwi_iface("en0", address="203.0.113.5")],
              "reach": None},
        ipv6={"interfaces": [], "reach": None},
        interfaces=["utun6", "en0"],
    )
    changes = diff_snapshots({"nwi": before}, {"nwi": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["ipv4.order"].before == ["en0", "utun6"]
    assert by_subject["ipv4.order"].after == ["utun6", "en0"]
    assert by_subject["ipv4.utun6.vpn_server"].after == "198.51.100.9"


def test_nwi_interface_added_removed():
    before = _ok(ipv4={"interfaces": [_nwi_iface("en0")], "reach": None},
                 ipv6={"interfaces": [], "reach": None}, interfaces=["en0"])
    after = _ok(ipv4={"interfaces": [_nwi_iface("en0"), _nwi_iface("utun6")], "reach": None},
                ipv6={"interfaces": [], "reach": None}, interfaces=["en0", "utun6"])
    changes = diff_snapshots({"nwi": before}, {"nwi": after})
    kinds = {c.subject: c.kind for c in changes}
    assert kinds["ipv4.utun6"] == "added"


# ---------------------------------------------------------------------------
# network_services：当前位置、服务增删/启用、顺序
# ---------------------------------------------------------------------------


def test_network_services_location_enabled_add_and_order():
    before = _ok(
        current_location="Automatic",
        services=[{"name": "Wi-Fi", "enabled": True}, {"name": "iPhone USB", "enabled": False}],
        order=[{"index": 1, "name": "Wi-Fi", "hardware_port": "Wi-Fi", "device": "en0"},
               {"index": 2, "name": "iPhone USB", "hardware_port": "iPhone USB", "device": "en5"}],
    )
    after = _ok(
        current_location="Office",
        services=[{"name": "Wi-Fi", "enabled": False}, {"name": "iPhone USB", "enabled": False},
                  {"name": "MyWireGuardTunnel", "enabled": True}],
        order=[{"index": 1, "name": "iPhone USB", "hardware_port": "iPhone USB", "device": "en5"},
               {"index": 2, "name": "Wi-Fi", "hardware_port": "Wi-Fi", "device": "en0"}],
    )
    changes = diff_snapshots({"network_services": before}, {"network_services": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["current_location"].after == "Office"
    assert by_subject["Wi-Fi.enabled"].after is False
    assert by_subject["MyWireGuardTunnel"].kind == "added"
    assert by_subject["order"].after == ["iPhone USB", "Wi-Fi"]


# ---------------------------------------------------------------------------
# dhcp：按接口比较关键字段，不比租约时间
# ---------------------------------------------------------------------------


def test_dhcp_field_change_and_ignores_lease_times():
    before = _ok(interfaces={"en0": {
        "error": None, "config_method": "DHCP", "router": "172.20.10.1", "domain_name": None,
        "domain_name_server": ["172.20.10.1"], "ssid": "redacted",
        "lease_start_time": "09/28/2026 15:28:49", "lease_expiration_time": "09/28/2026 16:28:49",
    }})
    after = _ok(interfaces={"en0": {
        "error": None, "config_method": "DHCP", "router": "172.20.10.254", "domain_name": None,
        "domain_name_server": ["172.20.10.1"], "ssid": "redacted",
        "lease_start_time": "09/28/2026 16:00:00", "lease_expiration_time": "09/28/2026 17:00:00",
    }})
    changes = diff_snapshots({"dhcp": before}, {"dhcp": after})
    assert changes == [Change("dhcp", "changed", "en0.router", "172.20.10.1", "172.20.10.254")]


def test_dhcp_interface_error_is_unavailable():
    before = _ok(interfaces={"en99": {"error": None, "config_method": "DHCP"}})
    after = _ok(interfaces={"en99": {"error": "ipconfig_get_summary(en99) failed: interface doesn't exist"}})
    changes = diff_snapshots({"dhcp": before}, {"dhcp": after})
    assert changes == [Change("dhcp", "unavailable", "en99", None,
                              "ipconfig_get_summary(en99) failed: interface doesn't exist")]


# ---------------------------------------------------------------------------
# gateway_mac：网关 MAC 变化是换网络的强信号
# ---------------------------------------------------------------------------


def test_gateway_mac_change():
    before = _ok(gateways={"172.20.10.1": {"error": None, "mac": "aa:bb:cc:dd:ee:ff", "interface": "en0"}})
    after = _ok(gateways={"172.20.10.1": {"error": None, "mac": "11:22:33:44:55:66", "interface": "en0"}})
    changes = diff_snapshots({"gateway_mac": before}, {"gateway_mac": after})
    assert changes == [Change("gateway_mac", "changed", "172.20.10.1", "aa:bb:cc:dd:ee:ff", "11:22:33:44:55:66")]


def test_gateway_mac_lookup_failure_toggle_is_unavailable():
    before = _ok(gateways={"172.20.10.1": {"error": None, "mac": "aa:bb:cc:dd:ee:ff", "interface": "en0"}})
    after = _ok(gateways={"172.20.10.1": {"error": "arp 缓存里没有 172.20.10.1（未连通或已过期）"}})
    changes = diff_snapshots({"gateway_mac": before}, {"gateway_mac": after})
    assert changes[0].kind == "unavailable"


# ---------------------------------------------------------------------------
# hosts：按 IP 合并多行后比较条目；条目未变但 sha256 变了单独报一条
# ---------------------------------------------------------------------------


def test_hosts_entries_added_and_changed_merges_same_ip_lines():
    before = _ok(entries=[{"ip": "127.0.0.1", "hostnames": ["localhost"]}], sha256="h1", mtime=1.0)
    after = _ok(
        entries=[
            {"ip": "127.0.0.1", "hostnames": ["localhost"]},
            {"ip": "127.0.0.1", "hostnames": ["view-localhost"]},
            {"ip": "1.2.3.4", "hostnames": ["example.com"]},
        ],
        sha256="h2", mtime=2.0,
    )
    changes = diff_snapshots({"hosts": before}, {"hosts": after})
    kinds = {c.subject: c.kind for c in changes}
    assert kinds["1.2.3.4"] == "added"
    assert kinds["127.0.0.1"] == "changed"


def test_hosts_sha_changed_without_entry_change_reports_file():
    before = _ok(entries=[{"ip": "127.0.0.1", "hostnames": ["localhost"]}], sha256="h1", mtime=1.0)
    after = _ok(entries=[{"ip": "127.0.0.1", "hostnames": ["localhost"]}], sha256="h2", mtime=2.0)
    changes = diff_snapshots({"hosts": before}, {"hosts": after})
    assert changes == [Change("hosts", "changed", "file", "h1", "h2")]


# ---------------------------------------------------------------------------
# resolver：按域名比较
# ---------------------------------------------------------------------------


def test_resolver_domain_added_and_changed():
    before = _ok(domains=[{"domain": "internal.example.com", "nameservers": ["10.0.0.1"],
                            "ports": [], "search_order": []}])
    after = _ok(domains=[
        {"domain": "internal.example.com", "nameservers": ["10.0.0.2"], "ports": [], "search_order": []},
        {"domain": "corp.example.com", "nameservers": ["10.0.1.1"], "ports": [], "search_order": []},
    ])
    changes = diff_snapshots({"resolver": before}, {"resolver": after})
    by_subject = {c.subject: c for c in changes}
    assert by_subject["internal.example.com.nameservers"].after == ["10.0.0.2"]
    assert by_subject["corp.example.com"].kind == "added"


# ---------------------------------------------------------------------------
# sysctl_net：值变化
# ---------------------------------------------------------------------------


def test_sysctl_net_value_change():
    before = _ok(values={"net.inet.ip.forwarding": {"error": None, "value": "0"}})
    after = _ok(values={"net.inet.ip.forwarding": {"error": None, "value": "1"}})
    changes = diff_snapshots({"sysctl_net": before}, {"sysctl_net": after})
    assert changes == [Change("sysctl_net", "changed", "net.inet.ip.forwarding", "0", "1")]


# ---------------------------------------------------------------------------
# summarize_changes：hosts/nwi 的定制中文摘要
# ---------------------------------------------------------------------------


def test_summarize_hosts_and_nwi_readable():
    before_hosts = _ok(entries=[], sha256="h1", mtime=1.0)
    after_hosts = _ok(entries=[{"ip": "1.2.3.4", "hostnames": ["example.com"]}], sha256="h2", mtime=2.0)
    before_nwi = _ok(ipv4={"interfaces": [_nwi_iface("en0"), _nwi_iface("utun6")], "reach": None},
                      ipv6={"interfaces": [], "reach": None}, interfaces=["en0", "utun6"])
    after_nwi = _ok(ipv4={"interfaces": [_nwi_iface("utun6"), _nwi_iface("en0")], "reach": None},
                     ipv6={"interfaces": [], "reach": None}, interfaces=["utun6", "en0"])
    text = summarize_changes(diff_snapshots(
        {"hosts": before_hosts, "nwi": before_nwi}, {"hosts": after_hosts, "nwi": after_nwi}))
    assert "hosts：新增 1 条（1.2.3.4 example.com）" in text
    assert "ipv4 主网卡：en0→utun6" in text
