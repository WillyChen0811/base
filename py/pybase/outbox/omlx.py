"""omlx 一边：照调用契约起本地代理（如 omlx-agent 的 ``--investigate`` 模式；契约详见同目录 README.md）::

    <命令…> --investigate --stream [--model 模型] [--scope 路径]... --system-file 文件 [--resume 会话号] -
    <命令…> --list-models          # 一行一个模型名
    <命令…> --default-model        # 一行：不给 --model 时用的模型

- ``--scope``：可以看的目录或文件（绝对路径，可多次）；``--system-file``：附加系统提示（已填好范围）的文件，用完由本库删；
  ``--resume``：接着这个会话问（上一轮回的会话号）；最后的 ``-``：提示词从 stdin 读（UTF-8）。
- ``--stream``：stdout 先逐行打 claude CLI 的 stream-json 事件（窗口看每一步），**最后一行**是结果 JSON
  （形状同 claude 的 result 事件，另带 ``omlx`` 对象：``denied``/``elapsed``/``model`` 记进这一轮的附带信息）。
- 退出码 0 = 答完；非 0 且没有结果那行 = 没跑起来（连不上模型等），原因在 stderr。
"""
from __future__ import annotations

import os
import secrets
from pathlib import Path
from typing import List, Mapping, Optional, Sequence

from . import files
from .agent import Event, Run, popen, run_quick

MODELS_TIMEOUT = 20.0


def investigate_argv(cmd: Sequence[str], scope: Sequence[str], system_file: str, resume: Optional[str] = None,
                     model: Optional[str] = None, stream: bool = True) -> List[str]:
    """照调用契约拼命令行（提示词走 stdin）。"""
    argv = list(cmd) + ["--investigate"]
    if stream:
        argv.append("--stream")
    if model:
        argv += ["--model", model]
    for p in scope:
        argv += ["--scope", str(p)]
    argv += ["--system-file", system_file]
    if resume:
        argv += ["--resume", resume]
    argv.append("-")
    return argv


class OmlxAgent:
    """``cmd`` 是代理的命令（如 ``["/path/omlx-agent"]``，可带固定参数）；``timeout`` 一轮最多跑多少秒；
    ``env`` 在本进程环境上加改的变量；``cwd`` 子进程的工作目录（缺省不改）；``run_dir`` 放临时附加提示词文件的目录
    （缺省由 :class:`~pybase.outbox.Outbox` 设成它数据目录下的 ``run/``）。"""

    side = "omlx"
    who = "本地代理"

    def __init__(self, cmd: Sequence[str], *, timeout: float = 3600.0, env: Optional[Mapping[str, str]] = None,
                 cwd: Optional[str] = None, run_dir: Optional[str] = None):
        if not cmd:
            raise ValueError("本地代理的命令不能为空")
        self.cmd = list(cmd)
        self.timeout = float(timeout)
        self.env = dict(env or {})
        self.cwd = cwd
        self.run_dir = run_dir

    def to_config(self) -> dict:
        return {"cmd": self.cmd, "timeout": self.timeout, "env": self.env, "cwd": self.cwd}

    @classmethod
    def from_config(cls, d: dict, run_dir: Optional[str] = None) -> "OmlxAgent":
        return cls(d["cmd"], timeout=d.get("timeout") or 3600.0, env=d.get("env") or {}, cwd=d.get("cwd"),
                   run_dir=run_dir)

    def _env(self) -> dict:
        env = dict(os.environ)
        env.update(self.env)
        return env

    def start(self, *, prompt: str, system: str, scope: Sequence[str], resume: Optional[str], model: Optional[str],
              on_event: Optional[Event] = None) -> Run:
        d = Path(self.run_dir or ".")
        files.ensure_private_dir(d)
        sysfile = files.write_private(d / ("omlx-%s.sys.txt" % secrets.token_hex(6)), system)
        try:
            proc = popen(investigate_argv(self.cmd, scope, str(sysfile), resume, model), env=self._env(), cwd=self.cwd)
        except BaseException:
            files.remove(sysfile)
            raise
        return Run(proc, prompt, self.timeout, on_event=on_event, who=self.who, cleanup=[str(sysfile)])

    def models(self) -> List[str]:
        """本机网关能用的模型（``--list-models``）；列不出 → RuntimeError（一句原因）。"""
        out = run_quick(self.cmd + ["--list-models"], env=self._env(), cwd=self.cwd, timeout=MODELS_TIMEOUT)
        return [x.strip() for x in out.splitlines() if x.strip()]

    def default_model(self) -> str:
        """不给 ``--model`` 时代理用的模型（``--default-model``）；问不出 → RuntimeError。"""
        out = run_quick(self.cmd + ["--default-model"], env=self._env(), cwd=self.cwd, timeout=MODELS_TIMEOUT)
        lines = [x.strip() for x in out.splitlines() if x.strip()]
        if not lines:
            raise RuntimeError("没回模型名")
        return lines[-1]
