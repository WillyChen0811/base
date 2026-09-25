"""把远端文件的内容读回来：整份下载，或流式算哈希。

`hash_file` 存在的理由是归档里躺着几 GB 的录屏——那种文件不能走
`download_file` 的"一次读进内存"。

混入方需要提供 `_ensure_connection()` / `_unc()` / `_cache`。
"""

import hashlib

import smbclient

from .common import _READ_ACCESS


class DownloadMixin:
    """下载与远端哈希。"""

    def download_file(self, path: str) -> bytes:
        """下载文件到内存（只读共享模式，允许其他进程同时访问）"""
        self._ensure_connection()
        unc = self._unc(path)
        with smbclient.open_file(
            unc, mode="rb", share_access="rwd",
            desired_access=_READ_ACCESS, connection_cache=self._cache,
        ) as f:
            return f.read()

    def hash_file(self, path: str, algo: str = "md5", chunk: int = 4 * 1024 * 1024) -> str:
        """算远程文件的哈希。**流式读，不整个下到内存**。

        归档里躺着几 GB 的录屏视频，`download_file` 那种一次读完的写法会直接把
        内存吃满。这里按块读、边读边喂给 hashlib。
        """
        self._ensure_connection()
        unc = self._unc(path)
        h = hashlib.new(algo)
        with smbclient.open_file(
            unc, mode="rb", share_access="rwd",
            desired_access=_READ_ACCESS, connection_cache=self._cache,
        ) as f:
            while True:
                block = f.read(chunk)
                if not block:
                    break
                h.update(block)
        return h.hexdigest()
