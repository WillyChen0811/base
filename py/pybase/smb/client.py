"""SMB 客户端：连接、会话、路径，其余能力由同目录的三个 mixin 混进来。

按"对服务器做什么"分的家：

| 文件 | 收什么 |
|---|---|
| `common.py` | 常量与纯函数（超时、访问掩码、临时名、FILETIME 换算） |
| `listing.py` | 列目录、遍历、存在性 |
| `download.py` | 下载、远端流式哈希 |
| `upload.py` | 建目录、上传（原子替换 + 时间戳）、重命名 |
| `stream.py` | 单文件细粒度操作：stat、流式读写、续写、replace/remove/rmdir/set_mtime |
| `shares.py` | 列共享（全包唯一还用 pysmb 的地方） |

`SMBClient` 的对外方法签名与返回结构不变，业务代码不受拆分影响；
`list_shares` 仍从这里再导出，`from pybase.smb.client import ...` 照常能用。
"""

import logging

import smbclient

from .common import CONNECT_TIMEOUT
from .download import DownloadMixin
from .listing import ListingMixin
from .shares import list_shares            # noqa: F401  （对外再导出，勿删）
from .stream import StreamMixin
from .transport import ensure_connection
from .upload import UploadMixin

logger = logging.getLogger(__name__)


class SMBClient(ListingMixin, DownloadMixin, UploadMixin, StreamMixin):
    """SMB 客户端 (基于 smbprotocol/smbclient，支持 SMB3 加密与保留修改时间)

    对外方法签名与返回结构与旧 pysmb 版保持一致，业务代码无需改动。
    每个实例持有独立的 connection_cache，相互隔离（主线程扫描与后台上传线程互不干扰）。
    """

    def __init__(
        self,
        address: str,
        share: str,
        username: str = "",
        password: str = "",
        port: int = 445,
    ):
        self.address = address
        self.share = share
        self.username = username
        self.password = password
        self.port = port
        # 本实例独立的 smbclient 连接缓存（隔离不同实例/线程）
        self._cache: dict = {}
        self._connected = False
        self._encrypted = False
        # 本次连接已确认/创建过的远程目录，避免每个文件都重复 mkdir
        self._ensured_dirs: set = set()

    def __enter__(self):
        self.connect()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.disconnect()
        return False

    # ---------- 路径 ----------

    def _unc(self, path: str = "") -> str:
        """把相对共享根的路径转成 UNC：\\\\addr\\share\\path"""
        rel = (path or "").replace("/", "\\").strip("\\")
        base = f"\\\\{self.address}\\{self.share}"
        return f"{base}\\{rel}" if rel else base

    @property
    def _kw(self) -> dict:
        """所有 smbclient 带路径的文件调用统一展开它：连接缓存 + 端口。
        smbclient 查会话时 port 默认 445，经转发端口（如 1445）连接时不带就找不到会话。"""
        return {"connection_cache": self._cache, "port": self.port}

    # ---------- 连接 ----------

    def connect(self) -> bool:
        """连接到 SMB 服务器（优先 SMB3 加密，服务器不支持则回退非加密）"""
        user = self.username or None
        pwd = self.password or None
        # 固定走 NTLM：与旧 pysmb(NTLMv2) 一致、对 IP 直连/工作组最兼容。
        # 默认的 negotiate 在部分机器会先试 Kerberos，而 Kerberos 无法用于 IP 目标
        # （无 SPN），会导致 STATUS_ACCESS_DENIED。
        try:
            self._wg_connect()
            smbclient.register_session(
                self.address, username=user, password=pwd, port=self.port,
                encrypt=True, auth_protocol="ntlm", connection_cache=self._cache,
                connection_timeout=CONNECT_TIMEOUT,
            )
            self._encrypted = True
        except Exception as e:
            # TCP 都没建起来（主机不可达/超时）时直接抛：加密回退只对"服务器有响应
            # 但拒绝 SMB3 加密"有意义，对连不上的主机重试一轮只是再白等一个超时。
            if isinstance(e, ValueError) and "Failed to connect" in str(e):
                raise
            logger.info(f"SMB 加密连接失败，回退非加密重试: {e}")
            try:
                smbclient.delete_session(self.address, port=self.port, connection_cache=self._cache)
            except Exception:
                pass
            self._wg_connect()
            smbclient.register_session(
                self.address, username=user, password=pwd, port=self.port,
                encrypt=False, auth_protocol="ntlm", connection_cache=self._cache,
                connection_timeout=CONNECT_TIMEOUT,
            )
            self._encrypted = False
        self._connected = True
        logger.info(f"SMB 连接成功: {self.address}/{self.share} 加密={self._encrypted}")
        return True

    def _wg_connect(self):
        """底层 TCP 经 wgnet.tcp 建好放进缓存，register_session 复用它（见 transport.py）。"""
        ensure_connection(self._cache, self.address, self.port, CONNECT_TIMEOUT)

    def disconnect(self):
        """断开连接（关闭本实例缓存的会话）"""
        try:
            smbclient.delete_session(self.address, port=self.port, connection_cache=self._cache)
        except Exception:
            pass
        self._connected = False
        self._ensured_dirs.clear()

    def is_alive(self) -> bool:
        """已连接，且缓存里每条会话的 TCP 传输仍在。**不发请求**，便宜，可反复调用。"""
        if not self._connected:
            return False
        conns = [v for v in self._cache.values() if hasattr(v, "transport")]
        return all(getattr(v.transport, "connected", True) for v in conns)

    def _ensure_connection(self):
        """确保已注册会话；未连接则连接。失效连接由上传重试中的 disconnect+重连兜底。"""
        if not self._connected:
            self.connect()

    def _safe_delete(self, unc: str):
        """尽力删除远程文件，忽略不存在等错误"""
        try:
            smbclient.remove(unc, **self._kw)
        except Exception as e:
            logger.debug(f"删除远程文件忽略 {unc}: {e}")
