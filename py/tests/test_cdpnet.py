"""pybase.cdpnet 的单元测试：CDP 网络事件的纯映射/判断函数。"""
from __future__ import annotations

from pybase.cdpnet import (
    body_decision,
    decode_response_body,
    request_record,
    response_fields,
)

MIME_ALLOW = ["application/json", "application/xml", "text/"]
MIME_DENY = ["text/css"]
FORCE_URL_HINTS = [".vtt", ".srt", "subtitle", "caption", "transcript"]
MAX_BYTES = 2 * 1024 * 1024


def _decide(**overrides):
    kwargs = dict(
        status=200, from_cache=0, url="https://a.com/api/x.json",
        mime="application/json", size=100,
        max_bytes=MAX_BYTES, mime_allow=MIME_ALLOW, mime_deny=MIME_DENY,
        force_url_hints=FORCE_URL_HINTS,
    )
    kwargs.update(overrides)
    return body_decision(**kwargs)


# --------------------------------------------------------------- request_record

def test_request_record_maps_fields_and_splits_url():
    event = {
        "requestId": "r1", "frameId": "F1", "loaderId": "L1",
        "documentURL": "https://a.com/", "type": "XHR", "timestamp": 1.0,
        "wallTime": 1700000000.0,
        "request": {"url": "https://a.com/api/x?y=1", "method": "GET",
                     "headers": {"accept": "*/*"}, "postData": "body-data"},
    }
    rec = request_record(session_id=7, event=event, capture_post_data=True)
    assert rec["session_id"] == 7
    assert rec["request_id"] == "r1"
    assert rec["frame_id"] == "F1"
    assert rec["loader_id"] == "L1"
    assert rec["document_url"] == "https://a.com/"
    assert rec["resource_type"] == "XHR"
    assert rec["is_redirect"] == 0
    assert rec["method"] == "GET"
    assert rec["url"] == "https://a.com/api/x?y=1"
    assert rec["url_host"] == "a.com"
    assert rec["url_path"] == "/api/x"
    assert rec["request_headers"] == {"accept": "*/*"}
    assert rec["post_data"] == "body-data"
    assert rec["ts_request"] == 1.0
    assert rec["wall_time"] == 1700000000.0
    assert rec["from_cache"] == 0
    assert rec["from_service_worker"] == 0
    assert rec["canceled"] == 0


def test_request_record_respects_capture_post_data_flag():
    event = {"requestId": "r1", "request": {"url": "https://a.com/x", "method": "POST",
                                              "postData": "secret"}}
    rec = request_record(session_id=1, event=event, capture_post_data=False)
    assert rec["post_data"] is None


def test_request_record_wall_time_falls_back_when_missing():
    event = {"requestId": "r1", "request": {"url": "https://a.com/x", "method": "GET"}}
    rec = request_record(session_id=1, event=event, capture_post_data=True)
    assert isinstance(rec["wall_time"], float)
    assert rec["wall_time"] > 0


# --------------------------------------------------------------- response_fields

def test_response_fields_maps_all_columns():
    response = {
        "status": 200, "statusText": "OK", "mimeType": "application/json",
        "headers": {"content-type": "application/json"},
        "remoteIPAddress": "1.2.3.4", "remotePort": 443, "protocol": "h2",
        "responseTime": 123.0,
    }
    fields = response_fields(response)
    assert fields == {
        "status": 200, "status_text": "OK", "mime_type": "application/json",
        "response_headers": {"content-type": "application/json"},
        "remote_ip": "1.2.3.4", "remote_port": 443, "protocol": "h2",
        "from_cache": 0, "from_service_worker": 0, "ts_response": 123.0,
    }


def test_response_fields_from_disk_cache_sets_from_cache():
    fields = response_fields({"fromDiskCache": True})
    assert fields["from_cache"] == 1


def test_response_fields_preserves_prior_from_cache_when_not_disk_cache():
    # Network.requestServedFromCache 先标过 from_cache=1，之后 responseReceived
    # 没带 fromDiskCache，不应该被覆盖回 0。
    fields = response_fields({"status": 200}, current_from_cache=1)
    assert fields["from_cache"] == 1


def test_response_fields_from_service_worker():
    fields = response_fields({"fromServiceWorker": True})
    assert fields["from_service_worker"] == 1


# --------------------------------------------------------------- body_decision

def test_body_decision_ok_case():
    want, reason, max_bytes = _decide()
    assert want is True
    assert reason == ""
    assert max_bytes == MAX_BYTES


def test_body_decision_204_no_content():
    want, reason, _ = _decide(status=204)
    assert want is False
    assert reason == "status 204"


def test_body_decision_304_not_modified():
    want, reason, _ = _decide(status=304)
    assert want is False
    assert reason == "status 304"


def test_body_decision_from_cache():
    want, reason, _ = _decide(from_cache=1)
    assert want is False
    assert reason == "from cache"


def test_body_decision_force_url_hint_widens_limit_and_ignores_mime():
    want, reason, max_bytes = _decide(
        url="https://a.com/x/subtitle?id=9", mime="application/octet-stream",
        size=3 * 1024 * 1024,
    )
    assert want is True
    assert reason == "hint:subtitle"
    assert max_bytes == MAX_BYTES * 10


def test_body_decision_mime_deny():
    want, reason, _ = _decide(url="https://a.com/s.css", mime="text/css")
    assert want is False
    assert "denied" in reason


def test_body_decision_mime_not_allowed():
    want, reason, _ = _decide(url="https://a.com/v.mp4", mime="video/mp4")
    assert want is False
    assert "not allowed" in reason


def test_body_decision_mime_missing_reports_unknown():
    want, reason, _ = _decide(mime=None)
    assert want is False
    assert "unknown" in reason


def test_body_decision_too_large():
    want, reason, _ = _decide(size=MAX_BYTES + 1)
    assert want is False
    assert "too large" in reason


def test_body_decision_hint_check_before_deny_and_allow():
    # 关键词命中时即使 mime 在 deny 列表里也应该放行（原逻辑：hint 检查先于 deny/allow）。
    want, reason, max_bytes = _decide(
        url="https://a.com/x.vtt", mime="text/css", size=100,
    )
    assert want is True
    assert reason == "hint:.vtt"
    assert max_bytes == MAX_BYTES * 10


# --------------------------------------------------------------- decode_response_body

def test_decode_response_body_plain_text():
    assert decode_response_body('{"a":1}', False) == b'{"a":1}'


def test_decode_response_body_base64():
    # "QUJD" 是 "ABC" 的 base64
    assert decode_response_body("QUJD", True) == b"QUJD"


def test_decode_response_body_utf8():
    assert decode_response_body("中文", False) == "中文".encode("utf-8")
