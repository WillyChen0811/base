# pybase

零业务依赖的 Python 基础模块集：RPC 底座、内存盘、AI 客户端、docx 写作、表格读取、SMB、加密、
Word→PDF 转换。

不依赖任何具体项目的业务逻辑或配置——所有项目相关的默认值（密钥文件路径、内存盘名字等）
都由调用方在自己的项目里传入，本库只提供机制。

## 模块一览

- `pybase.rpc`：JSON-RPC 客户端/服务端底座（业务路由、同步逻辑留在各项目自己的 rpc 包里）
- `pybase.ramdisk`：内存级临时盘（RAM Disk）生命周期管理，支持同名同路径的多进程共享
- `pybase.aiclient`：OpenAI 兼容接口的标准库客户端
- `pybase.docx`：docx 格式属性写入（`oxml`）与模板渲染（`render`）
- `pybase.sheets`：表格文件读取（`.xlsx`/`.xls`/`.csv`）
- `pybase.smb`：SMB 客户端与文件抓取（连接、列目录、下载、上传、共享枚举）
- `pybase.crypto`：基于 Fernet 的密码加密/解密（`PasswordEncryption` 类，不带项目默认值）
- `pybase.cdpnet`：Chrome DevTools Protocol 网络事件的纯映射/判断函数（事件聚合、
  落库仍由调用方负责）
- `pybase.playwright_launch`：按 cfg 拼 Playwright 启动/建 context 参数的纯
  dict→dict 映射
- `pybase.office`：Word→PDF 转换服务，服务端（仅 Windows + Office 2019，见下文
  「office 模块」一节）与跨平台客户端都在这里

### cdpnet / playwright_launch：从 ai-browser 搬来的纯函数

`cdpnet.py` 是从 ai-browser 的 `core/netcap.py` 里抽出来的——那份代码把 CDP 的一串
碎片网络事件按 requestId 聚合成一条完整记录再落库，其中"事件/响应该映射成什么字段"
"这个响应体要不要抓""base64 body 怎么解码"这几件事是没有状态的纯映射/判断，其余
（按 requestId 聚合、重定向复用 requestId 时拆分历史记录的时机、并发信号量、flush
收尾、落库）是有状态的业务编排，留在 ai-browser 自己决定。`playwright_launch.py`
同理，只收 `core/browser.py` 里"按 cfg 拼 launch/context 参数"这一小块纯映射，
连哪种模式（launch / launch_persistent_context / connect_over_cdp）、相对路径怎么
解析，都留在调用方。

不依赖 Playwright/CDP 之外的任何东西——`cdpnet` 只处理 CDP 事件的原始 dict，
不 import `playwright`；两个模块都不带项目默认值，mime 白/黑名单、体积上限、
URL 关键词、viewport 缺省值都由调用方传入。

```python
from pybase.cdpnet import request_record, response_fields, body_decision, decode_response_body
from pybase.playwright_launch import playwright_launch_args, playwright_context_args

rec = request_record(session_id, event, capture_post_data=True)
rec.update(response_fields(response_event["response"], rec.get("from_cache", 0)))

want, reason, max_bytes = body_decision(
    status=rec["status"], from_cache=rec["from_cache"], url=rec["url"], mime=rec["mime_type"],
    size=rec["encoded_data_length"] or 0,
    max_bytes=2 << 20, mime_allow=["application/json"], mime_deny=["text/css"],
    force_url_hints=[".vtt", "subtitle"],
)
raw = decode_response_body(body, base64_encoded)

launch_args = playwright_launch_args({"headless": True})
context_args = playwright_context_args({"width": 1440, "height": 900})
```

## office 模块：Word→PDF 转换

### 用途

python-docx 只管填内容，算不出真实分页——目录页码、章节起始页这些排版结果只有
Word 自己排一遍才知道。`pybase.office` 用一台专职的 Windows + Office 2019
虚拟机把"Word 排版 + 导出 PDF"做成局域网内的 HTTP 服务：调用方（backend、各端
客户端）把 docx 字节发过去，收回带页数的 PDF 字节，输入输出都是内存里的
bytes，不用落盘。协议细节（`/convert`、`/health`、错误码）见
`pybase/office/__init__.py` 的模块文档字符串。

### 虚拟机部署清单

一台 KVM 虚拟机装 Windows 10 + Office 2019 专业版，专职做转换：

1. **激活 Office**，并手动打开一次 Word，点掉首次启动向导/登录提示等一次性对话框
   （自动化打开文档时这些对话框会卡住整个服务）。
2. Word「文件 → 选项 → 信任中心 → 受保护视图」，关掉以下三项（不然 Word 会把
   自动打开的文档当成"来自不可信来源"用受保护视图打开，`ExportAsFixedFormat`
   在受保护视图下行为不可控）：
   - 为来自 Internet 的文件启用受保护视图
   - 为位于可能不安全位置的文件启用受保护视图
   - 为 Outlook 附件启用受保护视图
3. **自动登录桌面会话** + **计划任务「用户登录时」启动**
   `python -m pybase.office.server`。**不要注册成 Windows 服务**——Word COM
   自动化在 Session 0 / 服务账号下无法正常工作。
4. 关闭 Windows 自动更新的自动重启（或至少调整到维护窗口），关闭 Office 自动
   更新——转换服务是长驻进程，中途被强制重启/更新会打断在跑的任务。
5. 装内存盘（如 ImDisk）挂一个盘符，启动参数 `--work-dir` 指过去，并加
   `--work-dir-is-ramdisk` 显式声明——文档是检测报告，涉密，尽量不落盘
   （Word COM 只能打开路径，没法做到纯内存）。**这个声明不是摆设**：Windows
   上没有可靠的通用办法自动判断一个盘符是不是内存盘，服务端不会替你猜，只按
   这个开关如实上报 `/health` 的 `memory_backed`——传了 `--work-dir` 但忘了
   加这个开关，`memory_backed` 依然是 `false`，服务启动日志也会 warning 一句
   提醒；不传 `--work-dir` 时退化为系统临时目录，这个开关当然也不该加。
6. 可选：把这个用户账号的 `TEMP`/`TMP` 环境变量也指到内存盘——Word 自己也会在
   TEMP 目录写一些中间文件，同步挪过去能进一步减少落盘残留。

### 调用示例

```python
from pybase.office import WordPdfClient

client = WordPdfClient("http://192.168.1.50:8765", token="xxx")
result = client.convert(docx_bytes, update_fields=True)
result.pdf_bytes    # PDF 字节
result.page_count   # Word 实际排版后的页数
```

服务端启动（只能在部署好的 Windows 虚拟机上跑）：

```bash
pip install "pybase[office-server]"
python -m pybase.office.server --host 0.0.0.0 --port 8765 --work-dir R:\ --work-dir-is-ramdisk --token xxx
```

## 安装

**作为依赖引入**（推荐）：

```bash
pip install git+ssh://git@github.com/WillyChen0811/base.git#subdirectory=py
```

**本地开发**（克隆后可编辑安装）：

```bash
pip install -e ./py
```

## 测试

```bash
cd py && python -m pytest tests
```

内存盘的跨进程测试（`test_ram_disk_multiproc.py`）依赖 macOS 的 RAM disk 挂载语义，
在其他平台会被自动跳过。office 模块的测试（`test_office_client.py` /
`test_office_server.py`）用假 HTTP 服务/假 Word 驱动验证客户端重试与状态码映射、
服务端队列/看门狗/清理逻辑，不依赖真实 Word；真正驱动 Word COM 的部分只能在装了
Office 的 Windows 机器上实测，非 Windows 平台上相关用例会被自动跳过。
