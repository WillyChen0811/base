"""合并各块的输出：按编号去重；跨块首尾相接、主体相同的段并成一段。不绑具体业务字段。

    merge(outputs, key=...)                                   # 只去重
    merge(outputs, span=("start", "end"), same="req")         # 只接段（条目是 dict）
    merge(outputs, key=..., span=get_span, same=fn, combine=fn2)   # 条目是别的对象

- ``outputs``：各块的输出，按块顺序，每块一个列表；某块是 None（没跑通）当空。
- 去重（给了 ``key``）：``key(条目)`` 相同的只留先出现的那条，顺序不变。
- 接段（给了 ``span``）：只在**相邻两块之间**接——后一块的某段和前一块（含前一块刚接上的）某段主体相同
  （``same``）、且首尾相接（``adjacent(前段的止, 后段的起)``，缺省 ``后段的起 == 前段的止 + 1``，编号闭区间），
  就并成一段（``combine(前段, 后段)``；条目是 dict、``span`` 给的是两个字段名时缺省 = 前段复制一份、止换成后段的止）。
  能一直往后接（一段话跨三块也并成一段）。同一块里的段不动。
"""
from __future__ import annotations

from typing import Any, Callable, Hashable, Iterable, Mapping, Sequence


def _getter(spec: Any, what: str) -> Callable[[Any], Any]:
    if callable(spec):
        return spec
    if isinstance(spec, str):
        return lambda x: x[spec]
    if isinstance(spec, (tuple, list)) and all(isinstance(k, str) for k in spec):
        keys = tuple(spec)
        return lambda x: tuple(x[k] for k in keys)
    raise TypeError(f"{what} 要是函数、字段名或字段名元组：{spec!r}")


def _touching(a_end: Any, b_start: Any) -> bool:
    return b_start == a_end + 1


def merge(outputs: Iterable[Sequence[Any] | None], *, key: Callable[[Any], Hashable] | str | None = None,
          span: Callable[[Any], tuple[Any, Any]] | tuple[str, str] | None = None,
          same: Callable[[Any, Any], bool] | str | Sequence[str] | None = None,
          combine: Callable[[Any, Any], Any] | None = None,
          adjacent: Callable[[Any, Any], bool] | None = None) -> list:
    """各块的输出 → 合并后的一个列表（规则见模块说明）。``key``/``same`` 也可以给字段名（条目是 dict 时）。"""
    blocks = [list(b or []) for b in outputs]
    if key is not None:
        kf = _getter(key, "key")
        seen: set = set()
        deduped = []
        for b in blocks:
            keep = []
            for x in b:
                k = kf(x)
                if k in seen:
                    continue
                seen.add(k)
                keep.append(x)
            deduped.append(keep)
        blocks = deduped
    if span is None:
        return [x for b in blocks for x in b]

    if isinstance(span, (tuple, list)) and len(span) == 2 and all(isinstance(k, str) for k in span):
        s_key, e_key = span
        get_span = lambda x: (x[s_key], x[e_key])                       # noqa: E731
        if combine is None:
            def combine(a, b, _e=e_key):
                if not isinstance(a, Mapping):
                    raise TypeError("条目不是 dict，要给 combine")
                return {**a, _e: b[_e]}
    elif callable(span):
        get_span = span
        if combine is None:
            raise TypeError("span 给的是函数时要给 combine（怎么把两段并成一段）")
    else:
        raise TypeError(f"span 要是 (起字段名, 止字段名) 或函数：{span!r}")
    if same is None:
        raise TypeError("接段要给 same（怎么算主体相同）：函数或字段名")
    if callable(same):
        same_f = same
    else:
        g = _getter(same, "same")
        same_f = lambda a, b: g(a) == g(b)                               # noqa: E731
    adj = adjacent or _touching

    out: list = []
    prev: list[int] = []                    # 上一块留下（含接上）的段在 out 里的位置
    for b in blocks:
        cur: list[int] = []
        for x in b:
            xs, _xe = get_span(x)
            hit = next((i for i in prev if same_f(out[i], x) and adj(get_span(out[i])[1], xs)), None)
            if hit is not None:
                out[hit] = combine(out[hit], x)
                prev.remove(hit)
                cur.append(hit)
            else:
                out.append(x)
                cur.append(len(out) - 1)
        prev = cur
    return out
