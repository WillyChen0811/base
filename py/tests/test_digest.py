"""覆盖 pybase.digest：单块/多块 map-reduce、编号→时间映射、容错、取消、JSON 往返。

`complete` 全部用假函数，不依赖真实大模型。
"""
from __future__ import annotations

import json
import threading

import pytest

from pybase.digest import (
    Chapter,
    Digest,
    DigestCancelled,
    DigestError,
    Takeaway,
    compute_source_hash,
    summarize,
)
from pybase.subtitle import Cue


def _cues(n: int, start: int = 0, step: int = 1000, text: str | None = None) -> list[Cue]:
    return [
        Cue(start=start + i * step, end=start + i * step + step, text=(text or f"line {i}"))
        for i in range(n)
    ]


def _json_reply(payload: dict) -> str:
    return json.dumps(payload, ensure_ascii=False)


# ---------------------------------------------------------------------------
# 单块
# ---------------------------------------------------------------------------


def test_summarize_single_chunk_maps_index_to_time():
    cues = _cues(5)  # start: 0,1000,2000,3000,4000 end: +1000

    def complete(messages):
        assert messages[0]["role"] == "system"
        return _json_reply(
            {
                "overview": "总览",
                "chapters": [{"title": "第一章", "summary": "开场", "start": 0, "end": 2}],
                "takeaways": [{"text": "要点一", "start": 1, "end": 1}],
            }
        )

    digest = summarize(cues, complete, language="zh", max_chars=100_000)

    assert digest.overview == "总览"
    assert len(digest.chapters) == 1
    chapter = digest.chapters[0]
    assert (chapter.start_index, chapter.end_index) == (0, 2)
    assert (chapter.start, chapter.end) == (cues[0].start, cues[2].end)

    assert len(digest.takeaways) == 1
    takeaway = digest.takeaways[0]
    assert (takeaway.start, takeaway.end) == (cues[1].start, cues[1].end)


def test_summarize_wraps_json_in_code_block_and_extra_text():
    cues = _cues(3)

    def complete(messages):
        payload = _json_reply(
            {"overview": "ok", "chapters": [{"title": "t", "summary": "s", "start": 0, "end": 1}], "takeaways": []}
        )
        return f"这是我的分析：\n```json\n{payload}\n```\n谢谢！"

    digest = summarize(cues, complete, language="zh", max_chars=100_000)
    assert digest.overview == "ok"
    assert digest.chapters[0].start_index == 0


def test_summarize_extracts_json_from_surrounding_prose_without_code_block():
    cues = _cues(3)

    def complete(messages):
        payload = _json_reply(
            {"overview": "ok2", "chapters": [], "takeaways": [{"text": "x", "start": 0, "end": 0}]}
        )
        return f"Sure, here you go: {payload} hope that helps"

    digest = summarize(cues, complete, language="en", max_chars=100_000)
    assert digest.overview == "ok2"
    assert digest.takeaways[0].text == "x"


# ---------------------------------------------------------------------------
# 多块 map-reduce
# ---------------------------------------------------------------------------


def test_summarize_multi_chunk_calls_map_then_reduce():
    # 每行编号文本形如 "[0] (00:00) line 0"，长度约 20 字符；max_chars 设小一点强制多块
    cues = _cues(10)
    line_len = len(f"[0] (00:00) {cues[0].text}") + 1
    max_chars = line_len * 4  # 大约每块 4 句，10 句切成 3 块左右

    calls = []
    progress = []

    def complete(messages):
        content = messages[1]["content"]
        calls.append(content)
        is_merge_stage = "切成若干块后" in content  # 只有 _merge_user_prompt 里有这句
        if not is_merge_stage:
            # 分块阶段：随便给这块里第一句当章节
            return _json_reply(
                {"overview": f"块{len(calls)}概述", "chapters": [{"title": f"c{len(calls)}", "summary": "s", "start": 0, "end": 0}], "takeaways": []}
            )
        # 汇总阶段：跨块引用编号 0 和 9
        return _json_reply(
            {
                "overview": "整体总览",
                "chapters": [{"title": "合并章节", "summary": "s", "start": 0, "end": 9}],
                "takeaways": [{"text": "合并要点", "start": 5, "end": 5}],
            }
        )

    digest = summarize(
        cues,
        complete,
        language="zh",
        max_chars=max_chars,
        on_progress=lambda done, total: progress.append((done, total)),
    )

    assert len(calls) >= 2  # 至少一次分块 + 一次汇总
    assert digest.overview == "整体总览"
    assert digest.chapters[0].start_index == 0
    assert digest.chapters[0].end_index == 9
    assert digest.chapters[0].end == cues[9].end
    assert digest.takeaways[0].start == cues[5].start
    # on_progress 只报分块阶段进度，且 total 一致、done 递增到 total
    assert progress
    total = progress[-1][1]
    assert [d for d, _ in progress] == list(range(1, total + 1))


def test_summarize_single_call_when_everything_fits_one_chunk():
    cues = _cues(3)
    calls = []

    def complete(messages):
        calls.append(1)
        return _json_reply({"overview": "o", "chapters": [], "takeaways": [{"text": "t", "start": 0, "end": 0}]})

    summarize(cues, complete, language="zh", max_chars=1_000_000)
    assert len(calls) == 1  # 块数为 1 时只调用一次，不做多余的汇总请求


# ---------------------------------------------------------------------------
# 容错：越界/反向/缺字段/全部无效
# ---------------------------------------------------------------------------


def test_out_of_range_and_backwards_entries_are_dropped():
    cues = _cues(3)

    def complete(messages):
        return _json_reply(
            {
                "overview": "o",
                "chapters": [
                    {"title": "bad-range", "summary": "s", "start": 5, "end": 6},  # 越界
                    {"title": "backwards", "summary": "s", "start": 2, "end": 0},  # 反向
                    {"title": "not-int", "summary": "s", "start": "0", "end": 1},  # 非整数
                    {"title": "ok", "summary": "s", "start": 0, "end": 1},  # 合法
                ],
                "takeaways": [
                    {"text": "no-such-field-start"},  # 缺字段
                    {"text": "ok", "start": 0, "end": 0},
                ],
            }
        )

    digest = summarize(cues, complete, language="zh", max_chars=1_000_000)
    assert len(digest.chapters) == 1
    assert digest.chapters[0].title == "ok"
    assert len(digest.takeaways) == 1
    assert digest.takeaways[0].text == "ok"


def test_all_invalid_raises_digest_error():
    cues = _cues(3)

    def complete(messages):
        return _json_reply(
            {
                "overview": "o",
                "chapters": [{"title": "bad", "summary": "s", "start": 99, "end": 100}],
                "takeaways": [{"text": "bad", "start": -1, "end": -1}],
            }
        )

    with pytest.raises(DigestError):
        summarize(cues, complete, language="zh", max_chars=1_000_000)


def test_malformed_json_raises_digest_error():
    cues = _cues(3)

    def complete(messages):
        return "抱歉，我不知道你在说什么"

    with pytest.raises(DigestError):
        summarize(cues, complete, language="zh", max_chars=1_000_000)


def test_overlapping_chapters_are_truncated_not_dropped_when_partial():
    cues = _cues(6)

    def complete(messages):
        return _json_reply(
            {
                "overview": "o",
                "chapters": [
                    {"title": "first", "summary": "s", "start": 0, "end": 3},
                    {"title": "second", "summary": "s", "start": 2, "end": 5},
                ],
                "takeaways": [],
            }
        )

    digest = summarize(cues, complete, language="zh", max_chars=1_000_000)
    assert len(digest.chapters) == 2
    first, second = digest.chapters
    assert first.title == "first"
    assert first.end_index == 1  # 截断到 second.start_index - 1
    assert first.end == cues[1].end
    assert second.title == "second"
    assert (second.start_index, second.end_index) == (2, 5)


def test_fully_overlapped_chapter_is_dropped():
    cues = _cues(6)

    def complete(messages):
        return _json_reply(
            {
                "overview": "o",
                "chapters": [
                    {"title": "swallowed", "summary": "s", "start": 1, "end": 2},
                    {"title": "covers-all", "summary": "s", "start": 0, "end": 5},
                ],
                "takeaways": [],
            }
        )

    digest = summarize(cues, complete, language="zh", max_chars=1_000_000)
    titles = [c.title for c in digest.chapters]
    assert titles == ["covers-all"]


# ---------------------------------------------------------------------------
# 取消
# ---------------------------------------------------------------------------


def test_cancel_before_call_raises_digest_cancelled():
    cues = _cues(3)
    event = threading.Event()
    event.set()

    def complete(messages):
        raise AssertionError("不该调用到模型")

    with pytest.raises(DigestCancelled):
        summarize(cues, complete, language="zh", max_chars=1_000_000, cancel_event=event)


def test_cancel_mid_multi_chunk_stops_before_next_call():
    cues = _cues(10)
    line_len = len(f"[0] (00:00) {cues[0].text}") + 1
    max_chars = line_len * 4
    event = threading.Event()
    calls = []

    def complete(messages):
        calls.append(1)
        if len(calls) == 1:
            event.set()  # 第一次调用之后置位，第二次调用前应该被拦下
        return _json_reply({"overview": "o", "chapters": [], "takeaways": [{"text": "t", "start": 0, "end": 0}]})

    with pytest.raises(DigestCancelled):
        summarize(cues, complete, language="zh", max_chars=max_chars, cancel_event=event)
    assert len(calls) == 1


# ---------------------------------------------------------------------------
# 参数校验
# ---------------------------------------------------------------------------


def test_language_required():
    with pytest.raises(ValueError):
        summarize(_cues(3), lambda m: "{}", language="", max_chars=100)


def test_max_chars_required_positive():
    with pytest.raises(ValueError):
        summarize(_cues(3), lambda m: "{}", language="zh", max_chars=0)


def test_empty_cues_raises():
    with pytest.raises(DigestError):
        summarize([], lambda m: "{}", language="zh", max_chars=100)


# ---------------------------------------------------------------------------
# source_hash
# ---------------------------------------------------------------------------


def test_source_hash_stable_and_sensitive_to_text_and_time():
    cues = _cues(3)
    h1 = compute_source_hash(cues)
    h2 = compute_source_hash(_cues(3))
    assert h1 == h2  # 内容一样，稳定

    changed_text = _cues(3)
    changed_text[0] = Cue(start=changed_text[0].start, end=changed_text[0].end, text="different")
    assert compute_source_hash(changed_text) != h1

    changed_time = _cues(3)
    changed_time[0] = Cue(start=changed_time[0].start + 1, end=changed_time[0].end, text=changed_time[0].text)
    assert compute_source_hash(changed_time) != h1


# ---------------------------------------------------------------------------
# JSON 往返
# ---------------------------------------------------------------------------


def test_digest_to_dict_from_dict_roundtrip():
    chapters = (
        Chapter(title="t1", summary="s1", start_index=0, end_index=1, start=0, end=2000),
    )
    takeaways = (
        Takeaway(text="tk1", start_index=1, end_index=1, start=1000, end=2000),
    )
    digest = Digest(
        overview="总览",
        chapters=chapters,
        takeaways=takeaways,
        model="local-model",
        language="zh",
        source_hash="abc123",
        created_at="2026-01-01T00:00:00Z",
    )
    data = digest.to_dict()
    restored = Digest.from_dict(json.loads(json.dumps(data, ensure_ascii=False)))
    assert restored == digest
