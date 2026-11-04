"""pybase.asr.types：Cue / TranscriptionResult 的校验、JSON 读写（毫秒整数）、与旧类型互转。"""
from __future__ import annotations

import json

import pytest

from pybase.asr import Cue, Transcript, TranscriptionResult, load_result, save_result
from pybase.asr.types import dumps_result, loads_result
from pybase.subtitle.models import Cue as SubtitleCue


def test_cue_validates_times_and_types():
    assert Cue(0, 0, "").speaker == ""
    with pytest.raises(TypeError):
        Cue(0.5, 1000, "x")                 # 不是整数毫秒
    with pytest.raises(TypeError):
        Cue(True, 1000, "x")                # bool 不算整数
    with pytest.raises(ValueError):
        Cue(-1, 10, "x")
    with pytest.raises(ValueError):
        Cue(20, 10, "x")
    with pytest.raises(TypeError):
        Cue(0, 10, "x", speaker=None)


def test_json_round_trip_is_integer_milliseconds(tmp_path):
    result = TranscriptionResult(
        [Cue(0, 2_400, "第一句。", "测试员"), Cue(3_400, 4_400, "好。")], "omlx:w+q")
    assert isinstance(result.cues, tuple)
    data = json.loads(dumps_result(result))
    assert data == {"engine": "omlx:w+q", "cues": [
        {"start_ms": 0, "end_ms": 2_400, "text": "第一句。", "speaker": "测试员"},
        {"start_ms": 3_400, "end_ms": 4_400, "text": "好。", "speaker": ""},
    ]}
    path = save_result(result, tmp_path / "sub" / "transcript.qwen.json")
    assert load_result(path) == result
    assert list(path.parent.iterdir()) == [path]           # 不留临时文件
    assert "第一句" in path.read_text(encoding="utf-8")       # 中文原样，不转义


def test_loads_rejects_bad_shapes():
    assert loads_result('{"engine": "e", "cues": [{"start_ms": 1, "end_ms": 2, "text": "t"}]}'
                        ).cues == (Cue(1, 2, "t"),)       # speaker 可省
    for bad in ("not json", "[]", '{"cues": []}', '{"engine": "e"}',
                '{"engine": "e", "cues": [{"start_ms": 1.5, "end_ms": 2, "text": "t"}]}',
                '{"engine": "e", "cues": [{"start": 1, "end": 2, "text": "t"}]}',
                '{"engine": "e", "cues": [{"start_ms": 3, "end_ms": 2, "text": "t"}]}'):
        with pytest.raises(ValueError):
            loads_result(bad)


def test_conversions_with_subtitle_cue_and_transcript():
    sub = SubtitleCue(100, 900, "你好")
    assert Cue.from_subtitle(sub, speaker="客户") == Cue(100, 900, "你好", "客户")
    back = Cue(100, 900, "你好", "客户").to_subtitle()
    assert (back.start, back.end, back.text) == (100, 900, "你好")
    transcript = Transcript(text="你好", cues=(sub,), engine="apple-speech:zh-CN", language="zh-CN")
    assert TranscriptionResult.from_transcript(transcript) == TranscriptionResult(
        (Cue(100, 900, "你好"),), "apple-speech:zh-CN")
    with pytest.raises(TypeError):
        TranscriptionResult([sub], "e")                     # 字幕 Cue 不能直接塞
