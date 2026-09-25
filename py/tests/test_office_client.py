"""pybase.office 客户端测试。

本机是 macOS，没有真实的 office 服务，也没有 Word。这里起一个"剧本式"的假
HTTP 服务器，按测试预先编排好的响应顺序应答，验证客户端的状态码->异常映射
和重试行为，跟真实转换逻辑无关。
"""

import http.server
import json
import socket
import threading

import pytest

from pybase.office import errors as office_errors
from pybase.office.client import WordPdfClient


@pytest.fixture(autouse=True)
def _no_proxy_env(monkeypatch):
    """本机开发环境可能配了全局代理，连 127.0.0.1 都会被代理拦截并伪装成
    502，而不是真实的"连不上"。这里的测试全是打本机随机端口，跟代理无关，
    清掉环境变量避免代理把测试结果搅浑（不影响真实部署——那边没有这层代理）。"""
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
        self.calls = []
        self.headers_seen = []
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

            def _respond(self):
                outer.calls.append(self.path)
                outer.headers_seen.append(dict(self.headers))
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

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                self.rfile.read(length)
                self._respond()

            def do_GET(self):
                self._respond()

        return Handler

    def stop(self):
        self.httpd.shutdown()
        self.httpd.server_close()


def _json_response(status, payload):
    body = json.dumps(payload).encode("utf-8")
    headers = {"Content-Type": "application/json", "Content-Length": str(len(body))}
    return status, headers, body


def _pdf_response(status=200, page_count=3, body=b"%PDF-1.4 fake"):
    headers = {
        "Content-Type": "application/pdf",
        "Content-Length": str(len(body)),
        "X-Page-Count": str(page_count),
    }
    return status, headers, body


def test_convert_success():
    server = _ScriptedServer([_pdf_response(page_count=7)])
    try:
        client = WordPdfClient(server.base_url, max_retries=0)
        result = client.convert(b"fake docx bytes")
        assert result.pdf_bytes == b"%PDF-1.4 fake"
        assert result.page_count == 7
    finally:
        server.stop()


def test_unauthorized_maps_to_authentication_error():
    server = _ScriptedServer([_json_response(401, {"error": "unauthorized", "message": "bad token"})])
    try:
        client = WordPdfClient(server.base_url, max_retries=0)
        with pytest.raises(office_errors.AuthenticationError):
            client.convert(b"x")
    finally:
        server.stop()


def test_too_large_maps_to_payload_too_large():
    server = _ScriptedServer([_json_response(413, {"error": "too_large", "message": "too big"})])
    try:
        client = WordPdfClient(server.base_url, max_retries=0)
        with pytest.raises(office_errors.PayloadTooLargeError):
            client.convert(b"x")
    finally:
        server.stop()


def test_timeout_maps_to_conversion_timeout_error():
    server = _ScriptedServer([_json_response(504, {"error": "timeout", "message": "超时"})])
    try:
        client = WordPdfClient(server.base_url, max_retries=0)
        with pytest.raises(office_errors.ConversionTimeoutError):
            client.convert(b"x")
    finally:
        server.stop()


def test_conversion_failed_maps_to_conversion_failed_error():
    server = _ScriptedServer([_json_response(500, {"error": "conversion_failed", "message": "COM 出错了"})])
    try:
        client = WordPdfClient(server.base_url, max_retries=0)
        with pytest.raises(office_errors.ConversionFailedError, match="COM 出错了"):
            client.convert(b"x")
    finally:
        server.stop()


def test_busy_retries_then_succeeds():
    server = _ScriptedServer([
        _json_response(503, {"error": "busy", "message": "忙"}),
        _json_response(503, {"error": "busy", "message": "忙"}),
        _pdf_response(page_count=1),
    ])
    try:
        client = WordPdfClient(server.base_url, max_retries=3, retry_interval=0.01)
        result = client.convert(b"x")
        assert result.page_count == 1
        assert len(server.calls) == 3
    finally:
        server.stop()


def test_busy_exhausts_retries_raises_service_busy():
    server = _ScriptedServer([
        _json_response(503, {"error": "busy", "message": "忙"}),
        _json_response(503, {"error": "busy", "message": "忙"}),
    ])
    try:
        client = WordPdfClient(server.base_url, max_retries=1, retry_interval=0.01)
        with pytest.raises(office_errors.ServiceBusyError):
            client.convert(b"x")
        assert len(server.calls) == 2  # 首次 + 1 次重试，不多不少
    finally:
        server.stop()


def test_conversion_failed_does_not_retry():
    server = _ScriptedServer([_json_response(500, {"error": "conversion_failed", "message": "坏了"})])
    try:
        client = WordPdfClient(server.base_url, max_retries=3, retry_interval=0.01)
        with pytest.raises(office_errors.ConversionFailedError):
            client.convert(b"x")
        assert len(server.calls) == 1  # 确定性错误，不重试
    finally:
        server.stop()


def test_connection_unavailable_after_retries():
    # 没有服务器监听这个端口
    port = _free_port()
    client = WordPdfClient(f"http://127.0.0.1:{port}", max_retries=1, retry_interval=0.01)
    with pytest.raises(office_errors.ConnectionUnavailableError):
        client.convert(b"x")


def test_health_success():
    payload = {
        "word_alive": True, "queue_len": 0, "processed": 5, "last_error": None,
        "memory_backed": True, "work_dir": "R:\\",
    }
    server = _ScriptedServer([_json_response(200, payload)])
    try:
        client = WordPdfClient(server.base_url, max_retries=0)
        assert client.health() == payload
    finally:
        server.stop()


def test_health_error_maps_to_authentication_error():
    server = _ScriptedServer([_json_response(401, {"error": "unauthorized", "message": "no token"})])
    try:
        client = WordPdfClient(server.base_url, max_retries=0)
        with pytest.raises(office_errors.AuthenticationError):
            client.health()
    finally:
        server.stop()


def test_token_header_sent():
    server = _ScriptedServer([_pdf_response()])
    try:
        client = WordPdfClient(server.base_url, token="secret-token", max_retries=0)
        client.convert(b"x")
        assert server.headers_seen[0]["X-Office-Token"] == "secret-token"
    finally:
        server.stop()


def test_no_token_header_when_not_configured():
    server = _ScriptedServer([_pdf_response()])
    try:
        client = WordPdfClient(server.base_url, max_retries=0)
        client.convert(b"x")
        assert "X-Office-Token" not in server.headers_seen[0]
    finally:
        server.stop()
