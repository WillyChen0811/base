"""覆盖 pybase.textsplit 的 P-ATOM 配方：造块、模板填充、schema、每一条校验（通过 / 不通过）、软错误照收、合并、
说明为空、说明超长。模型输出全是手写的假输出；跑块用一个照 taskmodel.run 约定写的小跑法（不 import xxyi）。"""
from __future__ import annotations

import copy
import re
import string
import sys
from types import SimpleNamespace

import pytest

from pybase.textsplit import AtomLimits, Span, p_atom, sentences
from pybase.textsplit import p_atom_text as T

SEQ, NAME = "3", "用户查询"
DESC = "支持按姓名、手机号查询用户。查询结果在列表中显示，每页显示二十条。本功能为了方便管理员日常使用。"
S1 = "支持按姓名、手机号查询用户。"
S2 = "查询结果在列表中显示，每页显示二十条。"
S3 = "本功能为了方便管理员日常使用。"


def _atom(text, quote, terms=(), doubt=""):
    return {"text": text, "quote": quote, "terms": list(terms), "doubt": doubt}


GOOD = {
    "atoms": [
        _atom("[系统]支持按姓名查询用户", "支持按姓名、手机号查询用户", ["姓名"]),
        _atom("[系统]支持按手机号查询用户", "支持按姓名、手机号查询用户", ["手机号"]),
        _atom("查询结果在列表中显示", "查询结果在列表中显示"),
        _atom("每页显示二十条", "每页显示二十条"),
    ],
    "skipped": [S3],
}


def _good(**changes):
    v = copy.deepcopy(GOOD)
    for path, val in changes.items():
        if path in ("skipped", "atoms"):
            v[path] = copy.deepcopy(val)
        else:
            i, key = path.split("_", 1)
            v["atoms"][int(i[1:]) - 1][key] = val
    return v


@pytest.fixture
def recipe():
    return p_atom()


@pytest.fixture
def block(recipe):
    (b,) = recipe.make_blocks(SEQ, NAME, DESC)
    return b


def _fake_run(blocks, verify, replies, retries=2):
    """照 taskmodel.run 的约定：每块最多 1 + retries 次，verify 回空就通过；没通过的块 outputs 是 None，drafts 是最后一次。"""
    outputs, drafts = [], []
    for b in blocks:
        ok = last = None
        for _ in range(1 + retries):
            v = replies.pop(0)
            last = v
            if not verify(b, v):
                ok = v
                break
        outputs.append(ok)
        drafts.append(last)
    return SimpleNamespace(outputs=outputs, drafts=drafts)


def _quote_msg(n, best):
    return f"第 {n} 项的 quote 不是说明里连着的一段原文；说明里最像的是：『{best}』。照原文抄"


# ---------------------------------------------------------------- 配方的形状
def test_recipe_shape_and_template_fill(recipe, block):
    assert recipe.prompt == T.PROMPT and recipe.user == T.USER == "按上面的规则拆这条需求。"
    assert set(re.findall(r"\$\{(\w+)\}", recipe.prompt)) == {"req_seq", "req_name", "req_desc"}
    assert recipe.prompt.count("$") == 3
    assert block["text"] == recipe.user
    assert (block["req_seq"], block["req_name"], block["req_desc"]) == (SEQ, NAME, DESC)
    assert (block["desc_start"], block["desc_end"], block["part"], block["parts"]) == (0, len(DESC), 0, 1)
    # 照 taskmodel 的填法：块里除 text / images 以外的键都填进模板
    fields = {k: ("" if v is None else str(v)) for k, v in block.items() if k not in ("text", "images")}
    filled = string.Template(recipe.prompt).substitute(fields)
    assert "需求：3 用户查询\n说明：\n" + DESC + "\n规则：\n" in filled
    assert recipe.run_kwargs == {"keep_going": True, "retries": 2}


def test_schema_follows_design_and_limits(recipe):
    sc = recipe.schema
    assert sc["required"] == ["atoms", "skipped"] and sc["additionalProperties"] is False
    atoms = sc["properties"]["atoms"]
    assert (atoms["minItems"], atoms["maxItems"]) == (1, 12)
    item = atoms["items"]
    assert list(item["properties"]) == ["text", "quote", "terms", "doubt"]
    assert item["required"] == ["text", "quote", "terms", "doubt"] and item["additionalProperties"] is False
    assert item["properties"]["text"]["minLength"] == 1
    assert item["properties"]["terms"]["maxItems"] == 3
    assert item["properties"]["terms"]["items"]["maxLength"] == 8
    assert sc["properties"]["skipped"] == {"type": "array", "items": {"type": "string"}}
    custom = p_atom(AtomLimits(max_atoms=5, terms_max=2, term_max=6)).schema["properties"]["atoms"]
    assert custom["maxItems"] == 5 and custom["items"]["properties"]["terms"]["maxItems"] == 2
    assert custom["items"]["properties"]["terms"]["items"]["maxLength"] == 6


def test_messages_override_rules():
    with pytest.raises(ValueError):
        p_atom(messages={"bogus": "x"})
    with pytest.raises(ValueError):
        p_atom(messages={"quote": None})
    with pytest.raises(ValueError):
        p_atom(AtomLimits(min_atoms=0))


# ---------------------------------------------------------------- 每一条校验：通过与不通过
def test_good_output_passes_and_merges(recipe, block):
    assert recipe.verify(block, GOOD) == []
    out = recipe.merge([block], [GOOD])
    assert [a.quote for a in out.atoms] == ["支持按姓名、手机号查询用户"] * 2 + ["查询结果在列表中显示", "每页显示二十条"]
    assert [(a.start, a.end) for a in out.atoms] == [(0, 13), (0, 13), (14, 24), (25, 32)]   # 同一子串出现两次：同一处
    assert all(DESC[a.start:a.end] == a.quote for a in out.atoms)
    assert [a.terms for a in out.atoms] == [("姓名",), ("手机号",), (), ()]
    assert out.skipped == (Span(33, 48, S3),)
    assert out.uncovered == () and out.failed == () and out.warnings == () and not out.empty
    assert out.to_dict()["atoms"][0]["text"] == "[系统]支持按姓名查询用户"


def test_quote_not_original(recipe, block):
    bad = _good(a1_quote="支持按姓名查询用户")                       # 跳了一段：不是连着的
    assert recipe.verify(block, bad) == [_quote_msg(1, S1)]
    prefix = _good(a1_quote="支持按姓名、手机号查询用户和部门")         # 只对上开头：也不算
    assert recipe.verify(block, prefix) == [_quote_msg(1, S1)]


def test_quote_normalized_counts_and_maps_back(recipe, block):
    v = _good(a1_quote="支持按姓名,手机号查询用户", a2_quote="支持按姓名， 手机号查询用户")
    assert recipe.verify(block, v) == []
    out = recipe.merge([block], [v])
    assert out.atoms[0].quote == out.atoms[1].quote == "支持按姓名、手机号查询用户"
    assert out.atoms[0].traced == "normalized" and out.atoms[2].traced == "exact"
    assert (out.atoms[0].start, out.atoms[0].end) == (0, 13)


def test_skipped_not_original(recipe, block):
    errs = recipe.verify(block, _good(skipped=["本功能是为了方便管理员使用。"]))
    assert "第 1 项的 skipped 不是说明里连着的一段原文；说明里最像的是：『本功能为了方便管理员日常使用。』。照原文抄" in errs


def test_terms_not_original(recipe, block):
    errs = recipe.verify(block, _good(a3_terms=["列表显示"]))
    assert errs == ["第 3 项的 terms 不是说明里连着的一段原文；说明里最像的是：『查询结果在列表中显示，每页显示二十条。』。照原文抄"]
    assert recipe.verify(block, _good(a3_terms=["列表"])) == []


def test_text_with_words_not_in_name_or_desc(recipe, block):
    errs = recipe.verify(block, _good(a3_text="查询结果在表格中显示并排序"))
    assert errs == ["第 3 项的 text 用了说明里没有的词：『格』『并排序』。只用名称和说明里的词，补的主语或对象放进方括号"]
    # 正好一成不算超（10 个字里 1 个没出现过）；补的词放进方括号不算
    assert recipe.verify(block, _good(a3_text="查询结果在列表中显现")) == []
    assert recipe.verify(block, _good(a3_text="[后台程序]查询结果在列表中显示")) == []
    # 名称里的词也算（「用户查询」）；全半角、大小写不计较
    assert recipe.verify(block, _good(a1_text="[系统]支持按姓名查询用户")) == []


def test_uncovered_clauses_are_soft_and_accepted_after_tries(recipe, block):
    v = _good(atoms=GOOD["atoms"][:3])                                # 没有「每页显示二十条」那一项
    msg = "说明里这几句没有落到任何一项的 quote 或 skipped：『每页显示二十条。』"
    assert recipe.verify(block, v) == [msg]
    assert recipe.verify(block, v) == [msg]
    assert recipe.verify(block, v) == []                             # 回过两次：照收
    out = recipe.merge([block], [v])
    assert out.uncovered == (Span(25, 33, "每页显示二十条。"),)
    assert out.failed == ()
    # 两句都没落到：『』之间按「』『」连
    r2 = p_atom()
    assert r2.verify(block, _good(atoms=GOOD["atoms"][:3], skipped=[])) == [
        "说明里这几句没有落到任何一项的 quote 或 skipped：『每页显示二十条。』『本功能为了方便管理员日常使用。』"]


def test_uncovered_ignores_short_clauses(recipe):
    (b,) = recipe.make_blocks("1", "导出", "支持导出。导出格式为表格文件。")
    v = {"atoms": [_atom("导出格式为表格文件", "导出格式为表格文件")], "skipped": []}
    assert recipe.verify(b, v) == []                                 # 「支持导出。」只有 4 个汉字
    assert recipe.merge([b], [v]).uncovered == ()


def test_hard_errors_and_soft_together(recipe, block):
    v = _good(atoms=GOOD["atoms"][:3], a1_quote="支持按姓名查询用户")
    errs = recipe.verify(block, v)
    assert errs[0] == _quote_msg(1, S1)
    assert errs[-1].startswith("说明里这几句没有落到任何一项的 quote 或 skipped：")


def test_soft_tries_end_to_end_with_taskmodel_style_runner(recipe, block):
    v = _good(atoms=GOOD["atoms"][:3])
    res = _fake_run([block], recipe.verify, [v, v, v])
    assert res.outputs == [v]                                         # 第三次照收
    out = recipe.merge([block], res)
    assert len(out.atoms) == 3 and [s.text for s in out.uncovered] == ["每页显示二十条。"]
    bad = _good(a1_quote="编出来的一句话")
    res2 = _fake_run([block], p_atom().verify, [bad, bad, bad])
    assert res2.outputs == [None]
    out2 = p_atom().merge([block], res2)
    assert out2.failed == (Span(0, len(DESC), DESC),) and out2.atoms == ()


def test_merge_accepts_last_draft_with_only_soft_errors(recipe, block):
    v = _good(atoms=GOOD["atoms"][:3])
    out = recipe.merge([block], SimpleNamespace(outputs=[None], drafts=[v]))
    assert len(out.atoms) == 3 and out.failed == ()
    assert any("照收了最后一次的输出" in w for w in out.warnings)
    hard = _good(a2_quote="不是原文")
    assert recipe.merge([block], SimpleNamespace(outputs=[None], drafts=[hard])).failed == (Span(0, 48, DESC),)


def test_item_count_outside_1_to_12_is_not_accepted(recipe, block):
    many = _good(atoms=[_atom("每页显示二十条", "每页显示二十条")] * 13)
    assert recipe.merge([block], [many]).failed == (Span(0, 48, DESC),)
    assert recipe.merge([block], [_good(atoms=[])]).failed == (Span(0, 48, DESC),)


# ---------------------------------------------------------------- 设计稿没写措辞的门槛：只记 warnings，给了措辞才回给模型
def test_text_length_limit(recipe, block):
    long80 = _good(a1_text="查询" * 40)
    assert recipe.verify(block, long80) == []
    assert recipe.merge([block], [long80]).warnings == ()
    long81 = _good(a1_text="查询" * 40 + "用")
    assert recipe.verify(block, long81) == []                         # 没定措辞：不回给模型
    assert recipe.merge([block], [long81]).warnings == ("第 1 项的 text 去掉方括号后 81 字，超过 80",)
    told = p_atom(messages={"text_long": "LONG {n} {count} {limit}"})
    assert told.verify(block, long81) == ["LONG 1 81 80"]
    bracketed = _good(a1_text="[系统]" + "查询" * 40)                  # 方括号里的不算进 80
    assert recipe.merge([block], [bracketed]).warnings == ()


def test_bracket_limit(recipe, block):
    ok = _good(a1_text="[" + "系统" * 6 + "]支持按姓名查询用户")
    assert recipe.merge([block], [ok]).warnings == ()
    over = _good(a1_text="[" + "系统" * 7 + "]支持按姓名查询用户")
    assert recipe.verify(block, over) == []
    assert recipe.merge([block], [over]).warnings == ("第 1 项的 text 方括号里合计 14 字，超过 12",)
    told = p_atom(messages={"bracket_long": "B{n}:{count}/{limit}"})
    assert told.verify(block, over) == ["B1:14/12"]


def test_term_length_limit(recipe, block):
    ok = _good(a1_terms=["姓名、手机号查询"])                          # 8 个字
    assert recipe.merge([block], [ok]).warnings == ()
    over = _good(a1_terms=["支持按姓名、手机号查询"])
    assert recipe.verify(block, over) == []
    assert recipe.merge([block], [over]).warnings == ("第 1 项的 term「支持按姓名、手机号查询」11 字，超过 8",)
    told = p_atom(messages={"term_long": "T{n}:{term}"})
    assert told.verify(block, over) == ["T1:支持按姓名、手机号查询"]


def test_blank_text(recipe, block):
    blank = _good(a1_text="   ")
    assert recipe.verify(block, blank) == []
    assert recipe.merge([block], [blank]).warnings == ("第 1 项的 text 全是空白",)
    assert p_atom(messages={"text_blank": "空{n}"}).verify(block, blank) == ["空1"]


def test_verify_without_schema_shape_is_a_caller_error(recipe, block):
    with pytest.raises(TypeError):
        recipe.verify(block, {"atoms": "不是列表"})


# ---------------------------------------------------------------- 说明为空、说明超长
def test_empty_description_makes_no_blocks(recipe):
    for desc in ("", "  \n ", None):
        assert recipe.make_blocks("1", "模块标题", desc) == []
    out = recipe.merge([], None)
    assert out.empty and out.atoms == () and out.failed == ()


def test_short_description_is_stripped_but_positions_kept(recipe):
    desc = "  \n支持导出。  "
    (b,) = recipe.make_blocks(1, "导出", desc)
    assert b["req_desc"] == "支持导出。" and b["desc_start"] == 3 and b["req_seq"] == 1
    v = {"atoms": [_atom("支持导出", "支持导出")], "skipped": []}
    out = recipe.merge([b], [v])
    assert (out.atoms[0].start, out.atoms[0].end) == (3, 7) and desc[3:7] == "支持导出"


def _long_desc():
    return "".join(f"支持按第{i}个字段查询记录，结果在列表中显示。" for i in range(150))


def test_long_description_is_chunked_by_sentence(recipe):
    desc = _long_desc()
    assert len(desc) > 1500
    blocks = recipe.make_blocks(SEQ, NAME, desc)
    assert len(blocks) >= 3
    assert blocks[0]["desc_start"] == 0 and blocks[-1]["desc_end"] == len(desc)
    for k, b in enumerate(blocks):
        assert b["req_desc"] == desc[b["desc_start"]:b["desc_end"]]
        assert len(b["req_desc"]) <= 1500 and b["req_desc"].endswith("。")
        assert (b["part"], b["parts"]) == (k, len(blocks))
        assert b["text"] == T.USER and b["req_name"] == NAME
    for a, b in zip(blocks, blocks[1:]):
        assert a["desc_end"] == b["desc_start"]                     # 首尾相接，没有漏掉的字


def test_long_description_merges_in_order_with_global_positions(recipe):
    desc = _long_desc()
    blocks = recipe.make_blocks(SEQ, NAME, desc)
    outputs = []
    for b in blocks:
        ss = sentences(b["req_desc"])
        v = {"atoms": [_atom(s.text[:-1], s.text[:-1]) for s in ss[:12]], "skipped": [s.text for s in ss[12:]]}
        assert recipe.verify(b, v) == []
        outputs.append(v)
    out = recipe.merge(blocks, outputs)
    assert len(out.atoms) == 12 * len(blocks)
    assert all(desc[a.start:a.end] == a.quote for a in out.atoms)
    starts = [a.start for a in out.atoms]
    assert starts == sorted(starts)
    assert [a.block for a in out.atoms] == [k for k in range(len(blocks)) for _ in range(12)]
    assert all(desc[s.start:s.end] == s.text for s in out.skipped)
    assert out.uncovered == () and out.failed == ()
    # 中间一块没跑通：只记那一段，别的照收
    outputs[1] = None
    part = recipe.merge(blocks, outputs)
    assert part.failed == (Span(blocks[1]["desc_start"], blocks[1]["desc_end"], blocks[1]["req_desc"]),)
    assert len(part.atoms) == 12 * (len(blocks) - 1)


def test_verify_messages_same_without_rapidfuzz(monkeypatch, block):
    monkeypatch.setitem(sys.modules, "rapidfuzz", None)
    monkeypatch.setitem(sys.modules, "rapidfuzz.fuzz", None)
    r = p_atom()
    assert r.verify(block, _good(a1_quote="支持按姓名查询用户")) == [_quote_msg(1, S1)]
    assert r.verify(block, _good(a3_terms=["列表显示"])) == [
        "第 3 项的 terms 不是说明里连着的一段原文；说明里最像的是：『查询结果在列表中显示，每页显示二十条。』。照原文抄"]
