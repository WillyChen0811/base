"""把 macOS 词典（牛津系）的 XHTML 解析成结构：词头、音标、词性分组、义项、例句、词源。

苹果的牛津系词典（New Oxford American Dictionary、Oxford Dictionary of English、
牛津英汉汉英词典等）用同一套 class 标注结构，本机实测两种版式：

- 英英（NOAD/ODE）：`se1` 词性块（`posg/pos`）→ `msDict` 义项（`df` 释义、`eg/ex` 例句），
  `etym` 词源（`date`/`la` 语种/`ff` 词形/`q` 原义）
- 英汉：`gramb` 词性块（`ps`）→ `semb` 义项（`ind` 语义提示 + `trans` 译文，`ty_pinyin` 是拼音），
  `exg` 例句（`ex` 原文 + `trans` 译文）

只认这些 class，不认识的版式（非牛津系词典）返回 None，调用方退回纯文本。
不改写内容、不做摘要：结构是词典自己的，这里只是把它从排版里拆出来。
"""
from __future__ import annotations

import re
import xml.etree.ElementTree as ET
from dataclasses import dataclass, field

_WS = re.compile(r"\s+")


@dataclass(frozen=True)
class Example:
    text: str
    translation: str | None = None


@dataclass(frozen=True)
class Sense:
    number: str | None
    definitions: list[str]           # 英英是释义句；英汉是"(语义提示) 译文"
    examples: list[Example] = field(default_factory=list)


@dataclass(frozen=True)
class PosGroup:
    pos: str | None
    senses: list[Sense]


@dataclass(frozen=True)
class ParsedEntry:
    headword: str | None
    pronunciations: list[tuple[str | None, str]]   # (BrE/AmE 或 None, 音标)
    groups: list[PosGroup]
    etymology: str | None


def parse_entry(html: str) -> ParsedEntry | None:
    """解析一条词典 XHTML；不是牛津系版式（找不到词性块）返回 None。"""
    try:
        root = ET.fromstring(html.encode("utf-8"))
    except ET.ParseError:
        return None
    entry = next((e for e in root.iter() if "entry" in _classes(e)), None)
    if entry is None:
        return None

    groups = [_group(g) for g in _find_all(entry, "gramb")] or \
             [_group(g) for g in _find_all(entry, "se1")]
    groups = [g for g in groups if g.senses]
    if not groups:
        return None

    hw = _first(entry, "hw")
    etym = _first(entry, "etym")
    return ParsedEntry(
        headword=_text(hw) if hw is not None else None,
        pronunciations=_pronunciations(entry),
        groups=groups,
        etymology=_text(etym, skip={"ty_label"}) if etym is not None else None,
    )


# ---------------------------------------------------------------- 各部分

def _pronunciations(entry) -> list[tuple[str | None, str]]:
    # 只看词头组（英英 hg、英汉 hwg）：派生词、例句里也有音标
    head = _first(entry, "hg")
    if head is None:
        head = _first(entry, "hwg")
    if head is not None:
        entry = head
    result: list[tuple[str | None, str]] = []
    for prx in _find_all(entry, "prx"):
        ph = _first(prx, "ph")
        if ph is not None and _text(ph):
            result.append((prx.get("dialect"), _text(ph)))
    if not result:  # NOAD 的音标不包在 prx 里
        ph = _first(entry, "ph")
        if ph is not None and _text(ph):
            result.append((None, _text(ph)))
    return result


def _group(block) -> PosGroup:
    # Element 的真值看的是有没有子节点，不能用 or 串
    pos_el = _first(block, "ps")
    if pos_el is None:
        pos_el = _first(block, "pos")
    # 英英：se2 是编号义项，其下 msDict 是核心义 + 子义项；没编号的词条只有 msDict
    senses = [_sense(s) for s in _find_all(block, "semb")] or \
             [_sense(s) for s in _find_all(block, "se2")] or \
             [_sense(s) for s in _find_all(block, "msDict")]
    return PosGroup(pos=_text(pos_el) if pos_el is not None else None,
                    senses=[s for s in senses if s.definitions or s.examples])


def _sense(el) -> Sense:
    sn = _first(el, "sn")
    number = _text(sn).rstrip(".") if sn is not None and "ty_label" in _classes(sn) else None
    if _find_all(el, "df"):  # 英英
        definitions = [_text(d) for d in _find_all(el, "df")]
        examples = [Example(_text(x)) for x in _find_all(el, "ex")]
    else:  # 英汉：trg 是"(提示) 译文"，例句在 exg 里
        definitions = [_translation(t) for t in _direct_trg(el)]
        examples = []
        for exg in _find_all(el, "exg"):
            ex = _first(exg, "ex")
            trans = [_text(t, skip={"ty_pinyin"}) for t in _find_all(exg, "trans")
                     if "ty_pinyin" not in _classes(t)]
            if ex is not None:
                examples.append(Example(_text(ex), " ".join(t for t in trans if t) or None))
    return Sense(number=number, definitions=[d for d in definitions if d], examples=examples)


def _direct_trg(sense):
    """义项自己的 trg，不含例句（exg）里的。"""
    out = []

    def walk(node):
        for child in node:
            cls = _classes(child)
            if "exg" in cls:
                continue
            if "trg" in cls:
                out.append(child)
            else:
                walk(child)
    walk(sense)
    return out


def _translation(trg) -> str:
    ind = _first(trg, "ind")
    hint = _text(ind, skip={"gp"}) if ind is not None else ""
    trans = [_text(t) for t in _find_all(trg, "trans") if "ty_pinyin" not in _classes(t)]
    body = "；".join(t for t in trans if t)
    return f"({hint}) {body}" if hint and body else body or hint


# ---------------------------------------------------------------- ElementTree 小工具

def _classes(el) -> set[str]:
    return set((el.get("class") or "").split())


def _find_all(el, cls: str) -> list:
    return [e for e in el.iter() if e is not el and cls in _classes(e)]


def _first(el, cls: str):
    return next((e for e in el.iter() if e is not el and cls in _classes(e)), None)


def _text(el, skip: set[str] = frozenset()) -> str:
    """元素的可见文本；class 命中 skip 的子树整段跳过（拼音、标签等）。"""
    parts: list[str] = []

    def walk(node):
        if node is not el and _classes(node) & skip:
            if node.tail:
                parts.append(node.tail)
            return
        if node.text:
            parts.append(node.text)
        for child in node:
            walk(child)
        if node is not el and node.tail:
            parts.append(node.tail)
    walk(el)
    return _WS.sub(" ", "".join(parts)).strip(" ;；|")
