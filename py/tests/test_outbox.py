"""pybase.outbox：只测程序通路。本地代理用假的可执行文件（按参数和提示词回固定 JSON），不碰任何模型。"""
from __future__ import annotations

import json
import os
import stat
import subprocess
import sys
import time

import pytest

from pybase.outbox import (APPROVED, DRAFT, INVESTIGATE_PROMPT, REJECTED, RUNNING, WAITING, CommandAgent, Conflict,
                           Gate, GateHits, GateUnavailable, NotFound, Outbox, fill_scope, investigate_argv,
                           lines_source, names_source, parse_output, static_source)
from pybase.outbox.service import BadInput, RESTARTED

FAKE_AGENT = r'''#!__PY__
import json, os, sys, time
args = sys.argv[1:]
assert args[0] == "--investigate", args
scope, sysfile, resume, i = [], None, None, 1
while i < len(args) - 1:
    if args[i] == "--scope":
        scope.append(args[i + 1]); i += 2
    elif args[i] == "--system-file":
        sysfile = args[i + 1]; i += 2
    elif args[i] == "--resume":
        resume = args[i + 1]; i += 2
    else:
        raise SystemExit("不认识的参数 %r" % args[i])
assert args[-1] == "-", args
prompt = sys.stdin.read()
system = open(sysfile, encoding="utf-8").read()
log = os.environ["FAKE_AGENT_LOG"]
with open(log, "a", encoding="utf-8") as f:
    f.write(json.dumps({"argv": args, "prompt": prompt, "system": system, "sysfile": sysfile, "scope": scope,
                        "resume": resume, "pid": os.getpid()}, ensure_ascii=False) + "\n")
n = sum(1 for _ in open(log, encoding="utf-8"))
if "要失败" in prompt:
    print("oMLX 在 http://127.0.0.1:18192 约 2 分钟内没有响应", file=sys.stderr)
    sys.exit(3)
if "答不完" in prompt:
    print(json.dumps({"type": "result", "subtype": "error_max_turns", "is_error": True, "result": "",
                      "session_id": "sess-%d" % n}))
    sys.exit(1)
if "慢慢来" in prompt:
    time.sleep(60)
print("一些不是 JSON 的输出")
print(json.dumps({"type": "result", "subtype": "success", "is_error": False, "num_turns": 3,
                  "result": "\n\n回复%d：%s\n" % (n, prompt), "session_id": "sess-%d" % n,
                  "omlx": {"denied": 0, "elapsed": 1, "model": "fake"}}, ensure_ascii=False))
'''


@pytest.fixture
def agent_env(tmp_path, monkeypatch):
    exe = tmp_path / "bin" / "fake-agent"
    exe.parent.mkdir()
    exe.write_text(FAKE_AGENT.replace("__PY__", sys.executable), encoding="utf-8")
    exe.chmod(0o755)
    log = tmp_path / "agent.jsonl"
    monkeypatch.setenv("FAKE_AGENT_LOG", str(log))
    return exe, log


def calls(log):
    return [json.loads(x) for x in log.read_text(encoding="utf-8").splitlines()] if log.exists() else []


@pytest.fixture
def scope(tmp_path):
    d = tmp_path / "资料"
    d.mkdir()
    (d / "a.txt").write_text("蓝色风筝\n", encoding="utf-8")
    f = tmp_path / "单个文件.txt"
    f.write_text("x\n", encoding="utf-8")
    return [str(d), str(f)]


@pytest.fixture
def box(tmp_path, agent_env):
    exe, _log = agent_env
    return Outbox(str(tmp_path / "data"), approved_dir=str(tmp_path / "out" / "approved"),
                  rejected_dir=str(tmp_path / "out" / "rejected"), agent=CommandAgent([str(exe)], timeout=30),
                  gate=Gate([static_source({"机密甲", "ABC123"})]), id_prefix="inv-")


def mode(p) -> int:
    return stat.S_IMODE(os.stat(p).st_mode)


def wait_for(cond, timeout=10.0):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if cond():
            return True
        time.sleep(0.02)
    return False


# ---------------------------------------------------------------- 提示词、命令行、输出解析

def test_prompt_default_and_fill():
    assert "{范围}" in INVESTIGATE_PROMPT and INVESTIGATE_PROMPT.startswith("你在用户自己的电脑上帮他查资料、分析原因。")
    assert fill_scope("看：{范围}。{别的}", ["/a", "/b c"]) == "看：/a、/b c。{别的}"


def test_investigate_argv():
    assert investigate_argv(["agent", "-x"], ["/a", "/b"], "/s.txt", None) == [
        "agent", "-x", "--investigate", "--scope", "/a", "--scope", "/b", "--system-file", "/s.txt", "-"]
    assert investigate_argv(["agent"], [], "/s", "sess-1")[-3:] == ["--resume", "sess-1", "-"]


def test_parse_output_shapes():
    ok = parse_output('noise\n{"type":"result","is_error":false,"result":"好","session_id":"s1","num_turns":2}\n', "", 0)
    assert ok.ok and ok.reply == "好" and ok.session_id == "s1" and ok.info["num_turns"] == 2
    bad = parse_output('{"type":"result","is_error":true,"subtype":"error_max_turns","result":"","session_id":"s2"}',
                       "", 1)
    assert not bad.ok and bad.session_id == "s2" and "error_max_turns" in bad.error
    down = parse_output("", "line1\n连不上\n", 3)
    assert not down.ok and down.error.endswith("连不上") and down.session_id is None
    assert parse_output("", "", 2).error == "本地代理退出码 2，没有回结果"
    assert parse_output("", "", -15, stopped=True).error == "停下了"
    assert "超时" in parse_output("", "", -15, timed_out=5).error


# ---------------------------------------------------------------- 一整趟

def test_create_send_reply_followup_resume_draft_gate_approve_delete(box, agent_env, scope, tmp_path):
    _exe, log = agent_env
    inv = box.create("查一下 a 文件", "看看写了什么", scope, "a.txt 里写了什么？", origin="user")
    iid = inv["id"]
    assert iid.startswith("inv-") and inv["status"] == DRAFT
    assert [(r["n"], r["status"], r["prompt"]) for r in inv["rounds"]] == [(1, "pending", "a.txt 里写了什么？")]
    assert box.list()[0]["pending"] is True and not calls(log)                # 待发的没发出去

    got = box.send(iid)
    assert got["status"] == RUNNING and got["rounds"][0]["status"] == "running"
    got = box.wait(iid, 10)
    assert got["status"] == WAITING
    r1 = got["rounds"][0]
    assert r1["status"] == "done" and r1["reply"] == "回复1：a.txt 里写了什么？" and r1["error"] == ""
    assert got["draft"] == r1["reply"] and got["draft_edited"] is False         # 放行稿缺省跟着最新回复（首尾空行去掉）
    c1 = calls(log)[0]
    assert c1["scope"] == scope and c1["resume"] is None and c1["prompt"] == "a.txt 里写了什么？"
    assert c1["system"] == fill_scope(INVESTIGATE_PROMPT, scope) and "、".join(scope) in c1["system"]
    assert not os.path.exists(c1["sysfile"])                                   # 附加提示词文件用完就删

    got = box.wait(box.send(iid, "那单个文件呢？")["id"], 10)                  # 追问：接着上一轮的会话
    assert [r["status"] for r in got["rounds"]] == ["done", "done"]
    assert calls(log)[1]["resume"] == "sess-1" and got["draft"] == "回复2：那单个文件呢？"

    got = box.save_draft(iid, "改过的放行稿：机密甲 出现一次")
    assert got["draft_edited"] is True
    got = box.wait(box.send(iid, "再问一句")["id"], 10)
    assert calls(log)[2]["resume"] == "sess-2"
    assert got["draft"] == "改过的放行稿：机密甲 出现一次"                    # 改过就不再自动换

    assert box.check(iid) == ["机密甲"]
    assert box.check(iid, "ABC123 和 机密甲") == ["ABC123", "机密甲"]       # 窗口里还没存的文字也能查
    with pytest.raises(GateHits) as e:
        box.approve(iid)
    assert e.value.hits == ["机密甲"] and e.value.code == "gate_hits"
    assert not box.approved_path(iid).exists() and box.get(iid)["status"] == WAITING

    got = box.approve(iid, force=True)                                       # 「仍然放行」
    path = box.approved_path(iid)
    assert got["status"] == APPROVED and got["file"] == str(path) and got["closed"]
    assert mode(path) == 0o600 and mode(path.parent) == 0o700
    text = path.read_text(encoding="utf-8")
    assert text == "# 查一下 a 文件\n\n## 用途\n\n看看写了什么\n\n## 放行稿\n\n改过的放行稿：机密甲 出现一次\n"
    assert "回复1" not in text and "a.txt 里写了什么" not in text              # 各轮提示词、回复不外发
    for op in (lambda: box.send(iid, "x"), lambda: box.save_draft(iid, "y"), lambda: box.update(iid, title="z"),
               lambda: box.reject(iid, "n"), lambda: box.approve(iid, force=True)):
        with pytest.raises(Conflict):
            op()

    box.delete(iid)
    assert not path.exists() and box.list() == []
    with pytest.raises(NotFound):
        box.get(iid)


def test_reject_writes_title_and_note_only(box, scope):
    iid = box.create("标题乙", "用途乙", scope, "问", send=True)["id"]
    box.wait(iid, 10)
    with pytest.raises(GateHits) as e:
        box.reject(iid, "别外发 ABC123")
    assert e.value.hits == ["ABC123"] and not box.rejected_path(iid).exists()
    got = box.reject(iid, "方向不对，换个问法")
    p = box.rejected_path(iid)
    assert got["status"] == REJECTED and got["note"] == "方向不对，换个问法" and got["file"] == str(p)
    assert mode(p) == 0o600 and mode(p.parent) == 0o700
    assert p.read_text(encoding="utf-8") == "# 标题乙\n\n## 给指挥的话\n\n方向不对，换个问法\n"
    box.delete(iid)
    assert not p.exists()


def test_failed_round_resend_and_resume_from_earlier(box, agent_env, scope):
    _exe, log = agent_env
    iid = box.create("t", "", scope, "第一问", send=True)["id"]
    box.wait(iid, 10)
    got = box.wait(box.send(iid, "这次要失败")["id"], 10)
    r2 = got["rounds"][1]
    assert got["status"] == WAITING and r2["status"] == "failed" and "没有响应" in r2["error"] and r2["reply"] == ""
    assert got["draft"] == "回复1：第一问"                                     # 出错的轮不动放行稿
    with box.store.tx() as c:                                               # 重发前把提示词改成不失败的（同一轮）
        c.execute("UPDATE round SET prompt='重发这一轮' WHERE inv_id=? AND n=2", (iid,))
    got = box.wait(box.send(iid)["id"], 10)                                  # 不给提示词 = 重发出错的那一轮
    assert [r["status"] for r in got["rounds"]] == ["done", "done"] and len(got["rounds"]) == 2
    assert calls(log)[2]["resume"] == "sess-1"                               # 接第一轮的，不接失败那次


def test_not_done_keeps_session_for_next_round(box, agent_env, scope):
    _exe, log = agent_env
    iid = box.create("t", "", scope, "这次答不完", send=True)["id"]
    got = box.wait(iid, 10)
    assert got["rounds"][0]["status"] == "failed" and "error_max_turns" in got["rounds"][0]["error"]
    box.wait(box.send(iid, "接着说")["id"], 10)
    assert calls(log)[1]["resume"] == "sess-1"


def test_stop_kills_agent(box, agent_env, scope):
    _exe, log = agent_env
    iid = box.create("t", "", scope, "慢慢来", send=True)["id"]
    assert wait_for(lambda: len(calls(log)) == 1)
    pid = calls(log)[0]["pid"]
    got = box.stop(iid)
    assert got["status"] == WAITING and got["rounds"][0]["status"] == "failed" and got["rounds"][0]["error"] == "停下了"
    assert wait_for(lambda: not _alive(pid))
    with pytest.raises(Conflict):
        box.stop(iid)


def test_stop_before_agent_started(box, agent_env, scope):
    """点停下时代理还没起来（起子进程那一下）：起来马上停，这一轮记「停下了」。"""
    import threading
    inner = box.agent

    class SlowStart:
        def __init__(self):
            self.entered, self.go = threading.Event(), threading.Event()

        def start(self, **kw):
            self.entered.set()
            self.go.wait(5)
            return inner.start(**kw)
    box.agent = slow = SlowStart()
    iid = box.create("t", "", scope, "慢慢来", send=True)["id"]
    assert slow.entered.wait(5)
    out = {}
    th = threading.Thread(target=lambda: out.setdefault("item", box.stop(iid)))
    th.start()
    assert wait_for(lambda: iid in box._stop_asked)
    slow.go.set()
    th.join(15)
    got = out["item"]
    assert got["status"] == WAITING and got["rounds"][0]["status"] == "failed" and got["rounds"][0]["error"] == "停下了"


def test_delete_running_stops_it(box, agent_env, scope):
    _exe, log = agent_env
    iid = box.create("t", "", scope, "慢慢来", send=True)["id"]
    assert wait_for(lambda: len(calls(log)) == 1)
    box.delete(iid)
    assert wait_for(lambda: not _alive(calls(log)[0]["pid"])) and box.list() == []


def test_timeout(tmp_path, agent_env, scope):
    exe, _log = agent_env
    box = Outbox(str(tmp_path / "d2"), approved_dir=str(tmp_path / "a2"), rejected_dir=str(tmp_path / "r2"),
                 agent=CommandAgent([str(exe)], timeout=0.5), gate=Gate([static_source({"x"})]))
    got = box.wait(box.create("t", "", scope, "慢慢来", send=True)["id"], 15)
    assert got["rounds"][0]["status"] == "failed" and got["rounds"][0]["error"] == "超时：0.5 秒没跑完"


def _alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    try:                                                                      # 僵尸（还没被收）也算没了
        with open("/proc/%d/stat" % pid) as f:
            return f.read().split(") ")[-1][:1] != "Z"
    except OSError:
        return True


def test_recover_marks_stale_running(box, scope, tmp_path):
    iid = box.create("t", "", scope, "问")["id"]
    dead = subprocess.Popen([sys.executable, "-c", "pass"])
    dead.wait()
    with box.store.tx() as c:
        c.execute("UPDATE round SET status='running', runner_pid=? WHERE inv_id=?", (dead.pid, iid))
        c.execute("UPDATE investigation SET status='running' WHERE id=?", (iid,))
    assert box.recover() == 1
    got = box.get(iid)
    assert got["status"] == WAITING and got["rounds"][0]["status"] == "failed" and got["rounds"][0]["error"] == RESTARTED
    with box.store.tx() as c:                                               # 本进程跑的不碰
        c.execute("UPDATE round SET status='running', runner_pid=? WHERE inv_id=?", (os.getpid(), iid))
    assert box.recover() == 0


# ---------------------------------------------------------------- 不许的、存储

def test_send_refusals(tmp_path, box, scope):
    with pytest.raises(BadInput):
        box.create("t", "", scope, "  ", send=True)
    with pytest.raises(Conflict, match="范围是空的"):
        box.create("t", "", [], "问", send=True)
    with pytest.raises(Conflict, match="范围里这些不在"):
        box.create("t", "", [str(tmp_path / "没有")], "问", send=True)
    assert box.list() == []                                                  # 发不出就不建
    with pytest.raises(BadInput, match="绝对路径"):
        box.create("t", "", ["相对/路径"])
    iid = box.create("t", "", scope)["id"]
    with pytest.raises(Conflict, match="没有要发的一轮"):
        box.send(iid)
    with pytest.raises(Conflict, match="放行稿是空的"):
        box.approve(iid)
    nobody = Outbox(str(tmp_path / "d3"), approved_dir=str(tmp_path / "a3"), rejected_dir=str(tmp_path / "r3"))
    with pytest.raises(Conflict, match="没有配本地代理"):
        nobody.create("t", "", scope, "问", send=True)
    j = nobody.create("t", "", scope)["id"]
    nobody.save_draft(j, "稿")
    with pytest.raises(GateUnavailable):
        nobody.approve(j, force=True)                                        # 闸门查不了：说了仍然放行也不放
    assert nobody.get(j)["status"] == DRAFT and not nobody.approved_path(j).exists()


def test_put_prompt_replaces_pending_and_update(box, scope):
    iid = box.create("t", "", scope)["id"]
    box.put_prompt(iid, "第一版")
    got = box.put_prompt(iid, "第二版")
    assert [(r["n"], r["status"], r["prompt"]) for r in got["rounds"]] == [(1, "pending", "第二版")]
    got = box.update(iid, title="新标题", purpose="新用途", scope=[scope[0], scope[0] + "/"])
    assert got["title"] == "新标题" and got["purpose"] == "新用途" and got["scope"] == [scope[0]]


def test_store_files_private(box):
    assert mode(box.data_dir) == 0o700 and mode(box.store.path) == 0o600


def test_list_order(box, scope):
    a = box.create("草稿", "", scope)["id"]
    b = box.create("等用户", "", scope, "问", send=True)["id"]
    box.wait(b, 10)
    c = box.create("已退回", "", scope)["id"]
    box.reject(c, "")
    assert [x["id"] for x in box.list()] == [b, a, c]
    assert box.list()[0]["rounds"] == 1 and box.list()[0]["last_round"] == "done"


# ---------------------------------------------------------------- 闸门

def test_names_source_like_privacy_gate(tmp_path):
    root = tmp_path / "testdata"
    (root / "2026" / "某某单位-甲乙系统（第二期）").mkdir(parents=True)
    (root / "2026" / "某某单位-甲乙系统（第二期）" / "客户资料提供").mkdir()
    (root / "2026" / "260101A1234_报告.docx").write_text("", encoding="utf-8")
    (root / "2026" / "ab.txt").write_text("", encoding="utf-8")
    src = names_source(str(root), generic={"客户资料提供", "报告"}, code_patterns=[r"(\d{6})[A-Z]\d{4}"])
    got = src()
    assert {"某某单位-甲乙系统（第二期）", "某某单位", "甲乙系统", "第二期", "260101A1234_报告", "260101A1234"} <= got
    assert "客户资料提供" not in got and "报告" not in got and "2026" not in got and "ab" not in got
    g = Gate([src, static_source({"另一个"})])
    assert g.check("提到 甲乙系统 和 另一个，还有 260101A1234") == ["甲乙系统", "另一个", "260101A1234"]


def test_gate_unavailable_and_lines_and_cache(tmp_path):
    with pytest.raises(GateUnavailable, match="不在"):
        Gate([names_source(str(tmp_path / "没有"))]).check("x")
    empty = tmp_path / "空"
    empty.mkdir()
    with pytest.raises(GateUnavailable, match="一个串都没取到"):
        Gate([names_source(str(empty))]).check("x")
    with pytest.raises(GateUnavailable):
        Gate([]).check("x")
    f = tmp_path / "list.txt"
    f.write_text("# 注释\n\n  甲串  \n乙串\n", encoding="utf-8")
    now = [0.0]
    g = Gate([lines_source(str(f))], ttl=60, clock=lambda: now[0])
    assert g.strings() == {"甲串", "乙串"}
    f.write_text("丙串\n", encoding="utf-8")
    assert g.strings() == {"甲串", "乙串"}                                    # TTL 内用缓存
    now[0] = 61
    assert g.strings() == {"丙串"}
    with pytest.raises(GateUnavailable, match="读不了"):
        Gate([lines_source(str(tmp_path / "nofile"))]).check("x")

    def boom():
        raise RuntimeError("里面有 机密")
    with pytest.raises(GateUnavailable) as e:
        Gate([boom]).check("x")
    assert "机密" not in str(e.value)                                         # 错误里不带来源的内容
