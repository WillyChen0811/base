"""两份网络配置快照的差异对比。

`before`/`after` 是同一份快照结构：键为采集器名（`interfaces`/`routes`/`dns`/
`proxies`/`vpn_services`/`netbird`/`mihomo`/`owners`/`scenario`/`dynamic_store`/
`nwi`/`network_services`/`dhcp`/`gateway_mac`/`hosts`/`resolver`/`sysctl_net`
等），值为该采集器的返回信封（含 `error` 字段，见 `collectors.py`/`sysconfig.py`/
`sysfiles.py`）。

跟 `events.py` 的思路一样——按采集器分段、语义化比较而不是整段 JSON 比——两边
共用同一套 `_diff_*` 比较函数（`events.py` 从这里 import 复用，避免两份规则），
区别只在外层怎么用比较结果：
`events.py` 是给 `Recorder` 用的"发现变化就报事件"（不认识的采集器直接跳过、
两边有一个 error 就整段跳过），这里是给人看的"两个场景快照对比"，需要更完整：
- 一段在某一方缺失/带 error 时不是静默跳过，而是显式给一条 `unavailable`，
  让用户知道"这段没法比"而不是误以为"没变化"。
- 不认识的采集器名字（调用方自定义的段）也不处理，跟 `events.py` 一致。

纯函数，不写库、不发通知，编排留给调用方（CLI/网页展示）。
"""
from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Callable, Optional

from . import sysconfig as _sysconfig


@dataclass(frozen=True)
class Change:
    section: str  # 采集器名，如 "interfaces"
    kind: str  # added / removed / changed / unavailable
    subject: str  # 具体项，如接口名、"en0.mtu"、"global"
    before: object
    after: object


def _c(section: str, kind: str, subject: str, before, after) -> Change:
    return Change(section, kind, subject, before, after)


# ---------------------------------------------------------------------------
# interfaces：增删、状态、IPv4/IPv6 地址、MTU
# ---------------------------------------------------------------------------


def _diff_interfaces(section: str, before: dict, after: dict) -> list[Change]:
    b_map = {i["name"]: i for i in before.get("interfaces", []) if i.get("name")}
    a_map = {i["name"]: i for i in after.get("interfaces", []) if i.get("name")}
    changes: list[Change] = []
    for name in sorted(set(b_map) | set(a_map)):
        b, a = b_map.get(name), a_map.get(name)
        if b is None:
            changes.append(_c(section, "added", name, None, a))
            continue
        if a is None:
            changes.append(_c(section, "removed", name, b, None))
            continue
        if b.get("status") != a.get("status"):
            changes.append(_c(section, "changed", f"{name}.status", b.get("status"), a.get("status")))
        if b.get("mtu") != a.get("mtu"):
            changes.append(_c(section, "changed", f"{name}.mtu", b.get("mtu"), a.get("mtu")))
        b4 = {x["address"] for x in b.get("ipv4", []) if x.get("address")}
        a4 = {x["address"] for x in a.get("ipv4", []) if x.get("address")}
        if b4 != a4:
            changes.append(_c(section, "changed", f"{name}.ipv4", sorted(b4), sorted(a4)))
        b6 = {x["address"] for x in b.get("ipv6", []) if x.get("address")}
        a6 = {x["address"] for x in a.get("ipv6", []) if x.get("address")}
        if b6 != a6:
            changes.append(_c(section, "changed", f"{name}.ipv6", sorted(b6), sorted(a6)))
    return changes


# ---------------------------------------------------------------------------
# routes：按 (family, destination) 比较增删/网关/接口变化。
#
# 忽略噪声：
# - `expire` 字段（macOS netstat 给的路由过期倒计时，跟配置无关，不参与比较——
#   做法是压根不取这个字段）。
# - 带 `%` 的链路本地 IPv6 目的地（如 `fe80::%en0/64`）：这是内核给每个接口自动
#   生成的 on-link 条目，接口来去或睡眠唤醒都会瞬时增删/变化，不是用户改了配置，
#   比较前整体过滤掉。
# ---------------------------------------------------------------------------


def _is_noise_route(entry: dict) -> bool:
    destination = str(entry.get("destination") or "")
    return "%" in destination and destination.split("%", 1)[0].lower() in ("fe80::", "")


def _route_map(rows: list[dict]) -> dict:
    return {
        str(r.get("destination")): r
        for r in rows
        if r.get("destination") is not None and not _is_noise_route(r)
    }


def _diff_routes(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    for family in ("ipv4", "ipv6"):
        b_map = _route_map(before.get(family, []))
        a_map = _route_map(after.get(family, []))
        for dest in sorted(set(b_map) | set(a_map)):
            b, a = b_map.get(dest), a_map.get(dest)
            subject = f"{family}:{dest}"
            if b is None:
                changes.append(_c(section, "added", subject, None, a))
                continue
            if a is None:
                changes.append(_c(section, "removed", subject, b, None))
                continue
            b_key = (b.get("gateway"), b.get("interface"))
            a_key = (a.get("gateway"), a.get("interface"))
            if b_key != a_key:
                changes.append(_c(section, "changed", subject, b_key, a_key))
    return changes


# ---------------------------------------------------------------------------
# dns：全局 nameserver、search domain、按接口 resolver
# ---------------------------------------------------------------------------


def _resolver_key(r: dict) -> str:
    return r.get("if_name") or (str(r.get("if_index")) if r.get("if_index") is not None else "?")


def _diff_dns(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []

    def split(payload: dict) -> tuple[list[str], list[str], dict[str, dict]]:
        global_ns: list[str] = []
        search: list[str] = []
        scoped: dict[str, dict] = {}
        for r in payload.get("resolvers", []):
            if r.get("scoped"):
                scoped[_resolver_key(r)] = r
            else:
                global_ns.extend(r.get("nameservers", []))
                search.extend(r.get("search", []))
        return sorted(set(global_ns)), sorted(set(search)), scoped

    b_ns, b_search, b_scoped = split(before)
    a_ns, a_search, a_scoped = split(after)
    if b_ns != a_ns:
        changes.append(_c(section, "changed", "global.nameservers", b_ns, a_ns))
    if b_search != a_search:
        changes.append(_c(section, "changed", "global.search", b_search, a_search))
    for key in sorted(set(b_scoped) | set(a_scoped)):
        b, a = b_scoped.get(key), a_scoped.get(key)
        if b is None:
            changes.append(_c(section, "added", f"interface:{key}", None, a.get("nameservers")))
            continue
        if a is None:
            changes.append(_c(section, "removed", f"interface:{key}", b.get("nameservers"), None))
            continue
        if b.get("nameservers") != a.get("nameservers"):
            changes.append(
                _c(section, "changed", f"interface:{key}", b.get("nameservers"), a.get("nameservers"))
            )
    return changes


# ---------------------------------------------------------------------------
# proxies：系统代理各项、ExceptionsList 增删、环境变量代理
# ---------------------------------------------------------------------------


def _diff_proxies(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    b_sys, a_sys = before.get("system") or {}, after.get("system") or {}
    for key in sorted(set(b_sys) | set(a_sys)):
        if key == "ExceptionsList":
            continue
        b_val, a_val = b_sys.get(key), a_sys.get(key)
        if b_val != a_val:
            changes.append(_c(section, "changed", f"system.{key}", b_val, a_val))
    b_exc = set(b_sys.get("ExceptionsList") or [])
    a_exc = set(a_sys.get("ExceptionsList") or [])
    for item in sorted(b_exc - a_exc):
        changes.append(_c(section, "removed", "system.ExceptionsList", item, None))
    for item in sorted(a_exc - b_exc):
        changes.append(_c(section, "added", "system.ExceptionsList", None, item))

    b_env, a_env = before.get("env") or {}, after.get("env") or {}
    for key in sorted(set(b_env) | set(a_env)):
        b_val, a_val = b_env.get(key), a_env.get(key)
        if b_val != a_val:
            changes.append(_c(section, "changed", f"env.{key}", b_val, a_val))
    return changes


# ---------------------------------------------------------------------------
# vpn_services：按服务名比较增删/状态
# ---------------------------------------------------------------------------


def _service_key(s: dict) -> str:
    return s.get("name") or s.get("uuid") or ""


def _diff_vpn_services(section: str, before: dict, after: dict) -> list[Change]:
    b_map = {_service_key(s): s for s in before.get("services", [])}
    a_map = {_service_key(s): s for s in after.get("services", [])}
    changes: list[Change] = []
    for name in sorted(set(b_map) | set(a_map)):
        b, a = b_map.get(name), a_map.get(name)
        if b is None:
            changes.append(_c(section, "added", name, None, a.get("status")))
            continue
        if a is None:
            changes.append(_c(section, "removed", name, b.get("status"), None))
            continue
        if b.get("status") != a.get("status") or b.get("enabled") != a.get("enabled"):
            changes.append(
                _c(
                    section, "changed", name,
                    {"status": b.get("status"), "enabled": b.get("enabled")},
                    {"status": a.get("status"), "enabled": a.get("enabled")},
                )
            )
    return changes


# ---------------------------------------------------------------------------
# netbird：连接状态、netbird IP、peer 数
# ---------------------------------------------------------------------------


def _diff_netbird(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    if bool(before.get("available")) != bool(after.get("available")):
        changes.append(_c(section, "changed", "available", before.get("available"), after.get("available")))
    if not before.get("available") or not after.get("available"):
        return changes
    for field, subject in (
        ("connected", "connected"),
        ("daemon_status", "daemon_status"),
        ("netbird_ip", "netbird_ip"),
        ("peers_total", "peers_total"),
        ("peers_connected", "peers_connected"),
    ):
        if before.get(field) != after.get(field):
            changes.append(_c(section, "changed", subject, before.get(field), after.get(field)))
    return changes


# ---------------------------------------------------------------------------
# mihomo：mode、tun 开关/设备名、版本——不比连接数/流量（每时每刻都在变）
# ---------------------------------------------------------------------------


def _diff_mihomo(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    for field in ("mode", "tun_enabled", "tun_device", "version"):
        if before.get(field) != after.get(field):
            changes.append(_c(section, "changed", field, before.get(field), after.get(field)))
    return changes


# ---------------------------------------------------------------------------
# owners：接口归属变化（`evidence` 只是诊断文字，不参与比较，避免同一归属
# 因为措辞/证据细节不同就被判成"变了"）
# ---------------------------------------------------------------------------


def _diff_owners(section: str, before: dict, after: dict) -> list[Change]:
    b_map = before.get("owners", {})
    a_map = after.get("owners", {})
    changes: list[Change] = []
    for name in sorted(set(b_map) | set(a_map)):
        b, a = b_map.get(name), a_map.get(name)
        if b is None:
            changes.append(_c(section, "added", name, None, (a.get("owner_kind"), a.get("owner_name"))))
            continue
        if a is None:
            changes.append(_c(section, "removed", name, (b.get("owner_kind"), b.get("owner_name")), None))
            continue
        b_key = (b.get("owner_kind"), b.get("owner_name"))
        a_key = (a.get("owner_kind"), a.get("owner_name"))
        if b_key != a_key:
            changes.append(_c(section, "changed", name, b_key, a_key))
    return changes


# ---------------------------------------------------------------------------
# scenario：key/label 变化
# ---------------------------------------------------------------------------


def _diff_scenario(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    if before.get("key") != after.get("key"):
        changes.append(_c(section, "changed", "key", before.get("key"), after.get("key")))
    if before.get("label") != after.get("label"):
        changes.append(_c(section, "changed", "label", before.get("label"), after.get("label")))
    return changes


# ---------------------------------------------------------------------------
# dynamic_store：SCDynamicStore 全量键值（sysconfig.collect_dynamic_store）。
# 用户最关心"系统网络配置本身"变没变，这段信息量最大也最细，噪声也最多。
#
# 忽略噪声——依据：本机实测跑两次 collect_dynamic_store()（间隔数秒）对比 +
# 已知字段语义：
# - 键名以 `/LinkQuality` 结尾：`{"LinkQuality": "100"}` 这类瞬时链路质量分数
#   （Wi-Fi 断开时是 "-2"），几秒内会自己漂，跟配置无关，整条键忽略。
# - 字段名匹配 `AutoJoinTimestamp`/`CachedScanRecord`（AirPort 自动加入时间戳、
#   Wi-Fi 扫描缓存二进制块）/`LeaseStartTime`/`LeaseExpirationTime`（DHCP/DHCPv6
#   租约起止时间，持续倒计时）/`PLATDiscoveryStartTime`/`PLATDiscoveryCompletionTime`
#   （NAT64 发现耗时）：这些字段值随时间流逝自然变化，不代表配置被改。
# ---------------------------------------------------------------------------

_NOISE_DYNAMIC_STORE_KEY_RE = re.compile(r"/LinkQuality$")
_NOISE_DYNAMIC_STORE_FIELD_RE = re.compile(
    r"(AutoJoinTimestamp|CachedScanRecord|LeaseStartTime|LeaseExpirationTime|"
    r"PLATDiscoveryStartTime|PLATDiscoveryCompletionTime)$"
)
_SERVICE_UUID_IN_KEY_RE = re.compile(r"Service/(?P<uuid>[0-9A-Fa-f]{8}-[0-9A-Fa-f-]+)")


def _translate_service_uuid(key: str, names: dict) -> str:
    """把 key 里的服务 UUID 段替换成"服务名(uuid前8位)"，不认识的 UUID 原样保留。"""
    m = _SERVICE_UUID_IN_KEY_RE.search(key)
    if not m:
        return key
    name = names.get(m.group("uuid"))
    if not name:
        return key
    return key[:m.start("uuid")] + f"{name}({m.group('uuid')[:8]})" + key[m.end("uuid"):]


def _diff_dynamic_store(section: str, before: dict, after: dict) -> list[Change]:
    b_keys = before.get("keys") or {}
    a_keys = after.get("keys") or {}
    names: dict = {}
    names.update(_sysconfig.service_names(b_keys))
    names.update(_sysconfig.service_names(a_keys))
    changes: list[Change] = []
    for key in sorted(set(b_keys) | set(a_keys)):
        if _NOISE_DYNAMIC_STORE_KEY_RE.search(key):
            continue
        subject = _translate_service_uuid(key, names)
        if key not in b_keys:
            changes.append(_c(section, "added", subject, None, a_keys[key]))
            continue
        if key not in a_keys:
            changes.append(_c(section, "removed", subject, b_keys[key], None))
            continue
        b, a = b_keys[key], a_keys[key]
        if isinstance(b, dict) and isinstance(a, dict):
            for field in sorted(set(b) | set(a)):
                if _NOISE_DYNAMIC_STORE_FIELD_RE.search(field):
                    continue
                bv, av = b.get(field), a.get(field)
                if bv != av:
                    changes.append(_c(section, "changed", f"{subject}.{field}", bv, av))
            continue
        if b != a:
            changes.append(_c(section, "changed", subject, b, a))
    return changes


# ---------------------------------------------------------------------------
# nwi：scutil --nwi 摘要（sysconfig.collect_nwi）。接口顺序即系统采用顺序，
# 顺序变化（如主网卡从 en0 换成 utun6）比单纯的地址变化更能说明"切网络了"。
# ---------------------------------------------------------------------------


def _diff_nwi(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    for family in ("ipv4", "ipv6"):
        b_fam = before.get(family) or {}
        a_fam = after.get(family) or {}
        b_ifaces = b_fam.get("interfaces") or []
        a_ifaces = a_fam.get("interfaces") or []
        b_map = {i.get("name"): i for i in b_ifaces}
        a_map = {i.get("name"): i for i in a_ifaces}
        for name in sorted(set(b_map) | set(a_map)):
            b, a = b_map.get(name), a_map.get(name)
            subject = f"{family}.{name}"
            if b is None:
                changes.append(_c(section, "added", subject, None, a))
                continue
            if a is None:
                changes.append(_c(section, "removed", subject, b, None))
                continue
            for field in ("flags", "address", "vpn_server"):
                if b.get(field) != a.get(field):
                    changes.append(_c(section, "changed", f"{subject}.{field}", b.get(field), a.get(field)))
        b_names = [i.get("name") for i in b_ifaces]
        a_names = [i.get("name") for i in a_ifaces]
        if set(b_names) == set(a_names) and b_names != a_names:
            changes.append(_c(section, "changed", f"{family}.order", b_names, a_names))
    return changes


# ---------------------------------------------------------------------------
# network_services：networksetup 服务清单（sysfiles.collect_network_services）。
# ---------------------------------------------------------------------------


def _diff_network_services(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    if before.get("current_location") != after.get("current_location"):
        changes.append(_c(section, "changed", "current_location",
                           before.get("current_location"), after.get("current_location")))
    b_map = {s.get("name"): s for s in before.get("services") or []}
    a_map = {s.get("name"): s for s in after.get("services") or []}
    for name in sorted(set(b_map) | set(a_map)):
        b, a = b_map.get(name), a_map.get(name)
        if b is None:
            changes.append(_c(section, "added", name, None, a.get("enabled")))
            continue
        if a is None:
            changes.append(_c(section, "removed", name, b.get("enabled"), None))
            continue
        if b.get("enabled") != a.get("enabled"):
            changes.append(_c(section, "changed", f"{name}.enabled", b.get("enabled"), a.get("enabled")))
    b_order = [o.get("name") for o in before.get("order") or []]
    a_order = [o.get("name") for o in after.get("order") or []]
    if b_order != a_order:
        changes.append(_c(section, "changed", "order", b_order, a_order))
    return changes


# ---------------------------------------------------------------------------
# dhcp：按接口比较（sysfiles.collect_dhcp）。不比 lease_start_time/
# lease_expiration_time——租约剩余时间本来就会一直倒计时。
# ---------------------------------------------------------------------------

_DHCP_COMPARE_FIELDS = ("config_method", "router", "domain_name", "domain_name_server", "ssid")


def _diff_dhcp(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    b_ifaces = before.get("interfaces") or {}
    a_ifaces = after.get("interfaces") or {}
    for name in sorted(set(b_ifaces) | set(a_ifaces)):
        b, a = b_ifaces.get(name), a_ifaces.get(name)
        if b is None:
            changes.append(_c(section, "added", name, None, a))
            continue
        if a is None:
            changes.append(_c(section, "removed", name, b, None))
            continue
        if b.get("error") or a.get("error"):
            changes.append(_c(section, "unavailable", name, b.get("error"), a.get("error")))
            continue
        for field in _DHCP_COMPARE_FIELDS:
            if b.get(field) != a.get(field):
                changes.append(_c(section, "changed", f"{name}.{field}", b.get(field), a.get(field)))
    return changes


# ---------------------------------------------------------------------------
# gateway_mac：网关 IP -> MAC（sysfiles.collect_gateway_mac）。IP 没变但 MAC
# 变了，往往是换了实际网络（同网段的不同路由器/热点）。
# ---------------------------------------------------------------------------


def _diff_gateway_mac(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    b_gw = before.get("gateways") or {}
    a_gw = after.get("gateways") or {}
    for ip in sorted(set(b_gw) | set(a_gw)):
        b, a = b_gw.get(ip), a_gw.get(ip)
        if b is None:
            changes.append(_c(section, "added", ip, None, a))
            continue
        if a is None:
            changes.append(_c(section, "removed", ip, b, None))
            continue
        if b.get("error") or a.get("error"):
            if bool(b.get("error")) != bool(a.get("error")):
                changes.append(_c(section, "unavailable", ip, b.get("error"), a.get("error")))
            continue
        if b.get("mac") != a.get("mac"):
            changes.append(_c(section, "changed", ip, b.get("mac"), a.get("mac")))
    return changes


# ---------------------------------------------------------------------------
# hosts：/etc/hosts 条目（sysfiles.collect_hosts）。同一 IP 可能出现多行
# （如 127.0.0.1 对应 localhost 和 view-localhost 两行），按 IP 合并 hostnames
# 再比较，避免多行同 IP 互相覆盖导致假变化。
# ---------------------------------------------------------------------------


def _hosts_map(entries: list) -> dict:
    result: dict = {}
    for e in entries:
        ip = e.get("ip")
        bucket = result.setdefault(ip, [])
        for h in e.get("hostnames") or []:
            if h not in bucket:
                bucket.append(h)
    return result


def _diff_hosts(section: str, before: dict, after: dict) -> list[Change]:
    b_map = _hosts_map(before.get("entries") or [])
    a_map = _hosts_map(after.get("entries") or [])
    changes: list[Change] = []
    for ip in sorted(set(b_map) | set(a_map)):
        b, a = b_map.get(ip), a_map.get(ip)
        if b is None:
            changes.append(_c(section, "added", ip, None, a))
            continue
        if a is None:
            changes.append(_c(section, "removed", ip, b, None))
            continue
        if b != a:
            changes.append(_c(section, "changed", ip, b, a))
    # 条目本身没变但 sha256 变了：多半是注释/空行/格式变动，如实报一条，别装看不见
    if not changes and before.get("sha256") != after.get("sha256"):
        changes.append(_c(section, "changed", "file", before.get("sha256"), after.get("sha256")))
    return changes


# ---------------------------------------------------------------------------
# resolver：/etc/resolver/* 按域名比较（sysfiles.collect_resolver_dir）。
# ---------------------------------------------------------------------------


def _diff_resolver(section: str, before: dict, after: dict) -> list[Change]:
    b_map = {d.get("domain"): d for d in before.get("domains") or []}
    a_map = {d.get("domain"): d for d in after.get("domains") or []}
    changes: list[Change] = []
    for domain in sorted(set(b_map) | set(a_map)):
        b, a = b_map.get(domain), a_map.get(domain)
        if b is None:
            changes.append(_c(section, "added", domain, None, a))
            continue
        if a is None:
            changes.append(_c(section, "removed", domain, b, None))
            continue
        for field in ("nameservers", "ports", "search_order"):
            if b.get(field) != a.get(field):
                changes.append(_c(section, "changed", f"{domain}.{field}", b.get(field), a.get(field)))
    return changes


# ---------------------------------------------------------------------------
# sysctl_net：内核网络参数值（sysfiles.collect_sysctl_net）。
# ---------------------------------------------------------------------------


def _diff_sysctl_net(section: str, before: dict, after: dict) -> list[Change]:
    changes: list[Change] = []
    b_vals = before.get("values") or {}
    a_vals = after.get("values") or {}
    for name in sorted(set(b_vals) | set(a_vals)):
        b, a = b_vals.get(name), a_vals.get(name)
        if b is None:
            changes.append(_c(section, "added", name, None, (a or {}).get("value")))
            continue
        if a is None:
            changes.append(_c(section, "removed", name, (b or {}).get("value"), None))
            continue
        if b.get("error") or a.get("error"):
            if bool(b.get("error")) != bool(a.get("error")):
                changes.append(_c(section, "unavailable", name, b.get("error"), a.get("error")))
            continue
        if b.get("value") != a.get("value"):
            changes.append(_c(section, "changed", name, b.get("value"), a.get("value")))
    return changes


_HANDLERS: dict[str, Callable[[str, dict, dict], list[Change]]] = {
    "interfaces": _diff_interfaces,
    "routes": _diff_routes,
    "dns": _diff_dns,
    "proxies": _diff_proxies,
    "vpn_services": _diff_vpn_services,
    "netbird": _diff_netbird,
    "mihomo": _diff_mihomo,
    "owners": _diff_owners,
    "scenario": _diff_scenario,
    "dynamic_store": _diff_dynamic_store,
    "nwi": _diff_nwi,
    "network_services": _diff_network_services,
    "dhcp": _diff_dhcp,
    "gateway_mac": _diff_gateway_mac,
    "hosts": _diff_hosts,
    "resolver": _diff_resolver,
    "sysctl_net": _diff_sysctl_net,
}


def diff_snapshots(before: dict, after: dict) -> list[Change]:
    """比较两份快照，按段返回 `Change` 列表。

    只处理 `_HANDLERS` 里认识的采集器名字；不认识的段（调用方自定义采集器，
    或 `targets`/`interface_owners` 之外的派生结果）直接跳过，不报错——跟
    `events.py` 的约定一致。

    一段只要有一方缺失（没采集）或带 `error`（采集失败），就产出一条
    `kind="unavailable"` 的说明、不做该段的语义比较——出错/缺失快照的数据结构
    不可信，硬比只会出假差异。
    """
    changes: list[Change] = []
    for section, handler in _HANDLERS.items():
        b, a = before.get(section), after.get(section)
        if b is None or a is None:
            if b is None and a is None:
                continue
            changes.append(_c(section, "unavailable", section, "missing" if b is None else None,
                               "missing" if a is None else None))
            continue
        b_err, a_err = b.get("error"), a.get("error")
        if b_err or a_err:
            changes.append(_c(section, "unavailable", section, b_err, a_err))
            continue
        changes.extend(handler(section, b, a))
    return changes


_KIND_LABEL = {"added": "新增", "removed": "移除", "changed": "变化", "unavailable": "无法比较"}


def summarize_changes(changes: list[Change]) -> str:
    """按段汇总成中文概述，每段一行，供命令行/网页快速查看。没有变化返回固定提示。"""
    if not changes:
        return "两份快照没有差异"
    by_section: dict[str, list[Change]] = {}
    for ch in changes:
        by_section.setdefault(ch.section, []).append(ch)
    lines = []
    for section, items in by_section.items():
        unavailable = [c for c in items if c.kind == "unavailable"]
        if unavailable:
            lines.append(f"{section}：无法比较（数据缺失或采集失败）")
            continue
        custom = _SECTION_SUMMARY.get(section)
        if custom:
            lines.append(custom(section, items))
            continue
        parts = [_describe(c) for c in items]
        lines.append(f"{section}：{len(items)} 项变化（" + "、".join(parts) + "）")
    return "\n".join(lines)


def _summarize_hosts(section: str, items: list[Change]) -> str:
    """hosts 段单独成句："新增 N 条（ip host1 host2）"这类人话，而不是泛泛的"N 项变化"。"""
    added = [c for c in items if c.kind == "added"]
    removed = [c for c in items if c.kind == "removed"]
    changed = [c for c in items if c.kind == "changed" and c.subject != "file"]
    content_only = [c for c in items if c.subject == "file"]
    parts = []
    if added:
        parts.append(f"新增 {len(added)} 条（" +
                      "、".join(f"{c.subject} {' '.join(c.after or [])}" for c in added) + "）")
    if removed:
        parts.append(f"移除 {len(removed)} 条（" +
                      "、".join(f"{c.subject} {' '.join(c.before or [])}" for c in removed) + "）")
    if changed:
        parts.append(f"修改 {len(changed)} 条（" +
                      "、".join(f"{c.subject}：{' '.join(c.before or [])}→{' '.join(c.after or [])}"
                                for c in changed) + "）")
    if content_only:
        parts.append("文件内容变化（条目未变，可能是注释/格式改动）")
    return f"{section}：" + "；".join(parts)


def _summarize_nwi(section: str, items: list[Change]) -> str:
    """主网卡切换（顺序变化）单独说清楚"从哪换到哪"，比"N 项变化"直观得多。"""
    order_items = [c for c in items if c.subject.endswith(".order")]
    other = [c for c in items if not c.subject.endswith(".order")]
    parts = []
    for c in order_items:
        family = c.subject.rsplit(".", 1)[0]
        old = c.before[0] if c.before else "无"
        new = c.after[0] if c.after else "无"
        parts.append(f"{family} 主网卡：{old}→{new}")
    if other:
        parts.append(f"{len(other)} 项其他变化（" + "、".join(_describe(c) for c in other) + "）")
    return f"{section}：" + "；".join(parts) if parts else f"{section}：{len(items)} 项变化"


_SECTION_SUMMARY: dict[str, Callable[[str, list[Change]], str]] = {
    "hosts": _summarize_hosts,
    "nwi": _summarize_nwi,
}


def _describe(c: Change) -> str:
    """标量值的变化直接写出前后值，一眼能看懂；列表/字典这类复合值只报类型，细节看 Change 本身。"""
    scalar = (str, int, float, bool, type(None))
    if c.kind == "changed" and isinstance(c.before, scalar) and isinstance(c.after, scalar):
        return f"{c.subject}：{_show(c.before)}→{_show(c.after)}"
    if c.kind == "changed" and isinstance(c.before, dict) and isinstance(c.after, dict):
        keys = [k for k in {**c.before, **c.after}
                if c.before.get(k) != c.after.get(k)
                and isinstance(c.before.get(k), scalar) and isinstance(c.after.get(k), scalar)]
        if keys:
            return "、".join(
                f"{c.subject}.{k}：{_show(c.before.get(k))}→{_show(c.after.get(k))}" for k in keys)
    return f"{c.subject}{_KIND_LABEL[c.kind]}"


def _show(value) -> str:
    return "无" if value is None else str(value)
