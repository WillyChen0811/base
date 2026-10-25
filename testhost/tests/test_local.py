"""本机自测：假 TH 目录 + 已有环境作替身，走完 sync→snapshot→prepare→run→fetch→clean。"""
import json
import os
import subprocess
import sys
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
