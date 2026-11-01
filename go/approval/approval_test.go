package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type askResult struct {
	d   Decision
	err error
}

// waitForPending 轮询待批数量直到 n；判断"Ask 已挂上"只能靠它，不能等 Ask 返回。
func waitForPending(t *testing.T, q *Queue, n int) []Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p := q.Pending()
		if len(p) == n {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %d 条待批超时，当前 %d 条", n, len(p))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// recv 在超时内收取一个 Ask 结果，收不到直接判失败（避免测试挂死）。
func recv(t *testing.T, ch <-chan askResult) askResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("等待 Ask 返回超时")
		return askResult{}
	}
}

func TestAskBlocksUntilDecide(t *testing.T) {
	q := NewQueue()
	ch := make(chan askResult, 1)
	go func() {
		d, err := q.Ask(context.Background(), "Bash", json.RawMessage(`{"command":"ls"}`))
		ch <- askResult{d, err}
	}()

	p := waitForPending(t, q, 1)
	if len(p[0].ID) != 16 || strings.TrimLeft(p[0].ID, "0123456789abcdef") != "" {
		t.Fatalf("id 不是 16 位十六进制: %q", p[0].ID)
	}
	if p[0].Tool != "Bash" || p[0].Summary != "ls" {
		t.Fatalf("待批内容不对: %+v", p[0])
	}

	if err := q.Decide(p[0].ID, Decision{Allow: true, Message: "准了"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	r := recv(t, ch)
	if r.err != nil || !r.d.Allow || r.d.Message != "准了" {
		t.Fatalf("Ask 返回不对: %+v err=%v", r.d, r.err)
	}
	if got := q.Pending(); len(got) != 0 {
		t.Fatalf("决定后 Pending 应为空，实际 %d 条", len(got))
	}
	if err := q.Decide(p[0].ID, Decision{Allow: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复 Decide 应 ErrNotFound，实际 %v", err)
	}
	if err := q.Decide("deadbeefdeadbeef", Decision{Allow: false}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 id 应 ErrNotFound，实际 %v", err)
	}
}

func TestAskContextTimeout(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ch := make(chan askResult, 1)
	go func() {
		d, err := q.Ask(ctx, "Bash", json.RawMessage(`{"command":"sleep"}`))
		ch <- askResult{d, err}
	}()

	p := waitForPending(t, q, 1)
	r := recv(t, ch)
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("应返回超时错误，实际 %v", r.err)
	}
	if r.d != (Decision{}) {
		t.Fatalf("超时应返回空 Decision，实际 %+v", r.d)
	}
	if got := q.Pending(); len(got) != 0 {
		t.Fatalf("超时后 Pending 应为空，实际 %d 条", len(got))
	}
	if err := q.Decide(p[0].ID, Decision{Allow: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("超时后 Decide 应 ErrNotFound，实际 %v", err)
	}
}

func TestPendingOrderAndCancelAll(t *testing.T) {
	q := NewQueue()
	const n = 5
	ch := make(chan askResult, n)
	var ids []string
	// 逐条挂起（等下一条出现在 Pending 再发下一条），保证加入顺序确定。
	for i := 0; i < n; i++ {
		input := json.RawMessage(fmt.Sprintf(`{"command":"job-%d"}`, i))
		go func() {
			d, err := q.Ask(context.Background(), "Bash", input)
			ch <- askResult{d, err}
		}()
		p := waitForPending(t, q, i+1)
		ids = append(ids, p[i].ID)
		if p[i].Summary != fmt.Sprintf("job-%d", i) {
			t.Fatalf("第 %d 条摘要不对: %q", i, p[i].Summary)
		}
	}
	got := q.Pending()
	for i := range ids {
		if got[i].ID != ids[i] {
			t.Fatalf("Pending 顺序不对: %v", got)
		}
	}
	for i := 1; i < n; i++ {
		if got[i-1].Created.After(got[i].Created) {
			t.Fatalf("Created 应从早到晚: %v", got)
		}
	}

	q.CancelAll("协助结束")
	for i := 0; i < n; i++ {
		r := recv(t, ch)
		if r.err != nil {
			t.Fatalf("CancelAll 后 Ask 不该报错: %v", r.err)
		}
		if r.d.Allow || r.d.Message != "协助结束" {
			t.Fatalf("应一律拒绝并带消息: %+v", r.d)
		}
	}
	if got := q.Pending(); len(got) != 0 {
		t.Fatalf("CancelAll 后 Pending 应为空，实际 %d 条", len(got))
	}
	for _, id := range ids {
		if err := q.Decide(id, Decision{Allow: true}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("取消后 Decide 应 ErrNotFound，实际 %v", err)
		}
	}
}

func TestPendingIsCopy(t *testing.T) {
	q := NewQueue()
	go func() {
		_, _ = q.Ask(context.Background(), "Bash", json.RawMessage(`{"command":"ls"}`))
	}()
	p := waitForPending(t, q, 1)
	p[0].Tool = "改坏了"
	p[0].Input[0] = 'X'
	again := q.Pending()
	if again[0].Tool != "Bash" {
		t.Fatalf("Pending 应返回拷贝，队列被外部改了: %q", again[0].Tool)
	}
	if string(again[0].Input) != `{"command":"ls"}` {
		t.Fatalf("Input 应是拷贝，队列被外部改了: %s", again[0].Input)
	}
	q.CancelAll("")
}

func TestAskConcurrentWithCancelRace(t *testing.T) {
	// Ask 与 CancelAll/Decide 并发竞争，-race 下不应死锁或双重唤醒。
	q := NewQueue()
	var wg sync.WaitGroup
	const n = 20
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			d, err := q.Ask(ctx, "Bash", json.RawMessage(`{"command":"x"}`))
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("意外错误: %v", err)
			}
			_ = d
		}()
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if p := q.Pending(); len(p) > 0 {
				_ = q.Decide(p[0].ID, Decision{Allow: true})
			}
			time.Sleep(time.Millisecond)
		}
	}()
	q.CancelAll("收尾")
	close(done)
	waitTimeout(t, &wg, 5*time.Second)
	if got := q.Pending(); len(got) != 0 {
		t.Fatalf("队列应为空，实际 %d 条", len(got))
	}
}

func waitTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
	case <-time.After(d):
		t.Fatal("等待 goroutine 结束超时")
	}
}

func TestSummarize(t *testing.T) {
	long := strings.Repeat("a", maxSummaryLen)
	longCN := strings.Repeat("中", maxSummaryLen)
	tooLongCN := strings.Repeat("中", maxSummaryLen+1)
	in := func(s string) json.RawMessage { return json.RawMessage(fmt.Sprintf(`{"command":%q}`, s)) }

	tests := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"Bash", "Bash", `{"command":"npm test"}`, "npm test"},
		{"Bash 带引号转义取字符串值", "Bash", `{"command":"echo \"hi\""}`, `echo "hi"`},
		{"Edit", "Edit", `{"file_path":"/a/b.txt"}`, "/a/b.txt"},
		{"Write", "Write", `{"file_path":"/a/new.txt"}`, "/a/new.txt"},
		{"Read", "Read", `{"file_path":"/a/b.txt"}`, "/a/b.txt"},
		{"NotebookEdit 取 notebook_path", "NotebookEdit", `{"notebook_path":"/a/b.ipynb"}`, "/a/b.ipynb"},
		{"Edit 无 file_path 退回原文", "Edit", `{"other":1}`, `{"other":1}`},
		{"WebFetch", "WebFetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"其他工具取原文", "Grep", `{"pattern":"foo"}`, `{"pattern":"foo"}`},
		{"非法 JSON 不 panic", "Bash", `not json`, "not json"},
		{"多行换空格", "Bash", `{"command":"a\nb\r\nc"}`, "a b  c"},
		{"恰好 200 不加省略号", "Bash", string(in(long)), long},
		{"201 字符截断加省略号", "Bash", string(in(long + "x")), long + "…"},
		{"中文按 rune 计", "Bash", string(in(tooLongCN)), longCN + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Summarize(tt.tool, json.RawMessage(tt.input))
			if got != tt.want {
				t.Fatalf("Summarize(%q, %s) = %q, 期望 %q", tt.tool, tt.input, got, tt.want)
			}
		})
	}
}

func TestAskInputClonedOnStore(t *testing.T) {
	q := NewQueue()
	in := json.RawMessage(`{"command":"ls"}`)
	go func() { _, _ = q.Ask(context.Background(), "Bash", in) }()
	waitForPending(t, q, 1)
	in[0] = 'X'
	if got := q.Pending(); string(got[0].Input) != `{"command":"ls"}` {
		t.Fatalf("Ask 应克隆传入的 input: %s", got[0].Input)
	}
	q.CancelAll("")
}

func TestAskDecideVsContextRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		q := NewQueue()
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%5)*50*time.Microsecond)
		ch := make(chan askResult, 1)
		go func() {
			d, err := q.Ask(ctx, "Bash", json.RawMessage(`{"command":"x"}`))
			ch <- askResult{d, err}
		}()
		// 另一 goroutine 反复尝试 Decide，直到试过一次（成功或 ErrNotFound）或 Ask 已返回。
		derrCh := make(chan error, 1)
		go func() {
			for {
				if p := q.Pending(); len(p) > 0 {
					derrCh <- q.Decide(p[0].ID, Decision{Allow: true})
					return
				}
				select {
				case <-ctx.Done():
					derrCh <- nil
					return
				default:
				}
			}
		}()
		r := recv(t, ch)
		cancel()
		derr := <-derrCh
		if derr == nil && r.err != nil {
			// 没有 Decide 成功：Ask 只能是 ctx 错误。
			if !errors.Is(r.err, context.DeadlineExceeded) || r.d != (Decision{}) {
				t.Fatalf("第 %d 轮: 未决定时 Ask 应返回 ctx 错误: %+v err=%v", i, r.d, r.err)
			}
			continue
		}
		switch {
		case derr == nil:
			if !r.d.Allow {
				t.Fatalf("第 %d 轮: Decide 成功，Ask 应得到决定: %+v", i, r.d)
			}
		case errors.Is(derr, ErrNotFound):
			if !errors.Is(r.err, context.DeadlineExceeded) || r.d != (Decision{}) {
				t.Fatalf("第 %d 轮: Decide 未命中，Ask 应返回 ctx 错误: %+v err=%v", i, r.d, r.err)
			}
		default:
			t.Fatalf("意外错误: %v", derr)
		}
	}
}

func TestCancelAllVsDecideExactlyOne(t *testing.T) {
	for i := 0; i < 100; i++ {
		q := NewQueue()
		const n = 5
		ch := make(chan askResult, n)
		for j := 0; j < n; j++ {
			go func() {
				d, err := q.Ask(context.Background(), "Bash", json.RawMessage(`{"command":"x"}`))
				ch <- askResult{d, err}
			}()
		}
		p := waitForPending(t, q, n)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for _, r := range p {
				_ = q.Decide(r.ID, Decision{Allow: true})
			}
		}()
		go func() {
			defer wg.Done()
			q.CancelAll("取消")
		}()
		for j := 0; j < n; j++ {
			if r := recv(t, ch); r.err != nil {
				t.Fatalf("不该报错: %v", r.err)
			}
		}
		waitTimeout(t, &wg, 5*time.Second)
		select {
		case r := <-ch:
			t.Fatalf("多出一个结果: %+v", r)
		case <-time.After(time.Millisecond):
		}
		if got := q.Pending(); len(got) != 0 {
			t.Fatalf("Pending 应为空，实际 %d 条", len(got))
		}
	}
}

func TestAskCanceledContextReturnsImmediately(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan askResult, 1)
	go func() {
		d, err := q.Ask(ctx, "Bash", json.RawMessage(`{"command":"x"}`))
		ch <- askResult{d, err}
	}()
	r := recv(t, ch)
	if !errors.Is(r.err, context.Canceled) || r.d != (Decision{}) {
		t.Fatalf("应立即返回 ctx 错误: %+v err=%v", r.d, r.err)
	}
	if got := q.Pending(); len(got) != 0 {
		t.Fatalf("Pending 应为空，实际 %d 条", len(got))
	}
}

func TestAskWithNotifyReportsID(t *testing.T) {
	q := NewQueue()
	got := make(chan Request, 1)
	res := make(chan askResult, 1)
	go func() {
		d, err := q.AskWithNotify(context.Background(), "Bash", json.RawMessage(`{"command":"ls"}`), func(r Request) {
			// 回调在锁外：这里可以安全地再访问队列
			_ = q.Pending()
			got <- r
		})
		res <- askResult{d, err}
	}()
	var r Request
	select {
	case r = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("没收到通知")
	}
	if p := waitForPending(t, q, 1); p[0].ID != r.ID || r.Tool != "Bash" || r.Summary != "ls" {
		t.Fatalf("通知的请求与待批不一致: %+v vs %+v", r, p)
	}
	if err := q.Decide(r.ID, Decision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if a := recv(t, res); a.err != nil || !a.d.Allow {
		t.Fatalf("结果 = %+v", a)
	}
}
