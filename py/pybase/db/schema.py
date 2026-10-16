"""不分方言的表结构操作（SQLite / PostgreSQL 同一套调用）。

范围按 websubmit ``shared/db/migrations.py`` 实际用到的操作划定，不做通用 ORM 迁移工具。
实现优先用 SQLAlchemy inspector 与 DDL 构造，不写 ``PRAGMA`` 与方言专有 SQL。

约定：
- 第一个参数 ``bind`` 可以是 Engine / Connection / Session。给 Engine 时每个函数自己开
  事务并提交；给 Connection / Session 时并入调用方的事务，不提交。
- 查询类函数每次现查，不吃 inspector 缓存；表不存在返回空，不抛错。
- 修改类函数都是幂等的（该有的已有、该删的已没有就什么都不做），返回"这次是否真的动了"。
- SQLite 不能原地做的改动（删唯一约束、改可空性）按官方推荐的"建新表→拷数据→
  删旧表→改名"重建，见 ``_rebuild_sqlite``；PostgreSQL 直接 ALTER。
"""

import logging
import sqlite3
from typing import Any, Callable, Dict, List, Optional, Sequence

from sqlalchemy import (Boolean, Column, MetaData, Table, UniqueConstraint, false,
                        inspect, select, text, true)
from sqlalchemy.schema import CreateColumn, DropIndex, DropTable, Index
from sqlalchemy.types import TypeEngine

from ._bind import Bind, connection_of

logger = logging.getLogger(__name__)


class SchemaOpUnsupported(RuntimeError):
    """当前方言/版本做不了这个操作（比如 SQLite < 3.35 的删列）。"""


# ---------------------------------------------------------------- 查询

def table_names(bind: Bind) -> List[str]:
    with connection_of(bind, begin=False) as conn:
        return inspect(conn).get_table_names()


def has_table(bind: Bind, table: str) -> bool:
    with connection_of(bind, begin=False) as conn:
        return inspect(conn).has_table(table)


def get_columns(bind: Bind, table: str) -> List[Dict[str, Any]]:
    """inspector 的列信息（name/type/nullable/default/primary_key…）；表不存在返回 []。"""
    with connection_of(bind, begin=False) as conn:
        insp = inspect(conn)
        return list(insp.get_columns(table)) if insp.has_table(table) else []


def column_names(bind: Bind, table: str) -> List[str]:
    return [c["name"] for c in get_columns(bind, table)]


def has_column(bind: Bind, table: str, column: str) -> bool:
    return column in column_names(bind, table)


def get_indexes(bind: Bind, table: str) -> List[Dict[str, Any]]:
    """索引列表，每项 ``{name, column_names, unique}``。

    不含唯一约束在 PostgreSQL 里附带生成的那条索引（它归 ``get_unique_constraints``）。
    """
    with connection_of(bind, begin=False) as conn:
        insp = inspect(conn)
        if not insp.has_table(table):
            return []
        return [
            {"name": i["name"], "column_names": list(i["column_names"]),
             "unique": bool(i.get("unique"))}
            for i in insp.get_indexes(table) if not i.get("duplicates_constraint")
        ]


def has_index(bind: Bind, table: str, name: str) -> bool:
    return any(i["name"] == name for i in get_indexes(bind, table))


def get_unique_constraints(bind: Bind, table: str) -> List[Dict[str, Any]]:
    """唯一约束列表，每项 ``{name, column_names}``（SQLite 里匿名约束 name 为 None）。"""
    with connection_of(bind, begin=False) as conn:
        insp = inspect(conn)
        if not insp.has_table(table):
            return []
        return [{"name": u.get("name"), "column_names": list(u["column_names"])}
                for u in insp.get_unique_constraints(table)]


def find_unique(bind: Bind, table: str, columns: Sequence[str]) -> Dict[str, list]:
    """列集合恰为 ``columns`` 的唯一性来源。

    唯一性可能以 UNIQUE 约束或唯一索引两种形式存在，两处都查：
    返回 ``{"constraints": [name…], "indexes": [name…]}``。
    """
    want = set(columns)
    return {
        "constraints": [u["name"] for u in get_unique_constraints(bind, table)
                        if set(u["column_names"]) == want],
        "indexes": [i["name"] for i in get_indexes(bind, table)
                    if i["unique"] and set(i["column_names"]) == want],
    }


# ---------------------------------------------------------------- 建/删表

def create_table(bind: Bind, table: Table) -> bool:
    """表不存在才建（用 SQLAlchemy 表定义，自增主键等两种方言各出各的 DDL）。"""
    with connection_of(bind) as conn:
        if inspect(conn).has_table(table.name):
            return False
        table.create(bind=conn)
        return True


def drop_table(bind: Bind, table: str) -> bool:
    """``DROP TABLE IF EXISTS``；返回此前表是否存在。"""
    with connection_of(bind) as conn:
        existed = inspect(conn).has_table(table)
        conn.execute(DropTable(Table(table, MetaData()), if_exists=True))
        return existed


def rename_table(bind: Bind, old: str, new: str) -> None:
    with connection_of(bind) as conn:
        q = conn.dialect.identifier_preparer.quote
        conn.execute(text(f"ALTER TABLE {q(old)} RENAME TO {q(new)}"))


# ---------------------------------------------------------------- 列

def _server_default(value: Any, type_: Any = None):
    if value is None or hasattr(value, "compile"):
        return value
    if isinstance(value, bool):
        if isinstance(type_, Boolean) or type_ is Boolean:
            return true() if value else false()
        return text("1" if value else "0")  # 整数列：PostgreSQL 不接受 true 当整数默认值
    if isinstance(value, (int, float)):
        return text(repr(value))
    return value  # str：SQLAlchemy 按字符串字面量渲染


def add_column(bind: Bind, table: str, name: str, type_: "TypeEngine | type",
               *, nullable: bool = True, default: Any = None) -> bool:
    """补一列；表不存在或列已存在返回 False。

    ``default`` 是**库端默认值**（写进 DDL 的 DEFAULT）：str / int / float / bool，
    或 SQLAlchemy 表达式。``nullable=False`` 必须同时给 default，否则 SQLite 拒绝
    给非空表加无默认值的非空列。
    """
    with connection_of(bind) as conn:
        insp = inspect(conn)
        if not insp.has_table(table):
            return False
        if name in {c["name"] for c in insp.get_columns(table)}:
            return False
        col = Column(name, type_, nullable=nullable,
                     server_default=_server_default(default, type_))
        Table(table, MetaData(), col)  # 列要挂在表上才能出 DDL
        ddl = CreateColumn(col).compile(dialect=conn.dialect)
        q = conn.dialect.identifier_preparer.quote
        conn.execute(text(f"ALTER TABLE {q(table)} ADD COLUMN {ddl}"))
        return True


def rename_column(bind: Bind, table: str, old: str, new: str) -> bool:
    """改列名（两种方言都是 ``RENAME COLUMN``）；旧列不在或新列已在返回 False。"""
    with connection_of(bind) as conn:
        cols = {c["name"] for c in get_columns(conn, table)}
        if old not in cols or new in cols:
            return False
        q = conn.dialect.identifier_preparer.quote
        conn.execute(text(
            f"ALTER TABLE {q(table)} RENAME COLUMN {q(old)} TO {q(new)}"))
        return True


def drop_column(bind: Bind, table: str, column: str) -> bool:
    """删列；列不在返回 False。SQLite < 3.35 做不了，抛 ``SchemaOpUnsupported``
    （调用方可自行决定"留着不管"）。被索引/约束引用的列 SQLite 也会拒绝，原样抛库错误。"""
    with connection_of(bind) as conn:
        if column not in {c["name"] for c in get_columns(conn, table)}:
            return False
        if conn.dialect.name == "sqlite" and sqlite3.sqlite_version_info < (3, 35):
            raise SchemaOpUnsupported(
                f"SQLite {sqlite3.sqlite_version} 不支持 DROP COLUMN（需要 >= 3.35）")
        q = conn.dialect.identifier_preparer.quote
        conn.execute(text(f"ALTER TABLE {q(table)} DROP COLUMN {q(column)}"))
        return True


def set_nullable(bind: Bind, table: str, column: str, nullable: bool) -> bool:
    """改列的可空性；已经是目标状态返回 False。

    SQLite 没有 ALTER COLUMN，走重建；改成 NOT NULL 时库里已有 NULL 会失败，先自己清理。
    """
    with connection_of(bind) as conn:
        cur = {c["name"]: c for c in get_columns(conn, table)}.get(column)
        if cur is None or bool(cur["nullable"]) == nullable:
            return False
        if conn.dialect.name == "sqlite":
            def modify(new: Table) -> None:
                new.c[column].nullable = nullable
            _rebuild_sqlite(conn, table, modify)
        else:
            q = conn.dialect.identifier_preparer.quote
            action = "DROP NOT NULL" if nullable else "SET NOT NULL"
            conn.execute(text(
                f"ALTER TABLE {q(table)} ALTER COLUMN {q(column)} {action}"))
        return True


# ---------------------------------------------------------------- 索引与唯一性

def create_index(bind: Bind, table: str, columns: Sequence[str], *,
                 name: Optional[str] = None, unique: bool = False) -> bool:
    """``CREATE [UNIQUE] INDEX IF NOT EXISTS`` 的方言无关版（按索引名判重）。

    ``name`` 缺省为 ``ix_<表>_<列…>``（唯一索引 ``uq_`` 前缀）。表不存在或索引已在返回 False。
    """
    name = name or f"{'uq' if unique else 'ix'}_{table}_{'_'.join(columns)}"
    with connection_of(bind) as conn:
        if not inspect(conn).has_table(table) or has_index(conn, table, name):
            return False
        t = Table(table, MetaData(), *[Column(c) for c in columns])
        Index(name, *[t.c[c] for c in columns], unique=unique).create(bind=conn)
        return True


def drop_index(bind: Bind, name: str) -> None:
    """``DROP INDEX IF EXISTS``（索引名在 SQLite 与 PostgreSQL 里都是库内全局的）。"""
    with connection_of(bind) as conn:
        conn.execute(DropIndex(Index(name), if_exists=True))


def drop_unique(bind: Bind, table: str, columns: Sequence[str]) -> int:
    """删掉列集合恰为 ``columns`` 的唯一约束与唯一索引，返回删了几个。

    PostgreSQL：``DROP CONSTRAINT`` / ``DROP INDEX``。SQLite：唯一索引直接删；
    UNIQUE 约束写在建表语句里，只能重建表（其余索引、自增属性一并带过去）。
    """
    with connection_of(bind) as conn:
        found = find_unique(conn, table, columns)
        n = 0
        q = conn.dialect.identifier_preparer.quote
        if found["constraints"]:
            if conn.dialect.name == "sqlite":
                want = set(columns)

                def modify(new: Table) -> None:
                    for c in list(new.constraints):
                        if isinstance(c, UniqueConstraint) and \
                                {col.name for col in c.columns} == want:
                            new.constraints.discard(c)
                _rebuild_sqlite(conn, table, modify)
            else:
                for cname in found["constraints"]:
                    conn.execute(text(
                        f"ALTER TABLE {q(table)} DROP CONSTRAINT {q(cname)}"))
            n += len(found["constraints"])
        for iname in found["indexes"]:
            drop_index(conn, iname)
            n += 1
        return n


# ---------------------------------------------------------------- SQLite 重建

def _rebuild_sqlite(conn, table: str, modify: Callable[[Table], None]) -> None:
    """SQLite 官方推荐的改表办法：反射旧表 → 建新表(经 ``modify`` 改定义) → 拷数据 →
    删旧 → 改名 → 重建普通索引。必须在调用方的事务里（``conn``）。

    已知边界：只保留"列名索引"（表达式索引、部分索引不会重建）；触发器、视图不带；
    表被别的表外键引用且开了 ``foreign_keys`` 时，DROP 旧表会触发外键动作，需调用方自理。
    """
    old = Table(table, MetaData(), autoload_with=conn)
    ddl = conn.execute(text(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name=:n"),
        {"n": table}).scalar() or ""
    tmp = f"{table}__pybase_new"
    new = old.to_metadata(MetaData(), name=tmp)
    new.indexes.clear()
    if "AUTOINCREMENT" in ddl.upper():
        new.dialect_options["sqlite"]["autoincrement"] = True
    modify(new)

    conn.execute(DropTable(Table(tmp, MetaData()), if_exists=True))
    new.create(bind=conn)
    cols = [c.name for c in new.columns if c.name in old.c]
    conn.execute(new.insert().from_select(
        cols, select(*[old.c[c] for c in cols])))
    seq = conn.execute(text("SELECT seq FROM sqlite_sequence WHERE name=:n"),
                       {"n": table}).scalar() if "AUTOINCREMENT" in ddl.upper() else None
    conn.execute(DropTable(Table(table, MetaData())))
    rename_table(conn, tmp, table)
    if seq is not None:  # 自增计数跟着带过去，已删除的最大 id 不会被复用
        conn.execute(text("UPDATE sqlite_sequence SET seq = :s WHERE name = :n"),
                     {"s": seq, "n": table})
    for idx in old.indexes:
        names = [c.name for c in idx.columns]
        if all(n in new.c for n in names):
            create_index(conn, table, names, name=idx.name, unique=bool(idx.unique))
