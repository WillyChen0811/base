"""逐文件的细粒度操作：stat、流式读写、续写、改名覆盖、删除、改时间。

`SMBClient` 的一半（见 `client.py` 的组装）。给 `pybase.smbstorage`（传输队列的块级断点续传）
这类需要**随机读写句柄**的调用方用；整份上传/下载仍走 `upload_file` / `download_file`。

与 `upload_file` 的分工：这里的 `open_write` / `open_resume` 直接写目标路径，**不做**临时文件替换，
临时名（如队列的 `.xferq.part`）与转正时机由调用方定，转正用 `replace`。

读句柄一律走 `_READ_ACCESS` + 共享读写删（与 `download_file` 同一套，避开根目录 ACL 不授 EA 读权限
导致的 ACCESS_DENIED，也不阻塞别的进程）。

混入方需要提供 `_ensure_connection()` / `_unc()` / `mkdir()` / `list_dir()` / `_cache` / `_ensured_dirs`。
"""

import errno
import stat as stat_mod
from typing import BinaryIO, Dict, Optional

import smbclient

from .common import _READ_ACCESS, _dt_iso


def _is_missing(e: OSError) -> bool:
    return isinstance(e, FileNotFoundError) or e.errno == errno.ENOENT


class StreamMixin:
    """stat、流式读写、续写、replace / remove / rmdir / set_mtime。"""

    def stat(self, path: str) -> Optional[Dict]:
        """单个路径的元信息，字段与 `list_dir` 一致，另带 `mtime`（epoch 秒，保留小数）。

        **不存在返回 None**；连接/权限等其它错误照常抛（不能当"不存在"，与 `exists` 三态里
        的 None 不同——这里 None 就是确定没有）。
        """
        self._ensure_connection()
        rel = (path or "").strip("/")
        try:
            st = smbclient.lstat(self._unc(rel), connection_cache=self._cache)
        except OSError as e:
            if _is_missing(e):
                return None
            raise
        is_dir = stat_mod.S_ISDIR(st.st_mode or 0)
        return {
            "name": rel.rsplit("/", 1)[-1],
            "path": rel,
            "is_dir": is_dir,
            "size": 0 if is_dir else (st.st_size or 0),
            "created_at": _dt_iso(getattr(st, "st_birthtime", None)),
            "modified_at": _dt_iso(st.st_mtime),
            "accessed_at": _dt_iso(st.st_atime),
            "mtime": st.st_mtime,
        }

    def open_read(self, path: str) -> BinaryIO:
        """只读二进制句柄（流式，不整份读进内存）。"""
        self._ensure_connection()
        return smbclient.open_file(
            self._unc(path), mode="rb", share_access="rwd",
            desired_access=_READ_ACCESS, connection_cache=self._cache,
        )

    def open_write(self, path: str) -> BinaryIO:
        """新建/截断写入，上级目录不存在自动建。"""
        self._ensure_connection()
        self._mkparent(path)
        return smbclient.open_file(self._unc(path), mode="wb", connection_cache=self._cache)

    def open_resume(self, path: str, offset: int) -> BinaryIO:
        """从 `offset` 处接着写：已存在则 `r+b` 打开、截断到 `offset` 并定位；不存在且
        `offset == 0` 新建（自动建上级目录）；不存在而 `offset > 0` 抛 `FileNotFoundError`；
        文件比 `offset` 短抛 `ValueError`（不补洞）。"""
        if offset < 0:
            raise ValueError(f"offset 不能为负：{offset}")
        self._ensure_connection()
        unc = self._unc(path)
        try:
            f = smbclient.open_file(unc, mode="r+b", connection_cache=self._cache)
        except OSError as e:
            if not _is_missing(e) or offset:
                raise
            self._mkparent(path)
            return smbclient.open_file(unc, mode="wb", connection_cache=self._cache)
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

    def replace(self, src: str, dst: str) -> None:
        """改名并**覆盖**已存在的 `dst`（`rename` 则不覆盖）。"""
        self._ensure_connection()
        smbclient.replace(self._unc(src), self._unc(dst), connection_cache=self._cache)

    def remove(self, path: str) -> None:
        """删文件。"""
        self._ensure_connection()
        smbclient.remove(self._unc(path), connection_cache=self._cache)

    def rmdir(self, path: str) -> None:
        """只删空目录，非空抛 `OSError(ENOTEMPTY)`（先自己列一遍，不依赖服务端对非空目录的报错码）。"""
        self._ensure_connection()
        rel = (path or "").strip("/")
        if self.list_dir(rel):
            raise OSError(errno.ENOTEMPTY, "目录非空", path)
        smbclient.rmdir(self._unc(rel), connection_cache=self._cache)
        # 目录没了，mkdir 的"已确认"缓存里也要去掉，否则之后往里写会跳过重建
        self._ensured_dirs = {d for d in self._ensured_dirs if d != rel and not d.startswith(rel + "/")}

    def set_mtime(self, path: str, mtime: float) -> None:
        """把访问/修改时间都设成 `mtime`（epoch 秒）。"""
        self._ensure_connection()
        smbclient.utime(self._unc(path), times=(mtime, mtime), connection_cache=self._cache)

    def _mkparent(self, path: str) -> None:
        parent = (path or "").strip("/").rpartition("/")[0]
        if parent:
            self.mkdir(parent)
