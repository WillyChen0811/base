"""字幕轨来源（provenance）+ 用户修正层。

来源信息在下载落盘那一刻最全（YouTube 的 automatic_captions 标记、X 平台的
词级时间标签、调用方自己知道是不是本地识别出来的），但字幕文件本身（SRT/VTT）
没地方存这些。这里用旁车文件 `<字幕文件名>.meta.json` 把来源历史和用户修正
挂在字幕文件旁边，原字幕文件永远不改。

三层数据：
- `TrackMetadata`：轨道级元数据（语言/平台/来源地址/识别引擎/翻译来源），
  外加一份来源判断历史（`OriginRecord`），只追加、不覆盖。
- `detect_origin`：从原始字幕文本里找证据，证据不足一律返回 None——不能把
  "看不出来"当成"人工上传"。
- `Correction` / `apply_corrections`：用户修正一条字幕的文本/时间，按锚点
  （原始 start/end + 原文）定位，不用序号，换版本对不上就报 orphan 不静默丢。

零业务依赖：不内置语言/平台/引擎名等默认值；`created_at`/`at` 一律由调用方
给字符串，本模块不取时间、不解析时间格式，只透传。
"""
from __future__ import annotations

import json
import os
import re
import uuid
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Iterable, Literal

from .io_ import save as _save_cues
from .models import Cue
from .rolling import looks_rolling

Origin = Literal["uploaded", "platform_asr", "local_asr", "machine_translation", "unknown"]
DeterminedBy = Literal["acquired", "detected", "user"]

# 生效优先级：user 的手动判断永远盖过自动推断，acquired（下载时拿到的元数据）
# 又比内容推断（detected）更可信；历史全保留，这只决定"当前信谁"。
_PRIORITY: dict[DeterminedBy, int] = {"detected": 0, "acquired": 1, "user": 2}

_SCHEMA_VERSION = 1


# ---------------------------------------------------------------------------
# 来源历史 / 轨道元数据
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class OriginRecord:
    """一条来源判断：谁（by）在什么时候（at）判定这条轨是什么来源（origin）。"""

    origin: Origin
    by: DeterminedBy
    at: str
    note: str = ""


@dataclass(frozen=True)
class TrackMetadata:
    """字幕轨的元数据。除 `history` 外都是可选信息，缺了就是 None。"""

    language: str | None = None
    platform: str | None = None
    source_url: str | None = None
    engine: str | None = None  # local_asr 用的识别引擎名
    derived_from: str | None = None  # machine_translation 时，翻译自哪条轨（调用方定义 id 规则）
    created_at: str | None = None
    history: tuple[OriginRecord, ...] = ()

    def with_record(self, record: OriginRecord) -> "TrackMetadata":
        """追加一条来源历史。历史只增不减，不做去重——每次判断都是一条证据。"""
        return replace(self, history=self.history + (record,))

    @property
    def effective_origin(self) -> Origin | None:
        """按 user > acquired > detected 选出当前生效的来源；同优先级取列表里靠后的
        （视为更新的判断）。没有任何历史记录时返回 None。"""
        best: OriginRecord | None = None
        for record in self.history:
            if best is None or _PRIORITY[record.by] >= _PRIORITY[best.by]:
                best = record
        return best.origin if best else None


def record_origin(track: TrackMetadata, origin: Origin, by: DeterminedBy, at: str, note: str = "") -> TrackMetadata:
    """给轨道元数据追加一条来源判断历史的便捷写法。"""
    return track.with_record(OriginRecord(origin=origin, by=by, at=at, note=note))


# ---------------------------------------------------------------------------
# 内容推断
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Evidence:
    """一条推断证据：signal 是机器可读的短标识，detail 是给人看的说明。"""

    signal: str
    detail: str


# YouTube 自动字幕逐词时间戳标签，例如 <00:00:01.000>；跟 io_._CUE_TIMESTAMP_RE
# 同一个模式，但这里要在“剥掉之前”的原始文本上找，所以不能共用 io_ 剥完之后的结果。
_WORD_TIMESTAMP_RE = re.compile(r"<(?:\d+:)?\d{2}:\d{2}\.\d{3}>")
# X 平台把 ASR 结果标成普通字幕，但正文里带这个词级时间标签，是唯一能戳穿的证据。
_X_WORD_MS_RE = re.compile(r"<X-word-ms\b", re.IGNORECASE)

_TS_LINE_RE = re.compile(
    r"(?:(\d+):)?(\d{2}):(\d{2})[.,](\d{3})\s*-->\s*(?:(\d+):)?(\d{2}):(\d{2})[.,](\d{3})"
)


def _ts_to_ms(h: str | None, m: str, s: str, ms: str) -> int:
    return ((int(h or 0) * 60 + int(m)) * 60 + int(s)) * 1000 + int(ms)


def _rough_cues(raw_text: str) -> list[Cue]:
    """从原始字幕文本粗略切出 (start, end, text)，只给 `looks_rolling` 判断结构用，
    不追求跟 pysubs2 一样精确（不处理样式标签、多轨、BOM 之类）。"""
    text = _WORD_TIMESTAMP_RE.sub("", raw_text)
    lines = text.splitlines()
    cues: list[Cue] = []
    i = 0
    while i < len(lines):
        m = _TS_LINE_RE.match(lines[i].strip())
        if m:
            h1, m1, s1, ms1, h2, m2, s2, ms2 = m.groups()
            start = _ts_to_ms(h1, m1, s1, ms1)
            end = _ts_to_ms(h2, m2, s2, ms2)
            i += 1
            body: list[str] = []
            while i < len(lines) and lines[i].strip():
                body.append(lines[i].strip())
                i += 1
            cues.append(Cue(start=start, end=end, text="\n".join(body)))
        else:
            i += 1
    return cues


def detect_origin(raw_text: str) -> tuple[list[Evidence], Origin | None]:
    """从字幕原始文本（未清洗、逐词时间戳还没被剥掉）里找有明确证据的来源特征。

    只认列出的具体特征：X 平台词级时间标签、YouTube 逐词时间戳标签、YouTube 的
    滚动式结构（复用 `rolling.looks_rolling`）。三个都没有就返回 (证据列表, None)——
    "推断不出来"绝不能当成 "uploaded"，那是两回事。
    """
    evidence: list[Evidence] = []

    if _X_WORD_MS_RE.search(raw_text):
        evidence.append(Evidence("x_word_ms_tag", "文本含 X 平台词级时间标签 <X-word-ms...>"))
        return evidence, "platform_asr"

    if _WORD_TIMESTAMP_RE.search(raw_text):
        evidence.append(Evidence("youtube_word_timestamp", "文本含逐词时间戳标签 <00:00:01.000>"))
        return evidence, "platform_asr"

    if looks_rolling(_rough_cues(raw_text)):
        evidence.append(Evidence("rolling_structure", "字幕呈滚动式结构（looks_rolling 判定）"))
        return evidence, "platform_asr"

    return evidence, None


# ---------------------------------------------------------------------------
# 用户修正层
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Anchor:
    """定位一条原字幕的锚点。用 (original_start, original_end, text) 而不是序号——
    字幕换了版本后下标会错位，这三样凑一起基本能唯一定位一条台词。"""

    original_start: int
    original_end: int
    text: str


def _anchor_key(a: Anchor) -> tuple[int, int, str]:
    return (a.original_start, a.original_end, a.text)


def anchor_of(cue: Cue) -> Anchor:
    """给一条 cue（改没改过都行）生成修正锚点：一律用原始时间和原文。"""
    return Anchor(original_start=cue.original_start, original_end=cue.original_end, text=cue.original_text)


@dataclass(frozen=True)
class Correction:
    """一条用户修正。`new_*` 缺省（None）表示这项不改。

    `id` 默认随机生成，撤销时按 id 删——列表顺序会因为去重/合并而变，不能按下标。
    """

    anchor: Anchor
    at: str
    new_text: str | None = None
    new_start: int | None = None
    new_end: int | None = None
    id: str = field(default_factory=lambda: uuid.uuid4().hex)


def remove_correction(corrections: Iterable[Correction], correction_id: str) -> list[Correction]:
    """撤销一条修正：按 id 删。"""
    return [c for c in corrections if c.id != correction_id]


def _dedupe_latest(corrections: Iterable[Correction]) -> list[Correction]:
    """同一锚点（同一句）如果被改了好几次，只留最后一条——列表里靠后的视为更新的。"""
    latest: dict[tuple[int, int, str], Correction] = {}
    order: list[tuple[int, int, str]] = []
    for c in corrections:
        key = _anchor_key(c.anchor)
        if key not in latest:
            order.append(key)
        latest[key] = c
    return [latest[k] for k in order]


def apply_corrections(
    cues: Iterable[Cue], corrections: Iterable[Correction]
) -> tuple[list[Cue], list[Correction]]:
    """把修正应用到（未经修正的、原样加载出来的）cues 上。

    顺序：load →（需要时）collapse_rolling → apply_corrections → shift。修正里的
    `new_start/new_end` 是平移之前的时间轴；界面上"设为当前时间"拿到的是平移后的
    播放位置，写修正前要减掉偏移。

    原字幕文件永不修改：这里只是在内存里生成一份应用了修正的新列表。锚点按
    (original_start, original_end, text) 全等匹配 —— cue 被 `shift` 过之后
    start/end 会变但 original_* 不变，所以锚点必须用 original_*，不能用 start/end。

    匹配不上的修正（字幕换了版本、原文改过）原样放进 orphans 返回，不静默丢弃，
    调用方可以据此提示用户"这条修正找不到对应字幕了"。
    应用成功的 cue 会带上 `user_edited=True`，供调用方知道哪些是用户改过的。
    """
    active = _dedupe_latest(corrections)
    by_anchor = {_anchor_key(c.anchor): c for c in active}
    matched: set[tuple[int, int, str]] = set()

    result: list[Cue] = []
    for cue in cues:
        key = _anchor_key(anchor_of(cue))
        correction = by_anchor.get(key)
        if correction is None:
            result.append(cue)
            continue
        matched.add(key)
        result.append(replace(
            cue,
            start=cue.start if correction.new_start is None else correction.new_start,
            end=cue.end if correction.new_end is None else correction.new_end,
            text=cue.text if correction.new_text is None else correction.new_text,
            user_edited=True,
        ))

    orphans = [c for c in active if _anchor_key(c.anchor) not in matched]
    return result, orphans


def export(
    cues: Iterable[Cue],
    corrections: Iterable[Correction],
    path: str | Path,
    format: str | None = None,
) -> tuple[Path, list[Correction]]:
    """应用修正后另存一份字幕（走现有 `io_.save`），原字幕文件不受影响。

    返回保存路径和没能应用上的 orphans，方便调用方在导出后继续提示用户。
    """
    applied, orphans = apply_corrections(cues, corrections)
    saved_path = _save_cues(applied, path, format=format)
    return saved_path, orphans


# ---------------------------------------------------------------------------
# 旁车文件：<字幕文件名>.meta.json
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Meta:
    """旁车文件的内容：轨道元数据 + 修正列表 + schema 版本号。"""

    track: TrackMetadata = field(default_factory=TrackMetadata)
    corrections: tuple[Correction, ...] = ()
    schema_version: int = _SCHEMA_VERSION


def meta_path(subtitle_path: str | Path) -> Path:
    """字幕文件对应的旁车文件路径：`<字幕文件名>.meta.json`。"""
    return Path(str(subtitle_path) + ".meta.json")


def _record_to_dict(r: OriginRecord) -> dict:
    return {"origin": r.origin, "by": r.by, "at": r.at, "note": r.note}


def _record_from_dict(d: dict) -> OriginRecord:
    return OriginRecord(origin=d["origin"], by=d["by"], at=d["at"], note=d.get("note", ""))


def _track_to_dict(t: TrackMetadata) -> dict:
    return {
        "language": t.language,
        "platform": t.platform,
        "source_url": t.source_url,
        "engine": t.engine,
        "derived_from": t.derived_from,
        "created_at": t.created_at,
        "history": [_record_to_dict(r) for r in t.history],
    }


def _track_from_dict(d: dict) -> TrackMetadata:
    return TrackMetadata(
        language=d.get("language"),
        platform=d.get("platform"),
        source_url=d.get("source_url"),
        engine=d.get("engine"),
        derived_from=d.get("derived_from"),
        created_at=d.get("created_at"),
        history=tuple(_record_from_dict(r) for r in d.get("history", [])),
    )


def _anchor_to_dict(a: Anchor) -> dict:
    return {"original_start": a.original_start, "original_end": a.original_end, "text": a.text}


def _anchor_from_dict(d: dict) -> Anchor:
    return Anchor(original_start=d["original_start"], original_end=d["original_end"], text=d["text"])


def _correction_to_dict(c: Correction) -> dict:
    return {
        "id": c.id,
        "anchor": _anchor_to_dict(c.anchor),
        "at": c.at,
        "new_text": c.new_text,
        "new_start": c.new_start,
        "new_end": c.new_end,
    }


def _correction_from_dict(d: dict) -> Correction:
    return Correction(
        id=d["id"],
        anchor=_anchor_from_dict(d["anchor"]),
        at=d["at"],
        new_text=d.get("new_text"),
        new_start=d.get("new_start"),
        new_end=d.get("new_end"),
    )


def _meta_to_dict(m: Meta) -> dict:
    return {
        "schema_version": m.schema_version,
        "track": _track_to_dict(m.track),
        "corrections": [_correction_to_dict(c) for c in m.corrections],
    }


def _meta_from_dict(d: dict) -> Meta:
    return Meta(
        track=_track_from_dict(d.get("track", {})),
        corrections=tuple(_correction_from_dict(c) for c in d.get("corrections", [])),
        schema_version=d.get("schema_version", _SCHEMA_VERSION),
    )


def load_meta(subtitle_path: str | Path) -> Meta:
    """读旁车文件；不存在就返回空的 `Meta()`（首次接触某份字幕是正常状态，不报错）。"""
    path = meta_path(subtitle_path)
    if not path.exists():
        return Meta()
    data = json.loads(path.read_text(encoding="utf-8"))
    version = data.get("schema_version", _SCHEMA_VERSION)
    if version > _SCHEMA_VERSION:
        # 新版本写的文件：按旧结构读会丢字段，再保存就把用户的修正覆盖掉了
        raise ValueError(f"{path} 的 schema_version={version} 比本库支持的 {_SCHEMA_VERSION} 新")
    return _meta_from_dict(data)


def save_meta(subtitle_path: str | Path, meta: Meta) -> Path:
    """原子写旁车文件：先写临时文件，`os.replace` 一步切换，不会读到写一半的半截 JSON。"""
    path = meta_path(subtitle_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + f".tmp-{uuid.uuid4().hex}")
    tmp.write_text(json.dumps(_meta_to_dict(meta), ensure_ascii=False, indent=2), encoding="utf-8")
    os.replace(tmp, path)
    return path
