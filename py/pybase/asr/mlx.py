"""mlx-audio 后端（Apple Silicon，走 GPU）：`pip install "pybase[asr-mlx]"`。

模型由调用方给（HuggingFace 仓库名或本地目录），首次加载会下载权重。
各模型的输出形状不一样，这里统一成 `Transcript`：Parakeet 给句子级时间，
Whisper 给分段（开了 `word_timestamps` 还有逐词时间），Qwen3-ASR 只有分块级时间
（默认一块 20 分钟，可用 `chunk_duration` 调小）。
"""
from __future__ import annotations

import os
import threading
import time
from concurrent.futures import ThreadPoolExecutor, TimeoutError as FutureTimeout
from typing import Any

from ._stream import Stream
from ._text import clean, ms, segment_cues
from .models import AsrError, Hypothesis, Transcript, Word


def _load(model: str) -> Any:
    try:
        from mlx_audio.stt import load
    except ImportError as exc:
        raise ImportError('mlx-audio 未安装，运行 pip install "pybase[asr-mlx]"') from exc
    return load(model)


def _cues(output: Any, max_cue_ms: int | None) -> list:
    sentences = getattr(output, "sentences", None)
    if sentences is not None:  # Parakeet 等 NeMo 系：AlignedResult
        cues = []
        for s in sentences:
            cues += segment_cues(s.text, ms(s.start), ms(s.end), max_duration_ms=max_cue_ms)
        return cues
    cues = []
    for seg in getattr(output, "segments", None) or ():
        words = tuple(
            Word(w.get("word", w.get("text", "")), ms(w.get("start")), ms(w.get("end")))
            for w in seg.get("words") or ()
        )
        cues += segment_cues(
            seg.get("text", ""), ms(seg.get("start")), ms(seg.get("end")), words, max_cue_ms
        )
    return cues


def _language(output: Any) -> str | None:
    language = getattr(output, "language", None)
    if isinstance(language, (list, tuple)):  # Qwen3-ASR 按分块给
        language = next((x for x in language if x), None)
    return language or None


class MlxAudio:
    """`generate_kwargs` 原样传给模型的 `generate`（如 Whisper 的 `language="zh"`、
    `word_timestamps=True`），`stream_kwargs` 原样传给 `create_streaming_session`。

    MLX 的 GPU stream 绑定线程，跨线程用会报 "no Stream(gpu, N) in current thread"，
    所以加载、识别、实时解码全在本实例的专属线程里串行跑：实时会话没关之前，
    同一实例的 `transcribe` 会排队等着。
    """

    def __init__(
        self,
        model: str,
        *,
        generate_kwargs: dict | None = None,
        stream_kwargs: dict | None = None,
    ) -> None:
        self.model = model
        self.generate_kwargs = dict(generate_kwargs or {})
        self.stream_kwargs = dict(stream_kwargs or {})
        self._executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="pybase-mlx")
        self._loaded: Any = None

    @property
    def name(self) -> str:
        return f"mlx-audio:{self.model}"

    def _model(self) -> Any:  # 只能在专属线程里调
        if self._loaded is None:
            self._loaded = _load(self.model)
        return self._loaded

    def _call(self, fn):
        return self._executor.submit(fn).result()

    def transcribe(self, path: str | os.PathLike, *, max_cue_ms: int | None = None) -> Transcript:
        """识别整个音频文件。不支持中途取消（mlx-audio 的 generate 是一次阻塞调用）。

        `max_cue_ms` 见 `segment_cues`，不给就不限制单条字幕时长。
        """
        output = self._call(lambda: self._model().generate(str(path), **self.generate_kwargs))
        return Transcript(
            text=clean(output.text or ""),
            cues=tuple(_cues(output, max_cue_ms)),
            engine=self.name,
            language=_language(output),
        )

    def open_stream(self) -> "MlxStream":
        """只有带 `create_streaming_session` 的模型支持（Voxtral Realtime、Nemotron 等）。

        采样率由模型决定，见返回会话的 `sample_rate`。
        """

        def create():
            model = self._model()
            if not hasattr(model, "create_streaming_session"):
                raise AsrError(f"模型 {self.model} 不支持实时识别")
            return model.create_streaming_session(**self.stream_kwargs)

        return MlxStream(self, self._call(create))

    def close(self) -> None:
        self._executor.shutdown(wait=True)


class MlxStream(Stream):
    """mlx-audio 的流式模型只给追加式增量、不断句：整个会话算一段，
    `close()` 时才出唯一一条 final，结果都没有时间。
    """

    def __init__(self, engine: MlxAudio, session: Any) -> None:
        super().__init__(int(session.input_sample_rate))
        self._session = session
        self._pending = b""
        self._stop = threading.Event()
        self._done = engine._executor.submit(self._decode)

    def _decode(self) -> None:
        text = ""
        try:
            while not self._session.done:
                if self._stop.is_set():
                    return
                deltas = self._session.step()
                if deltas:
                    text += "".join(deltas)
                    self._put(Hypothesis(text=clean(text), final=False))
                else:
                    time.sleep(0.01)
            self._put(Hypothesis(text=clean(text), final=True))
        except Exception as exc:
            self._fail(exc)

    def _feed(self, pcm: bytes) -> None:
        import numpy as np

        data = self._pending + pcm
        usable = len(data) // 2 * 2  # 奇数字节留到下一块，保证按样本对齐
        self._pending = data[usable:]
        if usable:
            samples = np.frombuffer(data[:usable], dtype="<i2").astype(np.float32) / 32768.0
            self._session.feed(samples)

    def _finish(self, timeout: float | None) -> None:
        self._session.close()
        try:
            self._done.result(timeout=timeout)
        except FutureTimeout:
            self._stop.set()
            raise AsrError(f"识别收尾超时（{timeout}s）") from None

    def _abort(self) -> None:
        self._stop.set()
        self._session.close()
