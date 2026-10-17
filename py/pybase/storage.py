"""存储接口 —— 把"连到哪"和"怎么读写/核对/续传"解耦。

上层逻辑（`pybase.fileops` 的校验式拷贝与删除、`pybase.inventory` 的清单比对、
`pybase.retry` 的等待恢复）只认 `Storage` 这几个方法，不关心数据是在本地盘、
挂载好的网络盘，还是走 SMB 协议 / HTTPS（WebDAV）直连；**连接**（WireGuard、
NetBird 隧道、局域网直连……）更是通道之下的事，换隧道不该动上层一行。

路径一律是**相对存储根的 posix 字符串**（`a/b/c.zip`，根本身是 `""`），由各实现
自己换算成本地路径、UNC 或 URL。

新通道照 `Storage` 写一个实现即可；`available()` 必须便宜（只看连接/挂载在不在，
不做大请求），等待恢复时会反复调用它。本模块只带 `LocalStorage`：本地目录与
任何已挂载的网络盘（smbfs、NFS……）都用它。
"""

from __future__ import annotations

import io
import os
from dataclasses import dataclass
import shutil
from contextlib import contextmanager
from pathlib import Path, PurePosixPath
from typing import BinaryIO, Iterator, List, Optional, Protocol, Union, runtime_checkable


@dataclass(frozen=True)
class Entry:
    """`name` 是末级名字；`path` 是相对存储根的 posix 路径；`mtime` 为 epoch 秒（拿不到为 None）。"""

    name: str
    path: str
    is_dir: bool
    size: int = 0
    mtime: Optional[float] = None


@runtime_checkable
class Storage(Protocol):
    def available(self) -> bool:
        """连接/挂载此刻可用吗。必须便宜、不抛异常。"""

    def stat(self, path: str) -> Optional[Entry]:
        """不存在返回 None；连接问题抛 `OSError`（不能当"不存在"）。"""

    def listdir(self, path: str = "") -> List[Entry]:
        """一层目录内容；读不了抛 `OSError`。"""

    def open_read(self, path: str) -> BinaryIO: ...

    def open_write(self, path: str) -> BinaryIO:
        """新建/截断写入；上级目录不存在由实现负责建。"""

    def open_resume(self, path: str, offset: int) -> BinaryIO:
        """从 `offset` 字节处接着写：打开已存在的文件、截断到 `offset` 并定位过去；
        文件不存在且 `offset == 0` 时新建（上级目录自动建）。文件不存在而 `offset > 0`
        抛 `FileNotFoundError`；文件比 `offset` 短抛 `ValueError`（不能凭空补洞）。"""

    def replace(self, src: str, dst: str) -> None:
        """原子改名（覆盖 `dst`）。上层用它把临时文件转正。"""

    def remove(self, path: str) -> None: ...

    def rmdir(self, path: str) -> None:
        """只删空目录，非空抛 `OSError`。"""

    def set_mtime(self, path: str, mtime: float) -> None:
        """尽力而为；通道不支持时什么都不做。"""


def join(*parts: str) -> str:
    """拼相对 posix 路径，空段忽略：`join("", "a", "b.txt") == "a/b.txt"`。"""
    return "/".join(p.strip("/") for p in parts if p and p.strip("/"))


def parent(path: str) -> str:
    p = str(PurePosixPath(path).parent)
    return "" if p == "." else p


class LocalStorage:
    """本地目录或已挂载的网络盘。

    `require_mount=True` 时，`available()` 还要求根（或其上级某一层）是挂载点本身
    在挂——网络盘掉线后挂载点目录常常还在、只是空的，光看"是不是目录"会误判成可用。
    做法：给 `mount_point`（如 `/Volumes/XMST`），`available()` 查 `os.path.ismount`。
    """

    def __init__(self, root: Union[str, os.PathLike], mount_point: Union[str, os.PathLike, None] = None):
        self.root = Path(root)
        self.mount_point = Path(mount_point) if mount_point else None

    def __repr__(self) -> str:
        return f"LocalStorage({str(self.root)!r})"

    def _p(self, path: str) -> Path:
        rel = PurePosixPath(path or ".")
        if rel.is_absolute() or ".." in rel.parts:
            raise ValueError(f"不安全的相对路径：{path!r}")
        return self.root.joinpath(*rel.parts)

    def available(self) -> bool:
        try:
            if self.mount_point is not None and not os.path.ismount(self.mount_point):
                return False
            return self.root.is_dir()
        except OSError:
            return False

    def stat(self, path: str) -> Optional[Entry]:
        p = self._p(path)
        try:
            st = p.stat(follow_symlinks=False)
        except FileNotFoundError:
            return None
        return Entry(p.name, path, p.is_dir() and not p.is_symlink(), st.st_size, st.st_mtime)

    def listdir(self, path: str = "") -> List[Entry]:
        out = []
        with os.scandir(self._p(path)) as it:
            for e in it:
                if e.is_symlink():
                    continue
                st = e.stat(follow_symlinks=False)
                out.append(Entry(e.name, join(path, e.name), e.is_dir(follow_symlinks=False),
                                 st.st_size, st.st_mtime))
        return out

    def open_read(self, path: str) -> BinaryIO:
        return open(self._p(path), "rb")

    def open_write(self, path: str) -> BinaryIO:
        p = self._p(path)
        p.parent.mkdir(parents=True, exist_ok=True)
        return open(p, "wb")

    def open_resume(self, path: str, offset: int) -> BinaryIO:
        if offset < 0:
            raise ValueError(f"offset 不能为负：{offset}")
        p = self._p(path)
        try:
            f = open(p, "r+b")
        except FileNotFoundError:
            if offset:
                raise
            p.parent.mkdir(parents=True, exist_ok=True)
            return open(p, "wb")
        try:
            size = f.seek(0, os.SEEK_END)
            if offset > size:
                raise ValueError(f"{path} 只有 {size} 字节，无法从 {offset} 续写")
            f.truncate(offset)
            f.seek(offset)
        except BaseException:
            f.close()
            raise
        return f

    def replace(self, src: str, dst: str) -> None:
        os.replace(self._p(src), self._p(dst))

    def remove(self, path: str) -> None:
        self._p(path).unlink()

    def rmdir(self, path: str) -> None:
        self._p(path).rmdir()

    def set_mtime(self, path: str, mtime: float) -> None:
        os.utime(self._p(path), (mtime, mtime), follow_symlinks=False)


# ---- 读进内存：不落盘的读法 ----
# 临时读一份文件（委托单、需求文档……）就读进内存解析；只有必须给真实路径的外部
# 工具才走 materialize（本地盘直接给原路径，其他通道拷进内存盘）。

def read_bytes(storage: Storage, path: str, *, max_bytes: Optional[int] = None) -> bytes:
    """经 `open_read` 读成 bytes。给了 `max_bytes` 且文件更大抛 `ValueError`
    （先看 stat；读的过程中也计数，防 stat 不准或拿不到）。"""
    if max_bytes is not None:
        ent = storage.stat(path)
        if ent is not None and ent.size > max_bytes:
            raise ValueError(f"{path} 大小 {ent.size} 超过上限 {max_bytes} 字节")
    buf = bytearray()
    chunk = 1 << 20
    with storage.open_read(path) as f:
        while True:
            want = chunk if max_bytes is None else min(chunk, max_bytes + 1 - len(buf))
            data = f.read(want)
            if not data:
                break
            buf += data
            if max_bytes is not None and len(buf) > max_bytes:
                raise ValueError(f"{path} 超过上限 {max_bytes} 字节")
    return bytes(buf)


def as_memory_file(storage: Storage, path: str, **kw) -> io.BytesIO:
    """`read_bytes` 的 BytesIO 版（定位在 0），可直接交给 python-docx / openpyxl。
    带 `.name`（原文件名），`pybase.sheets` 据此认扩展名。"""
    bio = io.BytesIO(read_bytes(storage, path, **kw))
    bio.name = PurePosixPath(path).name
    return bio


@contextmanager
def materialize(storage: Storage, path: str, *, ramdisk=None) -> Iterator[str]:
    """给"必须要真实文件路径"的工具用。`LocalStorage` 直接给本地真实路径（不复制）；
    其他实现把文件拷进内存盘（`ramdisk.get_workspace()` 下的随机子目录，保留原文件名），
    退出时删除。`ramdisk` 缺省取 `pybase.ramdisk.get_ram_disk()`；Windows 退化为磁盘目录
    沿用 ramdisk 自己的语义。"""
    if isinstance(storage, LocalStorage):
        yield str(storage._p(path))
        return
    if ramdisk is None:
        from .ramdisk import get_ram_disk
        ramdisk = get_ram_disk()
    work = ramdisk.get_workspace()
    try:
        dst = os.path.join(work, PurePosixPath(path).name)
        with storage.open_read(path) as src, open(dst, "wb") as out:
            shutil.copyfileobj(src, out, 1 << 20)
        yield dst
    finally:
        shutil.rmtree(work, ignore_errors=True)
