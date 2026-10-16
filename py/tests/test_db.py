"""pybase.db：同一组用例参数化到 SQLite 与 PostgreSQL。

PostgreSQL 连本机 127.0.0.1:5432（当前系统用户免密），每次运行建一个 ``pybase_test_`` 开头的
临时库、跑完删除；连不上就 skip。只会动自己建的那个库。
"""

import os
import sys
import uuid

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

sa = pytest.importorskip("sqlalchemy")

from sqlalchemy import (Column, Integer, MetaData, String, Table, Text,  # noqa: E402
                        UniqueConstraint, select, text)

from pybase import db  # noqa: E402

PG_ADMIN_URL = "postgresql+psycopg://127.0.0.1:5432/postgres"


@pytest.fixture(scope="session")
def pg_database():
    """建临时库，返回它的 URL；连不上 skip。"""
    pytest.importorskip("psycopg")
    try:
        admin = db.make_engine(PG_ADMIN_URL)
        with admin.connect() as c:
            c.execute(text("SELECT 1"))
    except Exception as e:  # noqa: BLE001
        pytest.skip(f"本机 PostgreSQL 不可用: {type(e).__name__}")
    name = f"pybase_test_{uuid.uuid4().hex[:12]}"
    assert name.startswith("pybase_test_")
    with admin.connect().execution_options(isolation_level="AUTOCOMMIT") as c:
        c.execute(text(f'CREATE DATABASE "{name}"'))
    try:
        yield f"postgresql+psycopg://127.0.0.1:5432/{name}"
    finally:
        with admin.connect().execution_options(isolation_level="AUTOCOMMIT") as c:
            c.execute(text(f'DROP DATABASE IF EXISTS "{name}" WITH (FORCE)'))
        admin.dispose()


@pytest.fixture(params=["sqlite", "postgresql"])
def engine(request, tmp_path):
    if request.param == "sqlite":
        eng = db.make_engine(f"sqlite:///{tmp_path / 't.db'}")
    else:
        url = request.getfixturevalue("pg_database")
        eng = db.make_engine(url)
        with eng.begin() as c:  # 每个用例从空 schema 开始
            c.execute(text("DROP SCHEMA public CASCADE"))
            c.execute(text("CREATE SCHEMA public"))
    yield eng
    eng.dispose()


def _items() -> Table:
    return Table("items", MetaData(),
                 Column("id", Integer, primary_key=True, autoincrement=True),
                 Column("a", String(20), nullable=False),
                 Column("b", String(20), nullable=False),
                 Column("note", Text),
                 UniqueConstraint("a", "b", name="uq_items_ab"))


def _count(engine, table="items") -> int:
    with engine.connect() as c:
        return c.execute(text(f"SELECT count(*) FROM {table}")).scalar()


# ---------------------------------------------------------------- 引擎与会话

def test_建引擎并能查询(engine):
    with engine.connect() as c:
        assert c.execute(text("SELECT 1")).scalar() == 1


def test_sqlite连接设置(tmp_path):
    eng = db.make_engine(f"sqlite:///{tmp_path / 'w.db'}", sqlite_busy_timeout=7)
    with eng.connect() as c:
        assert c.execute(text("PRAGMA journal_mode")).scalar() == "wal"
        assert c.execute(text("PRAGMA busy_timeout")).scalar() == 7000
    eng.dispose()


def test_sqlite关闭wal(tmp_path):
    eng = db.make_engine(f"sqlite:///{tmp_path / 'n.db'}", sqlite_wal=False)
    with eng.connect() as c:
        assert c.execute(text("PRAGMA journal_mode")).scalar() != "wal"
    eng.dispose()


def test_sqlite内存库跨连接共享():
    eng = db.make_engine("sqlite://")
    _items().create(eng)
    with eng.connect() as c:
        assert c.execute(text("SELECT count(*) FROM items")).scalar() == 0


def test_url归一化与脱敏():
    assert db.normalize_url("postgres://u:p@h/d") == "postgresql+psycopg://u:p@h/d"
    assert db.normalize_url("postgresql://u@h/d") == "postgresql+psycopg://u@h/d"
    assert db.normalize_url("sqlite:///x.db") == "sqlite:///x.db"
    masked = db.mask_url("postgresql+psycopg://u:secret@h:5432/d")
    assert "secret" not in masked and "u:***@h" in masked
    assert "secret" not in db.mask_url("not a url secret")


def test_psycopg未安装给明确报错(monkeypatch):
    import importlib.util as iu
    real = iu.find_spec
    monkeypatch.setattr(iu, "find_spec",
                        lambda n, *a, **k: None if n == "psycopg" else real(n, *a, **k))
    with pytest.raises(db.DriverNotInstalled, match=r"pybase\[postgres\]"):
        db.make_engine("postgresql://u:p@127.0.0.1/x")


def test_日志里不出现密码(caplog):
    import logging
    with caplog.at_level(logging.INFO, logger="pybase.db.engine"):
        db.make_engine("sqlite:///:memory:")
    assert caplog.text  # 有日志
    pytest.importorskip("psycopg")
    with caplog.at_level(logging.INFO, logger="pybase.db.engine"):
        db.make_engine("postgresql://u:topsecret@127.0.0.1/x")
    assert "topsecret" not in caplog.text


def test_session_scope提交与回滚(engine):
    _items().create(engine)
    factory = db.create_session_factory(engine)
    with db.session_scope(factory) as s:
        s.execute(text("INSERT INTO items (a, b) VALUES ('1','1')"))
    assert _count(engine) == 1
    with pytest.raises(RuntimeError):
        with db.session_scope(factory) as s:
            s.execute(text("INSERT INTO items (a, b) VALUES ('2','2')"))
            raise RuntimeError("boom")
    assert _count(engine) == 1


def test_transaction提交与回滚(engine):
    _items().create(engine)
    with db.transaction(engine) as c:
        c.execute(text("INSERT INTO items (a, b) VALUES ('1','1')"))
    assert _count(engine) == 1
    with pytest.raises(RuntimeError):
        with db.transaction(engine) as c:
            c.execute(text("INSERT INTO items (a, b) VALUES ('2','2')"))
            raise RuntimeError("boom")
    assert _count(engine) == 1


# ---------------------------------------------------------------- 表结构

def test_表与列查询(engine):
    assert not db.has_table(engine, "items")
    assert db.get_columns(engine, "items") == []
    assert db.column_names(engine, "items") == []
    assert db.create_table(engine, _items()) is True
    assert db.create_table(engine, _items()) is False  # 幂等
    assert db.has_table(engine, "items")
    assert "items" in db.table_names(engine)
    assert db.column_names(engine, "items") == ["id", "a", "b", "note"]
    assert db.has_column(engine, "items", "note")
    assert not db.has_column(engine, "items", "nope")


def test_补列(engine):
    assert db.add_column(engine, "items", "x", Text) is False  # 表不在
    db.create_table(engine, _items())
    with engine.begin() as c:
        c.execute(text("INSERT INTO items (a, b) VALUES ('1','1')"))
    assert db.add_column(engine, "items", "x", Text) is True
    assert db.add_column(engine, "items", "x", Text) is False  # 幂等
    assert db.add_column(engine, "items", "n", Integer, nullable=False, default=0)
    assert db.add_column(engine, "items", "s", String(10), nullable=False, default="")
    assert db.add_column(engine, "items", "flag", Integer, default=True)
    assert db.add_column(engine, "items", "j", Text, nullable=False, default="[]")
    with engine.connect() as c:
        row = c.execute(text("SELECT n, s, flag, j FROM items")).one()
    assert (row[0], row[1], row[3]) == (0, "", "[]") and row[2] in (1, True)
    cols = {c["name"]: c for c in db.get_columns(engine, "items")}
    assert cols["n"]["nullable"] is False and cols["x"]["nullable"] is True


def test_索引(engine):
    db.create_table(engine, _items())
    assert db.create_index(engine, "items", ["a"]) is True
    assert db.create_index(engine, "items", ["a"]) is False  # 幂等
    assert db.has_index(engine, "items", "ix_items_a")
    assert db.create_index(engine, "items", ["a", "note"], name="ix_custom") is True
    assert db.create_index(engine, "items", ["note"], name="uq_note", unique=True)
    by_name = {i["name"]: i for i in db.get_indexes(engine, "items")}
    assert by_name["ix_custom"]["column_names"] == ["a", "note"]
    assert by_name["uq_note"]["unique"] and not by_name["ix_items_a"]["unique"]
    assert db.create_index(engine, "ghost", ["a"]) is False
    db.drop_index(engine, "ix_items_a")
    db.drop_index(engine, "ix_items_a")  # 幂等
    assert not db.has_index(engine, "items", "ix_items_a")


def test_唯一约束查询(engine):
    db.create_table(engine, _items())
    ucs = db.get_unique_constraints(engine, "items")
    assert [(u["name"], sorted(u["column_names"])) for u in ucs] == [("uq_items_ab", ["a", "b"])]
    found = db.find_unique(engine, "items", ["b", "a"])
    assert found == {"constraints": ["uq_items_ab"], "indexes": []}
    assert db.find_unique(engine, "items", ["a"]) == {"constraints": [], "indexes": []}


def test_删唯一约束_数据与其余索引保留(engine):
    db.create_table(engine, _items())
    db.create_index(engine, "items", ["a"])
    with engine.begin() as c:
        c.execute(text("INSERT INTO items (a, b, note) VALUES ('1','1','n')"))
    with pytest.raises(sa.exc.IntegrityError):
        with engine.begin() as c:
            c.execute(text("INSERT INTO items (a, b) VALUES ('1','1')"))
    assert db.drop_unique(engine, "items", ["a", "b"]) == 1
    assert db.drop_unique(engine, "items", ["a", "b"]) == 0  # 幂等
    with engine.begin() as c:  # 现在允许重复
        c.execute(text("INSERT INTO items (a, b) VALUES ('1','1')"))
    assert _count(engine) == 2
    assert db.has_index(engine, "items", "ix_items_a")
    with engine.connect() as c:
        assert c.execute(text("SELECT note FROM items WHERE id=1")).scalar() == "n"


def test_删唯一索引形式的唯一性(engine):
    t = Table("u", MetaData(), Column("id", Integer, primary_key=True),
              Column("a", String(10)), Column("b", String(10)))
    db.create_table(engine, t)
    db.create_index(engine, "u", ["a", "b"], name="ux_ab", unique=True)
    assert db.find_unique(engine, "u", ["a", "b"])["indexes"] == ["ux_ab"]
    assert db.drop_unique(engine, "u", ["a", "b"]) == 1
    assert not db.has_index(engine, "u", "ux_ab")


def test_sqlite重建保留自增(tmp_path):
    eng = db.make_engine(f"sqlite:///{tmp_path / 'ai.db'}")
    with eng.begin() as c:
        c.execute(text("CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, "
                       "a TEXT, b TEXT, UNIQUE (a, b))"))
        c.execute(text("INSERT INTO t (a, b) VALUES ('1','1')"))
        c.execute(text("DELETE FROM t"))
    assert db.drop_unique(eng, "t", ["a", "b"]) == 1  # 匿名约束
    with eng.begin() as c:
        c.execute(text("INSERT INTO t (a, b) VALUES ('2','2')"))
        assert c.execute(text("SELECT id FROM t")).scalar() == 2  # AUTOINCREMENT 不复用 id
        assert "AUTOINCREMENT" in c.execute(text(
            "SELECT sql FROM sqlite_master WHERE name='t'")).scalar().upper()


def test_改列名与表名(engine):
    db.create_table(engine, _items())
    assert db.rename_column(engine, "items", "note", "memo") is True
    assert db.rename_column(engine, "items", "note", "memo") is False  # 旧列已不在
    assert db.rename_column(engine, "items", "a", "b") is False  # 新名已被占
    assert "memo" in db.column_names(engine, "items")
    db.rename_table(engine, "items", "things")
    assert db.has_table(engine, "things") and not db.has_table(engine, "items")


def test_删列(engine):
    db.create_table(engine, _items())
    assert db.drop_column(engine, "items", "note") is True
    assert db.drop_column(engine, "items", "note") is False
    assert "note" not in db.column_names(engine, "items")


def test_改可空性(engine):
    db.create_table(engine, _items())
    db.create_index(engine, "items", ["a"])
    with engine.begin() as c:
        c.execute(text("INSERT INTO items (a, b, note) VALUES ('1','1','n')"))
    assert db.set_nullable(engine, "items", "note", False) is True
    assert db.set_nullable(engine, "items", "note", False) is False  # 已是目标状态
    cols = {c["name"]: c for c in db.get_columns(engine, "items")}
    assert cols["note"]["nullable"] is False
    assert _count(engine) == 1 and db.has_index(engine, "items", "ix_items_a")
    assert db.set_nullable(engine, "items", "a", True) is True
    assert {c["name"]: c for c in db.get_columns(engine, "items")}["a"]["nullable"] is True


def test_删表(engine):
    assert db.drop_table(engine, "items") is False
    db.create_table(engine, _items())
    assert db.drop_table(engine, "items") is True
    assert not db.has_table(engine, "items")


def test_重建表流程用现有函数拼得出来(engine):
    """业务侧"建新表→拷数据→删旧→改名→补索引"（migrations 0005 那种）只靠本包函数。"""
    db.create_table(engine, _items())
    with engine.begin() as c:
        c.execute(text("INSERT INTO items (a, b) VALUES ('1','1')"))
    new = Table("items_new", MetaData(), Column("id", Integer, primary_key=True),
                Column("a", String(20)), Column("extra", Text))
    db.create_table(engine, new)
    with engine.begin() as c:
        c.execute(text("INSERT INTO items_new (id, a) SELECT id, a FROM items"))
    db.drop_table(engine, "items")
    db.rename_table(engine, "items_new", "items")
    db.create_index(engine, "items", ["a"])
    assert db.column_names(engine, "items") == ["id", "a", "extra"] and _count(engine) == 1


def test_事务内的表结构操作可回滚(engine):
    db.create_table(engine, _items())
    with pytest.raises(RuntimeError):
        with engine.begin() as c:
            db.add_column(c, "items", "x", Text)
            assert db.has_column(c, "items", "x")
            raise RuntimeError("boom")
    assert not db.has_column(engine, "items", "x")


# ---------------------------------------------------------------- upsert / ignore

def test_upsert(engine):
    t = _items()
    db.create_table(engine, t)
    db.upsert(engine, t, {"a": "1", "b": "1", "note": "v1"}, index_elements=["a", "b"])
    db.upsert(engine, t, {"a": "1", "b": "1", "note": "v2"}, index_elements=["a", "b"])
    assert _count(engine) == 1
    with engine.connect() as c:
        assert c.execute(select(t.c.note)).scalar() == "v2"


def test_upsert批量与指定更新列(engine):
    t = _items()
    db.create_table(engine, t)
    db.upsert(engine, t, [{"a": "1", "b": "1", "note": "x"},
                          {"a": "2", "b": "2", "note": "y"}], index_elements=["a", "b"])
    db.upsert(engine, t, [{"a": "1", "b": "1", "note": "X"},
                          {"a": "3", "b": "3", "note": "z"}],
              index_elements=["a", "b"], update_columns=[])  # 空 = 只忽略
    with engine.connect() as c:
        got = dict(c.execute(select(t.c.a, t.c.note)).all())
    assert got == {"1": "x", "2": "y", "3": "z"}
    db.upsert(engine, t, {"a": "1", "b": "1", "note": "N"},
              index_elements=["a", "b"], update_columns=["note"])
    with engine.connect() as c:
        assert c.execute(select(t.c.note).where(t.c.a == "1")).scalar() == "N"
    with pytest.raises(ValueError):
        db.upsert(engine, t, [{"a": "1", "b": "1"}, {"a": "2", "b": "2", "note": "q"}],
                  index_elements=["a", "b"])
    assert db.upsert(engine, t, [], index_elements=["a", "b"]) == 0


def test_insert_ignore(engine):
    t = _items()
    db.create_table(engine, t)
    assert db.insert_ignore(engine, t, {"a": "1", "b": "1", "note": "first"}) == 1
    assert db.insert_ignore(engine, t, {"a": "1", "b": "1", "note": "second"}) == 0
    assert db.insert_ignore(engine, t, [{"a": "1", "b": "1", "note": "s"},
                                        {"a": "2", "b": "2", "note": "t"}],
                            index_elements=["a", "b"]) == 1
    assert _count(engine) == 2
    with engine.connect() as c:
        assert c.execute(select(t.c.note).where(t.c.a == "1")).scalar() == "first"


def test_upsert在会话与连接内不自行提交(engine):
    t = _items()
    db.create_table(engine, t)
    with pytest.raises(RuntimeError):
        with engine.begin() as c:
            db.upsert(c, t, {"a": "1", "b": "1"}, index_elements=["a", "b"])
            raise RuntimeError("boom")
    assert _count(engine) == 0
    factory = db.create_session_factory(engine)
    with db.session_scope(factory) as s:
        db.insert_ignore(s, t, {"a": "1", "b": "1"})
    assert _count(engine) == 1


# ---------------------------------------------------------------- 身份标记

def test_身份_无标记读为None且不建表(engine):
    assert db.read_identity(engine) is None
    assert not db.has_table(engine, db.IDENTITY_TABLE)
    assert db.IDENTITY_TABLE.startswith("_")


def test_身份_写入读取匹配(engine):
    db.write_identity(engine, "client")
    assert db.read_identity(engine) == "client"
    db.write_identity(engine, "client")  # 相同：不报错
    db.check_identity(engine, "client")


def test_身份_不匹配抛异常带期望与实际(engine):
    db.write_identity(engine, "client")
    with pytest.raises(db.IdentityMismatch) as ei:
        db.check_identity(engine, "server")
    assert (ei.value.expected, ei.value.actual) == ("server", "client")
    with pytest.raises(db.IdentityMismatch) as ei:
        db.write_identity(engine, "server")  # 不覆盖
    assert (ei.value.expected, ei.value.actual) == ("server", "client")
    assert db.read_identity(engine) == "client"


def test_身份_无标记_报错(engine):
    with pytest.raises(db.IdentityMissing) as ei:
        db.check_identity(engine, "client")
    assert ei.value.expected == "client" and ei.value.actual is None
    assert isinstance(ei.value, db.IdentityMismatch)
    assert db.read_identity(engine) is None  # 报错不顺手写


def test_身份_无标记_补写(engine):
    db.check_identity(engine, "client", on_missing="stamp")
    assert db.read_identity(engine) == "client"
    db.check_identity(engine, "client", on_missing="stamp")
    with pytest.raises(db.IdentityMismatch):
        db.check_identity(engine, "server", on_missing="stamp")  # 有标记不补写
    with pytest.raises(ValueError):
        db.check_identity(engine, "client", on_missing="whatever")


def test_身份_表名可配(engine):
    db.write_identity(engine, "a", table="_my_marker")
    assert db.has_table(engine, "_my_marker") and not db.has_table(engine, db.IDENTITY_TABLE)
    assert db.read_identity(engine, table="_my_marker") == "a"
    assert db.read_identity(engine) is None
    db.check_identity(engine, "a", table="_my_marker")
