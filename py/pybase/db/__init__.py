"""pybase.db —— 同一套代码在 SQLite 与 PostgreSQL 上都能跑的数据库基础操作（零业务）。

需要 extra：``pybase[db]``（SQLAlchemy）；连 PostgreSQL 再加 ``pybase[postgres]``。

- engine: 按 URL 建引擎（方言连接参数统一处理）、会话工厂、事务上下文、URL 脱敏
- schema: 不分方言的表结构操作（查表/列/索引、补列、建/删索引、删唯一约束、改名、删列…）
- upsert: ``ON CONFLICT`` 的"有则更新/有则忽略"
- identity: 库身份标记（写入/读取/校验）
- audit: PostgreSQL 行级审计触发器（安装/卸载/上下文/查询；SQLite 返回不支持）

业务表结构、业务升级脚本、身份取值都不在这里。
"""

from .audit import (audit_supported, audited_tables, install_audit, query_audit,
                    set_audit_context, uninstall_audit)
from .engine import (DriverNotInstalled, create_session_factory, make_engine,
                     mask_url, normalize_url, session_scope, transaction)
from .identity import (DEFAULT_TABLE as IDENTITY_TABLE, IdentityMismatch,
                       IdentityMissing, check_identity, read_identity,
                       write_identity)
from .schema import (SchemaOpUnsupported, add_column, column_names, create_index,
                     create_table, drop_column, drop_index, drop_table,
                     drop_unique, find_unique, get_columns, get_indexes,
                     get_unique_constraints, has_column, has_index, has_table,
                     rename_column, rename_table, set_nullable, table_names)
from .upsert import insert_ignore, upsert

__all__ = [
    "audit_supported", "audited_tables", "install_audit", "query_audit",
    "set_audit_context", "uninstall_audit",
    "DriverNotInstalled", "create_session_factory", "make_engine", "mask_url",
    "normalize_url", "session_scope", "transaction",
    "IDENTITY_TABLE", "IdentityMismatch", "IdentityMissing", "check_identity",
    "read_identity", "write_identity",
    "SchemaOpUnsupported", "add_column", "column_names", "create_index",
    "create_table", "drop_column", "drop_index", "drop_table", "drop_unique",
    "find_unique", "get_columns", "get_indexes", "get_unique_constraints",
    "has_column", "has_index", "has_table", "rename_column", "rename_table",
    "set_nullable", "table_names",
    "insert_ignore", "upsert",
]
