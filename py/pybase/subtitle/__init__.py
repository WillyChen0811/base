"""字幕基础能力：解析/输出、YouTube 滚动字幕折叠、时间校准、时间索引、给大模型用的格式化。

零业务依赖：不内置语言、路径、模型等任何项目相关默认值，都由调用方传入。
时间单位统一整数毫秒。

解析/输出依赖 `pysubs2`，延迟 import——没装时调用到的函数会抛
`ImportError("pysubs2 未安装，运行 pip install \"pybase[subtitle]\"（或 pip install pysubs2）")`，
本包顶层不 import 它，`import pybase.subtitle` 本身不需要装 pysubs2。
"""
from __future__ import annotations

from .index import CueIndex, Slice
from .io_ import dumps, load, save
from .llm import Chunk, chunk, to_timestamped_text
from .models import Cue
from .provenance import (
    Anchor,
    anchor_of,
    Correction,
    Evidence,
    Meta,
    Origin,
    OriginRecord,
    TrackMetadata,
    apply_corrections,
    detect_origin,
    export,
    load_meta,
    meta_from_dict,
    meta_path,
    meta_to_dict,
    record_origin,
    remove_correction,
    save_meta,
)
from .rolling import collapse_rolling, looks_rolling
from .timing import reset_timing, shift

__all__ = [
    "Cue",
    "load",
    "dumps",
    "save",
    "collapse_rolling",
    "looks_rolling",
    "shift",
    "reset_timing",
    "CueIndex",
    "Slice",
    "to_timestamped_text",
    "chunk",
    "Chunk",
    "Origin",
    "OriginRecord",
    "TrackMetadata",
    "record_origin",
    "Evidence",
    "detect_origin",
    "Anchor",
    "anchor_of",
    "Correction",
    "apply_corrections",
    "remove_correction",
    "export",
    "Meta",
    "meta_path",
    "meta_to_dict",
    "meta_from_dict",
    "load_meta",
    "save_meta",
]
