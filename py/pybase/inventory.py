"""目录树清单与比对 —— "这边的文件，那边有没有"，只看元数据、不读内容。

清单经 `pybase.storage.Storage` 列出，本地盘、挂载盘、SMB 协议、HTTPS 都一样。

判据分两级：

1. **相对路径 + 大小**一致 → `path`
2. 否则在对方树里找**同名 + 同大小**的文件 → `name`（对方整理过子目录时常见）
3. 都没有 → `None`

这只是快速判断，**不是内容证明**：同名同大小在通用文件（README、库文件）上可能撞，
需要证据时调用方再对命中的那一对算哈希（`pybase.fileops.hash_file`）。修改时间不参与
——拷贝、同步、SMB 都常把它改掉而内容不变。

跳过哪些名字、哪些目录由调用方给，本模块不内置项目约定；列不动的目录收进
`Listing.failed` 而不是吞掉——"这次没读到"不能被当成"那边没有"。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import PurePosixPath
from typing import Callable, Dict, Iterator, List, Optional, Tuple

from .storage import Storage


@dataclass
class Listing:
    """`files`：相对 `root` 的 posix 路径 → 大小；`failed`：没读到的目录（相对 `root`）。"""

    root: str
    files: Dict[str, int] = field(default_factory=dict)
    failed: List[str] = field(default_factory=list)


def walk_files(storage: Storage, root: str = "", *,
               skip_name: Optional[Callable[[str], bool]] = None,
               skip_dir: Optional[Callable[[str], bool]] = None) -> Listing:
    """递归列出 `root`（相对存储根）下的文件，键是相对 `root` 的路径。

    `skip_name(name)` 为真的文件、`skip_dir(rel_dir)` 为真的目录不进清单。
    """
    root = root.strip("/")
    out = Listing(root=root)
    stack = [""]
    while stack:
        rel_dir = stack.pop()
        try:
            entries = storage.listdir("/".join(p for p in (root, rel_dir) if p))
        except OSError:
            out.failed.append(rel_dir)
            continue
        for e in entries:
            rel = f"{rel_dir}/{e.name}" if rel_dir else e.name
            if e.is_dir:
                if not (skip_dir and skip_dir(rel)):
                    stack.append(rel)
            elif not (skip_name and skip_name(e.name)):
                out.files[rel] = e.size
    return out


class Index:
    """对方那棵（或几棵）树的查找表。`add` 时给个 `label`，命中后原样带回，方便定位。"""

    def __init__(self) -> None:
        self._by_path: Dict[Tuple[str, int], str] = {}
        self._by_name: Dict[Tuple[str, int], str] = {}

    def add(self, files: Dict[str, int], label: str = "") -> "Index":
        for rel, size in files.items():
            ref = f"{label}/{rel}" if label else rel
            self._by_path.setdefault((rel, size), ref)
            self._by_name.setdefault((PurePosixPath(rel).name, size), ref)
        return self

    def match(self, rel: str, size: int) -> Tuple[Optional[str], Optional[str]]:
        """返回 `(kind, ref)`：kind 为 `path` / `name` / None，ref 是对方那份的标识。"""
        if (rel, size) in self._by_path:
            return "path", self._by_path[(rel, size)]
        name = (PurePosixPath(rel).name, size)
        if name in self._by_name:
            return "name", self._by_name[name]
        return None, None


def compare(local: Dict[str, int], index: Index) -> Iterator[Tuple[str, int, Optional[str], Optional[str]]]:
    """逐个给出 `(rel, size, kind, ref)`，按相对路径排序。"""
    for rel in sorted(local):
        kind, ref = index.match(rel, local[rel])
        yield rel, local[rel], kind, ref
