"""合成的"会议录音"：给 pybase.audio 的测试和阈值实验用，不碰任何真实录音。

一场合成会议在混音时间轴上排好本机（self）和远端（remote）的话：
- 每句话 = 300–3400 Hz 带限噪声 × 音节包络（hann 形音节 120–280 ms，音节间
  0–60 ms 小空隙，偶有 80–120 ms 停顿），像真话一样一句之内能量起伏 20 dB 以上；
- 混音 = 本机 × G_SELF_MIX + 远端 × G_REMOTE + 底噪；
- 麦克风 = 本机 × G_SELF_MIC + 远端回声（相对本机电平衰减 echo_db、晚 ECHO_DELAY_MS）
  + 底噪，从混音时间 mic_start_ms 开录、mic_end_ms 停（offset = mic_start_ms）。

`python tests/audio_synth.py` 跑阈值实验（回声水平 × echo_ratio_db 的归属正确率表），
README「阈值怎么定的」里的数就是它打出来的。
"""
from __future__ import annotations

import sys
from dataclasses import dataclass

import numpy as np

SR = 16000
G_SELF_MIC = 0.1      # 本机在麦克风里：峰值帧约 -20 dBFS
G_SELF_MIX = 0.05     # 本机在混音里比麦克风低 6 dB（会议软件自己的增益）
G_REMOTE = 0.06       # 远端在混音里
NOISE_MIC_DB = -65.0
NOISE_MIX_DB = -60.0
ECHO_DELAY_MS = 50

# 混音时间轴（ms）
SELF = [(2000, 6000), (12000, 14500), (16000, 20000), (27000, 30000), (33300, 34300),
        (38800, 39400)]
REMOTE = [(200, 1000), (7000, 11000), (18000, 22000), (23500, 26000), (31000, 33000),
          (36500, 38500), (39700, 41500)]
TOTAL_MS = 43000
MIC_START_MS = 1234
MIC_END_MS = 42500

# 归属真值（混音时间）：麦克风没覆盖的 200–1000 是 unknown；16000–22000 里中间 2 秒同时说；
# 末尾是"远端—本机短回答—远端"的快速轮换
TRUTH = [
    (200, 1000, "unknown"),
    (2000, 6000, "self"),
    (7000, 11000, "remote"),
    (12000, 14500, "self"),
    (16000, 18000, "self"),
    (18000, 20000, "both"),
    (20000, 22000, "remote"),
    (23500, 26000, "remote"),
    (27000, 30000, "self"),
    (31000, 33000, "remote"),
    (33300, 34300, "self"),
    (36500, 38500, "remote"),
    (38800, 39400, "self"),     # 两段远端之间插一句 0.6 秒的短回答，前后各隔 300 ms
    (39700, 41500, "remote"),
]


def band_noise(rng: np.random.Generator, n: int, lo: float = 300, hi: float = 3400,
               sr: int = SR) -> np.ndarray:
    x = rng.standard_normal(n)
    spec = np.fft.rfft(x)
    f = np.fft.rfftfreq(n, 1 / sr)
    spec[(f < lo) | (f > hi)] = 0
    y = np.fft.irfft(spec, n)
    return y / (np.std(y) + 1e-12)


def syllable_envelope(rng: np.random.Generator, n: int, sr: int = SR) -> np.ndarray:
    """长 n 个采样的音节包络，从 0 起、在 n 处回到 0。"""
    env = np.zeros(n)
    i = 0
    while i < n:
        left = n - i
        size = int(rng.uniform(0.12, 0.28) * sr)
        if left - size < int(0.12 * sr):
            size = left  # 最后一个音节吃掉剩下的，包络正好在句末归零
        env[i:i + size] = rng.uniform(0.5, 1.0) * np.hanning(size)
        i += size
        if i < n:
            gap = rng.uniform(0.08, 0.12) if rng.random() < 0.15 else rng.uniform(0, 0.06)
            i += min(int(gap * sr), max(0, n - i - int(0.12 * sr)))
    return env


def speech_track(rng: np.random.Generator, segs, total_ms: int, sr: int = SR) -> np.ndarray:
    out = np.zeros(int(total_ms * sr / 1000))
    for s, e in segs:
        a, b = int(s * sr / 1000), int(e * sr / 1000)
        out[a:b] = band_noise(rng, b - a, sr=sr) * syllable_envelope(rng, b - a, sr)
    return out


@dataclass
class Meeting:
    mic: np.ndarray
    mix: np.ndarray
    offset_ms: int
    sr: int


def make_meeting(echo_db: float, *, seed: int = 7, sr: int = SR, self_segs=SELF,
                 remote_segs=REMOTE, total_ms: int = TOTAL_MS,
                 mic_start_ms: int = MIC_START_MS, mic_end_ms: int = MIC_END_MS,
                 echo_delay_ms: int = ECHO_DELAY_MS, remote_db: float = 0.0,
                 drift_db: float = 0.0, mix_start_ms: int = 0) -> Meeting:
    """`echo_db`：远端回声在麦克风里比"按本机电平换算"低多少（-20 外放、-40 耳机）。
    `remote_db`：远端在混音里比缺省响多少（同时说话时谁压过谁）。
    `drift_db`：本机在混音里的增益以 60 秒为周期慢慢摆 ±drift_db（会议软件的自动增益）。
    `mix_start_ms`：混音从这个时刻才开录（大于 mic_start_ms 时偏移为负）。
    真值段（SELF/REMOTE/TRUTH）都在"整场"时间轴上，混音时间 = 整场时间 - mix_start_ms。"""
    rng = np.random.default_rng(seed)
    me = speech_track(rng, self_segs, total_ms, sr)
    them = speech_track(rng, remote_segs, total_ms, sr)
    n = len(me)
    g_remote = G_REMOTE * 10 ** (remote_db / 20)
    t = np.arange(n) / sr
    g_self_mix = G_SELF_MIX * 10 ** (drift_db * np.sin(2 * np.pi * t / 60.0) / 20)
    mix = g_self_mix * me + g_remote * them + 10 ** (NOISE_MIX_DB / 20) * rng.standard_normal(n)
    d = int(echo_delay_ms * sr / 1000)
    echo = np.concatenate((np.zeros(d), them[: n - d]))
    echo_gain = g_remote * (G_SELF_MIC / G_SELF_MIX) * 10 ** (echo_db / 20)
    a, b = int(mic_start_ms * sr / 1000), int(mic_end_ms * sr / 1000)
    mic = G_SELF_MIC * me[a:b] + echo_gain * echo[a:b]
    mic = mic + 10 ** (NOISE_MIC_DB / 20) * rng.standard_normal(len(mic))
    mix = mix[int(mix_start_ms * sr / 1000):]
    return Meeting(mic.astype(np.float32), mix.astype(np.float32), mic_start_ms - mix_start_ms, sr)


def make_long_meeting(hours: float, *, offset_ms: int = 1234, echo_db: float = -20.0,
                      seed: int = 11, sr: int = SR) -> Meeting:
    """几小时的合成会议，float32 原地累加（两条轨各约 230 MB/小时），给对齐耗时测试用。

    一轮一轮地说：本机 60%、远端 40%，每轮 1–8 秒，轮间隔 0.3–2 秒。
    麦克风从混音时间 offset_ms 开录，录到混音结束前 offset_ms。
    """
    rng = np.random.default_rng(seed)
    n_mix = int(hours * 3600 * sr)
    a, b = int(offset_ms * sr / 1000), n_mix - int(offset_ms * sr / 1000)
    mix = np.zeros(n_mix, dtype=np.float32)
    mic = np.zeros(b - a, dtype=np.float32)
    echo_gain = G_REMOTE * (G_SELF_MIC / G_SELF_MIX) * 10 ** (echo_db / 20)
    d = int(ECHO_DELAY_MS * sr / 1000)
    t = int(0.5 * sr)
    while True:
        size = int(rng.uniform(1.0, 8.0) * sr)
        if t + size + d >= n_mix:
            break
        x = band_noise(rng, size, sr=sr) * syllable_envelope(rng, size, sr)
        if rng.random() < 0.6:
            mix[t:t + size] += (G_SELF_MIX * x).astype(np.float32)
            lo, hi = max(t, a), min(t + size, b)
            if hi > lo:
                mic[lo - a:hi - a] += (G_SELF_MIC * x[lo - t:hi - t]).astype(np.float32)
        else:
            mix[t:t + size] += (G_REMOTE * x).astype(np.float32)
            lo, hi = max(t + d, a), min(t + d + size, b)
            if hi > lo:
                mic[lo - a:hi - a] += (echo_gain * x[lo - t - d:hi - t - d]).astype(np.float32)
        t += size + int(rng.uniform(0.3, 2.0) * sr)
    for arr, level in ((mix, NOISE_MIX_DB), (mic, NOISE_MIC_DB)):
        step = 1 << 22
        for i in range(0, len(arr), step):
            j = min(len(arr), i + step)
            arr[i:j] += rng.standard_normal(j - i, dtype=np.float32) * np.float32(10 ** (level / 20))
    return Meeting(mic, mix, offset_ms, sr)


def report(line: str) -> None:
    """测试里要记下的数（耗时、误差）：打印，另外 PYBASE_TEST_REPORT_DIR 设了就追加进
    那里的 audio-report.txt（Studio 上指到 run 的 out/，client.py --out-dir 取回）。"""
    import os

    print(line)
    folder = os.environ.get("PYBASE_TEST_REPORT_DIR")
    if folder:
        os.makedirs(folder, exist_ok=True)
        with open(os.path.join(folder, "audio-report.txt"), "a", encoding="utf-8") as fh:
            fh.write(line + "\n")


def coverage(pieces: list[dict], s: int, e: int, who: str, shrink: int = 200) -> float:
    """真值段 [s+shrink, e-shrink] 里被标成 who 的时长占比。"""
    lo, hi = s + shrink, e - shrink
    hit = sum(max(0, min(hi, p["end_ms"]) - max(lo, p["start_ms"]))
              for p in pieces if p["who"] == who)
    return hit / (hi - lo)


def score(pieces: list[dict], truth=TRUTH) -> tuple[float, list[float]]:
    """每个真值段的正确覆盖率，和其中最小的那个。"""
    covs = [coverage(pieces, s, e, who) for s, e, who in truth]
    return min(covs), covs


def _sweep() -> None:  # pragma: no cover - 手动跑的实验
    """每格：4 个随机种子 × 14 个真值段 = 56 段里，标对的段数（去掉两头各 200 ms 后
    覆盖率 ≥ 0.8，即 test_audio 的判据），括号里是最差一段的覆盖率。"""
    from pybase.audio import align_offset, attribute, speech_segments

    echoes = [-10, -14, -17, -20, -25, -30, -40]
    ratios = [6, 8, 10, 12, 14]
    seeds = [7, 1, 2, 3]
    for remote_db, drift_db in [(0, 0), (6, 0), (0, 3), (6, 3)]:
        print(f"\n远端比缺省响 {remote_db} dB（同时说话时远端约比本机响 {1.6 + remote_db:.1f} dB），"
              f"本机增益漂移 ±{drift_db} dB")
        print("| 回声(dB) | 对齐最大误差(ms) | " + " | ".join(f"阈值 {r} dB" for r in ratios) + " |")
        print("|---" * (len(ratios) + 2) + "|")
        for echo in echoes:
            cells = [[] for _ in ratios]
            errs = []
            for seed in seeds:
                m = make_meeting(echo, seed=seed, remote_db=remote_db, drift_db=drift_db)
                off = align_offset(m.mic, m.mix, m.sr)
                errs.append(off - m.offset_ms)
                mic_segs = speech_segments(m.mic, m.sr)
                mix_segs = speech_segments(m.mix, m.sr)
                for i, r in enumerate(ratios):
                    pieces = attribute(mic_segs, mix_segs, off, m.mic, m.mix, m.sr, echo_ratio_db=r)
                    cells[i].extend(score(pieces)[1])
            text = [f"{sum(c >= 0.8 for c in cov)}/{len(cov)} ({min(cov):.2f})" for cov in cells]
            print(f"| {echo} | {max(errs, key=abs):+d} | " + " | ".join(text) + " |")


if __name__ == "__main__":  # pragma: no cover
    sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parents[1]))
    _sweep()
