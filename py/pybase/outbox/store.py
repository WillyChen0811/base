"""调查的存储：一个 SQLite 文件（标准库 sqlite3），目录 0700、文件 0600。

两张表（列名是这里定的）：

- ``investigation``：一次调查。id、标题、用途、范围（JSON 路径列表）、起头的是谁（调用方给的一个词）、状态、放行稿、
  放行稿是不是用户改过（改过就不再自动换成最新一轮的回复）、退回时那句话、建立 / 改动 / 放行或退回的时间。
- ``round``：一轮。所属调查、第几轮、提示词、回复、状态、错误、这一轮跑完回的会话号、这一轮接着的会话号、
  建立 / 开跑 / 跑完的时间、跑它的进程号（服务重启后认出没跑完的）、本地代理的进程号、几项附带信息（JSON）。

每次操作现开一个连接（多线程各用各的）；写一律 ``BEGIN IMMEDIATE``（:meth:`Store.tx`），状态的判断和修改在同一个事务里，
两个请求同时改只有一个成。日志模式用 WAL（轮询读和后台写不互相挡）。
"""
from __future__ import annotations

import json
import os
import sqlite3
from contextlib import contextmanager
from typing import Iterator, List, Optional

# 调查的状态
DRAFT, RUNNING, WAITING, APPROVED, REJECTED = "draft", "running", "waiting", "approved", "rejected"
STATUSES = (DRAFT, RUNNING, WAITING, APPROVED, REJECTED)
CLOSED = (APPROVED, REJECTED)
# 一轮的状态
R_PENDING, R_RUNNING, R_DONE, R_FAILED = "pending", "running", "done", "failed"
ROUND_STATUSES = (R_PENDING, R_RUNNING, R_DONE, R_FAILED)

SCHEMA_VERSION = 1
_SCHEMA = """
CREATE TABLE IF NOT EXISTS investigation (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    purpose TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL DEFAULT '[]',
    origin TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    draft TEXT NOT NULL DEFAULT '',
    draft_edited INTEGER NOT NULL DEFAULT 0,
    note TEXT NOT NULL DEFAULT '',
    created_at REAL NOT NULL,
    updated_at REAL NOT NULL,
    closed_at REAL
);
CREATE TABLE IF NOT EXISTS round (
    inv_id TEXT NOT NULL REFERENCES investigation(id) ON DELETE CASCADE,
    n INTEGER NOT NULL,
    prompt TEXT NOT NULL,
    reply TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    session_id TEXT,
    resume_from TEXT,
    created_at REAL NOT NULL,
    started_at REAL,
    finished_at REAL,
    runner_pid INTEGER,
    agent_pid INTEGER,
    info TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (inv_id, n)
);
CREATE INDEX IF NOT EXISTS ix_round_status ON round(status);
"""


def _ensure_private_file(path: str) -> None:
    d = os.path.dirname(path) or "."
    os.makedirs(d, mode=0o700, exist_ok=True)
    os.chmod(d, 0o700)
    if not os.path.exists(path):
        try:
            os.close(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
        except FileExistsError:
            pass
    os.chmod(path, 0o600)


def _inv(row: sqlite3.Row) -> dict:
    d = dict(row)
    try:
        scope = json.loads(d.get("scope") or "[]")
    except ValueError:
        scope = []
    d["scope"] = [str(p) for p in scope] if isinstance(scope, list) else []
    d["draft_edited"] = bool(d.get("draft_edited"))
    return d


def _round(row: sqlite3.Row) -> dict:
    d = dict(row)
    try:
        info = json.loads(d.get("info") or "{}")
    except ValueError:
        info = {}
    d["info"] = info if isinstance(info, dict) else {}
    return d


class Store:
    def __init__(self, path: str):
        self.path = os.path.abspath(os.path.expanduser(path))
        _ensure_private_file(self.path)
        with self._connect() as c:
            c.execute("PRAGMA journal_mode=WAL")
            c.executescript(_SCHEMA)
            if c.execute("PRAGMA user_version").fetchone()[0] < SCHEMA_VERSION:
                c.execute("PRAGMA user_version=%d" % SCHEMA_VERSION)

    @contextmanager
    def _connect(self) -> Iterator[sqlite3.Connection]:
        c = sqlite3.connect(self.path, timeout=30, isolation_level=None)
        try:
            c.row_factory = sqlite3.Row
            c.execute("PRAGMA foreign_keys=ON")
            yield c
        finally:
            c.close()

    @contextmanager
    def read(self) -> Iterator[sqlite3.Connection]:
        with self._connect() as c:
            yield c

    @contextmanager
    def tx(self) -> Iterator[sqlite3.Connection]:
        """写事务（BEGIN IMMEDIATE）：块里抛错整笔回滚。"""
        with self._connect() as c:
            c.execute("BEGIN IMMEDIATE")
            try:
                yield c
            except BaseException:
                c.execute("ROLLBACK")
                raise
            c.execute("COMMIT")

    # ---- 读 ----
    @staticmethod
    def investigation(c: sqlite3.Connection, inv_id: str) -> Optional[dict]:
        r = c.execute("SELECT * FROM investigation WHERE id=?", (inv_id,)).fetchone()
        return _inv(r) if r is not None else None

    @staticmethod
    def rounds(c: sqlite3.Connection, inv_id: str) -> List[dict]:
        return [_round(r) for r in c.execute("SELECT * FROM round WHERE inv_id=? ORDER BY n", (inv_id,))]

    @staticmethod
    def round(c: sqlite3.Connection, inv_id: str, n: int) -> Optional[dict]:
        r = c.execute("SELECT * FROM round WHERE inv_id=? AND n=?", (inv_id, n)).fetchone()
        return _round(r) if r is not None else None

    @staticmethod
    def summaries(c: sqlite3.Connection) -> List[dict]:
        """每份调查一行，带轮数、有没有待发的一轮、最后一轮的状态。"""
        rows = c.execute(
            "SELECT i.id, i.title, i.status, i.origin, i.created_at, i.updated_at, i.closed_at, "
            "(SELECT COUNT(*) FROM round r WHERE r.inv_id=i.id) AS rounds, "
            "(SELECT COUNT(*) FROM round r WHERE r.inv_id=i.id AND r.status='pending') AS pending, "
            "(SELECT r.status FROM round r WHERE r.inv_id=i.id ORDER BY r.n DESC LIMIT 1) AS last_round "
            "FROM investigation i").fetchall()
        return [dict(r) for r in rows]

    @staticmethod
    def running_rounds(c: sqlite3.Connection) -> List[dict]:
        return [_round(r) for r in c.execute("SELECT * FROM round WHERE status='running'")]
