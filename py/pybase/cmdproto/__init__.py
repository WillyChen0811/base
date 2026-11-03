"""cmdproto —— 零业务的指令解析层：`/动作 -组 --参数 值` 一行指令 → 参数从哪来 → 调函数 → 按流程表走。

一套注册表（JSON，由调用方生成）描述整个系统：
- commands / params / outputs：指令、参数、输出；
- bindings：每个参数的值从哪来（用户给、常量、配置、上下文、上一步输出、原子输出、人工值），
  可带条件与优先级，按顺序回退；
- human / context / config：人工值、上下文键、配置键；
- steps / calls / flows / entries：流程的步骤、每步的调用、步骤间的流转边与入口。

本库只提供机制，不带任何指令、原子、实现和默认值：
- 注册表数据由调用方读入（`Registry(data)` / `Registry.load(path)`）；
- 指令的实现表 `impls`（`cmd -> Impl(fn, args)`）、原子表 `atoms`（`cmd -> fn`）、
  随另一个参数算出的缺省 `computed_defaults` 都由调用方交给 `Session`；
- 执行前后的钩子（`Session.prepare` / `check` / `before_execute` / `after_execute` / `finish`）
  留给子类，项目自己的策略（来源筛选、路径白名单、同步取最新…）写在子类里。

模块：
- parser：一行指令拆成 (指令名, --参数, 定位值)，不管参数对不对
- registry：注册表索引与查询（绑定行、可达的人工值、上下文设定来源…）
- cond：绑定行/流转边条件列的求值（ast 白名单，不 eval）
- resolver：按绑定表把每个参数的值找出来，缺了报清楚可从哪来
- runtime：会话（history / prev / 上下文 / visited），执行指令与原子
- flow：按流程表一步步走：程序步、人步（可暂停/续走）、条件分流、逐个循环
"""

from .atoms import MISSING, kwarg
from .cond import CondError, Scope, evaluate
from .flow import CANCEL, END, Flow, FlowError, Outcome
from .parser import ParseError, Parsed, parse, tokenize
from .registry import ATOM_LAYER, Registry, signature
from .resolver import EMPTY, MissingParam, ResolveError, Resolver, coerce, extract, parse_pick
from .runtime import CommandFailed, Impl, NotWired, Session, failed

__all__ = [
    "MISSING", "kwarg",
    "CondError", "Scope", "evaluate",
    "CANCEL", "END", "Flow", "FlowError", "Outcome",
    "ParseError", "Parsed", "parse", "tokenize",
    "ATOM_LAYER", "Registry", "signature",
    "EMPTY", "MissingParam", "ResolveError", "Resolver", "coerce", "extract", "parse_pick",
    "CommandFailed", "Impl", "NotWired", "Session", "failed",
]
