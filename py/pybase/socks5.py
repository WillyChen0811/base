"""最小 SOCKS5 客户端 + 进程级代理设置。

wgtund 的程序级接管口（无认证、CONNECT）。域名**原样**交给代理（ATYP=3），本机不解析；
设了代理就一律经代理，代理连不上或拒绝时抛 `OSError`，**绝不回落直连**。

    set_proxy("127.0.0.1:11080")   # 设；传空/None 取消
    sock = connect_via_proxy(host, port, timeout)   # 未设代理时返回 None
"""

from __future__ import annotations

import socket
import struct
import threading
from typing import Optional, Tuple

_lock = threading.Lock()
_proxy: Optional[Tuple[str, int]] = None

_REPLY = {1: "代理服务器故障", 2: "规则不允许", 3: "网络不可达", 4: "主机不可达",
          5: "目标拒绝连接", 6: "TTL 过期", 7: "不支持的命令", 8: "不支持的地址类型"}


def set_proxy(addr: Optional[str]) -> None:
    """addr 形如 "127.0.0.1:11080"；空值取消。"""
    global _proxy
    if not addr:
        with _lock:
            _proxy = None
        return
    host, sep, port = addr.rpartition(":")
    if not sep or not host or not port.isdigit():
        raise ValueError(f"代理地址应为 host:port：{addr!r}")
    with _lock:
        _proxy = (host.strip("[]"), int(port))


def get_proxy() -> Optional[str]:
    with _lock:
        return f"{_proxy[0]}:{_proxy[1]}" if _proxy else None


def _recv(s: socket.socket, n: int) -> bytes:
    buf = b""
    while len(buf) < n:
        chunk = s.recv(n - len(buf))
        if not chunk:
            raise OSError("代理在握手中途关闭了连接")
        buf += chunk
    return buf


def connect(proxy: Tuple[str, int], host: str, port: int, timeout: float = 10) -> socket.socket:
    """经 SOCKS5 代理连 host:port，域名原样发给代理。失败抛 OSError。"""
    try:
        s = socket.create_connection(proxy, timeout=timeout)
    except OSError as e:
        raise OSError(f"连不上本机代理 {proxy[0]}:{proxy[1]}：{e}") from e
    try:
        s.sendall(b"\x05\x01\x00")
        ver, method = _recv(s, 2)
        if ver != 5 or method != 0:
            raise OSError(f"代理不接受无认证方式（回复 {ver},{method}）")
        try:
            ip = socket.inet_pton(socket.AF_INET, host)
            addr = b"\x01" + ip
        except OSError:
            try:
                ip = socket.inet_pton(socket.AF_INET6, host)
                addr = b"\x04" + ip
            except OSError:
                name = host.encode("idna") if not host.isascii() else host.encode()
                if len(name) > 255:
                    raise OSError(f"主机名过长：{host!r}")
                addr = b"\x03" + bytes([len(name)]) + name
        s.sendall(b"\x05\x01\x00" + addr + struct.pack(">H", port))
        ver, rep, _, atyp = _recv(s, 4)
        if ver != 5:
            raise OSError("代理回复不是 SOCKS5")
        if rep != 0:
            raise OSError(f"代理拒绝连接 {host}:{port}：{_REPLY.get(rep, rep)}")
        skip = _recv(s, 1)[0] if atyp == 3 else {1: 4, 4: 16}.get(atyp, 0)
        _recv(s, skip + 2)  # 绑定地址 + 端口，用不上
        s.settimeout(None)
        return s
    except BaseException:
        s.close()
        raise


def connect_via_proxy(host: str, port: int, timeout: float = 10) -> Optional[socket.socket]:
    """已设代理则经代理建连接；未设返回 None（调用方自行直连）。"""
    with _lock:
        proxy = _proxy
    if proxy is None:
        return None
    return connect(proxy, host, port, timeout)
