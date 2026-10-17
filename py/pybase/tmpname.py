"""临时文件名候选与"名字被拒"判定：不依赖 smbprotocol，各 Storage 通用。

文件名上限按 UTF-8 字节算，贴着 255 字节的中文名追加后缀后会被服务端拒掉。规则：
首选**追加**后缀（原名完整保留，残留文件一眼看出是谁）；备选把扩展名**换成**短后缀，
临时名不长于最终名。不预判长度，只在服务端真的拒绝时才降级（各家上限口径不一）。
候选只由名字决定，同一目标每次算出的序列相同，续传才找得回来。
"""

import errno
import os
from typing import List, Optional, Tuple

# SMB STATUS_OBJECT_NAME_INVALID；写死数值以免为此依赖 smbprotocol
_NT_NAME_INVALID = 0xC0000033


def tmp_names(name: str, suffix: str, short_suffix: Optional[str] = None) -> List[str]:
    """同目录临时文件名候选，按优先级排列：`name+suffix`，再 `stem+short_suffix`。"""
    names = [name + suffix]
    stem, _ext = os.path.splitext(name)
    # 无扩展名（含 .gitignore 这类纯点名）时没有降级余地
    if stem and stem != name:
        alt = stem + (short_suffix if short_suffix is not None else suffix)
        if alt not in names:
            names.append(alt)
    return names


def tmp_paths(path: str, suffix: str, short_suffix: Optional[str] = None) -> List[str]:
    """`tmp_names` 的路径版：只变最后一段，目录部分原样保留（posix 分隔）。"""
    head, sep, name = path.rpartition("/")
    return [head + sep + n for n in tmp_names(name, suffix, short_suffix)]


def is_name_rejected(exc: BaseException) -> bool:
    """服务端/文件系统拒绝的是名字本身（过长、非法），而非权限、连接、磁盘问题。"""
    if not isinstance(exc, OSError):
        return False
    nt = getattr(exc, "ntstatus", None)
    if isinstance(nt, int):
        return (nt & 0xFFFFFFFF) == _NT_NAME_INVALID
    return exc.errno == errno.ENAMETOOLONG
