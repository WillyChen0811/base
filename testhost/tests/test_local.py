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
