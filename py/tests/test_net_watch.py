"""pybase.net.monitor.watch 测试：

- 路由消息解析（本机 `route -n monitor` 实测样例 + 按 `strings /sbin/route`
  核实过的格式串编造的 RTM_ADD 样例）、`ip monitor`（Linux，未能本机实测，
  按 `ip-monitor(8)` 文档行为构造）。
- `scutil` 通知解析（按 `strings /usr/sbin/scutil` 核实过的格式串构造，本机
  只读观察窗口内没等到真实变化）。
- `NetWatcher`：用假脚本代替真实 `route`/`scutil`，验证事件分发、debounce
  突发合并、子进程意外退出重启（带退避）、`stop()` 杀干净子进程且可重复调用。
"""
from __future__ import annotations

import os
import stat
import sys
import threading
import time

import pytest

from pybase.net.monitor.watch import (
    NetWatcher,
    WatchEvent,
    parse_ip_monitor_line,
    parse_route_text,
    parse_scutil_text,
    to_recorder_event,
)

# ---------------------------------------------------------------------------
# route -n monitor 解析
# ---------------------------------------------------------------------------

# 本机 `route -n monitor` 实测抓到的原始输出（只读观察，没有主动制造网络变化；
# RTM_MISS 是系统对 DNS 服务器地址做路由查找失败的常规通知，不是配置变化，但
# 格式跟其余 RTM_* 类型一致，足够验证解析器）。
_REAL_RTM_MISS = """got message of size 120 on Mon Sep 28 16:12:11 2026
RTM_MISS: Lookup failed on this address: len 120, pid: 0, seq 0, errno 0, flags:<DONE>
locks:  inits:
sockaddrs: <DST>
 2001:4860:4860::8888
"""

# 按 `strings /sbin/route` 核实过的格式串（"%s: len %d, if# %d, flags:<%s>"、
# "pid: %ld, seq %d, errno %d, flags:<%s>"、"sockaddrs: <%s>"）编造的 RTM_ADD
# 样例——本机被动观察没等到真实路由增删（没有主动断开/重连网络）。
_SAMPLE_RTM_ADD = """got message of size 148 on Mon Sep 28 16:20:00 2026
RTM_ADD: Add Route: len 148, if# 15, pid: 0, seq 0, errno 0, flags:<UP,GATEWAY,DONE,STATIC>
locks:  inits:
sockaddrs: <DST,GATEWAY,NETMASK>
 10.0.0.0 10.0.0.1 255.255.255.0
"""


def test_parse_route_text_real_rtm_miss():
    events = parse_route_text(_REAL_RTM_MISS)
    assert len(events) == 1
    ev = events[0]
    assert ev["kind"] == "RTM_MISS"
    assert ev["detail"]["flags"] == ["DONE"]
    assert ev["detail"]["sockaddrs"] == {"DST": "2001:4860:4860::8888"}
    assert ev["subject"] == "2001:4860:4860::8888"


def test_parse_route_text_sample_rtm_add_with_if_index():
    events = parse_route_text(_SAMPLE_RTM_ADD)
    assert len(events) == 1
    ev = events[0]
    assert ev["kind"] == "RTM_ADD"
    assert ev["detail"]["if_index"] == 15
    assert set(ev["detail"]["flags"]) == {"UP", "GATEWAY", "DONE", "STATIC"}
    assert ev["detail"]["sockaddrs"] == {
        "DST": "10.0.0.0",
        "GATEWAY": "10.0.0.1",
        "NETMASK": "255.255.255.0",
    }


def test_parse_route_text_multiple_blocks_and_garbage():
    text = _REAL_RTM_MISS + "\nnot a route message\nrandom noise\n\n" + _SAMPLE_RTM_ADD
    events = parse_route_text(text)
    # 中间那段解析不出 RTM_ 头，直接跳过，不产生事件、不报错
    assert [e["kind"] for e in events] == ["RTM_MISS", "RTM_ADD"]


# ---------------------------------------------------------------------------
# ip monitor 解析（Linux，未本机实测，按 ip-monitor(8) 文档行为构造样例）
# ---------------------------------------------------------------------------


def test_parse_ip_monitor_link_line():
    ev = parse_ip_monitor_line("3: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...")
    assert ev["kind"] == "RTM_NEWLINK"
    assert ev["subject"] == "eth0"
    assert "UP" in ev["detail"]["flags"]


def test_parse_ip_monitor_addr_line():
    ev = parse_ip_monitor_line("inet 192.168.1.5/24 brd 192.168.1.255 scope global dev eth0")
    assert ev["kind"] == "RTM_NEWADDR"
    assert ev["subject"] == "eth0"
    assert ev["detail"]["address"] == "192.168.1.5/24"


def test_parse_ip_monitor_route_line():
    ev = parse_ip_monitor_line("default via 192.168.1.1 dev eth0 proto dhcp metric 100")
    assert ev["kind"] == "RTM_NEWROUTE"
    assert ev["detail"]["gateway"] == "192.168.1.1"


def test_parse_ip_monitor_deleted_prefix():
    ev = parse_ip_monitor_line("Deleted default via 192.168.1.1 dev eth0")
    assert ev["kind"] == "RTM_DELROUTE"


def test_parse_ip_monitor_line_blank_is_none():
    assert parse_ip_monitor_line("   ") is None


# ---------------------------------------------------------------------------
# scutil n.watch 解析
# ---------------------------------------------------------------------------

# 按 `strings /usr/sbin/scutil` 核实过的格式串（"notification callback (store
# address = %p)."、"  %s changedKey [%d] = %@"）构造——本机只读观察窗口内没等到
# 真实的 SCDynamicStore 键变化（没有主动改网络配置）。
_SAMPLE_SCUTIL_WATCH = """notification callback (store address = 0x7f8a2b006600).
  changedKey [0] = State:/Network/Global/IPv4
  changedKey [1] = State:/Network/Interface/en0/IPv4
notification callback (store address = 0x7f8a2b006600).
  changedKey [0] = Setup:/Network/Global/Proxies
"""


def test_parse_scutil_text_multiple_blocks():
    keys = parse_scutil_text(_SAMPLE_SCUTIL_WATCH)
    assert keys == [
        "State:/Network/Global/IPv4",
        "State:/Network/Interface/en0/IPv4",
        "Setup:/Network/Global/Proxies",
    ]


def test_parse_scutil_text_no_changes():
    assert parse_scutil_text("some unrelated output\n") == []


# ---------------------------------------------------------------------------
# to_recorder_event
# ---------------------------------------------------------------------------


def test_to_recorder_event_shape():
    ev = WatchEvent(at=123.0, source="route", kind="RTM_ADD", subject="en0", detail={"a": 1})
    out = to_recorder_event(ev)
    assert out == {
        "at": 123.0,
        "kind": "live_route_RTM_ADD",
        "subject": "en0",
        "before": None,
        "after": {"a": 1},
        "source": "local",  # source 是"哪台机器"，监听来源并进 kind
    }


def test_live_events_can_be_written_to_recorder():
    from pybase.net.monitor.recorder import Recorder

    r = Recorder(":memory:")
    ev = WatchEvent(at=5.0, source="store", kind="changed", subject="State:/Network/Global/IPv4", detail=None)
    r.add_events([to_recorder_event(ev)])
    got = r.events(0, 10)
    assert [(e["kind"], e["subject"], e["source"]) for e in got] == [
        ("live_store_changed", "State:/Network/Global/IPv4", "local")]


# ---------------------------------------------------------------------------
# NetWatcher：假脚本代替 route/scutil
# ---------------------------------------------------------------------------

_PY = sys.executable

_FAKE_ROUTE_STABLE = """import sys, time
print("got message of size 148 on fake time")
print("RTM_ADD: Add Route: len 148, pid: 0, seq 0, errno 0, flags:<UP,GATEWAY,DONE,STATIC>")
print("sockaddrs: <DST,GATEWAY,NETMASK>")
print(" 10.0.0.0 10.0.0.1 255.255.255.0")
print()
sys.stdout.flush()
time.sleep(60)
"""

# 第一次调用立即以非零退出（模拟崩溃），第二次（marker 文件已存在）才稳定打印
# +长睡，用于验证 NetWatcher 的自动重启（带退避）与 watcher_restarted 事件。
_FAKE_ROUTE_CRASH_ONCE = """import os, sys, time
marker = sys.argv[1]
if not os.path.exists(marker):
    open(marker, "w").close()
    sys.exit(1)
print("got message of size 148 on fake time")
print("RTM_ADD: Add Route: len 148, pid: 0, seq 0, errno 0, flags:<UP,GATEWAY,DONE,STATIC>")
print("sockaddrs: <DST,GATEWAY,NETMASK>")
print(" 10.0.0.0 10.0.0.1 255.255.255.0")
print()
sys.stdout.flush()
time.sleep(60)
"""

# 真实 scutil 是"读交互脚本（n.add/n.watch）后开始长驻吐通知"，假脚本模拟同样
# 的协议：读一行 stdin（吞掉 NetWatcher 写进来的 n.add/n.watch 脚本）后打印一条
# 通知，再长睡（等 stop() 来杀）。
_FAKE_SCUTIL_STABLE = """import sys, time
sys.stdin.readline()
print("notification callback (store address = 0x1).")
print("  changedKey [0] = State:/Network/Global/IPv4")
sys.stdout.flush()
time.sleep(60)
"""

_FAKE_SCUTIL_SLEEP_ONLY = """import sys, time
sys.stdin.readline()
time.sleep(60)
"""


def _write_script(tmp_path, name: str, body: str) -> str:
    """写一个可独立执行（shebang 指向当前解释器）的假命令脚本，`scutil_bin` 这类
    参数只接受"单个可执行文件路径、不带参数"，不能像 `route_cmd` 那样传 argv 列表。
    """
    path = tmp_path / name
    path.write_text(f"#!{_PY}\n{body}")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return str(path)


def _pids_alive(*pids) -> list[int]:
    alive = []
    for pid in pids:
        if pid is None:
            continue
        try:
            os.kill(pid, 0)
        except OSError:
            continue
        else:
            alive.append(pid)
    return alive


def test_netwatcher_dispatches_parsed_route_and_store_events(tmp_path):
    events: list[WatchEvent] = []
    lock = threading.Lock()

    def on_event(ev: WatchEvent) -> None:
        with lock:
            events.append(ev)

    scutil_bin = _write_script(tmp_path, "fake_scutil", _FAKE_SCUTIL_STABLE)
    w = NetWatcher(
        on_event,
        store_patterns=["State:/Network/.*"],
        route_cmd=[_PY, "-c", _FAKE_ROUTE_STABLE],
        scutil_bin=scutil_bin,
    )
    try:
        w.start()
        deadline = time.time() + 5
        while time.time() < deadline:
            with lock:
                kinds = {e.kind for e in events}
            if "RTM_ADD" in kinds and "store_key_changed" in kinds:
                break
            time.sleep(0.05)
    finally:
        w.stop()

    with lock:
        by_kind = {e.kind: e for e in events}
    assert "RTM_ADD" in by_kind
    assert by_kind["RTM_ADD"].source == "route"
    assert by_kind["RTM_ADD"].detail["sockaddrs"]["GATEWAY"] == "10.0.0.1"
    assert "store_key_changed" in by_kind
    store_ev = by_kind["store_key_changed"]
    assert store_ev.source == "store"
    assert store_ev.subject == "State:/Network/Global/IPv4"


def test_netwatcher_restarts_route_process_with_backoff(tmp_path):
    marker = tmp_path / "route-started-once"
    scutil_bin = _write_script(tmp_path, "fake_scutil", _FAKE_SCUTIL_SLEEP_ONLY)
    events: list[WatchEvent] = []
    lock = threading.Lock()

    def on_event(ev: WatchEvent) -> None:
        with lock:
            events.append(ev)

    w = NetWatcher(
        on_event,
        store_patterns=["State:/Network/.*"],
        route_cmd=[_PY, "-c", _FAKE_ROUTE_CRASH_ONCE, str(marker)],
        scutil_bin=scutil_bin,
        restart_backoff=(0.05,),
    )
    try:
        w.start()
        deadline = time.time() + 5
        while time.time() < deadline:
            with lock:
                kinds = [e.kind for e in events if e.source == "route"]
            if "watcher_restarted" in kinds and "RTM_ADD" in kinds:
                break
            time.sleep(0.05)
    finally:
        w.stop()

    with lock:
        kinds = [e.kind for e in events if e.source == "route"]
    assert "watcher_restarted" in kinds
    assert "RTM_ADD" in kinds
    # 重启事件必须在真正拿到消息之前
    assert kinds.index("watcher_restarted") < kinds.index("RTM_ADD")


def test_netwatcher_debounce_merges_burst_into_one_callback():
    bursts: list[list[WatchEvent]] = []
    burst_lock = threading.Lock()

    w = NetWatcher(
        lambda ev: None,
        on_burst=lambda batch: (bursts.append(batch)),
        store_patterns=["State:/Network/.*"],
        debounce=0.15,
    )
    # 不启动真实子进程/线程，直接单元测 debounce 合并机制本身：快速喂三个事件
    # （间隔小于 debounce），应该只触发一次 on_burst，且带全部三个事件；等静默期
    # 过后再喂一个，应该单独再触发一次。
    ev1 = WatchEvent(1.0, "route", "RTM_ADD", "en0", {})
    ev2 = WatchEvent(1.01, "route", "RTM_ADD", "en0", {})
    ev3 = WatchEvent(1.02, "store", "store_key_changed", "State:/x", {})
    w._dispatch(ev1)
    time.sleep(0.05)
    w._dispatch(ev2)
    time.sleep(0.05)
    w._dispatch(ev3)

    time.sleep(0.4)
    with burst_lock:
        pass
    assert bursts == [[ev1, ev2, ev3]]

    ev4 = WatchEvent(2.0, "route", "RTM_DELETE", "en0", {})
    w._dispatch(ev4)
    time.sleep(0.4)
    assert bursts == [[ev1, ev2, ev3], [ev4]]


def test_netwatcher_stop_kills_child_process_groups_and_is_idempotent(tmp_path):
    scutil_bin = _write_script(tmp_path, "fake_scutil", _FAKE_SCUTIL_STABLE)
    w = NetWatcher(
        lambda ev: None,
        store_patterns=["State:/Network/.*"],
        route_cmd=[_PY, "-c", _FAKE_ROUTE_STABLE],
        scutil_bin=scutil_bin,
    )
    w.start()
    time.sleep(0.5)
    with w._proc_lock:
        route_pid = w._route_proc.pid if w._route_proc else None
        store_pid = w._store_proc.pid if w._store_proc else None
    assert route_pid is not None
    assert store_pid is not None

    w.stop()
    assert _pids_alive(route_pid, store_pid) == []

    # 幂等：再 stop 一次不该抛异常
    w.stop()


def test_netwatcher_start_twice_without_stop_raises(tmp_path):
    scutil_bin = _write_script(tmp_path, "fake_scutil", _FAKE_SCUTIL_SLEEP_ONLY)
    w = NetWatcher(
        lambda ev: None,
        store_patterns=["State:/Network/.*"],
        route_cmd=[_PY, "-c", "import time; time.sleep(60)"],
        scutil_bin=scutil_bin,
    )
    w.start()
    try:
        with pytest.raises(Exception):
            w.start()
    finally:
        w.stop()
