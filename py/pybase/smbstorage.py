"""经 SMB 访问共享内的某个目录的 `Storage` 实现，建立在 `pybase.smb.SMBClient` 之上。

依赖 smbprotocol：`pip install "pybase[smb]"`（本模块 import 时不加载它，第一次连接才加载，
所以没装 smb 也能 import、序列化规格）。

- `share` 是共享名，`root` 是共享**内**的目录（可为空 = 共享根）；`Storage` 的相对路径都换算到
  `root` 下，含 `..` 或绝对路径一律 `ValueError`，跑不出 root。
- 本模块只做 `Storage` 接口到 `SMBClient` 的适配：连接（固定 NTLM、10 秒建连超时）、认证、路径→UNC、
  stat / 流式读写 / 续写 / replace / remove / rmdir / set_mtime / 列目录 / 建目录，全是 `SMBClient`
  的方法（`pybase/smb/stream.py` 等），这里不直接碰 `smbclient`。本模块只管：相对路径锁在 root 下、
  凭据来源、断线重连一次、`Entry` 转换。
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

import threading
from datetime import datetime
from pathlib import PurePosixPath
from typing import Any, BinaryIO, Callable, List, Optional
from urllib.parse import quote, unquote, urlsplit

from . import wgnet
from .storage import Entry, join

CredentialProvider = Callable[[str, str, Optional[str], int], Optional[str]]
_provider: Optional[CredentialProvider] = None


def set_credential_provider(fn: Optional[CredentialProvider]) -> None:
    """注册（或用 None 清除）密码回调：`fn(host, share, user, port) -> 密码或 None`。"""
    global _provider
    _provider = fn


def _tcp_probe(host: str, port: int, timeout: float) -> bool:
    try:
        wgnet.tcp(host, port, timeout).close()
        return True
    except OSError:
        return False


class SMBStorage:
    def __init__(self, host: str, share: str, root: str = "", *, user: Optional[str] = None,
                 password: Optional[str] = None, port: int = 445, connect_timeout: float = 10,
                 client_factory: Optional[Callable[..., Any]] = None,
                 probe: Optional[Callable[[str, int, float], bool]] = None):
        """`client_factory(host, share, user, password, port)` 默认造真实 `SMBClient`，测试可替换。"""
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

    def _alive(self) -> bool:
        c = self._client
        return c is not None and c.is_alive()

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
                try:
                    from .smb.errors import smb_error      # 沿用 SMBClient 一侧的错误翻译（中文原因 + 底层码）
                    fe = smb_error(e)
                    why = f"{fe.user_message}（{fe.code}）" if fe.code else fe.user_message
                except Exception:
                    why = str(e)
                raise OSError(f"SMB 连接 {self.host}:{self.port}/{self.share} 失败：{why}") from e
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

    # ---- Storage ----

    def stat(self, path: str) -> Optional[Entry]:
        p = self._p(path)
        d = self._run(lambda c: c.stat(p))
        if d is None:
            return None
        return Entry(PurePosixPath(p).name, path, d["is_dir"], d["size"], d["mtime"])

    def listdir(self, path: str = "") -> List[Entry]:
        p = self._p(path)
        return [Entry(e["name"], join(path, e["name"]), bool(e["is_dir"]), int(e.get("size") or 0),
                      _epoch(e.get("modified_at")))
                for e in self._run(lambda c: c.list_dir(p))]

    def open_read(self, path: str) -> BinaryIO:
        p = self._p(path)
        return self._run(lambda c: c.open_read(p))

    def open_write(self, path: str) -> BinaryIO:
        p = self._p(path)
        return self._run(lambda c: c.open_write(p))

    def open_resume(self, path: str, offset: int) -> BinaryIO:
        """见 `Storage.open_resume`。"""
        if offset < 0:
            raise ValueError(f"offset 不能为负：{offset}")
        p = self._p(path)
        return self._run(lambda c: c.open_resume(p, offset))

    def replace(self, src: str, dst: str) -> None:
        s, d = self._p(src), self._p(dst)
        self._run(lambda c: c.replace(s, d))

    def remove(self, path: str) -> None:
        p = self._p(path)
        self._run(lambda c: c.remove(p))

    def rmdir(self, path: str) -> None:
        p = self._p(path)
        self._run(lambda c: c.rmdir(p))

    def set_mtime(self, path: str, mtime: float) -> None:
        p = self._p(path)
        try:
            self._run(lambda c: c.set_mtime(p, mtime))
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
