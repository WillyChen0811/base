# pybase.textsplit

文本切分的共同底座：分句、切块、重切、回溯校验、合并，和语义拆分的配方。不带业务、不调模型、只用标准库；
近似比对装了 RapidFuzz 就用（`pip install "pybase[textsplit]"`），没装退回 `difflib`。

**容量切分** = `chunk` + `merge`（程序定边界，为了读得下，合回原形状）；
**语义拆分** = `atomize` 里的配方（模型定边界，产出新单位，靠 `trace` 指回原文并查漏）。
位置一律是原文的字符下标 `[start, end)`，`text == 原文[start:end]`；时间一律整数毫秒。

## 各件的输入输出

| 件 | 输入 | 输出 |
|---|---|---|
| `sentences(text)` | 一段文字 | `[Sentence(start, end, text, clauses)]`，`clauses` 是 `(Span, …)`。句在 。？！…（及半角 . ? !）和换行处断；小句在 ，；：（及 , ; :）处断；顿号不断；成对的引号、括号里面不拆 |
| `clauses(text)` | 一段文字 | 所有小句 `[Span]` |
| `chunk(lines, *, max_chars, max_lines, max_ms, context=0, measure=None)` | 带编号的行（`Line(no, text, start_ms, end_ms)`、元组或 dict）；预算可组合（至少一种）；参考几片 | `[Chunk(index, body, context)]`：`body`/`context` 是 `(Piece(no, text, start, end, …), …)`，`first`/`last` 是主体首尾行号。主体不重叠，每行（被切开的超长行是每一片）恰好在一块的主体里；超长单行先按句、再按小句、最后硬切 |
| `chunk_text(text, max_chars)` | 一整段文字 | 首尾相接的几段 `[Span]`（按行装、超长行按句切，换行算一个字） |
| `resegment(text, start_ms, end_ms, *, max_chars, max_ms, words=None, seq=None, speaker="")` | 一段话；可带字词级时间 `Word(text, start_ms, end_ms)` | `[Segment(seq, part, start_ms, end_ms, text, estimated, start, end, speaker)]`：只切不并（句末必切，超上限才在句内点号切，再不行硬切）；有字词级时间按词定时间，否则按字数比例估并标 `estimated` |
| `resegment_many(utterances, *, max_chars, max_ms)` | 一串话（对象或 dict，如 `pybase.asr.types.Cue`） | 逐段重切的生成器；一段一段喂和一次喂结果一样 |
| `normalize(text)` | 一段文字 | `Normalized(text, origin, source_len)`：逐字 NFKC + 补充表（。→`.`、、→`,`、省略号各算一个 `…`、各种引号→`"`、破折号→`-`）+ 去空白；`to_source(a, b)` 把归一后的位置指回原文 |
| `trace(original, quotes, *, min_hanzi=0, ordered=False, hint=True)` | 原文；若干引文 | `TraceReport(results, uncovered, fuzzy)`：每条 `QuoteTrace(quote, status, spans, span, best, score)`，`status` 是 `exact` / `normalized` / `not_found`；`uncovered` 是没被任何找到的引文碰到的小句 |
| `merge(outputs, *, key, span, same, combine, adjacent)` | 各块的输出（按块顺序，每块一个列表，None 当空） | 一个列表：按 `key` 去重（先出现的留下）；相邻两块之间主体相同（`same`）、首尾相接（缺省 `后起 == 前止 + 1`）的段并成一段 |
| `p_atom(limits=None, messages=None)` | 门槛（`AtomLimits`）；回给模型的话（缺省照设计稿） | `Recipe(prompt, user, schema, make_blocks, verify, merge, run_kwargs)`：拆需求（P-ATOM）的配方 |

## 回溯校验只认一条

引文（归一后）是原文（归一后）里**连着的一段**才算通过；**不接受只对上开头一小段**（Google LangExtract issue #289
的教训）。不是原文的给「原文里最像的一句」和分数，只作提示（写进回给模型的错误话），不参与判定。
近似比对：RapidFuzz `fuzz.partial_ratio`（引文不长于那一句）/ `fuzz.ratio`（更长）；没装用 `difflib.SequenceMatcher`
（`autojunk=False`）。两个后端分数不保证一样。

## 配方怎么跑（xxyi）

```python
from pybase.textsplit import p_atom

r = p_atom()
blocks = r.make_blocks(seq, name, desc)           # 说明为空 → []：不调模型，「整条」项由调用方自己建
res = taskmodel.run(blocks, r.prompt, r.schema, r.verify, **r.run_kwargs) if blocks else None
out = r.merge(blocks, res)                        # AtomizeResult(atoms, skipped, uncovered, failed, warnings, empty)
```

- 块是 dict：`text` 是固定的 user 一句；`req_seq`/`req_name`/`req_desc` 填进提示词；`desc_start`/`desc_end`/`part`/`parts`
  是这块在整段说明里的位置（程序用，不进提示词）。说明超过 1500 字时按句切成几块，每块的 `req_desc` 是说明里连着的一段。
- `verify` 返回回给模型看的错误话；「说明里这几句没有落到…」是软错误，同一块回 `soft_tries`（2）次后照收，
  进 `uncovered` 给用户看。
- 给模型看的文字（提示词、user 一句、错误话）在 `p_atom_text.py`，逐字照抄设计稿（xxyi-work 的
  `xxyi_work/biz/PLAN-testrec-v2.md` 第 4 节），改要经指挥。设计稿列了门槛却没写措辞的几条（text 去掉方括号后的字数、
  方括号里的字数、单个 term 的字数、text 全是空白）程序照查，超了不回给模型、只记进 `warnings`，措辞定了用
  `p_atom(messages={...})` 给。

## 不用哪些库

LangChain / LlamaIndex 的切分器块起点是事后 find 反推的、主体重叠、不认行号和时间窗；Chonkie 的中文配方运行时
才下载；HanLP 分句也是正则、不带位置，Stanza 要 torch。只借了做法：分隔符分层、国标标点、主体加前文窗口。
