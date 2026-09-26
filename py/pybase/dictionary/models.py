"""词典查询的统一数据模型。

只放数据结构，不放具体数据源的查询逻辑（那是各适配器子模块的事，比如
`macos.py`）。
"""
from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Entry:
    """一条词典查询结果。

    `headword` 是从 `text` 里尽力而为解析出的词头，解析不出就是 None——
    调用方展示时优先用 `headword`，取不到再退回显示 `query`。

    `dictionary` 是命中的具体词典名；未指定词典查询（比如 macOS 公开 API
    只返回文本、不透露命中的是哪本）时是 None。

    `html` 只有部分适配器、部分查询路径能拿到（比如结构化的词源信息），
    拿不到就是 None，`text` 才是必定有值的纯文本正文。

    `source`/`rights` 必填：分别说明数据出处和授权范围，任何适配器都不能
    不带出处地把结果吐出来（base 不内置词典数据，版权都在数据源那边）。
    """

    query: str
    text: str
    source: str
    rights: str
    headword: str | None = None
    dictionary: str | None = None
    html: str | None = None
