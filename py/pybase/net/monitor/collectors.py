"""网络状态采集器：每个函数只读系统命令/本机接口，不改任何网络设置。

约定：每个采集器返回可 JSON 序列化的 `dict`，且**永不抛异常**——失败（工具缺失、
解析不了、连不上）一律体现在返回值的 `"error"` 字段（成功时为 `None`），采集器
之间互不影响，调用方可以把多个采集器的结果直接拼进一份快照。不内置任何主机/
路径/URL 默认值，全部由调用方传入。
"""
from __future__ import annotations

import concurrent.futures
import json
import platform
import re
import urllib.error
import urllib.request
from typing import Optional, Sequence

from ..diag import classify_address, env_proxies, looks_proxied, route_of, tcp_probe
from ...media._proc import MediaError, ToolNotFoundError, resolve_tool, run

_TIMEOUT_DEFAULT = 3.0


def _fail(error: str, **extra) -> dict:
    return {"error": error, **extra}


def _ok(**data) -> dict:
    return {"error": None, **data}


# ---------- interfaces ----------

_IFCONFIG_HEAD_RE = re.compile(r"^(?P<name>\S+?):\s+flags=\d+<(?P<flags>[^>]*)>(?:.*\bmtu\s+(?P<mtu>\d+))?")
_IFCONFIG_ETHER_RE = re.compile(r"^ether\s+(?P<mac>\S+)")
_IFCONFIG_INET_RE = re.compile(
    r"^inet\s+(?P<addr>\S+)(?:\s+netmask\s+(?P<netmask>\S+))?(?:\s+broadcast\s+(?P<broadcast>\S+))?"
)
_IFCONFIG_INET6_RE = re.compile(
    r"^inet6\s+(?P<addr>\S+)(?:\s+prefixlen\s+(?P<prefixlen>\d+))?(?P<flags>(?:\s+\S+)*)"
)
_IFCONFIG_STATUS_RE = re.compile(r"^status:\s+(?P<status>\S+)")


def collect_interfaces(*, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """网卡列表：名称、状态、MTU、MAC、IPv4/IPv6 地址。"""
    system = platform.system()
    try:
        if system == "Darwin":
            return _collect_interfaces_ifconfig(timeout)
        if system == "Linux":
            return _collect_interfaces_ip(timeout)
        return _fail(f"不支持的平台：{system}")
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))


def _collect_interfaces_ifconfig(timeout: float) -> dict:
    tool = resolve_tool(None, "ifconfig")
    result = run([tool, "-a"], timeout=timeout)
    if result.returncode != 0:
        return _fail(f"ifconfig 退出码 {result.returncode}：{result.stderr.strip()[:500]}")
    interfaces: list[dict] = []
    current: Optional[dict] = None
    for line in result.stdout.splitlines():
        if not line.strip():
            continue
        if not line[0].isspace():
            head = _IFCONFIG_HEAD_RE.match(line)
            if not head:
                current = None
                continue
            current = {
                "name": head.group("name"),
                "flags": head.group("flags").split(",") if head.group("flags") else [],
                "mtu": int(head.group("mtu")) if head.group("mtu") else None,
                "mac": None,
                "status": None,
                "ipv4": [],
                "ipv6": [],
            }
            interfaces.append(current)
            continue
        if current is None:
            continue
        line = line.strip()
        m = _IFCONFIG_ETHER_RE.match(line)
        if m:
            current["mac"] = m.group("mac")
            continue
        m = _IFCONFIG_INET_RE.match(line)
        if m:
            current["ipv4"].append(
                {
                    "address": m.group("addr"),
                    "netmask": m.group("netmask"),
                    "broadcast": m.group("broadcast"),
                }
            )
            continue
        m = _IFCONFIG_INET6_RE.match(line)
        if m:
            addr = m.group("addr")
            scope = None
            if "%" in addr:
                addr, scope = addr.split("%", 1)
            current["ipv6"].append(
                {
                    "address": addr,
                    "scope": scope,
                    "prefixlen": int(m.group("prefixlen")) if m.group("prefixlen") else None,
                    "flags": (m.group("flags") or "").split(),
                }
            )
            continue
        m = _IFCONFIG_STATUS_RE.match(line)
        if m:
            current["status"] = m.group("status")
    return _ok(interfaces=interfaces)


def _collect_interfaces_ip(timeout: float) -> dict:
    tool = resolve_tool(None, "ip")
    result = run([tool, "-j", "addr"], timeout=timeout)
    if result.returncode != 0:
        return _fail(f"ip 退出码 {result.returncode}：{result.stderr.strip()[:500]}")
    try:
        raw = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        return _fail(f"ip -j addr 输出不是合法 JSON：{exc}")
    interfaces = []
    for entry in raw:
        ipv4, ipv6 = [], []
        for info in entry.get("addr_info", []):
            item = {
                "address": info.get("local"),
                "prefixlen": info.get("prefixlen"),
                "broadcast": info.get("broadcast"),
                "scope": info.get("scope"),
            }
            if info.get("family") == "inet":
                ipv4.append(item)
            elif info.get("family") == "inet6":
                ipv6.append(item)
        interfaces.append(
            {
                "name": entry.get("ifname"),
                "flags": entry.get("flags", []),
                "mtu": entry.get("mtu"),
                "mac": entry.get("address"),
                "status": entry.get("operstate"),
                "ipv4": ipv4,
                "ipv6": ipv6,
            }
        )
    return _ok(interfaces=interfaces)


# ---------- routes ----------


def collect_routes(*, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """路由表（IPv4 + IPv6，结构化条目）。"""
    system = platform.system()
    try:
        if system == "Darwin":
            v4 = _collect_routes_netstat(timeout, family="inet")
            v6 = _collect_routes_netstat(timeout, family="inet6")
            return _ok(ipv4=v4, ipv6=v6)
        if system == "Linux":
            v4 = _collect_routes_ip(timeout, ipv6=False)
            v6 = _collect_routes_ip(timeout, ipv6=True)
            return _ok(ipv4=v4, ipv6=v6)
        return _fail(f"不支持的平台：{system}")
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))


def _collect_routes_netstat(timeout: float, *, family: str) -> list[dict]:
    tool = resolve_tool(None, "netstat")
    result = run([tool, "-rn", "-f", family], timeout=timeout)
    if result.returncode != 0:
        raise MediaError(f"netstat 退出码 {result.returncode}：{result.stderr.strip()[:500]}")
    lines = result.stdout.splitlines()
    rows: list[dict] = []
    in_table = False
    for line in lines:
        if line.startswith("Destination"):
            in_table = True
            continue
        if not in_table or not line.strip():
            continue
        tokens = line.split()
        if len(tokens) < 4:
            continue
        destination, gateway, flags, interface = tokens[0], tokens[1], tokens[2], tokens[3]
        expire = tokens[4] if len(tokens) > 4 else None
        rows.append(
            {
                "destination": destination,
                "gateway": gateway,
                "flags": flags,
                "interface": interface,
                "expire": expire,
            }
        )
    return rows


def _collect_routes_ip(timeout: float, *, ipv6: bool) -> list[dict]:
    tool = resolve_tool(None, "ip")
    cmd = [tool]
    if ipv6:
        cmd.append("-6")
    cmd += ["-j", "route"]
    result = run(cmd, timeout=timeout)
    if result.returncode != 0:
        raise MediaError(f"ip 退出码 {result.returncode}：{result.stderr.strip()[:500]}")
    raw = json.loads(result.stdout)
    rows = []
    for entry in raw:
        rows.append(
            {
                "destination": entry.get("dst"),
                "gateway": entry.get("gateway"),
                "interface": entry.get("dev"),
                "source": entry.get("prefsrc"),
                "metric": entry.get("metric"),
                "flags": entry.get("flags", []),
            }
        )
    return rows


# ---------- dns ----------

_RESOLVER_HEAD_RE = re.compile(r"^resolver #\d+$")
_IF_INDEX_RE = re.compile(r"^(?P<index>\d+)\s*(?:\((?P<name>[^)]+)\))?$")


def collect_dns(*, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """`scutil --dns` 解析出的 resolver 列表（区分全局/按接口 scoped 两段）。"""
    try:
        tool = resolve_tool(None, "scutil")
        result = run([tool, "--dns"], timeout=timeout)
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))
    if result.returncode != 0:
        return _fail(f"scutil 退出码 {result.returncode}：{result.stderr.strip()[:500]}")
    resolvers: list[dict] = []
    scoped = False
    current: Optional[dict] = None
    for raw_line in result.stdout.splitlines():
        line = raw_line.strip()
        if line.startswith("DNS configuration"):
            scoped = "scoped queries" in line
            continue
        if _RESOLVER_HEAD_RE.match(line):
            current = {
                "scoped": scoped,
                "nameservers": [],
                "domain": None,
                "search": [],
                "if_index": None,
                "if_name": None,
                "order": None,
                "reach": None,
            }
            resolvers.append(current)
            continue
        if current is None or ":" not in line:
            continue
        key, _, value = line.partition(":")
        key, value = key.strip(), value.strip()
        if key.startswith("nameserver"):
            current["nameservers"].append(value)
        elif key.startswith("search domain"):
            current["search"].append(value)
        elif key == "domain":
            current["domain"] = value
        elif key == "order":
            current["order"] = int(value) if value.isdigit() else value
        elif key == "reach":
            current["reach"] = value
        elif key == "if_index":
            m = _IF_INDEX_RE.match(value)
            if m:
                current["if_index"] = int(m.group("index"))
                current["if_name"] = m.group("name")
    return _ok(resolvers=resolvers)


# ---------- proxies ----------


def _parse_scutil_dict(lines: list[str], start: int) -> tuple[dict, int]:
    """解析 `scutil --proxy` 这类 `<dictionary> { ... }` 输出，支持一层 `<array>` 嵌套。"""
    result: dict = {}
    i = start
    while i < len(lines):
        line = lines[i].strip()
        if line == "}":
            return result, i + 1
        if not line:
            i += 1
            continue
        key, _, rest = line.partition(":")
        key, rest = key.strip(), rest.strip()
        if rest.startswith("<array>"):
            items: list[str] = []
            i += 1  # 跳过 "key : <array> {" 这一行本身（左花括号在同一行）
            while i < len(lines) and lines[i].strip() != "}":
                item_line = lines[i].strip()
                _, _, value = item_line.partition(":")
                items.append(value.strip())
                i += 1
            result[key] = items
            i += 1
            continue
        result[key] = rest
        i += 1
    return result, i


def collect_proxies(*, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """系统代理设置（`scutil --proxy`）+ 进程环境变量代理。"""
    try:
        tool = resolve_tool(None, "scutil")
        result = run([tool, "--proxy"], timeout=timeout)
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc), env=env_proxies())
    if result.returncode != 0:
        return _fail(f"scutil 退出码 {result.returncode}：{result.stderr.strip()[:500]}", env=env_proxies())
    lines = result.stdout.splitlines()
    start = next((i + 1 for i, l in enumerate(lines) if l.strip().startswith("<dictionary>")), None)
    system_proxy = {} if start is None else _parse_scutil_dict(lines, start)[0]
    return _ok(system=system_proxy, env=env_proxies())


# ---------- vpn services (scutil --nc list) ----------

_NC_LINE_RE = re.compile(
    r'^(?P<enabled>[* ])\s*\((?P<status>[^)]+)\)\s+(?P<uuid>\S+)\s+(?P<desc>.*?)\s+'
    r'"(?P<name>[^"]*)"\s+\[(?P<subtype>[^\]]+)\]\s*$'
)


def collect_vpn_services(*, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """`scutil --nc list` 里的 VPN/PPP 服务列表（WireGuard.app 的隧道以此形式出现）。

    仅 macOS 有 `scutil`；其他平台返回 `error`，调用方应跳过。
    """
    try:
        tool = resolve_tool(None, "scutil")
        result = run([tool, "--nc", "list"], timeout=timeout)
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))
    if result.returncode != 0:
        return _fail(f"scutil 退出码 {result.returncode}：{result.stderr.strip()[:500]}")
    services = []
    for line in result.stdout.splitlines():
        m = _NC_LINE_RE.match(line)
        if not m:
            continue
        services.append(
            {
                "enabled": m.group("enabled") == "*",
                "status": m.group("status"),
                "uuid": m.group("uuid"),
                "description": m.group("desc"),
                "name": m.group("name"),
                "subtype": m.group("subtype"),
            }
        )
    return _ok(services=services)


# ---------- netbird ----------


def collect_netbird(netbird_path: Optional[str] = None, *, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """NetBird 状态（`netbird status --json`），保留原始 JSON 供以后深挖。

    没装 netbird CLI（`netbird_path` 没给且 PATH 里也找不到）时返回
    `{"error": None, "available": False}`——这不算失败，只是这台机器没装。
    """
    try:
        tool = resolve_tool(netbird_path, "netbird")
    except ToolNotFoundError:
        return _ok(available=False)
    try:
        result = run([tool, "status", "--json"], timeout=timeout)
    except MediaError as exc:
        return _fail(str(exc), available=True)
    if result.returncode != 0:
        return _fail(f"netbird 退出码 {result.returncode}：{result.stderr.strip()[:500]}", available=True)
    try:
        raw = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        return _fail(f"netbird status 输出不是合法 JSON：{exc}", available=True)
    peers = raw.get("peers") or {}
    management = raw.get("management") or {}
    return _ok(
        available=True,
        connected=management.get("connected"),
        daemon_status=raw.get("daemonStatus"),
        netbird_ip=raw.get("netbirdIp"),
        peers_total=peers.get("total"),
        peers_connected=peers.get("connected"),
        raw=raw,
    )


# ---------- mihomo / clash ----------


def _mihomo_get(base_url: str, path: str, secret: Optional[str], timeout: float) -> dict:
    req = urllib.request.Request(base_url.rstrip("/") + path)
    if secret:
        req.add_header("Authorization", f"Bearer {secret}")
    with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 - 调用方给的本地/内网地址
        return json.loads(resp.read().decode("utf-8"))


def collect_mihomo(base_url: str, secret: Optional[str] = None, *, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """mihomo/Clash 外部控制接口：版本、配置（mode/tun）、连接汇总。

    `base_url`（如 `http://127.0.0.1:9090`）与 `secret` 必须由调用方给，本函数
    不猜测、不读用户的 Clash 配置文件。`/connections` 是普通 REST GET 取一次
    快照，不是长连接的 traffic 流，读完即断开。
    """
    try:
        version = _mihomo_get(base_url, "/version", secret, timeout)
        configs = _mihomo_get(base_url, "/configs", secret, timeout)
        connections = _mihomo_get(base_url, "/connections", secret, timeout)
    except urllib.error.HTTPError as exc:
        return _fail(f"HTTP {exc.code}：{exc.reason}")
    except (urllib.error.URLError, OSError, json.JSONDecodeError, TimeoutError) as exc:
        return _fail(str(exc))

    conns = connections.get("connections") or []
    by_chain: dict[str, dict] = {}
    total_upload = total_download = 0
    for c in conns:
        total_upload += c.get("upload", 0) or 0
        total_download += c.get("download", 0) or 0
        chains = c.get("chains") or []
        key = " -> ".join(chains) if chains else (c.get("rule") or "unknown")
        bucket = by_chain.setdefault(key, {"count": 0, "upload": 0, "download": 0})
        bucket["count"] += 1
        bucket["upload"] += c.get("upload", 0) or 0
        bucket["download"] += c.get("download", 0) or 0
    top_chains = sorted(by_chain.items(), key=lambda kv: kv[1]["upload"] + kv[1]["download"], reverse=True)[:10]

    return _ok(
        version=version.get("version"),
        mode=configs.get("mode"),
        tun_enabled=bool((configs.get("tun") or {}).get("enable")),
        connections_count=len(conns),
        total_upload=total_upload,
        total_download=total_download,
        top_chains=[{"chain": k, **v} for k, v in top_chains],
    )


# ---------- targets ----------


def collect_targets(
    targets: Sequence[dict],
    *,
    route_timeout: float = _TIMEOUT_DEFAULT,
    probe_timeout: float = _TIMEOUT_DEFAULT,
    max_workers: int = 8,
) -> dict:
    """对一批目标（`[{"name", "host", "port"}, ...]`）并发做路由+连通性检查。

    每个目标互不影响：单个目标解析/探测出错，结果里这一条带 `error`，其余照常。
    """
    def probe_one(target: dict) -> dict:
        name, host, port = target.get("name"), target.get("host"), target.get("port")
        try:
            route = route_of(host, timeout=route_timeout)
            probe = tcp_probe(host, port, timeout=probe_timeout)
            return {
                "name": name,
                "host": host,
                "port": port,
                "error": None,
                "address_class": classify_address(host),
                "route": None if route is None else route.__dict__,
                "proxied": looks_proxied(route),
                "probe": {
                    "status": probe.status,
                    "elapsed_ms": probe.elapsed_ms,
                },
            }
        except Exception as exc:  # noqa: BLE001 - 单个目标出错不能拖垮整批采集
            return {"name": name, "host": host, "port": port, "error": str(exc)}

    results: list[dict] = []
    if not targets:
        return _ok(targets=results)
    with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers) as pool:
        futures = [pool.submit(probe_one, t) for t in targets]
        for fut in futures:
            results.append(fut.result())
    return _ok(targets=results)
