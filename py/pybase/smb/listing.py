"""列目录与存在性判断。

`SMBClient` 的一半（见 `client.py` 的组装）。**只读、不写**：`list_dir` /
`walk` / `scan` / `exists` 全是"服务器上有什么"这一类问题，写入在 `upload.py`。

混入方需要提供 `_ensure_connection()` / `_unc()` / `_cache`。
"""

import logging
from typing import Callable, Dict, Generator, List, Optional

import smbclient
import smbclient.path as smbpath

from .common import _FILE_ATTRIBUTE_DIRECTORY, _READ_ACCESS, _dt_iso

logger = logging.getLogger(__name__)


class ListingMixin:
    """列目录与存在性判断。"""

    # ---------- 列目录 ----------

    def list_dir(self, path: str = "") -> List[Dict]:
        """列出目录内容

        用 scandir 读取目录枚举信息（含大小/时间），不对每个条目再 stat，
        避免列大目录时每条目额外一次 SMB 往返。
        """
        self._ensure_connection()
        unc = self._unc(path)
        results = []

        try:
            for entry in smbclient.scandir(
                unc, connection_cache=self._cache, desired_access=_READ_ACCESS
            ):
                if entry.name in (".", ".."):
                    continue
                # _dir_info 来自目录枚举，无额外往返
                info = entry._dir_info
                is_dir = bool(info.file_attributes & _FILE_ATTRIBUTE_DIRECTORY)
                rel_path = f"{path}/{entry.name}".lstrip("/") if path else entry.name
                results.append({
                    "name": entry.name,
                    "path": rel_path,
                    "is_dir": is_dir,
                    "size": 0 if is_dir else info.end_of_file,
                    "created_at": _dt_iso(info.creation_time),
                    "modified_at": _dt_iso(info.last_write_time),
                    "accessed_at": _dt_iso(info.last_access_time),
                })
        except Exception as e:
            logger.error(f"列出目录失败 {path}: {e}")
            raise

        return results

    def walk(
        self,
        path: str = "",
        directory_filter: Optional[Callable[[Dict], bool]] = None,
        strict: bool = False,
    ) -> Generator[Dict, None, None]:
        """递归遍历目录

        Args:
            path: 起始路径
            directory_filter: 目录过滤器，接收目录项 dict，返回 True 表示进入，False 表示跳过
        """
        self._ensure_connection()

        try:
            items = self.list_dir(path)

            for item in items:
                yield item

                if item["is_dir"]:
                    try:
                        if directory_filter and not directory_filter(item):
                            continue
                        yield from self.walk(item["path"], directory_filter, strict=strict)
                    except Exception as e:
                        if strict:
                            raise
                        logger.warning(f"无法进入目录 {item['path']}: {e}")
                        continue

        except Exception as e:
            if strict:
                raise
            logger.error(f"遍历目录失败 {path}: {e}")

    def scan(
        self,
        path: str = "",
        callback: Callable[[int, str], None] = None,
        directory_filter: Optional[Callable[[Dict], bool]] = None,
        strict: bool = False,
    ) -> List[Dict]:
        """扫描目录，返回所有文件和目录

        Args:
            path: 起始路径
            callback: 进度回调函数 callback(count, current_path)
            directory_filter: 目录过滤器

        Returns:
            文件和目录列表
        """
        results = []
        count = 0

        for item in self.walk(path, directory_filter=directory_filter, strict=strict):
            if not item["is_dir"]:
                name = item["name"]
                ext = name.rsplit(".", 1)[-1].lower() if "." in name else ""
                item["extension"] = ext
            else:
                item["extension"] = ""

            results.append(item)
            count += 1

            if callback and count % 10 == 0:
                callback(count, item["path"])

        if callback:
            callback(count, "完成")

        return results

    # ---------- 元信息 ----------

    def exists(self, path: str) -> Optional[bool]:
        """这个路径在不在。**三态**：

        - `True`  —— 确实有
        - `False` —— 确实没有
        - `None`  —— 这次读不到（权限、断线、服务器抽风），**判不出来**

        原先任何异常都返回 `False`，于是"连不上"与"服务器上没有这个目录"
        在调用方眼里长得一模一样——核对会据此报"存档缺这份材料"，而真相
        可能是这一趟根本没问到。判不出来必须说判不出来，由调用方决定是
        当成"没读全"还是照实抛。
        """
        self._ensure_connection()
        try:
            return smbpath.exists(self._unc(path), connection_cache=self._cache)
        except Exception as exc:                              # noqa: BLE001
            logger.warning(f"判断远端路径是否存在失败，这次判不出来 {path}: {exc}")
            return None
