"""放行 / 退回落盘：目录 0700、文件 0600，先写临时文件再换上去（写不成不留半截、不动原来的）。

已放行的只有标题、用途、放行稿（调查过程、各轮提示词和回复、改前的样子都不写）；已退回的只有标题和用户那句话。
"""
from __future__ import annotations

import os
import secrets
from pathlib import Path


def ensure_private_dir(d: Path) -> None:
    """建出来的各级是 0700；这一级已有的也收成 0700（上面已有的各级不动）。"""
    d.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(d, 0o700)


def write_private(path: Path, text: str) -> Path:
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
        path.unlink()
    except FileNotFoundError:
        pass


def render_approved(title: str, purpose: str, draft: str) -> str:
    parts = [f"# {title or ''}"]
    if (purpose or "").strip():
        parts += ["", "## 用途", "", purpose.strip()]
    parts += ["", "## 放行稿", "", (draft or "").rstrip("\n")]
    return "\n".join(parts) + "\n"


def render_rejected(title: str, note: str) -> str:
    parts = [f"# {title or ''}"]
    if (note or "").strip():
        parts += ["", "## 给指挥的话", "", note.strip()]
    return "\n".join(parts) + "\n"
