"""脱敏审核：本地代理只读调查、用户审核后才外发。

和业务无关的部分都在这里，零第三方依赖（标准库 sqlite3 / subprocess）、不带任何项目相关的默认值（目录、代理命令、
敏感串来源都由调用方给）：

- :class:`Outbox`（service.py）：一次调查 = 标题、用途、范围（目录 / 文件）、一轮轮 {提示词, 回复, 状态, 错误, 时间}、
  放行稿；状态 草稿 / 在跑 / 等用户 / 已放行 / 已退回。发出一轮在后台跑本地代理，状态写进存储，供轮询；
  追问接着上一轮的会话（``--resume``）；放行 / 退回落盘（目录 0700、文件 0600）。
- :class:`CommandAgent`（agent.py）：照调用契约起本地代理（契约见 README.md）。
- :class:`Gate` 与来源 :func:`names_source` / :func:`lines_source` / :func:`static_source`（gate.py）：敏感串闸门。
- :data:`INVESTIGATE_PROMPT`（prompts.py）：给本地代理的调查附加提示词缺省文字（``{范围}`` 发出时填）。
"""
from __future__ import annotations

from .agent import AgentResult, CommandAgent, investigate_argv, parse_output
from .gate import Gate, GateUnavailable, hits_in, lines_source, names_source, static_source
from .prompts import INVESTIGATE_PROMPT, fill_scope
from .service import BadInput, Conflict, GateHits, NotFound, Outbox, OutboxError
from .store import (APPROVED, DRAFT, R_DONE, R_FAILED, R_PENDING, R_RUNNING, REJECTED, ROUND_STATUSES, RUNNING,
                    STATUSES, WAITING)

__all__ = [
    "AgentResult", "CommandAgent", "investigate_argv", "parse_output",
    "Gate", "GateUnavailable", "hits_in", "lines_source", "names_source", "static_source",
    "INVESTIGATE_PROMPT", "fill_scope",
    "BadInput", "Conflict", "GateHits", "NotFound", "Outbox", "OutboxError",
    "APPROVED", "DRAFT", "R_DONE", "R_FAILED", "R_PENDING", "R_RUNNING", "REJECTED", "ROUND_STATUSES", "RUNNING",
    "STATUSES", "WAITING",
]
