"""快照前后对比 -> 变化事件。每个采集器的比较逻辑各自成一个函数，按采集器名字分发；
不认识的采集器名字（调用方自定义的采集器）直接不产生事件，不报错。

事件只是"发现了什么变了"，不做进一步编排（告警、通知都是调用方的事）。

`dynamic_store`/`nwi`/`network_services`/`dhcp`/`gateway_mac`/`hosts`/`resolver`/
`sysctl_net` 这几段的语义比较（含噪声忽略、服务名翻译）跟 `diff.py`
（给人看的"两份快照对比"）是同一套规则，直接从那边 import `_diff_*` 函数复用，
避免两份实现分头维护、悄悄漂移。
"""
from __future__ import annotations

from typing import Callable, Optional

from . import diff as _diff


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


def _diff_scenario(before: dict, after: dict, at: float) -> list[dict]:
    if before.get("key") == after.get("key"):
        return []
    return [_event(at, "scenario_changed", "scenario", before.get("label"), after.get("label"))]


def _dominant_exit(exits: dict) -> Optional[str]:
    """出口分布里连接数最多的那个标签，代表这个应用"主要"走哪条路。"""
    if not exits:
        return None
    return max(exits.items(), key=lambda kv: kv[1])[0]


def _diff_apps(before: dict, after: dict, at: float) -> list[dict]:
    """应用采集器（`collect_apps`）的事件：启停、进程实例的启动/退出、每个实例
    主出口变化、每个域名每条路径的 ok/非 ok 切换，以及"多个域名的 router 路径
    解析 IP 同时变了"汇总成一条路由器 fake-ip 重置事件。应用列表本身由调用方的
    配置决定，两次快照之间增减应用不产生事件（那是改配置，不是网络变化）。

    实例按 pid 对齐：同一个 pid 在前后两次快照都出现就是同一个实例，pid 消失/
    出现就是这个实例退出/启动了（用户在不同终端窗口分别起了几份同一个应用，
    这样才能各自独立地记录启停，而不是混成一个"应用"的开关）。

    延迟/耗时/IP 这类每轮都会变的字段特意不参与这里的比较（否则每轮都会有一堆
    事件），只在状态真的从通变不通（或反过来）时才记一条。
    """
    b_map = {a["name"]: a for a in before.get("apps", [])}
    a_map = {a["name"]: a for a in after.get("apps", [])}
    events: list[dict] = []
    changed_router_domains: set[str] = set()

    for name in sorted(set(b_map) & set(a_map)):
        b, a = b_map[name], a_map[name]
        if b.get("running") != a.get("running"):
            kind = "app_started" if a.get("running") else "app_stopped"
            events.append(_event(at, kind, name, b.get("running"), a.get("running")))

        b_inst = {i["pid"]: i for i in b.get("instances") or []}
        a_inst = {i["pid"]: i for i in a.get("instances") or []}
        for pid in sorted(set(b_inst) - set(a_inst)):
            inst = b_inst[pid]
            subject = f"{name}:{inst.get('tty') or '?'}:{pid}"
            events.append(_event(at, "app_instance_stopped", subject, inst, None))
        for pid in sorted(set(a_inst) - set(b_inst)):
            inst = a_inst[pid]
            subject = f"{name}:{inst.get('tty') or '?'}:{pid}"
            events.append(_event(at, "app_instance_started", subject, None, inst))
        for pid in sorted(set(b_inst) & set(a_inst)):
            bi, ai = b_inst[pid], a_inst[pid]
            b_exit, a_exit = _dominant_exit(bi.get("exits") or {}), _dominant_exit(ai.get("exits") or {})
            if a_exit is not None and b_exit != a_exit:
                subject = f"{name}:{ai.get('tty') or '?'}:{pid}"
                events.append(_event(at, "app_exit_changed", subject, b_exit, a_exit))

        b_domains, a_domains = b.get("domains") or {}, a.get("domains") or {}
        for host in sorted(set(b_domains) | set(a_domains)):
            b_paths, a_paths = b_domains.get(host) or {}, a_domains.get(host) or {}
            for path in sorted(set(b_paths) | set(a_paths)):
                b_result, a_result = b_paths.get(path), a_paths.get(path)
                if b_result is None or a_result is None:
                    continue
                b_status, a_status = b_result.get("status"), a_result.get("status")
                if (b_status == "ok") != (a_status == "ok"):
                    events.append(_event(at, "app_domain_status_changed", f"{name}:{host}:{path}", b_status, a_status))
                if (
                    path == "router" and b_status == "ok" and a_status == "ok"
                    and b_result.get("ip") != a_result.get("ip")
                ):
                    changed_router_domains.add(host)

    if len(changed_router_domains) >= 2:
        events.append(_event(at, "router_fakeip_reset", "router", None, sorted(changed_router_domains)))
    return events


def _real_changes(changes: list) -> list:
    """过滤掉 `unavailable`（单项数据缺失/出错，不代表配置变了，不值得发事件）。"""
    return [c for c in changes if c.kind in ("added", "removed", "changed")]


def _diff_dynamic_store(before: dict, after: dict, at: float) -> list[dict]:
    changes = _real_changes(_diff._diff_dynamic_store("dynamic_store", before, after))
    return [_event(at, "dynamic_store_changed", c.subject, c.before, c.after) for c in changes]


def _diff_nwi(before: dict, after: dict, at: float) -> list[dict]:
    events: list[dict] = []
    for c in _real_changes(_diff._diff_nwi("nwi", before, after)):
        kind = "nwi_order_changed" if c.subject.endswith(".order") else "nwi_changed"
        events.append(_event(at, kind, c.subject, c.before, c.after))
    return events


def _diff_network_services(before: dict, after: dict, at: float) -> list[dict]:
    events: list[dict] = []
    for c in _real_changes(_diff._diff_network_services("network_services", before, after)):
        kind = "network_service_order_changed" if c.subject == "order" else "network_service_changed"
        events.append(_event(at, kind, c.subject, c.before, c.after))
    return events


def _diff_dhcp(before: dict, after: dict, at: float) -> list[dict]:
    changes = _real_changes(_diff._diff_dhcp("dhcp", before, after))
    return [_event(at, "dhcp_changed", c.subject, c.before, c.after) for c in changes]


def _diff_gateway_mac(before: dict, after: dict, at: float) -> list[dict]:
    changes = _real_changes(_diff._diff_gateway_mac("gateway_mac", before, after))
    return [_event(at, "gateway_mac_changed", c.subject, c.before, c.after) for c in changes]


def _diff_hosts(before: dict, after: dict, at: float) -> list[dict]:
    changes = _real_changes(_diff._diff_hosts("hosts", before, after))
    return [_event(at, "hosts_changed", c.subject, c.before, c.after) for c in changes]


def _diff_resolver(before: dict, after: dict, at: float) -> list[dict]:
    changes = _real_changes(_diff._diff_resolver("resolver", before, after))
    return [_event(at, "resolver_changed", c.subject, c.before, c.after) for c in changes]


def _diff_sysctl_net(before: dict, after: dict, at: float) -> list[dict]:
    changes = _real_changes(_diff._diff_sysctl_net("sysctl_net", before, after))
    return [_event(at, "sysctl_net_changed", c.subject, c.before, c.after) for c in changes]


_HANDLERS: dict[str, Callable[[dict, dict, float], list[dict]]] = {
    "interfaces": _diff_interfaces,
    "routes": _diff_routes,
    "dns": _diff_dns,
    "proxies": _diff_proxies,
    "vpn_services": _diff_vpn_services,
    "netbird": _diff_netbird,
    "mihomo": _diff_mihomo,
    "targets": _diff_targets,
    "apps": _diff_apps,
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
