"""两份网络配置快照的差异对比。

`before`/`after` 是同一份快照结构：键为采集器名（`interfaces`/`routes`/`dns`/
`proxies`/`vpn_services`/`netbird`/`mihomo`/`owners`/`scenario` 等），值为该
采集器的返回信封（含 `error` 字段，见 `collectors.py`）。

跟 `events.py` 的思路一样——按采集器分段、语义化比较而不是整段 JSON 比——但
`events.py` 是给 `Recorder` 用的"发现变化就报事件"（不认识的采集器直接跳过、
两边有一个 error 就整段跳过），这里是给人看的"两个场景快照对比"，需要更完整：
- 一段在某一方缺失/带 error 时不是静默跳过，而是显式给一条 `unavailable`，
  让用户知道"这段没法比"而不是误以为"没变化"。
- 不认识的采集器名字（调用方自定义的段）也不处理，跟 `events.py` 一致。

纯函数，不写库、不发通知，编排留给调用方（CLI/网页展示）。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Callable, Optional


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
        parts = [_describe(c) for c in items]
        lines.append(f"{section}：{len(items)} 项变化（" + "、".join(parts) + "）")
    return "\n".join(lines)


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
