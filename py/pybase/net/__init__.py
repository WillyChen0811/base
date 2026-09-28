"""网络内核基础件：自建 WireGuard 隧道（wireguard-go 引擎 + UAPI + 接口地址/路由）、
连接诊断（`diag`）、网络状态采集与记录（`monitor`）。

子模块按需 import，本包不连带加载任何子模块。需要 root 的系统命令一律经注入的
`runner` 执行，不内置网段、接口名、路径等业务默认值。
"""
