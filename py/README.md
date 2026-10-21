# pybase

零业务依赖的 Python 基础模块集：RPC 底座、内存盘、AI 客户端、docx 写作、表格读取、SMB、
加密、CDP/Playwright 纯函数、Word→PDF 转换、字幕与音视频、语音识别。

所有项目相关的默认值（密钥文件路径、内存盘名字等）都由调用方传入，本库只提供机制。

## 模块一览

- `pybase.rpc`：JSON-RPC 客户端/服务端底座（FastAPI 路由 / HTTP handler / stdio 三种服务端 + urllib 客户端）
- `pybase.ramdisk`：内存级临时盘生命周期管理，支持同名同路径的多进程共享
- `pybase.aiclient`：OpenAI 兼容客户端（`LlmClient`：chat.completions 与 embeddings；`headers` 给每个请求加附加头，如 oMLX 网关的 `X-Xxyi-Priority`）
- `pybase.docx`：docx 格式属性写入（`oxml`）与模板渲染（`render`）
- `pybase.sheets`：表格文件读取（`.xlsx`/`.xls`/`.csv`，路径或二进制文件对象均可），统一读成 `[[str]]`，不做类型推断
- `pybase.smb`：SMB/WebDAV/File Station 三通道文件抓取 + SMB 客户端（连接、列目录、下载、上传、共享枚举）
- `pybase.crypto`：基于 Fernet 的密码加密/解密（`PasswordEncryption`，不带项目默认值）
- `pybase.cdpnet`：Chrome DevTools Protocol 网络事件的纯映射/判断函数
- `pybase.playwright_launch`：按 cfg 拼 Playwright 启动/建 context 参数的纯映射
- `pybase.office`：Word→PDF 基础能力：Word COM 驱动（Windows + Office）+ 转换服务客户端
- `pybase.subtitle`：字幕读写（pysubs2，`pip install "pybase[subtitle]"`）、YouTube 自动字幕滚动折叠、
  时间校准、时间索引（`CueIndex`）、给大模型的 `[MM:SS]` 文本与分块；字幕来源（人工/平台识别/本地识别/机器翻译）
  与用户修正层，存旁车 `.meta.json`，原字幕文件不改。时间单位一律整数毫秒
- `pybase.media`：ffmpeg/ffprobe/ffsubsync 子进程封装：探测、按句截音频（含 padding）/截图、字幕自动对齐、
  网页 `<video>` 可播性判断（`playback_plan`，支持集合由调用方给）与转封装（`remux`）。
  外部工具需自行安装，路径可由调用方传入
- `pybase.asr`：语音识别（中文/英文 × 整段/实时），两个后端同一套接口：`AppleSpeech`（macOS 26+ 系统自带
  SpeechAnalyzer，零 pip 依赖，需 swiftc）与 `MlxAudio`（mlx-audio，Apple Silicon，`pip install "pybase[asr-mlx]"`）。
  **不内置任何模型**，模型和 locale 由调用方给
- `pybase.digest`：给带时间戳的字幕生成带出处的总结（概览 + 章节 + 要点），模型只引用句子编号，时间由程序查回；
  长字幕自动分块再汇总。模型调用由调用方注入（`complete(messages) -> str`）
- `pybase.search`：字幕/文本片段的本地检索，关键词（SQLite FTS5）+ 语义（调用方注入的向量函数）RRF 融合，
  中文 1~2 字查询也可用；`LlmClient.embed` 可作向量函数
- `pybase.net`：网络内核基础件。`wireguard`/`engine`/`routes`/`tunnel` 自建 WireGuard 隧道
  （wireguard-go 用户态引擎，需 root；生成密钥要 `pip install "pybase[net]"`）；`diag` 路由查询、
  地址分类、TCP 探测；`monitor` 采集接口/路由/DNS/代理/VPN/NetBird/mihomo/自定义目标，SQLite 记录并检测变化
- `pybase.storage`：存储接口 `Storage`（相对 posix 路径；`available`/`stat`/`listdir`/读写/原子改名/删除），
  把"连到哪"（本地盘、挂载的网络盘、SMB 协议、HTTPS…，以及其下的 WireGuard/NetBird 等隧道）和上层的读写、核对、
  续传逻辑解耦；自带 `LocalStorage`（本地目录与任何已挂载网络盘，可要求挂载点在挂才算可用），新通道照接口实现即可；另有 `read_bytes`/`as_memory_file`（读进内存，BytesIO 可直接交给 python-docx/openpyxl/`pybase.sheets`，不落盘，可限 `max_bytes`）和 `materialize`（必须要真实路径时用：本地盘给原路径，其他通道临时拷进内存盘，退出即删）
- `pybase.sshstorage`：`SSHStorage`，经 SFTP 读写远端目录（`pip install "pybase[ssh]"`）；路径锁在 root 下，`available()` 未连接/已断时会主动连一次（超时 ≤5 秒，成功后复用，失败 False 不抛异常），断线后下次操作自动重连，
  未知主机默认拒绝
- `pybase.smbstorage`：`SMBStorage`，基于 `SMBClient`（连接、认证、读写全走它的 `stream.py` 等方法）读写共享内某目录（`pip install "pybase[smb]"`）；`root` 是共享内目录，路径锁在 root 下，
  `available()` 未连接时先 TCP 探测再连一次（超时 ≤5 秒，不抛异常），断线后下次操作自动重连；规格 `smb://[user@]host[:port]/share/root`，
  **密码不进库**，由构造参数或 `set_credential_provider(fn)` 注册的回调 `fn(host, share, user, port)` 提供
- `pybase.fileops`：两端都是 `Storage` 的拷贝 `copy_file`（临时文件转正；`verify` 开关默认关、只比大小，开了边拷边哈希
  并读回核对；目标已有不同内容报 `conflict` 不覆盖）、`copy_file_resumable`（块级断点续传：按块写临时文件、每块
  回调进度；续传时截到最后确认块的边界并读回核对，不对就退一块；写完整体核对逐块 sha256，源被改则从头来）、`delete_confirmed`（调用方核对通过才删）、`prune_empty_dirs`
- `pybase.transfer`：持久化后台传输队列（需 `pybase[db,postgres]`，PostgreSQL）。`TransferQueue.submit` 立即返回
  task_id，任务与进度存 `transfer_tasks`/`transfer_items`，块哈希存 `transfer_chunks`（任务与项挂行级审计，进度列和块表不审计，审计量与文件大小无关）；单 worker 线程串行执行，断线（`OSError`/
  `available()` 为假）任务置 `waiting`，用 `wait_until` 间隔递增（封顶 60 秒）等两端恢复后从断点自动续传；冲突、源不存在、
  权限等非连接错误该项 `failed`、任务继续；进程重启时 `running`/`waiting` 自动回 `queued` 续传（`paused` 保持）；
  多进程靠 PG advisory lock 只让一个有 worker，其余只提交/查询并定时重试接手。**优先级**：`submit(..., priority=0)`，越大越先、同级先到先得，worker 每写完一块检查有无更高优先级的 queued 任务，有就让出（进度保留）、高的跑完自动回来续传；
  约定用户主动同步 100、放入 50、后台自动同步 0。**空闲闸**：构造参数 `busy()` + `idle_threshold`，`priority < idle_threshold` 的任务只在 `busy()` 为 False 时开始/继续，
  跑着变忙则写完当前块让出（`busy` 抛异常按繁忙）；`set_priority` 可改未结束任务的优先级；老库的 `transfer_tasks` 自动补 `priority` 列。另有 `pause`/`resume`/`cancel`、
  `list_tasks`/`task_detail`、`notifier` 回调；Storage 序列化成 `local:/abs/root` / `ssh://[user@]host[:port]/abs/root?key=钥匙路径` / `smb://[user@]host[:port]/share/root`
  （`storage_to_spec`/`storage_from_spec`，凭据不进库）。`Storage` 为此新增 `open_resume(path, offset)`
- `pybase.retry`：`wait_until`（等资源回来，间隔由调用方给）、`retry_call`（资源断着的时间不计入重试次数）
- `pybase.inventory`：经 `Storage` 列目录树清单（跳过规则由调用方给，列不动的目录单独上报），按"相对路径+大小"
  再"同名+大小"两级比对；只看元数据，不是内容证明
- `pybase.timemachine`：macOS Time Machine：备份路径识别（`inspect_selection`）、APFS 快照列表、复用系统已挂载的快照、
  其余只读挂载（`ensure_mounted`）
- `pybase.dictionary`：词典查询统一模型（`Entry`，必带出处与授权说明）+ 数据源适配器。**不内置任何词典数据**；
  目前有 `macos`（系统『词典』App 里已下载的词典，ctypes 直调，零依赖，仅限本机个人使用）

## office 模块：Word→PDF

python-docx 算不出真实分页，要靠 Word 排一遍。`pybase.office.word` 驱动 Word COM 转 PDF 并
统计页数（`pip install "pybase[office-word]"`，仅 Windows）；`WordPdfClient` 调用转换服务。
服务本身（队列、看门狗、HTTP、部署）在独立项目 wordpdf，协议见 `pybase/office/__init__.py`。

```python
from pybase.office import WordPdfClient

client = WordPdfClient("http://192.168.1.50:8765", token="xxx")
result = client.convert(docx_bytes, update_fields=True)
result.pdf_bytes    # PDF 字节
result.page_count   # Word 实际排版后的页数
```

## asr 模块：语音识别

整段音频出 `Transcript`（`cues` 可直接当字幕用，`engine` 可填进字幕来源的 `TrackMetadata.engine`）；
实时识别喂 s16le 单声道 PCM，取 `Hypothesis`（`text` 是当前这一段的完整文本，`final` 之后开始新的一段）。

```python
from pybase.asr import AppleSpeech, MlxAudio

engine = AppleSpeech("zh-CN")                       # 或 MlxAudio("<HF 仓库名或本地目录>")
transcript = engine.transcribe(                     # 视频/mkv 先用 media.extract_audio 转出音频
    "talk.wav", on_progress=lambda stage, frac: ..., max_cue_ms=6000,
)                                                    # on_progress 只有 AppleSpeech 有；
                                                      # stage 是 "installing"（下载识别资源）或
                                                      # "transcribing"，frac 是 0..1 或 None；
                                                      # max_cue_ms 两家都支持，不给就不限制单条字幕时长

with engine.open_stream() as stream:                # 采样率见 stream.sample_rate
    stream.feed(pcm_bytes)
    for hyp in stream.results(): ...
remaining = stream.results()                        # with 退出后收尾出的结果
```

| | `AppleSpeech` | `MlxAudio` |
|---|---|---|
| 语种 | 构造时给 locale（`zh-CN`/`en-US`…），不自动识别；`supported_locales()` 查可用的 | 由模型决定，参数经 `generate_kwargs` 透传 |
| 整段时间戳 | 句级（按逐字时间切） | 由模型决定：Parakeet 句级，Whisper 段级，Qwen3-ASR 只有分块级 |
| 实时 | 分段出临时/最终结果 | 仅带 `create_streaming_session` 的模型（Voxtral Realtime 等）；整个会话一段，结束时出 final |

## 安装

`pip install pybase` 本身不拉任何第三方依赖——只装用得到的模块对应的 extra：

```bash
# 作为依赖引入（推荐），只装 docx + smb 两块能力
pip install "pybase[docx,smb] @ git+ssh://git@github.com/WillyChen0811/base.git#subdirectory=py"

# 本地开发（克隆后可编辑安装）
pip install -e "./py[docx,smb]"

# 全装（CI、类型检查等场景）
pip install -e "./py[all]"
```

模块与 extra 对照表（没列出的模块——`rpc`（stdio/HTTP 部分）、`ramdisk`、`aiclient`、
`cdpnet`、`playwright_launch`、`media`、`dictionary`、`digest`、`search`、`asr`（Apple 后端）、`net`（密钥生成除外）、
`storage`、`fileops`、`retry`、`inventory`、`timemachine`——零第三方依赖，直接 `import` 即可）：

| extra | 第三方依赖 | 覆盖模块 |
|---|---|---|
| `docx` | python-docx | `pybase.docx.oxml` |
| `sheets` | openpyxl, xlrd | `pybase.sheets` |
| `smb` | cryptography, pyspnego, pyasn1, requests, urllib3（smbprotocol/pysmb 已拷进 `pybase/_vendor`） | `pybase.smb` |
| `crypto` | cryptography | `pybase.crypto` |
| `office` | requests | `pybase.office`（`WordPdfClient`，跨平台部分） |
| `office-word` | pywin32（仅 Windows） | `pybase.office.word` 的 Word COM 驱动 |
| `rpc-server` | fastapi | `pybase.rpc.create_fastapi_rpc_router` |
| `subtitle` | pysubs2 | `pybase.subtitle` |
| `ssh` | paramiko | `pybase.sshstorage`（`SSHStorage`） |
| `net` | cryptography | `pybase.net.wireguard` 的密钥生成/推导 |
| `db` | sqlalchemy | `pybase.db`（引擎/会话、方言无关的表结构操作、upsert、库身份标记） |
| `postgres` | `db` + psycopg[binary] | `pybase.db` 连 PostgreSQL |
| `asr-mlx` | mlx-audio（仅 Apple Silicon） | `pybase.asr.MlxAudio` |
| `all` | 以上全部 | —— |

## 测试

```bash
cd py && python -m pytest tests
```

内存盘跨进程测试（`test_ram_disk_multiproc.py`）依赖 macOS 的 RAM disk 挂载语义，office
模块驱动真实 Word COM 的部分只能在装了 Office 的 Windows 机器上实测；不匹配的平台上相关
用例会被自动跳过。
