"""Claude 一边：本机已登录的 claude CLI（走云端），只读会话。

一轮的命令行（:meth:`ClaudeAgent.argv`）::

    <claude> -p --output-format stream-json --verbose
             --restricted --tools Read,Glob,Grep --strict-mcp-config --disable-slash-commands
             --settings '{"disableAllHooks": true}' --permission-mode dontAsk
             --disallowedTools Edit Write Bash NotebookEdit WebFetch WebSearch Agent Task …
             --add-dir 目录…  [--append-system-prompt 附加提示词] [--model 模型] [--resume 会话号]

提示词走 stdin。这几样各管什么（claude 2.1.319 ``--help``，11/08 在 Ubuntu 实测过 system/init 事件）：

- ``--tools``：内置工具只剩读、找文件、搜内容；``--restricted``：不读用户 / 项目 / 本地的设置文件（用户设置里的钩子、插件、
  权限规则都不进来），文件工具只能碰工作目录和 ``--add-dir`` 的目录；``--strict-mcp-config``（不给 ``--mcp-config``）：
  一个 MCP 服务都不带（含账号上连的 claude.ai 连接器）；``disableAllHooks``：内置插件的钩子也不跑；``dontAsk``：要问的一律拒。
- 环境：去掉 ``ANTHROPIC_*``、``CLAUDECODE``、``CLAUDE_CODE_*``（不让调用方环境把它指到别的网关 / 模型），
  ``CLAUDE_CODE_TMPDIR`` 指到本库自己的空目录——claude 自己的临时目录（缺省 ``/tmp/claude-<uid>/<工作目录>/``，
  同一工作目录下别的会话的临时文件都在那里）它总能读，挪开才只剩给的那几个目录。
- **护栏**：system/init 事件里的工具多出 :attr:`ClaudeAgent.tools` 以外的、或带了 MCP 服务，马上整组杀，这一轮记出错。

缺省模型「同终端」：用户设置（``$CLAUDE_CONFIG_DIR`` 或 ``~/.claude`` 下 ``settings.json``）的 ``model``——``--restricted``
不读设置文件，所以要显式传。
"""
from __future__ import annotations

import json
import os
from pathlib import Path
from typing import List, Mapping, Optional, Sequence

from . import files
from .agent import Event, Run, popen

TOOLS = ("Read", "Glob", "Grep")
DENY = ("Edit", "Write", "Bash", "NotebookEdit", "WebFetch", "WebSearch", "Agent", "Task", "EnterPlanMode")
# claude CLI --model 认的别名（--help：「an alias for the latest model (e.g. 'fable', 'opus', or 'sonnet')」）
MODELS = ("opus", "sonnet", "haiku", "fable")
SETTINGS = '{"disableAllHooks": true}'
EXTRA_TOOLS = "Claude 会话多了不该有的东西（%s），没让它跑"


def clean_env(base: Mapping[str, str]) -> dict:
    return {k: v for k, v in base.items()
            if not (k.startswith("ANTHROPIC_") or k == "CLAUDECODE" or k.startswith("CLAUDE_CODE_"))}


def config_dir(env: Mapping[str, str] = os.environ) -> Path:
    return Path(os.path.expanduser(env.get("CLAUDE_CONFIG_DIR") or "~/.claude"))


def terminal_model(env: Mapping[str, str] = os.environ) -> str:
    """终端里 claude 缺省用的模型（用户设置里的 ``model``）；没有 / 读不了回空串（那就不传 ``--model``，用 CLI 自己的缺省）。"""
    try:
        d = json.loads((config_dir(env) / "settings.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return ""
    m = d.get("model") if isinstance(d, dict) else None
    return m if isinstance(m, str) else ""


def memory_dir(cwd: str, env: Mapping[str, str] = os.environ) -> Path:
    """claude 给这个工作目录存记忆的目录（``<配置目录>/projects/<工作目录，非字母数字换成 ->/memory``）。"""
    name = "".join(ch if ch.isascii() and ch.isalnum() else "-" for ch in os.path.abspath(cwd))
    return config_dir(env) / "projects" / name / "memory"


class ClaudeAgent:
    """``cmd``：claude 可执行文件（可带固定参数）；``cwd``：工作目录（会话按它存，``--resume`` 要同一个）；
    ``add_dirs``：另外能读的目录；``tmp_dir``：给 claude 当临时目录的空目录（缺省由 :class:`~pybase.outbox.Outbox`
    设成它数据目录下的 ``claude-tmp/``）；``default_model``：不选模型时用的（缺省 :func:`terminal_model` 现取）；
    ``timeout`` 一轮最多几秒；``env`` 在清过的环境上再加改的变量。"""

    side = "claude"
    who = "Claude"

    def __init__(self, cmd: Sequence[str], *, cwd: str, add_dirs: Sequence[str] = (), timeout: float = 3600.0,
                 env: Optional[Mapping[str, str]] = None, tmp_dir: Optional[str] = None,
                 default_model: Optional[str] = None, models: Sequence[str] = MODELS, tools: Sequence[str] = TOOLS):
        if not cmd:
            raise ValueError("claude 的命令不能为空")
        self.cmd = list(cmd)
        self.cwd = cwd
        self.add_dirs = [str(d) for d in add_dirs]
        self.timeout = float(timeout)
        self.env = dict(env or {})
        self.tmp_dir = tmp_dir
        self._default_model = default_model
        self.model_names = list(models)
        self.tools = list(tools)

    def to_config(self) -> dict:
        return {"cmd": self.cmd, "cwd": self.cwd, "add_dirs": self.add_dirs, "timeout": self.timeout, "env": self.env,
                "default_model": self._default_model, "models": self.model_names, "tools": self.tools}

    @classmethod
    def from_config(cls, d: dict, tmp_dir: Optional[str] = None) -> "ClaudeAgent":
        return cls(d["cmd"], cwd=d["cwd"], add_dirs=d.get("add_dirs") or (), timeout=d.get("timeout") or 3600.0,
                   env=d.get("env") or {}, tmp_dir=tmp_dir, default_model=d.get("default_model"),
                   models=d.get("models") or MODELS, tools=d.get("tools") or TOOLS)

    def default_model(self) -> str:
        return self._default_model if self._default_model is not None else terminal_model()

    def models(self) -> List[str]:
        return list(self.model_names)

    def argv(self, system: str, resume: Optional[str], model: Optional[str]) -> List[str]:
        a = list(self.cmd) + ["-p", "--output-format", "stream-json", "--verbose",
                              "--restricted", "--tools", ",".join(self.tools), "--strict-mcp-config",
                              "--disable-slash-commands", "--settings", SETTINGS, "--permission-mode", "dontAsk",
                              "--disallowedTools", *DENY]
        for d in self.add_dirs:
            a += ["--add-dir", d]
        if system:
            a += ["--append-system-prompt", system]
        if model:
            a += ["--model", model]
        if resume:
            a += ["--resume", resume]
        return a

    def _env(self) -> dict:
        env = clean_env(os.environ)
        if self.tmp_dir:
            files.ensure_private_dir(Path(self.tmp_dir))
            env["CLAUDE_CODE_TMPDIR"] = self.tmp_dir
        env.update(self.env)
        return env

    def guard(self, event: dict) -> Optional[str]:
        """system/init 事件：工具只能是 :attr:`tools` 里的、不能带 MCP 服务；不对回一句话（这一轮就此作罢）。"""
        if event.get("type") != "system" or event.get("subtype") != "init":
            return None
        extra = [str(t) for t in (event.get("tools") or []) if t not in self.tools]
        mcp = [str((m or {}).get("name") if isinstance(m, dict) else m) for m in (event.get("mcp_servers") or [])]
        bad = (["工具 " + "、".join(extra)] if extra else []) + (["MCP " + "、".join(mcp)] if mcp else [])
        return EXTRA_TOOLS % "；".join(bad) if bad else None

    def start(self, *, prompt: str, system: str, scope: Sequence[str] = (), resume: Optional[str],
              model: Optional[str], on_event: Optional[Event] = None) -> Run:
        model = model or self.default_model() or None
        proc = popen(self.argv(system, resume, model), env=self._env(), cwd=self.cwd)
        return Run(proc, prompt, self.timeout, on_event=on_event, guard=self.guard, who=self.who)
