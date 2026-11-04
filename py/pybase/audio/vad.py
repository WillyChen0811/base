"""能量 VAD：按帧算 dBFS 能量，过阈值的帧算有声，再并短空隙、丢短段。

不引入 webrtcvad 之类依赖，只用 numpy。阈值缺省按信号自适应（见
`auto_threshold_db`），所以同一套参数对近讲麦克风轨和会议混音轨都能用。
时间单位整数毫秒；帧从 0 开始无重叠排开，第 k 帧覆盖 [k*frame_ms, (k+1)*frame_ms)。
"""
from __future__ import annotations

import numpy as np

# 自适应阈值的常数（推导见 README「阈值怎么定的」）
FLOOR_PERCENTILE = 10   # 噪声底：帧能量的第 10 百分位
PEAK_PERCENTILE = 99    # 语音参考电平：第 99 百分位
MARGIN_DB = 10.0        # 阈值至少高出噪声底这么多
RANGE_DB = 40.0         # 也至少到"参考电平 - 40 dB"：噪声底被降噪压得极低时，不把底噪起伏当话
MIN_DYNAMIC_DB = 12.0   # 参考电平比噪声底高不到这么多 = 整条没人说话（或全是话），不切段
SILENCE_DB = -100.0     # 能量下限（数字静音记成这个，不是 -inf）

_BLOCK_SAMPLES = 1 << 22  # 分块算帧能量，几小时的轨也不额外占一整份平方数组


def grid_power(pcm: np.ndarray, sr: int, t0_ms: float, frame_ms: float, n_frames: int) -> np.ndarray:
    """在 pcm 自己的时间轴上，从 `t0_ms` 起按 `frame_ms` 排 `n_frames` 帧，算每帧均方功率。

    `t0_ms` 可以为负或越过结尾：落在 pcm 外（哪怕只缺一部分）的帧记 NaN。
    帧边界按 round(时间 * sr) 取采样点，采样率不整除帧长时不累计误差。
    """
    x = np.asarray(pcm)
    if x.ndim != 1:
        raise ValueError("pcm 必须是一维单声道数组")
    out = np.full(n_frames, np.nan)
    if n_frames <= 0 or len(x) == 0:
        return out
    k = np.arange(n_frames + 1, dtype=np.float64)
    edges = np.round((t0_ms + k * frame_ms) * sr / 1000.0).astype(np.int64)
    valid = (edges[:-1] >= 0) & (edges[1:] <= len(x)) & (edges[1:] > edges[:-1])
    idx = np.flatnonzero(valid)
    if idx.size == 0:
        return out
    lo, hi = int(idx[0]), int(idx[-1]) + 1  # 有效帧是连续的一段
    step = max(1, int(_BLOCK_SAMPLES // max(1.0, frame_ms * sr / 1000.0)))
    for i in range(lo, hi, step):
        j = min(hi, i + step)
        seg = x[edges[i]:edges[j]].astype(np.float64)
        seg *= seg
        sums = np.add.reduceat(seg, edges[i:j] - edges[i])
        out[i:j] = sums / np.diff(edges[i:j + 1])
    return out


def to_db(power: np.ndarray) -> np.ndarray:
    """均方功率转 dBFS（满幅正弦约 -3 dB），下限 `SILENCE_DB`；NaN 原样保留。"""
    with np.errstate(divide="ignore", invalid="ignore"):
        db = 10.0 * np.log10(power)
    return np.where(np.isnan(power), np.nan, np.maximum(db, SILENCE_DB))


def frame_energy_db(pcm: np.ndarray, sr: int, frame_ms: float = 30) -> np.ndarray:
    """每帧能量（dBFS），只算完整的帧，末尾不足一帧的丢掉。"""
    n_frames = int(len(pcm) * 1000 // (sr * frame_ms)) if len(pcm) else 0
    return to_db(grid_power(pcm, sr, 0.0, frame_ms, n_frames))


def auto_threshold_db(energy_db: np.ndarray) -> float | None:
    """按信号自适应的有声阈值（dBFS）；动态范围太小（整条安静或整条都是话）返回 None。

    阈值 = max(噪声底 + MARGIN_DB, 参考电平 - RANGE_DB)，但不超过两者中点。
    """
    e = np.asarray(energy_db, dtype=np.float64)
    e = e[~np.isnan(e)]
    if e.size == 0:
        return None
    floor = float(np.percentile(e, FLOOR_PERCENTILE))
    peak = float(np.percentile(e, PEAK_PERCENTILE))
    if peak - floor < MIN_DYNAMIC_DB:
        return None
    thr = max(floor + MARGIN_DB, peak - RANGE_DB)
    return min(thr, (floor + peak) / 2.0)


def runs(mask: np.ndarray) -> list[tuple[int, int]]:
    """布尔数组里连续 True 的 [起, 止) 下标段。"""
    m = np.asarray(mask, dtype=bool)
    if m.size == 0:
        return []
    d = np.diff(np.concatenate(([0], m.astype(np.int8), [0])))
    starts = np.flatnonzero(d == 1)
    ends = np.flatnonzero(d == -1)
    return list(zip(starts.tolist(), ends.tolist()))


def merge_runs(segs: list[tuple[int, int]], max_gap: int, min_len: int) -> list[tuple[int, int]]:
    """先把间隔 < max_gap 的段并起来，再丢掉长度 < min_len 的段（单位随调用方）。"""
    merged: list[list[int]] = []
    for s, e in segs:
        if merged and s - merged[-1][1] < max_gap:
            merged[-1][1] = e
        else:
            merged.append([s, e])
    return [(s, e) for s, e in merged if e - s >= min_len]


def speech_segments(
    pcm: np.ndarray,
    sr: int,
    *,
    frame_ms: int = 30,
    min_speech_ms: int = 300,
    min_gap_ms: int = 200,
    threshold: float | None = None,
) -> list[tuple[int, int]]:
    """返回有声段 `[(start_ms, end_ms), ...]`（按时间排序、互不重叠）。

    - 帧能量 ≥ 阈值的帧算有声；`threshold` 是 dBFS（如 -45.0），None 用
      `auto_threshold_db` 按这条信号自己的噪声底定，整条没有起伏时返回 []。
    - 相邻有声段间隔 < `min_gap_ms` 的并成一段（字词之间的停顿），
      之后短于 `min_speech_ms` 的段丢掉（咔哒声、键盘声）。
    - 边界落在帧边界上，误差在一帧（`frame_ms`）量级；end 不超过信号时长。
    """
    if frame_ms <= 0:
        raise ValueError("frame_ms 必须为正")
    energy = frame_energy_db(pcm, sr, frame_ms)
    thr = auto_threshold_db(energy) if threshold is None else float(threshold)
    if thr is None:
        return []
    frames = runs(energy >= thr)
    gap = int(np.ceil(min_gap_ms / frame_ms))
    min_len = int(np.ceil(min_speech_ms / frame_ms))
    total_ms = int(len(pcm) * 1000 // sr)
    return [
        (s * frame_ms, min(e * frame_ms, total_ms))
        for s, e in merge_runs(frames, gap, min_len)
    ]
