"""指令行解析：`/动作 -组 [--子参数] --参数 值 --开关`。

只管把字符串拆成结构，不管参数对不对——那是注册表和解析顺序的事。
指令名可以带一个 `--子参数`（如 `/list -project --parents`、
`/sync -archive --dry-run`），算不算指令名的一部分由调用方给的已知指令表定。
"""

import shlex
from dataclasses import dataclass, field


class ParseError(ValueError):
    pass


@dataclass
class Parsed:
    cmd: str                      # 注册表里的指令名，如 "/list -project --parents"
    flags: dict = field(default_factory=dict)   # "--参数" -> 字符串值，或 True（开关）
    positional: object = None     # 指令名里那个子参数后面直接跟的值


def _is_flag(tok: str) -> bool:
    return tok.startswith("--") and len(tok) > 2


def tokenize(line: str) -> list:
    try:
        return shlex.split(line.strip())
    except ValueError as e:
        raise ParseError(f"引号没配对：{e}") from None


def parse(line: str, known_cmds) -> Parsed:
    toks = tokenize(line)
    if not toks or not toks[0].startswith("/") or len(toks[0]) < 2:
        raise ParseError("指令要以 /动作 开头")
    if len(toks) < 2 or not toks[1].startswith("-") or _is_flag(toks[1]):
        raise ParseError(f"{toks[0]} 后面要跟 -组，例如 {toks[0]} -project")
    base = f"{toks[0]} {toks[1]}"
    rest = toks[2:]
    cmd, positional = base, None
    if len(rest) > 1 and _is_flag(rest[0]) and _is_flag(rest[1]) and f"{base} {rest[0]} {rest[1]}" in known_cmds:
        cmd = f"{base} {rest[0]} {rest[1]}"         # 指令名带两个开关（如 /check -report --diff --server）
        rest = rest[2:]
    elif rest and _is_flag(rest[0]) and f"{base} {rest[0]}" in known_cmds:
        cmd = f"{base} {rest[0]}"
        rest = rest[1:]
        if rest and not _is_flag(rest[0]):
            positional, rest = rest[0], rest[1:]
    if cmd not in known_cmds:
        raise ParseError(f"没有这条指令：{cmd}")

    flags = {}
    i = 0
    while i < len(rest):
        tok = rest[i]
        if not _is_flag(tok):
            raise ParseError(f"多出来的值：{tok}（参数要写成 --名字 值）")
        if tok in flags:
            raise ParseError(f"参数给了两次：{tok}")
        if i + 1 < len(rest) and not _is_flag(rest[i + 1]):
            flags[tok] = rest[i + 1]
            i += 2
        else:
            flags[tok] = True
            i += 1
    return Parsed(cmd=cmd, flags=flags, positional=positional)
