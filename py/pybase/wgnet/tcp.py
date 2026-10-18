"""显式 TCP：经 wgtund SOCKS5 CONNECT，域名原样交出，本机不解析。"""

from __future__ import annotations

import socket

from .. import socks5
from .config import route


def tcp(host: str, port: int, timeout: float = 10) -> socket.socket:
    """建到 host:port 的 TCP 连接。本机目的直连；其余经 wgtund，连不上抛 OSError，不回落直连。
    返回的套接字已清掉超时（阻塞模式）；要超时请自行 settimeout。"""
    proxy = route(host)
    if proxy is None:
        return socket.create_connection((host, port), timeout=timeout)
    return socks5.connect(proxy, host, port, timeout)
