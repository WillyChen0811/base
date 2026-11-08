"""终端里请本地 omlx 协查：建一份会话（来自终端、带发起方名字），**直接发给 omlx**；用户在窗口里看过、
点「转回给发起方」（可以先改，过闸门）后，发起方才拿到那段话。omlx 的回复和过程发起方一律拿不到。

    python -m pybase.outbox.ask [--title 标题] [--scope 路径]... [--model 模型] [--from 名字] [--wait] [--timeout 秒] 提示词文件|-
    python -m pybase.outbox.ask --wait-reply 会话号 [--timeout 秒]

- 提示词从文件读（UTF-8），``-`` 从 stdin 读。``--scope`` 是 omlx 能看的目录或文件（可多次）；不给就用窗口里最近一次填的范围。
  ``--from`` 是发起方的名字（窗口里显示，缺省「终端」）。``--model`` 选 omlx 的模型（缺省同 omlx-agent）。
- 不带 ``--wait``：stdout 两行——会话号、状态，马上退出（omlx 在后台进程里照跑）。之后 ``--wait-reply 会话号`` 再等。
- ``--wait`` / ``--wait-reply``：一直等到用户转回；stdout **只有转回的那段话**（会话号、状态打在 stderr）。
  用户转回几次就能取几次，每次取最早一条还没取的。``--timeout`` 秒内没等到退出码 3（之后照样能 ``--wait-reply``）。
- 和窗口用同一份库：配置文件 ``--config`` > 环境变量 ``PYBASE_OUTBOX_CONFIG`` > ``~/.config/pybase/outbox.json``
  （开着脱敏审核的服务写的，见 :mod:`.config`）。开关关着一律不让。

退出码：0 成；1 不让（开关关了、没有配置、范围不在、会话不在……）；2 用法不对；3 等超时；130 Ctrl-C。
"""
from __future__ import annotations

import argparse
import sys

from . import config as cfg_mod
from .checks import BadInput, NotFound, OutboxError, ScopeMissing
from .store import OMLX, ORIGIN_TERMINAL

DEFAULT_FROM = "终端"
DISABLED = "脱敏审核已关闭，不能发起协查"


class Disabled(RuntimeError):
    pass


def _err(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


def _read_prompt(src: str) -> str:
    if src == "-":
        return sys.stdin.read()
    with open(src, encoding="utf-8") as f:
        return f.read()


def _wait(box, conv_id: str, path, timeout) -> int:
    def check():
        if not cfg_mod.enabled(path):
            raise Disabled(DISABLED)
    try:
        text = box.wait_reply(conv_id, timeout=timeout, check=check)
    except TimeoutError:
        _err("%s：还没有转回的话（之后用 --wait-reply %s 接着等）" % (conv_id, conv_id))
        return 3
    except KeyboardInterrupt:
        _err("没等到；之后用 --wait-reply %s 接着等" % conv_id)
        return 130
    sys.stdout.write(text if text.endswith("\n") else text + "\n")
    sys.stdout.flush()
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(prog="python -m pybase.outbox.ask", description="请本地 omlx 协查（只拿得到用户转回的话）")
    ap.add_argument("prompt", nargs="?", help="提示词文件（UTF-8），- 从 stdin 读")
    ap.add_argument("--title")
    ap.add_argument("--scope", action="append", default=[])
    ap.add_argument("--model")
    ap.add_argument("--from", dest="requester", default=DEFAULT_FROM)
    ap.add_argument("--wait", action="store_true")
    ap.add_argument("--wait-reply", metavar="会话号")
    ap.add_argument("--timeout", type=float)
    ap.add_argument("--config")
    a = ap.parse_args(argv)
    if bool(a.wait_reply) == bool(a.prompt):
        ap.error("要么给提示词文件（或 -），要么 --wait-reply 会话号")
    path = cfg_mod.config_path(a.config)
    try:
        if not cfg_mod.enabled(path):
            _err(DISABLED)
            return 1
        box = cfg_mod.outbox_from_config(path)
        if a.wait_reply:
            box.get(a.wait_reply)                                # 没有这份就别等
            return _wait(box, a.wait_reply, path, a.timeout)
        try:
            prompt = _read_prompt(a.prompt)
        except (OSError, UnicodeDecodeError) as e:
            _err("提示词读不了：%s" % type(e).__name__)
            return 2
        scope = a.scope or box.last_scope()
        if not scope:
            _err("没给 --scope，窗口里也还没填过 omlx 的范围")
            return 1
        conv = box.create(OMLX, prompt, title=a.title, scope=scope, model=a.model, origin=ORIGIN_TERMINAL,
                          requester=a.requester, detach=True)
    except ScopeMissing as e:                                    # 路径可能带资料名：只报个数
        _err("范围里有 %d 项不在（在窗口里看、改）" % len(e.missing))
        return 1
    except BadInput as e:
        _err(str(e))
        return 2
    except (NotFound, OutboxError, cfg_mod.ConfigError, Disabled) as e:
        _err(str(e))
        return 1
    out = _err if a.wait else (lambda s: print(s, flush=True))
    out("会话号：%s" % conv["id"])
    out("状态：已发给 omlx，在跑；用户在窗口里转回后才有结果" + ("" if a.wait else "（python -m pybase.outbox.ask --wait-reply %s）"
                                                            % conv["id"]))
    if not a.wait:
        return 0
    try:
        return _wait(box, conv["id"], path, a.timeout)
    except (NotFound, OutboxError, cfg_mod.ConfigError, Disabled) as e:
        _err(str(e))
        return 1


if __name__ == "__main__":
    sys.exit(main())
