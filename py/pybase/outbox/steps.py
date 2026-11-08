"""claude CLI ``--output-format stream-json`` 的事件 → 窗口里显示的「每一步」。

一个事件里可能有几步（一条 assistant 消息里先说一句话、再调两次工具）。每一步是一个小字典：

- ``{"kind": "text", "text": 中间说的话}``
- ``{"kind": "tool", "id": 工具调用号, "tool": "Read"/"Grep"/…, "input": {参数名: 值}}``（读了哪个文件、搜了什么；值是字符串的截到
  :data:`INPUT_MAX` 字，最多 :data:`INPUT_KEYS` 个参数）
- ``{"kind": "tool_result", "id": 工具调用号, "error": 是不是出错 / 被拒, "text": 出错时的那句话（截到 :data:`ERROR_MAX` 字）}``
  ——成功的工具结果（文件内容、搜到的行）不存，只留「成了」。

思考（thinking）、系统事件、最后的 result 事件都不算步。
"""
from __future__ import annotations

from typing import List

TEXT_MAX = 4000
INPUT_MAX = 500
INPUT_KEYS = 10
ERROR_MAX = 300


def _cut(s, n: int) -> str:
    s = s if isinstance(s, str) else str(s)
    return s if len(s) <= n else s[:n] + "…"


def _input(inp) -> dict:
    if not isinstance(inp, dict):
        return {}
    out = {}
    for k, v in list(inp.items())[:INPUT_KEYS]:
        out[str(k)] = v if isinstance(v, (bool, int, float)) or v is None else _cut(v, INPUT_MAX)
    return out


def _result_text(content) -> str:
    if isinstance(content, list):
        content = " ".join(c.get("text", "") for c in content if isinstance(c, dict))
    return _cut(content or "", ERROR_MAX)


def steps_of(event) -> List[dict]:
    if not isinstance(event, dict) or event.get("parent_tool_use_id"):
        return []
    msg = event.get("message")
    content = msg.get("content") if isinstance(msg, dict) else None
    if not isinstance(content, list):
        return []
    out = []
    if event.get("type") == "assistant":
        for c in content:
            if not isinstance(c, dict):
                continue
            if c.get("type") == "text" and (c.get("text") or "").strip():
                out.append({"kind": "text", "text": _cut(c["text"].strip(), TEXT_MAX)})
            elif c.get("type") == "tool_use":
                out.append({"kind": "tool", "id": str(c.get("id") or ""), "tool": str(c.get("name") or ""),
                            "input": _input(c.get("input"))})
    elif event.get("type") == "user":
        for c in content:
            if isinstance(c, dict) and c.get("type") == "tool_result":
                err = bool(c.get("is_error"))
                out.append({"kind": "tool_result", "id": str(c.get("tool_use_id") or ""), "error": err,
                            "text": _result_text(c.get("content")) if err else ""})
    return out


def files_read(steps) -> List[str]:
    """这些步里 Read 过的文件（去重，按第一次读的先后；出错 / 被拒的那次不算）。"""
    failed = {s.get("id") for s in steps if s.get("kind") == "tool_result" and s.get("error")}
    seen = []
    for s in steps:
        if s.get("kind") == "tool" and s.get("tool") == "Read" and s.get("id") not in failed:
            p = (s.get("input") or {}).get("file_path")
            if isinstance(p, str) and p and p not in seen:
                seen.append(p)
    return seen
