# base

零业务依赖的基础模块集，按语言分两个包：`py/`（pybase）与 `js/`（jsbase）。
所有项目相关的默认值都由调用方在自己的项目里传入，本仓库只提供机制。

项目约定与易踩坑点见 [CLAUDE.md](./CLAUDE.md)。

## pybase

RPC 底座、内存盘、AI 客户端、docx 写作、表格读取、SMB、加密、CDP/Playwright 纯函数、
Word→PDF 转换。详见 [py/README.md](./py/README.md)。

## jsbase

Electron 版本↔能力台账三层设计 + Electron/代理运行时地基。纯 Node 内置模块，
`node-forge` 是唯一运行时依赖。详见 [js/README.md](./js/README.md)。
