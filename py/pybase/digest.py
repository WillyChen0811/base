"""带出处的视频内容总结：章节 + 要点，每条都能定位回字幕时间。

社区经验（字幕驱动播放器踩过的坑，只借语义不抄代码）：直接让大模型在回复里写时间戳，
它会编造、跟实际字幕对不上；正确做法是给每句字幕编号，模型只允许引用编号区间，
时间由程序按编号从原始 cues 里查回，绝不信模型自己写的时间戳。

长视频（按 `max_chars` 预算切出多块）用 map-reduce：先分块各自出章节/要点（仍然引用
全局编号），再用一次汇总调用合并出 overview 和最终章节/要点列表；块数为 1 时只调用
一次模型，不做多余的汇总请求。

零业务依赖：不 import `pybase.aiclient`，大模型调用通过 `complete` 依赖注入，调用方
自己接 `LlmClient.chat` 或别的实现；`language`/`max_chars` 无默认值，`model`/`created_at`
由调用方传入字符串，本模块不取当前时间、不知道 complete 背后是什么模型。
"""
from __future__ import annotations

import hashlib
import json
import re
import threading
from dataclasses import dataclass, replace
from typing import Callable, Sequence

from .subtitle.models import Cue

_SCHEMA_VERSION = 1


class DigestError(Exception):
    """总结生成失败：模型输出解析不出合法 JSON，或章节/要点全部无效。"""


class DigestCancelled(DigestError):
    """任务被取消（`cancel_event` 置位）。不复用 `aiclient.AiCanceled`——本模块不 import
    aiclient，取消语义自己定义一个子类，调用方按 `except DigestCancelled` 识别即可。"""


# ---------------------------------------------------------------------------
# 数据结构
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Chapter:
    """一个章节。`start_index`/`end_index` 是引用的 cue 下标（闭区间，对应原始
    `cues` 列表），`start`/`end` 是据此从 cues 查出的毫秒时间——不是模型直接给的。"""

    title: str
    summary: str
    start_index: int
    end_index: int
    start: int
    end: int

    def to_dict(self) -> dict:
        return {
            "title": self.title,
            "summary": self.summary,
            "start_index": self.start_index,
            "end_index": self.end_index,
            "start": self.start,
            "end": self.end,
        }

    @classmethod
    def from_dict(cls, data: dict) -> "Chapter":
        return cls(
            title=data["title"],
            summary=data["summary"],
            start_index=data["start_index"],
            end_index=data["end_index"],
            start=data["start"],
            end=data["end"],
        )


@dataclass(frozen=True)
class Takeaway:
    """一条要点，语义同 `Chapter`：编号定位、时间查表得来。"""

    text: str
    start_index: int
    end_index: int
    start: int
    end: int

    def to_dict(self) -> dict:
        return {
            "text": self.text,
            "start_index": self.start_index,
            "end_index": self.end_index,
            "start": self.start,
            "end": self.end,
        }

    @classmethod
    def from_dict(cls, data: dict) -> "Takeaway":
        return cls(
            text=data["text"],
            start_index=data["start_index"],
            end_index=data["end_index"],
            start=data["start"],
            end=data["end"],
        )


@dataclass(frozen=True)
class Digest:
    """一次总结的完整结果。

    `source_hash` 是输入 cues（时间 + 文本）的哈希，调用方拿它跟上次的值比对，
    判断字幕是不是变了、要不要重新算一遍——库本身不做缓存决策。
    `model`/`created_at` 都是调用方传入的字符串，本模块不知道 `complete` 背后
    是什么模型、也不取当前时间，保持可测。
    """

    overview: str
    chapters: tuple[Chapter, ...]
    takeaways: tuple[Takeaway, ...]
    model: str | None
    language: str
    source_hash: str
    created_at: str | None
    schema_version: int = _SCHEMA_VERSION

    def to_dict(self) -> dict:
        return {
            "schema_version": self.schema_version,
            "overview": self.overview,
            "chapters": [c.to_dict() for c in self.chapters],
            "takeaways": [t.to_dict() for t in self.takeaways],
            "model": self.model,
            "language": self.language,
            "source_hash": self.source_hash,
            "created_at": self.created_at,
        }

    @classmethod
    def from_dict(cls, data: dict) -> "Digest":
        return cls(
            overview=data["overview"],
            chapters=tuple(Chapter.from_dict(c) for c in data.get("chapters", [])),
            takeaways=tuple(Takeaway.from_dict(t) for t in data.get("takeaways", [])),
            model=data.get("model"),
            language=data["language"],
            source_hash=data["source_hash"],
            created_at=data.get("created_at"),
            schema_version=data.get("schema_version", _SCHEMA_VERSION),
        )


def compute_source_hash(cues: Sequence[Cue]) -> str:
    """对 cues 的时间 + 文本做 SHA-256，字幕内容或时间轴变了值就变，顺序也算在内。"""
    hasher = hashlib.sha256()
    for cue in cues:
        hasher.update(str(cue.start).encode("utf-8"))
        hasher.update(b"\0")
        hasher.update(str(cue.end).encode("utf-8"))
        hasher.update(b"\0")
        hasher.update(cue.text.encode("utf-8"))
        hasher.update(b"\n")
    return hasher.hexdigest()


# ---------------------------------------------------------------------------
# 编号 + 分块（编号必须是全局下标，块内块外都一样，模型和最终解析都按这个编号对时间）
# ---------------------------------------------------------------------------


def _numbered_line(index: int, cue: Cue) -> str:
    # 只给编号不给时间：时间占约四成字数，还诱导模型自己写时间区间
    return f"[{index}] {cue.text.replace(chr(10), ' ')}"


def _split_chunks(cues: Sequence[Cue], max_chars: int) -> list[list[int]]:
    """按 `_numbered_line` 的字符预算切块，返回每块的全局下标列表（不拆开单条 cue）。

    单条 cue 自己的编号行就超过 max_chars 时单独成一块——跟 `subtitle.llm.chunk`
    同样的让步：截断内容不是切块该做的事。
    """
    chunks: list[list[int]] = []
    current: list[int] = []
    current_len = 0
    for i, cue in enumerate(cues):
        line_len = len(_numbered_line(i, cue)) + 1
        if current and current_len + line_len > max_chars:
            chunks.append(current)
            current = []
            current_len = 0
        current.append(i)
        current_len += line_len
    if current:
        chunks.append(current)
    return chunks


# ---------------------------------------------------------------------------
# 提示词
# ---------------------------------------------------------------------------

_JSON_SHAPE = (
    '{"overview": "一句话概述", '
    '"chapters": [{"title": "...", "summary": "...", "start": 编号, "end": 编号}, ...], '
    '"takeaways": [{"text": "...", "start": 编号, "end": 编号}, ...]}'
)


def _system_prompt(language: str) -> str:
    return (
        "你是课程内容整理助手。输入是带编号的字幕，每行格式是 `[编号] 文本`，"
        "编号按时间顺序排列。引用位置时只能用这些编号，不要写任何时间或时间区间。"
        f"输出语言：{language}。严格输出 JSON，不要用 markdown 代码块包裹，不要输出任何多余文字。"
    )


def _chunk_user_prompt(
    numbered_text: str,
    language: str,
    *,
    whole: bool,
    first_index: int,
    last_index: int,
) -> str:
    scope = "整段内容" if whole else "这一段"
    takeaway_count = "10–20" if whole else "3–6"
    return (
        "以下是一段字幕，每行一句，已经按顺序编号：\n\n"
        f"{numbered_text}\n\n"
        f"请用{language}分析这段内容，划分出章节、提炼要点，满足以下要求：\n"
        f"1. overview：用 3–5 句话概述{scope}。\n"
        "2. chapters：按讲的内容划分，在话题切换、换题目、换问题的地方分章，"
        "不要按长度平均切，也不要为凑章数硬拆或硬并；按时间顺序、前后相接，"
        f"合起来覆盖整段：第一章 start 必须是 {first_index}，"
        f"最后一章 end 必须是 {last_index}，相邻两章既不漏编号也不重叠；"
        "每章一般 3–15 分钟的内容。title 要具体：在讲题就写出第几题、哪一问、"
        "考什么知识点（例如\"试题三第 2 问：数据流图局部出错定位\"），"
        "不要写\"习题讲解\"\"知识点串讲\"这类空话；"
        "summary 用 2–4 句说清这一章具体讲了什么。\n"
        f"3. takeaways：挑 {takeaway_count} 条最值得记住的知识点，每条引用编号区间。\n"
        f"严格输出如下 JSON：\n{_JSON_SHAPE}\n"
        "start/end 必须是上面出现过的编号、闭区间、start <= end，不要引用没出现过的编号。"
    )


def _merge_user_prompt(sections_text: str, language: str) -> str:
    return (
        "以下是一段长字幕切成若干块后，每块各自分析出的结果（编号仍是原字幕的全局编号，"
        "没有重新编号）：\n\n"
        f"{sections_text}\n\n"
        f"请用{language}把这些块的结果合并成整段视频统一的总结，满足以下要求：\n"
        "1. chapters：按讲的内容划分，在话题切换、换题目、换问题的地方分章，"
        "不要按长度平均切，也不要为凑章数硬拆或硬并；章节按时间顺序前后相接，"
        "合起来覆盖整段，相邻两章既不漏编号也不重叠。"
        "title 要具体：写出第几题、哪一问、考什么知识点，"
        "不要写\"习题讲解\"\"知识点串讲\"这类空话；summary 用 2–4 句说清这一章具体讲了什么。\n"
        "2. takeaways：挑 10–20 条最值得记住的知识点，每条引用编号区间。\n"
        f"严格输出如下 JSON：\n{_JSON_SHAPE}\n"
        "start/end 必须引用上面已经出现过的编号，不要编造新的编号。"
    )


# ---------------------------------------------------------------------------
# 模型输出解析（健壮性：容忍包裹、丢弃/修正无效条目）
# ---------------------------------------------------------------------------

_JSON_BLOCK_RE = re.compile(r"```(?:json)?\s*(.*?)\s*```", re.DOTALL)


def _extract_json(text: str) -> dict:
    """从模型回复里挖出 JSON 对象：优先取 ```json ...``` 代码块，否则整段文本尝试解析，
    再兜底取第一个 `{` 到最后一个 `}` 之间的内容（模型常在 JSON 前后加废话）。"""
    match = _JSON_BLOCK_RE.search(text)
    candidate = match.group(1) if match else text
    try:
        data = json.loads(candidate)
    except json.JSONDecodeError:
        start = candidate.find("{")
        end = candidate.rfind("}")
        if start == -1 or end == -1 or end <= start:
            raise DigestError(f"模型输出不是合法 JSON：{text[:200]!r}")
        try:
            data = json.loads(candidate[start : end + 1])
        except json.JSONDecodeError as exc:
            raise DigestError(f"模型输出不是合法 JSON：{text[:200]!r}") from exc
    if not isinstance(data, dict):
        raise DigestError(f"模型输出的 JSON 不是对象：{text[:200]!r}")
    return data


def _valid_index(value, n: int) -> int | None:
    """编号必须是 [0, n) 内的整数（`bool` 是 `int` 子类，显式排除）；越界/非整数返回 None，
    调用方按 None 丢弃这一条——不试图猜测调用者想指哪一句。"""
    if isinstance(value, bool) or not isinstance(value, int):
        return None
    if value < 0 or value >= n:
        return None
    return value


def _build_chapters(raw, cues: Sequence[Cue]) -> list[Chapter]:
    n = len(cues)
    parsed: list[Chapter] = []
    for item in raw or []:
        if not isinstance(item, dict):
            continue
        start_index = _valid_index(item.get("start"), n)
        end_index = _valid_index(item.get("end"), n)
        title = item.get("title")
        if start_index is None or end_index is None or end_index < start_index:
            continue  # 越界/反向的条目直接丢弃
        if not isinstance(title, str) or not title.strip():
            continue
        summary = item.get("summary")
        summary = summary if isinstance(summary, str) else ""
        parsed.append(
            Chapter(
                title=title,
                summary=summary,
                start_index=start_index,
                end_index=end_index,
                start=cues[start_index].start,
                end=cues[end_index].end,
            )
        )
    return _dedupe_overlapping_chapters(parsed, cues)


def _dedupe_overlapping_chapters(chapters: list[Chapter], cues: Sequence[Cue]) -> list[Chapter]:
    """章节按起点排序后去重叠：

    - 后一个完全落在前一个区间内（被前一个包含）：丢弃后一个，它不带来新信息；
    - 后一个起点早于前一个终点但延伸到前一个之外：截断前一个的终点到"后一个起点的前一句"；
      截断后前一个变成空/反向区间（起点都保不住）就整条丢弃前一个。
    """
    ordered = sorted(chapters, key=lambda c: (c.start_index, c.end_index))
    result: list[Chapter] = []
    for chapter in ordered:
        if result and chapter.start_index <= result[-1].end_index:
            prev = result[-1]
            if chapter.end_index <= prev.end_index:
                continue  # 完全被前一个包含，丢弃这个
            new_end_index = chapter.start_index - 1
            if new_end_index < prev.start_index:
                result.pop()
            else:
                result[-1] = replace(prev, end_index=new_end_index, end=cues[new_end_index].end)
        result.append(chapter)
    return result


def _build_takeaways(raw, cues: Sequence[Cue]) -> list[Takeaway]:
    n = len(cues)
    out: list[Takeaway] = []
    for item in raw or []:
        if not isinstance(item, dict):
            continue
        start_index = _valid_index(item.get("start"), n)
        end_index = _valid_index(item.get("end"), n)
        text = item.get("text")
        if start_index is None or end_index is None or end_index < start_index:
            continue
        if not isinstance(text, str) or not text.strip():
            continue
        out.append(
            Takeaway(
                text=text,
                start_index=start_index,
                end_index=end_index,
                start=cues[start_index].start,
                end=cues[end_index].end,
            )
        )
    # 要点本身可以互相重叠（多个要点提到同一句很正常），只排序不去重叠
    out.sort(key=lambda t: (t.start_index, t.end_index))
    return out


def _render_sections(section_summaries: list[dict]) -> str:
    parts = []
    for i, section in enumerate(section_summaries, start=1):
        parts.append(f"【块{i}】{json.dumps(section, ensure_ascii=False)}")
    return "\n".join(parts)


def _check_cancel(cancel_event: threading.Event | None) -> None:
    if cancel_event is not None and cancel_event.is_set():
        raise DigestCancelled("任务已取消")


# ---------------------------------------------------------------------------
# 主入口
# ---------------------------------------------------------------------------


def summarize(
    cues: Sequence[Cue],
    complete: Callable[[list[dict]], str],
    *,
    language: str,
    max_chars: int,
    cancel_event: threading.Event | None = None,
    on_progress: Callable[[int, int], None] | None = None,
    model: str | None = None,
    created_at: str | None = None,
) -> Digest:
    """给 cues 生成带出处的总结（章节 + 要点）。

    `complete` 是 `messages(list[dict]) -> 回复文本` 的依赖注入，不 import
    `pybase.aiclient`；建议调用方传 `lambda messages: client.chat(messages, json_mode=False)`——
    有的本地服务（实测 oMLX + Qwen3）开了 response_format=json_object 会关掉模型的思考、
    甚至把思考内容塞进 JSON 键名里，而本模块的解析本来就能从普通文本里挖出 JSON。
    `language`/`max_chars` 必传，无默认值。`model`/`created_at` 是可选的透传字符串，
    只落进返回的 `Digest`，本函数不做任何取值/取时间的逻辑（保持可测、零业务依赖）。

    上下文够大的模型建议把 `max_chars` 设大，让整段一次处理，效果明显好于分块。
    长度超过 `max_chars` 会先分块各自总结再合一次汇总；`on_progress(done, total)`
    只报"分块总结"这一段的进度（`total` 是块数），汇总那一次调用不算进度。
    """
    if not language:
        raise ValueError("language 不能为空")
    if not max_chars or max_chars <= 0:
        raise ValueError("max_chars 必须为正数")

    cues = list(cues)
    if not cues:
        raise DigestError("cues 为空，无法生成总结")

    source_hash = compute_source_hash(cues)
    chunks = _split_chunks(cues, max_chars)
    total = len(chunks)

    def call(messages: list[dict]) -> str:
        _check_cancel(cancel_event)
        return complete(messages)

    if total == 1:
        indices = chunks[0]
        numbered = "\n".join(_numbered_line(i, cues[i]) for i in indices)
        raw = call(
            [
                {"role": "system", "content": _system_prompt(language)},
                {
                    "role": "user",
                    "content": _chunk_user_prompt(
                        numbered, language, whole=True, first_index=indices[0], last_index=indices[-1]
                    ),
                },
            ]
        )
        if on_progress:
            on_progress(1, 1)
        parsed = _extract_json(raw)
        overview = parsed.get("overview") if isinstance(parsed.get("overview"), str) else ""
        chapters = _build_chapters(parsed.get("chapters"), cues)
        takeaways = _build_takeaways(parsed.get("takeaways"), cues)
    else:
        section_summaries: list[dict] = []
        for done, indices in enumerate(chunks, start=1):
            numbered = "\n".join(_numbered_line(i, cues[i]) for i in indices)
            raw = call(
                [
                    {"role": "system", "content": _system_prompt(language)},
                    {
                        "role": "user",
                        "content": _chunk_user_prompt(
                            numbered, language, whole=False, first_index=indices[0], last_index=indices[-1]
                        ),
                    },
                ]
            )
            try:
                section_summaries.append(_extract_json(raw))
            except DigestError as exc:
                raise DigestError(f"第 {done}/{total} 块模型输出解析失败：{exc}") from exc
            if on_progress:
                on_progress(done, total)

        sections_text = _render_sections(section_summaries)
        merge_raw = call(
            [
                {"role": "system", "content": _system_prompt(language)},
                {"role": "user", "content": _merge_user_prompt(sections_text, language)},
            ]
        )
        merged = _extract_json(merge_raw)
        overview = merged.get("overview") if isinstance(merged.get("overview"), str) else ""
        chapters = _build_chapters(merged.get("chapters"), cues)
        takeaways = _build_takeaways(merged.get("takeaways"), cues)

    if not chapters and not takeaways:
        raise DigestError("模型输出的章节/要点全部无效（编号越界、反向或格式不对），未能生成总结")

    return Digest(
        overview=overview,
        chapters=tuple(chapters),
        takeaways=tuple(takeaways),
        model=model,
        language=language,
        source_hash=source_hash,
        created_at=created_at,
        schema_version=_SCHEMA_VERSION,
    )
