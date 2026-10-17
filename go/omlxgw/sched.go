package omlxgw

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"
	"time"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

type entry struct {
	id       int64
	model    string
	prio     int
	client   string
	enqueued time.Time
	started  time.Time
	granted  chan struct{}
	excl     bool
}

// scheduler 按请求粒度排队，满足并发规则时按优先级放行。
type scheduler struct {
	mu      sync.Mutex
	cfg     *Config
	seq     int64
	running map[int64]*entry
	queue   []*entry // 按到达顺序
	now     func() time.Time
}

func newScheduler(cfg *Config) *scheduler {
	return &scheduler{cfg: cfg, running: map[int64]*entry{}, now: time.Now}
}

func (s *scheduler) setConfig(cfg *Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.dispatchLocked()
	s.mu.Unlock()
}

func (s *scheduler) config() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// acquire 排队直到放行。返回 release；ctx 取消时从队列移除并返回错误。
func (s *scheduler) acquire(ctx context.Context, model string, prio int, client string) (e *entry, release func(), err error) {
	s.mu.Lock()
	s.seq++
	e = &entry{id: s.seq, model: model, prio: prio, client: client, enqueued: s.now(), granted: make(chan struct{})}
	e.excl = s.cfg.rule(model).Exclusive
	s.queue = append(s.queue, e)
	s.dispatchLocked()
	s.mu.Unlock()

	select {
	case <-e.granted:
		return e, func() { s.release(e) }, nil
	case <-ctx.Done():
		s.mu.Lock()
		select {
		case <-e.granted: // 刚好被放行：交还名额
			s.mu.Unlock()
			s.release(e)
		default:
			s.removeQueuedLocked(e)
			s.dispatchLocked() // 队首离开可能解除对别人的阻挡
			s.mu.Unlock()
		}
		return e, nil, ctx.Err()
	}
}

func (s *scheduler) release(e *entry) {
	s.mu.Lock()
	delete(s.running, e.id)
	s.dispatchLocked()
	s.mu.Unlock()
}

func (s *scheduler) removeQueuedLocked(e *entry) {
	for i, q := range s.queue {
		if q == e {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

func (s *scheduler) canRunLocked(e *entry) bool {
	if len(s.running) >= s.cfg.MaxTotal {
		return false
	}
	rule := s.cfg.rule(e.model)
	if rule.Exclusive {
		if len(s.running) > 0 {
			return false
		}
	} else {
		n := 0
		for _, r := range s.running {
			if s.cfg.rule(r.model).Exclusive {
				return false
			}
			if r.model == e.model {
				n++
			}
		}
		if n >= rule.MaxConcurrent {
			return false
		}
	}
	return true
}

// dispatchLocked 反复从队列挑优先级最高（同级先到）且满足规则的请求放行。
// 防饿死：排队中有独占请求 E 时，优先级不高于 E 的非独占请求不放行。
func (s *scheduler) dispatchLocked() {
	for {
		sort.SliceStable(s.queue, func(i, j int) bool {
			if s.queue[i].prio != s.queue[j].prio {
				return s.queue[i].prio > s.queue[j].prio
			}
			return s.queue[i].id < s.queue[j].id
		})
		// 重新计算独占属性（配置可能热改）
		maxExcl := -1
		for _, q := range s.queue {
			q.excl = s.cfg.rule(q.model).Exclusive
			if q.excl && q.prio > maxExcl {
				maxExcl = q.prio
			}
		}
		picked := -1
		for i, q := range s.queue {
			if !q.excl && maxExcl >= q.prio {
				continue
			}
			if s.canRunLocked(q) {
				picked = i
				break
			}
		}
		if picked < 0 {
			return
		}
		q := s.queue[picked]
		s.queue = append(s.queue[:picked], s.queue[picked+1:]...)
		q.started = s.now()
		s.running[q.id] = q
		close(q.granted)
	}
}

// ReqInfo 是 /gw/status 里一条请求。
type ReqInfo struct {
	Model    string  `json:"model"`
	Priority int     `json:"priority"`
	Client   string  `json:"client,omitempty"`
	RunSec   float64 `json:"run_seconds,omitempty"`
	WaitSec  float64 `json:"wait_seconds,omitempty"`
}

func (s *scheduler) status() (running, queued []ReqInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	running, queued = []ReqInfo{}, []ReqInfo{}
	ids := make([]*entry, 0, len(s.running))
	for _, e := range s.running {
		ids = append(ids, e)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].id < ids[j].id })
	for _, e := range ids {
		running = append(running, ReqInfo{Model: e.model, Priority: e.prio, Client: e.client,
			RunSec: now.Sub(e.started).Seconds(), WaitSec: e.started.Sub(e.enqueued).Seconds()})
	}
	for _, e := range s.queue {
		queued = append(queued, ReqInfo{Model: e.model, Priority: e.prio, Client: e.client, WaitSec: now.Sub(e.enqueued).Seconds()})
	}
	return
}
