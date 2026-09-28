"""隧道接口的地址/路由管理：macOS 用 `ifconfig`/`route`，Linux 用 `ip`。

所有需要 root 权限的命令都经过注入的 `runner` 执行（默认 `subprocess.run`），调用方
可以换成 sudo 包装、特权助手进程，或者测试用的假 runner（断言命令序列）。
"""
from __future__ import annotations

import ipaddress
import re
import subprocess
import sys
from typing import Callable, Sequence

Runner = Callable[[Sequence[str]], subprocess.CompletedProcess]


class RouteError(RuntimeError):
    """执行 ifconfig/ip/route 命令失败（非 0 返回码）。"""


def default_runner(cmd: Sequence[str]) -> subprocess.CompletedProcess:
    return subprocess.run(list(cmd), capture_output=True, text=True)


def _run(runner: Runner, cmd: Sequence[str]) -> subprocess.CompletedProcess:
    result = runner(cmd)
    if result.returncode != 0:
        stderr = (getattr(result, "stderr", "") or "").strip()
        raise RouteError(f"命令失败：{' '.join(cmd)}\n{stderr}")
    return result


def _platform(explicit: str | None) -> str:
    return explicit or sys.platform


def set_addresses(
    ifname: str,
    addresses: Sequence[str],
    *,
    mtu: int | None = None,
    runner: Runner = default_runner,
    platform: str | None = None,
) -> None:
    """给接口配地址与 MTU。

    macOS 的 utun 是点对点接口，IPv4 要写成 `ifconfig utunN inet <ip> <ip> alias`
    （源和目的地址相同）；IPv6 走 `inet6 <ip> prefixlen <N>`。Linux 统一用
    `ip addr add <cidr> dev <ifname>`，不用区分点对点写法。
    """
    plat = _platform(platform)
    for addr in addresses:
        iface_addr = ipaddress.ip_interface(addr)
        if plat == "darwin":
            if iface_addr.version == 4:
                ip = str(iface_addr.ip)
                _run(runner, ["ifconfig", ifname, "inet", ip, ip, "alias"])
            else:
                _run(
                    runner,
                    ["ifconfig", ifname, "inet6", str(iface_addr.ip),
                     "prefixlen", str(iface_addr.network.prefixlen)],
                )
        else:
            _run(runner, ["ip", "addr", "add", str(iface_addr), "dev", ifname])
    if mtu is not None:
        if plat == "darwin":
            _run(runner, ["ifconfig", ifname, "mtu", str(mtu)])
        else:
            _run(runner, ["ip", "link", "set", "dev", ifname, "mtu", str(mtu)])


def bring_up(ifname: str, *, runner: Runner = default_runner, platform: str | None = None) -> None:
    plat = _platform(platform)
    if plat == "darwin":
        _run(runner, ["ifconfig", ifname, "up"])
    else:
        _run(runner, ["ip", "link", "set", "dev", ifname, "up"])


def add_route(
    network: str, ifname: str, *, runner: Runner = default_runner, platform: str | None = None
) -> None:
    plat = _platform(platform)
    net = ipaddress.ip_network(network, strict=False)
    family = "-inet6" if net.version == 6 else "-inet"
    if plat == "darwin":
        _run(runner, ["route", "-q", "-n", "add", family, str(net), "-interface", ifname])
    else:
        _run(runner, ["ip", "route", "add", str(net), "dev", ifname])


def remove_route(
    network: str, ifname: str, *, runner: Runner = default_runner, platform: str | None = None
) -> None:
    plat = _platform(platform)
    net = ipaddress.ip_network(network, strict=False)
    family = "-inet6" if net.version == 6 else "-inet"
    if plat == "darwin":
        _run(runner, ["route", "-q", "-n", "delete", family, str(net), "-interface", ifname])
    else:
        _run(runner, ["ip", "route", "del", str(net), "dev", ifname])


def default_gateway(
    *, ip_version: int = 4, runner: Runner = default_runner, platform: str | None = None
) -> str | None:
    """查当前（隧道之外的）默认网关，给 endpoint 保护用。查不到返回 `None`，不抛错——
    调用方决定查不到时是跳过保护还是报警。
    """
    plat = _platform(platform)
    if plat == "darwin":
        target = "-inet6" if ip_version == 6 else "-inet"
        result = runner(["route", "-n", "get", target, "default"])
        if result.returncode != 0:
            return None
        match = re.search(r"gateway:\s*(\S+)", result.stdout or "")
        return match.group(1) if match else None
    else:
        family = ["-6"] if ip_version == 6 else []
        result = runner(["ip", *family, "route", "show", "default"])
        if result.returncode != 0:
            return None
        match = re.search(r"default via (\S+)", result.stdout or "")
        return match.group(1) if match else None


def add_endpoint_host_route(
    endpoint_ip: str, gateway: str, *, runner: Runner = default_runner, platform: str | None = None
) -> None:
    """endpoint 保护：给 endpoint 的公网地址加一条经原默认网关的主机路由。

    当某个 peer 的 AllowedIPs 覆盖到它自己 endpoint 的地址时（比如以后接入
    `0.0.0.0/0` 做全局代理），不加这条路由的话，发往 endpoint 的 UDP 包会被
    引擎自己的隧道路由重新吞回隧道，握手包出不去。
    """
    plat = _platform(platform)
    addr = ipaddress.ip_address(endpoint_ip)
    cidr = f"{endpoint_ip}/32" if addr.version == 4 else f"{endpoint_ip}/128"
    family = "-inet6" if addr.version == 6 else "-inet"
    if plat == "darwin":
        _run(runner, ["route", "-q", "-n", "add", family, cidr, gateway])
    else:
        _run(runner, ["ip", "route", "add", cidr, "via", gateway])


def remove_endpoint_host_route(
    endpoint_ip: str, *, runner: Runner = default_runner, platform: str | None = None
) -> None:
    plat = _platform(platform)
    addr = ipaddress.ip_address(endpoint_ip)
    cidr = f"{endpoint_ip}/32" if addr.version == 4 else f"{endpoint_ip}/128"
    family = "-inet6" if addr.version == 6 else "-inet"
    if plat == "darwin":
        _run(runner, ["route", "-q", "-n", "delete", family, cidr])
    else:
        _run(runner, ["ip", "route", "del", cidr])


def needs_endpoint_protection(endpoint_ip: str, allowed_ips: Sequence[str]) -> bool:
    """`allowed_ips` 里是否有网段覆盖了 `endpoint_ip`（比如以后接入 `0.0.0.0/0`）。"""
    addr = ipaddress.ip_address(endpoint_ip)
    for cidr in allowed_ips:
        net = ipaddress.ip_network(cidr, strict=False)
        if net.version == addr.version and addr in net:
            return True
    return False
