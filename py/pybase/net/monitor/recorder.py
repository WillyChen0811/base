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

`scenario` vs `scene`：`scenario`（见 `scenario.py`）是程序从快照自动识别出的网络
组合（`key`/`label`），本文件不动它。`scene` 是用户自己命名、手动进入/退出的
"使用场景"（比如"公司"“咖啡馆"），带一份调用方采集好的配置快照，用于按人的
主观场景而不是自动识别的组合去分组监测数据——两者独立，互不依赖。
"""
from __future__ import annotations

import hashlib
import json
import sqlite3
import statistics
import threading
import time
from typing import Optional, Sequence

from . import events as _events

# `source` 是"这条观测来自哪台设备/哪个程序"的预留维度：本机监测恒为 `'local'`，
# 以后服务部署到路由器上按"设备+程序"管控时，同一张表能直接按 source 区分不同
# 来源，不用改表结构。列默认值保证旧调用方（不传 source）写入的行仍然可查。
_DEFAULT_SOURCE = "local"

_SCHEMA = """
CREATE TABLE IF NOT EXISTS snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at REAL NOT NULL,
    source TEXT NOT NULL DEFAULT 'local',
    scene_id INTEGER
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
    after TEXT,
    source TEXT NOT NULL DEFAULT 'local'
);
CREATE INDEX IF NOT EXISTS idx_events_at ON events(at);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind);

CREATE TABLE IF NOT EXISTS annotations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at REAL NOT NULL,
    source TEXT NOT NULL DEFAULT 'local',
    text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_annotations_at ON annotations(at);

CREATE TABLE IF NOT EXISTS scenes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    name_norm TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'local',
    created_at REAL NOT NULL,
    deleted INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_scenes_source_name ON scenes(source, name_norm) WHERE deleted = 0;

CREATE TABLE IF NOT EXISTS scene_snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scene_id INTEGER NOT NULL,
    at REAL NOT NULL,
    payload TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_scene_snapshots_scene ON scene_snapshots(scene_id, at);

CREATE TABLE IF NOT EXISTS scene_periods (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scene_id INTEGER NOT NULL,
    source TEXT NOT NULL DEFAULT 'local',
    start_at REAL NOT NULL,
    end_at REAL
);
CREATE INDEX IF NOT EXISTS idx_scene_periods_start ON scene_periods(start_at);
CREATE INDEX IF NOT EXISTS idx_scene_periods_open ON scene_periods(source, end_at);

CREATE TABLE IF NOT EXISTS scene_settings (
    scene_id INTEGER PRIMARY KEY,
    updated_at REAL NOT NULL,
    payload TEXT NOT NULL
);
"""

_SCHEMA_VERSION = 1


def _hash_of(payload_json: str) -> str:
    return hashlib.sha256(payload_json.encode("utf-8")).hexdigest()


def _norm_name(name: str) -> str:
    return name.strip().casefold()


class Recorder:
    """网络监测快照的记录与查询。`path` 是 SQLite 文件路径，`:memory:` 可用于测试。"""

    def __init__(self, path: str):
        self._con = sqlite3.connect(path, check_same_thread=False)
        self._con.execute("PRAGMA journal_mode=WAL")
        self._lock = threading.RLock()
        with self._lock:
            self._con.executescript(_SCHEMA)
            self._migrate()
            self._con.commit()

    def _migrate(self) -> None:
        """简单的 schema 版本迁移：`PRAGMA user_version` 记版本号，版本落后才补
        旧库缺的列（新建库走 `_SCHEMA` 已经带全，这里检查后直接跳过、不会重复
        `ALTER`）。新增表都用 `CREATE TABLE IF NOT EXISTS`，不需要走这里。
        """
        version = self._con.execute("PRAGMA user_version").fetchone()[0]
        if version < 1:
            cols = {row[1] for row in self._con.execute("PRAGMA table_info(snapshots)")}
            if "scene_id" not in cols:
                self._con.execute("ALTER TABLE snapshots ADD COLUMN scene_id INTEGER")
            version = 1
        self._con.execute(f"PRAGMA user_version = {_SCHEMA_VERSION}")

    def __enter__(self) -> "Recorder":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def close(self) -> None:
        with self._lock:
            self._con.close()

    # ---------- 写入 ----------

    def record(self, snapshot: dict, *, at: float, source: str = _DEFAULT_SOURCE) -> list[dict]:
        """存一轮快照（`{collector名: 采集器返回的信封dict}`），返回这一轮检测到的事件。

        `snapshot` 里的每个值必须是可 JSON 序列化的（采集器的约定本就如此）。
        `source` 标记这一轮观测来自哪（本机监测传默认值 `"local"` 即可）；只影响
        `snapshots`/`events` 表里的标签，不参与去重/`latest` 基准的计算——同一份
        `latest` 缓存目前是跨 source 共享的单一"当前值"，多来源合并展示是调用方
        以后的事。
        """
        with self._lock:
            scene_row = self._con.execute(
                "SELECT scene_id FROM scene_periods WHERE source = ? AND end_at IS NULL "
                "ORDER BY start_at DESC LIMIT 1",
                (source,),
            ).fetchone()
            scene_id = scene_row[0] if scene_row else None
            cur = self._con.execute(
                "INSERT INTO snapshots(at, source, scene_id) VALUES (?, ?, ?)", (at, source, scene_id)
            )
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
                            "INSERT INTO events(at, kind, subject, before, after, source) "
                            "VALUES (?, ?, ?, ?, ?, ?)",
                            (
                                ev["at"],
                                ev["kind"],
                                ev["subject"],
                                json.dumps(ev["before"], ensure_ascii=False),
                                json.dumps(ev["after"], ensure_ascii=False),
                                source,
                            ),
                        )
                        new_events.append(ev)
            self._con.commit()
            return new_events

    # ---------- 查询 ----------

    def events(
        self,
        since: float,
        until: float,
        kinds: Optional[Sequence[str]] = None,
        *,
        source: Optional[str] = None,
    ) -> list[dict]:
        sql = "SELECT at, kind, subject, before, after, source FROM events WHERE at BETWEEN ? AND ?"
        params: list = [since, until]
        if kinds:
            sql += " AND kind IN ({})".format(",".join("?" for _ in kinds))
            params.extend(kinds)
        if source:
            sql += " AND source = ?"
            params.append(source)
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
                "source": src,
            }
            for at, kind, subject, before, after, src in rows
        ]

    def probes(
        self, target: str, since: float, until: float, *, scene_id: Optional[int] = None
    ) -> list[dict]:
        """某个 target 的探测时序（来自 `targets` 采集器）。

        `snapshot_items` 里 `targets` 可能是 `ref`（这一 tick 跟上一次完全一样，
        极少见），这里回填到最近一条 `full` 的 payload 上再取值，保证时序不断档。
        `scene_id` 给了就只看那个用户命名场景下的观测（对应 `record()` 当时打的标）。
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
        """
        params: list = [since, until]
        if scene_id is not None:
            sql += " AND s.scene_id = ?"
            params.append(scene_id)
        sql += " ORDER BY s.at"
        with self._lock:
            rows = self._con.execute(sql, params).fetchall()
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
                        "via": t.get("via"),
                        "error": t.get("error"),
                    }
                )
        return out

    def scenarios(self, since: float, until: float) -> list[dict]:
        """`[since, until]` 范围内的组合分段：每段的 `key`/`label`/起止时间/持续时长。

        分段边界来自 `scenario` 这个"采集器"条目的变化点（`stored='full'` 的行，
        即 `summarize_scenario` 算出的 key 真的变了那一刻）——不是重新对比快照，
        复用 Recorder 自己的去重机制。`until` 之外查不到"下一次变化"就用 `until`
        本身收尾（当前仍在这个组合里）。
        """
        sql = """
        SELECT s.at, si.payload FROM snapshot_items si
        JOIN snapshots s ON s.id = si.snapshot_id
        WHERE si.collector = 'scenario' AND si.stored = 'full' AND s.at <= ?
        ORDER BY s.at
        """
        with self._lock:
            rows = self._con.execute(sql, (until,)).fetchall()
        changes = [(at, json.loads(payload)) for at, payload in rows if payload is not None]
        segments: list[dict] = []
        for i, (at, payload) in enumerate(changes):
            start = at
            end = changes[i + 1][0] if i + 1 < len(changes) else until
            if end <= since:
                continue
            seg_start, seg_end = max(start, since), min(end, until)
            if seg_end < seg_start:
                continue
            segments.append({
                "key": payload.get("key"),
                "label": payload.get("label"),
                "start": seg_start,
                "end": seg_end,
                "duration": seg_end - seg_start,
            })
        return segments

    def probes_by_scenario(self, target: str, since: float, until: float) -> list[dict]:
        """某个 target 按场景分组的聚合：每段组合下的成功率、延迟中位数/P90、via 分布。

        分组边界用 `scenarios()`；探测数据用 `probes()`（已含 `via`）。段与段
        之间左闭右开，最后一段右边界闭合，避免恰好落在 `until` 上的探测点丢失。
        """
        segments = self.scenarios(since, until)
        raw_probes = self.probes(target, since, until)
        out: list[dict] = []
        for idx, seg in enumerate(segments):
            is_last = idx == len(segments) - 1
            in_seg = [
                p for p in raw_probes
                if seg["start"] <= p["at"] < seg["end"] or (is_last and p["at"] == seg["end"])
            ]
            total = len(in_seg)
            ok = [p for p in in_seg if p["status"] == "ok"]
            elapsed = sorted(p["elapsed_ms"] for p in ok if p.get("elapsed_ms") is not None)
            via_counts: dict[str, int] = {}
            for p in in_seg:
                v = p.get("via") or "unknown"
                via_counts[v] = via_counts.get(v, 0) + 1
            out.append({
                "key": seg["key"],
                "label": seg["label"],
                "start": seg["start"],
                "end": seg["end"],
                "duration": seg["duration"],
                "total": total,
                "success_rate": (len(ok) / total) if total else None,
                "latency_median_ms": statistics.median(elapsed) if elapsed else None,
                "latency_p90_ms": elapsed[min(len(elapsed) - 1, int(len(elapsed) * 0.9))] if elapsed else None,
                "via": via_counts,
            })
        return out

    def add_events(self, events: list[dict]) -> None:
        """直接写入一批事件（如 `watch.to_recorder_event` 的实时通知），不经快照比较。
        每条需有 at/kind/subject，before/after 可缺省，source 缺省为本机。"""
        rows = [
            (ev["at"], ev["kind"], ev["subject"],
             json.dumps(ev.get("before"), ensure_ascii=False),
             json.dumps(ev.get("after"), ensure_ascii=False),
             ev.get("source") or _DEFAULT_SOURCE)
            for ev in events
        ]
        with self._lock:
            self._con.executemany(
                "INSERT INTO events(at, kind, subject, before, after, source) VALUES (?, ?, ?, ?, ?, ?)",
                rows,
            )
            self._con.commit()

    def annotate(self, at: float, text: str, *, source: str = _DEFAULT_SOURCE) -> None:
        """手动备注（"在公司，开了 WG-B" 这类），跟自动采集完全独立的旁路记录。"""
        with self._lock:
            self._con.execute(
                "INSERT INTO annotations(at, source, text) VALUES (?, ?, ?)", (at, source, text)
            )
            self._con.commit()

    def annotations(self, since: float, until: float) -> list[dict]:
        with self._lock:
            rows = self._con.execute(
                "SELECT at, source, text FROM annotations WHERE at BETWEEN ? AND ? ORDER BY at",
                (since, until),
            ).fetchall()
        return [{"at": at, "source": source, "text": text} for at, source, text in rows]

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

    # ---------- 用户命名场景（scene，见类型区别说明） ----------

    def create_scene(
        self, name: str, *, note: str = "", snapshot: dict, at: float, source: str = _DEFAULT_SOURCE
    ) -> int:
        """新建一个用户命名场景，并存下第一份配置快照。`name` 按
        `strip().casefold()` 规范化后在同一个 `source` 内唯一，重名抛 `ValueError`
        （由 `scenes` 表上的部分唯一索引兜底，防并发重名）。返回场景 id。
        """
        name = name.strip()
        if not name:
            raise ValueError("场景名不能为空")
        name_norm = _norm_name(name)
        with self._lock:
            existing = self._con.execute(
                "SELECT id FROM scenes WHERE source = ? AND name_norm = ? AND deleted = 0",
                (source, name_norm),
            ).fetchone()
            if existing is not None:
                raise ValueError(f"场景名已存在：{name!r}")
            try:
                cur = self._con.execute(
                    "INSERT INTO scenes(name, name_norm, note, source, created_at, deleted) "
                    "VALUES (?, ?, ?, ?, ?, 0)",
                    (name, name_norm, note, source, at),
                )
            except sqlite3.IntegrityError as exc:
                raise ValueError(f"场景名已存在：{name!r}") from exc
            scene_id = cur.lastrowid
            self._con.execute(
                "INSERT INTO scene_snapshots(scene_id, at, payload) VALUES (?, ?, ?)",
                (scene_id, at, json.dumps(snapshot, ensure_ascii=False)),
            )
            self._con.commit()
            return scene_id

    def capture_scene(self, scene_id: int, *, snapshot: dict, at: float) -> int:
        """给已有场景追加一份配置快照（历史保留，不覆盖旧的）。返回快照 id。"""
        with self._lock:
            self._require_scene(scene_id)
            cur = self._con.execute(
                "INSERT INTO scene_snapshots(scene_id, at, payload) VALUES (?, ?, ?)",
                (scene_id, at, json.dumps(snapshot, ensure_ascii=False)),
            )
            self._con.commit()
            return cur.lastrowid

    def _require_scene(self, scene_id: int) -> None:
        row = self._con.execute("SELECT id FROM scenes WHERE id = ?", (scene_id,)).fetchone()
        if row is None:
            raise ValueError(f"场景不存在：{scene_id}")

    def scene_snapshots(self, scene_id: int, *, include_data: bool = False) -> list[dict]:
        """某个场景的历史快照列表，按时间升序。`include_data=False`（默认）只给
        `id`/`at`/`size`（JSON 字符串长度）这份摘要，避免大快照把结果拖得很大；
        要完整内容传 `include_data=True` 或单独调 `scene_snapshot(id)`。
        """
        with self._lock:
            rows = self._con.execute(
                "SELECT id, at, payload FROM scene_snapshots WHERE scene_id = ? ORDER BY at",
                (scene_id,),
            ).fetchall()
        out = []
        for sid, at, payload_json in rows:
            item = {"id": sid, "at": at}
            if include_data:
                item["data"] = json.loads(payload_json)
            else:
                item["size"] = len(payload_json)
            out.append(item)
        return out

    def scene_snapshot(self, snapshot_id: int) -> dict:
        """单条快照的完整内容：`id`/`scene_id`/`at`/`data`。查不到抛 `ValueError`。"""
        with self._lock:
            row = self._con.execute(
                "SELECT id, scene_id, at, payload FROM scene_snapshots WHERE id = ?", (snapshot_id,)
            ).fetchone()
        if row is None:
            raise ValueError(f"快照不存在：{snapshot_id}")
        sid, scene_id, at, payload_json = row
        return {"id": sid, "scene_id": scene_id, "at": at, "data": json.loads(payload_json)}

    def enter_scene(self, scene_id: int, *, at: float, source: str = _DEFAULT_SOURCE) -> None:
        """进入场景：结束同一个 `source` 上一个仍打开的场景区间（如果有），再开一个
        新区间。同一时刻每个 `source` 只有一个"当前场景"。区间落库，重开 `Recorder`
        后 `current_scene()` 仍能读到——不需要额外的进程内状态。
        """
        with self._lock:
            self._require_scene(scene_id)
            self._close_open_period(source, at)
            self._con.execute(
                "INSERT INTO scene_periods(scene_id, source, start_at, end_at) VALUES (?, ?, ?, NULL)",
                (scene_id, source, at),
            )
            self._con.commit()

    def leave_scene(self, *, at: float, source: str = _DEFAULT_SOURCE) -> None:
        """结束当前场景（如果有）；没有打开的场景区间就什么也不做。"""
        with self._lock:
            self._close_open_period(source, at)
            self._con.commit()

    def _close_open_period(self, source: str, at: float) -> None:
        self._con.execute(
            "UPDATE scene_periods SET end_at = ? WHERE source = ? AND end_at IS NULL", (at, source)
        )

    def current_scene(self, *, source: str = _DEFAULT_SOURCE) -> Optional[dict]:
        """当前处于哪个场景（`source` 维度独立），没有就返回 `None`。"""
        with self._lock:
            row = self._con.execute(
                "SELECT sp.scene_id, sp.start_at, sc.name, sc.note, sc.deleted "
                "FROM scene_periods sp JOIN scenes sc ON sc.id = sp.scene_id "
                "WHERE sp.source = ? AND sp.end_at IS NULL "
                "ORDER BY sp.start_at DESC LIMIT 1",
                (source,),
            ).fetchone()
        if row is None:
            return None
        scene_id, since, name, note, deleted = row
        return {
            "scene_id": scene_id, "name": name, "note": note, "since": since,
            "source": source, "deleted": bool(deleted),
        }

    def scenes(self, *, source: Optional[str] = None, include_deleted: bool = False, until: Optional[float] = None) -> list[dict]:
        """场景列表：`id`/`name`/`note`/`source`/快照数/首末快照时间/累计处于该场景的
        时长（按 `scene_periods` 区间求和；仍打开的区间用 `until` —— 默认当前
        时间 —— 收尾）。默认不列出已删除场景，传 `include_deleted=True` 看全部。
        """
        until = time.time() if until is None else until
        sql = "SELECT id, name, note, source, deleted FROM scenes"
        conds, params = [], []
        if source is not None:
            conds.append("source = ?")
            params.append(source)
        if not include_deleted:
            conds.append("deleted = 0")
        if conds:
            sql += " WHERE " + " AND ".join(conds)
        sql += " ORDER BY id"
        with self._lock:
            rows = self._con.execute(sql, params).fetchall()
            out = []
            for scene_id, name, note, scene_source, deleted in rows:
                snap_row = self._con.execute(
                    "SELECT COUNT(*), MIN(at), MAX(at) FROM scene_snapshots WHERE scene_id = ?",
                    (scene_id,),
                ).fetchone()
                count, first_at, last_at = snap_row
                periods = self._con.execute(
                    "SELECT start_at, end_at FROM scene_periods WHERE scene_id = ? ORDER BY start_at",
                    (scene_id,),
                ).fetchall()
                duration = sum((end if end is not None else until) - start for start, end in periods)
                out.append({
                    "id": scene_id, "name": name, "note": note, "source": scene_source,
                    "deleted": bool(deleted), "snapshot_count": count,
                    "first_snapshot_at": first_at, "last_snapshot_at": last_at,
                    "duration": duration,
                })
        return out

    def rename_scene(self, scene_id: int, new_name: str) -> None:
        """改名，唯一性规则跟 `create_scene` 一样（同 `source` 内规范化后不重复）。"""
        new_name = new_name.strip()
        if not new_name:
            raise ValueError("场景名不能为空")
        name_norm = _norm_name(new_name)
        with self._lock:
            row = self._con.execute("SELECT source FROM scenes WHERE id = ?", (scene_id,)).fetchone()
            if row is None:
                raise ValueError(f"场景不存在：{scene_id}")
            source = row[0]
            dup = self._con.execute(
                "SELECT id FROM scenes WHERE source = ? AND name_norm = ? AND deleted = 0 AND id != ?",
                (source, name_norm, scene_id),
            ).fetchone()
            if dup is not None:
                raise ValueError(f"场景名已存在：{new_name!r}")
            self._con.execute(
                "UPDATE scenes SET name = ?, name_norm = ? WHERE id = ?", (new_name, name_norm, scene_id)
            )
            self._con.commit()

    def set_scene_settings(self, scene_id: int, settings: dict, *, at: float) -> None:
        """整体替换某场景的设置（内容由调用方定义，例如进入该场景时额外启用的监测目标）。"""
        payload = json.dumps(settings, ensure_ascii=False, sort_keys=True)
        with self._lock:
            if self._con.execute("SELECT 1 FROM scenes WHERE id = ?", (scene_id,)).fetchone() is None:
                raise ValueError(f"场景不存在：{scene_id}")
            self._con.execute(
                "INSERT INTO scene_settings (scene_id, updated_at, payload) VALUES (?, ?, ?) "
                "ON CONFLICT(scene_id) DO UPDATE SET updated_at = excluded.updated_at, payload = excluded.payload",
                (scene_id, at, payload),
            )
            self._con.commit()

    def scene_settings(self, scene_id: int) -> dict:
        """某场景的设置；没设过返回空 dict。"""
        with self._lock:
            row = self._con.execute(
                "SELECT payload FROM scene_settings WHERE scene_id = ?", (scene_id,)
            ).fetchone()
        return json.loads(row[0]) if row else {}

    def set_scene_note(self, scene_id: int, note: str) -> None:
        """改备注（可为空串）。"""
        with self._lock:
            cur = self._con.execute("UPDATE scenes SET note = ? WHERE id = ?", (note, scene_id))
            if cur.rowcount == 0:
                raise ValueError(f"场景不存在：{scene_id}")
            self._con.commit()

    def delete_scene(self, scene_id: int, *, at: Optional[float] = None) -> None:
        """删除场景：标记 `deleted=1`（软删），场景本身的快照/区间都保留，历史
        观测（`snapshots.scene_id`）也保留原编号不置空——置空会让"这条观测当时
        在哪个场景"的历史断链，标记删除既能在 `scenes()` 里默认隐藏，又不影响
        `scene_periods`/`probes(scene_id=...)` 回看旧数据。

        `at` 给了就顺手关闭这个场景当前还打开着的区间（避免删除后区间永远开着）；
        不给就不动区间，调用方自己决定要不要先 `leave_scene`。
        """
        with self._lock:
            self._require_scene(scene_id)
            self._con.execute("UPDATE scenes SET deleted = 1 WHERE id = ?", (scene_id,))
            if at is not None:
                self._con.execute(
                    "UPDATE scene_periods SET end_at = ? WHERE scene_id = ? AND end_at IS NULL",
                    (at, scene_id),
                )
            self._con.commit()

    def scene_periods(self, since: float, until: float, *, source: Optional[str] = None) -> list[dict]:
        """`[since, until]` 范围内的场景分段：每段处于哪个场景、起止、时长。跟
        `scenarios()`（自动识别组合）是平行的两张表，互不影响。仍打开的区间用
        `until` 收尾。
        """
        sql = (
            "SELECT sp.scene_id, sc.name, sp.source, sp.start_at, sp.end_at, sc.deleted "
            "FROM scene_periods sp JOIN scenes sc ON sc.id = sp.scene_id "
            "WHERE sp.start_at <= ? AND (sp.end_at IS NULL OR sp.end_at >= ?)"
        )
        params: list = [until, since]
        if source is not None:
            sql += " AND sp.source = ?"
            params.append(source)
        sql += " ORDER BY sp.start_at"
        with self._lock:
            rows = self._con.execute(sql, params).fetchall()
        out = []
        for scene_id, name, src, start_at, end_at, deleted in rows:
            end = until if end_at is None else end_at
            seg_start, seg_end = max(start_at, since), min(end, until)
            if seg_end < seg_start:
                continue
            out.append({
                "scene_id": scene_id, "name": name, "source": src, "deleted": bool(deleted),
                "start": seg_start, "end": seg_end, "duration": seg_end - seg_start,
            })
        return out
