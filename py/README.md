# pybase

零业务依赖的 Python 基础模块集：RPC 底座、内存盘、AI 客户端、docx 写作、表格读取、SMB、
加密、CDP/Playwright 纯函数、Word→PDF 转换。

所有项目相关的默认值（密钥文件路径、内存盘名字等）都由调用方传入，本库只提供机制。

## 模块一览

- `pybase.rpc`：JSON-RPC 客户端/服务端底座（FastAPI 路由 / HTTP handler / stdio 三种服务端 + urllib 客户端）
- `pybase.ramdisk`：内存级临时盘生命周期管理，支持同名同路径的多进程共享
- `pybase.aiclient`：OpenAI 兼容 chat.completions 客户端（`LlmClient`）
- `pybase.docx`：docx 格式属性写入（`oxml`）与模板渲染（`render`）
- `pybase.sheets`：表格文件读取（`.xlsx`/`.xls`/`.csv`），统一读成 `[[str]]`，不做类型推断
- `pybase.smb`：SMB/WebDAV/File Station 三通道文件抓取 + SMB 客户端（连接、列目录、下载、上传、共享枚举）
- `pybase.crypto`：基于 Fernet 的密码加密/解密（`PasswordEncryption`，不带项目默认值）
- `pybase.cdpnet`：Chrome DevTools Protocol 网络事件的纯映射/判断函数
- `pybase.playwright_launch`：按 cfg 拼 Playwright 启动/建 context 参数的纯映射
- `pybase.office`：Word→PDF 转换服务，服务端（Windows + Office 2019）与跨平台客户端

## office 模块：Word→PDF 转换

python-docx 算不出真实分页，`pybase.office` 用专职的 Windows + Office 2019 虚拟机做局域网
HTTP 服务：docx bytes 进、PDF bytes + 页数出。协议细节见 `pybase/office/__init__.py` 的模块
文档字符串。

虚拟机部署（Windows 10 + Office 2019）：

1. 激活 Office，手动开一次 Word 点掉首启向导（否则自动化会卡住）
2. 信任中心关闭受保护视图三项（Internet 文件/不安全位置/Outlook 附件）
3. 自动登录桌面 + 计划任务「用户登录时」启动服务；**不要注册成 Windows 服务**（Session 0 下 Word COM 挂死）
4. 关闭 Windows/Office 自动更新的强制重启
5. 装内存盘（如 ImDisk），`--work-dir` 指过去并加 `--work-dir-is-ramdisk`（不加则 `memory_backed` 报 `false`）
6. 可选：账号的 `TEMP`/`TMP` 也指到内存盘

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

内存盘跨进程测试（`test_ram_disk_multiproc.py`）依赖 macOS 的 RAM disk 挂载语义，office
模块驱动真实 Word COM 的部分只能在装了 Office 的 Windows 机器上实测；不匹配的平台上相关
用例会被自动跳过。
