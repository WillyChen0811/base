"""续传临时文件（`.xferq.part`）的命名、查找、写入与清理，供 fileops / transfer 用。

目标名顶满长度上限时追加 `.xferq.part` 会被服务端拒绝，于是降级为"换扩展名"的短后缀
（规则见 `tmpname`）。所有环节都按同一组候选走：找旧文件、开写、清理。
"""

from typing import BinaryIO, List, Sequence, Tuple

from .storage import Storage
from .tmpname import is_name_rejected, tmp_paths

RESUME_SUFFIX = ".xferq.part"
RESUME_SHORT_SUFFIX = ".xfq"
RESUME_SUFFIXES = (RESUME_SUFFIX, RESUME_SHORT_SUFFIX)


def resume_part_paths(dst_path: str) -> List[str]:
    """目标 `dst_path` 的续传临时文件候选路径，优先级从高到低；同一目标恒定不变。"""
    return tmp_paths(dst_path, RESUME_SUFFIX, RESUME_SHORT_SUFFIX)


def is_resume_part(name: str) -> bool:
    """文件名是否为续传临时文件（含降级后的短名），清理/跳过逻辑用。"""
    return name.endswith(RESUME_SUFFIXES)


def stat_part(storage: Storage, path: str):
    """`stat`，但名字本身被拒（过长）视为不存在——这样的名字上不可能有文件。"""
    try:
        return storage.stat(path)
    except OSError as e:
        if is_name_rejected(e):
            return None
        raise


def find_part(storage: Storage, parts: Sequence[str]) -> int:
    """按候选顺序找已存在的临时文件，返回下标；一个都没有返回 0。"""
    for i, p in enumerate(parts):
        if stat_part(storage, p) is not None:
            return i
    return 0


def drop_parts(storage: Storage, parts: Sequence[str]) -> None:
    """删掉所有候选名上存在的临时文件（含降级留下的），不留孤儿。"""
    for p in parts:
        if stat_part(storage, p) is not None:
            storage.remove(p)


def open_part(storage: Storage, parts: Sequence[str], idx: int, offset: int
              ) -> Tuple[BinaryIO, int, int]:
    """打开 `parts[idx]` 续写；名字被拒就换下一个候选（只有名字类错误才换，其余照抛）。

    换名后新名上还没有文件，偏移归零。返回 `(writer, 实际下标, 实际偏移)`。
    """
    while True:
        try:
            return storage.open_resume(parts[idx], offset), idx, offset
        except OSError as e:
            if not is_name_rejected(e) or idx + 1 >= len(parts):
                raise
            idx, offset = idx + 1, 0
