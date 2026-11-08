"""给命令行入口（:mod:`.ask`、:mod:`.worker`）找到同一份会话库、同样的两边代理：一个 JSON 配置文件。

谁建 :class:`~pybase.outbox.Outbox` 谁写它（如 xxyi 的对话服务启动时、开关改了时，:func:`write_config`），命令行入口读它
（:func:`outbox_from_config`）——本库不知道调用方把库放在哪、代理是哪个命令，都从这里来。

- 位置：``--config`` > 环境变量 ``PYBASE_OUTBOX_CONFIG`` > ``~/.config/pybase/outbox.json``（:func:`config_path`）；
  目录 0700、文件 0600。
- 形状：``{"data_dir": 库目录, "id_prefix": 会话号前缀, "enabled": 开关, "omlx": OmlxAgent.to_config(),
  "claude": ClaudeAgent.to_config(), "omlx_prompt": 文字（可无，缺省 INVESTIGATE_PROMPT）, "claude_prompt": 文字}``。
  不带闸门（入口不往外发东西；转回给发起方由窗口那边过闸门）、不带任何密钥。
"""
from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Optional

from . import files
from .claude import ClaudeAgent
from .omlx import OmlxAgent
from .prompts import INVESTIGATE_PROMPT

CONFIG_ENV = "PYBASE_OUTBOX_CONFIG"
DEFAULT_CONFIG = "~/.config/pybase/outbox.json"


class ConfigError(RuntimeError):
    """配置文件没有 / 读不了 / 形状不对：str(e) 给人看。"""


def config_path(explicit: Optional[str] = None) -> Path:
    return Path(os.path.expanduser(explicit or os.environ.get(CONFIG_ENV) or DEFAULT_CONFIG))


def write_config(path, *, data_dir: str, id_prefix: str = "", enabled: bool = True, omlx: Optional[OmlxAgent] = None,
                 claude: Optional[ClaudeAgent] = None, omlx_prompt: Optional[str] = None,
                 claude_prompt: Optional[str] = None) -> Path:
    d = {"data_dir": str(data_dir), "id_prefix": id_prefix, "enabled": bool(enabled),
         "omlx": omlx.to_config() if omlx is not None else None,
         "claude": claude.to_config() if claude is not None else None}
    if omlx_prompt is not None:
        d["omlx_prompt"] = omlx_prompt
    if claude_prompt is not None:
        d["claude_prompt"] = claude_prompt
    p = Path(path)
    files.ensure_private_dir(p.parent)
    return files.write_private(p, json.dumps(d, ensure_ascii=False, indent=1) + "\n")


def read_config(path) -> dict:
    p = Path(path)
    try:
        d = json.loads(p.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise ConfigError("没有脱敏审核的配置文件 %s（开着脱敏审核的服务起来时会写它）" % p) from None
    except (OSError, ValueError) as e:
        raise ConfigError("脱敏审核的配置文件 %s 读不了（%s）" % (p, type(e).__name__)) from None
    if not isinstance(d, dict) or not isinstance(d.get("data_dir"), str):
        raise ConfigError("脱敏审核的配置文件 %s 形状不对" % p)
    return d


def outbox_from_config(path):
    """配置文件 → :class:`~pybase.outbox.Outbox`（没有闸门；``config_path`` 是它，``detach=True`` 能用）。"""
    from .service import Outbox
    d = read_config(path)
    data = Path(os.path.expanduser(d["data_dir"]))
    try:
        omlx = OmlxAgent.from_config(d["omlx"], run_dir=str(data / "run")) if isinstance(d.get("omlx"), dict) else None
        claude = (ClaudeAgent.from_config(d["claude"], tmp_dir=str(data / "claude-tmp"))
                  if isinstance(d.get("claude"), dict) else None)
    except (KeyError, TypeError, ValueError) as e:
        raise ConfigError("脱敏审核的配置文件 %s 里代理那几项不对（%s）" % (path, type(e).__name__)) from None
    return Outbox(str(data), omlx=omlx, claude=claude, omlx_prompt=d.get("omlx_prompt") or INVESTIGATE_PROMPT,
                  claude_prompt=d.get("claude_prompt") or "", id_prefix=d.get("id_prefix") or "",
                  config_path=str(path))


def enabled(path) -> bool:
    return bool(read_config(path).get("enabled", True))
