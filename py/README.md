# pybase

零业务依赖的 Python 基础模块集：RPC 底座、内存盘、AI 客户端、docx 写作、表格读取、SMB、
加密、CDP/Playwright 纯函数、Word→PDF 转换、字幕与音视频。

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
- `pybase.office`：Word→PDF 基础能力：Word COM 驱动（Windows + Office）+ 转换服务客户端
- `pybase.subtitle`：字幕读写（pysubs2，`pip install "pybase[subtitle]"`）、YouTube 自动字幕滚动折叠、
  时间校准、时间索引（`CueIndex`）、给大模型的 `[MM:SS]` 文本与分块；字幕来源（人工/平台识别/本地识别/机器翻译）
  与用户修正层，存旁车 `.meta.json`，原字幕文件不改。时间单位一律整数毫秒
- `pybase.media`：ffmpeg/ffprobe/ffsubsync 子进程封装：探测、按句截音频（含 padding）/截图、字幕自动对齐、
  网页 `<video>` 可播性判断（`playback_plan`，支持集合由调用方给）与转封装（`remux`）。
  外部工具需自行安装，路径可由调用方传入
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
