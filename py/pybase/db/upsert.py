"""不分方言的"有则更新、无则插入"与"有则忽略"：两种方言都走 ``ON CONFLICT``。

``target`` 可以是 Engine / Connection / Session（规则同 ``schema``：Engine 自己提交）。
冲突目标 ``index_elements`` 必须对应表上的唯一约束或唯一索引。
"""

from typing import Any, Dict, List, Mapping, Optional, Sequence, Union

from sqlalchemy import Table

from ._bind import Bind, connection_of, dialect_name

Rows = Union[Mapping[str, Any], Sequence[Mapping[str, Any]]]


def _insert(bind: Bind, table: Table):
    name = dialect_name(bind)
    if name == "sqlite":
        from sqlalchemy.dialects.sqlite import insert
    elif name == "postgresql":
        from sqlalchemy.dialects.postgresql import insert
    else:
        raise NotImplementedError(f"不支持的方言: {name}")
    return insert(table)


def _rows(values: Rows) -> List[Dict[str, Any]]:
    rows = [dict(values)] if isinstance(values, Mapping) else [dict(v) for v in values]
    if not rows:
        return []
    keys = set(rows[0])
    if any(set(r) != keys for r in rows):
        raise ValueError("批量写入的每一行必须有相同的列")
    return rows


def _run(conn, table: Table, stmt, rows: List[Dict[str, Any]]) -> int:
    """多行合成 ``VALUES (...),(...)`` 一条语句，按参数数量分块避开 PostgreSQL 的 65535
    参数上限。行数靠 ``RETURNING`` 数（带自增主键的表 SQLAlchemy 隐式 RETURNING，
    cursor.rowcount 会是 -1，不可用）。"""
    step = max(1, 30000 // max(1, len(rows[0])))
    pk = list(table.primary_key.columns) or [list(table.c)[0]]
    stmt = stmt.returning(pk[0])
    return sum(len(conn.execute(stmt.values(rows[i:i + step])).fetchall())
               for i in range(0, len(rows), step))


def upsert(bind: Bind, table: Table, values: Rows, *,
           index_elements: Sequence[str],
           update_columns: Optional[Sequence[str]] = None) -> int:
    """有则更新、无则插入。

    Args:
        values: 一行（dict）或多行（同列的 dict 列表）。
        index_elements: 冲突判定的列。
        update_columns: 冲突时更新哪些列，用待插入的值覆盖；缺省为
            ``values`` 里除 ``index_elements`` 外的全部列。若为空（没有可更新的列）则退化为忽略。
    Returns:
        受影响行数（插入+更新）。
    """
    rows = _rows(values)
    if not rows:
        return 0
    cols = list(update_columns) if update_columns is not None else \
        [k for k in rows[0] if k not in set(index_elements)]
    with connection_of(bind) as conn:
        stmt = _insert(conn, table)
        if cols:
            stmt = stmt.on_conflict_do_update(
                index_elements=list(index_elements),
                set_={c: stmt.excluded[c] for c in cols})
        else:
            stmt = stmt.on_conflict_do_nothing(index_elements=list(index_elements))
        return _run(conn, table, stmt, rows)


def insert_ignore(bind: Bind, table: Table, values: Rows, *,
                  index_elements: Optional[Sequence[str]] = None) -> int:
    """有则忽略（冲突的行跳过，其余照插）。``index_elements`` 缺省 = 任何唯一冲突都忽略。

    Returns: 实际插入的行数。
    """
    rows = _rows(values)
    if not rows:
        return 0
    with connection_of(bind) as conn:
        stmt = _insert(conn, table).on_conflict_do_nothing(
            index_elements=list(index_elements) if index_elements else None)
        return _run(conn, table, stmt, rows)
