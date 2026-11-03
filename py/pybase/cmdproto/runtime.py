"""会话：解析一行指令 → 解析参数 → 调函数 → 记输出、设上下文、记走过的步骤。

上一步输出按指令名记最近一次（`history[cmd]`），另记 `prev`（紧挨着的上一条）；上下文由
X 表设定来源指到的指令输出设；配置从 `env.store.get_config(key, default)` 读（env 由调用方给，
只要求有这一个方法；没有配置类绑定可以传 None）。

visited：每跑一条指令，找它对应流程表里哪次调用 C——指令只有一次调用就是它；有多次时看这次
选中的绑定行，只有一次调用全都对得上才算（对不上或多次都对得上就不记，不猜）。回到已走过的
步骤视为新一轮（如逐文件处理的下一个文件），之后的记录清掉。

实现表 `impls`：``cmd -> Impl(fn, args)``，`fn(env, **kwargs)`，`args` 是 P_id → 形参名；
返回 dict 里 `success is False` 或 `status == "error"` 算失败（`failed()`），抛 `CommandFailed`。
原子表 `atoms`：``cmd -> fn``，形参名按 `atoms.kwarg` 规则由参数名得出。

项目自己的策略（来源筛选、路径白名单、执行前后同步……）写在子类里，覆盖这几个钩子：
`prepare(cmd, flags)`（解析完指令行、解参数前）、`check(values, trace)`（解完参数、执行前）、
`before_execute(cmd, values) -> state` / `after_execute(cmd, values, state)`（真正调函数前后，
after 在 finally 里）、`finish(cmd, result) -> result`（`run` 记账前改写结果）。
"""

from dataclasses import dataclass
from typing import Callable

from .atoms import MISSING, atom_lookup, kwarg
from .cond import Scope
from .parser import parse
from .registry import signature
from .resolver import Resolver, extract, parse_pick


@dataclass
class Impl:
    fn: Callable        # fn(env, **kwargs)
    args: dict          # P_id -> 形参名；不在表里的 P_id 不传


class CommandFailed(RuntimeError):
    def __init__(self, cmd, result):
        self.result = result
        msg = result.get("message") if isinstance(result, dict) else str(result)
        super().__init__(f"{cmd} 失败：{msg}")


class NotWired(RuntimeError):
    pass


def failed(result) -> bool:
    if not isinstance(result, dict):
        return False
    return result.get("success") is False or result.get("status") == "error"


class Session:
    def __init__(self, env, registry, entry=None, impls=None, atoms=None, computed_defaults=None):
        self.env = env
        self.reg = registry
        self.entry = entry
        self.impls = impls or {}
        self.atom_func = atom_lookup(atoms)
        self.context = {}
        self.history = {}
        self.prev = MISSING
        self.visited = []             # 本轮走过的 S_id，按先后
        self.log = []                 # [(指令行, 调用参数, 来源说明)]
        # 流程编排（flow.py）用：本轮每次调用的结果、失败的调用、人给的值、人取消的步骤
        self.calls, self.failed, self.answers, self.cancelled = {}, set(), {}, set()
        self.resolver = Resolver(self.reg, self, atoms=atoms, computed_defaults=computed_defaults)

    # ---- 钩子：子类按需覆盖 ----
    def prepare(self, cmd, flags):
        """解析完指令行、解参数前；flags 是用户给的 --参数（可改）。"""

    def check(self, values, trace):
        """解完参数、执行前；拒绝就抛 ResolveError。"""

    def before_execute(self, cmd, values):
        """真正调函数前；返回值原样交给 after_execute。"""
        return None

    def after_execute(self, cmd, values, state):
        """真正调函数后（成功失败都走）。"""

    def finish(self, cmd, result):
        """`run` 记账前改写结果（如标来源）；默认原样。"""
        return result

    def config_value(self, key):
        """「配置」类绑定从哪读：默认 `env.store.get_config(key, None)`；env 没有 store 就当没配置。"""
        store = getattr(self.env, "store", None)
        return MISSING if store is None else store.get_config(key, None)

    # ---- 入口与条件求值的范围 ----
    def enter(self, entry, **ctx):
        """从某个入口进来，入口可带上下文；换入口即重新开始走。"""
        if entry not in self.reg.entries:
            raise ValueError(f"没有这个入口：{entry}；有：{', '.join(self.reg.entries)}")
        self.entry = entry
        self.visited = []
        self.context.update(ctx)

    def scope(self):
        seen = set(self.visited) | {self.reg.steps[s]["task"] for s in self.visited}
        return Scope(entry=self.entry, prev=self.prev, context=self.context, visited=seen,
                     failed=self.failed, cancelled=self.cancelled, answers=self.answers,
                     output=self._call_output, config=self.resolver.config)

    def _call_output(self, call_id, oid):
        if call_id not in self.calls or oid not in self.reg.outputs:
            return MISSING
        return extract(self.calls[call_id], self.reg.outputs[oid].get("path", ""))

    def give(self, answers):
        """人给了值（流程里的人步）：记下；X 表里设定来源是这个人工值的上下文也跟着设。"""
        self.answers.update(answers)
        for x in self.reg.data.get("context", []):
            if x.get("set_by") in answers:
                self.context[x["key"]] = answers[x["set_by"]]

    def forget(self, steps):
        """新一轮（逐个处理的下一个）：这些步骤上的调用结果、失败、人给的值、取消都清掉。"""
        steps = set(steps)
        for c in [c for c in self.calls if self.reg.calls[c]["step"] in steps]:
            self.calls.pop(c)
        self.failed -= {c["id"] for c in self.reg.data.get("calls", []) if c["step"] in steps}
        for h in [h for h in self.answers if self.reg.human[h]["step"] in steps]:
            self.answers.pop(h)
        self.cancelled -= steps

    # ---- 解析与执行 ----
    def plan(self, line):
        """只解析不执行：返回 (指令, 形参→值, 逐个调用的形参, 来源说明)。"""
        cmd, values, fanout, trace, _chosen = self._plan(line)
        return cmd, values, fanout, trace

    def _plan(self, line):
        p = parse(line, set(self.reg.commands))
        flags = dict(p.flags)
        positional_pid = self.reg.commands[p.cmd].get("positional")
        if p.positional is not None:
            if not positional_pid:
                raise ValueError(f"{p.cmd} 后面不接值：{p.positional}")
            flags[self.reg.params[positional_pid]["name"]] = p.positional
        self.prepare(p.cmd, flags)
        values, fanout, trace, chosen = self.resolver.resolve(p.cmd, flags)
        self.check(values, trace)
        return p.cmd, values, fanout, trace, chosen

    def run(self, line):
        cmd, values, fanout, trace, chosen = self._plan(line)
        result = self.finish(cmd, self._execute(cmd, values, fanout))
        self.log.append((line, values, trace))
        self.record(cmd, result, chosen)
        return result

    def run_call(self, call_id, fixed=None):
        """流程编排：跑流程表里的一次调用，只用这次调用的绑定；fixed={P_id: 值} 钉死某参数
        （逐个循环每次钉一个）。失败记进 failed 再抛 CommandFailed。"""
        call = self.reg.calls[call_id]
        cmd = call["cmd"]
        self.prepare(cmd, None)
        values, fanout, trace, _chosen = self.resolver.resolve(cmd, {}, call=call_id)
        for pid, v in (fixed or {}).items():
            values[pid], trace[pid] = v, f"流程逐个：{v}"
            fanout.discard(pid)
        self.check(values, trace)
        try:
            result = self._execute(cmd, values, fanout)
        except CommandFailed as e:
            self.failed.add(call_id)
            self.calls[call_id] = e.result
            self.log.append((f"{cmd}  〔{call_id}〕", values, trace))
            raise
        self.log.append((f"{cmd}  〔{call_id}〕", values, trace))
        self.failed.discard(call_id)
        self.calls[call_id] = result
        self.record(cmd, result, step=call["step"])
        return result

    def _execute(self, cmd, values, fanout):
        state = self.before_execute(cmd, values)
        try:
            if self.reg.is_atom(cmd):
                return self._run_atom(cmd, values)
            return self._run_impl(cmd, values, fanout)
        finally:
            self.after_execute(cmd, values, state)

    def _run_atom(self, cmd, values):
        fn = self.atom_func(cmd)
        if fn is None:
            raise NotWired(f"{cmd} 原子没实现（注册表里只登记）")
        kwargs = {kwarg(self.reg.params[pid]["name"]): v for pid, v in values.items()}
        try:
            result = fn(**kwargs)
        except ValueError as e:
            raise CommandFailed(cmd, {"message": str(e)}) from None
        if result is MISSING:
            raise CommandFailed(cmd, {"message": "输入不够，算不出"})
        return result

    def _run_impl(self, cmd, values, fanout):
        impl = self.impls.get(cmd)
        if impl is None:
            raise NotWired(f"{cmd} 没接线")
        kwargs = {impl.args[pid]: v for pid, v in values.items() if pid in impl.args}
        fan_args = [impl.args[pid] for pid in fanout if pid in impl.args]
        if fan_args:                                    # 几个"逐条"参数按位置对齐（同一清单各取一个字段）
            n = {len(kwargs[a]) for a in fan_args}
            if len(n) != 1:
                raise CommandFailed(cmd, {"message": "几个逐条参数的条数对不上"})
            return [self._call(cmd, impl, {**kwargs, **{a: kwargs[a][i] for a in fan_args}})
                    for i in range(n.pop())]
        return self._call(cmd, impl, kwargs)

    def _call(self, cmd, impl, kwargs):
        result = impl.fn(self.env, **kwargs)
        if failed(result):
            raise CommandFailed(cmd, result)
        return result

    # ---- 记结果：history / prev / 上下文 / visited ----
    def record(self, cmd, result, chosen=None, step=None):
        """跑完一条指令后的记账；没接线的指令，测试也可以拿现成结果直接记。
        step 给了（流程编排知道在哪一步）就记它，不再按绑定行去猜。"""
        self.history[cmd] = result
        self.prev = result
        self._set_context(cmd, result)
        step = step or self.match_step(cmd, chosen or {})
        if step:
            self.visit(step)

    def visit(self, step):
        """记走过一步；回到走过的步骤视为新一轮，之后的记录清掉。"""
        if step in self.visited:
            self.visited = self.visited[:self.visited.index(step)]
        self.visited.append(step)

    def match_step(self, cmd, chosen):
        calls = self.reg.calls_of(cmd)
        if len(calls) == 1:
            return calls[0]["step"]
        fits = []
        for c in calls:
            ok = True
            for pid, row in chosen.items():
                sigs = {signature(b) for b in self.reg.data.get("bindings", [])
                        if b.get("call") == c["id"] and b["target"] == pid and not b.get("parent")}
                if signature(row) not in sigs:
                    ok = False
                    break
            if ok:
                fits.append(c["step"])
        return fits[0] if len(fits) == 1 else None

    def _set_context(self, cmd, result):
        for x, out in self.reg.setters_of(cmd):
            value = extract(result, out.get("path", ""))
            path, _ = parse_pick(x.get("pick"))
            value = extract(value, path)
            if value is MISSING or value is None:
                raise CommandFailed(cmd, {"message": f"没拿到 {out['name']}，上下文 {x['key']} 未设"})
            self.context[x["key"]] = value
