"""网络诊断的纯函数集合：查路由、判地址类型、启发式判断是否走了代理、探测端口连通性。

零业务依赖：不内置任何主机/端口默认值，只读系统命令与直连 socket，不改任何网络设置。
子进程调用复用 `pybase.media._proc`（超时/找可执行文件/整组杀掉子进程的机制不重复造）。
"""
from __future__ import annotations

import ipaddress
import os
import platform
import socket
import time
from dataclasses import dataclass
from typing import Literal, Optional

from ..media._proc import MediaError, ToolNotFoundError, resolve_tool, run

# mihomo/Clash 等内核默认的 fake-ip 网段（RFC 5735/2544 里的基准测试保留段，
# 被复用做 fake-ip 池）；不是标准协议常量，只是社区常见默认，调用方的内核如果
# 配了别的网段，`classify_address`/`looks_proxied` 判不准，这是启发式的固有局限。
_FAKE_IP_NET_V4 = ipaddress.ip_network("198.18.0.0/15")
_CGNAT_NET_V4 = ipaddress.ip_network("100.64.0.0/10")

AddressClass = Literal[
    "loopback", "link_local", "fake_ip", "cgnat", "private", "global",
]


@dataclass(frozen=True)
class RouteInfo:
    """一条路由查询的结果。`raw` 是原始命令输出，解析漏了字段时留作排查依据。

    `source` 只有 Linux `ip route get` 会给（`src`）；macOS `route get` 不输出
    源地址，这种情况下恒为 None，调用方不能把 None 当"查不到"处理。
    """

    host: str
    destination: Optional[str]
    gateway: Optional[str]
    interface: Optional[str]
    source: Optional[str]
    flags: Optional[str]
    raw: str


def route_of(host: str, *, timeout: float = 3.0) -> Optional[RouteInfo]:
    """查 `host`（必须是已解析的 IPv4/IPv6 字面量，不做域名解析）到本机的路由。

    命令本身失败（工具不存在、地址不可路由等）返回 None，不抛异常——诊断场景下
    "查不到路由"是正常结果之一，不该打断调用方的采集循环。
    """
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return None

    system = platform.system()
    try:
        if system == "Darwin":
            raw = _route_of_macos(ip, timeout)
        elif system == "Linux":
            raw = _route_of_linux(ip, timeout)
        else:
            return None
    except (ToolNotFoundError, MediaError):
        return None
    if raw is None:
        return None
    return raw


def _route_of_macos(ip, timeout: float) -> Optional[RouteInfo]:
    tool = resolve_tool(None, "route")
    cmd = [tool, "-n", "get"]
    if ip.version == 6:
        cmd.append("-inet6")
    cmd.append(str(ip))
    result = run(cmd, timeout=timeout)
    if result.returncode != 0:
        return None
    fields: dict[str, str] = {}
    for line in result.stdout.splitlines():
        line = line.strip()
        if not line or ":" not in line:
            continue
        key, _, value = line.partition(":")
        key = key.strip().lower()
        value = value.strip()
        if key in ("route to", "destination", "gateway", "interface", "flags"):
            fields[key] = value
    return RouteInfo(
        host=str(ip),
        destination=fields.get("destination"),
        gateway=fields.get("gateway"),
        interface=fields.get("interface"),
        source=None,
        flags=fields.get("flags"),
        raw=result.stdout,
    )


def _route_of_linux(ip, timeout: float) -> Optional[RouteInfo]:
    tool = resolve_tool(None, "ip")
    cmd = [tool]
    if ip.version == 6:
        cmd.append("-6")
    cmd += ["route", "get", str(ip)]
    result = run(cmd, timeout=timeout)
    if result.returncode != 0:
        return None
    first_line = next((l for l in result.stdout.splitlines() if l.strip()), "")
    tokens = first_line.split()
    if not tokens:
        return None
    destination = tokens[0]
    gateway = interface = source = None
    i = 1
    while i < len(tokens) - 1:
        key, value = tokens[i], tokens[i + 1]
        if key == "via":
            gateway = value
        elif key == "dev":
            interface = value
        elif key == "src":
            source = value
        i += 2
    return RouteInfo(
        host=str(ip),
        destination=destination,
        gateway=gateway,
        interface=interface,
        source=source,
        flags=None,
        raw=result.stdout,
    )


def classify_address(ip: str) -> Optional[AddressClass]:
    """按地址落在哪个特殊段分类；解析失败返回 None。

    判断顺序有讲究：fake-ip/cgnat 网段在 Python `ipaddress` 里也会被判成
    `is_private`（198.18.0.0/15 属于 IANA 保留段，100.64.0.0/10 反而不算
    private），所以更具体的类别必须排在 `is_private` 通用判断之前。
    """
    try:
        addr = ipaddress.ip_address(ip)
    except ValueError:
        return None
    if addr.is_loopback:
        return "loopback"
    if addr.is_link_local:
        return "link_local"
    if addr.version == 4 and addr in _FAKE_IP_NET_V4:
        return "fake_ip"
    if addr.version == 4 and addr in _CGNAT_NET_V4:
        return "cgnat"
    if addr.is_private:
        return "private"
    return "global"


def looks_proxied(route: Optional[RouteInfo]) -> bool:
    """启发式判断这条路由是否被本地代理/透明网关接管了。

    只看网关或（Linux 才有的）源地址是否落在 fake-ip 段——mihomo 等内核的 TUN
    模式典型表现就是网关指向 fake-ip 池；判不准是启发式的固有局限，不代表真相。
    """
    if route is None:
        return False
    for candidate in (route.gateway, route.source):
        if not candidate:
            continue
        if classify_address(candidate) == "fake_ip":
            return True
    return False


ProbeStatus = Literal["ok", "refused", "timeout", "unreachable"]


@dataclass(frozen=True)
class ProbeResult:
    """一次 TCP 连通性探测的结果。`elapsed_ms` 是 connect 耗时，探测失败时仍给出。"""

    host: str
    port: int
    status: ProbeStatus
    elapsed_ms: float


def tcp_probe(host: str, port: int, *, timeout: float = 3.0) -> ProbeResult:
    """直连 `host:port` 做一次 TCP 三次握手探测，不走任何代理（`socket` 直连）。

    只做 TCP，不做 ICMP：ICMP 在多数平台需要特权（raw socket），诊断场景下
    TCP 探测已经够用，不为了 ping 语义引入额外权限要求。
    """
    started = time.monotonic()
    try:
        with socket.create_connection((host, port), timeout=timeout):
            pass
        return ProbeResult(host, port, "ok", (time.monotonic() - started) * 1000)
    except socket.timeout:
        return ProbeResult(host, port, "timeout", (time.monotonic() - started) * 1000)
    except ConnectionRefusedError:
        return ProbeResult(host, port, "refused", (time.monotonic() - started) * 1000)
    except OSError:
        # 包含 EHOSTUNREACH/ENETUNREACH/域名解析失败等——诊断场景下统一归为不可达，
        # 具体原因调用方要细看可以自己再跑一次并捕获异常
        return ProbeResult(host, port, "unreachable", (time.monotonic() - started) * 1000)


def env_proxies() -> dict[str, Optional[str]]:
    """读环境变量里的代理设置，大小写变体都认（`HTTP_PROXY`/`http_proxy`）。"""
    keys = ("http_proxy", "https_proxy", "all_proxy", "no_proxy")
    result: dict[str, Optional[str]] = {}
    for key in keys:
        result[key] = os.environ.get(key) or os.environ.get(key.upper())
    return result
