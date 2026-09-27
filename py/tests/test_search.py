"""pybase.search 测试：upsert/remove、关键词排序、中文短查询、转义、语义路、
RRF 融合与 matched_by、维度/模型不一致、doc_ids 过滤、持久化重开、切片。
"""
from __future__ import annotations

import os
import tempfile
from dataclasses import dataclass

import pytest

from pybase.search import Hit, SearchIndex, Segment, segments_from_cues


# ---------------------------------------------------------------------------
# fixtures / 假 embed
# ---------------------------------------------------------------------------


@dataclass
class _FakeCue:
    start: int
    end: int
    text: str


def _bow_embed(keys):
    """按字符/词出现次数编成一个"字符袋"向量的假 embed，可控、可复现。"""

    def embed(texts):
        return [[float(t.count(k)) for k in keys] for t in texts]

    return embed


@pytest.fixture
def index():
    idx = SearchIndex(":memory:")
    yield idx
    idx.close()


# ---------------------------------------------------------------------------
# upsert / remove / doc_ids
# ---------------------------------------------------------------------------


def test_upsert_then_search_finds_segment(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="hello world")])
    hits = index.search("hello", limit=5)
    assert len(hits) == 1
    assert hits[0].doc_id == "doc1"
    assert hits[0].start == 0 and hits[0].end == 1000


def test_upsert_replaces_previous_segments(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="old content here")])
    index.upsert("doc1", [Segment(start=0, end=1000, text="new content here")])
    assert index.search("old", limit=5) == []
    hits = index.search("new", limit=5)
    assert len(hits) == 1


def test_remove_deletes_segments_and_vectors(index):
    embed = _bow_embed(["cat"])
    index.upsert("doc1", [Segment(start=0, end=1000, text="a cat")], embed=embed, embed_model="m1")
    index.remove("doc1")
    assert index.doc_ids() == []
    assert index.search("cat", limit=5) == []
    assert index.search("cat", limit=5, embed=embed, embed_model="m1") == []


def test_doc_ids_lists_all(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="a")])
    index.upsert("doc2", [Segment(start=0, end=1000, text="b")])
    assert sorted(index.doc_ids()) == ["doc1", "doc2"]


def test_with_statement_closes():
    with SearchIndex(":memory:") as idx:
        idx.upsert("doc1", [Segment(start=0, end=100, text="x")])
        assert idx.search("x", limit=5)


# ---------------------------------------------------------------------------
# 关键词排序 / 中文短查询 / 转义
# ---------------------------------------------------------------------------


def test_keyword_ranks_more_frequent_match_higher(index):
    index.upsert(
        "doc1",
        [
            Segment(start=0, end=1000, text="cat cat cat cat"),
            Segment(start=1000, end=2000, text="cat dog"),
        ],
    )
    hits = index.search("cat", limit=5)
    assert [h.start for h in hits] == [0, 1000]


@pytest.mark.parametrize(
    "query",
    ["识", "识别", "语音识别的模型"],
)
def test_chinese_queries_of_various_lengths_match(index, query):
    index.upsert("doc1", [Segment(start=0, end=1000, text="这是一段语音识别的模型测试文本")])
    hits = index.search(query, limit=5)
    assert len(hits) == 1


def test_chinese_two_char_query_does_not_false_positive(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="完全不相关的内容")])
    assert index.search("识别", limit=5) == []


def test_mixed_chinese_english_query(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="用 ASR 做语音识别")])
    hits = index.search("ASR 识别", limit=5)
    assert len(hits) == 1
    hits_case_insensitive = index.search("asr", limit=5)
    assert len(hits_case_insensitive) == 1


def test_english_word_query_case_insensitive(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="Model training pipeline")])
    assert len(index.search("model", limit=5)) == 1
    assert len(index.search("MODEL", limit=5)) == 1


def test_multi_word_query_is_and_semantics(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="识别 模型 测试")])
    index.upsert("doc2", [Segment(start=0, end=1000, text="只有 识别 没有别的")])
    hits = index.search("识别 模型", limit=5)
    assert [h.doc_id for h in hits] == ["doc1"]


@pytest.mark.parametrize(
    "query",
    ['"quoted"', "(parens)", "colon:star*", "a\"b", "% wildcard _ chars"],
)
def test_special_characters_do_not_raise(index, query):
    index.upsert("doc1", [Segment(start=0, end=1000, text='weird "quoted" (parens) colon:star* text')])
    # 只要求不抛异常；命中与否不是本测试的重点
    index.search(query, limit=5)


def test_empty_query_returns_empty(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="something")])
    assert index.search("", limit=5) == []
    assert index.search("   ", limit=5) == []


def test_no_match_returns_empty(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="something")])
    assert index.search("nonexistentword", limit=5) == []


# ---------------------------------------------------------------------------
# 语义路 / RRF 融合 / matched_by
# ---------------------------------------------------------------------------


def test_semantic_only_finds_synonym_without_keyword_overlap(index):
    # 关键词完全不重叠（"轿车" vs "car"），但假 embed 把它们编码成同一个向量维度上
    # 取值相同，制造"语义相关但关键词不相关"的场景
    def embed(texts):
        return [[1.0 if ("轿车" in t or "car" in t) else 0.0] for t in texts]

    index.upsert("doc1", [Segment(start=0, end=1000, text="我买了一辆轿车")], embed=embed, embed_model="m1")
    hits = index.search("car", limit=5, embed=embed, embed_model="m1")
    assert len(hits) == 1
    assert hits[0].matched_by == "semantic"


def test_hybrid_marks_both_when_matched_by_keyword_and_semantic(index):
    embed = _bow_embed(["cat"])
    index.upsert("doc1", [Segment(start=0, end=1000, text="a cat here")], embed=embed, embed_model="m1")
    hits = index.search("cat", limit=5, embed=embed, embed_model="m1")
    assert len(hits) == 1
    assert hits[0].matched_by == "both"


def test_rrf_fuses_keyword_and_semantic_distinct_docs(index):
    # doc1 只有关键词能命中（语义向量跟查询无关），doc2 只有语义能命中
    def embed(texts):
        return [[1.0 if "语义" in t else 0.0] for t in texts]

    index.upsert("doc1", [Segment(start=0, end=1000, text="keyword_only_term")], embed=embed, embed_model="m1")
    index.upsert("doc2", [Segment(start=0, end=1000, text="语义相关但没有关键词")], embed=embed, embed_model="m1")

    def query_embed(texts):
        return [[1.0] for _ in texts]  # 查询向量强制跟"doc2"的向量最相似

    hits = index.search("keyword_only_term", limit=5, embed=query_embed, embed_model="m1")
    by_doc = {h.doc_id: h.matched_by for h in hits}
    assert by_doc.get("doc1") == "keyword"  # 语义向量跟查询余弦为 0，被判定为不相关，不进语义候选池
    assert by_doc.get("doc2") == "semantic"


def test_embed_model_mismatch_falls_back_to_keyword_only(index):
    embed_a = _bow_embed(["cat"])
    index.upsert("doc1", [Segment(start=0, end=1000, text="a cat")], embed=embed_a, embed_model="model-a")
    hits = index.search("cat", limit=5, embed=embed_a, embed_model="model-b")
    assert len(hits) == 1
    assert hits[0].matched_by == "keyword"


def test_embed_dim_mismatch_falls_back_to_keyword_only(index):
    embed_2d = _bow_embed(["cat", "dog"])
    index.upsert("doc1", [Segment(start=0, end=1000, text="a cat")], embed=embed_2d, embed_model="m1")

    def embed_3d(texts):
        return [[0.0, 0.0, 0.0] for _ in texts]

    hits = index.search("cat", limit=5, embed=embed_3d, embed_model="m1")
    assert len(hits) == 1
    assert hits[0].matched_by == "keyword"


def test_embed_batch_size_calls_embed_in_chunks(index):
    calls = []

    def embed(texts):
        calls.append(list(texts))
        return [[1.0] for _ in texts]

    segs = [Segment(start=i * 100, end=i * 100 + 50, text=f"seg {i}") for i in range(5)]
    index.upsert("doc1", segs, embed=embed, embed_model="m1", embed_batch_size=2)
    assert [len(c) for c in calls] == [2, 2, 1]


# ---------------------------------------------------------------------------
# doc_ids 过滤
# ---------------------------------------------------------------------------


def test_doc_ids_filter_restricts_keyword_search(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="shared term")])
    index.upsert("doc2", [Segment(start=0, end=1000, text="shared term")])
    hits = index.search("shared", limit=5, doc_ids=["doc1"])
    assert [h.doc_id for h in hits] == ["doc1"]


def test_doc_ids_filter_restricts_semantic_search(index):
    embed = _bow_embed(["term"])
    index.upsert("doc1", [Segment(start=0, end=1000, text="term here")], embed=embed, embed_model="m1")
    index.upsert("doc2", [Segment(start=0, end=1000, text="term here")], embed=embed, embed_model="m1")
    hits = index.search("term", limit=5, embed=embed, embed_model="m1", doc_ids=["doc2"])
    assert [h.doc_id for h in hits] == ["doc2"]


# ---------------------------------------------------------------------------
# 持久化重开
# ---------------------------------------------------------------------------


def test_reopen_persisted_file_can_still_search():
    path = tempfile.mktemp(suffix=".sqlite")
    try:
        idx1 = SearchIndex(path)
        idx1.upsert("doc1", [Segment(start=0, end=1000, text="persisted content")])
        idx1.close()

        idx2 = SearchIndex(path)
        hits = idx2.search("persisted", limit=5)
        assert len(hits) == 1
        idx2.close()
    finally:
        for suffix in ("", "-wal", "-shm"):
            try:
                os.remove(path + suffix)
            except FileNotFoundError:
                pass


# ---------------------------------------------------------------------------
# snippet
# ---------------------------------------------------------------------------


def test_snippet_wraps_match_with_markers(index):
    index.upsert("doc1", [Segment(start=0, end=1000, text="前面的文字识别效果不错后面的文字")])
    hits = index.search("识别", limit=5, snippet_markers=("<<", ">>"))
    assert "<<识别>>" in hits[0].snippet


# ---------------------------------------------------------------------------
# segments_from_cues
# ---------------------------------------------------------------------------


def test_segments_from_cues_merges_by_char_budget():
    cues = [
        _FakeCue(0, 1000, "aaaa"),
        _FakeCue(1000, 2000, "bbbb"),
        _FakeCue(2000, 3000, "cccc"),
    ]
    segments = segments_from_cues(cues, max_chars=10, overlap=0)
    assert len(segments) == 2
    assert segments[0].start == 0 and segments[0].end == 2000
    assert segments[0].text == "aaaa bbbb"
    assert segments[1].start == 2000 and segments[1].end == 3000
    assert segments[1].text == "cccc"


def test_segments_from_cues_does_not_split_single_long_cue():
    cues = [_FakeCue(0, 1000, "a" * 50)]
    segments = segments_from_cues(cues, max_chars=10, overlap=0)
    assert len(segments) == 1
    assert segments[0].text == "a" * 50


def test_segments_from_cues_overlap_repeats_trailing_cue():
    cues = [
        _FakeCue(0, 1000, "aaaa"),
        _FakeCue(1000, 2000, "bbbb"),
        _FakeCue(2000, 3000, "cccc"),
    ]
    segments = segments_from_cues(cues, max_chars=9, overlap=4)
    # 第一段 "aaaa bbbb"（9 字符，正好塞满），第二段应该把 bbbb 带过去重叠
    assert segments[0].text == "aaaa bbbb"
    assert "bbbb" in segments[1].text
    assert "cccc" in segments[1].text


def test_segments_from_cues_empty_input():
    assert segments_from_cues([], max_chars=10, overlap=0) == []


def test_segments_from_cues_rejects_non_positive_max_chars():
    with pytest.raises(ValueError):
        segments_from_cues([_FakeCue(0, 1, "a")], max_chars=0, overlap=0)


def test_segments_from_cues_rejects_negative_overlap():
    with pytest.raises(ValueError):
        segments_from_cues([_FakeCue(0, 1, "a")], max_chars=10, overlap=-1)
