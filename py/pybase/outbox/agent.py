"""调用本地代理跑一轮调查（子进程）。

调用契约（本地代理照这个实现，如 omlx-agent 的 ``--investigate`` 模式；详见同目录 README.md）::

    <命令…> --investigate [--scope 路径]... --system-file 文件 [--resume 会话号] -

- ``--scope``：可以看的目录或文件（绝对路径，可多次）；``--system-file``：附加系统提示（已填好范围）的文件；
  ``--resume``：接着这个会话问（上一轮回的会话号）；最后的 ``-``：提示词从 stdin 读（UTF-8）。
- stdout 最后一行是一个 JSON 对象（形状同 claude CLI ``--output-format json`` 的结果）：``result``（回复文字）、
  ``session_id``（这一轮的会话号，下一轮 ``--resume`` 用它）、``is_error``、``subtype``，其余字段照收不认。
- 退出码 0 = 答完；非 0 且没有那行 JSON = 没跑起来（连不上模型等），原因在 stderr。

子进程自成一个进程组（停下、超时整组杀，代理自己起的子进程一起走）；stdout / stderr 只在内存里解析，不落盘、不打印。
"""
from __future__ import annotations

import json
import os
import signal
import subprocess
import threading
from dataclasses import dataclass, field
from typing import List, Mapping, Optional, Sequence

ERR_MAX = 2000
STDERR_LINES = 20
KILL_GRACE = 5.0

STOPPED = "停下了"
TIMED_OUT = "超时：%g 秒没跑完"
NO_OUTPUT = "本地代理退出码 %s，没有回结果"
NOT_DONE = "本地代理没答完（%s）"
EXIT_CODE = "本地代理退出码 %s"


@dataclass
class AgentResult:
    ok: bool
    reply: str = ""
    session_id: Optional[str] = None
    error: str = ""
    info: dict = field(default_factory=dict)


def investigate_argv(cmd: Sequence[str], scope: Sequence[str], system_file: str, resume: Optional[str] = None) -> List[str]:
    """照调用契约拼命令行（提示词走 stdin）。"""
    argv = list(cmd) + ["--investigate"]
    for p in scope:
        argv += ["--scope", str(p)]
    argv += ["--system-file", system_file]
    if resume:
        argv += ["--resume", resume]
    argv.append("-")
    return argv


def _tail(text: str, lines: int = STDERR_LINES, limit: int = ERR_MAX) -> str:
    t = "\n".join((text or "").strip().splitlines()[-lines:])
    return t[-limit:]


def _last_json(stdout: str) -> Optional[dict]:
    for line in reversed((stdout or "").splitlines()):
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if isinstance(obj, dict):
            return obj
    return None


def parse_output(stdout: str, stderr: str, rc: Optional[int], *, timed_out: Optional[float] = None,
                 stopped: bool = False) -> AgentResult:
    """代理的输出 → AgentResult（ok 只在：有结果 JSON、不是 is_error、退出码 0、没被停下 / 超时）。"""
    obj = _last_json(stdout)
    if obj is None:
        if stopped:
            err = STOPPED
        elif timed_out is not None:
            err = TIMED_OUT % timed_out
        else:
            err = _tail(stderr) or NO_OUTPUT % rc
        return AgentResult(ok=False, error=err, info={"rc": rc})
    reply = obj.get("result") if isinstance(obj.get("result"), str) else ""
    sid = obj.get("session_id") if isinstance(obj.get("session_id"), str) and obj.get("session_id") else None
    info = {"rc": rc}
    for k in ("subtype", "num_turns", "duration_ms"):
        if obj.get(k) is not None:
            info[k] = obj.get(k)
    extra = obj.get("omlx")
    if isinstance(extra, dict):
        for k in ("denied", "elapsed", "model"):
            if extra.get(k) is not None:
                info[k] = extra.get(k)
    if stopped:
        return AgentResult(ok=False, reply=reply, session_id=sid, error=STOPPED, info=info)
    if timed_out is not None:
        return AgentResult(ok=False, reply=reply, session_id=sid, error=TIMED_OUT % timed_out, info=info)
    if obj.get("is_error"):
        err = NOT_DONE % (obj.get("subtype") or "is_error")
        return AgentResult(ok=False, reply=reply, session_id=sid, error=err, info=info)
    if rc not in (0, None):
        err = EXIT_CODE % rc
        tail = _tail(stderr)
        return AgentResult(ok=False, reply=reply, session_id=sid, error=err + ("：" + tail if tail else ""), info=info)
    return AgentResult(ok=True, reply=reply, session_id=sid, info=info)


class Run:
    """一轮在跑的子进程：:meth:`wait` 等它跑完回 AgentResult；:meth:`stop` 整组杀（别的线程调）。"""

    def __init__(self, proc: subprocess.Popen, prompt: str, timeout: float):
        self.proc = proc
        self.pid = proc.pid
        self._input = (prompt or "").encode("utf-8")
        self._timeout = timeout
        self._stopped = threading.Event()

    def _kill(self) -> None:
        try:
            os.killpg(self.proc.pid, signal.SIGTERM)
        except (ProcessLookupError, PermissionError):
            return
        try:
            self.proc.wait(KILL_GRACE)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(self.proc.pid, signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                pass

    def stop(self) -> None:
        self._stopped.set()
        self._kill()

    def wait(self) -> AgentResult:
        timed_out = None
        try:
            out, err = self.proc.communicate(self._input, timeout=self._timeout)
        except subprocess.TimeoutExpired:
            timed_out = self._timeout
            self._kill()
            out, err = self.proc.communicate()
        return parse_output(out.decode("utf-8", "replace"), err.decode("utf-8", "replace"), self.proc.returncode,
                            timed_out=timed_out, stopped=self._stopped.is_set())


class CommandAgent:
    """照调用契约起本地代理。``cmd`` 是代理的命令（如 ``["/path/omlx-agent"]``，可带固定参数）；
    ``timeout`` 一轮最多跑多少秒；``env`` 在本进程环境上加改的变量；``cwd`` 子进程的工作目录（缺省不改）。"""

    def __init__(self, cmd: Sequence[str], *, timeout: float = 3600.0, env: Optional[Mapping[str, str]] = None,
                 cwd: Optional[str] = None):
        if not cmd:
            raise ValueError("本地代理的命令不能为空")
        self.cmd = list(cmd)
        self.timeout = float(timeout)
        self.env = dict(env or {})
        self.cwd = cwd

    def argv(self, scope: Sequence[str], system_file: str, resume: Optional[str]) -> List[str]:
        return investigate_argv(self.cmd, scope, system_file, resume)

    def start(self, *, scope: Sequence[str], system_file: str, prompt: str, resume: Optional[str]) -> Run:
        env = dict(os.environ)
        env.update(self.env)
        proc = subprocess.Popen(self.argv(scope, system_file, resume), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, env=env, cwd=self.cwd, start_new_session=True)
        return Run(proc, prompt, self.timeout)
