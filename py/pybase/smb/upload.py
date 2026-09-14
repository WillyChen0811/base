"""往服务器上写：建目录、上传、重命名。

这一块的三条硬规矩，都写在各自方法的注释里，改之前先读：
- **原子替换**：先写同目录临时文件，校验大小一致才 `replace` 成目标名——
  半截文件不能以正式名出现在归档里。
- **时间戳由调用方定死**（`upload_file` 的 `mtime_ns`）：传档传的是版本库里
  那份快照，时间必须取定版那一刻的值，不能在这里临上传前再读一次正位文件。
- **临时名被服务端拒绝要降级不要放弃**（`_tmp_names`）：中文长文件名顶满
  255 字节上限时，追加后缀会被整个拒掉。

混入方需要提供 `_ensure_connection()` / `_unc()` / `_safe_delete()` /
`disconnect()` / `_cache` / `_ensured_dirs` / `_encrypted`。
"""

import logging
import os
import tempfile
from datetime import datetime
from typing import Callable, Dict, Optional

import smbclient
# _set_basic_information：一次设全文件时间（创建/访问/修改），utime 只能设访问/修改
from smbclient._os import _set_basic_information

from .common import (
    _PROGRESS_THROTTLE,
    _UPLOAD_CHUNK,
    _ft_from_ns,
    _ft_from_seconds,
    _is_name_rejected,
    _tmp_names,
)

logger = logging.getLogger(__name__)


class UploadMixin:
    """建目录、上传、重命名。"""

    # ---------- 目录创建 ----------

    def mkdir(self, path: str) -> bool:
        """创建远程目录（递归创建父目录，幂等）"""
        self._ensure_connection()

        path = (path or "").strip("/")
        if not path:
            return True

        # 本次连接已确认过则直接跳过，避免重复往返
        if path in self._ensured_dirs:
            return True

        smbclient.makedirs(self._unc(path), exist_ok=True, connection_cache=self._cache)

        # 记录本次已确认的目录（含各级父目录），加速后续同目录文件上传
        prefix = ""
        for part in path.split("/"):
            prefix = f"{prefix}/{part}" if prefix else part
            self._ensured_dirs.add(prefix)

        return True

    # ---------- 上传 ----------

    def upload_file(
        self,
        local_path: str,
        remote_path: str,
        max_retries: int = 2,
        byte_progress_callback: Callable[[int, int], None] = None,
        mtime_ns: Optional[int] = None,
        atime_ns: Optional[int] = None,
        birthtime: Optional[float] = None,
    ) -> Dict:
        """上传本地文件到 SMB（原子写入、大小校验、重试重连、保留修改时间）

        先写入同目录临时文件（.wsu 后缀，见 _tmp_names），校验大小一致后用 replace 原子替换，
        再把目标文件的时间设成调用方给的这几个值（不给就用 local_path 自己的）。

        Args:
            local_path: 本地文件路径（传档传的常是版本库里那份快照）
            remote_path: 远程文件路径（相对共享根）
            max_retries: 失败后的最大重试次数
            byte_progress_callback: 单文件字节进度回调 callback(sent, total)，节流上报
            mtime_ns / atime_ns / birthtime: 远端要带的时间戳，**由调用方在定版那一刻
                现场 `os.stat` 好传进来**，不是在这里临上传前才读一次文件——正位文件在
                上传排队等待期间可能已经被换成同样大小的另一份，这里再读就会把"旧内容 +
                新时间"焊在一起。不给就退回 `local_path` 自己的时间（没有版本快照要
                区分的普通上传）。

        Returns:
            {success: bool, size: int, message?: str}
        """
        try:
            st_local = os.stat(local_path)
        except OSError as e:
            logger.error(f"读取本地文件失败 {local_path}: {e}")
            return {"success": False, "size": 0, "message": f"读取本地文件失败: {e}"}
        local_size = st_local.st_size
        # `mtime_ns` 在不在是"调用方有没有现场定死时间"的分界：给了就照给的用
        # （`birthtime` 为 None 就是那个平台真的没有创建时间，不能退回
        # local_path 现在的 birthtime 去凑——那多半已经是另一份内容了）；
        # 没给就是普通上传，退回 local_path 自己现在的时间。
        if mtime_ns is None:
            use_mtime_ns = st_local.st_mtime_ns
            use_atime_ns = st_local.st_atime_ns
            use_birthtime = getattr(st_local, "st_birthtime", None)
        else:
            use_mtime_ns = mtime_ns
            use_atime_ns = st_local.st_atime_ns if atime_ns is None else atime_ns
            use_birthtime = birthtime

        remote_norm = remote_path.strip("/")
        remote_parts = remote_norm.split("/")
        remote_dir = "/".join(remote_parts[:-1])
        target_unc = self._unc(remote_norm)

        tmp_candidates = _tmp_names(remote_parts[-1])
        tmp_index = 0
        tmp_unc = self._unc("/".join(remote_parts[:-1] + [tmp_candidates[0]]))

        last_error = None
        attempt = 0
        while True:
            try:
                self._ensure_connection()

                if remote_dir:
                    self.mkdir(remote_dir)

                # 写入临时文件并按块上报进度
                written = 0
                last_report = 0
                with open(local_path, "rb") as src, \
                        smbclient.open_file(tmp_unc, mode="wb", connection_cache=self._cache) as dst:
                    while True:
                        chunk = src.read(_UPLOAD_CHUNK)
                        if not chunk:
                            break
                        dst.write(chunk)
                        written += len(chunk)
                        if byte_progress_callback and written - last_report >= _PROGRESS_THROTTLE:
                            last_report = written
                            try:
                                byte_progress_callback(written, local_size)
                            except Exception:
                                pass

                # 校验大小，防止截断
                st_remote = smbclient.stat(tmp_unc, connection_cache=self._cache)
                if st_remote.st_size != local_size:
                    raise IOError(
                        f"上传大小不一致: 本地 {local_size} 字节, 远程 {st_remote.st_size} 字节"
                    )

                # 原子替换（覆盖已存在目标）
                smbclient.replace(tmp_unc, target_unc, connection_cache=self._cache)

                # 完整复制源文件时间信息（创建/访问/修改），像 Windows 复制那样保留。
                # 用 _set_basic_information 一次设全（utime 只能设访问/修改，无法设创建时间）；
                # 字段传 0 表示“不修改”，故 change_time / file_attributes 交由服务器维持。
                try:
                    create_ft = _ft_from_seconds(use_birthtime) if use_birthtime else 0
                    _set_basic_information(
                        target_unc,
                        creation_time=create_ft,
                        last_access_time=_ft_from_ns(use_atime_ns),
                        last_write_time=_ft_from_ns(use_mtime_ns),
                        connection_cache=self._cache,
                    )
                except Exception as e:
                    logger.warning(f"设置远程文件时间信息失败（忽略）{target_unc}: {e}")

                # 收尾补一帧 100%
                if byte_progress_callback:
                    try:
                        byte_progress_callback(local_size, local_size)
                    except Exception:
                        pass

                logger.info(
                    f"上传文件成功: {local_path} -> {target_unc}, 大小: {written}, 加密={self._encrypted}"
                )
                return {"success": True, "size": written}

            except Exception as e:
                last_error = e
                logger.warning(
                    f"上传失败 (第 {attempt + 1}/{max_retries + 1} 次) "
                    f"{local_path} -> {target_unc}: {e}"
                )
                # 清理残留临时文件，并强制下次重连
                self._safe_delete(tmp_unc)
                self.disconnect()

                # 服务端嫌临时名不合法（多为原名已顶满长度上限、追加后缀后溢出）：
                # 换用更短的候选名重试。不算一次失败重试——重试次数是留给网络抖动的，
                # 而换名解决的是另一类问题，不该互相挤占。
                if _is_name_rejected(e) and tmp_index + 1 < len(tmp_candidates):
                    tmp_index += 1
                    tmp_unc = self._unc(
                        "/".join(remote_parts[:-1] + [tmp_candidates[tmp_index]])
                    )
                    logger.info(f"临时文件名被服务端拒绝，改用 {tmp_candidates[tmp_index]} 重试")
                    continue

                attempt += 1
                if attempt > max_retries:
                    break

        logger.error(f"上传文件最终失败 {local_path} -> {remote_path}: {last_error}")
        return {"success": False, "size": 0, "message": str(last_error)}

    def upload_bytes(self, data: bytes, remote_path: str, mtime: str = None) -> Dict:
        """把内存里的字节写到远程路径（覆盖已存在文件）

        走临时本地文件 + upload_file，为的是沿用它的原子替换、大小校验和
        重试重连；mtime 传原文件的修改时间字符串时会一并保留（就地改内容的
        场景不该刷新时间戳）。
        """
        fd, tmp_path = tempfile.mkstemp(suffix=".smbupload")
        try:
            with os.fdopen(fd, "wb") as fh:
                fh.write(data)
            if mtime:
                try:
                    ts = datetime.fromisoformat(mtime).timestamp()
                    os.utime(tmp_path, (ts, ts))
                except (ValueError, OSError) as e:
                    logger.warning(f"解析原修改时间失败（忽略）{remote_path}: {e}")
            return self.upload_file(tmp_path, remote_path)
        finally:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass

    # ---------- 重命名 ----------

    def rename(self, path: str, new_path: str) -> bool:
        """重命名远程文件或目录（目标已存在时报错，不覆盖）"""
        self._ensure_connection()
        smbclient.rename(
            self._unc(path), self._unc(new_path), connection_cache=self._cache
        )
        return True
