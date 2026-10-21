# macexec v2 约定（10/16 用户要求收缩范围：只做够用的，不做边角）

目的：Ubuntu 上的 Claude 能在 Mac 上 编译 / 自检 / 打包 / 装上并重启 XXYI / 截图 / 看日志 / 看 git 状态。
内部工具，只在用户打开「协助」时可用，调用方只有总指挥。**不做**：任务持久化、重启恢复、日志/记录落盘（要留什么由调用方自己存）、job 轮询、写文件、任意命令。

## Go 包 go/macexec
- `paths.go`（已有，保留不动）：RepoRoot、ResolveInRepo、DefaultRepos。
- `runner.go`（重写，替换现有全部内容；runner_unix.go/runner_other.go 只放进程组相关的几行）：
  - `type Spec struct { Action, Dir string; Argv []string; Timeout time.Duration }`
  - `type Result struct { ExitCode int; TimedOut bool; Seconds float64; Tail string }`
  - `var ErrBusy = errors.New("Mac 上正有任务在跑")`
  - `func Run(ctx context.Context, s Spec) (Result, error)`：同步执行；全局 sync.Mutex.TryLock 保证同一时间只一个，拿不到返回 ErrBusy；
    exec.CommandContext、不经 shell、Setpgid，超时或 ctx 取消时杀整个进程组；cmd.WaitDelay = 5s；
    stdout+stderr 合并进内存，只保留最后 64KB（简单环形或截断即可），Tail 返回它。
- `actions.go`：`BuildSpec(action string, p Params, home string) (Spec, error)`，动作：
  `git_status`（`git status -b --porcelain=v1`，仓库根，30s）、
  `swift_build`（xxyi/mac，15min）、`selftest`（`swift run xxyi-selftest`，xxyi/mac，15min）、
  `node_test`（`node tests/js/<name>.test.js`，xxyi 根，name 只许 [a-z0-9_]+ 且解析后是普通文件，5min）、
  `build_app`（`<解析后的 mac/build.sh 绝对路径>`，xxyi/mac，20min）。
  工作目录用 ResolveInRepo(root, "mac") 取；build.sh 用 ResolveInRepo 解析并要求普通文件。

## HTTP（Mac 端，support/macexec.go，挂在 remoteMux）
- `POST /exec/run {action, repo, name, branch}` → `{exit_code, timed_out, seconds, tail}`（同步，忙 409，参数错 400）。
  - `branch` 可选，只对 swift_build/selftest/build_app/node_test 有意义；git_status 带 branch 报 400。空 = 编主工作区当前分支（与以前一致）。
  - 非空时：ValidBranch 校验 → 持 `syncLocks["xxyi"]`（与 /sync 同锁）在 xxyi 仓库 `git fetch --no-tags -- <Remote> refs/heads/<branch>`（不要求工作区干净，不 checkout、不动本地分支）→ 专用 worktree `<xxyi 仓库根的父目录>/.xxyi-branch-build`（Mac 上 ~/xxyi-work/.xxyi-branch-build，单槽位反复复用，保留 .build）：不存在则 `git worktree add --detach`，已是该仓库 worktree 则 `checkout --detach --force` + `clean -fd`（不带 -x），被普通目录占着先 `worktree prune`，仍不对报 502 不 rm。动作在 `<worktree>/mac`（node_test 在 worktree 根）里跑，命令/超时不变；`macexec.Params.RootOverride` 承载这个根。
  - tail 最前面加一行 `分支 <branch> @ <提交前12位>，目录 <worktree>`；build_app 产物在 `<worktree>/mac/build/XXYI.app`。
  - 专用 worktree 同一时间只服务一个请求（准备+动作整段持有），拿不到按忙 409；fetch/worktree 失败 502（文字写明哪一步）。
- `GET /exec/logs?name=service|wgtund&lines=N` → `{text}`（N ≤ 500；路径照现有代码查清楚）。
- `GET /exec/screenshot` → image/png（screencapture -x 整屏，sips -Z 1600 缩小，临时文件用完删；非 darwin 501）。
- `POST /exec/install` → `{started: true}`：把一段 Go 里写死的脚本写到临时文件，Setpgid 起 /bin/sh 执行后立即返回；
  脚本：校验 build/XXYI.app 签名 → 退出 XXYI → pkill 残留服务与 wgtund → 直接打开打包目录里的新版（不覆盖桌面 App）。不另存日志文件（调用方用截图和 /exec/logs 看结果）。
- 每次调用单独记一行到 `~/.xxyi/macexec.log`（时间、接口/动作、仓库、结果：退出码/超时/忙/参数错，**不记输出内容**）；**不写进协助日志**（不调 logEvent）。文件超过 1MB 时改名为 macexec.log.1 再新建（只留这一份旧的）。

## MCP（Ubuntu 端，cmd/xxyi-msg/macexec.go，重写替换现有内容）
工具：`mac_git_status{repo}`、`mac_build{branch?}`、`mac_selftest{branch?}`、`mac_node_test{name,branch?}`、`mac_build_app{branch?}`、`mac_install_app{}`、`mac_screenshot{}`（返回 MCP image）、`mac_logs{name,lines}`。
每次调用重新读入口（复用现有 connect）；唯一的重试例外：401 时重读入口再试一次（XXYI 重启后令牌会换）；HTTP 超时 = 动作超时 + 1 分钟（截图/日志 60s）；结果按文字返回 exit_code/用时/尾部输出。不轮询、不重试。
