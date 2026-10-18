"""显式 UDP：经 wgtund 的 SOCKS5 UDP ASSOCIATE，域名原样交出。

控制 TCP 连接在 UDP 对象存活期间保持；关掉它代理会收回中继。
"""

from __future__ import annotations

import select
import socket
import struct
import time
from typing import Optional, Tuple

from .. import socks5
from .config import get_proxy, is_local, route


class UDPSocket:
    def __init__(self) -> None:
        self._ctrl: Optional[socket.socket] = None
        self._relay: Optional[Tuple[str, int]] = None
        self._proxied: Optional[socket.socket] = None     # 发往非本机目的
        self._direct: Optional[socket.socket] = None      # 本机目的 / 无代理且非 strict
        self._timeout: Optional[float] = None

    def settimeout(self, t: Optional[float]) -> None:
        self._timeout = t

    def _open_relay(self, timeout: float = 10) -> None:
        proxy = get_proxy()
        try:
            ctrl = socket.create_connection(proxy, timeout=timeout)
        except OSError as e:
            raise OSError(f"连不上本机代理 {proxy[0]}:{proxy[1]}：{e}") from e
        try:
            bhost, bport = socks5.negotiate(ctrl, socks5.UDP_ASSOCIATE, "0.0.0.0", 0)
            if bhost in ("0.0.0.0", "::", ""):
                bhost = proxy[0]
            ctrl.settimeout(None)
            fam = socket.AF_INET6 if ":" in bhost else socket.AF_INET
            s = socket.socket(fam, socket.SOCK_DGRAM)
        except BaseException:
            ctrl.close()
            raise
        self._ctrl, self._relay, self._proxied = ctrl, (bhost, bport), s

    def sendto(self, data: bytes, addr: Tuple[str, int]) -> int:
        host, port = addr
        if route(host) is None and (is_local(host) or get_proxy() is None):
            if self._direct is None:
                self._direct = socket.socket(socket.AF_INET6 if ":" in host else socket.AF_INET, socket.SOCK_DGRAM)
            return self._direct.sendto(data, (host, port))
        if self._proxied is None:
            self._open_relay()
        self._proxied.sendto(b"\x00\x00\x00" + socks5.encode_addr(host, port) + data, self._relay)
        return len(data)

    def recvfrom(self, bufsize: int = 65535) -> Tuple[bytes, Tuple[str, int]]:
        """收一个数据报，返回 (data, (host, port))；超时抛 socket.timeout。"""
        socks = [s for s in (self._proxied, self._direct) if s is not None]
        if not socks:
            raise OSError("还没有发过数据，没有可收的")
        deadline = None if self._timeout is None else time.monotonic() + self._timeout
        while True:
            wait = None if deadline is None else max(0.0, deadline - time.monotonic())
            ready, _, _ = select.select(socks + ([self._ctrl] if self._ctrl else []), [], [], wait)
            if not ready:
                raise socket.timeout("timed out")
            for s in ready:
                if s is self._ctrl:
                    if not s.recv(1):
                        raise OSError("代理关闭了 UDP 控制连接")
                    continue
                pkt, src = s.recvfrom(bufsize + 300)
                if s is self._direct:
                    return pkt, src
                if src[0] != self._relay[0] or len(pkt) < 10 or pkt[2] != 0:
                    continue          # 不是中继来的 / 分片：丢弃
                host, port, off = socks5.decode_addr(pkt, 3)
                return pkt[off:], (host, port)

    def close(self) -> None:
        for s in (self._proxied, self._direct, self._ctrl):
            if s is not None:
                try:
                    s.close()
                except OSError:
                    pass
        self._proxied = self._direct = self._ctrl = None

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.close()


def udp() -> UDPSocket:
    return UDPSocket()
