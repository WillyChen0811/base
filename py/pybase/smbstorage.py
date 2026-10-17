"""经 SMB 访问共享内的某个目录的 `Storage` 实现，建立在 `pybase.smb.SMBClient` 之上。

依赖 smbprotocol：`pip install "pybase[smb]"`（本模块 import 时不加载它，第一次连接才加载，
所以没装 smb 也能 import、序列化规格）。

- `share` 是共享名，`root` 是共享**内**的目录（可为空 = 共享根）；`Storage` 的相对路径都换算到
  `root` 下，含 `..` 或绝对路径一律 `ValueError`，跑不出 root。
- 连接、列目录、建目录走 `SMBClient`（`connect` / `list_dir` / `mkdir`）；`SMBClient` 没有的
  stat、流式读写、续写、删除、改名覆盖、改时间，直接用 `smbclient` 的同名函数，并共用
  `SMBClient` 实例的连接缓存（`client._cache`），不另开会话。
- `stat` 不存在返回 None；连接出问题抛 `OSError`，不会被当成"不存在"。
- `available()` 已连接时只看缓存里连接的 `transport.connected`（不发请求）；未连接或已断则先做一次
  TCP 探测（超时 `min(connect_timeout, 5)` 秒），通了再 `SMBClient.connect()`，成功 True（连接留着复用）、
  失败 False，从不抛异常。操作遇到断线同样自动重连一次。
- **凭据不进规格**（`smb://[user@]host[:port]/share/root`）。密码来源，优先级：构造参数 `password` >
  `set_credential_provider` 注册的回调 `provider(host, share, user, port) -> Optional[str]`。
  `SMBClient` 本身没有凭据存储，业务侧（如 `read_via_smb`）是把 server 配置里的用户名/密码直接传给构造函数，
  所以这里同样由调用方在启动时注册一个回调，让它去读自己的 SMB 配置；传输队列从库里还原出的 SMBStorage
  就靠这个回调拿到密码。
- 已经打开的读写句柄不跟着重连：断线时它们报错，由上层重试（见 `pybase.retry`）。
"""

from __future__ import annotations

import errno
import socket
import stat as stat_mod
import threading
from datetime import datetime
from pathlib import PurePosixPath
from typing import Any, BinaryIO, Callable, List, Optional
from urllib.parse import quote, unquote, urlsplit

from .storage import Entry, join

CredentialProvider = Callable[[str, str, Optional[str], int], Optional[str]]
_provider: Optional[CredentialProvider] = None


def set_credential_provider(fn: Optional[CredentialProvider]) -> None:
    """注册（或用 None 清除）密码回调：`fn(host, share, user, port) -> 密码或 None`。"""
    global _provider
    _provider = fn


def _tcp_probe(host: str, port: int, timeout: float) -> bool:
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except OSError:
        return False


class SMBStorage:
    def __init__(self, host: str, share: str, root: str = "", *, user: Optional[str] = None,
                 password: Optional[str] = None, port: int = 445, connect_timeout: float = 10,
                 client_factory: Optional[Callable[..., Any]] = None, ops: Any = None,
                 probe: Optional[Callable[[str, int, float], bool]] = None):
        """`client_factory(host, share, user, password, port)` 与 `ops`（提供 `smbclient` 的
        lstat/open_file/remove/rmdir/replace/utime）默认用真实实现，测试可替换。"""
        if not share or "/" in share or "\\" in share:
            raise ValueError(f"共享名不合法：{share!r}")
        rel = PurePosixPath((root or "").strip("/") or ".")
        if ".." in rel.parts:
            raise ValueError(f"root 不能含 ..：{root!r}")
        self.host, self.share, self.user, self.port = host, share, user, port
        self.root = "/".join(rel.parts)
        self._password = password
        self.connect_timeout = connect_timeout
        self._client_factory = client_factory
        self._ops = ops
        self._probe = probe or _tcp_probe
        self._client: Any = None
        self._lock = threading.RLock()

    def __repr__(self) -> str:
        return f"SMBStorage({self.user + '@' if self.user else ''}{self.host}:{self.port}/{self.share}, {self.root!r})"

    def __enter__(self) -> "SMBStorage":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    # ---- 连接 ----

    def _pw(self) -> str:
        if self._password is not None:
            return self._password
        if _provider is not None:
            return _provider(self.host, self.share, self.user, self.port) or ""
        return ""

    def _make_client(self):
        if self._client_factory is not None:
            return self._client_factory(self.host, self.share, self.user or "", self._pw(), self.port)
        from .smb.client import SMBClient
        return SMBClient(self.host, self.share, self.user or "", self._pw(), self.port)

    def _smb(self):
        if self._ops is not None:
            return self._ops
        import smbclient
        return smbclient

    def _kw(self, c, read: bool = False) -> dict:
        kw = {"connection_cache": c._cache}
        if read and self._ops is None:
            from .smb.common import _READ_ACCESS
            kw.update(share_access="rwd", desired_access=_READ_ACCESS)
        return kw

    def _alive(self) -> bool:
        c = self._client
        if c is None or not getattr(c, "_connected", False):
            return False
        conns = [v for v in getattr(c, "_cache", {}).values() if hasattr(v, "transport")]
        return all(getattr(v.transport, "connected", True) for v in conns)

    def _drop(self) -> None:
        c, self._client = self._client, None
        if c is not None:
            try:
                c.disconnect()
            except Exception:
                pass

    def _connect(self):
        with self._lock:
            if self._client is not None and self._alive():
                return self._client
            self._drop()
            client = self._make_client()
            try:
                client.connect()
            except Exception as e:
                try:
                    client.disconnect()
                except Exception:
                    pass
                raise OSError(f"SMB 连接 {self.host}:{self.port}/{self.share} 失败：{e}") from e
            self._client = client
            return client

    def close(self) -> None:
        with self._lock:
            self._drop()

    def available(self) -> bool:
        try:
            with self._lock:
                if self._alive():
                    return True
                if not self._probe(self.host, self.port, min(self.connect_timeout, 5)):
                    return False
                self._connect()
                return True
        except Exception:
            return False

    def _run(self, fn):
        """连接就绪后执行 `fn(client)`；通道已断时重连一次再试。远端的"不存在/权限"等 `OSError` 原样透传。"""
        for attempt in (0, 1):
            client = self._connect()
            try:
                return fn(client)
            except OSError:
                if attempt == 0 and not self._alive():
                    self._drop()
                    continue
                raise

    # ---- 路径 ----

    def _p(self, path: str) -> str:
        """相对 root 的路径 -> 相对共享根的 posix 路径；越界抛 ValueError。"""
        rel = PurePosixPath(path or ".")
        if rel.is_absolute() or ".." in rel.parts:
            raise ValueError(f"不安全的相对路径：{path!r}")
        return join(self.root, *rel.parts)

    def _unc(self, client, path: str) -> str:
        return client._unc(self._p(path))

    # ---- Storage ----

    def stat(self, path: str) -> Optional[Entry]:
        p = self._p(path)
        try:
            st = self._run(lambda c: self._smb().lstat(c._unc(p), connection_cache=c._cache))
        except FileNotFoundError:
            return None
        except OSError as e:
            if e.errno == errno.ENOENT:
                return None
            raise
        return Entry(PurePosixPath(p).name, path, stat_mod.S_ISDIR(st.st_mode or 0),
                     st.st_size or 0, st.st_mtime)

    def listdir(self, path: str = "") -> List[Entry]:
        p = self._p(path)
        return [Entry(e["name"], join(path, e["name"]), bool(e["is_dir"]), int(e.get("size") or 0),
                      _epoch(e.get("modified_at")))
                for e in self._run(lambda c: c.list_dir(p))]

    def open_read(self, path: str) -> BinaryIO:
        return self._run(lambda c: self._smb().open_file(self._unc(c, path), mode="rb", **self._kw(c, read=True)))

    def _mkparent(self, c, path: str) -> None:
        parent = str(PurePosixPath(self._p(path)).parent)
        if parent != ".":
            c.mkdir(parent)

    def open_write(self, path: str) -> BinaryIO:
        def go(c):
            self._mkparent(c, path)
            return self._smb().open_file(self._unc(c, path), mode="wb", **self._kw(c))
        return self._run(go)

    def open_resume(self, path: str, offset: int) -> BinaryIO:
        """见 `Storage.open_resume`。已存在：`r+b` 打开、截断到 `offset`、定位；不存在且 `offset==0` 新建。"""
        if offset < 0:
            raise ValueError(f"offset 不能为负：{offset}")

        def go(c):
            unc = self._unc(c, path)
            try:
                f = self._smb().open_file(unc, mode="r+b", **self._kw(c))
            except OSError as e:
                if not (isinstance(e, FileNotFoundError) or e.errno == errno.ENOENT) or offset:
                    raise
                self._mkparent(c, path)
                return self._smb().open_file(unc, mode="wb", **self._kw(c))
            try:
                size = f.seek(0, 2)
                if offset > size:
                    raise ValueError(f"{path} 只有 {size} 字节，无法从 {offset} 续写")
                f.truncate(offset)
                f.seek(offset)
            except BaseException:
                try:
                    f.close()
                except Exception:
                    pass
                raise
            return f
        return self._run(go)

    def replace(self, src: str, dst: str) -> None:
        self._run(lambda c: self._smb().replace(self._unc(c, src), self._unc(c, dst), **self._kw(c)))

    def remove(self, path: str) -> None:
        self._run(lambda c: self._smb().remove(self._unc(c, path), **self._kw(c)))

    def rmdir(self, path: str) -> None:
        def go(c):
            if c.list_dir(self._p(path)):    # 不依赖服务端对非空目录的报错码
                raise OSError(errno.ENOTEMPTY, "目录非空", path)
            self._smb().rmdir(self._unc(c, path), **self._kw(c))
        self._run(go)

    def set_mtime(self, path: str, mtime: float) -> None:
        try:
            self._run(lambda c: self._smb().utime(self._unc(c, path), times=(mtime, mtime), **self._kw(c)))
        except OSError:
            if not self._alive():
                raise                        # 断线要报；单纯服务端不让改就算了（尽力而为）


def _epoch(iso: Optional[str]) -> Optional[float]:
    if not iso:
        return None
    try:
        return datetime.fromisoformat(iso).timestamp()
    except ValueError:
        return None


# ---- 规格 ----

def smb_spec(s: SMBStorage) -> str:
    host = f"[{s.host}]" if ":" in s.host else s.host
    auth = quote(s.user, safe="") + "@" if s.user else ""
    port = f":{s.port}" if s.port != 445 else ""
    root = "/" + quote(s.root, safe="/") if s.root else ""
    return f"smb://{auth}{host}{port}/{quote(s.share, safe='')}{root}"


def smb_from_spec(spec: str) -> SMBStorage:
    u = urlsplit(spec)
    if not u.hostname:
        raise ValueError(f"smb 规格缺主机：{spec!r}")
    share, _, root = u.path.lstrip("/").partition("/")
    if not share:
        raise ValueError(f"smb 规格缺共享名：{spec!r}")
    return SMBStorage(u.hostname, unquote(share), unquote(root), user=unquote(u.username) if u.username else None,
                      port=u.port or 445)
