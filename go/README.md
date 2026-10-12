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

## 测试
    cd go && go test ./...
