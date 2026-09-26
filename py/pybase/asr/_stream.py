"""实时识别会话的公共骨架：结果队列、错误传递、with 语义。各引擎只实现怎么喂、怎么收尾。"""
from __future__ import annotations

import queue

from .models import AsrError, Hypothesis


class Stream:
    """一次实时识别会话。

    `feed()` 喂 s16le 单声道 PCM，采样率必须是 `sample_rate`（重采样是调用方的事）；
    `results()` 非阻塞取出新结果；`close()` 结束输入、等引擎收尾，返回剩下的结果。
    引擎后台出的错会在下一次 `feed`/`results`/`close` 时抛出。
    with 块正常退出时结束输入并等收尾（剩下的结果留在队列里，之后 `results()` 取），
    异常退出时 `abort()`。
    """

    def __init__(self, sample_rate: int) -> None:
        self.sample_rate = sample_rate
        self._queue: "queue.Queue[Hypothesis]" = queue.Queue()
        self._error: BaseException | None = None
        self._closed = False

    # 引擎后台线程调用
    def _put(self, hypothesis: Hypothesis) -> None:
        self._queue.put(hypothesis)

    def _fail(self, error: BaseException) -> None:
        if self._error is None:
            self._error = error

    def _raise_if_failed(self) -> None:
        if self._error is not None:
            raise self._error

    def feed(self, pcm: bytes) -> None:
        if self._closed:
            raise AsrError("会话已结束，不能再喂音频")
        self._raise_if_failed()
        self._feed(pcm)

    def results(self) -> list[Hypothesis]:
        out = []
        while True:
            try:
                out.append(self._queue.get_nowait())
            except queue.Empty:
                break
        self._raise_if_failed()
        return out

    def close(self, timeout: float | None = 60) -> list[Hypothesis]:
        self._end(timeout)
        return self.results()

    def _end(self, timeout: float | None) -> None:
        if not self._closed:
            self._closed = True
            self._finish(timeout)

    def abort(self) -> None:
        """放弃会话，不等收尾。已经 close 过的会话不用再 abort。"""
        if not self._closed:
            self._closed = True
            self._abort()

    def __enter__(self) -> "Stream":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        if exc_type is None:
            self._end(60)
        else:
            self.abort()

    def _feed(self, pcm: bytes) -> None:
        raise NotImplementedError

    def _finish(self, timeout: float | None) -> None:
        raise NotImplementedError

    def _abort(self) -> None:
        raise NotImplementedError
