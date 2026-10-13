# gobase

纯 Go 的基础模块，可交叉编译到 macOS 与 iOS，不用 cgo。

## 模块
- `wgtunnel`：应用内 WireGuard。读 wg-quick 格式配置，用 wireguard-go 的程序内网络栈起隧道，
  不动系统路由、不要 root；一个进程里可同时开多条（网段可以重叠）。提供经隧道的 Dial/Listen 与握手统计。
- `wgtunnel/socks`：只监听回环地址的 SOCKS5 口（只支持 CONNECT），域名交给隧道那一侧解析。
- `sshterm`：经任意 Dialer 的 SSH 终端（PTY、窗口大小、退出码），以及把终端桥接到 WebSocket 的 HTTP Handler。
- `wgtund` + `cmd/wgtund`：助手程序，把上面三块组合起来给桌面应用用（见下）。

## wgtund 助手程序
    wgtund -dir <配置目录>

配置目录：
- `tunnels/<名字>.conf`：每个文件一条隧道（wg-quick 格式，权限建议 600）。
- `hosts.json`：
  ```json
  {"known_hosts": "~/.ssh/known_hosts",
   "allowed_origins": ["127.0.0.1:8765"],
   "socks": {"home": "127.0.0.1:1080"},
   "hosts": [{"name": "ubuntu", "tunnel": "home", "addr": "192.168.65.2:22", "user": "chenwy",
              "command": "tmux new -A -s main", "identity_files": ["~/.ssh/id_ed25519"]}]}
  ```
  `tunnel` 为空表示直连不走隧道；`allowed_origins` 是允许连终端的网页来源（主机:端口）；
  私钥也可以只放在 ssh-agent 里。

就绪后往 stdout 写一行 `{"http":"127.0.0.1:端口","token":"…","socks":{…}}`；stdin 关闭即退出。
HTTP 接口都要带 `token` 参数；每台主机共用一条 SSH 连接（第一次用到才拨，断了自动重拨）：
- `GET /hosts`：主机清单，每项 `{"name","tunnel","connected","error"}`；
  `connected` 是共用连接当前是否连着，`error` 是最近一次拨号/断开的原因（没有则空串）。
- `GET /status`：隧道握手与流量统计。
- `GET /sessions?host=`：列 tmux 会话 `{"sessions":[{"name","attached","windows","created","activity"}]}`
  （时间是 Unix 秒；远端没有 tmux 服务时返回空数组）。
- `POST /sessions/new?host=&name=`：新建会话，成功 `{"name":…}`；名字不合规 400、重名 409。
- `POST /sessions/kill?host=&name=`：结束会话，成功 `{"ok":true}`；不存在 404。
- `GET /capture?host=&name=&lines=`：抓会话内容 `{"text":…}`；lines 默认 200（1..5000）。
- WebSocket `/term?host=&cols=&rows=[&session=名]`：网页终端；带 `session` 时 attach 该 tmux 会话，
  否则执行主机配置的 `command`（空则登录 shell）。
写操作走 POST 但不带请求体和自定义头，浏览器跨域时属于简单请求、不发预检。
错误统一返回 `{"error":"原因"}`：参数不合规 400、主机/会话不存在 404、重名 409、方法不对 405、
连不上主机或远端执行失败 502。

### 协助服务（可选）
`hosts.json` 里加 `support` 段才启用；没有这段，下面的接口全部返回 404 `{"error":"没有配置协助"}`。
让远端主机上的 Claude 借本机做测试：本机主动经共用 SSH 连接在远端开只绑 127.0.0.1 的入口，
入口地址和令牌写到远端 `~/.xxyi-support/endpoint.json`（权限 600）。
  ```json
  "support": {
    "host": "ubuntu",
    "claude_bin": "claude",
    "workdir": "~/xxyi-work",
    "claude_settings": "~/.xxyi/support-claude.json",
    "claude_tools": "Bash,Read,Edit,Write,Glob,Grep",
    "claude_args": ["--model", "sonnet", "--max-budget-usd", "5"],
    "repos": {"xxyi": {"dir": "~/Documents/codes/xxyi", "remote": "ssh://chenwy@10.0.0.2/home/chenwy/xxyi"}},
    "git_tunnel": "home"
  }
  ```
- `host`（必填，须在 hosts 里）、`workdir`（必填，须是已存在的目录）、`claude_settings`（必填）、
  `repos`（每项 `dir`、`remote` 都不能空）；`claude_bin` 默认 `claude`；`claude_args` 追加给 Claude 的参数。
- `git_tunnel`：可选，git 同步经这条隧道的 SOCKS 口走 SSH（须在 `socks` 里有同名条目；
  用 `support.host` 第一个私钥，严格校验 known_hosts）。
- `claude_tools`：可选，协助 Claude 可用的工具（逗号分隔，仅字母），默认 `Bash,Read,Edit,Write,Glob,Grep`。
- 不合规时 wgtund 启动失败，错误以 `hosts.json: support…` 开头。

安全要求（启动时检查）：
- `claude_settings` 必须是 JSON 对象，且 `sandbox.enabled` 为 `true`、`sandbox.allowUnsandboxedCommands` 为 `false`、
  `sandbox.failIfUnavailable` 为 `true`；`permissions.defaultMode` 若有，只能是 `default` 或 `manual`。
- `sandbox.autoAllowBashIfSandboxed` 必须显式写出（布尔值），两种选择的含义：
  `true` = 沙盒内的命令不经你批准直接运行（只能写工作目录和仓库、网络受限），不会触发授权钩子；
  `false` = 每条命令都弹给你批准。
- 禁止削弱沙盒、让沙盒内连回本机服务（否则 Claude 可能拿到主令牌自己批准）：
  `sandbox.network.allowLocalBinding`、`sandbox.network.allowAllUnixSockets`、`sandbox.enableWeakerNetworkIsolation`、
  `sandbox.enableWeakerNestedSandbox`、`sandbox.filesystem.disabled` 不许为 `true`；
  `sandbox.network.allowUnixSockets` 必须为空；`sandbox.network.allowedDomains` 里不许有
  `localhost`、`*.localhost`、`127.*`、`::1`、`0.0.0.0`（去掉端口、不分大小写）。
  另外 `disableAllHooks`、`allowManagedHooksOnly` 不许为 `true`（合并设置时也会强制 `disableAllHooks=false`、删掉 `allowManagedHooksOnly`）。
- 自动追加 `sandbox.filesystem.denyRead`（系统级禁读，保留你原有的条目、去重）：`~/.ssh`、wgtund 配置目录、
  `claude_settings` 所在目录、wgtund 可执行文件所在目录。
- 目录隔离：配置目录、`claude_settings` 所在目录、wgtund 所在目录，都必须与 `workdir` 以及每个仓库 `dir`
  互不包含（符号链接解开后按路径分段比较）。原因：Claude 能写工作目录和仓库，若它们包含配置就能改掉钩子。
  每个仓库的 `dir` 必须存在且是 git 仓库顶层，两个仓库名不能指向同一目录。
- `claude_args` 是白名单：只允许 `--model`、`--fallback-model`、`--effort`、`--max-budget-usd`、`--max-turns`、
  `--append-system-prompt`，每个都必须带值（`--x 值` 或 `--x=值`，值不能以 `-` 开头）；出现任何别的参数
  （包括不认识的和位置参数）都启动失败。权限、工具、目录等由 wgtund 固定给出。
- 协助的 Claude 运行在受限模式：启动参数固定为 `--restricted --permission-prompts none --strict-mcp-config
  --tools <claude_tools> --settings <合并文件>`，并为每个仓库加 `--add-dir <dir>`（按仓库名排序），再接 `claude_args`。
  因此只读协助设置文件，不受用户全局/项目/本地设置（allow 规则、MCP 服务器等）影响；
  文件工具只能访问 `workdir` 和各仓库目录；只有 `claude_tools` 点名的工具可用；
  `--permission-prompts none` 下授权请求仍会触发 `PermissionRequest` 钩子，由钩子交给你批准。
  Claude 子进程只继承白名单环境变量（`PATH HOME USER LOGNAME SHELL TMPDIR LANG TERM TZ`、`LC_*`、`ANTHROPIC_*`、
  `CLAUDE_*`、各种 `*_proxy`），其余不传。
- 每次启动，wgtund 把 `claude_settings` 复制一份、覆盖其中的 `hooks.PermissionRequest`（指向下面的钩子子命令，
  失败即拒绝），写到配置目录的 `support-claude.merged.json`（权限 600），Claude 用 `--settings` 读它；其余设置保留。
  审计日志在配置目录的 `support-log.jsonl`。
- 远端 sshd 的 `GatewayPorts` 必须是 `no` 或 `clientspecified`：开监听后 wgtund 会检查实际地址是回环，否则报错并不启动协助。
- 远端 `endpoint.json` 内容为 `{"addr","token","until"}`，`until` 是截止时间（Unix 秒，延长时会重写）；
  `macsup` 发现已过期会直接报「Mac 上的协助已到期」，不发请求。
- 待批请求最多 50 条，超出的钩子请求直接拒绝；Claude 正在工作时 `/sync` 返回 409。

本机接口（同样要主令牌和 CORS 规则）：
- `GET /support`：状态与最近日志。
- `POST /support/start?minutes=N`（1..240）、`POST /support/stop`、`POST /support/decide?id=&allow=1|0[&message=]`：
  除主令牌外，还要求请求头 `Origin` 存在且在 `allowed_origins` 里，否则 403「需要从 xxyi 页面操作」（`GET /support` 不要求）。
- `POST /support/hook?token=<钩子令牌>`：不走主令牌，由协助服务自己校验钩子令牌；只供下面的子命令使用。

钩子子命令：`wgtund support-hook [http://127.0.0.1:端口/support/hook?token=…]`
（地址缺省取环境变量 `XXYI_HOOK_URL`；只接受本机地址）。从标准输入读钩子 JSON（最多 1MB）转给本机服务，
原样输出其回答；任何失败都输出「拒绝」并以退出码 0 结束，让 Claude Code 按拒绝处理。

## 测试
    cd go && go test ./...
