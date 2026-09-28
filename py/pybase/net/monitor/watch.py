"""实时网络配置变化监听：`route -n monitor`（BSD 路由套接字消息，macOS/BSD）+
`ip monitor`（Linux）拿路由/地址/链路变化，`scutil` 交互模式的 `n.add`/`n.watch`
拿 SCDynamicStore 键变化，统一成 `WatchEvent` 交给调用方。

定时采集（`runner.Monitor`）间隔几十秒，中间的变化（比如热点断开又重连）会被
漏掉；这个模块只管"变化本身+时间点"，不重新实现一遍全量采集——`on_burst`
回调是给调用方一个"该触发一次 `Monitor.tick()` 了"的信号。

只读监听，绝不改任何网络设置；两路监听各自的子进程独立进程组，`stop()` 保证
整组杀干净、可重复调用。长驻子进程的管理方式照抄 `net.engine.WireGuardGo`
（`media._proc` 的 `run()`/`run_streaming()` 都要等进程退出才返回，管不了常驻
前台进程，所以这里直接用 `Popen` + `_proc._GROUP_KWARGS`/`_proc._kill`）。
"""
from __future__ import annotations

import dataclasses
import os
import platform
import re
import signal
import socket
import subprocess
import threading
import time
from typing import Callable, Optional, Sequence

from ...media import _proc

__all__ = [
    "WatchError",
    "WatchEvent",
    "NetWatcher",
    "parse_route_text",
    "parse_route_block",
    "parse_scutil_text",
    "to_recorder_event",
]


class WatchError(RuntimeError):
    """NetWatcher 相关错误的基类（目前只用于 stop 后误用等编程错误）。"""


@dataclasses.dataclass(frozen=True)
class WatchEvent:
    """一次实时观测到的变化。`kind` 两路监听各自有自己的取值空间：
    `route` 侧是路由消息类型（`RTM_ADD` 等）或 `watcher_restarted`/`watcher_error`；
    `store` 侧是 `store_key_changed` 或同样的 `watcher_restarted`/`watcher_error`。
    `detail` 是该 kind 私有的附加信息，调用方按 kind 分发着看。
    """

    at: float
    source: str  # 'route' | 'store'
    kind: str
    subject: str
    detail: dict


def to_recorder_event(ev: WatchEvent, *, source: str = "local") -> dict:
    """转成跟 `Recorder` 的 `events` 表同形状的 dict：`{at, kind, subject, before,
    after, source}`，`kind` 加 `live_` 前缀（跟 `record()`/`diff.py` 产生的事件
    区分开——那批是"两次采集之间配置变了什么"，这批是"路由/系统配置这一刻收到
    了什么通知"，粒度和可信度都不一样，不能混着按同一套 kind 处理）。

    `before` 恒为 None（这里没有"变化前"的完整快照可比，只有通知本身）；
    `after` 放 `detail`。本模块不碰 `Recorder` 内部实现，写不写、怎么写由调用方
    决定（比如攒一批后走某个批量写入口，或者只是先落个文件）。
    """
    # `source` 是 Recorder 的"哪台机器"维度（本机 local，以后有路由器），
    # 监听来源（route/store）不能占用它，并进 kind 里
    return {
        "at": ev.at,
        "kind": f"live_{ev.source}_{ev.kind}",
        "subject": ev.subject,
        "before": None,
        "after": ev.detail,
        "source": source,
    }


# ============================================================
# route -n monitor（macOS/BSD）/ ip monitor（Linux）解析
# ============================================================

# 真实输出格式来自本机 `/sbin/route` 二进制里的 printf 格式串（`strings /sbin/route`）
# 加本机实测（见模块末尾注释/测试夹具），不是猜的：
#
#   got message of size 120 on Mon Sep 28 16:12:11 2026
#   RTM_MISS: Lookup failed on this address: len 120, pid: 0, seq 0, errno 0, flags:<DONE>
#   locks:  inits:
#   sockaddrs: <DST>
#    2001:4860:4860::8888
#
# 消息之间空行分隔；`sockaddrs: <LABEL1,LABEL2,...>` 后紧跟一行，按空格切分的地址
# 跟 label 顺序一一对应（DST/GATEWAY/NETMASK/IFP/IFA/... 具体出现哪些由消息类型
# 决定）。本机短时间观察只等到了 RTM_MISS（DNS 查询触发的路由查找失败通知，
# 不是真正的配置变化），RTM_ADD/RTM_IFINFO 等其余类型没能被动等到——按同一份
# 格式串（`%s: len %d, if# %d, flags:<%s>` / `pid: %ld, seq %d, errno %d, flags:<%s>`）
# 构造的样例做测试夹具，字段名称经 `strings` 核实存在，具体数值是合理编造。
_ROUTE_HEADER_RE = re.compile(r"^(?P<kind>RTM_[A-Z]+):\s*[^:]*:\s*(?P<rest>.*)$")
_ROUTE_IFNUM_RE = re.compile(r"if#\s*(?P<idx>\d+)")
_ROUTE_FLAGS_RE = re.compile(r"flags:<(?P<flags>[^>]*)>")
_ROUTE_SOCKADDR_LABELS_RE = re.compile(r"^sockaddrs:\s*<(?P<labels>[^>]*)>\s*$")


def _if_name_of(idx: Optional[int]) -> Optional[str]:
    if not idx:
        return None
    try:
        return socket.if_indextoname(idx)
    except (OSError, ValueError):
        return None


def parse_route_block(lines: Sequence[str]) -> Optional[dict]:
    """解析 `route -n monitor` 的一条消息（不含首尾空行），返回
    `{kind, subject, detail}` 或 `None`（不是一条能认出的消息，比如残缺片段）。
    """
    header = None
    header_rest = ""
    sockaddr_labels: list[str] = []
    sockaddr_values: list[str] = []
    for i, line in enumerate(lines):
        m = _ROUTE_HEADER_RE.match(line.strip())
        if m:
            header, header_rest = m.group("kind"), m.group("rest")
            continue
        m = _ROUTE_SOCKADDR_LABELS_RE.match(line.strip())
        if m:
            sockaddr_labels = [x.strip() for x in m.group("labels").split(",") if x.strip()]
            if i + 1 < len(lines):
                sockaddr_values = lines[i + 1].split()
            continue
    if header is None:
        return None
    flags_m = _ROUTE_FLAGS_RE.search(header_rest)
    flags = [f.strip() for f in flags_m.group("flags").split(",") if f.strip()] if flags_m else []
    ifnum_m = _ROUTE_IFNUM_RE.search(header_rest)
    if_index = int(ifnum_m.group("idx")) if ifnum_m else None
    if_name = _if_name_of(if_index)
    sockaddrs = dict(zip(sockaddr_labels, sockaddr_values))
    subject = if_name or sockaddrs.get("DST") or sockaddrs.get("IFP") or header
    detail = {
        "if_index": if_index,
        "if_name": if_name,
        "flags": flags,
        "sockaddrs": sockaddrs,
    }
    return {"kind": header, "subject": subject, "detail": detail}


def parse_route_text(text: str) -> list[dict]:
    """一次性解析一整段 `route -n monitor` 输出（按空行切块），跳过解析不出的块。
    主要给测试用；流式读取（子进程持续吐行）走 `_RouteBlockSplitter`。
    """
    blocks: list[list[str]] = []
    current: list[str] = []
    for line in text.splitlines():
        if line.strip() == "":
            if current:
                blocks.append(current)
                current = []
            continue
        current.append(line)
    if current:
        blocks.append(current)
    out = []
    for block in blocks:
        parsed = parse_route_block(block)
        if parsed is not None:
            out.append(parsed)
    return out


class _RouteBlockSplitter:
    """流式版本：逐行喂，遇到空行吐出上一块的解析结果（没有可解析内容就吐 `None`）。"""

    def __init__(self) -> None:
        self._buf: list[str] = []

    def feed(self, line: str) -> Optional[dict]:
        if line.strip() == "":
            if not self._buf:
                return None
            block, self._buf = self._buf, []
            return parse_route_block(block)
        self._buf.append(line)
        return None

    def flush(self) -> Optional[dict]:
        if not self._buf:
            return None
        block, self._buf = self._buf, []
        return parse_route_block(block)


# ---- Linux: ip monitor route address link ----
#
# 本机是 macOS，没法实测；按 iproute2 文档化的行为写（`ip-monitor(8)`：对象被删
# 除时整行前缀 `Deleted `，链路信息行形如 `<if#>: <ifname>: <FLAGS> ...`，地址行
# 形如 `inet/inet6 <addr> ... dev <ifname>`，路由行形如 `<dst> [via <gw>] dev
# <ifname> ...`）——没有本机真实抓包核实，字段名/正则按这份公开约定写，遇到真实
# 输出对不上就按需调整，不影响 macOS 路径。
_IP_LINK_RE = re.compile(
    r"^(?P<deleted>Deleted\s+)?\d+:\s+(?P<ifname>[^:@\s]+)(?:@\S+)?:\s*<(?P<flags>[^>]*)>"
)
_IP_ADDR_RE = re.compile(
    r"^(?P<deleted>Deleted\s+)?(?P<family>inet6?)\s+(?P<addr>\S+).*?\bdev\s+(?P<ifname>\S+)"
)
_IP_ROUTE_RE = re.compile(
    r"^(?P<deleted>Deleted\s+)?(?P<dst>\S+)\s+(?:via\s+(?P<gw>\S+)\s+)?dev\s+(?P<ifname>\S+)"
)


def parse_ip_monitor_line(line: str) -> Optional[dict]:
    """Linux `ip monitor route address link` 单行解析（每行本身就是一条完整消息，
    不像 `route -n monitor` 要跨行拼块）。
    """
    stripped = line.strip()
    if not stripped:
        return None
    m = _IP_LINK_RE.match(stripped)
    if m:
        kind = "RTM_DELLINK" if m.group("deleted") else "RTM_NEWLINK"
        flags = [f.strip() for f in m.group("flags").split(",") if f.strip()]
        return {
            "kind": kind,
            "subject": m.group("ifname"),
            "detail": {"if_name": m.group("ifname"), "flags": flags, "raw": stripped},
        }
    m = _IP_ADDR_RE.match(stripped)
    if m:
        kind = "RTM_DELADDR" if m.group("deleted") else "RTM_NEWADDR"
        return {
            "kind": kind,
            "subject": m.group("ifname"),
            "detail": {
                "if_name": m.group("ifname"),
                "family": m.group("family"),
                "address": m.group("addr"),
                "raw": stripped,
            },
        }
    m = _IP_ROUTE_RE.match(stripped)
    if m:
        kind = "RTM_DELROUTE" if m.group("deleted") else "RTM_NEWROUTE"
        return {
            "kind": kind,
            "subject": m.group("ifname"),
            "detail": {
                "if_name": m.group("ifname"),
                "destination": m.group("dst"),
                "gateway": m.group("gw"),
                "raw": stripped,
            },
        }
    return {"kind": "RTM_UNKNOWN", "subject": stripped.split()[0], "detail": {"raw": stripped}}


# ============================================================
# scutil n.add / n.watch 解析（SCDynamicStore 键变化）
# ============================================================

# 命令语法来自本机 `scutil` 交互模式的 `help`（实测）：
#   n.add key ["pattern"]   : add notification key
#   n.watch                 : watch for changes
# 输出格式没能在只读观察窗口里等到真实变化（没有制造网络变化），但从
# `strings /usr/sbin/scutil` 里核实了确实编译进了这几条 printf 格式串：
#   "notification callback (store address = %p)."
#   "  %s changedKey [%d] = %@"
#   "  no changed key's."
# 据此认为一次回调形如：
#   notification callback (store address = 0x7f8a2b006600).
#     changedKey [0] = State:/Network/Global/IPv4
# 解析只依赖 "changedKey [N] = <key>" 这一段（用 search 不用 match，不关心 %s
# 前缀具体是什么），对格式变化有一点容错空间。
_SCUTIL_CALLBACK_START_RE = re.compile(r"notification callback")
_SCUTIL_CHANGED_KEY_RE = re.compile(r"changedKey\s*\[\d+\]\s*=\s*(?P<key>\S.*?)\s*$")


def parse_scutil_text(text: str) -> list[str]:
    """一次性解析整段 `n.watch` 输出，返回按出现顺序的变化键列表（一次性版本，
    主要给测试用；流式读取走 `_ScutilBlockSplitter`）。
    """
    keys: list[str] = []
    for line in text.splitlines():
        m = _SCUTIL_CHANGED_KEY_RE.search(line)
        if m:
            keys.append(m.group("key"))
    return keys


class _ScutilBlockSplitter:
    """流式版本：按 `changedKey [N] = <key>` 逐行吐键；`notification callback` 只是
    块起始标记，不单独产出，靠 `feed()` 逐行喂即可，不需要跨行拼装。
    """

    def feed(self, line: str) -> Optional[str]:
        m = _SCUTIL_CHANGED_KEY_RE.search(line)
        return m.group("key") if m else None


# ============================================================
# NetWatcher：后台线程跑两路监听 + 突发合并
# ============================================================

_DEFAULT_STORE_PATTERNS = ("State:/Network/.*", "Setup:/Network/.*")  # 同 sysconfig.collect_dynamic_store 的默认
_DEFAULT_BACKOFF = (1.0, 2.0, 5.0, 10.0, 30.0)


class NetWatcher:
    """后台跑两个只读监听线程：`route -n monitor`/`ip monitor`（路由/地址/链路
    变化）+ `scutil` 交互模式（SCDynamicStore 键变化），把解析出的变化统一成
    `WatchEvent` 传给 `on_event`；给了 `on_burst` 就再按 `debounce` 秒的静默期
    合并成一批回调一次，供调用方触发一次即时全量采集。

    `store_patterns` 不给就用平台默认（跟 `sysconfig.collect_dynamic_store` 一致，
    可覆盖）。`route_cmd`/`scutil_bin` 是测试钩子：传假命令/假脚本代替真实
    `route`/`scutil`，验证重启、退出清理逻辑不用真的碰网络接口。

    `start()`/`stop()` 不是线程安全的（跟 `net.engine.WireGuardGo` 一致，调用方
    自己串行化）；`stop()` 幂等，可重复调用。
    """

    def __init__(
        self,
        on_event: Callable[[WatchEvent], None],
        *,
        store_patterns: Optional[Sequence[str]] = None,
        on_burst: Optional[Callable[[list[WatchEvent]], None]] = None,
        debounce: float = 2.0,
        route_cmd: Optional[Sequence[str]] = None,
        scutil_bin: Optional[str] = None,
        restart_backoff: Sequence[float] = _DEFAULT_BACKOFF,
        stop_timeout: float = 5.0,
    ) -> None:
        self._on_event = on_event
        self._on_burst = on_burst
        self._debounce = debounce
        self._store_patterns = tuple(store_patterns) if store_patterns else _DEFAULT_STORE_PATTERNS
        self._route_cmd = list(route_cmd) if route_cmd else None
        self._scutil_bin = scutil_bin
        self._restart_backoff = tuple(restart_backoff) or (1.0,)
        self._stop_timeout = stop_timeout

        self._stop_event = threading.Event()
        self._route_thread: Optional[threading.Thread] = None
        self._store_thread: Optional[threading.Thread] = None
        self._route_proc: Optional[subprocess.Popen] = None
        self._store_proc: Optional[subprocess.Popen] = None
        self._proc_lock = threading.Lock()

        self._burst_lock = threading.Lock()
        self._burst: list[WatchEvent] = []
        self._debounce_timer: Optional[threading.Timer] = None

    # ---------- 生命周期 ----------

    def start(self) -> None:
        if self._route_thread is not None or self._store_thread is not None:
            raise WatchError("已经在运行，不能重复 start（先 stop）")
        self._stop_event = threading.Event()
        self._route_thread = threading.Thread(target=self._route_loop, daemon=True)
        self._store_thread = threading.Thread(target=self._store_loop, daemon=True)
        self._route_thread.start()
        self._store_thread.start()

    def _kill_proc(self, proc: subprocess.Popen) -> None:
        """整组杀：先 SIGTERM 给个机会自己退，超时再 SIGKILL。幂等（进程已经
        退出就直接跳过）。
        """
        if proc.poll() is None:
            try:
                os.killpg(proc.pid, signal.SIGTERM)
            except OSError:
                pass
            try:
                proc.wait(timeout=self._stop_timeout)
            except subprocess.TimeoutExpired:
                _proc._kill(proc, drain=False)

    def stop(self) -> None:
        """幂等：没在跑直接返回。杀子进程组（先 SIGTERM 后超时强杀）、join 线程、
        取消未触发的 debounce 定时器。

        `stop_event` 置位与"抓子进程引用来杀"之间有个窗口：`start()` 后的读取
        线程可能刚跑到 `Popen()` 还没来得及把结果存进 `self._route_proc`/
        `self._store_proc`，这里抓到的就还是 `None`，杀不到——这种情况交给
        `_route_loop`/`_store_loop` 自己在赋值后立刻补一次 `stop_event` 检查处理
        （见那两处注释），不是这个方法能单独兜住的，所以这里也不重试等待。
        """
        self._stop_event.set()
        with self._proc_lock:
            procs = [self._route_proc, self._store_proc]
        for proc in procs:
            if proc is not None:
                self._kill_proc(proc)
        for t in (self._route_thread, self._store_thread):
            if t is not None:
                t.join(timeout=self._stop_timeout + 2)
        self._route_thread = None
        self._store_thread = None
        self._route_proc = None
        self._store_proc = None
        with self._burst_lock:
            if self._debounce_timer is not None:
                self._debounce_timer.cancel()
                self._debounce_timer = None
            self._burst = []

    # ---------- 事件分发 + 突发合并 ----------

    def _dispatch(self, ev: WatchEvent) -> None:
        try:
            self._on_event(ev)
        except Exception:  # noqa: BLE001 - 回调出错不该打断读取线程
            pass
        if self._on_burst is None:
            return
        with self._burst_lock:
            self._burst.append(ev)
            if self._debounce_timer is not None:
                self._debounce_timer.cancel()
            timer = threading.Timer(self._debounce, self._flush_burst)
            timer.daemon = True
            self._debounce_timer = timer
            timer.start()

    def _flush_burst(self) -> None:
        with self._burst_lock:
            batch, self._burst = self._burst, []
            self._debounce_timer = None
        if not batch or self._on_burst is None:
            return
        try:
            self._on_burst(batch)
        except Exception:  # noqa: BLE001 - 回调出错不该打断读取线程
            pass

    # ---------- route 监听 ----------

    def _resolve_route_cmd(self) -> list[str]:
        if self._route_cmd is not None:
            return self._route_cmd
        system = platform.system()
        if system == "Darwin":
            tool = _proc.resolve_tool(None, "route")
            return [tool, "-n", "monitor"]
        if system == "Linux":
            tool = _proc.resolve_tool(None, "ip")
            return [tool, "monitor", "route", "address", "link"]
        raise _proc.ToolNotFoundError(f"不支持的平台：{system}")

    def _route_loop(self) -> None:
        backoff_idx = 0
        linux = platform.system() == "Linux" and self._route_cmd is None
        while not self._stop_event.is_set():
            try:
                cmd = self._resolve_route_cmd()
                proc = subprocess.Popen(
                    cmd,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.DEVNULL,
                    stdin=subprocess.DEVNULL,
                    text=True,
                    encoding="utf-8",
                    errors="replace",
                    **_proc._GROUP_KWARGS,
                )
            except Exception as exc:  # noqa: BLE001 - 启动失败当一次"意外退出"处理
                self._dispatch(WatchEvent(time.time(), "route", "watcher_error", "route", {"error": str(exc)}))
                if self._stop_event.wait(self._restart_backoff[min(backoff_idx, len(self._restart_backoff) - 1)]):
                    return
                backoff_idx += 1
                continue

            with self._proc_lock:
                self._route_proc = proc
            # 补一次 stop_event 检查：`stop()` 可能在 Popen 完成、`self._route_proc`
            # 赋值之前就已经跑过"给已知子进程发信号"那一步，那样这个刚起来的
            # 进程谁都不会杀，只能靠这里自己发现并处理，否则 stop() 会在等线程
            # join 时一直卡到超时，子进程也就泄漏了。
            if self._stop_event.is_set():
                self._kill_proc(proc)
                with self._proc_lock:
                    self._route_proc = None
                return
            self._read_route_stream(proc, linux=linux)
            with self._proc_lock:
                self._route_proc = None

            if self._stop_event.is_set():
                return
            delay = self._restart_backoff[min(backoff_idx, len(self._restart_backoff) - 1)]
            self._dispatch(
                WatchEvent(time.time(), "route", "watcher_restarted", "route", {"delay": delay})
            )
            backoff_idx += 1
            if self._stop_event.wait(delay):
                return

    def _read_route_stream(self, proc: subprocess.Popen, *, linux: bool) -> None:
        splitter = _RouteBlockSplitter()
        try:
            assert proc.stdout is not None
            for line in proc.stdout:
                at = time.time()
                if linux:
                    parsed = parse_ip_monitor_line(line)
                else:
                    parsed = splitter.feed(line)
                if parsed is not None:
                    self._dispatch(WatchEvent(at, "route", parsed["kind"], parsed["subject"], parsed["detail"]))
            if not linux:
                parsed = splitter.flush()
                if parsed is not None:
                    self._dispatch(WatchEvent(time.time(), "route", parsed["kind"], parsed["subject"], parsed["detail"]))
        except Exception:  # noqa: BLE001 - 读取异常（比如管道被杀断）当进程退出处理
            pass
        proc.wait()

    # ---------- scutil (SCDynamicStore) 监听 ----------

    def _resolve_scutil_bin(self) -> str:
        return self._scutil_bin or _proc.resolve_tool(None, "scutil")

    def _store_loop(self) -> None:
        backoff_idx = 0
        script = "".join(f'n.add "{p}"\n' for p in self._store_patterns) + "n.watch\n"
        while not self._stop_event.is_set():
            try:
                tool = self._resolve_scutil_bin()
                proc = subprocess.Popen(
                    [tool],
                    stdin=subprocess.PIPE,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.DEVNULL,
                    text=True,
                    encoding="utf-8",
                    errors="replace",
                    **_proc._GROUP_KWARGS,
                )
            except Exception as exc:  # noqa: BLE001
                self._dispatch(WatchEvent(time.time(), "store", "watcher_error", "store", {"error": str(exc)}))
                if self._stop_event.wait(self._restart_backoff[min(backoff_idx, len(self._restart_backoff) - 1)]):
                    return
                backoff_idx += 1
                continue

            with self._proc_lock:
                self._store_proc = proc
            if self._stop_event.is_set():
                # 同 `_route_loop`：`stop()` 可能在 `self._store_proc` 赋值前就已经
                # 跑完"给已知子进程发信号"那一步，这里补一次检查兜底。
                self._kill_proc(proc)
                with self._proc_lock:
                    self._store_proc = None
                return
            try:
                if proc.stdin is not None:
                    proc.stdin.write(script)
                    proc.stdin.flush()
                    # 不能关闭 stdin：`scutil` 交互模式把 stdin EOF 当成隐式 `quit`
                    # 处理（本机实测——关了 stdin，即使正在 `n.watch` 里也会立刻
                    # 退出，返回码 0，完全不报错，只能靠这个来龙去脉解释"进程秒退"）。
                    # 保持管道打开，靠 stop()/进程退出时一并回收。
            except (BrokenPipeError, OSError):
                pass
            self._read_store_stream(proc)
            with self._proc_lock:
                self._store_proc = None

            if self._stop_event.is_set():
                return
            delay = self._restart_backoff[min(backoff_idx, len(self._restart_backoff) - 1)]
            self._dispatch(
                WatchEvent(time.time(), "store", "watcher_restarted", "store", {"delay": delay})
            )
            backoff_idx += 1
            if self._stop_event.wait(delay):
                return

    def _read_store_stream(self, proc: subprocess.Popen) -> None:
        splitter = _ScutilBlockSplitter()
        try:
            assert proc.stdout is not None
            for line in proc.stdout:
                key = splitter.feed(line)
                if key is not None:
                    self._dispatch(WatchEvent(time.time(), "store", "store_key_changed", key, {}))
        except Exception:  # noqa: BLE001
            pass
        proc.wait()
        if proc.stdin is not None:
            try:
                proc.stdin.close()
            except OSError:
                pass
