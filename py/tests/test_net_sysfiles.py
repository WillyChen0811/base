"""pybase.net.monitor.sysfiles 测试：networksetup/ipconfig/arp/ndp 的输出解析
（真实命令样例脱敏后固化成夹具）、/etc/hosts、/etc/resolver/*、sysctl，
覆盖正常解析、字段缺失、命令失败、文件不存在、非 macOS 平台。
"""
from __future__ import annotations

import subprocess

import pytest

from pybase.net.monitor import sysfiles as sf
from pybase.media._proc import ToolNotFoundError


def _fake_run(stdout="", returncode=0, stderr=""):
    def run(cmd, *, timeout):
        return subprocess.CompletedProcess(cmd, returncode, stdout, stderr)

    return run


def _fake_run_by_arg(mapping: dict, default=None):
    """按命令行里出现的某个关键 token 分派不同的返回值（用于一个采集器里跑多条命令的场景）。"""

    def run(cmd, *, timeout):
        for token, result in mapping.items():
            if token in cmd:
                return subprocess.CompletedProcess(cmd, *result)
        if default is not None:
            return subprocess.CompletedProcess(cmd, *default)
        raise AssertionError(f"未预料的命令：{cmd}")

    return run


# ---------------------------------------------------------------------------
# collect_network_services
# ---------------------------------------------------------------------------

_LOCATION_SAMPLE = "Automatic\n"

_SERVICES_SAMPLE = """An asterisk (*) denotes that a network service is disabled.
Wi-Fi
Thunderbolt Bridge
*iPhone USB
MyWireGuardTunnel
"""

_ORDER_SAMPLE = """An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)

(3) MyWireGuardTunnel
(Hardware Port: com.wireguard.macos, Device: )

"""


def test_collect_network_services_parsing(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/networksetup")
    monkeypatch.setattr(sf.platform, "system", lambda: "Darwin")
    monkeypatch.setattr(
        sf,
        "run",
        _fake_run_by_arg(
            {
                "-getcurrentlocation": (0, _LOCATION_SAMPLE, ""),
                "-listallnetworkservices": (0, _SERVICES_SAMPLE, ""),
                "-listnetworkserviceorder": (0, _ORDER_SAMPLE, ""),
            }
        ),
    )
    result = sf.collect_network_services()
    assert result["error"] is None
    assert result["current_location"] == "Automatic"
    assert result["services"] == [
        {"name": "Wi-Fi", "enabled": True},
        {"name": "Thunderbolt Bridge", "enabled": True},
        {"name": "iPhone USB", "enabled": False},
        {"name": "MyWireGuardTunnel", "enabled": True},
    ]
    assert result["order"] == [
        {"index": 1, "name": "Wi-Fi", "hardware_port": "Wi-Fi", "device": "en0"},
        {"index": 2, "name": "Thunderbolt Bridge", "hardware_port": "Thunderbolt Bridge", "device": "bridge0"},
        {"index": 3, "name": "MyWireGuardTunnel", "hardware_port": "com.wireguard.macos", "device": None},
    ]


def test_collect_network_services_command_failure(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/networksetup")
    monkeypatch.setattr(sf.platform, "system", lambda: "Darwin")
    monkeypatch.setattr(
        sf,
        "run",
        _fake_run_by_arg(
            {
                "-getcurrentlocation": (1, "", "boom"),
            },
            default=(0, "", ""),
        ),
    )
    result = sf.collect_network_services()
    assert result["error"] is not None


def test_collect_network_services_non_macos(monkeypatch):
    monkeypatch.setattr(sf.platform, "system", lambda: "Linux")
    result = sf.collect_network_services()
    assert result["error"] is not None


# ---------------------------------------------------------------------------
# collect_dhcp
# ---------------------------------------------------------------------------

# 结构照真实 `ipconfig getsummary en0` 输出（macOS 26），MAC/SSID/BSSID 打码，
# IP 换成示例网段；DHCP 转储里的花括号（如 domain_name_server）原样保留，
# 这正是不能用花括号计数做通用解析的原因。
_IPCONFIG_SUMMARY_WIFI_DHCP = """<dictionary> {
  BSSID : <redacted>
  ConnectionID : 15
  IPv4 : <array> {
    0 : <dictionary> {
      Addresses : <array> {
        0 : 172.20.10.3
      }
      ChildServiceID : LINKLOCAL-en0
      ConfigMethod : DHCP
      DHCP : <dictionary> {
        LeaseExpirationTime : 09/28/2026 16:28:49
        LeaseStartTime : 09/28/2026 15:28:49
        Packet : op = BOOTREPLY
htype = 1
chaddr = aa:bb:cc:dd:ee:ff
options:
Options count is 3
dhcp_message_type (uint8): ACK 0x5
domain_name_server (ip_mult): {172.20.10.1}
end (none):

        State : BOUND
      }
      IsPublished : TRUE
      Router : 172.20.10.1
      RouterARPVerified : TRUE
      SubnetMasks : <array> {
        0 : 255.255.255.240
      }
    }
  }
  InterfaceType : WiFi
  IsExpensive : TRUE
  LinkStatusActive : TRUE
  NetworkID : <redacted>
  SSID : <redacted>
  Security : WPA2_PSK
}
"""

_IPCONFIG_SUMMARY_BRIDGE_INACTIVE = """<dictionary> {
  Active : FALSE
  IPv4 : <array> {
    0 : <dictionary> {
      ConfigMethod : Manual
      IsPublished : FALSE
      LastFailureStatus : media inactive
      ManualAddress : 192.168.25.2
      ManualSubnetMask : 255.255.255.0
    }
  }
  InterfaceType : Bridge
}
"""


def test_collect_dhcp_wifi_parsing(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/ipconfig")
    monkeypatch.setattr(sf, "run", _fake_run(_IPCONFIG_SUMMARY_WIFI_DHCP))
    monkeypatch.setattr(sf.platform, "system", lambda: "Darwin")
    result = sf.collect_dhcp(["en0"])
    assert result["error"] is None
    en0 = result["interfaces"]["en0"]
    assert en0["error"] is None
    assert en0["config_method"] == "DHCP"
    assert en0["router"] == "172.20.10.1"
    assert en0["router_arp_verified"] is True
    assert en0["lease_start_time"] == "09/28/2026 15:28:49"
    assert en0["lease_expiration_time"] == "09/28/2026 16:28:49"
    assert en0["link_status_active"] is True
    assert en0["domain_name"] is None
    assert en0["domain_name_server"] == ["172.20.10.1"]
    # 系统隐藏定位相关信息时如实记为 redacted，不是拿不到
    assert en0["ssid"] == "redacted"
    assert en0["bssid"] == "redacted"


def test_collect_dhcp_inactive_bridge_missing_fields(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/ipconfig")
    monkeypatch.setattr(sf, "run", _fake_run(_IPCONFIG_SUMMARY_BRIDGE_INACTIVE))
    monkeypatch.setattr(sf.platform, "system", lambda: "Darwin")
    result = sf.collect_dhcp(["bridge0"])
    bridge0 = result["interfaces"]["bridge0"]
    assert bridge0["error"] is None
    assert bridge0["config_method"] == "Manual"
    assert bridge0["active"] is False
    assert bridge0["router"] is None
    assert bridge0["domain_name_server"] == []


def test_collect_dhcp_interface_not_found(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/ipconfig")
    monkeypatch.setattr(
        sf, "run", _fake_run("", returncode=1, stderr="ipconfig_get_summary(en99) failed: interface doesn't exist")
    )
    monkeypatch.setattr(sf.platform, "system", lambda: "Darwin")
    result = sf.collect_dhcp(["en99"])
    assert result["error"] is None  # 整体采集器不因单个接口失败而失败
    assert result["interfaces"]["en99"]["error"] is not None


def test_collect_dhcp_non_macos(monkeypatch):
    monkeypatch.setattr(sf.platform, "system", lambda: "Linux")
    result = sf.collect_dhcp(["eth0"])
    assert result["interfaces"]["eth0"]["error"] is not None


# ---------------------------------------------------------------------------
# collect_gateway_mac
# ---------------------------------------------------------------------------

_ARP_HIT_SAMPLE = "? (172.20.10.1) at aa:bb:cc:dd:ee:ff on en0 ifscope [ethernet]\n"
_ARP_MISS_SAMPLE = "10.255.255.1 (10.255.255.1) -- no entry\n"

_NDP_SAMPLE = """Neighbor                                Linklayer Address  Netif Expire    St Flgs Prbs
fe80::1%lo0                             (incomplete)         lo0 permanent R
fe80::905f:7aff:fec3:eb64%en0           aa:bb:cc:dd:ee:ff    en0 15s       R  R
"""


def test_collect_gateway_mac_arp_hit(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/arp")
    monkeypatch.setattr(sf, "run", _fake_run(_ARP_HIT_SAMPLE))
    result = sf.collect_gateway_mac(["172.20.10.1"])
    entry = result["gateways"]["172.20.10.1"]
    assert entry == {"error": None, "mac": "aa:bb:cc:dd:ee:ff", "interface": "en0"}


def test_collect_gateway_mac_arp_miss(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/arp")
    monkeypatch.setattr(sf, "run", _fake_run(_ARP_MISS_SAMPLE, returncode=1, stderr=""))
    result = sf.collect_gateway_mac(["10.255.255.1"])
    entry = result["gateways"]["10.255.255.1"]
    assert entry["error"] is not None
    assert "no entry" in entry["error"]


def test_collect_gateway_mac_ndp_hit_strips_scope(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/ndp")
    monkeypatch.setattr(sf, "run", _fake_run(_NDP_SAMPLE))
    result = sf.collect_gateway_mac(["fe80::905f:7aff:fec3:eb64%en0"])
    entry = result["gateways"]["fe80::905f:7aff:fec3:eb64%en0"]
    assert entry == {"error": None, "mac": "aa:bb:cc:dd:ee:ff", "interface": "en0"}


def test_collect_gateway_mac_ndp_incomplete_is_miss(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/ndp")
    monkeypatch.setattr(sf, "run", _fake_run(_NDP_SAMPLE))
    result = sf.collect_gateway_mac(["fe80::1%lo0"])
    assert result["gateways"]["fe80::1%lo0"]["error"] is not None


def test_collect_gateway_mac_tool_not_found(monkeypatch):
    def raise_not_found(explicit, name):
        raise ToolNotFoundError(f"找不到 {name}")

    monkeypatch.setattr(sf, "resolve_tool", raise_not_found)
    result = sf.collect_gateway_mac(["172.20.10.1"])
    assert result["gateways"]["172.20.10.1"]["error"] is not None


# ---------------------------------------------------------------------------
# collect_hosts
# ---------------------------------------------------------------------------

_HOSTS_SAMPLE = """127.0.0.1 view-localhost
##
# Host Database
##
127.0.0.1\tlocalhost
255.255.255.255\tbroadcasthost
::1             localhost

# 注释行整行忽略
127.0.0.1 example.internal alias.internal
"""


def test_collect_hosts_parsing(tmp_path):
    hosts_file = tmp_path / "hosts"
    hosts_file.write_text(_HOSTS_SAMPLE, encoding="utf-8")
    result = sf.collect_hosts(str(hosts_file))
    assert result["error"] is None
    assert {"ip": "127.0.0.1", "hostnames": ["view-localhost"]} in result["entries"]
    assert {"ip": "127.0.0.1", "hostnames": ["example.internal", "alias.internal"]} in result["entries"]
    assert len(result["sha256"]) == 64
    assert isinstance(result["mtime"], float)


def test_collect_hosts_missing_file(tmp_path):
    result = sf.collect_hosts(str(tmp_path / "does-not-exist"))
    assert result["error"] is not None


# ---------------------------------------------------------------------------
# collect_resolver_dir
# ---------------------------------------------------------------------------


def test_collect_resolver_dir_parsing(tmp_path):
    resolver_dir = tmp_path / "resolver"
    resolver_dir.mkdir()
    (resolver_dir / "internal.example.com").write_text(
        "nameserver 10.0.0.1\nnameserver 10.0.0.2\nport 5353\nsearch_order 1\n", encoding="utf-8"
    )
    result = sf.collect_resolver_dir(str(resolver_dir))
    assert result["error"] is None
    assert len(result["domains"]) == 1
    entry = result["domains"][0]
    assert entry["domain"] == "internal.example.com"
    assert entry["nameservers"] == ["10.0.0.1", "10.0.0.2"]
    assert entry["ports"] == ["5353"]
    assert entry["search_order"] == ["1"]
    assert isinstance(entry["mtime"], float)


def test_collect_resolver_dir_missing_is_empty_not_error(tmp_path):
    result = sf.collect_resolver_dir(str(tmp_path / "no-such-dir"))
    assert result == {"error": None, "domains": []}


def test_collect_resolver_dir_path_is_a_file(tmp_path):
    f = tmp_path / "not-a-dir"
    f.write_text("x", encoding="utf-8")
    result = sf.collect_resolver_dir(str(f))
    assert result["error"] is not None


# ---------------------------------------------------------------------------
# collect_sysctl_net
# ---------------------------------------------------------------------------


def test_collect_sysctl_net_parsing(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/sysctl")
    monkeypatch.setattr(
        sf,
        "run",
        _fake_run_by_arg(
            {
                "net.inet.ip.forwarding": (0, "0\n", ""),
                "net.inet6.ip6.forwarding": (0, "0\n", ""),
                "net.inet.tcp.mssdflt": (0, "512\n", ""),
            }
        ),
    )
    result = sf.collect_sysctl_net()
    assert result["error"] is None
    assert result["values"]["net.inet.ip.forwarding"] == {"error": None, "value": "0"}
    assert result["values"]["net.inet.tcp.mssdflt"] == {"error": None, "value": "512"}


def test_collect_sysctl_net_unknown_name(monkeypatch):
    monkeypatch.setattr(sf, "resolve_tool", lambda explicit, name: "/usr/sbin/sysctl")
    monkeypatch.setattr(sf, "run", _fake_run("", returncode=1, stderr="unknown oid"))
    result = sf.collect_sysctl_net(["net.does.not.exist"])
    assert result["error"] is None
    assert result["values"]["net.does.not.exist"]["error"] is not None


def test_collect_sysctl_net_tool_not_found(monkeypatch):
    def raise_not_found(explicit, name):
        raise ToolNotFoundError("找不到 sysctl")

    monkeypatch.setattr(sf, "resolve_tool", raise_not_found)
    result = sf.collect_sysctl_net()
    assert result["error"] is not None
