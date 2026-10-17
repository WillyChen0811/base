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
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    (tmp / "s/b.bin").write_bytes(b"bb")
    events = []
    q = make(notifier=events.append)
    tid = q.submit("两个文件", [(src, "a.bin", dst, "o/a.bin"), (src, "b.bin", dst, "o/b.bin")])
    until(lambda: status(q, tid) == "done")
    assert (tmp / "d/o/a.bin").read_bytes() == DATA and (tmp / "d/o/b.bin").read_bytes() == b"bb"
    d = q.task_detail(tid)
    assert [i["status"] for i in d["items"]] == ["done", "done"]
    assert d["items"][0]["done_bytes"] == len(DATA) and len(d["items"][0]["chunks"]) == 25
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
    make, src, dst, tmp, _ = env
    (tmp / "s/a.bin").write_bytes(DATA)
    dst.cut_after = 80
    q1 = make()
    tid = q1.submit("断", [(src, "a.bin", dst, "a.bin")])
    until(lambda: status(q1, tid) == "waiting")
    item = q1.task_detail(tid)["items"][0]
    assert item["done_bytes"] == 80 and len(item["chunks"]) == 10
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
    assert items[0]["op"] == "INSERT" and len(items) > 25     # 每块进度都有记录
    assert all(r["application_name"] == "xfer-test" for r in items)


def test_重复建表_第二个队列不报错(env):
    make, *_ = env
    make(start=False), make(start=False)
