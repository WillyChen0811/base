"""私有文件：目录 0700、文件 0600，先写临时文件再换上去（写不成不留半截、不动原来的）。

用在：发一轮时临时放的附加提示词文件、给 claude 当临时目录的空目录、给命令行入口找库用的配置文件（:mod:`.config`）。
"""
from __future__ import annotations

import os
import secrets
from pathlib import Path


def ensure_private_dir(d: Path) -> None:
    """建出来的各级是 0700；这一级已有的也收成 0700（上面已有的各级不动）。"""
    d = Path(d)
    d.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(d, 0o700)


def write_private(path: Path, text: str) -> Path:
    path = Path(path)
    ensure_private_dir(path.parent)
    tmp = path.with_name(f".{path.name}.{os.getpid()}.{secrets.token_hex(4)}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(text)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    return path


def remove(path: Path) -> None:
    try:
        Path(path).unlink()
    except FileNotFoundError:
        pass
