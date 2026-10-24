#!/usr/bin/env python3
"""测试机客户端（Ubuntu 端）：把指定仓库某个 commit 的测试派到 Mac 测试机跑，拿回结果。
只用标准库，调 ssh/git 子进程。用法见 README.md。"""
import argparse
import ast
import hashlib
import json
import re
import os
import shlex
import subprocess
import sys
import tempfile
import tarfile
import io
import time
import urllib.request
import xml.etree.ElementTree as ET
from datetime import datetime
from pathlib import Path

HERE = Path(__file__).resolve().parent
ENTRY = HERE / "mac_testhost.sh"
GATEWAY = os.environ.get("TESTHOST_GATEWAY", "http://127.0.0.1:18192/gw/status")


class THError(Exception):
    pass


class Transport:
    """经 ssh 调测试机入口；TESTHOST_LOCAL=<假 TH 目录> 时直接本机执行入口（自测用）。"""

    def __init__(self, host, key):
        self.host, self.key = host, key
        self.local = os.environ.get("TESTHOST_LOCAL")

    def call(self, cmd, stdin=None, raw=False, timeout=None):
        if self.local:
            env = dict(os.environ, SSH_ORIGINAL_COMMAND=cmd, TESTHOST_HOME=self.local)
            argv = ["bash", str(ENTRY)]
        else:
            ctl = Path.home() / ".cache" / "xxyi-testhost"
            ctl.mkdir(parents=True, exist_ok=True)
            env = None
            argv = ["ssh", "-i", self.key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
                    "-o", "ConnectTimeout=30", "-o", "ServerAliveInterval=30",
                    "-o", "ControlMaster=auto", "-o", "ControlPersist=120",
                    "-o", f"ControlPath={ctl}/cm-%C", self.host, cmd]
        p = subprocess.run(argv, input=stdin, capture_output=True, env=env, timeout=timeout)
        if raw:
            if p.returncode:
                raise THError(f"{cmd.split()[0]} 失败：{p.stderr.decode(errors='replace')[-300:]}")
            return p.stdout
        out = p.stdout.decode(errors="replace").strip().splitlines()
        try:
            res = json.loads(out[-1])
        except (IndexError, ValueError):
            raise THError(f"{cmd.split()[0]} 无有效返回（rc={p.returncode}）：{p.stderr.decode(errors='replace')[-300:]}")
        if not res.get("success", False):
            raise THError(f"{cmd.split()[0]}：{res.get('message')}")
        return res


def git(repo, *args, stdin=None):
    p = subprocess.run(["git", "-C", str(repo), *args], input=stdin, capture_output=True)
    if p.returncode:
        raise THError(f"git {' '.join(args[:2])} 失败：{p.stderr.decode(errors='replace')[-200:]}")
    return p.stdout


def resolve(repo, ref):
    return git(repo, "rev-parse", "--verify", f"{ref}^{{commit}}").decode().strip()


def make_bundle(repo, sha, have):
    """增量 bundle：排除 Mac 上已有且本地也有的 commit。要给 bundle 一个 ref，临时建一条再删。"""
    ref = f"refs/testhost/{sha}"
    # worktree 的 gitdir 没有对象库，要取共用仓库（--git-common-dir，可能是相对 repo 的路径）
    gd = git(repo, "rev-parse", "--git-common-dir").decode().strip()
    gd = str((Path(repo) / gd).resolve())
    with tempfile.TemporaryDirectory() as tmp:     # 临时仓库借对象库，不往源仓库写任何 ref
        t = Path(tmp) / "t.git"
        git(Path(tmp), "init", "-q", "--bare", str(t))
        (t / "objects/info/alternates").write_text(gd + "/objects\n")
        git(t, "update-ref", ref, sha)
        excl = []
        for h in have:
            if subprocess.run(["git", "-C", str(repo), "cat-file", "-e", f"{h}^{{commit}}"],
                              capture_output=True).returncode == 0:
                excl.append(f"^{h}")
        return git(t, "bundle", "create", "-", ref, *excl)


def eval_names(src, names):
    """从 Python 源码取模块级常量元组（支持 Name 引用与 *展开），不 import 源文件。"""
    env = {}

    def ev(n):
        if isinstance(n, ast.Constant):
            return n.value
        if isinstance(n, ast.Name):
            return env[n.id]
        if isinstance(n, (ast.Tuple, ast.List)):
            out = []
            for e in n.elts:
                out.extend(ev(e.value) if isinstance(e, ast.Starred) else [ev(e)])
            return tuple(out)
        raise ValueError
    for st in ast.parse(src).body:
        if isinstance(st, ast.Assign) and len(st.targets) == 1 and isinstance(st.targets[0], ast.Name):
            try:
                env[st.targets[0].id] = ev(st.value)
            except (ValueError, KeyError):
                pass
    out = []
    for nm in names:
        if nm not in env:
            raise THError(f"{nm} 不是可解析的常量")
        out.extend(env[nm])
    return out


def snapshot_paths(spec):
    items = Path(spec[1:]).read_text().split() if spec.startswith("@") else spec.split(",")
    return [x for x in items if x]


def gateway_busy():
    try:
        with urllib.request.urlopen(GATEWAY, timeout=2) as r:
            return bool(json.load(r).get("running"))
    except Exception:
        return False


def log(msg):
    print(f"[{datetime.now():%H:%M:%S}] {msg}", flush=True)


def parse_repo(spec):
    path, _, name = spec.partition("=")
    path = Path(path).expanduser().resolve()
    return (name or path.name.removesuffix(".git")), path


def plan(args):
    """算出：仓库 {名: (路径, 完整 commit)}、环境描述、快照 [(名, 仓库路径, commit, 路径列表)]。"""
    if not args.repo:
        raise THError("至少要一个 --repo")
    refs = dict(x.split("=", 1) for x in args.commit_of)
    repos = {}
    for spec in args.repo:
        n, p = parse_repo(spec)
        repos[n] = (p, resolve(p, refs.get(n, "HEAD")))
    first = next(iter(repos))
    fp, fc = repos[first]
    try:
        desc = json.loads(git(fp, "show", f"{fc}:{args.env_file}"))
    except ValueError:
        raise THError(f"{args.env_file} 不是合法 JSON")
    for n, d in desc.get("repos", {}).items():
        if n not in repos:
            p = (fp / d["path"]).resolve() if "path" in d else fp.parent / n
            repos[n] = (p, resolve(p, refs.get(n, d.get("ref", "HEAD"))))
    snaps, repos_src = [], {}
    for spec in args.snapshot:
        n, _, rest = spec.partition("=")
        repo_commit, _, pathspec = rest.partition(":")
        rp, _, c = repo_commit.rpartition("@")
        snaps.append((n, Path(rp).expanduser().resolve(), c, snapshot_paths(pathspec)))
    if not args.snapshot:
        for n, d in desc.get("snapshots", {}).items():
            rp = Path(os.environ.get(d.get("repo_env", ""), "")) if os.environ.get(d.get("repo_env", "")) \
                else (fp / d["repo_path"]).resolve()
            pf = d["paths_from"]
            src = git(fp, "show", f"{fc}:{pf['file']}").decode()
            snaps.append((n, rp, d["commit"], list(eval_names(src, pf["names"]))))
            if d.get("repo_env"):     # 测试里要 git show/archive 该仓库：整库同步成 <键>-src
                repos_src[f"{n}-src"] = (rp, resolve(rp, d["commit"]))
    return repos, desc, snaps, repos_src


def push_repos(tr, repos):
    inv = tr.call("inventory")["repos"]
    for n, (p, sha) in repos.items():
        have = inv.get(n, [])
        if sha in have:
            log(f"{n}@{sha[:8]} Mac 上已有")
            continue
        data = make_bundle(p, sha, have)
        log(f"同步 {n}@{sha[:8]}（{'增量' if have else '全量'} {len(data) // 1024} KB）")
        tr.call(f"sync {n}", stdin=data)


def push_snapshots(tr, snaps):
    inv = tr.call("inventory")["snapshots"]
    out = {}
    for n, rp, c, paths in snaps:
        full = resolve(rp, c)
        name = f"{n}-{full[:10]}-{hashlib.sha1(chr(10).join(sorted(paths)).encode()).hexdigest()[:6]}"
        out[n] = name
        if name in inv:
            log(f"快照 {name} 已有")
            continue
        tar = git(rp, "archive", "--format=tar", full, "--", *paths)
        log(f"发送快照 {name}（{len(tar) // 1024} KB）")
        tr.call(f"snapshot {name}", stdin=tar)
    return out


def summarize(tr, rid):
    data = tr.call(f"fetch {rid}", raw=True)
    tf = tarfile.open(fileobj=io.BytesIO(data))
    get = lambda n: tf.extractfile(n).read() if n in tf.getnames() else None
    ex = (get("exit") or b"").decode().strip()
    code = int(ex) if ex else 3
    xml = get("junit.xml")
    if not xml:
        print("没有 junit 结果（收集阶段就挂了？），日志尾部：")
        print((get("log.tail") or b"").decode(errors="replace")[-3000:])
        return code or 2
    root = ET.fromstring(xml)
    suites = [root] if root.tag == "testsuite" else list(root)
    tot = {k: sum(int(float(s.get(k, 0))) for s in suites) for k in ("tests", "failures", "errors", "skipped")}
    secs = sum(float(s.get("time", 0)) for s in suites)
    wall = 0
    try:
        wall = int(get("exit.end")) - int(get("start"))
    except (TypeError, ValueError):
        pass
    passed = tot["tests"] - tot["failures"] - tot["errors"] - tot["skipped"]
    print(f"\n{rid}: {passed} 通过 / {tot['failures'] + tot['errors']} 失败 / {tot['skipped']} 跳过，"
          f"墙钟 {wall}s（累计用例时间 {secs:.0f}s），pytest 退出码 {code}")
    for tc in root.iter("testcase"):
        bad = tc.find("failure") if tc.find("failure") is not None else tc.find("error")
        if bad is not None:
            msg = (bad.get("message") or bad.text or "").strip().splitlines()
            print(f"  FAIL {tc.get('classname')}::{tc.get('name')}\n       {msg[0][:200] if msg else ''}")
    return code


def fetch_extras(tr, rid, out_dir):
    """把外挂等在 run 目录里写出的文件取回 out_dir（只取普通文件，名字取末段，不信 tar 里的路径）。"""
    data = tr.call(f"fetchx {rid}", raw=True)
    out = Path(out_dir).expanduser()
    out.mkdir(parents=True, exist_ok=True)
    got = []
    with tarfile.open(fileobj=io.BytesIO(data)) as tf:
        for m in tf.getmembers():
            name = Path(m.name).name
            if not m.isfile() or not name or name.startswith("."):
                continue
            (out / name).write_bytes(tf.extractfile(m).read())
            got.append(name)
    print(f"取回 {len(got)} 个外挂产出文件到 {out}：{', '.join(got) if got else '（无）'}")
    return got


def wait(tr, rid, timeout, interval=15):
    t0, bad = time.time(), 0
    while True:
        try:
            st = tr.call(f"status {rid}")
            bad = 0
        except THError as e:
            bad += 1
            if bad > 8:
                raise
            log(f"status 失败（{bad}）：{e}")
            time.sleep(interval)
            continue
        log(f"{st['state']}：{st['passed']} 通过 {st['failed']} 失败 {st['skipped']} 跳过 {st['percent']}%")
        if st["finished"]:
            return st
        if time.time() - t0 > timeout:
            raise THError(f"超时 {timeout}s，run 仍在跑：python client.py result {rid}")
        time.sleep(interval)


def cmd_run(tr, a):
    repos, desc, snaps, repos_src = plan(a)
    ping = tr.call("ping")
    n = a.n
    plugs = [Path(x).expanduser() for x in a.plugin]
    helpers = [Path(x).expanduser() for x in a.plugin_file]
    for f in plugs + helpers:
        if not f.is_file() or not re.fullmatch(r"[A-Za-z0-9_]+\.py", f.name):
            raise THError(f"外挂文件不合法（要存在，文件名只许 [A-Za-z0-9_] 加 .py）：{f}")
    for kv in a.plugin_env:
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*=.*", kv, re.S):
            raise THError(f"--plugin-env 要 KEY=VALUE：{kv}")
    if n is None and plugs:
        n = 1      # 外挂（如 caseplug）按单进程设计，没指定 -n 就单进程
    if n is None:
        n = min(int(ping["ncpu"]), 16)
        if gateway_busy():
            n = max(2, n // 2)
            log(f"网关显示大模型在跑，并发减半为 {n}")
    log(f"测试机 {ping['host']}：{ping['ncpu']} 核，在跑 {ping['running']} 个 run，-n {n}")
    push_repos(tr, {**repos, **repos_src})
    snapmap = push_snapshots(tr, snaps)
    first = next(iter(repos))
    rid = a.run_id or f"r{datetime.now():%m%d%H%M%S}-{repos[first][1][:7]}"
    pc = ["prepare", rid, *[f"{k}={v[1]}" for k, v in repos.items()], "--env", a.env_file]
    for k, v in snapmap.items():
        pc += ["--snap", f"{k}={v}"]
    log("prepare（首次建环境较慢）")
    pr = tr.call(" ".join(shlex.quote(x) for x in pc))
    log(f"环境 {pr['env']}（{'复用' if pr['env_reused'] else '新建'}），用时 {pr['seconds']}s")
    for f in plugs + helpers:
        tr.call(f"plugin {rid} {f.name}", stdin=f.read_bytes())
        log(f"外挂 {f.name} 已上传")
    rc = ["run", rid, a.workdir, "-n", str(n)]
    for f in plugs:
        rc += ["--plugin", f.stem]
    for kv in a.plugin_env:
        rc += ["--plugin-env", kv]
    for flag, v in (("-k", a.k), ("-m", a.m)):
        if v:
            rc += [flag, v]
    tr.call(" ".join(shlex.quote(x) for x in rc + a.tests))
    log(f"已开跑 {rid}（Ctrl-C 不会停远端，之后 python client.py result {rid}）")
    wait(tr, rid, a.timeout)
    code = summarize(tr, rid)
    if a.out_dir:
        fetch_extras(tr, rid, a.out_dir)
    return code


def main(argv=None):
    ap = argparse.ArgumentParser(description="Mac 测试机客户端")
    ap.add_argument("--host", default=os.environ.get("TESTHOST_SSH", "omlxwake@192.168.147.196"))
    ap.add_argument("--key", default=os.environ.get("TESTHOST_KEY", str(Path.home() / ".ssh/xxyi_testhost")))
    sp = ap.add_subparsers(dest="cmd", required=True)
    sp.add_parser("ping")
    sp.add_parser("inventory")
    sp.add_parser("push-tool")
    for nm in ("status", "result"):
        s = sp.add_parser(nm)
        s.add_argument("run_id")
        s.add_argument("--timeout", type=int, default=3600)
        s.add_argument("--out-dir", help="把 run 目录里外挂写出的文件取回到这里")
    f = sp.add_parser("fetch", help="只取回外挂写出的文件（run 须已结束）")
    f.add_argument("run_id")
    f.add_argument("--out-dir", required=True)
    c = sp.add_parser("clean")
    c.add_argument("rest", nargs=argparse.REMAINDER)
    r = sp.add_parser("run")
    r.add_argument("--repo", action="append", default=[], help="本地路径[=名]，第一个带环境描述")
    r.add_argument("--commit-of", action="append", default=[], metavar="名=commit或分支")
    r.add_argument("--snapshot", action="append", default=[], metavar="名=仓库@commit:路径,路径|@清单文件")
    r.add_argument("--workdir", required=True, help="Mac 上 src/ 下的工作子目录，如 xxyi")
    r.add_argument("--env-file", default="cmdproto/testenv.json")
    r.add_argument("--run-id")
    r.add_argument("-n", type=int)
    r.add_argument("-k")
    r.add_argument("-m")
    r.add_argument("--timeout", type=int, default=3600)
    r.add_argument("--plugin", action="append", default=[], metavar="本地.py",
                   help="pytest 外挂，随 run 上传并 -p 加载（可多次；没给 -n 时缺省 -n 1）")
    r.add_argument("--plugin-file", action="append", default=[], metavar="本地.py",
                   help="外挂的配套文件，只上传到同一目录，不加载")
    r.add_argument("--plugin-env", action="append", default=[], metavar="KEY=VALUE",
                   help="外挂用的环境变量，值里 {run} 展开为 runs/<id>")
    r.add_argument("--out-dir", help="结束后把外挂写在 run 目录里的文件取回到这里")
    r.add_argument("tests", nargs="+")
    a = ap.parse_args(argv)
    tr = Transport(a.host, a.key)
    try:
        if a.cmd == "ping":
            print(json.dumps(tr.call("ping"), ensure_ascii=False, indent=1))
        elif a.cmd == "inventory":
            print(json.dumps(tr.call("inventory"), ensure_ascii=False, indent=1))
        elif a.cmd == "push-tool":
            print(tr.call("update", stdin=ENTRY.read_bytes())["message"])
            print(tr.call("ping")["testhost"])
        elif a.cmd == "status":
            print(json.dumps(tr.call(f"status {a.run_id}")))
        elif a.cmd == "result":
            wait(tr, a.run_id, a.timeout)
            code = summarize(tr, a.run_id)
            if a.out_dir:
                fetch_extras(tr, a.run_id, a.out_dir)
            return code
        elif a.cmd == "fetch":
            fetch_extras(tr, a.run_id, a.out_dir)
        elif a.cmd == "clean":
            print(json.dumps(tr.call("clean " + " ".join(shlex.quote(x) for x in a.rest))))
        else:
            return cmd_run(tr, a)
    except (THError, subprocess.TimeoutExpired) as e:
        print(f"错误：{e}", file=sys.stderr)
        return 3
    return 0


if __name__ == "__main__":
    sys.exit(main())
