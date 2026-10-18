"""pybase 里原先直接用 urllib/requests 的调用点，现在都经 wgnet：域名给桩 SOCKS5，断言它收到的是原主机名。"""

import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from pybase import wgnet                      # noqa: E402
from pybase.aiclient import AiRequestError, LlmClient   # noqa: E402
from pybase.net.monitor.collectors import collect_mihomo  # noqa: E402
from pybase.office.client import WordPdfClient          # noqa: E402
from pybase.rpc.client import RPCClient, RPCError       # noqa: E402
from test_socks5 import Stub                  # noqa: E402


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, body, ctype="application/json", extra=None):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            self._send(200, b'{"ok": true}')
        elif self.path in ("/version", "/configs"):
            self._send(200, b'{"version": "1.0", "mode": "rule"}')
        elif self.path == "/connections":
            self._send(200, b'{"connections": []}')
        else:
            self._send(404, b"{}")

    def do_POST(self):
        data = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path == "/v1/chat/completions":
            req = json.loads(data)
            if req["stream"]:
                sse = b"".join(b'data: {"choices":[{"delta":{"content":"%s"}}]}\n\n' % c for c in (b"a", b"b"))
                self._send(200, sse + b"data: [DONE]\n\n", "text/event-stream")
            else:
                self._send(200, b'{"choices":[{"message":{"content":"hi"}}]}')
        elif self.path == "/v1/embeddings":
            self._send(500, b"boom")
        elif self.path.startswith("/convert"):
            self._send(200, b"%PDF" + self.path.encode(), "application/pdf", {"X-Page-Count": "3"})
        elif self.path == "/rpc":
            req = json.loads(data)
            if isinstance(req, list):
                self._send(200, json.dumps([{"id": r["id"], "result": r["method"]} for r in req]).encode())
            else:
                self._send(200, json.dumps({"id": req["id"], "result": "ok"}).encode())
        else:
            self._send(404, b"{}")


@pytest.fixture
def via():
    """返回 (桩代理, 域名前缀)；strict，没有桩就连不上。"""
    srv = HTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    stub = Stub(forward=("127.0.0.1", srv.server_port))
    wgnet.configure(stub.addr, strict=True)
    yield stub
    wgnet.config._proxy = wgnet.config._strict = None
    os.environ.pop(wgnet.config.ENV_PROXY, None)
    os.environ.pop(wgnet.config.ENV_STRICT, None)
    stub.stop()
    srv.shutdown()


def test_aiclient_非流式_流式_错误(via):
    c = LlmClient("http://llm.lan:8080", model="m")
    assert c.chat([{"role": "user", "content": "x"}], json_mode=False) == "hi"
    got = []
    assert c.chat([{"role": "user", "content": "x"}], json_mode=False, on_delta=got.append) == "ab"
    assert got == ["a", "b"]
    with pytest.raises(AiRequestError, match="500"):
        c.embed(["x"])
    assert {h for _, h, _ in via.seen} == {"llm.lan"}


def test_office_client_convert_health(via):
    c = WordPdfClient("http://office.lan:9000", max_retries=0)
    r = c.convert(b"docx")
    assert r.pdf_bytes.startswith(b"%PDF/convert?update_fields=1") and r.page_count == 3
    assert c.health() == {"ok": True}
    assert {h for _, h, _ in via.seen} == {"office.lan"}


def test_rpc_client_call_batch(via):
    c = RPCClient("http://rpc.lan:7000/rpc")
    assert c.call("a.b") == "ok"
    assert c.batch([("x", None), ("y", None)]) == ["x", "y"]
    assert {h for _, h, _ in via.seen} == {"rpc.lan"}
    with pytest.raises(RPCError):
        RPCClient("http://rpc.lan:7000/nope").call("a")


def test_mihomo经wgnet(via):
    r = collect_mihomo("http://clash.lan:9090")
    assert r.get("ok") is not False and r.get("version") == "1.0"
    assert {h for _, h, _ in via.seen} == {"clash.lan"}
