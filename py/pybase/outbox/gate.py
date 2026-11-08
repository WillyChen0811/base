"""闸门：要外发的文字里有没有敏感串。敏感串从哪来由调用方配（本模块不带任何目录、词表、编号格式的缺省）。

来源是一个个无参函数 ``source() -> 可迭代的串``，:class:`Gate` 把它们取到的并起来，进程里留 ``ttl`` 秒（窗口改一次查一次，
每次都重走一遍目录太慢）。现成的来源：

- :func:`names_source`：走一个资料目录，收文件夹名、文件名（去扩展名）整个和按分隔符切出的各段——够 ``min_len`` 个字符，
  或 ``cjk_min_len`` 个以上且含汉字的算；再用调用方给的编号格式（正则）从里面抠编号；去掉调用方给的通用类别词、
  ``ignore`` 里的串、``digits_max`` 位以内的纯数字。走法照 xxyi-agentrun 的 privacy_gate.py（复用思路，不依赖它）。
- :func:`lines_source`：一个文本文件，一行一个串（空行和 ``#`` 开头的行不算）。
- :func:`static_source`：直接给一组串。

**不悄悄放过**：任一来源出错（目录不在、文件读不了），或全部来源一个串都没取到，都抛 :class:`GateUnavailable`；
调用方据此拒绝放行。命中串只回给调用方显示到用户屏幕上：本模块不打印、不写日志，异常文字里也不带任何敏感串。
"""
from __future__ import annotations

import os
import re
import threading
import time
from typing import Callable, Iterable, List, Optional, Sequence

DEFAULT_SPLIT = r"[-_\s（）()［］\[\]、，,。．.]+"
_CJK = re.compile(r"[一-鿿]")

NO_ROOT = "闸门的敏感串来源目录不在：%s"
NO_FILE = "闸门的敏感串清单读不了：%s（%s）"
NO_STRINGS = "闸门的敏感串来源里一个串都没取到"
SOURCE_FAILED = "闸门的敏感串来源出错：%s"


class GateUnavailable(RuntimeError):
    """闸门查不了：str(e) 给人看，不含任何敏感串。"""


Source = Callable[[], Iterable[str]]


def names_source(root: str, *, generic: Iterable[str] = (), code_patterns: Sequence[str] = (),
                 ignore: Iterable[str] = (), min_len: int = 4, cjk_min_len: int = 2, digits_max: int = 4,
                 split: str = DEFAULT_SPLIT) -> Source:
    """资料目录里的文件夹名、文件名及其各段（见文件头）。root 支持 ``~``；不在 → GateUnavailable。"""
    splitter = re.compile(split)
    codes = [re.compile(p) for p in code_patterns]
    generic, ignore = frozenset(generic), frozenset(ignore)

    def source() -> set:
        top = os.path.expanduser(root)
        if not os.path.isdir(top):
            raise GateUnavailable(NO_ROOT % root)
        pats = set()
        for _dp, dns, fns in os.walk(top):
            for n in dns + fns:
                stem = os.path.splitext(n)[0]
                for piece in [stem] + splitter.split(stem):
                    piece = piece.strip()
                    if len(piece) >= min_len or (len(piece) >= cjk_min_len and _CJK.search(piece)):
                        pats.add(piece)
        for n in list(pats):
            for rx in codes:
                for m in rx.finditer(n):                 # 整个匹配（正则里有分组也取整串）
                    if m.group(0):
                        pats.add(m.group(0))
        return {p for p in pats
                if p not in generic and p not in ignore and not (p.isdigit() and len(p) <= digits_max)}
    return source


def lines_source(path: str) -> Source:
    """文本文件，一行一个串（去首尾空白；空行、``#`` 开头的不算）。读不了 → GateUnavailable。"""
    def source() -> set:
        p = os.path.expanduser(path)
        try:
            with open(p, encoding="utf-8") as f:
                lines = f.read().splitlines()
        except (OSError, UnicodeDecodeError) as e:
            raise GateUnavailable(NO_FILE % (path, type(e).__name__)) from None
        return {s.strip() for s in lines if s.strip() and not s.strip().startswith("#")}
    return source


def static_source(strings: Iterable[str]) -> Source:
    got = {s for s in strings if isinstance(s, str) and s}
    return lambda: set(got)


def hits_in(text: str, pats) -> List[str]:
    """text 里出现过的敏感串，按第一次出现的位置排（同位置长的在前）。"""
    text = text or ""
    found = [p for p in pats if p and p in text]
    return sorted(found, key=lambda p: (text.find(p), -len(p), p))


class Gate:
    """``Gate([来源…], ttl=60).check(text)`` → 命中串列表（空 = 过）；查不了抛 GateUnavailable。

    ``factory``：每次缓存过期重取时现调一次、回来源列表（来源随调用方的配置文件变，改了配置不用重建闸门）；
    给了它就不看 ``sources``。"""

    def __init__(self, sources: Sequence[Source] = (), ttl: float = 60.0, clock: Callable[[], float] = time.monotonic,
                 *, factory: Optional[Callable[[], Sequence[Source]]] = None):
        self.sources = list(sources)
        self.factory = factory
        self.ttl = ttl
        self._clock = clock
        self._lock = threading.Lock()
        self._cache: Optional[tuple] = None             # (取到的时刻, 串集合)

    def strings(self) -> frozenset:
        now = self._clock()
        with self._lock:
            if self._cache is not None and now - self._cache[0] < self.ttl:
                return self._cache[1]
        if self.factory is not None:
            try:
                sources = list(self.factory())
            except GateUnavailable:
                raise
            except Exception as e:                       # noqa: BLE001
                raise GateUnavailable(SOURCE_FAILED % type(e).__name__) from None
        else:
            sources = self.sources
        if not sources:
            raise GateUnavailable(NO_STRINGS)
        pats = set()
        for src in sources:
            try:
                got = src()
            except GateUnavailable:
                raise
            except Exception as e:                       # noqa: BLE001  原因只给异常类型名（别把串带进错误）
                raise GateUnavailable(SOURCE_FAILED % type(e).__name__) from None
            pats.update(s for s in got if isinstance(s, str) and s)
        if not pats:
            raise GateUnavailable(NO_STRINGS)
        frozen = frozenset(pats)
        with self._lock:
            self._cache = (now, frozen)
        return frozen

    def check(self, text: str) -> List[str]:
        return hits_in(text, self.strings())

    def clear(self) -> None:
        with self._lock:
            self._cache = None
