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

// PeerState 是对端上报的忙闲状态。只有明确上报过 idle 的地址才允许送非 Now 消息；
// busy、gone（对方窗口里不是 Claude）、以及没上报过的地址都按住。
// busy 超过 busyStale 没再变化视为失联，按可送处理（见 peerReadyLocked）。
type PeerState string

const (
	PeerBusy PeerState = "busy"
	PeerIdle PeerState = "idle"
	PeerGone PeerState = "gone"
)

// 消息类型：KindRequest 要对方做事并回复，KindReply 是对某条请求的回复，
// KindNotice 只是通知，KindCode 表示代码已按 repo/branch 交过去。
const (
	KindRequest = "request"
	KindReply   = "reply"
	KindNotice  = "notice"
	KindCode    = "code"
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

	Kind     string `json:"kind"`
	ReplyTo  string `json:"reply_to,omitempty"` // 仅 reply 使用，指向被回复的 request
	AskUser  bool   `json:"ask_user,omitempty"` // 发件人声明：涉及决定的部分要交给用户
	Answered bool   `json:"answered,omitempty"` // 仅 request 有意义：收到过合法 reply
}

// ErrMsgNotFound 表示消息 ID 不在 Hub 里（不存在或已被淘汰）。
var ErrMsgNotFound = errors.New("support: 没有这条消息")

// ErrPeerNotClaude 表示送达时发现对方窗口里跑的不是 Claude。
// 撞上它不算投递失败：不计数、不退避，只把对端记成 gone 按住消息，等对方重新上报 idle。
var ErrPeerNotClaude = errors.New("对方窗口里不是 Claude")

const (
	maxMsgTextLen  = 64 * 1024   // Text 上限 64KB
	maxMsgFileLen  = 1024 * 1024 // 附件上限 1MB
	maxMsgKeep     = 200         // 内存保留的消息条数（只淘汰已结束的）
	maxMsgOpen     = 50          // 未结束（pending/queued/sync_failed）消息上限，超了拒收新消息
	maxNameLen     = 100         // 附件名净化后按字节截断的长度
	maxAutoMinutes = 600         // 自动模式最长分钟数

	unackedTakeTimeout = 60 * time.Second // 出站消息被取走后多久没确认送达就重新排队
	backoffBase        = 2 * time.Second  // Deliver 失败退避基数
	backoffMax         = 60 * time.Second // Deliver 失败退避上限

	busyStale = 15 * time.Minute // busy 这么久没再变化视为失联，非 Now 消息按可送处理
	nagMax    = 2                // 单条欠回复请求经 NagUnanswered 提醒的次数上限
	nagWindow = 24 * time.Hour   // 欠回复提醒窗口，创建超过窗口的不再提醒
)

// unackedReason 是取走后超时未确认写进 Reason 的提示语。
const unackedReason = "取走后 60 秒未确认送达，重新排队"

// deliveredWhileRejectedReason 是送达成功但消息已被拒绝时写进日志的提示语（不改状态）。
const deliveredWhileRejectedReason = "拒绝时已经送达"

var addrNameIllegal = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// sanitizeText 把 \r\n 统一成 \n 后，去掉除 \n、\t 外的所有 C0 控制字符（0x00–0x1F）、
// 0x7F 和 C1（U+0080–U+009F），防止终端转义序列混进正文。
func sanitizeText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, text)
}

// sanitizeFileName 把不在 [A-Za-z0-9._-] 里的字符换成 '_'，按字节截到 maxNameLen；
// 结果为空或全是点则报错。先净化再截断，截断点一定落在 ASCII 上，不会切坏多字节字符。
func sanitizeFileName(name string) (string, error) {
	var b strings.Builder
	for _, r := range name {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > maxNameLen {
		out = out[:maxNameLen]
	}
	if out == "" || strings.Trim(out, ".") == "" {
		return "", fmt.Errorf("support: 附件 FileName %q 净化后为空或全是点", name)
	}
	return out, nil
}

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
	msgs      []Message            // 按加入顺序（即创建时间先后），File 一直留在msgs 里，取用时给完整内容
	outbox    []string             // 等 mac: 接收方取的消息 ID，按放入顺序；内容以 h.msgs 当前值为准
	outPlaced map[string]bool      // 已放进过 outbox 的消息 ID（同一条只放一次；取走后超时未确认才允许重放）
	inflight  map[string]bool      // 正在 pumpOne 处理（Sync/Deliver）的消息 ID，防并发双发；整个 pumpOne 结束才释放
	takenAt   map[string]time.Time // 出站消息被 TakeOutbox 取走的时间；超时未确认由 pumpOne 重新排队
	nextTry   map[string]time.Time // Deliver 失败后的下次可重试时间（指数退避）
	fails     map[string]int       // Deliver 连续失败次数，退避用
	peers     map[string]PeerState
	peerAt    map[string]time.Time // 对端状态最近一次变化的时间（busy 失联判定用）
	peerSeq   map[string]uint64    // 对端状态版本号：送达期间有新上报就不覆盖对方状态
	nagged    map[string]int       // 单条欠回复请求已提醒过的次数（NagUnanswered 用）
	autoUntil time.Time            // 零值表示自动模式关闭
	wakeup    chan struct{}        // outbox 有变化或 SetState 时翻转，唤醒长轮询的 TakeOutbox
}

// NewHub 新建一个空的 Hub。
func NewHub(cfg HubConfig) *Hub {
	return &Hub{
		cfg:       cfg,
		outbox:    nil,
		outPlaced: make(map[string]bool),
		inflight:  make(map[string]bool),
		takenAt:   make(map[string]time.Time),
		nextTry:   make(map[string]time.Time),
		fails:     make(map[string]int),
		peers:     make(map[string]PeerState),
		peerAt:    make(map[string]time.Time),
		peerSeq:   make(map[string]uint64),
		nagged:    make(map[string]int),
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

// setPeerLocked 更新对端状态并盖时间戳、版本号（版本号每次变化 +1）；调用方须持锁。
func (h *Hub) setPeerLocked(addr string, st PeerState) {
	h.peers[addr] = st
	h.peerAt[addr] = h.now()
	h.peerSeq[addr]++
}

// peerReadyLocked 判断能否给 addr 送非 Now 消息：上报过 idle 可送；
// busy 超过 busyStale 视为失联（进程挂了/忘了报），也按可送处理；
// gone 与没上报过仍按住。调用方须持锁。
func (h *Hub) peerReadyLocked(addr string) bool {
	switch h.peers[addr] {
	case PeerIdle:
		return true
	case PeerBusy:
		return h.now().Sub(h.peerAt[addr]) > busyStale
	default: // gone、没上报过
		return false
	}
}

// markAnsweredLocked 把一条已送达回复指向的原请求标成已回复；原请求已被淘汰就算了。
// 只有回复真正送达时才调；调用方须持锁。
func (h *Hub) markAnsweredLocked(reply Message) {
	if reply.Kind != KindReply || reply.ReplyTo == "" {
		return
	}
	if j := h.findLocked(reply.ReplyTo); j >= 0 {
		h.msgs[j].Answered = true
	}
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

// trimLocked 把消息数裁到 maxMsgKeep：只删最早已结束（delivered/rejected）的消息，
// 一条已结束的都找不到就不删（允许暂时超过 maxMsgKeep，未结束消息另有 Send 的 maxMsgOpen 上限兜底）。
// 若清理动过 outbox，返回要 close 的 wakeup（锁外 close）。
// 调用方须持锁。
func (h *Hub) trimLocked() (wake chan struct{}) {
	touched := false
	for len(h.msgs) > maxMsgKeep {
		idx := -1
		for i, m := range h.msgs {
			if m.State == MsgDelivered || m.State == MsgRejected {
				idx = i
				break
			}
		}
		if idx < 0 {
			break // 全是未结束的消息：不删，让超限暂时存在
		}
		gone := h.msgs[idx]
		h.msgs = append(h.msgs[:idx], h.msgs[idx+1:]...)
		delete(h.outPlaced, gone.ID)
		delete(h.inflight, gone.ID)
		delete(h.takenAt, gone.ID)
		delete(h.nextTry, gone.ID)
		delete(h.fails, gone.ID)
		delete(h.nagged, gone.ID)
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
	if m.Kind == "" {
		m.Kind = KindNotice
	}
	switch m.Kind {
	case KindRequest, KindReply, KindNotice, KindCode:
	default:
		return "", fmt.Errorf("support: 未知消息类型 %q（只能是 request/reply/notice/code）", m.Kind)
	}
	// reply 的收件人来自被回复的那条请求，所以要先查出原消息才能校验 To。
	var orig Message
	var haveOrig bool
	if m.Kind == KindReply {
		if m.ReplyTo == "" {
			return "", fmt.Errorf("support: 回复必须给 reply_to")
		}
		h.mu.Lock()
		i := h.findLocked(m.ReplyTo)
		if i >= 0 {
			orig, haveOrig = h.msgs[i], true
		}
		h.mu.Unlock()
		if !haveOrig {
			return "", fmt.Errorf("support: reply_to %q: %w", m.ReplyTo, ErrMsgNotFound)
		}
		if orig.Kind != KindRequest {
			return "", fmt.Errorf("support: #%s 不是请求，不能回复", m.ReplyTo)
		}
		if orig.To != m.From {
			return "", fmt.Errorf("support: #%s 不是发给 %s 的，不能回复", m.ReplyTo, m.From)
		}
		if orig.State != MsgDelivered {
			return "", fmt.Errorf("support: #%s 还没送达，不能回复", m.ReplyTo)
		}
		// 收件人默认就是原请求的发件人；显式给了别的地址等于凭空换人，拒收。
		if m.To == "" {
			m.To = orig.From
		} else if m.To != orig.From {
			return "", fmt.Errorf("support: 回复的收件人应是原请求的发件人 %s，不能填 %q", orig.From, m.To)
		}
	} else if m.ReplyTo != "" {
		return "", fmt.Errorf("support: 只有回复才能带 reply_to")
	}
	if m.Kind == KindCode && (m.Repo == "" || m.Branch == "") {
		return "", fmt.Errorf("support: 交代码（code）必须带仓库和分支")
	}
	if m.Kind == KindReply && (m.Repo != "" || m.Branch != "") {
		return "", fmt.Errorf("support: 回复不能带仓库和分支，交代码请用 code")
	}
	if err := ValidAddr(m.From); err != nil {
		return "", err
	}
	if err := ValidAddr(m.To); err != nil {
		return "", err
	}
	if m.From == m.To {
		return "", fmt.Errorf("support: From 与 To 不能相同（%q）", m.From)
	}
	// 地址已校验过，前缀相同即同一侧；同侧消息不经中转同步代码，带上只会引起误解
	if strings.HasPrefix(m.From, "mac:") == strings.HasPrefix(m.To, "mac:") &&
		(m.Repo != "" || m.Branch != "") {
		return "", fmt.Errorf("support: 同一台机器上的消息不需要同步代码，去掉 --repo/--branch")
	}
	m.Text = sanitizeText(m.Text)
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
		cleaned, err := sanitizeFileName(m.FileName)
		if err != nil {
			return "", err
		}
		m.FileName = cleaned
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
	open := 0
	for i := range h.msgs {
		switch h.msgs[i].State {
		case MsgPending, MsgQueued, MsgSyncFailed:
			open++
		}
	}
	if open >= maxMsgOpen {
		h.mu.Unlock()
		return "", fmt.Errorf("support: 未处理的消息太多（%d 条），先处理再发", maxMsgOpen)
	}
	for {
		m.ID = newID()
		if h.findLocked(m.ID) < 0 {
			break
		}
	}
	// AskUser 的消息即使自动模式开着也要停下来等用户：标了"要用户定"就不能自动放行。
	if !h.autoUntil.IsZero() && now.Before(h.autoUntil) && !m.AskUser {
		m.State = MsgQueued
	} else {
		m.State = MsgPending
	}
	h.msgs = append(h.msgs, m)
	// 这里不把被回复的请求标成 Answered：回复只有真正送达（pumpOne 成功 / MarkDelivered）
	// 才算回了；发出后被拒/一直没送，原请求仍算欠回复。
	wake := h.trimLocked()
	h.mu.Unlock()

	if wake != nil {
		close(wake) // 淘汰顺带清了 outbox 条目，叫醒等待中的 TakeOutbox
	}
	h.logMsg(m)
	return m.ID, nil
}

// SetState 记录对端忙闲状态（busy/idle/gone）；gone 表示对方窗口里此刻不是 Claude。
// 没上报过的地址不在表里，与 busy/gone 一样按住非 Now 消息。
// 每次调用都会翻转 wakeup：等待中的 TakeOutbox 要能在对方变 idle 时醒来重查。
func (h *Hub) SetState(addr string, st PeerState) error {
	if err := ValidAddr(addr); err != nil {
		return err
	}
	if st != PeerBusy && st != PeerIdle && st != PeerGone {
		return fmt.Errorf("support: 未知对端状态 %q", st)
	}
	h.mu.Lock()
	h.setPeerLocked(addr, st)
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

// kindLabel 是消息类型在终端里的中文说法。
func kindLabel(kind string) string {
	switch kind {
	case KindRequest:
		return "请求"
	case KindReply:
		return "回复"
	case KindCode:
		return "交代码"
	default:
		return "通知"
	}
}

// Display 把消息渲染成注入终端的文本，Mac 与 Ubuntu 两侧共用同一套格式。
// attachPath 由调用方算好（Mac 侧用 MacInboxPath），空表示不带附件行。
func Display(m Message, attachPath string) string {
	head := fmt.Sprintf("[XXYI 消息 #%s · ", m.ID)
	if m.Kind == KindReply {
		head += fmt.Sprintf("回复 #%s", m.ReplyTo)
	} else {
		head += kindLabel(m.Kind)
	}
	head += fmt.Sprintf(" · 来自 %s]", m.From)

	out := head + "\n" + m.Text
	if m.Kind == KindCode {
		if m.Synced {
			out += fmt.Sprintf("\n（代码已同步：仓库 %s 分支 %s）", m.Repo, m.Branch)
		} else {
			// 用户选了不同步直接送：必须让收件人知道代码还没到。
			out += fmt.Sprintf("\n（注意：代码没有同步，仓库 %s 分支 %s）", m.Repo, m.Branch)
		}
	}
	if attachPath != "" {
		out += fmt.Sprintf("\n（附件：%s）", attachPath)
	}
	if m.Kind == KindRequest {
		out += fmt.Sprintf("\n（这是请求：用 xxyi_reply 回复 #%s；做不了也要回复说明原因）", m.ID)
	}
	if m.AskUser {
		out += "\n（对方标了「要用户定」：涉及决定的部分请交给用户，不要自己拍板）"
	}
	return out
}

// MacInboxPath 是附件落到 mac 收件箱的路径；没有附件返回空串。
func MacInboxPath(m Message) string {
	if m.FileName == "" {
		return ""
	}
	return fmt.Sprintf("~/xxyi-work/inbox/%s-%s", m.ID, m.FileName)
}

// Unanswered 返回发给 addr、已送达但还没被回复的请求，按创建时间从旧到新。
// 返回的是拷贝，不含 File。
func (h *Hub) Unanswered(addr string) []Message {
	h.mu.Lock()
	var out []Message
	for _, m := range h.msgs {
		if m.To == addr && m.State == MsgDelivered && m.Kind == KindRequest && !m.Answered {
			out = append(out, m)
		}
	}
	h.mu.Unlock()
	for i := range out {
		out[i].File = nil
	}
	return out
}

// NagUnanswered 从 Unanswered(addr) 里挑出还能提醒的：已提醒次数没到 nagMax、
// 且创建时间在 nagWindow 之内的，每条计数加一后返回。超窗或提醒够数的不再打扰，
// 调用方（界面轮询）反复调也只会在每条上限内返回。
func (h *Hub) NagUnanswered(addr string) []Message {
	un := h.Unanswered(addr)
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	var out []Message
	for _, m := range un {
		if h.nagged[m.ID] >= nagMax || now.Sub(m.Created) > nagWindow {
			continue
		}
		h.nagged[m.ID]++
		out = append(out, m)
	}
	return out
}

// Inbox 返回发给 addr 的最近 n 条消息，最新在前；返回拷贝，不含 File。
func (h *Hub) Inbox(addr string, n int) []Message {
	h.mu.Lock()
	var out []Message
	for i := len(h.msgs) - 1; i >= 0 && len(out) < n; i-- {
		if h.msgs[i].To == addr {
			out = append(out, h.msgs[i])
		}
	}
	h.mu.Unlock()
	for i := range out {
		out[i].File = nil
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
		// 已结束：清退避/取走跟踪，附件不再占内存
		delete(h.takenAt, id)
		delete(h.nextTry, id)
		delete(h.fails, id)
		cur.File = nil
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
		ready := h.peerReadyLocked(to) // idle 可送；失联的 busy 也可送；gone/没上报按住
		for k := 0; k < len(h.outbox); k++ {
			id := h.outbox[k]
			i := h.findLocked(id)
			if i < 0 || h.msgs[i].State != MsgQueued {
				// 已被拒绝/淘汰/确认：脏条目直接清掉
				h.outbox = append(h.outbox[:k], h.outbox[k+1:]...)
				k-- // 元素前移，原位重查
				continue
			}
			if h.msgs[i].To != to {
				continue // outbox 是全局队列，只取发给本收件人的
			}
			if !ready && !h.msgs[i].Now {
				continue // 对方不可送（busy 未失联/gone/没上报），非 now 的按住
			}
			out := h.msgs[i]
			h.outbox = append(h.outbox[:k], h.outbox[k+1:]...)
			h.takenAt[id] = h.now()       // 记取走时间，超时未确认由 pumpOne 重新排队
			h.setPeerLocked(to, PeerBusy) // 取走即算送出，置忙避免连发
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
	delete(h.takenAt, id)
	delete(h.nextTry, id)
	delete(h.fails, id)
	cur.File = nil // 已结束，附件不再占内存
	// 回复确认送达，原请求才算真被回了；原请求已被淘汰就算了
	h.markAnsweredLocked(*cur)
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
// 同步/送达回调期间消息可能被 reject/淘汰，每个落状态的点都必须在同一把锁内
// 重查当前状态仍是 queued，否则就地收尾，不再往下送。
// ctx 已取消时回调结果一概不写状态（取消不等于失败），只释放 inflight。
func (h *Hub) pumpOne(ctx context.Context, id string) {
	h.mu.Lock()
	i := h.findLocked(id)
	if i < 0 || h.msgs[i].State != MsgQueued {
		h.mu.Unlock()
		return
	}
	m := h.msgs[i]
	var reclaimed *Message
	// 取走后超时未确认：收回取走标记，让它重新进 outbox
	if taken, ok := h.takenAt[id]; ok && h.now().Sub(taken) > unackedTakeTimeout {
		delete(h.outPlaced, id)
		delete(h.takenAt, id)
		h.msgs[i].Reason = unackedReason
		cp := h.msgs[i]
		reclaimed = &cp
		m = cp
	}
	// 只对可送的对端推进非 Now 消息：idle 或失联的 busy；gone、没上报过的都按住。
	if !m.Now && !h.peerReadyLocked(m.To) {
		h.mu.Unlock()
		if reclaimed != nil {
			h.logMsg(*reclaimed)
		}
		return
	}
	if next, ok := h.nextTry[id]; ok && h.now().Before(next) {
		h.mu.Unlock() // 送达失败退避中，Now 也要遵守
		if reclaimed != nil {
			h.logMsg(*reclaimed)
		}
		return
	}
	needSync := m.Repo != "" && !m.Synced
	macDest := strings.HasPrefix(m.To, "mac:")
	if h.inflight[id] {
		h.mu.Unlock()
		if reclaimed != nil {
			h.logMsg(*reclaimed)
		}
		return
	}
	h.inflight[id] = true
	h.mu.Unlock()
	if reclaimed != nil {
		h.logMsg(*reclaimed)
	}

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
		if ctx.Err() != nil {
			// 取消不是失败：状态、Reason、Synced 都不动
			delete(h.inflight, id)
		} else if j := h.findLocked(id); j >= 0 && h.msgs[j].State == MsgQueued {
			if err != nil {
				h.msgs[j].State = MsgSyncFailed
				h.msgs[j].Reason = err.Error()
				delete(h.nextTry, id) // 离开 queued，退避作废
				delete(h.fails, id)
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
			return // 同步失败、被取消，或同步期间被 reject/淘汰
		}
	}

	if macDest {
		h.mu.Lock()
		// 与取件端一致的闸门：对端不是 idle 时非 Now 消息不进 outbox，
		// 等它回 idle 后由下一次 Pump 重新放进来。
		if !m.Now && !h.peerReadyLocked(m.To) {
			delete(h.inflight, id)
			h.mu.Unlock()
			if reclaimed != nil {
				h.logMsg(*reclaimed)
			}
			return
		}
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

	// 记下调用 Deliver 前的对端状态版本号：送达期间对端若有新上报（比如钩子报了 idle），
	// 回来后不能拿 busy 把人覆盖掉。
	h.mu.Lock()
	seq0 := h.peerSeq[m.To]
	h.mu.Unlock()

	var err error
	if h.cfg.Deliver == nil {
		err = errors.New("未配置 Deliver，无法送达")
	} else {
		err = h.cfg.Deliver(ctx, m)
	}
	h.mu.Lock()
	var out *Message
	if ctx.Err() != nil {
		// 取消不是失败：状态、Reason 都不动
	} else if j := h.findLocked(id); j >= 0 {
		if h.msgs[j].State == MsgQueued {
			if errors.Is(err, ErrPeerNotClaude) {
				// 对面不是 Claude：记 gone 把后续消息整体按住。这不算投递失败——
				// 不累计失败次数、不设退避，对方重新上报 idle 后下一轮 Pump 立刻重试。
				h.setPeerLocked(m.To, PeerGone)
				h.msgs[j].Reason = err.Error()
			} else if err != nil {
				// 保持 queued，只记 Reason；指数退避，下次 Pump 到点才再试。
				h.msgs[j].Reason = err.Error()
				h.fails[id]++
				delay := backoffBase << uint(h.fails[id]-1)
				if delay > backoffMax || delay <= 0 { // 溢出视为封顶
					delay = backoffMax
				}
				h.nextTry[id] = h.now().Add(delay)
			} else {
				h.msgs[j].State = MsgDelivered
				h.msgs[j].Reason = ""
				h.msgs[j].File = nil // 已结束，附件不再占内存
				// 送达期间对端没再上报才置 busy，别把钩子刚报的 idle 覆盖掉
				if h.peerSeq[m.To] == seq0 {
					h.setPeerLocked(m.To, PeerBusy)
				}
				// 回复送达成功，这条请求才算真被回了
				h.markAnsweredLocked(h.msgs[j])
				delete(h.nextTry, id)
				delete(h.fails, id)
				cp := h.msgs[j]
				out = &cp
			}
		} else if err == nil {
			// 送达成功但这期间已被 reject/淘汰：状态不动，只写一条日志提示
			cp := h.msgs[j]
			cp.Reason = deliveredWhileRejectedReason
			out = &cp
		}
	}
	delete(h.inflight, id)
	h.mu.Unlock()
	if out != nil {
		h.logMsg(*out)
	}
}
