# base 项目记忆

## 全库通用

- 零业务依赖：不写死任何项目相关的默认值（路径、密钥、CA 名字等），一律由调用方传入
- 重依赖（`win32com`/`pythoncom`、`electron` 等）延迟 import，模块顶层只 import 标准库
- 文档分层：README 只放不易变的模块一览/安装/测试/调用契约；会踩坑的约束进本文件；
  函数级说明进代码注释；实现细节和推导过程不写文档，留在代码里
- 测试（改动后必须跑）：
  - `cd py && python -m pytest tests`
  - `cd js && node --test "tests/**/*.test.js"`

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
- 上传必须走临时文件（`.wsu`）+ `replace` 原子替换；临时名被拒时降级为替换扩展名（见 `common._tmp_names`）
- `exists()` 是三态：`None` 表示判不出，不能当 `False` 用
- 固定使用 NTLM，不用 negotiate（Kerberos 在 IP 直连场景会导致 `ACCESS_DENIED`）
- `CONNECT_TIMEOUT=10s` 是有意设置：RPC stdio 是单线程，超时太长会堵塞管道
- 只有 `shares.py` 用 `pysmb`（列共享），其余都走 `smbprotocol`

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

### media
- 子进程必须独立进程组、整组杀：ffsubsync 自己会起 ffmpeg，只杀父进程留孤儿
- 临时产物名保留扩展名（`x.part.mp3`），ffmpeg/ffsubsync 按扩展名猜格式
- 解析 ffsubsync 输出只认源码里的原文 `offset seconds:`，别用宽松正则（会吃到质量告警里的数）

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
