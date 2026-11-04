# testhost：Studio 算力机

Ubuntu 派活，Mac Studio（omlxwake 账户）跑任意仓库的 pytest 或描述里声明的耗时任务（导表、生成、批量处理、构建），结果拿回来。Ubuntu 只发 commit 与测试文件名/任务名，不在本机跑。

- `mac_testhost.sh`：Mac 端入口（bash 3.2 兼容，单文件），经 `update` 推到 `~/xxyi-testhost/bin/testhost`
- `client.py`：Ubuntu 端客户端（标准库 + ssh/git 子进程）
- `media.json`：media 动作用的 ffmpeg 环境描述（conda-forge ffmpeg），随 `push-tool` 推到 `~/xxyi-testhost/bin/media.json`
- `tests/test_local.py`：本机自测（假目录模拟 Mac 端）：`python -m pytest testhost/tests`

## 用法

```
python client.py ping                 # 在线、核数、负载、在跑 run 数、是否空闲
python client.py push-tool            # 把 mac_testhost.sh 与 media.json 推到 Mac（改了入口或 media.json 就推）
python client.py run --repo <本地路径>[=<名>] ... [--commit-of 名=commit或分支] \
       --workdir <名> [-n N] [-k 表达式] [-m 标记] <测试文件...>
       [--plugin 外挂.py]... [--plugin-file 配套.py]... [--plugin-env KEY=VALUE]... [--out-dir 目录]
python client.py run --repo ... --workdir <名> --task <任务名> [--plugin-env KEY=VALUE]... [--out-dir 目录] [-- 追加参数...]
python client.py status <run_id> | result <run_id> [--out-dir 目录] | fetch <run_id> --out-dir 目录 | inventory
python client.py cancel <run_id>     # 停止 run（见 Mac 端动作 cancel：只杀属于该 run 的进程）
python client.py clean runs [--keep N] | run <id> | envs | snapshots | repos
python client.py media put <本地文件> [--name 名]                       # 见「media 动作（11/04）」
python client.py media audio <哈希> [--segment-min 10] --out <目录>
python client.py media frames <哈希> --at 1234,5678 [--width 960] --out <目录>
python client.py media clean <哈希> | --older-than <天>
```

`run` 流程：增量 bundle → sync → snapshot → prepare → run → 每 15 秒 status → fetch → 汇总。
退出码 = pytest 退出码（0 通过，1 有失败，5 没收集到），3 = 测试机/通道错误。Ctrl-C 只停本地等待，远端继续，之后 `result <run_id>`。

- 第一个 `--repo` 是被测仓库，必须带环境描述：`--env-file` 显式给的优先，缺省找仓库根的 `testhost.json`，再找 `cmdproto/testenv.json`（兼容 xxyi）。描述里 `repos` 列的其他仓库
  （如 base）没写 `--repo` 时按描述的相对路径自动加入，`--commit-of base=main` 可改版本（缺省描述里的 ref）。
- `--workdir` 是 Mac 上 `runs/<id>/src/` 下的子目录，即被测仓库在 Mac 上的名字；测试文件相对它。
- 快照：缺省按环境描述里的 `snapshots`（`paths_from` 从仓库里的 Python 文件读路径常量，不复制）；
  环境描述里快照带 `repo_env` 时（指向本机某仓库的环境变量），该仓库另按 `<键>-src` 整库同步到 Mac，测试里可对它 git show/archive。
  也可 `--snapshot 名=仓库@commit:路径1,路径2` 或 `:@清单文件`。快照名 = `名-commit前10位-路径清单hash`，同名跳过。
- `-n` 缺省取 Mac 核数（至多 16）；网关 `127.0.0.1:18192/gw/status` 的 running 非空时减半（至少 2）；给了 -n 就照给的。
- 外挂（导测试用例表等）：`--plugin x.py`（可多次）随 run 上传到 Mac `runs/<id>/plugins/`，该目录进 PYTHONPATH，pytest 加 `-p x`；
  `--plugin-file` 只上传配套文件（如 caseplug_child.py），不加载；`--plugin-env KEY=VALUE` 传给外挂，值里 `{run}` 换成 `runs/<id>`
  （如 `CASE_OUT={run}/cases.json`）。外挂按单进程设计，用了 `--plugin` 而没给 `-n` 时缺省 `-n 1`。
  `--out-dir` 在跑完后把 run 目录顶层与 `out/` 里外挂写出的普通文件（单个 ≤50MB，内置文件除外）取回本地；`fetch`/`result --out-dir` 可事后单取。
  导表例：`--plugin caseplug.py --plugin-file caseplug_child.py --plugin-env 'CASE_OUT={run}/cases.json' --out-dir out/ <测试文件>`。
  Mac 端校验：模块名只许 `[A-Za-z0-9_]`，文件只能在本 run 的 plugins 目录（不认软链），`--plugin-env` 不许改 PATH/HOME/TMPDIR/PYTHON*/TESTHOST_*/LD_*/DYLD_*。
- 地址/账户/钥匙：`--host`/`TESTHOST_SSH`（缺省 omlxwake@192.168.147.196）、`--key`/`TESTHOST_KEY`（缺省 ~/.ssh/xxyi_testhost）。
  ssh 开了连接复用（控制套接字在 ~/.cache/xxyi-testhost）。

## 环境描述（testhost.json）

`python`、`pip`、可选 `conda`（conda 包列表，如 `"nodejs=20"`）一起决定环境 hash（pip 自动补 pytest、pytest-xdist；没写 conda 时 hash 与旧版相同，旧环境缓存继续命中）、`repos`、`pythonpath`（相对工作子目录，进 PYTHONPATH）、
`env`（`{run}` 换成 run 目录，`{snapshot:键}` 换成快照目录）、`snapshots`、可选 `tasks`。不用 `-e ../base/py`（可编辑安装会绑死某个 run 的路径），改用 pythonpath。
prepare 会在环境的 site-packages 里写 `testhost_extra.pth`，把环境变量 `TESTHOST_PYPATH`（同 pythonpath）并进 sys.path，
这样子进程清空 PYTHONPATH 后仍能 import pybase。

## 任务（tasks）

`"tasks": {"名": {"cmd": ["argv0", "argv1", ...], "cwd": "相对工作子目录(可选)", "outputs": ["相对 run 目录或其 out/ 的路径/通配(可选)"]}}`。
`run --task 名 [-- 追加参数]`：Mac 端从**该 run 检出的代码里的描述**读任务，不接受 Ubuntu 传命令；argv 直接执行（不经 shell），
`cmd` 里 `{run}` 换成 run 目录，追加参数接在 argv 末尾（不许含换行）；argv0 不带斜杠时在环境的 bin 与 PATH 里找。
环境变量与隔离同 pytest（env -i、HOME/TMPDIR/内存盘根指进 run 目录、`env`、pythonpath、`--plugin-env`，另有 `TESTHOST_TASK`）。
`--task` 不能与测试文件、`-n -k -m --plugin` 并用。没有 junit：status 报 state/exit/日志末几行（`task`、`log_tail`），退出码 = 任务退出码。
产出：`--out-dir`/`fetch` 取回 run 目录顶层与 `out/` 里的普通文件，加上 `outputs` 命中的文件（≤50MB，不跟软链），任务运行保留相对路径（去掉开头的 `out/`）。

## Mac 端动作

固定动作，参数全校验（名字只许 `[A-Za-z0-9._-]`，路径不许 `..`/绝对路径，pytest 只放行 `-n -k -m --plugin --plugin-env`）：

| 动作 | 作用 |
|---|---|
| `ping` | 机器信息 + 在跑 run 数 + idle |
| `update` | stdin 新版入口，语法检查后替换（旧版留 .prev） |
| `inventory` | 已同步的 commit、快照、环境（客户端据此做增量） |
| `sync <repo>` | stdin 是 bundle，refs/testhost/<sha> 并入 repos/<repo>.git |
| `snapshot <名>` | stdin 是 tar，解到 snapshots/<名>/（已存在跳过） |
| `prepare <id> <repo>=<commit>... [--env 路径] [--snap 键=名]` | git archive 检出到 runs/<id>/src/<repo>；描述路径缺省 testhost.json 再 cmdproto/testenv.json；按描述 hash 建/复用 envs/<hash>；输出 JSON |
| `run <id> <子目录> [-n N] [-k] [-m] [--plugin 模块] [--plugin-env K=V] <文件...>` | nohup 后台跑 pytest -v，立即返回 |
| `run <id> <子目录> --task <名> [--plugin-env K=V] [-- 追加参数...]` | 跑描述里的任务，其余同上 |
| `status <id>` | state（prepared/running/finished/dead/cancelled）、已跑通过/失败/跳过（进行中数日志里顶层 pytest 的结果行，不数内层子进程 pytest 的输出；结束后以 junit 为准）、百分比、退出码 |
| `plugin <id> <名.py>` | stdin 是外挂文件，落到 runs/<id>/plugins/（prepare 后、run 前） |
| `fetch <id>` | tar：junit.xml、log.tail、exit、start |
| `fetchx <id>` | tar：外挂写在 run 目录顶层与 out/ 里的普通文件 |
| `cancel <id>` | 停止该 run，回 killed/survivors 计数（找进程与记状态的规则见下） |
| `clean runs [--keep N] / run <id> / envs / snapshots / repos` | 手动清理，只删 ~/xxyi-testhost 下；有 run 在跑时不清缓存 |
| `media-put <sha256> <文件名> [--size 字节]` | stdin 是文件本身，校验 sha256 后存 media/<哈希>/<文件名>；已有同哈希不读 stdin、回 existed |
| `media-audio <sha256> [--segment-min 分钟]` | tar：`audio/0001.wav…`（16 kHz 单声道 16 位）+ `index.json` |
| `media-frames <sha256> --at 毫秒,… [--width 像素]` | tar：`frames/<毫秒>.jpg` + `index.json` |
| `media-clean <sha256> / --older-than <天>` | 按哈希删，或删超过 N 天没用过的（在用的跳过） |
| `media-spec` | stdin 是 media.json，校验后替换 bin/media.json（push-tool 在 update 之后调） |

`cancel <id>`：要停的进程按**本 run 的目录路径**找——`out/pid` 里那个进程及其全部后代（`ps` 自己算进程树），
加上命令行里含 `runs/<id>/`（带尾斜杠，防 run1 误中 run10）的进程（抓已孤儿化、父进程变 launchd 的，如测试起的临时 PG postmaster）；
**绝不按进程名（postgres/python/sleep）杀**，别的 run 的进程不受影响。先 SIGTERM、每 0.5 秒查一次共 5 秒，还活着的 SIGKILL 再等 1 秒。
记 `out/cancelled`（时间戳），`out/exit` 没有补 143、`out/exit.end` 没有补当前时间；status 此后报 state=cancelled。
run 已自己结束且无残留时只回 `state:"finished"` 不写 cancelled。之后 `clean runs`/`clean run <id>` 正常删除。
客户端 `run`/`result` 等待中遇到 cancelled 报「已取消」退出码 130。

## 目录（~/xxyi-testhost）

`bin/` 入口（与 `media.json`）、`repos/<名>.git` 裸仓库、`snapshots/<名>/`、`envs/<hash>/` 与 `envs/<hash>.ok`（成功标记）、
`runs/<id>/{src,home,tmp,ramdisk,out}`、`dl/` 日志、`media/<sha256>/`（上传的媒体，见下）。隔离：HOME、TMPDIR、CMDPROTO_RAMDISK_ROOT 都指进 run 目录，`env -i` 起 pytest。

## media 动作（11/04）

给「Ubuntu 发起、媒体处理交 Studio」用（Ubuntu 的 CPU 不做媒体处理）：录像传上去，要 16k 音频或某几个时间点的画面时同步调一次，
不走 prepare/run/轮询，不占 run。Studio 不在文件同步里，文件只能经 `media put` 上传。

- **存放**：`~/xxyi-testhost/media/<sha256>/<原文件名>`，旁边 `.last_used`（最后一次 put/audio/frames 的时间戳）。
  文件名取自 `media put` 的本地文件名（或 `--name`），只用来留扩展名与认人；不能有 `/` `\` 和控制字符、不能以 `.` 或 `-` 开头、UTF-8 ≤200 字节，中文、空格可以。
  同一内容第二次 put 回「已有」，保留第一次的文件名。
- **put**：客户端流式算 sha256（1 MB 一块，不整块读进内存），再把文件描述符直接交给 ssh 当 stdin。Mac 端边收边算哈希，
  哈希或字节数（`--size`）对不上就删掉半成品、拒（`hash_mismatch`/`size_mismatch`）；先写 `media/.tmp-<pid>-*/`，校验过了整目录改名，
  并发 put 同一份只留一份。已有同哈希时 Mac 端不读 stdin、直接回 `existed:true`（只多传 ssh 窗口里那一点）。
- **audio**：`ffmpeg -i <文件> -map 0:a:0 -af aresample=async=1:first_pts=0 -ac 1 -ar 16000 -f s16le -`，Mac 端按样本数切段，
  每段写好 WAV 头就进 tar、随即删掉，磁盘上至多一段（10 分钟一段约 19 MB）。时间轴与 frames 的 `-ss` 相同（文件起点 = 0，
  音轨开头的空缺补静音）。`--segment-min` 0.01..600、至多 3 位小数（段界都是整毫秒），最后一段不足照切。
  `index.json`：`{hash, name, sample_rate:16000, channels:1, bits:16, segment_min, segment_ms, duration_ms, samples,
  segments:[{file:"audio/0001.wav", offset_ms, duration_ms, samples, bytes}]}`。
- **frames**：每个时间点一条 `ffmpeg -ss <秒> -i <文件> -frames:v 1 -vf scale=w='min(<width>,iw)':h=-2 -q:v 3`（输入前 `-ss`，
  解码寻址到准确时间；宽度不放大；并发至多 4 条）。`--at` 至多 500 个非负整数毫秒，重复的去掉；`--width` 缺省 960。
  `index.json`：`{hash, name, duration_ms, max_width, video_stream, frames:[{t_ms, file:"frames/<t>.jpg", bytes, width, height}],
  out_of_range:[t…]（≥ ffprobe 报的时长，不截）, failed:[{t_ms, error}]（时长未知或最后一帧之后没截到的）}`。有超出/失败也算成功。
- **clean**：`<sha256>` 删那一份（没有也回成功，`deleted:[]`）；`--older-than <天>` 删 `.last_used` 早于 N 天的（`0` = 全删）。
  正在被 audio/frames 用的（`.busy-<pid>` 且进程活着）不删：指名删报 `busy`，按天删列在 `busy` 里。顺手清掉中断留下的
  `.tmp-*`/`.work-*`（进程不在了或超过 2 天）。回 `deleted, busy, freed_bytes, kept, free_gb`。保留多久由调用方定（本库不自动删）。
- **ffmpeg 从哪来**：`media.json`（`python 3.12` + `conda: ["ffmpeg"]`）照 prepare 的环境机制建 `envs/<hash>/`，动作里用
  `envs/<hash>/bin/ffmpeg`、`ffprobe` 的绝对路径（强制命令的 PATH 不含 /opt/homebrew/bin，也不需要）。audio/frames 第一次调用时环境没建
  就当场建（conda-forge 联网，Studio 要能上网；建的日志在 `dl/env-<hash>.log`），这一次要几分钟，之后复用。参数和哈希先校验，不对不会去建环境。
  `media.json` 与 `testenv.json` 的 python/pip/conda 等价，两者共用一个环境（测试守着）。`clean envs` 会连它一起删，下次 media 动作再建。
- **推到 Studio**：Studio 上不用装任何东西。`python client.py push-tool` 先 `update` 推入口（语法检查后替换，旧版留 .prev），
  `ping` 确认版本（v5 起有 media 动作），再 `media-spec` 推 media.json（v4 的旧入口不认 media-spec，只会回「不认识的动作」，
  不会把 JSON 当入口换上去）。改了 media.json 的包，重推后 hash 变了，下次 audio/frames 自动建新环境。
- **出错的形状**：Mac 端 JSON 带 `code`（`bad_args`、`not_found`、`hash_mismatch`、`size_mismatch`、`too_big`、`no_space`、`busy`、
  `bad_media`、`no_audio`、`no_video`、`ffmpeg`）。audio/frames 还没开始写 tar 就出错时 stdout 是一行 JSON；写到一半出错时
  JSON 写 stderr、退出码 1、不写 index.json。客户端只认 `audio/NNNN.wav`、`frames/<毫秒>.jpg`、`index.json` 三种成员（别的、链接、
  `..` 一律拒），每个文件先写 `.part` 再改名，收完核对 index 里列的文件都到了，index.json 写进 `--out`（不清 `--out` 里原有的文件，每次用新目录或以 index 为准）。
- **客户端退出码**（media 子命令）：0 成功；4 = Studio 上没有这个哈希（先 `media put`）；3 = 其余（通道、校验不过、参数、ffmpeg）。
  最后一行打印 JSON 结果（audio/frames 只打摘要，完整 index 在 `--out/index.json`）。Python 里可直接调 `media_put`/`media_audio`/
  `media_frames`/`media_clean`（返回同样的字典）。
- **限制**：单文件 ≤20 GiB；写完后磁盘至少还剩 10 GiB（给了 `--size` 先查，收的过程中每 256 MB 再查），`ping` 回 `disk_free_gb`。
  没有排队：每次调用是一条 ssh 会话里的同步活，几个客户端同时调就同时跑 ffmpeg（不看网关忙不忙）。上传的录像不会自己过期，要调用方
  `media clean --older-than N`。`clean envs` 与正在跑的 audio/frames 不互斥（会删掉正在用的 ffmpeg）。
- **Studio 实测（11/04，v5）**：第一次 audio 当场建 ffmpeg 环境连同调用共约 50 秒，之后复用；30 秒合成样片（1.5 MB mp4）
  put 0.2 s、再 put（已有）0.2 s、audio 切 3 段 0.3 s、frames 三个点（一个超出）0.35 s、clean 0.2 s（都是客户端墙钟，含 ssh）；
  300 MB 上传 1.1 s，客户端常驻内存 28 MB。testhost 自测 59 过 / 0 败 / 1 跳（跳的是「没有 ffmpeg 时报错」那条）。

## 本机自测与已知差异

- `TESTHOST_LOCAL=<目录>` 让客户端直接本机执行入口（不走 ssh）；`TESTHOST_ENV_STUB=<conda 环境前缀>` 让 prepare 不建环境、
  软链已有环境（media 环境同理）。Mac 上不要设这两个变量。本机模式下入口在假目录 `bin/` 里找不到 media.json 时用入口旁的那份。
- media 的上传/去重/校验/清理/参数自测哪里都跑；转音频、抽帧要真 ffmpeg，只在 macOS 且测试环境里有 ffmpeg 时跑（Studio：
  `client.py run --repo <base> --workdir base --env-file testhost/testenv.json testhost/tests/test_local.py`），Ubuntu 上跳过。
  `testenv.json` 的任务 `media-sample` 用 lavfi 合成 30 秒带音轨的测试视频（`--task media-sample --out-dir 目录` 取回 sample.mp4），真跑用它，不用真实录像。
- 入口里 sysctl/sw_vers/scutil 有 Linux 兜底；bash 3.2 不支持的语法（mapfile、关联数组、`${v,,}`）一律不用；变量后接中文要写 `${VAR}`。
- macOS 的 tar 是 bsdtar，GNU 与 bsd 的 `-x -C -f -` 写法一致；`find`/`paste`/`date` 只用两边共有的选项。
- 没有防睡处理；没有 setsid，后台进程靠 nohup + 全部重定向，ssh 会话结束后不受影响（无 pty）。
- 首次 prepare 要联网装 conda/pip 包，慢（本机实测 3~4 分钟）；之后同描述复用。
- 失败的 prepare 会留下 runs/<id> 半成品目录，`clean runs` 清掉。
