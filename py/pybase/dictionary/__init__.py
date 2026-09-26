"""统一词典查询：条目模型 + 各数据源适配器。

版权原因，base 不内置任何词典数据；每个数据源一个适配器子模块，查询结果
（`Entry`）必须带 `source`/`rights`，说清出处和授权范围。第一个适配器是
`macos`（macOS 系统词典，随系统授权给本机登录用户使用，正版离线）。
"""
from __future__ import annotations

from . import macos
from .models import Entry

__all__ = ["Entry", "macos"]
