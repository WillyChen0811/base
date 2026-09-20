"""Chrome DevTools Protocol 网络事件的纯映射/判断函数。

只回答"这个 CDP 事件/响应该记成什么字段""这个响应体要不要抓""base64 体怎么解码"，
不做聚合、不落库、不持有状态。事件按 requestId 的聚合（_pending）、重定向复用
requestId 时拆成两条历史记录的时机、并发信号量、flush 收尾等"何时调用、调用几次"
的流程判断，都留在调用方——那些是有状态的业务编排，不是本模块的事。

本模块不带任何项目默认值：mime 白/黑名单、体积上限、URL 关键词均由调用方传入。
"""
from __future__ import annotations

import time
from typing import Any
from urllib.parse import urlparse


def request_record(session_id: Any, event: dict, capture_post_data: bool) -> dict[str, Any]:
    """把 `Network.requestWillBeSent` 事件映射成一条记录的初始字段。

    不含重定向判断（同一 requestId 复用时要不要把上一跳先落库）、不含
    `_pending` 登记——那是调用方按 requestId 聚合的事。
    """
    req = event.get("request", {})
    u = urlparse(req.get("url", ""))
    return {
        "session_id": session_id,
        "request_id": event["requestId"],
        "frame_id": event.get("frameId"),
        "loader_id": event.get("loaderId"),
        "document_url": event.get("documentURL"),
        "resource_type": event.get("type"),
        "is_redirect": 0,
        "method": req.get("method"),
        "url": req.get("url"),
        "url_host": u.netloc,
        "url_path": u.path,
        "request_headers": req.get("headers", {}),
        "post_data": req.get("postData") if capture_post_data else None,
        "ts_request": event.get("timestamp"),         # CDP 单调时钟
        "wall_time": event.get("wallTime") or time.time(),  # 墙上时间，两者口径不同，不要统一
        "from_cache": 0,
        "from_service_worker": 0,
        "canceled": 0,
    }


def response_fields(response: dict, current_from_cache: int = 0) -> dict[str, Any]:
    """把 CDP response 对象（`Network.responseReceived` 或 `redirectResponse`）
    映射成要写回记录的字段。

    `from_cache` 只有响应带 `fromDiskCache` 时才置 1，否则沿用调用方传入的
    `current_from_cache`（例如 `Network.requestServedFromCache` 事件已经先标过）。
    """
    return {
        "status": response.get("status"),
        "status_text": response.get("statusText"),
        "mime_type": response.get("mimeType"),
        "response_headers": response.get("headers", {}),
        "remote_ip": response.get("remoteIPAddress"),
        "remote_port": response.get("remotePort"),
        "protocol": response.get("protocol"),
        "from_cache": 1 if response.get("fromDiskCache") else current_from_cache,
        "from_service_worker": 1 if response.get("fromServiceWorker") else 0,
        "ts_response": response.get("responseTime"),
    }


def body_decision(
    *,
    status: int | None,
    from_cache: bool | int,
    url: str | None,
    mime: str | None,
    size: int,
    max_bytes: int,
    mime_allow: list[str],
    mime_deny: list[str],
    force_url_hints: list[str],
) -> tuple[bool, str, int]:
    """判断一个响应体要不要抓，返回 (要不要, 原因, 这次实际生效的体积上限)。

    判断顺序（与原调用方逐条对应）：
    1. 204/304 无内容
    2. 命中磁盘缓存（录制时若关闭 `Network.setCacheDisabled` 则拿不到 body）
    3. URL 命中 `force_url_hints` 关键词（如字幕）→ 无视 mime 白名单，
       体积上限放宽 10 倍
    4. 命中 `mime_deny`
    5. 不在 `mime_allow` 白名单内
    6. 超过体积上限
    """
    u = (url or "").lower()
    m = (mime or "").lower()

    if status in (204, 304):
        return False, f"status {status}", max_bytes
    if from_cache:
        return False, "from cache", max_bytes

    for hint in force_url_hints:
        if hint.lower() in u:
            return True, f"hint:{hint}", max_bytes * 10

    for deny in mime_deny:
        if m.startswith(deny.lower()):
            return False, f"mime denied ({mime})", max_bytes

    if not any(m.startswith(a.lower()) for a in mime_allow):
        return False, f"mime not allowed ({mime or 'unknown'})", max_bytes

    if size > max_bytes:
        return False, f"too large ({size} B)", max_bytes

    return True, "", max_bytes


def decode_response_body(body: str, base64_encoded: bool) -> bytes:
    """把 `Network.getResponseBody` 的返回（`body` + `base64Encoded`）解码成字节。

    body 必须在 `Network.loadingFinished` 事件触发时**立刻**取——晚了 CDP 的
    响应体缓冲区就被清掉，会报 "No resource with given identifier found"。
    这个坑发生在调用 `Network.getResponseBody` 的时机上，不在这个解码函数里，
    调用方要自己保证时机对。
    """
    return body.encode("ascii") if base64_encoded else body.encode("utf-8")
