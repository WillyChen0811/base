"""全局网络接管：进程里所有 TCP 对外连接都经 wgtund 的 SOCKS5 口，由 wgtund 按线路规则选路。

    install()     # 替换 socket 底层入口；越早越好（任何对外连接之前）
    uninstall()   # 还原

做法：
- ``socket.getaddrinfo`` / ``gethostbyname`` / ``gethostbyname_ex``：非本机名字**不在本机解析**，
  返回 198.18.0.0/15 里分配的假 IP，进程内记「假 IP -> 域名」。
- ``socket.socket.connect`` / ``connect_ex``（仅 TCP 流式）：目的是假 IP 就还原成域名，否则用 IP，
  把这条套接字改连到代理口并做 SOCKS5 CONNECT，之后它就是到真实目的的通道。
  urllib、requests、paramiko、smbprotocol 等不用改。
- 例外只有本机：127.0.0.0/8、::1、localhost、AF_UNIX、代理口自己，照常直连。
- 代理地址取 ``socks5.set_proxy`` 设的，或环境变量 ``PYBASE_SOCKS5_PROXY``（子进程继承）。
  **代理地址未知或连不上一律抛 OSError，绝不回落系统直发。**

没覆盖：UDP（wgtund 只收 TCP；DNS 以外的 UDP 照常直发，对假 IP 的 UDP 直接报错）；
不经 Python socket 的出口（C 扩展自带网络栈如 libpq，Node/Swift/git/ssh 等外部进程）；
IPv6 套接字连非本机 IPv6 字面量（报错）。
子进程各自 install，各自分配假 IP，互不共享。
"""

from __future__ import annotations

import errno
import ipaddress
import socket
import threading
from typing import Dict, Optional, Tuple

from . import socks5

_BASE = int(ipaddress.IPv4Address("198.18.0.0"))
_SIZE = 1 << 17            # 198.18.0.0/15
_FAKE_NET = ipaddress.ip_network("198.18.0.0/15")
_HANDSHAKE_TIMEOUT = 10.0

_lock = threading.Lock()
_name2ip: Dict[str, str] = {}
_ip2name: Dict[str, str] = {}
_next = 1                  # 0 号不用
_orig: Dict[str, object] = {}


def _is_local_name(host: str) -> bool:
    h = host.lower().rstrip(".")
    return h in ("localhost", "localhost.localdomain", "ip6-localhost") or h.endswith(".localhost")


def _ip_of(host: str) -> Optional[ipaddress._BaseAddress]:
    try:
        return ipaddress.ip_address(host.split("%", 1)[0])
    except ValueError:
        return None


def _is_local_ip(ip) -> bool:
    if getattr(ip, "ipv4_mapped", None):
        ip = ip.ipv4_mapped
    return ip.is_loopback or ip.is_unspecified


def _fake_ip(name: str) -> str:
    global _next
    key = name.lower()
    with _lock:
        ip = _name2ip.get(key)
        if ip:
            return ip
        ip = str(ipaddress.IPv4Address(_BASE + _next))
        _next = _next % (_SIZE - 2) + 1
        old = _ip2name.pop(ip, None)       # 地址池绕了一圈：挤掉最老的
        if old:
            _name2ip.pop(old, None)
        _name2ip[key] = ip
        _ip2name[ip] = key
        return ip


def _needs_fake(host) -> bool:
    """host 是需要代理解析的名字（非空、非 IP 字面量、非本机名）。"""
    if isinstance(host, bytes):
        host = host.decode("idna", "ignore")
    return bool(host) and _ip_of(host) is None and not _is_local_name(host)


def _port_num(port) -> int:
    if port is None or port == "":
        return 0
    if isinstance(port, int) or str(port).isdigit():
        return int(port)
    return socket.getservbyname(str(port))


def _getaddrinfo(host, port, family=0, type=0, proto=0, flags=0):
    if not _needs_fake(host):
        return _orig["getaddrinfo"](host, port, family, type, proto, flags)
    if isinstance(host, bytes):
        host = host.decode("idna", "ignore")
    if family not in (0, socket.AF_INET):
        raise socket.gaierror(socket.EAI_FAMILY, "全局接管下名字只给 IPv4 假地址")
    ip = _fake_ip(host)
    p = _port_num(port)
    canon = host if flags & socket.AI_CANONNAME else ""
    types = [type] if type else [socket.SOCK_STREAM]
    return [(socket.AF_INET, t, proto, canon, (ip, p)) for t in types]


def _gethostbyname(name):
    if not _needs_fake(name):
        return _orig["gethostbyname"](name)
    return _fake_ip(name)


def _gethostbyname_ex(name):
    if not _needs_fake(name):
        return _orig["gethostbyname_ex"](name)
    return (name, [], [_fake_ip(name)])


def _proxy_addr() -> Optional[Tuple[str, int]]:
    addr = socks5.get_proxy()
    return socks5._parse(addr) if addr else None


def _target(sock, address):
    """返回 (host, port) 表示要经代理；返回 None 表示放行直连。"""
    if sock.family not in (socket.AF_INET, socket.AF_INET6) or not isinstance(address, tuple) or len(address) < 2:
        return None
    host, port = address[0], address[1]
    if isinstance(host, bytes):
        host = host.decode("idna", "ignore")
    if not isinstance(host, str) or not isinstance(port, int):
        return None
    if host in ("", "<broadcast>"):
        return None
    ip = _ip_of(host)
    if ip is not None:
        if _is_local_ip(ip):
            return None
        if ip in _FAKE_NET:
            with _lock:
                name = _ip2name.get(str(ip))
            if name is None:
                raise OSError(errno.EHOSTUNREACH, f"未知的假地址 {ip}（不是本进程分配的）")
            if sock.type != socket.SOCK_STREAM:
                raise OSError(errno.ENETUNREACH, "全局接管不支持 UDP 连接域名目的")
            return name, port
        host = str(ip)
    elif _is_local_name(host):
        return None
    if sock.type != socket.SOCK_STREAM:
        return None
    pr = _proxy_addr()
    if pr is not None and (host, port) == pr:
        return None
    return host, port


def _connect(self, address):
    tgt = _target(self, address)
    if tgt is None:
        return _orig["connect"](self, address)
    host, port = tgt
    proxy = _proxy_addr()
    if proxy is None:
        raise OSError(errno.ENETUNREACH, f"全局接管已开启但没有代理地址（wgtund 未就绪），拒绝直连 {host}:{port}")
    if self.family != socket.AF_INET:
        raise OSError(errno.EAFNOSUPPORT, "全局接管下连外部目的需要 IPv4 套接字")
    old = self.gettimeout()
    try:
        # 握手期间一律阻塞 + 超时（非阻塞套接字如 asyncio 也在这里短暂阻塞一下）
        self.settimeout(old if old else _HANDSHAKE_TIMEOUT)
        try:
            _orig["connect"](self, proxy)
        except OSError as e:
            raise OSError(e.errno, f"连不上本机代理 {proxy[0]}:{proxy[1]}：{e}（不回落直连）") from e
        socks5.handshake(self, host, port)
    finally:
        try:
            self.settimeout(old)
        except OSError:
            pass


def _connect_ex(self, address):
    try:
        _connect(self, address)
    except OSError as e:
        return e.errno or errno.ECONNREFUSED
    return 0


def install() -> None:
    """幂等。"""
    if _orig:
        return
    _orig.update(getaddrinfo=socket.getaddrinfo, gethostbyname=socket.gethostbyname,
                 gethostbyname_ex=socket.gethostbyname_ex,
                 connect=socket.socket.connect, connect_ex=socket.socket.connect_ex)
    socket.getaddrinfo = _getaddrinfo
    socket.gethostbyname = _gethostbyname
    socket.gethostbyname_ex = _gethostbyname_ex
    socket.socket.connect = _connect
    socket.socket.connect_ex = _connect_ex


def uninstall() -> None:
    if not _orig:
        return
    socket.getaddrinfo = _orig["getaddrinfo"]
    socket.gethostbyname = _orig["gethostbyname"]
    socket.gethostbyname_ex = _orig["gethostbyname_ex"]
    socket.socket.connect = _orig["connect"]
    socket.socket.connect_ex = _orig["connect_ex"]
    _orig.clear()


def installed() -> bool:
    return bool(_orig)
