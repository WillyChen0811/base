"""麦克风轨与会议混音轨对齐（`align_offset`），再按对齐后的能量比给混音里的
每段话标"谁在说"（`attribute`）。

场景：本机麦克风单录一条（浏览器开着回声消除，远端声音基本不进），会议软件
另录一条混音（本机 + 远端混成单轨）。两条轨同一句本机说的话都有，远端的话
只有混音里有（外放时麦克风里会漏一点衰减过的回声）。

偏移的约定：`offset_ms = 混音时间 - 麦克风时间`（同一个声音）。麦克风比混音
晚开录 1.234 秒，offset_ms 就是 +1234。

只用 numpy，不做全采样率全长互相关：先按 5 ms 一帧算能量包络，再降到 100 ms
分块粗搜、众数定位，最后每块在 ±300 ms 内按 5 ms 精搜取中位数。
推导、阈值来历与局限见同目录 README。
"""
from __future__ import annotations

import numpy as np

from .vad import auto_threshold_db, grid_power, merge_runs, runs, to_db

FINE_MS = 5            # 精搜包络的帧长
COARSE_FACTOR = 20     # 粗搜包络 = 20 个精帧（100 ms）
CHUNK_MS = 30000       # 分块长度；块间重叠一半
MIN_ACTIVE = 0.05      # 一块里有声的粗帧不到 5% 就不拿来估计
MIN_R = 0.2            # 块的最佳相关系数低于这个不算数
AGREE_MS = 200         # 与众数相差这么多以内的块算"同意"
REFINE_MS = 300        # 精搜范围：众数 ±300 ms

DEFAULT_ECHO_RATIO_DB = 10.0  # 比本机说话时的能量比低这么多以上 = 回声/远端（合成实验定，见 README）
OVERLAP_DB = 4.0       # 归属：本机在说时，某一帧混音比"本机电平换算"响 4 dB 以上 = 这一帧还有远端
LOUD_DB = 6.0          # 归属：上一条只认不比附近 210 ms 平均电平低 6 dB 以上的帧（音节起头的弱帧对齐差几毫秒比值就乱）
CAL_WINDOW_MS = 5000   # 归属：本机电平按前后各 5 秒就地标定（会议软件的自动增益会慢慢变）
CAL_STEP_MS = 1000     # 就地标定每 1 秒算一个点，中间插值
CAL_MIN_MS = 600       # 窗口里本机有声帧不到这么多就不就地标定，用整段的
SMOOTH_MS = 210        # 归属：判"本机在说"用的能量比按这么长的窗口平滑（回声晚几十毫秒，逐帧比会乱跳）
MIC_PAD_MS = 120       # 归属：麦克风有声段两头各放宽这么多再用（两条轨的 VAD 边界不会刚好对上）
GAP_MS = 400           # 归属：同一方的帧之间空隙 < 400 ms 连成一片
MIN_REGION_MS = 150    # 归属：短于 150 ms 的一片丢掉（对齐误差、起止帧抖动）
MIN_PIECE_MS = 300     # 归属：结果里短于 300 ms 的片段并进相邻片段


class AlignError(ValueError):
    """两条轨对不齐：可用的有声块太少或相关性太弱。"""


# ---------------------------------------------------------------- 对齐

def _feature(pcm: np.ndarray, sr: int, *, linear: bool) -> np.ndarray | None:
    """5 ms 一帧的包络，低于 VAD 阈值的帧置 0；整条没有起伏返回 None。

    两条轨用不同的包络（合成实验比过 5 种组合，见 README）：
    - 麦克风（`linear=True`）用幅度（均方根）：外放回声比本机说话低 20 dB，幅度只有
      十分之一，几乎不进相关；用 dB 包络时回声（比混音晚几十毫秒）会把峰往回声的
      延迟方向拽（-20 dB 回声、晚 50 ms：dB 包络偏 -11 ms，幅度包络 ≤1 ms）。
    - 混音（`linear=False`）用"高出阈值多少 dB"：混音里远端比本机响时，幅度包络会让
      麦克风的本机话去对混音里更响的远端话（远端响 6 dB 时对错几秒）；dB 把两方拉平。
    """
    n = int(len(pcm) * 1000 // (sr * FINE_MS)) if len(pcm) else 0
    power = grid_power(pcm, sr, 0.0, FINE_MS, n)
    db = to_db(power)
    thr = auto_threshold_db(db)
    if thr is None:
        return None
    if linear:
        return np.where(db >= thr, np.sqrt(power), 0.0)
    return np.maximum(db - thr, 0.0)


def _coarse(f: np.ndarray) -> np.ndarray:
    n = len(f) // COARSE_FACTOR
    return f[: n * COARSE_FACTOR].reshape(n, COARSE_FACTOR).mean(axis=1)


def _pearson_scan(a: np.ndarray, b: np.ndarray, b_spec: np.ndarray | None = None,
                  nfft: int | None = None) -> np.ndarray:
    """r[j] = a 与 b[j:j+len(a)] 的皮尔逊相关系数，j = 0..len(b)-len(a)。FFT 算分子。"""
    m, n = len(a), len(b)
    if m == 0 or n < m:
        return np.zeros(0)
    a0 = a - a.mean()
    na = float(np.sqrt(np.dot(a0, a0)))
    if na == 0.0:
        return np.zeros(n - m + 1)
    if nfft is None:
        nfft = 1 << int(np.ceil(np.log2(max(n, 2))))
    if b_spec is None:
        b_spec = np.fft.rfft(b, nfft)
    # 循环相关在 j <= n-m 时不会绕回（a 只有前 m 个非零，i + j < n <= nfft）
    num = np.fft.irfft(b_spec * np.conj(np.fft.rfft(a0, nfft)), nfft)[: n - m + 1]
    cs = np.concatenate(([0.0], np.cumsum(b)))
    cs2 = np.concatenate(([0.0], np.cumsum(b * b)))
    s1 = cs[m:] - cs[:-m]
    s2 = cs2[m:] - cs2[:-m]
    var = np.maximum(s2 - s1 * s1 / m, 0.0)
    den = na * np.sqrt(var)
    with np.errstate(divide="ignore", invalid="ignore"):
        r = np.where(den > 1e-9 * max(1.0, na), num / den, 0.0)
    return r


def _parabolic(r: np.ndarray, j: int) -> float:
    if 0 < j < len(r) - 1:
        y0, y1, y2 = r[j - 1], r[j], r[j + 1]
        d = y0 - 2 * y1 + y2
        if d < 0:
            return j + 0.5 * (y0 - y2) / d
    return float(j)


def align_detail(
    mic_pcm: np.ndarray,
    mix_pcm: np.ndarray,
    sr: int,
    *,
    max_shift_ms: int | None = None,
    chunk_ms: int = CHUNK_MS,
) -> dict:
    """`align_offset` 的完整结果，用来看对齐靠不靠得住（比如有没有漂移、录制中断）。

    返回 `{"offset_ms": int, "chunks": [...], "used": 同意众数的块数,
    "total": 拿来估计的块数, "spread_ms": 同意块精搜结果的极差}`；
    `chunks` 每项 `{"mic_ms": 块在麦克风轨的起点, "coarse_ms": 粗搜偏移,
    "offset_ms": 精搜偏移（不同意众数的块为 None）, "r": 粗搜相关系数}`。
    对不齐抛 `AlignError`。
    """
    fm = _feature(mic_pcm, sr, linear=True)
    fx = _feature(mix_pcm, sr, linear=False)
    if fm is None or fx is None:
        raise AlignError("对不齐：有一条轨整条没有起伏（没人说话或没有静音）")
    cm, cx = _coarse(fm), _coarse(fx)
    coarse_ms = FINE_MS * COARSE_FACTOR
    size = min(max(1, int(chunk_ms // coarse_ms)), len(cm), len(cx))
    if size < 20:  # 不到 2 秒，包络形状说明不了什么
        raise AlignError("对不齐：轨太短")
    hop = max(1, size // 2)
    starts = list(range(0, len(cm) - size + 1, hop))
    if starts[-1] != len(cm) - size:
        starts.append(len(cm) - size)  # 末尾再放一块：麦克风比混音先开录时，开头几块落在混音之外

    nfft = 1 << int(np.ceil(np.log2(max(len(cx), 2))))
    x_spec = np.fft.rfft(cx, nfft)
    lag_limit = None if max_shift_ms is None else max_shift_ms / coarse_ms + 1

    chunks: list[dict] = []
    for c in starts:
        a = cm[c:c + size]
        if np.mean(a > 0) < MIN_ACTIVE:
            continue
        r = _pearson_scan(a, cx, x_spec, nfft)
        lags = np.arange(len(r)) - c
        if lag_limit is not None:
            r = np.where(np.abs(lags) <= lag_limit, r, -np.inf)
        j = int(np.argmax(r))
        if not np.isfinite(r[j]) or r[j] < MIN_R:
            continue
        chunks.append({"mic_ms": int(c * coarse_ms), "coarse_ms": int(lags[j] * coarse_ms),
                       "offset_ms": None, "r": round(float(r[j]), 3), "_c": c})
    if not chunks:
        raise AlignError("对不齐：麦克风轨里没有能和混音对上的有声块")

    # 众数：每个候选偏移数 ±1 个粗帧内有多少块（按相关系数加权）
    cand = np.array([ch["coarse_ms"] for ch in chunks], dtype=np.float64)
    w = np.array([ch["r"] for ch in chunks])
    score = [(w[np.abs(cand - v) <= coarse_ms].sum(), v) for v in cand]
    mode = max(score)[1]

    fine_offsets: list[float] = []
    m_f = size * COARSE_FACTOR
    reach = int(REFINE_MS // FINE_MS)
    for ch in chunks:
        if abs(ch["coarse_ms"] - mode) > AGREE_MS:
            continue
        c_f = ch["_c"] * COARSE_FACTOR
        a = fm[c_f:c_f + m_f]
        center = c_f + int(round(mode / FINE_MS))
        p_lo = max(0, center - reach)
        p_hi = min(len(fx) - m_f, center + reach)
        if p_hi < p_lo:
            continue
        r = _pearson_scan(a, fx[p_lo:p_hi + m_f])
        if r.size == 0:
            continue
        j = int(np.argmax(r))
        off = (p_lo + _parabolic(r, j) - c_f) * FINE_MS
        ch["offset_ms"] = int(round(off))
        fine_offsets.append(off)
    for ch in chunks:
        del ch["_c"]
    if not fine_offsets:
        raise AlignError("对不齐：精搜没有结果")
    return {
        "offset_ms": int(round(float(np.median(fine_offsets)))),
        "chunks": chunks,
        "used": len(fine_offsets),
        "total": len(chunks),
        "spread_ms": int(round(max(fine_offsets) - min(fine_offsets))),
    }


def align_offset(
    mic_pcm: np.ndarray,
    mix_pcm: np.ndarray,
    sr: int,
    *,
    max_shift_ms: int | None = None,
    chunk_ms: int = CHUNK_MS,
) -> int:
    """麦克风轨相对混音轨的偏移 `offset_ms = 混音时间 - 麦克风时间`。

    两条轨同一采样率 `sr`、单声道（`decode_pcm` 出来的就是）。
    `max_shift_ms` 限定 |offset| 的搜索范围；None = 不限，麦克风这段落在混音
    哪里都能找到（一场会议里录了好几段麦克风时，每段单独对）。
    几小时的轨按 `chunk_ms` 分块各自估计，取众数附近各块精搜结果的中位数。
    对不齐抛 `AlignError`；想看各块结果（漂移、中断）用 `align_detail`。
    """
    return align_detail(mic_pcm, mix_pcm, sr, max_shift_ms=max_shift_ms,
                        chunk_ms=chunk_ms)["offset_ms"]


# ---------------------------------------------------------------- 归属

def _seg_mask(segs, t: np.ndarray) -> np.ndarray:
    """t（毫秒）落在哪个 [start, end) 段里。段可以无序、重叠。"""
    if not segs:
        return np.zeros(len(t), dtype=bool)
    merged: list[list[float]] = []
    for s, e in sorted((float(s), float(e)) for s, e in segs):
        if merged and s <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], e)
        else:
            merged.append([s, e])
    st = np.array([m[0] for m in merged])
    en = np.array([m[1] for m in merged])
    i = np.searchsorted(st, t, side="right") - 1
    ok = i >= 0
    out = np.zeros(len(t), dtype=bool)
    out[ok] = t[ok] < en[i[ok]]
    return out


def _self_level(raw: np.ndarray, mic_db: np.ndarray, cal: np.ndarray, F: float,
                echo_ratio_db: float) -> np.ndarray:
    """本机说话时的「麦克风/混音」能量比（dB），每帧一个值。

    先在整段上按麦克风功率加权取中位数（本机说话的帧最响，占比小也压得住回声帧）；
    再去掉比它低 echo_ratio_db 以上的帧（回声），每 1 秒按前后 5 秒就地重算一次加权
    中位数、中间线性插值——会议软件的自动增益让本机电平慢慢变时跟得上。
    窗口里本机有声帧太少的点用整段的值。麦克风整段没声时返回 0（用不上）。
    """
    n = len(raw)
    if not cal.any():
        return np.zeros(n)
    w_all = np.power(10.0, mic_db / 10.0)
    overall = _weighted_median(raw[cal], w_all[cal])
    keep = cal & (raw >= overall - echo_ratio_db)
    idx = np.flatnonzero(keep)
    half = int(round(CAL_WINDOW_MS / F))
    step = max(1, int(round(CAL_STEP_MS / F)))
    need = max(1, int(round(CAL_MIN_MS / F)))
    pts, vals = [], []
    for c in range(0, n, step):
        lo, hi = np.searchsorted(idx, c - half), np.searchsorted(idx, c + half + 1)
        if hi - lo >= need:
            sel = idx[lo:hi]
            pts.append(c)
            vals.append(_weighted_median(raw[sel], w_all[sel]))
    if not pts:
        return np.full(n, overall)
    return np.interp(np.arange(n), pts, vals)


def _dilate(mask: np.ndarray, k: int) -> np.ndarray:
    if k <= 0 or not mask.any():
        return mask
    return np.convolve(mask.astype(np.int32), np.ones(2 * k + 1, dtype=np.int32), mode="same") > 0


def _weighted_median(v: np.ndarray, w: np.ndarray) -> float:
    o = np.argsort(v)
    cw = np.cumsum(w[o])
    return float(v[o][np.searchsorted(cw, cw[-1] / 2.0)])


def _region(mask: np.ndarray, gap: int, min_len: int) -> np.ndarray:
    out = np.zeros(len(mask), dtype=bool)
    for s, e in merge_runs(runs(mask), gap, min_len):
        out[s:e] = True
    return out


def attribute(
    mic_segs,
    mix_segs,
    offset_ms: float,
    mic_pcm: np.ndarray,
    mix_pcm: np.ndarray,
    sr: int,
    *,
    echo_ratio_db: float = DEFAULT_ECHO_RATIO_DB,
    frame_ms: int = 30,
    self_ratio_db: float | None = None,
) -> list[dict]:
    """把混音的每个有声段分成若干片，标谁在说。

    - `mic_segs`：麦克风轨的有声段（麦克风时间，毫秒），通常是
      `speech_segments(mic_pcm, sr)`；`mix_segs`：混音的有声段（混音时间）。
    - `offset_ms`：`align_offset` 的结果。
    - 返回按时间排序的 `[{"start_ms", "end_ms", "who", "ratio_db"}, ...]`，时间是
      **混音时间**，片段拼起来正好覆盖 `mix_segs`。`who`：
      `"self"` 本机说、`"remote"` 远端说、`"both"` 同时说、`"unknown"` 麦克风轨
      没覆盖到这段（对齐后落在麦克风录音之外）或这段里没有过阈值的帧。
    - `ratio_db`：这片里有声帧的「麦克风/混音能量比」中位数，减去本机说话时的
      能量比（`self_ratio_db`）后的值——本机说约为 0，远端说约为 -回声衰减量；
      没法算时为 None。
    - 判法（帧长 `frame_ms`，只看混音过阈值的帧）：
      本机帧 = 在麦克风有声段附近、且约 210 ms 窗口平滑后的能量比不比本机电平
      低 `echo_ratio_db` 以上（回声比混音晚几十毫秒，逐帧比会乱跳，平滑后稳在
      "-回声衰减量"）；远端帧 = 麦克风没声，或这一帧混音比"本机电平换算"响 4 dB
      以上且不是附近的弱帧（同时说话时远端填在本机音节的空隙里，要逐帧看）。
      两种帧各自隔 < 400 ms 连成片、丢掉短于 150 ms 的，两片重叠处是 both，
      最后短于 300 ms 的片并进相邻片。
    - `self_ratio_db`：本机说话时的「麦克风/混音」能量比（dB）。None 时自动标定：
      按麦克风功率加权取中位数（本机说话的帧最响，占比小也压得住回声帧），再每秒
      按前后 5 秒就地重算，跟住会议软件自动增益的慢变化。整场测试员几乎不说话、
      只有外放回声时自动标定会把回声当本机，见 README 局限；给了数就整段用这个数。
    """
    if frame_ms <= 0:
        raise ValueError("frame_ms 必须为正")
    F = float(frame_ms)
    n = int(len(mix_pcm) * 1000 // (sr * F)) if len(mix_pcm) else 0
    if n == 0 or not mix_segs:
        return []
    mix_db = to_db(grid_power(mix_pcm, sr, 0.0, F, n))
    mic_db = to_db(grid_power(mic_pcm, sr, -float(offset_ms), F, n))
    centers = (np.arange(n) + 0.5) * F
    in_mix = _seg_mask(mix_segs, centers)
    in_mic = _seg_mask(mic_segs, centers - float(offset_ms))
    covered = ~np.isnan(mic_db)
    thr = auto_threshold_db(mix_db)
    active = in_mix & ((mix_db >= thr) if thr is not None else True)

    raw = np.where(covered, mic_db - np.nan_to_num(mix_db), np.nan)
    if self_ratio_db is None:
        level = _self_level(raw, mic_db, active & covered & in_mic, F, float(echo_ratio_db))
    else:
        level = np.full(n, float(self_ratio_db))
    rel = raw - level

    usable = active & covered
    pad = int(np.ceil(MIC_PAD_MS / F))
    near_mic = _dilate(in_mic, pad)
    # 本机在说：平滑后的能量比接近本机电平（回声、远端都低 echo_ratio_db 以上）
    w = np.ones(max(1, int(round(SMOOTH_MS / F))))
    p_mic = np.where(usable, np.power(10.0, np.nan_to_num(mic_db, nan=-100.0) / 10.0), 0.0)
    p_mix = np.where(usable, np.power(10.0, mix_db / 10.0), 0.0)
    s_mic = np.convolve(p_mic, w, mode="same")
    s_mix = np.convolve(p_mix, w, mode="same")
    with np.errstate(divide="ignore", invalid="ignore"):
        rel_s = 10.0 * np.log10(s_mic / s_mix) - level
    is_self = usable & near_mic & (rel_s >= -float(echo_ratio_db))
    # 远端在说：逐帧看，混音比麦克风按本机电平该有的响 echo_ratio_db 以上（同时说话时
    # 远端的声音填在本机音节之间的空隙里，逐帧才看得出来）
    n_use = np.convolve(usable.astype(np.float64), w, mode="same")
    with np.errstate(divide="ignore", invalid="ignore"):
        local_db = 10.0 * np.log10(s_mix / n_use)
    loud = mix_db >= local_db - LOUD_DB
    is_remote = usable & (~near_mic | ((rel < -min(float(echo_ratio_db), OVERLAP_DB)) & loud))
    gap = int(np.ceil(GAP_MS / F))
    min_len = int(np.ceil(MIN_REGION_MS / F))
    self_reg = _region(is_self, gap, min_len)
    remote_reg = _region(is_remote, gap, min_len)
    min_piece = int(np.ceil(MIN_PIECE_MS / F))

    out: list[dict] = []
    for seg_s, seg_e in sorted((int(s), int(e)) for s, e in mix_segs):
        if seg_e <= seg_s:
            continue
        k0 = max(0, int(seg_s // F))
        k1 = min(n, int(np.ceil(seg_e / F)))
        if k1 <= k0:
            out.append({"start_ms": seg_s, "end_ms": seg_e, "who": "unknown", "ratio_db": None})
            continue
        labels: list[str | None] = []
        for k in range(k0, k1):
            if not covered[k]:
                labels.append("unknown")
            elif self_reg[k] and remote_reg[k]:
                labels.append("both")
            elif self_reg[k]:
                labels.append("self")
            elif remote_reg[k]:
                labels.append("remote")
            else:
                labels.append(None)
        _fill(labels)
        pieces = _pieces(labels, min_piece)
        for i, (a, b, who) in enumerate(pieces):
            ks, ke = k0 + a, k0 + b
            start = seg_s if i == 0 else int(round(ks * F))
            end = seg_e if i == len(pieces) - 1 else int(round(ke * F))
            sel = usable[ks:ke]
            ratio = None
            if who != "unknown" and sel.any():
                ratio = round(float(np.median(rel[ks:ke][sel])), 1)
            out.append({"start_ms": start, "end_ms": end, "who": who, "ratio_db": ratio})
    return out


def _fill(labels: list) -> None:
    """没判出来的帧（段内低于阈值的停顿）沿用前一帧，开头的沿用后一帧；全没有就 unknown。"""
    last = None
    for i, v in enumerate(labels):
        if v is None:
            labels[i] = last
        else:
            last = v
    nxt = None
    for i in range(len(labels) - 1, -1, -1):
        if labels[i] is None:
            labels[i] = nxt
        else:
            nxt = labels[i]
    for i, v in enumerate(labels):
        if v is None:
            labels[i] = "unknown"


def _pieces(labels: list[str], min_len: int) -> list[tuple[int, int, str]]:
    """标签序列 → [起, 止, 标签]；短于 min_len 的片并进较长的邻居，再合并同标签。"""
    ps: list[list] = []
    for i, v in enumerate(labels):
        if ps and ps[-1][2] == v:
            ps[-1][1] = i + 1
        else:
            ps.append([i, i + 1, v])
    while len(ps) > 1:
        i = min(range(len(ps)), key=lambda k: ps[k][1] - ps[k][0])
        if ps[i][1] - ps[i][0] >= min_len:
            break
        if i == 0:
            j = 1
        elif i == len(ps) - 1:
            j = i - 1
        else:
            j = i - 1 if ps[i - 1][1] - ps[i - 1][0] >= ps[i + 1][1] - ps[i + 1][0] else i + 1
        lo, hi = min(ps[i][0], ps[j][0]), max(ps[i][1], ps[j][1])
        ps[j][0], ps[j][1] = lo, hi
        del ps[i]
        merged: list[list] = []
        for p in ps:
            if merged and merged[-1][2] == p[2]:
                merged[-1][1] = p[1]
            else:
                merged.append(p)
        ps = merged
    return [(a, b, v) for a, b, v in ps]
