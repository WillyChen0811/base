"""pybase.transfer：持久化传输队列（PostgreSQL；没有本机 PG 时 skip）。"""

import os
import sys
import time

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
pytest.importorskip("sqlalchemy")

from sqlalchemy import text  # noqa: E402

from pybase import db  # noqa: E402
from pybase.storage import LocalStorage  # noqa: E402
from pybase.transfer import (TransferQueue, storage_from_spec, storage_to_spec)  # noqa: E402
from test_db import pg_database  # noqa: E402,F401

CH = 8
DATA = os.urandom(200)          # 25 块


class Flaky(LocalStorage):
    """可开关的本地存储：`up=False` 时 available 为假、所有操作抛 OSError；
    `cut_after` 写够这么多字节就"断线"；`delay` 每次写睡一下（给暂停留窗口）。"""

    def __init__(self, root):
        super().__init__(root)
        self.up, self.cut_after, self.delay = True, None, 0.0
        self.written = 0
        self.seeks = []

    def available(self):
        return self.up and super().available()

    def _chk(self):
        if not self.up:
            raise ConnectionError("断线")

    def stat(self, path):
        self._chk()
        return super().stat(path)

    def open_read(self, path):
        self._chk()
        f = super().open_read(path)
        orig = f.seek
        f.seek = lambda *a: (self.seeks.append(a[0]), orig(*a))[1]
        return f

    def open_resume(self, path, offset):
        self._chk()
        return _W(self, super().open_resume(path, offset))

    def replace(self, a, b):
        self._chk()
        super().replace(a, b)


class _W:
    def __init__(self, owner, f):
        self._o, self._f = owner, f

    def write(self, b):
        o = self._o
        if o.cut_after is not None and o.written + len(b) > o.cut_after:
            o.up = False
            o.cut_after = None
            raise ConnectionError("写到一半断了")
        if o.delay:
            time.sleep(o.delay)
        o.written += len(b)
        return self._f.write(b)

    def __getattr__(self, n):
        return getattr(self._f, n)

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self._f.close()


def until(fn, timeout=20.0):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        v = fn()
        if v:
            return v
        time.sleep(0.02)
    raise AssertionError("等待超时")


@pytest.fixture
def pg(pg_database):  # noqa: F811
    eng = db.make_engine(pg_database, application_name="xfer-test", pool_size=10)
    with eng.begin() as c:
        c.execute(text("DROP SCHEMA public CASCADE"))
        c.execute(text("DROP SCHEMA IF EXISTS audit CASCADE"))
        c.execute(text("CREATE SCHEMA public"))
    yield eng
    eng.dispose()


@pytest.fixture
def env(pg, tmp_path):
    (tmp_path / "s").mkdir()
    (tmp_path / "d").mkdir()
    src, dst = Flaky(tmp_path / "s"), Flaky(tmp_path / "d")
    reg = {storage_to_spec(src): src, storage_to_spec(dst): dst}
    queues = []

    def make(**kw):
        kw.setdefault("chunk", CH)
        kw.setdefault("idle_poll", 0.05)
        kw.setdefault("lock_poll", 0.1)
        kw.setdefault("wait_initial", 0.05)
        kw.setdefault("wait_cap", 0.2)
        kw.setdefault("error_retry", 0.1)
        q = TransferQueue(pg, storage_factory=lambda spec: reg[spec], **kw)
        queues.append(q)
        return q
    yield make, src, dst, tmp_path, pg
    for q in queues:
        q.close()


def n_chunks(pg, item_id):
    with pg.connect() as c:
        return c.execute(text("SELECT count(*) FROM transfer_chunks WHERE item_id=:i"), {"i": item_id}).scalar()


def status(q, tid):
    return q.task_detail(tid)["status"]


def test_规格往返_凭据不进库(tmp_path):
    s = storage_from_spec(storage_to_spec(LocalStorage(tmp_path / "含?号 目录")))
    assert isinstance(s, LocalStorage) and s.root == tmp_path / "含?号 目录"
    paramiko = pytest.importorskip("paramiko")  # noqa: F841
    from pybase.sshstorage import SSHStorage
    spec = storage_to_spec(SSHStorage("h.example", "/data/x", user="bob", port=2222, key_filename="/k/id"))
    assert spec == "ssh://bob@h.example:2222/data/x?key=%2Fk%2Fid"
    r = storage_from_spec(spec)
    assert (r.host, r.root, r.user, r.port, r.key_filename) == ("h.example", "/data/x", "bob", 2222, "/k/id")
    assert storage_to_spec(SSHStorage("h", "/r")) == "ssh://h/r"
    with pytest.raises(ValueError):
        storage_to_spec(object())
    with pytest.raises(ValueError):
        storage_from_spec("smb://x")


def test_基本流程_提交立即返回_传完done(env):
    make, src, dst, tmp, pg = env
    (tmp / "s/a.bin").write_bytes(DATA)
    (tmp / "s/b.bin").write_bytes(b"bb")
    events = []
    q = make(notifier=events.append)
    tid = q.submit("两个文件", [(src, "a.bin", dst, "o/a.bin"), (src, "b.bin", dst, "o/b.bin")])
    until(lambda: status(q, tid) == "done")
    assert (tmp / "d/o/a.bin").read_bytes() == DATA and (tmp / "d/o/b.bin").read_bytes() == b"bb"
    d = q.task_detail(tid)
    assert [i["status"] for i in d["items"]] == ["done", "done"]
    assert d["items"][0]["done_bytes"] == len(DATA) and n_chunks(pg, d["items"][0]["id"]) == 25
    row = q.list_tasks()[0]
    assert row["n_items"] == 2 and row["done_items"] == 2 and row["done_bytes"] == len(DATA) + 2
    assert [e["status"] for e in events if e["type"] == "task"][:3] == ["queued", "running", "done"]


def test_持久化_新实例还看得到任务并自动传完(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    q1 = make(start=False)
    tid = q1.submit("先存着", [(src, "a.bin", dst, "a.bin")])
    q1.close()
    q2 = make()                                   # 另一个"进程"起来
    assert q2.task_detail(tid)["title"] == "先存着"
    until(lambda: status(q2, tid) == "done")
    assert (tmp / "d/a.bin").read_bytes() == DATA


def test_重启恢复_running残留自动续传_从断点接着(env):
    make, src, dst, tmp, pg = env
    (tmp / "s/a.bin").write_bytes(DATA)
    dst.cut_after = 80
    q1 = make()
    tid = q1.submit("断", [(src, "a.bin", dst, "a.bin")])
    until(lambda: status(q1, tid) == "waiting")
    item = q1.task_detail(tid)["items"][0]
    assert item["done_bytes"] == 80 and n_chunks(pg, item["id"]) == 10
    q1.close()                                    # 进程"死"在 waiting
    dst.up = True
    src.seeks.clear()
    q2 = make()
    until(lambda: status(q2, tid) == "done")
    assert (tmp / "d/a.bin").read_bytes() == DATA
    assert 80 in src.seeks                        # 从第 80 字节读，没从头来


def test_重启恢复_库里留着running也会恢复_paused保持(env):
    make, src, dst, tmp, pg = env
    (tmp / "s/a.bin").write_bytes(b"x" * 30)
    q0 = make(start=False)
    t_run = q0.submit("r", [(src, "a.bin", dst, "r.bin")])
    t_pause = q0.submit("p", [(src, "a.bin", dst, "p.bin")])
    q0.pause(t_pause)
    with pg.begin() as c:
        c.execute(text("UPDATE transfer_tasks SET status='running' WHERE id=:i"), {"i": t_run})
        c.execute(text("UPDATE transfer_items SET status='running' WHERE task_id=:i"), {"i": t_run})
    q = make()
    until(lambda: status(q, t_run) == "done")
    time.sleep(0.3)
    assert status(q, t_pause) == "paused" and not (tmp / "d/p.bin").exists()
    assert q.resume(t_pause)
    until(lambda: status(q, t_pause) == "done")


def test_断线进waiting_恢复后自动续完(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    dst.cut_after = 50
    q = make()
    tid = q.submit("断线", [(src, "a.bin", dst, "a.bin")])
    until(lambda: status(q, tid) == "waiting")
    time.sleep(0.3)
    assert status(q, tid) == "waiting" and not (tmp / "d/a.bin").exists()   # 没人点之前一直等
    dst.up = True                                  # 网络回来，不点任何按钮
    until(lambda: status(q, tid) == "done")
    assert (tmp / "d/a.bin").read_bytes() == DATA
    assert q.task_detail(tid)["items"][0]["error"] is None


def test_断线时源端先不可用也进waiting(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    src.up = False
    q = make()
    tid = q.submit("源掉了", [(src, "a.bin", dst, "a.bin")])
    until(lambda: status(q, tid) == "waiting")
    src.up = True
    until(lambda: status(q, tid) == "done")


def test_断线等待中_有更高优先级任务就让出_回来再续(env):
    """连不上的低优先级任务在等网络时不能堵死串行队列：高优先级的照跑，跑完低优先级的回去接着等。"""
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    (tmp / "s/b.bin").write_bytes(b"b" * 20)
    (tmp / "d2").mkdir()
    dead = Flaky(tmp / "d2")
    dead.up = False
    q = make()
    q._factory = lambda spec, _f=q._factory: dead if spec == storage_to_spec(dead) else _f(spec)
    low = q.submit("连不上", [(src, "a.bin", dead, "a.bin")], priority=0)
    until(lambda: status(q, low) == "waiting")
    hi = q.submit("要紧", [(src, "b.bin", dst, "b.bin")], priority=100)
    until(lambda: status(q, hi) == "done", timeout=5)
    assert (tmp / "d/b.bin").read_bytes() == b"b" * 20
    until(lambda: status(q, low) == "waiting")                # 回去接着等，不算失败
    dead.up = True
    until(lambda: status(q, low) == "done")
    assert (tmp / "d2/a.bin").read_bytes() == DATA


def test_非连接错误_该项failed_任务继续_如实汇总(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/ok1.bin").write_bytes(b"1" * 20)
    (tmp / "s/conf.bin").write_bytes(b"c" * 20)
    (tmp / "s/ok2.bin").write_bytes(b"2" * 20)
    (tmp / "d/conf.bin").write_bytes(b"z" * 20)
    q = make()
    tid = q.submit("混合", [(src, "ok1.bin", dst, "ok1.bin"), (src, "missing.bin", dst, "m.bin"),
                            (src, "conf.bin", dst, "conf.bin"), (src, "ok2.bin", dst, "ok2.bin")])
    until(lambda: status(q, tid) == "failed")
    d = q.task_detail(tid)
    assert [i["status"] for i in d["items"]] == ["done", "failed", "failed", "done"]
    assert "不存在" in d["items"][1]["error"] and "未覆盖" in d["items"][2]["error"]
    assert d["error"] == "2 项失败"
    assert (tmp / "d/conf.bin").read_bytes() == b"z" * 20 and (tmp / "d/ok2.bin").exists()
    # 修好之后可以重来：失败项重置，已完成的不动
    (tmp / "s/missing.bin").write_bytes(b"m" * 5)
    (tmp / "d/conf.bin").unlink()
    assert q.resume(tid)
    until(lambda: status(q, tid) == "done")


def test_暂停_续传_取消(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    dst.delay = 0.02
    q = make()
    tid = q.submit("p", [(src, "a.bin", dst, "a.bin")])
    until(lambda: q.task_detail(tid)["items"][0]["done_bytes"] >= 24)
    assert q.pause(tid)
    until(lambda: q.task_detail(tid)["items"][0]["status"] == "pending")
    d = q.task_detail(tid)
    assert d["status"] == "paused" and 24 <= d["items"][0]["done_bytes"] < len(DATA)
    frozen = d["items"][0]["done_bytes"]
    time.sleep(0.3)
    assert q.task_detail(tid)["items"][0]["done_bytes"] == frozen      # 真停了
    assert q.resume(tid)
    until(lambda: status(q, tid) == "done")
    assert (tmp / "d/a.bin").read_bytes() == DATA

    (tmp / "s/b.bin").write_bytes(DATA)
    t2 = q.submit("c", [(src, "b.bin", dst, "b.bin")])
    until(lambda: q.task_detail(t2)["items"][0]["done_bytes"] >= 16)
    assert q.cancel(t2)
    until(lambda: not (tmp / "d/b.bin.xferq.part").exists())
    time.sleep(0.2)
    d2 = q.task_detail(t2)
    assert d2["status"] == "cancelled" and d2["items"][0]["status"] == "cancelled"
    assert not (tmp / "d/b.bin").exists()
    assert not q.cancel(t2) and not q.pause(t2)


def test_任务串行执行(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    dst.delay = 0.01
    q = make()
    t1 = q.submit("1", [(src, "a.bin", dst, "1.bin")])
    t2 = q.submit("2", [(src, "a.bin", dst, "2.bin")])
    until(lambda: status(q, t1) == "running")
    assert status(q, t2) == "queued"
    until(lambda: status(q, t2) == "done")
    assert status(q, t1) == "done"


def test_advisory_lock_同一个库只有一个worker(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(b"x" * 30)
    q1, q2 = make(), make()
    until(lambda: q1.is_worker or q2.is_worker)
    time.sleep(0.4)
    assert q1.is_worker != q2.is_worker
    loser, winner = (q2, q1) if q1.is_worker else (q1, q2)
    tid = loser.submit("从非worker提交", [(src, "a.bin", dst, "a.bin")])
    until(lambda: status(loser, tid) == "done")             # 由 worker 那个进程跑
    assert loser.list_tasks()[0]["id"] == tid
    winner.close()                                           # worker 走了，另一个接手
    until(lambda: loser.is_worker)
    (tmp / "s/b.bin").write_bytes(b"y" * 30)
    t2 = loser.submit("接手后", [(src, "b.bin", dst, "b.bin")])
    until(lambda: status(loser, t2) == "done")


def test_审计表有记录(env):
    make, src, dst, tmp, pg = env
    (tmp / "s/a.bin").write_bytes(DATA)
    q = make()
    tid = q.submit("审计", [(src, "a.bin", dst, "a.bin")])
    until(lambda: status(q, tid) == "done")
    assert {"transfer_tasks", "transfer_items"} <= set(db.audited_tables(pg))
    tasks = db.query_audit(pg, table="transfer_tasks")
    assert tasks[0]["op"] == "INSERT" and tasks[0]["new_row"]["title"] == "审计"
    assert tasks[-1]["new_row"]["status"] == "done"
    items = db.query_audit(pg, table="transfer_items", limit=10000)
    assert items[0]["op"] == "INSERT" and all(r["application_name"] == "xfer-test" for r in items)


def test_审计量与块数无关_块表不审计(env):
    make, src, dst, tmp, pg = env
    big = os.urandom(50 * CH)                       # 50 块
    (tmp / "s/big.bin").write_bytes(big)
    (tmp / "s/small.bin").write_bytes(b"x" * CH)    # 1 块
    q = make()
    t1 = q.submit("大", [(src, "big.bin", dst, "big.bin")])
    until(lambda: status(q, t1) == "done")
    t2 = q.submit("小", [(src, "small.bin", dst, "small.bin")])
    until(lambda: status(q, t2) == "done")
    ids = [q.task_detail(t)["items"][0]["id"] for t in (t1, t2)]
    assert n_chunks(pg, ids[0]) == 50
    rows = db.query_audit(pg, table="transfer_items", limit=10000)
    per = {i: [r for r in rows if r["pk"]["id"] == i] for i in ids}
    assert len(per[ids[0]]) == len(per[ids[1]]) <= 5     # 只随状态变化次数，和块数无关
    assert all("sha256" not in (r["new_row"] or {}) for r in rows)
    assert db.query_audit(pg, table="transfer_chunks") == []
    assert "transfer_chunks" not in db.audited_tables(pg)


def test_重复建表_第二个队列不报错(env):
    make, *_ = env
    make(start=False), make(start=False)


# ---------------------------------------------------------------- 优先级与空闲闸

def _seq(events, tid):
    return [e["status"] for e in events if e["type"] == "task" and e["task_id"] == tid]


def test_同优先级先到先得_高优先级先跑(env):
    make, src, dst, tmp, _ = env
    for n in "abc":
        (tmp / f"s/{n}.bin").write_bytes(b"x" * 16)
    events = []
    q = make(start=False, notifier=events.append)
    ta = q.submit("a", [(src, "a.bin", dst, "a.bin")])
    tb = q.submit("b", [(src, "b.bin", dst, "b.bin")])
    tc = q.submit("c", [(src, "c.bin", dst, "c.bin")], priority=5)
    q.start()
    until(lambda: all(status(q, t) == "done" for t in (ta, tb, tc)))
    order = [e["task_id"] for e in events if e["type"] == "task" and e["status"] == "running"]
    assert order == [tc, ta, tb]


def test_高优先级抢占低优先级_低的之后续完(env):
    make, src, dst, tmp, pg = env
    (tmp / "s/low.bin").write_bytes(DATA)
    (tmp / "s/high.bin").write_bytes(b"h" * 16)
    dst.delay = 0.02
    events = []
    q = make(notifier=events.append)
    low = q.submit("low", [(src, "low.bin", dst, "low.bin")], priority=0)
    until(lambda: q.task_detail(low)["items"][0]["done_bytes"] >= 24)
    high = q.submit("high", [(src, "high.bin", dst, "high.bin")], priority=100)
    until(lambda: status(q, high) == "done")
    until(lambda: status(q, low) == "done")
    assert (tmp / "d/low.bin").read_bytes() == DATA and (tmp / "d/high.bin").read_bytes() == b"h" * 16
    assert _seq(events, low) == ["queued", "running", "queued", "running", "done"]   # 被让出过一次、回来续传
    assert _seq(events, high) == ["queued", "running", "done"]
    # 低优先级是接着传的，不是从头：高优先级跑的时候它的进度没被清零
    order = [(e["task_id"], e["status"]) for e in events if e["type"] == "task"]
    assert order.index((low, "queued")) < order.index((high, "running")) < order.index((high, "done")) \
        < len(order) - order[::-1].index((low, "running")) - 1


def test_同优先级不抢占(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    (tmp / "s/b.bin").write_bytes(b"b" * 8)
    dst.delay = 0.01
    events = []
    q = make(notifier=events.append)
    a = q.submit("a", [(src, "a.bin", dst, "a.bin")], priority=7)
    until(lambda: q.task_detail(a)["items"][0]["done_bytes"] >= 8)
    b = q.submit("b", [(src, "b.bin", dst, "b.bin")], priority=7)
    until(lambda: status(q, b) == "done")
    assert _seq(events, a) == ["queued", "running", "done"]


def test_空闲闸_繁忙不动_空闲后续完(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    flag = [True]
    q = make(busy=lambda: flag[0], idle_threshold=10)
    t = q.submit("bg", [(src, "a.bin", dst, "a.bin")], priority=0)
    time.sleep(0.5)
    d = q.task_detail(t)
    assert d["status"] == "queued" and d["items"][0]["done_bytes"] == 0
    # 不受闸的高优先级照跑
    (tmp / "s/u.bin").write_bytes(b"u" * 8)
    u = q.submit("user", [(src, "u.bin", dst, "u.bin")], priority=100)
    until(lambda: status(q, u) == "done")
    assert status(q, t) == "queued"
    flag[0] = False
    until(lambda: status(q, t) == "done")
    assert (tmp / "d/a.bin").read_bytes() == DATA


def test_空闲闸_跑到一半变忙会让出_空闲再续完(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    dst.delay = 0.02
    flag = [False]
    q = make(busy=lambda: flag[0], idle_threshold=10)
    t = q.submit("bg", [(src, "a.bin", dst, "a.bin")], priority=0)
    until(lambda: q.task_detail(t)["items"][0]["done_bytes"] >= 24)
    flag[0] = True
    until(lambda: status(q, t) == "queued")
    until(lambda: q.task_detail(t)["items"][0]["status"] == "pending")
    frozen = q.task_detail(t)["items"][0]["done_bytes"]
    assert 24 <= frozen < len(DATA)
    time.sleep(0.3)
    assert q.task_detail(t)["items"][0]["done_bytes"] == frozen      # 真停了，进度保留
    flag[0] = False
    until(lambda: status(q, t) == "done")
    assert (tmp / "d/a.bin").read_bytes() == DATA


def test_busy抛异常按繁忙处理(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(b"a" * 16)
    state = {"boom": True}

    def busy():
        if state["boom"]:
            raise RuntimeError("探测失败")
        return False
    q = make(busy=busy, idle_threshold=1)
    t = q.submit("bg", [(src, "a.bin", dst, "a.bin")])
    time.sleep(0.4)
    assert status(q, t) == "queued"
    state["boom"] = False
    until(lambda: status(q, t) == "done")


def test_set_priority(env):
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(b"a" * 16)
    flag = [True]
    q = make(busy=lambda: flag[0], idle_threshold=10)
    t = q.submit("bg", [(src, "a.bin", dst, "a.bin")])
    time.sleep(0.2)
    assert status(q, t) == "queued"
    assert q.set_priority(t, 100)
    until(lambda: status(q, t) == "done")
    assert not q.set_priority(t, 1)          # 已结束


def test_已有表补priority列(env):
    make, src, dst, tmp, pg = env
    q0 = make(start=False)
    q0.submit("old", [(src, "x", dst, "x")])
    with pg.begin() as c:
        c.execute(text("ALTER TABLE transfer_tasks DROP COLUMN priority"))
    from pybase.transfer import create_schema
    create_schema(pg)
    create_schema(pg)                         # 幂等
    with pg.connect() as c:
        row = c.execute(text("SELECT priority FROM transfer_tasks")).all()
    assert [r[0] for r in row] == [0]         # 老任务默认 0
    t = q0.submit("new", [(src, "y", dst, "y")], priority=50)
    assert q0.task_detail(t)["priority"] == 50
