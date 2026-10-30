#!/bin/bash
# 测试机入口（Mac 端，bash 3.2 兼容）：只由 authorized_keys 的 command= 调用，动作取自 $SSH_ORIGINAL_COMMAND。
# 经 update 动作推送到 ~/xxyi-testhost/bin/testhost。固定动作，不执行任意命令。
# 本机自测：TESTHOST_HOME 指向假目录；TESTHOST_ENV_STUB 指向已有 conda 环境前缀，prepare 不再建环境。
TH="${TESTHOST_HOME:-$HOME/xxyi-testhost}"
export PATH="$TH/miniforge/bin:${PATH:-/usr/bin:/bin}:/usr/bin:/bin:/usr/sbin:/sbin"
PY="$TH/miniforge/bin/python"; [ -x "$PY" ] || PY=python3
CONDA="$TH/miniforge/bin/conda"; [ -x "$CONDA" ] || CONDA=conda
VERSION=4
mkdir -p "$TH"/{bin,repos,snapshots,envs,runs,dl} 2>/dev/null

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
  printf '{"success":true,"host":"%s","user":"%s","macos":"%s","arch":"%s","ncpu":%s,"mem_gb":%s,"load":"%s","python":"%s","node":"%s","running":%s,"idle":%s,"testhost":"v%s"}\n' \
    "$(jstr "$(host_name)")" "$(id -un)" "$(jstr "$(os_ver)")" "$(uname -m)" "$(ncpu)" "$(mem_gb)" "$(load_avg)" \
    "$("$PY" -V 2>&1 | awk '{print $2}')" "$(node -v 2>/dev/null)" "$r" "$([ "$r" -eq 0 ] && echo true || echo false)" "$VERSION"
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

  local E="$TH/envs/$hash" reused=true lock="$TH/envs/.lock-$hash" i=0
  if ! { [ -e "$E.ok" ] && [ -x "$E/bin/python" ]; }; then
    reused=false
    if [ -n "${TESTHOST_ENV_STUB:-}" ]; then
      ln -sfn "$TESTHOST_ENV_STUB" "$E"; touch "$E.ok"
    else
      until mkdir "$lock" 2>/dev/null; do i=$((i + 1)); [ $i -gt 900 ] && fail "等环境锁超时"; sleep 2; done
      trap 'rmdir "$lock" 2>/dev/null' EXIT
      if [ -e "$E.ok" ] && [ -x "$E/bin/python" ]; then reused=true; else
        rm -rf "$E" "$E.ok"; local lg="$TH/dl/env-$hash.log"
        { "$CONDA" create -y -q -p "$E" "python=$pyv" pip ${cpk[@]+"${cpk[@]}"} \
          && "$E/bin/python" -m pip install -q --disable-pip-version-check ${reqs[@]+"${reqs[@]}"}; } >"$lg" 2>&1 \
          || { rm -rf "$E"; fail "建环境失败（日志 ${lg}）：$(tail -3 "$lg")"; }
        touch "$E.ok"
      fi
    fi
  fi
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
  *) fail "不认识的动作：${ARGS[0]:-（空）}"; ;;
esac
