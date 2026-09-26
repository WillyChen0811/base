"""Apple SpeechAnalyzer 后端（macOS 26+，系统自带，零 pip 依赖）。

SpeechAnalyzer 只有 Swift API：随包带 `apple_speech.swift`，首次使用时用 swiftc
（Xcode Command Line Tools 自带）编译，之后以子进程调用，协议见该文件头部注释。
识别资源由系统管理，缺了会在首次识别时自动下载，所以首次调用可能很慢。
"""
from __future__ import annotations

import hashlib
import json
import os
import platform
import subprocess
import sys
import tempfile
import threading
from pathlib import Path
from typing import Callable

from ..media._proc import discard, resolve_tool, run, run_streaming
from ._stream import Stream
from ._text import clean, segment_cues
from .models import AsrError, Hypothesis, Transcript, Word

_SOURCE = Path(__file__).with_name("apple_speech.swift")


def _check_platform() -> None:
    if sys.platform != "darwin":
        raise AsrError("Apple SpeechAnalyzer 只在 macOS 上可用")
    major = platform.mac_ver()[0].split(".")[0]
    if not major.isdigit() or int(major) < 26:
        raise AsrError(f"Apple SpeechAnalyzer 需要 macOS 26+，当前 {platform.mac_ver()[0] or '未知'}")


def build_helper(
    out: str | os.PathLike | None = None, *, swiftc: str | None = None, timeout: float = 300
) -> Path:
    """编译辅助程序，返回可执行文件路径；`out` 已存在就直接用，不重编。

    `out` 不给就放系统临时目录下、按源码哈希命名，源码变了自然换一个文件名。
    """
    _check_platform()
    source = _SOURCE.read_bytes()
    if out is None:
        digest = hashlib.sha256(source).hexdigest()[:12]
        out = Path(tempfile.gettempdir()) / f"pybase-apple-speech-{digest}"
    out = Path(out)
    if out.exists():
        return out
    tool = resolve_tool(swiftc, "swiftc")
    out.parent.mkdir(parents=True, exist_ok=True)
    tmp = out.with_name(f"{out.name}.{os.getpid()}.part")
    cmd = [tool, "-O", "-parse-as-library", str(_SOURCE), "-o", str(tmp)]
    result = run(cmd, timeout=timeout)
    if result.returncode != 0 or not tmp.exists():
        discard(tmp)
        raise AsrError(f"编译 Apple 辅助程序失败：{(result.stderr or '').strip()[-2000:]}")
    os.replace(tmp, out)
    return out


def _events(stdout: str) -> list[dict]:
    events = []
    for line in stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        event = json.loads(line)
        if "error" in event:
            raise AsrError(event["error"])
        events.append(event)
    return events


def _words(event: dict) -> tuple[Word, ...]:
    return tuple(Word(w["text"], w.get("start"), w.get("end")) for w in event.get("words", ()))


def supported_locales(*, helper: str | os.PathLike | None = None, timeout: float = 60) -> dict[str, list[str]]:
    """`{"supported": [...], "installed": [...]}`，BCP 47 形式（`zh-CN`、`en-US`）。

    supported 里没装的，首次识别时系统会自动下载。
    """
    helper = str(helper or build_helper())
    result = run([helper, "locales"], timeout=timeout)
    events = _events(result.stdout)
    if result.returncode != 0 or not events:
        raise AsrError(f"查询 locale 失败：{(result.stderr or '').strip()[-2000:]}")
    return events[0]


class AppleSpeech:
    """`locale` 如 `zh-CN`/`en-US`，不做自动语种识别。`helper` 不给就首次使用时编译。

    `fast_results` 只影响实时识别：临时结果约 1s 一批（默认约一句一批），
    临时文本更不准，final 结果不受影响。
    """

    def __init__(
        self,
        locale: str,
        *,
        helper: str | os.PathLike | None = None,
        fast_results: bool = False,
    ) -> None:
        self.locale = locale
        self.fast_results = fast_results
        self._helper = helper

    @property
    def name(self) -> str:
        return f"apple-speech:{self.locale}"

    def _helper_path(self) -> str:
        if self._helper is None:
            self._helper = build_helper()
        return str(self._helper)

    def transcribe(
        self,
        path: str | os.PathLike,
        *,
        timeout: float = 3600,
        cancel_event: "threading.Event | None" = None,
        on_progress: "Callable[[str, float | None], None] | None" = None,
        max_cue_ms: int | None = None,
    ) -> Transcript:
        """识别整个音频文件。AVAudioFile 实测能读 wav/aiff/caf/m4a/mp3/flac/opus，
        读不了 Matroska（webm/mkv/mka）和 ogg，这些先用 `pybase.media.extract_audio` 转出来。

        超时/取消沿用 `pybase.media` 的 `MediaTimeoutError`/`MediaCancelled`。
        `on_progress(stage, fraction)` 里 `stage` 是 `"installing"`（识别资源下载中）
        或 `"transcribing"`，`fraction` 是 0..1 或 None（算不出时）；不给就不回调，
        识别照常跑。`max_cue_ms` 见 `segment_cues`，不给就不限制单条字幕时长。
        """
        events: list[dict] = []

        def handle(line: str) -> None:
            line = line.strip()
            if not line:
                return
            event = json.loads(line)
            if "error" in event:
                raise AsrError(event["error"])
            progress = event.get("progress")
            if progress is not None:
                if on_progress is not None:
                    on_progress(progress.get("stage", ""), progress.get("fraction"))
                return
            events.append(event)

        result = run_streaming(
            [self._helper_path(), "file", self.locale, str(path)],
            handle,
            timeout=timeout,
            cancel_event=cancel_event,
        )
        if result.returncode != 0:
            raise AsrError(f"识别失败：{(result.stderr or '').strip()[-2000:] or result.returncode}")
        finals = [e for e in events if "text" in e and e.get("final")]
        cues = []
        for e in finals:
            cues += segment_cues(e["text"], e.get("start"), e.get("end"), _words(e), max_cue_ms)
        # 英文各段自带前导空格、中文没有，原样拼接即可
        text = clean("".join(e["text"] for e in finals))
        return Transcript(text=text, cues=tuple(cues), engine=self.name, language=self.locale)

    def open_stream(self, sample_rate: int = 16000) -> "AppleStream":
        cmd = [self._helper_path(), "stream", self.locale, str(sample_rate)]
        if self.fast_results:
            cmd.append("fast")
        return AppleStream(cmd, sample_rate)


class AppleStream(Stream):
    def __init__(self, cmd: list[str], sample_rate: int) -> None:
        super().__init__(sample_rate)
        self._proc = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,
        )
        self._stderr = b""
        self._reader = threading.Thread(target=self._read_stdout, daemon=True)
        self._stderr_reader = threading.Thread(target=self._read_stderr, daemon=True)
        self._reader.start()
        self._stderr_reader.start()

    def _read_stdout(self) -> None:
        for raw in self._proc.stdout:
            try:
                event = json.loads(raw)
            except ValueError:
                continue
            if "error" in event:
                self._fail(AsrError(event["error"]))
            elif "text" in event:
                self._put(Hypothesis(
                    text=clean(event["text"]),
                    final=bool(event.get("final")),
                    start=event.get("start"),
                    end=event.get("end"),
                    words=_words(event),
                ))

    def _read_stderr(self) -> None:
        # 必须持续读走，否则 stderr 管道写满会把辅助程序堵住
        for raw in self._proc.stderr:
            self._stderr = (self._stderr + raw)[-4000:]

    def _feed(self, pcm: bytes) -> None:
        try:
            self._proc.stdin.write(pcm)
            self._proc.stdin.flush()
        except (BrokenPipeError, ValueError):
            self._reader.join(timeout=5)
            self._raise_if_failed()
            raise AsrError(f"辅助程序已退出：{self._stderr.decode('utf-8', 'replace').strip()}")

    def _finish(self, timeout: float | None) -> None:
        try:
            self._proc.stdin.close()
        except BrokenPipeError:
            pass
        try:
            self._proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            self._abort()
            raise AsrError(f"识别收尾超时（{timeout}s）")
        self._reader.join()
        self._stderr_reader.join()
        if self._proc.returncode != 0:
            self._fail(AsrError(
                f"辅助程序异常退出（{self._proc.returncode}）：{self._stderr.decode('utf-8', 'replace').strip()}"
            ))

    def _abort(self) -> None:
        if self._proc.poll() is None:
            try:
                os.killpg(self._proc.pid, 9)
            except OSError:
                pass
        self._proc.wait()
