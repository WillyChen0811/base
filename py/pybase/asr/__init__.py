"""语音识别：中文/英文 × 整段音频/实时，本机两个后端同一套接口，外加远程识别。

- `AppleSpeech`：macOS 26+ 系统自带的 SpeechAnalyzer，零 pip 依赖（需要 swiftc 编译随包的辅助程序）
- `MlxAudio`：mlx-audio（Apple Silicon GPU），模型由调用方选，`pip install "pybase[asr-mlx]"`
- `transcribe_remote`/`refine_cues`：服务器上「Whisper 定时间 + Qwen 出文字」（`remote`），零 pip 依赖

整段：`engine.transcribe(path) -> Transcript`（`cues` 可直接当字幕用）；
实时：`with engine.open_stream() as s: s.feed(pcm); s.results()`，喂 s16le 单声道 PCM。
便捷整段识别 `transcribe_apple`（= `apple.transcribe`）与远程识别返回 `TranscriptionResult`
（`types`：`Cue` 带 `start_ms`/`end_ms`/`speaker`，JSON 读写毫秒整数）。
不内置任何模型、语种或路径默认值。时间单位整数毫秒。
"""
from __future__ import annotations

from ._stream import Stream
from .apple import AppleSpeech, build_helper, supported_locales
from .apple import transcribe as transcribe_apple
from .mlx import MlxAudio
from .models import AsrError, Hypothesis, Transcript, TranscriptionCancelled, Word
from .remote import refine_cues, transcribe_remote
from .types import Cue, TranscriptionResult, load_result, save_result

__all__ = [
    "AppleSpeech",
    "AsrError",
    "Cue",
    "Hypothesis",
    "MlxAudio",
    "Stream",
    "Transcript",
    "TranscriptionCancelled",
    "TranscriptionResult",
    "Word",
    "build_helper",
    "load_result",
    "refine_cues",
    "save_result",
    "supported_locales",
    "transcribe_apple",
    "transcribe_remote",
]
