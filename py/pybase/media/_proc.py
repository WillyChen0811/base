"""子进程执行的公共小工具：找可执行文件、跑子进程且支持超时/取消、
落盘产物时先写临时文件再原子 `replace`。

只管"怎么跑、怎么收尾"，命令行怎么拼是 probe/clip/sync 各自的事。
"""
from __future__ import annotations

import os
import shutil
import signal
import subprocess
import threading
import time
from pathlib import Path
from typing import Callable, Sequence


class MediaError(RuntimeError):
    """媒体处理失败的基类。"""


class ToolNotFoundError(MediaError):
    """找不到需要的可执行文件（ffmpeg/ffprobe/ffsubsync 等）。"""


class MediaTimeoutError(MediaError):
    """子进程跑超时，已被杀掉。"""


class MediaCancelled(MediaError):
    """`cancel_event` 置位，子进程已被杀掉。"""


def resolve_tool(explicit: str | os.PathLike | None, name: str) -> str:
    """`explicit` 优先；没给就用 `shutil.which` 按 `name` 找 PATH；
    都找不到抛 `ToolNotFoundError`，消息里说清缺的是哪个工具。
    """
    if explicit:
        candidate = str(explicit)
        found = (
            candidate
            if os.path.isfile(candidate) and os.access(candidate, os.X_OK)
            else shutil.which(candidate)
        )
        if not found:
            raise ToolNotFoundError(f"指定的可执行文件不存在或不可执行：{candidate!r}")
        return found
    found = shutil.which(name)
    if not found:
        raise ToolNotFoundError(
            f"找不到可执行文件 {name!r}：请安装后加入 PATH，或显式传入其路径"
        )
    return found


def run(
    cmd: Sequence[str],
    *,
    timeout: float,
    cancel_event: "threading.Event | None" = None,
    poll_interval: float = 0.2,
) -> subprocess.CompletedProcess:
    """跑一个子进程，轮询 `timeout`/`cancel_event`，命中就杀掉子进程。

    反复以短超时调用 `communicate()` 是标准库支持的轮询写法：`TimeoutExpired`
    时已读到的输出会留在内部缓冲区，下次调用接着读，不会丢数据。
    """
    process = subprocess.Popen(
        list(cmd),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
        errors="replace",
        # 独立进程组：ffsubsync 这类工具自己还会起 ffmpeg，只杀父进程会留下孤儿
        **_GROUP_KWARGS,
    )
    started = time.monotonic()
    while True:
        try:
            stdout, stderr = process.communicate(timeout=poll_interval)
            break
        except subprocess.TimeoutExpired:
            if cancel_event is not None and cancel_event.is_set():
                _kill(process)
                raise MediaCancelled(f"任务已取消：{cmd[0]}")
            if time.monotonic() - started > timeout:
                _kill(process)
                raise MediaTimeoutError(f"命令超时（{timeout}s）：{cmd[0]}")
    return subprocess.CompletedProcess(list(cmd), process.returncode, stdout, stderr)


_GROUP_KWARGS = (
    {"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP}
    if os.name == "nt" else {"start_new_session": True}
)


def _kill(process: subprocess.Popen) -> None:
    try:
        if os.name == "nt":
            # Windows 没有进程组信号，taskkill /T 连子进程树一起杀
            subprocess.run(
                ["taskkill", "/F", "/T", "/PID", str(process.pid)],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
        else:
            os.killpg(process.pid, signal.SIGKILL)
    except OSError:
        pass
    try:
        process.communicate(timeout=5)
    except Exception:
        pass


def discard(tmp: str | os.PathLike) -> None:
    """删掉一个可能不存在的临时文件，静默忽略删不掉的情况。"""
    try:
        Path(tmp).unlink()
    except OSError:
        pass


def tmp_path_for(out: str | os.PathLike) -> Path:
    """同目录临时文件路径，保留原扩展名（`clip.mp3` -> `clip.part.mp3`）。

    不能简单追加 `.part`（`clip.mp3.part`）：ffmpeg/ffsubsync 这类工具按
    输出文件名的扩展名猜格式，扩展名不在结尾会直接报错（"Unable to choose
    an output format"）。
    """
    out = Path(out)
    return out.with_name(out.stem + ".part" + out.suffix)


def run_to_file(
    cmd_for_output: Callable[[Path], list[str]],
    out_path: str | os.PathLike,
    *,
    timeout: float,
    cancel_event: "threading.Event | None" = None,
) -> subprocess.CompletedProcess:
    """跑一个会落盘产物的子进程：先写同目录临时文件，成功才 `os.replace`
    成 `out_path`；失败/超时/取消都会删掉临时文件，不留半截产物。

    `cmd_for_output(tmp_path)` 返回完整命令行——临时路径插进命令的哪个
    参数由调用方决定（ffmpeg 是位置输出参数，ffsubsync 是 `-o`）。
    """
    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    tmp = tmp_path_for(out)
    try:
        result = run(cmd_for_output(tmp), timeout=timeout, cancel_event=cancel_event)
    except (MediaCancelled, MediaTimeoutError):
        discard(tmp)
        raise
    if result.returncode != 0 or not tmp.exists():
        discard(tmp)
        detail = (result.stderr or "").strip()[-2000:] or f"exit code {result.returncode}"
        raise MediaError(f"命令执行失败：{detail}")
    os.replace(tmp, out)
    return result
