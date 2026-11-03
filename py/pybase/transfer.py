"""持久化的后台传输队列 —— 提交后任务不丢，断线自动续传，续传精确到文件内部的块。

需要 extra：``pybase[db,postgres]``（任务与进度存 PostgreSQL）；SSH 端点再加 ``pybase[ssh]``。

- 任务与逐项进度存在 ``transfer_tasks`` / ``transfer_items`` 两张表（``create_schema`` 建表，
  ``transfer_chunks`` 存各块 sha256；任务与项挂 ``pybase.db`` 的行级审计，其中项上高频的进度列不进审计，块表不审计）。**库是唯一的真相**：暂停/取消直接写库，worker 每块读一次状态。
- ``TransferQueue.submit`` 立即返回 task_id；后台 worker 线程**串行**执行任务，任务内逐项传，
  每项用 ``fileops.copy_file_resumable`` 按块拷，每块写完把进度（已传字节、各块 sha256）落库。
- 断线：某项因 ``OSError`` 失败且两端有一端 ``available()`` 为假（或错误本身不像本地问题），
  任务置 ``waiting``，用 ``retry.wait_until`` 等两端恢复（间隔递增，封顶 60 秒），恢复后自动
  从断点接着传。冲突、源不存在、权限、磁盘满这类非连接问题：该项 ``failed``，任务继续下一项，
  最后任务状态如实汇总（全部成功 ``done``，有失败 ``failed``）。
- 进程重启：抢到 worker 资格时，把 ``running`` / ``waiting`` 的任务恢复为 ``queued`` 自动续传；
  ``paused`` 的保持。
- 多进程：同一个库可以有多个进程各开一个队列，用 PG 会话级 advisory lock 抢 worker 资格，
  只有一个在跑；抢不到的进程只提交/查询，并每隔几秒再试一次（worker 进程挂了会有人接手）。
- Storage 以可序列化的规格存库：``local:/abs/root``、``ssh://[user@]host[:port]/abs/root?key=钥匙文件``、
  ``smb://[user@]host[:port]/share/root``，**凭据不进库**：SSH 只存钥匙路径，SMB 的密码由
  ``pybase.smbstorage.set_credential_provider`` 注册的回调在连接时提供。
- **优先级**：``submit(..., priority=0)``，数字越大越先；同优先级先到先得（``id`` 序）。
  约定：用户主动要求的同步用最高优先级（如 100），放入用 50，后台自动同步用 0 且受空闲闸控制。
  **抢占**：worker 每写完一块（以及项与项之间、断线等待的每次探测之间）检查有没有**更高优先级**的 queued 任务，有就把当前任务
  置回 ``queued``（进度保留），去跑高优先级的；高优先级的跑完，调度自然回到它，从断点续传。
  **空闲闸**：构造参数 ``busy`` / ``idle_threshold``（缺省 0）。``priority < idle_threshold`` 的任务只在
  ``busy()`` 为 False 时才开始/继续；跑着时 ``busy()`` 变 True，写完当前块就让出（置回 ``queued``，进度保留）。
  ``busy()`` 抛异常按"繁忙"处理。``priority >= idle_threshold`` 的任务不受闸控制。
  ``set_priority(task_id, n)`` 可改未结束任务的优先级（例如用户把后台任务提为立即同步）。

状态：任务 ``queued/running/paused/waiting/done/failed/cancelled``；
项 ``pending/running/done/failed/cancelled``。
"""

from __future__ import annotations

import errno
import logging
import threading
import time
from dataclasses import dataclass
from typing import Any, Callable, Dict, Iterable, List, Optional, Sequence, Tuple, Union
from urllib.parse import parse_qs, quote, unquote, urlencode, urlsplit

from sqlalchemy import (BigInteger, Column, DateTime, Float, ForeignKey, Integer, MetaData,
                        String, Table, Text, and_, delete, func, insert, select, text, update)
from sqlalchemy.exc import SQLAlchemyError

from . import db
from .fileops import RESUME_CHUNK, ResumeState, copy_file_resumable
from .resumepart import drop_parts, resume_part_paths
from .retry import wait_until
from .storage import LocalStorage, Storage

# 供调用方引用的约定优先级（只是建议值，任意整数都行）
PRIORITY_USER = 100        # 用户主动要求的同步
PRIORITY_INGEST = 50       # 放入
PRIORITY_BACKGROUND = 0    # 后台自动同步（配合 busy / idle_threshold 受空闲闸控制）

logger = logging.getLogger(__name__)

TASK_STATES = ("queued", "running", "paused", "waiting", "done", "failed", "cancelled")
_ACTIVE = ("queued", "running", "paused", "waiting")
LOCK_NAME = "pybase_transfer_worker"
SCHEMA_LOCK_NAME = "pybase_transfer_schema"

_meta = MetaData()
tasks_table = Table(
    "transfer_tasks", _meta,
    Column("id", Integer, primary_key=True, autoincrement=True),
    Column("title", Text, nullable=False, server_default=""),
    Column("status", String(16), nullable=False, server_default="queued"),
    Column("priority", Integer, nullable=False, server_default="0"),
    Column("error", Text),
    Column("created_at", DateTime(timezone=True), nullable=False, server_default=func.now()),
    Column("updated_at", DateTime(timezone=True), nullable=False, server_default=func.now()),
)
items_table = Table(
    "transfer_items", _meta,
    Column("id", Integer, primary_key=True, autoincrement=True),
    Column("task_id", Integer, ForeignKey("transfer_tasks.id", ondelete="CASCADE"), nullable=False),
    Column("seq", Integer, nullable=False),
    Column("src_spec", Text, nullable=False),
    Column("src_path", Text, nullable=False),
    Column("dst_spec", Text, nullable=False),
    Column("dst_path", Text, nullable=False),
    Column("size", BigInteger),
    Column("mtime", Float(53)),
    Column("chunk_size", Integer),
    Column("done_bytes", BigInteger, nullable=False, server_default="0"),
    Column("status", String(16), nullable=False, server_default="pending"),
    Column("error", Text),
    Column("attempts", Integer, nullable=False, server_default="0"),
    Column("updated_at", DateTime(timezone=True), nullable=False, server_default=func.now()),
)
# 块哈希单独成表，只插入（续传回退时删超出的行）。**不挂审计**：它是纯进度数据，每块一行，
# 审计只会把同样的内容再存一遍；item 上的状态/错误/尝试次数变化照常审计。
chunks_table = Table(
    "transfer_chunks", _meta,
    Column("item_id", Integer, ForeignKey("transfer_items.id", ondelete="CASCADE"), primary_key=True),
    Column("idx", Integer, primary_key=True, autoincrement=False),
    Column("sha256", String(64), nullable=False),
)
T, I = tasks_table.c, items_table.c
C = chunks_table.c
# transfer_items 上这些列每块都会变，只改它们的 UPDATE 不进审计
_PROGRESS_COLS = ("done_bytes", "updated_at")


def create_schema(engine) -> None:
    """建表（幂等，多进程并发建也安全）并挂行级审计（审计表在 ``audit`` schema）。"""
    _require_pg(engine)
    with engine.begin() as conn:
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": SCHEMA_LOCK_NAME})
        db.create_table(conn, tasks_table)
        db.add_column(conn, "transfer_tasks", "priority", Integer, nullable=False, default=0)   # 老表补列
        db.create_table(conn, items_table)
        db.create_table(conn, chunks_table)
        db.create_index(conn, "transfer_items", ["task_id", "seq"], name="transfer_items_task_idx")
    have = set(db.audited_tables(engine))
    if not {"transfer_tasks", "transfer_items"} <= have:   # 已挂就不重装（重装要短暂锁表）
        db.install_audit(engine, ["transfer_tasks"])
        db.install_audit(engine, ["transfer_items"], ignore_columns=_PROGRESS_COLS)


def _require_pg(engine) -> None:
    if engine.dialect.name != "postgresql":
        raise ValueError("传输队列只支持 PostgreSQL（需要 advisory lock 与 jsonb）")


# ---------------------------------------------------------------- Storage 规格

def _esc(s: str) -> str:
    return s.replace("%", "%25").replace("?", "%3F").replace("#", "%23")


def storage_to_spec(storage: Storage) -> str:
    """Storage -> 可存库的字符串。支持 LocalStorage、SSHStorage 与 SMBStorage；凭据不进规格，只带钥匙文件路径。"""
    if isinstance(storage, LocalStorage):
        spec = "local:" + _esc(str(storage.root.absolute()))
        if storage.mount_point is not None:
            spec += "?" + urlencode({"mount": str(storage.mount_point)})
        return spec
    try:
        from .sshstorage import SSHStorage
    except ImportError:
        SSHStorage = None  # type: ignore[assignment]
    if SSHStorage is not None and isinstance(storage, SSHStorage):
        host = f"[{storage.host}]" if ":" in storage.host else storage.host
        auth = quote(storage.user, safe="") + "@" if storage.user else ""
        port = f":{storage.port}" if storage.port != 22 else ""
        spec = f"ssh://{auth}{host}{port}{_esc(storage.root)}"
        if storage.key_filename:
            spec += "?" + urlencode({"key": storage.key_filename})
        return spec
    try:
        from .smbstorage import SMBStorage, smb_spec
    except ImportError:
        SMBStorage = None  # type: ignore[assignment]
    if SMBStorage is not None and isinstance(storage, SMBStorage):
        return smb_spec(storage)
    raise ValueError(f"这种 Storage 不能序列化：{storage!r}")


def storage_from_spec(spec: str) -> Storage:
    if spec.startswith("local:"):
        root, _, query = spec[len("local:"):].partition("?")
        mount = parse_qs(query).get("mount", [None])[0]
        return LocalStorage(unquote(root), mount)
    if spec.startswith("ssh://"):
        from .sshstorage import SSHStorage
        u = urlsplit(spec)
        if not u.hostname:
            raise ValueError(f"ssh 规格缺主机：{spec!r}")
        key = parse_qs(u.query).get("key", [None])[0]
        return SSHStorage(u.hostname, unquote(u.path) or "/", user=unquote(u.username) if u.username else None,
                          port=u.port or 22, key_filename=key)
    if spec.startswith("smb://"):
        from .smbstorage import smb_from_spec
        return smb_from_spec(spec)
    raise ValueError(f"不认识的 Storage 规格：{spec!r}")


# ---------------------------------------------------------------- 队列

@dataclass(frozen=True)
class TransferItem:
    """提交时描述一项：`src`/`dst` 可以是 Storage 对象或规格字符串。"""

    src: Union[Storage, str]
    src_path: str
    dst: Union[Storage, str]
    dst_path: str


class TransferInterrupted(Exception):
    """进度回调里发现任务被暂停/取消，借异常中断当前拷贝。"""


# 这些 OSError 不是连接问题，重试没用
_FATAL_ERRNO = {errno.EACCES, errno.EPERM, errno.ENOENT, errno.ENOSPC, errno.EROFS, errno.EISDIR,
                errno.ENOTDIR, errno.EDQUOT, errno.ENAMETOOLONG, errno.EEXIST}
_FATAL_CLASSES = (PermissionError, FileNotFoundError, IsADirectoryError, NotADirectoryError, FileExistsError)


def _is_fatal_os(e: OSError) -> bool:
    return isinstance(e, _FATAL_CLASSES) or e.errno in _FATAL_ERRNO


class TransferQueue:
    """持久化传输队列。

    `engine`：PostgreSQL 引擎。`notifier(event: dict)`：任务状态变化与单项完成时调用
    （event 含 `type`/`task_id`/`status`…），它抛的异常会被吞掉。`start=False` 不起后台线程
    （只提交/查询）。时间参数（秒）：`idle_poll` worker 空闲时轮询库的间隔（跨进程提交靠它发现）；
    `lock_poll` 没抢到 worker 资格时重试的间隔；`wait_initial`/`wait_cap` 断线等待的起始与封顶间隔
    （每轮翻倍）；`error_retry` 库出错后的退避；`max_retries` 连接没断却反复失败时放弃的次数。
    `storage_factory(spec)` 默认 `storage_from_spec`，测试可换。`busy` / `idle_threshold`：空闲闸，
    见模块文档（`busy` 为 None 则不设闸）。
    """

    def __init__(self, engine, *, notifier: Optional[Callable[[dict], None]] = None,
                 start: bool = True, chunk: int = RESUME_CHUNK,
                 idle_poll: float = 1.0, lock_poll: float = 5.0,
                 wait_initial: float = 2.0, wait_cap: float = 60.0,
                 error_retry: float = 5.0, max_retries: int = 3,
                 storage_factory: Callable[[str], Storage] = storage_from_spec,
                 busy: Optional[Callable[[], bool]] = None, idle_threshold: int = 0):
        _require_pg(engine)
        self.engine = engine
        self.notifier = notifier
        self.chunk = chunk
        self.idle_poll, self.lock_poll = idle_poll, lock_poll
        self.wait_initial, self.wait_cap = wait_initial, wait_cap
        self.error_retry, self.max_retries = error_retry, max_retries
        self._factory = storage_factory
        self.busy, self.idle_threshold = busy, idle_threshold
        self._storages: Dict[str, Storage] = {}
        self._wake = threading.Event()
        self._closed = threading.Event()
        self._lock_conn = None
        self._need_recover = True
        self._thread: Optional[threading.Thread] = None
        create_schema(engine)
        if start:
            self.start()

    # ---- 生命周期 ----

    @property
    def is_worker(self) -> bool:
        """本实例当前是否持有 worker 资格。"""
        return self._lock_conn is not None

    def start(self) -> None:
        if self._thread is None:
            self._closed.clear()
            self._thread = threading.Thread(target=self._loop, name="transfer-worker", daemon=True)
            self._thread.start()

    def close(self) -> None:
        """停 worker（正在传的那块写完、进度落库后退出，任务保持 running，下次抢到资格会恢复）并放掉锁。"""
        self._closed.set()
        self._wake.set()
        t, self._thread = self._thread, None
        if t is not None and t is not threading.current_thread():
            t.join(timeout=30)
        self._release_lock()
        for s in self._storages.values():
            closer = getattr(s, "close", None)
            if closer:
                try:
                    closer()
                except Exception:
                    pass
        self._storages.clear()

    def __enter__(self) -> "TransferQueue":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    # ---- 对外 API ----

    def submit(self, title: str, items: Sequence[Union[TransferItem, Tuple[Any, str, Any, str]]],
               priority: int = 0) -> int:
        """提交一批文件，立即返回 task_id。源/目标只存规格，此刻不连接。
        `priority` 越大越先，同优先级先到先得；更高优先级的任务会抢占正在传的低优先级任务。"""
        rows = []
        for seq, it in enumerate(items):
            if not isinstance(it, TransferItem):
                it = TransferItem(*it)
            rows.append(dict(seq=seq, src_spec=_spec_of(it.src), src_path=it.src_path,
                             dst_spec=_spec_of(it.dst), dst_path=it.dst_path))
        if not rows:
            raise ValueError("没有要传的文件")
        with self.engine.begin() as conn:
            tid = conn.execute(insert(tasks_table).values(title=title, status="queued", priority=int(priority))
                               .returning(T.id)).scalar_one()
            conn.execute(insert(items_table), [dict(r, task_id=tid) for r in rows])
        self._emit("task", tid, "queued")
        self._wake.set()
        return tid

    def list_tasks(self, *, status: Optional[Iterable[str]] = None) -> List[Dict[str, Any]]:
        agg = (select(I.task_id, func.count().label("n_items"),
                      func.count().filter(I.status == "done").label("done_items"),
                      func.count().filter(I.status == "failed").label("failed_items"),
                      func.coalesce(func.sum(I.size), 0).label("total_bytes"),
                      func.coalesce(func.sum(I.done_bytes), 0).label("done_bytes"))
               .group_by(I.task_id).subquery())
        q = (select(tasks_table, agg.c.n_items, agg.c.done_items, agg.c.failed_items,
                    agg.c.total_bytes, agg.c.done_bytes)
             .outerjoin(agg, agg.c.task_id == T.id).order_by(T.id))
        if status:
            q = q.where(T.status.in_(list(status)))
        with self.engine.connect() as conn:
            return [dict(r._mapping) for r in conn.execute(q)]

    def task_detail(self, task_id: int) -> Optional[Dict[str, Any]]:
        with self.engine.connect() as conn:
            t = conn.execute(select(tasks_table).where(T.id == task_id)).first()
            if t is None:
                return None
            its = conn.execute(select(items_table).where(I.task_id == task_id).order_by(I.seq)).all()
        out = dict(t._mapping)
        out["items"] = [dict(r._mapping) for r in its]
        return out

    def pause(self, task_id: int) -> bool:
        """queued/running/waiting -> paused。运行中的任务在当前块写完后停下。返回是否生效。"""
        return self._set_status(task_id, "paused", ("queued", "running", "waiting"))

    def resume(self, task_id: int) -> bool:
        """paused -> queued；failed 的任务也可以重来（失败项重置为待传，已传好的块不丢）。"""
        with self.engine.begin() as conn:
            row = conn.execute(select(T.status).where(T.id == task_id).with_for_update()).first()
            if row is None or row[0] not in ("paused", "failed"):
                return False
            if row[0] == "failed":
                conn.execute(update(items_table).where(and_(I.task_id == task_id, I.status == "failed"))
                             .values(status="pending", error=None, attempts=0, updated_at=func.now()))
            conn.execute(update(tasks_table).where(T.id == task_id)
                         .values(status="queued", error=None, updated_at=func.now()))
        self._emit("task", task_id, "queued")
        self._wake.set()
        return True

    def cancel(self, task_id: int) -> bool:
        """未结束的任务 -> cancelled。运行中的在当前块写完后停下，并清掉临时文件。"""
        ok = self._set_status(task_id, "cancelled", _ACTIVE)
        if ok:
            with self.engine.begin() as conn:
                conn.execute(update(items_table).where(and_(I.task_id == task_id, I.status.in_(("pending", "running"))))
                             .values(status="cancelled", updated_at=func.now()))
        return ok

    def set_priority(self, task_id: int, priority: int) -> bool:
        """改未结束任务的优先级；返回是否生效。"""
        with self.engine.begin() as conn:
            n = conn.execute(update(tasks_table).where(and_(T.id == task_id, T.status.in_(_ACTIVE)))
                             .values(priority=int(priority), updated_at=func.now())).rowcount
        if n:
            self._wake.set()
        return bool(n)

    def _set_status(self, task_id: int, status: str, from_states: Sequence[str]) -> bool:
        with self.engine.begin() as conn:
            n = conn.execute(update(tasks_table).where(and_(T.id == task_id, T.status.in_(list(from_states))))
                             .values(status=status, updated_at=func.now())).rowcount
        if n:
            self._emit("task", task_id, status)
            self._wake.set()
        return bool(n)

    # ---- worker 资格 ----

    def _try_lock(self) -> bool:
        if self._lock_conn is not None:
            try:
                self._lock_conn.execute(text("SELECT 1"))
                self._lock_conn.commit()
                return True
            except Exception:
                logger.warning("worker 锁连接已失效，重新抢资格")
                self._release_lock()
        conn = self.engine.connect()
        try:
            got = conn.execute(text("SELECT pg_try_advisory_lock(hashtext(:k))"), {"k": LOCK_NAME}).scalar()
            conn.commit()
        except Exception:
            conn.close()
            raise
        if not got:
            conn.close()
            return False
        self._lock_conn = conn
        self._need_recover = True
        return True

    def _release_lock(self) -> None:
        conn, self._lock_conn = self._lock_conn, None
        if conn is not None:
            try:
                conn.execute(text("SELECT pg_advisory_unlock(hashtext(:k))"), {"k": LOCK_NAME})
                conn.commit()
            except Exception:
                pass
            try:
                conn.close()
            except Exception:
                pass

    # ---- worker ----

    def _loop(self) -> None:
        while not self._closed.is_set():
            try:
                if not self._try_lock():
                    self._closed.wait(self.lock_poll)
                    continue
                if self._need_recover:
                    self._recover()
                    self._need_recover = False
                got = self._claim()
                if got is None:
                    self._wake.wait(self.idle_poll)
                    self._wake.clear()
                    continue
                self._run_task(*got)
            except SQLAlchemyError:
                logger.exception("传输队列访问数据库出错，稍后重试")
                self._need_recover = True       # 手上那个任务可能还挂在 running
                self._closed.wait(self.error_retry)
            except Exception:
                logger.exception("传输 worker 异常")
                self._need_recover = True
                self._closed.wait(self.error_retry)

    def _recover(self) -> None:
        """本进程是唯一 worker 且手上没有任务：库里还挂着 running/waiting 的都是上一任留下的。"""
        with self.engine.begin() as conn:
            ids = [r[0] for r in conn.execute(
                update(tasks_table).where(T.status.in_(("running", "waiting")))
                .values(status="queued", updated_at=func.now()).returning(T.id))]
            conn.execute(update(items_table).where(I.status == "running")
                         .values(status="pending", updated_at=func.now()))
        for tid in ids:
            self._emit("task", tid, "queued", recovered=True)

    def _is_busy(self) -> bool:
        if self.busy is None:
            return False
        try:
            return bool(self.busy())
        except Exception:
            logger.debug("busy() 抛了异常，按繁忙处理", exc_info=True)
            return True

    def _claim(self) -> Optional[Tuple[int, int]]:
        """取优先级最高（同级最早）且过得了空闲闸的 queued 任务，置 running，返回 (id, priority)。"""
        q = select(T.id, T.priority).where(T.status == "queued")
        if self.busy is not None and self._is_busy():
            q = q.where(T.priority >= self.idle_threshold)
        with self.engine.begin() as conn:
            row = conn.execute(q.order_by(T.priority.desc(), T.id).limit(1)).first()
            if row is None:
                return None
            tid, prio = row
            n = conn.execute(update(tasks_table).where(and_(T.id == tid, T.status == "queued"))
                             .values(status="running", error=None, updated_at=func.now())).rowcount
        if not n:
            return None
        self._emit("task", tid, "running")
        return tid, prio

    def _should_yield(self, prio: int) -> bool:
        """当前任务该让出吗：自己被空闲闸挡住（繁忙），或有更高优先级的 queued 任务在等。"""
        busy: Optional[bool] = None
        if self.busy is not None and prio < self.idle_threshold:
            busy = self._is_busy()
            if busy:
                return True
        with self.engine.connect() as conn:
            top = conn.execute(select(func.max(T.priority)).where(T.status == "queued")).scalar()
        if top is None or top <= prio:
            return False
        if self.busy is None or top >= self.idle_threshold:
            return True
        if busy is None:
            busy = self._is_busy()
        return not busy

    def _yield_task(self, tid: int) -> None:
        self._guard_set(tid, "queued", "running")

    def _task_status(self, tid: int) -> Optional[str]:
        with self.engine.connect() as conn:
            return conn.execute(select(T.status).where(T.id == tid)).scalar()

    def _guard_set(self, tid: int, new: str, expect: str, error: Optional[str] = None) -> bool:
        """只在任务仍是 `expect` 状态时改（用户在中间暂停/取消了就别覆盖）。"""
        with self.engine.begin() as conn:
            n = conn.execute(update(tasks_table).where(and_(T.id == tid, T.status == expect))
                             .values(status=new, error=error, updated_at=func.now())).rowcount
        if n:
            self._emit("task", tid, new, error=error)
        return bool(n)

    def _storage(self, spec: str) -> Storage:
        s = self._storages.get(spec)
        if s is None:
            s = self._storages[spec] = self._factory(spec)
        return s

    def _run_task(self, tid: int, prio: int = 0) -> None:
        with self.engine.connect() as conn:
            items = conn.execute(select(items_table).where(and_(I.task_id == tid, I.status.in_(("pending", "running"))))
                                 .order_by(I.seq)).all()
        for row in items:
            if self._closed.is_set():
                return
            if self._should_yield(prio):
                self._yield_task(tid)
                return
            outcome = self._run_item(tid, dict(row._mapping), prio)
            if outcome == "stopped":
                return
        with self.engine.connect() as conn:
            failed = conn.execute(select(func.count()).where(and_(I.task_id == tid, I.status == "failed"))).scalar()
        if failed:
            self._guard_set(tid, "failed", "running", f"{failed} 项失败")
        else:
            self._guard_set(tid, "done", "running")

    def _item_update(self, item_id: int, **values) -> None:
        with self.engine.begin() as conn:
            conn.execute(update(items_table).where(I.id == item_id).values(updated_at=func.now(), **values))

    def _run_item(self, tid: int, item: dict, prio: int = 0) -> str:
        """传一项，返回 'done' / 'failed' / 'stopped'（任务被暂停/取消/关闭/让出）。连接问题在这里循环等待，不返回。"""
        iid = item["id"]
        src_spec, dst_spec = item["src_spec"], item["dst_spec"]
        parts = resume_part_paths(item["dst_path"])
        with self.engine.connect() as conn:
            shas = [r[0] for r in conn.execute(
                select(C.sha256).where(C.item_id == iid).order_by(C.idx))]
        state = ResumeState(size=item["size"], mtime=item["mtime"],
                            chunk=item["chunk_size"] or self.chunk, chunks=shas)
        retries = 0

        def stop_now() -> bool:
            return self._closed.is_set() or self._task_status(tid) != "running"

        def progress(done: int, idx: int, sha: str) -> None:
            vals = dict(done_bytes=done, updated_at=func.now())
            if idx == 0:                # 从头开始：记下源的快照（只此一次，进度更新不碰这些列）
                vals.update(size=state.size, mtime=state.mtime, chunk_size=state.chunk)
            with self.engine.begin() as conn:
                conn.execute(delete(chunks_table).where(and_(C.item_id == iid, C.idx >= idx)))
                conn.execute(insert(chunks_table).values(item_id=iid, idx=idx, sha256=sha))
                conn.execute(update(items_table).where(I.id == iid).values(**vals))
                status = conn.execute(select(T.status).where(T.id == tid)).scalar()
            if self._closed.is_set() or status != "running":
                raise TransferInterrupted()
            if self._should_yield(prio):
                raise _Yield()

        while True:
            if stop_now():
                return self._stopped(tid, iid)
            try:
                src, dst = self._storage(src_spec), self._storage(dst_spec)
                if not (src.available() and dst.available()):
                    raise _Disconnected()
                self._item_update(iid, status="running", attempts=item["attempts"] + 1)
                item["attempts"] += 1
                res = copy_file_resumable(src, item["src_path"], dst, item["dst_path"],
                                          part_path=parts[0], part_fallbacks=parts[1:],
                                          chunk=state.chunk, progress=progress, state=state)
            except TransferInterrupted:
                return self._stopped(tid, iid)
            except _Yield:
                self._yield_task(tid)           # 先置回 queued，项随后回 pending（进度已落库）
                return self._stopped(tid, iid)
            except SQLAlchemyError:
                raise
            except _Disconnected:
                if self._wait_for_connection(tid, src_spec, dst_spec, prio) == "stopped":
                    return self._stopped(tid, iid)
                continue
            except OSError as e:
                if _is_fatal_os(e):
                    return self._fail_item(iid, f"{type(e).__name__}: {e}")
                if not self._both_available(src_spec, dst_spec):
                    if self._wait_for_connection(tid, src_spec, dst_spec, prio) == "stopped":
                        return self._stopped(tid, iid)
                    continue
                retries += 1
                if retries > self.max_retries:
                    return self._fail_item(iid, f"{type(e).__name__}: {e}")
                self._closed.wait(min(self.error_retry, 1.0 * retries))
                continue
            except Exception as e:      # noqa: BLE001  ValueError（不安全路径）等：重试没用
                return self._fail_item(iid, f"{type(e).__name__}: {e}")

            if res.ok:
                self._item_update(iid, status="done", error=None, done_bytes=res.size, size=res.size)
                self._emit("item", tid, "done", item_id=iid, dst_path=item["dst_path"])
                return "done"
            if res.status == "conflict":
                return self._fail_item(iid, "目标已存在且内容不同，未覆盖")
            if res.status == "source-missing":
                return self._fail_item(iid, "源文件不存在")
            # copy-differs / source-changed：进度已清零，整项重来几次
            retries += 1
            if retries > self.max_retries:
                return self._fail_item(iid, f"反复失败：{res.status}")
            with self.engine.begin() as conn:
                conn.execute(delete(chunks_table).where(C.item_id == iid))
            self._item_update(iid, done_bytes=0)

    def _fail_item(self, iid: int, msg: str) -> str:
        self._item_update(iid, status="failed", error=msg)
        with self.engine.connect() as conn:
            tid = conn.execute(select(I.task_id).where(I.id == iid)).scalar()
        self._emit("item", tid, "failed", item_id=iid, error=msg)
        return "failed"

    def _stopped(self, tid: int, iid: int) -> str:
        """被暂停/取消/关闭：项回到待传（取消则标取消并清临时文件）。"""
        status = self._task_status(tid)
        with self.engine.connect() as conn:
            row = conn.execute(select(I.dst_spec, I.dst_path).where(I.id == iid)).first()
        if status == "cancelled":
            self._item_update(iid, status="cancelled")
            if row is not None:
                try:
                    dst = self._storage(row[0])
                    drop_parts(dst, resume_part_paths(row[1]))
                except Exception:
                    logger.debug("清理临时文件失败", exc_info=True)
        else:
            self._item_update(iid, status="pending")
        return "stopped"

    def _both_available(self, *specs: str) -> bool:
        try:
            return all(self._storage(s).available() for s in specs)
        except Exception:
            return False

    def _wait_for_connection(self, tid: int, src_spec: str, dst_spec: str, prio: int = 0) -> str:
        """任务置 waiting，等两端都回来（间隔递增封顶）；回来返回 'ok'，被暂停/取消/关闭返回 'stopped'。
        等的期间有更高优先级的 queued 任务（或自己被空闲闸挡住）就让出：置回 ``queued``（进度保留）、返回 'stopped'，
        worker 去跑别的——否则一个连不上的任务会把整个串行队列堵死。"""
        if not self._guard_set(tid, "waiting", "running"):
            return "stopped"
        interrupted = False

        def ready() -> bool:
            nonlocal interrupted
            if self._closed.is_set() or self._task_status(tid) != "waiting":
                interrupted = True
                return True
            if self._should_yield(prio) and self._guard_set(tid, "queued", "waiting"):
                interrupted = True
                return True
            return self._both_available(src_spec, dst_spec)

        def sleep(sec: float) -> None:
            self._wake.wait(sec)
            self._wake.clear()

        delay = self.wait_initial
        while not wait_until(ready, delay, timeout=delay, sleep=sleep):
            delay = min(delay * 2, self.wait_cap)
        if interrupted:
            return "stopped"
        return "ok" if self._guard_set(tid, "running", "waiting") else "stopped"

    # ---- 通知 ----

    def _emit(self, kind: str, task_id: int, status: str, **info) -> None:
        if self.notifier is None:
            return
        try:
            self.notifier({"type": kind, "task_id": task_id, "status": status, "time": time.time(), **info})
        except Exception:
            logger.debug("notifier 抛了异常，已忽略", exc_info=True)


class _Yield(Exception):
    """内部：有更高优先级任务或空闲闸关了，当前任务让出（进度保留）。"""


class _Disconnected(Exception):
    """内部：开传前发现某端不可用。"""


def _spec_of(x: Union[Storage, str]) -> str:
    if isinstance(x, str):
        return x
    return storage_to_spec(x)
