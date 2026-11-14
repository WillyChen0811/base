"""Task context is available without a .git checkout and cannot be spoofed."""
import json
import subprocess

from test_local import client, toy


def test_task_context(toy):
    repo, home = toy
    desc = {"python": "3.12", "pip": [], "env": {
        "TESTHOST_OUT_DIR": "/not-the-output",
        "TESTHOST_REPO_COMMIT": "spoof"}, "tasks": {"context": {
            "cmd": ["python", "context.py"], "outputs": ["out/context-result.json"]}}}
    (repo / "testhost.json").write_text(json.dumps(desc))
    (repo / "context.py").write_text(
        "import json,os,pathlib\n"
        "p=pathlib.Path(os.environ['TESTHOST_CONTEXT_FILE'])\n"
        "d=json.loads(p.read_text())\n"
        "assert os.environ['TESTHOST_REPO_COMMIT']==d['repos']['toy']\n"
        "out=pathlib.Path(os.environ['TESTHOST_OUT_DIR'])\n"
        "assert out==p.parent/'out'\n"
        "(out/'context-result.json').write_text(json.dumps(d))\n")
    for args in (["add", "-A"], ["-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "context"]):
        subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True)
    commit = subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True).strip()
    assert client.main(["run", "--repo", f"{repo}=toy", "--workdir", "toy",
                        "--task", "context", "--run-id", "ctx"]) == 0
    run = home / "runs/ctx"
    got = json.loads((run / "out/context-result.json").read_text())
    assert got['version'] == 1
    assert got['repos'] == {'toy': commit}
    assert got['primary_repo'] == got['workdir'] == 'toy'
    assert got['env_file'] == 'testhost.json'
    assert got['env_hash'] == (run / 'envhash').read_text().strip()
    assert got['task'] == 'context'
    assert got['platform']['system'] and got['platform']['arch']
    assert not (run / 'src/toy/.git').exists()


def test_legacy_prepared_context_optional(toy):
    repo, home = toy
    # Run a successful task to create a run, then simulate the metadata of an old prepare.
    desc = {"python": "3.12", "pip": [], "tasks": {"old": {
        "cmd": ["python", "-c", "print('legacy task')"]}}}
    (repo / 'testhost.json').write_text(json.dumps(desc))
    subprocess.run(['git', '-C', str(repo), 'add', '-A'], check=True)
    subprocess.run(['git', '-C', str(repo), '-c', 'user.name=t', '-c', 'user.email=t@t',
                    'commit', '-qm', 'legacy'], check=True, capture_output=True)
    assert client.main(['run', '--repo', f'{repo}=toy', '--workdir', 'toy',
                        '--task', 'old', '--run-id', 'oldctx']) == 0
    run = home / 'runs/oldctx'
    (run / 'context.json').unlink()
    (run / 'context.version').unlink()
    tr = client.Transport('x', 'x')
    tr.call('run oldctx toy --task old')
    status = client.wait(tr, 'oldctx', 30)
    assert status['exit'] == 0
    # New prepare metadata missing its context must fail rather than fabricate provenance.
    (run / 'context.version').write_text('1\n')
    import pytest
    with pytest.raises(client.THError, match='上下文'):
        tr.call('run oldctx toy --task old')
