"""注册表：调用方生成的 JSON（表名 → 行列表），这里只做索引与查询。

表与字段（都是字符串，空值写 ""）：
- commands: cmd, layer（原子是 ATOM_LAYER）, positional（指令名后直接跟的值对应哪个 P_id，可无）
- params:   id, cmd, name（"--x" 是显式参数，其余是隐式）, type（整数/布尔/列表/对象/其他）, required（"是"）, default
- outputs:  id, cmd, name, path（从结果里取值的 "a.b" 路径，空=整个结果）
- bindings: id, target（P_id 或 H_id）, kind（常量/配置/上下文/上步输出/原子输出/人工）, ref, cond, prio,
            pick（".x" 取字段；"逐个 .x" 取字段并按列表逐个调用）, call（属于哪次调用，空=人工值的缺省行）,
            src_call（上步输出来自哪次调用）, parent（原子输出行名下的输入行指回所属行）
- human:    id, name, flag（用户可直接给的 "--x"，可无）, step
- context:  id, key, set_by（设它的 O_id 列表，逗号分隔；或 H_id）, pick
- config:   id, key, where
- calls:    id, cmd, step, seq
- steps:    id, task, actor（"人" 是人步）
- flows:    src, dst, cond
- entries:  id（入口名）
"""

import json
from functools import cached_property
from pathlib import Path

ATOM_LAYER = "L0原子"


def signature(b):
    """绑定行的"来源签名"：多个调用用同一指令时，签名相同的行只算一条。
    原子输出行带着自己名下的输入行，不合并。"""
    if b["kind"] == "原子输出":
        return ("原子输出", b["id"])
    return (b.get("cond", ""), b["kind"], b.get("src_call", ""), str(b["ref"]), b.get("pick", ""))


class Registry:
    def __init__(self, data):
        self.data = data

    @classmethod
    def load(cls, path):
        return cls(json.loads(Path(path).read_text("utf-8")))

    def _index(self, table, key="id"):
        return {r[key]: r for r in self.data.get(table, [])}

    @cached_property
    def commands(self):
        return self._index("commands", "cmd")

    @cached_property
    def params(self):
        return self._index("params")

    @cached_property
    def outputs(self):
        return self._index("outputs")

    @cached_property
    def human(self):
        return self._index("human")

    @cached_property
    def context(self):
        return self._index("context")

    @cached_property
    def config(self):
        return self._index("config")

    @cached_property
    def calls(self):
        return self._index("calls")

    @cached_property
    def steps(self):
        return self._index("steps")

    @cached_property
    def entries(self):
        return self._index("entries")

    def flows_from(self, step):
        """F 表从这一步出去的边，有条件的在前（同 B 表：空条件是缺省），同类按表里先后。"""
        rows = [f for f in self.data.get("flows", []) if f["src"] == step]
        return sorted(rows, key=lambda f: 0 if f.get("cond") else 1)

    def calls_in(self, step):
        return sorted((c for c in self.data.get("calls", []) if c["step"] == step), key=lambda c: c["seq"])

    def humans_in(self, step):
        return [h for h in self.data.get("human", []) if h["step"] == step]

    def is_atom(self, cmd):
        return self.commands[cmd]["layer"] == ATOM_LAYER

    def params_of(self, cmd):
        return [p for p in self.data.get("params", []) if p["cmd"] == cmd]

    def cmd_of_call(self, call_id):
        return self.calls[call_id]["cmd"]

    def calls_of(self, cmd):
        return [c for c in self.data.get("calls", []) if c["cmd"] == cmd]

    def bindings(self, target, cmd=None, parent=None, call=None):
        """目标的绑定行（未过滤条件）。

        - parent 给了：某原子输出行名下、给原子参数 target 的行；
        - call 给了：只要这一次调用里给参数 target 的行（流程编排按 F 表走到哪次调用就用哪套）；
        - cmd 给了：该指令所有调用里给参数 target 的行（不含原子输入行）；
        - 都没给：人工值 target 的缺省行（C_id 空）。
        签名相同的行合并成一条（先出现的那条）。
        """
        rows, seen = [], set()
        for b in self.data.get("bindings", []):
            if b["target"] != target:
                continue
            if parent is not None:
                if b.get("parent") != parent:
                    continue
            elif b.get("parent"):
                continue
            elif call is not None:
                if b.get("call") != call:
                    continue
            elif cmd is not None:
                if not b.get("call") or self.cmd_of_call(b["call"]) != cmd:
                    continue
            elif b.get("call"):
                continue
            sig = signature(b)
            if sig in seen:
                continue
            seen.add(sig)
            rows.append(b)
        return rows

    def children(self, row):
        """原子输出行：原子参数 P_id → 名下的绑定行。"""
        atom = self.outputs[row["ref"]]["cmd"]
        return {p["id"]: self.bindings(p["id"], parent=row["id"]) for p in self.params_of(atom)}

    def humans_reachable(self, cmd):
        """这条指令的绑定会用到哪些人工值（含原子输入、人工值缺省里用到的）。"""
        found, todo = [], []
        for p in self.params_of(cmd):
            todo += self.bindings(p["id"], cmd=cmd)
        while todo:
            b = todo.pop()
            if b["kind"] == "原子输出":
                for rows in self.children(b).values():
                    todo += rows
            ref = str(b["ref"])
            if b["kind"] == "人工" and ref not in found:
                found.append(ref)
                todo += self.bindings(ref)
        return [self.human[h] for h in found]

    def allowed_flags(self, cmd):
        flags = {p["name"] for p in self.params_of(cmd) if p["name"].startswith("--")}
        flags |= {h["flag"] for h in self.humans_reachable(cmd) if h.get("flag")}
        return flags

    def setters_of(self, cmd):
        """(上下文行, 设它的 O 行)：X 表设定来源里属于这条指令的输出。"""
        out = []
        for x in self.data.get("context", []):
            for oid in str(x.get("set_by", "")).split(","):
                oid = oid.strip()
                if oid in self.outputs and self.outputs[oid]["cmd"] == cmd:
                    out.append((x, self.outputs[oid]))
        return out
