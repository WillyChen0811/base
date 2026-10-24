# testhost：Mac 测试机

Ubuntu 派活，Mac Studio（omlxwake 账户）跑测试，结果拿回来。Ubuntu 只发 commit 与测试文件名，不跑测试。

- `mac_testhost.sh`：Mac 端入口（bash 3.2 兼容，单文件），经 `update` 推到 `~/xxyi-testhost/bin/testhost`
- `client.py`：Ubuntu 端客户端（标准库 + ssh/git 子进程）
- `tests/test_local.py`：本机自测（假目录模拟 Mac 端）：`python -m pytest testhost/tests`

## 用法

```
python client.py ping                 # 在线、核数、负载、在跑 run 数、是否空闲
python client.py push-tool            # 把 mac_testhost.sh 推到 Mac（改了入口就推）
python client.py run --repo <本地路径>[=<名>] ... [--commit-of 名=commit或分支] \
       --workdir <名> [-n N] [-k 表达式] [-m 标记] <测试文件...>
python client.py status <run_id> | result <run_id> | inventory
python client.py clean runs [--keep N] | run <id> | envs | snapshots | repos
```

`run` 流程：增量 bundle → sync → snapshot → prepare → run → 每 15 秒 status → fetch → 汇总。
退出码 = pytest 退出码（0 通过，1 有失败，5 没收集到），3 = 测试机/通道错误。Ctrl-C 只停本地等待，远端继续，之后 `result <run_id>`。

- 第一个 `--repo` 是被测仓库，必须带环境描述 `cmdproto/testenv.json`（`--env-file` 可改）。描述里 `repos` 列的其他仓库
  （如 base）没写 `--repo` 时按描述的相对路径自动加入，`--commit-of base=main` 可改版本（缺省描述里的 ref）。
- `--workdir` 是 Mac 上 `runs/<id>/src/` 下的子目录，即被测仓库在 Mac 上的名字；测试文件相对它。
- 快照：缺省按环境描述里的 `snapshots`（`paths_from` 从仓库里的 Python 文件读路径常量，不复制）；
  环境描述里快照带 `repo_env` 时（指向本机某仓库的环境变量），该仓库另按 `<键>-src` 整库同步到 Mac，测试里可对它 git show/archive。
  也可 `--snapshot 名=仓库@commit:路径1,路径2` 或 `:@清单文件`。快照名 = `名-commit前10位-路径清单hash`，同名跳过。
- `-n` 缺省取 Mac 核数（至多 16）；网关 `127.0.0.1:18192/gw/status` 的 running 非空时减半（至少 2）；给了 -n 就照给的。
- 地址/账户/钥匙：`--host`/`TESTHOST_SSH`（缺省 omlxwake@192.168.147.196）、`--key`/`TESTHOST_KEY`（缺省 ~/.ssh/xxyi_testhost）。
  ssh 开了连接复用（控制套接字在 ~/.cache/xxyi-testhost）。

## 环境描述（testenv.json）

`python`、`pip`（决定环境 hash，自动补 pytest、pytest-xdist）、`repos`、`pythonpath`（相对工作子目录，进 PYTHONPATH）、
`env`（`{run}` 换成 run 目录，`{snapshot:键}` 换成快照目录）、`snapshots`。不用 `-e ../base/py`（可编辑安装会绑死某个 run 的路径），改用 pythonpath。
prepare 会在环境的 site-packages 里写 `testhost_extra.pth`，把环境变量 `TESTHOST_PYPATH`（同 pythonpath）并进 sys.path，
这样子进程清空 PYTHONPATH 后仍能 import pybase。

## Mac 端动作

固定动作，参数全校验（名字只许 `[A-Za-z0-9._-]`，路径不许 `..`/绝对路径，pytest 只放行 `-n -k -m`）：

| 动作 | 作用 |
|---|---|
| `ping` | 机器信息 + 在跑 run 数 + idle |
| `update` | stdin 新版入口，语法检查后替换（旧版留 .prev） |
| `inventory` | 已同步的 commit、快照、环境（客户端据此做增量） |
| `sync <repo>` | stdin 是 bundle，refs/testhost/<sha> 并入 repos/<repo>.git |
| `snapshot <名>` | stdin 是 tar，解到 snapshots/<名>/（已存在跳过） |
| `prepare <id> <repo>=<commit>... [--env 路径] [--snap 键=名]` | git archive 检出到 runs/<id>/src/<repo>；按描述 hash 建/复用 envs/<hash>；输出 JSON |
| `run <id> <子目录> [-n N] [-k] [-m] <文件...>` | nohup 后台跑 pytest -v，立即返回 |
| `status <id>` | state（prepared/running/finished/dead）、已跑通过/失败/跳过、百分比、退出码 |
| `fetch <id>` | tar：junit.xml、log.tail、exit、start |
| `clean runs [--keep N] / run <id> / envs / snapshots / repos` | 手动清理，只删 ~/xxyi-testhost 下；有 run 在跑时不清缓存 |

## 目录（~/xxyi-testhost）

`bin/` 入口、`repos/<名>.git` 裸仓库、`snapshots/<名>/`、`envs/<hash>/` 与 `envs/<hash>.ok`（成功标记）、
`runs/<id>/{src,home,tmp,ramdisk,out}`、`dl/` 日志。隔离：HOME、TMPDIR、CMDPROTO_RAMDISK_ROOT 都指进 run 目录，`env -i` 起 pytest。

## 本机自测与已知差异

- `TESTHOST_LOCAL=<目录>` 让客户端直接本机执行入口（不走 ssh）；`TESTHOST_ENV_STUB=<conda 环境前缀>` 让 prepare 不建环境、
  软链已有环境。Mac 上不要设这两个变量。
- 入口里 sysctl/sw_vers/scutil 有 Linux 兜底；bash 3.2 不支持的语法（mapfile、关联数组、`${v,,}`）一律不用；变量后接中文要写 `${VAR}`。
- macOS 的 tar 是 bsdtar，GNU 与 bsd 的 `-x -C -f -` 写法一致；`find`/`paste`/`date` 只用两边共有的选项。
- 没有防睡处理；没有 setsid，后台进程靠 nohup + 全部重定向，ssh 会话结束后不受影响（无 pty）。
- 首次 prepare 要联网装 conda/pip 包，慢（本机实测 3~4 分钟）；之后同描述复用。
- 失败的 prepare 会留下 runs/<id> 半成品目录，`clean runs` 清掉。
