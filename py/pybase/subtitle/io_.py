"""字幕的读、写、清洗。

解析/序列化全部委托给 `pysubs2`（成熟、纯 Python，支持 SRT/WebVTT/ASS/SSA/TTML/
SAMI/MicroDVD/JSON 等），本模块顶层只 import 标准库，`pysubs2` 延迟到函数体内
import——没装时抛出清楚的提示，不让调用方去猜 ImportError 是什么意思。
"""
from __future__ import annotations

import html
import re
from pathlib import Path
from typing import Iterable

from .models import Cue

_PYSUBS2_HINT = 'pysubs2 未安装，运行 pip install "pybase[subtitle]"（或 pip install pysubs2）'


def _pysubs2():
    try:
        import pysubs2
    except ImportError as exc:  # pragma: no cover - 依赖装没装是环境问题，不在覆盖率要求内
        raise ImportError(_PYSUBS2_HINT) from exc
    return pysubs2


# WebVTT 正文允许内联标记：<i>、<c.loud>、<v 说话人>、<00:00:01.000> 逐词计时光标。
# pysubs2 只把 SRT/ASS 的样式标签折叠进 plaintext，VTT 的这些内联标记原样保留在
# text 里，要自己再清一遍，否则纯文本里会混进尖括号标记。
# VTT 逐词计时标签（YouTube 自动字幕满篇都是）。pysubs2 按 SRT 的思路解析 VTT，
# 会把含时间戳的正文行误当成下一条的开头并丢掉整行（报 PossibleMissedSubtitleWarning），
# 所以必须在交给 pysubs2 之前剥掉，不能留到 _plain_text 再清。
_CUE_TIMESTAMP_RE = re.compile(r"<(?:\d+:)?\d{2}:\d{2}\.\d{3}>")

_TAG_RE = re.compile(r"</?[^<>\n]{0,300}>")


def _plain_text(raw: str) -> str:
    """把 pysubs2 的 `plaintext` 进一步清成纯台词：去 VTT 内联标签、解 HTML 实体。"""
    stripped = _TAG_RE.sub("", raw)
    return "\n".join(line.strip() for line in html.unescape(stripped).split("\n")).strip()


def _basic_clean(cues: list[Cue]) -> list[Cue]:
    """按 start 排序、丢空文本、丢 end<=start、去掉完全相同的重复条目。

    重复条目的判定是 (start, end, text) 三元组完全一致（排序后必然相邻），
    只是网络抓包/多轨合并时常见的整条重复，不是"文本相似但时间不同"的模糊去重。
    """
    ordered = sorted(cues, key=lambda c: c.start)
    seen: set[tuple[int, int, str]] = set()
    result: list[Cue] = []
    for cue in ordered:
        if not cue.text or cue.end <= cue.start:
            continue
        key = (cue.start, cue.end, cue.text)
        if key in seen:
            continue
        seen.add(key)
        result.append(cue)
    return result


def _resolve_text(path_or_text: str | Path, encoding: str | None) -> tuple[str, str | None]:
    """判断入参是文件路径还是字幕文本本身，返回 (文本内容, 从扩展名猜出的格式)。

    判定规则：`Path` 实例、或不含换行且指向一个存在文件的字符串，按路径处理；
    其余一律当作字幕文本本身（文本里带换行是最常见、最可靠的信号）。
    """
    if isinstance(path_or_text, Path):
        p = path_or_text
    elif path_or_text and "\n" not in path_or_text and Path(path_or_text).exists():
        p = Path(path_or_text)
    else:
        p = None

    pysubs2 = _pysubs2()
    if p is None:
        # 单行、以字幕扩展名结尾却不存在：几乎一定是路径写错了，别当成字幕正文去解析
        if "\n" not in path_or_text and (
            Path(path_or_text).suffix.lower() in pysubs2.formats.FILE_EXTENSION_TO_FORMAT_IDENTIFIER
        ):
            raise FileNotFoundError(path_or_text)
        return path_or_text, None
    fmt = pysubs2.formats.FILE_EXTENSION_TO_FORMAT_IDENTIFIER.get(p.suffix.lower())
    return p.read_text(encoding=encoding or "utf-8-sig"), fmt


def load(path_or_text: str | Path, format: str | None = None, encoding: str | None = None) -> list[Cue]:
    """加载字幕，返回清洗后的 `Cue` 列表（按 start 排序、去空、去重复条目）。

    `path_or_text` 可以是文件路径也可以是字幕文本本身（判定规则见 `_resolve_text`）。
    `format` 显式指定时优先于路径扩展名/内容探测；两者都没有则交给 pysubs2 按内容
    自动识别。
    """
    pysubs2 = _pysubs2()
    text, guessed_format = _resolve_text(path_or_text, encoding)
    fmt = format or guessed_format
    if fmt == "vtt" or (fmt is None and text.lstrip("\ufeff").startswith("WEBVTT")):
        text = _CUE_TIMESTAMP_RE.sub("", text)
    subs = pysubs2.SSAFile.from_string(text, format_=fmt)
    cues = [
        Cue(start=int(e.start), end=int(e.end), text=_plain_text(e.plaintext))
        for e in subs
        if not e.is_comment
    ]
    return _basic_clean(cues)


def _to_ssafile(cues: Iterable[Cue]):
    """`shift` 之后可能有负时间：整条在 0 之前的丢掉，跨 0 的把开头截到 0。"""
    pysubs2 = _pysubs2()
    subs = pysubs2.SSAFile()
    for cue in cues:
        if cue.end <= 0:
            continue
        event = pysubs2.SSAEvent(start=max(0, cue.start), end=cue.end)
        event.plaintext = cue.text
        subs.append(event)
    return subs


def dumps(cues: Iterable[Cue], format: str) -> str:
    """把 cues 序列化成指定格式（`format` 必填：'srt'/'vtt'/'ass'/'ttml' 等，
    取值见 pysubs2 支持的格式标识符）的字符串。"""
    return _to_ssafile(cues).to_string(format)


def save(cues: Iterable[Cue], path: str | Path, format: str | None = None) -> Path:
    """把 cues 存成文件。`format` 缺省时按路径扩展名推断（pysubs2 的规则）。"""
    pysubs2 = _pysubs2()
    destination = Path(path)
    fmt = format or pysubs2.formats.FILE_EXTENSION_TO_FORMAT_IDENTIFIER.get(destination.suffix.lower())
    if fmt is None:
        raise ValueError(f"无法从扩展名 {destination.suffix!r} 推断字幕格式，请显式传 format")
    destination.parent.mkdir(parents=True, exist_ok=True)
    _to_ssafile(cues).save(str(destination), format_=fmt)
    return destination
