"""拷贝与有前提的删除 —— 搬数据、清理副本时用，两端都是 `pybase.storage.Storage`。

**拷贝只有一个 `copy_file`，校验是开关**（默认关）：

- 不校验：只按**大小**判断——目标已有同大小的算拷过，拷完核对大小
- `verify=True`（或给了 `expected_hash`）：边读源边算哈希，写完把目标读回来再算一遍，
  期望值 / 源 / 目标三处哈希任何一处对不上都不算成功

两种都先写临时文件再 `replace` 成正式名：中途断开只剩临时文件，正式名下不会有半截
文件，下次同名重拷直接覆盖临时文件。

**目标已存在且判定不同一律不覆盖**，报 `conflict` 交给调用方——目标处那份可能是
别人放的另一版，覆盖它就是丢数据。

**删除**：只删调用方核对函数说"可以"的那一个文件，判据（对方有没有、哈希对不对）
全由调用方给；删完只收拾因此变空的目录，有东西的目录一个不碰。

两端走什么通道（本地、挂载盘、SMB 协议、HTTPS）都由 `Storage` 实现决定，这里不管；
断线等待与重试见 `pybase.retry`，本模块的 I/O 错误原样抛出。
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import Callable, Iterable, Optional, Union

from .storage import Storage, parent

CHUNK = 4 * 1024 * 1024
TMP_SUFFIX = ".part"


def new_hasher(algorithm: str = "sha256"):
    """`hashlib.new` 认的名字都行，另认 `blake2b-256`（32 字节摘要的 BLAKE2b）。"""
    if algorithm == "blake2b-256":
        return hashlib.blake2b(digest_size=32)
    return hashlib.new(algorithm)


def hash_file(storage: Storage, path: str, algorithm: str = "sha256", chunk: int = CHUNK) -> str:
    h = new_hasher(algorithm)
    with storage.open_read(path) as f:
        while block := f.read(chunk):
            h.update(block)
    return h.hexdigest()


@dataclass(frozen=True)
class CopyResult:
    """`status` 取值（"一致"在不校验时指大小一致，校验时指哈希一致）：

    - `copied`：新拷贝完成，目标与源一致
    - `already-present`：目标早已存在且与源一致，没有重写
    - `conflict`：目标已存在但与源不一致，**未覆盖**
    - `source-missing`：源不存在
    - `source-hash-differs`：源内容与 `expected_hash` 不符（临时文件已删）
    - `copy-differs`：写完的目标与源不一致（目标已删，可重拷）

    `verified` 表示这次结论是否有哈希为据；`digest` 只在校验时有值。
    """

    status: str
    src: str
    dst: str
    size: int = 0
    digest: Optional[str] = None
    verified: bool = False

    @property
    def ok(self) -> bool:
        return self.status in ("copied", "already-present")


def copy_file(src: Storage, src_path: str, dst: Storage, dst_path: str, *,
              verify: bool = False, expected_hash: Optional[str] = None,
              algorithm: str = "sha256", preserve_mtime: bool = True,
              tmp_suffix: str = TMP_SUFFIX, chunk: int = CHUNK) -> CopyResult:
    """把 `src:src_path` 拷成 `dst:dst_path`。I/O 错误原样抛出（断线等待/重试见 `pybase.retry`）。

    `verify` 默认关：只比大小。开了（或给了 `expected_hash`，此时自动开）就用 `algorithm`
    算哈希：已存在的目标比哈希、拷贝时边拷边算、写完读回核对。
    """
    verify = verify or expected_hash is not None
    entry = src.stat(src_path)
    if entry is None or entry.is_dir:
        return CopyResult("source-missing", src_path, dst_path)
    size = entry.size

    def result(status, digest=None):
        return CopyResult(status, src_path, dst_path, size, digest, verify)

    existing = dst.stat(dst_path)
    if existing is not None:
        if existing.is_dir or existing.size != size:
            return result("conflict")
        if not verify:
            return result("already-present")
        have = hash_file(dst, dst_path, algorithm, chunk)
        want = expected_hash or hash_file(src, src_path, algorithm, chunk)
        return result("already-present", have) if have == want else result("conflict")

    tmp = dst_path + tmp_suffix
    h = new_hasher(algorithm) if verify else None
    with src.open_read(src_path) as reader, dst.open_write(tmp) as writer:
        while block := reader.read(chunk):
            if h:
                h.update(block)
            writer.write(block)
    digest = h.hexdigest() if h else None
    if expected_hash and digest != expected_hash:
        dst.remove(tmp)
        return result("source-hash-differs", digest)
    if preserve_mtime and entry.mtime is not None:
        dst.set_mtime(tmp, entry.mtime)
    dst.replace(tmp, dst_path)
    written = dst.stat(dst_path)
    if (written is None or written.size != size
            or (verify and hash_file(dst, dst_path, algorithm, chunk) != digest)):
        dst.remove(dst_path)
        return result("copy-differs", digest)
    return result("copied", digest)


def delete_confirmed(storage: Storage, path: str,
                     confirm: Callable[[str], Union[bool, str]]) -> str:
    """`confirm(path)` 返回 True 才删；返回字符串视为不删的原因，原样返回。

    返回 `deleted`、`missing`（本来就不在）、`not-a-file`，或核对函数给的原因。
    核对函数抛的异常不拦——判不出来就不该删，让调用方记下来。
    """
    entry = storage.stat(path)
    if entry is None:
        return "missing"
    if entry.is_dir:
        return "not-a-file"
    verdict = confirm(path)
    if verdict is not True:
        return verdict if isinstance(verdict, str) else "not-confirmed"
    storage.remove(path)
    return "deleted"


def prune_empty_dirs(storage: Storage, dirs: Iterable[str], stop_at: str = "") -> int:
    """从给定目录往上逐级删空目录，到 `stop_at`（不含，默认存储根）为止；返回删掉的个数。

    遇到非空（或删不掉）的目录，这条链就停。
    """
    stop = stop_at.strip("/")
    removed = 0
    for d in sorted({d.strip("/") for d in dirs}, key=lambda p: -p.count("/")):
        while d and d != stop and (not stop or d.startswith(stop + "/")):
            try:
                storage.rmdir(d)
            except OSError:
                break
            removed += 1
            d = parent(d)
    return removed
