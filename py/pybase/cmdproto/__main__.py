"""命令行：拿一份注册表 JSON 看指令怎么解析（不执行实现，原子也不调）。

    python -m pybase.cmdproto --registry reg.json commands            # 列指令与参数
    python -m pybase.cmdproto --registry reg.json plan "/open -book --name A"   # 每个参数从哪来

真要执行要把实现表接上，用 `Session(env, registry, impls=..., atoms=...)`，这里不带。
"""

import argparse
import json
import sys

from .registry import Registry
from .resolver import ResolveError
from .runtime import Session


def _commands(reg):
    for cmd, row in reg.commands.items():
        ps = ", ".join(f"{p['name']}{'*' if p.get('required') == '是' else ''}" for p in reg.params_of(cmd))
        print(f"{cmd:40s} {row.get('layer', ''):8s} {ps}")


def _plan(reg, line):
    s = Session(None, reg)
    try:
        cmd, values, fanout, trace = s.plan(line)
    except (ValueError, ResolveError) as e:          # ParseError / ResolveError 都是 ValueError
        print(str(e), file=sys.stderr)
        return 1
    print(cmd)
    for pid, v in values.items():
        name = reg.params[pid]["name"]
        fan = "（逐个）" if pid in fanout else ""
        print(f"  {name}{fan} = {json.dumps(v, ensure_ascii=False)}    ← {trace.get(pid, '')}")
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(prog="python -m pybase.cmdproto")
    ap.add_argument("--registry", required=True, help="注册表 JSON 路径")
    sub = ap.add_subparsers(dest="action", required=True)
    sub.add_parser("commands")
    p = sub.add_parser("plan")
    p.add_argument("line")
    a = ap.parse_args(argv)
    reg = Registry.load(a.registry)
    if a.action == "commands":
        _commands(reg)
        return 0
    return _plan(reg, a.line)


if __name__ == "__main__":
    sys.exit(main())
