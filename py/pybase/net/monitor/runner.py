"""按固定间隔跑一组采集器 -> 记录进 `Recorder` 的循环。

采集器列表、目标、路径全部由调用方组装——本模块不带任何默认采集器/目标。
"""
from __future__ import annotations

import threading
import time
from typing import Callable, Mapping


class Monitor:
    """`collectors` 是 `{采集器名: 无参可调用 -> dict}` 的映射（用 `functools.partial`
    把主机列表、URL、secret 这些参数提前绑好）；每轮 `tick()` 依次调用，拼成一份
    快照交给 `recorder.record()`。

    `interval` 秒之间用 `stop_event.wait()` 休眠，`run()` 收到置位后立即退出，
    不会卡在长睡眠里；单次 `tick()` 拆出来方便测试，不必启动线程/循环。
    """

    def __init__(
        self,
        collectors: Mapping[str, Callable[[], dict]],
        recorder,
        *,
        interval: float,
    ):
        self._collectors = dict(collectors)
        self._recorder = recorder
        self._interval = interval

    def tick(self, *, at: float | None = None) -> list[dict]:
        """跑一轮全部采集器，记录快照，返回这一轮产生的事件。"""
        at = time.time() if at is None else at
        snapshot = {name: fn() for name, fn in self._collectors.items()}
        return self._recorder.record(snapshot, at=at)

    def run(self, stop_event: threading.Event) -> None:
        """阻塞循环，直到 `stop_event` 置位。调用方自己决定放哪个线程跑。"""
        while not stop_event.is_set():
            self.tick()
            stop_event.wait(self._interval)
