"""脱敏审核：一份会话两边——云端的 Claude（只读代码）和本地的 omlx（只读用户的资料），用户在中间看、改、转。

和业务无关的部分都在这里，零第三方依赖（标准库 sqlite3 / subprocess）、不带任何项目相关的默认值（目录、代理命令、
敏感串来源、Claude 能读哪些仓库、给 Claude 的附加提示词都由调用方给）：

- :class:`Outbox`（service.py）：会话 = 标题、omlx 的范围、一轮轮 {哪一边, 模型, 提示词, 回复, 状态, 错误, 来源, 过程}；
  两边各自 ``--resume`` 接着聊、可以同时跑（每边同时最多一轮）；发给 Claude 的过闸门；终端发起的可以转回给发起方。
- :class:`OmlxAgent`（omlx.py）：照调用契约起本地代理（``omlx-agent --investigate --stream``，契约见 README.md）。
- :class:`ClaudeAgent`（claude.py）：本机已登录的 claude CLI，只读（Read/Glob/Grep、限在给的目录、没有 MCP 和钩子）。
- :class:`Gate` 与来源 :func:`names_source` / :func:`lines_source` / :func:`static_source`（gate.py）：敏感串闸门。
- :data:`INVESTIGATE_PROMPT`（prompts.py）：给 omlx 的调查附加提示词缺省文字（``{范围}`` 发出时填）。
- 命令行：``python -m pybase.outbox.ask``（终端请 omlx 协查，只拿得到用户转回的话；ask.py）；配置文件（config.py）。
"""
from __future__ import annotations

from .agent import AgentResult, Run, parse_output, parse_result
from .checks import BadInput, Conflict, GateHits, NotFound, OutboxError, ScopeMissing
from .claude import ClaudeAgent, terminal_model
from .config import ConfigError, config_path, outbox_from_config, read_config, write_config
from .gate import Gate, GateUnavailable, hits_in, lines_source, names_source, static_source
from .omlx import OmlxAgent, investigate_argv
from .prompts import INVESTIGATE_PROMPT, fill_scope
from .service import Outbox
from .steps import files_read, steps_of
from .store import (CLAUDE, OMLX, ORIGIN_TERMINAL, ORIGIN_WINDOW, R_DONE, R_FAILED, R_RUNNING, REQUESTER,
                    ROUND_STATUSES, SIDES)

__all__ = [
    "AgentResult", "Run", "parse_output", "parse_result",
    "BadInput", "Conflict", "GateHits", "NotFound", "OutboxError", "ScopeMissing",
    "ClaudeAgent", "terminal_model",
    "ConfigError", "config_path", "outbox_from_config", "read_config", "write_config",
    "Gate", "GateUnavailable", "hits_in", "lines_source", "names_source", "static_source",
    "OmlxAgent", "investigate_argv",
    "INVESTIGATE_PROMPT", "fill_scope",
    "Outbox",
    "files_read", "steps_of",
    "CLAUDE", "OMLX", "ORIGIN_TERMINAL", "ORIGIN_WINDOW", "R_DONE", "R_FAILED", "R_RUNNING", "REQUESTER",
    "ROUND_STATUSES", "SIDES",
]
