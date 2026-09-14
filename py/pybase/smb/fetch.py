"""三种取文件通道的统一入口（SMB / WebDAV / File Station）。

三条通道代码各自独立、互不串联：每个函数只走一种通道，失败都抛 errors.FetchError
（带用户友好文案 + 底层原始码）。是否换一条通道、按什么顺序，由上层/前端按服务器
配置的优先级决定（前端逐级征求用户同意），后端不做静默自动回退。

后台自动流程（无人交互，如解析/索引）用 read_default：只走优先级第一的“默认通道”，
失败即如实报错，不尝试其它通道。
"""

import logging
from typing import List

from .client import SMBClient
from .http_fetch import fetch_webdav, fetch_filestation
from .errors import FetchError, smb_error

logger = logging.getLogger(__name__)

CHANNEL_SMB = "smb"
CHANNEL_WEBDAV = "webdav"
CHANNEL_FILESTATION = "filestation"

_ALL_CHANNELS = (CHANNEL_SMB, CHANNEL_WEBDAV, CHANNEL_FILESTATION)
_DEFAULT_PRIORITY = [CHANNEL_SMB, CHANNEL_WEBDAV, CHANNEL_FILESTATION]


def channel_priority(server: dict) -> List[str]:
    """解析服务器的通道优先级（逗号串 -> 列表）；非法/缺失回退默认顺序。"""
    raw = (server.get("channel_priority") or "").strip()
    order = [c.strip() for c in raw.split(",") if c.strip() in _ALL_CHANNELS]
    # 去重并补齐遗漏的通道，保证三条都在
    seen, result = set(), []
    for c in order + _DEFAULT_PRIORITY:
        if c not in seen:
            seen.add(c)
            result.append(c)
    return result


def default_channel(server: dict) -> str:
    """优先级第一的通道（默认通道）。"""
    return channel_priority(server)[0]


def next_channel(server: dict, current: str):
    """按优先级返回 current 的下一个通道；已是最后一个则返回 None。"""
    order = channel_priority(server)
    if current in order:
        i = order.index(current)
        if i + 1 < len(order):
            return order[i + 1]
    return None


def channel_label(channel: str) -> str:
    """通道的中文显示名。"""
    return {
        CHANNEL_SMB: "SMB",
        CHANNEL_WEBDAV: "WebDAV",
        CHANNEL_FILESTATION: "File Station",
    }.get(channel, channel)


def read_via_smb(server: dict, path: str) -> bytes:
    """走 SMB 取文件。失败抛 FetchError。"""
    try:
        with SMBClient(
            server["address"], server["share"],
            server.get("username") or "", server.get("password") or "",
            server.get("port", 445),
        ) as client:
            return client.download_file(path)
    except FetchError:
        raise
    except Exception as e:
        logger.info(f"SMB 取文件失败: {e}")
        raise smb_error(e)


def read_via_webdav(server: dict, path: str) -> bytes:
    """走 WebDAV 取文件。失败抛 FetchError。"""
    return fetch_webdav(server, path)


def read_via_filestation(server: dict, path: str) -> bytes:
    """走 File Station 取文件。失败抛 FetchError。"""
    return fetch_filestation(server, path)


_READERS = {
    CHANNEL_SMB: read_via_smb,
    CHANNEL_WEBDAV: read_via_webdav,
    CHANNEL_FILESTATION: read_via_filestation,
}


def read_via_channel(server: dict, path: str, channel: str) -> bytes:
    """按指定通道取文件（单通道，不回退）。失败抛 FetchError。"""
    reader = _READERS.get(channel)
    if reader is None:
        raise FetchError(f"取文件失败：未知通道 {channel}", f"channel={channel}")
    return reader(server, path)


def read_default(server: dict, path: str) -> bytes:
    """走默认通道（优先级第一）取文件，供后台无交互流程使用。失败抛 FetchError。"""
    return read_via_channel(server, path, default_channel(server))
