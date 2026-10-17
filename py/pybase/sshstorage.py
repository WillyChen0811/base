"""经 SSH（SFTP）访问远端目录的 `Storage` 实现 —— 本机程序读写另一台机器上的工作目录。

依赖 paramiko：`pip install "pybase[ssh]"`。

- `root` 是远端**绝对路径**；`Storage` 的相对路径都换算到它下面，含 `..` 或绝对路径
  一律 `ValueError`，跑不出 root。
- `stat` 不存在返回 None；连接出问题抛 `OSError`，不会被当成"不存在"。
- `available()` 只看 SSH 通道是否还活着（`transport.is_active()`），不发请求、不抛异常；
  连接断了不会自己恢复，但**下一次操作会自动重连一次**（再失败才抛 `OSError`）。
- 认证走 ssh-agent 与 `~/.ssh` 下的默认钥匙，也可用 `key_filename` 指定；
  主机公钥核对 `~/.ssh/known_hosts`，未知主机默认拒绝。
- 已经打开的读写句柄不跟着重连：断线时它们报错，由上层重试（见 `pybase.retry`）。
"""

from __future__ import annotations

import errno
import os
import stat as stat_mod
import threading
from pathlib import PurePosixPath
from typing import BinaryIO, List, Optional

import paramiko

from .storage import Entry, join


class SSHStorage:
    def __init__(self, host: str, root: str, *, user: Optional[str] = None, port: int = 22,
                 key_filename: Optional[str] = None, connect_timeout: float = 10,
                 trust_unknown: bool = False):
        """`trust_unknown=True` 会自动接受 known_hosts 里没有的主机公钥（不做核对、不写回）：
        第一次连接时若被人中间人劫持，钥匙就被认下了。只用于测试或可信内网。"""
        if not root.startswith("/"):
            raise ValueError(f"root 必须是远端绝对路径：{root!r}")
        self.host = host
        self.root = root.rstrip("/") or "/"
        self.user = user
        self.port = port
        self.key_filename = key_filename
        self.connect_timeout = connect_timeout
        self.trust_unknown = trust_unknown
        self._client: Optional[paramiko.SSHClient] = None
        self._sftp: Optional[paramiko.SFTPClient] = None
        self._lock = threading.RLock()

    def __repr__(self) -> str:
        return f"SSHStorage({self.user + '@' if self.user else ''}{self.host}:{self.port}, {self.root!r})"

    def __enter__(self) -> "SSHStorage":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    # ---- 连接 ----

    def _alive(self) -> bool:
        t = self._client.get_transport() if self._client else None
        return bool(t and t.is_active())

    def _drop(self) -> None:
        for obj in (self._sftp, self._client):
            try:
                if obj:
                    obj.close()
            except Exception:
                pass
        self._sftp = self._client = None

    def _connect(self) -> paramiko.SFTPClient:
        with self._lock:
            if self._sftp is not None and self._alive():
                return self._sftp
            self._drop()
            client = paramiko.SSHClient()
            try:
                known = os.path.expanduser("~/.ssh/known_hosts")
                if os.path.exists(known):
                    client.load_host_keys(known)
                client.set_missing_host_key_policy(
                    paramiko.AutoAddPolicy() if self.trust_unknown else paramiko.RejectPolicy())
                client.connect(self.host, port=self.port, username=self.user,
                               key_filename=self.key_filename, allow_agent=True, look_for_keys=True,
                               timeout=self.connect_timeout, banner_timeout=self.connect_timeout,
                               auth_timeout=self.connect_timeout)
                sftp = client.open_sftp()
            except (paramiko.SSHException, EOFError, OSError) as e:
                client.close()
                raise OSError(f"SSH 连接 {self.host}:{self.port} 失败：{e}") from e
            self._client, self._sftp = client, sftp
            return sftp

    def close(self) -> None:
        with self._lock:
            self._drop()

    def available(self) -> bool:
        try:
            return self._alive()
        except Exception:
            return False

    def _run(self, fn):
        """在 SFTP 通道上执行 `fn(sftp)`；通道已断（连不上或中途死掉）时重连一次再试。
        SSH 层的错误统一转成 `OSError`；远端的"不存在/权限"等 `OSError` 原样透传。"""
        for attempt in (0, 1):
            sftp = self._connect()
            try:
                return fn(sftp)
            except (paramiko.SSHException, EOFError, OSError) as e:
                if not self._alive() and attempt == 0:
                    self._drop()
                    continue
                if isinstance(e, OSError):
                    raise
                raise OSError(f"SSH 通道出错：{e}") from e

    # ---- 路径 ----

    def _p(self, path: str) -> str:
        rel = PurePosixPath(path or ".")
        if rel.is_absolute() or ".." in rel.parts:
            raise ValueError(f"不安全的相对路径：{path!r}")
        return "/".join([self.root.rstrip("/")] + list(rel.parts)) if rel.parts else self.root

    # ---- Storage ----

    def stat(self, path: str) -> Optional[Entry]:
        p = self._p(path)
        try:
            st = self._run(lambda s: s.lstat(p))
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
        out = []
        for a in self._run(lambda s: s.listdir_attr(p)):
            mode = a.st_mode or 0
            if stat_mod.S_ISLNK(mode):
                continue
            out.append(Entry(a.filename, join(path, a.filename), stat_mod.S_ISDIR(mode),
                             a.st_size or 0, a.st_mtime))
        return out

    def open_read(self, path: str) -> BinaryIO:
        p = self._p(path)
        return self._run(lambda s: s.open(p, "rb"))

    def _makedirs(self, sftp: paramiko.SFTPClient, abs_dir: str) -> None:
        todo = []
        d = abs_dir
        while d not in ("", "/"):
            try:
                sftp.stat(d)
                break
            except FileNotFoundError:
                todo.append(d)
                d = str(PurePosixPath(d).parent)
        for d in reversed(todo):
            try:
                sftp.mkdir(d)
            except OSError:
                if not stat_mod.S_ISDIR(sftp.stat(d).st_mode or 0):   # 并发抢建没关系，目录在就行
                    raise

    def open_write(self, path: str) -> BinaryIO:
        p = self._p(path)

        def go(sftp):
            self._makedirs(sftp, str(PurePosixPath(p).parent))
            f = sftp.open(p, "wb")
            f.set_pipelined(True)   # 写不逐块等回执，快很多；写入错误在 close() 时才抛
            return f
        return self._run(go)

    def open_resume(self, path: str, offset: int) -> BinaryIO:
        """见 `Storage.open_resume`。已存在：`r+b` 打开、截断到 `offset`、定位；不存在且 `offset==0` 新建。"""
        if offset < 0:
            raise ValueError(f"offset 不能为负：{offset}")
        p = self._p(path)

        def go(sftp):
            try:
                f = sftp.open(p, "r+b")
            except OSError as e:
                if e.errno != errno.ENOENT or offset:
                    raise
                self._makedirs(sftp, str(PurePosixPath(p).parent))
                f = sftp.open(p, "wb")
                f.set_pipelined(True)
                return f
            try:
                size = f.stat().st_size or 0
                if offset > size:
                    raise ValueError(f"{path} 只有 {size} 字节，无法从 {offset} 续写")
                f.truncate(offset)
                f.seek(offset)
                f.set_pipelined(True)
            except BaseException:
                try:
                    f.close()
                except Exception:
                    pass
                raise
            return f
        return self._run(go)

    def replace(self, src: str, dst: str) -> None:
        a, b = self._p(src), self._p(dst)

        def go(sftp):
            try:
                sftp.posix_rename(a, b)      # OpenSSH 扩展：原子覆盖
                return
            except OSError as e:
                if e.errno is not None:      # ENOENT、权限等真错误，原样抛
                    raise
            # 服务端没有 posix-rename 扩展：先删目标再改名。删完到改名完成之间的
            # 那一刻 dst 不存在（非原子）；此时若断线，dst 会缺失而 src 仍在，可重做。
            sftp.lstat(a)                    # 源不在就别误删目标
            try:
                sftp.remove(b)
            except FileNotFoundError:
                pass
            sftp.rename(a, b)
        self._run(go)

    def remove(self, path: str) -> None:
        p = self._p(path)
        self._run(lambda s: s.remove(p))

    def rmdir(self, path: str) -> None:
        p = self._p(path)

        def go(sftp):
            if sftp.listdir(p):              # 有的服务端对非空目录 rmdir 不报错/报错码不统一
                raise OSError(errno.ENOTEMPTY, "目录非空", p)
            sftp.rmdir(p)
        self._run(go)

    def set_mtime(self, path: str, mtime: float) -> None:
        p = self._p(path)
        try:
            self._run(lambda s: s.utime(p, (mtime, mtime)))
        except OSError:
            if not self._alive():
                raise                        # 断线要报；单纯服务端不让改就算了（尽力而为）
