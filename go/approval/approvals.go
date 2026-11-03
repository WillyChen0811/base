package approval

// 任意会话（Mac/Ubuntu 上交互式的 Claude、Codex）的通用审批通道：
// 客户端经证书接口 POST /approval/ask 提交钩子输入并阻塞等决定，
// 界面经本机接口 GET /approval 查看、POST /approval/decide 批准/拒绝。
// 与待批队列 Queue 完全独立。
//
// 从 go/support 挪出来时：原来挂在 support.Service 上的 s.approvals、s.cfg.MsgAuth 换成 Channel
// 自带的审批簿和 Config.Verifier；approvalWait / maxApprovalPending 两个包级变量换成 Config 字段
// （缺省值不变）；原来借用 support 的几个小工具（侧别校验、读正文、证书校验、写 JSON）在 http.go 里自带一份。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/WillyChen0811/base/go/permca"
)

const (
	// DefaultWait 是 POST /approval/ask 等用户决定的缺省最长时间。
	DefaultWait = 10 * time.Minute
	// DefaultMaxPending 是通用审批簿的缺省待批上限，超出直接拒绝。
	DefaultMaxPending = 50
)

// errApprovalFull 表示审批簿已满。
var errApprovalFull = errors.New("approval: 待批审批太多")

// ApprovalRequest 是一条通用审批请求（任意会话的授权钩子）。
type ApprovalRequest struct {
	ID      string          `json:"id"`
	Addr    string          `json:"addr"`    // 发起会话地址 mac:/ubuntu:
	Agent   string          `json:"agent"`   // claude / codex / 其他
	Cwd     string          `json:"cwd"`     // 会话工作目录
	Tool    string          `json:"tool"`    // tool_name
	Input   json.RawMessage `json:"input"`   // tool_input 原样
	Summary string          `json:"summary"` // 人读摘要：Bash 取 command，Edit/Write 取 file_path，其他取 input 前 200 字节
	Created time.Time       `json:"created"`
}

// approvalWaiter 是审批簿里一个等待决定的 ask 调用。
type approvalWaiter struct {
	req     ApprovalRequest
	done    chan Decision // 缓冲 1；每个 waiter 至多被写一次，所以写方永不阻塞
	settled bool          // 受 approvalBook.mu 保护：已被决定/超时/断开占定
}

// approvalBook 是通用审批簿，并发安全。结构与 Queue 同套路，但生命周期独立（不受 Queue.CancelAll 影响）。
type approvalBook struct {
	mu         sync.Mutex
	items      map[string]*approvalWaiter
	order      []string // 加入顺序，pending 靠它保证同时间按加入顺序
	maxPending int      // 待批上限
}

// newApprovalBook 建一个空的审批簿；maxPending <= 0 用 DefaultMaxPending。
func newApprovalBook(maxPending int) *approvalBook {
	if maxPending <= 0 {
		maxPending = DefaultMaxPending
	}
	return &approvalBook{items: make(map[string]*approvalWaiter), maxPending: maxPending}
}

// add 登记一条请求并返回它的 waiter；待批已达上限返回 errApprovalFull。
// ID 与 Created 由簿生成/盖章，调用方给的这两个字段会被覆盖。
func (b *approvalBook) add(req ApprovalRequest) (*approvalWaiter, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.order) >= b.maxPending {
		return nil, errApprovalFull
	}
	for {
		req.ID = newID()
		if _, dup := b.items[req.ID]; !dup {
			break
		}
	}
	req.Created = time.Now()
	req.Input = cloneRaw(req.Input)
	w := &approvalWaiter{req: req, done: make(chan Decision, 1)}
	b.items[req.ID] = w
	b.order = append(b.order, req.ID)
	return w, nil
}

// wait 阻塞到有决定，返回 (Decision, true)；ctx 先到期（超时/客户端断开）时
// 把这条从簿中摘掉（除非 decide 抢先占定）并返回 (Decision{}, false)。
// decide 抢在 ctx 到期后、摘除前占定的，仍返回那个决定——与 Queue.Ask 同样的抢占规则。
func (b *approvalBook) wait(ctx context.Context, w *approvalWaiter) (Decision, bool) {
	select {
	case d := <-w.done:
		return d, true
	case <-ctx.Done():
		b.mu.Lock()
		if !w.settled {
			w.settled = true
			b.removeLocked(w.req.ID)
			b.mu.Unlock()
			return Decision{}, false
		}
		b.mu.Unlock()
		// 并发下 decide 已占定，值一定已在带缓冲的通道里。
		return <-w.done, true
	}
}

// decide 给某条请求下决定并唤醒等待者，该请求随即从簿中移除；
// id 不存在（或已决定、已超时）返回 ErrNotFound。
func (b *approvalBook) decide(id string, d Decision) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.items[id]
	if !ok || w.settled {
		return ErrNotFound
	}
	w.settled = true
	b.removeLocked(id)
	w.done <- d
	return nil
}

// pending 返回当前待批请求的拷贝，按 Created 从旧到新（同时间按加入顺序）。
func (b *approvalBook) pending() []ApprovalRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ApprovalRequest, 0, len(b.order))
	for _, id := range b.order {
		if w, ok := b.items[id]; ok {
			r := w.req
			r.Input = cloneRaw(r.Input)
			out = append(out, r)
		}
	}
	return out
}

// removeLocked 从簿中摘掉一条；调用方须持锁。
func (b *approvalBook) removeLocked(id string) {
	delete(b.items, id)
	for i, o := range b.order {
		if o == id {
			b.order = append(b.order[:i], b.order[i+1:]...)
			return
		}
	}
}

// Config 是 Channel 的注入依赖。
type Config struct {
	// Verifier 校验客户端接口（POST /approval/ask）的请求证书：对 method+RequestURI+body 验签，
	// 时间戳/nonce/吊销由 Verifier 管，并要求 addr 落在证书 Perm.AddrPrefix 内。
	// nil = 不要证书，也不查证书前缀（只查侧别）。
	Verifier *permca.Verifier
	// Wait 是 ask 等用户决定的最长时间；0 用 DefaultWait。
	Wait time.Duration
	// MaxPending 是待批上限，满了新的 ask 直接 503；0 用 DefaultMaxPending。
	MaxPending int
}

// Channel 是通用审批通道：一本审批簿加三个 HTTP 入口。
type Channel struct {
	book *approvalBook
	cfg  Config
}

// NewChannel 建一个通用审批通道。
func NewChannel(cfg Config) *Channel {
	if cfg.Wait <= 0 {
		cfg.Wait = DefaultWait
	}
	return &Channel{book: newApprovalBook(cfg.MaxPending), cfg: cfg}
}

// Pending 返回当前待批请求的拷贝，按 Created 从旧到新。
func (c *Channel) Pending() []ApprovalRequest { return c.book.pending() }

// Decide 给某条请求下决定并唤醒等待者；id 不存在（或已决定、已超时）返回 ErrNotFound。
func (c *Channel) Decide(id string, d Decision) error { return c.book.decide(id, d) }

// approvalAskReq 是 POST /approval/ask 的正文；Hook 是 PermissionRequest 钩子输入原样 JSON。
type approvalAskReq struct {
	Addr  string          `json:"addr"`
	Agent string          `json:"agent"`
	Cwd   string          `json:"cwd"`
	Hook  json.RawMessage `json:"hook"`
}

// approvalAsk 是 POST /approval/ask；side 是本侧允许的 addr 前缀。
// 配了 Verifier 时是证书接口：先读正文验证书，
// 再查 addr 落在本侧前缀与证书 Perm.AddrPrefix 内，然后 ParseHookInput 入簿，
// 阻塞等用户决定，最长 Config.Wait 或客户端断开：
//   - 用户决定 → {"decided":true,"allow":...,"message":...}
//   - 超时/断开 → 从簿中删除，{"decided":false}（客户端据此退回终端确认）。
func (c *Channel) approvalAsk(side string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := readBody(w, r)
		if body == nil {
			return
		}
		cert, ok := c.certCheck(w, r, body)
		if !ok {
			return
		}
		var req approvalAskReq
		if err := json.Unmarshal(body, &req); err != nil {
			errJSON(w, http.StatusBadRequest, "正文不是合法 JSON")
			return
		}
		if code, msg := checkSide(req.Addr, side); code != 0 {
			errJSON(w, code, msg)
			return
		}
		if c.cfg.Verifier != nil {
			if code, msg := certAddrPerm(cert.Perm, req.Addr); code != 0 {
				errJSON(w, code, msg)
				return
			}
		}
		h, err := ParseHookInput(req.Hook)
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}

		w2, err := c.book.add(ApprovalRequest{
			Addr:    req.Addr,
			Agent:   req.Agent,
			Cwd:     req.Cwd,
			Tool:    h.Tool,
			Input:   h.Input,
			Summary: Summarize(h.Tool, h.Input),
		})
		if err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, errApprovalFull) {
				code = http.StatusServiceUnavailable
			}
			errJSON(w, code, "待批太多")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), c.cfg.Wait)
		defer cancel()
		d, decided := c.book.wait(ctx, w2)
		if !decided {
			writeJSON(w, http.StatusOK, map[string]any{"decided": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"decided": true, "allow": d.Allow, "message": d.Message})
	}
}

// approvalList 是 GET /approval（界面内部接口，不要证书；来源校验如 Origin 由挂载方在路由层做）：
// 当前待批列表，按 Created 从旧到新。
func (c *Channel) approvalList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"requests": c.book.pending()})
}

// approvalDecide 是 POST /approval/decide?id=&allow=1|0&message=（本机内部接口）。
// 没有这条请求（不存在/已决定/已超时）404。
func (c *Channel) approvalDecide(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	allow := q.Get("allow")
	if allow != "1" && allow != "0" {
		errJSON(w, http.StatusBadRequest, "allow 必须是 1 或 0")
		return
	}
	message := truncRunes(q.Get("message"), maxDecideMessage)
	err := c.book.decide(q.Get("id"), Decision{Allow: allow == "1", Message: message})
	if errors.Is(err, ErrNotFound) {
		errJSON(w, http.StatusNotFound, "没有这条待批请求")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "决定失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// MountRemote 把客户端接口挂进远端 mux：POST /approval/ask，只收 ubuntu: 一侧的地址
// （外层的令牌之类由挂载方管，这里管证书与侧别）。
func (c *Channel) MountRemote(mux *http.ServeMux) {
	mux.HandleFunc("/approval/ask", only(http.MethodPost, c.approvalAsk("ubuntu:")))
}

// MountLocal 把通用审批挂进本机 mux：客户端 ask（mac: 侧，要证书）+
// 界面内部接口 GET /approval、POST /approval/decide（不要证书，Origin 校验由挂载方在路由层做）。
func (c *Channel) MountLocal(mux *http.ServeMux) {
	mux.HandleFunc("/approval", only(http.MethodGet, c.approvalList))
	mux.HandleFunc("/approval/decide", only(http.MethodPost, c.approvalDecide))
	mux.HandleFunc("/approval/ask", only(http.MethodPost, c.approvalAsk("mac:")))
}
