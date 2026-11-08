"""脱敏审核的会话：一份会话两边（Claude、omlx），每边各自一条 claude CLI 会话，用户每条话选好发给哪边。

- **一轮**：哪一边、模型、提示词、回复、状态（``running`` → ``done`` / ``failed``）、错误、来源（窗口 / 终端 + 发起方名字）、
  过程（代理的每一步，跑的时候逐条落库，见 :mod:`.steps`）。两边可以同时在跑，**每边同时最多一轮**。
- **接着聊**：每边各存一个会话号（这一边最近一轮跑完回的），下一轮 ``--resume`` 它；没回会话号的（没跑起来、停下、超时）不换。
- **发给 Claude 的每一条先过闸门**：有命中且没说「仍然发送」→ :class:`GateHits`；闸门查不了（:class:`GateUnavailable`）
  一律不发（说了仍然发送也不行）。发给 omlx 不查（本地）。
- **终端发起的会话**（``origin="terminal"``，带发起方名字）：用户在窗口里可以「转回给发起方」（:meth:`Outbox.forward`，同样过闸门），
  发起方只拿得到这些转回的话（:meth:`Outbox.take_reply` / :meth:`Outbox.wait_reply`），拿不到两边的回复和过程。
- 一轮在本进程的后台线程跑（窗口），或在另起的后台进程跑（``detach=True``，终端入口用：命令退出了这一轮照跑）；
  记下跑它的进程号，进程没了还标着在跑的由 :meth:`Outbox.recover` 标成出错。停下：本进程的直接停；别的进程跑的整组杀它的代理。

出错一律抛 :class:`OutboxError` 的子类（``code`` 给程序分辨，``str(e)`` 给人看），调用方自己翻成接口回包。
提示词、回复都不打印、不写日志。
"""
from __future__ import annotations

import itertools
import json
import os
import secrets
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Callable, Dict, List, Optional, Union

from . import files
from .agent import AgentResult
from .checks import (PROMPT_MAX, QUERY_MAX, REQUESTER_MAX, TITLE_MAX, BadInput, Conflict,  # noqa: F401
                     GateHits, NotFound, OutboxError, ScopeMissing, auto_title, need_bool, need_id, need_model,
                     need_prompt, need_side, need_text, norm_scope)
from .gate import Gate, GateUnavailable
from .prompts import INVESTIGATE_PROMPT, fill_scope
from .steps import files_read, steps_of
from .store import (CLAUDE, OMLX, ORIGIN_TERMINAL, ORIGIN_WINDOW, ORIGINS, R_DONE, R_FAILED, R_RUNNING, REQUESTER,
                    SIDES, Store)

TS_FORMAT = "%Y-%m-%dT%H:%M:%S"
STOP_WAIT = 10.0
MODELS_TTL = 60.0
WAIT_POLL = 1.0
SIDE_TEXT = {CLAUDE: "Claude", OMLX: "omlx"}
RESTARTED = "跑这一轮的进程没了（服务重启了或被杀了），这一轮没跑完；可以再发一次"
STOPPED = "停下了"
NO_AGENT = "没有配 %s 一边"
NO_GATE = "没有配闸门"
NOT_TERMINAL = "这份会话不是终端发起的，没有发起方可以转回"


def _ts(t) -> str:
    return time.strftime(TS_FORMAT, time.localtime(t)) if t else ""


def _alive(pid) -> bool:
    if not pid:
        return False
    try:
        os.kill(int(pid), 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except (OSError, ValueError):
        return False
    return True


Template = Union[str, Callable[[], str]]


class Outbox:
    """会话的全部操作。

    ``data_dir``：存储（``outbox.sqlite3``）、发一轮时临时放附加提示词的 ``run/``、给 claude 当临时目录的 ``claude-tmp/``；
    ``omlx`` / ``claude``：两边的代理（:class:`~pybase.outbox.omlx.OmlxAgent` / :class:`~pybase.outbox.claude.ClaudeAgent`，
    或同样有 ``start(...)`` 的对象），不给那一边就发不出；``gate``：:class:`~pybase.outbox.gate.Gate`，不给就发不了 Claude、转不回；
    ``omlx_prompt`` / ``claude_prompt``：两边的附加提示词（字符串，或每发一轮现取一次的无参函数；omlx 的 ``{范围}`` 换成范围）；
    ``id_prefix``：会话号的前缀；``config_path``：:mod:`.config` 的配置文件（``detach=True`` 另起后台进程时交给它）。
    都没有缺省目录、缺省代理（本库不带项目相关的默认值）。
    """

    def __init__(self, data_dir: str, *, omlx=None, claude=None, gate: Optional[Gate] = None,
                 omlx_prompt: Template = INVESTIGATE_PROMPT, claude_prompt: Template = "", id_prefix: str = "",
                 clock: Callable[[], float] = time.time, config_path: Optional[str] = None):
        self.data_dir = Path(os.path.expanduser(data_dir))
        files.ensure_private_dir(self.data_dir)
        self.store = Store(str(self.data_dir / "outbox.sqlite3"))
        self.run_dir = self.data_dir / "run"
        self.claude_tmp = self.data_dir / "claude-tmp"
        if omlx is not None and getattr(omlx, "run_dir", "") is None:
            omlx.run_dir = str(self.run_dir)
        if claude is not None and getattr(claude, "tmp_dir", "") is None:
            claude.tmp_dir = str(self.claude_tmp)
        self.agents = {OMLX: omlx, CLAUDE: claude}
        self.gate = gate
        self.prompts = {OMLX: omlx_prompt, CLAUDE: claude_prompt}
        self.id_prefix = id_prefix
        self.config_path = config_path
        self._clock = clock
        self._lock = threading.Lock()
        self._runs = {}                         # (会话号, 第几条) → 在跑的 Run
        self._threads = {}                      # (会话号, 第几条) → 跑这一轮的线程
        self._stop_asked = set()                # 点了停下、代理还没起来的 (会话号, 第几条)
        self._models = None                     # (时刻, omlx 的模型清单与缺省)

    # ------------------------------------------------------------------ 看
    def _round_view(self, r: dict, steps: List[dict]) -> dict:
        st = [dict(s["data"], k=s["k"], at=_ts(s["at"])) for s in steps]
        v = {"n": r["n"], "side": r["side"], "model": r["model"] or str(r["info"].get("model") or ""),
             "prompt": r["prompt"], "reply": r["reply"], "status": r["status"], "error": r["error"],
             "source": r["source"], "requester": r["requester"], "forced": r["forced"],
             "created": _ts(r["created_at"]), "started": _ts(r["started_at"]), "finished": _ts(r["finished_at"]),
             "seconds": (round(r["finished_at"] - r["started_at"], 1)
                         if r["finished_at"] and r["started_at"] else None),
             "info": r["info"], "steps": st, "files": len(files_read(st))}
        if r["side"] == REQUESTER:
            v["delivered"] = _ts(r["delivered_at"])
        return v

    def _view(self, conv: dict, rounds: List[dict], steps: Dict[int, List[dict]]) -> dict:
        last_model = {}
        for r in rounds:
            if r["side"] in SIDES and (r["model"] or r["info"].get("model")):
                last_model[r["side"]] = r["model"] or str(r["info"].get("model"))
        return {"id": conv["id"], "title": conv["title"], "scope": list(conv["scope"]), "origin": conv["origin"],
                "requester": conv["requester"], "created": _ts(conv["created_at"]), "updated": _ts(conv["updated_at"]),
                "rev": conv["updated_at"], "running": [r["side"] for r in rounds if r["status"] == R_RUNNING],
                "requester_waiting": _alive(conv["wait_pid"]),
                "last_model": {s: last_model.get(s, "") for s in SIDES},
                "rounds": [self._round_view(r, steps.get(r["n"], [])) for r in rounds]}

    def _load(self, c, conv_id: str) -> dict:
        conv = self.store.conversation(c, need_id(conv_id))
        if conv is None:
            raise NotFound("没有这份会话：%s" % conv_id)
        return conv

    def get(self, conv_id: str) -> dict:
        with self.store.read() as c:
            conv = self._load(c, conv_id)
            return self._view(conv, self.store.rounds(c, conv["id"]), self.store.steps(c, conv["id"]))

    def list(self, q=None, origin=None) -> List[dict]:
        """全部会话（改动新的在前）；``q`` 按文字找（标题、发起方、各轮的话和回复），``origin`` 只要窗口 / 终端的。"""
        q = need_text(q, "q", QUERY_MAX).strip()
        origin = need_text(origin, "origin", 20)
        if origin and origin not in ORIGINS:
            raise BadInput("origin 要是 window 或 terminal")
        with self.store.read() as c:
            rows = self.store.summaries(c, q, origin)
        rows.sort(key=lambda r: (-(r["updated_at"] or 0), r["id"]))
        return [{"id": r["id"], "title": r["title"], "origin": r["origin"], "requester": r["requester"],
                 "created": _ts(r["created_at"]), "updated": _ts(r["updated_at"]), "rounds": r["rounds"],
                 "running": sorted(set((r["running"] or "").split(",")) - {""}),
                 "last_side": r["last_side"] or "", "last_status": r["last_status"] or "",
                 "requester_waiting": _alive(r["wait_pid"]), "rev": r["updated_at"]} for r in rows]

    def last_scope(self) -> List[str]:
        """最近一次填的 omlx 范围（窗口新建时作缺省）。"""
        with self.store.read() as c:
            return self.store.last_scope(c)

    # ------------------------------------------------------------------ 建、改
    def new_id(self) -> str:
        return "%s%s-%s" % (self.id_prefix, time.strftime("%Y%m%d-%H%M%S", time.localtime(self._clock())),
                            secrets.token_hex(3))

    def create(self, side, prompt, *, title=None, scope=None, model=None, force=None, origin=ORIGIN_WINDOW,
               requester=None, detach: bool = False) -> dict:
        """新建一份会话并把第一句话发给 ``side`` 那一边（后台跑，马上回）。发不出（没配那边、范围不在、闸门……）就不建。
        标题空就取第一句话的前 30 字。"""
        side, text = need_side(side), need_prompt(prompt)
        title = need_text(title, "title", TITLE_MAX).strip() or auto_title(text)
        scope, model, force = norm_scope(scope), need_model(model), need_bool(force)
        if origin not in ORIGINS:
            raise BadInput("origin 要是 window 或 terminal")
        requester = need_text(requester, "requester", REQUESTER_MAX).strip()
        if origin == ORIGIN_TERMINAL and not requester:
            raise BadInput("终端发起的会话要有发起方的名字")
        forced = self._preflight(side, scope, text, force, detach)
        now = self._clock()
        conv_id = self.new_id()
        with self.store.tx() as c:
            c.execute("INSERT INTO conversation (id, title, scope, scope_at, origin, requester, created_at, "
                      "updated_at) VALUES (?,?,?,?,?,?,?,?)",
                      (conv_id, title, json.dumps(scope, ensure_ascii=False), now if scope else None, origin,
                       requester, now, now))
            n = self._insert_round(c, conv_id, side, text, model, forced, origin, requester, None)
        self._launch(conv_id, n, detach)
        return self.get(conv_id)

    def update(self, conv_id, title=None, scope=None) -> dict:
        """改标题、omlx 的范围（给了哪项改哪项）。范围改了下一轮才用上。"""
        vals = {}
        if title is not None:
            t = need_text(title, "title", TITLE_MAX).strip()
            if not t:
                raise BadInput("标题不能是空的")
            vals["title"] = t
        now = self._clock()
        if scope is not None:
            vals["scope"] = json.dumps(norm_scope(scope), ensure_ascii=False)
            vals["scope_at"] = now
        with self.store.tx() as c:
            conv = self._load(c, conv_id)
            if vals:
                vals["updated_at"] = now
                c.execute("UPDATE conversation SET %s WHERE id=?" % ", ".join("%s=?" % k for k in vals),
                          (*vals.values(), conv["id"]))
        return self.get(conv_id)

    # ------------------------------------------------------------------ 发
    def _agent(self, side: str):
        a = self.agents.get(side)
        if a is None:
            raise Conflict(NO_AGENT % SIDE_TEXT[side])
        return a

    def _gate(self) -> Gate:
        if self.gate is None:
            raise GateUnavailable(NO_GATE)
        return self.gate

    @staticmethod
    def _check_scope(scope: List[str]) -> None:
        if not scope:
            raise Conflict("omlx 的范围是空的：先写上它可以看的目录或文件")
        missing = [p for p in scope if not os.path.exists(p)]
        if missing:
            raise ScopeMissing(missing)

    def _gate_check(self, text: str, force: bool, what: str) -> bool:
        """闸门查 ``text``：查不了抛 GateUnavailable；有命中没说仍然发 → GateHits。回「有命中、仍然发」。"""
        hits = self._gate().check(text)
        if hits and not force:
            raise GateHits(hits, what)
        return bool(hits)

    def _preflight(self, side: str, scope: List[str], text: str, force: bool, detach: bool) -> bool:
        self._agent(side)
        if detach and not self.config_path:
            raise Conflict("没有配置文件，不能另起后台进程跑这一轮")
        if side == OMLX:
            self._check_scope(scope)
            return False
        return self._gate_check(text, force, "要发给 Claude 的话")

    def _insert_round(self, c, conv_id: str, side: str, text: str, model: str, forced: bool, source: str,
                      requester: str, resume: Optional[str]) -> int:
        now = self._clock()
        n = (c.execute("SELECT MAX(n) FROM round WHERE conv_id=?", (conv_id,)).fetchone()[0] or 0) + 1
        c.execute("INSERT INTO round (conv_id, n, side, model, prompt, status, source, requester, forced, resume_from, "
                  "created_at, started_at, runner_pid) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
                  (conv_id, n, side, model, text, R_RUNNING, source, requester, int(forced), resume, now, now,
                   os.getpid()))
        c.execute("UPDATE conversation SET updated_at=? WHERE id=?", (now, conv_id))
        return n

    def send(self, conv_id, side, prompt, *, model=None, force=None, source=ORIGIN_WINDOW, requester=None,
             detach: bool = False) -> dict:
        """把一句话发给这份会话的 ``side`` 那一边（接着它上一轮的会话；后台跑，马上回）。这一边正在跑 → Conflict。"""
        side, text, model, force = need_side(side), need_prompt(prompt), need_model(model), need_bool(force)
        if source not in ORIGINS:
            raise BadInput("source 要是 window 或 terminal")
        requester = need_text(requester, "requester", REQUESTER_MAX).strip()
        with self.store.read() as c:
            conv = self._load(c, conv_id)
        forced = self._preflight(side, conv["scope"], text, force, detach)
        with self.store.tx() as c:
            conv = self._load(c, conv_id)
            if self.store.running(c, conv["id"], side) is not None:
                raise Conflict("%s 一边正在跑，等这一轮跑完或先停下" % SIDE_TEXT[side])
            n = self._insert_round(c, conv["id"], side, text, model, forced, source, requester,
                                   conv[side + "_session"])
        self._launch(conv["id"], n, detach)
        return self.get(conv["id"])

    def _launch(self, conv_id: str, n: int, detach: bool) -> None:
        if detach:
            self._spawn(conv_id, n)
            return
        key = (conv_id, n)
        t = threading.Thread(target=self._work, args=key, name="outbox-%s-%d" % key, daemon=True)
        with self._lock:
            self._threads[key] = t
        t.start()

    def _spawn(self, conv_id: str, n: int) -> None:
        """另起后台进程跑这一轮（``python -m pybase.outbox.worker``）：调用方退出了它照跑。"""
        root = str(Path(__file__).resolve().parents[2])                  # pybase 包所在的目录
        env = dict(os.environ)
        env["PYTHONPATH"] = os.pathsep.join([root] + [p for p in [env.get("PYTHONPATH", "")] if p])
        argv = [sys.executable, "-m", "pybase.outbox.worker", "--config", str(self.config_path), conv_id, str(n)]
        try:
            p = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                 env=env, start_new_session=True, close_fds=True)
        except OSError as e:
            self._finish(conv_id, n, AgentResult(ok=False, error="起不了后台进程：%s" % e))
            return
        with self.store.tx() as c:
            c.execute("UPDATE round SET runner_pid=? WHERE conv_id=? AND n=? AND status=?",
                      (p.pid, conv_id, n, R_RUNNING))
        threading.Thread(target=p.wait, daemon=True).start()            # 跑完收掉（本进程还活着的话）

    def run_detached(self, conv_id: str, n: int) -> None:
        """后台进程里跑这一轮（:mod:`.worker` 调）：先把跑它的进程号记成自己，这一轮已不在跑就什么也不做。"""
        with self.store.tx() as c:
            k = c.execute("UPDATE round SET runner_pid=? WHERE conv_id=? AND n=? AND status=?",
                          (os.getpid(), conv_id, n, R_RUNNING)).rowcount
        if k == 1:
            self._work(conv_id, n)

    def _system(self, side: str, scope: List[str]) -> str:
        t = self.prompts[side]
        t = t() if callable(t) else t
        t = t if isinstance(t, str) else (INVESTIGATE_PROMPT if side == OMLX else "")
        return fill_scope(t, scope) if side == OMLX else t

    def _on_event(self, conv_id: str, n: int, counter, event: dict) -> None:
        got = steps_of(event)
        if not got:
            return
        now = self._clock()
        with self.store.tx() as c:
            for s in got:
                c.execute("INSERT INTO step (conv_id, n, k, at, data) VALUES (?,?,?,?,?)",
                          (conv_id, n, next(counter), now, json.dumps(s, ensure_ascii=False)))
            c.execute("UPDATE conversation SET updated_at=? WHERE id=?", (now, conv_id))

    def _forget(self, key) -> None:
        with self._lock:
            if self._threads.get(key) is threading.current_thread():
                self._threads.pop(key, None)
            self._runs.pop(key, None)
            self._stop_asked.discard(key)

    def _work(self, conv_id: str, n: int) -> None:
        key = (conv_id, n)
        with self.store.read() as c:
            r = self.store.round(c, conv_id, n)
            conv = self.store.conversation(c, conv_id)
        if r is None or conv is None or r["status"] != R_RUNNING:
            self._forget(key)
            return
        side = r["side"]
        try:
            agent = self._agent(side)
            counter = itertools.count(1)
            run = agent.start(prompt=r["prompt"], system=self._system(side, conv["scope"]), scope=conv["scope"],
                              resume=r["resume_from"], model=r["model"] or None,
                              on_event=lambda e: self._on_event(conv_id, n, counter, e))
        except Exception as e:                                   # noqa: BLE001  起不来：这一轮记出错
            self._finish(conv_id, n, AgentResult(ok=False, error="起不了%s：%s: %s" % (
                getattr(self.agents.get(side), "who", SIDE_TEXT[side]), type(e).__name__, e)))
            self._forget(key)
            return
        with self._lock:
            self._runs[key] = run
            stop_now = key in self._stop_asked
        if stop_now:                                             # 起来之前就点了停下
            run.stop()
        try:
            with self.store.tx() as c:
                k = c.execute("UPDATE round SET agent_pid=? WHERE conv_id=? AND n=? AND status=?",
                              (run.pid, conv_id, n, R_RUNNING)).rowcount
            if k != 1:                                           # 起来之前已被别处停下 / 删掉
                run.stop()
            result = run.wait()
        except Exception as e:                                   # noqa: BLE001
            result = AgentResult(ok=False, error="跑这一轮出错：%s" % type(e).__name__)
        self._finish(conv_id, n, result)
        self._forget(key)

    def _finish(self, conv_id: str, n: int, res: AgentResult) -> None:
        now = self._clock()
        reply = (res.reply or "").strip()                       # 模型的回复常以空行开头：首尾空白不要
        model = str((res.info or {}).get("model") or "")
        try:
            with self.store.tx() as c:
                r = self.store.round(c, conv_id, n)
                if r is None or r["status"] != R_RUNNING:     # 删了、或已被标成出错（停下 / 重启）
                    return
                c.execute("UPDATE round SET status=?, reply=?, error=?, session_id=?, finished_at=?, info=?, "
                          "model=CASE WHEN model='' THEN ? ELSE model END WHERE conv_id=? AND n=?",
                          (R_DONE if res.ok else R_FAILED, reply, res.error or "", res.session_id, now,
                           json.dumps(res.info or {}, ensure_ascii=False), model, conv_id, n))
                if res.session_id and r["side"] in SIDES:
                    c.execute("UPDATE conversation SET %s_session=?, updated_at=? WHERE id=?" % r["side"],
                              (res.session_id, now, conv_id))
                else:
                    c.execute("UPDATE conversation SET updated_at=? WHERE id=?", (now, conv_id))
        except Exception:                                        # noqa: BLE001  库写不进：留着「在跑」，recover 收拾
            pass

    # ------------------------------------------------------------------ 停、收拾
    def stop(self, conv_id, side) -> dict:
        """停下这份会话 ``side`` 那一边在跑的那一轮（整组杀代理）：那一轮记「停下了」。"""
        side = need_side(side)
        with self.store.read() as c:
            conv = self._load(c, conv_id)
            r = self.store.running(c, conv["id"], side)
        if r is None:
            raise Conflict("%s 一边没在跑" % SIDE_TEXT[side])
        key = (conv["id"], r["n"])
        with self._lock:
            run = self._runs.get(key)
            t = self._threads.get(key)
            if t is not None and run is None:
                self._stop_asked.add(key)
        if run is not None:
            run.stop()
        if t is not None:
            t.join(STOP_WAIT)
        if run is None and t is None:                            # 别的进程在跑（终端发起的），或跑它的已经没了
            self._stop_elsewhere(r)
        return self.get(conv["id"])

    def _stop_elsewhere(self, r: dict) -> None:
        if self._mark_failed(r["conv_id"], STOPPED, only=lambda x: x["n"] == r["n"]) and r["agent_pid"]:
            try:                                                 # 代理自成进程组（组号 = 它的进程号）
                os.killpg(int(r["agent_pid"]), signal.SIGTERM)
            except (ProcessLookupError, PermissionError, OSError):
                pass

    def _mark_failed(self, conv_id: str, why: str, only=None) -> int:
        now = self._clock()
        k = 0
        with self.store.tx() as c:
            for r in self.store.rounds(c, conv_id):
                if r["status"] != R_RUNNING or (only is not None and not only(r)):
                    continue
                k += c.execute("UPDATE round SET status=?, error=?, finished_at=? WHERE conv_id=? AND n=? AND status=?",
                               (R_FAILED, why, now, conv_id, r["n"], R_RUNNING)).rowcount
            if k:
                c.execute("UPDATE conversation SET updated_at=? WHERE id=?", (now, conv_id))
        return k

    def recover(self) -> int:
        """跑它的进程已经没了、还标着在跑的轮：标成出错（:data:`RESTARTED`），回改了几轮。服务启动时调一次。"""
        with self.store.read() as c:
            stale = [r for r in self.store.running_rounds(c) if not _alive(r["runner_pid"])]
        k = 0
        for conv_id in sorted({r["conv_id"] for r in stale}):
            k += self._mark_failed(conv_id, RESTARTED, only=lambda r: not _alive(r["runner_pid"]))
        return k

    def wait(self, conv_id, timeout: Optional[float] = None) -> dict:
        """等这份会话在本进程里跑的轮都跑完，回最新的样子。测试用。"""
        conv_id = need_id(conv_id)
        end = None if timeout is None else time.monotonic() + timeout
        with self._lock:
            ts = [t for (cid, _n), t in self._threads.items() if cid == conv_id]
        for t in ts:
            t.join(None if end is None else max(0.0, end - time.monotonic()))
        return self.get(conv_id)

    def delete(self, conv_id) -> None:
        """删掉这份会话（在跑的先停下）。"""
        with self.store.read() as c:
            conv = self._load(c, conv_id)
            sides = sorted({r["side"] for r in self.store.rounds(c, conv["id"]) if r["status"] == R_RUNNING})
        for side in sides:
            try:
                self.stop(conv["id"], side)
            except Conflict:
                pass
        with self.store.tx() as c:
            if c.execute("DELETE FROM conversation WHERE id=?", (conv["id"],)).rowcount != 1:
                raise NotFound("没有这份会话：%s" % conv["id"])

    # ------------------------------------------------------------------ 闸门、转回发起方
    def check(self, text) -> List[str]:
        """闸门查一段话（发给 Claude 的、转回给发起方的）→ 命中串。查不了抛 GateUnavailable。"""
        return self._gate().check(need_text(text, "text", PROMPT_MAX, required=True))

    def forward(self, conv_id, text, force=None) -> dict:
        """把一段话转回给终端发起方（过闸门）；发起方用 :meth:`take_reply` / :meth:`wait_reply` 取。"""
        text, force = need_prompt(text, "text"), need_bool(force)
        with self.store.read() as c:
            conv = self._load(c, conv_id)
        if conv["origin"] != ORIGIN_TERMINAL or not conv["requester"]:
            raise Conflict(NOT_TERMINAL)
        forced = self._gate_check(text, force, "要转回给发起方的话")
        now = self._clock()
        with self.store.tx() as c:
            conv = self._load(c, conv_id)
            n = (c.execute("SELECT MAX(n) FROM round WHERE conv_id=?", (conv["id"],)).fetchone()[0] or 0) + 1
            c.execute("INSERT INTO round (conv_id, n, side, prompt, status, source, requester, forced, created_at, "
                      "started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                      (conv["id"], n, REQUESTER, text, R_DONE, ORIGIN_WINDOW, conv["requester"], int(forced), now, now,
                       now))
            c.execute("UPDATE conversation SET updated_at=? WHERE id=?", (now, conv["id"]))
        return self.get(conv["id"])

    def take_reply(self, conv_id) -> Optional[str]:
        """取走最早一条还没取的转回的话（取了就记下，下次不再给）；没有回 None。"""
        with self.store.tx() as c:
            conv = self._load(c, conv_id)
            r = c.execute("SELECT n, prompt FROM round WHERE conv_id=? AND side=? AND delivered_at IS NULL "
                          "ORDER BY n LIMIT 1", (conv["id"], REQUESTER)).fetchone()
            if r is None:
                return None
            now = self._clock()
            c.execute("UPDATE round SET delivered_at=? WHERE conv_id=? AND n=?", (now, conv["id"], r["n"]))
            c.execute("UPDATE conversation SET updated_at=? WHERE id=?", (now, conv["id"]))
            return r["prompt"]

    def wait_reply(self, conv_id, *, poll: float = WAIT_POLL, timeout: Optional[float] = None,
                   check: Optional[Callable[[], None]] = None) -> str:
        """一直等到有转回的话（:meth:`take_reply`），回它。等的时候窗口里看得到「发起方在等」。
        ``check`` 每轮看一次（抛错就不等了，如开关关了）；``timeout`` 秒还没有 → TimeoutError。会话被删了 → NotFound。"""
        conv_id = need_id(conv_id)
        with self.store.tx() as c:
            self._load(c, conv_id)
            c.execute("UPDATE conversation SET wait_pid=? WHERE id=?", (os.getpid(), conv_id))
        end = None if timeout is None else time.monotonic() + timeout
        try:
            while True:
                text = self.take_reply(conv_id)
                if text is not None:
                    return text
                if check is not None:
                    check()
                if end is not None and time.monotonic() >= end:
                    raise TimeoutError("%g 秒内没有转回的话" % timeout)
                time.sleep(poll)
        finally:
            try:
                with self.store.tx() as c:
                    c.execute("UPDATE conversation SET wait_pid=NULL WHERE id=? AND wait_pid=?", (conv_id, os.getpid()))
            except Exception:                                    # noqa: BLE001
                pass

    # ------------------------------------------------------------------ 模型
    def models(self) -> dict:
        """两边能选的模型：``{claude: {default, items, error}, omlx: {default, items, error}}``（列不出的那边 ``error`` 是原因）。
        omlx 的清单要问本机网关，成功的留 60 秒。"""
        out = {}
        a = self.agents.get(CLAUDE)
        out[CLAUDE] = ({"default": a.default_model(), "items": a.models(), "error": ""} if a is not None
                       else {"default": "", "items": [], "error": NO_AGENT % SIDE_TEXT[CLAUDE]})
        a = self.agents.get(OMLX)
        if a is None:
            out[OMLX] = {"default": "", "items": [], "error": NO_AGENT % SIDE_TEXT[OMLX]}
            return out
        now = time.monotonic()
        with self._lock:
            cached = self._models if self._models is not None and now - self._models[0] < MODELS_TTL else None
        if cached is not None:
            out[OMLX] = dict(cached[1])
            return out
        try:
            items = a.models()
        except Exception as e:                                   # noqa: BLE001
            out[OMLX] = {"default": "", "items": [], "error": "列不出本地模型：%s" % e}
            return out
        try:
            default = a.default_model()
        except Exception:                                        # noqa: BLE001  缺省问不出：窗口显示「缺省」
            default = ""
        out[OMLX] = {"default": default, "items": items, "error": ""}
        with self._lock:
            self._models = (now, dict(out[OMLX]))
        return out
