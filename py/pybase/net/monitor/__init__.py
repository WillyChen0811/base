"""网络状态监测：采集（`collectors`；系统配置本身见 `sysconfig`/`sysfiles`）+ 存储与变化检测（`Recorder`）+ 定时循环（`Monitor`）。

零业务依赖，不内置任何主机/路径/URL 默认值。采集器互相独立、失败不抛异常
（结果里的 `error` 字段体现），调用方按需组装、传给 `Monitor`/`Recorder`。
"""
from __future__ import annotations

from . import collectors, sysconfig, sysfiles
from .diff import Change, diff_snapshots, summarize_changes
from .recorder import Recorder
from .runner import Monitor
from .scenario import Scenario, scenario_envelope, summarize_scenario

__all__ = [
    "collectors",
    "sysconfig",
    "sysfiles",
    "Recorder",
    "Monitor",
    "Scenario",
    "summarize_scenario",
    "scenario_envelope",
    "Change",
    "diff_snapshots",
    "summarize_changes",
]
