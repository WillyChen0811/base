"""隧道编排：wireguard-go 引擎 + UAPI 下发配置 + 接口地址/路由，一个 `Tunnel` 对应一份配置。

DNS（wg-quick 的 `DNS=`）第一步不改系统 DNS：只在 `status()` 里原样带出并标注
`dns_applied=False`——系统 DNS 是不是要接管、接管到什么程度是产品层决策，跟现有的
DNS 设置怎么共存也还没想清楚，这一层先不碰，只负责把值存下来传出去。
"""
from __future__ import annotations

import ipaddress
import socket as socket_mod
from dataclasses import dataclass

from pybase.net import routes
from pybase.net.engine import WireGuardGo
from pybase.net.wireguard import (
    DeviceStatus,
    WireGuardConfig,
    connect_uapi,
    split_endpoint,
    uapi_get,
    uapi_set,
)


class TunnelError(RuntimeError):
    """隧道 up() 编排失败；已尽量回滚已完成的步骤。"""


@dataclass(frozen=True)
class TunnelStatus:
    ifname: str | None
    engine_alive: bool
    device: DeviceStatus | None
    routes: tuple[str, ...]
    endpoint_routes: tuple[str, ...]
    dns: tuple[str, ...]
    dns_applied: bool  # 恒为 False：见模块说明，DNS 目前只记录、不生效


def _resolve_host(host: str) -> str | None:
    """endpoint 的主机部分可能是域名（wg-quick 允许），保护路由需要具体 IP。
    解析失败（离线/DNS 挂了）就返回 `None`，调用方跳过这个 peer 的保护而不是报错——
    endpoint 保护是锦上添花，不该因为解析不了域名就让 up() 整体失败。
    """
    for family in (socket_mod.AF_INET, socket_mod.AF_INET6):
        try:
            socket_mod.inet_pton(family, host)
            return host
        except OSError:
            continue
    try:
        infos = socket_mod.getaddrinfo(host, None)
    except socket_mod.gaierror:
        return None
    return infos[0][4][0] if infos else None


class Tunnel:
    """`up()`/`down()` 都是幂等的；`up()` 中途失败会尽量回滚已做的步骤再抛
    `TunnelError`（原始异常挂在 `__cause__` 上）。
    """

    def __init__(
        self,
        cfg: WireGuardConfig,
        *,
        runner: routes.Runner = routes.default_runner,
        engine: WireGuardGo | None = None,
        ifname_hint: str = "utun",
        platform: str | None = None,
    ) -> None:
        self._cfg = cfg
        self._runner = runner
        self._engine = engine or WireGuardGo()
        self._ifname_hint = ifname_hint
        self._platform = platform
        self._routes_added: list[str] = []
        self._endpoint_routes_added: list[str] = []
        self._up = False

    def up(self) -> None:
        if self._up:
            return  # 幂等
        stage = "start"
        try:
            ifname = self._engine.start(ifname_hint=self._ifname_hint)

            stage = "uapi_set"
            sock = connect_uapi(self._engine.sock_path)
            try:
                uapi_set(sock, self._cfg)
            finally:
                sock.close()

            stage = "addresses"
            routes.set_addresses(
                ifname, self._cfg.interface.addresses,
                mtu=self._cfg.interface.mtu, runner=self._runner, platform=self._platform,
            )
            routes.bring_up(ifname, runner=self._runner, platform=self._platform)

            stage = "routes"
            own_networks = {ipaddress.ip_interface(a).network for a in self._cfg.interface.addresses}
            for peer in self._cfg.peers:
                for cidr in peer.allowed_ips:
                    net = ipaddress.ip_network(cidr, strict=False)
                    if net in own_networks:
                        continue  # 跟接口自己的地址重复，不用再加路由
                    routes.add_route(cidr, ifname, runner=self._runner, platform=self._platform)
                    self._routes_added.append(cidr)

            stage = "endpoint_protection"
            for peer in self._cfg.peers:
                if not peer.endpoint:
                    continue
                host, _port = split_endpoint(peer.endpoint)
                ip = _resolve_host(host)
                if ip is None:
                    continue
                if not routes.needs_endpoint_protection(ip, peer.allowed_ips):
                    continue
                gw = routes.default_gateway(
                    ip_version=6 if ":" in ip else 4, runner=self._runner, platform=self._platform,
                )
                if gw:
                    routes.add_endpoint_host_route(ip, gw, runner=self._runner, platform=self._platform)
                    self._endpoint_routes_added.append(ip)
        except Exception as exc:
            self._teardown()
            raise TunnelError(f"隧道启动失败（阶段：{stage}）：{exc}") from exc
        self._up = True

    def _teardown(self) -> None:
        """尽量清理：每一步独立 try/except，前面失败不影响后面继续做。"""
        ifname = self._engine.ifname
        for ip in reversed(self._endpoint_routes_added):
            try:
                routes.remove_endpoint_host_route(ip, runner=self._runner, platform=self._platform)
            except Exception:
                pass
        self._endpoint_routes_added.clear()
        if ifname is not None:
            for cidr in reversed(self._routes_added):
                try:
                    routes.remove_route(cidr, ifname, runner=self._runner, platform=self._platform)
                except Exception:
                    pass
        self._routes_added.clear()
        try:
            self._engine.stop()
        except Exception:
            pass

    def down(self) -> None:
        if not self._up and not self._engine.alive:
            return  # 幂等
        self._teardown()
        self._up = False

    def status(self) -> TunnelStatus:
        device = None
        sock_path = self._engine.sock_path
        if self._engine.alive and sock_path:
            try:
                sock = connect_uapi(sock_path)
                try:
                    device = uapi_get(sock)
                finally:
                    sock.close()
            except OSError:
                device = None
        return TunnelStatus(
            ifname=self._engine.ifname,
            engine_alive=self._engine.alive,
            device=device,
            routes=tuple(self._routes_added),
            endpoint_routes=tuple(self._endpoint_routes_added),
            dns=tuple(self._cfg.interface.dns),
            dns_applied=False,
        )
