"""Word→PDF 转换服务的客户端 —— 跨平台，纯 requests。

输入输出都是 bytes，不落盘：docx 字节直接作为请求体发出，PDF 字节直接从响应体
读回。协议细节（query 参数、错误 JSON 格式、错误码含义）见 `pybase.office`
模块文档字符串。
"""

import json
import logging
import time
from dataclasses import dataclass, field
from typing import Optional
from urllib.parse import unquote

import requests

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

logger = logging.getLogger(__name__)

# 服务端状态码 -> 客户端异常类型的直接映射。不在表里的（比如 500 conversion_failed）
# 走 _map_error 里的兜底逻辑。
_STATUS_TO_ERROR = {
    401: AuthenticationError,
    413: PayloadTooLargeError,
    503: ServiceBusyError,
    504: ConversionTimeoutError,
}


@dataclass
class ConvertResult:
    """一次成功转换的结果。

    warnings 是服务端转换过程中遇到的非致命问题（比如某个 TOC 更新失败），
    这类失败服务端不会悄悄吞掉——收到的 PDF 状态码是 200，但目录页码之类的
    东西可能不准，调用方该按需要提示用户或记日志，别当作"完全正常"。
    """
    pdf_bytes: bytes
    page_count: int
    warnings: list = field(default_factory=list)


class WordPdfClient:
    """Word→PDF 转换服务的客户端。

    **只对"连不上"和"忙（503）"做有限次退避重试**——这两种是暂态，等一下大概率
    能好；鉴权失败、请求过大、转换失败（500）都是确定性的，同一份 docx 重试
    结果不会变，不重试，直接把异常抛给调用方。

    `timeout` 是单次 HTTP 请求的等待上限，要盖过服务端 `--task-timeout` 加一点
    余量（服务端会在到点后自己返回 504，不需要客户端去猜服务端超时点），否则
    客户端会先于服务端超时而拿不到明确的错误分类。
    """

    def __init__(
        self,
        endpoint: str,
        token: Optional[str] = None,
        timeout: float = 180.0,
        max_retries: int = 3,
        retry_interval: float = 2.0,
    ):
        self.endpoint = endpoint.rstrip("/")
        self.token = token
        self.timeout = timeout
        self.max_retries = max_retries
        self.retry_interval = retry_interval

    def _headers(self) -> dict:
        headers = {}
        if self.token:
            headers["X-Office-Token"] = self.token
        return headers

    def convert(self, docx_bytes: bytes, update_fields: bool = True) -> ConvertResult:
        """转换一份 docx。update_fields=True 时服务端会更新域/目录并重新分页
        （生成端 python-docx 算不出真实页码，这是拿到准确页码的唯一办法）。"""
        url = f"{self.endpoint}/convert"
        params = {"update_fields": "1" if update_fields else "0"}

        attempt = 0
        while True:
            attempt += 1
            try:
                resp = requests.post(
                    url, params=params, data=docx_bytes,
                    headers=self._headers(), timeout=self.timeout,
                )
            except requests.Timeout as e:
                # 客户端本地等到没等到——不重试：如果是服务端卡住了，重试大概率
                # 还是卡住（队列里排在后面）；如果只是网络抖了一下，交给下面
                # 的 RequestException 分支处理更合适，Timeout 单独判是为了不
                # 把它误判成"连不上"从而做无意义的重试消耗调用方的耐心。
                raise ConversionTimeoutError(f"客户端等待超时（{self.timeout}s 未收到响应）: {e}") from e
            except requests.RequestException as e:
                if attempt <= self.max_retries:
                    logger.warning(
                        "连接 office 服务失败，%.1fs 后重试(%d/%d): %s",
                        self.retry_interval, attempt, self.max_retries, e,
                    )
                    time.sleep(self.retry_interval)
                    continue
                raise ConnectionUnavailableError(f"连不上 office 服务 {self.endpoint}: {e}") from e

            if resp.status_code == 200:
                page_count = 0
                try:
                    page_count = int(resp.headers.get("X-Page-Count", 0) or 0)
                except ValueError:
                    pass
                warnings = self._parse_warnings(resp)
                return ConvertResult(pdf_bytes=resp.content, page_count=page_count, warnings=warnings)

            error_code, message = self._parse_error(resp)

            if resp.status_code == 503 and attempt <= self.max_retries:
                logger.warning(
                    "office 服务繁忙(503)，%.1fs 后重试(%d/%d): %s",
                    self.retry_interval, attempt, self.max_retries, message,
                )
                time.sleep(self.retry_interval)
                continue

            raise self._map_error(resp.status_code, error_code, message)

    def health(self) -> dict:
        url = f"{self.endpoint}/health"
        try:
            resp = requests.get(url, headers=self._headers(), timeout=self.timeout)
        except requests.RequestException as e:
            raise ConnectionUnavailableError(f"连不上 office 服务 {self.endpoint}: {e}") from e
        if resp.status_code != 200:
            error_code, message = self._parse_error(resp)
            raise self._map_error(resp.status_code, error_code, message)
        return resp.json()

    @staticmethod
    def _parse_error(resp) -> "tuple[str, str]":
        try:
            body = resp.json()
            return body.get("error", "unknown"), body.get("message", resp.text)
        except ValueError:
            return "unknown", (resp.text or f"HTTP {resp.status_code}")

    @staticmethod
    def _parse_warnings(resp) -> list:
        """解析成功响应里的 X-Warnings 头（URL 编码的 JSON 数组）。解析不出来
        就当没有——warnings 本身是锦上添花的诊断信息，不该因为它解析失败而
        让一次成功的转换报错。"""
        raw = resp.headers.get("X-Warnings")
        if not raw:
            return []
        try:
            parsed = json.loads(unquote(raw))
            return parsed if isinstance(parsed, list) else [str(parsed)]
        except (ValueError, TypeError) as e:
            logger.warning("解析 X-Warnings 响应头失败（忽略）: %s", e)
            return []

    @staticmethod
    def _map_error(status_code: int, error_code: str, message: str) -> OfficeClientError:
        cls = _STATUS_TO_ERROR.get(status_code)
        if cls is not None:
            return cls(message)
        if error_code == "conversion_failed" or status_code >= 500:
            return ConversionFailedError(message)
        return OfficeProtocolError(f"HTTP {status_code} [{error_code}]: {message}")
