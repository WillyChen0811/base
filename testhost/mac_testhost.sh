#!/bin/bash
# 测试机入口（Mac 端，bash 3.2 兼容）：只由 authorized_keys 的 command= 调用，动作取自 $SSH_ORIGINAL_COMMAND。
# 经 update 动作推送到 ~/xxyi-testhost/bin/testhost。固定动作，不执行任意命令。
# 本机自测：TESTHOST_HOME 指向假目录；TESTHOST_ENV_STUB 指向已有 conda 环境前缀，prepare 不再建环境。
TH="${TESTHOST_HOME:-$HOME/xxyi-testhost}"
export PATH="$TH/miniforge/bin:${PATH:-/usr/bin:/bin}:/usr/bin:/bin:/usr/sbin:/sbin"
PY="$TH/miniforge/bin/python"; [ -x "$PY" ] || PY=python3
CONDA="$TH/miniforge/bin/conda"; [ -x "$CONDA" ] || CONDA=conda
VERSION=5
mkdir -p "$TH"/{bin,repos,snapshots,envs,runs,dl,media} 2>/dev/null
# 退出时放掉还握着的环境锁（fail 在任何位置 exit 都会走到这里）
LOCK_HELD=
on_exit() { [ -n "$LOCK_HELD" ] && rmdir "$LOCK_HELD" 2>/dev/null; }
trap on_exit EXIT

jstr() { printf '%s' "$1" | tr '\n\t' '  ' | sed 's/\\/\\\\/g; s/"/\\"/g'; }
fail() { printf '{"success":false,"message":"%s"}\n' "$(jstr "$*")"; exit 1; }
# 名字：字母数字开头，只许 [A-Za-z0-9._-]（排除 . .. 与 - 开头）
safe() { [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$ ]]; }
# 相对路径：不以 / 或 - 开头，不含 ..
RE_REL='^[]A-Za-z0-9_./@+=,:[-]+$'
saferel() { case "$1" in ''|/*|-*|*..*) return 1;; esac; [[ "$1" =~ $RE_REL ]]; }
isint() { [[ "$1" =~ ^[0-9]{1,3}$ ]]; }

ARGS=()
while IFS= read -r -d '' a; do ARGS+=("$a"); done < <("$PY" -c 'import shlex,sys
sys.stdout.write("".join(x+"\0" for x in shlex.split(sys.argv[1])))' "${SSH_ORIGINAL_COMMAND:-}" 2>/dev/null)

# ---------- 系统信息（macOS 命令都有 Linux 兜底，便于本机测）
host_name() { scutil --get LocalHostName 2>/dev/null || hostname; }
os_ver() { sw_vers -productVersion 2>/dev/null || uname -sr; }
ncpu() { sysctl -n hw.ncpu 2>/dev/null || nproc; }
mem_gb() {
  local b; b=$(sysctl -n hw.memsize 2>/dev/null)
  if [ -n "$b" ]; then echo $((b / 1073741824)); else awk '/MemTotal/{print int($2/1048576)}' /proc/meminfo; fi
}
disk_free_gb() { df -Pk "$TH" 2>/dev/null | awk 'NR == 2 { print int($4 / 1048576) }'; }
load_avg() {
  local l; l=$(sysctl -n vm.loadavg 2>/dev/null)
  if [ -n "$l" ]; then echo $l | tr -d '{}' | xargs; else cut -d' ' -f1-3 /proc/loadavg; fi
}

# ---------- run 状态
alive() { local p; p=$(cat "$1/out/pid" 2>/dev/null); [ -n "$p" ] && kill -0 "$p" 2>/dev/null; }
count_running() {
  local n=0 d
  for d in "$TH"/runs/*/; do [ -d "$d" ] || continue; [ -e "$d/out/exit" ] && continue; alive "${d%/}" && n=$((n + 1)); done
  echo $n
}
guard_th() { [ -n "$TH" ] && [ "$TH" != "/" ] && [ -d "$TH/runs" ] || fail "测试机目录异常"; }

a_ping() {
  local r; r=$(count_running)
  printf '{"success":true,"host":"%s","user":"%s","macos":"%s","arch":"%s","ncpu":%s,"mem_gb":%s,"load":"%s","python":"%s","node":"%s","running":%s,"idle":%s,"disk_free_gb":%s,"testhost":"v%s"}\n' \
    "$(jstr "$(host_name)")" "$(id -un)" "$(jstr "$(os_ver)")" "$(uname -m)" "$(ncpu)" "$(mem_gb)" "$(load_avg)" \
    "$("$PY" -V 2>&1 | awk '{print $2}')" "$(node -v 2>/dev/null)" "$r" "$([ "$r" -eq 0 ] && echo true || echo false)" \
    "$(disk_free_gb || echo null)" "$VERSION"
}

a_update() {
  local tmp="$TH/bin/testhost.incoming"; cat >"$tmp"
  bash -n "$tmp" || { rm -f "$tmp"; fail "新版语法检查不过，没替换"; }
  cp -p "$TH/bin/testhost" "$TH/bin/testhost.prev" 2>/dev/null; chmod 700 "$tmp"; mv "$tmp" "$TH/bin/testhost"
  echo '{"success":true,"message":"已更新，旧版在 testhost.prev"}'
}

# 已有内容清单：仓库里已同步的 commit、已解开的快照、已建的环境
a_inventory() {
  local d n sep=
  printf '{"success":true,"repos":{'
  for d in "$TH"/repos/*.git; do
    [ -d "$d" ] || continue; n=$(basename "$d" .git)
    printf '%s"%s":[%s]' "$sep" "$n" "$(git --git-dir="$d" for-each-ref --format='"%(refname:lstrip=2)"' refs/testhost 2>/dev/null | paste -sd, -)"
    sep=,
  done
  printf '},"snapshots":[%s],"envs":[%s]}\n' \
    "$(cd "$TH/snapshots" && for x in *; do [ -e "$x" ] && printf '"%s"\n' "$x"; done | paste -sd, -)" \
    "$(cd "$TH/envs" && for x in *; do [ -e "$x.ok" ] && printf '"%s"\n' "$x"; done | paste -sd, -)"
}

# sync <repo>：stdin 是 git bundle，里面的 refs/testhost/<sha> 并进 repos/<repo>.git
a_sync() {
  safe "${1:-}" || fail "仓库名不合法"
  local g="$TH/repos/$1.git" b="$TH/dl/sync-$$.bundle"
  [ -d "$g" ] || git init -q --bare "$g" || fail "建仓库失败"
  cat >"$b"
  git --git-dir="$g" fetch -q "$b" '+refs/testhost/*:refs/testhost/*' 2>"$b.err" \
    || { local m; m=$(tail -3 "$b.err"); rm -f "$b" "$b.err"; fail "fetch 失败：$m"; }
  rm -f "$b" "$b.err"
  printf '{"success":true,"repo":"%s"}\n' "$1"
}

# snapshot <名>：stdin 是 tar；已存在就跳过。先解到临时目录再改名，半截的不会被当成成品
a_snapshot() {
  safe "${1:-}" || fail "快照名不合法"
  local d="$TH/snapshots/$1" t="$TH/snapshots/.tmp-$1-$$"
  if [ -d "$d" ]; then cat >/dev/null; printf '{"success":true,"snapshot":"%s","existed":true}\n' "$1"; return; fi
  mkdir -p "$t"
  tar -x -C "$t" -f - 2>"$t.err" || { local m; m=$(tail -3 "$t.err"); rm -rf "$t" "$t.err"; fail "解包失败：$m"; }
  rm -f "$t.err"; mv "$t" "$d"
  printf '{"success":true,"snapshot":"%s","existed":false}\n' "$1"
}

# 环境描述解析：输出 hash/python/conda/pip/pp(pythonpath)/env 行；hash 只看 python、pip 与（若有）conda，换依赖才重建
# 没写 conda 时 hash 的输入与旧版完全一样，已建的环境继续命中
SPEC_PY='
import hashlib, json, os, re, sys
f, run, th = sys.argv[1:4]
snaps = dict(a.split("=", 1) for a in sys.argv[4:])
d = json.load(open(f))
pipl = [str(x) for x in d.get("pip", [])]
for x in ("pytest", "pytest-xdist"):
    if not any(p.lower().startswith(x) for p in pipl): pipl.append(x)
py = str(d.get("python", "3.12"))
cl = [str(x) for x in d.get("conda", [])]
for c in cl:
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._=<>!*,-]{0,80}", c): sys.exit("conda 包名不合法：" + c)
key = [py, sorted(pipl)] + ([sorted(cl)] if cl else [])
h = hashlib.sha256(json.dumps(key).encode()).hexdigest()[:12]
print("hash", h); print("python", py)
for c in cl: print("conda", c)
for p in pipl: print("pip", p)
for p in d.get("pythonpath", []): print("pp", p)
for k, sd in d.get("snapshots", {}).items():
    if sd.get("repo_env") and os.path.isdir(th + "/repos/" + k + "-src.git"):
        print("env", sd["repo_env"] + "=" + th + "/repos/" + k + "-src.git")
for k, v in d.get("env", {}).items():
    v = v.replace("{run}", run)
    while "{snapshot:" in v:
        a = v.index("{snapshot:"); b = v.index("}", a); key = v[a + 10:b]
        if key not in snaps or not os.path.isdir(th + "/snapshots/" + snaps[key]):
            sys.exit("快照 %s 缺失（--snap %s=<名>）" % (key, key))
        v = v[:a] + th + "/snapshots/" + snaps[key] + v[b + 1:]
    print("env", k + "=" + v)
'

# 按描述 hash 建/复用 envs/<hash>。读调用方的 hash、pyv、cpk[]、reqs[]，设调用方的 E、reused（bash 动态作用域）。
# 同 hash 并发靠 envs/.lock-<hash> 目录锁串行；锁建完就放（不等动作结束），fail 时由 EXIT 陷阱放
ensure_env() {
  E="$TH/envs/$hash"; reused=true
  local lock="$TH/envs/.lock-$hash" i=0
  [ -e "$E.ok" ] && [ -x "$E/bin/python" ] && return 0
  reused=false
  if [ -n "${TESTHOST_ENV_STUB:-}" ]; then ln -sfn "$TESTHOST_ENV_STUB" "$E"; touch "$E.ok"; return 0; fi
  until mkdir "$lock" 2>/dev/null; do i=$((i + 1)); [ $i -gt 900 ] && fail "等环境锁超时"; sleep 2; done
  LOCK_HELD="$lock"
  if [ -e "$E.ok" ] && [ -x "$E/bin/python" ]; then reused=true; else
    rm -rf "$E" "$E.ok"; local lg="$TH/dl/env-$hash.log"
    { "$CONDA" create -y -q -p "$E" "python=$pyv" pip ${cpk[@]+"${cpk[@]}"} \
      && "$E/bin/python" -m pip install -q --disable-pip-version-check ${reqs[@]+"${reqs[@]}"}; } >"$lg" 2>&1 \
      || { rm -rf "$E"; fail "建环境失败（日志 ${lg}）：$(tail -3 "$lg")"; }
    touch "$E.ok"
  fi
  rmdir "$lock" 2>/dev/null; LOCK_HELD=
}

# prepare <run_id> <repo>=<commit>... [--env 相对路径] [--snap 键=快照名]
a_prepare() {
  local rid="${1:-}"; safe "$rid" || fail "run_id 不合法"; shift
  local R="$TH/runs/$rid" envrel= t0=$SECONDS first= spec x repo commit
  local items=() snaps=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --env) envrel="${2:-}"; shift 2;;
      --snap) [[ "${2:-}" =~ ^[A-Za-z0-9._-]+=[A-Za-z0-9._-]+$ ]] || fail "--snap 格式不对"; snaps+=("$2"); shift 2;;
      *=*) items+=("$1"); shift;;
      *) fail "不认识的参数：$1";;
    esac
  done
  [ ${#items[@]} -gt 0 ] || fail "至少要一个 <仓库>=<commit>"
  [ -z "$envrel" ] || saferel "$envrel" || fail "--env 路径不合法"
  guard_th
  if [ -e "$R" ]; then alive "$R" && fail "run 正在跑"; rm -rf "$R"; fi
  mkdir -p "$R"/{src,home,tmp,ramdisk,out}
  local res=
  for x in "${items[@]}"; do
    repo="${x%%=*}"; commit="${x#*=}"
    safe "$repo" || fail "仓库名不合法：$repo"
    [[ "$commit" =~ ^[0-9a-f]{7,40}$ ]] || fail "commit 不合法：$commit"
    local g="$TH/repos/$repo.git"
    [ -d "$g" ] || fail "仓库 $repo 还没同步"
    commit=$(git --git-dir="$g" rev-parse --verify -q "$commit^{commit}") || fail "$repo 里没有 ${commit}，先 sync"
    mkdir -p "$R/src/$repo"
    git --git-dir="$g" archive "$commit" | tar -x -C "$R/src/$repo" || fail "检出 $repo 失败"
    [ -n "$first" ] || first="$repo"
    res="$res${res:+,}\"$repo\":\"$commit\""
  done
  if [ -z "$envrel" ]; then   # 没给 --env：先 testhost.json，再兼容旧的 cmdproto/testenv.json
    for x in testhost.json cmdproto/testenv.json; do [ -f "$R/src/$first/$x" ] && { envrel="$x"; break; }; done
  fi
  [ -f "$R/src/$first/$envrel" ] || fail "$first 里没有环境描述 $envrel"
  spec=$("$PY" -c "$SPEC_PY" "$R/src/$first/$envrel" "$R" "$TH" ${snaps[@]+"${snaps[@]}"} 2>"$R/spec.err") \
    || fail "环境描述不可用：$(tail -2 "$R/spec.err")"
  local hash pyv reqs=() cpk=() pps= envs=()
  while read -r k v; do
    case "$k" in hash) hash="$v";; python) pyv="$v";; pip) reqs+=("$v");; conda) cpk+=("$v");; pp) pps="$pps$v"$'\n';; env) envs+=("$v");; esac
  done <<<"$spec"
  printf '%s' "$pps" >"$R/pythonpath"
  { printf '%s\n' ${envs[@]+"${envs[@]}"}; } >"$R/envvars"
  echo "$hash" >"$R/envhash"; echo "src/$first/$envrel" >"$R/envfile"

  local E reused
  ensure_env
  # 子进程会清掉 PYTHONPATH（如基准子进程）；pythonpath 另走环境变量，由 .pth 并进 sys.path
  local sp; sp=$("$E/bin/python" -c 'import site;print(site.getsitepackages()[0])' 2>/dev/null) \
    && [ -d "$sp" ] && printf '%s\n' 'import os,sys; sys.path.extend(p for p in os.environ.get("TESTHOST_PYPATH","").split(":") if p)' >"$sp/testhost_extra.pth"
  printf '{"success":true,"run_id":"%s","env":"%s","env_reused":%s,"repos":{%s},"seconds":%s}\n' \
    "$rid" "$hash" "$reused" "$res" $((SECONDS - t0))
}

# plugin <run_id> <文件名.py>：stdin 是外挂文件，落到 runs/<id>/plugins/（prepare 之后、run 之前）
a_plugin() {
  local rid="${1:-}" f="${2:-}"; safe "$rid" || fail "run_id 不合法"
  [[ "$f" =~ ^[A-Za-z0-9_]{1,60}\.py$ ]] || fail "外挂文件名只许 [A-Za-z0-9_] 加 .py"
  local R="$TH/runs/$rid"; [ -d "$R/src" ] || fail "没有 $rid，先 prepare"
  guard_th; ! alive "$R" || [ -e "$R/out/exit" ] || fail "run 正在跑"
  mkdir -p "$R/plugins"; rm -f "$R/plugins/$f"
  cat >"$R/plugins/$f"
  printf '{"success":true,"plugin":"%s"}\n' "$f"
}

# 任务解析：从该 run 检出的环境描述里读 tasks[名]，不信任何外部传入的命令。
# 输出（NUL 分隔）：cwd（相对工作子目录，"." 表示就是它）、argv...；outputs 模式写到第 4 个参数文件，一行一个。{run} 换成 run 目录。
TASK_PY='
import json, sys
f, name, run, outf = sys.argv[1:5]
t = json.load(open(f)).get("tasks", {}).get(name)
if not isinstance(t, dict): sys.exit("描述里没有任务 " + name)
cmd = t.get("cmd")
if not (isinstance(cmd, list) and cmd and all(isinstance(x, str) and x and "\n" not in x for x in cmd)):
    sys.exit("任务 cmd 要是非空字符串数组")
def rel(x, what):
    if not isinstance(x, str) or not x or x.startswith(("/", "-")) or ".." in x.split("/") or "\n" in x:
        sys.exit(what + "路径不合法：" + str(x))
    return x
cwd = rel(t.get("cwd", "."), "cwd")
outs = [rel(x, "outputs ") for x in t.get("outputs", [])]
open(outf, "w").write("".join(x + "\n" for x in outs))
sys.stdout.write("".join(x.replace("{run}", run) + "\0" for x in [cwd] + cmd))
'

# run <run_id> <工作子目录> --task <任务名> [--plugin-env K=V]... [-- 追加参数...]：跑描述里声明的任务（argv 直接执行，不经 shell）
# run <run_id> <工作子目录> [-n N] [-k 表达式] [-m 标记] [--plugin 模块]... [--plugin-env K=V]... <测试文件...>：后台跑，立即返回
a_run() {
  local rid="${1:-}" wd="${2:-}"; safe "$rid" || fail "run_id 不合法"; saferel "$wd" || fail "工作子目录不合法"
  shift 2
  local R="$TH/runs/$rid" n= k= m= files=() plugs=() penv=() x task= extra=()
  [ -d "$R/src/$wd" ] || fail "没有 $rid/src/${wd}，先 prepare"
  while [ $# -gt 0 ]; do
    case "$1" in
      -n) isint "${2:-}" && [ "$2" -ge 1 ] && [ "$2" -le 64 ] || fail "-n 要 1..64"; n="$2"; shift 2;;
      -k) k="${2:-}"; case "$k" in ''|-*) fail "-k 值不合法";; esac; shift 2;;
      -m) m="${2:-}"; case "$m" in ''|-*) fail "-m 值不合法";; esac; shift 2;;
      --plugin) [[ "${2:-}" =~ ^[A-Za-z0-9_]{1,60}$ ]] || fail "--plugin 模块名只许 [A-Za-z0-9_]"
        [ -f "$R/plugins/$2.py" ] && [ ! -L "$R/plugins/$2.py" ] || fail "外挂 $2 没上传（plugin 动作）"
        plugs+=("$2"); shift 2;;
      --plugin-env) [[ "${2:-}" =~ ^[A-Za-z_][A-Za-z0-9_]{0,60}=[^[:cntrl:]]*$ ]] || fail "--plugin-env 要 KEY=VALUE"
        case "${2%%=*}" in PATH|HOME|TMPDIR|PYTHON*|TESTHOST_*|LD_*|DYLD_*|CMDPROTO_RAMDISK_ROOT) fail "--plugin-env 不许改 ${2%%=*}";; esac
        x="${2#*=}"; penv+=("${2%%=*}=${x//\{run\}/$R}"); shift 2;;
      --task) safe "${2:-}" || fail "--task 任务名不合法"; task="$2"; shift 2;;
      --) shift; [ -n "$task" ] || fail "-- 追加参数只能跟 --task 用"
        for x in "$@"; do case "$x" in *$'\n'*) fail "追加参数不许含换行";; esac; extra+=("$x"); done; break;;
      -*) fail "不许的选项：${1}（只放行 -n -k -m --plugin --plugin-env）";;
      *) saferel "$1" || fail "测试路径不合法：$1"; files+=("$1"); shift;;
    esac
  done
  if [ -n "$task" ]; then
    [ ${#files[@]} -eq 0 ] && [ ${#plugs[@]} -eq 0 ] && [ -z "$n$k$m" ] || fail "--task 不能与测试文件、-n -k -m、--plugin 并用"
  else [ ${#files[@]} -gt 0 ] || fail "没有测试文件"; fi
  [ -e "$R/out/exit" ] || ! alive "$R" || fail "这个 run 已经在跑"
  rm -f "$R"/out/*
  local E="$TH/envs/$(cat "$R/envhash")" pp= p q
  while read -r p; do
    [ -n "$p" ] || continue
    q=$(cd "$R/src/$wd/$p" 2>/dev/null && pwd -P) || fail "pythonpath 不存在：$p"
    case "$q" in "$R"/src/*) pp="$pp${pp:+:}$q";; *) fail "pythonpath 逃出了 run 目录：$p";; esac
  done <"$R/pythonpath"
  if [ ${#plugs[@]} -gt 0 ]; then
    q=$(cd "$R/plugins" 2>/dev/null && pwd -P) || fail "没有 plugins 目录"
    [ "$q" = "$(cd "$R" && pwd -P)/plugins" ] || fail "plugins 目录逃出了 run 目录"
    pp="$q${pp:+:}$pp"
  fi
  local ev=(HOME="$R/home" TMPDIR="$R/tmp" CMDPROTO_RAMDISK_ROOT="$R/ramdisk" PYTHONPATH="$pp" TESTHOST_PYPATH="$pp"
            PATH="$E/bin:$PATH" PYTHONUNBUFFERED=1 PYTHONUTF8=1 TESTHOST_RUN_ID="$rid")
  while IFS= read -r p; do [ -n "$p" ] && ev+=("$p"); done <"$R/envvars"
  ev+=(${penv[@]+"${penv[@]}"})
  local cmd cwd="$R/src/$wd"
  if [ -n "$task" ]; then
    local tl=() cw
    "$PY" -c "$TASK_PY" "$R/$(cat "$R/envfile")" "$task" "$R" "$R/out/outputs" >"$R/taskargv" 2>"$R/spec.err" \
      || fail "任务不可用：$(tail -2 "$R/spec.err")"
    while IFS= read -r -d '' x; do tl+=("$x"); done <"$R/taskargv"
    cw="${tl[0]}"; cmd=("${tl[@]:1}")
    q=$(cd "$R/src/$wd/$cw" 2>/dev/null && pwd -P) || fail "任务 cwd 不存在：$cw"
    case "$q" in "$R"/src/*) cwd="$q";; *) fail "任务 cwd 逃出了 run 目录：$cw";; esac
    case "${cmd[0]}" in */*) ;; *) q=$(PATH="$E/bin:$PATH" command -v "${cmd[0]}") || fail "找不到命令 ${cmd[0]}"; cmd[0]="$q";; esac
    cmd+=(${extra[@]+"${extra[@]}"})
    ev+=(TESTHOST_TASK="$task"); echo "$task" >"$R/out/task"
  else
    cmd=("$E/bin/python" -m pytest -p no:cacheprovider -v -rfE --junitxml="$R/out/junit.xml")
    [ -n "$n" ] && [ "$n" -gt 1 ] && cmd+=(-n "$n")
    [ -n "$k" ] && cmd+=(-k "$k")
    [ -n "$m" ] && cmd+=(-m "$m")
    for x in ${plugs[@]+"${plugs[@]}"}; do cmd+=(-p "$x"); done
    cmd+=("${files[@]}")
  fi
  date +%s >"$R/out/start"
  # 后台化必须只贴 nohup 这一条简单命令（fork+exec 立刻带重定向）；若把 cd&&nohup 整条放后台，
  # bash 起的作业 shell 会全程握着调用方的 stdout 管道（ssh/capture 要等 run 跑完才返回）
  ( cd "$cwd" && { nohup env -i "${ev[@]}" bash -c 'rc=0; "$@" || rc=$?; date +%s >"$0.end"; echo $rc >"$0"' \
      "$R/out/exit" "${cmd[@]}" >"$R/out/log" 2>&1 </dev/null & echo $! >"$R/out/pid"; } )
  printf '{"success":true,"run_id":"%s","started":true,"workers":%s}\n' "$rid" "${n:-1}"
}

# ---------- cancel：停止一个 run。只认本 run 目录路径与 pid 进程树，绝不按进程名（postgres/python/sleep）杀
run_alive() { local s; s=$(ps -o state= -p "$1" 2>/dev/null); [ -n "$s" ] && [ "$s" != Z ]; }
# 自杀防护：脚本自身与整条父进程链（fake TH 自测时祖先的命令行里也带 run 目录路径）
self_excl() {
  local p=$$ out=" $$" pp
  while [ -n "$p" ] && [ "$p" -gt 1 ]; do
    pp=$(ps -o ppid= -p "$p" 2>/dev/null | tr -dc '0-9')
    [ -n "$pp" ] || break
    [ "$pp" -eq "$p" ] && break
    out="$out $pp"
    p=$pp
  done
  printf '%s' "$out"
}
# 进程树：ps 全表交给 awk 求后代闭包（输出不含 roots 自身；逐行读两列）
tree_pids() {
  ps -axo pid=,ppid= 2>/dev/null | awk -v roots=" $1 " '
    { ppid[$1] = $2 }
    END {
      n = split(roots, r, " ")
      for (i = 1; i <= n; i++) if (r[i] != "") { seed[r[i]] = 1; keep[r[i]] = 1 }
      ch = 1
      while (ch) {
        ch = 0
        for (p in ppid)
          if (!(p in keep) && (ppid[p] in keep)) { keep[p] = 1; ch = 1 }
      }
      out = ""
      for (p in keep) if (!(p in seed)) out = out " " p
      printf "%s", out
    }'
}
# 非僵尸的存活 pid 列表（僵尸已死，launchd/init 回收前不该算幸存者）
live_pids() {
  local s="" p st
  for p in $1; do
    st=$(ps -o state= -p "$p" 2>/dev/null)
    [ -n "$st" ] && [ "$st" != Z ] && s="$s $p"
  done
  printf '%s' "$s"
}
a_cancel() {
  safe "${1:-}" || fail "run_id 不合法"
  local R="$TH/runs/$1"; [ -d "$R/out" ] || fail "没有这个 run"
  guard_th
  local excl tset pg p left="" secs killed=0 survivors=0
  excl=$(self_excl)
  tset=""
  pg=$(cat "$R/out/pid" 2>/dev/null | tr -dc '0-9')
  [ -n "$pg" ] && run_alive "$pg" && tset="$pg"
  # (a) pid 文件进程树（含正常子进程与 xdist worker）
  [ -n "${tset// /}" ] && tset="$tset $(tree_pids "$tset")"
  # (b) 命令行含 "$R/"（带尾斜杠防 run1 前缀误中 run10）：抓已孤儿化的进程（如临时 PG postmaster）
  for pg in $(pgrep -f -- "$R/" 2>/dev/null); do
    case " $excl " in *" $pg "*) continue;; esac
    case " $tset " in *" $pg "*) continue;; esac
    tset="$tset $pg"
  done
  if [ -z "${tset// /}" ] && [ -e "$R/out/exit" ]; then
    printf '{"success":true,"run_id":"%s","state":"finished","killed":0,"survivors":0,"note":"这个 run 已经结束"}\n' "$1"
    return
  fi
  echo "$tset" >"$R/out/cancelled.set"     # 审计：本次准备结束的 pid 集合
  for p in $tset; do kill -TERM "$p" 2>/dev/null; done
  secs=0
  while [ $secs -lt 10 ]; do
    left=$(live_pids "$tset"); left="${left// /}"
    [ -z "$left" ] && break
    sleep 0.5; secs=$((secs + 1))
  done
  for p in $left; do kill -KILL "$p" 2>/dev/null; done
  sleep 1
  for p in $tset; do
    if run_alive "$p"; then survivors=$((survivors + 1)); else killed=$((killed + 1)); fi
  done
  [ -e "$R/out/exit" ] || echo 143 >"$R/out/exit"
  [ -e "$R/out/exit.end" ] || date +%s >"$R/out/exit.end"
  date +%s >"$R/out/cancelled"
  printf '{"success":true,"run_id":"%s","state":"cancelled","killed":%s,"survivors":%s}\n' "$1" "$killed" "$survivors"
}

# 日志计数：只数顶层 pytest 的结果行。xdist 下 `[gw3] [ 12%] FAILED x.py::t`，单进程 `x.py::t FAILED  [ 12%]`；
# 内层子进程 pytest 的输出只会出现在末尾 FAILURES/ERRORS 报告的 Captured 段里，读到该段标题就停。
log_counts() {
  awk '
    /^=+ (FAILURES|ERRORS) =+$/ { exit }
    /^\[gw[0-9]+\] \[ *[0-9]+%\] (PASSED|FAILED|SKIPPED) / {
      l = $0; sub(/^\[gw[0-9]+\] \[ *[0-9]+%\] /, "", l); split(l, a, " "); c[a[1]]++; next }
    /^[^ ]+::.* (PASSED|FAILED|SKIPPED)( .*)? +\[ *[0-9]+%\]$/ {
      match($0, / (PASSED|FAILED|SKIPPED)( |$)/); l = substr($0, RSTART + 1, RLENGTH - 1); split(l, a, " "); c[a[1]]++ }
    END { printf "%d %d %d\n", c["PASSED"], c["FAILED"], c["SKIPPED"] }' "$1" 2>/dev/null
}
# 跑完以 junit 为准；读不到才用日志。输出 "通过 失败 跳过"
junit_counts() {
  "$PY" -c 'import sys, xml.etree.ElementTree as E
r = E.parse(sys.argv[1]).getroot()
ss = [r] if r.tag == "testsuite" else list(r)
g = lambda k: sum(int(float(s.get(k, 0))) for s in ss)
t, f, e, k = g("tests"), g("failures"), g("errors"), g("skipped")
print(t - f - e - k, f + e, k)' "$1" 2>/dev/null
}

a_status() {
  local rid="${1:-}"; safe "$rid" || fail "run_id 不合法"
  R="$TH/runs/$rid"; [ -d "$R/out" ] || fail "没有这个 run"
  local state=prepared ex=null now st=0 en p f s pct cnt=
  if [ -e "$R/out/exit" ]; then state=finished; ex=$(cat "$R/out/exit")
  elif alive "$R"; then state=running
  elif [ -e "$R/out/pid" ]; then state=dead; fi
  [ -e "$R/out/cancelled" ] && state=cancelled
  now=$(date +%s); st=$(cat "$R/out/start" 2>/dev/null || echo "$now"); en=$(cat "$R/out/exit.end" 2>/dev/null || echo "$now")
  [ "$state" = finished ] && [ -s "$R/out/junit.xml" ] && cnt=$(junit_counts "$R/out/junit.xml")
  [ -n "$cnt" ] || cnt=$(log_counts "$R/out/log")
  read -r p f s <<<"${cnt:-0 0 0}"
  pct=$(grep -oE '\[ *[0-9]+%\]' "$R/out/log" 2>/dev/null | tail -1 | tr -dc '0-9')
  local task tail=
  task=$(cat "$R/out/task" 2>/dev/null)
  if [ -n "$task" ]; then   # 任务没有通过/失败计数，给日志末几行
    tail=$(tail -n 5 "$R/out/log" 2>/dev/null | LC_ALL=C tr '\000-\011\013-\037\177' ' ' | sed 's/\\/\\\\/g; s/"/\\"/g' \
      | awk '{printf "%s%s", (NR > 1 ? "\\n" : ""), $0}')
    [ "$state" = finished ] && pct=
  fi
  printf '{"success":true,"run_id":"%s","state":"%s","finished":%s,"exit":%s,"passed":%s,"failed":%s,"skipped":%s,"percent":%s,"elapsed":%s,"task":"%s","log_tail":"%s"}\n' \
    "$rid" "$state" "$([ "$state" = running ] || [ "$state" = prepared ] && echo false || echo true)" "$ex" \
    "${p:-0}" "${f:-0}" "${s:-0}" "${pct:-0}" $((en - st)) "$task" "$tail"
}

# fetch <run_id>：junit、日志尾部、退出码打成 tar 输出
a_fetch() {
  safe "${1:-}" || fail "run_id 不合法"
  local o="$TH/runs/$1/out"; [ -d "$o" ] || fail "没有这个 run"
  tail -n 400 "$o/log" >"$o/log.tail" 2>/dev/null
  local fs=() x; for x in junit.xml log.tail exit exit.end start; do [ -e "$o/$x" ] && fs+=("$x"); done
  tar -c -C "$o" -f - "${fs[@]}"
}

# 任务声明的 outputs（out/outputs，一行一个相对路径或通配，相对 run 目录或 run 的 out/）：命中的普通文件（不跟软链、≤50MB、不出 run 目录）
task_outputs() {
  local R="$1" pat x
  [ -f "$R/out/outputs" ] || return 0
  "$PY" -c 'import glob, os, sys
R = os.path.realpath(sys.argv[1])
for pat in open(sys.argv[2]).read().split("\n"):
    if not pat: continue
    for base in (R, R + "/out"):
        for x in glob.glob(os.path.join(base, pat), recursive=True):
            if os.path.islink(x) or not os.path.isfile(x): continue
            if not os.path.realpath(x).startswith(R + "/") or os.path.getsize(x) > 52428800: continue
            print(os.path.relpath(x, R))' "$R" "$R/out/outputs"
}

# fetchx <run_id>：run 目录里外挂等写出的文件（run 目录顶层、out/ 里非内置的普通文件），tar 输出，成员名相对 run 目录
a_fetchx() {
  safe "${1:-}" || fail "run_id 不合法"
  local R="$TH/runs/$1"; [ -d "$R/out" ] || fail "没有这个 run"
  local x
  { for x in "$R"/* "$R"/out/*; do
    [ -f "$x" ] && [ ! -L "$x" ] || continue
    case "${x#"$R"/}" in pythonpath|envvars|envhash|envfile|taskargv|spec.err|out/outputs|out/task|out/log|out/log.tail|out/pid|out/exit|out/exit.end|out/start|out/junit.xml) continue;; esac
    [ "$(wc -c <"$x")" -le 52428800 ] || continue
    printf '%s\n' "${x#"$R"/}"
  done
  task_outputs "$R"; } | sort -u | tar -c -C "$R" -T - -f -
}

wipe_dir() { guard_th; find "$TH/$1" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null; }
a_clean() {
  local what="${1:-}"; shift; guard_th
  case "$what" in
    runs)
      local keep=0 i=0 r del=0
      [ "${1:-}" = "--keep" ] && { isint "${2:-}" || fail "--keep 要整数"; keep="$2"; }
      for r in $(ls -t "$TH/runs"); do
        i=$((i + 1)); [ $i -le "$keep" ] && continue
        alive "$TH/runs/$r" && [ ! -e "$TH/runs/$r/out/exit" ] && continue
        rm -rf "${TH:?}/runs/$r"; del=$((del + 1))
      done
      printf '{"success":true,"deleted":%s}\n' "$del";;
    run)
      safe "${1:-}" || fail "run_id 不合法"
      [ ! -e "$TH/runs/$1/out/exit" ] && alive "$TH/runs/$1" && fail "run 正在跑"
      rm -rf "${TH:?}/runs/$1"; echo '{"success":true}';;
    envs|snapshots|repos)
      [ "$(count_running)" -eq 0 ] || fail "有 run 在跑，不清 $what"
      wipe_dir "$what"; echo '{"success":true}';;
    *) fail "clean 用法：runs [--keep N] | run <id> | envs | snapshots | repos";;
  esac
}

# ---------- media：上传的媒体文件按内容 sha256 存 media/<哈希>/<原文件名>，同步动作（不走 prepare/run）
# ffmpeg/ffprobe 用 media.json 描述的 conda 环境（envs/<hash>/bin 下的绝对路径；没建就当场建一次）。
# 重活都在下面这段 Python 里（只用标准库，跑在 miniforge 的 python 上）；输出 tar 的动作出错时：
# 还没开始写 tar 就在 stdout 回 JSON（以 { 开头），已经开始写了就把 JSON 写 stderr、退出码 1、不写 index.json。
MEDIA_PY='
import hashlib, io, json, os, re, shutil, signal, struct, subprocess, sys, tarfile, tempfile, time
from concurrent.futures import ThreadPoolExecutor

MAX_PUT = 20 << 30      # 单文件上限
RESERVE = 10 << 30      # 写完后磁盘至少还留这么多
SR = 16000
HEX = re.compile(r"[0-9a-f]{64}")
STREAMING = []          # 非空 = stdout 已开始写 tar
BUSY, PROCS = [], []

def _term(*_):
    raise SystemExit(1)     # 断连/被杀：走 finally 清临时文件
for _s in (signal.SIGTERM, signal.SIGHUP):
    signal.signal(_s, _term)

def say(**kw):
    sys.stdout.write(json.dumps(dict(success=True, **kw), ensure_ascii=False) + "\n")
    sys.stdout.flush()

def die(msg, code="error"):
    out = sys.stderr if STREAMING else sys.stdout
    out.write(json.dumps({"success": False, "message": msg, "code": code}, ensure_ascii=False) + "\n")
    out.flush()
    sys.exit(1)

def tail(b):
    if hasattr(b, "seek"):
        b.seek(0); b = b.read()
    return b.decode("utf-8", "replace").strip()[-300:]

def free_gb():
    return round(shutil.disk_usage(ROOT).free / 2 ** 30, 1)

def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except OSError:
        return True
    return True

def pid_of(name):     # .tmp-<pid>-xxx / .work-<pid>-xxx / .busy-<pid>
    try:
        return int(name.split("-")[1])
    except (IndexError, ValueError):
        return None

def media_file(d):
    try:
        ns = [n for n in os.listdir(d) if not n.startswith(".")]
    except OSError:
        return None
    ns = [n for n in ns if os.path.isfile(os.path.join(d, n)) and not os.path.islink(os.path.join(d, n))]
    return os.path.join(d, ns[0]) if len(ns) == 1 else None

def check_hash(h):
    if not HEX.fullmatch(h or ""):
        die("哈希要 64 位小写十六进制 sha256", "bad_args")
    return os.path.join(ROOT, h)

def need(h):
    d = check_hash(h)
    f = media_file(d)
    if not f:
        die("测试机上没有这个哈希的文件（先 media put）", "not_found")
    return d, f

def touch(d):
    with open(os.path.join(d, ".last_used"), "w") as w:
        w.write(str(int(time.time())))

def last_used(d):
    try:
        with open(os.path.join(d, ".last_used")) as r:
            return int(r.read().strip())
    except (OSError, ValueError):
        try:
            return int(os.path.getmtime(d))
        except OSError:
            return 0

def busy(d):
    p = os.path.join(d, ".busy-%d" % os.getpid())
    open(p, "w").close()
    BUSY.append(p)

def in_use(d):
    try:
        ns = os.listdir(d)
    except OSError:
        return False
    for n in ns:
        if n.startswith(".busy-") and pid_of(n) is not None:
            if alive(pid_of(n)):
                return True
            try:
                os.remove(os.path.join(d, n))
            except OSError:
                pass
    return False

def dir_bytes(d):
    n = 0
    for a, _, fs in os.walk(d):
        for x in fs:
            try:
                n += os.lstat(os.path.join(a, x)).st_size
            except OSError:
                pass
    return n

def sweep():
    """清掉中断留下的 .tmp-*（上传半截）/.work-*（转换中间产物）：进程已不在或超过 2 天。"""
    now = time.time()
    for n in os.listdir(ROOT):
        if not n.startswith((".tmp-", ".work-")):
            continue
        p = os.path.join(ROOT, n)
        pid = pid_of(n)
        try:
            old = now - os.path.getmtime(p) > 2 * 86400
        except OSError:
            continue
        if old or pid is None or not alive(pid):
            shutil.rmtree(p, ignore_errors=True)

def good_name(n):
    try:
        b = n.encode("utf-8")
    except UnicodeEncodeError:
        return False
    return (0 < len(b) <= 200 and not n.startswith((".", "-")) and "/" not in n and "\\" not in n
            and not any(ord(c) < 32 or ord(c) == 127 for c in n))

def cmd_put(a):
    if len(a) not in (2, 4) or (len(a) == 4 and not (a[2] == "--size" and re.fullmatch(r"[0-9]{1,15}", a[3]))):
        die("用法：media-put <sha256> <文件名> [--size 字节数]", "bad_args")
    h, name = a[0], a[1]
    size = int(a[3]) if len(a) == 4 else None
    d = check_hash(h)
    if not good_name(name):
        die("文件名不合法（不能有 / \\ 和控制字符，不能以 . 或 - 开头，UTF-8 不超过 200 字节）", "bad_args")
    sweep()
    f = media_file(d)
    if f:       # 已有：不读 stdin，客户端那边 ssh 随即收尾
        touch(d)
        say(hash=h, name=os.path.basename(f), size=os.path.getsize(f), existed=True, free_gb=free_gb())
        return
    if size is not None:
        if size > MAX_PUT:
            die("单文件超过上限 %d GiB" % (MAX_PUT >> 30), "too_big")
        if shutil.disk_usage(ROOT).free - size < RESERVE:
            die("磁盘不够：写完后要留 %d GiB（现在剩 %s GiB）" % (RESERVE >> 30, free_gb()), "no_space")
    tmp = tempfile.mkdtemp(prefix=".tmp-%d-" % os.getpid(), dir=ROOT)
    t0 = time.time()
    try:
        sha, n, chk = hashlib.sha256(), 0, 0
        inp = sys.stdin.buffer
        with open(os.path.join(tmp, name), "wb") as w:
            while True:
                b = inp.read(1 << 20)
                if not b:
                    break
                n += len(b)
                if n > MAX_PUT:
                    die("单文件超过上限 %d GiB" % (MAX_PUT >> 30), "too_big")
                if n - chk >= 256 << 20:
                    chk = n
                    if shutil.disk_usage(ROOT).free < RESERVE:
                        die("磁盘不够：要留 %d GiB" % (RESERVE >> 30), "no_space")
                sha.update(b)
                w.write(b)
        if size is not None and n != size:
            die("收到 %d 字节，和 --size %d 不一致（传输中断？）" % (n, size), "size_mismatch")
        got = sha.hexdigest()
        if got != h:
            die("哈希校验不一致：收到的内容是 %s" % got, "hash_mismatch")
        touch(tmp)
        existed = False
        if os.path.isdir(d) and not media_file(d) and not in_use(d):
            shutil.rmtree(d, ignore_errors=True)     # 残缺目录（没有成品文件）
        try:
            os.rename(tmp, d)
        except OSError:
            if not media_file(d):
                raise
            existed = True                          # 并发 put 了同一份，别人先落盘
        say(hash=h, name=name, size=n, existed=existed, seconds=round(time.time() - t0, 2), free_gb=free_gb())
    finally:
        if os.path.isdir(tmp):
            shutil.rmtree(tmp, ignore_errors=True)

def probe(f):
    p = subprocess.run([FP, "-v", "error", "-of", "json", "-show_entries",
                        "format=duration,start_time:stream=index,codec_type,codec_name,duration,width,height"
                        ":stream_disposition=attached_pic", "file:" + f], capture_output=True)
    if p.returncode:
        die("ffprobe 读不了这个文件：" + tail(p.stderr), "bad_media")
    j = json.loads(p.stdout or b"{}")
    try:
        dur = float(j.get("format", {}).get("duration"))
        dur = int(dur * 1000) if dur == dur and dur > 0 else None
    except (TypeError, ValueError):
        dur = None
    return j.get("streams", []), dur

class Tar:
    """stdout 上的流式 tar，第一次写成员时才打开（之前出错还能回 JSON）。"""
    def __init__(self):
        self.t = None
    def _open(self):
        if self.t is None:
            STREAMING.append(1)
            self.t = tarfile.open(fileobj=sys.stdout.buffer, mode="w|", format=tarfile.GNU_FORMAT)
    def add(self, path, arc):
        self._open()
        self.t.add(path, arcname=arc, recursive=False)
    def add_bytes(self, arc, data):
        self._open()
        ti = tarfile.TarInfo(arc)
        ti.size, ti.mtime, ti.mode = len(data), int(time.time()), 0o644
        self.t.addfile(ti, io.BytesIO(data))
    def close(self):
        self._open()
        self.t.close()
        sys.stdout.buffer.flush()

def wav_header(nbytes):
    return (b"RIFF" + struct.pack("<I", 36 + nbytes) + b"WAVEfmt "
            + struct.pack("<IHHIIHH", 16, 1, 1, SR, SR * 2, 2, 16) + b"data" + struct.pack("<I", nbytes))

def cmd_audio(a):
    if not a:
        die("用法：media-audio <sha256> [--segment-min 分钟]", "bad_args")
    h, rest, seg_min = a[0], a[1:], 10.0
    if rest:
        if len(rest) != 2 or rest[0] != "--segment-min" or not re.fullmatch(r"[0-9]{1,3}(\.[0-9]{1,3})?", rest[1]):
            die("用法：media-audio <sha256> [--segment-min 分钟]", "bad_args")
        seg_min = float(rest[1])
    if not 0.01 <= seg_min <= 600:
        die("--segment-min 要在 0.01..600", "bad_args")
    d, f = need(h)
    if FF == "-":
        return
    touch(d)
    busy(d)
    streams, _ = probe(f)
    if not any(s.get("codec_type") == "audio" for s in streams):
        die("这个文件没有音轨", "no_audio")
    seg = int(round(seg_min * 60 * SR))      # 每段样本数（--segment-min 至多 3 位小数，必是 16 的倍数 = 整毫秒）
    work = tempfile.mkdtemp(prefix=".work-%d-" % os.getpid(), dir=ROOT)
    tar, segs, total = Tar(), [], [0]     # total：已切出的样本数
    errf = open(os.path.join(work, "ffmpeg.err"), "w+b")
    # 时间轴与 -ss 相同（文件起点 = 0）：first_pts=0 把音轨前面的空缺补静音，中间断档按时间戳补齐
    p = subprocess.Popen([FF, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", "file:" + f,
                          "-map", "0:a:0", "-vn", "-sn", "-dn", "-af", "aresample=async=1:first_pts=0",
                          "-ac", "1", "-ar", str(SR), "-c:a", "pcm_s16le", "-f", "s16le", "-"],
                         stdout=subprocess.PIPE, stderr=errf)
    PROCS.append(p)
    cur = [None, 0]         # 当前段的文件对象、已写字节

    def finish():
        w, nb = cur
        nb -= nb % 2
        w.truncate(44 + nb)
        w.seek(0)
        w.write(wav_header(nb))
        w.close()
        cur[0], cur[1] = None, 0
        if nb == 0:
            return
        i = len(segs) + 1
        arc = "audio/%04d.wav" % i
        path = os.path.join(work, "%04d.wav" % i)
        tar.add(path, arc)
        os.remove(path)
        n = nb // 2
        segs.append({"file": arc, "offset_ms": total[0] * 1000 // SR, "duration_ms": int(round(n * 1000.0 / SR)),
                     "samples": n, "bytes": 44 + nb})
        total[0] += n

    try:
        while True:
            b = p.stdout.read(1 << 16)
            if not b:
                break
            while b:
                if cur[0] is None:
                    cur[0] = open(os.path.join(work, "%04d.wav" % (len(segs) + 1)), "w+b")
                    cur[0].write(wav_header(0))
                take = min(len(b), seg * 2 - cur[1])
                cur[0].write(b[:take])
                cur[1] += take
                b = b[take:]
                if cur[1] == seg * 2:
                    finish()
        if p.wait():
            die("ffmpeg 转音频失败：" + tail(errf), "ffmpeg")
        if cur[0] is not None:
            finish()        # 最后一段不足一段照切
        idx = {"hash": h, "name": os.path.basename(f), "sample_rate": SR, "channels": 1, "bits": 16,
               "segment_min": seg_min, "segment_ms": seg * 1000 // SR, "duration_ms": int(round(total[0] * 1000.0 / SR)),
               "samples": total[0], "segments": segs}
        tar.add_bytes("index.json", json.dumps(idx, ensure_ascii=False, indent=1).encode())
        tar.close()
    finally:
        if p.poll() is None:
            p.kill()
        errf.close()
        shutil.rmtree(work, ignore_errors=True)

def jpeg_size(path):
    """读 JPEG 的 SOF 段取宽高；读不出返回 (None, None)。"""
    try:
        with open(path, "rb") as r:
            b = r.read(1 << 16)
    except OSError:
        return None, None
    i = 2
    while i + 9 < len(b) and b[i] == 0xFF:
        m = b[i + 1]
        ln = struct.unpack(">H", b[i + 2:i + 4])[0]
        if m in (0xC0, 0xC1, 0xC2):
            hh, ww = struct.unpack(">HH", b[i + 5:i + 9])
            return ww, hh
        i += 2 + ln
    return None, None

def cmd_frames(a):
    usage = "用法：media-frames <sha256> --at 毫秒,毫秒,... [--width 像素]"
    if not a:
        die(usage, "bad_args")
    h, rest, ats, width = a[0], a[1:], None, 960
    while rest:
        if len(rest) < 2:
            die(usage, "bad_args")
        k, v, rest = rest[0], rest[1], rest[2:]
        if k == "--at" and re.fullmatch(r"[0-9]{1,9}(,[0-9]{1,9}){0,499}", v):
            ats = []
            for x in v.split(","):
                if int(x) not in ats:
                    ats.append(int(x))
        elif k == "--width" and re.fullmatch(r"[0-9]{2,4}", v) and 16 <= int(v) <= 7680:
            width = int(v)
        else:
            die(usage + "（--at 至多 500 个非负整数，--width 16..7680）", "bad_args")
    if ats is None:
        die(usage, "bad_args")
    d, f = need(h)
    if FF == "-":
        return
    touch(d)
    busy(d)
    streams, dur = probe(f)
    vs = [s for s in streams if s.get("codec_type") == "video" and not (s.get("disposition") or {}).get("attached_pic")]
    if not vs:
        die("这个文件没有视频流", "no_video")
    vi = vs[0].get("index", 0)
    out_of_range = [t for t in ats if dur is not None and t >= dur]
    todo = [t for t in ats if t not in out_of_range]
    work = tempfile.mkdtemp(prefix=".work-%d-" % os.getpid(), dir=ROOT)

    def grab(t):
        o = os.path.join(work, "%d.jpg" % t)
        r = subprocess.run([FF, "-nostdin", "-hide_banner", "-loglevel", "error", "-ss", "%.3f" % (t / 1000.0),
                            "-i", "file:" + f, "-map", "0:%d" % vi, "-frames:v", "1",
                            "-vf", "scale=w=\x27min(%d,iw)\x27:h=-2" % width, "-q:v", "3",
                            "-f", "image2", "-update", "1", "-y", o], capture_output=True)
        if os.path.isfile(o) and os.path.getsize(o) > 0:
            return t, None
        return t, (tail(r.stderr) or "没截到帧（时间点可能在最后一帧之后）")
    try:
        with ThreadPoolExecutor(max_workers=max(1, min(4, len(todo)))) as ex:
            res = list(ex.map(grab, todo))
        tar, frames, failed = Tar(), [], []
        for t, err in res:
            if err:
                failed.append({"t_ms": t, "error": err})
                continue
            p = os.path.join(work, "%d.jpg" % t)
            ww, hh = jpeg_size(p)
            frames.append({"t_ms": t, "file": "frames/%d.jpg" % t, "bytes": os.path.getsize(p), "width": ww, "height": hh})
            tar.add(p, "frames/%d.jpg" % t)
        idx = {"hash": h, "name": os.path.basename(f), "duration_ms": dur, "max_width": width, "video_stream": vi,
               "frames": frames, "out_of_range": out_of_range, "failed": failed}
        tar.add_bytes("index.json", json.dumps(idx, ensure_ascii=False, indent=1).encode())
        tar.close()
    finally:
        shutil.rmtree(work, ignore_errors=True)

def cmd_clean(a):
    usage = "用法：media-clean <sha256> | --older-than 天"
    sweep()
    deleted, skipped, freed = [], [], 0
    now = time.time()
    if len(a) == 1 and HEX.fullmatch(a[0]):
        d = os.path.join(ROOT, a[0])
        if os.path.isdir(d):
            if in_use(d):
                die("这个文件正在被 media-audio/media-frames 用，稍后再删", "busy")
            freed += dir_bytes(d)
            shutil.rmtree(d, ignore_errors=True)
            deleted.append(a[0])
    elif len(a) == 2 and a[0] == "--older-than" and re.fullmatch(r"[0-9]{1,4}(\.[0-9]{1,3})?", a[1]):
        lim = float(a[1]) * 86400
        for n in sorted(os.listdir(ROOT)):
            d = os.path.join(ROOT, n)
            if not HEX.fullmatch(n) or not os.path.isdir(d) or now - last_used(d) < lim:
                continue
            if in_use(d):
                skipped.append(n)
                continue
            freed += dir_bytes(d)
            shutil.rmtree(d, ignore_errors=True)
            deleted.append(n)
    else:
        die(usage, "bad_args")
    kept = [n for n in os.listdir(ROOT) if HEX.fullmatch(n)]
    say(deleted=deleted, busy=skipped, freed_bytes=freed, kept=len(kept), free_gb=free_gb())

sub, ROOT, FF, FP = sys.argv[1:5]
try:
    {"put": cmd_put, "audio": cmd_audio, "frames": cmd_frames, "clean": cmd_clean}[sub](sys.argv[5:])
finally:
    for _p in BUSY:
        try:
            os.remove(_p)
        except OSError:
            pass
'

# 媒体环境：media.json（push-tool 推到 bin/；本机自测时用入口旁的那份）→ envs/<hash>，没建就建（首次要联网，几分钟）
media_env() {
  local spec="$TH/bin/media.json" s k v
  [ -f "$spec" ] || spec="$(dirname "$0")/media.json"
  [ -f "$spec" ] || fail "测试机上没有 media.json（先 python client.py push-tool）"
  s=$("$PY" -c "$SPEC_PY" "$spec" "" "$TH" 2>"$TH/dl/media-spec-$$.err") \
    || { k=$(tail -2 "$TH/dl/media-spec-$$.err"); rm -f "$TH/dl/media-spec-$$.err"; fail "media.json 不可用：$k"; }
  rm -f "$TH/dl/media-spec-$$.err"
  hash= pyv= reqs=() cpk=()
  while read -r k v; do
    case "$k" in hash) hash="$v";; python) pyv="$v";; pip) reqs+=("$v");; conda) cpk+=("$v");; esac
  done <<<"$s"
  ensure_env
  FF="$E/bin/ffmpeg"; FP="$E/bin/ffprobe"
  [ -x "$FF" ] && [ -x "$FP" ] || fail "媒体环境 $hash 里没有 ffmpeg/ffprobe（media.json 的 conda 要有 ffmpeg）"
}

# media-put/-audio/-frames/-clean：参数校验与实现都在 MEDIA_PY（FF 传 - 时 audio/frames 只校验）；audio/frames 再备好 ffmpeg 环境
a_media() {
  local sub="$1"; shift
  guard_th
  FF=- FP=-
  case "$sub" in audio|frames)     # 先只校验参数与哈希（FF 为 - 时不干活），不对就不必去建 ffmpeg 环境
    env PYTHONUTF8=1 "$PY" -c "$MEDIA_PY" "$sub" "$TH/media" - - "$@" || exit 1
    media_env;;
  esac
  exec env PYTHONUTF8=1 "$PY" -c "$MEDIA_PY" "$sub" "$TH/media" "$FF" "$FP" "$@"
}

# media-spec：stdin 是 media.json（push-tool 在 update 之后推），校验能解析、conda 里有 ffmpeg 再替换 bin/media.json
a_media_spec() {
  local tmp="$TH/bin/media.json.incoming-$$" s m
  cat >"$tmp"
  s=$("$PY" -c "$SPEC_PY" "$tmp" "" "$TH" 2>"$tmp.err") \
    || { m=$(tail -2 "$tmp.err"); rm -f "$tmp" "$tmp.err"; fail "media.json 不可用：$m"; }
  rm -f "$tmp.err"
  grep -qE '^conda ffmpeg([=<>!]|$)' <<<"$s" || { rm -f "$tmp"; fail "media.json 的 conda 里要有 ffmpeg"; }
  mv "$tmp" "$TH/bin/media.json"
  printf '{"success":true,"env":"%s","message":"media.json 已更新"}\n' "$(awk '$1 == "hash" { print $2 }' <<<"$s")"
}

case "${ARGS[0]:-}" in
  ping) a_ping;;
  update) a_update;;
  inventory) a_inventory;;
  sync) a_sync "${ARGS[@]:1}";;
  snapshot) a_snapshot "${ARGS[@]:1}";;
  prepare) a_prepare "${ARGS[@]:1}";;
  run) a_run "${ARGS[@]:1}";;
  status) a_status "${ARGS[@]:1}";;
  fetch) a_fetch "${ARGS[@]:1}";;
  fetchx) a_fetchx "${ARGS[@]:1}";;
  plugin) a_plugin "${ARGS[@]:1}";;
  cancel) a_cancel "${ARGS[@]:1}";;
  clean) a_clean "${ARGS[@]:1}";;
  media-put) a_media put "${ARGS[@]:1}";;
  media-audio) a_media audio "${ARGS[@]:1}";;
  media-frames) a_media frames "${ARGS[@]:1}";;
  media-clean) a_media clean "${ARGS[@]:1}";;
  media-spec) a_media_spec;;
  *) fail "不认识的动作：${ARGS[0]:-（空）}"; ;;
esac
