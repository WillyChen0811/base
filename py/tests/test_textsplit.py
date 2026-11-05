"""覆盖 pybase.textsplit 的底层件：分句、归一、回溯校验、切块、重切、合并。全部用合成文本。"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

from pybase.textsplit import (
    EXACT,
    NORMALIZED,
    NOT_FOUND,
    Line,
    Word,
    chunk,
    chunk_text,
    clauses,
    count_hanzi,
    fuzzy_backend,
    merge,
    normalize,
    resegment,
    resegment_many,
    sentences,
    similarity,
    trace,
)
from pybase.textsplit import tracing


def _texts(items):
    return [x.text for x in items]


def _positions_ok(original, items):
    return all(original[x.start:x.end] == x.text for x in items)


# ---------------------------------------------------------------- 分句
def test_sentences_full_and_half_width_stops_and_newline():
    t = "第一句。第二句？Third one! 第四句…第五句\n第六句"
    ss = sentences(t)
    assert _texts(ss) == ["第一句。", "第二句？", "Third one!", "第四句…", "第五句", "第六句"]
    assert _positions_ok(t, ss)


def test_sentences_ellipsis_and_repeated_stops_are_one_break():
    t = "等一下……然后呢？？好的。"
    assert _texts(sentences(t)) == ["等一下……", "然后呢？？", "好的。"]
    assert _texts(sentences("wait... ok.")) == ["wait...", "ok."]


def test_sentences_half_width_dot_exceptions():
    t = "价格是1.5元。1.支持查询；2.支持导出。打开config.json文件。"
    ss = sentences(t)
    assert _texts(ss) == ["价格是1.5元。", "1.支持查询；2.支持导出。", "打开config.json文件。"]
    assert _texts(ss[1].clauses) == ["1.支持查询；", "2.支持导出。"]


def test_clauses_break_on_comma_semicolon_colon_not_on_dunhao():
    t = "姓名、手机号，部门；时间：12:30，金额1,000元。"
    cs = clauses(t)
    assert _texts(cs) == ["姓名、手机号，", "部门；", "时间：", "12:30，", "金额1,000元。"]
    assert _positions_ok(t, cs)


def test_quotes_and_brackets_are_not_split_inside():
    t = "点击后提示“保存成功，请返回。”系统返回列表。"
    ss = sentences(t)
    assert _texts(ss) == ["点击后提示“保存成功，请返回。”", "系统返回列表。"]
    assert _texts(ss[0].clauses) == ["点击后提示“保存成功，请返回。”"]

    t2 = "支持导出（格式：xlsx，csv。默认xlsx）。下一句"
    ss2 = sentences(t2)
    assert _texts(ss2) == ["支持导出（格式：xlsx，csv。默认xlsx）。", "下一句"]
    assert len(ss2[0].clauses) == 1

    t3 = "他说：“好的。”，然后走了。"
    ss3 = sentences(t3)
    assert _texts(ss3) == [t3]
    assert _texts(ss3[0].clauses) == ["他说：", "“好的。”，", "然后走了。"]

    t4 = 'He said "a, b." Then.'
    assert _texts(sentences(t4)) == ['He said "a, b."', "Then."]


def test_unbalanced_quote_does_not_swallow_the_rest():
    t = "他说“没有右引号。下一句。"
    assert _texts(sentences(t)) == ["他说“没有右引号。", "下一句。"]


def test_sentences_strip_whitespace_and_empty():
    assert sentences("") == []
    assert sentences("  \n ") == []
    (s,) = sentences("  好。 ")
    assert (s.start, s.end, s.text) == (2, 4, "好。")
    assert count_hanzi("ab汉字，㐀") == 3


# ---------------------------------------------------------------- 归一与偏移表
def test_normalize_full_half_width_and_table():
    n = normalize("ＡＢ，１２。…“引”—")
    assert n.text == 'AB,12.…"引"-'
    assert n.origin == tuple(range(11))
    assert normalize("……").text == "……"
    assert normalize("「甲」『乙』‘丙’").text == '"甲""乙""丙"'
    assert normalize("、､｡").text == ",,."


def test_normalize_offsets_point_back_to_original():
    src = "a b　c​d"
    n = normalize(src)
    assert n.text == "abcd"
    assert n.origin == (0, 2, 4, 6)
    s, e = n.to_source(1, 3)
    assert src[s:e] == "b　c"
    x = normalize("x㈠y")                       # 一个字变成三个字
    assert x.text == "x(一)y"
    assert x.origin == (0, 1, 1, 1, 2)
    assert x.to_source(1, 4) == (1, 2)
    assert x.to_source(2, 3) == (1, 2)
    with pytest.raises(ValueError):
        x.to_source(0, 99)


# ---------------------------------------------------------------- 回溯校验
DESC = "支持按姓名、手机号查询用户。查询结果在列表中显示，每页显示二十条。本功能为了方便管理员日常使用。"


def test_trace_exact_and_all_occurrences():
    t = "支持按姓名查询。支持按姓名查询。导出报表。"
    rep = trace(t, ["支持按姓名查询"])
    r = rep.results[0]
    assert r.status == EXACT and r.ok
    assert r.spans == ((0, 7), (8, 15))
    assert r.span == (0, 7)
    assert r.best is None and r.score is None
    assert rep.fuzzy is None                     # 没有找不到的，没用上近似比对


def test_trace_ordered_picks_occurrence_not_before_previous():
    t = "甲支持查询。乙支持查询。"
    plain = trace(t, ["乙", "支持查询"])
    assert plain.results[1].span == (1, 5)
    ordered = trace(t, ["乙", "支持查询"], ordered=True)
    assert ordered.results[1].span == (7, 11)
    same = trace(t, ["支持查询", "支持查询"], ordered=True)     # 几项出自同一段：同一处
    assert same.results[0].span == same.results[1].span == (1, 5)


def test_trace_normalized_maps_back_to_original():
    for q in ("姓名,手机号", "姓名， 手机号", "姓名，手机号"):
        r = trace("姓名、手机号。", [q]).results[0]
        assert r.status == NORMALIZED and r.ok
        assert r.span == (0, 6)
    r = trace('点击“保存”按钮', ["点击「保存」按钮"]).results[0]
    assert r.status == NORMALIZED and r.span == (0, 8)
    r = trace("共　计 三条", ["共计三条"]).results[0]          # 中间夹着全角、半角空格
    assert r.status == NORMALIZED and r.span == (0, 6)


def test_trace_long_quote_over_64_chars():
    s1 = "用户在查询页面" + "输入姓名手机号部门" * 8 + "后点击查询按钮。"
    s2 = "系统在列表里显示结果，每页二十条。"
    t = s1 + s2
    q = s1[5:80]
    assert len(q) >= 64
    r = trace(t, [q]).results[0]
    assert r.status == EXACT and r.span == (5, 80)
    bad = q[:40] + "X" + q[41:]
    r = trace(t, [bad]).results[0]
    assert r.status == NOT_FOUND and r.span is None and r.spans == ()
    assert r.best.text == s1 and r.best.start == 0
    assert 0 < r.score < 100


def test_trace_rejects_prefix_only_match():
    r = trace("支持导出报表。", ["支持导出报表和打印"]).results[0]
    assert r.status == NOT_FOUND and not r.ok
    assert r.best.text == "支持导出报表。"
    long_src = "查询结果在列表中显示，每页显示二十条。"
    r = trace(long_src, [long_src[:10] + "并且可以按任意字段排序和筛选结果"]).results[0]
    assert r.status == NOT_FOUND


def test_trace_empty_or_blank_quote_is_not_original():
    for q in ("", "  ", "　"):
        r = trace("甲 乙。", [q]).results[0]
        assert r.status == NOT_FOUND


def test_trace_uncovered_clauses_with_min_hanzi():
    t = "背景说明。支持按姓名查询，支持按手机号查询。OK。"
    rep = trace(t, ["支持按姓名查询", "不存在的一句"])
    assert _texts(rep.uncovered) == ["背景说明。", "支持按手机号查询。", "OK。"]
    assert _positions_ok(t, rep.uncovered)
    rep6 = trace(t, ["支持按姓名查询"], min_hanzi=6)
    assert _texts(rep6.uncovered) == ["支持按手机号查询。"]
    # 碰到一点也算碰到；同一子串出现多次，每处都算碰到
    rep2 = trace("甲乙丙。丁戊己。甲乙丙。", ["丙。丁", "甲乙"])
    assert rep2.uncovered == ()


def test_trace_with_rapidfuzz():
    pytest.importorskip("rapidfuzz")
    assert fuzzy_backend() == "rapidfuzz"
    rep = trace(DESC, ["支持按姓名查询用户", "列表显示"])
    assert rep.fuzzy == "rapidfuzz"
    assert rep.results[0].best.text == "支持按姓名、手机号查询用户。"
    assert rep.results[1].best.text == "查询结果在列表中显示，每页显示二十条。"
    assert similarity("abc", "xxabcxx", backend="rapidfuzz") == 100


def test_trace_without_rapidfuzz_falls_back_to_difflib(monkeypatch):
    monkeypatch.setitem(sys.modules, "rapidfuzz", None)
    monkeypatch.setitem(sys.modules, "rapidfuzz.fuzz", None)
    assert fuzzy_backend() == "difflib"
    rep = trace(DESC, ["支持按姓名查询用户", "列表显示"])
    assert rep.fuzzy == "difflib"
    assert rep.results[0].best.text == "支持按姓名、手机号查询用户。"
    assert rep.results[1].best.text == "查询结果在列表中显示，每页显示二十条。"
    assert similarity("abc", "xxabcxx", backend="difflib") == 100
    assert tracing._difflib_partial("", "abc") == 0.0
    # 64 字以上的长引文，没装 RapidFuzz 时同样给出最像的那句
    s1 = "用户在查询页面" + "输入姓名手机号部门" * 8 + "后点击查询按钮。"
    bad = s1[5:45] + "X" + s1[46:80]
    r = trace(s1 + "系统在列表里显示结果。", [bad]).results[0]
    assert r.status == NOT_FOUND and r.best.text == s1


def test_textsplit_does_not_import_xxyi_or_aiclient():
    import ast

    pkg = Path(__import__("pybase.textsplit", fromlist=["x"]).__file__).parent
    for f in pkg.glob("*.py"):
        for node in ast.walk(ast.parse(f.read_text("utf-8"))):
            if isinstance(node, ast.Import):
                names = [a.name for a in node.names]
            elif isinstance(node, ast.ImportFrom):
                names = [node.module or ""] + [a.name for a in node.names]
            else:
                continue
            for name in names:
                top = name.split(".")[0]
                assert top not in ("xxyi", "cmdproto", "xxyi_work"), f"{f.name} import 了 {name}"
                assert "aiclient" not in name, f"{f.name} import 了 {name}"


# ---------------------------------------------------------------- 切块
def _body_nos(chunks):
    return [[p.no for p in c.body] for c in chunks]


def test_chunk_bodies_do_not_overlap_and_each_line_once():
    lines = [Line(i, "字" * (3 + (i * 7) % 20)) for i in range(40)]
    chunks = chunk(lines, max_chars=50, context=2)
    seen = [p.no for c in chunks for p in c.body]
    assert seen == list(range(40))                      # 每行恰好一次、按顺序
    for c in chunks:
        assert all(p.whole for p in c.body)
        assert sum(len(p.text) for p in c.body) <= 50
        assert c.first == c.body[0].no and c.last == c.body[-1].no
        if c.index:
            assert [p.no for p in c.context] == list(range(c.first - 2, c.first))
        else:
            assert c.context == ()
    assert [c.index for c in chunks] == list(range(len(chunks)))


def test_chunk_accepts_tuples_and_dicts_and_max_lines():
    lines = [(i, f"第{i}行") for i in range(7)]
    assert _body_nos(chunk(lines, max_lines=3)) == [[0, 1, 2], [3, 4, 5], [6]]
    dl = [{"no": f"L{i}", "text": "x", "start_ms": i, "end_ms": i} for i in range(3)]
    assert _body_nos(chunk(dl, max_lines=2)) == [["L0", "L1"], ["L2"]]
    with pytest.raises(ValueError):
        chunk(lines)
    with pytest.raises(ValueError):
        chunk(lines, max_chars=0)


def test_chunk_time_window_and_char_budget_together():
    minute = 60_000
    lines = [Line(i, "一二三四五六七八九十", i * minute, i * minute + 59_000) for i in range(20)]
    assert _body_nos(chunk(lines, max_chars=45, max_ms=10 * minute)) == [list(range(k, k + 4)) for k in range(0, 20, 4)]
    by_time = chunk(lines, max_chars=1000, max_ms=3 * minute)
    assert [len(c.body) for c in by_time] == [3, 3, 3, 3, 3, 3, 2]
    mixed = [Line(0, "甲" * 10, 0, 5000), Line(1, "乙" * 10, 60_000, 65_000), Line(2, "丙" * 30, 120_000, 125_000),
             Line(3, "丁" * 5, 600_000, 605_000), Line(4, "戊" * 5, 610_000, 615_000)]
    # 第一块字数先满（20 + 30 > 40）；第二块时间窗先满（120 秒到 605 秒 > 2 分钟）
    assert _body_nos(chunk(mixed, max_chars=40, max_ms=2 * minute)) == [[0, 1], [2], [3, 4]]
    with pytest.raises(ValueError):
        chunk([Line(0, "x")], max_ms=1000)


def test_chunk_super_long_single_line_sentence_then_clause_then_hard():
    a = "甲" * 30 + "。"
    b = "乙" * 15 + "，" + "丙" * 15 + "，" + "戊" * 15 + "。"
    c = "丁" * 70
    text = a + b + c
    chunks = chunk([Line("only", text)], max_chars=40)
    bodies = [[p.text for p in ch.body] for ch in chunks]
    assert bodies == [[a], ["乙" * 15 + "，" + "丙" * 15 + "，"], ["戊" * 15 + "。"], ["丁" * 40], ["丁" * 30]]
    pieces = [p for ch in chunks for p in ch.body]
    assert "".join(p.text for p in pieces) == text           # 片首尾相接拼回整行
    assert all(text[p.start:p.end] == p.text and p.no == "only" and not p.whole for p in pieces)
    assert [p.start for p in pieces] == [0, 31, 63, 79, 119]


def test_chunk_text_contiguous_spans():
    l0 = "甲" * 30 + "。"
    l2 = "乙" * 20 + "。" + "丙" * 20 + "。"
    l3 = "丁" * 5
    text = l0 + "\n\n" + l2 + "\n" + l3
    spans = chunk_text(text, 40)
    assert _texts(spans) == [l0, "乙" * 20 + "。", "丙" * 20 + "。\n" + l3]
    assert _positions_ok(text, spans)
    covered = {i for s in spans for i in range(s.start, s.end)}
    assert all(i in covered for i, ch in enumerate(text) if not ch.isspace())
    assert chunk_text("  \n ", 10) == []


# ---------------------------------------------------------------- 重切
def test_resegment_only_splits_and_estimates_time():
    text = "第一句。第二句话比较长，有两个小句，还有第三个小句。短。"
    segs = resegment(text, 0, 10_000, max_chars=10, seq=7, speaker="本机")
    assert _texts(segs) == ["第一句。", "第二句话比较长，", "有两个小句，", "还有第三个小句。", "短。"]
    assert [s.part for s in segs] == [0, 1, 2, 3, 4]
    assert all(s.seq == 7 and s.speaker == "本机" and s.estimated for s in segs)
    assert _positions_ok(text, segs)
    # 按字数比例估（只数汉字，标点不占时间）：3、7、5、7、1 个字，共 23
    assert [(s.start_ms, s.end_ms) for s in segs] == [(0, 1304), (1304, 4348), (4348, 6522), (6522, 9565),
                                                      (9565, 10_000)]


def test_resegment_always_cuts_at_sentence_end_without_limits():
    segs = resegment("好。好的。", 0, 1000)
    assert _texts(segs) == ["好。", "好的。"]                  # 短句也不并


def test_resegment_hard_cut_by_time_without_words():
    segs = resegment("一二三四五六七八九十", 0, 10_000, max_ms=3000)
    assert _texts(segs) == ["一二三", "四五六", "七八九", "十"]
    assert [(s.start_ms, s.end_ms) for s in segs] == [(0, 3000), (3000, 6000), (6000, 9000), (9000, 10_000)]


def test_resegment_uses_word_timing():
    text = "打开查询页面，输入手机号。"
    words = [Word("打开", 0, 400), ("查询", 500, 900), {"text": "页面", "start_ms": 1000, "end_ms": 1400},
             Word("输入", 3000, 3400), Word("手机号", 3500, 4200)]
    (one,) = resegment(text, 0, 5000, words=words)
    assert (one.start_ms, one.end_ms, one.estimated) == (0, 4200, False)
    segs = resegment(text, 0, 5000, max_chars=7, words=words)
    assert _texts(segs) == ["打开查询页面，", "输入手机号。"]
    assert [(s.start_ms, s.end_ms, s.estimated) for s in segs] == [(0, 1400, False), (3000, 4200, False)]


def test_resegment_hard_cut_on_word_boundaries():
    text = "甲乙丙丁戊己庚辛"
    words = [Word(c, i * 1000, i * 1000 + 900) for i, c in enumerate(text)]
    segs = resegment(text, 0, 8000, max_ms=2500, words=words)
    assert _texts(segs) == ["甲乙", "丙丁", "戊己", "庚辛"]
    assert [(s.start_ms, s.end_ms, s.estimated) for s in segs] == [
        (0, 1900, False), (2000, 3900, False), (4000, 5900, False), (6000, 7900, False)]


def test_resegment_words_that_do_not_match_fall_back_to_estimate():
    segs = resegment("完全不同的文字。", 1000, 2000, words=[Word("毫不相干", 1000, 1500)])
    assert len(segs) == 1 and segs[0].estimated
    assert (segs[0].start_ms, segs[0].end_ms) == (1000, 2000)
    clamped = resegment("好的。", 1000, 2000, words=[Word("好的", 500, 2500)])
    assert (clamped[0].start_ms, clamped[0].end_ms, clamped[0].estimated) == (1000, 2000, False)


def test_resegment_blank_and_bad_args():
    assert resegment("  ", 0, 10) == []
    with pytest.raises(ValueError):
        resegment("x", 10, 5)
    with pytest.raises(ValueError):
        resegment("x", 0, 5, max_chars=0)


def test_resegment_incremental_equals_batch_and_is_deterministic():
    utts = [{"seq": i, "text": t, "start_ms": i * 10_000, "end_ms": i * 10_000 + 8000, "speaker": "对方"}
            for i, t in enumerate(["第一段，带逗号的长句子在这里。短。", "第二段没有标点但是很长很长很长很长",
                                   "第三段。", "   "])]
    batch = list(resegment_many(utts, max_chars=8, max_ms=3000))
    fed = [s for u in utts for s in resegment_many([u], max_chars=8, max_ms=3000)]
    assert batch == fed == list(resegment_many(utts, max_chars=8, max_ms=3000))
    ids = [(s.seq, s.part) for s in batch]
    assert len(ids) == len(set(ids))
    assert all(s.speaker == "对方" for s in batch)
    for s in batch:
        assert len(s.text) <= 8 and s.end_ms - s.start_ms <= 3000


# ---------------------------------------------------------------- 合并
def test_merge_dedupes_by_key():
    outs = [[{"no": 1}, {"no": 2}], None, [{"no": 2, "x": 1}, {"no": 3}]]
    assert merge(outs, key="no") == [{"no": 1}, {"no": 2}, {"no": 3}]
    assert merge(outs, key=lambda x: x["no"]) == [{"no": 1}, {"no": 2}, {"no": 3}]
    assert merge(outs) == [{"no": 1}, {"no": 2}, {"no": 2, "x": 1}, {"no": 3}]


def test_merge_joins_touching_segments_across_blocks():
    outs = [
        [{"start": 1, "end": 5, "req": "A"}, {"start": 8, "end": 10, "req": "B"}],
        [{"start": 11, "end": 20, "req": "B"}],
        [{"start": 21, "end": 25, "req": "B"}, {"start": 26, "end": 28, "req": "C"}],
        [{"start": 30, "end": 31, "req": "C"}],                            # 不相接：不并
    ]
    assert merge(outs, span=("start", "end"), same="req") == [
        {"start": 1, "end": 5, "req": "A"}, {"start": 8, "end": 25, "req": "B"},
        {"start": 26, "end": 28, "req": "C"}, {"start": 30, "end": 31, "req": "C"}]
    # 同一块里的不动；主体不同的不并
    same_block = [[{"start": 1, "end": 3, "req": "A"}, {"start": 4, "end": 6, "req": "A"}]]
    assert merge(same_block, span=("start", "end"), same="req") == same_block[0]
    diff = [[{"start": 1, "end": 3, "req": "A"}], [{"start": 4, "end": 6, "req": "B"}]]
    assert len(merge(diff, span=("start", "end"), same="req")) == 2


def test_merge_with_custom_accessors():
    class Seg:
        def __init__(self, a, b, who):
            self.a, self.b, self.who = a, b, who

    order = ["L3", "L7", "L9", "L12"]                       # 编号不连续：按顺序算相接
    outs = [[Seg("L3", "L7", "x")], [Seg("L9", "L12", "x")]]
    got = merge(outs, span=lambda s: (s.a, s.b), same=lambda p, q: p.who == q.who,
                combine=lambda p, q: Seg(p.a, q.b, p.who),
                adjacent=lambda e, s: order.index(s) == order.index(e) + 1)
    assert [(s.a, s.b) for s in got] == [("L3", "L12")]
    with pytest.raises(TypeError):
        merge(outs, span=lambda s: (s.a, s.b), same=lambda p, q: True)
    with pytest.raises(TypeError):
        merge([[{"start": 1, "end": 2}]], span=("start", "end"))
