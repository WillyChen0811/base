#!/usr/bin/env python3
"""手动验证脚本：拿一份真实 wg-quick 风格的 `.conf`，起真实 wireguard-go，配地址/路由，
打印 status()，等一会儿再拆。**必须 sudo 跑**（建 utun、配地址、加路由都要 root）。

前置条件：
  1. `brew install wireguard-go`（本机没装，先装）。
  2. 准备一个单独的测试 peer（比如用 wg-peers 新建一个专门测试用的账号），别用
     你正在用的 WireGuard.app 那个 peer/网段——两边抢同一个 AllowedIPs 网段会打架。
  3. 把这个 peer 的 `.conf` 存到某个路径，跑：
       sudo python3 py/scripts/net_tunnel_smoke.py /path/to/test-peer.conf

这个脚本只做手动验证，不进 pytest（本机没有 root，CI/pytest 环境也没有 utun 权限）。
"""
from __future__ import annotations

import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from pybase.net.tunnel import Tunnel
from pybase.net.wireguard import parse_conf


def main() -> None:
    if len(sys.argv) != 2:
        print(f"用法：sudo {sys.argv[0]} /path/to/test-peer.conf", file=sys.stderr)
        raise SystemExit(2)

    conf_path = Path(sys.argv[1])
    cfg = parse_conf(conf_path.read_text())
    print(f"解析到 {len(cfg.peers)} 个 peer，接口地址 {cfg.interface.addresses}")

    tunnel = Tunnel(cfg)
    print("up() ...")
    tunnel.up()
    try:
        status = tunnel.status()
        print(f"接口名：{status.ifname}")
        print(f"引擎是否存活：{status.engine_alive}")
        print(f"已加路由：{status.routes}")
        print(f"endpoint 保护路由：{status.endpoint_routes}")
        print(f"DNS（未应用，仅记录）：{status.dns}")
        if status.device:
            for peer in status.device.peers:
                print(
                    f"  peer {peer.public_key[:12]}...  "
                    f"endpoint={peer.endpoint}  握手={peer.last_handshake_sec}  "
                    f"rx={peer.rx_bytes}  tx={peer.tx_bytes}"
                )

        print("等 5 秒（可以另开一个终端 ping 隧道对端，观察握手/流量变化）...")
        time.sleep(5)

        status = tunnel.status()
        if status.device:
            for peer in status.device.peers:
                print(f"  peer {peer.public_key[:12]}...  握手={peer.last_handshake_sec}  rx={peer.rx_bytes}")
    finally:
        print("down() ...")
        tunnel.down()
    print("done")


if __name__ == "__main__":
    main()
