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
