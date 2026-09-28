"""系统网络配置来源采集器：`networksetup` 服务清单、DHCP 详情、网关 MAC、
`/etc/hosts`、`/etc/resolver/*`、关键内核网络参数。

与 `collectors.py` 同样的约定：每个函数只读，返回可 JSON 序列化的 `dict`，
永不抛异常，失败体现在 `"error"` 字段；子进程复用 `pybase.media._proc` 的
超时机制。不内置网段/接口名等业务默认值；下面用到的系统路径
（`/etc/hosts`、`/etc/resolver`）是平台约定的默认值，仍然可以被调用方覆盖。

SCDynamicStore 的完整读取由另一个采集器（sysconfig.py）负责，这里不重复。
"""
from __future__ import annotations

import hashlib
import platform
import re
from pathlib import Path
from typing import Optional, Sequence

from ...media._proc import MediaError, ToolNotFoundError, resolve_tool, run

_TIMEOUT_DEFAULT = 3.0

# 少量关键内核网络参数，覆盖转发开关与常见 MSS 调优；调用方可传别的列表覆盖。
_DEFAULT_SYSCTL_NAMES = (
    "net.inet.ip.forwarding",
    "net.inet6.ip6.forwarding",
    "net.inet.tcp.mssdflt",
)


def _fail(error: str, **extra) -> dict:
    return {"error": error, **extra}


def _ok(**data) -> dict:
    return {"error": None, **data}


# ---------- networksetup: 网络服务清单/启用状态/优先级 ----------


def collect_network_services(*, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """`networksetup` 三条命令的汇总：当前位置、全部服务（含禁用）、服务优先级顺序。

    仅 macOS 有 `networksetup`；其他平台返回 `error`，调用方应跳过。
    """
    if platform.system() != "Darwin":
        return _fail(f"不支持的平台：{platform.system()}")
    try:
        tool = resolve_tool(None, "networksetup")
        location = run([tool, "-getcurrentlocation"], timeout=timeout)
        services = run([tool, "-listallnetworkservices"], timeout=timeout)
        order = run([tool, "-listnetworkserviceorder"], timeout=timeout)
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))
    if location.returncode != 0:
        return _fail(f"networksetup -getcurrentlocation 退出码 {location.returncode}："
                      f"{location.stderr.strip()[:500]}")
    if services.returncode != 0:
        return _fail(f"networksetup -listallnetworkservices 退出码 {services.returncode}："
                      f"{services.stderr.strip()[:500]}")
    if order.returncode != 0:
        return _fail(f"networksetup -listnetworkserviceorder 退出码 {order.returncode}："
                      f"{order.stderr.strip()[:500]}")
    return _ok(
        current_location=location.stdout.strip(),
        services=_parse_network_services(services.stdout),
        order=_parse_network_service_order(order.stdout),
    )


def _parse_network_services(text: str) -> list[dict]:
    """`-listallnetworkservices` 的每一行是服务名，`*` 前缀表示已禁用。"""
    services = []
    for line in text.splitlines():
        line = line.rstrip("\n")
        if not line.strip() or line.startswith("An asterisk"):
            continue
        enabled = not line.startswith("*")
        name = line[1:].strip() if not enabled else line.strip()
        services.append({"name": name, "enabled": enabled})
    return services


_ORDER_HEAD_RE = re.compile(r"^\((?P<index>\d+)\)\s+(?P<name>.+)$")
_ORDER_PORT_RE = re.compile(r"^\(Hardware Port:\s*(?P<port>[^,]*),\s*Device:\s*(?P<device>[^)]*)\)$")


def _parse_network_service_order(text: str) -> list[dict]:
    """`-listnetworkserviceorder` 每条服务两行：`(序号) 名字` + `(Hardware Port: .., Device: ..)`。"""
    order: list[dict] = []
    current: Optional[dict] = None
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("An asterisk"):
            continue
        head = _ORDER_HEAD_RE.match(line)
        if head:
            current = {
                "index": int(head.group("index")),
                "name": head.group("name").strip(),
                "hardware_port": None,
                "device": None,
            }
            order.append(current)
            continue
        port = _ORDER_PORT_RE.match(line)
        if port and current is not None:
            current["hardware_port"] = port.group("port").strip() or None
            current["device"] = port.group("device").strip() or None
    return order


# ---------- ipconfig getsummary: DHCP/链路详情 ----------
#
# `ipconfig getsummary` 输出是 scutil 风格的嵌套 <dictionary>/<array>，但里面混着
# DHCP/DHCPv6 报文的原始多行转储（`Packet` 字段），这段转储本身还带花括号
# （如 `domain_name_server (ip_mult): {172.20.10.1}`），没法用花括号计数做通用
# 树形解析（会被转储里的花括号带偏）。这里只按行正则抓取需要的关键字段，
# 不做完整结构还原——足够回答"这个接口现在是什么配置"。


_DHCP_FIELD_PATTERNS = {
    "config_method": re.compile(r"^\s*ConfigMethod\s*:\s*(\S+)"),
    "router": re.compile(r"^\s*Router\s*:\s*(\S+)"),
    "router_arp_verified": re.compile(r"^\s*RouterARPVerified\s*:\s*(\S+)"),
    "lease_start_time": re.compile(r"^\s*LeaseStartTime\s*:\s*(.+)$"),
    "lease_expiration_time": re.compile(r"^\s*LeaseExpirationTime\s*:\s*(.+)$"),
    "link_status_active": re.compile(r"^\s*LinkStatusActive\s*:\s*(\S+)"),
    "active": re.compile(r"^\s*Active\s*:\s*(\S+)"),
    "interface_type": re.compile(r"^\s*InterfaceType\s*:\s*(\S+)"),
    "security": re.compile(r"^\s*Security\s*:\s*(\S+)"),
    "ssid": re.compile(r"^\s*SSID\s*:\s*(.+)$"),
    "bssid": re.compile(r"^\s*BSSID\s*:\s*(.+)$"),
}
# 只在 DHCP 报文转储里出现的字段（options 段），格式是 `<key> (<type>): <value>`
_DHCP_OPTION_DOMAIN_NAME_RE = re.compile(r"^domain_name\s*\(string\)\s*:\s*(.+)$")
_DHCP_OPTION_DNS_RE = re.compile(r"^domain_name_server\s*\(ip_mult\)\s*:\s*\{([^}]*)\}")
_TRUE_FALSE = {"TRUE": True, "FALSE": False}


def _redact(value: Optional[str]) -> Optional[str]:
    """系统没给定位权限时 SSID/BSSID/NetworkID 会显示成字面量 `<redacted>`，
    如实记成 `"redacted"`（不是拿不到，是系统主动隐藏）。
    """
    if value is None:
        return None
    value = value.strip()
    return "redacted" if value == "<redacted>" else (value or None)


def _parse_ipconfig_summary(text: str) -> dict:
    fields: dict = {key: None for key in _DHCP_FIELD_PATTERNS}
    domain_name = None
    domain_name_server: list[str] = []
    for line in text.splitlines():
        for key, pattern in _DHCP_FIELD_PATTERNS.items():
            if fields[key] is not None:
                continue
            m = pattern.match(line)
            if m:
                fields[key] = m.group(1).strip()
                break
        stripped = line.strip()
        if domain_name is None:
            m = _DHCP_OPTION_DOMAIN_NAME_RE.match(stripped)
            if m:
                domain_name = m.group(1).strip()
        if not domain_name_server:
            m = _DHCP_OPTION_DNS_RE.match(stripped)
            if m:
                domain_name_server = m.group(1).split()

    for bool_key in ("router_arp_verified", "link_status_active", "active"):
        raw = fields.get(bool_key)
        fields[bool_key] = _TRUE_FALSE.get(raw, raw) if raw is not None else None
    fields["ssid"] = _redact(fields.get("ssid"))
    fields["bssid"] = _redact(fields.get("bssid"))
    fields["domain_name"] = domain_name
    fields["domain_name_server"] = domain_name_server
    return fields


def collect_dhcp(interfaces: Sequence[str], *, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """对给定接口逐个跑 `ipconfig getsummary`，解析出关键 DHCP/链路字段。

    `interfaces` 由调用方给出（一般是活动接口名列表）；单个接口失败只影响它
    自己那一条（值里带 `error`），不影响其余接口，也不让整个采集器失败。
    """
    result: dict[str, dict] = {}
    for ifname in interfaces:
        result[ifname] = _collect_dhcp_one(ifname, timeout)
    return _ok(interfaces=result)


def _collect_dhcp_one(ifname: str, timeout: float) -> dict:
    if platform.system() != "Darwin":
        return _fail(f"不支持的平台：{platform.system()}")
    try:
        tool = resolve_tool(None, "ipconfig")
        result = run([tool, "getsummary", ifname], timeout=timeout)
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))
    if result.returncode != 0:
        return _fail(f"ipconfig getsummary 退出码 {result.returncode}：{result.stderr.strip()[:500]}")
    return _ok(**_parse_ipconfig_summary(result.stdout))


# ---------- 网关 MAC（arp / ndp）----------


_ARP_LINE_RE = re.compile(r"^\S+\s+\([\d.]+\)\s+at\s+(?P<mac>\S+)\s+on\s+(?P<iface>\S+)")
_NDP_LINE_RE = re.compile(
    r"^(?P<addr>\S+)\s+(?P<mac>\S+)\s+(?P<iface>\S+)\s+\S+\s+\S+"
)


def collect_gateway_mac(gateways: Sequence[str], *, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """给定网关 IP 列表，查 ARP（IPv4，`arp -n`）/ NDP（IPv6，`ndp -an`）缓存里的 MAC。

    用来判断"现在真的连的是哪个网络"：MAC 变了但 IP 没变，往往说明网关/网络
    组合换了（比如从家里路由器切到手机热点，网段刚好一样）。查不到（缓存没有、
    命令失败）落在对应条目的 `error`，不影响其他网关。
    """
    result: dict[str, dict] = {}
    ndp_cache: Optional[dict] = None
    for ip in gateways:
        if ":" in ip:
            if ndp_cache is None:
                ndp_cache = _run_ndp(timeout)
            # IPv6 可能带 `%接口名` scope id（调用方给的地址常这样），ndp 表里按裸地址存，查询前去掉
            result[ip] = _lookup_ndp(ndp_cache, ip.split("%", 1)[0])
        else:
            result[ip] = _run_arp(ip, timeout)
    return _ok(gateways=result)


def _run_arp(ip: str, timeout: float) -> dict:
    try:
        tool = resolve_tool(None, "arp")
        result = run([tool, "-n", ip], timeout=timeout)
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))
    if result.returncode != 0:
        # arp 查不到条目时把提示打在 stdout（如 "1.2.3.4 (1.2.3.4) -- no entry"），stderr 常是空的
        message = result.stderr.strip() or result.stdout.strip()
        return _fail(f"arp 退出码 {result.returncode}：{message[:500]}")
    m = _ARP_LINE_RE.match(result.stdout.strip())
    if not m:
        return _fail(f"arp 缓存里没有 {ip}（未连通或已过期）：{result.stdout.strip()[:200]}")
    return _ok(mac=m.group("mac"), interface=m.group("iface"))


def _run_ndp(timeout: float) -> dict:
    """`ndp -an` 一次性拿全表，`error` 不为 None 时表示命令本身失败。"""
    try:
        tool = resolve_tool(None, "ndp")
        result = run([tool, "-an"], timeout=timeout)
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))
    if result.returncode != 0:
        return _fail(f"ndp 退出码 {result.returncode}：{result.stderr.strip()[:500]}")
    entries: dict[str, tuple[str, str]] = {}
    for line in result.stdout.splitlines()[1:]:  # 第一行是表头
        m = _NDP_LINE_RE.match(line.strip())
        if not m or m.group("mac") == "(incomplete)":
            continue
        addr = m.group("addr").split("%", 1)[0]
        entries[addr] = (m.group("mac"), m.group("iface"))
    return _ok(entries=entries)


def _lookup_ndp(ndp_cache: dict, ip: str) -> dict:
    if ndp_cache.get("error"):
        return _fail(ndp_cache["error"])
    hit = ndp_cache["entries"].get(ip)
    if hit is None:
        return _fail(f"ndp 缓存里没有 {ip}（未连通或已过期）")
    return _ok(mac=hit[0], interface=hit[1])


# ---------- /etc/hosts ----------


def collect_hosts(path: str = "/etc/hosts", *, encoding: str = "utf-8") -> dict:
    """`/etc/hosts` 的非注释条目（IP -> 主机名列表）+ 整份内容的 sha256 + mtime。

    sha256 用来快速判断文件有没有变，不用每次都比对全部条目；`path` 可覆盖
    （测试用），生产用默认的系统路径。文件不存在/读不了是失败。
    """
    p = Path(path)
    try:
        raw = p.read_bytes()
        mtime = p.stat().st_mtime
    except OSError as exc:
        return _fail(f"读取 {path} 失败：{exc}")
    entries = []
    for line in raw.decode(encoding, errors="replace").splitlines():
        line = line.split("#", 1)[0].strip()
        if not line:
            continue
        parts = line.split()
        if len(parts) < 2:
            continue
        entries.append({"ip": parts[0], "hostnames": parts[1:]})
    return _ok(entries=entries, sha256=hashlib.sha256(raw).hexdigest(), mtime=mtime)


# ---------- /etc/resolver/* ----------


_RESOLVER_KV_RE = re.compile(r"^(?P<key>\S+)\s+(?P<value>.+)$")


def collect_resolver_dir(path: str = "/etc/resolver") -> dict:
    """`/etc/resolver/<domain>` 目录：每个文件名就是一个 scoped 域名，内容是
    `nameserver`/`port`/`search_order` 等键值对（一行一个）。

    目录不存在是正常情况（没配任何 per-domain resolver），返回空列表，不算错误；
    单个文件读不了只影响它自己那一条。
    """
    p = Path(path)
    if not p.exists():
        return _ok(domains=[])
    if not p.is_dir():
        return _fail(f"{path} 不是目录")
    domains = []
    try:
        names = sorted(f.name for f in p.iterdir() if f.is_file())
    except OSError as exc:
        return _fail(f"列出 {path} 失败：{exc}")
    for name in names:
        file_path = p / name
        try:
            text = file_path.read_text(encoding="utf-8", errors="replace")
            mtime = file_path.stat().st_mtime
        except OSError as exc:
            domains.append({"domain": name, "error": str(exc)})
            continue
        nameservers, ports, search_orders, other = [], [], [], {}
        for line in text.splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            m = _RESOLVER_KV_RE.match(line)
            if not m:
                continue
            key, value = m.group("key"), m.group("value").strip()
            if key == "nameserver":
                nameservers.append(value)
            elif key == "port":
                ports.append(value)
            elif key == "search_order":
                search_orders.append(value)
            else:
                other.setdefault(key, []).append(value)
        domains.append({
            "domain": name,
            "error": None,
            "nameservers": nameservers,
            "ports": ports,
            "search_order": search_orders,
            "other": other,
            "mtime": mtime,
        })
    return _ok(domains=domains)


# ---------- sysctl 内核网络参数 ----------


def collect_sysctl_net(names: Optional[Sequence[str]] = None, *, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """只读几个关键内核网络参数（`sysctl -n <name>`）。

    `names` 不传时用一小撮默认值（转发开关 + 常见 MSS 调优）；调用方可以整份
    覆盖。单个参数不存在/读失败只落在它自己那条的 `error`，不影响其余参数。
    """
    names = tuple(names) if names is not None else _DEFAULT_SYSCTL_NAMES
    try:
        tool = resolve_tool(None, "sysctl")
    except ToolNotFoundError as exc:
        return _fail(str(exc))
    values: dict[str, dict] = {}
    for name in names:
        try:
            result = run([tool, "-n", name], timeout=timeout)
        except MediaError as exc:
            values[name] = _fail(str(exc))
            continue
        if result.returncode != 0:
            values[name] = _fail(f"sysctl 退出码 {result.returncode}：{result.stderr.strip()[:300]}")
            continue
        values[name] = _ok(value=result.stdout.strip())
    return _ok(values=values)
