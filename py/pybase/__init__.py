"""pybase —— 从 websubmit 抽出的零业务依赖基础模块。

子模块：
- rpc: JSON-RPC 客户端/服务端底座（业务路由、同步逻辑仍留在各项目自己的 rpc 包里）
- ramdisk: 内存级临时盘（RAM Disk）管理
- aiclient: OpenAI 兼容接口客户端
- docx: docx 格式属性写入（oxml）与模板渲染（render）
- sheets: 表格文件读取（.xlsx/.xls/.csv）
- smb: SMB 客户端与文件抓取（连接、列目录、下载、上传、共享枚举）
- crypto: 密码加密/解密（Fernet 对称加密）
"""
