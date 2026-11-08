"""会话的存储：一个 SQLite 文件（标准库 sqlite3），目录 0700、文件 0600。

三张表（列名是这里定的）：

- ``conversation``：一份会话。id、标题、omlx 一边能看的范围（JSON 路径列表）与它是什么时候填的、从哪来的（窗口 / 终端）、
  终端发起方的名字、两边各自的会话号（接着聊用）、终端里在等转回的那个进程号、建立 / 改动时间。
- ``round``：一轮。所属会话、第几条（两边和转回发起方统一编号）、哪一边（``claude`` / ``omlx`` / ``requester`` = 转回给发起方）、
  模型、提示词（转回的那条就是转回的话）、回复、状态、错误、来源（窗口 / 终端）与发起方名字、是不是闸门有命中仍然发的、
  这一轮跑完回的会话号、这一轮接着的会话号、建立 / 开跑 / 跑完 / 发起方取走的时间、跑它的进程号、代理的进程号、附带信息（JSON）。
- ``step``：一轮在跑时代理的每一步（读了哪个文件、搜了什么、中间说的话），逐条追加。

每次操作现开一个连接（多线程、多进程各用各的）；写一律 ``BEGIN IMMEDIATE``（:meth:`Store.tx`），状态的判断和修改在同一个事务里，
两个请求同时改只有一个成。日志模式用 WAL（轮询读和后台写不互相挡）。
打开的是 11/08 之前「调查」那一版的库（有 ``investigation`` 表）：那一版只有冒烟和测试数据，整库的表删掉重建，不迁移。
"""
from __future__ import annotations

import json
import os
import sqlite3
from contextlib import contextmanager
from typing import Iterator, List, Optional

# 两边，和「转回给发起方」那一条
CLAUDE, OMLX, REQUESTER = "claude", "omlx", "requester"
SIDES = (CLAUDE, OMLX)
# 一轮的状态
R_RUNNING, R_DONE, R_FAILED = "running", "done", "failed"
ROUND_STATUSES = (R_RUNNING, R_DONE, R_FAILED)
# 会话从哪来 / 一轮的提示词从哪来
ORIGIN_WINDOW, ORIGIN_TERMINAL = "window", "terminal"
ORIGINS = (ORIGIN_WINDOW, ORIGIN_TERMINAL)

SCHEMA_VERSION = 2
_SCHEMA = """
CREATE TABLE IF NOT EXISTS conversation (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL DEFAULT '[]',
    scope_at REAL,
    origin TEXT NOT NULL DEFAULT '',
    requester TEXT NOT NULL DEFAULT '',
    claude_session TEXT,
    omlx_session TEXT,
    wait_pid INTEGER,
    created_at REAL NOT NULL,
    updated_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS round (
    conv_id TEXT NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
    n INTEGER NOT NULL,
    side TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    prompt TEXT NOT NULL,
    reply TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    requester TEXT NOT NULL DEFAULT '',
    forced INTEGER NOT NULL DEFAULT 0,
    session_id TEXT,
    resume_from TEXT,
    created_at REAL NOT NULL,
    started_at REAL,
    finished_at REAL,
    delivered_at REAL,
    runner_pid INTEGER,
    agent_pid INTEGER,
    info TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (conv_id, n)
);
CREATE INDEX IF NOT EXISTS ix_round_status ON round(status);
CREATE TABLE IF NOT EXISTS step (
    conv_id TEXT NOT NULL,
    n INTEGER NOT NULL,
    k INTEGER NOT NULL,
    at REAL NOT NULL,
    data TEXT NOT NULL,
    PRIMARY KEY (conv_id, n, k),
    FOREIGN KEY (conv_id, n) REFERENCES round(conv_id, n) ON DELETE CASCADE
);
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


def _json(text, empty):
    try:
        v = json.loads(text or "")
    except ValueError:
        return empty
    return v if isinstance(v, type(empty)) else empty


def _conv(row: sqlite3.Row) -> dict:
    d = dict(row)
    d["scope"] = [str(p) for p in _json(d.get("scope"), [])]
    return d


def _round(row: sqlite3.Row) -> dict:
    d = dict(row)
    d["info"] = _json(d.get("info"), {})
    d["forced"] = bool(d.get("forced"))
    return d


def like(q: str) -> str:
    """LIKE 的模式：``%`` ``_`` ``\\`` 转义（配 ``ESCAPE '\\'``），两头加 ``%``。"""
    return "%" + q.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_") + "%"


class Store:
    def __init__(self, path: str):
        self.path = os.path.abspath(os.path.expanduser(path))
        _ensure_private_file(self.path)
        with self._connect() as c:
            c.execute("PRAGMA journal_mode=WAL")
            if c.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='investigation'").fetchone():
                # 11/08 前的「调查」库（版本 1，只有冒烟和测试数据，用户定丢掉重建）：表名 round 撞了，先删
                c.executescript("BEGIN; DROP TABLE IF EXISTS step; DROP TABLE IF EXISTS round; "
                                "DROP TABLE IF EXISTS conversation; DROP TABLE IF EXISTS investigation; COMMIT;")
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
    def conversation(c: sqlite3.Connection, conv_id: str) -> Optional[dict]:
        r = c.execute("SELECT * FROM conversation WHERE id=?", (conv_id,)).fetchone()
        return _conv(r) if r is not None else None

    @staticmethod
    def rounds(c: sqlite3.Connection, conv_id: str) -> List[dict]:
        return [_round(r) for r in c.execute("SELECT * FROM round WHERE conv_id=? ORDER BY n", (conv_id,))]

    @staticmethod
    def round(c: sqlite3.Connection, conv_id: str, n: int) -> Optional[dict]:
        r = c.execute("SELECT * FROM round WHERE conv_id=? AND n=?", (conv_id, n)).fetchone()
        return _round(r) if r is not None else None

    @staticmethod
    def running(c: sqlite3.Connection, conv_id: str, side: str) -> Optional[dict]:
        r = c.execute("SELECT * FROM round WHERE conv_id=? AND side=? AND status=? ORDER BY n DESC LIMIT 1",
                      (conv_id, side, R_RUNNING)).fetchone()
        return _round(r) if r is not None else None

    @staticmethod
    def steps(c: sqlite3.Connection, conv_id: str) -> dict:
        """``{n: [{k, at, data}…]}``（这份会话所有轮的步骤）。"""
        out = {}
        for r in c.execute("SELECT n, k, at, data FROM step WHERE conv_id=? ORDER BY n, k", (conv_id,)):
            out.setdefault(r["n"], []).append({"k": r["k"], "at": r["at"], "data": _json(r["data"], {})})
        return out

    @staticmethod
    def summaries(c: sqlite3.Connection, q: str = "", origin: str = "") -> List[dict]:
        """每份会话一行，带轮数、在跑的两边、最后一条是哪边 / 什么状态。``q`` 按标题、发起方、各轮提示词和回复找（包含即可）。"""
        where, args = [], []
        if origin:
            where.append("c.origin=?")
            args.append(origin)
        if q:
            pat = like(q)
            where.append("(c.title LIKE ? ESCAPE '\\' OR c.requester LIKE ? ESCAPE '\\' OR EXISTS (SELECT 1 FROM round "
                         "r WHERE r.conv_id=c.id AND (r.prompt LIKE ? ESCAPE '\\' OR r.reply LIKE ? ESCAPE '\\')))")
            args += [pat, pat, pat, pat]
        rows = c.execute(
            "SELECT c.id, c.title, c.origin, c.requester, c.wait_pid, c.created_at, c.updated_at, "
            "(SELECT COUNT(*) FROM round r WHERE r.conv_id=c.id) AS rounds, "
            "(SELECT GROUP_CONCAT(r.side) FROM round r WHERE r.conv_id=c.id AND r.status='running') AS running, "
            "(SELECT r.side FROM round r WHERE r.conv_id=c.id ORDER BY r.n DESC LIMIT 1) AS last_side, "
            "(SELECT r.status FROM round r WHERE r.conv_id=c.id ORDER BY r.n DESC LIMIT 1) AS last_status "
            "FROM conversation c" + (" WHERE " + " AND ".join(where) if where else ""), args).fetchall()
        return [dict(r) for r in rows]

    @staticmethod
    def last_scope(c: sqlite3.Connection) -> List[str]:
        """最近一次填的范围（新建或改范围的时间最近的那份会话的）。"""
        r = c.execute("SELECT scope FROM conversation WHERE scope_at IS NOT NULL AND scope NOT IN ('', '[]') "
                      "ORDER BY scope_at DESC LIMIT 1").fetchone()
        return [str(p) for p in _json(r["scope"], [])] if r is not None else []

    @staticmethod
    def running_rounds(c: sqlite3.Connection) -> List[dict]:
        return [_round(r) for r in c.execute("SELECT * FROM round WHERE status='running'")]
