"""wgtund 显式网络库：程序里要出网就明确调用它，不用系统 TCP/UDP，一切按 wgtund 的线路规则走。

    wgnet.configure("127.0.0.1:11080", strict=True)   # 也可只设环境变量 PYBASE_WGNET_PROXY / PYBASE_WGNET_STRICT
    s = wgnet.tcp(host, port, timeout=10)             # socket；域名原样交给 wgtund，本机不解析
    u = wgnet.udp(); u.sendto(b"..", (host, port)); data, addr = u.recvfrom(); u.close()
    r = wgnet.http.get(url) / wgnet.http.post(url, body) / wgnet.http.request(method, url, ...)
    r.status, r.headers, r.body        # stream=True 时用 r.read(n) / r.close()

规则：
- 本机目的（127.0.0.0/8、::1、localhost）直接连本机，不经代理。
- 其余一律经 wgtund；wgtund 连不上或拒绝就抛 OSError，**绝不回落系统直连**。
- 没配置 wgtund：strict=True（默认）抛错；strict=False 才用系统直连——给没有 wgtund 的
  pybase 使用者用（configure(None, strict=False)，或设 PYBASE_WGNET_STRICT=0）。
- configure 同时写环境变量，子进程继承即同样生效。
本库不替换任何系统函数（不 monkeypatch socket）；没改用它的调用仍走系统网络。
"""

from . import http
from .config import configure, get_proxy, is_strict
from .tcp import tcp
from .udp import UDPSocket, udp

__all__ = ["configure", "get_proxy", "is_strict", "tcp", "udp", "UDPSocket", "http"]
