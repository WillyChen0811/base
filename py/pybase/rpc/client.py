"""JSON-RPC 客户端

统一的 RPC 客户端，通过配置 endpoint 连接不同服务
"""

import json
import logging
from typing import Any, Dict, Optional
from urllib.request import urlopen, Request
from urllib.error import URLError, HTTPError

logger = logging.getLogger(__name__)


class RPCClient:
    """JSON-RPC HTTP 客户端"""

    def __init__(self, endpoint: str, timeout: int = 30):
        """
        Args:
            endpoint: RPC 服务地址 (如 http://localhost:8123/rpc)
            timeout: 请求超时时间（秒）
        """
        self.endpoint = endpoint.rstrip('/')
        self.timeout = timeout
        self._request_id = 0

    def call(self, method: str, **params) -> Any:
        """调用 RPC 方法

        Args:
            method: 方法名 (如 tableTemplate.get)
            **params: 参数

        Returns:
            调用结果

        Raises:
            RPCError: RPC 调用失败
        """
        self._request_id += 1
        request_body = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": method,
            "params": params if params else {},
        }

        try:
            req = Request(
                self.endpoint,
                data=json.dumps(request_body).encode('utf-8'),
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urlopen(req, timeout=self.timeout) as resp:
                response = json.loads(resp.read().decode('utf-8'))
        except HTTPError as e:
            raise RPCError(-32000, f"HTTP error: {e.code} {e.reason}")
        except URLError as e:
            raise RPCError(-32000, f"Connection error: {e.reason}")
        except json.JSONDecodeError as e:
            raise RPCError(-32700, f"Invalid JSON response: {e}")

        if "error" in response:
            error = response["error"]
            raise RPCError(error.get("code", -32000), error.get("message", "Unknown error"))

        return response.get("result")

    def batch(self, calls: list) -> list:
        """批量调用

        Args:
            calls: [(method, params), ...]

        Returns:
            结果列表
        """
        requests = []
        for i, (method, params) in enumerate(calls):
            self._request_id += 1
            requests.append({
                "jsonrpc": "2.0",
                "id": self._request_id,
                "method": method,
                "params": params if params else {},
            })

        try:
            req = Request(
                self.endpoint,
                data=json.dumps(requests).encode('utf-8'),
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urlopen(req, timeout=self.timeout) as resp:
                responses = json.loads(resp.read().decode('utf-8'))
        except (HTTPError, URLError, json.JSONDecodeError) as e:
            raise RPCError(-32000, f"Batch request failed: {e}")

        results = []
        for response in responses:
            if "error" in response:
                results.append(RPCError(
                    response["error"].get("code", -32000),
                    response["error"].get("message", "Unknown error")
                ))
            else:
                results.append(response.get("result"))

        return results


class RPCError(Exception):
    """RPC 错误"""

    def __init__(self, code: int, message: str):
        self.code = code
        self.message = message
        super().__init__(f"[{code}] {message}")


# ============ 便捷方法 ============

_default_client: Optional[RPCClient] = None


def configure(endpoint: str, timeout: int = 30):
    """配置默认客户端

    Args:
        endpoint: RPC 服务地址
        timeout: 超时时间
    """
    global _default_client
    _default_client = RPCClient(endpoint, timeout)


def get_client() -> RPCClient:
    """获取默认客户端"""
    if _default_client is None:
        raise RuntimeError("RPC client not configured. Call configure() first.")
    return _default_client


def call(method: str, **params) -> Any:
    """使用默认客户端调用"""
    return get_client().call(method, **params)
