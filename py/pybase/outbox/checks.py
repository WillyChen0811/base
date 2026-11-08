"""会话接口的入参检查与出错类型（:mod:`.service` 用；调用方按 ``code`` 翻成接口回包）。"""
from __future__ import annotations

import os
import re
from typing import List

from .store import SIDES

ID_RE = re.compile(r"^[A-Za-z0-9_-]{1,64}$")
MODEL_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,99}$")
TITLE_MAX = 200
TITLE_AUTO = 30
PROMPT_MAX = 200000
REQUESTER_MAX = 50
QUERY_MAX = 200
SCOPE_MAX = 50
PATH_MAX = 4096


class OutboxError(ValueError):
    code = "error"


class BadInput(OutboxError):
    """请求本身不对（类型、长度、格式）。"""
    code = "bad_input"


class NotFound(OutboxError):
    code = "not_found"


class Conflict(OutboxError):
    """状态不允许（这一边正在跑、没在跑、范围空或不在、没配这一边……）。"""
    code = "conflict"


class ScopeMissing(Conflict):
    """范围里有路径不在：``missing`` 是那些路径（窗口显示；终端入口只报个数，路径可能带资料名）。"""

    def __init__(self, missing: List[str]):
        super().__init__("范围里这些不在：%s" % "、".join(missing))
        self.missing = list(missing)


class GateHits(OutboxError):
    """闸门有命中、没说仍然发送：``hits`` 是命中串（只给用户看）。"""
    code = "gate_hits"

    def __init__(self, hits: List[str], what: str):
        super().__init__("%s里有 %d 处敏感串" % (what, len(hits)))
        self.hits = list(hits)


def need_id(v) -> str:
    if not isinstance(v, str) or not ID_RE.fullmatch(v):
        raise BadInput("id 要是会话号（字母、数字、- 和 _，最长 64）")
    return v


def need_side(v) -> str:
    if v not in SIDES:
        raise BadInput("side 要是 claude 或 omlx")
    return v


def need_bool(v, name: str = "force") -> bool:
    if v is None:
        return False
    if not isinstance(v, bool):
        raise BadInput("%s 要是 true/false" % name)
    return v


def need_text(v, name: str, limit: int, required: bool = False) -> str:
    if v is None:
        if required:
            raise BadInput("缺少 %s" % name)
        return ""
    if not isinstance(v, str):
        raise BadInput("%s 要是字符串" % name)
    if len(v) > limit:
        raise BadInput("%s 超过 %d 字（现在 %d 字）" % (name, limit, len(v)))
    return v


def need_prompt(v, name: str = "prompt") -> str:
    t = need_text(v, name, PROMPT_MAX, required=True)
    if not t.strip():
        raise BadInput("要发的话不能是空的")
    return t


def need_model(v) -> str:
    if v in (None, ""):
        return ""
    if not isinstance(v, str) or not MODEL_RE.fullmatch(v):
        raise BadInput("model 要是模型名（字母、数字和 ._:/@[]-，最长 100）")
    return v


def norm_scope(v) -> List[str]:
    """范围：路径列表（字符串，``~`` 展开，要是绝对路径；去重、保留先后）。存在与否发给 omlx 时才查。"""
    if v is None:
        return []
    if not isinstance(v, (list, tuple)) or any(not isinstance(p, str) for p in v):
        raise BadInput("scope 要是路径（字符串）列表")
    out = []
    for p in v:
        p = p.strip()
        if not p:
            continue
        if len(p) > PATH_MAX:
            raise BadInput("scope 里有路径太长")
        q = os.path.normpath(os.path.expanduser(p))
        if not os.path.isabs(q):
            raise BadInput("scope 里的路径要是绝对路径：%s" % p)
        if q not in out:
            out.append(q)
    if len(out) > SCOPE_MAX:
        raise BadInput("scope 最多 %d 项" % SCOPE_MAX)
    return out


def auto_title(prompt: str) -> str:
    return " ".join((prompt or "").split())[:TITLE_AUTO]
