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
HTTP 接口都要带 `token` 参数：`GET /hosts`、`GET /status`、WebSocket `/term?host=&cols=&rows=`。

## 测试
    cd go && go test ./...
