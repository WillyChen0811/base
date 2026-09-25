"""SMB 这一层的常量与纯函数：连接参数、访问掩码、临时文件名、FILETIME 换算。

**不 import smbclient、不碰连接**——这里全是可以单独推敲的小算术与命名规则，
`client` / `listing` / `download` / `upload` 各取所需。
"""

import os
from datetime import datetime
from typing import List, Optional

# NtStatus：判定“服务端嫌文件名不合法”这一种失败，见 _is_name_rejected
from smbprotocol.exceptions import NtStatus
from smbprotocol.open import FilePipePrinterAccessMask

# 建连超时（秒）。smbprotocol 默认 60，而 connect() 在加密握手失败时会原样重试一次
# 非加密，一台不可达的服务器最坏能卡满 2×60 秒；RPC 是 stdio 单线程，这段卡顿会
# 堵住整根管道、拖垮所有其它调用。局域网内 10 秒足够，不可达就该快速失败。
CONNECT_TIMEOUT = 10

_FILE_ATTRIBUTE_DIRECTORY = 0x10
# 最小读权限：仅列目录/读数据 + 读属性。
# smbclient 默认还会请求 FILE_READ_EA（读扩展属性），但有些共享（尤其根目录）的 ACL
# 不授予 EA 读权限，会导致打开即 STATUS_ACCESS_DENIED。pysmb 不请求 EA 故能通过。
_READ_ACCESS = (
    FilePipePrinterAccessMask.FILE_READ_DATA
    | FilePipePrinterAccessMask.FILE_READ_ATTRIBUTES
)
_UPLOAD_CHUNK = 1024 * 1024          # 上传分块大小
_PROGRESS_THROTTLE = 4 * 1024 * 1024  # 字节进度上报节流（每 ~4MB 一次）
# 上传中间态的临时文件后缀（见 _tmp_names）。刻意取 4 字节、与常见扩展名等长，
# 这样降级成“替换扩展名”时临时名不会比最终名更长。
_TMP_SUFFIX = ".wsu"
# Unix epoch(1970) 对应的 Windows FILETIME（100ns since 1601）；用于把本地时间转成 SMB 时间
_EPOCH_FILETIME = 116444736000000000


def _tmp_names(name: str) -> List[str]:
    """同目录临时文件名的候选，按优先级排列。

    首选**追加** .wsu（原名完整保留，残留临时文件一眼能看出是哪个文件传断了）。但文件名
    上限按 UTF-8 字节算，贴着 255 字节上限的中文名追加后缀会被服务端以
    STATUS_OBJECT_NAME_INVALID 整个拒掉（连文件都建不出来）。备选把扩展名**换成** .wsu，
    临时名恒不长于最终名，多长的原名都放得下（丢掉原扩展名无影响，因为临时名只在传输中间
    存在，传完就 replace 成原名）。

    不预判长度、只在服务端真的拒绝时才降级（见 upload_file）：各家服务器上限口径不一样，
    照某一台的规则预先截名字会误伤别处。
    """
    names = [name + _TMP_SUFFIX]
    stem, _ext = os.path.splitext(name)
    # 无扩展名（含 .gitignore 这类纯点名）时备选与首选相同，没有降级余地
    if stem and stem != name:
        names.append(stem + _TMP_SUFFIX)
    return names


def _is_name_rejected(exc: Exception) -> bool:
    """服务端拒绝的是这个文件名本身（而非权限/网络/磁盘问题）"""
    return getattr(exc, "ntstatus", None) == NtStatus.STATUS_OBJECT_NAME_INVALID


def _ft_from_ns(ns: int) -> int:
    """纳秒 epoch -> FILETIME(100ns since 1601) int"""
    return ns // 100 + _EPOCH_FILETIME


def _ft_from_seconds(sec: float) -> int:
    """秒 epoch(float) -> FILETIME int（创建时间在 macOS 只有秒级精度）"""
    return int(round(sec * 10_000_000)) + _EPOCH_FILETIME


def _dt_iso(value) -> Optional[str]:
    """把 smbprotocol 的 tz-aware datetime / epoch 秒 转成本地 naive ISO 字符串。

    与旧 pysmb 实现（datetime.fromtimestamp(...).isoformat()）保持一致的格式，
    避免下游（如冲突检测的 fromisoformat 比较）出现 tz-aware/naive 混用。
    """
    if not value:
        return None
    try:
        ts = value.timestamp() if hasattr(value, "timestamp") else float(value)
        return datetime.fromtimestamp(ts).isoformat()
    except (OverflowError, OSError, ValueError):
        return None
