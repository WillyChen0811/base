"""按"用户关心的应用"为核心的采集器：每个应用当前有哪些**进程实例**（区分终端
窗口/pid，用户可能同时在好几个终端里跑 ssh/codex/Claude Code）、各自的连接
出口、代表域名在 system/router/clash 三条路径下分别通不通。

约定同 `collectors.py`：永不抛异常，失败体现在返回值的 `error`/单项 `status`
字段；不内置任何应用/域名/网卡/DNS 默认值，全部由调用方（netcore）传入。

进程实例识别：一个应用常有"主进程 + 一堆 Helper 子进程"，这些互为父子的进程
合并成一个实例；同一个应用在不同终端窗口各起一份（互相不是父子关系）则各自
成一个实例，带各自的 pid/tty/cwd。联网但自己没匹配到任何应用规则的进程（比如
Claude Code 调用的 `git`），沿父进程链找到属于哪个应用实例后归到其下、标
`child:<命令名>`；挂在终端上、找不到归属的联网进程归入 `terminal_other`。

进程枚举用 macOS `libproc`（ctypes 调 `proc_listallpids`+`proc_pidpath`，实测约
3ms，比每轮 fork `ps`/`lsof` 子进程快一个数量级），非 macOS 平台直接返回空列表
（调用方据此得到"没有应用在跑"，不是错误）。

耗时/IP/HTTP 状态码这类**每轮探测都会变**的字段特意不参与"变了才存事件"的判断：
`Recorder` 的去重存储本身按内容哈希决定存引用还是存全量，探测结果每轮哈希都不同
是预期行为（跟 `collect_targets`/`collect_mihomo` 一样，交给存储层的通用去重机制
处理，不需要这里再单独处理）；但**事件**只在状态在 ok/非 ok 之间切换、出口变化、
应用启停、多个域名的路由器解析同时变化时才生成（见 `events._diff_apps`），避免
延迟抖动把真正的信号淹没在噪声里。
"""
from __future__ import annotations

import concurrent.futures
import ctypes
import ctypes.util
import platform
import random
import re
import socket
import ssl
import struct
import threading
import time
from typing import Callable, Optional, Sequence

from ..diag import classify_address, route_of
from ...media._proc import MediaError, ToolNotFoundError, resolve_tool, run
from .collectors import _fail, _mihomo_get, _ok, via_of

_TIMEOUT_DEFAULT = 3.0
_HTTPS_PORT = 443

# macOS <netinet/in.h> 的 IP_BOUND_IF；不是 Python socket 模块的标准常量，只在
# Darwin 上有意义（`probe_router`/`_dns_query_router` 用它强制走物理网卡，绕开
# Clash TUN 的路由劫持）。
_IP_BOUND_IF = 25


# ---------------------------------------------------------------------------
# 进程枚举 + 匹配
# ---------------------------------------------------------------------------


def _libproc():
    lib_path = ctypes.util.find_library("proc")
    if not lib_path:
        return None
    try:
        return ctypes.CDLL(lib_path)
    except OSError:
        return None


def list_processes() -> list[dict]:
    """列出当前所有进程的 `{"pid", "path"}`。只在 macOS 有效；其他平台/加载库
    失败都返回空列表，不抛异常——调用方据此认为"这台机器上没有可匹配的应用"。
    """
    if platform.system() != "Darwin":
        return []
    lib = _libproc()
    if lib is None:
        return []
    try:
        n = lib.proc_listallpids(None, 0)
        if n <= 0:
            return []
        buf = (ctypes.c_int * (n * 2))()
        n = lib.proc_listallpids(buf, ctypes.sizeof(buf))
        pids = [p for p in buf[:n] if p > 0]
        path_buf = ctypes.create_string_buffer(4096)
        result: list[dict] = []
        for pid in pids:
            r = lib.proc_pidpath(pid, path_buf, 4096)
            if r > 0:
                result.append({"pid": pid, "path": path_buf.value.decode("utf-8", "replace")})
        return result
    except OSError:
        return []


def _matching_pattern(patterns: Sequence[str], path: str) -> Optional[str]:
    """`patterns` 里第一条命中的规则：先按前缀匹配（最常见、最快），前缀不中
    再当正则试一次（无效正则直接跳过，不报错——配置写错正则不该炸掉整个采集）。
    返回命中的那条规则本身（后面判断"是不是主进程"要用），不命中返回 None。
    """
    for pat in patterns:
        if path.startswith(pat):
            return pat
    for pat in patterns:
        try:
            if re.search(pat, path):
                return pat
        except re.error:
            continue
    return None


def _process_matches(patterns: Sequence[str], path: str) -> bool:
    return _matching_pattern(patterns, path) is not None


def _is_main_process(pattern: str, path: str) -> bool:
    """通用的"这是应用主进程，不是 Helper"判定。

    标准 macOS app bundle 的主可执行文件恒在
    `<bundle>.app/Contents/MacOS/<可执行文件名>`——比 bundle 路径前缀正好多出
    `Contents`/`MacOS`/`<名字>` 三段。Helper（`Contents/Helpers/...`）、嵌套的
    Helper bundle（`Contents/Frameworks/Foo Helper.app/Contents/MacOS/...`，
    段数更多）都不满足这个形状，判不是主进程——这就是为什么 Claude 桌面版的
    `Contents/Helpers/chrome-native-host`（Chrome 扩展的辅助进程）不会被误判
    成"桌面版在运行"。

    `pattern` 不是标准 `.app/` bundle 前缀（比如 Claude Code 那种 CLI 可执行
    文件路径/版本号正则）时，没有"主进程 vs Helper"这一说，命中即算主进程。
    """
    if not pattern.endswith(".app/"):
        return True
    if not path.startswith(pattern):
        return False
    parts = [p for p in path[len(pattern):].split("/") if p]
    return len(parts) == 3 and parts[0] == "Contents" and parts[1] == "MacOS"


def match_processes(apps: Sequence[dict], processes: Sequence[dict]) -> dict[str, dict]:
    """按每个应用的 `process` 规则匹配进程，返回
    `{app_name: {"running", "pids", "main_pids", "paths"}}`。一个应用常常带
    一堆 Helper 子进程，`process` 规则要能把它们都覆盖到（比如给一个共同的
    路径前缀）；但"在不在运行"只看主进程（见 `_is_main_process`），Helper
    单独活着不代表应用在运行。
    """
    matches: dict[str, dict] = {}
    for app in apps:
        name = app.get("name")
        patterns = app.get("process") or []
        if isinstance(patterns, str):
            patterns = [patterns]
        pids: list[int] = []
        main_pids: list[int] = []
        paths: set[str] = set()
        for proc in processes:
            path = proc.get("path")
            if not path:
                continue
            pat = _matching_pattern(patterns, path)
            if pat is None:
                continue
            pids.append(proc["pid"])
            paths.add(path)
            if _is_main_process(pat, path):
                main_pids.append(proc["pid"])
        matches[name] = {
            "running": bool(main_pids), "pids": pids, "main_pids": main_pids, "paths": sorted(paths),
        }
    return matches


# ---------------------------------------------------------------------------
# 进程实例：pid / tty / 父子链 / cwd（同一个应用可能同时在多个终端窗口各跑一份）
# ---------------------------------------------------------------------------


def list_process_details(*, timeout: float = _TIMEOUT_DEFAULT) -> dict[int, dict]:
    """`ps -axwwo pid=,ppid=,tty=,command=`：`{pid: {"ppid", "tty", "command"}}`。

    `-ww` 保证长命令行不被截断（要从里面找 `--resume <会话id>` 这类参数）；
    `tty` 是 `??` 表示这个进程不挂在任何终端上。`ps` 缺失/失败返回空字典，
    不算错误——调用方据此得到的 tty/父进程信息缺失，不影响进程匹配本身
    （匹配走的是 `list_processes` 的 libproc 路径）。
    """
    try:
        tool = resolve_tool(None, "ps")
        result = run([tool, "-axwwo", "pid=,ppid=,tty=,command="], timeout=timeout)
    except (ToolNotFoundError, MediaError):
        return {}
    if result.returncode != 0:
        return {}
    details: dict[int, dict] = {}
    for line in result.stdout.splitlines():
        parts = line.split(None, 3)
        if len(parts) < 4:
            continue
        pid_s, ppid_s, tty, command = parts
        if not pid_s.isdigit() or not ppid_s.lstrip("-").isdigit():
            continue
        details[int(pid_s)] = {"ppid": int(ppid_s), "tty": tty, "command": command}
    return details


def cwd_of_pids(pids: Sequence[int], *, timeout: float = _TIMEOUT_DEFAULT) -> dict[int, str]:
    """`lsof -a -p <pids> -d cwd -Fn`：`{pid: 工作目录}`。只对调用方给定的 pid 查，
    不是全量——`lsof` 打开 cwd 这类文件描述符信息本身不便宜，没必要对所有进程都问。
    """
    pids = [p for p in pids if p]
    if not pids:
        return {}
    try:
        tool = resolve_tool(None, "lsof")
        result = run(
            [tool, "-a", "-p", ",".join(str(p) for p in pids), "-d", "cwd", "-Fn"], timeout=timeout,
        )
    except (ToolNotFoundError, MediaError):
        return {}
    if result.returncode not in (0, 1):
        return {}
    cwd: dict[int, str] = {}
    current_pid: Optional[int] = None
    for line in result.stdout.splitlines():
        if not line:
            continue
        tag, value = line[0], line[1:]
        if tag == "p":
            try:
                current_pid = int(value)
            except ValueError:
                current_pid = None
        elif tag == "n" and current_pid is not None:
            cwd[current_pid] = value
    return cwd


def _own_command_name(command: str) -> str:
    """从 `ps command=` 字段（含参数）里取可执行文件的短名字：第一个空格前的
    token，再取路径最后一段。用于给子进程打 `child:<名字>` 标签。
    """
    exe = (command or "").split(" ", 1)[0]
    return exe.rsplit("/", 1)[-1] or "?"


_BACKGROUND_KEY = "background"


def _instance_key(pid: int, details: dict[int, dict]) -> str:
    """一个应用的进程按这个 key 分组成"实例"：挂在终端上的（`tty` 不是 `??`）
    按 tty 分组——同一个应用在不同终端窗口各起一份，互相独立算不同实例；没挂
    在终端上的（图形界面应用及其一堆 Helper 子进程，`tty` 都是 `??`）全部
    归到同一个 `"background"` 分组，合并成应用唯一的一个实例——图形界面应用
    不像终端里的 CLI 工具那样有"哪个窗口"的概念，进程树关系（谁是谁的
    Helper）本身也不总是父子直连（沙盒化的 XPC Helper 经常挂在 launchd 下），
    按 tty 分组比按进程树分组更可靠、也更符合用户对"这是同一个应用"的直觉。
    """
    tty = details.get(pid, {}).get("tty")
    if not tty or tty == "??":
        return _BACKGROUND_KEY
    return tty


def _find_owning_app(
    pid: int, details: dict[int, dict], pid_to_owner: dict[int, tuple[str, str]], *, max_depth: int = 20,
) -> Optional[tuple[str, str, str]]:
    """一个联网但自己没匹配到任何应用的进程，沿父进程链往上找，看是不是某个
    已识别应用的子孙（比如 Claude Code 调用的 `git`/`curl`）。找到就返回
    `(应用名, 实例 key, 这个子进程自己的命令名)`，用于打 `child:<名字>` 标签；
    走到 launchd（pid 1）或链断了还没找到就返回 None——不是任何已知应用的子进程。
    """
    own_command = details.get(pid, {}).get("command", "")
    own_comm = _own_command_name(own_command)
    current = details.get(pid, {}).get("ppid")
    seen = {pid}
    depth = 0
    while current is not None and current not in seen and current != 0 and depth < max_depth:
        owner = pid_to_owner.get(current)
        if owner is not None:
            return owner[0], owner[1], own_comm
        seen.add(current)
        info = details.get(current)
        if info is None or current == 1:
            break
        current = info["ppid"]
        depth += 1
    return None


# ---------------------------------------------------------------------------
# 已建立连接 + 出口归属
# ---------------------------------------------------------------------------


def list_established_connections(*, timeout: float = _TIMEOUT_DEFAULT) -> dict[int, list[tuple[str, int]]]:
    """`lsof -nP -iTCP -sTCP:ESTABLISHED -Fpn`：`{pid: [(对端ip, 对端port), ...]}`。

    `lsof` 缺失、跑失败都返回空字典而不是抛异常——调用方据此得到的出口统计是
    空的，这不代表进程没在联网，只是这一轮没测到，不算错误。
    """
    try:
        tool = resolve_tool(None, "lsof")
        result = run([tool, "-nP", "-iTCP", "-sTCP:ESTABLISHED", "-Fpn"], timeout=timeout)
    except (ToolNotFoundError, MediaError):
        return {}
    # lsof 一条连接都没找到时退出码是 1，不是失败
    if result.returncode not in (0, 1):
        return {}
    conns: dict[int, list[tuple[str, int]]] = {}
    current_pid: Optional[int] = None
    for line in result.stdout.splitlines():
        if not line:
            continue
        tag, value = line[0], line[1:]
        if tag == "p":
            try:
                current_pid = int(value)
            except ValueError:
                current_pid = None
            else:
                conns.setdefault(current_pid, [])
        elif tag == "n" and current_pid is not None:
            addr = value.rpartition("->")[2] if "->" in value else None
            if not addr:
                continue
            host, _, port = addr.rpartition(":")
            host = host.strip("[]")
            if host and port.isdigit():
                conns[current_pid].append((host, int(port)))
    return conns


def _exit_label(host: str, port: int, via: str, clash_ports: Sequence[int]) -> str:
    """把一条连接的出口归到用户关心的几类标签：`clash_proxy`（连去本机 mihomo
    端口）、`router_fakeip`（目标是 198.18.0.0/15 且直连出去，说明路由器在给这个
    域名下发 fake-ip）、其余沿用 `via_of` 给的 `clash_tun`/`wireguard:x`/
    `direct:<iface>`/`unknown`。
    """
    if host in ("127.0.0.1", "::1") and port in clash_ports:
        return "clash_proxy"
    if classify_address(host) == "fake_ip" and via.startswith("direct:"):
        return "router_fakeip"
    return via


def app_exits(
    pids: Sequence[int],
    connections: dict[int, list[tuple[str, int]]],
    owners: Optional[dict],
    clash_ports: Sequence[int],
    *,
    route_timeout: float = _TIMEOUT_DEFAULT,
    route_cache: Optional[dict[str, str]] = None,
) -> dict[str, int]:
    """某个应用当前已建立连接的出口分布：`{出口标签: 连接数}`。

    `route_cache` 建议调用方跨应用共享同一个字典传入：同一个目标 IP 被多个
    应用的连接共用时只查一次路由（`route` 命令本身要 fork 子进程，不是免费的）。
    """
    cache = route_cache if route_cache is not None else {}
    counts: dict[str, int] = {}
    for pid in pids:
        for host, port in connections.get(pid, []):
            via = cache.get(host)
            if via is None:
                route = route_of(host, timeout=route_timeout)
                via = via_of(route.interface if route else None, owners)
                cache[host] = via
            label = _exit_label(host, port, via, clash_ports)
            counts[label] = counts.get(label, 0) + 1
    return counts


# ---------------------------------------------------------------------------
# 域名探测：system / router / clash 三条路径
# ---------------------------------------------------------------------------


def _now_ms(start: float) -> float:
    return (time.monotonic() - start) * 1000


def _bound_socket(iface: Optional[str]) -> socket.socket:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    if iface:
        sock.setsockopt(socket.IPPROTO_IP, _IP_BOUND_IF, socket.if_nametoindex(iface))
    return sock


def _finish_tls_head(
    sock: socket.socket, host: str, t0: float, connect_ms: float,
    dns_ms: Optional[float], timeout: float, *, ip: Optional[str] = None,
) -> dict:
    """在已连接的 socket 上做 TLS 握手 + `HEAD /`。只要收到任何 HTTP 状态行（哪怕
    403/404）就算 `ok`——探测目的是"通不通"，不是"这条路径鉴权对不对"。
    """
    try:
        ctx = ssl.create_default_context()
        sock.settimeout(timeout)
        tls_sock = ctx.wrap_socket(sock, server_hostname=host)
    except (ssl.SSLError, OSError) as exc:
        return {"status": "tls_fail", "error": str(exc), "dns_ms": dns_ms, "ip": ip, "connect_ms": connect_ms}
    tls_ms = _now_ms(t0)
    try:
        tls_sock.settimeout(timeout)
        req = (
            f"HEAD / HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n"
            f"User-Agent: netcore-probe\r\n\r\n"
        ).encode()
        tls_sock.sendall(req)
        resp = tls_sock.recv(64)
        status_line = resp.split(b"\r\n", 1)[0].decode("latin-1", "replace")
        parts = status_line.split(" ", 2)
        http_status = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else None
    except socket.timeout:
        return {"status": "http_timeout", "error": "HEAD 超时", "dns_ms": dns_ms, "ip": ip,
                "connect_ms": connect_ms, "tls_ms": tls_ms}
    except OSError as exc:
        return {"status": "http_timeout", "error": str(exc), "dns_ms": dns_ms, "ip": ip,
                "connect_ms": connect_ms, "tls_ms": tls_ms}
    finally:
        try:
            tls_sock.close()
        except OSError:
            pass
    if http_status is None:
        return {"status": "http_timeout", "error": f"读不到合法状态行：{status_line!r}", "dns_ms": dns_ms,
                "ip": ip, "connect_ms": connect_ms, "tls_ms": tls_ms}
    return {
        "status": "ok", "error": None, "dns_ms": dns_ms, "ip": ip,
        "connect_ms": connect_ms, "tls_ms": tls_ms, "total_ms": _now_ms(t0), "http_status": http_status,
    }


def _finish_probe(host: str, ip: str, port: int, dns_ms: float, timeout: float, *, bind_iface: Optional[str]) -> dict:
    t0 = time.monotonic()
    try:
        sock = _bound_socket(bind_iface)
        sock.settimeout(timeout)
        sock.connect((ip, port))
    except ConnectionRefusedError as exc:
        return {"status": "connect_refused", "error": str(exc), "dns_ms": dns_ms, "ip": ip, "connect_ms": _now_ms(t0)}
    except socket.timeout:
        return {"status": "connect_timeout", "error": "connect 超时", "dns_ms": dns_ms, "ip": ip,
                "connect_ms": _now_ms(t0)}
    except OSError as exc:
        return {"status": "connect_timeout", "error": str(exc), "dns_ms": dns_ms, "ip": ip, "connect_ms": _now_ms(t0)}
    connect_ms = _now_ms(t0)
    return _finish_tls_head(sock, host, t0, connect_ms, dns_ms, timeout, ip=ip)


def probe_system(host: str, *, port: int = _HTTPS_PORT, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """系统当前实际路径：`getaddrinfo` 解析 + TCP + TLS + `HEAD /`，走系统默认
    路由——有没有代理/VPN/Clash TUN 接管，全看系统这时候怎么选路，不强制走
    哪张网卡。
    """
    t0 = time.monotonic()
    try:
        infos = socket.getaddrinfo(host, port, proto=socket.IPPROTO_TCP)
    except OSError as exc:
        return {"status": "dns_fail", "error": str(exc), "dns_ms": _now_ms(t0)}
    dns_ms = _now_ms(t0)
    ip = infos[0][4][0]
    return _finish_probe(host, ip, port, dns_ms, timeout, bind_iface=None)


def _dns_query_router(host: str, server: str, iface: str, *, timeout: float) -> tuple[list[str], float]:
    """手写最小 DNS A 记录查询，UDP socket 绑物理网卡直接问 `server`——Clash 没有
    UDP:53 的透明代理规则，绑物理网卡是唯一能绕开它、直接问到路由器的办法。
    只解析 A 记录，压缩指针之外的花样不支持，不重试（超时/失败交给调用方处理）。
    """
    txid = random.randint(0, 65535)
    header = struct.pack(">HHHHHH", txid, 0x0100, 1, 0, 0, 0)
    question = b"".join(bytes([len(label)]) + label.encode("idna") for label in host.split(".")) + b"\x00"
    question += struct.pack(">HH", 1, 1)  # QTYPE=A, QCLASS=IN
    query = header + question

    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        sock.setsockopt(socket.IPPROTO_IP, _IP_BOUND_IF, socket.if_nametoindex(iface))
        sock.settimeout(timeout)
        t0 = time.monotonic()
        sock.sendto(query, (server, 53))
        data, _ = sock.recvfrom(2048)
        elapsed_ms = _now_ms(t0)
    finally:
        sock.close()
    return _parse_dns_a_response(data, expected_txid=txid), elapsed_ms


def _parse_dns_a_response(data: bytes, *, expected_txid: int) -> list[str]:
    if len(data) < 12:
        return []
    txid = struct.unpack(">H", data[0:2])[0]
    if txid != expected_txid:
        return []
    answer_count = struct.unpack(">H", data[6:8])[0]
    i = 12
    while i < len(data) and data[i]:
        i += data[i] + 1
    i += 5  # 结尾的 0x00 + QTYPE(2) + QCLASS(2)
    ips: list[str] = []
    for _ in range(answer_count):
        if i >= len(data):
            break
        if data[i] & 0xC0 == 0xC0:
            i += 2
        else:
            while i < len(data) and data[i]:
                i += data[i] + 1
            i += 1
        if i + 10 > len(data):
            break
        rtype, _rclass, _ttl, rdlen = struct.unpack(">HHIH", data[i:i + 10])
        i += 10
        if rtype == 1 and rdlen == 4 and i + 4 <= len(data):
            ips.append(socket.inet_ntoa(data[i:i + 4]))
        i += rdlen
    return ips


def probe_router(
    host: str, *, iface: str, dns_server: str, port: int = _HTTPS_PORT, timeout: float = _TIMEOUT_DEFAULT,
) -> dict:
    """路由器路径：绑定物理网卡 `iface`，DNS 直接问 `dns_server`（路由器/DHCP 给
    的 DNS），拿到解析结果后同一张网卡再做 TCP/TLS/HEAD——绕开 Clash TUN、系统
    resolver 缓存，看路由器自己怎么解析、怎么放行这个域名。
    """
    t0 = time.monotonic()
    try:
        ips, dns_ms = _dns_query_router(host, dns_server, iface, timeout=timeout)
    except (OSError, ValueError) as exc:
        return {"status": "dns_fail", "error": str(exc), "dns_ms": _now_ms(t0)}
    if not ips:
        return {"status": "dns_fail", "error": "路由器未返回 A 记录", "dns_ms": dns_ms}
    return _finish_probe(host, ips[0], port, dns_ms, timeout, bind_iface=iface)


def _lookup_clash_connection(
    clash_api: tuple[str, Optional[str]], host: str, timeout: float, *, attempts: int = 3, retry_delay: float = 0.1,
) -> Optional[dict]:
    """探测连接的 CONNECT 一成功，立刻去 mihomo 控制接口的 `/connections` 查
    这个 host 命中了哪条规则、走了哪条节点链——这是判断"为什么不通"的关键信息
    （比如 platform.claude.com 没配进 DOMAIN-SUFFIX 规则，落到"漏网之鱼"
    分组，而那个分组选的是 DIRECT，TLS 就可能失败）。查询发生在我们自己的
    探测连接还开着的时候，这条连接理应已经在 mihomo 的连接列表里；`host` 相同
    的连接可能不止一条（同一域名先前的连接还没关），取最后一条（最新建立的，
    大概率就是我们自己这次探测）。查不到、mihomo 控制接口不通都不算错误，
    只是拿不到这份补充信息，返回 None。

    已知局限（实测确认，不是这份代码的 bug）：mihomo 只有在**真正拨通目标
    出站连接**之后才会把它登记进 `/connections`；如果目标恰好连不上（DIRECT
    出站卡死/一直不返回，比如域名没配进任何 DOMAIN-SUFFIX 规则、落到"漏网之
    鱼"分组又选了 DIRECT 这种典型场景），TLS 握手会一直卡到超时，而这条
    连接全程都不会出现在 `/connections` 里——也就是说，恰恰是最需要看
    "命中了哪条规则"的这类失败场景，这个办法可能查不到。目标连得通时（哪怕
    最终 HTTP 状态码不是 200）这份信息是可靠的；进一步能覆盖"连不通"这类
    场景的办法是接 mihomo 的 `/logs` 实时日志流（规则匹配在日志里发生得比
    拨号早，不依赖连接是否拨通），本次没有实现，是已知待改进项。
    """
    base_url, secret = clash_api
    # 连接刚 CONNECT 成功那一刻，mihomo 可能还没把它登记进 /connections
    # （内核要先真正拨通出站才会上榜，探测目标恰好不通时这一步本身就慢），
    # 短暂重试几次比查一次就放弃靠谱得多；总共多等的时间在探测的 timeout
    # 预算内可以忽略不计。
    for attempt in range(attempts):
        try:
            data = _mihomo_get(base_url, "/connections", secret, timeout)
        except Exception:  # noqa: BLE001 - 补充信息拿不到不该影响探测本身的结果
            return None
        matches = [c for c in (data.get("connections") or []) if (c.get("metadata") or {}).get("host") == host]
        if matches:
            c = matches[-1]
            return {"rule": c.get("rule"), "rule_payload": c.get("rulePayload"), "chains": c.get("chains")}
        if attempt < attempts - 1:
            time.sleep(retry_delay)
    return None


def probe_clash(
    host: str, *, proxy_host: str, proxy_port: int, port: int = _HTTPS_PORT, timeout: float = _TIMEOUT_DEFAULT,
    clash_api: Optional[tuple[str, Optional[str]]] = None,
) -> dict:
    """经 mihomo 的 http/mixed 端口发 `CONNECT`，隧道建立后在里面做 TLS/HEAD——
    看 Clash 自己的出站（不管它内部选了直连还是代理规则）通不通。只有 mihomo 在
    跑（调用方传了 `clash_proxy`）才会走到这里。

    `clash_api` 给了（mihomo 控制接口的 `(base_url, secret)`）就额外查一次
    这个 host 命中的规则/节点链（见 `_lookup_clash_connection`），结果并进
    返回值的 `rule`/`rule_payload`/`chains`；不给就没有这几个字段。

    这份查询跟接下来的 TLS 握手在一个后台线程里**并发**跑，不是查完了再做
    TLS——mihomo 只有在真正拨通出站之后才会把连接登记进 `/connections`，
    如果目标本身连不通（正是最值得诊断的情况！），TLS 握手会一直卡到
    `timeout`，连接查询就有了同样长的时间窗口去反复重试，不需要额外等待。
    """
    t0 = time.monotonic()
    try:
        sock = socket.create_connection((proxy_host, proxy_port), timeout=timeout)
    except ConnectionRefusedError as exc:
        return {"status": "connect_refused", "error": str(exc), "connect_ms": _now_ms(t0)}
    except OSError as exc:
        return {"status": "connect_timeout", "error": str(exc), "connect_ms": _now_ms(t0)}
    connect_ms = _now_ms(t0)
    try:
        sock.settimeout(timeout)
        req = f"CONNECT {host}:{port} HTTP/1.1\r\nHost: {host}:{port}\r\n\r\n".encode()
        sock.sendall(req)
        resp = sock.recv(4096)
        status_line = resp.split(b"\r\n", 1)[0]
        if b" 200 " not in b" " + status_line:
            return {"status": "connect_refused", "error": status_line.decode("latin-1", "replace"),
                    "connect_ms": connect_ms}
    except socket.timeout:
        return {"status": "connect_timeout", "error": "CONNECT 超时", "connect_ms": connect_ms}
    except OSError as exc:
        return {"status": "connect_timeout", "error": str(exc), "connect_ms": connect_ms}

    rule_holder: dict = {}
    lookup_thread = None
    if clash_api:
        retry_delay = 0.2
        attempts = max(1, int(timeout / retry_delay))

        def _do_lookup() -> None:
            info = _lookup_clash_connection(clash_api, host, timeout, attempts=attempts, retry_delay=retry_delay)
            if info:
                rule_holder.update(info)

        lookup_thread = threading.Thread(target=_do_lookup, daemon=True)
        lookup_thread.start()

    result = _finish_tls_head(sock, host, t0, connect_ms, None, timeout)
    if lookup_thread is not None:
        lookup_thread.join(timeout=0.5)  # TLS 都跑完了，给查询线程一点收尾时间
    if rule_holder:
        result.update(rule_holder)
    return result


def _safe_probe(fn: Callable[..., dict], *args, **kwargs) -> dict:
    """采集器约定：探测函数本身不该抛出去，这里再兜一层，防止某条路径的实现
    细节（比如 `socket.if_nametoindex` 在网卡消失的一瞬间抛 OSError）拖垮整批。
    """
    try:
        return fn(*args, **kwargs)
    except Exception as exc:  # noqa: BLE001
        return {"status": "error", "error": str(exc)}


# ---------------------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------------------


def _representative_pid(members: Sequence[int], main_pids: set[int]) -> int:
    """一组成员 pid 里挑一个代表（展示用的 pid/cwd/command）：优先挑主进程
    （见 `_is_main_process`），没有主进程（比如子进程还没归属完）就随便挑一个
    确定性的最小 pid，保证同一份快照多次格式化结果一致。
    """
    candidates = [p for p in members if p in main_pids] or list(members)
    return min(candidates)


def collect_apps(
    apps: Sequence[dict],
    *,
    owners: Optional[dict] = None,
    router_iface: Optional[str] = None,
    router_dns: Optional[str] = None,
    clash_proxy: Optional[tuple[str, int]] = None,
    clash_api: Optional[tuple[str, Optional[str]]] = None,
    timeout: float = _TIMEOUT_DEFAULT,
    max_workers: int = 8,
    process_lister: Callable[[], list[dict]] = list_processes,
    process_detail_lister: Callable[[], dict[int, dict]] = list_process_details,
    connection_lister: Callable[..., dict[int, list[tuple[str, int]]]] = list_established_connections,
    cwd_lister: Callable[[Sequence[int]], dict[int, str]] = cwd_of_pids,
) -> dict:
    """按应用采集：每个应用当前有哪些进程实例、各自的连接出口分布，以及代表
    域名的 system/router/clash 三条路径连通性。

    `apps` 每项至少 `{"name", "process": [...], "domains": [...]}`。进程实例
    按 `_instance_key` 分组：挂在终端上的按 tty 分组（同一应用在不同终端窗口
    各起一份，各自独立成实例），没挂终端的图形界面应用（及其一堆 Helper）
    全部合并成一个 `"background"` 实例——图形界面应用没有"哪个窗口"的概念，
    这样才不会把 Safari/微信/Chrome 的多个 Helper 进程拆成好几个"实例"。
    "运行中"只看主进程是否存在（`match_processes`），Helper 单独活着不算数。

    联网但自己没匹配到任何应用的进程（比如 Claude Code 调用的 `git`），沿父
    进程链找到属于哪个应用的哪个实例后，归到该实例下、标 `child:<命令名>`；
    找不到归属、但挂在某个终端（tty 不是 `??`）上的联网进程，归入返回值的
    `terminal_other`（按命令名汇总，不区分具体是哪个应用调的）。

    `router_iface`/`router_dns` 不给就跳过 router 路径；`clash_proxy`
    （`(host, port)`）不给就跳过 clash 路径——调用方自己先判断 mihomo 在不在
    跑，不在跑就不传，不是这里瞎猜。`clash_api` 给了就额外把 clash 路径命中的
    规则/节点链也记下来（见 `probe_clash`）。同一个域名被多个应用共用时只
    探测一次，且域名探测结果是**应用级别**的一份，不挂在具体某个实例下
    （实例可以有好几个，但"这个应用的代表域名通不通"是应用整体的属性）。
    """
    try:
        processes = process_lister()
    except Exception as exc:  # noqa: BLE001
        return _fail(f"枚举进程失败：{exc}")
    proc_matches = match_processes(apps, processes)

    try:
        details = process_detail_lister()
    except Exception:  # noqa: BLE001 - tty/父进程信息拿不到不影响进程匹配本身
        details = {}

    try:
        connections = connection_lister()
    except Exception:  # noqa: BLE001 - 连接列表拿不到不算致命，出口统计留空即可
        connections = {}

    clash_ports = (clash_proxy[1],) if clash_proxy else ()
    route_cache: dict[str, str] = {}

    # 第一步：每个应用把自己匹配到的 pid 按 `_instance_key`（tty 或
    # "background"）分组成实例
    pid_to_owner: dict[int, tuple[str, str]] = {}  # pid -> (app_name, 实例 key)
    app_groups: dict[str, dict[str, list[int]]] = {}
    for app in apps:
        name = app["name"]
        pids = proc_matches.get(name, {}).get("pids", [])
        groups: dict[str, list[int]] = {}
        for pid in pids:
            key = _instance_key(pid, details)
            groups.setdefault(key, []).append(pid)
            pid_to_owner[pid] = (name, key)
        app_groups[name] = groups

    # 代表 pid（展示用）才需要查 cwd，不用对分组里每个成员都查一遍
    representative_pids = {
        name: {
            key: _representative_pid(members, set(proc_matches.get(name, {}).get("main_pids", [])))
            for key, members in groups.items()
        }
        for name, groups in app_groups.items()
    }
    cwd_targets = [pid for reps in representative_pids.values() for pid in reps.values()]
    try:
        cwd_map = cwd_lister(cwd_targets) if cwd_targets else {}
    except Exception:  # noqa: BLE001 - cwd 拿不到不是致命错误
        cwd_map = {}

    # 第二步：联网但自己没被任何应用匹配到的进程，尝试沿父进程链归属到某个应用实例
    child_owner: dict[int, tuple[str, str, str]] = {}  # pid -> (app_name, 实例 key, 自己的命令名)
    for pid in connections:
        if pid in pid_to_owner or not connections.get(pid):
            continue
        owner = _find_owning_app(pid, details, pid_to_owner)
        if owner is not None:
            child_owner[pid] = owner

    # 第三步：域名探测（跟具体是哪个实例无关，应用级别一份代表域名）
    domain_to_hosts: set[str] = set()
    for app in apps:
        domain_to_hosts.update(app.get("domains") or [])

    def probe_one(host: str) -> tuple[str, dict]:
        paths: dict[str, dict] = {"system": _safe_probe(probe_system, host, timeout=timeout)}
        if router_iface and router_dns:
            paths["router"] = _safe_probe(
                probe_router, host, iface=router_iface, dns_server=router_dns, timeout=timeout,
            )
        if clash_proxy:
            paths["clash"] = _safe_probe(
                probe_clash, host, proxy_host=clash_proxy[0], proxy_port=clash_proxy[1], timeout=timeout,
                clash_api=clash_api,
            )
        return host, paths

    domain_results: dict[str, dict] = {}
    if domain_to_hosts:
        with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers) as pool:
            for host, paths in pool.map(probe_one, sorted(domain_to_hosts)):
                domain_results[host] = paths

    # 第四步：组装每个应用的实例列表（自身出口 + 归属给它的子进程出口）
    result_apps = []
    for app in apps:
        name = app["name"]
        match = proc_matches.get(name, {"running": False, "pids": [], "paths": []})
        instances = []
        for key, members in app_groups.get(name, {}).items():
            rep = representative_pids[name][key]
            exits = app_exits(members, connections, owners, clash_ports, route_cache=route_cache)
            for cpid, (capp, ckey, ccomm) in child_owner.items():
                if capp == name and ckey == key:
                    label = f"child:{ccomm}"
                    exits[label] = exits.get(label, 0) + len(connections.get(cpid, []))
            info = details.get(rep, {})
            instances.append({
                "id": key,
                "pid": rep,
                "tty": None if key == _BACKGROUND_KEY else key,
                "cwd": cwd_map.get(rep),
                "command": info.get("command"),
                "member_pids": sorted(members),
                "exits": exits,
            })
        # background（图形界面应用）排最前，其余按 tty 名字排序，保证同一份
        # 快照多次格式化顺序稳定
        instances.sort(key=lambda i: (i["id"] != _BACKGROUND_KEY, i["id"]))
        result_apps.append({
            "name": name,
            "running": match["running"],
            "pid_count": len(match["pids"]),
            "paths": match["paths"],
            "instances": instances,
            "domains": {host: domain_results.get(host, {}) for host in (app.get("domains") or [])},
        })

    # 第五步："终端其他"：挂在某个终端上、联网、但既不属于任何应用也不是已知
    # 应用子进程的进程，按命令名汇总——用户在终端里手敲的裸 curl/ssh 之类。
    covered = set(pid_to_owner) | set(child_owner)
    terminal_other: dict[str, dict] = {}
    for pid, conns in connections.items():
        if pid in covered or not conns:
            continue
        info = details.get(pid, {})
        tty = info.get("tty")
        if not tty or tty == "??":
            continue
        comm = _own_command_name(info.get("command", "")) or str(pid)
        bucket = terminal_other.setdefault(comm, {"count": 0, "instances": []})
        bucket["count"] += 1
        exits = app_exits([pid], connections, owners, clash_ports, route_cache=route_cache)
        bucket["instances"].append({"pid": pid, "tty": tty, "cwd": None, "exits": exits})

    return _ok(apps=result_apps, terminal_other=terminal_other)
