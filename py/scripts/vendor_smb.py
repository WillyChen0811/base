"""把 smbprotocol / smbclient / pysmb(smb, nmb) 整库拷进 pybase/_vendor，并把所有外连换成 pybase.wgnet。

用法（升级库后重跑即可，生成物直接提交）：
    python scripts/vendor_smb.py [--site-packages DIR]
缺省取当前解释器里已安装的版本。步骤：拷贝 -> 改包内绝对 import -> 替换外连 -> 自检 -> 写 README/PATCHES。
自检失败（替换点数对不上、或拷贝里还有外连用的 socket）以非零退出，不写半成品说明。
"""

import argparse
import re
import shutil
import sys
from importlib import metadata
from pathlib import Path

PKGS = ["smbprotocol", "smbclient", "smb", "nmb"]
DIST = {"smbprotocol": "smbprotocol", "smbclient": "smbprotocol", "smb": "pysmb", "nmb": "pysmb"}
VENDOR = Path(__file__).resolve().parent.parent / "pybase" / "_vendor"
PREFIX = "pybase._vendor."
WG_IMPORT = "from pybase import wgnet\n"

# (文件, 原文, 新文, 期望次数, 类别, 说明)；原文必须恰好出现这么多次，否则报错（库升级后逼人来看）
TCP, UDP = "TCP", "UDP"
REPLACEMENTS = [
    ("smbprotocol/transport.py",
     "socket.create_connection((self.server, self.port), timeout=self.timeout)",
     "wgnet.tcp(self.server, self.port, self.timeout)", 1, TCP,
     "Tcp.connect：SMB2 直连 445 的 TCP"),
    ("smb/SMBConnection.py",
     "        if sock_family:\n"
     "            self.sock = socket.socket(sock_family)\n"
     "            self.sock.settimeout(timeout)\n"
     "            self.sock.connect(( ip, port ))\n"
     "        else:\n"
     "            self.sock = socket.create_connection(( ip, port ), timeout = timeout)\n",
     "        self.sock = wgnet.tcp(ip, port, timeout)\n"
     "        self.sock.settimeout(timeout)\n", 1, TCP,
     "SMBConnection.connect：pysmb 的 TCP（sock_family 分支并入，wgnet 按目的地址定族）"),
    ("nmb/NetBIOS.py",
     "socket.socket(socket.AF_INET, socket.SOCK_DGRAM)", "wgnet.udp()", 1, UDP,
     "NetBIOS.__init__：NetBIOS 名字查询的 UDP"),
    ("nmb/NetBIOS.py",
     "        if self.broadcast:\n"
     "            self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)\n"
     "        if listen_port:\n"
     "            self.sock.bind(( '', listen_port ))\n",
     "        # wgnet.udp 无 setsockopt/bind：广播与固定监听口不再支持（名字查询只用单播到指定 IP）\n", 1, UDP,
     "NetBIOS.__init__：去掉 SO_BROADCAST/bind（wgnet.udp 不支持）"),
    ("nmb/NetBIOS.py",
     "                ready, _, _ = select.select([ self.sock.fileno() ], [ ], [ ], _timeout)\n"
     "                if not ready:\n"
     "                    return None\n"
     "\n"
     "                data, _ = self.sock.recvfrom(0xFFFF)\n",
     "                self.sock.settimeout(_timeout)\n"
     "                try:\n"
     "                    data, _ = self.sock.recvfrom(0xFFFF)\n"
     "                except socket.timeout:\n"
     "                    return None\n", 2, UDP,
     "NetBIOS._pollFor*Packet：select(fileno) 改 settimeout+recvfrom（wgnet.udp 无 fileno）"),
]
# 这些文件需要 `from pybase import wgnet`
NEEDS_WG = {r[0] for r in REPLACEMENTS}

IMPORT_RE = re.compile(r"^(\s*)from\s+(smbprotocol|smbclient|smb|nmb)(?=[\s.])", re.M)
PLAIN_IMPORT = [("import smbclient.path\n", "from pybase._vendor.smbclient import path  # noqa: F401\n")]
# 自检：拷贝里不得再有外连用法
FORBIDDEN = [r"socket\.create_connection", r"socket\.socket\(", r"\.connect\(\s*\(", r"\bsocket\.fromfd\b"]


MARK = "# 注意：本文件已被 pybase 修改，不是原版（import 路径与外连改动见 pybase/_vendor/PATCHES.md）。\n"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--site-packages", help="源码所在目录，缺省取已安装的 smbclient 所在处")
    sp = ap.parse_args().site_packages
    if not sp:
        import smbclient
        sp = str(Path(smbclient.__file__).parent.parent)
    src = Path(sp)

    if VENDOR.exists():
        shutil.rmtree(VENDOR)
    VENDOR.mkdir(parents=True)
    (VENDOR / "__init__.py").write_text('"""第三方库的拷贝，由 scripts/vendor_smb.py 生成，勿手改。见 README.md / PATCHES.md。"""\n', encoding="utf-8")
    for p in PKGS:
        shutil.copytree(src / p, VENDOR / p, ignore=shutil.ignore_patterns("__pycache__"))
    for dist, d in (("smbprotocol", "smbprotocol"), ("pysmb", "pysmb")):
        lic = next((src / f"{dist}-{metadata.version(dist)}.dist-info" / "licenses").glob("LICENSE*"), None)
        if lic is None:
            print(f"找不到 {dist} 的 LICENSE", file=sys.stderr)
            return 1
        shutil.copy(lic, VENDOR / f"LICENSE.{d}")

    # 1) 包内 import 指向拷贝
    n_imp = 0
    altered = set()     # 改过的文件：最后在开头标明是修改版（pysmb 的 zlib 许可证要求）
    for f in VENDOR.rglob("*.py"):
        if f.parent == VENDOR:
            continue
        text = f.read_text(encoding="utf-8")
        new, k = IMPORT_RE.subn(lambda m: f"{m.group(1)}from {PREFIX}{m.group(2)}", text)
        for a, b in PLAIN_IMPORT:
            if a in new:
                new, k = new.replace(a, b), k + 1
        n_imp += k
        if new != text:
            f.write_text(new, encoding="utf-8")
            altered.add(f)
    left = [str(f.relative_to(VENDOR)) for f in VENDOR.rglob("*.py")
            if re.search(r"^\s*(import|from)\s+(smbprotocol|smbclient|smb|nmb)\b", f.read_text(encoding="utf-8"), re.M)]
    if left:
        print("仍有指向原版的 import：", left, file=sys.stderr)
        return 1

    # 2) 外连替换
    rows = []
    for rel, old, new, expect, kind, note in REPLACEMENTS:
        f = VENDOR / rel
        text = f.read_text(encoding="utf-8")
        if text.count(old) != expect:
            print(f"{rel}：期望 {expect} 处，实际 {text.count(old)} 处（库版本变了？）\n--- {old!r}", file=sys.stderr)
            return 1
        f.write_text(text.replace(old, new), encoding="utf-8")
        altered.add(f)
        rows.append((rel, kind, expect, note))
    for rel in sorted(NEEDS_WG):
        f = VENDOR / rel
        lines = f.read_text(encoding="utf-8").splitlines(keepends=True)
        idx = next(i for i, l in enumerate(lines) if re.match(r"(import|from)\s", l))
        lines.insert(idx, WG_IMPORT)
        f.write_text("".join(lines), encoding="utf-8")

    for f in altered:   # 标明修改版：放在第一行之后，不挤掉 shebang/编码声明的位置要求
        lines = f.read_text(encoding="utf-8").splitlines(keepends=True)
        mark = MARK
        at = 1 if lines and (lines[0].startswith("#!") or "coding" in lines[0]) else 0
        if at == 1 and len(lines) > 1 and "coding" in lines[1]:
            at = 2
        lines.insert(at, mark)
        f.write_text("".join(lines), encoding="utf-8")

    # 3) 自检
    bad = []
    for f in VENDOR.rglob("*.py"):
        for i, line in enumerate(f.read_text(encoding="utf-8").splitlines(), 1):
            if line.lstrip().startswith("#"):
                continue
            if any(re.search(p, line) for p in FORBIDDEN):
                bad.append(f"{f.relative_to(VENDOR)}:{i}: {line.strip()}")
    if bad:
        print("拷贝里仍有外连用的 socket：\n  " + "\n  ".join(bad), file=sys.stderr)
        return 1

    # 4) 说明
    v = {d: metadata.version(d) for d in ("smbprotocol", "pysmb")}
    (VENDOR / "README.md").write_text(
        "# _vendor\n\n"
        f"- smbprotocol {v['smbprotocol']}：`smbprotocol/`、`smbclient/`（LICENSE.smbprotocol）\n"
        f"- pysmb {v['pysmb']}：`smb/`、`nmb/`（LICENSE.pysmb）\n\n"
        "整库拷贝，包内 import 已改指向本目录，外连（TCP/UDP）已换成 `pybase.wgnet`。\n"
        "由 `py/scripts/vendor_smb.py` 生成，升级库后重跑脚本，不要手改。改动清单见 PATCHES.md。\n\n"
        "它们依赖的 cryptography、pyspnego、pyasn1 等不拷，照旧用安装的。\n", encoding="utf-8")
    out = ["# 对原库的改动（脚本自动生成）\n",
           f"- 包内绝对 import 改为 `pybase._vendor.*`：{n_imp} 处\n",
           "- 外连替换：\n\n| 文件 | 类别 | 处数 | 说明 |\n|---|---|---|---|\n"]
    out += [f"| {r[0]} | {r[1]} | {r[2]} | {r[3]} |\n" for r in rows]
    tcp = sum(r[2] for r in rows if r[1] == TCP)
    udp = sum(r[2] for r in rows if r[1] == UDP)
    out.append(f"\n合计 TCP {tcp} 处、UDP {udp} 处（含配套改动）。\n\n"
               "未改：`nmb/NetBIOSProtocol.py`（Twisted 版，不被 pybase 使用，不建 socket）、"
               "`smb/SMBHandler.py`（只取本机主机名）。\n")
    (VENDOR / "PATCHES.md").write_text("".join(out), encoding="utf-8")
    print(f"完成：import {n_imp} 处；TCP {tcp} 处、UDP {udp} 处")
    return 0


if __name__ == "__main__":
    sys.exit(main())
