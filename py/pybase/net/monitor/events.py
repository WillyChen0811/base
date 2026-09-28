"""快照前后对比 -> 变化事件。每个采集器的比较逻辑各自成一个函数，按采集器名字分发；
不认识的采集器名字（调用方自定义的采集器）直接不产生事件，不报错。

事件只是"发现了什么变了"，不做进一步编排（告警、通知都是调用方的事）。
"""
from __future__ import annotations

from typing import Callable, Optional


def diff(collector: str, before: Optional[dict], after: dict, at: float) -> list[dict]:
    """`before`/`after` 都是采集器返回的完整信封（含 `error` 字段）。

    `before` 为 None（这个采集器第一次出现）时不产生事件——没有"之前"可比。
    双方有一个带 `error` 就跳过对比：出错快照的数据结构不可信，硬比只会出假事件。
    """
    if before is None or before.get("error") or after.get("error"):
        return []
    handler = _HANDLERS.get(collector)
    if handler is None:
        return []
    return handler(before, after, at)


def _event(at: float, kind: str, subject: str, before, after) -> dict:
    return {"at": at, "kind": kind, "subject": subject, "before": before, "after": after}


def _diff_interfaces(before: dict, after: dict, at: float) -> list[dict]:
    b_map = {i["name"]: i for i in before.get("interfaces", [])}
    a_map = {i["name"]: i for i in after.get("interfaces", [])}
    events: list[dict] = []
    for name in sorted(set(b_map) | set(a_map)):
        b, a = b_map.get(name), a_map.get(name)
        if b is None:
            events.append(_event(at, "interface_added", name, None, a))
            continue
        if a is None:
            events.append(_event(at, "interface_removed", name, b, None))
            continue
        if b.get("status") != a.get("status"):
            kind = "interface_up" if a.get("status") == "active" else "interface_down"
            events.append(_event(at, kind, name, b.get("status"), a.get("status")))
        b_addrs = {x["address"] for x in b.get("ipv4", []) + b.get("ipv6", [])}
        a_addrs = {x["address"] for x in a.get("ipv4", []) + a.get("ipv6", [])}
        if b_addrs != a_addrs:
            events.append(_event(at, "interface_address_changed", name, sorted(b_addrs), sorted(a_addrs)))
    return events


def _default_route(routes: list[dict]) -> Optional[dict]:
    return next((r for r in routes if r.get("destination") == "default"), None)


def _diff_routes(before: dict, after: dict, at: float) -> list[dict]:
    events: list[dict] = []
    for family in ("ipv4", "ipv6"):
        b_def = _default_route(before.get(family, []))
        a_def = _default_route(after.get(family, []))
        b_key = (b_def or {}).get("gateway"), (b_def or {}).get("interface")
        a_key = (a_def or {}).get("gateway"), (a_def or {}).get("interface")
        if b_key != a_key:
            events.append(_event(at, "default_route_changed", family, b_def, a_def))
    return events


def _diff_dns(before: dict, after: dict, at: float) -> list[dict]:
    def global_nameservers(payload: dict) -> list[str]:
        seen = []
        for r in payload.get("resolvers", []):
            if not r.get("scoped"):
                seen.extend(r.get("nameservers", []))
        return sorted(set(seen))

    b_ns, a_ns = global_nameservers(before), global_nameservers(after)
    if b_ns != a_ns:
        return [_event(at, "dns_resolvers_changed", "global", b_ns, a_ns)]
    return []


def _diff_proxies(before: dict, after: dict, at: float) -> list[dict]:
    if before.get("system") != after.get("system"):
        return [_event(at, "system_proxy_changed", "system", before.get("system"), after.get("system"))]
    return []


def _diff_vpn_services(before: dict, after: dict, at: float) -> list[dict]:
    b_map = {s["uuid"]: s for s in before.get("services", [])}
    a_map = {s["uuid"]: s for s in after.get("services", [])}
    events: list[dict] = []
    for uuid in sorted(set(b_map) & set(a_map)):
        b, a = b_map[uuid], a_map[uuid]
        if b.get("status") != a.get("status"):
            events.append(_event(at, "vpn_service_status_changed", a.get("name") or uuid, b.get("status"), a.get("status")))
    return events


def _diff_netbird(before: dict, after: dict, at: float) -> list[dict]:
    if not before.get("available") or not after.get("available"):
        return []
    events: list[dict] = []
    if before.get("daemon_status") != after.get("daemon_status"):
        events.append(_event(at, "netbird_status_changed", "daemon_status", before.get("daemon_status"), after.get("daemon_status")))
    if before.get("connected") != after.get("connected"):
        events.append(_event(at, "netbird_status_changed", "management_connected", before.get("connected"), after.get("connected")))
    return events


def _diff_mihomo(before: dict, after: dict, at: float) -> list[dict]:
    events = []
    if before.get("mode") != after.get("mode"):
        events.append(_event(at, "mihomo_mode_changed", "mode", before.get("mode"), after.get("mode")))
    if before.get("tun_enabled") != after.get("tun_enabled"):
        events.append(_event(at, "mihomo_tun_changed", "tun_enabled", before.get("tun_enabled"), after.get("tun_enabled")))
    return events


def _diff_targets(before: dict, after: dict, at: float) -> list[dict]:
    b_map = {t["name"]: t for t in before.get("targets", []) if not t.get("error")}
    a_map = {t["name"]: t for t in after.get("targets", []) if not t.get("error")}
    events: list[dict] = []
    for name in sorted(set(b_map) & set(a_map)):
        b, a = b_map[name], a_map[name]
        b_iface = (b.get("route") or {}).get("interface")
        a_iface = (a.get("route") or {}).get("interface")
        if b_iface != a_iface:
            events.append(_event(at, "target_interface_changed", name, b_iface, a_iface))
        if b.get("proxied") != a.get("proxied"):
            events.append(_event(at, "target_proxied_changed", name, b.get("proxied"), a.get("proxied")))
        b_status = (b.get("probe") or {}).get("status")
        a_status = (a.get("probe") or {}).get("status")
        if b_status != a_status:
            events.append(_event(at, "target_connectivity_changed", name, b_status, a_status))
    return events


_HANDLERS: dict[str, Callable[[dict, dict, float], list[dict]]] = {
    "interfaces": _diff_interfaces,
    "routes": _diff_routes,
    "dns": _diff_dns,
    "proxies": _diff_proxies,
    "vpn_services": _diff_vpn_services,
    "netbird": _diff_netbird,
    "mihomo": _diff_mihomo,
    "targets": _diff_targets,
}
