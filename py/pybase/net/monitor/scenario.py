"""从一次快照归纳出"当前网络组合"：哪些 WireGuard 隧道连着、Clash/NetBird/自建隧道
状态、默认路由出口归属，给出稳定的 `key`（同样组合同样 key，用于分组统计）与人类
可读的中文 `label`。

纯函数：只读传入的 `snapshot`（各采集器信封 dict 的映射），不跑任何系统命令、不带
任何项目默认值——有状态的编排（写库、比较变化）留给 `Recorder`/调用方。
"""
from __future__ import annotations

import dataclasses
import ipaddress
from dataclasses import dataclass
from typing import Mapping, Optional

from .collectors import via_of


@dataclass(frozen=True)
class Scenario:
    key: str
    label: str
    wireguard: tuple[str, ...]
    netbird_available: bool
    netbird_connected: bool
    netbird_status: Optional[str]
    clash_mode: Optional[str]
    clash_tun: bool
    system_proxy: bool          # 系统代理是否真的开着
    system_proxy_to_clash: bool  # 开着且指向本机 mihomo 在听的端口
    self_tunnels: tuple[str, ...]
    default_interface: Optional[str]
    default_via: str
    ipv6_default_interface: Optional[str]


# 文档专用地址（RFC 5737 / RFC 3849），只拿来在路由表里做最长前缀匹配，不发包
_PROBE_ADDRESS = {"ipv4": ipaddress.ip_address("192.0.2.1"),
                  "ipv6": ipaddress.ip_address("2001:db8::1")}


def _route_network(destination: str, family: str):
    """把 netstat 的目的地写法转成网段：`default`、`1`（=1.0.0.0/8）、`128.0/1`、`fe80::%en0/64`。"""
    if destination == "default":
        return ipaddress.ip_network("::/0" if family == "ipv6" else "0.0.0.0/0")
    addr, _, prefix = destination.partition("/")
    addr = addr.split("%", 1)[0]
    try:
        if family == "ipv4":
            parts = addr.split(".")
            if not 1 <= len(parts) <= 4:
                return None
            # macOS 省略写法：缺的字节补 0，没写前缀时按写出的字节数算
            addr = ".".join(parts + ["0"] * (4 - len(parts)))
            prefix = prefix or str(len(parts) * 8)
        return ipaddress.ip_network(f"{addr}/{prefix or (128 if family == 'ipv6' else 32)}", strict=False)
    except ValueError:
        return None


def system_proxy_target(proxies: dict) -> Optional[tuple[str, int]]:
    """系统代理实际开着时返回 (主机, 端口)，否则 None。

    只看开关项：代理全关时 `scutil --proxy` 仍会留 `FTPPassive` 之类的无关键，
    不能拿"字典非空"当作开启。自动代理配置（PAC）开着时主机记为 "pac"、端口 0。
    """
    if proxies.get("error"):
        return None
    system = proxies.get("system") or {}
    for kind in ("HTTP", "HTTPS", "SOCKS"):
        if str(system.get(f"{kind}Enable")) == "1":
            try:
                return str(system.get(f"{kind}Proxy") or ""), int(system.get(f"{kind}Port") or 0)
            except ValueError:
                return str(system.get(f"{kind}Proxy") or ""), 0
    if str(system.get("ProxyAutoConfigEnable")) == "1":
        return "pac", 0
    return None


def _default_interface(routes_payload: dict, family: str) -> Optional[str]:
    """实际的默认出口：对公网地址做最长前缀匹配，而不是找字面 `default`——
    Clash/mihomo 的 TUN 用 1/8、2/7…128/1 一组拆分路由盖过 default，不改 default 本身。
    macOS 的 I 标志（按接口限定的路由）不参与全局选路，跳过。
    """
    if routes_payload.get("error"):
        return None
    probe = _PROBE_ADDRESS[family]
    best = None
    for r in routes_payload.get(family, []):
        if "I" in (r.get("flags") or ""):
            continue
        network = _route_network(str(r.get("destination") or ""), family)
        if network is None or probe not in network:
            continue
        if best is None or network.prefixlen > best[0]:
            best = (network.prefixlen, r.get("interface"))
    return best[1] if best else None


def summarize_scenario(snapshot: Mapping[str, dict]) -> Scenario:
    """`snapshot` 至少应包含 `vpn_services`/`netbird`/`mihomo`/`proxies`/`routes`/
    `owners`（`collect_interface_owners` 的返回值）；缺哪个就当那部分不可用，
    不报错、不猜测。
    """
    vpn_services = snapshot.get("vpn_services") or {}
    netbird = snapshot.get("netbird") or {}
    mihomo = snapshot.get("mihomo") or {}
    proxies = snapshot.get("proxies") or {}
    routes = snapshot.get("routes") or {}
    owners = (snapshot.get("owners") or {}).get("owners") or {}

    wireguard = tuple(sorted(
        s.get("name") for s in vpn_services.get("services", [])
        if not vpn_services.get("error") and s.get("enabled") and s.get("status") == "Connected"
    ))
    self_tunnels = tuple(sorted({
        o["owner_name"] for o in owners.values()
        if o.get("owner_kind") == "self_tunnel" and o.get("owner_name")
    }))

    netbird_available = bool(not netbird.get("error") and netbird.get("available"))
    netbird_connected = bool(netbird_available and netbird.get("connected"))
    netbird_status = netbird.get("daemon_status") if netbird_available else None

    clash_mode = mihomo.get("mode") if not mihomo.get("error") else None
    clash_tun = bool(not mihomo.get("error") and mihomo.get("tun_enabled"))
    system_proxy = system_proxy_target(proxies)
    listen_ports = set(mihomo.get("listen_ports") or ()) if clash_mode else set()
    proxy_to_clash = bool(system_proxy and system_proxy[1] in listen_ports
                          and system_proxy[0] in ("127.0.0.1", "localhost", "::1"))

    v4_iface = _default_interface(routes, "ipv4")
    v6_iface = _default_interface(routes, "ipv6")
    default_via = via_of(v4_iface, owners)

    key = "|".join([
        "wg:" + (",".join(wireguard) if wireguard else "none"),
        "nb:" + (netbird_status or "off" if netbird_available else "absent"),
        f"clash:{clash_mode or 'off'}:{'tun' if clash_tun else 'notun'}:"
        f"{'sysproxy-clash' if proxy_to_clash else 'sysproxy-other' if system_proxy else 'nosysproxy'}",
        "self:" + (",".join(self_tunnels) if self_tunnels else "none"),
        f"via:{default_via}",
    ])

    bits: list[str] = []
    if wireguard:
        bits.append("WireGuard " + "+".join(wireguard))
    if netbird_connected:
        bits.append("NetBird 已连接")
    elif netbird_available:
        bits.append(f"NetBird {netbird_status or '未连接'}")
    # 按"实际接管了什么"描述：内核在跑不等于在用
    if clash_tun or proxy_to_clash:
        how = "+".join(x for x, on in (("TUN", clash_tun), ("系统代理", proxy_to_clash)) if on)
        bits.append(f"Clash 接管（{how}，{clash_mode} 模式）")
    elif clash_mode:
        bits.append("Clash 内核在运行但未接管")
    if system_proxy and not proxy_to_clash:
        bits.append(f"系统代理→{system_proxy[0]}:{system_proxy[1]}")
    if self_tunnels:
        bits.append("自建隧道 " + "+".join(self_tunnels))
    if not bits:
        bits.append("直连（无 VPN/代理）")
    label = "、".join(bits) + f"，出口经 {default_via}"

    return Scenario(
        key=key, label=label, wireguard=wireguard,
        netbird_available=netbird_available, netbird_connected=netbird_connected,
        netbird_status=netbird_status, clash_mode=clash_mode, clash_tun=clash_tun,
        system_proxy=system_proxy is not None, system_proxy_to_clash=proxy_to_clash,
        self_tunnels=self_tunnels,
        default_interface=v4_iface, default_via=default_via,
        ipv6_default_interface=v6_iface,
    )


def scenario_envelope(scenario: Scenario) -> dict:
    """把 `Scenario` 包成采集器约定的信封 dict（`error=None` + 字段），方便调用方
    把它当成普通一条快照塞进 `Recorder.record()`。"""
    return {"error": None, **dataclasses.asdict(scenario)}
