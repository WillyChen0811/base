"""JSON-RPC 客户端

统一的 RPC 客户端，通过配置 endpoint 连接不同服务
"""

import json
import logging
from typing import Any, Dict, Optional
import http.client

from .. import wgnet

logger = logging.getLogger(__name__)


class RPCClient:
    """JSON-RPC HTTP 客户端"""

    def __init__(self, endpoint: str, timeout: int = 30):
        self.endpoint = endpoint.rstrip('/')
        self.timeout = timeout
        self._request_id = 0

    def _post(self, payload):
        return wgnet.http.post(self.endpoint, json.dumps(payload).encode("utf-8"),
                               {"Content-Type": "application/json"}, timeout=self.timeout)

    def call(self, method: str, **params) -> Any:
        """调用 RPC 方法（如 method="tableTemplate.get"），失败抛 RPCError。"""
        self._request_id += 1
        request_body = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": method,
            "params": params if params else {},
        }

        try:
            with self._post(request_body) as resp:
                if resp.status >= 400:
                    raise RPCError(-32000, f"HTTP error: {resp.status} {resp.reason}")
                response = resp.json()
        except RPCError:
            raise
        except (OSError, http.client.HTTPException) as e:
            raise RPCError(-32000, f"Connection error: {e}")
        except json.JSONDecodeError as e:
            raise RPCError(-32700, f"Invalid JSON response: {e}")

        if "error" in response:
            error = response["error"]
            raise RPCError(error.get("code", -32000), error.get("message", "Unknown error"))

        return response.get("result")

    def batch(self, calls: list) -> list:
        """批量调用，calls 为 [(method, params), ...]；单条失败对应位置返回 RPCError 而非抛出。"""
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
            with self._post(requests) as resp:
                if resp.status >= 400:
                    raise RPCError(-32000, f"Batch request failed: HTTP {resp.status} {resp.reason}")
                responses = resp.json()
        except RPCError:
            raise
        except (OSError, http.client.HTTPException, json.JSONDecodeError) as e:
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
