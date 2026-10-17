"""pybase.db 审计触发器：PostgreSQL 上验证，SQLite 上验证"不支持"。"""

import os
import sys
import uuid
from datetime import datetime, timedelta, timezone

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
pytest.importorskip("sqlalchemy")

from sqlalchemy import text  # noqa: E402

from pybase import db  # noqa: E402
from test_db import PG_ADMIN_URL, pg_database  # noqa: E402,F401


@pytest.fixture
def pg(pg_database):  # noqa: F811
    eng = db.make_engine(pg_database, application_name="audit-test")
    with eng.begin() as c:
        c.execute(text("DROP SCHEMA public CASCADE"))
        c.execute(text("DROP SCHEMA IF EXISTS audit CASCADE"))
        c.execute(text("CREATE SCHEMA public"))
        c.execute(text("CREATE TABLE plans (id serial PRIMARY KEY, name text, n int)"))
        c.execute(text("CREATE TABLE links (a int, b int, PRIMARY KEY (a, b))"))
        c.execute(text("CREATE TABLE skipped (id serial PRIMARY KEY)"))
    yield eng
    eng.dispose()


def _install(eng, tables=("plans", "links")):
    return db.install_audit(eng, list(tables), context_keys=["xxyi.plan_id"])


def test_增删改都记到且改前改后正确(pg):
    assert _install(pg) is True
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name, n) VALUES ('a', 1)"))
    with pg.begin() as c:
        c.execute(text("UPDATE plans SET name='b' WHERE id=1"))
    with pg.begin() as c:
        c.execute(text("DELETE FROM plans WHERE id=1"))
    rows = db.query_audit(pg, table="plans")
    assert [r["op"] for r in rows] == ["INSERT", "UPDATE", "DELETE"]
    ins, upd, dele = rows
    assert ins["old_row"] is None and ins["new_row"] == {"id": 1, "name": "a", "n": 1}
    assert upd["old_row"]["name"] == "a" and upd["new_row"]["name"] == "b"
    assert dele["old_row"]["name"] == "b" and dele["new_row"] is None
    assert all(r["pk"] == {"id": 1} for r in rows)
    assert all(r["application_name"] == "audit-test" for r in rows)
    assert all(isinstance(r["changed_at"], datetime) and r["changed_at"].tzinfo for r in rows)
    assert all(isinstance(r["txid"], int) for r in rows)


def test_复合主键与未挂的表(pg):
    _install(pg)
    with pg.begin() as c:
        c.execute(text("INSERT INTO links VALUES (1, 2)"))
        c.execute(text("INSERT INTO skipped DEFAULT VALUES"))
    rows = db.query_audit(pg)
    assert [r["table_name"] for r in rows] == ["links"]
    assert rows[0]["pk"] == {"a": 1, "b": 2}


def test_上下文plan_id记到_没设为null(pg):
    _install(pg)
    with pg.begin() as c:
        db.set_audit_context(c, **{"xxyi.plan_id": "P-7"})
        c.execute(text("INSERT INTO plans (name) VALUES ('x')"))
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name) VALUES ('y')"))
    # 同一连接池里复用的连接，SET LOCAL 不应泄漏到下一个事务
    rows = db.query_audit(pg, table="plans")
    assert rows[0]["context"] == {"xxyi.plan_id": "P-7"}
    assert rows[1]["context"] == {"xxyi.plan_id": None}
    hit = db.query_audit(pg, context={"xxyi.plan_id": "P-7"})
    assert [r["id"] for r in hit] == [rows[0]["id"]]


def test_查询按时间范围和操作过滤(pg):
    _install(pg)
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name) VALUES ('x')"))
        c.execute(text("UPDATE plans SET n=5"))
    now = datetime.now(timezone.utc)
    assert len(db.query_audit(pg, since=now - timedelta(minutes=5))) == 2
    assert db.query_audit(pg, since=now + timedelta(minutes=5)) == []
    assert db.query_audit(pg, until=now - timedelta(minutes=5)) == []
    assert len(db.query_audit(pg, op="update")) == 1


def test_值没变的更新不记(pg):
    _install(pg)
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name, n) VALUES ('x', 1)"))
        c.execute(text("UPDATE plans SET n=1"))
    assert len(db.query_audit(pg)) == 1


def test_安装幂等不重复挂不丢数据(pg):
    _install(pg)
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name) VALUES ('x')"))
    _install(pg)
    _install(pg)
    with pg.connect() as c:
        n = c.execute(text("SELECT count(*) FROM pg_trigger WHERE tgname='pybase_audit' "
                           "AND NOT tgisinternal")).scalar()
        idx = c.execute(text("SELECT count(*) FROM pg_indexes WHERE schemaname='audit' "
                             "AND indexname='row_log_table_time_idx'")).scalar()
    assert n == 2 and idx == 1
    assert len(db.query_audit(pg)) == 1
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name) VALUES ('y')"))
    assert len(db.query_audit(pg)) == 2  # 重复安装没有造成一次写入记两笔


def test_卸载保留审计表(pg):
    _install(pg)
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name) VALUES ('x')"))
    assert db.audited_tables(pg) == ["links", "plans"]
    assert db.uninstall_audit(pg, ["plans"]) is True
    assert db.audited_tables(pg) == ["links"]
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name) VALUES ('y')"))
    assert len(db.query_audit(pg)) == 1


def test_表不存在与非法键报错(pg):
    with pytest.raises(ValueError):
        db.install_audit(pg, ["nope"])
    with pytest.raises(ValueError):
        db.install_audit(pg, ["plans"], context_keys=["plan_id"])
    assert db.query_audit(pg) == []  # 审计表未建时返回空


def test_application_name参数(pg_database):
    eng = db.make_engine(pg_database, application_name="my-app")
    with eng.connect() as c:
        assert c.execute(text("SHOW application_name")).scalar() == "my-app"
    eng.dispose()


def test_sqlite返回不支持(tmp_path):
    eng = db.make_engine(f"sqlite:///{tmp_path / 't.db'}", application_name="ignored")
    with eng.begin() as c:
        c.execute(text("CREATE TABLE plans (id INTEGER PRIMARY KEY)"))
    assert db.audit_supported(eng) is False
    assert db.install_audit(eng, ["plans"], context_keys=["xxyi.plan_id"]) is False
    assert db.uninstall_audit(eng, ["plans"]) is False
    with eng.begin() as c:
        assert db.set_audit_context(c, **{"xxyi.plan_id": "1"}) is False
    assert db.query_audit(eng) == []
    assert db.audited_tables(eng) == []
    with eng.connect() as c:
        names = [r[0] for r in c.execute(text("SELECT name FROM sqlite_master"))]
    assert not any("audit" in n or "row_log" in n for n in names)


def test_ignore_columns_只改忽略列的UPDATE不记(pg):
    assert db.install_audit(pg, ["plans"], ignore_columns=["n"]) is True
    with pg.begin() as c:
        c.execute(text("INSERT INTO plans (name, n) VALUES ('a', 1)"))
    with pg.begin() as c:
        c.execute(text("UPDATE plans SET n=2 WHERE id=1"))        # 只动忽略列：不记
    with pg.begin() as c:
        c.execute(text("UPDATE plans SET name='b', n=3 WHERE id=1"))   # 连带别的列：记
    assert [r["op"] for r in db.query_audit(pg, table="plans")] == ["INSERT", "UPDATE"]
    with pytest.raises(ValueError):
        db.install_audit(pg, ["plans"], ignore_columns=["n; drop"])
