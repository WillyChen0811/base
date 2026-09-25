"""pybase.office —— Word→PDF 转换的基础能力：Word COM 驱动 + HTTP 协议 + 客户端

python-docx 算不出真实分页（目录页码、章节起始页），只有 Word 排一遍才知道。
- `word`：驱动 Word 把 docx 转成 pdf 并统计页数（仅 Windows + Office）
- `client`：调用转换服务的 HTTP 客户端（跨平台）
- 服务本身（队列、看门狗、HTTP、部署）在独立项目 wordpdf 里，遵循下面的协议

## HTTP 协议

`POST /convert`
    请求体：docx 原始字节（不是 multipart，Content-Type 服务端不检查）
    query 参数：`update_fields=1|0`，是否更新域（目录页码等），默认 1
    请求头（可选）：`X-Office-Token`，需与服务端构造参数一致；服务端不传
    token 则不校验
    成功：200，`Content-Type: application/pdf`，body 是 PDF 字节，
        响应头 `X-Page-Count` 带页数；转换过程中出过非致命问题（比如某个
        TOC 更新失败）时还会带 `X-Warnings`（URL 编码的 JSON 字符串数组）——
        状态码仍是 200，但内容可能不完全准，客户端 `ConvertResult.warnings`
        原样透出，不悄悄吞掉
    失败：非 200，`Content-Type: application/json`，
        body 形如 `{"error": "<错误码>", "message": "<...>"}`：

    | 状态码 | error 码 | 含义 |
    |---|---|---|
    | 400 | bad_request | 请求体为空 / Content-Length 非法 |
    | 401 | unauthorized | token 校验失败 |
    | 413 | too_large | 请求体超过服务端配置的上限 |
    | 503 | busy | 转换队列已满，客户端应退避重试 |
    | 504 | timeout | 转换超时，服务端已强杀并重建 Word 实例 |
    | 500 | conversion_failed | Word 转换过程出错（COM 异常等），message 原样带回 |
    | 500 | internal | 服务内部异常（如看门狗未按预期触发），极少见 |

`GET /health`
    200，body 形如：
    ```json
    {
        "word_alive": true,
        "queue_len": 0,
        "processed": 128,
        "last_error": null,
        "memory_backed": true,
        "work_dir": "R:\\\\"
    }
    ```
    `memory_backed` 是部署方的显式声明（CLI: `--work-dir-is-ramdisk`），不是
    服务端猜的——Windows 上没有可靠的通用办法判断一个盘符是不是内存盘，传了
    `--work-dir` 不代表那就是内存盘，不加这个开关一律如实报 `False`（风格上
    跟 `pybase.ramdisk` 的 `memory_backed` 一致：不确定就别自称内存盘）。

## 客户端异常分类

见 `pybase.office.errors`：`ConnectionUnavailableError` / `ServiceBusyError` /
`ConversionTimeoutError` / `ConversionFailedError` / `AuthenticationError` /
`PayloadTooLargeError` / `OfficeProtocolError`。重试策略见 `WordPdfClient`。

## 用法

```python
from pybase.office import WordPdfClient

client = WordPdfClient("http://192.168.1.50:8765", token="xxx")
result = client.convert(docx_bytes, update_fields=True)
result.pdf_bytes   # PDF 字节
result.page_count  # 页数
```
"""

from .client import ConvertResult, WordPdfClient
from .errors import (
    AuthenticationError,
    ConnectionUnavailableError,
    ConversionFailedError,
    ConversionTimeoutError,
    OfficeClientError,
    OfficeProtocolError,
    PayloadTooLargeError,
    ServiceBusyError,
)
from .word import WordCOMConverter, WordConverter

__all__ = [
    # 客户端
    "WordPdfClient",
    "ConvertResult",
    # 客户端异常
    "OfficeClientError",
    "ConnectionUnavailableError",
    "ServiceBusyError",
    "ConversionTimeoutError",
    "ConversionFailedError",
    "AuthenticationError",
    "PayloadTooLargeError",
    "OfficeProtocolError",
    # Word 驱动（真正跑起来仅限 Windows，但在其它平台也能安全 import）
    "WordConverter",
    "WordCOMConverter",
]
