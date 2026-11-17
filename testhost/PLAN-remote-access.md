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
