"""OpenAI 兼容接口客户端(标准库实现,filecore 与 backend 共用)。

只做 chat.completions 一件事:非流式取完整文本,流式逐 delta 回调。
json_mode 请求失败时自动降级为普通模式重试一次(部分本地服务不支持
response_format)。
"""
import json
import threading
import urllib.error
import urllib.request
from typing import Callable, List, Optional


class AiCanceled(Exception):
    """任务被取消"""


class AiRequestError(Exception):
    """AI 接口调用失败"""


def _join_endpoint(base_url: str, path: str) -> str:
    return f"{(base_url or '').rstrip('/')}/{(path or '').lstrip('/')}"


class LlmClient:

    def __init__(
        self,
        base_url: str,
        path: str = "/v1/chat/completions",
        api_key: str = "",
        model: str = None,
        temperature: float = 0.2,
        timeout: float = 600,
    ):
        if not base_url:
            raise AiRequestError("AI 接口地址未配置,请到 AI 设置页填写")
        self.endpoint = _join_endpoint(base_url, path)
        self.api_key = api_key or ""
        self.model = model or None
        self.temperature = temperature
        self.timeout = timeout

    def chat(
        self,
        messages: List[dict],
        json_mode: bool = True,
        on_delta: Optional[Callable[[str], None]] = None,
        cancel_event: Optional[threading.Event] = None,
    ) -> str:
        """发送对话请求,返回完整回复文本"""
        self._check_cancel(cancel_event)
        try:
            return self._request(messages, json_mode, on_delta, cancel_event)
        except AiRequestError:
            if not json_mode:
                raise
            self._check_cancel(cancel_event)
            return self._request(messages, False, on_delta, cancel_event)

    # ---------- 内部 ----------

    @staticmethod
    def _check_cancel(cancel_event):
        if cancel_event is not None and cancel_event.is_set():
            raise AiCanceled("任务已取消")

    def _request(self, messages, json_mode, on_delta, cancel_event) -> str:
        body = {
            "temperature": self.temperature,
            "messages": messages,
            "stream": bool(on_delta),
        }
        if self.model:
            body["model"] = self.model
        if json_mode:
            body["response_format"] = {"type": "json_object"}
        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        request = urllib.request.Request(
            self.endpoint,
            data=json.dumps(body).encode("utf-8"),
            headers=headers,
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                if on_delta:
                    return self._read_stream(response, on_delta, cancel_event)
                data = json.loads(response.read().decode("utf-8"))
                return (
                    (data.get("choices") or [{}])[0]
                    .get("message", {})
                    .get("content", "")
                    or ""
                )
        except urllib.error.HTTPError as exc:
            detail = ""
            try:
                detail = exc.read().decode("utf-8", errors="replace")[:300]
            except Exception:
                pass
            raise AiRequestError(
                f"AI 接口返回 {exc.code}: {detail or exc.reason}"
            ) from exc
        except urllib.error.URLError as exc:
            raise AiRequestError(f"无法连接 AI 接口 {self.endpoint}: {exc.reason}") from exc
        except TimeoutError as exc:
            raise AiRequestError(f"AI 接口 {self.endpoint} 响应超时") from exc

    def _read_stream(self, response, on_delta, cancel_event) -> str:
        """逐行解析 SSE,拼出完整文本并逐 delta 回调"""
        full = []
        for raw_line in response:
            self._check_cancel(cancel_event)
            line = raw_line.decode("utf-8", errors="replace").strip()
            if not line.startswith("data:"):
                continue
            payload = line[len("data:"):].strip()
            if payload == "[DONE]":
                break
            try:
                chunk = json.loads(payload)
            except json.JSONDecodeError:
                continue
            delta = (
                (chunk.get("choices") or [{}])[0]
                .get("delta", {})
                .get("content")
            )
            if delta:
                full.append(delta)
                on_delta(delta)
        return "".join(full)
