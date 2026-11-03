"""cmdproto：解析、绑定回退、原子、执行与记账、流程编排。注册表是手写的小样本（虚构的"书/页"域）。"""

import json

import pytest

from pybase.cmdproto import (CANCEL, MISSING, CommandFailed, CondError, Flow, FlowError, Impl,
                             MissingParam, NotWired, ParseError, Registry, ResolveError, Scope,
                             Session, evaluate, parse)
from pybase.cmdproto.__main__ import main as cli_main

ATOM = "L0原子"


def _rows(cols, *lines):
    return [dict(zip(cols, line)) for line in lines]


DATA = {
    "commands": _rows(("cmd", "layer", "positional"),
                      ("/open -book", "L1", ""),
                      ("/list -page", "L1", ""),
                      ("/edit -page", "L1", ""),
                      ("/set -book --name", "L1", "P0111"),       # 子参数后直接跟值
                      ("/get -page --title", ATOM, "")),
    "params": _rows(("id", "cmd", "name", "type", "required", "default"),
                    ("P0101", "/open -book", "--name", "", "是", ""),
                    ("P0201", "/list -page", "book", "", "是", ""),         # 隐式：只能从上下文/绑定来
                    ("P0202", "/list -page", "--max", "整数", "", "10"),    # 字面缺省
                    ("P0203", "/list -page", "--tag", "", "", ""),
                    ("P0301", "/get -page --title", "--page", "", "是", ""),
                    ("P0302", "/get -page --title", "--prefix", "", "", ""),
                    ("P0401", "/edit -page", "--page", "", "是", ""),
                    ("P0402", "/edit -page", "title", "", "是", ""),
                    ("P0403", "/edit -page", "--serial", "", "", ""),       # 随 --page 算出的缺省
                    ("P0111", "/set -book --name", "--name", "", "是", "")),
    "outputs": _rows(("id", "cmd", "name", "path"),
                     ("O0101", "/open -book", "书名", "name"),
                     ("O0201", "/list -page", "页清单", "pages"),
                     ("O0301", "/get -page --title", "标题", ""),
                     ("O0401", "/edit -page", "结果", "status")),
    "human": _rows(("id", "name", "flag", "step"),
                   ("H0001", "要开哪本书", "--name", "S01"),
                   ("H0301", "标签", "--tag", "")),
    "context": _rows(("id", "key", "set_by", "pick"),
                     ("X01", "book.name", "O0101", "")),
    "config": _rows(("id", "key", "where"),
                    ("K01", "default_tag", "设置")),
    "steps": _rows(("id", "task", "actor"),
                   ("S01", "T0", "人"), ("S02", "T0", "程序"), ("S03", "T0", "程序"),
                   ("S04", "T0", "程序"), ("S05", "T0", "程序")),
    "calls": _rows(("id", "cmd", "step", "seq"),
                   ("C0002", "/open -book", "S02", 1),
                   ("C0003", "/list -page", "S03", 1),
                   ("C0004", "/edit -page", "S04", 1)),
    "flows": _rows(("src", "dst", "cond"),
                   ("S01", "S02", ""),
                   ("S02", "S03", 'C0002.O0101 == "A"'),
                   ("S02", "S05", ""),
                   ("S03", "S04", ""),
                   ("S03", "S05", 'failed("C0003")'),
                   ("S04", "S04", 'C0004.O0401 == "retry"'),      # 逐个循环：按本次调用的输出分流
                   ("S04", "结束", "")),
    "entries": _rows(("id",), ("shelf",)),
    "bindings": _rows(("id", "target", "kind", "ref", "cond", "prio", "pick", "call",
                                   "src_call", "parent"),
                      # /open -book 在流程里：书名来自人步给的 H0001
                      ("B01", "P0101", "人工", "H0001", "", "", "", "C0002", "", ""),
                      # /list -page：book 从上下文；--tag 条件行在前、配置行回退、再人工值缺省
                      ("B02", "P0201", "上下文", "X01", "", "", "", "C0003", "", ""),
                      ("B03", "P0203", "常量", "急", 'book.name == "B"', "", "", "C0003", "", ""),
                      ("B04", "P0203", "配置", "K01", "", "2", "", "C0003", "", ""),
                      ("B05", "P0203", "人工", "H0301", "", "1", "", "C0003", "", ""),
                      ("B06", "H0301", "常量", "普通", "", "", "", "", "", ""),       # 人工值的缺省行
                      # /edit -page：--page 逐个取上一步页清单的 id；title 由原子算出，原子输入指回所属行
                      ("B07", "P0401", "上步输出", "O0201", "", "", "逐个 .id", "C0004", "C0003", ""),
                      ("B08", "P0402", "原子输出", "O0301", "", "", "", "C0004", "", ""),
                      ("B09", "P0301", "上下文", "X01", "", "", "", "", "", "B08"),
                      ("B10", "P0302", "常量", "第", "", "", "", "", "", "B08")),
}


class Store:
    def __init__(self, **cfg):
        self.cfg = cfg

    def get_config(self, key, default=None):
        return self.cfg.get(key, default)


class Env:
    def __init__(self, **cfg):
        self.store = Store(**cfg)
        self.calls = []


def open_book(env, name):
    env.calls.append(("open", name))
    if name == "坏":
        return {"success": False, "message": "打不开"}
    return {"name": name}


def list_pages(env, book, max=None, tag=None):
    env.calls.append(("list", book, max, tag))
    if tag == "炸":
        return {"status": "error", "message": "列不出"}
    return {"pages": [{"id": f"{book}-{i}"} for i in range(1, 3)]}


def edit_page(env, page, title, serial=None):
    env.calls.append(("edit", page, title, serial))
    return {"status": "ok", "page": page}


def page_title(page, prefix=None):
    if not page:
        return MISSING
    return f"{prefix or ''}{page}"


IMPLS = {
    "/open -book": Impl(open_book, {"P0101": "name"}),
    "/list -page": Impl(list_pages, {"P0201": "book", "P0202": "max", "P0203": "tag"}),
    "/edit -page": Impl(edit_page, {"P0401": "page", "P0402": "title", "P0403": "serial"}),
}
ATOMS = {"/get -page --title": page_title}
COMPUTED = {"P0403": ("P0401", lambda page: f"{page}#")}


def session(data=None, **cfg):
    env = Env(**cfg)
    return Session(env, Registry(data or DATA), entry="shelf", impls=IMPLS, atoms=ATOMS,
                   computed_defaults=COMPUTED)


# ---------------------------------------------------------------- parser

def test_parse_names_flags_positional():
    known = set(Registry(DATA).commands)
    p = parse("/list -page --max 3 --tag", known)
    assert (p.cmd, p.flags, p.positional) == ("/list -page", {"--max": "3", "--tag": True}, None)
    p = parse('/set -book --name "有 空格"', known)
    assert p.cmd == "/set -book --name" and p.positional == "有 空格"
    p = parse("/get -page --title --page x", known)              # 子参数是指令名的一部分
    assert p.cmd == "/get -page --title" and p.flags == {"--page": "x"}


@pytest.mark.parametrize("line,msg", [
    ("list -page", "/动作"), ("/list", "-组"), ("/list -nothing", "没有这条指令"),
    ("/list -page stray", "多出来的值"), ("/list -page --max 1 --max 2", "两次"), ('/list -page "x', "引号"),
])
def test_parse_errors(line, msg):
    with pytest.raises(ParseError, match=msg):
        parse(line, set(Registry(DATA).commands))


# ---------------------------------------------------------------- cond

def test_cond_whitelist_and_missing_is_false():
    sc = Scope(entry="shelf", prev={"status": "ok", "a": {"b": 1}}, context={"book.name": "A"},
               visited={"S03", "T0"}, failed={"C0003"})
    assert evaluate("", sc) and evaluate(None, sc)
    assert evaluate('entry == "shelf" and prev.a.b == 1 and book.name in ["A", "B"]', sc)
    assert evaluate('visited("T0") and not visited("S09") and failed("C0003")', sc)
    assert not evaluate('prev.nope == "x"', sc) and not evaluate('prev.nope != "x"', sc)
    assert evaluate("empty(prev.nope) and not empty(prev.status)", sc)
    for bad in ('__import__("os")', "prev.status.upper()", "1 +", "visited(x)"):
        with pytest.raises(CondError):
            evaluate(bad, sc)


# ---------------------------------------------------------------- resolver

def test_user_flag_beats_binding_and_literal_default():
    s = session()
    s.context["book.name"] = "A"
    cmd, values, fanout, trace = s.plan("/list -page --max 5")
    assert values["P0201"] == "A" and trace["P0201"].startswith("B02")
    assert values["P0202"] == 5 and trace["P0202"] == "用户 --max"
    s2 = session()
    s2.context["book.name"] = "A"
    assert s2.plan("/list -page")[1]["P0202"] == 10 and "P 缺省" in s2.plan("/list -page")[3]["P0202"]


def test_binding_order_conditional_then_prio_then_human_default():
    s = session(default_tag="配置的")
    s.context["book.name"] = "B"
    assert s.plan("/list -page")[1]["P0203"] == "急"                       # 条件为真的行最先
    s.context["book.name"] = "A"
    assert s.plan("/list -page")[1]["P0203"] == "普通"                     # prio 1 的人工值缺省先于 prio 2 的配置
    assert s.plan("/list -page --tag 手给")[1]["P0203"] == "手给"          # 人工值的 flag 用户给了
    s2 = session()                                                          # 配置空：人工值缺省仍在
    s2.context["book.name"] = "A"
    _c, values, _f, trace = s2.plan("/list -page")
    assert values["P0203"] == "普通" and "取缺省 B06" in trace["P0203"]


def test_missing_param_lists_sources_not_guessing():
    s = session()
    with pytest.raises(MissingParam) as e:
        s.plan("/list -page")
    msg = str(e.value)
    assert "缺参数 book" in msg and "上下文 book.name" in msg and "用户直接给" not in msg   # 隐式参数用户给不了
    with pytest.raises(ResolveError, match="不认识参数 --nope"):
        s.plan("/list -page --nope 1")
    with pytest.raises(ValueError, match="不接值"):
        s.plan("/get -page --title x")                                      # 这条指令没登记定位值参数


def test_atom_output_row_and_computed_default():
    s = session()
    s.context["book.name"] = "A"
    s.record("/list -page", {"pages": [{"id": "A-1"}, {"id": "A-2"}]}, step="S03")
    cmd, values, fanout, trace = s.plan("/edit -page")
    assert values["P0401"] == ["A-1", "A-2"] and fanout == {"P0401"}       # 逐个
    assert values["P0402"] == "第A" and "原子 /get -page --title" in trace["P0402"]
    out = s.run("/edit -page --page A-1")
    assert out == {"status": "ok", "page": "A-1"} and s.env.calls[-1] == ("edit", "A-1", "第A", "A-1#")


def test_atom_as_command_and_atom_missing():
    s = session()
    assert s.run("/get -page --title --page p --prefix 第") == "第p"
    with pytest.raises(CommandFailed, match="算不出"):
        s.run('/get -page --title --page ""')
    s2 = Session(Env(), Registry(DATA), entry="shelf", impls=IMPLS)      # 没给原子表
    with pytest.raises(NotWired):
        s2.run("/get -page --title --page p")


# ---------------------------------------------------------------- runtime

def test_run_records_history_prev_context_and_failures():
    s = session()
    assert s.run("/open -book --name A") == {"name": "A"}
    assert s.context["book.name"] == "A" and s.prev == {"name": "A"} and s.visited == ["S02"]
    pages = s.run("/list -page")
    assert s.history["/list -page"] is pages and s.env.calls[-1] == ("list", "A", 10, "普通")
    with pytest.raises(CommandFailed, match="打不开"):
        s.run("/open -book --name 坏")
    assert s.context["book.name"] == "A"                                   # 失败不记账
    with pytest.raises(NotWired):
        Session(Env(), Registry(DATA), entry="shelf").run("/open -book --name A")


def test_fanout_calls_impl_per_item():
    s = session()
    s.run("/open -book --name A")
    s.run("/list -page")
    out = s.run("/edit -page")
    assert [o["page"] for o in out] == ["A-1", "A-2"]
    assert [c[1] for c in s.env.calls[-2:]] == ["A-1", "A-2"]


def test_hooks_are_called_in_order():
    seen = []

    class Mine(Session):
        def prepare(self, cmd, flags):
            seen.append(("prepare", cmd, flags))
            if flags is not None:
                flags.setdefault("--name", "A")

        def check(self, values, trace):
            seen.append(("check", sorted(values)))

        def before_execute(self, cmd, values):
            seen.append(("before", cmd))
            return "token"

        def after_execute(self, cmd, values, state):
            seen.append(("after", state))

        def finish(self, cmd, result):
            return {**result, "note": "标过"}

    s = Mine(Env(), Registry(DATA), entry="shelf", impls=IMPLS)
    assert s.run("/open -book") == {"name": "A", "note": "标过"}
    assert [x[0] for x in seen] == ["prepare", "check", "before", "after"]


# ---------------------------------------------------------------- flow

def test_flow_pauses_at_human_step_then_routes_by_output():
    s = session()
    f = Flow(s)
    out = f.start("S01")
    assert out.status == "paused" and out.step == "S01" and out.need == ["H0001"]
    out = f.resume({"H0001": "A"})
    assert out.status == "done"
    assert [t[1:] for t in out.trail] == [("S01", "S02"), ("S02", "S03"), ("S03", "S04"), ("S04", "结束"), ("S04", "结束")]
    assert s.visited == ["S01", "S02", "S03", "S04"]
    assert [c[0] for c in s.env.calls] == ["open", "list", "edit", "edit"]   # 逐页各走一遍 S04


def test_flow_default_edge_cancel_and_failed_edge():
    out = Flow(session(), ask={"S01": {"H0001": "Z"}}).start("S01")
    assert out.status == "done" and (None, "S02", "S05") in out.trail         # 条件不成立走缺省边
    out = Flow(session(), ask={"S01": CANCEL}).start("S01")
    assert out.status == "failed" and "缺参数" in out.message                 # 取消了人步，书名没人给
    data = json.loads(json.dumps(DATA))
    data["bindings"][5]["ref"] = "炸"                                         # 人工值缺省改成会让 list 失败的值
    s = session(data)
    out = Flow(s, ask={"S01": {"H0001": "A"}}).start("S01")
    assert out.status == "done" and (None, "S03", "S05") in out.trail and "C0003" in s.failed


def test_flow_human_step_wrong_id():
    with pytest.raises(FlowError, match="没有这些人工值"):
        Flow(session(), ask={"S01": {"H9999": 1}}).start("S01")


# ---------------------------------------------------------------- cli

def test_cli_plan_and_commands(tmp_path, capsys):
    reg = tmp_path / "reg.json"
    reg.write_text(json.dumps(DATA, ensure_ascii=False), "utf-8")
    assert cli_main(["--registry", str(reg), "commands"]) == 0
    assert "/open -book" in capsys.readouterr().out
    assert cli_main(["--registry", str(reg), "plan", "/open -book --name A"]) == 0
    assert "--name = \"A\"" in capsys.readouterr().out
    assert cli_main(["--registry", str(reg), "plan", "/open -book"]) == 1
    assert "缺参数" in capsys.readouterr().err
