"""pybase.timemachine：备份路径识别、快照列表、挂载表解析、复用已挂载快照（命令执行器打桩）。"""

import os
import plistlib
import sys
from pathlib import Path

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

from pybase import timemachine as tm  # noqa: E402


def test_识别自己挂载的快照路径():
    s = tm.inspect_selection("/private/tmp/x/2025-11-28-095837.backup/Macintosh HD - Data/Users/alice/Desktop")
    assert s.layout == "apfs-mounted-snapshot"
    assert s.snapshot_name == "2025-11-28-095837.backup"
    assert s.user_name == "alice" and s.source_root == "Users/alice/Desktop"


def test_识别系统挂载与旧格式():
    s = tm.inspect_selection("/Volumes/.timemachine/UUID/2026-02-02-211444.backup/Data/Users/bob")
    assert s.layout == "apfs" and s.snapshot_name == "2026-02-02-211444.backup"
    s = tm.inspect_selection("/Volumes/TM/Backups.backupdb/mac/2020-01-01-000000/Macintosh HD/Users/bob")
    assert s.layout == "legacy" and s.snapshot_name == "2020-01-01-000000"


def test_不许扫整个备份历史():
    with pytest.raises(ValueError):
        tm.inspect_selection("/Volumes/.timemachine/UUID")
    with pytest.raises(ValueError):
        tm.inspect_selection("/Users/alice")


MOUNT = """/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
com.apple.TimeMachine.2025-10-27-121524.backup@/dev/disk7s2 on /Volumes/.timemachine/7063/2025-10-27-121524.backup (apfs, local, nodev, nosuid, read-only, journaled, noowners, nobrowse)
//u@10.0.0.1/XMST on /Volumes/XMST (smbfs, nodev, nosuid, mounted by u)
"""

SNAPS = plistlib.dumps({"Snapshots": [
    {"SnapshotName": "com.apple.TimeMachine.2025-11-28-095837.backup", "SnapshotXID": 1808},
    {"SnapshotName": "com.apple.TimeMachine.2025-10-27-121524.backup", "SnapshotXID": 480},
    {"SnapshotName": "com.apple.os.update-XYZ", "SnapshotXID": 1},
]}).decode()


class Runner:
    def __init__(self):
        self.calls = []

    def __call__(self, cmd):
        self.calls.append(list(cmd))
        if cmd[0] == "mount":
            return MOUNT
        if cmd[:3] == ["diskutil", "apfs", "listSnapshots"]:
            return SNAPS
        if cmd[:2] == ["diskutil", "info"]:
            return plistlib.dumps({"DeviceNode": "/dev/disk7s2"}).decode()
        return ""


def test_快照列表只列TimeMachine的():
    names = [s.name for s in tm.list_snapshots("/Volumes/backup", Runner())]
    assert names == ["2025-10-27-121524.backup", "2025-11-28-095837.backup"]


def test_挂载表解析():
    m = tm.mounted_snapshots(Runner())
    assert m == {"2025-10-27-121524.backup": Path("/Volumes/.timemachine/7063/2025-10-27-121524.backup")}


def test_已挂的复用_没挂的只读挂载(tmp_path):
    run = Runner()
    out = tm.ensure_mounted("/Volumes/backup", ["2025-10-27-121524.backup", "2025-11-28-095837.backup"],
                            tmp_path, run)
    assert out["2025-10-27-121524.backup"] == Path("/Volumes/.timemachine/7063/2025-10-27-121524.backup")
    assert out["2025-11-28-095837.backup"] == tmp_path / "2025-11-28-095837"
    mounts = [c for c in run.calls if c[0] == "mount_apfs"]
    assert mounts == [["mount_apfs", "-o", "rdonly,nobrowse", "-s",
                       "com.apple.TimeMachine.2025-11-28-095837.backup", "/dev/disk7s2",
                       str(tmp_path / "2025-11-28-095837")]]


def test_快照不存在要报错(tmp_path):
    with pytest.raises(ValueError):
        tm.ensure_mounted("/Volumes/backup", ["2099-01-01-000000.backup"], tmp_path, Runner())
