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

运行日志写到 `<配置目录>/wgtund.log`（同时输出 stderr），每行带本地时间；按天轮转为 `wgtund-YYYY-MM-DD.log`，保留 3 天，单文件超 20MB 也会轮转（同日加序号）。日志不含令牌、密钥、密码和终端内容。

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
- 多线路（可选）：同一台主机有多条路能到时，用 `routes` 代替 `tunnel`/`addr`（两种写法只能选一种，否则启动失败）：
  ```json
  {"name": "ubuntu", "user": "chenwy", "identity_files": ["~/.ssh/id_ed25519"],
   "routes": [{"label": "德国", "tunnel": "de",   "addr": "192.168.150.3:22"},
              {"label": "家里", "tunnel": "home", "addr": "192.168.65.2:22"}]}
  ```
  列表第一条是首选，其余是备用；wgtund 每次启动都从首选开始（不记忆上次用哪条）。`label` 可省略（缺省用隧道名，
  隧道名也空则显示「直连」）；每条线路的 `addr` 要写成 主机:端口，`tunnel` 非空时必须是存在的隧道。
  首选连不上（拨号 15 秒超时仍失败）时**不会自动切换**，只在状态里标出 `preferred_down`，由用户在页面上同意后才切到备用；
  走备用期间，wgtund 每 30 秒在后台探测一次首选的 TCP 端口，通了只标出 `preferred_back`，是否切回同样由用户决定。
  不写 `routes` 的旧配置等于只有一条线路，行为不变。
- 端口转发（可选）：`forwards` 把本机端口上的访问转到远端地址（比如 omlx 访问 Studio 上的 omlx HTTP 服务）：
  ```json
  "forwards": [{"name": "omlx", "listen": "127.0.0.1:18192",
               "routes": [{"label": "内网", "tunnel": "", "addr": "192.168.147.196:8192"},
                          {"label": "家里（程序）", "tunnel": "home", "addr": "192.168.65.4:8192"}]}]
  ```
  `listen` 必须是 127.0.0.1 或 localhost 的 主机:端口（不许监听别的地址）；`name` 不能为空、不能互相重复、不能与主机重名；
  `routes` 写法与主机多线路相同（第一条是首选，`tunnel` 空为直连）。每个进来的连接拨到当前线路的 `addr`（超时 15 秒）再双向拷贝；
  线路规则和主机一样：启动从首选开始、连不通只标 `preferred_down` 不自动切换，走备用期间探测首选标 `preferred_back`。
  监听失败（端口被占等）不会让 wgtund 启动失败：该转发在 `/forwards` 的 `error` 写原因（如「监听 127.0.0.1:18770 失败：address already in use」），
  后台每 10 秒重试监听，成功后清掉；运行中监听异常退出同样处理。配置写错（`listen` 非法、`via` 指向不存在的主机等）仍启动失败。
  线路还可以写 `via`（hosts 里的主机名）：经该主机已有的共用 SSH 连接开 direct-tcpip 通道（等同 `ssh -L`）连 `addr`，
  `addr` 是从那台主机上看的地址，不另拨 SSH；`via` 与 `tunnel` 不能同写，主机必须存在（否则启动失败），`label` 缺省显示「经 <主机名>」：
  ```json
  {"name": "chat", "listen": "127.0.0.1:18770", "routes": [{"via": "ubuntu", "addr": "127.0.0.1:8770"}]}
  ```
  适合远端服务只听 127.0.0.1 的情况，流量全程走 SSH 加密。SSH 断开重连后新进来的连接用新连接，在途的随旧连接结束；
  `/forwards` 的线路项多带 `via` 字段（未用时省略）。
- 程序级接管（可选）：顶层 `proxy` 在回环上开 SOCKS5 口（无认证，支持 CONNECT 和 UDP ASSOCIATE；IPv4/域名），程序照常写真实地址，
  由 wgtund 按线路选路。转发的 `target`（数组，每条是 IP、CIDR 或域名，可带 `:端口`）是匹配规则，命中多条取最具体的
  （精确 IP > 长前缀 > 短前缀，带端口的优先）；命中后用该转发的当前线路拨到请求的真实目的（不是 route 里的 addr）。
  没命中的走 `proxy_default`：`direct`（缺省，流量计入 `/forwards` 里名为「其他（直连）」的项）或某条转发的名字。
  `listen` 可省（只给 `target` 的转发不开本机口）；`listen` 的端口转发仍拨 route 的 addr。
  域名不预先解析：直连在本机解析，隧道线路用该隧道 conf 的 `DNS=` 在隧道内解析（没配则失败并在转发 `error` 写明，不回落本机），
  `via` 线路原样交给 SSH 对端解析。代理口地址在启动信息和 `/status` 的 `proxy` 字段。
  UDP（SOCKS5 UDP ASSOCIATE，`proxy_udp.go`）：每条控制连接开一个回环 UDP 中继口，客户端数据报带标准 UDP 头（FRAG≠0 丢弃），
  规则选转发与 TCP 相同；隧道线路由隧道 netstack 发（域名用隧道 DNS，没配则丢包并记 `error`），直连本机发，
  `via` 线路带不了 UDP（丢包，`error` 写「经 SSH 的线路不支持 UDP」）。出站套接字按「客户端源地址+目的」复用、空闲 60 秒回收，
  控制连接断开即关中继口；UDP 流量计入对应转发（或「其他（直连）」）。
  ```json
  "proxy": "127.0.0.1:1080", "proxy_default": "direct",
  "forwards": [{"name": "smb", "target": ["10.116.7.250:445", "10.116.0.0/16"],
               "routes": [{"label": "内网", "addr": "10.116.7.250:445"},
                          {"label": "家里", "tunnel": "home", "addr": "10.116.7.250:445"}]}]
  ```

就绪后往 stdout 写一行 `{"http":"127.0.0.1:端口","token":"…","socks":{…}}`；stdin 关闭即退出。
HTTP 接口都要带 `token` 参数；每台主机共用一条 SSH 连接（第一次用到才拨，断了自动重拨）：
- `GET /hosts`：主机清单，每项 `{"name","tunnel","connected","error","routes","active","preferred_down","preferred_back"}`；
  `tunnel` 是当前线路的隧道名，`connected` 是共用连接当前是否连着，`error` 是最近一次拨号/断开的原因（没有则空串）；
  `routes` 是线路列表 `[{"label","tunnel","addr"}]`，`active` 是当前线路下标，`preferred_down` 表示首选线路连不上，
  `preferred_back` 表示走备用期间探测到首选已恢复（没写 `routes` 的主机只有一条线路，两个标记恒为 false）。
- `POST /hosts/route?host=&route=下标`：切换线路，成功返回该主机在 `/hosts` 里的那一项（切换后的状态）。
  会关掉旧连接（终端会话随之结束），下次使用时经新线路重连。除主令牌外，还要求请求头 `Origin` 存在且在
  `allowed_origins` 里，否则 403「需要从 xxyi 页面操作」；`route` 不是整数或越界 400、主机不存在 404、非 POST 405。
- `GET /forwards`：端口转发状态，每项字段名与 `/hosts` 的一项一致（外加 `listen`，网页按同一种线路条渲染）；
  没配置 `forwards` 时返回空数组 `[]`。
- `POST /forwards/route?name=&route=下标`：切换转发的线路，成功返回该转发在 `/forwards` 里的那一项；
  会关掉当前所有已转发的连接（之后的新连接走新线路）。鉴权与 `/hosts/route` 一致（`Origin` 必需否则 403、
  非 POST 405、`route` 不是整数或越界 400、转发名不存在 404）。
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
    "claude_args": ["--model", "sonnet", "--max-budget-usd", "5"],
    "repos": {"xxyi": {"dir": "~/Documents/codes/xxyi", "remote": "ssh://chenwy@10.0.0.2/home/chenwy/xxyi"}},
    "git_tunnel": "home"
  }
  ```
- `host`（必填，须在 hosts 里）、`workdir`（必填，须是已存在的目录）、`claude_settings`（可选，Claude Code 设置文件）、
  `repos`（每项 `dir`、`remote` 都不能空）；`claude_bin` 默认 `claude`；`claude_args` 原样追加给 Claude 的参数（不校验）。旧配置里的 `claude_tools` 已废弃，写了也被忽略。
- `git_tunnel`：可选，git 同步经这条隧道的 SOCKS 口走 SSH（须在 `socks` 里有同名条目；
  用 `support.host` 第一个私钥，严格校验 known_hosts）。
- 主机有多条线路（`routes` 多于一条）时不用写 `git_tunnel`（写了启动失败）：wgtund 会在本机 127.0.0.1 开一个转发口，
  git 经它自动走主机的当前线路；此时 `support.host` 必须有 `identity_files`（用第一个私钥，同样严格校验 known_hosts）。
- 不合规时 wgtund 启动失败，错误以 `hosts.json: support…` 开头。

安全模型（只保留一道）：`PermissionRequest` 钩子。
- 协助的 Claude 就是普通的 Claude Code，用它自己的设置；所有授权请求都经钩子交给你批准，钩子失败即拒绝。
- `claude_settings` 可选：不写则合并设置里只有钩子；写了则必须是 JSON 对象，内容（沙盒、权限、`defaultMode` 等）不做校验，照你写的用。
- 启动参数固定为 `--permission-mode auto --permission-prompts none --settings <合并文件>`，并为每个仓库加 `--add-dir <dir>`（按仓库名排序），
  再原样接 `claude_args`（不校验、无白名单）。自动模式下普通操作直接放行，分类器拿不准的授权请求仍会触发 `PermissionRequest` 钩子交给用户；要换模式可在 `claude_args` 里再写一次 `--permission-mode`（后写的生效）。
- 每个仓库的 `dir` 必须存在且是 git 仓库顶层，两个仓库名不能指向同一目录。
- Claude 子进程只继承白名单环境变量（`PATH HOME USER LOGNAME SHELL TMPDIR LANG TERM TZ`、`LC_*`、`ANTHROPIC_*`、
  `CLAUDE_*`、各种 `*_proxy`），其余不传。
- 每次开启协助，wgtund 重新读 `claude_settings`（改设置不用重启；文件不是 JSON 对象则开启失败）、复制一份、覆盖其中的 `hooks.PermissionRequest`（指向下面的钩子子命令，
  失败即拒绝），并强制 `disableAllHooks=false`、删掉 `allowManagedHooksOnly`，写到配置目录的 `support-claude.merged.json`（权限 600），Claude 用 `--settings` 读它；其余设置保留。
  审计日志在配置目录的 `support-log.jsonl`。
- 远端 sshd 的 `GatewayPorts` 必须是 `no` 或 `clientspecified`：开监听后 wgtund 会检查实际地址是回环，否则报错并不启动协助。
- 远端 `endpoint.json` 内容为 `{"addr","token","until"}`，`until` 是截止时间（Unix 秒，延长时会重写）；
  `macsup` 发现已过期会直接报「Mac 上的协助已到期」，不发请求。
- 待批请求最多 50 条，超出的钩子请求直接拒绝；Claude 正在工作时 `/sync` 返回 409。

本机接口（同样要主令牌和 CORS 规则）：
- `GET /support`：状态与最近日志。
- `POST /support/start?minutes=N`（1..600）、`POST /support/stop`、`POST /support/decide?id=&allow=1|0[&message=]`：
  除主令牌外，还要求请求头 `Origin` 存在且在 `allowed_origins` 里，否则 403「需要从 xxyi 页面操作」（`GET /support` 不要求）。
- `POST /support/hook?token=<钩子令牌>`：不走主令牌，由协助服务自己校验钩子令牌；只供下面的子命令使用。

钩子子命令：`wgtund support-hook [http://127.0.0.1:端口/support/hook?token=…]`
（地址缺省取环境变量 `XXYI_HOOK_URL`；只接受本机地址）。从标准输入读钩子 JSON（最多 1MB）转给本机服务，
原样输出其回答；任何失败都输出「拒绝」并以退出码 0 结束，让 Claude Code 按拒绝处理。
`macsup projsync <项目名>`：向远端请求同步一个项目，等待用户批准后执行，成功后以 JSON 打印结果。

### 项目同步（可选）

`hosts.json` 的可选段 `projsync`：在本机和某台主机之间交换同一个项目的已提交代码，并双向补齐两边的 Claude Code 对话目录。

    "projsync": {
      "host": "ubuntu",
      "local_name": "mac",
      "remote_name": "ubuntu",
      "projects": [
        {"name": "ai-browser", "local": "~/Documents/codes/ai-browser",
         "remote": "/home/chenwy/Documents/codes/ai-browser"}
      ]
    }

- `host` 必须是 `hosts` 里的主机且有 `identity_files`（git 只用第一个私钥）。`local_name` 缺省 `mac`，`remote_name` 缺省主机名，
  只许 `[a-z0-9-]`（1..32）。项目名 `[A-Za-z0-9._-]`、1..64、不能重复。
- `local` 可写 `~/`，必须存在且是 git 仓库顶层；`remote` 是远端仓库绝对路径，只许 `[A-Za-z0-9._/-]`，不含 `..`。
  `local_claude` 缺省 `~/.claude/projects/<本机仓库绝对路径转成的目录名>`；`remote_claude` 缺省 `.claude/projects/<远端路径转成的目录名>`（相对远端登录目录）。
  配置有误时启动失败，错误以「hosts.json: projsync」开头。
- 代码：git 经主机当前线路的转发口走 ssh（严格校验主机密钥、只用指定私钥）。同步后，对方仓库里有 `refs/remotes/mac/*`
  （本机各分支），本机仓库里有 `refs/remotes/<主机名>/*`（对方各分支）。不改任何一边的 HEAD、`refs/heads/*` 和工作区。
- 对话：两边目录互相补齐缺的文件；同名文件一边是另一边的前缀（会话在增长）就用长的覆盖短的；内容分叉的算冲突，两边都不动；永不删除。
- 同一项目同一时刻只能同步一次，单次最长 30 分钟。

本机接口（要主令牌；没配置 `projsync` 时都返回 404「没有配置项目同步」）：
- `GET /projsync`：`{"projects":[{"name","running","last"}]}`，按配置顺序；`last` 是上次结果（代码与对话各有各的错误字段）或 null。
- `POST /projsync/run?name=`：要求 `Origin` 存在且在 `allowed_origins` 里，否则 403；未知项目 404；正在同步 409「正在同步」；
  否则在后台开始并立即返回 202 `{"started":true}`，之后用 `GET /projsync` 看进度。

远端 Claude 也可以用 `macsup projsync <项目名>` 发起：进入待批队列，用户在 xxyi 点允许后才同步，同步完成后打印结果 JSON。

## omlxgw（oMLX 网关）

所有访问 oMLX 的请求都经它统一控制并发与优先级。只监听回环地址（Mac 经 SSH 转发连）。

    omlxgw -config <json文件>

配置（`listen`、`upstream` 启动后不可改；其余可经 `PUT /gw/config` 热改）：

    {"listen":"127.0.0.1:18192","upstream":"http://192.168.147.196:8192","default_priority":5,
     "models":{"Qwen3.8-Flash-Next-oQ4e-mtp":{"max_concurrent":1,"exclusive":true}},
     "default_model":{"max_concurrent":8,"exclusive":false},"max_total":16}

- 透明反向代理：路径、请求头（含 Authorization / x-api-key）、请求体原样转发，响应流式回传（SSE 边收边发）。
  从请求体 JSON 的 `model` 判断模型；不带模型的请求（如 `GET /v1/models`）不排队直接转。
- 调度按请求粒度，不打断在跑的请求；一个请求从开始转发到响应结束（或客户端断开）占一个名额。
  有名额空出时挑优先级最高、同级先到的放行，须满足并发规则：`exclusive` 模型在跑时其他请求不放，其他在跑时它不进；
  普通模型各有 `max_concurrent` 上限，全局另有 `max_total`。
  防饿死：独占模型在排队且优先级不低于某请求时，不再放这个请求进去，等在跑的结束让独占模型先进。
  排队中客户端断开即移出队列。
- 优先级：请求头 `X-Xxyi-Priority: 0-9`（缺省 `default_priority`）。约定：9 用户现场交互；7 用户发起、在后台跑的对话任务；
  5 agent 编码/整理任务；3 评测与批量；1 闲时。`X-Xxyi-Client` 可选，只用于状态与日志。
- 管理接口（同一监听口，均返回 JSON，错误为 `{"error":"..."}`）：
  - `GET /gw/status`：`{"running":[...],"queued":[...]}`，每项含 model、priority、client、run_seconds / wait_seconds。
  - `GET /gw/config`、`PUT /gw/config`：PUT 整份配置，先校验（改 listen / upstream 或值非法返回 400），
    通过后原子写回配置文件（临时文件 + rename）并立即生效，在跑的请求不受影响。
- 唤醒（可选 `wake`：`host`、`knock_ports`、`max_wait_seconds` 缺省 120；省略整段即关闭）：模型请求拿到名额后、转发前，
  若最近 30 秒内没成功收到过上游响应，先探 `GET /v1/models`（3 秒超时，任何 HTTP 状态都算醒）；
  不通就 TCP 连一下 `knock_ports`（5 秒超时）叫醒，之后每 3 秒再探，直到醒或超时；超时回 503 `{"error":"Studio 没叫醒（等了 N 秒）"}`。
  多个请求同时等醒只敲一次门、共享同一次等待。转发时上游 dial 失败也按没醒处理：唤醒后重试一次。
- 防睡（可选 `keepawake`：`command`、`refresh_seconds` 缺省 240、`idle_seconds` 缺省 600；省略即关闭）：每个模型请求开始转发记一次活动；
  后台每秒检查，最近活动在 `idle_seconds` 内且距上次起命令满 `refresh_seconds`（或从没起过）就 exec 一次 `command`（不等待，
  输出丢弃，退出码记日志）。Studio 侧 `omlxwake` 账号被强制只跑 `caffeinate -i -t 300`，新旧进程重叠所以无空档。
  空闲超过 `idle_seconds` 不再起新的；网关退出时杀掉还活着的子进程。示例见 `cmd/omlxgw/omlxgw.example.json`。
- `GET /gw/status` 另含 `studio`（`awake_checked_ago_seconds` 距上次成功收到上游响应、`waking`、`last_wake_seconds` 最近一次唤醒耗时）
  与 `keepawake`（`active`、`last_started_ago_seconds`、`running` 子进程数），没有对应数据时为 null。
- 日志写 stderr，每个请求一行：时间、model、prio、client、wait（排队秒）、run（运行秒）、status（排队中断开记 499）。唤醒开始/成功（耗时）/超时、防睡起进程/进程退出（退出码）各记一行。
- systemd 用户服务示例：`cmd/omlxgw/omlxgw.service`。

## 测试
    cd go && go test ./...
