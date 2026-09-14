# base

零业务依赖的基础模块，从 [websubmit](https://github.com/) 抽出，供 websubmit 的
backend/filecore 以路径依赖方式引用。目前只有 Python 包 `pybase`（`py/`），JS 侧不受影响。

## 安装

在使用方（backend、filecore 等）的虚拟环境里可编辑安装：

```bash
pip install -e /Users/weiyichen/Documents/codes/base/py
```

## 内容

见 `py/pybase/__init__.py` 的模块 docstring。

## 测试

```bash
cd py && python -m pytest tests
```
