"""音频基础能力：解码成 PCM、能量 VAD、麦克风轨与会议混音对齐并标说话人。

- `decode_pcm(path, sr=16000)`：任意音视频 → 单声道 float32 PCM（经 ffmpeg 管道，不落中间文件）
- `speech_segments(pcm, sr)`：能量 VAD，阈值缺省按信号自适应 → `[(start_ms, end_ms)]`
- `align_offset(mic_pcm, mix_pcm, sr)`：麦克风轨相对混音轨的偏移（`混音时间 - 麦克风时间`）；
  `align_detail` 带各块结果
- `attribute(mic_segs, mix_segs, offset_ms, mic_pcm, mix_pcm, sr)`：混音每段话标
  self / remote / both / unknown

依赖 numpy（`pip install "pybase[audio]"`），解码另需 ffmpeg 可执行文件。包本身
import 不碰 numpy：用到哪个名字才加载对应子模块。用法、机器分工、阈值来历与
局限见同目录 README.md。
"""
from __future__ import annotations

_EXPORTS = {
    "decode_pcm": "pcm",
    "DEFAULT_SR": "pcm",
    "speech_segments": "vad",
    "frame_energy_db": "vad",
    "auto_threshold_db": "vad",
    "align_offset": "align",
    "align_detail": "align",
    "attribute": "align",
    "AlignError": "align",
    "DEFAULT_ECHO_RATIO_DB": "align",
}


def __getattr__(name: str):
    mod = _EXPORTS.get(name)
    if mod is None:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    import importlib

    return getattr(importlib.import_module(f".{mod}", __name__), name)


__all__ = list(_EXPORTS)
