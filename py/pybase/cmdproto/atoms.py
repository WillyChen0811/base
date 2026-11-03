"""原子（L0）的机制部分：哨兵值、形参名规则、原子表的查找约定。

原子是"输入组合固定、输出组合固定、中间没有分支和人工决定"的纯函数，在注册表里登记成
`layer == ATOM_LAYER` 的指令，有自己的 P/O；绑定行用「原子输出」引用。既能被绑定行调用
（输入由所属B 名下的行给），也能直接当指令跑。

本库不内置任何原子：调用方把 ``{指令名: 函数}`` 表（或 ``cmd -> fn | None`` 的查找函数）
交给 `Session`/`Resolver`。原子算不出来时返回 `MISSING`（或返回 dict 里缺那个键），
解析层据此往下回退或报缺参数，不猜。
"""

from typing import Callable, Mapping, Optional

#: "取不到值"的哨兵：与 None 区分（None 是合法的空值，MISSING 是没有这个值）。
MISSING = object()


def kwarg(param_name: str) -> str:
    """P 表参数名 → 形参名：去掉前导 `-`，`-` 换 `_`（`--folder-name` → `folder_name`）。"""
    return param_name.lstrip("-").replace("-", "_")


def atom_lookup(atoms) -> Callable[[str], Optional[Callable]]:
    """把调用方给的原子表统一成 ``cmd -> fn | None``：dict 直接查；可调用对象原样用；None 全查不到。"""
    if atoms is None:
        return lambda cmd: None
    if isinstance(atoms, Mapping):
        return atoms.get
    if callable(atoms):
        return atoms
    raise TypeError(f"atoms 要是 dict 或 cmd -> fn 的函数，不是 {type(atoms).__name__}")
