"""pybase.net.monitor.events：新增段（dynamic_store/nwi/network_services/dhcp/
gateway_mac/hosts/resolver/sysctl_net）的事件检测。这几段的比较规则从
`diff.py` 复用，这里只验证事件种类/subject 对得上，以及跟 diff 同一套忽略/
翻译规则（不重复 diff 测试里已经覆盖的细节）。
"""
from __future__ import annotations

import time

from pybase.net.monitor import events


def _ok(**data) -> dict:
    return {"error": None, **data}


def _at() -> float:
    return time.time()


# ---------------------------------------------------------------------------
# 通用约定：before 为 None（第一次出现）不产生事件；一方 error 时跳过
# ---------------------------------------------------------------------------


def test_before_none_produces_no_events():
    after = _ok(keys={"State:/A": "1"}, count=1)
    assert events.diff("dynamic_store", None, after, _at()) == []


def test_error_on_either_side_skips_all_sections():
    after = _ok(keys={"State:/A": "1"}, count=1)
    before = {"error": "scutil 超时"}
    for collector in ("dynamic_store", "nwi", "network_services", "dhcp", "gateway_mac",
                       "hosts", "resolver", "sysctl_net"):
        assert events.diff(collector, before, after, _at()) == []


def test_unknown_collector_produces_no_events():
    assert events.diff("custom_thing", _ok(x=1), _ok(x=2), _at()) == []


# ---------------------------------------------------------------------------
# dynamic_store：跟 diff.py 同一套噪声忽略/服务名翻译
# ---------------------------------------------------------------------------


def test_dynamic_store_changed_event_per_key_and_ignores_noise():
    before = _ok(keys={
        "State:/Network/Global/IPv4": {"PrimaryInterface": "en0"},
        "State:/Network/Interface/en0/LinkQuality": {"LinkQuality": "100"},
    }, count=2)
    after = _ok(keys={
        "State:/Network/Global/IPv4": {"PrimaryInterface": "utun6"},
        "State:/Network/Interface/en0/LinkQuality": {"LinkQuality": "42"},
    }, count=2)
    result = events.diff("dynamic_store", before, after, _at())
    assert len(result) == 1
    assert result[0]["kind"] == "dynamic_store_changed"
    assert result[0]["subject"] == "State:/Network/Global/IPv4.PrimaryInterface"
    assert result[0]["after"] == "utun6"


def test_dynamic_store_translates_service_uuid_in_subject():
    uuid = "AAAA1111-2222-3333-4444-555555555555"
    before = _ok(keys={
        f"Setup:/Network/Service/{uuid}": {"UserDefinedName": "My Ethernet"},
        f"Setup:/Network/Service/{uuid}/Proxies": {"HTTPEnable": "0"},
    }, count=2)
    after = _ok(keys={
        f"Setup:/Network/Service/{uuid}": {"UserDefinedName": "My Ethernet"},
        f"Setup:/Network/Service/{uuid}/Proxies": {"HTTPEnable": "1"},
    }, count=2)
    result = events.diff("dynamic_store", before, after, _at())
    assert result[0]["subject"] == f"Setup:/Network/Service/My Ethernet({uuid[:8]})/Proxies.HTTPEnable"


# ---------------------------------------------------------------------------
# nwi：主网卡切换单独一种事件 kind
# ---------------------------------------------------------------------------


def _nwi_iface(name, address=None, vpn_server=None):
    return {"name": name, "flags": {"value": "0x5", "names": []}, "address": address,
            "vpn_server": vpn_server, "reach": None}


def test_nwi_order_changed_event():
    before = _ok(ipv4={"interfaces": [_nwi_iface("en0"), _nwi_iface("utun6")], "reach": None},
                 ipv6={"interfaces": [], "reach": None}, interfaces=["en0", "utun6"])
    after = _ok(ipv4={"interfaces": [_nwi_iface("utun6"), _nwi_iface("en0")], "reach": None},
                ipv6={"interfaces": [], "reach": None}, interfaces=["utun6", "en0"])
    result = events.diff("nwi", before, after, _at())
    kinds = {e["subject"]: e["kind"] for e in result}
    assert kinds["ipv4.order"] == "nwi_order_changed"


def test_nwi_field_change_event_kind():
    before = _ok(ipv4={"interfaces": [_nwi_iface("utun6", vpn_server="198.51.100.1")], "reach": None},
                 ipv6={"interfaces": [], "reach": None}, interfaces=["utun6"])
    after = _ok(ipv4={"interfaces": [_nwi_iface("utun6", vpn_server="198.51.100.9")], "reach": None},
                ipv6={"interfaces": [], "reach": None}, interfaces=["utun6"])
    result = events.diff("nwi", before, after, _at())
    assert result == [{"at": result[0]["at"], "kind": "nwi_changed", "subject": "ipv4.utun6.vpn_server",
                        "before": "198.51.100.1", "after": "198.51.100.9"}]


# ---------------------------------------------------------------------------
# network_services：服务顺序变化单独一种 kind
# ---------------------------------------------------------------------------


def test_network_service_order_changed_event():
    before = _ok(current_location="Automatic",
                 services=[{"name": "Wi-Fi", "enabled": True}],
                 order=[{"index": 1, "name": "Wi-Fi", "hardware_port": "Wi-Fi", "device": "en0"},
                        {"index": 2, "name": "Thunderbolt Bridge", "hardware_port": "TB", "device": "bridge0"}])
    after = _ok(current_location="Automatic",
                services=[{"name": "Wi-Fi", "enabled": True}],
                order=[{"index": 1, "name": "Thunderbolt Bridge", "hardware_port": "TB", "device": "bridge0"},
                       {"index": 2, "name": "Wi-Fi", "hardware_port": "Wi-Fi", "device": "en0"}])
    result = events.diff("network_services", before, after, _at())
    assert result == [{"at": result[0]["at"], "kind": "network_service_order_changed", "subject": "order",
                        "before": ["Wi-Fi", "Thunderbolt Bridge"], "after": ["Thunderbolt Bridge", "Wi-Fi"]}]


def test_network_service_enabled_changed_event():
    before = _ok(current_location="Automatic", services=[{"name": "Wi-Fi", "enabled": True}], order=[])
    after = _ok(current_location="Automatic", services=[{"name": "Wi-Fi", "enabled": False}], order=[])
    result = events.diff("network_services", before, after, _at())
    assert result[0]["kind"] == "network_service_changed"
    assert result[0]["subject"] == "Wi-Fi.enabled"


# ---------------------------------------------------------------------------
# dhcp：忽略租约时间，只在真实字段变化时才有事件
# ---------------------------------------------------------------------------


def test_dhcp_changed_event_ignores_lease_time():
    before = _ok(interfaces={"en0": {
        "error": None, "config_method": "DHCP", "router": "172.20.10.1", "domain_name": None,
        "domain_name_server": ["172.20.10.1"], "ssid": "redacted",
        "lease_start_time": "a", "lease_expiration_time": "b",
    }})
    after = _ok(interfaces={"en0": {
        "error": None, "config_method": "DHCP", "router": "172.20.10.254", "domain_name": None,
        "domain_name_server": ["172.20.10.1"], "ssid": "redacted",
        "lease_start_time": "c", "lease_expiration_time": "d",
    }})
    result = events.diff("dhcp", before, after, _at())
    assert result == [{"at": result[0]["at"], "kind": "dhcp_changed", "subject": "en0.router",
                        "before": "172.20.10.1", "after": "172.20.10.254"}]


def test_dhcp_interface_error_produces_no_event():
    before = _ok(interfaces={"en99": {"error": None, "config_method": "DHCP"}})
    after = _ok(interfaces={"en99": {"error": "interface doesn't exist"}})
    assert events.diff("dhcp", before, after, _at()) == []


# ---------------------------------------------------------------------------
# gateway_mac：换网络的强信号
# ---------------------------------------------------------------------------


def test_gateway_mac_changed_event():
    before = _ok(gateways={"172.20.10.1": {"error": None, "mac": "aa:bb:cc:dd:ee:ff", "interface": "en0"}})
    after = _ok(gateways={"172.20.10.1": {"error": None, "mac": "11:22:33:44:55:66", "interface": "en0"}})
    result = events.diff("gateway_mac", before, after, _at())
    assert result == [{"at": result[0]["at"], "kind": "gateway_mac_changed", "subject": "172.20.10.1",
                        "before": "aa:bb:cc:dd:ee:ff", "after": "11:22:33:44:55:66"}]


# ---------------------------------------------------------------------------
# hosts
# ---------------------------------------------------------------------------


def test_hosts_changed_event_on_new_entry():
    before = _ok(entries=[], sha256="h1", mtime=1.0)
    after = _ok(entries=[{"ip": "1.2.3.4", "hostnames": ["example.com"]}], sha256="h2", mtime=2.0)
    result = events.diff("hosts", before, after, _at())
    assert result == [{"at": result[0]["at"], "kind": "hosts_changed", "subject": "1.2.3.4",
                        "before": None, "after": ["example.com"]}]


# ---------------------------------------------------------------------------
# resolver
# ---------------------------------------------------------------------------


def test_resolver_changed_event():
    before = _ok(domains=[{"domain": "internal.example.com", "nameservers": ["10.0.0.1"],
                            "ports": [], "search_order": []}])
    after = _ok(domains=[{"domain": "internal.example.com", "nameservers": ["10.0.0.2"],
                           "ports": [], "search_order": []}])
    result = events.diff("resolver", before, after, _at())
    assert result[0]["kind"] == "resolver_changed"
    assert result[0]["subject"] == "internal.example.com.nameservers"


# ---------------------------------------------------------------------------
# sysctl_net
# ---------------------------------------------------------------------------


def test_sysctl_net_changed_event():
    before = _ok(values={"net.inet.ip.forwarding": {"error": None, "value": "0"}})
    after = _ok(values={"net.inet.ip.forwarding": {"error": None, "value": "1"}})
    result = events.diff("sysctl_net", before, after, _at())
    assert result == [{"at": result[0]["at"], "kind": "sysctl_net_changed", "subject": "net.inet.ip.forwarding",
                        "before": "0", "after": "1"}]


def test_sysctl_net_error_toggle_produces_no_event():
    before = _ok(values={"net.does.not.exist": {"error": None, "value": "0"}})
    after = _ok(values={"net.does.not.exist": {"error": "unknown oid"}})
    assert events.diff("sysctl_net", before, after, _at()) == []
