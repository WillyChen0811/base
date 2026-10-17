"""显式 HTTP/HTTPS：http.client 的连接类，底层 socket 来自 wgnet.tcp。不依赖 requests，不跟随重定向。"""

from __future__ import annotations

import http.client
import ssl
from typing import Dict, Optional, Tuple
from urllib.parse import urlsplit

from .tcp import tcp


class _Conn(http.client.HTTPConnection):
    def connect(self) -> None:
        self.sock = tcp(self.host, self.port, self.timeout)
        self.sock.settimeout(self.timeout)


class _SConn(http.client.HTTPSConnection):
    def connect(self) -> None:
        raw = tcp(self.host, self.port, self.timeout)
        raw.settimeout(self.timeout)
        try:
            self.sock = self._context.wrap_socket(raw, server_hostname=self.host)   # SNI 用原主机名
        except BaseException:
            raw.close()
            raise


class Response:
    """status/reason/headers 立即可用；body 读全部，read(n) 流式读；用完 close()。"""

    def __init__(self, conn, resp) -> None:
        self._conn, self._resp = conn, resp
        self.status, self.reason, self.headers = resp.status, resp.reason, resp.headers
        self._body: Optional[bytes] = None

    def read(self, n: int = -1) -> bytes:
        return self._resp.read() if n < 0 else self._resp.read(n)

    @property
    def body(self) -> bytes:
        if self._body is None:
            self._body = self.read()
            self.close()
        return self._body

    def close(self) -> None:
        self._conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.close()


def request(method: str, url: str, headers: Optional[Dict[str, str]] = None, body=None,
            timeout: float = 30, stream: bool = False, context: Optional[ssl.SSLContext] = None) -> Response:
    """发一次请求。stream=False 先读完 body 并关连接；stream=True 由调用方 read()/close()。
    context 只用于 https（缺省系统证书校验）。"""
    u = urlsplit(url)
    if u.scheme not in ("http", "https") or not u.hostname:
        raise ValueError(f"只支持 http/https URL：{url!r}")
    port = u.port or (443 if u.scheme == "https" else 80)
    if u.scheme == "https":
        conn = _SConn(u.hostname, port, timeout=timeout, context=context or ssl.create_default_context())
    else:
        conn = _Conn(u.hostname, port, timeout=timeout)
    target = (u.path or "/") + (f"?{u.query}" if u.query else "")
    try:
        conn.request(method.upper(), target, body=body, headers=headers or {})
        r = Response(conn, conn.getresponse())
        if not stream:
            r.body
        return r
    except BaseException:
        conn.close()
        raise


def get(url: str, headers=None, **kw) -> Response:
    return request("GET", url, headers, **kw)


def post(url: str, body=None, headers=None, **kw) -> Response:
    return request("POST", url, headers, body, **kw)
