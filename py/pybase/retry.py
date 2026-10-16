"""等资源回来、断了就重试 —— 网络盘、外接盘这类会中途消失的东西用。

**等待的间隔由调用方给，没有默认值**：几秒一查适合交互界面，挂一夜的批处理
十几二十分钟查一次就够——查得太勤只是白耗。

资源可用与否通常就是 `Storage.available`（见 `pybase.storage`），与具体通道无关。

`retry_call` 每次失败后先等资源回来再重试；资源断着的那段时间不计入重试次数
（断网几小时不该把次数耗光），只有"资源在、调用照样失败"才算一次。
"""

from __future__ import annotations

import time
from typing import Callable, Optional, Tuple, Type, TypeVar

T = TypeVar("T")


def wait_until(predicate: Callable[[], bool], poll_seconds: float, *,
               timeout: Optional[float] = None,
               on_wait: Optional[Callable[[], None]] = None,
               sleep: Callable[[float], None] = time.sleep,
               clock: Callable[[], float] = time.monotonic) -> bool:
    """`predicate()` 为真立即返回 True；否则每 `poll_seconds` 查一次。

    `on_wait` 只在第一次发现要等时调一次（打日志用）。给了 `timeout` 到点仍不满足
    返回 False；`predicate` 抛 `OSError` 视同"还没好"。
    """
    def ready() -> bool:
        try:
            return bool(predicate())
        except OSError:
            return False

    if ready():
        return True
    if on_wait:
        on_wait()
    deadline = None if timeout is None else clock() + timeout
    while True:
        if deadline is not None and clock() >= deadline:
            return False
        sleep(poll_seconds)
        if ready():
            return True


def retry_call(fn: Callable[[], T], *, attempts: int, delay_seconds: float,
               available: Optional[Callable[[], bool]] = None,
               poll_seconds: Optional[float] = None,
               retry_on: Tuple[Type[BaseException], ...] = (OSError,),
               on_retry: Optional[Callable[[int, BaseException], None]] = None,
               sleep: Callable[[float], None] = time.sleep) -> T:
    """调用 `fn()`，抛 `retry_on` 里的异常就重试，最多 `attempts` 次后把最后的异常抛出。

    给了 `available`（如"挂载点在不在"）时：每次调用前先 `wait_until(available,
    poll_seconds)`；失败时若资源已不可用，这次不计数，等它回来再试。
    """
    if attempts < 1:
        raise ValueError("attempts 至少为 1")
    if available is not None and poll_seconds is None:
        raise ValueError("给了 available 就必须给 poll_seconds")
    used = 0
    while True:
        if available is not None:
            wait_until(available, poll_seconds, sleep=sleep)
        try:
            return fn()
        except retry_on as exc:
            if available is not None and not _safe(available):
                continue
            used += 1
            if used >= attempts:
                raise
            if on_retry:
                on_retry(used, exc)
            sleep(delay_seconds)


def _safe(predicate: Callable[[], bool]) -> bool:
    try:
        return bool(predicate())
    except OSError:
        return False
