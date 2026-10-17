"""拷贝与有前提的删除 —— 搬数据、清理副本时用，两端都是 `pybase.storage.Storage`。

**拷贝只有一个 `copy_file`，校验是开关**（默认关）：

- 不校验：只按**大小**判断——目标已有同大小的算拷过，拷完核对大小
- `verify=True`（或给了 `expected_hash`）：边读源边算哈希，写完把目标读回来再算一遍，
  期望值 / 源 / 目标三处哈希任何一处对不上都不算成功

两种都先写临时文件再 `replace` 成正式名：中途断开只剩临时文件，正式名下不会有半截
文件，下次同名重拷直接覆盖临时文件。

**目标已存在且判定不同一律不覆盖**，报 `conflict` 交给调用方——目标处那份可能是
别人放的另一版，覆盖它就是丢数据。

**块级可续传拷贝 `copy_file_resumable`**：按块写到目标旁的临时文件，每块写完回调
`progress` 让调用方持久化进度；断开后带着 `ResumeState` 再调，从断点所在的块接着传
（见该函数文档）。它是 `copy_file` 的补充，不改 `copy_file` 的行为。

**删除**：只删调用方核对函数说"可以"的那一个文件，判据（对方有没有、哈希对不对）
全由调用方给；删完只收拾因此变空的目录，有东西的目录一个不碰。

两端走什么通道（本地、挂载盘、SMB 协议、HTTPS）都由 `Storage` 实现决定，这里不管；
断线等待与重试见 `pybase.retry`，本模块的 I/O 错误原样抛出。
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from typing import Callable, Iterable, List, Optional, Union

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


RESUME_CHUNK = 8 * 1024 * 1024
RESUME_SUFFIX = ".xferq.part"
_MTIME_EPS = 1e-3


@dataclass
class ResumeState:
    """可续传拷贝的进度：源文件的大小/修改时间快照、块大小、已确认各块的 sha256。

    `copy_file_resumable` 会**原地维护**它（续传前先校验、必要时清零，每块写完先更新
    `chunks` 再调 `progress`），调用方在回调里把它持久化即可。
    """

    size: Optional[int] = None
    mtime: Optional[float] = None
    chunk: int = RESUME_CHUNK
    chunks: List[str] = field(default_factory=list)

    def reset(self, size: Optional[int] = None, mtime: Optional[float] = None, chunk: Optional[int] = None) -> None:
        self.size, self.mtime = size, mtime
        if chunk:
            self.chunk = chunk
        self.chunks = []

    def matches(self, size: int, mtime: Optional[float], chunk: int) -> bool:
        if self.size != size or self.chunk != chunk:
            return False
        if (self.mtime is None) != (mtime is None):
            return False
        if mtime is not None and abs(self.mtime - mtime) > _MTIME_EPS:
            return False
        return len(self.chunks) <= (size + chunk - 1) // chunk

    @property
    def done_bytes(self) -> int:
        return min(len(self.chunks) * self.chunk, self.size or 0)


def _read_full(f, n: int) -> bytes:
    """读满 `n` 字节（遇到文件尾才会少）；底层 read 可能一次给不满。"""
    buf = bytearray()
    while len(buf) < n:
        b = f.read(n - len(buf))
        if not b:
            break
        buf += b
    return bytes(buf)


def _chunk_sha(storage: Storage, path: str, start: int, length: int) -> str:
    with storage.open_read(path) as f:
        if start:
            f.seek(start)
        data = _read_full(f, length)
    return hashlib.sha256(data).hexdigest() if len(data) == length else ""


def _chunk_hashes(storage: Storage, path: str, chunk: int) -> List[str]:
    out = []
    with storage.open_read(path) as f:
        while block := _read_full(f, chunk):
            out.append(hashlib.sha256(block).hexdigest())
    return out


def copy_file_resumable(src: Storage, src_path: str, dst: Storage, dst_path: str, *,
                        part_path: str, chunk: int = RESUME_CHUNK,
                        progress: Optional[Callable[[int, int, str], None]] = None,
                        state: Optional[ResumeState] = None,
                        preserve_mtime: bool = True) -> CopyResult:
    """块级可续传拷贝：`src:src_path` -> `dst:dst_path`，先写 `dst:part_path`，核对通过再转正。

    - 每块（`chunk` 字节）写完调 `progress(done_bytes, chunk_index, chunk_sha256)`；`state.chunks`
      此时已含该块。回调里抛异常会中断拷贝（暂停/取消就这么做），异常原样抛出，临时文件留着。
    - 续传：传入上次的 `state`。先核对源的大小/修改时间与块大小没变（变了就清零重来），
      再把临时文件截到最后一个已确认块的边界，**读回最后一块比 sha256**，不对就退一块，
      直到对上或退到 0；然后从源的对应偏移接着读写。
    - 写完整体核对：全部块的 sha256（续传前的块用 `state` 里记的，新写的边读边算）与目标读回的
      逐块哈希必须一致，另外源的大小/修改时间要与开始时一致（变了就从头来一遍，仍变则放弃）。
    - 目标已存在：大小与逐块哈希都一致算 `already-present`，否则 `conflict`，**不覆盖**。
    - 返回 `CopyResult`，`status` 在 `copy_file` 的基础上多一个 `source-changed`（源在拷贝期间一直在变）。
      I/O 错误原样抛出，调用方带着 `state` 再调就是续传。
    """
    if chunk < 1:
        raise ValueError("chunk 至少为 1")
    st = state if state is not None else ResumeState(chunk=chunk)

    def result(status, size=0):
        return CopyResult(status, src_path, dst_path, size, None, True)

    for attempt in (0, 1):
        entry = src.stat(src_path)
        if entry is None or entry.is_dir:
            return result("source-missing")
        size = entry.size
        if not st.matches(size, entry.mtime, chunk):
            st.reset(size, entry.mtime, chunk)

        existing = dst.stat(dst_path)
        if existing is not None:
            if existing.is_dir or existing.size != size:
                return result("conflict", size)
            if _chunk_hashes(dst, dst_path, chunk) == _chunk_hashes(src, src_path, chunk):
                _drop(dst, part_path)
                return result("already-present", size)
            return result("conflict", size)

        n = _verified_chunks(dst, part_path, st, size)
        del st.chunks[n:]

        offset = n * chunk
        with src.open_read(src_path) as reader, dst.open_resume(part_path, offset) as writer:
            if offset:
                reader.seek(offset)
            idx = n
            while offset < size:
                block = _read_full(reader, chunk)
                if len(block) != min(chunk, size - offset):
                    break                       # 源比开始时短了：下面的复查会发现
                sha = hashlib.sha256(block).hexdigest()
                writer.write(block)
                writer.flush()
                offset += len(block)
                st.chunks.append(sha)
                if progress:
                    progress(offset, idx, sha)
                idx += 1

        again = src.stat(src_path)
        if (again is None or again.size != size
                or (entry.mtime is not None and again.mtime is not None
                    and abs(again.mtime - entry.mtime) > _MTIME_EPS)
                or offset != size):
            st.reset()
            _drop(dst, part_path)
            if attempt == 0:
                continue
            return result("source-changed", size)

        got = dst.stat(part_path)
        if got is None or got.size != size or _chunk_hashes(dst, part_path, chunk) != st.chunks:
            st.reset()
            _drop(dst, part_path)
            return result("copy-differs", size)

        if dst.stat(dst_path) is not None:      # 写的期间目标被别人放了东西：不覆盖
            _drop(dst, part_path)
            return result("conflict", size)
        if preserve_mtime and entry.mtime is not None:
            dst.set_mtime(part_path, entry.mtime)
        dst.replace(part_path, dst_path)
        return result("copied", size)
    return result("source-changed")             # 不可达


def _drop(storage: Storage, path: str) -> None:
    if storage.stat(path) is not None:
        storage.remove(path)


def _verified_chunks(dst: Storage, part_path: str, st: ResumeState, size: int) -> int:
    """临时文件里确实有、且最后一块读回哈希对得上的块数（从已确认数往回退）。"""
    n = len(st.chunks)
    if n == 0:
        return 0
    have = dst.stat(part_path)
    have_size = have.size if have is not None and not have.is_dir else -1
    while n > 0:
        start = (n - 1) * st.chunk
        length = min(st.chunk, size - start)
        if have_size >= start + length and _chunk_sha(dst, part_path, start, length) == st.chunks[n - 1]:
            return n
        n -= 1
    return 0


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
