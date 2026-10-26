package projsync

// 本文件管测速和预计时间：每个方向各一份指数平均的吞吐。
//
// 样本由传输层按块给（块 ≥ MinSampleBytes 才算，缺省 1MB）；没有样本时用调用方给的缺省值
// （网好 5MB/s、热点 0.3MB/s，由调用方在网络状态变化时通过 Reset 换）。
// 边界：只算「每个方向大概多快」，不负责判断网络好坏，也不决定放不放行（暂缓是调用方的事）。

import (
	"sync"
	"time"
)

// PFDirection 是传输方向。
type PFDirection int

const (
	PFUpload   PFDirection = iota // 上传：Mac 本地文件 → 远端暂存区
	PFDownload                    // 下载：远端文件 → 本机暂存区
)

func (d PFDirection) String() string {
	switch d {
	case PFUpload:
		return "上传"
	case PFDownload:
		return "下载"
	}
	return "未知方向"
}

func (d PFDirection) valid() bool { return d == PFUpload || d == PFDownload }

// 缺省速度（字节/秒）：调用方没给时用。
const (
	PFSpeedDefaultGood    = 5 << 20 // 网好 5MB/s
	PFSpeedDefaultHotspot = 300_000 // 热点 0.3MB/s
)

const pfSpeedAlpha = 0.3 // 指数平均的新样本权重

// PFSpeed 记每个方向的吞吐估计，并发安全。
type PFSpeed struct {
	mu   sync.Mutex
	def  float64
	rate [2]float64
	have [2]bool
}

// NewPFSpeed 建一个测速器；defaultBps 是没有测量值时用的缺省（字节/秒），<=0 用网好缺省。
func NewPFSpeed(defaultBps float64) *PFSpeed {
	if defaultBps <= 0 {
		defaultBps = PFSpeedDefaultGood
	}
	return &PFSpeed{def: defaultBps}
}

// Observe 记一个样本：这个方向在 d 时间内传了 bytes 字节。第一个样本直接当估计，之后指数平均。
func (s *PFSpeed) Observe(dir PFDirection, bytes int64, d time.Duration) {
	if !dir.valid() || bytes <= 0 || d <= 0 {
		return
	}
	v := float64(bytes) / d.Seconds()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have[dir] {
		s.rate[dir], s.have[dir] = v, true
		return
	}
	s.rate[dir] = pfSpeedAlpha*v + (1-pfSpeedAlpha)*s.rate[dir]
}

// Rate 返回这个方向当前的估计（字节/秒）；measured 为 false 表示还没有测量值，返回的是缺省。
func (s *PFSpeed) Rate(dir PFDirection) (bps float64, measured bool) {
	if !dir.valid() {
		return s.def, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.have[dir] {
		return s.rate[dir], true
	}
	return s.def, false
}

// Reset 是「网络状态变了」的接口：丢掉两个方向所有测量值，缺省换成 defaultBps（<=0 则沿用原缺省）。
func (s *PFSpeed) Reset(defaultBps float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if defaultBps > 0 {
		s.def = defaultBps
	}
	s.have = [2]bool{}
	s.rate = [2]float64{}
}

// EstimateSeconds 预计传完 remaining 字节还要多少秒（剩余 ÷ 该方向估计速度；剩余 <=0 返回 0）。
// 传了一半的文件，调用方传剩余字节即可。
func (s *PFSpeed) EstimateSeconds(remaining int64, dir PFDirection) float64 {
	if remaining <= 0 {
		return 0
	}
	bps, _ := s.Rate(dir)
	if bps <= 0 {
		bps = PFSpeedDefaultHotspot
	}
	return float64(remaining) / bps
}
