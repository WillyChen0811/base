"""macOS Time Machine 备份的读取支持 —— 识别备份路径、列 APFS 快照、只读挂载。

APFS 备份盘上每次备份是一个快照（`com.apple.TimeMachine.<日期>.backup`），要读里面的
文件得先挂出来。系统自己常已把某些快照挂在 `/Volumes/.timemachine/<卷UUID>/` 下，
这时再 `mount_apfs` 同一个快照会报 "Resource busy"——**先查挂载表复用已有挂载**，
没有才自己挂，且一律 `rdonly,nobrowse`：只读、不在 Finder 里冒出来。

挂载点下的结构是 `<挂载点>/<日期>.backup/<卷名>/Users/<用户>/...`，卷名随系统版本
不同（`Macintosh HD - Data`、`Data`），所以路径里的快照与用户靠 `inspect_selection`
识别，不写死。

只用标准库；调 `diskutil`/`mount_apfs`/`umount`，命令执行器可由调用方注入（测试用）。
"""

from __future__ import annotations

import os
import plistlib
import re
import subprocess
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Callable, Dict, List, Optional, Sequence

PREFIX = "com.apple.TimeMachine."

Runner = Callable[[Sequence[str]], str]


def _run(cmd: Sequence[str]) -> str:
    return subprocess.run(list(cmd), check=True, capture_output=True, text=True).stdout


# ---------------------------------------------------------------- 路径识别


@dataclass(frozen=True)
class Selection:
    """一个备份里的某个个人目录。`layout` 见 `inspect_selection`。"""

    layout: str
    snapshot_name: str
    snapshot_path: str
    source_root: str
    user_name: str

    def as_dict(self) -> dict:
        return asdict(self)


def inspect_selection(root: os.PathLike | str) -> Selection:
    """确认 `root` 是**单个**备份里 `Users/<用户>` 或其下级目录，并拆出快照与用户。

    认得四种布局：`apfs`（`/Volumes/.timemachine/...`）、`legacy`（`Backups.backupdb`）、
    `apfs-destination`（`/Volumes/<盘>/<日期>.previous/...`）、`apfs-mounted-snapshot`
    （自己挂到任意位置的 `<日期>.backup`）。要求落到 `Users/<用户>`，是为了防止一不小心
    把整盘所有历史备份都扫一遍。不认识就抛 `ValueError`。
    """
    parts = Path(root).parts
    lowered = [part.casefold() for part in parts]
    if ".timemachine" in lowered:
        marker, layout = lowered.index(".timemachine"), "apfs"
    elif "backups.backupdb" in lowered:
        marker, layout = lowered.index("backups.backupdb"), "legacy"
    elif len(parts) >= 4 and lowered[1] == "volumes" and any(_looks_dated(p) for p in parts[3:]):
        marker, layout = 2, "apfs-destination"
    elif any(p.endswith(".backup") for p in lowered[1:]):
        dated = next(i for i, p in enumerate(lowered) if p.endswith(".backup"))
        marker, layout = max(0, dated - 1), "apfs-mounted-snapshot"
    else:
        raise ValueError("不像 Time Machine 备份路径：没有 .timemachine、Backups.backupdb 或日期备份目录")

    users = [i for i in range(marker + 1, len(parts)) if lowered[i] == "users" and i + 1 < len(parts)]
    if not users:
        raise ValueError("要选单个备份里的 Users/<用户名> 或其下级目录，不能选整个备份历史")
    users_index = users[-1]
    user_name = parts[users_index + 1]
    if user_name.casefold() in {"shared", ".localized"}:
        raise ValueError("要选 Users 下某个用户的个人目录")

    snapshot_index = next((i for i in range(users_index - 1, marker, -1)
                           if lowered[i].endswith(".backup") or _looks_dated(parts[i])), None)
    if snapshot_index is None:
        # legacy: Backups.backupdb/<机器>/<快照>/<卷>/Users/...；APFS 也可能是 <UUID>/<快照>/Data/Users/...
        snapshot_index = marker + 2
        if snapshot_index >= users_index:
            raise ValueError("无法从路径识别出单个备份快照")
    return Selection(layout=layout, snapshot_name=parts[snapshot_index],
                     snapshot_path=str(Path(*parts[:snapshot_index + 1])),
                     source_root=Path(*parts[users_index:]).as_posix(), user_name=user_name)


def _looks_dated(value: str) -> bool:
    # 2024-01-31-120000、2024-01-31-120000.previous 这类
    return len(value) >= 17 and value[:4].isdigit() and value[4] == "-" and value[5:7].isdigit()


# ---------------------------------------------------------------- 快照与挂载


@dataclass(frozen=True)
class Snapshot:
    name: str           # 2025-11-28-095837.backup
    full_name: str      # com.apple.TimeMachine.2025-11-28-095837.backup
    xid: Optional[int] = None


def device_of(volume: os.PathLike | str, run: Runner = _run) -> str:
    """卷挂载点 → 设备节点（`/dev/disk7s2`）。"""
    return plistlib.loads(run(["diskutil", "info", "-plist", os.fspath(volume)]).encode())["DeviceNode"]


def list_snapshots(volume: os.PathLike | str, run: Runner = _run) -> List[Snapshot]:
    """备份卷上的 Time Machine 快照，按名字（即时间）排序；非 Time Machine 快照不列。"""
    data = plistlib.loads(run(["diskutil", "apfs", "listSnapshots", "-plist", os.fspath(volume)]).encode())
    out = []
    for item in data.get("Snapshots", []):
        full = item.get("SnapshotName", "")
        if full.startswith(PREFIX):
            out.append(Snapshot(full[len(PREFIX):], full, item.get("SnapshotXID")))
    return sorted(out, key=lambda s: s.name)


_MOUNT_LINE = re.compile(r"^(?P<snap>" + re.escape(PREFIX) + r"\S+)@(?P<dev>\S+) on (?P<path>.+) \((?P<opts>[^)]*)\)$")


def mounted_snapshots(run: Runner = _run) -> Dict[str, Path]:
    """当前已挂载的 Time Machine 快照：`<日期>.backup` → 挂载点（其下有同名目录）。"""
    out = {}
    for line in run(["mount"]).splitlines():
        m = _MOUNT_LINE.match(line.strip())
        if m:
            out[m.group("snap")[len(PREFIX):]] = Path(m.group("path"))
    return out


def mount_snapshot(device: str, snapshot: str, mount_point: os.PathLike | str, run: Runner = _run) -> Path:
    """只读挂载一个快照（`snapshot` 用短名 `<日期>.backup`）。挂载点不存在就建。"""
    mount_point = Path(mount_point)
    mount_point.mkdir(parents=True, exist_ok=True)
    run(["mount_apfs", "-o", "rdonly,nobrowse", "-s", PREFIX + snapshot, device, os.fspath(mount_point)])
    return mount_point


def unmount(mount_point: os.PathLike | str, run: Runner = _run) -> None:
    run(["umount", os.fspath(mount_point)])


def ensure_mounted(volume: os.PathLike | str, snapshots: Sequence[str], mount_root: os.PathLike | str,
                   run: Runner = _run) -> Dict[str, Path]:
    """保证这些快照都可读，返回 `<日期>.backup` → 挂载点。

    已挂着的（含系统自动挂的）直接复用；没挂的挂到 `mount_root/<日期>`。返回值里
    `mount_root` 下的那些才是本次挂的，用完调用方自己 `unmount`。快照不存在抛 `ValueError`。
    """
    existing = mounted_snapshots(run)
    available = {s.name for s in list_snapshots(volume, run)}
    missing = [s for s in snapshots if s not in available]
    if missing:
        raise ValueError(f"备份卷上没有这些快照：{', '.join(missing)}")
    device = None
    out = {}
    for snap in snapshots:
        if snap in existing:
            out[snap] = existing[snap]
            continue
        device = device or device_of(volume, run)
        out[snap] = mount_snapshot(device, snap, Path(mount_root) / snap.removesuffix(".backup"), run)
    return out
