"""后台跑一轮（``Outbox.send(..., detach=True)`` 另起的进程）::

    python -m pybase.outbox.worker --config 配置文件 会话号 第几条

照配置文件（:mod:`.config`）建同一个 Outbox，跑完这一轮就退出。不打印任何东西（提示词、回复都不出这个进程）。
"""
from __future__ import annotations

import argparse
import sys

from .config import outbox_from_config


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(prog="python -m pybase.outbox.worker")
    ap.add_argument("--config", required=True)
    ap.add_argument("conv_id")
    ap.add_argument("n", type=int)
    a = ap.parse_args(argv)
    outbox_from_config(a.config).run_detached(a.conv_id, a.n)
    return 0


if __name__ == "__main__":
    sys.exit(main())
