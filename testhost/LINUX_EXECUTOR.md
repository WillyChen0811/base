# Linux guest 协议适配（首期）

`linux_executor.LimaTransport` 是可信调度代码使用的传输对象，复用现有
Transport 的 call/call_tar。它不解析业务任务、不接受任意 shell，也不绕开 run。
第一版由 Mac 端已声明任务调用；尚未接进客户端 --target 或受限 SSH 新动作。

## 机器合同

- 机器配置提供绝对 limactl 路径、LIMA_HOME、具名 instance；项目不得决定这些值。
- 默认 VM 为 studio-linux-arm64；安装和启动 VM 由独立管理任务完成。
- 每次 guest 动作通过 limactl shell 执行固定 Python trampoline，再调用 guest
  `~/.local/share/testhost/bin/testhost`，仅允许 testhost 既有固定动作。
- 不直连 guest SSH，不挂载宿主 home；Lima 环境从真实账户 home 构造，不沿用 run HOME。

## 明确 bootstrap

bootstrap(agent, miniforge_installer, installer_sha256) 只接受已下载的 Linux arm64
Miniforge 安装器和官方固定 SHA256。来源 URL、版本和摘要须由管理任务固定记录。
宿主先校验，guest 收 tar 后再次校验，再安装 guest 专用 Miniforge 和 agent。
既有 Miniforge 来源摘要不匹配时拒绝覆盖。不自动 sudo/apt，不安装 host 工具。
guest 需要 python3、git、bash、tar、ps/pgrep、nproc、awk；缺失时报错，交由
受控 VM provisioning 补齐。Ubuntu arm64 的 Python3 可执行 bootstrap；bootstrap
校验 uname 为 Linux aarch64，Mac 的 conda/venv 永远不复制进 guest。

核查现有 agent：ensure_env 调用独立 TH/miniforge/bin/conda；它不负责下载 conda。
系统信息有 uname、nproc、/proc/meminfo 等 Linux fallback；临时/ramdisk 位于 run
子目录，无 diskutil/RAM disk 依赖；取消用 ps/pgrep 和 run 路径匹配。
这些为代码核查结论，真实取消与后台子进程行为仍须 Studio guest 集成验收。

## 数据与生命周期

源码使用既有 sync 的 Git bundle 和 snapshot，不共享宿主 checkout。
prepare 读取 guest 已同步仓库的环境描述，环境缓存仅在 guest TH/envs。
run 只执行已提交描述中的 task 或 pytest 参数；status/cancel/clean 全部转给 guest
agent，fetch/artifacts 使用已有产物大小及SHA256校验协议。任务断连后可继续查询。
普通 clean 只处理 guest run，不停止/删除持久 VM，也不清宿主测试缓存。

## 尚需 root 集成验收

1. 在 Studio 的声明任务中 bootstrap guest agent，并记录固定安装器来源。
2. 通过 LimaTransport 执行 ping、sync、prepare、run、status、artifacts。
3. 提交带子进程的任务后 cancel，证明无遗留且其他 run 不受影响。
4. 显式资源预算仍由共享宿主调度维护；此适配器不构成独立调度系统。
5. 将可信 runtime 探测传回 targets 校验，不能将 registry 声明当实际平台证明。

在上述实际验收完成前，T06/T09 不应标记完成。

CLI 核对：官方 https://lima-vm.io/docs/reference/limactl_shell/ 与 v2.3.0
https://github.com/lima-vm/lima/blob/v2.3.0/cmd/limactl/shell.go
的 shellAction 显式支持 INSTANCE 后的 --；本适配不使用 copy 或共享挂载。
