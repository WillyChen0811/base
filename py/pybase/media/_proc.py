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
    """解析一个可执行文件的路径，返回可直接用于拼命令行的绝对/可执行路径字符串。

    `explicit` 优先：给了就校验它存在且可执行（或能在 PATH 里按它解析），
    无效抛 `ToolNotFoundError`；没给就用 `shutil.which` 按 `name` 在 PATH 里找。
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
    """跑一个子进程并等它结束，轮询 `timeout`/`cancel_event`，命中就杀掉整个进程组。

    给需要自己拼命令的调用方用（如拼一条 ffmpeg 命令读它的 stderr）；
    probe/clip/sync 等封装函数内部也走这里，超时/取消语义一致。

    - `cmd`：参数列表（不走 shell）；子进程在独立进程组里启动，
      超时/取消时连同它自己起的子进程（如 ffsubsync 起的 ffmpeg）一起杀掉。
    - `timeout`：秒数，超时抛 `MediaTimeoutError`；`cancel_event` 置位抛
      `MediaCancelled`。
    - 返回 `subprocess.CompletedProcess`，`stdout`/`stderr` 为 UTF-8 文本
      （非法字节替换）；**不检查返回码**，失败与否由调用方看 `returncode` 决定。

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


def _kill(process: subprocess.Popen, *, drain: bool = True) -> None:
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
    if drain:
        # 调用方自己在读 stdout/stderr（run_streaming）时不能再用 communicate 抢读，
        # 否则两边线程同时读同一个管道对象，行为未定义
        try:
            process.communicate(timeout=5)
        except Exception:
            pass


def run_streaming(
    cmd: Sequence[str],
    on_line: Callable[[str], None],
    *,
    timeout: float,
    cancel_event: "threading.Event | None" = None,
    poll_interval: float = 0.2,
) -> subprocess.CompletedProcess:
    """跟 `run()` 语义一致（超时/取消都整组杀掉），但边跑边把 stdout 逐行喂给
    `on_line`，用于需要实时进度的场景——`run()` 要等进程退出才能拿到输出。

    stdout 在独立线程里读，主线程只按 `poll_interval` 轮询超时/取消，命中就杀掉
    子进程（会让 stdout 读到 EOF，读线程随之结束）；stderr 也要开线程持续读走，
    否则写满会堵住子进程。`on_line` 里抛的异常会在 stdout 线程读完后原样重新抛出。
    """
    process = subprocess.Popen(
        list(cmd),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
        errors="replace",
        **_GROUP_KWARGS,
    )
    stderr_chunks: list[str] = []
    line_errors: list[BaseException] = []

    def read_stdout() -> None:
        try:
            for line in process.stdout:
                on_line(line)
        except BaseException as exc:  # noqa: BLE001 - 转到主线程重新抛出
            line_errors.append(exc)

    def read_stderr() -> None:
        stderr_chunks.append(process.stderr.read() or "")

    out_thread = threading.Thread(target=read_stdout, daemon=True)
    err_thread = threading.Thread(target=read_stderr, daemon=True)
    out_thread.start()
    err_thread.start()

    started = time.monotonic()
    killed: MediaError | None = None
    while out_thread.is_alive():
        out_thread.join(poll_interval)
        if not out_thread.is_alive():
            break
        if cancel_event is not None and cancel_event.is_set():
            killed = MediaCancelled(f"任务已取消：{cmd[0]}")
            _kill(process, drain=False)
            break
        if time.monotonic() - started > timeout:
            killed = MediaTimeoutError(f"命令超时（{timeout}s）：{cmd[0]}")
            _kill(process, drain=False)
            break

    out_thread.join()
    returncode = process.wait()
    err_thread.join()
    if killed is not None:
        raise killed
    if line_errors:
        raise line_errors[0]
    return subprocess.CompletedProcess(list(cmd), returncode, "", "".join(stderr_chunks))


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
