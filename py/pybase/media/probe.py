"""ffprobe 包装：拿时长和音视频流概要，整理成规整 dict。

只做映射，不做业务判断（比如"这条流该不该用"留给调用方）。
"""
from __future__ import annotations

import json
import os
import threading

from ._proc import MediaError, resolve_tool, run


def probe(
    path: str | os.PathLike,
    *,
    ffprobe: str | None = None,
    timeout: float = 30,
    cancel_event: "threading.Event | None" = None,
) -> dict:
    """返回 `{"duration_ms": int|None, "streams": [...], "container": (...)}`。

    每条 stream 只保留业务用得上的字段：`codec_type`/`codec_name`，视频再加
    `width`/`height`/`fps`，音频再加 `sample_rate`/`channels`，有语言标签
    （字幕/音轨常见）就带 `language`；封面图伪装的视频流带 `attached_pic: True`。ffprobe 原始 JSON 字段远不止这些，
    这里不透传全部，用不上的不留。

    `container` 是 ffprobe `format_name` 按逗号拆开、去空格、转小写后的
    tuple——ffprobe 常把等价格式合并报告（比如 mp4/mov/m4a 一起报成
    `"mov,mp4,m4a,3gp,3g2,mj2"`），拆开方便按 token 做包含判断。**注意**：
    mkv 和 webm 共用同一个 demuxer，两者的 `format_name` 都是
    `"matroska,webm"`，ffprobe 层面无法区分，这里也不试图猜——需要精确
    区分时调用方自己按文件扩展名兜底。
    """
    tool = resolve_tool(ffprobe, "ffprobe")
    cmd = [
        tool, "-v", "error", "-print_format", "json",
        "-show_format", "-show_streams", str(path),
    ]
    result = run(cmd, timeout=timeout, cancel_event=cancel_event)
    if result.returncode != 0:
        raise MediaError(f"ffprobe 失败：{(result.stderr or '').strip()[-2000:]}")
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise MediaError("ffprobe 输出不是合法 JSON") from exc

    fmt = data.get("format") or {}
    duration = fmt.get("duration")
    duration_ms = (
        round(float(duration) * 1000) if duration not in (None, "N/A") else None
    )
    format_name = fmt.get("format_name") or ""
    container = tuple(
        tok.strip().lower() for tok in format_name.split(",") if tok.strip()
    )

    streams = []
    for s in data.get("streams") or []:
        entry = {
            "index": s.get("index"),
            "codec_type": s.get("codec_type"),
            "codec_name": s.get("codec_name"),
        }
        if s.get("codec_type") == "video":
            entry["width"] = s.get("width")
            entry["height"] = s.get("height")
            entry["fps"] = _parse_rate(s.get("avg_frame_rate"))
            # mp3/m4a 的封面图在 ffprobe 里是一条 mjpeg/png 视频流，不是真视频
            if (s.get("disposition") or {}).get("attached_pic"):
                entry["attached_pic"] = True
        elif s.get("codec_type") == "audio":
            sample_rate = s.get("sample_rate")
            entry["sample_rate"] = int(sample_rate) if sample_rate else None
            entry["channels"] = s.get("channels")
        language = (s.get("tags") or {}).get("language")
        if language:
            entry["language"] = language
        streams.append(entry)

    return {"duration_ms": duration_ms, "streams": streams, "container": container}


def _parse_rate(value: str | None) -> float | None:
    """ffprobe 的帧率是 `"30000/1001"` 这种分数字符串，转成 float。"""
    if not value or value in ("0/0", "N/A"):
        return None
    try:
        num, _, den = value.partition("/")
        num_f, den_f = float(num), float(den or 1)
        return num_f / den_f if den_f else None
    except ValueError:
        return None
