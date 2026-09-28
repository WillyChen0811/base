"""SCDynamicStore（`scutil`）系统网络配置采集：State:/Setup: 全量键值 + `--nwi` 摘要。

用户关心的是"系统的网络配置本身"——不同场景下是什么、何时变了，用来判断
"是我的操作没生效，还是系统/某软件改了配置"。这两个采集器只读 `scutil`
（list/show/--nwi），不 sudo、不改任何设置。

返回约定跟 `collectors.py` 一致：永不抛异常，成功 `_ok(...)`，失败
`{"error": "...", ...}`；这里直接复用 `collectors._ok`/`_fail` 保持信封形状
一致，方便 `Recorder`/`diff` 无差别处理。
"""
from __future__ import annotations

import platform
import re
import subprocess
from typing import Optional, Sequence

from .collectors import _fail, _ok
from ...media._proc import MediaError, ToolNotFoundError, resolve_tool, run

_TIMEOUT_DEFAULT = 5.0


def _run_scutil_script(tool: str, script: str, timeout: float) -> subprocess.CompletedProcess:
    """把整段命令一次性喂给 `scutil` 的交互模式（stdin），一次调用批量执行完。

    `scutil` 没有非交互的"传参跑一条命令"模式，`list`/`show` 只能靠 stdin；
    `media._proc.run()` 不支持传 stdin，这里用标准库 `subprocess.run(input=...)`
    ——它内部用线程/select 同时喂 stdin、收 stdout/stderr，大输出（本机 147 个
    键的全量 dump）不会有管道互相等待的死锁风险。超时统一转成 `MediaError`，
    跟 collectors.py 其余分支的异常类型保持一致，调用方只需捕一种异常。
    """
    try:
        return subprocess.run(
            [tool],
            input=script,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=timeout,
        )
    except subprocess.TimeoutExpired as exc:
        raise MediaError(f"scutil 超时（{timeout}s）") from exc


# ---------- dynamic store (list + show) ----------

_SUBKEY_RE = re.compile(r"^\s*subKey \[\d+\] = (.+?)\s*$")
_NO_SUCH_KEY = "No such key"


def _scutil_scalar(rest: str) -> Optional[str]:
    """非容器值：`<data>` 保留十六进制字符串原样，其余按原文本（可能是空串）。"""
    if rest.startswith("<data>"):
        return rest[len("<data>"):].strip()
    return rest


def _parse_scutil_container(lines: list[str], i: int, *, is_array: bool):
    """解析 `<dictionary> { ... }` / `<array> { ... }` 内部，按 `key : value` 逐行读，
    遇到 `<dictionary> {`/`<array> {` 递归，遇到 `}` 收尾。支持任意深度嵌套
    （scutil 实测里 Proxies 的 `__SCOPED__`、路由的 `AdditionalRoutes` 数组元素
    都是字典套字典/数组套字典）。数组条目的 key（下标）本身不使用，只按出现
    顺序 append，顺序即 scutil 给出的顺序。
    """
    result: object = [] if is_array else {}
    while i < len(lines):
        stripped = lines[i].strip()
        if stripped == "}":
            return result, i + 1
        if not stripped:
            i += 1
            continue
        key, _, rest = stripped.partition(":")
        key, rest = key.strip(), rest.strip()
        if rest == "<dictionary> {":
            value, i = _parse_scutil_container(lines, i + 1, is_array=False)
        elif rest == "<array> {":
            value, i = _parse_scutil_container(lines, i + 1, is_array=True)
        else:
            value = _scutil_scalar(rest)
            i += 1
        if is_array:
            result.append(value)
        else:
            result[key] = value
    return result, i


def _parse_scutil_top_value(lines: list[str], i: int):
    """解析一次 `show <key>` 的完整输出：`<dictionary>`/`<array>` 走容器解析；
    `No such key`（键不存在）解析成 `None`；其余当标量整行返回。
    """
    if i >= len(lines):
        return None, i
    stripped = lines[i].strip()
    if stripped == "<dictionary> {":
        return _parse_scutil_container(lines, i + 1, is_array=False)
    if stripped == "<array> {":
        return _parse_scutil_container(lines, i + 1, is_array=True)
    if stripped == _NO_SUCH_KEY:
        return None, i + 1
    return _scutil_scalar(stripped), i + 1


def collect_dynamic_store(
    *,
    patterns: Sequence[str] = ("State:/Network/.*", "Setup:/Network/.*"),
    timeout: float = _TIMEOUT_DEFAULT,
) -> dict:
    """SCDynamicStore 里匹配 `patterns` 的全部键值（`list` 拿键名，批量 `show`
    拿值），解析成 `{key: value}`。两次 `scutil` 调用（一次 list、一次批量
    show），不管键有多少都不会起上百个子进程。

    键名里的服务 UUID（`Setup:/Network/Service/<uuid>`）原样保留——它们能指向
    具体网络服务/VPN，是"谁改的"线索，不做任何归一化/翻译（翻译交给
    `service_names`）。
    """
    if platform.system() != "Darwin":
        return _fail("unsupported platform")
    try:
        tool = resolve_tool(None, "scutil")
    except ToolNotFoundError as exc:
        return _fail(str(exc))

    list_script = "".join(f"list {p}\n" for p in patterns) + "quit\n"
    try:
        list_result = _run_scutil_script(tool, list_script, timeout)
    except MediaError as exc:
        return _fail(str(exc))
    if list_result.returncode != 0:
        return _fail(f"scutil 退出码 {list_result.returncode}：{list_result.stderr.strip()[:500]}")

    # 去重保序：多个 pattern 可能命中同一个键
    keys = list(dict.fromkeys(m.group(1) for m in map(_SUBKEY_RE.match, list_result.stdout.splitlines()) if m))
    if not keys:
        return _ok(keys={}, count=0)

    show_script = "".join(f"show {k}\n" for k in keys) + "quit\n"
    try:
        show_result = _run_scutil_script(tool, show_script, timeout)
    except MediaError as exc:
        return _fail(str(exc))
    if show_result.returncode != 0:
        return _fail(f"scutil 退出码 {show_result.returncode}：{show_result.stderr.strip()[:500]}")

    lines = show_result.stdout.splitlines()
    i = 0
    store: dict = {}
    for key in keys:
        value, i = _parse_scutil_top_value(lines, i)
        store[key] = value
    return _ok(keys=store, count=len(store))


_SERVICE_KEY_RE = re.compile(r"^Setup:/Network/Service/(?P<uuid>[0-9A-Fa-f-]+)$")


def service_names(store: dict) -> dict:
    """从 `collect_dynamic_store()` 返回的 `keys` 里取 `Setup:/Network/Service/<uuid>`
    的 `UserDefinedName`，得到 `{uuid: 用户可见服务名}`，方便展示/diff 时把 UUID
    翻成人话。传入的是 `keys` 字段本身，不是外层带 `error` 的信封。
    """
    names: dict = {}
    for key, value in store.items():
        m = _SERVICE_KEY_RE.match(key)
        if not m or not isinstance(value, dict):
            continue
        name = value.get("UserDefinedName")
        if name:
            names[m.group("uuid")] = name
    return names


# ---------- nwi (scutil --nwi) ----------

_NWI_SECTION_RE = re.compile(r"^(?P<family>IPv[46]) network interface information")
_NWI_NO_STATES_RE = re.compile(r"^\s*No IPv[46] states found\s*$")
_NWI_REACH_RE = re.compile(r"^\s*REACH\s*:\s*flags\s+(?P<value>\S+)\s*\((?P<names>[^)]*)\)\s*$")
_NWI_HEADER_RE = re.compile(r"^\s*(?P<name>\S+)\s*:\s*flags\s*:\s*(?P<value>\S+)\s*\((?P<names>[^)]*)\)\s*$")
_NWI_FIELD_RE = re.compile(r"^\s+(?P<key>address|VPN server|server address|reach)\s*:\s*(?P<value>.*)$")
_NWI_IFACES_RE = re.compile(r"^Network interfaces:\s*(?P<names>.*)$")


def _flag_names(csv: str) -> list[str]:
    return [n.strip() for n in csv.split(",") if n.strip()]


def collect_nwi(*, timeout: float = _TIMEOUT_DEFAULT) -> dict:
    """`scutil --nwi`：IPv4/IPv6 各自网卡采用顺序（列表顺序即系统采用顺序）、
    flags/address/VPN server/reach，以及末尾的总接口列表。
    """
    if platform.system() != "Darwin":
        return _fail("unsupported platform")
    try:
        tool = resolve_tool(None, "scutil")
        run_result = run([tool, "--nwi"], timeout=timeout)  # 复用 collectors.py 同款超时/取消轮询写法
    except (ToolNotFoundError, MediaError) as exc:
        return _fail(str(exc))
    if run_result.returncode != 0:
        return _fail(f"scutil 退出码 {run_result.returncode}：{run_result.stderr.strip()[:500]}")

    families = {"IPv4": {"interfaces": [], "reach": None}, "IPv6": {"interfaces": [], "reach": None}}
    current_family: Optional[str] = None
    current_if: Optional[dict] = None
    all_interfaces: list[str] = []

    for line in run_result.stdout.splitlines():
        m = _NWI_SECTION_RE.match(line)
        if m:
            current_family, current_if = m.group("family"), None
            continue
        m = _NWI_IFACES_RE.match(line)
        if m:
            all_interfaces = m.group("names").split()
            current_family = current_if = None
            continue
        if current_family is None:
            continue
        if _NWI_NO_STATES_RE.match(line):
            current_if = None
            continue
        m = _NWI_REACH_RE.match(line)
        if m:
            families[current_family]["reach"] = {"value": m.group("value"), "names": _flag_names(m.group("names"))}
            current_if = None
            continue
        m = _NWI_HEADER_RE.match(line)
        if m:
            current_if = {
                "name": m.group("name"),
                "flags": {"value": m.group("value"), "names": _flag_names(m.group("names"))},
                "address": None,
                "vpn_server": None,
                "reach": None,
            }
            families[current_family]["interfaces"].append(current_if)
            continue
        m = _NWI_FIELD_RE.match(line)
        if m and current_if is not None:
            key, value = m.group("key"), m.group("value").strip()
            if key == "address":
                current_if["address"] = value
            elif key in ("VPN server", "server address"):
                current_if["vpn_server"] = value
            elif key == "reach":
                fm = re.match(r"^(?P<value>\S+)\s*\((?P<names>[^)]*)\)\s*$", value)
                current_if["reach"] = (
                    {"value": fm.group("value"), "names": _flag_names(fm.group("names"))} if fm else {"value": value, "names": []}
                )

    return _ok(ipv4=families["IPv4"], ipv6=families["IPv6"], interfaces=all_interfaces)
