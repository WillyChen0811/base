"""参数解析：给一条已解析的指令，按绑定表把每个参数的值找出来。

顺序：用户在指令行给的 > 绑定行。绑定行在同一 (指令, 参数) 组里：
先过滤条件为真的（空条件恒真，是缺省），有条件的排前面，再按优先级回退（定案3）。
人工值（H）先看用户给没给，没给走它自己的缺省行（定案1）；原子输出行先解原子的每个
输入（所属B 名下的行），再调原子取那个 O（定案2）。缺参数时报清楚缺哪个、可从哪来，不猜。
"""

import json

from .atoms import MISSING, atom_lookup, kwarg
from .cond import evaluate

EMPTY = ("", "空", None)


class ResolveError(ValueError):
    pass


class MissingParam(ResolveError):
    def __init__(self, cmd, param, sources):
        self.param, self.sources = param, sources
        where = "；".join(sources) if sources else "没有任何绑定，只能用户给"
        super().__init__(f"{cmd} 缺参数 {param['name']}（{param['id']}）。可从：{where}")


def extract(value, path):
    """按 "a.b" 取值；列表逐个取；不是 dict 的（用户直接给的字符串）原样返回。"""
    if value is MISSING or not path:
        return value
    if isinstance(value, list):
        return [extract(v, path) for v in value]
    if not isinstance(value, dict):
        return value
    cur = value
    for part in path.split("."):
        if not isinstance(cur, dict) or part not in cur:
            return MISSING
        cur = cur[part]
    return cur


def parse_pick(pick):
    """取法列：".x" 取字段；"逐个 .x"/"逐条 .x" 取字段并按列表逐个调用；空=整个值。"""
    pick = (pick or "").strip()
    fan = pick.startswith(("逐个", "逐条"))
    if fan:
        pick = pick[2:].strip()
    return (pick[1:] if pick.startswith(".") else ""), fan


def _json(value, name):
    try:
        return json.loads(value)
    except json.JSONDecodeError as e:
        who = f"参数 {name}" if name else "参数"
        raise ResolveError(f"{who} 不是合法 JSON：{value}（{e.msg}）") from None


def coerce(value, ptype, name=None):
    if value is MISSING or value is None:
        return value
    if ptype == "整数":
        return int(value)
    if ptype == "布尔":
        if isinstance(value, bool):
            return value
        return str(value).lower() in ("1", "true", "yes", "是")
    if ptype == "列表" and isinstance(value, str):
        return _json(value, name) if value.startswith("[") else [v for v in value.split(",") if v]
    if ptype == "对象" and isinstance(value, str):
        return _json(value, name)
    return value


def constant(text, ptype):
    if text in EMPTY:
        return None
    return coerce(str(text), ptype) if ptype else text


def prio(row):
    """优先级列：空=1；0 是合法值，不能用 `or 1` 吞掉。"""
    p = row.get("prio")
    return 1 if p in EMPTY else int(p)


class Resolver:
    def __init__(self, registry, session, atoms=None, computed_defaults=None):
        """atoms：原子表（dict 或 cmd -> fn）；computed_defaults：{P_id: (另一个 P_id, 函数)}，
        缺省随同一条指令里另一个参数算出来（那个参数没值就算不出）。两者都由调用方给。"""
        self.reg, self.s = registry, session
        self.atom_func = atom_lookup(atoms)
        self.computed_defaults = computed_defaults or {}
        self.human_src = {}           # H_id -> 这次取值实际来自哪（"用户" / 缺省行）

    # ---- 候选行：过滤条件、排序 ----
    def candidates(self, rows):
        scope = self.s.scope()
        live = [(i, r) for i, r in enumerate(rows) if evaluate(r.get("cond"), scope)]
        live.sort(key=lambda t: (0 if t[1].get("cond") else 1, prio(t[1]), t[0]))
        return [r for _, r in live]

    def first_value(self, rows, flags, ptype=None):
        """按候选顺序取第一个取得到的值：返回 (值, 是否逐个, 行)；都取不到值为 MISSING。"""
        for row in self.candidates(rows):
            v, fan = self.row_value(row, flags, ptype)
            if v is MISSING or (v is None and row["kind"] != "常量"):
                continue
            return v, fan, row
        return MISSING, False, None

    # ---- 单个来源 ----
    def config(self, kid):
        v = self.s.config_value(self.reg.config[kid]["key"])
        return MISSING if v in ("", None, []) else v

    def context(self, xid):
        key = self.reg.context[xid]["key"]
        return self.s.context[key] if key in self.s.context else MISSING

    def output(self, oid, src_call=None):
        out = self.reg.outputs[oid]
        cmd = self.reg.cmd_of_call(src_call) if src_call else out["cmd"]
        # 第六轮：流程里跑过那次调用就取那次的结果（含失败的：如导出撞同名，失败返回里带冲突名，
        # 下一步重调要用）；history 只记成功的、且按指令不按调用
        if src_call and src_call in getattr(self.s, "calls", {}):
            return extract(self.s.calls[src_call], out.get("path", ""))
        if cmd not in self.s.history:
            return MISSING
        return extract(self.s.history[cmd], out.get("path", ""))

    def human(self, hid, flags):
        h = self.reg.human[hid]
        if h.get("flag") and h["flag"] in flags:
            self.human_src[hid] = "用户"
            return flags[h["flag"]]
        if hid in self.s.answers:                    # 流程编排里人在那一步给的（flow.py）
            self.human_src[hid] = "用户"
            return self.s.answers[hid]
        v, _fan, row = self.first_value(self.reg.bindings(hid), flags)
        if row is not None:
            self.human_src[hid] = f"未给，取缺省 {row['id']} {self.describe(row)}"
        return v

    def atom(self, row, flags):
        """原子输出行：解原子输入 → 调原子 → 取那个 O。"""
        out = self.reg.outputs[row["ref"]]
        kwargs = {}
        for pid, rows in self.reg.children(row).items():
            p = self.reg.params[pid]
            v, _fan, _row = self.first_value(rows, flags, p["type"])
            if v is not MISSING:
                kwargs[kwarg(p["name"])] = coerce(v, p["type"], p["name"])
            elif p.get("required") == "是":
                return MISSING                           # 原子的必填输入取不到：这行不成，往下回退
        fn = self.atom_func(out["cmd"])
        if fn is None:
            raise ResolveError(f"{row['id']} 引用的原子 {out['cmd']} 没有实现")
        result = fn(**kwargs)
        return extract(result, out.get("path", ""))

    # ---- 一条绑定行 ----
    def row_value(self, row, flags, ptype=None):
        """返回 (值, 是否逐个调用)。"""
        path, fan = parse_pick(row.get("pick"))
        kind, ref = row["kind"], str(row["ref"])
        if kind == "常量":
            return constant(ref, ptype), fan
        if kind == "原子输出":
            v = self.atom(row, flags)
        elif kind == "人工":
            v = self.human(ref, flags)
        elif kind == "上步输出":
            v = self.output(ref, row.get("src_call") or None)
        elif kind == "上下文":
            v = self.context(ref)
        elif kind == "配置":
            v = self.config(ref)
        else:
            raise ResolveError(f"{row['id']} 来源类型认不出：{kind}")
        return extract(v, path), fan

    def describe(self, row):
        kind, ref = row["kind"], str(row["ref"])
        cond = f"〔{row['cond']}〕" if row.get("cond") else ""
        if kind == "人工":
            h = self.reg.human[ref]
            return f"{cond}用户给 {h.get('flag') or '?'}（{h['name']}，{ref}）"
        if kind == "上步输出":
            cmd = self.reg.cmd_of_call(row["src_call"]) if row.get("src_call") else "?"
            return f"{cond}先跑 {cmd}（{ref}）"
        if kind == "上下文":
            x = self.reg.context[ref]
            return f"{cond}上下文 {x['key']}（{ref}，由 {x['set_by']} 设）"
        if kind == "配置":
            k = self.reg.config[ref]
            return f"{cond}配置 {k['key']}（{ref}，{k['where']}）"
        if kind == "原子输出":
            out = self.reg.outputs[ref]
            ins = []
            for pid, rows in self.reg.children(row).items():
                srcs = "/".join(self._short(r) for r in rows) or "无绑定"
                ins.append(f"{self.reg.params[pid]['name']}←{srcs}")
            return f"{cond}原子 {out['cmd']}({', '.join(ins)}) 的 {ref}"
        return f"{cond}常量 {ref}"

    def _short(self, row):
        kind, ref = row["kind"], str(row["ref"])
        if kind == "人工":
            h = self.reg.human[ref]
            return f"{h.get('flag') or ref} {h['name']}"
        if kind == "上步输出":
            return f"{ref}←{self.reg.cmd_of_call(row['src_call'])}"
        if kind == "上下文":
            return f"上下文 {self.reg.context[ref]['key']}"
        return f"{kind} {ref}"

    # ---- 整条指令 ----
    def resolve(self, cmd, flags, call=None):
        """返回 (kwargs: P_id->值, fanout: P_id 集合, trace: P_id->来源说明, chosen: P_id->行)。
        call 给了（流程编排知道走到哪次调用）只用那次调用的绑定行；没给就合并该指令全部调用。"""
        allowed = self.reg.allowed_flags(cmd)
        unknown = sorted(set(flags) - allowed)
        if unknown:
            raise ResolveError(f"{cmd} 不认识参数 {', '.join(unknown)}；可用：{', '.join(sorted(allowed))}")
        values, fanout, trace, chosen = {}, set(), {}, {}
        self.human_src = {}
        for p in self.reg.params_of(cmd):
            if p["name"] in flags:
                values[p["id"]] = coerce(flags[p["name"]], p["type"], p["name"])
                trace[p["id"]] = f"用户 {p['name']}"
                continue
            rows = self.reg.bindings(p["id"], cmd=cmd, call=call)
            v, fan, row = self.first_value(rows, flags, p["type"])
            if row is not None:
                values[p["id"]] = v if fan else coerce(v, p["type"], p["name"])
                trace[p["id"]] = f"{row['id']} {self.describe(row)}"
                if row["kind"] == "人工" and self.human_src.get(row["ref"], "用户") != "用户":
                    trace[p["id"]] += f"；{self.human_src[row['ref']]}"
                chosen[p["id"]] = row
                if fan:
                    fanout.add(p["id"])
                continue
            default = p.get("default")
            computed = self._computed_default(p, values)
            if computed is not MISSING:
                values[p["id"]] = computed
                trace[p["id"]] = f"P 缺省按 {self.computed_defaults[p['id']][0]} 算：{computed}"
            elif p["name"].startswith("--") and default not in EMPTY and default in self._literal_defaults(p):
                values[p["id"]] = constant(default, p["type"])
                trace[p["id"]] = f"P 缺省 {default}"
            elif p.get("required") == "是":
                live = self.candidates(rows)
                dead = [r for r in rows if r not in live]
                srcs = [f"用户直接给 {p['name']}"] if p["name"].startswith("--") else []
                srcs += [self.describe(r) for r in live]
                srcs += [f"（条件不成立）{self.describe(r)}" for r in dead]
                raise MissingParam(cmd, p, srcs)
        return values, fanout, trace, chosen

    def _computed_default(self, p, values):
        """缺省要看同一条指令里另一个参数的值（computed_defaults）；那个参数没值就算不出来。"""
        spec = self.computed_defaults.get(p["id"])
        if not spec or values.get(spec[0]) in EMPTY:
            return MISSING
        return spec[1](values[spec[0]])

    @staticmethod
    def _literal_defaults(p):
        """P 缺省列里能直接当值的（数字、true/false、短流水）；"工作目录""同类型最大…"这类是说明。"""
        d = str(p.get("default"))
        return [d] if d.isdigit() or d in ("true", "false") else []
