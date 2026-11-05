"""语义拆分配方：交给调用方去跑模型的一组东西（提示词模板、固定的 user 一句、schema、造块、校验、合并）。

**本模块不调模型，也不 import xxyi**：跑块的机器是调用方的（xxyi 的 ``cmdproto.taskmodel.run`` / ``t.model``）。
配方交出去的形状正好对上 ``taskmodel.run(blocks, prompt, schema, verify, …)``：

- ``make_blocks(...)`` → 块的列表：每块是 dict，``text`` 是 user 那一句，别的键填进 ``prompt``（``string.Template``）
  里的 ``${键}``，另带程序自己用的位置信息（不在提示词里出现，不影响 taskmodel 的存档摘要）；
- ``verify(块, 输出)`` → 回给模型看的错误话清单（空 = 通过）；taskmodel 先按 ``schema`` 校验，过了才调它；
- ``merge(块, taskmodel 的结果)`` → 合并后的结果（各块按出现顺序接起来，位置换成整段原文里的）。

需求用的预设是 ``p_atom()``（设计稿 PLAN-testrec-v2 第 4 节的 P-ATOM）::

    r = p_atom()                                    # 门槛缺省用设计稿的，见 AtomLimits
    blocks = r.make_blocks(seq, name, desc)         # 说明为空 → []：不调模型（「整条」项由调用方自己建）
    res = taskmodel.run(blocks, r.prompt, r.schema, r.verify, **r.run_kwargs) if blocks else None
    out = r.merge(blocks, res)                      # AtomizeResult：atoms、skipped、uncovered、failed、warnings

- 说明超过 ``chunk_chars``（1500）字：先按句切块（``chunk_text``），逐块拆，按出现顺序接起来；每块的
  ``${req_desc}`` 是说明里连着的一段，quote 只在这一段里核对。
- 拆一个已有的原子项：用同一份配方，``desc`` 换成那一项的文字。
- 「说明里这几句没有落到任何一项的 quote 或 skipped」是**软**错误：同一块回给模型 ``soft_tries`` 次（缺省 2，
  和 taskmodel 缺省的重试次数一样）以后就不再回，结果照收，这些小句进 ``AtomizeResult.uncovered`` 给用户看。
  为此 ``verify`` 按块的内容记着回过几次（配方对象里的状态；同一个配方对象再跑同样的块，会直接照收）。
  万一某块因为别的原因重试用完、最后一次输出只剩软错误，``merge`` 也照收它（``warnings`` 里记一句）。
- 设计稿列了门槛、但**没写回给模型的话**的几条（text 去掉方括号后的字数、方括号里的字数、单个 term 的字数、
  text 全是空白）：程序照查，超了**不回给模型**，记进 ``AtomizeResult.warnings`` 给人看；措辞由指挥定了以后经
  ``p_atom(messages={...})`` 给，就会回给模型。项数、text 非空、terms 个数、单个 term 的字数同时写进了 schema
  （``minItems``/``maxItems``/``minLength``/``maxLength``），这几样由约束解码和 taskmodel 的通用校验管。
"""
from __future__ import annotations

import re
import threading
from dataclasses import dataclass, field
from typing import Any, Callable, Mapping, Sequence

from . import p_atom_text as T
from .chunking import chunk_text
from .normal import norm_char
from .sentence import Span
from .tracing import QuoteTrace, trace, uncovered_clauses

_BRACKET = re.compile(r"[\[［]([^\[\]［］]*)[\]］]")


@dataclass(frozen=True)
class Recipe:
    """一份语义拆分配方。字段含义见模块说明；``run_kwargs`` 是建议传给 ``taskmodel.run`` / ``t.model`` 的参数。"""

    prompt: str
    user: str
    schema: dict
    make_blocks: Callable[..., list]
    verify: Callable[[Mapping, Any], list]
    merge: Callable[[Sequence[Mapping], Any], Any]
    run_kwargs: Mapping[str, Any] = field(default_factory=dict)


@dataclass(frozen=True)
class AtomLimits:
    """P-ATOM 的门槛（缺省是设计稿的起步值）。"""

    min_atoms: int = 1               # 项数 1–12
    max_atoms: int = 12
    text_max: int = 80               # text 去掉方括号后最多几个字（不数空白）
    bracket_max: int = 12            # 方括号里合计最多几个字
    terms_max: int = 3               # terms 最多几个
    term_max: int = 8                # 每个 term 最多几个字
    foreign_max: float = 0.1         # text 方括号外的字里，不在「名称 + 说明」里出现过的超过这个比例就回给模型（一成）
    uncovered_min_hanzi: int = 6     # 没落到 quote / skipped 的小句，汉字不少于几个才算
    chunk_chars: int = 1500          # 说明超过这么多字先按句切块
    soft_tries: int = 2              # 没落到的小句最多回给模型几次（之后照收）；和 taskmodel.run 的 retries 一致


# 回给模型的话：键 → 模板（None = 设计稿没写措辞，超了只记进 warnings、不回给模型）
DEFAULT_MESSAGES: dict[str, str | None] = {
    "quote": T.QUOTE_NOT_TRACED,
    "skipped": T.SKIPPED_NOT_TRACED,
    "terms": T.TERMS_NOT_TRACED,
    "foreign": T.TEXT_FOREIGN,
    "uncovered": T.UNCOVERED,
    "text_blank": None,      # text 全是空白（可用占位：{n}）
    "text_long": None,       # text 去掉方括号后超过 text_max（{n}、{count}、{limit}、{text}）
    "bracket_long": None,    # 方括号里合计超过 bracket_max（{n}、{count}、{limit}、{text}）
    "term_long": None,       # 某个 term 超过 term_max（{n}、{count}、{limit}、{term}）
}


@dataclass(frozen=True)
class Atom:
    """拆出的一项。``quote``/``terms`` 是**说明原文里那一段**（模型抄的只差全半角、空白时，换回原文的样子）；
    ``start``/``end`` 是 quote 在整段说明里的位置（同一段出现几次时，按项的先后取不早于上一项的那处）；
    ``block`` 是出自第几块（从 0 起）；``traced`` 是 quote 原样对上（exact）还是归一后才对上（normalized）。"""

    text: str
    quote: str
    start: int
    end: int
    terms: tuple[str, ...]
    doubt: str
    block: int
    traced: str

    def to_dict(self) -> dict:
        return {"text": self.text, "quote": self.quote, "start": self.start, "end": self.end,
                "terms": list(self.terms), "doubt": self.doubt, "block": self.block, "traced": self.traced}


@dataclass(frozen=True)
class AtomizeResult:
    """合并后的结果（位置都是整段说明里的）。

    - ``atoms``：各块拆出的项，按出现顺序；``skipped``：模型说只是背景的句子（原文那一段与位置）；
    - ``uncovered``：重试后仍没落到任何 quote / skipped 的小句（汉字 ≥ 门槛），给用户看，不算失败；
    - ``failed``：没拆成的块在说明里的那一段（重试完还有硬错误，或模型没跑）；
    - ``warnings``：给人看的提醒（超了门槛但没定措辞的、从最后一次输出照收的块）；
    - ``empty``：说明为空（没调模型；「整条」项由调用方建）。
    """

    atoms: tuple[Atom, ...]
    skipped: tuple[Span, ...]
    uncovered: tuple[Span, ...]
    failed: tuple[Span, ...]
    warnings: tuple[str, ...]
    empty: bool = False

    def to_dict(self) -> dict:
        def sp(s: Span) -> dict:
            return {"start": s.start, "end": s.end, "text": s.text}
        return {"atoms": [a.to_dict() for a in self.atoms], "skipped": [sp(s) for s in self.skipped],
                "uncovered": [sp(s) for s in self.uncovered], "failed": [sp(s) for s in self.failed],
                "warnings": list(self.warnings), "empty": self.empty}


# ---------------------------------------------------------------- 检查
def _nonspace(s: str) -> int:
    return sum(1 for c in s if not c.isspace())


def _spoken(c: str) -> bool:
    import unicodedata
    return unicodedata.category(c)[0] in "LN"


def _keys(c: str) -> list[str]:
    """一个字归一（逐字 NFKC + 补充表）、不分大小写后，算数的那几个字（字母、数字、汉字）。"""
    return [x for x in norm_char(c).casefold() if _spoken(x)]


def _outside(text: str) -> tuple[list[str], int]:
    """text → (方括号外的几段, 方括号里合计几个字)。只认配上对的 [] ［］，不套嵌。"""
    parts, inner, pos = [], 0, 0
    for m in _BRACKET.finditer(text):
        parts.append(text[pos:m.start()])
        inner += _nonspace(m.group(1))
        pos = m.end()
    parts.append(text[pos:])
    return parts, inner


def _foreign(parts: Sequence[str], source: set[str]) -> tuple[int, int, list[str]]:
    """方括号外的字里不在 source 出现过的 → (算数的字数, 没出现过的字数, 连着的没出现过的字串)。"""
    counted = bad = 0
    runs: list[str] = []
    for part in parts:
        cur = ""
        for c in part:
            ks = _keys(c)
            if not ks:
                if cur:
                    runs.append(cur)
                    cur = ""
                continue
            miss = [k for k in ks if k not in source]
            counted += len(ks)
            bad += len(miss)
            if miss:
                cur += c
            elif cur:
                runs.append(cur)
                cur = ""
        if cur:
            runs.append(cur)
    seen: list[str] = []
    for r in runs:
        if r not in seen:
            seen.append(r)
    return counted, bad, seen


@dataclass
class _Analysis:
    hard: list[str]
    soft: list[str]
    warns: list[str]
    quotes: tuple[QuoteTrace, ...]
    skipped: tuple[QuoteTrace, ...]
    terms: list[tuple[QuoteTrace, ...]]
    uncovered: list[Span]


def _shape_ok(value: Any, lim: AtomLimits) -> bool:
    """结构和项数对不对（schema 管的那几样；给照收最后一次输出时用）。"""
    if not isinstance(value, Mapping) or not isinstance(value.get("atoms"), list) \
            or not isinstance(value.get("skipped"), list):
        return False
    if not (lim.min_atoms <= len(value["atoms"]) <= lim.max_atoms):
        return False
    if not all(isinstance(s, str) for s in value["skipped"]):
        return False
    for a in value["atoms"]:
        if not isinstance(a, Mapping) or not all(isinstance(a.get(k), str) for k in ("text", "quote", "doubt")):
            return False
        if not a["text"] or not isinstance(a.get("terms"), list) or len(a["terms"]) > lim.terms_max \
                or not all(isinstance(t, str) for t in a["terms"]):
            return False
    return True


class _PAtom:
    """P-ATOM 的造块、校验、合并（``p_atom()`` 包成 Recipe）。"""

    def __init__(self, limits: AtomLimits, messages: Mapping[str, str | None]):
        self.lim = limits
        self.msg = messages
        self._soft: dict[tuple, int] = {}
        self._lock = threading.Lock()

    # ---- 造块
    def make_blocks(self, seq: Any, name: str, desc: str | None) -> list[dict]:
        """一条需求 → 块。说明为空（None / 全是空白）→ []（不调模型）。说明超过 chunk_chars 字按句切成几块。"""
        desc = desc or ""
        name = "" if name is None else str(name)
        if not desc.strip():
            return []
        if len(desc) > self.lim.chunk_chars:
            spans = chunk_text(desc, self.lim.chunk_chars)
        else:
            a, b = 0, len(desc)
            while desc[a].isspace():
                a += 1
            while desc[b - 1].isspace():
                b -= 1
            spans = [Span(a, b, desc[a:b])]
        n = len(spans)
        return [{"text": T.USER, "req_seq": "" if seq is None else seq, "req_name": name, "req_desc": sp.text,
                 "desc_start": sp.start, "desc_end": sp.end, "part": i, "parts": n}
                for i, sp in enumerate(spans)]

    # ---- 检查一块的输出
    def _analyze(self, block: Mapping, value: Mapping) -> _Analysis:
        lim, msg = self.lim, self.msg
        desc = block["req_desc"]
        name = str(block.get("req_name") or "")
        atoms = list(value.get("atoms") or [])
        skipped = list(value.get("skipped") or [])
        tq = trace(desc, [a["quote"] for a in atoms], ordered=True)
        ts = trace(desc, skipped, ordered=True)
        tt = [trace(desc, list(a.get("terms") or [])).results for a in atoms]
        source = {k for c in name + desc for k in _keys(c)}
        hard: list[str] = []
        warns: list[str] = []

        def limit(key: str, human: str, **fields: Any) -> None:
            tpl = msg.get(key)
            if tpl:
                hard.append(tpl.format(**fields))
            else:
                warns.append(human)

        for n, (a, rq, rts) in enumerate(zip(atoms, tq.results, tt), 1):
            text = a["text"]
            parts, inner = _outside(text)
            if not text.strip():
                limit("text_blank", f"第 {n} 项的 text 全是空白", n=n)
            out_n = sum(_nonspace(p) for p in parts)
            if out_n > lim.text_max:
                limit("text_long", f"第 {n} 项的 text 去掉方括号后 {out_n} 字，超过 {lim.text_max}",
                      n=n, count=out_n, limit=lim.text_max, text=text)
            if inner > lim.bracket_max:
                limit("bracket_long", f"第 {n} 项的 text 方括号里合计 {inner} 字，超过 {lim.bracket_max}",
                      n=n, count=inner, limit=lim.bracket_max, text=text)
            if not rq.ok:
                hard.append(msg["quote"].format(n=n, best=rq.best.text if rq.best else ""))
            for term, rt in zip(a.get("terms") or [], rts):
                if _nonspace(term) > lim.term_max:
                    limit("term_long", f"第 {n} 项的 term「{term}」{_nonspace(term)} 字，超过 {lim.term_max}",
                          n=n, count=_nonspace(term), limit=lim.term_max, term=term)
                if not rt.ok:
                    hard.append(msg["terms"].format(n=n, best=rt.best.text if rt.best else ""))
            counted, bad, runs = _foreign(parts, source)
            if counted and bad / counted > lim.foreign_max:
                hard.append(msg["foreign"].format(n=n, words=T.JOIN.join(runs)))
        for k, rs in enumerate(ts.results, 1):
            if not rs.ok:
                hard.append(msg["skipped"].format(n=k, best=rs.best.text if rs.best else ""))
        covered = [sp for r in tq.results + ts.results for sp in r.spans]
        unc = uncovered_clauses(desc, covered, min_hanzi=lim.uncovered_min_hanzi)
        soft = [msg["uncovered"].format(clauses=T.JOIN.join(c.text for c in unc))] if unc else []
        return _Analysis(hard, soft, warns, tq.results, ts.results, tt, unc)

    @staticmethod
    def _key(block: Mapping) -> tuple:
        return (str(block.get("req_seq")), str(block.get("req_name")), block.get("req_desc"), block.get("desc_start"))

    def verify(self, block: Mapping, value: Any) -> list[str]:
        """taskmodel 的 verify：→ 回给模型看的错误话（空 = 通过）。软错误（没落到的小句）回过 soft_tries 次就不再回。"""
        if not _shape_ok(value, AtomLimits(min_atoms=0, max_atoms=10 ** 9, terms_max=10 ** 9)):
            # 结构由 schema 管（taskmodel 先按 schema 校验，过了才调这里）；走到这里是调用方没带配方的 schema
            raise TypeError("P-ATOM 的 verify 收到的输出结构不对：调 taskmodel.run 时要带上配方的 schema")
        an = self._analyze(block, value)
        if not an.soft:
            return an.hard
        k = self._key(block)
        with self._lock:
            told = self._soft.get(k, 0)
            if told >= self.lim.soft_tries:
                return an.hard
            self._soft[k] = told + 1
        return an.hard + an.soft

    # ---- 合并
    def merge(self, blocks: Sequence[Mapping], result: Any = None) -> AtomizeResult:
        """块 + taskmodel 的结果（``ModelResult``，或各块输出的列表）→ ``AtomizeResult``。没有块 → 说明为空。"""
        blocks = list(blocks)
        if not blocks:
            return AtomizeResult((), (), (), (), (), empty=True)
        outputs = getattr(result, "outputs", result)
        drafts = getattr(result, "drafts", None)
        outputs = list(outputs) if outputs is not None else []
        drafts = list(drafts) if drafts is not None else []
        atoms: list[Atom] = []
        skipped: list[Span] = []
        uncovered: list[Span] = []
        failed: list[Span] = []
        warns: list[str] = []
        for i, b in enumerate(blocks):
            off = int(b.get("desc_start") or 0)
            desc = b["req_desc"]
            whole = Span(off, off + len(desc), desc)
            tag = f"第 {i + 1}/{len(blocks)} 块" if len(blocks) > 1 else "这条需求"
            out_i = outputs[i] if i < len(outputs) else None
            draft = drafts[i] if i < len(drafts) else None
            value = an = None
            for cand, from_draft in ((out_i, False), (draft, True)):
                if cand is None or (from_draft and cand is out_i) or not _shape_ok(cand, self.lim):
                    continue
                got = self._analyze(b, cand)
                if got.hard:                        # 有硬错误（没过校验）的不收
                    continue
                value, an = cand, got
                if from_draft:
                    warns.append(f"{tag}重试后只剩没落到 quote / skipped 的小句，照收了最后一次的输出")
                break
            if an is None:
                failed.append(whole)
                continue
            warns.extend(f"{tag}：{w}" if len(blocks) > 1 else w for w in an.warns)

            def orig(sp: tuple[int, int]) -> Span:
                return Span(off + sp[0], off + sp[1], desc[sp[0]:sp[1]])

            for a, rq, rts in zip(value["atoms"], an.quotes, an.terms):
                q = orig(rq.span)
                terms = tuple(desc[rt.span[0]:rt.span[1]] for rt in rts)
                atoms.append(Atom(a["text"], q.text, q.start, q.end, terms, a["doubt"], i, rq.status))
            skipped.extend(orig(rs.span) for rs in an.skipped)
            uncovered.extend(Span(off + c.start, off + c.end, c.text) for c in an.uncovered)
        return AtomizeResult(tuple(atoms), tuple(skipped), tuple(uncovered), tuple(failed), tuple(warns))


def _schema(lim: AtomLimits) -> dict:
    """设计稿的 schema ``{"atoms":[{"text":str,"quote":str,"terms":[str],"doubt":str}],"skipped":[str]}``，
    字段顺序照设计稿；项数、text 非空、terms 个数与每个的字数写成约束。"""
    return {
        "type": "object",
        "properties": {
            "atoms": {
                "type": "array", "minItems": lim.min_atoms, "maxItems": lim.max_atoms,
                "items": {
                    "type": "object",
                    "properties": {
                        "text": {"type": "string", "minLength": 1},
                        "quote": {"type": "string"},
                        "terms": {"type": "array", "maxItems": lim.terms_max,
                                  "items": {"type": "string", "maxLength": lim.term_max}},
                        "doubt": {"type": "string"},
                    },
                    "required": ["text", "quote", "terms", "doubt"],
                    "additionalProperties": False,
                },
            },
            "skipped": {"type": "array", "items": {"type": "string"}},
        },
        "required": ["atoms", "skipped"],
        "additionalProperties": False,
    }


def p_atom(limits: AtomLimits | None = None, messages: Mapping[str, str | None] | None = None) -> Recipe:
    """P-ATOM（拆需求）的配方。``limits`` 改门槛；``messages`` 改 / 补回给模型的话（键见 ``DEFAULT_MESSAGES``；
    给模型看的文字要经指挥定）。每次调用是一份新的配方（软错误的计数各管各的）。"""
    lim = limits or AtomLimits()
    if lim.min_atoms < 1 or lim.max_atoms < lim.min_atoms or lim.soft_tries < 0 or lim.chunk_chars < 1:
        raise ValueError(f"门槛不对：{lim}")
    msg = dict(DEFAULT_MESSAGES)
    for k, v in (messages or {}).items():
        if k not in msg:
            raise ValueError(f"没有这种话：{k}（有 {'、'.join(msg)}）")
        if v is not None and not isinstance(v, str):
            raise TypeError(f"{k} 的话要是字符串或 None：{v!r}")
        msg[k] = v
    for k in ("quote", "skipped", "terms", "foreign", "uncovered"):
        if not msg[k]:
            raise ValueError(f"{k} 的话不能空（设计稿写了措辞）")
    impl = _PAtom(lim, msg)
    return Recipe(prompt=T.PROMPT, user=T.USER, schema=_schema(lim), make_blocks=impl.make_blocks,
                  verify=impl.verify, merge=impl.merge,
                  run_kwargs={"keep_going": True, "retries": lim.soft_tries})
