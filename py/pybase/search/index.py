"""字幕/文本片段的本地检索：SQLite FTS5 关键词 + 暴力余弦语义，RRF 融合。

零第三方依赖：关键词路用 SQLite 自带的 FTS5（trigram tokenizer，本机 SQLite ≥3.34
起支持），语义路把向量存成 float32 BLOB，查询时纯 Python 算余弦——个人媒体库的规模
（几万条片段）用不着专门的向量索引，也不引入 sqlite-vec 这类额外依赖。

中文关键词检索：FTS5 默认的 unicode61 分词器不切中文（一整句连续汉字会被当成一个
token），trigram tokenizer 按字符三元组切，对中文是天然可用的子串匹配；但 trigram
对短于 3 个字符的查询会直接返回空（SQLite 文档明确写了这个限制，本模块也用
真实数据验证过），而中文最常见的查询恰恰是 1~2 个字（“识别”“模型”）。所以查询词
按有效字符数分流：≥3 走 trigram（子串匹配，大小写不敏感，中英文通用），<3 走
LIKE 全表扫描兜底（个人库规模够用；真上千万条片段再考虑专门的双字索引）。

多词查询（空格分隔）按 AND 语义：候选集在各词的命中集合上求交集。为了在不引入
分词器（jieba 等）的前提下安全处理任意查询词——包括引号、括号这类 FTS5 语法
字符——统一把每个词包成 FTS5 的引号短语（`"..."`，内部双引号翻倍转义）交给
trigram，短语语法下括号、冒号、星号等操作符字符都不再有特殊含义，不会让查询报错。
"""
from __future__ import annotations

import array
import json
import math
import re
import sqlite3
import threading
from typing import Callable, Iterable, Optional, Sequence

from .models import Hit, Segment

_SCHEMA = """
CREATE TABLE IF NOT EXISTS docs (
    doc_id TEXT PRIMARY KEY,
    meta TEXT
);

CREATE TABLE IF NOT EXISTS segments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    doc_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    start INTEGER NOT NULL,
    end INTEGER NOT NULL,
    text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_segments_doc ON segments(doc_id);

CREATE VIRTUAL TABLE IF NOT EXISTS fts_tri USING fts5(
    text, tokenize="trigram"
);

CREATE TABLE IF NOT EXISTS vectors (
    id INTEGER PRIMARY KEY,
    model TEXT NOT NULL,
    dim INTEGER NOT NULL,
    vector BLOB NOT NULL
);
"""

_RRF_K = 60  # RRF 融合常数，社区常用默认值


def _escape_like(token: str) -> str:
    return token.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")


def _fts_phrase(token: str) -> str:
    """把任意字符串包成 FTS5 引号短语，短语内部只需转义双引号本身。"""
    return '"' + token.replace('"', '""') + '"'


def _vector_to_blob(vector: Sequence[float]) -> bytes:
    return array.array("f", vector).tobytes()


def _blob_to_vector(blob: bytes) -> array.array:
    arr = array.array("f")
    arr.frombytes(blob)
    return arr


def _cosine(a: array.array, b: array.array) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0 or nb == 0:
        return 0.0
    return dot / (na * nb)


class SearchIndex:
    """字幕/文本片段的本地检索索引。

    `path` 是 SQLite 文件路径，`:memory:` 可用于测试。同一个实例内部用一把锁
    串行化所有读写——SQLite 的写操作本身互斥，FTS5+向量的组合查询也不是热路径
    到需要真并发的程度，锁住整个实例最简单也最不容易出错。连接开
    `check_same_thread=False` 是配合这把锁：多线程可以共用同一个 `SearchIndex`
    实例，但不要绕开它直接拿 `_con` 用。
    """

    def __init__(self, path: str):
        self._con = sqlite3.connect(path, check_same_thread=False)
        self._con.execute("PRAGMA journal_mode=WAL")
        self._lock = threading.RLock()
        with self._lock:
            self._con.executescript(_SCHEMA)
            self._con.commit()

    def __enter__(self) -> "SearchIndex":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def close(self) -> None:
        with self._lock:
            self._con.close()

    # ---------- 写入 ----------

    def upsert(
        self,
        doc_id: str,
        segments: Sequence[Segment],
        *,
        meta: Optional[dict] = None,
        embed: Optional[Callable[[list[str]], list[list[float]]]] = None,
        embed_model: str = "",
        embed_batch_size: int = 64,
    ) -> None:
        """替换 `doc_id` 的全部片段。

        `embed` 给了才存向量，按 `embed_batch_size` 分批调用；`embed_model` 是
        调用方给这批向量起的模型标识，查询时用来判断能不能跟当前查询向量比较
        （维度不一致、或模型标识不一致的向量，在语义检索里会被跳过而不是报错——
        个人库里换过 embedding 模型是常态，不该让旧片段的向量拖垮整次查询）。
        """
        with self._lock:
            self._remove_locked(doc_id)
            self._con.execute(
                "INSERT INTO docs(doc_id, meta) VALUES (?, ?)",
                (doc_id, json.dumps(meta, ensure_ascii=False) if meta is not None else None),
            )
            seg_ids: list[int] = []
            for seq, seg in enumerate(segments):
                cur = self._con.execute(
                    "INSERT INTO segments(doc_id, seq, start, end, text) VALUES (?, ?, ?, ?, ?)",
                    (doc_id, seq, seg.start, seg.end, seg.text),
                )
                seg_id = cur.lastrowid
                seg_ids.append(seg_id)
                self._con.execute(
                    "INSERT INTO fts_tri(rowid, text) VALUES (?, ?)", (seg_id, seg.text)
                )

            if embed is not None and segments:
                texts = [seg.text for seg in segments]
                for batch_start in range(0, len(texts), embed_batch_size):
                    batch_texts = texts[batch_start : batch_start + embed_batch_size]
                    batch_ids = seg_ids[batch_start : batch_start + embed_batch_size]
                    vectors = embed(batch_texts)
                    if len(vectors) != len(batch_texts):
                        raise ValueError(
                            f"embed 返回 {len(vectors)} 条向量，跟输入的 {len(batch_texts)} 条文本数量不一致"
                        )
                    for seg_id, vector in zip(batch_ids, vectors):
                        self._con.execute(
                            "INSERT INTO vectors(id, model, dim, vector) VALUES (?, ?, ?, ?)",
                            (seg_id, embed_model, len(vector), _vector_to_blob(vector)),
                        )
            self._con.commit()

    def remove(self, doc_id: str) -> None:
        with self._lock:
            self._remove_locked(doc_id)
            self._con.commit()

    def _remove_locked(self, doc_id: str) -> None:
        rows = self._con.execute(
            "SELECT id FROM segments WHERE doc_id = ?", (doc_id,)
        ).fetchall()
        seg_ids = [r[0] for r in rows]
        for seg_id in seg_ids:
            self._con.execute("DELETE FROM fts_tri WHERE rowid = ?", (seg_id,))
        self._con.execute("DELETE FROM vectors WHERE id IN (SELECT id FROM segments WHERE doc_id = ?)", (doc_id,))
        self._con.execute("DELETE FROM segments WHERE doc_id = ?", (doc_id,))
        self._con.execute("DELETE FROM docs WHERE doc_id = ?", (doc_id,))

    def doc_ids(self) -> list[str]:
        with self._lock:
            rows = self._con.execute("SELECT doc_id FROM docs ORDER BY doc_id").fetchall()
            return [r[0] for r in rows]

    # ---------- 查询 ----------

    def search(
        self,
        query: str,
        *,
        limit: int,
        embed: Optional[Callable[[list[str]], list[list[float]]]] = None,
        embed_model: str = "",
        doc_ids: Optional[Sequence[str]] = None,
        snippet_markers: tuple[str, str] = ("[", "]"),
        snippet_radius: int = 60,
    ) -> list[Hit]:
        """混合检索：`embed` 给了且库里有可比的向量时走关键词+语义 RRF 融合，
        否则只走关键词。`doc_ids` 给了就只在这些 doc 里查。
        """
        query = (query or "").strip()
        if not query or limit <= 0:
            return []
        with self._lock:
            doc_filter = set(doc_ids) if doc_ids is not None else None
            keyword_scores = self._keyword_candidates(query, doc_filter)
            semantic_scores: dict[int, float] = {}
            if embed is not None:
                semantic_scores = self._semantic_candidates(query, embed, embed_model, doc_filter)

            semantic_scores = self._top_semantic_pool(semantic_scores, limit)

            if not keyword_scores and not semantic_scores:
                return []

            fused = self._fuse(keyword_scores, semantic_scores)
            top = sorted(fused.items(), key=lambda kv: kv[1][0], reverse=True)[:limit]

            hits: list[Hit] = []
            for seg_id, (score, matched_by) in top:
                row = self._con.execute(
                    "SELECT doc_id, start, end, text FROM segments WHERE id = ?", (seg_id,)
                ).fetchone()
                if row is None:
                    continue
                doc_id, start, end, text = row
                snippet = self._make_snippet(text, query, snippet_radius, snippet_markers)
                hits.append(
                    Hit(
                        doc_id=doc_id,
                        start=start,
                        end=end,
                        text=text,
                        score=score,
                        snippet=snippet,
                        matched_by=matched_by,
                    )
                )
            return hits

    def _keyword_candidates(self, query: str, doc_filter: Optional[set]) -> dict[int, float]:
        tokens = [t for t in query.split() if t]
        if not tokens:
            return {}
        per_token: list[dict[int, float]] = []
        for token in tokens:
            per_token.append(self._match_token(token, doc_filter))
            if not per_token[-1]:
                return {}  # AND 语义：任一词没有命中，整体就没有命中
        # 多词 AND：交集，分数取各词分数之和（分数约定“越大越相关”）
        common_ids = set(per_token[0].keys())
        for scores in per_token[1:]:
            common_ids &= set(scores.keys())
        return {seg_id: sum(scores[seg_id] for scores in per_token) for seg_id in common_ids}

    def _match_token(self, token: str, doc_filter: Optional[set]) -> dict[int, float]:
        if len(token) >= 3:
            return self._match_token_trigram(token, doc_filter)
        return self._match_token_like(token, doc_filter)

    def _match_token_trigram(self, token: str, doc_filter: Optional[set]) -> dict[int, float]:
        phrase = _fts_phrase(token)
        try:
            rows = self._con.execute(
                "SELECT rowid, bm25(fts_tri) FROM fts_tri WHERE fts_tri MATCH ?", (phrase,)
            ).fetchall()
        except sqlite3.OperationalError:
            return {}
        scores: dict[int, float] = {}
        for seg_id, bm25 in rows:
            if doc_filter is not None and not self._seg_in_docs(seg_id, doc_filter):
                continue
            scores[seg_id] = -bm25  # bm25 越小越相关，取负变成越大越相关
        return scores

    def _match_token_like(self, token: str, doc_filter: Optional[set]) -> dict[int, float]:
        pattern = f"%{_escape_like(token)}%"
        sql = "SELECT id, doc_id, text FROM segments WHERE text LIKE ? ESCAPE '\\'"
        rows = self._con.execute(sql, (pattern,)).fetchall()
        scores: dict[int, float] = {}
        for seg_id, doc_id, text in rows:
            if doc_filter is not None and doc_id not in doc_filter:
                continue
            occurrences = text.count(token)
            scores[seg_id] = 1.0 + 0.1 * occurrences  # 没有 bm25 可用，弱排序：出现次数多一点更相关
        return scores

    def _seg_in_docs(self, seg_id: int, doc_filter: set) -> bool:
        row = self._con.execute("SELECT doc_id FROM segments WHERE id = ?", (seg_id,)).fetchone()
        return row is not None and row[0] in doc_filter

    def _semantic_candidates(
        self,
        query: str,
        embed: Callable[[list[str]], list[list[float]]],
        embed_model: str,
        doc_filter: Optional[set],
    ) -> dict[int, float]:
        vectors = embed([query])
        if not vectors:
            return {}
        query_vec = array.array("f", vectors[0])

        sql = "SELECT v.id, v.model, v.dim, v.vector FROM vectors v"
        params: tuple = ()
        if doc_filter is not None:
            sql += " JOIN segments s ON s.id = v.id WHERE s.doc_id IN ({})".format(
                ",".join("?" for _ in doc_filter)
            )
            params = tuple(doc_filter)
        rows = self._con.execute(sql, params).fetchall()

        scores: dict[int, float] = {}
        for seg_id, model, dim, blob in rows:
            if model != embed_model or dim != len(query_vec):
                continue  # 模型/维度不一致：跳过这条，不参与语义路，不报错
            vec = _blob_to_vector(blob)
            scores[seg_id] = _cosine(query_vec, vec)
        return scores

    @staticmethod
    def _top_semantic_pool(scores: dict[int, float], limit: int) -> dict[int, float]:
        """语义候选先按相似度过滤/截断，再拿去融合——暴力算完全库的余弦后，
        不能让大量弱相关（或 0 相关）的片段都进 RRF 陪跑，挤占真正相关的排名。
        """
        pool_size = max(limit * 20, 100)
        positive = [(seg_id, score) for seg_id, score in scores.items() if score > 0]
        positive.sort(key=lambda kv: kv[1], reverse=True)
        return dict(positive[:pool_size])

    @staticmethod
    def _fuse(
        keyword_scores: dict[int, float], semantic_scores: dict[int, float]
    ) -> dict[int, tuple[float, str]]:
        if not semantic_scores:
            return {seg_id: (score, "keyword") for seg_id, score in keyword_scores.items()}
        if not keyword_scores:
            return {seg_id: (score, "semantic") for seg_id, score in semantic_scores.items()}

        def ranks(scores: dict[int, float]) -> dict[int, int]:
            ordered = sorted(scores.items(), key=lambda kv: kv[1], reverse=True)
            return {seg_id: i + 1 for i, (seg_id, _) in enumerate(ordered)}

        kw_rank = ranks(keyword_scores)
        sem_rank = ranks(semantic_scores)
        all_ids = set(kw_rank) | set(sem_rank)
        fused: dict[int, tuple[float, str]] = {}
        for seg_id in all_ids:
            rrf = 0.0
            if seg_id in kw_rank:
                rrf += 1.0 / (_RRF_K + kw_rank[seg_id])
            if seg_id in sem_rank:
                rrf += 1.0 / (_RRF_K + sem_rank[seg_id])
            matched_by = "both" if seg_id in kw_rank and seg_id in sem_rank else (
                "keyword" if seg_id in kw_rank else "semantic"
            )
            fused[seg_id] = (rrf, matched_by)
        return fused

    @staticmethod
    def _make_snippet(text: str, query: str, radius: int, markers: tuple[str, str]) -> str:
        open_mark, close_mark = markers
        needle = query.split()[0] if query.split() else query
        pos = text.lower().find(needle.lower()) if needle else -1
        if pos < 0:
            excerpt = text[: radius * 2]
            return excerpt if len(excerpt) == len(text) else excerpt + "…"
        start = max(0, pos - radius)
        end = min(len(text), pos + len(needle) + radius)
        prefix = "…" if start > 0 else ""
        suffix = "…" if end < len(text) else ""
        before = text[start:pos]
        hit = text[pos : pos + len(needle)]
        after = text[pos + len(needle) : end]
        return f"{prefix}{before}{open_mark}{hit}{close_mark}{after}{suffix}"
