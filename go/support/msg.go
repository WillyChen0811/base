package support

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WillyChen0811/base/go/xxyica"
)

// MsgState 是一条中转消息的状态。
type MsgState string

const (
	MsgPending    MsgState = "pending"     // 待用户批准
	MsgQueued     MsgState = "queued"      // 已批准，等对方空闲
	MsgInjected   MsgState = "injected"    // 已注入对方终端，等对方下一次上报确认（Now 消息不做这一步）
	MsgDelivered  MsgState = "delivered"   // 已送达（对方确认收到）
	MsgRejected   MsgState = "rejected"    // 用户拒绝
	MsgSyncFailed MsgState = "sync_failed" // 同步重试用完（预算内先自动重试），等用户处理
	MsgFailed     MsgState = "failed"      // 重试预算用完，需人工处理（带失败报告）
)

// Attempt 是一次自动重试的失败记录（Stage：sync=代码同步、deliver=注入、ack=注入后等确认）。
type Attempt struct {
	At    time.Time `json:"at"`
	Stage string    `json:"stage"`
	Err   string    `json:"err"`
}

// FailReport 是转人工时的现场报告：卡在哪个阶段、当时对端什么状态、对方终端最后画面。
type FailReport struct {
	Stage     string    `json:"stage"`
	At        time.Time `json:"at"`
	PeerState string    `json:"peer_state"`       // 当时对端状态，没上报过为 "unknown"
	Screen    string    `json:"screen,omitempty"` // Capture 给的对方终端最后 40 行
}

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

// retryStages 是参与重试预算的阶段；预算键一律 id+"|"+stage。
var retryStages = []string{"sync", "deliver", "ack"}

// maxClientIDLen 是客户端幂等 ID（Message.ClientID）的字节上限。
const maxClientIDLen = 64

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

	Kind    string `json:"kind"`
	ReplyTo string `json:"reply_to,omitempty"` // 仅 reply 使用，指向被回复的 request
	AskUser bool   `json:"ask_user,omitempty"` // 发件人声明：涉及决定的部分要交给用户
	// ClientID 是发送客户端生成的幂等 ID（可空，≤64 字节、只许 [A-Za-z0-9._-]）：
	// 同 From 且同 ClientID 的 Send 不新建消息，直接返回已有那条的 ID。
	ClientID string `json:"client_id,omitempty"`
	Answered bool   `json:"answered,omitempty"` // 仅 request 有意义：收到过合法 reply
	// Overdue 仅 request 有意义：送达后超过 HubConfig.ReplyTimeout 仍没有回复，
	// Hub 标一次并由系统通知（From xxyi:hub）提醒原发件人。界面据此标红。
	Overdue bool `json:"overdue,omitempty"`

	Attempts []Attempt   `json:"attempts,omitempty"` // 自动重试失败记录，最多留最近 20 条
	Report   *FailReport `json:"report,omitempty"`   // 转人工（failed/sync_failed）时的现场报告

	// Sig 是 XXYI CA（HubConfig.Sign）对签名域（xxyica.MessageSigningBytes）的签名；
	// 没配 Sign 为空。收件方拿 CA 公钥即可核验"这条消息确实出自本 XXYI 中转"。
	Sig []byte `json:"sig,omitempty"`
	// FileSHA256 是附件内容的 SHA-256 hex（Send 时算；没附件为空）。
	// 在签名域里，附件释放（File 置 nil）后仍能核验"当时那个附件"。
	FileSHA256 string `json:"file_sha256,omitempty"`
}

// msgSignFields 抽出消息的签名域。签名和核验两侧都只经这里取字段，防止漏字段。
func msgSignFields(m Message) xxyica.MsgFields {
	return xxyica.MsgFields{
		ID:         m.ID,
		From:       m.From,
		To:         m.To,
		Kind:       m.Kind,
		ReplyTo:    m.ReplyTo,
		Text:       m.Text,
		FileName:   m.FileName,
		Repo:       m.Repo,
		Branch:     m.Branch,
		FileSHA256: m.FileSHA256,
		AskUser:    m.AskUser,
		Created:    m.Created,
	}
}

// ErrMsgNotFound 表示消息 ID 不在 Hub 里（不存在或已被淘汰）。
var ErrMsgNotFound = errors.New("support: 没有这条消息")

// ErrPeerNotClaude 表示送达时发现对方窗口里跑的不是 Claude。
// 撞上它不算投递失败：不计数、不退避，只把对端记成 gone 按住消息，等对方重新上报 idle。
var ErrPeerNotClaude = errors.New("对方窗口里不是 Claude")

// ErrLinkDown 表示线路不通（SSH/隧道断了这类）。Sync/Deliver 撞上它不算失败：
// 不记 Attempt、不计次数、不退避，Hub 整体进入暂停，之后按 linkProbeEvery 探测恢复。
var ErrLinkDown = errors.New("线路不通")

const (
	maxMsgTextLen  = 64 * 1024   // Text 上限 64KB
	maxMsgFileLen  = 1024 * 1024 // 附件上限 1MB
	maxMsgKeep     = 200         // 内存保留的消息条数（只淘汰已结束的）
	maxMsgOpen     = 50          // 未结束（pending/queued/sync_failed/failed）消息上限，超了拒收新消息；injected 不占名额（最多 AckTimeout 就会自己动）
	maxNameLen     = 100         // 附件名净化后按字节截断的长度
	maxAutoMinutes = 600         // 自动模式最长分钟数

	unackedTakeTimeout = 60 * time.Second // 出站消息被取走后多久没确认送达就重新排队
	backoffBase        = 2 * time.Second  // 失败退避基数
	backoffMax         = 5 * time.Minute  // 失败退避上限

	// 重试预算：同一阶段累计失败次数或有效经过时间先到者触发转人工
	// （failed / sync_failed）。线路不通的暂停时间不计入经过时间。
	defaultMaxAttempts = 5
	defaultRetryWindow = 30 * time.Minute

	// defaultAckTimeout：注入成功后再过这么久对端没有任何新上报，视为对方根本没开始处理
	// （额度用尽、界面报错、卡在确认框）——按 ack 阶段失败重回 queued 走同一套重试预算。
	defaultAckTimeout = 30 * time.Second
	// defaultReplyTimeout：请求送达后再过这么久仍没有回复，Hub 标 overdue 并通知原发件人。
	defaultReplyTimeout = 2 * time.Hour
	maxAttemptsKept     = 20              // Attempts 最多保留最近条数
	captureTimeout      = 5 * time.Second // 转人工时抓屏的超时

	// linkProbeEvery：暂停期间多久允许挑一条需要线路的消息做探测（测试可调小）。
	linkProbeEvery = 30 * time.Second

	// linkDownReason 是线路不通暂停时写进 Reason 的提示语。
	linkDownReason = "线路不通，暂停重试"

	// redirectReasonFmt 是 Redirect 给原消息写的拒绝原因。
	redirectReasonFmt = "已改送 #%s"

	// busyStale：busy 这么久没再变化视为失联，非 Now 消息按可送处理。
	// 取 30 分钟的取舍：Claude Code 会把输入排到当前任务后面，
	// 真跑超过 30 分钟的一轮可能在干活中途被注入新消息；
	// 权衡下来接受这个代价——换来对端失联（进程挂了/忘了报）后最多卡 30 分钟就能恢复送达。
	busyStale = 30 * time.Minute
	nagMax    = 2              // 单条欠回复请求经 NagUnanswered 提醒的次数上限
	nagWindow = 24 * time.Hour // 欠回复提醒窗口，创建超过窗口的不再提醒
)

// outboxRecheck 是 TakeOutbox 长轮询等不到事件时的定时重查间隔（测试里可调小）。
// busy 变成失联（busyStale）是纯时间推移，没有任何事件会叫醒等待者；
// 不定时重查，busy 过期后本可送出的消息会一直干等到 ctx 结束。
var outboxRecheck = 30 * time.Second

// unackedReason 是取走后超时未确认写进 Reason 的提示语。
const unackedReason = "取走后 60 秒未确认送达，重新排队"

// deliveredWhileRejectedReason 是送达成功但消息已被拒绝时写进日志的提示语（不改状态）。
const deliveredWhileRejectedReason = "拒绝时已经送达"

// HubSenderAddr 是 Hub 自己发的系统通知的 From。ValidAddr 不接受这个前缀，
// 所以正常 Send（含远端代发）冒充不了它；它只当发件人，不再被回复、也不参与欠回复超时。
const HubSenderAddr = "xxyi:hub"

const (
	// ackErrFmt 是注入后等不到对方确认写进 Attempt/Reason 的提示语。
	ackErrFmt = "注入后 %s 对方没开始处理（可能额度用尽、报错或卡在确认框）"
	// peerGoneReason 是钩子上报 gone 时把该地址 injected 消息退回 queued 写的 Reason；
	// 对方退出不是投递失败，不记 Attempt、不退避。
	peerGoneReason = "对方已退出"
	// overdueNoticeFmt 是欠回复超时系统通知的正文。
	overdueNoticeFmt = "#%s 发给 %s 的请求超过 %s没有回复（对方状态：%s）。需要的话请换个方式或交给用户。"
	// overdueLogKind 是欠回复超时的日志 kind。
	overdueLogKind = "msg_overdue"
)

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

// humanDuration 把时长写成中文（"2 小时"、"30 分钟"、"45 秒"），给用户看的提示语用。
// 能整除小时写小时，能整除分钟写分钟，其余写秒。
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d 小时", int(d/time.Hour))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	default:
		return fmt.Sprintf("%d 秒", int(d/time.Second))
	}
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
	// StorePath 是落盘文件路径：消息（含附件）、对端状态、自动模式、欠回复计数写进这个文件，
	// NewHub 时读回来，wgtund 重启不丢消息；空 = 不落盘（保持纯内存行为）。
	StorePath string
	// MaxAttempts 是单个阶段（sync/deliver）的自动重试次数预算，<=0 取默认 5；
	// 用完转人工（sync_failed / failed）并生成 FailReport。
	MaxAttempts int
	// RetryWindow 是重试预算的时间窗，<=0 取默认 30 分钟；从首次失败起算，
	// 线路不通的暂停时间不算。窗口先到也转人工。
	RetryWindow time.Duration
	// Capture 抓接收方终端画面（最后 40 行）用于失败报告；可为 nil（报告不带画面）。
	Capture func(ctx context.Context, addr string) (string, error)
	// AckTimeout 是注入成功后等对方新上报（钩子报忙闲都算"我这边动了"）的时限，
	// <=0 取默认 30 秒；超时按 ack 阶段失败重回 queued 走重试预算。
	// 发往 mac: 的 outbox 取走后 unackedTakeTimeout 未确认的重排与此无关，各自独立。
	AckTimeout time.Duration
	// ReplyTimeout 是请求送达后等回复的时限，<=0 取默认 2 小时；
	// 超时把请求标 overdue 并发一条系统通知给原发件人。
	ReplyTimeout time.Duration
	// Sign 用 XXYI CA 私钥给消息签名（入参是 xxyica.MessageSigningBytes 的原文）；
	// nil = 不签名（Message.Sig 为空）。凡经 Send/系统通知入库的消息都入库前签。
	Sign func([]byte) []byte
}

// Hub 是消息中转队列，并发安全；网络与 git 由 cfg 注入。
// cfg.StorePath 非空时把消息/对端状态等落盘（见 msgstore.go），空则纯内存。
type Hub struct {
	mu        sync.Mutex
	cfg       HubConfig
	msgs      []Message            // 按加入顺序（即创建时间先后），File 一直留在msgs 里，取用时给完整内容
	outbox    []string             // 等 mac: 接收方取的消息 ID，按放入顺序；内容以 h.msgs 当前值为准
	outPlaced map[string]bool      // 已放进过 outbox 的消息 ID（同一条只放一次；取走后超时未确认才允许重放）
	inflight  map[string]bool      // 正在 pumpOne 处理（Sync/Deliver）的消息 ID，防并发双发；整个 pumpOne 结束才释放
	injecting map[string]bool      // 正被 Deliver 注入的消息 ID（调前置上、返回即清）；落盘，重启撞上它转人工（见 loadStore）
	takenAt   map[string]time.Time // 出站消息被 TakeOutbox 取走的时间；超时未确认由 pumpOne 重新排队
	nextTry   map[string]time.Time // 失败后的下次可重试时间（指数退避）
	fails     map[string]int       // 连续失败次数，退避用
	// 重试预算按阶段各算各的（key: id+"|"+stage，stage：sync/deliver/ack）：
	// firstFail 是该阶段首次真失败（非线路不通）的时间，pausedAtFirst 是当时已累计的
	// 暂停时长（首败发生在暂停中就再加上当时那段 now-pausedSince）；
	// RetryWindow 只算两者差出来之后的时间。某阶段成功就把该阶段的三个键一起清掉，
	// 别的阶段的预算不受牵连（同步失败过 40 分钟不该压缩送达/确认阶段的窗口）。
	firstFail     map[string]time.Time
	pausedAtFirst map[string]time.Duration
	// stageFails 是本轮预算内的逐阶段失败计数（key: id+"|"+stage）。
	// 不能拿 Attempts 历史数：retry 保留历史但预算要重新给；不落盘，重启视为重新起算。
	stageFails map[string]int
	// 线路暂停：linkDown 时需要同步或经线路送达的消息全部按住，
	// 每 linkProbeEvery 放一条做探测；pausedTotal 累计暂停时长（预算窗要扣除）。
	linkDown    bool
	pausedSince time.Time
	pausedTotal time.Duration
	lastProbe   time.Time // 上一次放探测消息的时间
	peers       map[string]PeerState
	peerAt      map[string]time.Time // 对端状态最近一次变化的时间（busy 失联判定用）
	peerSeq     map[string]uint64    // 对端状态版本号：送达期间有新上报就不覆盖对方状态
	injectedAt  map[string]time.Time // 注入成功、开始等对方确认的时间（AckTimeout 起算点）
	ackSeq      map[string]uint64    // 注入前的对端基准（取自 ackBase）；钩子 busy 上报的编号大于它才算确认
	ackBase     map[string]uint64    // 取走/注入前的对端版本号：Ubuntu 在调 Deliver 前、Mac 在 TakeOutbox 置忙前记下，markInjectedLocked 消费
	// hookBusySeq 是钩子经 SetState 上报 busy 时最近一次的 peerSeq；只有它大于
	// ackSeq 才把 injected 落 delivered。Hub 自己置的 busy（送达/取走/确认超时）
	// 不记这里；idle 上报也不确认（空闲不代表对方接住了消息）。
	hookBusySeq map[string]uint64
	deliveredAt map[string]time.Time // 变成 delivered 的时间（ReplyTimeout 起算点）
	nagged      map[string]int       // 单条欠回复请求已提醒过的次数（NagUnanswered 用）
	autoUntil   time.Time            // 零值表示自动模式关闭
	wakeup      chan struct{}        // outbox 有变化或 SetState 时翻转，唤醒长轮询的 TakeOutbox

	// 落盘（见 msgstore.go）：改动状态的点在锁内置 dirty；persist 只在 dirty 时
	// 锁内分配快照序号并拷快照（nextSnapSeq 只增，序号跟快照一起定，序列化多慢都不改归属），
	// 锁外序列化后 flush 拿 saveMu 原子写，只有序号比上次写盘的大才真写——
	// 后拍的快照不会被先拍晚到的覆盖。
	// 附件不进快照：首次入库写 <StorePath>.files/<id>（0700/0600，原子写），
	// 释放/淘汰时删；hasFile 记账，快照里只留 has_file 标记。
	storePath         string          // cfg.StorePath；空 = 不落盘（loadStore 保不住原文件时也会置空）
	saveMu            sync.Mutex      // 串行化磁盘写
	dirty             bool            // 有改动还没进快照；只在 h.mu 内读写
	nextSnapSeq       uint64          // 下一个要分配的快照序号；拍快照时在 h.mu 内取号（从 1 起），序号与内容同时定
	writtenSeq        uint64          // 最近成功写盘的快照序号；只在 flush 持 saveMu 时读写
	hasFile           map[string]bool // 附件文件已写入磁盘的消息 ID；释放/淘汰时删文件并撤账
	pendingFileDelete []string        // 待删的附件文件 ID（释放/淘汰时攒下，persist 锁外清掉）
}

// NewHub 新建 Hub。cfg.StorePath 非空时读回上次落盘的快照（消息含附件、对端状态、
// 自动模式、欠回复计数；takenAt/outbox/inflight 等瞬时状态一律从空开始，见 msgstore.go）。
func NewHub(cfg HubConfig) *Hub {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.RetryWindow <= 0 {
		cfg.RetryWindow = defaultRetryWindow
	}
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = defaultAckTimeout
	}
	if cfg.ReplyTimeout <= 0 {
		cfg.ReplyTimeout = defaultReplyTimeout
	}
	h := &Hub{
		cfg:           cfg,
		outbox:        nil,
		outPlaced:     make(map[string]bool),
		inflight:      make(map[string]bool),
		injecting:     make(map[string]bool),
		takenAt:       make(map[string]time.Time),
		nextTry:       make(map[string]time.Time),
		fails:         make(map[string]int),
		firstFail:     make(map[string]time.Time),
		pausedAtFirst: make(map[string]time.Duration),
		stageFails:    make(map[string]int),
		peers:         make(map[string]PeerState),
		peerAt:        make(map[string]time.Time),
		peerSeq:       make(map[string]uint64),
		injectedAt:    make(map[string]time.Time),
		ackSeq:        make(map[string]uint64),
		ackBase:       make(map[string]uint64),
		hookBusySeq:   make(map[string]uint64),
		deliveredAt:   make(map[string]time.Time),
		nagged:        make(map[string]int),
		wakeup:        make(chan struct{}),
		storePath:     cfg.StorePath,
		hasFile:       make(map[string]bool),
		nextSnapSeq:   1, // 序号从 1 起：0 留作"从没拍过快照"，flush 只认比 writtenSeq 新的
	}
	if h.storePath != "" {
		h.loadStore()
	}
	return h
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

// markDirtyLocked 标记"有落盘状态改动还没进快照"；每个改了落盘状态（快照里的字段，
// 见 msgstore.go hubSnapshot）的点都要在锁内调它，persist 靠它决定这次碰不碰磁盘。
// 调用方须持锁。
func (h *Hub) markDirtyLocked() { h.dirty = true }

// releaseAttachLocked 释放消息的附件：内存里的 File 置 nil，磁盘上已落盘的附件文件
// 记入待删清单（附件跟着消息一起走，消息结束了文件就不该再留着）。
// 只撤账不直接删——磁盘 IO 必须在锁外，由 persist 排干 pendingFileDelete。
// 调用方须持锁。
func (h *Hub) releaseAttachLocked(m *Message) {
	m.File = nil
	if h.hasFile[m.ID] {
		delete(h.hasFile, m.ID)
		h.pendingFileDelete = append(h.pendingFileDelete, m.ID)
	}
}

// setPeerLocked 更新对端状态并盖时间戳、版本号（版本号每次变化 +1）；调用方须持锁。
func (h *Hub) setPeerLocked(addr string, st PeerState) {
	h.peers[addr] = st
	h.peerAt[addr] = h.now()
	h.peerSeq[addr]++
	h.markDirtyLocked()
}

// stageKey 拼阶段预算的 map 键；stage 取 retryStages 之一。
func stageKey(id, stage string) string { return id + "|" + stage }

// recordAttemptLocked 给消息追加一条失败记录，并记该阶段的预算起点：
// firstFail[id|stage] 是该阶段首次真失败的时间，pausedAtFirst 是当时已累计的暂停时长——
// 若首败正发生在暂停中，把当前这段暂停（now - pausedSince）也算进起点，
// 这样 retryElapsedLocked 照旧"再扣当前暂停段"时，只扣 [首败, now] 内的部分，
// 暂停早于首败的那截不会被冤枉多扣。Attempts 只留最近 maxAttemptsKept 条。调用方须持锁。
func (h *Hub) recordAttemptLocked(id, stage string, err error) {
	i := h.findLocked(id)
	if i < 0 {
		return
	}
	now := h.now()
	cur := &h.msgs[i]
	cur.Attempts = append(cur.Attempts, Attempt{At: now, Stage: stage, Err: err.Error()})
	if len(cur.Attempts) > maxAttemptsKept {
		trimmed := make([]Attempt, maxAttemptsKept)
		copy(trimmed, cur.Attempts[len(cur.Attempts)-maxAttemptsKept:])
		cur.Attempts = trimmed
	}
	key := stageKey(id, stage)
	h.stageFails[key]++
	if _, ok := h.firstFail[key]; !ok {
		h.firstFail[key] = now
		base := h.pausedTotal
		if h.linkDown && !h.pausedSince.IsZero() {
			base += now.Sub(h.pausedSince)
		}
		h.pausedAtFirst[key] = base
	}
	h.markDirtyLocked() // Attempts 历史与阶段预算起点都在快照里
}

// retryElapsedLocked 算该阶段从首次失败起的有效经过时间（key = stageKey(id, stage)）：
// 扣掉首败之后累计的暂停时长，正在暂停中的这一段（now - pausedSince）也扣——
// 首败若在暂停中发生，pausedAtFirst 已把当时那段算进起点，这里不会重复扣。
// 没记过该阶段起点返回 false。调用方须持锁。
func (h *Hub) retryElapsedLocked(key string, now time.Time) (time.Duration, bool) {
	first, ok := h.firstFail[key]
	if !ok {
		return 0, false
	}
	d := now.Sub(first) - (h.pausedTotal - h.pausedAtFirst[key])
	if h.linkDown && !h.pausedSince.IsZero() {
		d -= now.Sub(h.pausedSince)
	}
	if d < 0 {
		d = 0
	}
	return d, true
}

// budgetExhaustedLocked 判断该消息在这一阶段的重试预算是否用完：
// 同阶段失败次数到 MaxAttempts，或该阶段窗口（从该阶段首败起算）到 RetryWindow。
// 调用方须持锁（且在记录本次失败之后）。
func (h *Hub) budgetExhaustedLocked(id, stage string) bool {
	if h.findLocked(id) < 0 {
		return false
	}
	n := h.stageFails[stageKey(id, stage)]
	if n >= h.cfg.MaxAttempts {
		return true
	}
	if el, ok := h.retryElapsedLocked(stageKey(id, stage), h.now()); ok && el >= h.cfg.RetryWindow {
		return true
	}
	return false
}

// enterPauseLocked 进入线路暂停（已在暂停中就不重置 pausedSince）。调用方须持锁。
func (h *Hub) enterPauseLocked() {
	if !h.linkDown {
		h.linkDown = true
		h.pausedSince = h.now()
		h.markDirtyLocked()
	}
}

// recoverLinkLocked 结束线路暂停：把这段暂停时长计入 pausedTotal。
// 探测成功或返回的不是 ErrLinkDown 都算线路回来了。调用方须持锁。
func (h *Hub) recoverLinkLocked() {
	if h.linkDown {
		h.linkDown = false
		if !h.pausedSince.IsZero() {
			h.pausedTotal += h.now().Sub(h.pausedSince)
			h.pausedSince = time.Time{}
		}
		h.markDirtyLocked()
	}
}

// LinkDown 报告 Hub 是否处于线路暂停。
func (h *Hub) LinkDown() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.linkDown
}

// backoffLocked 按连续失败次数记指数退避（base 起翻倍，封顶 backoffMax）。调用方须持锁。
func (h *Hub) backoffLocked(id string) {
	h.fails[id]++
	delay := backoffBase << uint(h.fails[id]-1)
	if delay > backoffMax || delay <= 0 { // 溢出视为封顶
		delay = backoffMax
	}
	h.nextTry[id] = h.now().Add(delay)
}

// clearStageFailsLocked 清掉一条消息所有阶段（sync/deliver/ack）的预算计数
// （retry 重开、或消息结束时回收；ack 预算一并覆盖）。调用方须持锁。
func clearStageFailsLocked(h *Hub, id string) {
	for _, st := range retryStages {
		delete(h.stageFails, stageKey(id, st))
	}
}

// clearStageBudgetLocked 清掉一条消息所有阶段的完整预算：失败计数加预算起点。
// 消息结束（delivered/rejected/淘汰）或 retry/now 重开时用。调用方须持锁。
func clearStageBudgetLocked(h *Hub, id string) {
	for _, st := range retryStages {
		delete(h.stageFails, stageKey(id, st))
		delete(h.firstFail, stageKey(id, st))
		delete(h.pausedAtFirst, stageKey(id, st))
	}
	h.markDirtyLocked() // firstFail/pausedAtFirst 在快照里
}

// clearDoneStageLocked 清掉某个已成功阶段的记录（同步成功/注入成功时）：
// 该阶段之前的失败不再占用它的窗口；别的阶段的预算不受牵连。调用方须持锁。
func clearDoneStageLocked(h *Hub, id, stage string) {
	delete(h.stageFails, stageKey(id, stage))
	delete(h.firstFail, stageKey(id, stage))
	delete(h.pausedAtFirst, stageKey(id, stage))
	h.markDirtyLocked()
}

// resetRetryLocked 清空一条消息的重试预算（retry/now 重开时用）；Attempts 历史保留。
// 调用方须持锁。
func resetRetryLocked(h *Hub, id string, cur *Message) {
	delete(h.nextTry, id)
	delete(h.fails, id)
	delete(h.injectedAt, id)
	delete(h.ackSeq, id)
	delete(h.ackBase, id)
	clearStageBudgetLocked(h, id)
	cur.Report = nil
	h.markDirtyLocked()
}

// reportReq 是转人工时在锁内摘好的报告素材；Screen 必须锁外经 Capture 抓，抓完再 attach。
type reportReq struct {
	id        string
	stage     string
	addr      string
	peerState string
	at        time.Time
}

func newReportReqLocked(h *Hub, id, stage, addr string) *reportReq {
	ps := "unknown"
	if st, ok := h.peers[addr]; ok {
		ps = string(st)
	}
	return &reportReq{id: id, stage: stage, addr: addr, peerState: ps, at: h.now()}
}

// captureScreen 锁外抓接收方终端画面；Capture 没配返回空串，抓失败把原因写进画面文本。
func (h *Hub) captureScreen(ctx context.Context, addr string) string {
	if h.cfg.Capture == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	s, err := h.cfg.Capture(cctx, addr)
	if err != nil {
		return "抓屏失败：" + err.Error()
	}
	return s
}

// attachReport 把抓好的报告挂回消息（消息还在转人工状态才挂，期间被 retry/淘汰就放弃）。
func (h *Hub) attachReport(req *reportReq, screen string) {
	h.mu.Lock()
	if j := h.findLocked(req.id); j >= 0 && (h.msgs[j].State == MsgFailed || h.msgs[j].State == MsgSyncFailed) {
		h.msgs[j].Report = &FailReport{Stage: req.stage, At: req.at, PeerState: req.peerState, Screen: screen}
		h.markDirtyLocked()
	}
	h.mu.Unlock()
	h.persist() // Report 落盘；没挂上（消息已 retry/淘汰）时 dirty 没置起，不会碰磁盘
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
	if j := h.findLocked(reply.ReplyTo); j >= 0 && !h.msgs[j].Answered {
		h.msgs[j].Answered = true
		h.markDirtyLocked()
	}
}

// finishDeliveredLocked 把消息落到 delivered 并做完送达收尾：清退避与重试预算、释放附件、
// 记下送达时间（欠回复超时的起算点），回复则把原请求标成已回复。调用方须持锁。
func (h *Hub) finishDeliveredLocked(i int) {
	cur := &h.msgs[i]
	cur.State = MsgDelivered
	cur.Reason = ""
	h.releaseAttachLocked(cur) // 已结束：附件不再占内存，磁盘上的附件文件也一起清掉
	h.deliveredAt[cur.ID] = h.now()
	// 回复真正送达，这条请求才算真被回了
	h.markAnsweredLocked(*cur)
	delete(h.nextTry, cur.ID)
	delete(h.fails, cur.ID)
	delete(h.injectedAt, cur.ID)
	delete(h.ackSeq, cur.ID)
	delete(h.ackBase, cur.ID)
	clearStageBudgetLocked(h, cur.ID) // 确认成功：所有阶段的预算到此为止
}

// markInjectedLocked 把刚注入成功的非 Now 消息记成 injected：记下注入时间和
// "注入前的对端基准"（ackBase：Ubuntu 在调 Deliver 前、Mac 在 TakeOutbox 置忙前
// 记下的版本号；缺兜底取当前 peerSeq）。之后该对端钩子 busy 上报的编号大于它才算
// 注入期间或之后对方真的动过——注入前记基准，注入过程中到的 busy 也算确认。
// 注入成功不等于对方开始处理：附件、ack 阶段预算留着，等真送达才收尾。调用方须持锁。
func (h *Hub) markInjectedLocked(i int) {
	cur := &h.msgs[i]
	cur.State = MsgInjected
	cur.Reason = ""
	h.injectedAt[cur.ID] = h.now()
	base, ok := h.ackBase[cur.ID]
	if !ok {
		base = h.peerSeq[cur.To] // 没走 TakeOutbox/Deliver 记点的调用路径：退回当前版本号
	}
	delete(h.ackBase, cur.ID)
	h.ackSeq[cur.ID] = base
	// 注入本身成功了：同步/送达阶段的退避作废（ack 阶段的预算另算）。
	delete(h.nextTry, cur.ID)
	delete(h.fails, cur.ID)
	clearDoneStageLocked(h, cur.ID, "sync")
	clearDoneStageLocked(h, cur.ID, "deliver")
	h.markDirtyLocked() // state=injected、injectedAt/ackSeq 都在快照里
	if h.hookBusySeq[cur.To] > h.ackSeq[cur.ID] {
		// 注入前/注入期间钩子已报过忙：对方收到了，不必再等下一次上报。
		h.finishDeliveredLocked(i)
	}
}

// checkInjectedLocked 推进 injected 消息：钩子 busy 上报的编号（hookBusySeq）超过
// 注入前记下的 ackSeq 就落 delivered——只有钩子报忙才算对方接住了并开始处理，
// idle 不算（空闲不代表收过）、Hub 自己置的 busy 不算。
// 注入后超过 AckTimeout 仍没有钩子 busy，就记一次 ack 阶段失败、
// 重回 queued 走重试预算（用完转 failed，报告 Stage=ack）。
// onlyAddr 非空时只看该对端（SetState 用：刚上报的那个地址）；
// 空则扫全部对端（Pump 用：查超时）。
// 返回要记日志的消息拷贝，和要在锁外抓屏转人工的报告请求。调用方须持锁。
func (h *Hub) checkInjectedLocked(now time.Time, onlyAddr string) (logged []Message, reps []*reportReq) {
	var timedOut []string
	for i := range h.msgs {
		m := &h.msgs[i]
		if m.State != MsgInjected {
			continue
		}
		if onlyAddr != "" && m.To != onlyAddr {
			continue
		}
		if h.hookBusySeq[m.To] > h.ackSeq[m.ID] {
			// 注入基准之后钩子报过忙：说明消息真被接住了，开始处理
			h.finishDeliveredLocked(i)
			logged = append(logged, *m)
			continue
		}
		if now.Sub(h.injectedAt[m.ID]) <= h.cfg.AckTimeout {
			continue
		}
		err := fmt.Errorf(ackErrFmt, humanDuration(h.cfg.AckTimeout))
		m.State = MsgQueued
		m.Reason = err.Error()
		delete(h.injectedAt, m.ID)
		delete(h.ackSeq, m.ID)
		h.recordAttemptLocked(m.ID, "ack", err)
		if h.budgetExhaustedLocked(m.ID, "ack") {
			m.State = MsgFailed
			delete(h.nextTry, m.ID)
			delete(h.fails, m.ID)
			reps = append(reps, newReportReqLocked(h, m.ID, "ack", m.To))
		} else {
			h.backoffLocked(m.ID) // 留在 queued，退避后重新注入
		}
		timedOut = append(timedOut, m.To)
		logged = append(logged, *m)
	}
	// 超时那条把对端标成不可送（busy 且时间戳重置），等它再上报或熬过 busyStale 才试下一次。
	// 这一步必须等上面全判完：置 busy 会让版本号 +1，先 bump 会把同批别的
	// injected 消息误判成"对方已经接收"。
	for _, to := range timedOut {
		h.setPeerLocked(to, PeerBusy)
	}
	return logged, reps
}

// newSystemNotice 组装一条 Hub 系统通知（From 固定 HubSenderAddr、queued、未签名）。
// 纯组装不加锁：ID 在持锁时查重后预留好传进来，签名在锁外算，签完再加锁入库。
// 不走 Send：ValidAddr 不接受 xxyi: 前缀，而系统通知只当发件人；也不需要对方回复。
func newSystemNotice(now time.Time, id, to, text string) Message {
	return Message{
		ID:      id,
		From:    HubSenderAddr,
		To:      to,
		Text:    sanitizeText(text),
		Kind:    KindNotice,
		State:   MsgQueued,
		Created: now,
	}
}

// reserveIDLocked 预留一个不与现有消息冲突的 ID。调用方须持锁。
func (h *Hub) reserveIDLocked() string {
	for {
		id := newID()
		if h.findLocked(id) < 0 {
			return id
		}
	}
}

// markOverdueReplies 扫已送达的请求：没回复（Answered 和"已经发出去、没被拒绝的回复"
// 都没有）且从变成 delivered 起算超过 ReplyTimeout，就标 overdue（只标一次）、
// 记 msg_overdue 日志，并给原发件人发一条系统通知。
// 系统通知本身是 notice，不参与欠回复判定，也不会再触发超时。
func (h *Hub) markOverdueReplies() {
	h.mu.Lock()
	now := h.now()
	var overdue, drafts []Message
	var logs []map[string]string
	for i := range h.msgs {
		m := &h.msgs[i]
		if m.State != MsgDelivered || m.Kind != KindRequest || m.Overdue || m.Answered {
			continue
		}
		at, ok := h.deliveredAt[m.ID]
		if !ok || now.Sub(at) <= h.cfg.ReplyTimeout {
			continue
		}
		if _, live := h.hasLiveReplyLocked(m.ID, m.To); live {
			continue // 回复已经发出去了（哪怕还没送到），不算超时
		}
		m.Overdue = true // 在锁内标记：并发两路（Pump/CheckTimers）只会有一路拿到这条超时
		h.markDirtyLocked()
		ps := "unknown"
		if st, ok := h.peers[m.To]; ok {
			ps = string(st)
		}
		text := fmt.Sprintf(overdueNoticeFmt, m.ID, m.To, humanDuration(h.cfg.ReplyTimeout), ps)
		// ID 锁内预留，签名留到锁外算（不允许持 h.mu 调 Sign）
		draft := newSystemNotice(now, h.reserveIDLocked(), m.From, text)
		drafts = append(drafts, draft)
		overdue = append(overdue, *m)
		logs = append(logs, map[string]string{
			"id":           m.ID,
			"from":         m.From,
			"to":           m.To,
			"peer_state":   ps,
			"delivered_at": at.Format(time.RFC3339),
		})
	}
	wake := h.trimLocked()
	h.mu.Unlock()

	// 锁外逐条签名，签完加锁入库（ID 已在锁内预留，不会撞）。
	var notices []Message
	for _, n := range drafts {
		if h.cfg.Sign != nil {
			n.Sig = h.cfg.Sign(xxyica.MessageSigningBytes(msgSignFields(n)))
		}
		h.mu.Lock()
		h.msgs = append(h.msgs, n)
		h.markDirtyLocked()
		h.mu.Unlock()
		notices = append(notices, n)
	}

	if h.cfg.Log != nil { // 日志回调放锁外：不让外部代码在持 Hub 锁时运行
		for _, d := range logs {
			h.cfg.Log(overdueLogKind, d)
		}
	}

	if wake != nil {
		close(wake)
	}
	for _, n := range notices {
		h.logMsg(n)
	}
	if len(overdue) > 0 {
		h.persist() // overdue 标记和系统通知落盘
	}
}

// hasLiveReplyLocked 查有没有人（from）已对 reqID 发出过还没被拒绝的回复
// （pending/queued/delivered/sync_failed 都算"已经回复过了"，只有 rejected 不算——
// 被拒绝的回复等于没回，之后还可以再回复）。调用方须持锁。
func (h *Hub) hasLiveReplyLocked(reqID, from string) (string, bool) {
	for _, m := range h.msgs {
		if m.Kind == KindReply && m.ReplyTo == reqID && m.From == from && m.State != MsgRejected {
			return m.ID, true
		}
	}
	return "", false
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
		delete(h.injecting, gone.ID)
		delete(h.takenAt, gone.ID)
		delete(h.nextTry, gone.ID)
		delete(h.fails, gone.ID)
		clearStageBudgetLocked(h, gone.ID)
		delete(h.nagged, gone.ID)
		delete(h.injectedAt, gone.ID)
		delete(h.ackSeq, gone.ID)
		delete(h.ackBase, gone.ID)
		delete(h.deliveredAt, gone.ID)
		h.releaseAttachLocked(&gone) // 被淘汰：附件文件也跟着清（正常早已释放，防漏网）
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
	// client_id 查重放在最前（任何校验之前，包括 reply 的「已经回复过了」拦截）：
	// 同 From 同 ClientID 重发一律直接返回原 ID——重发的那次哪怕字段填得不合规，
	// 也早就在第一次入库时被幂等吸收，不该再报别的错。
	if m.ClientID != "" {
		h.mu.Lock()
		for i := range h.msgs {
			if h.msgs[i].From == m.From && h.msgs[i].ClientID == m.ClientID {
				dupID := h.msgs[i].ID
				h.mu.Unlock()
				return dupID, nil
			}
		}
		h.mu.Unlock()
	}
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
	var dupReplyID string // 同一发件人对同一请求已有的没被拒绝的回复
	if m.Kind == KindReply {
		if m.ReplyTo == "" {
			return "", fmt.Errorf("support: 回复必须给 reply_to")
		}
		h.mu.Lock()
		i := h.findLocked(m.ReplyTo)
		if i >= 0 {
			orig, haveOrig = h.msgs[i], true
		}
		dupReplyID, _ = h.hasLiveReplyLocked(m.ReplyTo, m.From)
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
		// 同一个人对同一条请求只能有一个没被拒绝的回复：重复回复拦下；
		// 上一条被拒绝后 dupReplyID 为空，可以再回复。
		if dupReplyID != "" {
			return "", fmt.Errorf("support: #%s 已经回复过了（#%s）", m.ReplyTo, dupReplyID)
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
	if m.ClientID != "" {
		if len(m.ClientID) > maxClientIDLen {
			return "", fmt.Errorf("support: client_id 超过 %d 字节（%d 字节）", maxClientIDLen, len(m.ClientID))
		}
		if bad := addrNameIllegal.FindString(m.ClientID); bad != "" {
			return "", fmt.Errorf("support: client_id 包含非法字符 %q，只允许 ASCII 字母数字和 . _ -", bad)
		}
	}

	now := h.now()
	m.Created = now
	m.Reason = ""
	m.Now = false
	m.Synced = false
	// 附件哈希在这里算好进签名域并永久保留：附件送到/释放后仍能核验"当时那个附件"没被换。
	if len(m.File) > 0 {
		sum := sha256.Sum256(m.File)
		m.FileSHA256 = hex.EncodeToString(sum[:])
	}

	// 先预留 ID、附件先写盘、消息后进 h.msgs：文件在消息可见之前就已存在，
	// 中途崩溃只会留下孤儿附件（loadStore 清理），不会出现快照说有附件而文件不在。
	h.mu.Lock()
	open := 0
	for i := range h.msgs {
		switch h.msgs[i].State {
		case MsgPending, MsgQueued, MsgSyncFailed, MsgFailed:
			open++
		}
	}
	if open >= maxMsgOpen {
		h.mu.Unlock()
		return "", fmt.Errorf("support: 未处理的消息太多（%d 条），先处理再发", maxMsgOpen)
	}
	m.ID = h.reserveIDLocked() // 锁内预留，附件按这个 ID 先落盘
	h.mu.Unlock()

	// 附件先落盘再入库：写一次 <StorePath>.files/<id>（0700/0600，原子写）。
	// 写失败只记日志：消息照常入库、附件留在内存照常投递，只是重启后回不来
	//（拿 FileSHA256 能查出来）。
	hasFile := false
	if len(m.File) > 0 && h.storePath != "" {
		if err := h.writeAttachFile(m.ID, m.File); err != nil {
			h.logStoreError("attach_write", err)
		} else {
			hasFile = true
		}
	}

	h.mu.Lock()
	// ClientID 幂等复查（与入库同一把锁，防并发双发穿过最前面的预检）：
	// 同 From 同 ClientID 已经发过就不再新建，把原 ID 原样还回去
	//（客户端超时重发不会刷出两条一样的消息）；刚写到盘的附件删掉别成孤儿。
	if m.ClientID != "" {
		for i := range h.msgs {
			if h.msgs[i].From == m.From && h.msgs[i].ClientID == m.ClientID {
				dupID := h.msgs[i].ID
				h.mu.Unlock()
				if hasFile {
					h.removeAttachFile(m.ID)
				}
				return dupID, nil
			}
		}
	}
	open = 0 // 写附件期间可能有新消息入库，名额重新数一遍
	for i := range h.msgs {
		switch h.msgs[i].State {
		case MsgPending, MsgQueued, MsgSyncFailed, MsgFailed:
			open++
		}
	}
	if open >= maxMsgOpen {
		h.mu.Unlock()
		if hasFile {
			h.removeAttachFile(m.ID)
		}
		return "", fmt.Errorf("support: 未处理的消息太多（%d 条），先处理再发", maxMsgOpen)
	}
	// AskUser 的消息即使自动模式开着也要停下来等用户：标了"要用户定"就不能自动放行。
	if !h.autoUntil.IsZero() && now.Before(h.autoUntil) && !m.AskUser {
		m.State = MsgQueued
	} else {
		m.State = MsgPending
	}
	h.msgs = append(h.msgs, m) // 附件文件已在盘上才入库；先以未签名入库，Sign 在锁外算，签完写回
	if hasFile {
		h.hasFile[m.ID] = true // 快照里 has_file=true 时文件一定已经在
	}
	// 这里不把被回复的请求标成 Answered：回复只有真正送达（pumpOne 成功 / MarkDelivered）
	// 才算回了；发出后被拒/一直没送，原请求仍算欠回复。
	h.markDirtyLocked()
	wake := h.trimLocked()
	h.mu.Unlock()

	// 签名挪到锁外：Sign（CA 私钥，可能是外部实现）不许持 h.mu 跑。
	// 签名域含 ID，入库后不再改；期间消息若已被 reject/淘汰，写回自动跳过。
	if h.cfg.Sign != nil {
		sig := h.cfg.Sign(xxyica.MessageSigningBytes(msgSignFields(m)))
		m.Sig = sig
		h.mu.Lock()
		if j := h.findLocked(m.ID); j >= 0 {
			h.msgs[j].Sig = sig
			h.markDirtyLocked() // Sig 进快照；写回了就得落盘（下面/后续的 persist 靠它碰磁盘）
		}
		h.mu.Unlock()
	}

	if wake != nil {
		close(wake) // 淘汰顺带清了 outbox 条目，叫醒等待中的 TakeOutbox
	}
	h.logMsg(m)
	h.persist() // 新消息入库连同 trim 淘汰一起落盘（附件已单独写成文件，快照只记 has_file）
	return m.ID, nil
}

// SetState 记录对端忙闲状态（busy/idle/gone）；gone 表示对方窗口里此刻不是 Claude。
// 没上报过的地址不在表里，与 busy/gone 一样按住非 Now 消息。
// 每次调用都会翻转 wakeup：等待中的 TakeOutbox 要能在对方变 idle 时醒来重查。
// 送达确认只认 busy：上报 busy 记下钩子版本号（hookBusySeq），发给这个地址、
// 注入基准更早的 injected 消息在这里落 delivered（附件释放、回复标 Answered）；
// idle 不算确认（空闲不代表接住过消息）。
// 上报 gone：发给这个地址、正等确认的 injected 消息全部退回 queued
// （Reason「对方已退出」）——对方窗口关了消息多半没被处理；退回不算失败，
// 不记 Attempt、不退避，等对方重新上报 idle 再走正常送达。
func (h *Hub) SetState(addr string, st PeerState) error {
	if err := ValidAddr(addr); err != nil {
		return err
	}
	if st != PeerBusy && st != PeerIdle && st != PeerGone {
		return fmt.Errorf("support: 未知对端状态 %q", st)
	}
	h.mu.Lock()
	h.setPeerLocked(addr, st)
	if st == PeerBusy {
		// 只有钩子报的忙算确认证据；Hub 自己置 busy 的几处不经这里
		h.hookBusySeq[addr] = h.peerSeq[addr]
	}
	var evicted []Message
	if st == PeerGone {
		for i := range h.msgs {
			m := &h.msgs[i]
			if m.State == MsgInjected && m.To == addr {
				m.State = MsgQueued
				m.Reason = peerGoneReason
				delete(h.injectedAt, m.ID)
				delete(h.ackSeq, m.ID)
				delete(h.ackBase, m.ID)
				delete(h.injecting, m.ID) // 注入半途对端退出：标记别再留到重启
				// mac 侧已取走过：清掉痕迹让下一轮 Pump 能重新放进 outbox
				delete(h.outPlaced, m.ID)
				delete(h.takenAt, m.ID)
				h.markDirtyLocked() // 附件没释放：退回 queued 还要靠它重投
				evicted = append(evicted, *m)
			}
		}
	}
	acked, _ := h.checkInjectedLocked(h.now(), addr)
	wake := h.flipWakeupLocked()
	h.mu.Unlock()
	close(wake)
	for _, m := range acked {
		h.logMsg(m)
	}
	for _, m := range evicted {
		h.logMsg(m)
	}
	h.persist() // 对端状态、peerAt 以及落 delivered/退回 queued 的消息一起落盘
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
// Hub 自己发的系统通知（From == HubSenderAddr）头部不写"来自 xxyi:hub"，直接标系统通知。
func Display(m Message, attachPath string) string {
	var head string
	if m.From == HubSenderAddr {
		head = fmt.Sprintf("[XXYI 系统通知 #%s]", m.ID)
	} else {
		head = fmt.Sprintf("[XXYI 消息 #%s · ", m.ID)
		if m.Kind == KindReply {
			head += fmt.Sprintf("回复 #%s", m.ReplyTo)
		} else {
			head += kindLabel(m.Kind)
		}
		head += fmt.Sprintf(" · 来自 %s]", m.From)
	}

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
// "已回复"看两条：Answered（有回复已送达），或有一条 addr 发出、还没被拒绝的回复
// （pending/queued/delivered/sync_failed 都算）——回复发出去了就不该再催，
// 哪怕还没送到发件人手里；被拒绝的回复不算回复，照旧催。
// 返回的是拷贝，不含 File。
func (h *Hub) Unanswered(addr string) []Message {
	h.mu.Lock()
	var out []Message
	for _, m := range h.msgs {
		if m.To == addr && m.State == MsgDelivered && m.Kind == KindRequest && !m.Answered {
			if _, live := h.hasLiveReplyLocked(m.ID, addr); live {
				continue
			}
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
	now := h.now()
	var out []Message
	for _, m := range un {
		if h.nagged[m.ID] >= nagMax || now.Sub(m.Created) > nagWindow {
			continue
		}
		h.nagged[m.ID]++
		out = append(out, m)
	}
	h.mu.Unlock()
	if len(out) > 0 {
		h.persist() // nagged 计数涨了，落盘
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

// Decide 对一条消息做决定。action：approve / now / reject / retry / retry_sync / skip_sync。
// retry（retry_sync 是其别名，保留兼容）把 failed/sync_failed 重新排队并清空重试预算
// （Attempts 历史保留、Report 清掉）；now 对 failed 也可用，等于 retry + 立即送达。
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
		target, ok = MsgQueued, m.State == MsgPending || m.State == MsgQueued || m.State == MsgFailed
	case "reject":
		target, ok = MsgRejected, m.State == MsgPending || m.State == MsgQueued || m.State == MsgSyncFailed || m.State == MsgFailed
	case "retry", "retry_sync":
		target, ok = MsgQueued, m.State == MsgSyncFailed || m.State == MsgFailed
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
		if m.State == MsgFailed || m.State == MsgSyncFailed {
			resetRetryLocked(h, id, cur) // now 对已转人工的消息等于 retry + 立即送达
		}
	case "retry", "retry_sync":
		cur.Reason = ""
		resetRetryLocked(h, id, cur)
	case "skip_sync":
		cur.Synced = true
	case "reject":
		// 已放进 outbox 未取走的要移掉；移掉也可能改变等待者下一轮该拿什么
		if h.removeFromOutboxLocked(id) {
			wake = h.flipWakeupLocked()
		}
		// 已结束：清退避/预算/取走跟踪/注入标记，附件连同磁盘文件一起释放
		delete(h.takenAt, id)
		delete(h.nextTry, id)
		delete(h.fails, id)
		clearStageBudgetLocked(h, id)
		delete(h.injectedAt, id)
		delete(h.ackSeq, id)
		delete(h.ackBase, id)
		delete(h.deliveredAt, id)
		delete(h.injecting, id)
		h.releaseAttachLocked(cur)
	}
	h.markDirtyLocked()
	out := *cur
	h.mu.Unlock()

	if wake != nil {
		close(wake)
	}
	h.logMsg(out)
	h.persist() // 决定后的状态/Now/Synced/附件释放（删附件文件）落盘
	return nil
}

// Redirect 把一条消息改送给别人：按原消息内容（Kind/正文/附件/仓库/地址侧校验都走 Send）
// 复制一条新消息发给 to，原消息 rejected、Reason 写「已改送 #<新id>」。
// 原消息必须是 pending/queued/failed/sync_failed 之一。返回新消息 ID。
func (h *Hub) Redirect(id, to string) (string, error) {
	h.mu.Lock()
	i := h.findLocked(id)
	if i < 0 {
		h.mu.Unlock()
		return "", ErrMsgNotFound
	}
	m := h.msgs[i]
	switch m.State {
	case MsgPending, MsgQueued, MsgFailed, MsgSyncFailed:
	default:
		h.mu.Unlock()
		return "", fmt.Errorf("support: 消息 %s 当前状态 %s，不能改送", id, m.State)
	}
	h.mu.Unlock()

	// 校验和入库完全交给 Send：新消息按正常消息重新过一遍规则（含自动模式判定）。
	newID, err := h.Send(Message{
		From:     m.From,
		To:       to,
		Text:     m.Text,
		FileName: m.FileName,
		File:     m.File,
		Repo:     m.Repo,
		Branch:   m.Branch,
		Kind:     m.Kind,
		ReplyTo:  m.ReplyTo,
		AskUser:  m.AskUser,
	})
	if err != nil {
		return "", err
	}

	h.mu.Lock()
	var (
		wake chan struct{}
		out  *Message
	)
	if j := h.findLocked(id); j >= 0 {
		cur := &h.msgs[j]
		cur.State = MsgRejected
		cur.Reason = fmt.Sprintf(redirectReasonFmt, newID)
		if h.removeFromOutboxLocked(id) {
			wake = h.flipWakeupLocked()
		}
		delete(h.takenAt, id)
		delete(h.nextTry, id)
		delete(h.fails, id)
		clearStageBudgetLocked(h, id)
		delete(h.injectedAt, id)
		delete(h.ackSeq, id)
		delete(h.ackBase, id)
		delete(h.deliveredAt, id)
		delete(h.injecting, id)
		h.releaseAttachLocked(cur) // 原消息结束：它的附件文件删掉（新消息那份由 Send 落）
		h.markDirtyLocked()
		cp := *cur
		out = &cp
	}
	h.mu.Unlock()

	if wake != nil {
		close(wake)
	}
	if out != nil {
		h.logMsg(*out)
	}
	h.persist() // 原消息 rejected（含改送原因、删原附件文件）落盘
	return newID, nil
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
	h.markDirtyLocked()
	h.mu.Unlock()
	h.persist() // 自动模式开关落盘
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
// 等不到事件时每隔 outboxRecheck 自己重查一次（busy 熬过 busyStale 没有事件通知）。
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
			h.ackBase[id] = h.peerSeq[to] // 置忙**之前**的版本号：MarkDelivered 后确认基准
			h.setPeerLocked(to, PeerBusy) // 取走即算送出，置忙避免连发
			h.mu.Unlock()
			h.persist() // 对端置 busy 落盘（takenAt/ackBase 不存，重启后由重放/确认收敛）
			return out, true
		}
		wake := h.wakeup
		h.mu.Unlock()
		select {
		case <-wake:
		case <-time.After(outboxRecheck):
			// 定时重查：busy 熬过 busyStale 变成失联只是时间推移，
			// 不会有 SetState 之类的事件叫醒这里，不重查就只能干等。
		case <-ctx.Done():
			return Message{}, false
		}
	}
}

// MarkDelivered 确认 outbox 里被取走的消息已注入终端。
// Now 消息（用户强送，对方可能是 shell）到此就算送达；非 Now 只进 injected，
// 等接收方下一次上报忙闲才算真送达——注入成功不代表对方的 Claude 接住了它。
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
	delete(h.takenAt, id)
	delete(h.injecting, id) // mac 侧的注入由取件端完成，这里的确认即收尾，不留注入标记
	if cur.Now {
		h.finishDeliveredLocked(i)
	} else {
		h.markInjectedLocked(i) // 附件等确认后才释放
	}
	out := *cur
	h.mu.Unlock()

	h.logMsg(out)
	h.persist() // delivered 状态（和原请求 Answered）落盘
	return nil
}

// Pump 推进一轮消息：先处理正等送达确认（injected）的消息和欠回复超时，
// 再按创建时间先后尝试推进所有 queued 消息。
// 规则：接收方忙且不是 Now 则跳过；要先同步且没同步过则先调 Sync；
// 发往 mac: 的进 outbox 等 TakeOutbox，其余调 Deliver；
// Deliver 成功或消息被取走后把接收方置忙。Sync/Deliver 在锁外调用。
// 注入成功的非 Now 消息只进 injected，等对端下一次上报才落 delivered；
// 超过 AckTimeout 没上报按 ack 阶段失败重回 queued（SetState 里也会推进）。
// 线路暂停（LinkDown）时碰线路的消息按住不推进，每 linkProbeEvery 放一条做探测；
// 发往 mac: 且不用同步的消息照常。真失败按预算（MaxAttempts/RetryWindow）自动重试，
// 用完转 failed（送达/确认）/ sync_failed（同步）并生成 FailReport。
func (h *Hub) Pump(ctx context.Context) {
	h.mu.Lock()
	advanced, reps := h.checkInjectedLocked(h.now(), "")
	var queue []Message
	for _, m := range h.msgs {
		if m.State == MsgQueued {
			queue = append(queue, m)
		}
	}
	h.mu.Unlock()
	for _, m := range advanced {
		h.logMsg(m) // injected → delivered，或确认超时回 queued/failed
	}
	if len(advanced) > 0 {
		h.persist()
	}
	for _, rep := range reps {
		h.attachReport(rep, h.captureScreen(ctx, rep.addr)) // 抓屏在锁外
	}

	sort.SliceStable(queue, func(i, j int) bool {
		return queue[i].Created.Before(queue[j].Created)
	})

	for _, m := range queue {
		if ctx.Err() != nil {
			return
		}
		h.pumpOne(ctx, m.ID)
	}
	h.markOverdueReplies() // 送达后一直没人回复的请求：标 overdue 并通知原发件人
}

// CheckTimers 只推进"靠时钟到期"的消息：injected 的确认超时（ack 阶段）与请求
// 欠回复超时（overdue），不碰 queued、不做注入。Pump 每轮也做这些；但 Pump 可能
// 被慢 Sync/Deliver 拖住一整轮，导致 ack 超时迟判——调用方可用独立循环定期调它，
// 保证时间判定不受慢投递牵连。转人工的抓屏在锁外做。
func (h *Hub) CheckTimers() {
	h.mu.Lock()
	advanced, reps := h.checkInjectedLocked(h.now(), "")
	h.mu.Unlock()
	for _, m := range advanced {
		h.logMsg(m) // injected → delivered，或确认超时回 queued/failed
	}
	if len(advanced) > 0 {
		h.persist()
	}
	ctx := context.Background()
	for _, rep := range reps {
		h.attachReport(rep, h.captureScreen(ctx, rep.addr)) // 抓屏在锁外
	}
	h.markOverdueReplies()
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
		h.markDirtyLocked() // Reason 进快照
		cp := h.msgs[i]
		reclaimed = &cp
		m = cp
	}
	// 每个早退分支：这一路只有 reclaim 改过落盘状态（Reason），没 reclaim 就什么都没改，
	// 不调 persist——是否碰磁盘由 dirty 决定，不再序列化比字节。
	if !m.Now && !h.peerReadyLocked(m.To) {
		h.mu.Unlock()
		if reclaimed != nil {
			h.logMsg(*reclaimed)
			h.persist() // reclaim 写的 Reason 落盘
		}
		return
	}
	if next, ok := h.nextTry[id]; ok && h.now().Before(next) {
		h.mu.Unlock() // 失败退避中，Now 也要遵守
		if reclaimed != nil {
			h.logMsg(*reclaimed)
			h.persist()
		}
		return
	}
	needSync := m.Repo != "" && !m.Synced
	macDest := strings.HasPrefix(m.To, "mac:")
	// 线路暂停：要先同步的、以及要经 Deliver 送出去的都得等线路；
	// 每 linkProbeEvery 放一条做探测（lastProbe 当场扣掉，同锁内并发也只放过一条）。
	// 发往 mac: 且不用同步的走 outbox 等本机来取，不碰线路，照常推进。
	if h.linkDown && (needSync || !macDest) {
		now := h.now()
		if now.Sub(h.lastProbe) < linkProbeEvery {
			h.mu.Unlock()
			if reclaimed != nil {
				h.logMsg(*reclaimed)
				h.persist()
			}
			return
		}
		h.lastProbe = now // lastProbe 不进快照，不用置 dirty
	}
	if h.inflight[id] {
		h.mu.Unlock()
		if reclaimed != nil {
			h.logMsg(*reclaimed)
			h.persist()
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
		var rep *reportReq
		if ctx.Err() != nil {
			// 取消不是失败：状态、Reason、Synced 都不动；
			// inflight 不进快照，这里没改任何落盘状态，末尾 persist 空转不写盘
			delete(h.inflight, id)
		} else {
			if errors.Is(err, ErrLinkDown) {
				// 线路不通不算同步失败：不记 Attempt 不计次数，Hub 整体暂停等探测恢复
				h.enterPauseLocked()
			} else {
				h.recoverLinkLocked()
			}
			if j := h.findLocked(id); j >= 0 && h.msgs[j].State == MsgQueued {
				if err != nil {
					if errors.Is(err, ErrLinkDown) {
						h.msgs[j].Reason = linkDownReason
						h.markDirtyLocked() // 已在暂停中时 enterPauseLocked 不再置 dirty，这里补
					} else {
						h.msgs[j].Reason = err.Error()
						h.recordAttemptLocked(id, "sync", err)
						if h.budgetExhaustedLocked(id, "sync") {
							// 预算用完：转人工，离开 queued，退避作废；抓屏在锁外做
							h.msgs[j].State = MsgSyncFailed
							delete(h.nextTry, id)
							delete(h.fails, id)
							rep = newReportReqLocked(h, id, "sync", m.To)
						} else {
							h.backoffLocked(id) // 留在 queued，退避后自动重试
						}
					}
					cp := h.msgs[j]
					out = &cp
				} else {
					h.msgs[j].Synced = true
					// 同步成功：sync 阶段的预算记录作废（后面阶段各算各的窗口）
					clearDoneStageLocked(h, id, "sync")
					continueSend = true // 保持 inflight，送达/进 outbox 后再释放
				}
			}
		}
		if !continueSend {
			delete(h.inflight, id)
		}
		h.mu.Unlock()
		if out != nil {
			h.logMsg(*out)
		}
		h.persist() // 同步结果（含暂停/预算状态）落盘；没置 dirty 就不碰磁盘
		if rep != nil {
			h.attachReport(rep, h.captureScreen(ctx, rep.addr))
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
				h.persist()
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
		// 进 outbox 不算状态变化，也不置忙（被取走时才置），不写日志；
		// outbox/outPlaced 不进快照，这一段没改任何落盘状态，不调 persist。
		return
	}

	// 记下调用 Deliver 前的对端状态版本号：
	// ① 送达期间对端若有新上报（比如钩子报了 idle/busy），回来后不能拿 busy/gone 覆盖掉；
	// ② 同时作为 ack 基准（ackBase）：注入过程中到的钩子 busy 也是注入之后发生的，
	// 注入成功时（markInjectedLocked）拿它比 hookBusySeq，已经忙过就直接落 delivered。
	// ③ injecting：注入进行中的标记先落盘再调 Deliver——进程若在注入中途重启，
	// 加载时撞上它就知道"可能已经送进去了"，转人工不盲目重投（见 loadStore）。
	h.mu.Lock()
	seq0 := h.peerSeq[m.To]
	h.ackBase[id] = seq0
	h.injecting[id] = true
	h.markDirtyLocked()
	h.mu.Unlock()

	// 注入前标记必须真正同步写进盘里才能调 Deliver——只在内存里置起 injecting，
	// 注入中途崩溃就永远不会出现在快照里，重启后会当成"从没投过"重投第二条。
	// 写盘失败则撤销标记、本轮不投递：不记 Attempt、不计次数，按普通退避下次再试。
	if err := h.persistNow(); err != nil {
		h.mu.Lock()
		delete(h.injecting, id)
		delete(h.ackBase, id)
		var out *Message
		if j := h.findLocked(id); j >= 0 && h.msgs[j].State == MsgQueued {
			h.msgs[j].Reason = fmt.Sprintf("落盘失败，暂不投递：%v", err)
			h.backoffLocked(id) // 落盘失败不算投递失败：不记 Attempt，只按普通退避
			cp := h.msgs[j]
			out = &cp
		}
		delete(h.inflight, id)
		h.mu.Unlock()
		if out != nil {
			h.logMsg(*out)
		}
		return // persistNow 已把写盘失败记进日志；dirty 还在，下轮 persist 把撤销后的状态写回去
	}

	var err error
	if h.cfg.Deliver == nil {
		err = errors.New("未配置 Deliver，无法送达")
	} else {
		err = h.cfg.Deliver(ctx, m)
	}
	h.mu.Lock()
	var (
		out *Message
		rep *reportReq
	)
	if ctx.Err() != nil {
		// 取消不是失败：状态、Reason 都不动
	} else {
		if errors.Is(err, ErrLinkDown) {
			h.enterPauseLocked()
		} else {
			h.recoverLinkLocked()
		}
	}
	if j := h.findLocked(id); j >= 0 {
		if h.msgs[j].State == MsgQueued {
			if errors.Is(err, ErrPeerNotClaude) {
				// 对面不是 Claude：记 gone 把后续消息整体按住。这不算投递失败——
				// 不累计失败次数、不设退避，对方重新上报 idle 后下一轮 Pump 立刻重试。
				// 与送达成功置 busy 同一条规矩：送达期间对端有新上报（版本号变了）
				// 就不动状态，钩子刚报的 idle 不能被 gone 覆盖。
				if h.peerSeq[m.To] == seq0 {
					h.setPeerLocked(m.To, PeerGone)
				}
				h.msgs[j].Reason = err.Error()
				h.markDirtyLocked() // 对端在 Deliver 期间上报过状态时 setPeerLocked 不会走，这里补
			} else if err != nil {
				if errors.Is(err, ErrLinkDown) {
					// 线路不通不算投递失败：不记 Attempt 不计次数，暂停等探测恢复
					h.msgs[j].Reason = linkDownReason
					h.markDirtyLocked() // 已在暂停中时 enterPauseLocked 不再置 dirty，这里补
				} else {
					h.msgs[j].Reason = err.Error()
					h.recordAttemptLocked(id, "deliver", err)
					if h.budgetExhaustedLocked(id, "deliver") {
						// 预算用完：转人工 failed，退避作废；抓屏在锁外做
						h.msgs[j].State = MsgFailed
						delete(h.nextTry, id)
						delete(h.fails, id)
						rep = newReportReqLocked(h, id, "deliver", m.To)
					} else {
						// 保持 queued，记退避，下次 Pump 到点才再试
						h.backoffLocked(id)
					}
				}
			} else {
				// 送达期间对端没再上报才置 busy，别把钩子刚报的 idle 覆盖掉
				if h.peerSeq[m.To] == seq0 {
					h.setPeerLocked(m.To, PeerBusy)
				}
				// Now 是用户强送（对方窗口里可能是 shell，不会有钩子来确认），
				// 注入成功就算送达；其余先进 injected，等对端下一次上报。
				if m.Now {
					h.finishDeliveredLocked(j)
				} else {
					h.markInjectedLocked(j)
				}
				h.markDirtyLocked() // 注入阶段成功记一笔；即便 finish 早已置过 dirty 也无妨
				cp := h.msgs[j]
				out = &cp
			}
		} else if err == nil {
			// 送达成功但这期间已被 reject/淘汰：状态不动，只写一条日志提示；
			// dirty 兜底——确保 injecting 标记被尽快带出快照，重启不误判"正在注入"
			cp := h.msgs[j]
			cp.Reason = deliveredWhileRejectedReason
			out = &cp
			h.markDirtyLocked()
		}
	}
	delete(h.inflight, id)
	delete(h.injecting, id) // 注入结束（成功、失败、被取消都算）：清掉进行中标记
	h.markDirtyLocked()
	h.mu.Unlock()
	if out != nil {
		h.logMsg(*out)
	}
	h.persist() // delivered/gone/暂停/退避 Reason 等送达结果落盘
	if rep != nil {
		h.attachReport(rep, h.captureScreen(ctx, rep.addr))
	}
}
