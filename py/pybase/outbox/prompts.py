"""给本地代理的调查附加提示词（缺省文字）。

:data:`INVESTIGATE_PROMPT` 逐字照 xxyi 仓库 PLAN-outbox-v2.md 第 5 节（指挥写，原样用）；调用方可以换成自己的文字
（xxyi 设置窗口的提示词页以后能改），:class:`pybase.outbox.Outbox` 每发一轮现取一次。
``{范围}`` 换成这次调查可以看的目录和文件（用「、」连起来）；文字里别的花括号原样留着（不用 ``str.format``）。
"""
from __future__ import annotations

SCOPE_SLOT = "{范围}"
SCOPE_JOIN = "、"

INVESTIGATE_PROMPT = (
    "你在用户自己的电脑上帮他查资料、分析原因。你只能读文件和搜索，不能改文件、不能运行命令、不能上网。可以看的地方：{范围}。\n"
    "你的回复会先给用户看，用户可能接着问你，也可能把你的回复改一改再转给别人。回复里写清楚：你看了哪些文件、看到了什么、"
    "据此怎么判断；拿不准的说拿不准。"
)


def fill_scope(template: str, scope) -> str:
    """把模板里的 ``{范围}`` 换成 scope（路径列表）用「、」连成的一串。"""
    return (template or "").replace(SCOPE_SLOT, SCOPE_JOIN.join(str(p) for p in scope))
