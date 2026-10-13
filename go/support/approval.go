// Package support 提供 Mac 协助服务的待批队列与 Claude Code 钩子格式转换，纯内存、无网络依赖。
package support

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// Request 是一条等待用户批准的请求。
type Request struct {
	ID      string          `json:"id"`      // 16 位十六进制随机串
	Tool    string          `json:"tool"`    // 工具名，如 "Bash"、"Edit"
	Input   json.RawMessage `json:"input"`   // 工具参数原文
	Summary string          `json:"summary"` // 给人看的一行摘要，见 Summarize
	Created time.Time       `json:"created"`
}

// Decision 是对一条请求的决定。
type Decision struct {
	Allow   bool   `json:"allow"`
	Message string `json:"message"` // 拒绝原因（允许时可空）
}

// ErrNotFound 表示要决定的请求不在待批队列里（不存在、已决定或已超时）。
var ErrNotFound = errors.New("support: 没有这条待批请求")

// maxSummaryLen 是摘要的最大长度（按 rune 计）。
const maxSummaryLen = 200

// waiter 是队列里一个等待决定的 Ask 调用。
type waiter struct {
	req     Request
	done    chan Decision // 缓冲 1；每个 waiter 至多被写一次，所以写方永不阻塞
	settled bool          // 受 Queue.mu 保护：已被决定/取消/超时占定
}

// Queue 是待批队列，并发安全。
type Queue struct {
	mu      sync.Mutex
	waiters map[string]*waiter
	order   []string // 加入顺序，Pending 靠它保证同时间按加入顺序
}

// NewQueue 新建一个空的待批队列。
func NewQueue() *Queue {
	return &Queue{waiters: make(map[string]*waiter)}
}

// newID 生成 16 位十六进制随机串。
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand 自 Go 1.24 起不会失败
	return hex.EncodeToString(b[:])
}

// Ask 加入一条待批请求并阻塞到有决定。
// ctx 先到期：把这条从待批中移除，返回 Decision{} 和 ctx.Err()。
// Decide 若在 ctx 到期后、Ask 处理前抢先成功，Ask 返回该决定且无错误
// （保证 Decide 返回 nil 即调用方一定拿到这个决定）。
func (q *Queue) Ask(ctx context.Context, tool string, input json.RawMessage) (Decision, error) {
	return q.AskWithNotify(ctx, tool, input, nil)
}

// AskWithNotify 同 Ask；加入待批之后、阻塞之前，在锁外调用 added（可为 nil），
// 让调用方拿到这条请求的 ID（例如写日志）。
func (q *Queue) AskWithNotify(ctx context.Context, tool string, input json.RawMessage, added func(Request)) (Decision, error) {
	input = cloneRaw(input)
	w := &waiter{
		req: Request{
			Tool:    tool,
			Input:   input,
			Summary: Summarize(tool, input),
			Created: time.Now(),
		},
		done: make(chan Decision, 1),
	}
	q.mu.Lock()
	for {
		w.req.ID = newID()
		if _, dup := q.waiters[w.req.ID]; !dup {
			break
		}
	}
	q.waiters[w.req.ID] = w
	q.order = append(q.order, w.req.ID)
	req := w.req
	req.Input = cloneRaw(req.Input)
	q.mu.Unlock()

	if added != nil {
		added(req)
	}

	select {
	case d := <-w.done:
		return d, nil
	case <-ctx.Done():
		q.mu.Lock()
		if !w.settled {
			w.settled = true
			q.removeLocked(w.req.ID)
			q.mu.Unlock()
			return Decision{}, ctx.Err()
		}
		q.mu.Unlock()
		// 并发下 Decide/CancelAll 已占定，值一定已在带缓冲的通道里。
		return <-w.done, nil
	}
}

// cloneRaw 复制 RawMessage，保持 nil 为 nil。
func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return append(json.RawMessage{}, b...)
}

// Pending 返回当前待批请求的拷贝，按 Created 从早到晚（同时间按加入顺序）。
func (q *Queue) Pending() []Request {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Request, 0, len(q.order))
	for _, id := range q.order {
		if w, ok := q.waiters[id]; ok {
			r := w.req
			r.Input = cloneRaw(r.Input)
			out = append(out, r)
		}
	}
	return out
}

// Decide 给某条请求下决定并唤醒等待者；该请求随即从待批中移除。
// id 不存在（或已决定、已超时）返回 ErrNotFound。
func (q *Queue) Decide(id string, d Decision) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	w, ok := q.waiters[id]
	if !ok || w.settled {
		return ErrNotFound
	}
	w.settled = true
	q.removeLocked(id)
	w.done <- d
	return nil
}

// CancelAll 让所有等待者立即得到 Decision{Allow:false, Message: msg}，清空待批。
func (q *Queue) CancelAll(msg string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range q.order {
		if w, ok := q.waiters[id]; ok {
			w.settled = true
			w.done <- Decision{Allow: false, Message: msg}
		}
	}
	q.waiters = make(map[string]*waiter)
	q.order = nil
}

// removeLocked 从待批中摘掉一条；调用方须持锁。
func (q *Queue) removeLocked(id string) {
	delete(q.waiters, id)
	for i, o := range q.order {
		if o == id {
			q.order = append(q.order[:i], q.order[i+1:]...)
			return
		}
	}
}

// Summarize 生成一行摘要：Bash 取 command；Edit/Write/Read/NotebookEdit 取
// file_path（缺失则 notebook_path）；WebFetch 取 url；其他工具取 input 原文。
// 取字段时先解成字符串（不能带 JSON 的引号和转义）；取不到就退回 input 原文。
// 换行换成空格；超过 200 个字符（按 rune）截断加 "…"。input 非法 JSON 不 panic。
func Summarize(tool string, input json.RawMessage) string {
	var s string
	switch tool {
	case "Bash":
		s = jsonString(input, "command")
	case "Edit", "Write", "Read", "NotebookEdit":
		if s = jsonString(input, "file_path"); s == "" {
			s = jsonString(input, "notebook_path")
		}
	case "WebFetch":
		s = jsonString(input, "url")
	}
	if s == "" {
		s = string(input)
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxSummaryLen {
		s = string(r[:maxSummaryLen]) + "…"
	}
	return s
}

// jsonString 取 JSON 对象里某字段的字符串值；
// input 不是合法 JSON 对象、字段缺失或不是字符串时返回空串。
func jsonString(input json.RawMessage, name string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m[name], &s); err != nil {
		return ""
	}
	return s
}
