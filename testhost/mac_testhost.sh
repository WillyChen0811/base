#!/bin/bash
# 测试机入口（Mac 端，bash 3.2 兼容）：只由 authorized_keys 的 command= 调用，动作取自 $SSH_ORIGINAL_COMMAND。
# 经 update 动作推送到 ~/xxyi-testhost/bin/testhost。固定动作，不执行任意命令。
# 本机自测：TESTHOST_HOME 指向假目录；TESTHOST_ENV_STUB 指向已有 conda 环境前缀，prepare 不再建环境。
TH="${TESTHOST_HOME:-$HOME/xxyi-testhost}"
export PATH="$TH/miniforge/bin:${PATH:-/usr/bin:/bin}:/usr/bin:/bin:/usr/sbin:/sbin"
PY="$TH/miniforge/bin/python"; [ -x "$PY" ] || PY=python3
CONDA="$TH/miniforge/bin/conda"; [ -x "$CONDA" ] || CONDA=conda
VERSION=1
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

# 环境描述解析：输出 hash/python/pip/pp(pythonpath)/env 行；hash 只看 python 与 pip，换依赖才重建
SPEC_PY='
import hashlib, json, os, sys
f, run, th = sys.argv[1:4]
snaps = dict(a.split("=", 1) for a in sys.argv[4:])
d = json.load(open(f))
pipl = [str(x) for x in d.get("pip", [])]
for x in ("pytest", "pytest-xdist"):
    if not any(p.lower().startswith(x) for p in pipl): pipl.append(x)
py = str(d.get("python", "3.12"))
h = hashlib.sha256(json.dumps([py, sorted(pipl)]).encode()).hexdigest()[:12]
print("hash", h); print("python", py)
for p in pipl: print("pip", p)
for p in d.get("pythonpath", []): print("pp", p)
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
  local R="$TH/runs/$rid" envrel=cmdproto/testenv.json t0=$SECONDS first= spec x repo commit
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
  saferel "$envrel" || fail "--env 路径不合法"
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
  [ -f "$R/src/$first/$envrel" ] || fail "$first 里没有环境描述 $envrel"
  spec=$("$PY" -c "$SPEC_PY" "$R/src/$first/$envrel" "$R" "$TH" ${snaps[@]+"${snaps[@]}"} 2>"$R/spec.err") \
    || fail "环境描述不可用：$(tail -2 "$R/spec.err")"
  local hash pyv reqs=() pps= envs=()
  while read -r k v; do
    case "$k" in hash) hash="$v";; python) pyv="$v";; pip) reqs+=("$v");; pp) pps="$pps$v"$'\n';; env) envs+=("$v");; esac
  done <<<"$spec"
  printf '%s' "$pps" >"$R/pythonpath"
  { printf '%s\n' ${envs[@]+"${envs[@]}"}; } >"$R/envvars"
  echo "$hash" >"$R/envhash"

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
        { "$CONDA" create -y -q -p "$E" "python=$pyv" pip \
          && "$E/bin/python" -m pip install -q --disable-pip-version-check ${reqs[@]+"${reqs[@]}"}; } >"$lg" 2>&1 \
          || { rm -rf "$E"; fail "建环境失败（日志 ${lg}）：$(tail -3 "$lg")"; }
        touch "$E.ok"
      fi
    fi
  fi
  printf '{"success":true,"run_id":"%s","env":"%s","env_reused":%s,"repos":{%s},"seconds":%s}\n' \
    "$rid" "$hash" "$reused" "$res" $((SECONDS - t0))
}

# run <run_id> <工作子目录> [-n N] [-k 表达式] [-m 标记] <测试文件...>：后台跑，立即返回
a_run() {
  local rid="${1:-}" wd="${2:-}"; safe "$rid" || fail "run_id 不合法"; saferel "$wd" || fail "工作子目录不合法"
  shift 2
  local R="$TH/runs/$rid" n= k= m= files=()
  [ -d "$R/src/$wd" ] || fail "没有 $rid/src/${wd}，先 prepare"
  while [ $# -gt 0 ]; do
    case "$1" in
      -n) isint "${2:-}" && [ "$2" -ge 1 ] && [ "$2" -le 64 ] || fail "-n 要 1..64"; n="$2"; shift 2;;
      -k) k="${2:-}"; case "$k" in ''|-*) fail "-k 值不合法";; esac; shift 2;;
      -m) m="${2:-}"; case "$m" in ''|-*) fail "-m 值不合法";; esac; shift 2;;
      -*) fail "不许的选项：${1}（只放行 -n -k -m）";;
      *) saferel "$1" || fail "测试路径不合法：$1"; files+=("$1"); shift;;
    esac
  done
  [ ${#files[@]} -gt 0 ] || fail "没有测试文件"
  [ -e "$R/out/exit" ] || ! alive "$R" || fail "这个 run 已经在跑"
  rm -f "$R"/out/*
  local E="$TH/envs/$(cat "$R/envhash")" pp= p q
  while read -r p; do
    [ -n "$p" ] || continue
    q=$(cd "$R/src/$wd/$p" 2>/dev/null && pwd -P) || fail "pythonpath 不存在：$p"
    case "$q" in "$R"/src/*) pp="$pp${pp:+:}$q";; *) fail "pythonpath 逃出了 run 目录：$p";; esac
  done <"$R/pythonpath"
  local ev=(HOME="$R/home" TMPDIR="$R/tmp" CMDPROTO_RAMDISK_ROOT="$R/ramdisk" PYTHONPATH="$pp"
            PATH="$E/bin:$PATH" PYTHONUNBUFFERED=1 PYTHONUTF8=1 TESTHOST_RUN_ID="$rid")
  while IFS= read -r p; do [ -n "$p" ] && ev+=("$p"); done <"$R/envvars"
  local cmd=("$E/bin/python" -m pytest -p no:cacheprovider -v -rfE --junitxml="$R/out/junit.xml")
  [ -n "$n" ] && [ "$n" -gt 1 ] && cmd+=(-n "$n")
  [ -n "$k" ] && cmd+=(-k "$k")
  [ -n "$m" ] && cmd+=(-m "$m")
  cmd+=("${files[@]}")
  date +%s >"$R/out/start"
  ( cd "$R/src/$wd" && nohup env -i "${ev[@]}" bash -c 'rc=0; "$@" || rc=$?; date +%s >"$0.end"; echo $rc >"$0"' \
      "$R/out/exit" "${cmd[@]}" >"$R/out/log" 2>&1 </dev/null & echo $! >"$R/out/pid" )
  printf '{"success":true,"run_id":"%s","started":true,"workers":%s}\n' "$rid" "${n:-1}"
}

count_log() { local c; c=$(grep -cE " $1( |\$)" "$R/out/log" 2>/dev/null); echo "${c:-0}"; }

a_status() {
  local rid="${1:-}"; safe "$rid" || fail "run_id 不合法"
  R="$TH/runs/$rid"; [ -d "$R/out" ] || fail "没有这个 run"
  local state=prepared ex=null now st=0 en p f s pct
  if [ -e "$R/out/exit" ]; then state=finished; ex=$(cat "$R/out/exit")
  elif alive "$R"; then state=running
  elif [ -e "$R/out/pid" ]; then state=dead; fi
  now=$(date +%s); st=$(cat "$R/out/start" 2>/dev/null || echo "$now"); en=$(cat "$R/out/exit.end" 2>/dev/null || echo "$now")
  p=$(count_log PASSED); f=$(count_log FAILED); s=$(count_log SKIPPED)
  pct=$(grep -oE '\[ *[0-9]+%\]' "$R/out/log" 2>/dev/null | tail -1 | tr -dc '0-9')
  printf '{"success":true,"run_id":"%s","state":"%s","finished":%s,"exit":%s,"passed":%s,"failed":%s,"skipped":%s,"percent":%s,"elapsed":%s}\n' \
    "$rid" "$state" "$([ "$state" = running ] || [ "$state" = prepared ] && echo false || echo true)" "$ex" \
    "$p" "${f:-0}" "$s" "${pct:-0}" $((en - st))
}

# fetch <run_id>：junit、日志尾部、退出码打成 tar 输出
a_fetch() {
  safe "${1:-}" || fail "run_id 不合法"
  local o="$TH/runs/$1/out"; [ -d "$o" ] || fail "没有这个 run"
  tail -n 400 "$o/log" >"$o/log.tail" 2>/dev/null
  local fs=() x; for x in junit.xml log.tail exit exit.end start; do [ -e "$o/$x" ] && fs+=("$x"); done
  tar -c -C "$o" -f - "${fs[@]}"
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
  clean) a_clean "${ARGS[@]:1}";;
  *) fail "不认识的动作：${ARGS[0]:-（空）}"; ;;
esac
