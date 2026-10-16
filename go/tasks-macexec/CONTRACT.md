# macexec 接口约定（各任务都按这份写，别自己改名字）

## Go 包 `go/macexec`（新建，纯逻辑，不依赖 HTTP，不依赖 support 包）
- `paths.go`
  - `func RepoRoot(home, name string, allowed []string) (string, error)`：只认 allowed 里的名字，返回 `<home>/xxyi-work/<name>`（目录须存在）。
  - `func ResolveInRepo(root, rel string) (string, error)`：拒绝空、绝对路径、含 `..` 段、含 `.git` 段；`filepath.EvalSymlinks` 之后仍须在 root（同样 EvalSymlinks 过）之内；返回绝对路径。
  - `var DefaultRepos = []string{"xxyi", "base", "websubmit", "ai-browser"}`
- `runner.go`
  - `type Spec struct { Action, Dir string; Argv []string; Env []string; Timeout time.Duration }`
  - `type Job struct { ID, Action, State string; ExitCode int; Started, Ended time.Time; Note string }`，State ∈ running|done|failed|timeout
  - `func NewRunner(dir string, keep int) (*Runner, error)`：dir 存 `<id>.log`（完整输出）与 `<id>.json`（Job）；只保留最近 keep 个 job 的文件。
  - `func (r *Runner) Start(s Spec) (Job, error)`：已有 running 的返回 `ErrBusy`；用 exec.CommandContext（不经 shell），stdout+stderr 写 log；结束更新 json。
  - `func (r *Runner) Get(id string) (Job, error)`：内存没有就读 json（进程重启后也能查）。
  - `func Tail(path string, lines, maxBytes int) (string, error)`
  - `var ErrBusy = errors.New("Mac 上正有任务在跑")`
- `actions.go`
  - `type Params struct { Repo, Name string; Lines int }`
  - `func BuildSpec(action string, p Params, home string) (Spec, error)`：固定命令——
    `git_status`: `git status --porcelain=v1 -b`（Dir=仓库根，30s）；
    `swift_build`: `swift build`（Dir=xxyi/mac，15min）；`selftest`: `swift run xxyi-selftest`（xxyi/mac，15min）；
    `node_test`: `node tests/js/<Name>.test.js`（Dir=xxyi 根；Name 只许 `[a-z0-9_]+` 且文件存在，5min）；
    `build_app`: `./build.sh`（xxyi/mac，20min）。未知动作返回错误。

## HTTP（wgtund 协助远端接口 remoteMux，`support/macexec.go`）
- `POST /exec/start {action, repo, name}` → `{job: Job}`；忙时 409。
- `GET /exec/job?id=` → `{job: Job, tail: string}`（tail ≤200 行 / 64KB）。
- `POST /exec/read {repo, path, offset, limit}` → `{text, lines_total}`（按行，≤256KB，非 UTF-8 报 400）。
- `GET /exec/logs?name=&lines=` → `{text}`；name ∈ service|wgtund|ui，lines ≤500。
- `GET /exec/screenshot?target=screen|xxyi` → `image/png`（≤1600 像素宽）。
- 每次调用 `s.logEvent("exec", …)`；runner 目录 `~/.xxyi/macexec`，keep=50。

## MCP（Ubuntu 端 `cmd/xxyi-msg`，经现有远端地址与令牌）
`mac_git_status{repo}`、`mac_read_file{repo,path,offset,limit}`、`mac_logs{name,lines}`、`mac_job{id}`、
`mac_build{}`、`mac_selftest{}`、`mac_node_test{name}`、`mac_build_app{}`、`mac_screenshot{target}`（返回 MCP image 内容）。
长任务工具内部每 5 秒查一次 job，最多等 9 分钟；没完就返回 job id 让调用方用 mac_job 接着查。
