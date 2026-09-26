"""字幕的基础数据模型。

只放数据结构，不放解析/序列化逻辑（那是 `io_.py` 的事）。
"""
from __future__ import annotations

from dataclasses import dataclass

_UNSET = object()  # 哨兵：跟真实时间戳（含 0、负值）不会撞


@dataclass(frozen=True)
class Cue:
    """一条字幕。

    时间单位统一为整数毫秒（与 pysubs2 内部表示一致，读写时不用再做单位换算）。

    `original_start`/`original_end` 记录校准前（`shift` 之前）的时间，语义上
    对应"平移后仍能还原"的需求：直接构造一条 cue 时不传就默认等于 `start`/`end`；
    经过 `shift` 之后，`original_*` 保持首次赋值不变，只有 `start`/`end` 变化，
    `reset_timing` 就是把 `start`/`end` 重新赋回 `original_*`。
    """

    start: int
    end: int
    text: str
    original_start: int = _UNSET  # type: ignore[assignment]
    original_end: int = _UNSET  # type: ignore[assignment]

    def __post_init__(self) -> None:
        # frozen dataclass 不能直接赋值，用 object.__setattr__ 补默认值。
        if self.original_start is _UNSET:
            object.__setattr__(self, "original_start", self.start)
        if self.original_end is _UNSET:
            object.__setattr__(self, "original_end", self.end)
