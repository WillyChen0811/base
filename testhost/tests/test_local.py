"""本机自测：假 TH 目录 + 已有环境作替身，走完 sync→snapshot→prepare→run→fetch→clean。"""
import json
import os
import shlex
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import client  # noqa: E402

pytest.importorskip("xdist")
pytestmark = pytest.mark.skipif(not Path(client.ENTRY).exists(), reason="no entry")


def _git(d, *a):
    subprocess.run(["git", "-C", str(d), "-c", "user.name=t", "-c", "user.email=t@t", *a], check=True,
                   capture_output=True)


@pytest.fixture
def toy(tmp_path, monkeypatch):
    r = tmp_path / "toy"
    (r / "tests").mkdir(parents=True)
    (r / "cmdproto").mkdir()
    (r / "cmdproto/testenv.json").write_text(json.dumps({"python": "3.12", "pip": [], "env": {"FOO": "{run}/x"}}))
    (r / "tests/test_a.py").write_text(
        "import os\ndef test_ok(): assert os.environ['FOO'].endswith('/x')\n"
        "def test_bad(): assert 1 == 2, 'boom'\n")
    subprocess.run(["git", "init", "-q", str(r)], check=True)
    _git(r, "add", "-A")
    _git(r, "commit", "-qm", "i")
    th = tmp_path / "th"
    monkeypatch.setenv("TESTHOST_LOCAL", str(th))
    monkeypatch.setenv("TESTHOST_ENV_STUB", sys.prefix)
    return r, th


def test_full_flow(toy, capsys):
    r, th = toy
    argv = ["run", "--repo", f"{r}=toy", "--workdir", "toy", "-n", "2", "--run-id", "t1", "tests/test_a.py"]
    assert client.main(argv) == 1  # test_bad 失败，退出码跟随 pytest
    out = capsys.readouterr().out
    assert "1 通过 / 1 失败" in out and "test_bad" in out and "boom" in out
    assert (th / "runs/t1/src/toy/tests/test_a.py").exists()
    assert client.main(argv) == 1  # 第二次：仓库、环境都复用
    assert client.main(["clean", "runs"]) == 0
    assert not list((th / "runs").iterdir())


@pytest.mark.parametrize("cmd", [
    "run t1 toy --bad x.py", "run t1 toy ../etc/passwd", "run t1 toy -k -x a.py",
    "sync ../x", "prepare t1 toy=zzzz", "clean runs --keep x", "nope"])
def test_rejects(toy, cmd):
    tr = client.Transport("x", "x")
    with pytest.raises(client.THError):
        tr.call(cmd)


def test_eval_names():
    src = "A = ('a', 'b')\nB = ('c',)\nC = (*A, 'd', *B)\n"
    assert client.eval_names(src, ["C"]) == ["a", "b", "d", "c"]


def test_make_bundle_from_worktree(tmp_path):
    main = tmp_path / "main"
    main.mkdir()
    subprocess.run(["git", "init", "-q", str(main)], check=True)
    (main / "a.txt").write_text("a")
    _git(main, "add", "-A")
    _git(main, "commit", "-qm", "i")
    wt = tmp_path / "wt"
    _git(main, "worktree", "add", "-q", "-b", "side", str(wt))
    (wt / "b.txt").write_text("b")
    _git(wt, "add", "-A")
    _git(wt, "commit", "-qm", "w")
    sha = client.resolve(wt, "HEAD")
    before = _refs(wt)
    data = client.make_bundle(wt, sha, [])
    assert _refs(wt) == before          # 不往源仓库写 ref
    b = tmp_path / "x.bundle"
    b.write_bytes(data)
    dst = tmp_path / "dst.git"
    subprocess.run(["git", "init", "-q", "--bare", str(dst)], check=True)
    subprocess.run(["git", "-C", str(dst), "fetch", "-q", str(b), f"refs/testhost/{sha}:refs/testhost/{sha}"],
                   check=True)
    assert client.resolve(dst, f"refs/testhost/{sha}") == sha


def _refs(d):
    return subprocess.run(["git", "-C", str(d), "for-each-ref"], capture_output=True, check=True).stdout


XDIST_LOG = """\
[gw0] [ 10%] PASSED a/t.py::t1
[gw1] [ 20%] FAILED a/t.py::t2
[gw1] [ 30%] SKIPPED a/t.py::t3
[gw2] [ 40%] FAILED a/t.py::test_params[a b]
=================================== FAILURES ===================================
___________________________________ t2 ___________________________________
----------------------------- Captured stdout call -----------------------------
inner/x.py::t FAILED                                                     [ 50%]
[gw0] [ 50%] FAILED inner/x.py::t
 x.py::t FAILED
=========================== short test summary info ============================
FAILED a/t.py::t2 - boom
"""
PLAIN_LOG = """\
a/t.py::t1 PASSED                                                        [ 33%]
a/t.py::t2[x y] FAILED                                                   [ 66%]
a/t.py::t3 SKIPPED (why)                                                 [100%]
=================================== FAILURES ===================================
inner/x.py::t FAILED                                                     [ 50%]
"""


def _status(th, log):
    (th / "runs/s1/out").mkdir(parents=True)
    (th / "runs/s1/out/log").write_text(log)
    return client.Transport("x", "x").call("status s1")


def test_progress_ignores_inner_pytest_lines(toy):
    _, th = toy
    st = _status(th, XDIST_LOG)
    assert (st["passed"], st["failed"], st["skipped"]) == (1, 2, 1)
    (th / "runs/s1/out/log").write_text(PLAIN_LOG)
    st = client.Transport("x", "x").call("status s1")
    assert (st["passed"], st["failed"], st["skipped"]) == (1, 1, 1)


def test_finished_counts_from_junit(toy):
    _, th = toy
    _status(th, XDIST_LOG)
    (th / "runs/s1/out/exit").write_text("1\n")
    (th / "runs/s1/out/junit.xml").write_text(
        '<testsuites><testsuite tests="5" failures="1" errors="1" skipped="1"/></testsuites>')
    st = client.Transport("x", "x").call("status s1")
    assert (st["passed"], st["failed"], st["skipped"]) == (2, 2, 1)


PLUG = '''
import json, os
def pytest_sessionfinish(session, exitstatus):
    with open(os.environ["PLUG_OUT"], "w") as f:
        json.dump({"n": session.testscollected, "x": os.environ.get("PLUG_X")}, f)
'''


def test_plugin_and_fetch_extras(toy, tmp_path, capsys):
    r, th = toy
    plug = tmp_path / "myplug.py"
    plug.write_text(PLUG)
    out = tmp_path / "got"
    argv = ["run", "--repo", f"{r}=toy", "--workdir", "toy", "--run-id", "p1", "--plugin", str(plug),
            "--plugin-env", "PLUG_OUT={run}/cases.json", "--plugin-env", "PLUG_X=a=b",
            "--out-dir", str(out), "tests/test_a.py"]
    assert client.main(argv) == 1
    txt = capsys.readouterr().out
    assert "-n 1" in txt and "cases.json" in txt
    assert json.loads((out / "cases.json").read_text()) == {"n": 2, "x": "a=b"}
    assert not (out / "log.tail").exists() and not (out / "envvars").exists()
    # 另取
    out2 = tmp_path / "got2"
    assert client.main(["fetch", "p1", "--out-dir", str(out2)]) == 0
    assert (out2 / "cases.json").exists()


@pytest.mark.parametrize("cmd", [
    "plugin p1 ../x.py", "plugin p1 a-b.py", "plugin p1 a.txt",
    "run p1 toy --plugin ../x t.py", "run p1 toy --plugin a.b t.py", "run p1 toy --plugin-env PYTHONPATH=/x t.py",
    "run p1 toy --plugin-env 1A=x t.py"])
def test_plugin_rejects(toy, cmd):
    tr = client.Transport("x", "x")
    with pytest.raises(client.THError):
        tr.call(cmd)


# ---------- 通用环境描述 / 任务

def _toy2(tmp_path, monkeypatch, desc, relpath="testhost.json"):
    r = tmp_path / "toy2"
    (r / "tests").mkdir(parents=True)
    f = r / relpath
    f.parent.mkdir(parents=True, exist_ok=True)
    f.write_text(json.dumps(desc))
    (r / "tests/test_a.py").write_text("def test_ok(): pass\n")
    (r / "tool.py").write_text(
        "import os, sys\nos.makedirs('res/sub', exist_ok=True)\nopen('res/sub/a.txt', 'w').write(' '.join(sys.argv[1:]))\n"
        "open(os.environ['TMPDIR'] + '/../out/b.txt', 'w').write(os.environ['HOME'] + '|' + os.environ['TESTHOST_TASK'])\n"
        "print('done', len(sys.argv) - 1)\nsys.exit(int(os.environ.get('RC', '0')))\n")
    subprocess.run(["git", "init", "-q", str(r)], check=True)
    _git(r, "add", "-A")
    _git(r, "commit", "-qm", "i")
    th = tmp_path / "th"
    monkeypatch.setenv("TESTHOST_LOCAL", str(th))
    monkeypatch.setenv("TESTHOST_ENV_STUB", sys.prefix)
    return r, th


def _envhash(th, rid):
    return (th / "runs" / rid / "envhash").read_text().strip()


def test_env_path_resolution(tmp_path, monkeypatch):
    r, th = _toy2(tmp_path, monkeypatch, {"python": "3.12", "pip": []})
    base = ["run", "--repo", f"{r}=toy2", "--workdir", "toy2", "tests/test_a.py"]
    assert client.main(base + ["--run-id", "e1"]) == 0                # 缺省找到根下 testhost.json
    assert (th / "runs/e1/envfile").read_text().strip() == "src/toy2/testhost.json"
    # 显式路径优先；两个都有时缺省仍是 testhost.json
    (r / "other").mkdir()
    (r / "other/env.json").write_text(json.dumps({"python": "3.12", "pip": ["six"]}))
    (r / "cmdproto").mkdir()
    (r / "cmdproto/testenv.json").write_text(json.dumps({"python": "3.12", "pip": ["six", "idna"]}))
    _git(r, "add", "-A")
    _git(r, "commit", "-qm", "more")
    assert client.main(base + ["--run-id", "e2", "--env-file", "other/env.json"]) == 0
    assert (th / "runs/e2/envfile").read_text().strip() == "src/toy2/other/env.json"
    assert client.main(base + ["--run-id", "e3"]) == 0
    assert (th / "runs/e3/envfile").read_text().strip() == "src/toy2/testhost.json"
    assert _envhash(th, "e2") != _envhash(th, "e3")
    with pytest.raises(client.THError):
        client.plan(client.argparse.Namespace(repo=[f"{r}=toy2"], commit_of=[], snapshot=[], env_file="nope.json", task=None))


def test_env_path_legacy_fallback(toy):
    r, th = toy       # toy 只有 cmdproto/testenv.json
    assert client.main(["run", "--repo", f"{r}=toy", "--workdir", "toy", "-n", "1", "--run-id", "l1", "tests/test_a.py"]) == 1
    assert (th / "runs/l1/envfile").read_text().strip() == "src/toy/cmdproto/testenv.json"


def test_hash_old_desc_unchanged_and_conda(tmp_path, monkeypatch):
    import hashlib
    r, th = _toy2(tmp_path, monkeypatch, {"python": "3.12", "pip": ["six"]})
    argv = ["run", "--repo", f"{r}=toy2", "--workdir", "toy2", "tests/test_a.py", "--run-id"]
    assert client.main(argv + ["h1"]) == 0
    old = hashlib.sha256(json.dumps(["3.12", sorted(["six", "pytest", "pytest-xdist"])]).encode()).hexdigest()[:12]
    assert _envhash(th, "h1") == old              # 没有 conda：与旧算法完全一致，旧缓存继续命中
    (r / "testhost.json").write_text(json.dumps({"python": "3.12", "pip": ["six"], "conda": ["nodejs=20"]}))
    _git(r, "add", "-A")
    _git(r, "commit", "-qm", "conda")
    assert client.main(argv + ["h2"]) == 0
    assert _envhash(th, "h2") != old
    (r / "testhost.json").write_text(json.dumps({"python": "3.12", "pip": [], "conda": ["-bad"]}))
    _git(r, "add", "-A")
    _git(r, "commit", "-qm", "bad")
    assert client.main(argv + ["h3"]) == 3        # conda 包名不合法


TASK_DESC = {"python": "3.12", "pip": [], "tasks": {
    "mk": {"cmd": ["python", "tool.py", "{run}/x"], "cwd": ".", "outputs": ["res/**/*.txt", "src/toy2/res/sub/a.txt"]},
    "sub": {"cmd": ["python", "../tool.py"], "cwd": "tests", "outputs": []},
    "evil": {"cmd": ["python", "tool.py"], "outputs": ["../../etc/passwd"]}}}


def test_task_run_and_fetch(tmp_path, monkeypatch, capsys):
    r, th = _toy2(tmp_path, monkeypatch, TASK_DESC)
    out = tmp_path / "got"
    argv = ["run", "--repo", f"{r}=toy2", "--workdir", "toy2", "--task", "mk", "--run-id", "k1", "--out-dir", str(out)]
    assert client.main(argv + ["--", "--flag", "a b", "-x"]) == 0
    txt = capsys.readouterr().out
    assert "任务 mk 退出码 0" in txt and "done 4" in txt           # {run}/x + 3 个追加参数
    assert (out / "src/toy2/res/sub/a.txt").read_text() == f"{th}/runs/k1/x --flag a b -x"
    assert (out / "b.txt").read_text() == f"{th}/runs/k1/home|mk"      # out/ 前缀去掉，HOME 指进 run 目录
    st = client.Transport("x", "x").call("status k1")
    assert st["task"] == "mk" and st["state"] == "finished" and st["exit"] == 0 and "done 4" in st["log_tail"]
    # 退出码照传；cwd 相对工作子目录
    assert client.main(["run", "--repo", f"{r}=toy2", "--workdir", "toy2", "--task", "sub", "--run-id", "k2",
                        "--plugin-env", "RC=7"]) == 7
    assert (th / "runs/k2/src/toy2/tests/res/sub/a.txt").exists()


def test_task_rejects(tmp_path, monkeypatch):
    r, th = _toy2(tmp_path, monkeypatch, TASK_DESC)
    base = ["run", "--repo", f"{r}=toy2", "--workdir", "toy2"]
    assert client.main(base + ["--task", "nope"]) == 3                       # 描述里没有
    assert client.main(base + ["--task", "mk", "-n", "2"]) == 3              # 不能与 pytest 选项并用
    assert client.main(base + ["--task", "evil", "--run-id", "ev"]) == 3     # outputs 逃逸
    assert client.main(base) == 3                                            # 没测试也没任务
    tr = client.Transport("x", "x")
    for cmd in ["run ev toy2 -- a", "run ev toy2 --task ../x", "run ev toy2 --task mk t.py", "run ev toy2 --task mk -k x"]:
        with pytest.raises(client.THError):
            tr.call(cmd)


def _pids_matching(pattern):
    """命令行含指定路径的进程（macOS/Linux 通用）。"""
    out = subprocess.run(["ps", "-axo", "pid=,command="], capture_output=True, text=True).stdout
    hits = []
    for line in out.splitlines():
        parts = line.strip().split(None, 1)
        if len(parts) == 2 and pattern in parts[1]:
            hits.append(int(parts[0]))
    return hits


def _alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    st = subprocess.run(["ps", "-o", "state=", "-p", str(pid)], capture_output=True, text=True).stdout.strip()
    return st != "" and st[0] != "Z"


def _kill_soft(pids):
    for pid in sorted(pids):
        try:
            os.kill(pid, signal.SIGKILL)
        except (ProcessLookupError, ValueError):
            pass


# ---------- cancel：停止运行
# 长跑测试体：p1 命令行带 run 目录路径（子进程）；p2 纯 sleep 不带路径（只能靠进程树）；
# 再起一个 nohup 脱离父链的孤儿（模拟测试起的临时 PG postmaster），命令行带 run 目录路径。
LONG_SRC = '''import os, subprocess, sys, time


def test_long():
    tmp = os.environ["TMPDIR"]
    path = os.path.join(tmp, "orphan.py")
    with open(path, "w") as f:
        f.write("import time\\ntime.sleep(300)\\n")
    p1 = subprocess.Popen([sys.executable, path])
    p2 = subprocess.Popen(["sh", "-c", "exec sleep 300"])
    subprocess.Popen(["sh", "-c", "nohup %s %s >/dev/null 2>&1 & exit 0" % (sys.executable, path)])
    with open(os.path.join(tmp, "kids.txt"), "w") as f:
        f.write("%d %d\\n" % (p1.pid, p2.pid))
    time.sleep(120)
'''


@pytest.fixture
def toy_long(tmp_path, monkeypatch):
    r = tmp_path / "longrepo"
    (r / "tests").mkdir(parents=True)
    (r / "testhost.json").write_text(json.dumps({"python": "3.12", "pip": [], "env": {}}))
    (r / "tests/test_long.py").write_text(LONG_SRC)
    subprocess.run(["git", "init", "-q", str(r)], check=True)
    _git(r, "add", "-A")
    _git(r, "commit", "-qm", "i")
    th = tmp_path / "th"
    monkeypatch.setenv("TESTHOST_LOCAL", str(th))
    monkeypatch.setenv("TESTHOST_ENV_STUB", sys.prefix)
    yield r, th
    # 兜底：kids.txt 记录的裸 sleep + 命令行含假 TH 路径的一切残留
    for kf in th.glob("runs/*/tmp/kids.txt"):
        _kill_soft(int(x) for x in kf.read_text().split())
    _kill_soft(_pids_matching(str(th) + "/"))


def _start_long_run(tr, th, r, sha, rid):
    """不走 client 的 run 编排（那会一直等）：prepare + run 直接起，等它进入 running 且孩子都起来了。"""
    tr.call(" ".join(shlex.quote(x) for x in ["prepare", rid, f"longrepo={sha}", "--env", "testhost.json"]))
    tr.call(f"run {rid} longrepo -n 1 tests/test_long.py")
    R = th / "runs" / rid
    for i in range(120):
        if (R / "tmp" / "kids.txt").exists():
            time.sleep(0.5)     # 再等孤儿起稳
            return [int(x) for x in (R / "tmp" / "kids.txt").read_text().split()]
        st = tr.call(f"status {rid}")
        assert st["state"] in ("prepared", "running"), st
        time.sleep(0.5)
    raise AssertionError("长跑 run 没起来")


def _wait_gone(th, pat, secs=15):
    """等命令行含 pat 的进程全部消失（僵尸由 init 回收，可能慢半拍）。"""
    for _ in range(secs * 2):
        if not _pids_matching(pat):
            return
        time.sleep(0.5)
    raise AssertionError(f"残留进程：{_pids_matching(pat)}")


def test_cancel_kills_run_and_spares_other(toy_long):
    r, th = toy_long
    tr = client.Transport("x", "x")
    sha = client.resolve(r, "HEAD")
    tr.call("sync longrepo", stdin=client.make_bundle(r, sha, []))
    kids1 = _start_long_run(tr, th, r, sha, "a1")
    kids2 = _start_long_run(tr, th, r, sha, "a2")
    assert tr.call("status a1")["state"] == "running"

    pat1 = str(th / "runs/a1") + "/"
    # 孤儿确已脱离父链（kids.txt 之外的路径匹配进程），靠命令行路径识别
    assert any(pid not in kids1 for pid in _pids_matching(pat1))

    res = tr.call("cancel a1")
    assert res["success"] and res["state"] == "cancelled" and res["survivors"] == 0
    assert res["killed"] >= 2
    for pid in kids1:
        assert not _alive(pid), f"pid {pid} 还活着"
    _wait_gone(th, pat1)
    # 另一个 run 不受影响
    assert all(_alive(pid) for pid in kids2), "不该按进程名乱杀别的 run"
    assert tr.call("status a2")["state"] == "running"
    # 状态与清理
    st = tr.call("status a1")
    assert st["state"] == "cancelled" and st["finished"] and st["exit"] == 143
    assert (th / "runs/a1/out/cancelled").read_text().strip().isdigit()
    assert tr.call("clean run a1")["success"]

    res = tr.call("cancel a2")     # 收尾：收掉第二个 run
    assert res["success"] and res["killed"] >= 2 and res["survivors"] == 0
    for pid in kids2:
        assert not _alive(pid)
    _wait_gone(th, str(th / "runs/a2") + "/")
    assert tr.call("clean runs")["success"]


def test_cancel_finished_run(toy):
    r, th = toy
    assert client.main(["run", "--repo", f"{r}=toy", "--workdir", "toy", "-n", "1",
                       "--run-id", "fc", "tests/test_a.py"]) == 1
    tr = client.Transport("x", "x")
    res = tr.call("cancel fc")
    assert res["success"] and res["state"] == "finished" and res["killed"] == 0
    assert "已经结束" in res["note"]
    assert not (th / "runs/fc/out/cancelled").exists()


def test_cancel_running_then_result(toy_long, capsys):
    r, th = toy_long
    tr = client.Transport("x", "x")
    sha = client.resolve(r, "HEAD")
    tr.call("sync longrepo", stdin=client.make_bundle(r, sha, []))
    kids = _start_long_run(tr, th, r, sha, "b1")
    assert client.main(["cancel", "b1"]) == 0
    assert '"state": "cancelled"' in capsys.readouterr().out
    assert not any(_alive(p) for p in kids)
    assert client.main(["result", "b1"]) == 130
    assert "已取消" in capsys.readouterr().out


@pytest.mark.parametrize("cmd", ["cancel a/../b", "cancel nope", "cancel 'bad id'"])
def test_cancel_rejects(toy_long, cmd):
    tr = client.Transport("x", "x")
    with pytest.raises(client.THError):
        tr.call(cmd)


def test_ancestor_commit_skips_bundle(toy, capsys, monkeypatch):
    r, th = toy
    (r / "tests/b.txt").write_text("b")
    _git(r, "add", "-A")
    _git(r, "commit", "-qm", "second")
    first = subprocess.run(["git", "-C", str(r), "rev-parse", "HEAD~1"], capture_output=True, text=True).stdout.strip()
    argv = ["run", "--repo", f"{r}=toy", "--workdir", "toy", "-n", "2", "tests/test_a.py"]
    assert client.main(argv + ["--run-id", "a1"]) == 1          # 先同步 tip
    capsys.readouterr()
    called = []
    orig = client.make_bundle
    monkeypatch.setattr(client, "make_bundle", lambda *a: called.append(a) or orig(*a))
    assert client.main(["run", "--repo", f"{r}=toy", "--commit-of", f"toy={first}", "--workdir", "toy", "-n", "2",
                        "--run-id", "a2", "tests/test_a.py"]) == 1
    assert not called and "Mac 上已有" in capsys.readouterr().out
    assert (th / "runs/a2/src/toy/tests/test_a.py").exists()
    assert not (th / "runs/a2/src/toy/tests/b.txt").exists()     # 检出的是祖先版本


# ---------- media 动作（11/04）
# 上传/去重/校验/清理/参数在任何机器上都跑（不解码媒体）；转音频、抽帧要真 ffmpeg，只在 macOS 且测试环境里有 ffmpeg 时跑
# （Studio：client.py run --env-file testhost/testenv.json，该环境 conda 装了 ffmpeg）。Ubuntu 的 CPU 不做媒体处理，本机一律跳过。
FF_BIN = Path(sys.prefix) / "bin"
HAVE_FF = sys.platform == "darwin" and (FF_BIN / "ffmpeg").exists() and (FF_BIN / "ffprobe").exists()
need_ff = pytest.mark.skipif(not HAVE_FF, reason="要真 ffmpeg：只在 Studio（macOS + 测试环境里的 conda ffmpeg）跑")


@pytest.fixture
def mth(tmp_path, monkeypatch):
    th = tmp_path / "th"
    monkeypatch.setenv("TESTHOST_LOCAL", str(th))
    monkeypatch.setenv("TESTHOST_ENV_STUB", sys.prefix)      # media 环境软链到当前环境（Studio 上它有 ffmpeg）
    return th, client.Transport("x", "x")


def _sha(b):
    import hashlib
    return hashlib.sha256(b).hexdigest()


def _code(tr, cmd, stdin=None):
    with pytest.raises(client.THError) as e:
        tr.call(cmd, stdin=stdin)
    return e.value.code


def test_media_put_dedupe_and_verify(mth, tmp_path, capsys):
    th, tr = mth
    f = tmp_path / "会议 录像.bin"
    data = os.urandom(3 << 20) + b"tail"
    f.write_bytes(data)
    h = _sha(data)
    res = client.media_put(tr, f)
    assert res["hash"] == h and res["existed"] is False and res["size"] == len(data)
    stored = th / "media" / h / "会议 录像.bin"
    assert stored.read_bytes() == data
    assert (th / "media" / h / ".last_used").read_text().strip().isdigit()
    capsys.readouterr()
    assert client.main(["media", "put", str(f)]) == 0                      # 第二次：已有，不重传
    assert json.loads(capsys.readouterr().out.strip().splitlines()[-1])["existed"] is True
    # 已有时不读 stdin：给个不对的内容也直接回已有
    assert tr.call(f"media-put {h} other.bin --size 1", stdin=b"x")["existed"] is True
    # 哈希对不上、大小对不上：拒，且不留半成品
    assert _code(tr, f"media-put {'0' * 64} x.bin --size 4", stdin=b"abcd") == "hash_mismatch"
    assert _code(tr, f"media-put {_sha(b'abcd')} x.bin --size 5", stdin=b"abcd") == "size_mismatch"
    left = sorted(p.name for p in (th / "media").iterdir())
    assert left == [h]
    # 不给 --size 也行
    assert tr.call(f"media-put {_sha(b'abcd')} x.bin", stdin=b"abcd")["existed"] is False


@pytest.mark.parametrize("cmd", [
    "media-put abc x.bin", f"media-put {'a' * 64} ../x", f"media-put {'a' * 64} .hidden", f"media-put {'a' * 64} -x",
    f"media-put {'a' * 64} x.bin --size -1", f"media-put {'A' * 64} x.bin", f"media-put {'a' * 64}",
    "media-audio zz", f"media-audio {'a' * 64} --segment-min 0", f"media-audio {'a' * 64} --segment-min x",
    f"media-frames {'a' * 64}", f"media-frames {'a' * 64} --at a,b", f"media-frames {'a' * 64} --at 1 --width 5",
    f"media-frames {'a' * 64} --at -1", "media-clean", "media-clean --older-than x", "media-clean abc"])
def test_media_rejects(mth, cmd):
    _, tr = mth
    h = _sha(b"abcd")
    tr.call(f"media-put {h} x.bin", stdin=b"abcd")
    cmd = cmd.replace("a" * 64, h)        # 参数错要在哈希存在时也报 bad_args
    assert _code(tr, cmd) == "bad_args"


def test_media_not_found_exit_4(mth, tmp_path, capsys):
    _, tr = mth
    h = "f" * 64
    assert _code(tr, f"media-audio {h}") == "not_found"
    assert _code(tr, f"media-frames {h} --at 1") == "not_found"
    assert client.main(["media", "audio", h, "--out", str(tmp_path / "o")]) == 4
    assert client.main(["media", "frames", h, "--at", "1,2", "--out", str(tmp_path / "o")]) == 4
    assert "先 media put" in capsys.readouterr().err


@pytest.mark.skipif(HAVE_FF, reason="测试环境里有 ffmpeg 时由真跑覆盖")
def test_media_without_ffmpeg_reports(mth):
    th, tr = mth
    h = _sha(b"abcd")
    tr.call(f"media-put {h} x.bin", stdin=b"abcd")
    with pytest.raises(client.THError) as e:
        tr.call(f"media-audio {h}")
    assert "ffmpeg" in str(e.value)


def test_media_clean(mth, tmp_path):
    th, tr = mth
    hs = []
    for i in range(3):
        b = b"file%d" % i
        hs.append(_sha(b))
        tr.call(f"media-put {hs[-1]} f{i}.bin", stdin=b)
    old = int(time.time()) - 10 * 86400
    (th / "media" / hs[0] / ".last_used").write_text(str(old))
    (th / "media" / hs[1] / f".busy-{os.getpid()}").write_text("")      # 本进程活着 = 在用
    (th / "media" / hs[1] / ".last_used").write_text(str(old))
    (th / "media" / ".tmp-999999-abc").mkdir()                         # 中断上传留下的（进程已不在）
    res = tr.call("media-clean --older-than 7")
    assert res["deleted"] == [hs[0]] and res["busy"] == [hs[1]] and res["kept"] == 2 and res["freed_bytes"] > 0
    assert not (th / "media" / ".tmp-999999-abc").exists()
    assert _code(tr, f"media-clean {hs[1]}") == "busy"
    (th / "media" / hs[1] / f".busy-{os.getpid()}").unlink()
    (th / "media" / hs[1] / ".busy-999999").write_text("")             # 进程不在了：不算在用
    assert tr.call(f"media-clean {hs[1]}")["deleted"] == [hs[1]]
    assert tr.call(f"media-clean {hs[1]}")["deleted"] == []            # 再删：幂等
    assert client.main(["media", "clean", "--older-than", "0"]) == 0
    assert not [p for p in (th / "media").iterdir()]


def test_media_spec_and_push_tool(mth, capsys):
    th, tr = mth
    assert _code(tr, "media-spec", stdin=b'{"python": "3.12", "conda": ["nodejs=20"]}') is None
    assert not (th / "bin/media.json").exists()                        # 没有 ffmpeg 的描述不收
    assert client.main(["push-tool"]) == 0
    out = capsys.readouterr().out
    assert "v5" in out and "media.json 已更新" in out
    assert (th / "bin/media.json").read_bytes() == client.MEDIA_SPEC.read_bytes()
    assert (th / "bin/testhost").read_bytes() == client.ENTRY.read_bytes()
    with pytest.raises(client.THError):
        tr.call("media-spec", stdin=b"not json")
    assert (th / "bin/media.json").read_bytes() == client.MEDIA_SPEC.read_bytes()   # 坏的不覆盖


def test_media_env_shares_testenv_hash():
    """media.json 与 testenv.json 的 python/pip/conda 等价：Studio 上共用一个环境（只建一次）。"""
    import hashlib

    def key(d):
        pipl = list(d.get("pip", []))
        for x in ("pytest", "pytest-xdist"):
            if not any(p.lower().startswith(x) for p in pipl):
                pipl.append(x)
        cl = d.get("conda", [])
        return hashlib.sha256(json.dumps([str(d.get("python", "3.12")), sorted(pipl)] + ([sorted(cl)] if cl else []))
                              .encode()).hexdigest()[:12]
    here = Path(client.HERE)
    assert key(json.loads((here / "media.json").read_text())) == key(json.loads((here / "testenv.json").read_text()))


TAR_PY = r'''
import io, json, sys, tarfile
mode = sys.argv[1]
t = tarfile.open(fileobj=sys.stdout.buffer, mode="w|")
def add(n, b):
    ti = tarfile.TarInfo(n); ti.size = len(b); t.addfile(ti, io.BytesIO(b))
add("frames/1.jpg", b"\xff\xd8x")
if mode == "evil":
    add("../evil.txt", b"x")
if mode == "midfail":
    sys.stdout.buffer.flush()
    sys.stderr.write(json.dumps({"success": False, "message": "ffmpeg 挂了", "code": "ffmpeg"}) + "\n")
    sys.exit(1)
add("index.json", json.dumps({"frames": [{"t_ms": 1, "file": "frames/1.jpg"}], "out_of_range": [], "failed": []}).encode())
t.close()
'''


@pytest.mark.parametrize("mode", ["ok", "evil", "midfail"])
def test_call_tar_stream(tmp_path, monkeypatch, mode):
    tr = client.Transport("x", "x")
    monkeypatch.setattr(tr, "_argv", lambda cmd: ([sys.executable, "-c", TAR_PY, mode], None))
    out = tmp_path / "o" / "deep"
    if mode == "ok":
        idx = tr.call_tar("media-frames x", out, client.MEDIA_FRAMES_RE)
        assert idx["_received"] == ["frames/1.jpg"] and (out / "frames/1.jpg").read_bytes() == b"\xff\xd8x"
        assert json.loads((out / "index.json").read_text())["frames"][0]["t_ms"] == 1
        assert not list(out.rglob("*.part"))
        return
    with pytest.raises(client.THError) as e:
        tr.call_tar("media-frames x", out, client.MEDIA_FRAMES_RE)
    if mode == "evil":
        assert "不认的成员" in str(e.value) and not (tmp_path / "o" / "evil.txt").exists()
    else:
        assert e.value.code == "ffmpeg" and "ffmpeg 挂了" in str(e.value)
        assert not (out / "index.json").exists()


# ---- 以下要真 ffmpeg（Studio）

def _ff(*args):
    subprocess.run([str(FF_BIN / "ffmpeg"), "-nostdin", "-hide_banner", "-loglevel", "error", "-y", *args], check=True)


def _jpeg_wh(b):
    i = 2
    while i + 9 < len(b) and b[i] == 0xFF:
        m, ln = b[i + 1], int.from_bytes(b[i + 2:i + 4], "big")
        if m in (0xC0, 0xC1, 0xC2):
            return int.from_bytes(b[i + 7:i + 9], "big"), int.from_bytes(b[i + 5:i + 7], "big")
        i += 2 + ln
    return None


@need_ff
def test_media_audio_frames_real(mth, tmp_path):
    import wave
    th, tr = mth
    v = tmp_path / "测试 录像.mp4"          # 带空格与中文的文件名
    _ff("-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000",
        "-t", "2", "-map", "0:v", "-map", "1:a", "-ac", "2", "-c:v", "mpeg4", "-c:a", "aac", str(v))
    h = client.media_put(tr, v)["hash"]
    idx = client.media_audio(tr, h, tmp_path / "a", segment_min=0.01)   # 0.6 秒一段
    segs = idx["segments"]
    assert [s["file"] for s in segs] == ["audio/0001.wav", "audio/0002.wav", "audio/0003.wav", "audio/0004.wav"]
    assert [s["offset_ms"] for s in segs] == [0, 600, 1200, 1800]
    assert [s["duration_ms"] for s in segs[:3]] == [600, 600, 600] and 100 <= segs[3]["duration_ms"] <= 300
    assert 1900 <= idx["duration_ms"] <= 2100 and idx["sample_rate"] == 16000
    for s in segs:
        with wave.open(str(tmp_path / "a" / s["file"])) as w:
            assert (w.getnchannels(), w.getframerate(), w.getsampwidth(), w.getnframes()) == (1, 16000, 2, s["samples"])
    assert json.loads((tmp_path / "a/index.json").read_text())["segments"] == segs
    fx = client.media_frames(tr, h, [500, 1500, 5000], tmp_path / "f", width=160)
    assert [x["t_ms"] for x in fx["frames"]] == [500, 1500] and fx["out_of_range"] == [5000] and not fx["failed"]
    for x in fx["frames"]:
        b = (tmp_path / "f" / x["file"]).read_bytes()
        assert b[:2] == b"\xff\xd8" and _jpeg_wh(b) == (160, 120) == (x["width"], x["height"])
    fx = client.media_frames(tr, h, [0], tmp_path / "f2")                 # 比原片宽的上限不放大
    assert (fx["frames"][0]["width"], fx["frames"][0]["height"]) == (320, 240)
    # 用完不留中间产物、不留在用标记
    assert sorted(p.name for p in (th / "media" / h).iterdir()) == [".last_used", "测试 录像.mp4"]
    assert not [p for p in (th / "media").iterdir() if p.name.startswith(".")]
    assert tr.call(f"media-clean {h}")["deleted"] == [h]


@need_ff
def test_media_no_audio_no_video(mth, tmp_path):
    th, tr = mth
    a, v = tmp_path / "a.wav", tmp_path / "v.mp4"
    _ff("-f", "lavfi", "-i", "sine=frequency=500:sample_rate=22050", "-t", "1.5", str(a))
    _ff("-f", "lavfi", "-i", "testsrc=size=160x120:rate=5", "-t", "1", "-c:v", "mpeg4", str(v))
    ha, hv = client.media_put(tr, a)["hash"], client.media_put(tr, v)["hash"]
    idx = client.media_audio(tr, ha, tmp_path / "o")                     # 缺省 10 分钟一段：一段
    assert len(idx["segments"]) == 1 and 1450 <= idx["duration_ms"] <= 1550
    assert _code(tr, f"media-frames {ha} --at 0") == "no_video"
    assert _code(tr, f"media-audio {hv}") == "no_audio"
