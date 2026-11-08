"""跑一轮的子进程（两边共用）：起进程、喂提示词、逐行读 stdout 的 JSON 事件、停下 / 超时整组杀、收结果。

两边的代理（:mod:`.omlx` 的 :class:`~pybase.outbox.omlx.OmlxAgent`、:mod:`.claude` 的
:class:`~pybase.outbox.claude.ClaudeAgent`）都是 claude CLI 的 ``--output-format stream-json``：stdout 一行一个 JSON 事件，
最后一行是 ``{"type": "result", ...}``（``result`` 回复文字、``session_id``、``is_error``、``subtype``，其余照收不认）。

- 每一行事件交给 ``on_event``（存「每一步」用，见 :mod:`.steps`）；``guard`` 看事件、回一句话就整组杀掉、这一轮记成那句话。
- 子进程自成一个进程组（停下、超时整组杀，代理自己起的子进程一起走）；stdout 只在内存里一行行过，stderr 留末尾若干行，
  都不落盘、不打印。
"""
from __future__ import annotations

import collections
import json
import os
import signal
import subprocess
import threading
from dataclasses import dataclass, field
from typing import Callable, Mapping, Optional, Sequence

ERR_MAX = 2000
STDERR_LINES = 20
KILL_GRACE = 5.0

STOPPED = "停下了"
TIMED_OUT = "超时：%g 秒没跑完"
NO_OUTPUT = "%s退出码 %s，没有回结果"
NOT_DONE = "%s没答完（%s）"
EXIT_CODE = "%s退出码 %s"

Event = Callable[[dict], None]
Guard = Callable[[dict], Optional[str]]


@dataclass
class AgentResult:
    ok: bool
    reply: str = ""
    session_id: Optional[str] = None
    error: str = ""
    info: dict = field(default_factory=dict)


def _tail(text: str, lines: int = STDERR_LINES, limit: int = ERR_MAX) -> str:
    t = "\n".join((text or "").strip().splitlines()[-lines:])
    return t[-limit:]


def parse_result(obj: Optional[dict], stderr: str, rc: Optional[int], *, timed_out: Optional[float] = None,
                 stopped: bool = False, refused: str = "", who: str = "本地代理") -> AgentResult:
    """最后一个事件（``type`` 是 ``result`` 才算结果）→ AgentResult。ok 只在：有结果、不是 is_error、退出码 0、
    没被停下 / 超时 / 拦下。``who`` 是出错话里的主语（「本地代理」「Claude」）。"""
    if refused:
        return AgentResult(ok=False, error=refused, info={"rc": rc})
    if not isinstance(obj, dict) or obj.get("type") != "result":
        if stopped:
            err = STOPPED
        elif timed_out is not None:
            err = TIMED_OUT % timed_out
        else:
            err = _tail(stderr) or NO_OUTPUT % (who, rc)
        return AgentResult(ok=False, error=err, info={"rc": rc})
    reply = obj.get("result") if isinstance(obj.get("result"), str) else ""
    sid = obj.get("session_id") if isinstance(obj.get("session_id"), str) and obj.get("session_id") else None
    info = {"rc": rc}
    for k in ("subtype", "num_turns", "duration_ms"):
        if obj.get(k) is not None:
            info[k] = obj.get(k)
    extra = obj.get("omlx")
    if isinstance(extra, dict):                          # 本地代理：claude 按云端价目算的花费对本地模型没意义，不记
        for k in ("denied", "elapsed", "model"):
            if extra.get(k) is not None:
                info[k] = extra.get(k)
    else:
        if obj.get("total_cost_usd") is not None:
            info["total_cost_usd"] = obj["total_cost_usd"]
        if isinstance(obj.get("permission_denials"), list):
            info["denied"] = len(obj["permission_denials"])
    if stopped:
        return AgentResult(ok=False, reply=reply, session_id=sid, error=STOPPED, info=info)
    if timed_out is not None:
        return AgentResult(ok=False, reply=reply, session_id=sid, error=TIMED_OUT % timed_out, info=info)
    if obj.get("is_error"):
        return AgentResult(ok=False, reply=reply, session_id=sid,
                           error=NOT_DONE % (who, obj.get("subtype") or "is_error"), info=info)
    if rc not in (0, None):
        tail = _tail(stderr)
        return AgentResult(ok=False, reply=reply, session_id=sid,
                           error=EXIT_CODE % (who, rc) + ("：" + tail if tail else ""), info=info)
    return AgentResult(ok=True, reply=reply, session_id=sid, info=info)


def last_json(stdout: str) -> Optional[dict]:
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


def parse_output(stdout: str, stderr: str, rc: Optional[int], **kw) -> AgentResult:
    """整段 stdout（取最后一行 JSON）→ AgentResult（同 :func:`parse_result`）。"""
    return parse_result(last_json(stdout), stderr, rc, **kw)


class Run:
    """一轮在跑的子进程：:meth:`wait` 一行行读事件直到它退出，回 AgentResult；:meth:`stop` 整组杀（别的线程调）。
    ``cleanup``：跑完（不管成没成）要删的临时文件。"""

    def __init__(self, proc: subprocess.Popen, prompt: str, timeout: float, *, on_event: Optional[Event] = None,
                 guard: Optional[Guard] = None, who: str = "本地代理", cleanup: Sequence[str] = ()):
        self.proc = proc
        self.pid = proc.pid
        self._input = (prompt or "").encode("utf-8")
        self._timeout = timeout
        self._on_event = on_event
        self._guard = guard
        self._who = who
        self._cleanup = list(cleanup)
        self._stopped = threading.Event()
        self._timed_out = threading.Event()
        self._refused = ""
        self._err = collections.deque(maxlen=200)
        self.init: Optional[dict] = None          # claude 的 system/init 事件（看实际的模型、工具）

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

    def refuse(self, why: str) -> None:
        if not self._refused:
            self._refused = why
        self._kill()

    def _on_timeout(self) -> None:
        self._timed_out.set()
        self._kill()

    def _feed(self) -> None:
        try:
            self.proc.stdin.write(self._input)
            self.proc.stdin.close()
        except (OSError, ValueError):
            pass

    def _drain_err(self) -> None:
        try:
            for raw in self.proc.stderr:
                self._err.append(raw.decode("utf-8", "replace").rstrip("\n"))
        except (OSError, ValueError):
            pass

    def _event(self, obj: dict) -> None:
        if obj.get("type") == "system" and obj.get("subtype") == "init" and self.init is None:
            self.init = obj
        if self._guard is not None and not self._refused:
            why = self._guard(obj)
            if why:
                self.refuse(why)
                return
        if self._on_event is not None and not self._refused:
            try:
                self._on_event(obj)
            except Exception:                                     # noqa: BLE001  存不进步骤不耽误这一轮
                pass

    def wait(self) -> AgentResult:
        feeder = threading.Thread(target=self._feed, daemon=True)
        drainer = threading.Thread(target=self._drain_err, daemon=True)
        timer = threading.Timer(self._timeout, self._on_timeout)
        timer.daemon = True
        feeder.start()
        drainer.start()
        timer.start()
        last = None
        try:
            for raw in self.proc.stdout:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("{"):
                    continue
                try:
                    obj = json.loads(line)
                except ValueError:
                    continue
                if isinstance(obj, dict):
                    last = obj
                    self._event(obj)
            self.proc.wait()
        finally:
            timer.cancel()
            drainer.join(KILL_GRACE)
            for p in self._cleanup:
                try:
                    os.unlink(p)
                except OSError:
                    pass
        res = parse_result(last, "\n".join(self._err), self.proc.returncode,
                           timed_out=self._timeout if self._timed_out.is_set() else None,
                           stopped=self._stopped.is_set(), refused=self._refused, who=self._who)
        if self.init is not None and "model" not in res.info and isinstance(self.init.get("model"), str):
            res.info["model"] = self.init["model"]
        return res


def popen(argv: Sequence[str], *, env: Mapping[str, str], cwd: Optional[str]) -> subprocess.Popen:
    """起子进程：stdin / stdout / stderr 都是管道，自成进程组。"""
    return subprocess.Popen(list(argv), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            env=dict(env), cwd=cwd, start_new_session=True)


def run_quick(argv: Sequence[str], *, env: Mapping[str, str], cwd: Optional[str], timeout: float) -> str:
    """跑一个很快就该结束的命令（列模型之类），回 stdout；超时整组杀、非 0 退出码 → RuntimeError（stderr 末几行）。"""
    p = subprocess.Popen(list(argv), stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         env=dict(env), cwd=cwd, start_new_session=True)
    try:
        out, err = p.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
        p.communicate()
        raise RuntimeError("%g 秒没答" % timeout) from None
    if p.returncode != 0:
        raise RuntimeError(_tail(err.decode("utf-8", "replace"), 5, 500) or "退出码 %s" % p.returncode)
    return out.decode("utf-8", "replace")
