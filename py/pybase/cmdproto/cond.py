"""B 表条件列的求值（定案3）。

只认一个很小的表达式子集（用 Python 的 ast 解析，不 eval）：
- 名字：`entry`（入口名）、`prev` / `prev.a.b`（上一步输出）、上下文键（如 `project.code`）
- 函数：`visited("S353")` / `visited("T34")`（本会话这一轮走过的步骤 / 子任务）
- 运算：`==` `!=` `in` `not in`、`and` `or` `not`、字符串/数字/布尔常量、常量列表
第三轮为 F 表（流转条件由解析层执行）加了几样，B 表也能用：
- `C3211.O1801`：本轮这次调用的某个输出；`H3511`：本轮人给的值；`K01`：配置值
- `failed("C2411")`：本轮这次调用失败了；`cancelled("S341")`：人在这一步取消
- `empty(x)`：x 取不到 / 空串 / 空列表 / 空对象
取不到的名字（上下文没设、prev 里没这个字段）让所在比较为假，不报错——
条件为假就是这行不用，交给下一行回退，与"缺参数报清楚"的口径分开。
"""

import ast
import re
from dataclasses import dataclass, field
from typing import Any, Callable

from .atoms import MISSING


class CondError(ValueError):
    pass


@dataclass
class Scope:
    entry: str = ""
    prev: Any = MISSING
    context: dict = field(default_factory=dict)
    visited: set = field(default_factory=set)
    failed: set = field(default_factory=set)          # 本轮失败的 C_id
    cancelled: set = field(default_factory=set)       # 本轮人取消的 S_id
    answers: dict = field(default_factory=dict)       # H_id -> 人给的值
    output: Callable = None                           # (C_id, O_id) -> 值 / MISSING
    config: Callable = None                           # K_id -> 值 / MISSING


CALL_OUT = re.compile(r"^C\d{4,}\.O\d{4,}$")
HUMAN = re.compile(r"^H\d{4,}$")
CONFIG = re.compile(r"^K\d{2}$")
FUNCS = ("visited", "failed", "cancelled", "empty")


def is_empty(v):
    return v is MISSING or v is None or v == "" or (isinstance(v, (list, dict, tuple)) and not v)


def _dotted(node):
    parts = []
    while isinstance(node, ast.Attribute):
        parts.append(node.attr)
        node = node.value
    if not isinstance(node, ast.Name):
        raise CondError("只能引用名字或 a.b 形式")
    parts.append(node.id)
    return ".".join(reversed(parts))


def _lookup(name, scope):
    if name == "entry":
        return scope.entry
    if CALL_OUT.match(name):
        return scope.output(*name.split(".")) if scope.output else MISSING
    if HUMAN.match(name):
        return scope.answers.get(name, MISSING)
    if CONFIG.match(name):
        return scope.config(name) if scope.config else MISSING
    if name == "prev" or name.startswith("prev."):
        cur = scope.prev
        for part in name.split(".")[1:]:
            if not isinstance(cur, dict) or part not in cur:
                return MISSING
            cur = cur[part]
        return cur
    return scope.context.get(name, MISSING)


def _value(node, scope):
    if isinstance(node, ast.Constant):
        return node.value
    if isinstance(node, (ast.List, ast.Tuple)):
        return [_value(e, scope) for e in node.elts]
    if isinstance(node, (ast.Name, ast.Attribute)):
        return _lookup(_dotted(node), scope)
    return _truth(node, scope)


_OPS = {ast.Eq: lambda a, b: a == b, ast.NotEq: lambda a, b: a != b,
        ast.In: lambda a, b: a in b, ast.NotIn: lambda a, b: a not in b}


def _truth(node, scope):
    if isinstance(node, ast.BoolOp):
        vals = (_truth(v, scope) for v in node.values)
        return all(vals) if isinstance(node.op, ast.And) else any(vals)
    if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.Not):
        return not _truth(node.operand, scope)
    if isinstance(node, ast.Call):
        fn = node.func.id if isinstance(node.func, ast.Name) else None
        if fn not in FUNCS or len(node.args) != 1 or node.keywords:
            raise CondError(f"条件里的函数只认 {'/'.join(FUNCS)}，各收一个参数")
        if fn == "empty":
            return is_empty(_value(node.args[0], scope))
        if not isinstance(node.args[0], ast.Constant):
            raise CondError(f"{fn}() 要给一个 id 字符串")
        arg = node.args[0].value
        return arg in {"visited": scope.visited, "failed": scope.failed,
                       "cancelled": scope.cancelled}[fn]
    if isinstance(node, ast.Compare):
        left = _value(node.left, scope)
        for op, right_node in zip(node.ops, node.comparators):
            right = _value(right_node, scope)
            if left is MISSING or right is MISSING or type(op) not in _OPS:
                return False
            try:
                if not _OPS[type(op)](left, right):
                    return False
            except TypeError:
                return False
            left = right
        return True
    if isinstance(node, ast.Constant):
        return bool(node.value)
    if isinstance(node, (ast.Name, ast.Attribute)):
        v = _lookup(_dotted(node), scope)
        return v is not MISSING and bool(v)
    raise CondError(f"条件里不认 {type(node).__name__}")


def evaluate(expr, scope: Scope) -> bool:
    """空条件恒真。"""
    expr = (expr or "").strip()
    if not expr:
        return True
    try:
        tree = ast.parse(expr, mode="eval")
    except SyntaxError as e:
        raise CondError(f"条件写错了：{expr}（{e.msg}）") from None
    return _truth(tree.body, scope)
