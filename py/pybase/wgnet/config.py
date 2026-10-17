"""wgnet 配置：wgtund 的 SOCKS5 口地址 + strict 开关；同步写环境变量给子进程继承。"""

from __future__ import annotations

import ipaddress
import os
import threading
from typing import Optional, Tuple

from .. import socks5

ENV_PROXY = "PYBASE_WGNET_PROXY"
ENV_STRICT = "PYBASE_WGNET_STRICT"

_lock = threading.Lock()
_proxy: Optional[Tuple[str, int]] = None
_strict: Optional[bool] = None      # None = 取环境变量，再缺省 True


def configure(proxy: Optional[str] = "127.0.0.1:11080", strict: bool = True) -> None:
    """设 wgtund 代理口。proxy 为空则视为未配置（环境变量一并清掉）。"""
    global _proxy, _strict
    p = socks5.parse_addr(proxy) if proxy else None
    with _lock:
        _proxy, _strict = p, bool(strict)
        os.environ[ENV_STRICT] = "1" if strict else "0"
        if proxy:
            os.environ[ENV_PROXY] = f"{p[0]}:{p[1]}"
        else:
            os.environ.pop(ENV_PROXY, None)


def get_proxy() -> Optional[Tuple[str, int]]:
    with _lock:
        if _proxy is not None:
            return _proxy
    env = os.environ.get(ENV_PROXY)
    return socks5.parse_addr(env) if env else None


def is_strict() -> bool:
    with _lock:
        if _strict is not None:
            return _strict
    return os.environ.get(ENV_STRICT, "1").strip().lower() not in ("0", "false", "no", "")


def is_local(host: str) -> bool:
    """本机目的：127.0.0.0/8、::1、localhost。"""
    h = host.strip("[]").lower().rstrip(".")
    if h == "localhost" or h.endswith(".localhost"):
        return True
    try:
        return ipaddress.ip_address(h).is_loopback
    except ValueError:
        return False


def route(host: str) -> Optional[Tuple[str, int]]:
    """返回该目的该走的代理；None 表示系统直连（本机目的，或 strict=False 且没配置）。
    strict 且没配置抛 OSError。"""
    if is_local(host):
        return None
    p = get_proxy()
    if p is None and is_strict():
        raise OSError("wgnet 没有配置 wgtund 代理口（strict 模式不允许直连），请先 wgnet.configure(...)")
    return p
