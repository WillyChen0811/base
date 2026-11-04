"""识别结果的数据模型。时间单位整数毫秒，引擎给不出时间的字段是 None。"""
from __future__ import annotations

from dataclasses import dataclass

from ..subtitle.models import Cue


class AsrError(RuntimeError):
    """识别失败：引擎不可用、模型不支持、辅助程序报错等。"""


class TranscriptionCancelled(AsrError):
    """调用方置位了 `cancel_event`（`remote` 与 `apple.transcribe` 用；
    `AppleSpeech.transcribe` 自己仍沿用 `pybase.media.MediaCancelled`）。
    同 `MediaCancelled` 之于 `MediaError`：要和失败分开处理就先 except 它。"""


@dataclass(frozen=True)
class Word:
    """一个词（中文一般是一个字）。`text` 保留引擎原样的前导空格，直接拼接即得原文。"""

    text: str
    start: int | None
    end: int | None


@dataclass(frozen=True)
class Hypothesis:
    """实时识别的一条结果。

    `text` 是当前这一段（上一条 final 之后）到目前为止的完整文本，不是增量：
    `final=False` 的结果会被同一段的下一条整条替换，`final=True` 之后开始新的一段。
    """

    text: str
    final: bool
    start: int | None = None
    end: int | None = None
    words: tuple[Word, ...] = ()


@dataclass(frozen=True)
class Transcript:
    """整段音频的识别结果。

    `cues` 按句切分（引擎给了逐词时间时）或按引擎自己的分段；引擎完全不给时间时为空，
    此时只有 `text`。`engine` 可直接填进字幕来源的 `TrackMetadata.engine`。
    """

    text: str
    cues: tuple[Cue, ...]
    engine: str
    language: str | None = None
