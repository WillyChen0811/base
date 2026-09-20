"""按 cfg 拼 Playwright 启动/建 context 参数的纯 dict→dict 映射。

只回答"给定配置片段，Playwright 的 launch()/new_context() 该传什么参数"，
不涉及 Playwright 对象本身、不管连哪种模式（launch / launch_persistent_context /
connect_over_cdp）、不解析相对路径——那些是调用方的编排逻辑。

本模块不带任何项目默认值：viewport 缺省值、user_data_dir 等均由调用方决定。
"""
from __future__ import annotations

from typing import Any


def playwright_launch_args(browser_cfg: dict) -> dict[str, Any]:
    """browser 配置片段（headless / channel / args）→ Playwright `launch()` 的关键字参数。"""
    launch_args: dict[str, Any] = {"headless": bool(browser_cfg.get("headless", False))}
    if browser_cfg.get("channel"):
        launch_args["channel"] = browser_cfg["channel"]
    if browser_cfg.get("args"):
        launch_args["args"] = list(browser_cfg["args"])
    return launch_args


def playwright_context_args(viewport: dict) -> dict[str, Any]:
    """viewport 配置（{"width":.., "height":..}）→ Playwright `new_context()` 的关键字参数。"""
    return {"viewport": {"width": int(viewport["width"]), "height": int(viewport["height"])}}
