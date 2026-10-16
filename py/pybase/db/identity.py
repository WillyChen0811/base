"""通用的库身份标记：在库里存一个值，证明"这个库是谁的"。

搬自 websubmit ``shared/db/connection.py`` 的 ``read_identity`` / ``write_identity`` /
``check_identity`` / ``IdentityMismatch``，去掉业务取值（客户端/服务器）；取值由调用方决定，
这里只当字符串比较。库里没有标记时的行为由调用方选：``on_missing="error"`` 抛
``IdentityMissing``，``"stamp"`` 按期望值补写。
"""

from typing import Optional

from sqlalchemy import Column, MetaData, String, Table, Text, inspect, select

from ._bind import Bind, connection_of
from .upsert import insert_ignore

DEFAULT_TABLE = "_pybase_identity"
_KEY = "identity"


class IdentityMismatch(RuntimeError):
    """库里记的身份与期望不一致。``expected`` 期望值，``actual`` 实际值。"""

    def __init__(self, expected: str, actual: Optional[str]):
        shown = "（无标记）" if actual is None else actual
        super().__init__(f"库身份不符：期望 {expected}，实际 {shown}")
        self.expected = expected
        self.actual = actual


class IdentityMissing(IdentityMismatch):
    """库里没有标记（``actual`` 为 None）；是 ``IdentityMismatch`` 的子类。"""


def _table(name: str) -> Table:
    return Table(name, MetaData(),
                 Column("key", String(60), primary_key=True),
                 Column("value", Text, nullable=False))


def read_identity(bind: Bind, *, table: str = DEFAULT_TABLE) -> Optional[str]:
    """读标记；表不在或没写过返回 None。纯只读，不建表。"""
    with connection_of(bind, begin=False) as conn:
        if not inspect(conn).has_table(table):
            return None
        t = _table(table)
        return conn.execute(select(t.c.value).where(t.c.key == _KEY)).scalar()


def write_identity(bind: Bind, value: str, *, table: str = DEFAULT_TABLE) -> None:
    """写标记（必要时建表）。没有就写入；已有且相同什么都不做；
    已有且不同抛 ``IdentityMismatch``，**不覆盖**。并发首写安全（冲突时以先到者为准再比对）。"""
    t = _table(table)
    with connection_of(bind) as conn:
        t.create(bind=conn, checkfirst=True)
        insert_ignore(conn, t, {"key": _KEY, "value": value},
                      index_elements=["key"])
        actual = conn.execute(select(t.c.value).where(t.c.key == _KEY)).scalar()
        if actual != value:
            raise IdentityMismatch(value, actual)


def check_identity(bind: Bind, expected: str, *, table: str = DEFAULT_TABLE,
                   on_missing: str = "error") -> None:
    """校验：标记与 ``expected`` 相同才放行，不符抛 ``IdentityMismatch``。

    ``on_missing`` 决定库里没有标记时怎么办：``"error"`` 抛 ``IdentityMissing``；
    ``"stamp"`` 按 ``expected`` 补写后放行。
    """
    if on_missing not in ("error", "stamp"):
        raise ValueError("on_missing 只能是 'error' 或 'stamp'")
    actual = read_identity(bind, table=table)
    if actual is None:
        if on_missing == "stamp":
            write_identity(bind, expected, table=table)
            return
        raise IdentityMissing(expected, None)
    if actual != expected:
        raise IdentityMismatch(expected, actual)
