"""建引擎与会话：方言相关的连接设置统一收在这里。

搬自 websubmit ``shared/db/connection.py``（``normalize_db_url`` / ``make_engine``）
并扩充：PostgreSQL 连接池参数、psycopg 缺失的明确报错、URL 脱敏、会话与事务上下文。

- SQLite：``check_same_thread=False``；连接时开 WAL、设 busy_timeout、synchronous=NORMAL。
- PostgreSQL：``pool_pre_ping`` + 合理的池大小/回收时间。驱动 psycopg 3 按需检查，
  只用 SQLite 时不必装它。
"""

import importlib.util
import logging
from contextlib import contextmanager
from typing import Any, Dict, Iterator, Optional

from sqlalchemy import Connection, Engine, create_engine, event
from sqlalchemy.engine import make_url
from sqlalchemy.orm import Session, sessionmaker
from sqlalchemy.pool import StaticPool

logger = logging.getLogger(__name__)


class DriverNotInstalled(ImportError):
    """URL 指向的数据库需要的驱动没装。"""


def normalize_url(url: str) -> str:
    """``postgresql://`` / ``postgres://`` 补成 psycopg 3 驱动；其余原样。"""
    if url.startswith("postgres://"):
        url = "postgresql://" + url[len("postgres://"):]
    if url.startswith("postgresql://"):
        url = "postgresql+psycopg://" + url[len("postgresql://"):]
    return url


def mask_url(url: str) -> str:
    """URL 里的密码换成 ``***``，日志里打印 URL 一律走它。解析不了的串原样不输出细节。"""
    try:
        return make_url(url).render_as_string(hide_password=True)
    except Exception:  # noqa: BLE001  畸形 URL 也不能把原文（可能带密码）写进日志
        return "<unparsable url>"


def make_engine(
    url: str,
    *,
    echo: bool = False,
    sqlite_busy_timeout: float = 30.0,
    sqlite_wal: bool = True,
    pool_size: int = 5,
    max_overflow: int = 10,
    pool_recycle: int = 1800,
    pool_timeout: float = 30.0,
    connect_args: Optional[Dict[str, Any]] = None,
) -> Engine:
    """按 URL 建引擎。

    Args:
        sqlite_busy_timeout: 秒。同时设给驱动 ``timeout`` 与 ``PRAGMA busy_timeout``。
        sqlite_wal: 文件库连接时 ``PRAGMA journal_mode=WAL``（读不再被写阻塞）。
        pool_size / max_overflow / pool_recycle / pool_timeout: 仅 PostgreSQL 用。
        connect_args: 追加给驱动的参数，覆盖本函数的默认值。

    Raises:
        DriverNotInstalled: 要连 PostgreSQL（psycopg）但没装驱动。
    """
    url = normalize_url(url)
    parsed = make_url(url)
    backend = parsed.get_backend_name()

    if backend == "sqlite":
        engine = _make_sqlite(parsed, url, echo, sqlite_busy_timeout, sqlite_wal,
                              connect_args)
    elif backend == "postgresql":
        if parsed.get_driver_name() == "psycopg" and \
                importlib.util.find_spec("psycopg") is None:
            raise DriverNotInstalled(
                "连接 PostgreSQL 需要 psycopg 3，请安装："
                "pip install \"pybase[postgres]\"")
        engine = create_engine(
            url, echo=echo, pool_pre_ping=True, pool_size=pool_size,
            max_overflow=max_overflow, pool_recycle=pool_recycle,
            pool_timeout=pool_timeout,
            **({"connect_args": connect_args} if connect_args else {}))
    else:
        engine = create_engine(
            url, echo=echo, pool_pre_ping=True,
            **({"connect_args": connect_args} if connect_args else {}))
    logger.info("数据库引擎已创建: %s", mask_url(url))
    return engine


def _make_sqlite(parsed, url, echo, busy_timeout, wal, connect_args) -> Engine:
    args = {"check_same_thread": False, "timeout": busy_timeout}
    args.update(connect_args or {})
    kwargs: Dict[str, Any] = {}
    in_memory = not parsed.database or parsed.database == ":memory:"
    if in_memory:
        # 内存库每条连接是一份独立的库，必须共用同一条连接
        kwargs["poolclass"] = StaticPool
    engine = create_engine(url, echo=echo, connect_args=args, **kwargs)

    @event.listens_for(engine, "connect")
    def _pragmas(dbapi_conn, _record):
        # pysqlite 默认不会在 DDL 前开事务，导致建表/加列无法随事务回滚；
        # 关掉它的隐式事务管理，改由下面 begin 事件显式 BEGIN（SQLAlchemy 文档的标准做法）
        dbapi_conn.isolation_level = None
        cur = dbapi_conn.cursor()
        try:
            if wal and not in_memory:
                cur.execute("PRAGMA journal_mode=WAL")
            cur.execute(f"PRAGMA busy_timeout={int(busy_timeout * 1000)}")
            cur.execute("PRAGMA synchronous=NORMAL")
        finally:
            cur.close()

    @event.listens_for(engine, "begin")
    def _begin(conn):
        conn.exec_driver_sql("BEGIN")

    return engine


def create_session_factory(engine: Engine, **kwargs) -> sessionmaker:
    """会话工厂；默认 ``autoflush=False``，其余参数透传给 ``sessionmaker``。"""
    kwargs.setdefault("autoflush", False)
    return sessionmaker(bind=engine, **kwargs)


@contextmanager
def session_scope(factory: sessionmaker) -> Iterator[Session]:
    """会话事务：正常结束提交，异常回滚并重新抛出，最后一定关闭。"""
    session = factory()
    try:
        yield session
        session.commit()
    except Exception:
        session.rollback()
        raise
    finally:
        session.close()


@contextmanager
def transaction(engine: Engine) -> Iterator[Connection]:
    """连接事务（Core 用）：正常结束提交，异常回滚。"""
    with engine.begin() as conn:
        yield conn
