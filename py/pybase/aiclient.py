"""OpenAI 兼容接口客户端(标准库实现,filecore 与 backend 共用)。

chat.completions:非流式取完整文本,流式逐 delta 回调。json_mode 请求失败时自动
降级为普通模式重试一次(部分本地服务不支持 response_format)。

embeddings:批量文本转向量,按返回的 index 排回请求顺序(部分实现不保证顺序);
返回数量跟输入数量不一致视为接口错误。
"""
import json
import threading
from typing import Callable, List, Optional

from . import wgnet


class AiCanceled(Exception):
    """任务被取消"""


class AiRequestError(Exception):
    """AI 接口调用失败"""


def _join_endpoint(base_url: str, path: str) -> str:
    return f"{(base_url or '').rstrip('/')}/{(path or '').lstrip('/')}"


def _http_error(response) -> str:
    try:
        detail = response.body[:2000].decode("utf-8", errors="replace")[:300]
    except Exception:
        detail = ""
    return f"AI 接口返回 {response.status}: {detail or response.reason}"


class LlmClient:
    """OpenAI 兼容 chat.completions 客户端，标准库实现（无第三方依赖）。"""

    def __init__(
        self,
        base_url: str,
        path: str = "/v1/chat/completions",
        api_key: str = "",
        model: str = None,
        temperature: float = 0.2,
        timeout: float = 600,
        embeddings_path: str = "/v1/embeddings",
        headers: Optional[dict] = None,
    ):
        if not base_url:
            raise AiRequestError("AI 接口地址未配置,请到 AI 设置页填写")
        self.endpoint = _join_endpoint(base_url, path)
        # embeddings 走独立路径,跟 chat 的 path 各拼各的(不能从 self.endpoint 反推)
        self.embeddings_endpoint = _join_endpoint(base_url, embeddings_path)
        self.api_key = api_key or ""
        self.model = model or None
        self.temperature = temperature
        self.timeout = timeout
        # 每个请求都带上的附加请求头，如 oMLX 网关的 X-Xxyi-Priority / X-Xxyi-Client
        self.headers = dict(headers or {})

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

    def embed(
        self,
        texts: List[str],
        *,
        model: Optional[str] = None,
        cancel_event: Optional[threading.Event] = None,
    ) -> List[List[float]]:
        """把文本列表转成向量列表,按输入顺序返回(内部按响应的 index 排序)。"""
        self._check_cancel(cancel_event)
        if not texts:
            return []
        body = {"input": texts}
        use_model = model or self.model
        if use_model:
            body["model"] = use_model
        headers = self._headers()
        try:
            with wgnet.http.post(self.embeddings_endpoint, json.dumps(body).encode("utf-8"), headers,
                                 timeout=self.timeout) as response:
                if response.status >= 400:
                    raise AiRequestError(_http_error(response))
                data = response.json()
        except AiRequestError:
            raise
        except TimeoutError as exc:
            raise AiRequestError(f"AI 接口 {self.embeddings_endpoint} 响应超时") from exc
        except OSError as exc:
            raise AiRequestError(f"无法连接 AI 接口 {self.embeddings_endpoint}: {exc}") from exc

        items = data.get("data") or []
        if len(items) != len(texts):
            raise AiRequestError(
                f"AI 接口返回 {len(items)} 条向量,跟输入的 {len(texts)} 条文本数量不一致"
            )
        try:
            ordered = sorted(items, key=lambda item: item["index"])
        except (KeyError, TypeError) as exc:
            raise AiRequestError("AI 接口返回的向量缺少 index 字段") from exc
        return [item["embedding"] for item in ordered]

    # ---------- 内部 ----------

    def _headers(self) -> dict:
        headers = {**self.headers, "Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        return headers

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
        headers = self._headers()
        try:
            with wgnet.http.post(self.endpoint, json.dumps(body).encode("utf-8"), headers,
                                 timeout=self.timeout, stream=bool(on_delta)) as response:
                if response.status >= 400:
                    raise AiRequestError(_http_error(response))
                if on_delta:
                    return self._read_stream(response, on_delta, cancel_event)
                data = response.json()
                return (
                    (data.get("choices") or [{}])[0]
                    .get("message", {})
                    .get("content", "")
                    or ""
                )
        except AiRequestError:
            raise
        except TimeoutError as exc:
            raise AiRequestError(f"AI 接口 {self.endpoint} 响应超时") from exc
        except OSError as exc:
            raise AiRequestError(f"无法连接 AI 接口 {self.endpoint}: {exc}") from exc

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
