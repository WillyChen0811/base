"""WireGuard 配置模型、wg-quick 风格 `.conf` 解析/生成、密钥生成、UAPI 客户端。

不内置任何网段/接口名/socket 路径等业务默认值——都由调用方传入或从配置文件读出。

X25519（生成私钥/由私钥推公钥）需要 `cryptography`（extra 名 `net`），顶层不 import，
只在对应函数里延迟 import；预共享密钥只是 32 字节随机数，`secrets` 够用，不需要该依赖。
"""
from __future__ import annotations

import base64
import ipaddress
import re
import secrets
import socket
from dataclasses import dataclass, field
from typing import Sequence


class WireGuardConfigError(ValueError):
    """`.conf` 解析/校验失败，或配置模型字段不合法。"""


class UapiError(RuntimeError):
    """UAPI（wireguard-go 跨平台配置协议）请求返回非 0 errno，或响应格式不对。"""


# ---------- 配置模型 ----------
#
# Address/AllowedIPs/DNS/Endpoint 都保留调用方给的原始字符串（只做校验、不做归一化），
# 这样 parse_conf -> dump_conf 往返是语义稳定的（用同一份 ipaddress 对象比较，不比字节）。

@dataclass(frozen=True)
class Interface:
    private_key: str
    addresses: tuple[str, ...] = ()
    listen_port: int | None = None
    mtu: int | None = None
    dns: tuple[str, ...] = ()
    # wg-quick 专有键（PostUp/PreUp/PostDown/PreDown/Table/SaveConfig/FwMark 等）：
    # 原样保留、不执行，key 保留原始大小写，value 按出现顺序去掉了逗号切分（这些键
    # 本身语义上不是"多值列表"，没必要拆）。dump_conf 会原样写回。
    extras: dict[str, tuple[str, ...]] = field(default_factory=dict)


@dataclass(frozen=True)
class Peer:
    public_key: str
    allowed_ips: tuple[str, ...] = ()
    endpoint: str | None = None
    preshared_key: str | None = None
    persistent_keepalive: int | None = None
    extras: dict[str, tuple[str, ...]] = field(default_factory=dict)


@dataclass(frozen=True)
class WireGuardConfig:
    interface: Interface
    peers: tuple[Peer, ...] = ()


# ---------- 校验小工具 ----------

def _validate_key(name: str, value: str) -> str:
    try:
        raw = base64.b64decode(value, validate=True)
    except Exception as exc:  # noqa: BLE001 - 统一包成配置错误
        raise WireGuardConfigError(f"{name} 不是合法的 base64：{value!r}") from exc
    if len(raw) != 32:
        raise WireGuardConfigError(f"{name} 解码后应为 32 字节，实际 {len(raw)} 字节：{value!r}")
    return value


def _parse_int(name: str, value: str) -> int:
    try:
        return int(value)
    except ValueError as exc:
        raise WireGuardConfigError(f"{name} 不是整数：{value!r}") from exc


def _parse_keepalive(value: str) -> int | None:
    if value.strip().lower() == "off":
        return None
    n = _parse_int("PersistentKeepalive", value)
    if not (0 <= n <= 65535):
        raise WireGuardConfigError(f"PersistentKeepalive 超出范围（0-65535）：{value!r}")
    return n


def _split_csv(values: Sequence[str]) -> list[str]:
    out: list[str] = []
    for v in values:
        out.extend(p.strip() for p in v.split(",") if p.strip())
    return out


def _validate_addresses(values: Sequence[str]) -> tuple[str, ...]:
    tokens = _split_csv(values)
    for t in tokens:
        try:
            ipaddress.ip_interface(t)
        except ValueError as exc:
            raise WireGuardConfigError(f"Address 不是合法的地址/网段：{t!r}") from exc
    return tuple(tokens)


def _validate_networks(values: Sequence[str]) -> tuple[str, ...]:
    tokens = _split_csv(values)
    for t in tokens:
        try:
            ipaddress.ip_network(t, strict=False)
        except ValueError as exc:
            raise WireGuardConfigError(f"AllowedIPs 不是合法的网段：{t!r}") from exc
    return tuple(tokens)


def split_endpoint(value: str) -> tuple[str, int]:
    """把 `host:port` / `[ipv6]:port` 拆成 `(host, port)`；host 可以是主机名。"""
    if value.startswith("["):
        if "]:" not in value:
            raise WireGuardConfigError(f"Endpoint 格式不对（IPv6 需要 [addr]:port）：{value!r}")
        host, _, port_s = value[1:].partition("]:")
    else:
        if ":" not in value:
            raise WireGuardConfigError(f"Endpoint 缺少端口：{value!r}")
        host, _, port_s = value.rpartition(":")
    try:
        port = int(port_s)
    except ValueError as exc:
        raise WireGuardConfigError(f"Endpoint 端口不是整数：{value!r}") from exc
    return host, port


def _validate_endpoint(value: str) -> str:
    host, port = split_endpoint(value)
    if not host:
        raise WireGuardConfigError(f"Endpoint 缺少主机部分：{value!r}")
    if not (0 < port <= 65535):
        raise WireGuardConfigError(f"Endpoint 端口不合法：{value!r}")
    return value


# ---------- .conf 解析/生成 ----------

_INTERFACE_KEYS = {"privatekey", "address", "listenport", "mtu", "dns"}
_PEER_KEYS = {"publickey", "presharedkey", "allowedips", "endpoint", "persistentkeepalive"}
_SECTION_RE = re.compile(r"\[\s*(\w+)\s*\]")


def parse_conf(text: str) -> WireGuardConfig:
    """解析 wg-quick 风格的 `.conf`：键大小写不敏感、逗号分隔多值、`#`/`;` 整行注释、
    可以有多个 `[Peer]`。wg-quick 专有键（不参与隧道语义、只是给 wg-quick 脚本用的）
    原样存进 `extras`，不解释、不执行。
    """
    section: str | None = None
    iface_kv: dict[str, list[str]] = {}
    iface_extras: dict[str, list[str]] = {}
    peers_raw: list[tuple[dict[str, list[str]], dict[str, list[str]]]] = []
    cur_kv: dict[str, list[str]] | None = None
    cur_extras: dict[str, list[str]] | None = None

    for lineno, raw in enumerate(text.splitlines(), start=1):
        line = raw.strip()
        if not line or line.startswith("#") or line.startswith(";"):
            continue
        header = _SECTION_RE.fullmatch(line)
        if header:
            name = header.group(1).lower()
            if name == "interface":
                section = "interface"
            elif name == "peer":
                if cur_kv is not None:
                    peers_raw.append((cur_kv, cur_extras))  # type: ignore[arg-type]
                cur_kv, cur_extras = {}, {}
                section = "peer"
            else:
                raise WireGuardConfigError(f"第 {lineno} 行：未知节 [{header.group(1)}]")
            continue
        if section is None:
            raise WireGuardConfigError(f"第 {lineno} 行：内容出现在 [Interface]/[Peer] 之前")
        if "=" not in line:
            raise WireGuardConfigError(f"第 {lineno} 行：不是 `key = value` 形式：{line!r}")
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip()
        key_norm = key.lower().replace("_", "")
        if section == "interface":
            if key_norm in _INTERFACE_KEYS:
                iface_kv.setdefault(key_norm, []).append(value)
            else:
                iface_extras.setdefault(key, []).append(value)
        else:
            assert cur_kv is not None and cur_extras is not None
            if key_norm in _PEER_KEYS:
                cur_kv.setdefault(key_norm, []).append(value)
            else:
                cur_extras.setdefault(key, []).append(value)

    if cur_kv is not None:
        peers_raw.append((cur_kv, cur_extras))  # type: ignore[arg-type]
    if section is None:
        raise WireGuardConfigError("空配置：没有 [Interface] 节")
    if "privatekey" not in iface_kv:
        raise WireGuardConfigError("[Interface] 缺少 PrivateKey")

    interface = Interface(
        private_key=_validate_key("PrivateKey", iface_kv["privatekey"][0]),
        addresses=_validate_addresses(iface_kv.get("address", [])),
        listen_port=_parse_int("ListenPort", iface_kv["listenport"][0]) if "listenport" in iface_kv else None,
        mtu=_parse_int("MTU", iface_kv["mtu"][0]) if "mtu" in iface_kv else None,
        dns=tuple(_split_csv(iface_kv.get("dns", []))),
        extras={k: tuple(v) for k, v in iface_extras.items()},
    )

    peers = []
    for kv, extras in peers_raw:
        if "publickey" not in kv:
            raise WireGuardConfigError("[Peer] 缺少 PublicKey")
        peers.append(
            Peer(
                public_key=_validate_key("PublicKey", kv["publickey"][0]),
                allowed_ips=_validate_networks(kv.get("allowedips", [])),
                endpoint=_validate_endpoint(kv["endpoint"][0]) if "endpoint" in kv else None,
                preshared_key=_validate_key("PresharedKey", kv["presharedkey"][0])
                if "presharedkey" in kv else None,
                persistent_keepalive=_parse_keepalive(kv["persistentkeepalive"][0])
                if "persistentkeepalive" in kv else None,
                extras={k: tuple(v) for k, v in extras.items()},
            )
        )
    return WireGuardConfig(interface=interface, peers=tuple(peers))


def dump_conf(cfg: WireGuardConfig) -> str:
    """生成 wg-quick 兼容的 `.conf` 文本。按固定顺序输出已知键，extras 原样跟在后面。"""
    lines: list[str] = ["[Interface]", f"PrivateKey = {cfg.interface.private_key}"]
    if cfg.interface.addresses:
        lines.append(f"Address = {', '.join(cfg.interface.addresses)}")
    if cfg.interface.listen_port is not None:
        lines.append(f"ListenPort = {cfg.interface.listen_port}")
    if cfg.interface.mtu is not None:
        lines.append(f"MTU = {cfg.interface.mtu}")
    if cfg.interface.dns:
        lines.append(f"DNS = {', '.join(cfg.interface.dns)}")
    for key, values in cfg.interface.extras.items():
        for v in values:
            lines.append(f"{key} = {v}")

    for peer in cfg.peers:
        lines.append("")
        lines.append("[Peer]")
        lines.append(f"PublicKey = {peer.public_key}")
        if peer.preshared_key:
            lines.append(f"PresharedKey = {peer.preshared_key}")
        if peer.allowed_ips:
            lines.append(f"AllowedIPs = {', '.join(peer.allowed_ips)}")
        if peer.endpoint:
            lines.append(f"Endpoint = {peer.endpoint}")
        if peer.persistent_keepalive is not None:
            lines.append(f"PersistentKeepalive = {peer.persistent_keepalive}")
        for key, values in peer.extras.items():
            for v in values:
                lines.append(f"{key} = {v}")
    return "\n".join(lines) + "\n"


# ---------- 密钥 ----------

def generate_private_key() -> str:
    """生成一个 X25519 私钥（base64）。需要 `pip install "pybase[net]"`。"""
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey

    priv = X25519PrivateKey.generate()
    raw = priv.private_bytes(
        serialization.Encoding.Raw, serialization.PrivateFormat.Raw, serialization.NoEncryption()
    )
    return base64.b64encode(raw).decode()


def public_key_of(private_key: str) -> str:
    """由私钥（base64）推出对应公钥（base64）。需要 `pip install "pybase[net]"`。"""
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey

    raw = base64.b64decode(_validate_key("private_key", private_key))
    priv = X25519PrivateKey.from_private_bytes(raw)
    pub = priv.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    return base64.b64encode(pub).decode()


def generate_preshared_key() -> str:
    """生成一个预共享密钥（base64，32 字节随机数）。不需要 `cryptography`。"""
    return base64.b64encode(secrets.token_bytes(32)).decode()


# ---------- UAPI（wireguard-go 跨平台配置协议） ----------
#
# 协议见 wireguard-go 的 cross-platform UAPI：纯文本、以空行结束一次请求/响应。
# set：`set=1` 开头，逐行 `key=value`（密钥是 hex，不是 base64！），
#      `replace_peers=true` 清空旧 peer 列表，每个 peer 用 `public_key=` 另起一段，
#      段内 `replace_allowed_ips=true` + 多行 `allowed_ip=`；以空行结束，
#      响应是单行 `errno=N` + 空行。
# get：`get=1` + 空行；响应是逐行 `key=value`（同样 public_key= 分段），
#      以 `errno=0`（或非 0）结尾 + 空行。

def _key_to_hex(b64_key: str) -> str:
    return base64.b64decode(b64_key).hex()


def _hex_to_b64(hex_key: str) -> str:
    return base64.b64encode(bytes.fromhex(hex_key)).decode()


def default_uapi_path(ifname: str, *, uapi_dir: str | None = None) -> str:
    base = uapi_dir or "/var/run/wireguard"
    return f"{base}/{ifname}.sock"


def connect_uapi(path: str, *, timeout: float = 5.0) -> socket.socket:
    """连到 wireguard-go 的 UAPI unix socket。"""
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(timeout)
    sock.connect(path)
    return sock


def _read_until_blank(sock: socket.socket) -> str:
    """UAPI 响应以空行（`\n\n`）结束，一直读到看见它为止。"""
    buf = b""
    while b"\n\n" not in buf:
        chunk = sock.recv(4096)
        if not chunk:
            break
        buf += chunk
    return buf.decode("utf-8", errors="replace")


def _parse_errno(text: str) -> int:
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("errno="):
            return int(line.partition("=")[2])
    raise UapiError(f"响应里没有 errno 行：{text!r}")


def uapi_set(sock: socket.socket, cfg: WireGuardConfig) -> None:
    """把配置整体下发给 wireguard-go（`replace_peers`，即完全替换现有 peer 列表）。"""
    lines = ["set=1", f"private_key={_key_to_hex(cfg.interface.private_key)}"]
    if cfg.interface.listen_port is not None:
        lines.append(f"listen_port={cfg.interface.listen_port}")
    lines.append("replace_peers=true")
    for peer in cfg.peers:
        lines.append(f"public_key={_key_to_hex(peer.public_key)}")
        if peer.preshared_key:
            lines.append(f"preshared_key={_key_to_hex(peer.preshared_key)}")
        if peer.endpoint:
            lines.append(f"endpoint={peer.endpoint}")
        if peer.persistent_keepalive is not None:
            lines.append(f"persistent_keepalive_interval={peer.persistent_keepalive}")
        lines.append("replace_allowed_ips=true")
        for cidr in peer.allowed_ips:
            lines.append(f"allowed_ip={cidr}")
    payload = "\n".join(lines) + "\n\n"
    sock.sendall(payload.encode())
    errno = _parse_errno(_read_until_blank(sock))
    if errno != 0:
        raise UapiError(f"UAPI set 失败，errno={errno}")


@dataclass(frozen=True)
class PeerStatus:
    public_key: str
    endpoint: str | None
    last_handshake_sec: int | None  # None 表示从未握手（UAPI 给 0）
    rx_bytes: int
    tx_bytes: int
    persistent_keepalive: int | None
    allowed_ips: tuple[str, ...]
    preshared_key: str | None = None


@dataclass(frozen=True)
class DeviceStatus:
    listen_port: int | None
    fwmark: int | None
    peers: tuple[PeerStatus, ...]


def _finalize_peer(cur: dict) -> PeerStatus:
    hs_sec = cur.get("hs_sec", 0)
    return PeerStatus(
        public_key=cur["public_key"],
        preshared_key=cur.get("preshared_key"),
        endpoint=cur.get("endpoint"),
        last_handshake_sec=hs_sec or None,
        rx_bytes=cur.get("rx_bytes", 0),
        tx_bytes=cur.get("tx_bytes", 0),
        persistent_keepalive=cur.get("keepalive"),
        allowed_ips=tuple(cur.get("allowed_ips", ())),
    )


def _parse_device_status(text: str) -> DeviceStatus:
    listen_port: int | None = None
    fwmark: int | None = None
    errno = 0
    peers: list[PeerStatus] = []
    cur: dict | None = None

    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or "=" not in line:
            continue
        key, _, value = line.partition("=")
        if key == "public_key":
            if cur is not None:
                peers.append(_finalize_peer(cur))
            cur = {"public_key": _hex_to_b64(value)}
        elif key == "private_key":
            pass  # 不把私钥带出去
        elif key == "listen_port":
            listen_port = int(value)
        elif key == "fwmark":
            fwmark = int(value)
        elif key == "errno":
            errno = int(value)
        elif cur is not None:
            if key == "preshared_key":
                cur["preshared_key"] = _hex_to_b64(value) if int(value or "0", 16) else None
            elif key == "endpoint":
                cur["endpoint"] = value
            elif key == "last_handshake_time_sec":
                cur["hs_sec"] = int(value)
            elif key == "last_handshake_time_nsec":
                pass
            elif key == "tx_bytes":
                cur["tx_bytes"] = int(value)
            elif key == "rx_bytes":
                cur["rx_bytes"] = int(value)
            elif key == "persistent_keepalive_interval":
                n = int(value)
                cur["keepalive"] = n or None
            elif key == "allowed_ip":
                cur.setdefault("allowed_ips", []).append(value)
    if cur is not None:
        peers.append(_finalize_peer(cur))
    if errno != 0:
        raise UapiError(f"UAPI get 返回 errno={errno}")
    return DeviceStatus(listen_port=listen_port, fwmark=fwmark, peers=tuple(peers))


def uapi_get(sock: socket.socket) -> DeviceStatus:
    """查询设备当前状态（每个 peer 的握手时间/流量/AllowedIPs 等）。"""
    sock.sendall(b"get=1\n\n")
    return _parse_device_status(_read_until_blank(sock))
