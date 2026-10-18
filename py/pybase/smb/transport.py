"""SMB 传输层：两个第三方库自己建 TCP 的地方，改成 `wgnet.tcp`（线路、DNS 全由 wgtund 决定）。

不替换任何库的全局函数，只子类化、再通过库的正常 API 接进去：

- smbprotocol：`Connection.connect` 里写死 `self.transport = Tcp(...)`，没有注入口。
  `WgConnection` 用 `transport` 属性的 setter 把它换成 `WgTcp`（只改 `connect` 建 socket 那一处）；
  `ensure_connection` 先把这样一条连接放进 `connection_cache`，`smbclient.register_session`
  按缓存键 `server:port` 找到就直接复用，不再自己建。
- pysmb：`SMBConnection.connect` 同样写死 `socket.create_connection`，`WgSMBConnection` 重写 connect，
  只把建 socket 一行换成 `wgnet.tcp`，其余与库原文一致。
"""

from smb.SMBConnection import SMBConnection
from smbclient import ClientConfig
from smbprotocol.connection import Connection
from smbprotocol.transport import Tcp

from .. import wgnet


class WgTcp(Tcp):
    def connect(self):
        with self._sock_lock:
            if not self.connected:
                try:
                    self._sock = wgnet.tcp(self.server, self.port, self.timeout)
                except OSError as err:      # 与库原文同一异常口径，上层靠 "Failed to connect" 判断
                    raise ValueError(f"Failed to connect to '{self.server}:{self.port}': {err}") from err
                self._sock.settimeout(None)
                self.connected = True


class WgConnection(Connection):
    @property
    def transport(self):
        return self._wg_transport

    @transport.setter
    def transport(self, value):
        if isinstance(value, Tcp) and not isinstance(value, WgTcp):
            value = WgTcp(value.server, value.port, value.timeout)
        self._wg_transport = value


def ensure_connection(cache: dict, address: str, port: int, timeout: float, require_signing: bool = True) -> None:
    """缓存里没有可用连接时，建一条走 wgnet.tcp 的放进去。键与 smbclient 一致。"""
    key = f"{address.lower()}:{port}"
    conn = cache.get(key)
    if conn and conn.transport.connected:
        return
    conn = WgConnection(ClientConfig().client_guid, address, port, require_signing=require_signing)
    conn.connect(timeout=timeout)
    cache[key] = conn


class WgSMBConnection(SMBConnection):
    def connect(self, ip, port=139, sock_family=None, timeout=60):
        if self.sock:
            self.sock.close()
        self.auth_result = None
        self.sock = wgnet.tcp(ip, port, timeout)       # 库原文：socket.create_connection((ip, port), timeout=timeout)
        self.sock.settimeout(timeout)
        self.is_busy = True
        try:
            if not self.is_direct_tcp:
                self.requestNMBSession()
            else:
                self.onNMBSessionOK()
            while self.auth_result is None:
                self._pollForNetBIOSPacket(timeout)
        finally:
            self.is_busy = False
        return self.auth_result
