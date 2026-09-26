"""pybase —— 零业务依赖的 Python 基础模块集。

子模块：
- rpc: JSON-RPC 客户端/服务端底座（业务路由、同步逻辑仍留在各项目自己的 rpc 包里）
- ramdisk: 内存级临时盘（RAM Disk）管理
- aiclient: OpenAI 兼容接口客户端
- docx: docx 格式属性写入（oxml）与模板渲染（render）
- sheets: 表格文件读取（.xlsx/.xls/.csv）
- smb: SMB 客户端与文件抓取（连接、列目录、下载、上传、共享枚举）
- crypto: 密码加密/解密（Fernet 对称加密）
- cdpnet: Chrome DevTools Protocol 网络事件的纯映射/判断函数
- playwright_launch: 按 cfg 拼 Playwright 启动/建 context 参数
- office: Word→PDF 基础能力（Word COM 驱动 + 转换服务客户端）
- subtitle: 字幕读写、YouTube 滚动字幕折叠、时间校准、时间索引、给大模型的时间戳文本
- media: ffmpeg/ffprobe/ffsubsync 子进程封装（探测、按句截音频/截图、字幕自动对齐）
- dictionary: 词典查询统一模型 + 数据源适配器（macOS 系统词典），不内置任何词典数据
"""
