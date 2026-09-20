# pybase

零业务依赖的 Python 基础模块集：RPC 底座、内存盘、AI 客户端、docx 写作、表格读取、SMB、加密。

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
在其他平台会被自动跳过。
