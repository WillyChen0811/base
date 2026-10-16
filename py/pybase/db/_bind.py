"""内部：把 Engine / Connection / Session 统一成"拿到一个连接"。

本包的函数都接受这三种对象：给 Engine 就自己开事务、成功提交；给 Connection
或 Session 就并入调用方已有的事务，**不提交也不回滚**，由调用方管。
"""

from contextlib import contextmanager
from typing import Iterator, Union

from sqlalchemy import Connection, Engine
from sqlalchemy.orm import Session

Bind = Union[Engine, Connection, Session]


@contextmanager
def connection_of(bind: Bind, *, begin: bool = True) -> Iterator[Connection]:
    """``begin=True`` 且给的是 Engine：开事务，正常提交、异常回滚；其余情形直接借用。"""
    if isinstance(bind, Session):
        yield bind.connection()
    elif isinstance(bind, Connection):
        yield bind
    elif begin:
        with bind.begin() as conn:
            yield conn
    else:
        with bind.connect() as conn:
            yield conn


def dialect_name(bind: Bind) -> str:
    if isinstance(bind, Session):
        return bind.get_bind().dialect.name
    return bind.dialect.name
