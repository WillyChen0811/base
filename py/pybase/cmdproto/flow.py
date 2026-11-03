"""流程编排（第三轮）：解析层按样张 F 表一步步走，替代前端 composable 里的编排。

走法：
- 程序步：按 C 表顺序跑这一步的调用（`Session.run_call`，只用这次调用的绑定）。
- 人步：问 `ask(step, humans, flow)` 要这一步的值。返回 dict = 人给的值（可以空，空就走各
  人工值的缺省绑定）；`CANCEL` = 人取消；None = 还没答 → 停下，`Outcome.status == "paused"`，
  人答完调 `resume()` 从这一步接着走。ask 也可以是 dict：键是 S_id 或 (第几个, S_id)。
- 走完一步按 F 表选出边：条件为真的第一条（有条件的在前，空条件是缺省）。这一步有调用失败时，
  只认条件里写了 failed(…) 的边；没有就停下报 failed，不当成功往下走。
- 逐个循环：某步的调用有"逐个"参数（如 S321 逐文件解析），每个值单独走一遍到结束，再走下一个；
  每个新一轮把该步之后走过的步骤的调用结果、人给的值清掉（`Session.forget`）。
- 防死循环：一次 start/resume 最多走 MAX_STEPS 步。
"""

from dataclasses import dataclass, field

from .cond import evaluate
from .resolver import ResolveError
from .runtime import CommandFailed

END = "结束"
CANCEL = object()
MAX_STEPS = 200


class FlowError(RuntimeError):
    pass


@dataclass
class Outcome:
    status: str                          # done / paused / failed
    step: str = ""                       # 停在哪一步
    need: list = field(default_factory=list)   # paused：要人给的 H_id
    message: str = ""
    trail: list = field(default_factory=list)  # [(第几个, S_id, 去向)]


class Flow:
    def __init__(self, session, ask=None):
        self.s, self.reg = session, session.reg
        self.ask = ask if ask is not None else {}
        self.trail = []
        self.loop = None                 # {"step", "call", "pid", "items", "i"}
        self.paused = None
        self.given = {}                  # resume() 时人补的答案，优先于 ask
        self.resumed = None              # resume() 刚接上的那个人步：停下时已记过 visited，不再当新一轮截断

    # ---------------------------------------------------------------- 对外
    def start(self, step):
        self.trail, self.loop, self.paused, self.resumed = [], None, None, None
        return self._walk(step)

    def resume(self, answers=None):
        if not self.paused:
            raise FlowError("没有停着的步骤")
        if answers is not None:
            self.given[self._key(self.paused)] = answers
        step, self.paused = self.paused, None
        self.resumed = step
        return self._walk(step)

    @property
    def item(self):
        return self.loop["i"] if self.loop else None

    # ---------------------------------------------------------------- 内部
    def _key(self, step):
        return (self.loop["i"], step) if self.loop else step

    def _answer(self, step, humans):
        if self._key(step) in self.given:
            return self.given.pop(self._key(step))
        if callable(self.ask):
            return self.ask(step, humans, self)
        if self.loop and (self.loop["i"], step) in self.ask:
            return self.ask[(self.loop["i"], step)]
        return self.ask.get(step)

    def _walk(self, step):
        for _ in range(MAX_STEPS):
            if step == END:
                step = self._next_item()
                if step is None:
                    return Outcome("done", trail=self.trail)
                continue
            out = self._do(step)
            if isinstance(out, Outcome):
                return out
            step = out
        raise FlowError(f"走了 {MAX_STEPS} 步还没完，疑似死循环（最后在 {step}）")

    def _do(self, step):
        """做一步，返回下一步的 S_id（或 END），停下时返回 Outcome。"""
        s = self.reg.steps[step]
        if self.resumed == step and step in self.s.visited:
            self.resumed = None          # 人在停着的这一步期间敲的缓冲指令（N3：配对、右栏）留在 visited 里，别被重进截断
        else:
            self.resumed = None
            self.s.visit(step)
        humans = self.reg.humans_in(step)
        if s.get("actor") == "人" and (humans or not self.reg.calls_in(step)):
            ans = self._answer(step, humans)
            if ans is None:
                self.paused = step
                return Outcome("paused", step=step, need=[h["id"] for h in humans],
                               trail=self.trail)
            if ans is CANCEL:
                self.s.cancelled.add(step)
            else:
                bad = set(ans) - {h["id"] for h in humans}
                if bad:
                    raise FlowError(f"{step} 没有这些人工值：{sorted(bad)}")
                self.s.give(ans)
        failed = None
        if step not in self.s.cancelled:
            for call in self.reg.calls_in(step):
                try:
                    self._run(call["id"])
                except CommandFailed as e:
                    failed = e
                    break
                except ResolveError as e:                    # 缺参数：停下报清楚，不猜
                    return Outcome("failed", step=step, message=str(e), trail=self.trail)
        if not self.reg.flows_from(step) and not failed:
            nxt = END                                    # 样张里没有出边的步骤就是这条线的末端
        else:
            nxt = self._route(step, failed)
        if nxt is None:
            msg = str(failed) if failed else f"{step} 没有一条出边的条件成立"
            return Outcome("failed", step=step, message=msg, trail=self.trail)
        self.trail.append((self.item, step, nxt))
        return nxt

    def _run(self, call_id):
        if self.loop and self.loop["call"] == call_id:
            return self.s.run_call(call_id, {self.loop["pid"]: self.loop["items"][self.loop["i"]]})
        call = self.reg.calls[call_id]
        _v, fanout, _t, _c = self.s.resolver.resolve(call["cmd"], {}, call=call_id)
        if fanout and self.loop is None and self._loops(call_id):
            values = _v
            (pid,) = fanout
            self.loop = {"step": call["step"], "call": call_id, "pid": pid,
                         "items": list(values[pid]), "i": 0}
            if not self.loop["items"]:
                raise CommandFailed(call["cmd"], {"message": "逐个清单是空的"})
            return self.s.run_call(call_id, {pid: self.loop["items"][0]})
        return self.s.run_call(call_id)

    def _loops(self, call_id):
        """这次调用的逐个结果要分别往下走（有按它的输出分流的出边）。"""
        return any(f"{call_id}." in (f.get("cond") or "")
                   for f in self.reg.flows_from(self.reg.calls[call_id]["step"]))

    def _next_item(self):
        if not self.loop:
            return None
        self.loop["i"] += 1
        if self.loop["i"] >= len(self.loop["items"]):
            self.loop = None
            return None
        start = self.loop["step"]
        after = self.s.visited[self.s.visited.index(start):] if start in self.s.visited else []
        self.s.forget(after)
        return start

    def _route(self, step, failed):
        scope = self.s.scope()
        for f in self.reg.flows_from(step):
            cond = f.get("cond") or ""
            if failed and "failed(" not in cond:
                continue
            if evaluate(cond, scope):
                return f["dst"]
        return None
