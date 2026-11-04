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
import shutil
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
MEDIA_SPEC = HERE / "media.json"
GATEWAY = os.environ.get("TESTHOST_GATEWAY", "http://127.0.0.1:18192/gw/status")


class THError(Exception):
    """code：Mac 端回的错误类别（not_found、hash_mismatch、bad_args……），没有为 None。"""

    def __init__(self, msg, code=None):
        super().__init__(msg)
        self.code = code


class Transport:
    """经 ssh 调测试机入口；TESTHOST_LOCAL=<假 TH 目录> 时直接本机执行入口（自测用）。"""

    def __init__(self, host, key):
        self.host, self.key = host, key
        self.local = os.environ.get("TESTHOST_LOCAL")

    def _argv(self, cmd):
        if self.local:
            return ["bash", str(ENTRY)], dict(os.environ, SSH_ORIGINAL_COMMAND=cmd, TESTHOST_HOME=self.local)
        ctl = Path.home() / ".cache" / "xxyi-testhost"
        ctl.mkdir(parents=True, exist_ok=True)
        return ["ssh", "-i", self.key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
                "-o", "ConnectTimeout=30", "-o", "ServerAliveInterval=30",
                "-o", "ControlMaster=auto", "-o", "ControlPersist=120",
                "-o", f"ControlPath={ctl}/cm-%C", self.host, cmd], None

    def call(self, cmd, stdin=None, raw=False, timeout=None):
        """stdin：bytes 整块送；打开的文件对象则把文件描述符直接交给 ssh 读（流式，不进内存）。"""
        argv, env = self._argv(cmd)
        feed = {"input": stdin} if stdin is None or isinstance(stdin, (bytes, bytearray)) else {"stdin": stdin}
        p = subprocess.run(argv, capture_output=True, env=env, timeout=timeout, **feed)
        if raw:
            if p.returncode:
                raise THError(f"{cmd.split()[0]} 失败：{p.stderr.decode(errors='replace')[-300:]}")
            return p.stdout
        return _parse_result(cmd, p.stdout, p.stderr, p.returncode)

    def call_tar(self, cmd, out_dir, member_re):
        """调输出 tar 的动作（media-audio/media-frames），边收边落盘到 out_dir，返回 index.json 的内容。
        成员名必须整串匹配 member_re（再加 index.json），别的成员、链接、绝对路径一律拒；每个文件先写 .part 再改名。
        Mac 端还没开始写 tar 就出错时 stdout 是一行 JSON（以 { 开头）；写到一半出错时 JSON 在 stderr、退出码非 0。"""
        argv, env = self._argv(cmd)
        out = Path(out_dir).expanduser()
        out.mkdir(parents=True, exist_ok=True)
        pat = re.compile(member_re)
        got, idx, broken = [], None, None
        with tempfile.TemporaryFile() as err:
            p = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=err, env=env)
            try:
                head = p.stdout.read(1)
                if head in (b"", b"{"):
                    rest = p.stdout.read()
                    p.wait()
                    err.seek(0)
                    _parse_result(cmd, head + rest, err.read(), p.returncode)
                    raise THError(f"{cmd.split()[0]}：没有输出")
                try:
                    with tarfile.open(fileobj=_Prefixed(head, p.stdout), mode="r|") as tf:
                        for m in tf:
                            if m.name == "index.json" and m.isfile():
                                idx = json.loads(tf.extractfile(m).read())
                                continue
                            if not (m.isfile() and pat.fullmatch(m.name)):
                                raise THError(f"{cmd.split()[0]}：tar 里有不认的成员 {m.name!r}")
                            dst = out / m.name
                            dst.parent.mkdir(parents=True, exist_ok=True)
                            part = dst.with_name(dst.name + ".part")
                            with open(part, "wb") as w:
                                shutil.copyfileobj(tf.extractfile(m), w, 1 << 20)
                            os.replace(part, dst)
                            got.append(m.name)
                except tarfile.TarError as e:
                    idx, broken = None, e           # 多半是 Mac 端写到一半出错，下面先看它的 stderr
                p.stdout.read()                     # 收掉 tar 结尾的补齐块，免得对端写不完
                rc = p.wait()
            finally:
                if p.poll() is None:
                    p.kill()
                    p.wait()
            if rc or idx is None:
                err.seek(0)
                e = err.read().decode(errors="replace").strip().splitlines()
                try:
                    j = json.loads(e[-1])
                    raise THError(f"{cmd.split()[0]}：{j.get('message')}", j.get("code"))
                except (IndexError, ValueError):
                    why = chr(10).join(e)[-300:] or (f"tar 流坏了：{broken}" if broken else "没有 index.json")
                    raise THError(f"{cmd.split()[0]} 失败（rc={rc}）：{why}")
        (out / "index.json").write_text(json.dumps(idx, ensure_ascii=False, indent=1))
        idx["_received"] = got
        return idx


class _Prefixed:
    """把已经读出的开头几个字节接回管道前面，给 tarfile 的流模式读。"""

    def __init__(self, head, f):
        self.head, self.f = head, f

    def read(self, n=-1):
        if not self.head:
            return self.f.read(n)
        if n is None or n < 0:
            b, self.head = self.head + self.f.read(), b""
            return b
        b, self.head = self.head[:n], self.head[n:]
        return b + (self.f.read(n - len(b)) if len(b) < n else b"")


def _parse_result(cmd, stdout, stderr, rc):
    out = stdout.decode(errors="replace").strip().splitlines()
    try:
        res = json.loads(out[-1])
    except (IndexError, ValueError):
        raise THError(f"{cmd.split()[0]} 无有效返回（rc={rc}）：{stderr.decode(errors='replace')[-300:]}")
    if not res.get("success", False):
        raise THError(f"{cmd.split()[0]}：{res.get('message')}", res.get("code"))
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


def is_ancestor(repo, sha, tip):
    """sha 是 tip 的祖先（tip 本地没有时视为否）。"""
    return subprocess.run(["git", "-C", str(repo), "merge-base", "--is-ancestor", sha, tip],
                          capture_output=True).returncode == 0


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


ENV_CANDIDATES = ("testhost.json", "cmdproto/testenv.json")    # 缺省：先通用名，再兼容 xxyi 旧名


def find_env_file(repo, commit, given):
    """环境描述在被测仓库该 commit 里的相对路径：显式给的优先，否则按候选顺序找第一个存在的。"""
    for c in ([given] if given else ENV_CANDIDATES):
        if subprocess.run(["git", "-C", str(repo), "cat-file", "-e", f"{commit}:{c}"], capture_output=True).returncode == 0:
            return c
    raise THError(f"{given or ' 或 '.join(ENV_CANDIDATES)} 在被测仓库 {commit[:8]} 里不存在")


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
    args.env_file = find_env_file(fp, fc, args.env_file)
    try:
        desc = json.loads(git(fp, "show", f"{fc}:{args.env_file}"))
    except ValueError:
        raise THError(f"{args.env_file} 不是合法 JSON")
    if args.task and args.task not in desc.get("tasks", {}):
        raise THError(f"{args.env_file} 里没有任务 {args.task}（有：{', '.join(desc.get('tasks', {})) or '无'}）")
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
        if sha in have or any(is_ancestor(p, sha, h) for h in have):
            # 祖先 commit：对象已随某个已同步 tip 在 Mac 裸仓库里，prepare 按 sha 直接 archive，不需要 ref
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


def summarize(tr, rid, task=None):
    data = tr.call(f"fetch {rid}", raw=True)
    tf = tarfile.open(fileobj=io.BytesIO(data))
    get = lambda n: tf.extractfile(n).read() if n in tf.getnames() else None
    ex = (get("exit") or b"").decode().strip()
    code = int(ex) if ex else 3
    if task:
        try:
            wall = int(get("exit.end")) - int(get("start"))
        except (TypeError, ValueError):
            wall = 0
        print("日志尾部：")
        print("\n".join((get("log.tail") or b"").decode(errors="replace").splitlines()[-40:]))
        print(f"\n{rid}: 任务 {task} 退出码 {code}，墙钟 {wall}s")
        return code
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


def fetch_extras(tr, rid, out_dir, tree=None):
    """把外挂/任务在 run 目录里写出的文件取回 out_dir（只取普通文件，不信 tar 里的路径）。
    外挂产出只取名字末段；任务（tree）保留相对路径（去掉开头的 out/），任何 .. 或绝对路径的成员丢弃。"""
    if tree is None:
        tree = bool(tr.call(f"status {rid}").get("task"))
    data = tr.call(f"fetchx {rid}", raw=True)
    out = Path(out_dir).expanduser()
    out.mkdir(parents=True, exist_ok=True)
    got = []
    with tarfile.open(fileobj=io.BytesIO(data)) as tf:
        for m in tf.getmembers():
            name = Path(m.name).name
            if tree:
                parts = Path(m.name).parts
                parts = parts[1:] if parts[:1] == ("out",) else parts
                if not parts or Path(m.name).is_absolute() or ".." in parts:
                    continue
                name = "/".join(parts)
            if not m.isfile() or not name or name.startswith("."):
                continue
            dst = out / name
            dst.parent.mkdir(parents=True, exist_ok=True)
            dst.write_bytes(tf.extractfile(m).read())
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
        if st["state"] == "cancelled":   # 被别处 cancel：正常收尾，不再轮询
            log(f"{rid} 已被取消")
            return st
        if st.get("task"):
            last = (st.get("log_tail") or "").split("\n")[-1][:100]
            log(f"{st['state']}（任务 {st['task']}）：{last}")
        else:
            log(f"{st['state']}：{st['passed']} 通过 {st['failed']} 失败 {st['skipped']} 跳过 {st['percent']}%")
        if st["finished"]:
            return st
        if time.time() - t0 > timeout:
            raise THError(f"超时 {timeout}s，run 仍在跑：python client.py result {rid}")
        time.sleep(interval)


# ---------- media：Studio 上的 ffmpeg（Ubuntu 的 CPU 不做媒体处理，抽音频、抽帧交 Studio）

MEDIA_AUDIO_RE = r"audio/[0-9]{4,6}\.wav"
MEDIA_FRAMES_RE = r"frames/[0-9]{1,9}\.jpg"


def file_sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for b in iter(lambda: f.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()


def good_media_name(n):
    """与 Mac 端同一规则：不能有 / \\ 和控制字符，不能以 . 或 - 开头，UTF-8 不超过 200 字节。"""
    try:
        b = n.encode("utf-8")
    except UnicodeEncodeError:
        return False
    return (0 < len(b) <= 200 and not n.startswith((".", "-"))
            and "/" not in n and "\\" not in n and not any(ord(c) < 32 or ord(c) == 127 for c in n))


def media_put(tr, path, name=None):
    """上传本地文件（流式），返回 Mac 端结果（hash、name、size、existed、seconds、free_gb）。已有同哈希的不重传。"""
    path = Path(path).expanduser()
    if not path.is_file():
        raise THError(f"不是文件：{path}")
    name = name or path.name
    if not good_media_name(name):
        raise THError(f"文件名不合法：{name!r}（用 --name 另起一个）")
    size = path.stat().st_size
    t0 = time.time()
    h = file_sha256(path)
    log(f"sha256 {h}（{size / 1e6:.1f} MB，算哈希 {time.time() - t0:.1f}s）")
    t1 = time.time()
    with open(path, "rb") as f:
        res = tr.call(f"media-put {h} {shlex.quote(name)} --size {size}", stdin=f)
    res["wall"] = round(time.time() - t1, 2)
    log(f"{'测试机上已有，没重传' if res['existed'] else '已上传'}：{res['name']}，{res['wall']}s，Studio 剩 {res['free_gb']} GiB")
    return res


def media_audio(tr, h, out_dir, segment_min=10):
    """转 16 kHz 单声道 16 位 WAV、按 segment_min 分钟切段，落到 out_dir/audio/NNNN.wav + out_dir/index.json。"""
    seg = f"{float(segment_min):.3f}".rstrip("0").rstrip(".")
    t0 = time.time()
    idx = tr.call_tar(f"media-audio {h} --segment-min {seg}", out_dir, MEDIA_AUDIO_RE)
    for s in idx["segments"]:
        if s["file"] not in idx["_received"]:
            raise THError(f"media-audio：index 里的 {s['file']} 没收到")
    idx["wall"] = round(time.time() - t0, 2)
    log(f"音频 {len(idx['segments'])} 段，共 {idx['duration_ms'] / 1000:.1f}s，{idx['wall']}s → {out_dir}")
    return idx


def media_frames(tr, h, at_ms, out_dir, width=960):
    """按毫秒时间点各截一帧（宽不超过 width），落到 out_dir/frames/<t_ms>.jpg + out_dir/index.json。"""
    ats = [int(x) for x in at_ms]
    if not ats or any(x < 0 for x in ats):
        raise THError("--at 要非负整数毫秒")
    t0 = time.time()
    idx = tr.call_tar(f"media-frames {h} --at {','.join(map(str, ats))} --width {int(width)}", out_dir, MEDIA_FRAMES_RE)
    for fr in idx["frames"]:
        if fr["file"] not in idx["_received"]:
            raise THError(f"media-frames：index 里的 {fr['file']} 没收到")
    idx["wall"] = round(time.time() - t0, 2)
    log(f"帧 {len(idx['frames'])} 张，超出时长 {idx['out_of_range'] or '无'}，失败 {len(idx['failed'])}，{idx['wall']}s → {out_dir}")
    return idx


def media_clean(tr, h=None, older_than=None):
    if (h is None) == (older_than is None):
        raise THError("media clean 要么给哈希，要么给 --older-than 天")
    arg = h if h else f"--older-than {older_than:g}"
    return tr.call(f"media-clean {arg}")


def cmd_media(tr, a):
    if a.mcmd == "put":
        res = media_put(tr, a.file, a.name)
    elif a.mcmd == "audio":
        res = media_audio(tr, a.hash, a.out, a.segment_min)
    elif a.mcmd == "frames":
        try:
            ats = [int(x) for x in a.at.split(",") if x.strip()]
        except ValueError:
            raise THError("--at 要逗号分隔的整数毫秒")
        res = media_frames(tr, a.hash, ats, a.out, a.width)
    else:
        res = media_clean(tr, a.hash, a.older_than)
    res.pop("_received", None)
    if a.mcmd in ("audio", "frames"):       # 完整 index 在 --out/index.json，这里只回摘要
        n = len(res.get("segments") or res.get("frames") or [])
        res = {k: v for k, v in res.items() if k not in ("segments", "frames")}
        res.update(count=n, out=str(Path(a.out).expanduser()))
    print(json.dumps(res, ensure_ascii=False))
    return 0


def push_tool(tr):
    """先推入口（update），确认新版在了再推 media.json（media-spec；旧入口不认这个动作，不会误替换）。"""
    print(tr.call("update", stdin=ENTRY.read_bytes())["message"])
    print(tr.call("ping")["testhost"])
    print(tr.call("media-spec", stdin=MEDIA_SPEC.read_bytes())["message"])


def cmd_run(tr, a):
    if a.task and (a.k or a.m or a.plugin or a.plugin_file or a.n):
        raise THError("--task 不能与 -n -k -m --plugin 并用（任务的参数写在描述里或跟在 -- 后）")
    if not a.task and not a.tests:
        raise THError("要给测试文件，或用 --task <任务名>")
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
    rc = ["run", rid, a.workdir] + (["--task", a.task] if a.task else ["-n", str(n)])
    for f in plugs:
        rc += ["--plugin", f.stem]
    for kv in a.plugin_env:
        rc += ["--plugin-env", kv]
    for flag, v in (("-k", a.k), ("-m", a.m)):
        if v:
            rc += [flag, v]
    tr.call(" ".join(shlex.quote(x) for x in rc + (["--"] + a.tests if a.task else a.tests)))
    log(f"已开跑 {rid}（Ctrl-C 不会停远端，之后 python client.py result {rid}；要停远端用 cancel {rid}）")
    st = wait(tr, rid, a.timeout)
    if st["state"] == "cancelled":
        print(f"\n{rid}: 已取消（测试机上的进程已停止）")
        return 130
    code = summarize(tr, rid, a.task)
    if a.out_dir:
        fetch_extras(tr, rid, a.out_dir, tree=bool(a.task))
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
    x = sp.add_parser("cancel", help="停止 run：结束其进程树与命令行含该 run 目录路径的残留进程（不按进程名杀）")
    x.add_argument("run_id")
    c = sp.add_parser("clean")
    c.add_argument("rest", nargs=argparse.REMAINDER)
    md = sp.add_parser("media", help="Studio 上的 ffmpeg：上传媒体、转 16k 音频、定点抽帧、清理")
    ms = md.add_subparsers(dest="mcmd", required=True)
    m = ms.add_parser("put", help="上传本地文件（按 sha256 存，已有就不传）")
    m.add_argument("file")
    m.add_argument("--name", help="存到 Studio 上用的文件名，缺省本地文件名")
    m = ms.add_parser("audio", help="转 16 kHz 单声道 16 位 WAV 并分段")
    m.add_argument("hash")
    m.add_argument("--segment-min", type=float, default=10.0, help="每段分钟数（0.01..600，至多 3 位小数），缺省 10")
    m.add_argument("--out", required=True, help="落到 <目录>/audio/NNNN.wav 与 <目录>/index.json")
    m = ms.add_parser("frames", help="按毫秒时间点截帧（jpg）")
    m.add_argument("hash")
    m.add_argument("--at", required=True, help="逗号分隔的毫秒时间点，至多 500 个")
    m.add_argument("--width", type=int, default=960, help="最大宽度（不放大），缺省 960")
    m.add_argument("--out", required=True, help="落到 <目录>/frames/<毫秒>.jpg 与 <目录>/index.json")
    m = ms.add_parser("clean", help="按哈希删，或删超过 N 天没用过的")
    m.add_argument("hash", nargs="?")
    m.add_argument("--older-than", type=float, metavar="天")
    r = sp.add_parser("run")
    r.add_argument("--repo", action="append", default=[], help="本地路径[=名]，第一个带环境描述")
    r.add_argument("--commit-of", action="append", default=[], metavar="名=commit或分支")
    r.add_argument("--snapshot", action="append", default=[], metavar="名=仓库@commit:路径,路径|@清单文件")
    r.add_argument("--workdir", required=True, help="Mac 上 src/ 下的工作子目录，如 xxyi")
    r.add_argument("--env-file", help="环境描述在第一个仓库里的相对路径；缺省找 testhost.json，再找 cmdproto/testenv.json")
    r.add_argument("--task", help="跑环境描述 tasks 里的这个任务（不是 pytest）；其后的位置参数（建议用 -- 隔开）作为追加 argv")
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
    r.add_argument("tests", nargs="*", help="测试文件；给了 --task 时是任务的追加参数")
    a = ap.parse_args(argv)
    tr = Transport(a.host, a.key)
    try:
        if a.cmd == "ping":
            print(json.dumps(tr.call("ping"), ensure_ascii=False, indent=1))
        elif a.cmd == "inventory":
            print(json.dumps(tr.call("inventory"), ensure_ascii=False, indent=1))
        elif a.cmd == "push-tool":
            push_tool(tr)
        elif a.cmd == "media":
            return cmd_media(tr, a)
        elif a.cmd == "status":
            print(json.dumps(tr.call(f"status {a.run_id}")))
        elif a.cmd == "result":
            st = wait(tr, a.run_id, a.timeout)
            if st["state"] == "cancelled":
                print(f"{a.run_id}: 已取消（测试机上的进程已停止）")
                return 130
            code = summarize(tr, a.run_id, st.get("task"))
            if a.out_dir:
                fetch_extras(tr, a.run_id, a.out_dir)
            return code
        elif a.cmd == "fetch":
            fetch_extras(tr, a.run_id, a.out_dir)
        elif a.cmd == "cancel":
            print(json.dumps(tr.call(f"cancel {a.run_id}"), ensure_ascii=False))
        elif a.cmd == "clean":
            print(json.dumps(tr.call("clean " + " ".join(shlex.quote(x) for x in a.rest))))
        else:
            return cmd_run(tr, a)
    except (THError, subprocess.TimeoutExpired) as e:
        print(f"错误：{e}", file=sys.stderr)
        return 4 if getattr(e, "code", None) == "not_found" else 3
    return 0


if __name__ == "__main__":
    sys.exit(main())
