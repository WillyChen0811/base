package support

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// MsgState 是一条中转消息的状态。
type MsgState string

const (
	MsgPending    MsgState = "pending"     // 待用户批准
	MsgQueued     MsgState = "queued"      // 已批准，等对方空闲
	MsgDelivered  MsgState = "delivered"   // 已送达
	MsgRejected   MsgState = "rejected"    // 用户拒绝
	MsgSyncFailed MsgState = "sync_failed" // 代码同步失败，等用户处理
)

// PeerState 是对端上报的忙闲状态；没上报过的地址视为 idle。
type PeerState string

const (
	PeerBusy PeerState = "busy"
	PeerIdle PeerState = "idle"
)

// Message 是一条中转消息。File 不进 JSON，由调用方经 Hub 流转。
type Message struct {
	ID       string    `json:"id"` // 随机 16 位十六进制
	From     string    `json:"from"`
	To       string    `json:"to"`
	Text     string    `json:"text"`
	FileName string    `json:"file_name,omitempty"`
	File     []byte    `json:"-"` // 附件内容，不进 JSON
	Repo     string    `json:"repo,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	State    MsgState  `json:"state"`
	Reason   string    `json:"reason,omitempty"`
	Now      bool      `json:"now,omitempty"` // 用户点了立即送达
	Synced   bool      `json:"synced,omitempty"`
	Created  time.Time `json:"created"`
}

// ErrMsgNotFound 表示消息 ID 不在 Hub 里（不存在或已被淘汰）。
var ErrMsgNotFound = errors.New("support: 没有这条消息")

const (
	maxMsgTextLen  = 64 * 1024   // Text 上限 64KB
	maxMsgFileLen  = 1024 * 1024 // 附件上限 1MB
	maxMsgKeep     = 200         // 内存保留的消息条数
	maxAutoMinutes = 600         // 自动模式最长分钟数
)

var addrNameIllegal = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// ValidAddr 校验中转地址：必须是 "mac:<名>" 或 "ubuntu:<名>"，
// 名字 1..64 字节，只允许 ASCII 字母数字和 . _ -。
func ValidAddr(addr string) error {
	host, name, ok := strings.Cut(addr, ":")
	if !ok || (host != "mac" && host != "ubuntu") {
		return fmt.Errorf("support: 地址 %q 必须以 mac: 或 ubuntu: 开头", addr)
	}
	if len(name) < 1 || len(name) > 64 {
		return fmt.Errorf("support: 地址名字长度必须 1..64 字节，实际 %d", len(name))
	}
	if m := addrNameIllegal.FindString(name); m != "" {
		return fmt.Errorf("support: 地址名字包含非法字符 %q，只允许 ASCII 字母数字和 . _ -", m)
	}
	return nil
}

// HubConfig 是 Hub 的注入依赖。
type HubConfig struct {
	// Sync 把 from 一侧的 repo/branch 同步到 to 一侧；返回错误则消息进 sync_failed，Reason=err.Error()。
	Sync func(ctx context.Context, m Message) error
	// Deliver 把消息注入接收方终端。返回错误：消息保持 queued、Reason 记错误，下次再试。
	// 发往 "mac:" 开头地址的消息不调 Deliver，而是放进 outbox 等 TakeOutbox 取走。
	Deliver func(ctx context.Context, m Message) error
	Log     func(kind string, detail map[string]string) // 可为 nil
	Now     func() time.Time                            // 可为 nil，用 time.Now
}

// Hub 是消息中转队列，纯内存、并发安全；网络与 git 由 cfg 注入。
type Hub struct {
	mu        sync.Mutex
	cfg       HubConfig
	msgs      []Message       // 按加入顺序（即创建时间先后），File 一直留在msgs 里，取用时给完整内容
	outbox    []string        // 等 mac: 接收方取的消息 ID，按放入顺序；内容以 h.msgs 当前值为准
	outPlaced map[string]bool // 已放进过 outbox 的消息 ID（同一条只放一次，取走也不再加）
	inflight  map[string]bool // 正在 pumpOne 处理（Sync/Deliver）的消息 ID，防并发双发；整个 pumpOne 结束才释放
	peers     map[string]PeerState
	autoUntil time.Time     // 零值表示自动模式关闭
	wakeup    chan struct{} // outbox 有变化或 SetState 时翻转，唤醒长轮询的 TakeOutbox
}

// NewHub 新建一个空的 Hub。
func NewHub(cfg HubConfig) *Hub {
	return &Hub{
		cfg:       cfg,
		outbox:    nil,
		outPlaced: make(map[string]bool),
		inflight:  make(map[string]bool),
		peers:     make(map[string]PeerState),
		wakeup:    make(chan struct{}),
	}
}

func (h *Hub) now() time.Time {
	if h.cfg.Now != nil {
		return h.cfg.Now()
	}
	return time.Now()
}

// logMsg 通知一条消息的状态变化；必须在锁外调用。
func (h *Hub) logMsg(m Message) {
	if h.cfg.Log == nil {
		return
	}
	h.cfg.Log("msg", map[string]string{
		"id":     m.ID,
		"from":   m.From,
		"to":     m.To,
		"state":  string(m.State),
		"reason": m.Reason,
	})
}

// findLocked 按 ID 找消息下标，找不到返回 -1。调用方须持锁。
func (h *Hub) findLocked(id string) int {
	for i := range h.msgs {
		if h.msgs[i].ID == id {
			return i
		}
	}
	return -1
}

// flipWakeupLocked 摘下当前 wakeup 换成新的，返回被摘下的；调用方须在锁外 close。
// 凡是"等待中的 TakeOutbox 可能因此有（或没了）可取消息"的动作都要翻转一次：
// 进 outbox、SetState、decide now/reject、淘汰清理。
func (h *Hub) flipWakeupLocked() chan struct{} {
	w := h.wakeup
	h.wakeup = make(chan struct{})
	return w
}

// removeFromOutboxLocked 把 ID 移出 outbox（若在里面），返回是否真的移除了。
// 调用方须持锁。
func (h *Hub) removeFromOutboxLocked(id string) bool {
	for i := range h.outbox {
		if h.outbox[i] == id {
			h.outbox = append(h.outbox[:i], h.outbox[i+1:]...)
			return true
		}
	}
	return false
}

// trimLocked 把消息数裁到 maxMsgKeep：优先删最早的已结束消息（delivered/rejected），
// 都没结束就删最早的。若清理动过 outbox，返回要 close 的 wakeup（锁外 close）。
// 调用方须持锁。
func (h *Hub) trimLocked() (wake chan struct{}) {
	touched := false
	for len(h.msgs) > maxMsgKeep {
		idx := 0
		for i, m := range h.msgs {
			if m.State == MsgDelivered || m.State == MsgRejected {
				idx = i
				break
			}
		}
		gone := h.msgs[idx]
		h.msgs = append(h.msgs[:idx], h.msgs[idx+1:]...)
		delete(h.outPlaced, gone.ID)
		delete(h.inflight, gone.ID)
		if h.removeFromOutboxLocked(gone.ID) {
			touched = true
		}
	}
	if touched {
		wake = h.flipWakeupLocked()
	}
	return wake
}

// Send 校验消息后入库，返回 ID。自动模式开着则直接 queued，否则 pending。
// 入参里的 State/Reason/Now/Synced 会被重置，由 Hub 统一管理。
func (h *Hub) Send(m Message) (string, error) {
	if err := ValidAddr(m.From); err != nil {
		return "", err
	}
	if err := ValidAddr(m.To); err != nil {
		return "", err
	}
	if m.From == m.To {
		return "", fmt.Errorf("support: From 与 To 不能相同（%q）", m.From)
	}
	if strings.TrimSpace(m.Text) == "" {
		return "", fmt.Errorf("support: 消息内容不能为空")
	}
	if len(m.Text) > maxMsgTextLen {
		return "", fmt.Errorf("support: 消息内容超过 64KB（%d 字节）", len(m.Text))
	}
	if len(m.File) > maxMsgFileLen {
		return "", fmt.Errorf("support: 附件超过 1MB（%d 字节）", len(m.File))
	}
	if len(m.File) > 0 {
		if m.FileName == "" {
			return "", fmt.Errorf("support: 有附件必须给 FileName")
		}
		m.FileName = path.Base(m.FileName)
		if m.FileName == "" || m.FileName == "." || m.FileName == "/" {
			return "", fmt.Errorf("support: 附件 FileName %q 无效", m.FileName)
		}
	}
	if (m.Repo == "") != (m.Branch == "") {
		return "", fmt.Errorf("support: Repo 与 Branch 必须同时给出或同时为空")
	}
	if m.Branch != "" {
		if err := ValidBranch(m.Branch); err != nil {
			return "", err
		}
	}

	now := h.now()
	m.Created = now
	m.Reason = ""
	m.Now = false
	m.Synced = false

	h.mu.Lock()
	for {
		m.ID = newID()
		if h.findLocked(m.ID) < 0 {
			break
		}
	}
	if !h.autoUntil.IsZero() && now.Before(h.autoUntil) {
		m.State = MsgQueued
	} else {
		m.State = MsgPending
	}
	h.msgs = append(h.msgs, m)
	wake := h.trimLocked()
	h.mu.Unlock()

	if wake != nil {
		close(wake) // 淘汰顺带清了 outbox 条目，叫醒等待中的 TakeOutbox
	}
	h.logMsg(m)
	return m.ID, nil
}

// SetState 记录对端忙闲状态；没上报过的地址视为 idle。
// 每次调用都会翻转 wakeup：等待中的 TakeOutbox 要能在对方变 idle 时醒来重查。
func (h *Hub) SetState(addr string, st PeerState) error {
	if err := ValidAddr(addr); err != nil {
		return err
	}
	if st != PeerBusy && st != PeerIdle {
		return fmt.Errorf("support: 未知对端状态 %q", st)
	}
	h.mu.Lock()
	h.peers[addr] = st
	wake := h.flipWakeupLocked()
	h.mu.Unlock()
	close(wake)
	return nil
}

// Peers 返回对端状态表的拷贝。
func (h *Hub) Peers() map[string]PeerState {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]PeerState, len(h.peers))
	for k, v := range h.peers {
		out[k] = v
	}
	return out
}

// List 返回所有消息的拷贝，最新在前；不含 File。
func (h *Hub) List() []Message {
	h.mu.Lock()
	out := make([]Message, len(h.msgs))
	copy(out, h.msgs)
	h.mu.Unlock()
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	for i := range out {
		out[i].File = nil
	}
	return out
}

// Decide 对一条消息做决定。action：approve / now / reject / retry_sync / skip_sync。
// 状态不满足返回错误；id 不存在返回 ErrMsgNotFound。
// reject 时若消息还躺在 outbox 里（已放未取），一并移除。
func (h *Hub) Decide(id, action string) error {
	h.mu.Lock()
	i := h.findLocked(id)
	if i < 0 {
		h.mu.Unlock()
		return ErrMsgNotFound
	}
	m := h.msgs[i]
	var (
		target MsgState
		ok     bool
	)
	switch action {
	case "approve":
		target, ok = MsgQueued, m.State == MsgPending
	case "now":
		target, ok = MsgQueued, m.State == MsgPending || m.State == MsgQueued
	case "reject":
		target, ok = MsgRejected, m.State == MsgPending || m.State == MsgQueued || m.State == MsgSyncFailed
	case "retry_sync":
		target, ok = MsgQueued, m.State == MsgSyncFailed
	case "skip_sync":
		target, ok = MsgQueued, m.State == MsgSyncFailed
	default:
		h.mu.Unlock()
		return fmt.Errorf("support: 未知操作 %q", action)
	}
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("support: 消息 %s 当前状态 %s，不能 %s", id, m.State, action)
	}
	cur := &h.msgs[i]
	cur.State = target
	var wake chan struct{}
	switch action {
	case "now":
		cur.Now = true
		// 消息可能正躺在 outbox 里等对方空闲：now 让它立刻可取，必须叫醒长轮询
		wake = h.flipWakeupLocked()
	case "retry_sync":
		cur.Reason = ""
	case "skip_sync":
		cur.Synced = true
	case "reject":
		// 已放进 outbox 未取走的要移掉；移掉也可能改变等待者下一轮该拿什么
		if h.removeFromOutboxLocked(id) {
			wake = h.flipWakeupLocked()
		}
	}
	out := *cur
	h.mu.Unlock()

	if wake != nil {
		close(wake)
	}
	h.logMsg(out)
	return nil
}

// SetAuto 开/关自动模式：minutes 为 0 关闭，1..600 表示从现在起开放的分钟数，其余报错。
func (h *Hub) SetAuto(minutes int) error {
	if minutes < 0 || minutes > maxAutoMinutes {
		return fmt.Errorf("support: 自动模式时长必须 0..%d 分钟，实际 %d", maxAutoMinutes, minutes)
	}
	h.mu.Lock()
	if minutes == 0 {
		h.autoUntil = time.Time{}
	} else {
		h.autoUntil = h.now().Add(time.Duration(minutes) * time.Minute)
	}
	h.mu.Unlock()
	return nil
}

// AutoUntil 返回自动模式截止时间；关着或已过期返回零值（过期视为关）。
func (h *Hub) AutoUntil() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.autoUntil.IsZero() && !h.now().Before(h.autoUntil) {
		return time.Time{}
	}
	return h.autoUntil
}

// TakeOutbox 长轮询取一条发往 to 的出站消息（含 File）。
// 只返回"现在可送"的消息：接收方不忙，或消息被点了 now（都按 h.msgs 里的当前值判断）。
// 取走即从 outbox 移除且不会重复给；送达确认仍要靠 MarkDelivered。
// ctx 结束仍没有可取的消息时返回 false。
func (h *Hub) TakeOutbox(ctx context.Context, to string) (Message, bool) {
	for {
		h.mu.Lock()
		busy := h.peers[to] == PeerBusy
		for k := 0; k < len(h.outbox); k++ {
			id := h.outbox[k]
			i := h.findLocked(id)
			if i < 0 || h.msgs[i].State != MsgQueued {
				// 已被拒绝/淘汰/确认：脏条目直接清掉
				h.outbox = append(h.outbox[:k], h.outbox[k+1:]...)
				k-- // 元素前移，原位重查
				continue
			}
			if busy && !h.msgs[i].Now {
				continue // 对方还忙，非 now 的按住
			}
			out := h.msgs[i]
			h.outbox = append(h.outbox[:k], h.outbox[k+1:]...)
			h.peers[to] = PeerBusy // 取走即算送出，置忙避免连发
			h.mu.Unlock()
			return out, true
		}
		wake := h.wakeup
		h.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return Message{}, false
		}
	}
}

// MarkDelivered 把 outbox 里被取走、已确认注入终端的消息标为 delivered。
func (h *Hub) MarkDelivered(id string) error {
	h.mu.Lock()
	i := h.findLocked(id)
	if i < 0 {
		h.mu.Unlock()
		return ErrMsgNotFound
	}
	cur := &h.msgs[i]
	if cur.State != MsgQueued {
		h.mu.Unlock()
		return fmt.Errorf("support: 消息 %s 当前状态 %s，不能确认送达", id, cur.State)
	}
	cur.State = MsgDelivered
	out := *cur
	h.mu.Unlock()

	h.logMsg(out)
	return nil
}

// Pump 按创建时间先后尝试推进所有 queued 消息一次。
// 规则：接收方忙且不是 Now 则跳过；要先同步且没同步过则先调 Sync；
// 发往 mac: 的进 outbox 等 TakeOutbox，其余调 Deliver；
// Deliver 成功或消息被取走后把接收方置忙。Sync/Deliver 在锁外调用。
func (h *Hub) Pump(ctx context.Context) {
	h.mu.Lock()
	var queue []Message
	for _, m := range h.msgs {
		if m.State == MsgQueued {
			queue = append(queue, m)
		}
	}
	h.mu.Unlock()

	sort.SliceStable(queue, func(i, j int) bool {
		return queue[i].Created.Before(queue[j].Created)
	})

	for _, m := range queue {
		if ctx.Err() != nil {
			return
		}
		h.pumpOne(ctx, m.ID)
	}
}

// pumpOne 推进一条 queued 消息。锁内只做检查和落状态，Sync/Deliver 在锁外调用。
// inflight 一路持有到本函数末尾才释放：同步成功后到送达/进 outbox 之间
// 若有另一个 Pump 进来，会因 inflight 被挡住，避免双发。
// 同步回调期间消息可能被 reject/淘汰，每个落状态的点都必须在同一把锁内
// 重查当前状态仍是 queued，否则就地收尾，不再往下送。
func (h *Hub) pumpOne(ctx context.Context, id string) {
	h.mu.Lock()
	i := h.findLocked(id)
	if i < 0 || h.msgs[i].State != MsgQueued {
		h.mu.Unlock()
		return
	}
	m := h.msgs[i]
	if !m.Now && h.peers[m.To] == PeerBusy {
		h.mu.Unlock()
		return
	}
	needSync := m.Repo != "" && !m.Synced
	macDest := strings.HasPrefix(m.To, "mac:")
	if h.inflight[id] {
		h.mu.Unlock()
		return
	}
	h.inflight[id] = true
	h.mu.Unlock()

	if needSync {
		var err error
		if h.cfg.Sync == nil {
			err = errors.New("未配置 Sync，无法同步代码")
		} else {
			err = h.cfg.Sync(ctx, m)
		}
		h.mu.Lock()
		continueSend := false
		var out *Message
		if j := h.findLocked(id); j >= 0 && h.msgs[j].State == MsgQueued {
			if err != nil {
				h.msgs[j].State = MsgSyncFailed
				h.msgs[j].Reason = err.Error()
				cp := h.msgs[j]
				out = &cp
			} else {
				h.msgs[j].Synced = true
				continueSend = true // 保持 inflight，送达/进 outbox 后再释放
			}
		}
		if !continueSend {
			delete(h.inflight, id)
		}
		h.mu.Unlock()
		if out != nil {
			h.logMsg(*out)
		}
		if !continueSend {
			return // 同步失败，或同步期间被 reject/淘汰
		}
	}

	if macDest {
		h.mu.Lock()
		var wake chan struct{}
		if j := h.findLocked(id); j >= 0 && h.msgs[j].State == MsgQueued {
			if !h.outPlaced[id] {
				h.outPlaced[id] = true
				h.outbox = append(h.outbox, id) // 只记 ID，取用读当前值
				wake = h.flipWakeupLocked()
			}
		}
		delete(h.inflight, id)
		h.mu.Unlock()
		if wake != nil {
			close(wake)
		}
		// 进 outbox 不算状态变化，也不置忙（被取走时才置），不写日志。
		return
	}

	var err error
	if h.cfg.Deliver == nil {
		err = errors.New("未配置 Deliver，无法送达")
	} else {
		err = h.cfg.Deliver(ctx, m)
	}
	h.mu.Lock()
	var out *Message
	if j := h.findLocked(id); j >= 0 && h.msgs[j].State == MsgQueued {
		if err != nil {
			// 保持 queued，只记 Reason；不置忙，下次 Pump 再试。
			h.msgs[j].Reason = err.Error()
		} else {
			h.msgs[j].State = MsgDelivered
			h.msgs[j].Reason = ""
			h.peers[m.To] = PeerBusy
			cp := h.msgs[j]
			out = &cp
		}
	}
	delete(h.inflight, id)
	h.mu.Unlock()
	if out != nil {
		h.logMsg(*out)
	}
}
