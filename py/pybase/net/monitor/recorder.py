"""采集快照的存储、变化检测与查询。SQLite，路径由调用方给。

存储策略：每次 `record()` 把这一轮所有采集器的结果按内容哈希去重——跟这个
采集器"当前已知的最新值"一样就只存一条引用（`snapshot_items.stored='ref'`,
`payload` 留空），不一样才把 JSON 全量存进这一行并更新"最新值"缓存
（`latest` 表）。这一个机制天然覆盖了两种数据：interfaces/routes/dns/proxies/
vpn_services/netbird 这类基本不变的数据，绝大多数 tick 都只留一条引用，
几乎不占空间；targets/mihomo 这类每 tick 都在变（探测延迟、流量计数）的数据，
哈希总是不同，天然每条都全量存下来——不需要为不同采集器单独定策略。

`latest` 表独立于快照历史：`prune()` 删旧快照不影响"当前最新值"，重启后
`Recorder` 也能立刻从这张表恢复去重/对比的基准，不用扫全表。

线程安全：整个实例一把可重入锁串行化所有读写，跟 `pybase.search.SearchIndex`
同样的取舍——写操作本身互斥，个人监测场景的并发量也用不上真并发。
"""
from __future__ import annotations

import hashlib
import json
import sqlite3
import threading
from typing import Optional, Sequence

from . import events as _events

_SCHEMA = """
CREATE TABLE IF NOT EXISTS snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_at ON snapshots(at);

CREATE TABLE IF NOT EXISTS snapshot_items (
    snapshot_id INTEGER NOT NULL,
    collector TEXT NOT NULL,
    stored TEXT NOT NULL,
    payload TEXT,
    hash TEXT NOT NULL,
    PRIMARY KEY (snapshot_id, collector)
);
CREATE INDEX IF NOT EXISTS idx_snapshot_items_collector ON snapshot_items(collector, snapshot_id);

CREATE TABLE IF NOT EXISTS latest (
    collector TEXT PRIMARY KEY,
    payload TEXT NOT NULL,
    hash TEXT NOT NULL,
    at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at REAL NOT NULL,
    kind TEXT NOT NULL,
    subject TEXT NOT NULL,
    before TEXT,
    after TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_at ON events(at);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind);
"""


def _hash_of(payload_json: str) -> str:
    return hashlib.sha256(payload_json.encode("utf-8")).hexdigest()


class Recorder:
    """网络监测快照的记录与查询。`path` 是 SQLite 文件路径，`:memory:` 可用于测试。"""

    def __init__(self, path: str):
        self._con = sqlite3.connect(path, check_same_thread=False)
        self._con.execute("PRAGMA journal_mode=WAL")
        self._lock = threading.RLock()
        with self._lock:
            self._con.executescript(_SCHEMA)
            self._con.commit()

    def __enter__(self) -> "Recorder":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def close(self) -> None:
        with self._lock:
            self._con.close()

    # ---------- 写入 ----------

    def record(self, snapshot: dict, *, at: float) -> list[dict]:
        """存一轮快照（`{collector名: 采集器返回的信封dict}`），返回这一轮检测到的事件。

        `snapshot` 里的每个值必须是可 JSON 序列化的（采集器的约定本就如此）。
        """
        with self._lock:
            cur = self._con.execute("INSERT INTO snapshots(at) VALUES (?)", (at,))
            snapshot_id = cur.lastrowid
            new_events: list[dict] = []
            for collector, payload in snapshot.items():
                payload_json = json.dumps(payload, sort_keys=True, ensure_ascii=False)
                h = _hash_of(payload_json)
                prev_row = self._con.execute(
                    "SELECT payload, hash FROM latest WHERE collector = ?", (collector,)
                ).fetchone()
                prev_payload = json.loads(prev_row[0]) if prev_row else None
                changed = prev_row is None or prev_row[1] != h
                stored = "full" if changed else "ref"
                self._con.execute(
                    "INSERT INTO snapshot_items(snapshot_id, collector, stored, payload, hash) "
                    "VALUES (?, ?, ?, ?, ?)",
                    (snapshot_id, collector, stored, payload_json if changed else None, h),
                )
                if changed:
                    self._con.execute(
                        "INSERT INTO latest(collector, payload, hash, at) VALUES (?, ?, ?, ?) "
                        "ON CONFLICT(collector) DO UPDATE SET payload=excluded.payload, "
                        "hash=excluded.hash, at=excluded.at",
                        (collector, payload_json, h, at),
                    )
                    for ev in _events.diff(collector, prev_payload, payload, at):
                        self._con.execute(
                            "INSERT INTO events(at, kind, subject, before, after) VALUES (?, ?, ?, ?, ?)",
                            (
                                ev["at"],
                                ev["kind"],
                                ev["subject"],
                                json.dumps(ev["before"], ensure_ascii=False),
                                json.dumps(ev["after"], ensure_ascii=False),
                            ),
                        )
                        new_events.append(ev)
            self._con.commit()
            return new_events

    # ---------- 查询 ----------

    def events(
        self, since: float, until: float, kinds: Optional[Sequence[str]] = None
    ) -> list[dict]:
        sql = "SELECT at, kind, subject, before, after FROM events WHERE at BETWEEN ? AND ?"
        params: list = [since, until]
        if kinds:
            sql += " AND kind IN ({})".format(",".join("?" for _ in kinds))
            params.extend(kinds)
        sql += " ORDER BY at"
        with self._lock:
            rows = self._con.execute(sql, params).fetchall()
        return [
            {
                "at": at,
                "kind": kind,
                "subject": subject,
                "before": json.loads(before) if before is not None else None,
                "after": json.loads(after) if after is not None else None,
            }
            for at, kind, subject, before, after in rows
        ]

    def probes(self, target: str, since: float, until: float) -> list[dict]:
        """某个 target 的探测时序（来自 `targets` 采集器）。

        `snapshot_items` 里 `targets` 可能是 `ref`（这一 tick 跟上一次完全一样，
        极少见），这里回填到最近一条 `full` 的 payload 上再取值，保证时序不断档。
        """
        sql = """
        SELECT s.at,
               (SELECT si2.payload FROM snapshot_items si2
                WHERE si2.collector = 'targets' AND si2.stored = 'full'
                  AND si2.snapshot_id <= si.snapshot_id
                ORDER BY si2.snapshot_id DESC LIMIT 1) AS payload
        FROM snapshots s
        JOIN snapshot_items si ON si.snapshot_id = s.id
        WHERE si.collector = 'targets' AND s.at BETWEEN ? AND ?
        ORDER BY s.at
        """
        with self._lock:
            rows = self._con.execute(sql, (since, until)).fetchall()
        out: list[dict] = []
        for at, payload_json in rows:
            if payload_json is None:
                continue
            payload = json.loads(payload_json)
            for t in payload.get("targets", []):
                if t.get("name") != target:
                    continue
                probe = t.get("probe") or {}
                out.append(
                    {
                        "at": at,
                        "status": probe.get("status"),
                        "elapsed_ms": probe.get("elapsed_ms"),
                        "interface": (t.get("route") or {}).get("interface"),
                        "proxied": t.get("proxied"),
                        "error": t.get("error"),
                    }
                )
        return out

    def latest(self) -> dict:
        """每个采集器目前已知的最新值（解码后的信封 dict）。"""
        with self._lock:
            rows = self._con.execute("SELECT collector, payload FROM latest").fetchall()
        return {collector: json.loads(payload) for collector, payload in rows}

    def prune(self, older_than: float) -> None:
        """删掉 `at < older_than` 的快照历史与事件。`latest` 表不受影响。"""
        with self._lock:
            self._con.execute(
                "DELETE FROM snapshot_items WHERE snapshot_id IN "
                "(SELECT id FROM snapshots WHERE at < ?)",
                (older_than,),
            )
            self._con.execute("DELETE FROM snapshots WHERE at < ?", (older_than,))
            self._con.execute("DELETE FROM events WHERE at < ?", (older_than,))
            self._con.commit()
