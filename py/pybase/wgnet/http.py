"""显式 HTTP/HTTPS：http.client 的连接类，底层 socket 来自 wgnet.tcp。不依赖 requests；像 urllib 一样跟随重定向（最多 5 次）。"""

from __future__ import annotations

import http.client
import json
import ssl
from typing import Dict, Optional, Tuple
from urllib.parse import urljoin, urlsplit

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

    def readline(self) -> bytes:
        return self._resp.readline()

    def __iter__(self):
        return iter(self._resp)

    def json(self):
        return json.loads(self.body.decode("utf-8"))

    @property
    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")

    def close(self) -> None:
        self._resp.close()      # Connection: close / HTTP/1.0 时套接字归响应持有，只关连接关不到
        self._conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.close()


_REDIRECT = (301, 302, 303, 307, 308)
MAX_REDIRECTS = 5


def request(method: str, url: str, headers: Optional[Dict[str, str]] = None, body=None,
            timeout: float = 30, stream: bool = False, context: Optional[ssl.SSLContext] = None) -> Response:
    """发一次请求。stream=False 先读完 body 并关连接；stream=True 由调用方 read()/close()。
    context 只用于 https（缺省系统证书校验）。跟随重定向同 urllib：303 及 POST 遇 301/302 改 GET 去掉正文，
    307/308 原样重发。"""
    for _ in range(MAX_REDIRECTS + 1):
        r = _once(method, url, headers, body, timeout, stream, context)
        loc = r.headers.get("Location")
        if r.status not in _REDIRECT or not loc:
            return r
        r.close()
        url = urljoin(url, loc)
        if r.status == 303 or (r.status in (301, 302) and method.upper() == "POST"):
            method, body = "GET", None
    raise OSError(f"重定向超过 {MAX_REDIRECTS} 次：{url}")


def _once(method, url, headers, body, timeout, stream, context) -> Response:
    u = urlsplit(url)
    if u.scheme not in ("http", "https") or not u.hostname:
        raise ValueError(f"只支持 http/https URL：{url!r}")
    port = u.port or (443 if u.scheme == "https" else 80)
    if u.scheme == "https":
        conn = _SConn(u.hostname, port, timeout=timeout, context=context or ssl.create_default_context())
    else:
        conn = _Conn(u.hostname, port, timeout=timeout)
    target = (u.path or "/") + (f"?{u.query}" if u.query else "")
    r = None
    try:
        conn.request(method.upper(), target, body=body, headers=headers or {})
        r = Response(conn, conn.getresponse())
        if not stream:
            r.body
        return r
    except BaseException:
        if r is not None:
            r.close()       # 套接字可能归响应持有（Connection: close），只关连接关不到
        conn.close()
        raise


def get(url: str, headers=None, **kw) -> Response:
    return request("GET", url, headers, **kw)


def post(url: str, body=None, headers=None, **kw) -> Response:
    return request("POST", url, headers, body, **kw)
