"""pybase.net.monitor.sysconfig 测试：`collect_dynamic_store`/`collect_nwi`/
`service_names` 的解析（真实 `scutil` 输出样例，脱敏后固化成夹具：MAC、公网
IP、BSSID、UUID 都换成假值），以及失败隔离、非 macOS 平台。
"""
from __future__ import annotations

import subprocess

import pytest

from pybase.net.monitor import sysconfig


def _fake_run(stdout, returncode=0):
    def run(cmd, *, timeout):
        return subprocess.CompletedProcess(cmd, returncode, stdout, "")

    return run


def _fake_scutil_script(list_stdout, show_stdout, *, list_returncode=0, show_returncode=0):
    """`_run_scutil_script` 会被调用两次（一次 list、一次批量 show），按 script
    内容（含不含 `show `）分流返回不同的夹具文本——跟真机行为一致：list 命令
    只吐 subKey 行，show 命令才吐 `<dictionary>`/`<array>` 结构。
    """

    def run(tool, script, timeout):
        if "show " in script:
            return subprocess.CompletedProcess([tool], show_returncode, show_stdout, "")
        return subprocess.CompletedProcess([tool], list_returncode, list_stdout, "")

    return run


# ---------------------------------------------------------------------------
# collect_dynamic_store：脱敏后的真机样例，覆盖 dictionary/array/data/嵌套/空值
# ---------------------------------------------------------------------------

_LIST_SAMPLE = """  subKey [0] = State:/Network/Global/IPv4
  subKey [1] = State:/Network/Global/Proxies
  subKey [2] = State:/Network/Interface/en0/AirPort
  subKey [3] = State:/Network/Interface/en0/IPv6
  subKey [4] = Setup:/Network/Service/AAAAAAAA-1111-2222-3333-444444444444
  subKey [5] = State:/Network/DoesNotExist
"""

# 顺序必须跟 _LIST_SAMPLE 里的 6 个键一一对应
_SHOW_SAMPLE = """<dictionary> {
  PrimaryInterface : utun6
  PrimaryService : AAAAAAAA-1111-2222-3333-444444444444
  Router : 192.0.2.1
}
<dictionary> {
  FTPPassive : 1
  __SCOPED__ : <dictionary> {
    en0 : <dictionary> {
      ExceptionsList : <array> {
      }
      HTTPEnable : 0
      HTTPProxy :
    }
  }
}
<dictionary> {
  BSSID : <data> 0xaabbccddeeff
  SSID_STR :
  CHANNEL : 149
}
<dictionary> {
  Addresses : <array> {
    0 : 2001:db8::1
    1 : fe80::1%en0
  }
  Flags : <array> {
    0 : 1024
    1 : 1088
  }
  AdditionalRoutes : <array> {
    0 : <dictionary> {
      DestinationAddress : 198.51.100.1
      SubnetMask : 255.255.255.255
    }
  }
}
<dictionary> {
  UserDefinedName : My Ethernet
}
  No such key
"""


def test_collect_dynamic_store_parses_nested_structures(monkeypatch):
    monkeypatch.setattr(sysconfig, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(sysconfig, "_run_scutil_script", _fake_scutil_script(_LIST_SAMPLE, _SHOW_SAMPLE))
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Darwin")

    result = sysconfig.collect_dynamic_store()
    assert result["error"] is None
    assert result["count"] == 6
    keys = result["keys"]

    # 简单字典
    assert keys["State:/Network/Global/IPv4"] == {
        "PrimaryInterface": "utun6",
        "PrimaryService": "AAAAAAAA-1111-2222-3333-444444444444",
        "Router": "192.0.2.1",
    }

    # 嵌套字典（__SCOPED__ 套 en0）+ 空数组 + 空字符串值
    proxies = keys["State:/Network/Global/Proxies"]
    scoped_en0 = proxies["__SCOPED__"]["en0"]
    assert scoped_en0["ExceptionsList"] == []
    assert scoped_en0["HTTPProxy"] == ""
    assert scoped_en0["HTTPEnable"] == "0"

    # <data> 保留为十六进制字符串，空字符串值
    airport = keys["State:/Network/Interface/en0/AirPort"]
    assert airport["BSSID"] == "0xaabbccddeeff"
    assert airport["SSID_STR"] == ""
    assert airport["CHANNEL"] == "149"

    # 数组套字典（AdditionalRoutes）、带 scope 的地址原样保留
    ipv6 = keys["State:/Network/Interface/en0/IPv6"]
    assert ipv6["Addresses"] == ["2001:db8::1", "fe80::1%en0"]
    assert ipv6["AdditionalRoutes"] == [{"DestinationAddress": "198.51.100.1", "SubnetMask": "255.255.255.255"}]

    # 键不存在 -> None，不是缺失/异常
    assert keys["State:/Network/DoesNotExist"] is None

    # 服务 UUID 键名原样保留（大小写、连字符不动）
    assert "Setup:/Network/Service/AAAAAAAA-1111-2222-3333-444444444444" in keys


def test_service_names_maps_uuid_to_user_defined_name(monkeypatch):
    monkeypatch.setattr(sysconfig, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(sysconfig, "_run_scutil_script", _fake_scutil_script(_LIST_SAMPLE, _SHOW_SAMPLE))
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Darwin")

    result = sysconfig.collect_dynamic_store()
    names = sysconfig.service_names(result["keys"])
    assert names == {"AAAAAAAA-1111-2222-3333-444444444444": "My Ethernet"}
    # 服务的子键（IPv4/DNS 等）不应该被当成服务本身
    assert "State:/Network/DoesNotExist" not in names


def test_collect_dynamic_store_no_matching_keys(monkeypatch):
    monkeypatch.setattr(sysconfig, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(sysconfig, "_run_scutil_script", _fake_scutil_script("", ""))
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Darwin")

    result = sysconfig.collect_dynamic_store()
    assert result == {"error": None, "keys": {}, "count": 0}


def test_collect_dynamic_store_tool_missing_returns_error_not_raise(monkeypatch):
    from pybase.media._proc import ToolNotFoundError

    def raise_not_found(explicit, name):
        raise ToolNotFoundError(f"找不到 {name}")

    monkeypatch.setattr(sysconfig, "resolve_tool", raise_not_found)
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Darwin")

    result = sysconfig.collect_dynamic_store()
    assert result["error"] is not None


def test_collect_dynamic_store_list_command_failure(monkeypatch):
    monkeypatch.setattr(sysconfig, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(sysconfig, "_run_scutil_script", _fake_scutil_script("boom", "", list_returncode=1))
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Darwin")

    result = sysconfig.collect_dynamic_store()
    assert result["error"] is not None
    assert "keys" not in result


def test_collect_dynamic_store_unsupported_platform(monkeypatch):
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Linux")
    assert sysconfig.collect_dynamic_store() == {"error": "unsupported platform"}


# ---------------------------------------------------------------------------
# collect_nwi：脱敏后的真机样例（含 VPN、双栈都有地址、reach 标志）
# ---------------------------------------------------------------------------

_NWI_SAMPLE = """Network information

IPv4 network interface information
   utun6 : flags      : 0x5 (IPv4,DNS)
           address    : 198.51.100.2
           VPN server : 198.51.100.1
           reach      : 0x00000003 (Reachable,Transient Connection)
     en0 : flags      : 0x7 (IPv4,IPv6,DNS)
           address    : 203.0.113.5
           reach      : 0x00000002 (Reachable)

   REACH : flags 0x00000003 (Reachable,Transient Connection)

IPv6 network interface information
     en0 : flags      : 0x2 (IPv6)
           address    : 2001:db8::5
           reach      : 0x00000002 (Reachable)

   REACH : flags 0x00000002 (Reachable)

Network interfaces: utun6 en0
"""

_NWI_EMPTY_SAMPLE = """Network information

IPv4 network interface information
   No IPv4 states found

IPv6 network interface information
   No IPv6 states found

Network interfaces:
"""


def test_collect_nwi_parses_dual_stack_and_vpn(monkeypatch):
    monkeypatch.setattr(sysconfig, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(sysconfig, "run", _fake_run(_NWI_SAMPLE))
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Darwin")

    result = sysconfig.collect_nwi()
    assert result["error"] is None
    assert result["interfaces"] == ["utun6", "en0"]

    ipv4 = result["ipv4"]
    assert [i["name"] for i in ipv4["interfaces"]] == ["utun6", "en0"]
    utun6 = ipv4["interfaces"][0]
    assert utun6["flags"] == {"value": "0x5", "names": ["IPv4", "DNS"]}
    assert utun6["address"] == "198.51.100.2"
    assert utun6["vpn_server"] == "198.51.100.1"
    assert utun6["reach"] == {"value": "0x00000003", "names": ["Reachable", "Transient Connection"]}
    en0_v4 = ipv4["interfaces"][1]
    assert en0_v4["vpn_server"] is None
    assert ipv4["reach"] == {"value": "0x00000003", "names": ["Reachable", "Transient Connection"]}

    ipv6 = result["ipv6"]
    assert ipv6["interfaces"][0]["address"] == "2001:db8::5"
    assert ipv6["reach"] == {"value": "0x00000002", "names": ["Reachable"]}


def test_collect_nwi_no_states_found(monkeypatch):
    monkeypatch.setattr(sysconfig, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(sysconfig, "run", _fake_run(_NWI_EMPTY_SAMPLE))
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Darwin")

    result = sysconfig.collect_nwi()
    assert result["error"] is None
    assert result["ipv4"] == {"interfaces": [], "reach": None}
    assert result["ipv6"] == {"interfaces": [], "reach": None}
    assert result["interfaces"] == []


def test_collect_nwi_command_failure_returns_error(monkeypatch):
    monkeypatch.setattr(sysconfig, "resolve_tool", lambda explicit, name: "/usr/sbin/scutil")
    monkeypatch.setattr(sysconfig, "run", _fake_run("boom", returncode=1))
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Darwin")

    result = sysconfig.collect_nwi()
    assert result["error"] is not None


def test_collect_nwi_unsupported_platform(monkeypatch):
    monkeypatch.setattr(sysconfig.platform, "system", lambda: "Linux")
    assert sysconfig.collect_nwi() == {"error": "unsupported platform"}
