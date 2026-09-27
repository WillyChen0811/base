"""pybase.aiclient.LlmClient 测试。

用本机随机端口起一个"剧本式"假 HTTP 服务器（跟 test_office_client.py 同一套路），
按预先编排好的响应应答，覆盖 chat 的 json_mode 降级、embed 的排序/数量校验/错误映射。
"""

import http.server
import json
import socket
import threading

import pytest

from pybase.aiclient import AiRequestError, LlmClient


@pytest.fixture(autouse=True)
def _no_proxy_env(monkeypatch):
    for key in ("HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"):
        monkeypatch.delenv(key, raising=False)


def _free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class _ScriptedServer:
    """按顺序吐出预先编排好的响应，每次请求消耗一条；空了就 500。"""

    def __init__(self, responses):
        self.responses = list(responses)
        self.requests = []  # 记录每次请求的 path + 解析后的 body
        port = _free_port()
        self.port = port
        self.httpd = http.server.HTTPServer(("127.0.0.1", port), self._make_handler())
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def _make_handler(self):
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_a, **_k):
                pass

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                raw = self.rfile.read(length)
                try:
                    parsed = json.loads(raw.decode("utf-8")) if raw else None
                except json.JSONDecodeError:
                    parsed = None
                outer.requests.append((self.path, parsed))
                if not outer.responses:
                    self.send_response(500)
                    self.end_headers()
                    return
                status, headers, body = outer.responses.pop(0)
                if isinstance(body, str):
                    body = body.encode("utf-8")
                self.send_response(status)
                for k, v in headers.items():
                    self.send_header(k, v)
                self.end_headers()
                self.wfile.write(body)

        return Handler

    def stop(self):
        self.httpd.shutdown()
        self.httpd.server_close()


def _json_response(status, payload):
    body = json.dumps(payload).encode("utf-8")
    headers = {"Content-Type": "application/json", "Content-Length": str(len(body))}
    return status, headers, body


# ---------------------------------------------------------------------------
# chat：json_mode 失败降级
# ---------------------------------------------------------------------------


def test_chat_json_mode_falls_back_on_error():
    server = _ScriptedServer(
        [
            _json_response(400, {"error": "response_format not supported"}),
            _json_response(200, {"choices": [{"message": {"content": "hi"}}]}),
        ]
    )
    try:
        client = LlmClient(server.base_url)
        text = client.chat([{"role": "user", "content": "hello"}])
        assert text == "hi"
        assert len(server.requests) == 2
        assert "response_format" in server.requests[0][1]
        assert "response_format" not in server.requests[1][1]
    finally:
        server.stop()


# ---------------------------------------------------------------------------
# embed
# ---------------------------------------------------------------------------


def test_embed_basic_request_and_response():
    server = _ScriptedServer(
        [
            _json_response(
                200,
                {
                    "data": [
                        {"index": 0, "embedding": [0.1, 0.2]},
                        {"index": 1, "embedding": [0.3, 0.4]},
                    ]
                },
            )
        ]
    )
    try:
        client = LlmClient(server.base_url, model="text-embedding-3-small")
        vectors = client.embed(["foo", "bar"])
        assert vectors == [[0.1, 0.2], [0.3, 0.4]]
        path, body = server.requests[0]
        assert path == "/v1/embeddings"
        assert body["input"] == ["foo", "bar"]
        assert body["model"] == "text-embedding-3-small"
    finally:
        server.stop()


def test_embed_sorted_by_index_even_when_out_of_order():
    server = _ScriptedServer(
        [
            _json_response(
                200,
                {
                    "data": [
                        {"index": 1, "embedding": [9.0]},
                        {"index": 0, "embedding": [1.0]},
                    ]
                },
            )
        ]
    )
    try:
        client = LlmClient(server.base_url)
        vectors = client.embed(["a", "b"])
        assert vectors == [[1.0], [9.0]]
    finally:
        server.stop()


def test_embed_count_mismatch_raises():
    server = _ScriptedServer(
        [_json_response(200, {"data": [{"index": 0, "embedding": [1.0]}]})]
    )
    try:
        client = LlmClient(server.base_url)
        with pytest.raises(AiRequestError):
            client.embed(["a", "b"])
    finally:
        server.stop()


def test_embed_empty_input_returns_empty_without_request():
    server = _ScriptedServer([])
    try:
        client = LlmClient(server.base_url)
        assert client.embed([]) == []
        assert server.requests == []
    finally:
        server.stop()


def test_embed_http_error_maps_to_ai_request_error():
    server = _ScriptedServer(
        [_json_response(500, {"error": "boom"})]
    )
    try:
        client = LlmClient(server.base_url)
        with pytest.raises(AiRequestError):
            client.embed(["a"])
    finally:
        server.stop()


def test_embed_uses_independent_path_from_chat():
    server = _ScriptedServer(
        [_json_response(200, {"data": [{"index": 0, "embedding": [1.0]}]})]
    )
    try:
        client = LlmClient(server.base_url, path="/custom/chat", embeddings_path="/custom/embeddings")
        client.embed(["a"])
        assert server.requests[0][0] == "/custom/embeddings"
        assert client.endpoint == server.base_url + "/custom/chat"
    finally:
        server.stop()


def test_embed_per_call_model_overrides_default():
    server = _ScriptedServer(
        [_json_response(200, {"data": [{"index": 0, "embedding": [1.0]}]})]
    )
    try:
        client = LlmClient(server.base_url, model="default-model")
        client.embed(["a"], model="override-model")
        assert server.requests[0][1]["model"] == "override-model"
    finally:
        server.stop()
