# base 项目记忆

## 全库通用

- 零业务依赖：不写死任何项目相关的默认值（路径、密钥、CA 名字等），一律由调用方传入
- 重依赖（`win32com`/`pythoncom`、`electron` 等）延迟 import，模块顶层只 import 标准库
- 依赖按模块拆 extra：`[project].dependencies` 保持为空，新模块要第三方库就加同名 extra 并加进 `all`；
  包的 `__init__` 不能连带 import 需要 extra 的子模块（用模块级 `__getattr__` 延迟，见 `office/__init__.py`）
- 文档分层：README 只放不易变的模块一览/安装/测试/调用契约；会踩坑的约束进本文件；
  函数级说明进代码注释；实现细节和推导过程不写文档，留在代码里
- 测试（改动后必须跑）：
  - `cd py && python -m pytest tests`
  - `cd js && node --test "tests/**/*.test.js"`
  - `cd go && go test ./...`

## pybase

### rpc
- `run_stdio` 的 stdin/stdout 必须 reconfigure 成 UTF-8
- `handle_request` 先校验方法存在/签名；业务内部抛出的 `ValueError`/`TypeError` 不能当路由错误处理

### ramdisk
- 同名同路径的实例共享同一块盘；Windows 分支会退化成落盘的临时目录

### crypto
- 真实密钥是随机生成后存入 `key_file`；`passphrase` 不参与派生，仅为兼容签名保留
- `key_file` 不存在才生成（原子创建，防并发覆盖）；读不了/内容无效抛 `KeyFileError`，绝不自动覆盖——
  换不换由用户决定（先重启重试，仍不行由调用方调 `PasswordEncryption.regenerate`）
- `fingerprint`（当前密钥 SHA-256）库不存，调用方存起来跟旧值比对，查"密钥是不是被换过"

### docx/oxml
- 新建 `rPr`/`pPr`/`tcPr` 等子元素必须用 `ensure_child` 按 schema 顺序插入，禁止直接 `append`，
  否则 Word 打开时报"内容有问题"

### smb
- `upload_file` 的 `mtime`/`atime`/`birthtime` 由调用方在定版时 stat 好传入，函数内部不能再读本地文件
- 上传必须走临时文件（`.wsu`）+ `replace` 原子替换；临时名被拒时降级为替换扩展名（见 `common._tmp_names`）；规则在 `pybase/tmpname.py`，传输队列续传临时名（`.xferq.part`，降级为 `.xfq`）同理，清理一律认 `resumepart.is_resume_part`
- `exists()` 是三态：`None` 表示判不出，不能当 `False` 用
- 固定使用 NTLM，不用 negotiate（Kerberos 在 IP 直连场景会导致 `ACCESS_DENIED`）
- `CONNECT_TIMEOUT=10s` 是有意设置：RPC stdio 是单线程，超时太长会堵塞管道
- `smbstorage.SMBStorage` 只是 `Storage` 到 `SMBClient` 的适配，不直接碰 `smbclient`；新能力加进 `pybase/smb/stream.py`，不要在 smbstorage 里另起一套连接/认证
- 只有 `shares.py` 用 `pysmb`（列共享），其余都走 `smbprotocol`；两库已整库拷进 `py/pybase/_vendor`（`py/scripts/vendor_smb.py` 生成，外连已换成 wgnet，勿手改）

### office
- 只放基础能力（协议、客户端、Word COM 驱动）；服务编排和部署在独立项目 `../wordpdf`，协议改动两边一起改
- `word.py` 顶层只 import 标准库，`win32com`/`pythoncom` 延迟 import
- 非致命 warnings 必须原样返回给调用方，不能吞
- 客户端只重试"连不上"和 503，其余错误不重试

### subtitle
- 借鉴外部方案先查协议：asbplayer（AGPL）、mpvacious（GPL）只能借语义，不能抄/改写代码
- VTT 交给 pysubs2 前必须先剥逐词时间戳 `<00:00:01.000>`，否则整行被丢（YouTube 自动字幕满篇都是）
- 滚动字幕相邻 cue 首尾相接不重叠，不能拿"时间重叠"当判据；`looks_rolling` 判否必须原样返回
- `shift` 严格按给定偏移、允许负时间，负值只在写文件时丢弃/截断，不要改成"收窄偏移"
- 来源：原字幕文件永不修改，来源历史和用户修正都在旁车 `.meta.json`；生效优先级 user > detected > acquired（证据优先于平台声明，X 把识别结果当普通字幕给），
  历史只追加；`detect_origin` 没证据必须返回 None，绝不当成 uploaded，且要喂未剥时间戳的原始文本
- 修正锚点一律 `anchor_of(cue)`（original_start/end + original_text），不用当前文本/序号；
  顺序 load → collapse_rolling → apply_corrections → shift，修正里的时间是平移前的时间轴
- 变换 Cue 用 `dataclasses.replace`，别手写构造——漏字段会悄悄丢掉原文/修正标记
- `load` 默认 `strict=True`：SRT/VTT 时间行交给 pysubs2 前先按规范校验（pysubs2 对越界时间戳照单全收）
- `provenance.Meta.offset_ms`（自动对齐/手动整体偏移）只存数、不编排；旧 `.meta.json` 没这字段读成 0

### media
- 子进程必须独立进程组、整组杀：ffsubsync 自己会起 ffmpeg，只杀父进程留孤儿
- 临时产物名保留扩展名（`x.part.mp3`），ffmpeg/ffsubsync 按扩展名猜格式
- 解析 ffsubsync 输出只认源码里的原文 `offset seconds:`，别用宽松正则（会吃到质量告警里的数）
- `frames_at` 抽不到帧（超出时长、末尾无帧）不报错，返回 `skipped` 原因；ffmpeg 非 0 退出才抛——别把两者合并
- ffprobe 分不清 mkv/webm（都报 `matroska,webm`），按 WebM 编码白名单区分，不能因调用方写了 webm 就放行 h264 的 mkv
- mp3/m4a 的封面图是一条 `attached_pic` 视频流，判可播性时要忽略，否则播客音频全被判成要转码
- `playback_plan` 不带默认支持集合（各播放端不同）；`-movflags +faststart` 只能给 mp4 系输出

### audio
- `offset_ms = 混音时间 - 麦克风时间`，别反过来；`attribute` 输出的是混音时间
- 对齐的包络是有意不对称的：麦克风用幅度（压回声）、混音用"高出阈值多少 dB"（防更响的远端抢峰），
  统一成一种会退化（README 里有对比表）
- 归属里"是不是本机"看 210 ms 平滑后的能量比，"有没有远端"逐帧看；别把前者改成逐帧
  （回声晚几十毫秒，逐帧比在远端独白里乱跳，会冒出假 both），也别把后者改成平滑（同时说话就看不出来）
- 阈值改了要重跑 `python tests/audio_synth.py`，把 README 的表一起更新
- `pybase/audio/__init__` 用 `__getattr__` 延迟加载，import 包不能连带 numpy；两小时长轨测试靠
  `PYBASE_LONG_AUDIO_TEST=1` 打开（只在 Studio）

### dictionary
- 不内置任何词典数据，每条 `Entry` 必带 `source`/`rights`；新数据源先查数据本身的授权，不只看代码协议
- macOS：除 `DCSCopyTextDefinition` 外全是私有符号，必须特性探测；不指定词典的 `lookup` 不能碰私有符号
- 列表里的词典不一定能查：`DCSDictionaryGetURL` 为空即未下载
- 从 CFSet 里取出的 DCSDictionaryRef 不要带出 `_dictionary` 的 with 块；`DCSRecordCopyData` 必须传 version
- 测试样本不能放词典原文（出版方版权），照真实 class 结构手写假词条
- `apple_xhtml` 只认牛津系 class；ElementTree 元素的真值看有没有子节点，判空必须 `is None`，不能用 `or` 串

### asr
- 不内置模型/locale 默认值；两个后端（Apple、mlx）接口必须保持一致，新后端照 `Stream`/`Transcript` 实现
- Apple：SpeechAnalyzer 只有 Swift API，逻辑尽量留在 Python，Swift 辅助程序只做采集结果、按行吐 JSON；
  改协议两边一起改。Locale 比较用 BCP 47（`Locale.identifier` 是 `zh_CN`，直接比永远不相等）
- mlx：MLX 的 GPU stream 绑定线程，加载/识别/流式 `step` 必须在同一个线程（实例的专属线程），
  别改成调用方线程或多线程池
- 实时会话 with 退出只收尾不取结果，剩下的 final 留给 `results()`——别改回 `close()` 里清队列
- `AppleSpeech`/`MlxAudio.transcribe` 返回的 `Transcript`（`cues` 是 `pybase.subtitle.Cue`）不改形状：ai-browser 按
  `isinstance(cue, pybase.subtitle.Cue)` 校验。远程识别和 `apple.transcribe` 用 `types.Cue`（`start_ms`/`end_ms`/`speaker`），
  两边靠 `from_subtitle`/`from_transcript` 互转，别把一个塞进另一个
- remote：HTTP 只走 `wgnet.http`（xxyi 进程是 strict，requests/urllib 会绕过 wgtund）；`checkpoint_dir` 必填，
  清理只删本模块自己的文件名（`seg-NNNN.json`、`refine.json`、`audio-16k.*`），目录空了才 rmdir，绝不 rmtree
- remote：16k 单声道 16 位 WAV 的路径上不许出现 ffmpeg 调用（调用方会在不许做媒体处理的机器上跑）；
  ffmpeg 只在输入不是这种 WAV 时转一次。重叠去重按句子**中点**归段，小样余量不出段、不过相邻句空隙一半
- 语言：remote 发主语言码、None 就不带（不内置语种默认）；`apple.transcribe` 要具体 locale，裸 `zh` 抛错

### digest
- 时间戳只能由程序按句子编号从 cues 查回，别改成让模型直接写时间（会编造/对不上）
- 不 import `aiclient`：模型调用经 `complete` 注入；越界/反向编号的条目丢弃，不猜着修

### textsplit
- 不调模型、不 import xxyi / `aiclient`：配方（`p_atom`）只交出 prompt/schema/造块/verify/merge，跑块的是调用方的 `taskmodel.run`
- 给模型看的文字全在 `p_atom_text.py`，逐字照抄 xxyi-work `xxyi_work/biz/PLAN-testrec-v2.md` 第 4 节；改字、补设计稿没写的话都要先经指挥，
  不许自己编一句回给模型。每条校验都有设计稿定的话，超限一律回给模型（`warnings` 只记「照收了最后一次输出」这类给人看的事）
- P-ATOM 的 schema 只用 taskmodel 自查认的 `minItems`/`maxItems`/`minLength`，不加 `maxLength`（字数上限由 verify 查）；字段顺序 quote、text、terms、doubt
- `trace` 只认「归一后是原文连着的一段」；近似比对（RapidFuzz / difflib）只出「最像的一句」作提示，绝不参与判定，别加「部分匹配也算过」
- 归一必须**逐字**做并留 `origin` 偏移表（`……` 等会改变长度）；补充表改了要连带看 `resegment` 的词对齐和 P-ATOM 的生词检查
- `chunk` 主体不重叠；超长行切出的片首尾相接拼回整行；`resegment` 只切不并、无全局状态（行身份是调用方给的 `seq` + `part`）
- RapidFuzz 每次调用现查（`fuzzy_backend()`），测试屏蔽导入走 difflib；不要在模块顶层缓存后端

### outbox
- 不带任何目录、代理命令、敏感串来源、Claude 能读的目录与附加提示词的缺省（xxyi 传）；提示词、回复、命中串都不打印、不写日志，
  异常文字里不带它们；`ask` 的范围不在只报个数（路径可能带资料名）
- 闸门查不了（来源目录不在、一个串都没取到、来源出错）一律 `GateUnavailable`，发给 Claude、转回发起方都拒（force 也不行），
  别改成「查不了就放过」；发给 omlx 不查
- 每边的会话号只在这一边某轮跑完**回了**会话号时换（没跑起来 / 停下 / 超时的不换）；每边同时最多一轮，判断和插入同一个 `BEGIN IMMEDIATE` 事务
- Claude 一边：`--restricted` + `--tools Read,Glob,Grep` + `--strict-mcp-config` + `disableAllHooks`，环境去 `ANTHROPIC_*`/`CLAUDE_CODE_*`；
  `CLAUDE_CODE_TMPDIR` 一定要指到自己的空目录（claude 自己的临时目录它总能读，缺省是同一工作目录下所有会话共用的 `/tmp/claude-<uid>/…`）；
  system/init 里工具多了或有 MCP 就停（护栏，别删）
- 发起方只能经 `take_reply` / `wait_reply` 拿转回的话；`ask` 的 stdout 不许出现两边的回复和过程
- 子进程 `start_new_session`，停下 / 超时 `killpg` 整组；别的进程跑的轮（`ask` 起的后台进程）停下 = 先标出错、再 `killpg` 它的代理，
  后台进程记代理进程号时条件更新不成就自己停；`recover` 只动跑它的进程已经没了的轮

### search
- FTS5 trigram 对不足 3 字符的查询直接返回空，短词走 LIKE 兜底——中文最常见的 1~2 字查询全靠它
- 查询词一律包成 FTS5 引号短语转义，不手写语法逃逸表
- 向量按 `embed_model` + 维度过滤，不兼容的逐行跳过不报错（换模型后旧片段自然失效）；
  语义候选先截断再进 RRF，否则全库弱相关片段会稀释排名
- `LlmClient.embed` 用独立的 `embeddings_path`，不能从 chat 的 endpoint 反推

### net
- 只读采集绝不改网络设置；改路由/地址的命令一律走注入的 `runner`（root 由调用方解决）
- UAPI 的密钥是 hex，`.conf` 里是 base64，两套编码别混；`last_handshake_time_sec=0` 表示从未握手，解析成 None
- 真实 wireguard-go 的 socket 目录固定 `/var/run/wireguard`，`uapi_dir`/`WG_UAPI_DIR` 只是测试钩子
- `classify_address` 必须先判 fake_ip/cgnat 再判 private：stdlib 把 198.18/15 算 private、100.64/10 不算
- 采集器失败返回 `{"error": ...}` 不抛异常；recorder 按"最新值"哈希去重，不是比相邻两次快照
- 实际默认出口要对公网地址做最长前缀匹配：mihomo TUN 用 1/8…128/1 拆分路由盖过 `default` 而不改它，
  只认字面 `default` 会把 Clash 接管误判成直连；macOS 带 `I` 标志的按接口限定路由不参与全局选路
- 接口归属：WireGuard.app（App Store 版）`scutil --nc` 拿不到接口名/地址，只能靠调用方给的已知地址匹配；
  `owners`/`scenario`/`targets` 是派生采集器，由 `Monitor` 的 derived 阶段在同一轮按顺序算
- `scene`（用户命名、手动进入/退出）与 `scenario`（自动识别的组合）是两套机制，别合并；
  `delete_scene` 是软删，历史观测的 `scene_id` 保留编号不置空，否则历史断链
- 系统配置采集（`sysconfig`/`sysfiles`）优先于软件状态：SCDynamicStore 用一次 list + 一次批量 show 的
  `scutil` 交互调用读全，不逐键起进程；服务 UUID 键名原样保留（是"谁改的"线索），展示时用 `service_names` 翻译
- `ipconfig getsummary` 内嵌的 DHCP 报文自带花括号，只能按行抓字段；SSID/BSSID 被系统隐藏记为 "redacted"，不算失败
- `diff.py`/`events.py` 共用同一套逐段比较函数（events 从 diff 复用），改规则只改 diff；dynamic_store 的
  易变噪声（LinkQuality、扫描缓存、租约时间等）在 `_NOISE_*` 常量里，发现新噪声往里补
- 实时监听（`watch.NetWatcher`）：长驻 `scutil` 不能关 stdin（EOF 等于 quit，n.watch 里也会退出）；
  子进程赋值后要再查一次 stop_event，否则紧接着的 stop() 可能杀不到刚起的进程
- 路由套接字大量消息只是查询/缓存（RTM_GET/RTM_MISS、带 LLINFO/WASCLONED 的邻居缓存），默认 `is_config_change`
  过滤掉，否则淹没真变化并触发多余的即时采集
- 事件的 `source` 只表示"哪台机器"（本机 local）；监听来源 route/store 并进 kind（`live_route_*`/`live_store_*`）
- `diff.py` 比的是"配置"：忽略 `expire`、带 `%` 的链路本地路由、mihomo 连接/流量计数、owners 的 evidence

### cmdproto
- 只有机制：注册表数据、实现表 `impls`、原子表 `atoms`、`computed_defaults` 全由调用方注入，不内置任何指令/原子/缺省；
  项目策略（来源筛选、路径白名单、取最新）写在 `Session` 子类的钩子里（`prepare`/`check`/`before_execute`/`after_execute`/`finish`），别加回本库
- `MISSING` 与 `None` 分开：None 是合法空值，MISSING 是"没有这个值"；原子算不出返回 MISSING，解析层据此回退/报缺参数，不猜
- 条件为假/取不到的名字 = 这行不用（回退到下一行），与"缺参数要报清楚"是两套口径，不要合并
- `prio` 空当 1，0 是合法值（不能 `or 1`）；人工值的缺省行是 `call` 为空的绑定行
- 注册表的 kind/layer/required 用中文字面量（「原子输出」「人工」「L0原子」「是」），是数据约定的一部分，改了要连带改调用方的注册表

### cdpnet / playwright_launch
- 纯映射/判断函数，无状态、不带项目默认值；有状态的编排（事件聚合、并发控制、落库）留给调用方

## jsbase

### electron 台账
- key 规则（见 `js/README.md`）一旦定下不要改，改了要连带迁移第 2、3 层的挂靠数据
- `modules.js` 只管收录清单，模块名不要散进提取逻辑里
- `capabilities.json` 是手写的，机器只校验、不生成
- 断代层（`history/`）不做归类判断，"这算不算一个能力"是能力台账层的事
- `historyOf` 里 `linkedVia: 'mention'` 是启发式关联，不保证准确

### 运行时地基
- Electron 对象、代理工厂、CA 目录、状态文件路径一律由调用方注入，不写死
- `ca.js` 生成的 PEM 必须 `normalizePem`（`\r\n`→`\n`）且主体名保持纯 ASCII，否则 TLS 握手会报
  看不懂的 base64 错误
- `recording-proxy` 的重试逻辑不许放宽 TLS `minVersion`/`ciphers`（`@SECLEVEL` 在 BoringSSL 下
  会同步抛错，导致主进程崩溃）
- `contentSize()` 返回的是物理像素（已乘 `scaleFactor`），调用方不要再乘一遍，否则 Retina 录像会糊
- `RecordingProxy.stop`/`dispose` 必须先 destroy 所有 socket 再 close server，顺序反了会挂死

### media（网页播放器）
- 播放计划的所有模式都按 `[prevMs, nowMs]` 区间穿越判断，不能只看瞬时值：否则暂停后续播反复触发，
  循环模式下用户拖进度条会被拽回来
- 驱动用 `requestVideoFrameCallback`，`timeupdate` 只是降级（每 150–250ms 一次，逐句暂停会停过头）；
  rVFC/rAF 暂停后不再触发，靠 `play` 事件重挂；外部改 `currentTime` 靠 `seeked` 拉回基准
- Electron 本地媒体协议必须 `stream: true` 且自己处理 Range（electron#38749，否则拖不动）；
  不给 `bypassCSP`；哪些文件能读由调用方 `resolvePath` 决定
- `segmentWords` 会把 `take-off` 拆成三段、`don't` 不拆，合并短语是界面层的事

### storage / fileops / retry / inventory
- 上层逻辑只认 `Storage` 接口，不许在 fileops/inventory 里写死本地路径或某种协议；连接方式（挂载、SMB 协议、HTTPS、
  隧道）只出现在 `Storage` 实现里
- 网络盘掉线后挂载点目录常常还在、只是空的：`LocalStorage` 判可用要给 `mount_point` 查 `ismount`，不能只看 `is_dir`；
  `stat` 遇连接问题必须抛 `OSError`，不能返回 None（None 表示"确实不存在"，会被当成"对方没有"）
- 拷贝只有 `copy_file` 一个函数，校验是 `verify` 开关（默认关），不要再加 `copy_verified` 之类的平行函数
- `copy_file` 目标已存在且判定不同一律 `conflict`，不加"强制覆盖"开关；正式名只在临时文件写完后 `replace`
- `retry_call` 资源不可用期间不计次数；等待间隔没有默认值（交互几秒、过夜批处理十几分钟，由调用方定）
- 可续传拷贝是 `copy_file_resumable`（不是 `copy_file` 的开关）：续传只信 `ResumeState` 里调用方持久化的块哈希，且必须先读回最后一块核对；暂停/取消靠进度回调抛异常
- `transfer` 队列：断线 `waiting` 的任务在每次探测之间也要检查更高优先级的 queued 任务并让出，否则连不上的任务把串行队列堵死
- `transfer` 队列：库是唯一真相（暂停/取消直接写库，worker 每块读一次状态）；恢复 running/waiting 只在抢到 advisory lock 之后做，否则会把别的进程正在跑的任务踢回队列；非连接 `OSError`（权限/不存在/磁盘满）不能进 waiting，否则会无限等
- inventory 的"同名+大小"只是快速判断，不是内容证明；修改时间不参与比对

### timemachine
- 系统常已把快照挂在 `/Volumes/.timemachine/<UUID>/`，再 `mount_apfs` 同一快照报 Resource busy：先查挂载表复用
- 自己挂一律 `rdonly,nobrowse`；快照里卷名随系统版本不同（`Macintosh HD - Data`/`Data`），不要写死

### db
- 只放零业务的方言无关操作；业务表结构、升级脚本、身份取值都留在调用方
- 函数的 `bind` 可传 Engine / Connection / Session：给 Engine 自己提交，给 Connection/Session 并入调用方事务，绝不自行提交
- SQLite 引擎在 `make_engine` 里关掉 pysqlite 隐式事务并显式 BEGIN，DDL 才能随事务回滚；别绕开 `make_engine` 自己 `create_engine`
- SQLite 删唯一约束/改可空性只能重建表（`schema._rebuild_sqlite`），只带普通列索引，表达式/部分索引、触发器不带
- upsert 的受影响行数靠 `RETURNING` 数，不用 `cursor.rowcount`（psycopg 下带自增主键时是 -1）
- 身份标记不覆盖：已有且不同一律抛 `IdentityMismatch`；无标记行为由 `check_identity(on_missing=)` 选
- PostgreSQL 测试连本机、建 `pybase_test_*` 临时库，连不上 skip
- 审计（`audit`）仅 PostgreSQL：触发器挂业务表，写入 `audit.row_log`；SQLite 上返回 False/空列表不报错。上下文键靠 `SET LOCAL`（`set_audit_context` 要传 Connection/Session），`make_engine(application_name=)` 区分写入方

## gobase

- 模块路径 `github.com/WillyChen0811/base/go`；本机 Go 工具链在 `~/.local/go`
- 必须能交叉编译到 `darwin/arm64` 和 `ios/arm64`（`CGO_ENABLED=0`）：不用 cgo，不碰系统 tun/路由，网络一律走程序内网络栈
- WireGuard 的程序内网络栈在主模块 `golang.zx2c4.com/wireguard` 的 `tun/netstack` 包里；
  不要 `go get golang.zx2c4.com/wireguard/tun/netstack`，那会拉到 2022 年单独发布的旧模块
- 监听一律只绑 127.0.0.1；测试只用本机回环和程序内网络栈，不需要 root
- 依赖只用 MIT/BSD/Apache/ISC 这类宽松协议，新增前先看模块缓存里的 LICENSE

