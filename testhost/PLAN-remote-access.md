# Studio 测试环境远程接入

## 范围

支持两个入口，复用 testhost 原有同步、任务、状态和产物协议：

1. 远端经指定 TCP 转发端口直接连接 Studio SSH。远端持有 Studio 调用私钥，并校验 Studio 主机公钥。
2. 远端向 Ubuntu SSH 独立认证，由受 forced command 约束的 broker 将 testhost 协议转交 Studio。Studio 私钥只由 Ubuntu 持有；Ubuntu 校验 Studio 主机身份。

不在本次任务中修改线上 authorized_keys、分发私钥或开放公网监听。wgtund 继续复用现有 TCP 转发能力。

## 分工

- direct_transport：client.py 与目标声明支持 SSH 端口、HostKeyAlias、known_hosts；严格校验和连接复用隔离；对应测试和 TARGETS.md。
- ubuntu_broker：流式协议转接入口、机器本地配置、固定动作校验、对应测试与 REMOTE.md。
- 主 agent：接口协调、代码评审、集成，在 Studio 执行测试并核对结果。

## 验收

- 保持既有直连调用兼容；新增 CLI 和目标配置可以访问转发端口。
- 两层 SSH 各自校验身份，未知或错误主机密钥不能通过。
- broker 不接受远端覆盖 Studio 地址、密钥和配置，不执行远端任意 shell。
- 上传和二进制产物双向流式传递；stderr 和非零退出码保留。
- 两种路径覆盖成功、认证/主机身份失败和中断场景；测试使用合成数据。
- 相关测试在 Studio 执行；完成后记录提交和验证结果。

## 完成记录（2026-11-17）

- 已实现客户端 SSH 端口、主机别名、known_hosts 配置与严格身份校验，见 TARGETS.md。
- 已实现 Ubuntu SSH forced-command broker，双向流式传递原有 testhost 协议，见 REMOTE.md。
- Studio 验证提交：15495a4；run：r1117021911-15495a4。
- testhost/tests 全套：189 通过、0 失败、1 跳过，pytest 退出码 0，墙钟 339 秒。跳过项为环境已安装 ffmpeg 时不适用的缺少 ffmpeg 用例。
- 两个真实 SSH 集成用例均通过：临时端口转发与 Ubuntu broker 两跳，包含独立凭据、未知/错误主机密钥拒绝、2MB 二进制往返、退出码和断链。
- 首次连接发现 SSH 控制套接字路径过长，已缩短为单一配置哈希并在上述提交中通过实际连接及回归验证。
- 本次交付为基础库代码及接入文档；未更改线上 authorized_keys、分发凭据或开放公网监听。

## Review 修复验收（2026-11-17）

- SSH 复用缓存纳入每次 `ssh -G` 解析的实际配置；真实 SSH 测试验证旧 master 存活时修改同一别名的 HostName、User、Port 不会误用旧连接。
- 临时 sshd 日志改为普通文件，退出前关闭临时 master，并回收 TCP 转发线程；broker 集成用例从 119.969 秒降至 0.752 秒。
- Studio `r1117055745-90b4e3f`：相关四个测试文件 69 通过、1 失败。失败来自新测试假设 macOS 上 127.0.0.2 会立即拒绝连接，实际等待超时；产品连接超时未修改。
- 将该测试改为切换到可达的 localhost，并检查新旧 master 套接字同时存在。Studio `r1117055906-4e5a6dd` 仅复测该文件：2 通过、0 失败，退出码 0；已通过的其他用例未重复运行。
