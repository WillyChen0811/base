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

import os
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import BinaryIO, List, Optional, Protocol, Union, runtime_checkable


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

    def replace(self, src: str, dst: str) -> None:
        os.replace(self._p(src), self._p(dst))

    def remove(self, path: str) -> None:
        self._p(path).unlink()

    def rmdir(self, path: str) -> None:
        self._p(path).rmdir()

    def set_mtime(self, path: str, mtime: float) -> None:
        os.utime(self._p(path), (mtime, mtime), follow_symlinks=False)
