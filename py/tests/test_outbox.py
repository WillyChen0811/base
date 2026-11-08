"""pybase.outbox：只测程序通路。两边的代理都是假的可执行文件（按参数和提示词回固定的 stream-json），不碰任何模型、不连云端。"""
from __future__ import annotations

import json
import os
import stat
import subprocess
import sys
import threading
import time
from pathlib import Path

import pytest

import pybase
from pybase.outbox import (CLAUDE, INVESTIGATE_PROMPT, OMLX, ORIGIN_TERMINAL, ClaudeAgent, Conflict, Gate, GateHits,
                           GateUnavailable, NotFound, OmlxAgent, Outbox, ScopeMissing, files_read, fill_scope,
                           investigate_argv, lines_source, names_source, outbox_from_config, parse_output,
                           static_source, steps_of, write_config)
from pybase.outbox.checks import BadInput
from pybase.outbox.claude import EXTRA_TOOLS, clean_env, memory_dir, terminal_model
from pybase.outbox.service import RESTARTED

FAKE_OMLX = r'''#!__PY__
import json, os, sys, time
a = sys.argv[1:]
if a == ["--list-models"]:
    if os.environ.get("FAKE_NO_MODELS"):
        print("oMLX 在 … 约 2 分钟内没有响应", file=sys.stderr); sys.exit(3)
    print("model-a\nmodel-b"); sys.exit(0)
if a == ["--default-model"]:
    print("model-a"); sys.exit(0)
assert a[0] == "--investigate" and a[-1] == "-", a
opts = {"--scope": [], "--system-file": [], "--resume": [], "--model": []}
stream, i = False, 1
while i < len(a) - 1:
    if a[i] == "--stream":
        stream = True; i += 1
    else:
        opts[a[i]].append(a[i + 1]); i += 2
prompt = sys.stdin.read()
sysfile = opts["--system-file"][0]
log = os.environ["FAKE_LOG"]
with open(log, "a", encoding="utf-8") as f:
    f.write(json.dumps({"side": "omlx", "argv": a, "prompt": prompt, "system": open(sysfile, encoding="utf-8").read(),
                        "sysfile": sysfile, "scope": opts["--scope"], "resume": (opts["--resume"] or [None])[0],
                        "model": (opts["--model"] or [None])[0], "stream": stream, "pid": os.getpid()},
                       ensure_ascii=False) + "\n")
n = sum(1 for x in open(log, encoding="utf-8") if '"side": "omlx"' in x)
if "要失败" in prompt:
    print("oMLX 在 http://127.0.0.1:18192 约 2 分钟内没有响应", file=sys.stderr); sys.exit(3)
def out(o):
    print(json.dumps(o, ensure_ascii=False), flush=True)
out({"type": "system", "subtype": "init", "session_id": "x"})
out({"type": "assistant", "message": {"content": [{"type": "text", "text": "先看看范围"},
     {"type": "tool_use", "id": "t1", "name": "Read", "input": {"file_path": opts["--scope"][0] + "/a.txt"}}]}})
if "一步一步" in prompt:                                     # 等测试放行（看得到前两步已经落库）
    while not os.path.exists(log + ".go"):
        time.sleep(0.02)
if "慢慢来" in prompt:
    time.sleep(60)
out({"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "content": "文件内容"}]}})
out({"type": "assistant", "message": {"content": [{"type": "thinking", "thinking": "嗯"},
     {"type": "tool_use", "id": "t2", "name": "Grep", "input": {"pattern": "风筝", "path": opts["--scope"][0]}}]}})
out({"type": "result", "subtype": "success", "is_error": False, "result": "原样的 claude 结果"})
out({"type": "result", "subtype": "success", "is_error": False, "num_turns": 3,
     "result": "\n\nomlx 回复%d：%s\n" % (n, prompt), "session_id": "omlx-sess-%d" % n,
     "omlx": {"denied": 0, "elapsed": 1, "model": (opts["--model"] or ["model-a"])[0]}})
'''

FAKE_CLAUDE = r'''#!__PY__
import json, os, sys, time
a = sys.argv[1:]
def val(k):
    return a[a.index(k) + 1] if k in a else None
prompt = sys.stdin.read()
log = os.environ["FAKE_LOG"]
adds = [a[i + 1] for i, x in enumerate(a) if x == "--add-dir"]
with open(log, "a", encoding="utf-8") as f:
    f.write(json.dumps({"side": "claude", "argv": a, "prompt": prompt, "system": val("--append-system-prompt"),
                        "resume": val("--resume"), "model": val("--model"), "adds": adds, "cwd": os.getcwd(),
                        "pid": os.getpid(), "env": {k: os.environ.get(k) for k in
                        ("CLAUDE_CODE_TMPDIR", "ANTHROPIC_BASE_URL", "CLAUDECODE", "CLAUDE_CONFIG_DIR")}},
                       ensure_ascii=False) + "\n")
n = sum(1 for x in open(log, encoding="utf-8") if '"side": "claude"' in x)
sid = val("--resume") or "claude-sess-%d" % n
def out(o):
    print(json.dumps(o, ensure_ascii=False), flush=True)
out({"type": "system", "subtype": "init", "session_id": sid, "model": "claude-" + (val("--model") or "缺省"),
     "tools": os.environ.get("FAKE_CLAUDE_TOOLS", "Glob,Grep,Read").split(","),
     "mcp_servers": [{"name": m} for m in os.environ.get("FAKE_CLAUDE_MCP", "").split(",") if m]})
if "慢慢来" in prompt:
    time.sleep(60)
out({"type": "assistant", "message": {"content": [{"type": "text", "text": "看了代码"}]}})
out({"type": "result", "subtype": "success", "is_error": False, "result": "Claude 回复：" + prompt, "session_id": sid,
     "total_cost_usd": 0.01})
'''

CLAUDE_PROMPT = "你在审核窗口里，代码随便读。"


def _exe(path, text):
    path.write_text(text.replace("__PY__", sys.executable), encoding="utf-8")
    path.chmod(0o755)
    return str(path)


@pytest.fixture
def rig(tmp_path, monkeypatch):
    b = tmp_path / "bin"
    b.mkdir()
    log = tmp_path / "calls.jsonl"
    cfgdir = tmp_path / "claude-config"
    cfgdir.mkdir()
    (cfgdir / "settings.json").write_text(json.dumps({"model": "opus", "hooks": {"Stop": []}}), encoding="utf-8")
    monkeypatch.setenv("FAKE_LOG", str(log))
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(cfgdir))
    monkeypatch.setenv("ANTHROPIC_BASE_URL", "http://不该用这个")
    monkeypatch.setenv("CLAUDECODE", "1")
    for k in ("FAKE_CLAUDE_TOOLS", "FAKE_CLAUDE_MCP", "FAKE_NO_MODELS", "PYBASE_OUTBOX_CONFIG"):
        monkeypatch.delenv(k, raising=False)
    repo = tmp_path / "代码仓库"
    other = tmp_path / "另一个仓库"
    repo.mkdir()
    other.mkdir()
    return {"omlx": _exe(b / "fake-omlx", FAKE_OMLX), "claude": _exe(b / "fake-claude", FAKE_CLAUDE), "log": log,
            "repo": str(repo), "other": str(other), "tmp": tmp_path}


def calls(rig, side=None):
    log = rig["log"]
    got = [json.loads(x) for x in log.read_text(encoding="utf-8").splitlines()] if log.exists() else []
    return [c for c in got if side is None or c["side"] == side]


def make_box(rig, sub="data", *, gate=True, timeout=30, config=None):
    d = rig["tmp"] / sub
    return Outbox(str(d), omlx=OmlxAgent([rig["omlx"]], timeout=timeout),
                  claude=ClaudeAgent([rig["claude"]], cwd=rig["repo"], add_dirs=[rig["other"]], timeout=timeout),
                  gate=Gate([static_source({"机密甲", "ABC123"})]) if gate else None, claude_prompt=CLAUDE_PROMPT,
                  id_prefix="conv-", config_path=config)


@pytest.fixture
def box(rig):
    return make_box(rig)


@pytest.fixture
def scope(tmp_path):
    d = tmp_path / "资料"
    d.mkdir()
    (d / "a.txt").write_text("蓝色风筝\n", encoding="utf-8")
    f = tmp_path / "单个文件.txt"
    f.write_text("x\n", encoding="utf-8")
    return [str(d), str(f)]


def mode(p) -> int:
    return stat.S_IMODE(os.stat(p).st_mode)


def wait_for(cond, timeout=15.0):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if cond():
            return True
        time.sleep(0.02)
    return False


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


def side_status(item):
    return [(r["side"], r["status"]) for r in item["rounds"]]


# ---------------------------------------------------------------- 命令行、输出解析、步骤

def test_argv_and_env(rig, tmp_path):
    assert investigate_argv(["agent", "-x"], ["/a", "/b"], "/s.txt", None, "m1") == [
        "agent", "-x", "--investigate", "--stream", "--model", "m1", "--scope", "/a", "--scope", "/b",
        "--system-file", "/s.txt", "-"]
    assert investigate_argv(["agent"], [], "/s", "sess-1", stream=False)[-4:] == ["/s", "--resume", "sess-1", "-"]
    c = ClaudeAgent(["claude"], cwd="/w", add_dirs=["/r1", "/r2"])
    a = c.argv("附加", "s-1", "sonnet")
    assert a[:5] == ["claude", "-p", "--output-format", "stream-json", "--verbose"]
    for flag in ("--restricted", "--strict-mcp-config", "--disable-slash-commands"):
        assert flag in a
    assert a[a.index("--tools") + 1] == "Read,Glob,Grep" and a[a.index("--permission-mode") + 1] == "dontAsk"
    assert json.loads(a[a.index("--settings") + 1]) == {"disableAllHooks": True}
    deny = a[a.index("--disallowedTools") + 1:a.index("--add-dir")]
    assert {"Edit", "Write", "Bash", "WebFetch", "WebSearch", "Agent", "Task", "NotebookEdit"} <= set(deny)
    assert [a[i + 1] for i, x in enumerate(a) if x == "--add-dir"] == ["/r1", "/r2"]
    assert a[-6:] == ["--append-system-prompt", "附加", "--model", "sonnet", "--resume", "s-1"]
    assert "--resume" not in c.argv("", None, None) and "--append-system-prompt" not in c.argv("", None, None)
    env = clean_env({"ANTHROPIC_BASE_URL": "x", "ANTHROPIC_AUTH_TOKEN": "y", "CLAUDECODE": "1",
                     "CLAUDE_CODE_ENTRYPOINT": "cli", "CLAUDE_CONFIG_DIR": "/c", "PATH": "/bin"})
    assert env == {"CLAUDE_CONFIG_DIR": "/c", "PATH": "/bin"}
    assert terminal_model() == "opus"                                         # rig 里假的用户设置
    assert terminal_model({"CLAUDE_CONFIG_DIR": str(tmp_path / "没有")}) == ""
    assert memory_dir("/home/u/Documents/codes/x-work", {"CLAUDE_CONFIG_DIR": "/c"}) == \
        Path("/c/projects/-home-u-Documents-codes-x-work/memory")


def test_parse_output_shapes():
    ok = parse_output('noise\n{"type":"result","is_error":false,"result":"好","session_id":"s1","num_turns":2}\n', "", 0)
    assert ok.ok and ok.reply == "好" and ok.session_id == "s1" and ok.info["num_turns"] == 2
    cl = parse_output('{"type":"result","result":"x","total_cost_usd":0.5,"permission_denials":[{}, {}]}', "", 0)
    assert cl.info["total_cost_usd"] == 0.5 and cl.info["denied"] == 2
    om = parse_output('{"type":"result","result":"x","total_cost_usd":0.5,"omlx":{"denied":1,"model":"m"}}', "", 0)
    assert "total_cost_usd" not in om.info and om.info["denied"] == 1 and om.info["model"] == "m"
    bad = parse_output('{"type":"result","is_error":true,"subtype":"error_max_turns","result":"","session_id":"s2"}',
                       "", 1)
    assert not bad.ok and bad.session_id == "s2" and "error_max_turns" in bad.error
    down = parse_output("", "line1\n连不上\n", 3)
    assert not down.ok and down.error.endswith("连不上") and down.session_id is None
    assert parse_output("", "", 2).error == "本地代理退出码 2，没有回结果"
    assert parse_output('{"type":"assistant"}', "", 2, who="Claude").error == "Claude退出码 2，没有回结果"
    assert parse_output("", "", -15, stopped=True).error == "停下了"
    assert "超时" in parse_output("", "", -15, timed_out=5).error
    assert parse_output('{"type":"result","result":"x"}', "", 0, refused="不让").error == "不让"


def test_steps_of_and_files_read():
    ev = {"type": "assistant", "message": {"content": [
        {"type": "thinking", "thinking": "想"}, {"type": "text", "text": "  先看看  "},
        {"type": "tool_use", "id": "t1", "name": "Read", "input": {"file_path": "/a", "limit": 5, "x": "y" * 900}},
        {"type": "tool_use", "id": "t2", "name": "Read", "input": {"file_path": "/b"}}]}}
    got = steps_of(ev)
    assert [s["kind"] for s in got] == ["text", "tool", "tool"] and got[0]["text"] == "先看看"
    assert got[1]["input"]["limit"] == 5 and len(got[1]["input"]["x"]) == 501             # 长参数截断
    res = steps_of({"type": "user", "message": {"content": [
        {"type": "tool_result", "tool_use_id": "t1", "content": "文件全文"},
        {"type": "tool_result", "tool_use_id": "t2", "is_error": True, "content": [{"type": "text", "text": "被拒了"}]}]}})
    assert res == [{"kind": "tool_result", "id": "t1", "error": False, "text": ""},              # 成功的结果不存内容
                   {"kind": "tool_result", "id": "t2", "error": True, "text": "被拒了"}]
    assert files_read(got + res) == ["/a"]                                                # 被拒的那次不算看过
    assert steps_of({"type": "assistant", "parent_tool_use_id": "p", "message": ev["message"]}) == []
    assert steps_of({"type": "result", "result": "x"}) == [] and steps_of("不是字典") == []


# ---------------------------------------------------------------- 两边各自开始、接着聊、转给另一边

def test_new_from_omlx_resume_and_steps(box, rig, scope):
    item = box.create(OMLX, "a.txt 里写了什么？", scope=scope)
    cid = item["id"]
    assert cid.startswith("conv-") and item["title"] == "a.txt 里写了什么？" and item["origin"] == "window"
    assert side_status(item) == [("omlx", "running")] and item["running"] == ["omlx"]
    item = box.wait(cid, 15)
    r1 = item["rounds"][0]
    assert r1["status"] == "done" and r1["reply"] == "omlx 回复1：a.txt 里写了什么？" and r1["error"] == ""
    assert r1["model"] == "model-a" and r1["source"] == "window" and r1["info"]["num_turns"] == 3
    assert [s["kind"] for s in r1["steps"]] == ["text", "tool", "tool_result", "tool"]          # 思考、result 不算步
    assert r1["steps"][1]["input"] == {"file_path": scope[0] + "/a.txt"} and r1["files"] == 1
    c1 = calls(rig, "omlx")[0]
    assert c1["stream"] and c1["scope"] == scope and c1["resume"] is None and c1["model"] is None
    assert c1["system"] == fill_scope(INVESTIGATE_PROMPT, scope) and not os.path.exists(c1["sysfile"])

    item = box.wait(box.send(cid, OMLX, "那单个文件呢？", model="model-b")["id"], 15)     # 接着聊：带上一轮的会话号
    c2 = calls(rig, "omlx")[1]
    assert c2["resume"] == "omlx-sess-1" and c2["model"] == "model-b"
    assert side_status(item) == [("omlx", "done"), ("omlx", "done")] and item["rounds"][1]["model"] == "model-b"
    assert item["last_model"] == {"claude": "", "omlx": "model-b"} and not calls(rig, "claude")


def test_new_from_claude_resume_then_forward_to_omlx(box, rig, scope):
    item = box.create(CLAUDE, "  帮我写一段给 omlx 的话：\n查 a.txt  ", title="")
    cid = item["id"]
    assert item["title"] == "帮我写一段给 omlx 的话： 查 a.txt"                     # 标题空：第一句话前 30 字
    item = box.wait(cid, 15)
    r1 = item["rounds"][0]
    assert r1["status"] == "done" and r1["reply"].startswith("Claude 回复：") and r1["model"] == "claude-opus"
    c1 = calls(rig, "claude")[0]
    assert c1["system"] == CLAUDE_PROMPT and c1["resume"] is None and c1["model"] == "opus"   # 缺省同终端（用户设置）
    assert c1["cwd"] == rig["repo"] and c1["adds"] == [rig["other"]]
    assert c1["env"]["ANTHROPIC_BASE_URL"] is None and c1["env"]["CLAUDECODE"] is None       # 不让环境把它指到别处
    tmpd = c1["env"]["CLAUDE_CODE_TMPDIR"]
    assert tmpd == str(box.claude_tmp) and mode(tmpd) == 0o700

    item = box.wait(box.send(cid, CLAUDE, "再改短点", model="sonnet")["id"], 15)
    c2 = calls(rig, "claude")[1]
    assert c2["resume"] == "claude-sess-1" and c2["model"] == "sonnet" and item["rounds"][1]["model"] == "sonnet"

    with pytest.raises(Conflict, match="范围是空的"):                                # 转给 omlx：先填范围
        box.send(cid, OMLX, item["rounds"][1]["reply"])
    box.update(cid, scope=scope)
    item = box.wait(box.send(cid, OMLX, "改过的：" + item["rounds"][1]["reply"])["id"], 15)
    assert side_status(item) == [("claude", "done"), ("claude", "done"), ("omlx", "done")]
    assert calls(rig, "omlx")[0]["resume"] is None                                   # 两边各是各的会话
    box.wait(box.send(cid, CLAUDE, "omlx 说：" + item["rounds"][2]["reply"])["id"], 15)
    assert calls(rig, "claude")[2]["resume"] == "claude-sess-1"


# ---------------------------------------------------------------- 闸门

def test_claude_gate_hits_need_force(box, rig, scope):
    with pytest.raises(GateHits) as e:
        box.create(CLAUDE, "里面有 机密甲 和 ABC123")
    assert e.value.hits == ["机密甲", "ABC123"] and e.value.code == "gate_hits"
    assert box.list() == [] and not calls(rig)                                       # 没建、没发
    item = box.wait(box.create(OMLX, "omlx 不查：机密甲", scope=scope)["id"], 15)     # 发给 omlx 不查
    cid = item["id"]
    with pytest.raises(GateHits):
        box.send(cid, CLAUDE, "转来的：机密甲")
    assert len(box.get(cid)["rounds"]) == 1
    item = box.wait(box.send(cid, CLAUDE, "转来的：机密甲", force=True)["id"], 15)     # 「仍然发送」
    assert item["rounds"][1]["forced"] is True and calls(rig, "claude")[0]["prompt"] == "转来的：机密甲"
    assert box.check("ABC123 在这") == ["ABC123"] and box.check("干净的") == []


def test_gate_unavailable_never_sends(rig, scope):
    nobody = make_box(rig, "d2", gate=False)
    with pytest.raises(GateUnavailable):
        nobody.create(CLAUDE, "干净的话", force=True)                               # 查不了：说了仍然发送也不发
    cid = nobody.create(OMLX, "问", scope=scope)["id"]
    nobody.wait(cid, 15)
    with pytest.raises(GateUnavailable):
        nobody.send(cid, CLAUDE, "干净的话", force=True)
    with pytest.raises(GateUnavailable):
        nobody.check("x")

    def boom():
        raise RuntimeError("里面有 机密")
    broken = make_box(rig, "d3")
    broken.gate = Gate([boom])
    with pytest.raises(GateUnavailable) as e:
        broken.create(CLAUDE, "x")
    assert "机密" not in str(e.value) and broken.list() == [] and not calls(rig, "claude")


# ---------------------------------------------------------------- 两边同时跑、停下、过程逐条落库、护栏

def test_both_sides_run_at_once_one_round_each(box, rig, scope):
    cid = box.create(OMLX, "慢慢来", scope=scope)["id"]
    item = box.send(cid, CLAUDE, "慢慢来")
    assert sorted(item["running"]) == ["claude", "omlx"]
    assert wait_for(lambda: len(calls(rig)) == 2)
    for side in (OMLX, CLAUDE):
        with pytest.raises(Conflict, match="正在跑"):
            box.send(cid, side, "再来一句")
    assert box.list()[0]["running"] == ["claude", "omlx"]
    item = box.stop(cid, OMLX)
    assert side_status(item) == [("omlx", "failed"), ("claude", "running")] and item["rounds"][0]["error"] == "停下了"
    assert wait_for(lambda: not _alive(calls(rig, "omlx")[0]["pid"]))
    with pytest.raises(Conflict, match="没在跑"):
        box.stop(cid, OMLX)
    item = box.stop(cid, CLAUDE)
    assert side_status(item) == [("omlx", "failed"), ("claude", "failed")] and item["running"] == []
    box.wait(box.send(cid, OMLX, "停了以后接着问")["id"], 15)
    assert calls(rig, "omlx")[1]["resume"] is None                                    # 停下的那轮没回会话号，不换


def test_steps_land_while_running(box, rig, scope):
    cid = box.create(OMLX, "一步一步", scope=scope)["id"]
    assert wait_for(lambda: len(box.get(cid)["rounds"][0]["steps"]) == 2)
    r = box.get(cid)["rounds"][0]
    assert r["status"] == "running" and [s["kind"] for s in r["steps"]] == ["text", "tool"]
    assert r["steps"][0]["text"] == "先看看范围" and r["steps"][0]["k"] == 1 and r["steps"][0]["at"]
    (rig["log"].parent / (rig["log"].name + ".go")).write_text("", encoding="utf-8")
    item = box.wait(cid, 15)
    assert item["rounds"][0]["status"] == "done" and len(item["rounds"][0]["steps"]) == 4


def test_claude_guard_extra_tools_or_mcp(box, rig, monkeypatch):
    monkeypatch.setenv("FAKE_CLAUDE_TOOLS", "Glob,Grep,Read,Bash")
    item = box.wait(box.create(CLAUDE, "问代码")["id"], 15)
    r = item["rounds"][0]
    assert r["status"] == "failed" and r["error"] == EXTRA_TOOLS % "工具 Bash" and r["reply"] == ""
    monkeypatch.setenv("FAKE_CLAUDE_TOOLS", "Read")
    monkeypatch.setenv("FAKE_CLAUDE_MCP", "claude.ai Gmail")
    item = box.wait(box.send(item["id"], CLAUDE, "再问")["id"], 15)
    assert item["rounds"][1]["error"] == EXTRA_TOOLS % "MCP claude.ai Gmail"


def test_failed_round_keeps_session(box, rig, scope):
    cid = box.create(OMLX, "第一问", scope=scope)["id"]
    box.wait(cid, 15)
    item = box.wait(box.send(cid, OMLX, "这次要失败")["id"], 15)
    r2 = item["rounds"][1]
    assert r2["status"] == "failed" and "没有响应" in r2["error"] and r2["reply"] == ""
    box.wait(box.send(cid, OMLX, "再问")["id"], 15)
    assert calls(rig, "omlx")[2]["resume"] == "omlx-sess-1"                       # 接最近一次答完的，不接失败那次


def test_delete_running_stops_it(box, rig, scope):
    cid = box.create(OMLX, "慢慢来", scope=scope)["id"]
    assert wait_for(lambda: len(calls(rig)) == 1)
    box.delete(cid)
    assert wait_for(lambda: not _alive(calls(rig)[0]["pid"])) and box.list() == []
    with pytest.raises(NotFound):
        box.get(cid)


def test_timeout(rig, scope):
    b = make_box(rig, "d4", timeout=0.5)
    item = b.wait(b.create(OMLX, "慢慢来", scope=scope)["id"], 15)
    assert item["rounds"][0]["status"] == "failed" and item["rounds"][0]["error"] == "超时：0.5 秒没跑完"


def test_stop_before_agent_started(box, scope):
    """点停下时代理还没起来（起子进程那一下）：起来马上停，这一轮记「停下了」。"""
    inner = box.agents[OMLX]

    class SlowStart:
        def __init__(self):
            self.entered, self.go = threading.Event(), threading.Event()

        def start(self, **kw):
            self.entered.set()
            self.go.wait(5)
            return inner.start(**kw)
    box.agents[OMLX] = slow = SlowStart()
    cid = box.create(OMLX, "慢慢来", scope=scope)["id"]
    assert slow.entered.wait(5)
    out = {}
    th = threading.Thread(target=lambda: out.setdefault("item", box.stop(cid, OMLX)))
    th.start()
    assert wait_for(lambda: bool(box._stop_asked))
    slow.go.set()
    th.join(15)
    assert out["item"]["rounds"][0]["status"] == "failed" and out["item"]["rounds"][0]["error"] == "停下了"


def test_recover_marks_stale_running(box, scope):
    cid = box.create(OMLX, "问", scope=scope)["id"]
    box.wait(cid, 15)
    dead = subprocess.Popen([sys.executable, "-c", "pass"])
    dead.wait()
    with box.store.tx() as c:
        c.execute("UPDATE round SET status='running', runner_pid=? WHERE conv_id=?", (dead.pid, cid))
    assert box.recover() == 1
    r = box.get(cid)["rounds"][0]
    assert r["status"] == "failed" and r["error"] == RESTARTED
    with box.store.tx() as c:                                               # 本进程跑的不碰
        c.execute("UPDATE round SET status='running', runner_pid=? WHERE conv_id=?", (os.getpid(), cid))
    assert box.recover() == 0


# ---------------------------------------------------------------- 终端发起：转回发起方、命令行入口

def test_forward_only_for_terminal_and_gated(box, scope):
    win = box.wait(box.create(OMLX, "窗口里建的", scope=scope)["id"], 15)
    with pytest.raises(Conflict, match="不是终端发起的"):
        box.forward(win["id"], "给谁")
    with pytest.raises(BadInput):
        box.create(OMLX, "问", scope=scope, origin=ORIGIN_TERMINAL)                     # 终端发起要有名字
    t = box.wait(box.create(OMLX, "终端来的", scope=scope, origin=ORIGIN_TERMINAL, requester="指挥")["id"], 15)
    assert t["origin"] == "terminal" and t["requester"] == "指挥" and t["rounds"][0]["requester"] == "指挥"
    assert t["rounds"][0]["source"] == "terminal"
    assert box.take_reply(t["id"]) is None
    with pytest.raises(GateHits):
        box.forward(t["id"], "有 ABC123")
    box.forward(t["id"], "第一段")
    item = box.forward(t["id"], "第二段 ABC123", force=True)
    assert side_status(item)[1:] == [("requester", "done"), ("requester", "done")] and item["rounds"][2]["forced"]
    assert item["rounds"][1]["delivered"] == ""
    assert box.take_reply(t["id"]) == "第一段" and box.take_reply(t["id"]) == "第二段 ABC123"
    assert box.take_reply(t["id"]) is None and box.get(t["id"])["rounds"][1]["delivered"]
    assert [x["id"] for x in box.list(origin="terminal")] == [t["id"]]
    assert [x["id"] for x in box.list(origin="window")] == [win["id"]]


def _ask(cfg, *args):
    root = os.path.dirname(os.path.dirname(os.path.abspath(pybase.__file__)))
    env = dict(os.environ, PYTHONPATH=os.pathsep.join([root] + [p for p in [os.environ.get("PYTHONPATH", "")] if p]))
    return subprocess.Popen([sys.executable, "-m", "pybase.outbox.ask", "--config", str(cfg), *args],
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)


def _config(rig, enabled=True):
    box = make_box(rig, "shared")
    cfg = rig["tmp"] / "cfg" / "outbox.json"
    write_config(cfg, data_dir=str(box.data_dir), id_prefix="conv-", enabled=enabled, omlx=box.agents[OMLX],
                 claude=box.agents[CLAUDE], claude_prompt=CLAUDE_PROMPT)
    box.config_path = str(cfg)
    return box, cfg


def _agent_pid(box, cid, side):
    with box.store.read() as c:
        r = box.store.running(c, cid, side)
    return r["agent_pid"] if r else None


def test_ask_wait_gets_only_forwarded_text(rig, scope):
    box, cfg = _config(rig)
    assert mode(cfg) == 0o600 and mode(cfg.parent) == 0o700
    p = _ask(cfg, "--title", "协查一下", "--scope", scope[0], "--from", "指挥", "--wait", "--timeout", "30", "-")
    p.stdin.write("帮我查 a.txt 写了什么".encode())
    p.stdin.close()
    p.stdin = None                                                                  # 后面 communicate 不再碰它
    assert wait_for(lambda: bool(box.list()) and box.list()[0]["last_status"] == "done", 30)
    s = box.list()[0]
    assert s["origin"] == "terminal" and s["requester"] == "指挥" and s["title"] == "协查一下"
    assert wait_for(lambda: box.get(s["id"])["requester_waiting"], 15)               # 窗口里看得到「发起方在等」
    r1 = box.get(s["id"])["rounds"][0]
    assert r1["side"] == "omlx" and r1["reply"].startswith("omlx 回复") and r1["source"] == "terminal"
    assert len(r1["steps"]) == 4
    assert calls(rig, "omlx")[0]["prompt"] == "帮我查 a.txt 写了什么" and calls(rig, "omlx")[0]["scope"] == [scope[0]]
    assert p.poll() is None                                                         # 用户没转：发起方什么也拿不到
    box.forward(s["id"], "用户看过、改过的结论")
    out, err = p.communicate(timeout=30)
    assert p.returncode == 0 and out.decode() == "用户看过、改过的结论\n"           # stdout 只有转回的话
    both = (out + err).decode()
    assert s["id"] in err.decode() and "omlx 回复" not in both and "先看看" not in both and "a.txt" not in both
    assert not box.get(s["id"])["requester_waiting"]


def test_ask_no_wait_then_wait_reply(rig, scope):
    box, cfg = _config(rig)
    first = box.create(OMLX, "先在窗口里填过范围", scope=[scope[1]])["id"]
    box.wait(first, 15)
    box.update(first, scope=[scope[0]])
    p = _ask(cfg, "-")                                                              # 不给 --scope：用最近一次填的
    out, err = p.communicate("问一句".encode(), timeout=30)
    lines = out.decode().splitlines()
    assert p.returncode == 0 and len(lines) == 2 and lines[0].startswith("会话号：conv-"), (out, err)
    cid = lines[0].split("：", 1)[1]
    assert wait_for(lambda: box.get(cid)["rounds"][0]["status"] == "done", 30)       # 命令退出了，后台进程照跑完
    assert box.get(cid)["requester"] == "终端" and calls(rig, "omlx")[-1]["scope"] == [scope[0]]
    p = _ask(cfg, "--wait-reply", cid, "--timeout", "0.3")
    out, err = p.communicate(timeout=30)
    assert p.returncode == 3 and out == b""
    box.forward(cid, "转回来的")
    p = _ask(cfg, "--wait-reply", cid)
    out, err = p.communicate(timeout=30)
    assert p.returncode == 0 and out.decode() == "转回来的\n"
    assert _ask(cfg, "--wait-reply", "conv-没有这个").wait(30) == 2                   # 会话号格式不对
    assert _ask(cfg, "--wait-reply", "conv-none").wait(30) == 1


def test_ask_refused_when_disabled_or_scope_missing(rig, scope, tmp_path):
    box, cfg = _config(rig, enabled=False)
    p = _ask(cfg, "--scope", scope[0], "-")
    out, err = p.communicate("问".encode(), timeout=30)
    assert p.returncode == 1 and "已关闭" in err.decode() and box.list() == [] and out == b""
    box, cfg = _config(rig)
    secret_path = str(tmp_path / "某某单位资料")
    p = _ask(cfg, "--scope", secret_path, "-")
    out, err = p.communicate("问".encode(), timeout=30)
    assert p.returncode == 1 and "1 项不在" in err.decode() and secret_path not in err.decode() and box.list() == []
    p = _ask(cfg, "-")
    out, err = p.communicate("问".encode(), timeout=30)
    assert p.returncode == 1 and "范围" in err.decode()                            # 没给，窗口里也没填过
    assert _ask(tmp_path / "没有配置.json", "--scope", scope[0], "-").wait(30) == 1


def test_stop_round_running_in_another_process(rig, scope):
    box, cfg = _config(rig)
    p = _ask(cfg, "--scope", scope[0], "-")
    out, _ = p.communicate("慢慢来".encode(), timeout=30)
    cid = out.decode().splitlines()[0].split("：", 1)[1]
    assert wait_for(lambda: len(calls(rig, "omlx")) == 1 and bool(_agent_pid(box, cid, OMLX)))
    other = make_box(rig, "shared")                                                  # 窗口那边（另一个 Outbox）
    item = other.stop(cid, OMLX)
    assert item["rounds"][0]["status"] == "failed" and item["rounds"][0]["error"] == "停下了"
    assert wait_for(lambda: not _alive(calls(rig, "omlx")[0]["pid"]))


# ---------------------------------------------------------------- 列表、模型、不许的、存储

def test_list_search_and_last_scope(box, scope):
    a = box.wait(box.create(OMLX, "蓝色风筝在哪", scope=[scope[0]], title="找风筝")["id"], 15)["id"]
    b = box.wait(box.create(CLAUDE, "代码里的 outbox 模块")["id"], 15)["id"]
    assert [x["id"] for x in box.list()] == [b, a]
    assert [x["id"] for x in box.list(q="风筝")] == [a] and [x["id"] for x in box.list(q="omlx 回复")] == [a]
    assert [x["id"] for x in box.list(q="Claude 回复")] == [b] and box.list(q="100%") == []
    s = box.list()[1]
    assert s["rounds"] == 1 and s["last_side"] == "omlx" and s["last_status"] == "done" and s["running"] == []
    assert box.last_scope() == [scope[0]]
    box.update(b, scope=[scope[1]])
    assert box.last_scope() == [scope[1]]
    with pytest.raises(BadInput):
        box.list(origin="别处")


def test_models(box, rig, monkeypatch):
    m = box.models()
    assert m["claude"] == {"default": "opus", "items": ["opus", "sonnet", "haiku", "fable"], "error": ""}
    assert m["omlx"] == {"default": "model-a", "items": ["model-a", "model-b"], "error": ""}
    monkeypatch.setenv("FAKE_NO_MODELS", "1")
    assert box.models()["omlx"]["items"] == ["model-a", "model-b"]                          # 60 秒内用缓存
    box._models = None
    got = box.models()["omlx"]
    assert got["items"] == [] and got["error"].startswith("列不出本地模型：") and "没有响应" in got["error"]


def test_refusals(box, rig, scope, tmp_path):
    for bad in (lambda: box.create("别的", "问"), lambda: box.create(OMLX, "  ", scope=scope),
                lambda: box.create(OMLX, "问", scope=["相对/路径"]), lambda: box.create(CLAUDE, "问", model="a b"),
                lambda: box.create(CLAUDE, "问", force="yes"), lambda: box.get("conv-没有"),
                lambda: box.create(CLAUDE, "x" * 200001)):
        with pytest.raises(BadInput):
            bad()
    with pytest.raises(ScopeMissing) as e:
        box.create(OMLX, "问", scope=[str(tmp_path / "没有")])
    assert e.value.missing == [str(tmp_path / "没有")] and e.value.code == "conflict"
    with pytest.raises(Conflict, match="范围是空的"):
        box.create(OMLX, "问")
    with pytest.raises(NotFound):
        box.send("conv-none", OMLX, "问")
    with pytest.raises(Conflict, match="不能另起后台进程"):
        box.create(OMLX, "问", scope=scope, detach=True)
    assert box.list() == [] and not calls(rig)                                     # 发不出就不建
    only = Outbox(str(tmp_path / "d5"), omlx=OmlxAgent([rig["omlx"]]))
    with pytest.raises(Conflict, match="没有配 Claude 一边"):
        only.create(CLAUDE, "问")
    assert only.models()["claude"]["error"] == "没有配 Claude 一边"


def test_old_investigation_db_is_rebuilt(rig, tmp_path, scope):
    import sqlite3
    d = tmp_path / "旧库"
    d.mkdir()
    c = sqlite3.connect(str(d / "outbox.sqlite3"))
    c.executescript("CREATE TABLE investigation (id TEXT PRIMARY KEY); CREATE TABLE round (inv_id TEXT, n INTEGER, "
                    "prompt TEXT); INSERT INTO investigation VALUES ('inv-1'); PRAGMA user_version=1;")
    c.close()
    b = Outbox(str(d), omlx=OmlxAgent([rig["omlx"]], timeout=30))
    item = b.wait(b.create(OMLX, "旧库换了以后", scope=scope)["id"], 15)
    assert item["rounds"][0]["status"] == "done" and [x["id"] for x in b.list()] == [item["id"]]


def test_store_files_private_and_config_roundtrip(box, rig, tmp_path):
    assert mode(box.data_dir) == 0o700 and mode(box.store.path) == 0o600
    cfg = tmp_path / "c" / "o.json"
    write_config(cfg, data_dir=str(box.data_dir), id_prefix="conv-", omlx=box.agents[OMLX], claude=box.agents[CLAUDE],
                 claude_prompt=CLAUDE_PROMPT)
    b2 = outbox_from_config(cfg)
    assert b2.agents[OMLX].cmd == [rig["omlx"]] and b2.agents[CLAUDE].add_dirs == [rig["other"]]
    assert b2.agents[CLAUDE].cwd == rig["repo"] and b2.prompts[CLAUDE] == CLAUDE_PROMPT and b2.gate is None
    assert b2.agents[OMLX].run_dir == str(box.run_dir) and b2.config_path == str(cfg)


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
