"""脱敏审核的调查：建、改、发出一轮（后台跑本地代理）、追问、整理放行稿、闸门、放行、退回、删。

一次调查的状态（:mod:`.store`）::

    draft 草稿 ──发出──▶ running 在跑 ──这一轮跑完 / 出错 / 停下──▶ waiting 等用户 ──追问 / 重发──▶ running …
    draft / waiting ──放行──▶ approved 已放行（只读）      draft / waiting ──退回──▶ rejected 已退回（只读）
    哪种状态都能删（在跑的先停下）

一轮的状态：pending 待发（写好了还没发）、running、done、failed。一份调查同一时刻最多一轮在跑；只有最后一轮可以是待发。

- **追问**接着上一轮的会话：发第 n 轮时 ``--resume`` 给第 n 轮之前最近一个有会话号的那一轮回的会话号（第一轮不带）。
  重发出错的那一轮也是这样接，不接它自己失败那次的会话。
- **放行稿**缺省跟着最新一轮的回复（每答完一轮换成它的回复）；用户改过（:meth:`Outbox.save_draft`）就不再自动换。
- **闸门**查要外发的全部文字：放行查标题、用途、放行稿；退回查标题和那句话。有命中且没说「仍然放行」→ :class:`GateHits`；
  闸门查不了（:class:`~pybase.outbox.gate.GateUnavailable`）一律不放、也不退（说了仍然放行也不行）。
- 后台跑的一轮记下跑它的进程号；进程没了（服务重启）还标着在跑的，由 :meth:`Outbox.recover` 标成出错，可以重发。

出错一律抛 :class:`OutboxError` 的子类（``code`` 给程序分辨，``str(e)`` 给人看），调用方自己翻成接口回包。
提示词、回复、放行稿都不打印、不写日志。
"""
from __future__ import annotations

import json
import os
import re
import secrets
import threading
import time
from pathlib import Path
from typing import Callable, List, Optional, Sequence, Union

from . import files
from .agent import AgentResult, CommandAgent
from .gate import Gate, GateUnavailable
from .prompts import INVESTIGATE_PROMPT, fill_scope
from .store import (APPROVED, CLOSED, DRAFT, R_DONE, R_FAILED, R_PENDING, R_RUNNING, REJECTED, RUNNING, Store,
                    WAITING)

ID_RE = re.compile(r"^[A-Za-z0-9_-]{1,64}$")
TITLE_MAX = 200
PURPOSE_MAX = 4000
PROMPT_MAX = 20000
DRAFT_MAX = 200000
NOTE_MAX = 2000
ORIGIN_MAX = 20
SCOPE_MAX = 50
PATH_MAX = 4096
TS_FORMAT = "%Y-%m-%dT%H:%M:%S"
STOP_WAIT = 10.0

# 列表的先后：等用户、在跑、草稿在上，已放行、已退回在下（各自改动新的在前）
ORDER = {WAITING: 0, RUNNING: 1, DRAFT: 2, APPROVED: 3, REJECTED: 4}
STATUS_TEXT = {DRAFT: "草稿", RUNNING: "在跑", WAITING: "等用户", APPROVED: "已放行", REJECTED: "已退回"}

RESTARTED = "服务重启了，这一轮没跑完；可以重发"
STOPPED_NO_PROC = "停下了（没找到在跑的进程）"
NO_AGENT = "没有配本地代理"
NO_GATE = "没有配闸门"


class OutboxError(ValueError):
    code = "error"


class BadInput(OutboxError):
    """请求本身不对（类型、长度、格式）。"""
    code = "bad_input"


class NotFound(OutboxError):
    code = "not_found"


class Conflict(OutboxError):
    """状态不允许（在跑、已放行 / 已退回、没有要发的一轮……）。"""
    code = "conflict"


class GateHits(OutboxError):
    """闸门有命中、没说仍然放行：``hits`` 是命中串（只给用户看）。"""
    code = "gate_hits"

    def __init__(self, hits: List[str], what: str):
        super().__init__("%s里有 %d 处敏感串" % (what, len(hits)))
        self.hits = list(hits)


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


def need_id(v) -> str:
    if not isinstance(v, str) or not ID_RE.fullmatch(v):
        raise BadInput("id 要是调查号（字母、数字、- 和 _，最长 64）")
    return v


def _text(v, name: str, limit: int, required: bool = False) -> str:
    if v is None:
        if required:
            raise BadInput("缺少 %s" % name)
        return ""
    if not isinstance(v, str):
        raise BadInput("%s 要是字符串" % name)
    if len(v) > limit:
        raise BadInput("%s 超过 %d 字（现在 %d 字）" % (name, limit, len(v)))
    return v


def _prompt(v) -> str:
    t = _text(v, "prompt", PROMPT_MAX, required=True)
    if not t.strip():
        raise BadInput("提示词不能是空的")
    return t


def norm_scope(v) -> List[str]:
    """范围：路径列表（字符串，``~`` 展开，要是绝对路径；去重、保留先后）。存在与否发出时才查。"""
    if v is None:
        return []
    if not isinstance(v, (list, tuple)) or any(not isinstance(p, str) for p in v):
        raise BadInput("scope 要是路径（字符串）列表")
    out = []
    for p in v:
        p = p.strip()
        if not p:
            continue
        if len(p) > PATH_MAX:
            raise BadInput("scope 里有路径太长")
        q = os.path.normpath(os.path.expanduser(p))
        if not os.path.isabs(q):
            raise BadInput("scope 里的路径要是绝对路径：%s" % p)
        if q not in out:
            out.append(q)
    if len(out) > SCOPE_MAX:
        raise BadInput("scope 最多 %d 项" % SCOPE_MAX)
    return out


class Outbox:
    """调查的全部操作。

    ``data_dir``：存储（``outbox.sqlite3``）与发一轮时临时放附加提示词文件的地方（``run/``）；
    ``approved_dir`` / ``rejected_dir``：放行 / 退回落盘的目录（``<id>.md``）；
    ``agent``：跑一轮的本地代理（:class:`~pybase.outbox.agent.CommandAgent` 或同样有 ``start(...)`` 的对象），不给就发不出；
    ``gate``：:class:`~pybase.outbox.gate.Gate`，不给就放不了、退不了；
    ``prompt_template``：附加提示词模板（字符串，或每发一轮现取一次的无参函数；``{范围}`` 换成范围）；
    ``id_prefix``：调查号的前缀。都没有缺省目录、缺省代理（本库不带项目相关的默认值）。
    """

    def __init__(self, data_dir: str, *, approved_dir: str, rejected_dir: str, agent: Optional[CommandAgent] = None,
                 gate: Optional[Gate] = None, prompt_template: Union[str, Callable[[], str]] = INVESTIGATE_PROMPT,
                 id_prefix: str = "", clock: Callable[[], float] = time.time):
        self.data_dir = Path(os.path.expanduser(data_dir))
        files.ensure_private_dir(self.data_dir)
        self.store = Store(str(self.data_dir / "outbox.sqlite3"))
        self.run_dir = self.data_dir / "run"
        self.approved_dir = Path(os.path.expanduser(approved_dir))
        self.rejected_dir = Path(os.path.expanduser(rejected_dir))
        self.agent = agent
        self.gate = gate
        self.prompt_template = prompt_template
        self.id_prefix = id_prefix
        self._clock = clock
        self._lock = threading.Lock()
        self._runs = {}                         # 调查号 → 在跑的 Run
        self._threads = {}                      # 调查号 → 跑这一轮的线程
        self._stop_asked = set()                # 点了停下、代理还没起来的调查号（起来马上停）

    # ------------------------------------------------------------------ 看
    def _view(self, inv: dict, rounds: List[dict]) -> dict:
        out = {"id": inv["id"], "title": inv["title"], "purpose": inv["purpose"], "scope": list(inv["scope"]),
               "origin": inv["origin"], "status": inv["status"], "draft": inv["draft"],
               "draft_edited": inv["draft_edited"], "note": inv["note"], "created": _ts(inv["created_at"]),
               "updated": _ts(inv["updated_at"]), "closed": _ts(inv["closed_at"]), "rev": inv["updated_at"],
               "rounds": [{"n": r["n"], "prompt": r["prompt"], "reply": r["reply"], "status": r["status"],
                           "error": r["error"], "created": _ts(r["created_at"]), "started": _ts(r["started_at"]),
                           "finished": _ts(r["finished_at"]),
                           "seconds": (round(r["finished_at"] - r["started_at"], 1)
                                       if r["finished_at"] and r["started_at"] else None)} for r in rounds]}
        if inv["status"] == APPROVED:
            out["file"] = str(self.approved_path(inv["id"]))
        elif inv["status"] == REJECTED:
            out["file"] = str(self.rejected_path(inv["id"]))
        return out

    def _load(self, c, inv_id: str):
        inv = self.store.investigation(c, need_id(inv_id))
        if inv is None:
            raise NotFound("没有这份调查：%s" % inv_id)
        return inv

    def get(self, inv_id: str) -> dict:
        with self.store.read() as c:
            inv = self._load(c, inv_id)
            return self._view(inv, self.store.rounds(c, inv["id"]))

    def list(self) -> List[dict]:
        with self.store.read() as c:
            rows = self.store.summaries(c)
        rows.sort(key=lambda r: (ORDER.get(r["status"], 9), -(r["updated_at"] or 0), r["id"]))
        return [{"id": r["id"], "title": r["title"], "status": r["status"], "origin": r["origin"],
                 "created": _ts(r["created_at"]), "updated": _ts(r["updated_at"]), "closed": _ts(r["closed_at"]),
                 "rounds": r["rounds"], "pending": bool(r["pending"]), "last_round": r["last_round"] or "",
                 "rev": r["updated_at"]} for r in rows]

    def approved_path(self, inv_id: str) -> Path:
        return self.approved_dir / f"{need_id(inv_id)}.md"

    def rejected_path(self, inv_id: str) -> Path:
        return self.rejected_dir / f"{need_id(inv_id)}.md"

    # ------------------------------------------------------------------ 建、改
    def new_id(self) -> str:
        return "%s%s-%s" % (self.id_prefix, time.strftime("%Y%m%d-%H%M%S", time.localtime(self._clock())),
                            secrets.token_hex(3))

    def create(self, title, purpose=None, scope=None, prompt=None, origin=None, send: bool = False) -> dict:
        """建一份调查（草稿）；给了 prompt 就带一轮待发的；``send`` 建好马上发出（发不出就不建）。"""
        title = _text(title, "title", TITLE_MAX, required=True)
        if not title.strip():
            raise BadInput("标题不能是空的")
        purpose = _text(purpose, "purpose", PURPOSE_MAX)
        origin = _text(origin, "origin", ORIGIN_MAX)
        scope = norm_scope(scope)
        text = ""
        if prompt is not None:
            if not isinstance(prompt, str):
                raise BadInput("prompt 要是字符串")
            if prompt.strip():
                text = _prompt(prompt)
        if send:
            if not text:
                raise BadInput("发出要写提示词")
            self._check_can_run(scope)
        now = self._clock()
        inv_id = self.new_id()
        with self.store.tx() as c:
            c.execute("INSERT INTO investigation (id, title, purpose, scope, origin, status, created_at, updated_at) "
                      "VALUES (?,?,?,?,?,?,?,?)",
                      (inv_id, title, purpose, json.dumps(scope, ensure_ascii=False), origin, DRAFT, now, now))
            if text:
                c.execute("INSERT INTO round (inv_id, n, prompt, status, created_at) VALUES (?,?,?,?,?)",
                          (inv_id, 1, text, R_PENDING, now))
        if send:
            return self.send(inv_id)
        return self.get(inv_id)

    def update(self, inv_id, title=None, purpose=None, scope=None) -> dict:
        """改标题、用途、范围（给了哪项改哪项；已放行 / 已退回的不能改）。范围改了下一轮才用上。"""
        vals = {}
        if title is not None:
            t = _text(title, "title", TITLE_MAX)
            if not t.strip():
                raise BadInput("标题不能是空的")
            vals["title"] = t
        if purpose is not None:
            vals["purpose"] = _text(purpose, "purpose", PURPOSE_MAX)
        if scope is not None:
            vals["scope"] = json.dumps(norm_scope(scope), ensure_ascii=False)
        with self.store.tx() as c:
            inv = self._load(c, inv_id)
            self._not_closed(inv)
            if vals:
                vals["updated_at"] = self._clock()
                c.execute("UPDATE investigation SET %s WHERE id=?" % ", ".join("%s=?" % k for k in vals),
                          (*vals.values(), inv["id"]))
        return self.get(inv_id)

    def put_prompt(self, inv_id, prompt) -> dict:
        """放一轮待发的（最后一轮已是待发的就换掉它的提示词），不发出。"""
        text = _prompt(prompt)
        with self.store.tx() as c:
            inv = self._load(c, inv_id)
            self._not_closed(inv)
            if inv["status"] == RUNNING:
                raise Conflict("这份调查正在跑，等这一轮跑完再写下一轮")
            self._put_pending(c, inv["id"], text)
            c.execute("UPDATE investigation SET updated_at=? WHERE id=?", (self._clock(), inv["id"]))
        return self.get(inv_id)

    def _put_pending(self, c, inv_id: str, text: str) -> int:
        rounds = self.store.rounds(c, inv_id)
        now = self._clock()
        if rounds and rounds[-1]["status"] == R_PENDING:
            n = rounds[-1]["n"]
            c.execute("UPDATE round SET prompt=?, created_at=? WHERE inv_id=? AND n=?", (text, now, inv_id, n))
            return n
        n = (rounds[-1]["n"] if rounds else 0) + 1
        c.execute("INSERT INTO round (inv_id, n, prompt, status, created_at) VALUES (?,?,?,?,?)",
                  (inv_id, n, text, R_PENDING, now))
        return n

    @staticmethod
    def _not_closed(inv: dict) -> None:
        if inv["status"] in CLOSED:
            raise Conflict("这份调查%s，只读" % STATUS_TEXT[inv["status"]])

    # ------------------------------------------------------------------ 发出
    def _check_can_run(self, scope: Sequence[str]) -> None:
        if self.agent is None:
            raise Conflict(NO_AGENT)
        if not scope:
            raise Conflict("范围是空的：先写上可以看的目录或文件")
        missing = [p for p in scope if not os.path.exists(p)]
        if missing:
            raise Conflict("范围里这些不在：%s" % "、".join(missing))

    def _template(self) -> str:
        t = self.prompt_template() if callable(self.prompt_template) else self.prompt_template
        return t if isinstance(t, str) else INVESTIGATE_PROMPT

    def send(self, inv_id, prompt=None) -> dict:
        """发出一轮（后台跑，马上回）。``prompt`` 给了：最后一轮是待发的就换成它再发，否则新加一轮（追问）；
        没给：发最后那一轮待发的，或重发最后那一轮出错的。"""
        text = _prompt(prompt) if prompt is not None else None
        with self.store.tx() as c:
            inv = self._load(c, inv_id)
            self._not_closed(inv)
            if inv["status"] == RUNNING:
                raise Conflict("这份调查正在跑，等这一轮跑完或先停下")
            self._check_can_run(inv["scope"])
            rounds = self.store.rounds(c, inv["id"])
            last = rounds[-1] if rounds else None
            if text is not None:
                n = self._put_pending(c, inv["id"], text)
            elif last is not None and last["status"] in (R_PENDING, R_FAILED):
                n = last["n"]
            else:
                raise Conflict("没有要发的一轮：写上要问的话再发")
            rounds = self.store.rounds(c, inv["id"])
            this = [r for r in rounds if r["n"] == n][0]
            earlier = [r for r in rounds if r["n"] < n and r["session_id"]]
            resume = earlier[-1]["session_id"] if earlier else None
            now = self._clock()
            c.execute("UPDATE round SET status=?, reply='', error='', session_id=NULL, resume_from=?, started_at=?, "
                      "finished_at=NULL, runner_pid=?, agent_pid=NULL, info='{}' WHERE inv_id=? AND n=?",
                      (R_RUNNING, resume, now, os.getpid(), inv["id"], n))
            c.execute("UPDATE investigation SET status=?, updated_at=? WHERE id=?", (RUNNING, now, inv["id"]))
            job = (inv["id"], n, list(inv["scope"]), this["prompt"], resume)
        t = threading.Thread(target=self._work, args=job, name="outbox-%s-%d" % (inv["id"], n), daemon=True)
        with self._lock:
            self._threads[inv["id"]] = t
            self._stop_asked.discard(inv["id"])
        t.start()
        return self.get(inv_id)

    def _work(self, inv_id: str, n: int, scope: List[str], prompt: str, resume: Optional[str]) -> None:
        sysfile = None
        try:
            files.ensure_private_dir(self.run_dir)
            sysfile = files.write_private(self.run_dir / ("%s-%d-%s.sys.txt" % (inv_id, n, secrets.token_hex(3))),
                                          fill_scope(self._template(), scope))
            run = self.agent.start(scope=scope, system_file=str(sysfile), prompt=prompt, resume=resume)
        except Exception as e:                                   # noqa: BLE001  起不来：这一轮记出错
            if sysfile is not None:
                files.remove(sysfile)
            self._finish(inv_id, n, AgentResult(ok=False, error="起不了本地代理：%s: %s" % (type(e).__name__, e)))
            with self._lock:
                self._threads.pop(inv_id, None)
                self._stop_asked.discard(inv_id)
            return
        with self._lock:
            self._runs[inv_id] = run
            stop_now = inv_id in self._stop_asked
        if stop_now:                                             # 起来之前就点了停下
            run.stop()
        try:
            with self.store.tx() as c:
                c.execute("UPDATE round SET agent_pid=? WHERE inv_id=? AND n=? AND status=?",
                          (run.pid, inv_id, n, R_RUNNING))
            result = run.wait()
        except Exception as e:                                   # noqa: BLE001
            result = AgentResult(ok=False, error="跑这一轮出错：%s" % type(e).__name__)
        finally:
            files.remove(sysfile)
            with self._lock:
                self._runs.pop(inv_id, None)
        self._finish(inv_id, n, result)
        with self._lock:
            self._threads.pop(inv_id, None)
            self._stop_asked.discard(inv_id)

    def _finish(self, inv_id: str, n: int, res: AgentResult) -> None:
        now = self._clock()
        reply = (res.reply or "").strip()                       # 模型的回复常以空行开头：首尾空白不要
        try:
            with self.store.tx() as c:
                r = self.store.round(c, inv_id, n)
                if r is None or r["status"] != R_RUNNING:     # 删了、或已被标成出错（停下 / 重启）
                    return
                c.execute("UPDATE round SET status=?, reply=?, error=?, session_id=?, finished_at=?, info=? "
                          "WHERE inv_id=? AND n=?",
                          (R_DONE if res.ok else R_FAILED, reply, res.error or "", res.session_id, now,
                           json.dumps(res.info or {}, ensure_ascii=False), inv_id, n))
                if res.ok and reply:
                    c.execute("UPDATE investigation SET status=?, updated_at=?, "
                              "draft=CASE WHEN draft_edited=0 THEN ? ELSE draft END WHERE id=? AND status=?",
                              (WAITING, now, reply, inv_id, RUNNING))
                else:
                    c.execute("UPDATE investigation SET status=?, updated_at=? WHERE id=? AND status=?",
                              (WAITING, now, inv_id, RUNNING))
        except Exception:                                        # noqa: BLE001  库写不进：留着「在跑」，recover 收拾
            pass

    def stop(self, inv_id) -> dict:
        """停下在跑的那一轮（整组杀本地代理）：那一轮记「停下了」，调查回到等用户。"""
        with self.store.read() as c:
            inv = self._load(c, inv_id)
        if inv["status"] != RUNNING:
            raise Conflict("这份调查没在跑")
        with self._lock:
            run = self._runs.get(inv["id"])
            t = self._threads.get(inv["id"])
            if t is not None and run is None:
                self._stop_asked.add(inv["id"])
        if run is not None:
            run.stop()
        if t is not None:
            t.join(STOP_WAIT)
        if run is None and t is None:                            # 不是这个进程在跑（或已经没了）：直接记下
            self._mark_failed(inv["id"], STOPPED_NO_PROC)
        return self.get(inv_id)

    def _mark_failed(self, inv_id: str, why: str, only_if=None) -> int:
        now = self._clock()
        k = 0
        with self.store.tx() as c:
            for r in self.store.rounds(c, inv_id):
                if r["status"] != R_RUNNING or (only_if is not None and not only_if(r)):
                    continue
                c.execute("UPDATE round SET status=?, error=?, finished_at=? WHERE inv_id=? AND n=? AND status=?",
                          (R_FAILED, why, now, inv_id, r["n"], R_RUNNING))
                k += 1
            if k:
                c.execute("UPDATE investigation SET status=?, updated_at=? WHERE id=? AND status=?",
                          (WAITING, now, inv_id, RUNNING))
        return k

    def recover(self) -> int:
        """跑它的进程已经没了、还标着在跑的轮：标成出错（:data:`RESTARTED`），回改了几轮。服务启动时调一次。"""
        with self.store.read() as c:
            stale = [r for r in self.store.running_rounds(c) if not _alive(r["runner_pid"])]
        k = 0
        for inv_id in sorted({r["inv_id"] for r in stale}):
            k += self._mark_failed(inv_id, RESTARTED, only_if=lambda r: not _alive(r["runner_pid"]))
        return k

    def wait(self, inv_id, timeout: Optional[float] = None) -> dict:
        """等这份调查在跑的那一轮跑完（本进程里起的），回最新的样子。命令行与测试用。"""
        with self._lock:
            t = self._threads.get(need_id(inv_id))
        if t is not None:
            t.join(timeout)
        return self.get(inv_id)

    # ------------------------------------------------------------------ 放行稿、闸门
    def save_draft(self, inv_id, text) -> dict:
        """存放行稿（用户改过：之后答完的轮不再自动换掉它）。"""
        text = _text(text, "text", DRAFT_MAX, required=True)
        with self.store.tx() as c:
            inv = self._load(c, inv_id)
            self._not_closed(inv)
            c.execute("UPDATE investigation SET draft=?, draft_edited=1, updated_at=? WHERE id=?",
                      (text, self._clock(), inv["id"]))
        return self.get(inv_id)

    @staticmethod
    def outgoing(title: str, purpose: str, draft: str) -> str:
        return "\n".join(str(x or "") for x in (title, purpose, draft))

    def _gate(self) -> Gate:
        if self.gate is None:
            raise GateUnavailable(NO_GATE)
        return self.gate

    def check(self, inv_id, text=None) -> List[str]:
        """闸门检查要放行的文字（标题、用途、放行稿；``text`` 给了就拿它当放行稿查，窗口里还没存的也能查）→ 命中串。
        查不了抛 GateUnavailable。"""
        with self.store.read() as c:
            inv = self._load(c, inv_id)
        draft = inv["draft"] if text is None else _text(text, "text", DRAFT_MAX)
        return self._gate().check(self.outgoing(inv["title"], inv["purpose"], draft))

    # ------------------------------------------------------------------ 放行、退回、删
    def _closable(self, inv: dict, what: str) -> None:
        self._not_closed(inv)
        if inv["status"] == RUNNING:
            raise Conflict("这份调查正在跑，等这一轮跑完或先停下再%s" % what)

    def approve(self, inv_id, force: bool = False) -> dict:
        """放行：写 ``approved_dir/<id>.md``（标题、用途、放行稿），状态改成已放行。"""
        if not isinstance(force, bool):
            raise BadInput("force 要是 true/false")
        with self.store.read() as c:
            inv = self._load(c, inv_id)
        self._closable(inv, "放行")
        if not (inv["draft"] or "").strip():
            raise Conflict("放行稿是空的")
        hits = self._gate().check(self.outgoing(inv["title"], inv["purpose"], inv["draft"]))
        if hits and not force:
            raise GateHits(hits, "要放行的文字")
        path = self.approved_path(inv["id"])
        files.write_private(path, files.render_approved(inv["title"], inv["purpose"], inv["draft"]))
        try:
            now = self._clock()
            with self.store.tx() as c:
                k = c.execute("UPDATE investigation SET status=?, closed_at=?, updated_at=? WHERE id=? AND status IN "
                              "(?,?) AND title=? AND purpose=? AND draft=?",
                              (APPROVED, now, now, inv["id"], DRAFT, WAITING, inv["title"], inv["purpose"],
                               inv["draft"])).rowcount
            if k != 1:
                raise Conflict("这份调查刚被改过（或状态变了），重新看一眼再放行")
        except BaseException:
            files.remove(path)
            raise
        return self.get(inv_id)

    def reject(self, inv_id, note=None, force: bool = False) -> dict:
        """退回：写 ``rejected_dir/<id>.md``（标题和那句话；这句话也外发，过闸门），状态改成已退回。"""
        if not isinstance(force, bool):
            raise BadInput("force 要是 true/false")
        note = _text(note, "note", NOTE_MAX)
        with self.store.read() as c:
            inv = self._load(c, inv_id)
        self._closable(inv, "退回")
        hits = self._gate().check("\n".join([inv["title"], note]))
        if hits and not force:
            raise GateHits(hits, "标题和退回的话")
        path = self.rejected_path(inv["id"])
        files.write_private(path, files.render_rejected(inv["title"], note))
        try:
            now = self._clock()
            with self.store.tx() as c:
                k = c.execute("UPDATE investigation SET status=?, note=?, closed_at=?, updated_at=? WHERE id=? AND "
                              "status IN (?,?) AND title=?",
                              (REJECTED, note, now, now, inv["id"], DRAFT, WAITING, inv["title"])).rowcount
            if k != 1:
                raise Conflict("这份调查刚被改过（或状态变了），重新看一眼再退回")
        except BaseException:
            files.remove(path)
            raise
        return self.get(inv_id)

    def delete(self, inv_id) -> None:
        """删掉这份调查（在跑的先停下），已放行 / 已退回的文件一起删。"""
        with self.store.read() as c:
            inv = self._load(c, inv_id)
        if inv["status"] == RUNNING:
            try:
                self.stop(inv["id"])
            except Conflict:
                pass
        with self.store.tx() as c:
            if c.execute("DELETE FROM investigation WHERE id=?", (inv["id"],)).rowcount != 1:
                raise NotFound("没有这份调查：%s" % inv["id"])
        files.remove(self.approved_path(inv["id"]))
        files.remove(self.rejected_path(inv["id"]))
