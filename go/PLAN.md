# 计划：base 应用级 WireGuard + xxyi 内置 tmux 终端

## 目标与分期
- 第一期（最小可用）：Mac 上的 xxyi 开一条应用内 WireGuard 隧道，点一下就在 xxyi 页面里连上 Ubuntu 的 tmux。
- 第二期：多条隧道同时开，每条一个本地 SOCKS5 口；ai-browser 的 Electron 浏览器按窗口/实例指定走哪条隧道。
- 第三期：用 gomobile 打包给 iPhone（需要 Mac 上的 Xcode）。
- 以后：系统级（其他程序也能用），暂不做。

## 结构
**base 新增 `go/` 模块**（`github.com/WillyChen0811/base/go`），零业务依赖，所有配置由调用方传入：
- `wgtunnel`：读 wg-quick 格式配置 → 用 wireguard-go 的程序内网络栈起隧道；提供"通过隧道建连接"、关闭、状态（最近握手、流量）。一个进程里可同时开多条，各自独立。
- `wgtunnel/socks`：每条隧道一个只监听 127.0.0.1 的 SOCKS5 口，域名在隧道那一侧解析。（第二期用，第一期先做好）
- `sshterm`：在隧道连接上跑 SSH（Go 官方 x/crypto/ssh），校验 known_hosts，认证用 ssh-agent 或密钥文件；开终端、跑调用方给的命令（如 `tmux new -A -s main`）、窗口大小变化；通过 WebSocket 把终端数据给网页。
- `cmd/wgtund`：小助手程序。启动参数给配置目录；只监听 127.0.0.1，带随机令牌和来源校验；提供终端 WebSocket 和各隧道的 SOCKS 口；控制走标准输入输出的 JSON-RPC（和 pybase 的 rpc 同一个路数）。

**xxyi**：
- 服务端拉起并看护 wgtund；隧道配置放 `~/.xxyi/wireguard/*.conf`（权限 600，私钥只在 Mac 上）；主机清单 `~/.xxyi/hosts.json`（名字、走哪条隧道、地址、用户、tmux 会话名）。
- 页面加"终端"卡片：列出主机，点一下打开 xterm.js 终端（MIT，随项目带上，不走外网）。
- Swift 外壳原则上不改。

## 依赖（都要核实协议后才引入）
wireguard-go（MIT）、gVisor（Apache-2.0）、x/crypto（BSD-3）、WebSocket 库（gorilla/websocket BSD-2 或 coder/websocket ISC，二选一）、SOCKS5 服务端库（找 MIT/BSD 的现成库）、xterm.js（MIT）。

## 怎么测（Ubuntu 上不要 root）
- 同一个测试进程里起两个程序内 WireGuard 端点，经本机 UDP 互连；对端网络栈里再起一个测试用 SSH 服务器。端到端测：建连接、SOCKS、SSH 开终端回显、断线重连后 tmux 会话还在（用假的 shell 代替 tmux）。
- 真实验收：交叉编译 Mac 版，在 Air 上用你的真实配置连 Ubuntu。

## 任务拆分与谁来做
规则：编码主力是本地模型，Sonnet 平时只评审；本地模型卡死就按梯子升级，不硬耗。小而机械的（装工具、拉依赖、建分支、提交）我自己做。
- 选模型：简单任务 35B（最快）；要严格对齐、求稳的简单活备用 27B（最慢）；求稳或要长链推理的优先 Flash Next（居中）。
- 执行升级梯子：35B（或 27B）→ Flash Next → Sonnet → 我。
  升级条件（任一）：看门狗发现死循环（同一命令/同一失败连续 3 次、日志 5 分钟不动、15 分钟没改文件）就立刻停掉；它求教、我回答后续一次还卡在同一处；跑满轮数；评审打回两次同一问题仍没改对；动了受保护测试。
  升级时把上一级的改动和失败经过写进新任务说明，开新工作目录重来，不在卡住的会话里续。
- 评审永远换模型、尽量换厂商：35B/27B 执行→Flash Next 评审；Flash Next 执行→Sonnet 评审；升级到 Sonnet 执行→Flash Next 评审 + 我复核。
- 简单、提示词能写死的任务：35B（或 27B）执行 → Flash Next 评审。
- 难一点的任务：Flash Next 执行 → Sonnet 评审（换厂商）；以后可换成 opencode 接 codex 评审。
- 验收测试由我先写好，用 `--protect` 锁住，执行的模型只改实现；测试结果是最终裁判，评审只当筛子。
- 每个任务：执行 → 评审 → 我跑测试、看改动 → 我提交。

| # | 任务 | 起始执行者 | 评审 |
|---|---|---|---|
| T0 | 装 Go、建分支和工作目录、`go mod init`、拉依赖、核实协议、base 的 CLAUDE.md 补 Go 约定 | 我 | — |
| T1 | wg-quick 配置解析（字段、多 Peer、错误提示） | 35B | Flash Next |
| T2 | wgtunnel 核心 + 两端点互连的测试脚手架 | Flash Next | Sonnet |
| T3 | SOCKS5 口（建在 T2 的建连接口上） | 35B | Flash Next |
| T4 | sshterm：SSH 开终端、known_hosts、WebSocket 桥 + 测试用 SSH 服务器 | Flash Next | Sonnet |
| T5 | wgtund 助手：配置加载、JSON-RPC 控制、令牌和来源校验 | Flash Next | Sonnet |
| T6 | xxyi 服务端：拉起/看护助手、主机清单、接口 | 35B | Flash Next |
| T7 | xxyi 页面终端卡片（xterm.js） | Flash Next | Sonnet |
| T8 | 交叉编译 Mac 版，你在 Air 上真实验收 | 我 + 你 | — |

T1（35B）和 T2（Flash Next）用的是不同模型，可以同时开工；T3/T4 依赖 T2；T5 依赖 T3/T4；T6、T7 依赖 T5。
