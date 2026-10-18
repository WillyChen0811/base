"""最小 SOCKS5 客户端握手（wgtund 程序级代理口：无认证，CONNECT / UDP ASSOCIATE）。

只做协议，不持有任何全局状态；使用方请走 `pybase.wgnet`。
域名**原样**交给代理（ATYP=3），本机不解析；代理拒绝或中途断开抛 `OSError`。
"""

from __future__ import annotations

import socket
import struct
from typing import Tuple

CONNECT, UDP_ASSOCIATE = 1, 3

_REPLY = {1: "代理服务器故障", 2: "规则不允许", 3: "网络不可达", 4: "主机不可达",
          5: "目标拒绝连接", 6: "TTL 过期", 7: "不支持的命令", 8: "不支持的地址类型"}


def parse_addr(addr: str) -> Tuple[str, int]:
    host, sep, port = addr.rpartition(":")
    if not sep or not host or not port.isdigit():
        raise ValueError(f"代理地址应为 host:port：{addr!r}")
    return host.strip("[]"), int(port)


def recv_exact(s: socket.socket, n: int) -> bytes:
    buf = b""
    while len(buf) < n:
        chunk = s.recv(n - len(buf))
        if not chunk:
            raise OSError("代理在握手中途关闭了连接")
        buf += chunk
    return buf


def encode_addr(host: str, port: int) -> bytes:
    """ATYP + 地址 + 端口；IP 字面量按 IP，其余一律当域名原样发。"""
    try:
        addr = b"\x01" + socket.inet_pton(socket.AF_INET, host)
    except OSError:
        try:
            addr = b"\x04" + socket.inet_pton(socket.AF_INET6, host)
        except OSError:
            name = host.encode("idna") if not host.isascii() else host.encode()
            if len(name) > 255:
                raise OSError(f"主机名过长：{host!r}")
            addr = b"\x03" + bytes([len(name)]) + name
    return addr + struct.pack(">H", port)


def decode_addr(buf: bytes, off: int = 0) -> Tuple[str, int, int]:
    """从 buf[off:] 解 ATYP+地址+端口，返回 (host, port, 下一个偏移)。"""
    atyp = buf[off]
    off += 1
    if atyp == 1:
        host, off = socket.inet_ntoa(buf[off:off + 4]), off + 4
    elif atyp == 4:
        host, off = socket.inet_ntop(socket.AF_INET6, buf[off:off + 16]), off + 16
    elif atyp == 3:
        n = buf[off]
        host, off = buf[off + 1:off + 1 + n].decode("utf-8", "replace"), off + 1 + n
    else:
        raise OSError(f"未知地址类型 {atyp}")
    return host, struct.unpack(">H", buf[off:off + 2])[0], off + 2


def negotiate(s: socket.socket, cmd: int, host: str, port: int) -> Tuple[str, int]:
    """在已连到代理的套接字上握手并发 cmd，返回代理回复里的绑定地址。失败抛 OSError（调用方负责关套接字）。"""
    s.sendall(b"\x05\x01\x00")
    ver, method = recv_exact(s, 2)
    if ver != 5 or method != 0:
        raise OSError(f"代理不接受无认证方式（回复 {ver},{method}）")
    s.sendall(bytes([5, cmd, 0]) + encode_addr(host, port))
    ver, rep, _, atyp = recv_exact(s, 4)
    if ver != 5:
        raise OSError("代理回复不是 SOCKS5")
    if rep != 0:
        raise OSError(f"代理拒绝 {host}:{port}：{_REPLY.get(rep, rep)}")
    rest = recv_exact(s, {1: 4, 4: 16}.get(atyp, 0) + 2) if atyp != 3 else None
    if atyp == 3:
        n = recv_exact(s, 1)
        rest = n + recv_exact(s, n[0] + 2)
    bhost, bport, _ = decode_addr(bytes([atyp]) + rest)
    return bhost, bport


def connect(proxy: Tuple[str, int], host: str, port: int, timeout: float = 10) -> socket.socket:
    """经 SOCKS5 代理连 host:port，域名原样发给代理。失败抛 OSError。"""
    try:
        s = socket.create_connection(proxy, timeout=timeout)
    except OSError as e:
        raise OSError(f"连不上本机代理 {proxy[0]}:{proxy[1]}：{e}") from e
    try:
        negotiate(s, CONNECT, host, port)
        s.settimeout(None)
        return s
    except BaseException:
        s.close()
        raise
