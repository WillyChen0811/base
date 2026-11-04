"""远程识别（`remote`）与 Apple 便捷识别（`apple.transcribe`）共用的结果类型。

时间一律整数毫秒，JSON 里也是毫秒整数（字段名带 `_ms`，不会和秒混）：

    {"engine": "omlx:<timing_model>+<text_model>",
     "cues": [{"start_ms": 0, "end_ms": 2400, "text": "第一句。", "speaker": ""}]}

`speaker` 是谁在说（识别后端给不出时为空串），由调用方按麦克风轨对齐、会议转写等补上。

`AppleSpeech`/`MlxAudio` 的 `transcribe` 仍返回 `models.Transcript`（`cues` 是
`pybase.subtitle.Cue`，字段 `start`/`end`），外部调用方按那个形状用着，不改；
两边互转用 `Cue.from_subtitle`/`Cue.to_subtitle`/`TranscriptionResult.from_transcript`。
"""
from __future__ import annotations

import json
import os
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Any, Iterable

if TYPE_CHECKING:
    from ..subtitle.models import Cue as SubtitleCue
    from .models import Transcript


def _is_ms(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


@dataclass(frozen=True)
class Cue:
    """一句识别结果：`[start_ms, end_ms]` 是相对音频开头的毫秒。

    构造时校验：时间必须是 int（bool 不算）、`0 <= start_ms <= end_ms`，
    `text`/`speaker` 必须是字符串。不合法抛 `TypeError`/`ValueError`。
    """

    start_ms: int
    end_ms: int
    text: str
    speaker: str = ""

    def __post_init__(self) -> None:
        if not _is_ms(self.start_ms) or not _is_ms(self.end_ms):
            raise TypeError(f"Cue 时间必须是整数毫秒：{self.start_ms!r}, {self.end_ms!r}")
        if self.start_ms < 0 or self.end_ms < self.start_ms:
            raise ValueError(f"Cue 时间不合法：{self.start_ms}–{self.end_ms}")
        if not isinstance(self.text, str) or not isinstance(self.speaker, str):
            raise TypeError("Cue 的 text/speaker 必须是字符串")

    def to_dict(self) -> dict:
        return {"start_ms": self.start_ms, "end_ms": self.end_ms,
                "text": self.text, "speaker": self.speaker}

    @classmethod
    def from_dict(cls, data: Any) -> "Cue":
        """`speaker` 可省（当空串）；其余字段缺了或类型不对抛 `ValueError`。"""
        if not isinstance(data, dict):
            raise ValueError("cue 必须是 JSON 对象")
        try:
            return cls(data["start_ms"], data["end_ms"], data["text"], data.get("speaker", ""))
        except KeyError as exc:
            raise ValueError(f"cue 缺少字段 {exc.args[0]!r}") from exc
        except TypeError as exc:
            raise ValueError(str(exc)) from exc

    @classmethod
    def from_subtitle(cls, cue: "SubtitleCue", *, speaker: str = "") -> "Cue":
        """`pybase.subtitle.Cue`（`start`/`end` 毫秒）转过来，只取时间和文字。"""
        return cls(cue.start, cue.end, cue.text, speaker)

    def to_subtitle(self) -> "SubtitleCue":
        """转成 `pybase.subtitle.Cue`（字幕读写、`digest` 用）；`speaker` 没有对应字段，丢掉。"""
        from ..subtitle.models import Cue as SubtitleCue

        return SubtitleCue(self.start_ms, self.end_ms, self.text)


@dataclass(frozen=True)
class TranscriptionResult:
    """一次识别的产出：按时间排好的 `cues`（tuple）+ 引擎标识 `engine`。"""

    cues: tuple[Cue, ...]
    engine: str

    def __post_init__(self) -> None:
        cues = tuple(self.cues)
        for index, cue in enumerate(cues, 1):
            if not isinstance(cue, Cue):
                raise TypeError(f"第 {index} 条不是 pybase.asr.Cue：{type(cue).__name__}")
        if not isinstance(self.engine, str):
            raise TypeError("engine 必须是字符串")
        object.__setattr__(self, "cues", cues)

    def to_dict(self) -> dict:
        return {"engine": self.engine, "cues": [cue.to_dict() for cue in self.cues]}

    @classmethod
    def from_dict(cls, data: Any) -> "TranscriptionResult":
        if not isinstance(data, dict):
            raise ValueError("识别结果必须是 JSON 对象")
        engine, cues = data.get("engine"), data.get("cues")
        if not isinstance(engine, str):
            raise ValueError("识别结果缺少字符串字段 engine")
        if not isinstance(cues, list):
            raise ValueError("识别结果缺少数组字段 cues")
        parsed = []
        for index, item in enumerate(cues, 1):
            try:
                parsed.append(Cue.from_dict(item))
            except ValueError as exc:
                raise ValueError(f"第 {index} 条 cue 不合法：{exc}") from exc
        return cls(tuple(parsed), engine)

    @classmethod
    def from_transcript(cls, transcript: "Transcript") -> "TranscriptionResult":
        """`AppleSpeech`/`MlxAudio` 的 `Transcript` 转过来（丢掉整段 `text`/`language`）。"""
        return cls(tuple(Cue.from_subtitle(c) for c in transcript.cues), transcript.engine)


def dumps_result(result: TranscriptionResult) -> str:
    return json.dumps(result.to_dict(), ensure_ascii=False, indent=1) + "\n"


def loads_result(text: str) -> TranscriptionResult:
    """解析失败（不是 JSON、形状不对）一律 `ValueError`。"""
    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ValueError(f"识别结果不是合法 JSON：{exc}") from exc
    return TranscriptionResult.from_dict(data)


def save_result(result: TranscriptionResult, path: str | os.PathLike) -> Path:
    """写 UTF-8 JSON：先写同目录临时文件再 `os.replace`，不留半截文件。"""
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    tmp = out.with_name(f"{out.name}.{os.getpid()}.{threading.get_ident()}.tmp")
    try:
        tmp.write_text(dumps_result(result), encoding="utf-8")
        os.replace(tmp, out)
    finally:
        tmp.unlink(missing_ok=True)
    return out


def load_result(path: str | os.PathLike) -> TranscriptionResult:
    return loads_result(Path(path).read_text(encoding="utf-8"))


def as_cues(cues: Iterable[Cue]) -> list[Cue]:
    """校验一串 cue 都是本模块的 `Cue`（不是 `pybase.subtitle.Cue`），不是就 `TypeError`。"""
    values = list(cues)
    for index, cue in enumerate(values, 1):
        if not isinstance(cue, Cue):
            raise TypeError(
                f"第 {index} 条不是 pybase.asr.Cue（{type(cue).__name__}）；"
                "字幕 Cue 先用 Cue.from_subtitle 转"
            )
    return values
