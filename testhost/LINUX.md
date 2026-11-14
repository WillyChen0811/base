# Studio Linux arm64 执行环境（安装准备）

状态：已准备工具和固定资源；是否已运行须以实际 testhost run 记录为准。
首期采用 Lima 2.3.0 + Apple VZ + Ubuntu 24.04 arm64，无 GUI、Homebrew、sudo 或 QEMU 依赖。
Ubuntu dated image 可能数月后移除，首次成功下载后应保留缓存；不回退未校验的 latest。

## 审核后运行

在 base 仓库根目录提交后执行：

```sh
python testhost/client.py run --repo . --workdir base --env-file testhost/lima-testenv.json --task lima-install --out-dir /tmp/testhost-lima-install
```

任务显式包含 `--start`，因此会安装并启动具名 VM；移除此参数仅安装与校验配置。
调用者通过现有受限 testhost SSH 派发任务；不增加任意 SSH shell 或 Ubuntu 直连 VM。
脚本支持 `--help`，所有安装资源均显式固定版本和 SHA256。
来源校验：Lima release asset digest 与 SHA256SUMS 一致；Ubuntu checksum 与 Lima 官方模板及 Ubuntu SHA256SUMS 一致（2026-11-14 核对）。

## 本机路径和预算

脚本通过 passwd 取得服务账户真实 home，而非 testhost 替换的 run HOME：

- `~/xxyi-testhost/tools/lima-2.3.0/`：二进制与安装来源、逐文件 SHA256 收据 `.testhost-source.json`。
- `~/xxyi-testhost/config/studio-linux-arm64.yaml`：JSON 格式 YAML 配置。
- `~/xxyi-testhost/lima/`：LIMA_HOME，VM 状态、磁盘及连接资料。
- VM 名：`studio-linux-arm64`，8 vCPU / 16GiB RAM / 100GiB 磁盘。
- `mounts: []`：不默认挂载真实账户 home；禁用用户与系统 containerd。

复用安装前检查收据来源及文件摘要；未知来源或内容变化立即失败，不覆盖现存安装。
配置变化同样拒绝静默覆盖。安装收据属于本机来源校验，不代替签名信任或抗本机恶意篡改机制。
资源与 macOS/oMLX 共用，不得将 VM 当作独立物理机重复分配全部宿主资源。
未创建 LaunchAgent 或 autostart 配置；当前不会随登录或系统重启自动启动。
任务完成也不主动停止 VM；root 应核对 testhost 任务结束/清理与 VM 后台进程的关系。

## 验收与清理

安装任务只校验配置、启动并查询 `uname -sm`；不能据此标记完整 Linux 调度已实现。
后续需验证系统重启恢复、锁屏运行、进程取消、来宾独立 run/依赖环境、产物回传和业务 smoke。

管理命令必须使用真实账户 home 和专用 LIMA_HOME，例如在已审核的 testhost 管理任务内执行：

```sh
LIMA_HOME="$REAL_ACCOUNT_HOME/xxyi-testhost/lima" "$REAL_ACCOUNT_HOME/xxyi-testhost/tools/lima-2.3.0/bin/limactl" stop studio-linux-arm64
```

`REAL_ACCOUNT_HOME` 应从 passwd 解析，不从隔离 HOME 猜测。删除前确认无运行任务，先停机，再使用同样环境的
`limactl delete studio-linux-arm64`；该操作会删除 VM 磁盘，应先导出需保留的产物。
仅在确认不再使用后删除本工具专用的版本目录和配置文件，不清空用户其他 Lima 实例或共享缓存。
现有 `clean runs` 只负责 run；此持久实例不应由普通 run 清理顺带删除。

官方资料：
- https://lima-vm.io/docs/installation/
- https://lima-vm.io/docs/config/vmtype/vz/
- https://github.com/lima-vm/lima/releases/tag/v2.3.0

## 下载超时与缓存

下载每项最多重试3次，socket超时30秒，每次总耗时上限900秒、大小上限4GiB。
持续输出阶段和已下载字节，TLS验证保持开启。缓存为真实账户的
`~/xxyi-testhost/downloads/<SHA256>.tar.gz` 和 `<SHA256>.img`；每次使用前重新验证摘要。
镜像先由安装工具校验缓存，VM配置引用本地镜像，避免Lima另行不透明下载。
可通过 `--archive-file`、`--image-file` 提供testhost snapshot中的校验资源；
本地来源同样验证SHA256，不能将任意本地文件作为未校验资源。快照路径应写在声明cmd中使用
已有snapshot变量替换，不使用任意SSH或关闭TLS。断点续传尚未实现，失败会删除本次临时文件。
