"""行级审计：在 PostgreSQL 的业务表上挂触发器，任何程序写入都自动记一笔改前/改后。

仅 PostgreSQL。SQLite 上 ``install_audit`` / ``uninstall_audit`` / ``set_audit_context`` 返回
``False``（不支持，不报错），``query_audit`` 返回空列表；先用 ``audit_supported`` 判断也行。

- 审计表 ``<schema>.row_log``（默认 ``audit.row_log``）：自增 id、变更时间（timestamptz）、表名、
  操作类型（INSERT/UPDATE/DELETE）、主键（jsonb）、改前整行、改后整行、application_name、
  事务号、上下文（jsonb）。按（表名, 变更时间）建索引。
- 上下文：安装时指定若干自定义设置键（如 ``xxyi.plan_id``），触发时用
  ``current_setting(key, true)`` 读，没设就是 null；写入方在事务里用 ``set_audit_context``
  设置（``SET LOCAL`` 语义，随事务结束失效）。
- 触发器名固定为 ``pybase_audit``；重复安装只更新触发器参数，不重复挂、不动已有审计数据。
- 值没变的 UPDATE（改前 == 改后）不记。
- 卸载只摘触发器，审计表与函数保留。
"""

import re
from datetime import datetime
from typing import Any, Dict, Iterable, List, Optional, Sequence

from sqlalchemy import inspect, text

from ._bind import Bind, connection_of, dialect_name

DEFAULT_SCHEMA = "audit"
LOG_TABLE = "row_log"
TRIGGER_NAME = "pybase_audit"
_FUNC = "log_change"

_IDENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
_CTX_KEY = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_.]*$")


def audit_supported(bind: Bind) -> bool:
    return dialect_name(bind) == "postgresql"


def _ident(name: str) -> str:
    if not _IDENT.match(name):
        raise ValueError(f"非法标识符: {name!r}")
    return '"' + name + '"'


def _check_keys(keys: Iterable[str]) -> List[str]:
    out = list(keys)
    for k in out:
        if not _CTX_KEY.match(k):
            raise ValueError(f"上下文键必须形如 'xxyi.plan_id': {k!r}")
    return out


def install_audit(bind: Bind, tables: Sequence[str], *, schema: str = DEFAULT_SCHEMA,
                  context_keys: Sequence[str] = ()) -> bool:
    """给 ``tables``（public 等默认 search_path 下的表）挂审计触发器，幂等。

    Returns:
        True 已安装；False 当前方言不支持（什么都没做）。
    Raises:
        ValueError: 表不存在、标识符或上下文键不合法。
    """
    if not audit_supported(bind):
        return False
    keys = _check_keys(context_keys)
    s = _ident(schema)
    ctx_arg = ",".join(keys)
    with connection_of(bind) as conn:
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext('pybase_audit_install'))"))
        insp = inspect(conn)
        plans = []
        for t in tables:
            if not insp.has_table(t):
                raise ValueError(f"表不存在，无法挂审计: {t}")
            pk = insp.get_pk_constraint(t).get("constrained_columns") or []
            plans.append((t, ",".join(pk)))
        conn.execute(text(f"CREATE SCHEMA IF NOT EXISTS {s}"))
        conn.execute(text(f"""
            CREATE TABLE IF NOT EXISTS {s}.{LOG_TABLE} (
                id bigserial PRIMARY KEY,
                changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
                table_name text NOT NULL,
                op text NOT NULL,
                pk jsonb,
                old_row jsonb,
                new_row jsonb,
                application_name text,
                txid bigint,
                context jsonb
            )"""))
        conn.execute(text(
            f"CREATE INDEX IF NOT EXISTS {LOG_TABLE}_table_time_idx "
            f"ON {s}.{LOG_TABLE} (table_name, changed_at)"))
        conn.execute(text(f"""
            CREATE OR REPLACE FUNCTION {s}.{_FUNC}() RETURNS trigger AS $fn$
            DECLARE
                pk_cols text[] := string_to_array(coalesce(TG_ARGV[0], ''), ',');
                ctx_keys text[] := string_to_array(coalesce(TG_ARGV[1], ''), ',');
                row_j jsonb;
                old_j jsonb;
                new_j jsonb;
                pk_j jsonb;
                ctx_j jsonb;
                k text;
                v text;
            BEGIN
                IF TG_OP <> 'INSERT' THEN old_j := to_jsonb(OLD); END IF;
                IF TG_OP <> 'DELETE' THEN new_j := to_jsonb(NEW); END IF;
                IF TG_OP = 'UPDATE' AND old_j = new_j THEN
                    RETURN NULL;
                END IF;
                row_j := coalesce(new_j, old_j);
                pk_j := '{{}}'::jsonb;
                FOREACH k IN ARRAY pk_cols LOOP
                    IF k <> '' THEN pk_j := pk_j || jsonb_build_object(k, row_j -> k); END IF;
                END LOOP;
                ctx_j := '{{}}'::jsonb;
                FOREACH k IN ARRAY ctx_keys LOOP
                    IF k <> '' THEN
                        v := nullif(current_setting(k, true), '');
                        ctx_j := ctx_j || jsonb_build_object(k, v);
                    END IF;
                END LOOP;
                INSERT INTO {s}.{LOG_TABLE}
                    (table_name, op, pk, old_row, new_row, application_name, txid, context)
                VALUES (TG_TABLE_NAME, TG_OP, pk_j, old_j, new_j,
                        current_setting('application_name', true),
                        txid_current(), ctx_j);
                RETURN NULL;
            END
            $fn$ LANGUAGE plpgsql"""))
        for t, pk in plans:
            q = _ident(t)
            conn.execute(text(f"DROP TRIGGER IF EXISTS {TRIGGER_NAME} ON {q}"))
            args = "'" + pk.replace("'", "''") + "','" + ctx_arg.replace("'", "''") + "'"
            conn.execute(text(
                f"CREATE TRIGGER {TRIGGER_NAME} AFTER INSERT OR UPDATE OR DELETE "
                f"ON {q} FOR EACH ROW EXECUTE FUNCTION {s}.{_FUNC}({args})"))
    return True


def uninstall_audit(bind: Bind, tables: Sequence[str]) -> bool:
    """摘掉 ``tables`` 上的审计触发器（审计表与函数保留）。False 表示方言不支持。"""
    if not audit_supported(bind):
        return False
    with connection_of(bind) as conn:
        insp = inspect(conn)
        for t in tables:
            if insp.has_table(t):
                conn.execute(text(f"DROP TRIGGER IF EXISTS {TRIGGER_NAME} ON {_ident(t)}"))
    return True


def audited_tables(bind: Bind) -> List[str]:
    """当前挂着审计触发器的表名（排序）；SQLite 返回空列表。"""
    if not audit_supported(bind):
        return []
    with connection_of(bind, begin=False) as conn:
        rows = conn.execute(text(
            "SELECT c.relname FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid "
            "WHERE t.tgname = :n AND NOT t.tgisinternal ORDER BY c.relname"),
            {"n": TRIGGER_NAME}).all()
    return [r[0] for r in rows]


def set_audit_context(bind: Bind, **settings: str) -> bool:
    """在当前事务里 ``SET LOCAL`` 自定义设置，如 ``set_audit_context(conn, **{"xxyi.plan_id": "P1"})``。

    应传 Connection / Session 以并入调用方事务；传 Engine 则设置随自己那个事务结束而失效，没有意义。
    False 表示方言不支持。
    """
    if not audit_supported(bind):
        return False
    _check_keys(settings)
    with connection_of(bind) as conn:
        for k, v in settings.items():
            conn.execute(text("SELECT set_config(:k, :v, true)"), {"k": k, "v": str(v)})
    return True


def query_audit(bind: Bind, *, table: Optional[str] = None, op: Optional[str] = None,
                since: Optional[datetime] = None, until: Optional[datetime] = None,
                context: Optional[Dict[str, str]] = None,
                limit: int = 1000, schema: str = DEFAULT_SCHEMA) -> List[Dict[str, Any]]:
    """按表/操作/时间范围（含 since，不含 until）/上下文键值查审计记录，按 id 升序。
    SQLite 或审计表尚未建立时返回空列表。"""
    if not audit_supported(bind):
        return []
    s = _ident(schema)
    where, params = [], {"limit": limit}
    if table:
        where.append("table_name = :table"); params["table"] = table
    if op:
        where.append("op = :op"); params["op"] = op.upper()
    if since:
        where.append("changed_at >= :since"); params["since"] = since
    if until:
        where.append("changed_at < :until"); params["until"] = until
    for i, (k, v) in enumerate((context or {}).items()):
        where.append(f"context ->> :ck{i} = :cv{i}")
        params[f"ck{i}"], params[f"cv{i}"] = k, str(v)
    sql = (f"SELECT id, changed_at, table_name, op, pk, old_row, new_row, "
           f"application_name, txid, context FROM {s}.{LOG_TABLE}"
           + (" WHERE " + " AND ".join(where) if where else "")
           + " ORDER BY id LIMIT :limit")
    with connection_of(bind, begin=False) as conn:
        if not inspect(conn).has_table(LOG_TABLE, schema=schema):
            return []
        return [dict(r._mapping) for r in conn.execute(text(sql), params)]
