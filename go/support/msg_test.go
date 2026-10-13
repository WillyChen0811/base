package support

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 是可推进的假时钟，供自动模式等按时间判断的测试用。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// msgFixture 把 Hub 和它的注入依赖打包，记录 Deliver/Sync 调用和状态日志。
type msgFixture struct {
	hub        *Hub
	clock      *fakeClock
	mu         sync.Mutex
	deliver    []Message
	syncReqs   []Message
	logs       []map[string]string
	syncErr    error
	deliverErr error
}

func newMsgFixture() *msgFixture {
	f := &msgFixture{clock: newFakeClock()}
	f.hub = NewHub(HubConfig{
		Now: f.clock.Now,
		Sync: func(_ context.Context, m Message) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.syncReqs = append(f.syncReqs, m)
			return f.syncErr
		},
		Deliver: func(_ context.Context, m Message) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deliver = append(f.deliver, m)
			return f.deliverErr
		},
		Log: func(kind string, detail map[string]string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if kind != "msg" {
				return
			}
			f.logs = append(f.logs, detail)
		},
	})
	return f
}

// setSyncErr 设置 Sync 的返回错误。
func (f *msgFixture) setSyncErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncErr = err
}

// setDeliverErr 设置 Deliver 的返回错误。
func (f *msgFixture) setDeliverErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliverErr = err
}

func (f *msgFixture) delivered() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.deliver...)
}

func (f *msgFixture) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncReqs)
}

func (f *msgFixture) loggedStates(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, d := range f.logs {
		if d["id"] == id {
			out = append(out, d["state"])
		}
	}
	return out
}

func okMsg() Message {
	return Message{From: "ubuntu:macsrv", To: "mac:alice", Text: "hello"}
}

func stateOf(t *testing.T, h *Hub, id string) MsgState {
	t.Helper()
	for _, m := range h.List() {
		if m.ID == id {
			return m.State
		}
	}
	t.Fatalf("消息 %s 不在 List 里", id)
	return ""
}

func TestValidAddr(t *testing.T) {
	good := []string{"mac:alice", "ubuntu:lab-1", "mac:a", "ubuntu:A.b_c-9", "mac:" + strings.Repeat("x", 64)}
	for _, a := range good {
		if err := ValidAddr(a); err != nil {
			t.Errorf("ValidAddr(%q) 应通过， got %v", a, err)
		}
	}
	bad := []string{"", "alice", "mac", "mac:", "ubuntu:", "windows:alice", "mac:a b", "mac:a/b", "mac:a:b",
		"mac:" + strings.Repeat("x", 65), "ubuntu:含中文", "mac:a\tb"}
	for _, a := range bad {
		if err := ValidAddr(a); err == nil {
			t.Errorf("ValidAddr(%q) 应报错", a)
		}
	}
}

func TestSendValidation(t *testing.T) {
	f := newMsgFixture()

	cases := []struct {
		name string
		mod  func(*Message)
	}{
		{"From 非法", func(m *Message) { m.From = "bad:addr" }},
		{"To 非法", func(m *Message) { m.To = "mac:" }},
		{"自发自", func(m *Message) { m.To = m.From }},
		{"空文本", func(m *Message) { m.Text = "   \t\n" }},
		{"文本超 64KB", func(m *Message) { m.Text = strings.Repeat("a", 64*1024+1) }},
		{"附件超 1MB", func(m *Message) { m.File = bytes.Repeat([]byte("x"), 1024*1024+1); m.FileName = "a.txt" }},
		{"有附件无文件名", func(m *Message) { m.File = []byte("x") }},
		{"只给 Repo", func(m *Message) { m.Repo = "base" }},
		{"只给 Branch", func(m *Message) { m.Branch = "main" }},
		{"Branch 非法", func(m *Message) { m.Repo = "base"; m.Branch = "bad..name" }},
	}
	for _, c := range cases {
		m := okMsg()
		c.mod(&m)
		if id, err := f.hub.Send(m); err == nil {
			t.Errorf("%s: Send 应报错，实际入库 id=%s", c.name, id)
		}
	}

	// 边界：恰好 64KB 文本、恰好 1MB 附件应当通过。
	m := okMsg()
	m.Text = strings.Repeat("a", 64*1024)
	m.File = bytes.Repeat([]byte("x"), 1024*1024)
	m.FileName = "/tmp/dir/note.txt"
	m.Repo = "base"
	m.Branch = "feat/msg"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatalf("边界内 Send 失败: %v", err)
	}
	if !regexp16(id) {
		t.Errorf("ID 应为 16 位十六进制，实际 %q", id)
	}
	if state := stateOf(t, f.hub, id); state != MsgPending {
		t.Errorf("应 pending，实际 %s", state)
	}
}

func TestSendStoresSanitized(t *testing.T) {
	f := newMsgFixture()
	m := okMsg()
	m.Text = "  正文  "
	m.FileName = "dir/sub/note.txt"
	m.File = []byte("data")
	m.State = MsgDelivered // 入参状态应被重置
	m.Now = true
	m.Synced = true
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if !regexp16(id) {
		t.Errorf("ID 应为 16 位十六进制，实际 %q", id)
	}
	list := f.hub.List()
	if len(list) != 1 {
		t.Fatalf("List 长度 %d", len(list))
	}
	got := list[0]
	if got.State != MsgPending {
		t.Errorf("手动模式 Send 后应 pending，实际 %s", got.State)
	}
	if got.Now || got.Synced || got.Reason != "" {
		t.Errorf("入参 Now/Synced/Reason 应被重置: %+v", got)
	}
	if got.FileName != "note.txt" {
		t.Errorf("FileName 应只取 base 名，实际 %q", got.FileName)
	}
	if got.Text != m.Text {
		t.Errorf("Text 应原样保存")
	}
	if got.File != nil {
		t.Errorf("List 不应带出 File")
	}
}

func regexp16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func TestManualApprovePump(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "ubuntu:bob" // ubuntu 走 Deliver；mac: 走 outbox 另有专门测试
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if state := stateOf(t, f.hub, id); state != MsgPending {
		t.Fatalf("应 pending，实际 %s", state)
	}
	// pending 时 Pump 不该动它
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 0 {
		t.Fatalf("pending 时不应送达，实际 %d 条", n)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if state := stateOf(t, f.hub, id); state != MsgQueued {
		t.Fatalf("approve 后应 queued，实际 %s", state)
	}
	f.hub.Pump(ctx)
	if state := stateOf(t, f.hub, id); state != MsgDelivered {
		t.Fatalf("Pump 后应 delivered，实际 %s", state)
	}
	d := f.delivered()
	if len(d) != 1 || d[0].ID != id || d[0].Text != "hello" {
		t.Fatalf("Deliver 收到的消息不对: %+v", d)
	}
	if f.hub.Peers()["ubuntu:bob"] != PeerBusy {
		t.Errorf("送达后接收方应置忙")
	}
	states := f.loggedStates(id)
	want := []string{string(MsgPending), string(MsgQueued), string(MsgDelivered)}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("状态日志应依次 %v，实际 %v", want, states)
	}
}

func TestAutoMode(t *testing.T) {
	f := newMsgFixture()
	if err := f.hub.SetAuto(10); err != nil {
		t.Fatal(err)
	}
	if f.hub.AutoUntil().IsZero() {
		t.Errorf("开启后 AutoUntil 不应为零值")
	}
	id, err := f.hub.Send(okMsg())
	if err != nil {
		t.Fatal(err)
	}
	if state := stateOf(t, f.hub, id); state != MsgQueued {
		t.Fatalf("自动模式 Send 应直接 queued，实际 %s", state)
	}
	// 到期前一刻仍算开着
	f.clock.Add(10*time.Minute - time.Second)
	if f.hub.AutoUntil().IsZero() {
		t.Errorf("到期前 AutoUntil 不应为零值")
	}
	// 到期：视为关，新消息回 pending
	f.clock.Add(time.Second)
	if !f.hub.AutoUntil().IsZero() {
		t.Errorf("过期后 AutoUntil 应为零值")
	}
	id2, err := f.hub.Send(okMsg())
	if err != nil {
		t.Fatal(err)
	}
	if state := stateOf(t, f.hub, id2); state != MsgPending {
		t.Fatalf("到期后 Send 应回 pending，实际 %s", state)
	}
	// 手动关掉
	if err := f.hub.SetAuto(0); err != nil {
		t.Fatal(err)
	}
	if !f.hub.AutoUntil().IsZero() {
		t.Errorf("SetAuto(0) 后应为零值")
	}
	// 边界
	if err := f.hub.SetAuto(600); err != nil {
		t.Errorf("SetAuto(600) 应通过，got %v", err)
	}
	for _, bad := range []int{-1, 601, 1 << 30} {
		if err := f.hub.SetAuto(bad); err == nil {
			t.Errorf("SetAuto(%d) 应报错", bad)
		}
	}
}

func TestBusySkipsUnlessNow(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "ubuntu:bob"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState("ubuntu:bob", PeerBusy); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 0 {
		t.Fatalf("对方忙时不应送达，实际 %d 条", n)
	}
	if state := stateOf(t, f.hub, id); state != MsgQueued {
		t.Fatalf("应仍 queued，实际 %s", state)
	}
	// now：忙也照送
	if err := f.hub.Decide(id, "now"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if state := stateOf(t, f.hub, id); state != MsgDelivered {
		t.Fatalf("now 应无视 busy 送达，实际 %s", state)
	}
	// 刚送达后对方被置忙：再来一条应停住
	id2, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id2, "approve"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 1 {
		t.Fatalf("置忙后第二条不应送达，实际共 %d 条", n)
	}
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 2 {
		t.Fatalf("空闲后应送达第二条，实际共 %d 条", n)
	}
}

func TestSyncFailRetryAndSkip(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "ubuntu:bob"
	m.Repo = "base"
	m.Branch = "feat/msg"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	syncBoom := errors.New("ssh 断了")
	f.setSyncErr(syncBoom)
	f.hub.Pump(ctx)
	if state := stateOf(t, f.hub, id); state != MsgSyncFailed {
		t.Fatalf("同步失败应 sync_failed，实际 %s", state)
	}
	for _, lm := range f.hub.List() {
		if lm.ID == id && lm.Reason != syncBoom.Error() {
			t.Errorf("Reason 应为错误原文，实际 %q", lm.Reason)
		}
	}
	if n := len(f.delivered()); n != 0 {
		t.Fatalf("同步失败不应送达，实际 %d 条", n)
	}
	// sync_failed 时 approve 应报错
	if err := f.hub.Decide(id, "approve"); err == nil {
		t.Errorf("sync_failed 不能 approve")
	}
	// retry_sync：重新同步
	if err := f.hub.Decide(id, "retry_sync"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx) // 还是错，回到 sync_failed——sync_failed 不会被 Pump 再碰
	if state := stateOf(t, f.hub, id); state != MsgSyncFailed {
		t.Fatalf("Sync 仍失败应回 sync_failed，实际 %s", state)
	}
	// sync_failed 必须先 retry_sync 才会重新进 Pump
	if err := f.hub.Decide(id, "retry_sync"); err != nil {
		t.Fatal(err)
	}
	f.setSyncErr(nil)
	f.hub.Pump(ctx)
	if state := stateOf(t, f.hub, id); state != MsgDelivered {
		t.Fatalf("重试同步成功后应 delivered，实际 %s", state)
	}
	if n := len(f.delivered()); n != 1 {
		t.Errorf("应送达 1 条，实际 %d", n)
	}

	// skip_sync：不再同步直接送
	m2 := okMsg()
	m2.To = "ubuntu:bob"
	m2.Repo = "base"
	m2.Branch = "feat/msg"
	id2, err := f.hub.Send(m2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id2, "approve"); err != nil {
		t.Fatal(err)
	}
	// 上一条送达时把 bob 置了忙，先放回空闲，这条 Pump 才会真去同步
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.setSyncErr(syncBoom)
	f.hub.Pump(ctx)
	if state := stateOf(t, f.hub, id2); state != MsgSyncFailed {
		t.Fatalf("应 sync_failed，实际 %s", state)
	}
	if err := f.hub.Decide(id2, "skip_sync"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	for _, lm := range f.hub.List() {
		if lm.ID == id2 && lm.State != MsgDelivered {
			t.Errorf("skip_sync 后应不再同步直接送达，实际 %s", lm.State)
		}
	}
}

func TestMacOutbox(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.File = []byte("payload")
	m.FileName = "a.bin"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	// 空 outbox：超时返回 false
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, ok := f.hub.TakeOutbox(shortCtx, "mac:alice"); ok {
		t.Fatalf("空 outbox 应超时返回 false")
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 0 {
		t.Fatalf("mac: 地址不应调 Deliver，实际调了 %d 次", n)
	}
	got, ok := f.hub.TakeOutbox(ctx, "mac:alice")
	if !ok {
		t.Fatalf("Pump 后应能取到消息")
	}
	if got.ID != id || string(got.File) != "payload" {
		t.Errorf("取走的内容不对: id=%s file=%q", got.ID, got.File)
	}
	if f.hub.Peers()["mac:alice"] != PeerBusy {
		t.Errorf("取走后应置忙")
	}
	// 取走未确认：不重复给
	shortCtx2, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	if _, ok := f.hub.TakeOutbox(shortCtx2, "mac:alice"); ok {
		t.Fatalf("已取走的不应重复给")
	}
	// 再 Pump 也不该把它塞回 outbox
	f.hub.Pump(ctx)
	shortCtx3, cancel3 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel3()
	if _, ok := f.hub.TakeOutbox(shortCtx3, "mac:alice"); ok {
		t.Fatalf("已取走的再 Pump 也不该回到 outbox")
	}
	if err := f.hub.MarkDelivered(id); err != nil {
		t.Fatal(err)
	}
	if state := stateOf(t, f.hub, id); state != MsgDelivered {
		t.Fatalf("MarkDelivered 后应 delivered，实际 %s", state)
	}
	if err := f.hub.MarkDelivered(id); err == nil {
		t.Errorf("重复确认应报错")
	}
}

func TestTakeOutboxLongPollWakes(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	got := make(chan Message, 1)
	pollCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() {
		msg, ok := f.hub.TakeOutbox(pollCtx, "mac:alice")
		if ok {
			got <- msg
		} else {
			close(got)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	f.hub.Pump(ctx)
	select {
	case msg, ok := <-got:
		if !ok || msg.ID != id {
			t.Fatalf("长轮询应拿到 %s，实际 ok=%v msg=%+v", id, ok, msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("长轮询没有被 Pump 唤醒")
	}
}

// 复核 1：发给同一个 mac 地址的多条 queued 会一次全进 outbox，
// 但取走第一条置忙后，非 now 的第二条必须按住；对方回 idle 才能取到。
func TestMacOutboxBusyGating(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "mac:amy"
	id1, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id1, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id2, "approve"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx) // 两条都进 outbox
	got1, ok := f.hub.TakeOutbox(ctx, "mac:amy")
	if !ok || got1.ID != id1 {
		t.Fatalf("第一条应取到 id1，实际 ok=%v got=%+v", ok, got1)
	}
	// 取走第一条后 amy 已 busy：第二条不能被拿走
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if got, ok := f.hub.TakeOutbox(shortCtx, "mac:amy"); ok {
		t.Fatalf("对方忙时第二条不应取到，实际 got=%+v", got)
	}
	// SetState 唤醒等待中的 TakeOutbox，回 idle 后能取到第二条
	if err := f.hub.SetState("mac:amy", PeerIdle); err != nil {
		t.Fatal(err)
	}
	got2, ok := f.hub.TakeOutbox(ctx, "mac:amy")
	if !ok || got2.ID != id2 {
		t.Fatalf("空闲后应取到 id2，实际 ok=%v got=%+v", ok, got2)
	}
	if err := f.hub.MarkDelivered(id1); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.MarkDelivered(id2); err != nil {
		t.Fatal(err)
	}
}

// 复核 1（now 例外）：对方忙时，被点 now 的消息仍可取走。
func TestTakeOutboxNowBypassesBusy(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "mac:amy"
	m.File = []byte("p")
	m.FileName = "p.bin"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if err := f.hub.SetState("mac:amy", PeerBusy); err != nil {
		t.Fatal(err)
	}
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, ok := f.hub.TakeOutbox(shortCtx, "mac:amy"); ok {
		t.Fatalf("忙且非 now 不应取到")
	}
	// 复核 4：忙判定和返回内容都以 h.msgs 当前值为准，
	// 放进 outbox 之后才点的 now 要能生效。
	if err := f.hub.Decide(id, "now"); err != nil {
		t.Fatal(err)
	}
	got, ok := f.hub.TakeOutbox(ctx, "mac:amy")
	if !ok {
		t.Fatalf("点 now 后忙中也应取到")
	}
	if !got.Now {
		t.Errorf("TakeOutbox 应返回 h.msgs 里的当前值（Now=true），实际 %+v", got)
	}
	if string(got.File) != "p" {
		t.Errorf("取走应带 File，实际 %q", got.File)
	}
	if err := f.hub.MarkDelivered(id); err != nil {
		t.Fatal(err)
	}
}

// 复核 3：被拒绝的消息即使已放进 outbox 也不能被取走，脏条目要清掉。
func TestOutboxSkipsRejected(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "mac:amy"
	id1, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id1, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id2, "approve"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx) // outbox: [id1, id2]
	if err := f.hub.Decide(id1, "reject"); err != nil {
		t.Fatal(err)
	}
	// 被拒绝的那条取不到，且不影响后面正常条目
	got, ok := f.hub.TakeOutbox(ctx, "mac:amy")
	if !ok || got.ID != id2 {
		t.Fatalf("应跳过被拒绝的 id1 取到 id2，实际 ok=%v got=%+v", ok, got)
	}
	if err := f.hub.MarkDelivered(id2); err != nil {
		t.Fatal(err)
	}
	if state := stateOf(t, f.hub, id1); state != MsgRejected {
		t.Errorf("id1 应保持 rejected，实际 %s", state)
	}
	if err := f.hub.MarkDelivered(id1); err == nil {
		t.Errorf("rejected 不能 MarkDelivered")
	}
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, ok := f.hub.TakeOutbox(shortCtx, "mac:amy"); ok {
		t.Errorf("被拒绝的消息不该还留在 outbox 里")
	}
}

// 复核 2：同步成功到送达之间不能有空档让第二个 Pump 双发。
// Sync、Deliver 都用信道卡在关键位置，窗口是确定性的而非赌时序。
// 卡住的 TakeOutbox 必须被"立刻叫醒"覆盖：decide-now、SetState(idle)、reject、
// 淘汰清理都要让等待中的长轮询重新扫描；ctx 到期立刻返回 false。
func TestBlockedTakeOutboxWakesOnDecideNow(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "mac:amy"
	ids := make([]string, 3)
	for i := range ids {
		id, err := f.hub.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.hub.Decide(id, "approve"); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	f.hub.Pump(ctx) // 空闲时一条 Pump 全部入队
	if _, ok := f.hub.TakeOutbox(ctx, "mac:amy"); !ok || f.hub.Peers()["mac:amy"] != PeerBusy {
		t.Fatalf("第一条应取到并置忙")
	}
	// amy 忙，outbox 里还剩两条非 now：长轮询必须卡住
	res := make(chan Message, 1)
	pollCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() {
		if msg, ok := f.hub.TakeOutbox(pollCtx, "mac:amy"); ok {
			res <- msg
		} else {
			close(res)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case msg := <-res:
		t.Fatalf("忙时不该取到第二条，实际 %+v", msg)
	default:
	}
	// decide-now 第三条：必须立刻叫醒，且跳过前面非 now 的拿第三条
	if err := f.hub.Decide(ids[2], "now"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg, ok := <-res:
		if !ok || msg.ID != ids[2] || !msg.Now {
			t.Fatalf("decide-now 后应立刻取到第三条，实际 ok=%v msg=%+v", ok, msg)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("decide-now 没叫醒阻塞中的长轮询")
	}
	if err := f.hub.MarkDelivered(ids[2]); err != nil {
		t.Fatal(err)
	}
	// ctx 到期：阻塞的调用要立刻结束，不泄漏
	pollCtx2, cancel2 := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel2()
	start := time.Now()
	if _, ok := f.hub.TakeOutbox(pollCtx2, "mac:amy"); ok {
		t.Errorf("忙时不该取到")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("ctx 到期后应立刻返回，实际等了 %v", el)
	}
}

// 复核：第一条被取走置忙后，SetState(idle) 叫醒长轮询，立刻能继续取第二条。
func TestBlockedTakeOutboxWakesOnIdle(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "mac:amy"
	id1, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id1, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id2, "approve"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx) // 两条都进 outbox
	if _, ok := f.hub.TakeOutbox(ctx, "mac:amy"); !ok {
		t.Fatalf("第一条应取到")
	}
	res := make(chan Message, 1)
	pollCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() {
		if msg, ok := f.hub.TakeOutbox(pollCtx, "mac:amy"); ok {
			res <- msg
		} else {
			close(res)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case msg := <-res:
		t.Fatalf("忙时不该取到第二条，实际 %+v", msg)
	default:
	}
	if err := f.hub.SetState("mac:amy", PeerIdle); err != nil {
		t.Fatal(err)
	}
	select {
	case msg, ok := <-res:
		if !ok || msg.ID != id2 {
			t.Fatalf("SetState(idle) 后应立刻取到第二条，实际 ok=%v msg=%+v", ok, msg)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("SetState(idle) 没叫醒阻塞中的长轮询")
	}
	if err := f.hub.MarkDelivered(id1); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.MarkDelivered(id2); err != nil {
		t.Fatal(err)
	}
}

// 复核：reject 和淘汰不能把 outbox 里的脏东西漏给阻塞或后续的 TakeOutbox。
func TestBlockedTakeOutboxRejectAndTrim(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "mac:amy"
	ids := make([]string, 4)
	for i := range ids {
		id, err := f.hub.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.hub.Decide(id, "approve"); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	f.hub.Pump(ctx)
	if _, ok := f.hub.TakeOutbox(ctx, "mac:amy"); !ok { // 取走第一条，amy 置忙
		t.Fatalf("第一条应取到")
	}
	// outbox 剩 ids[1..3]，amy 忙：阻塞两个长轮询，都不该拿到
	res := make(chan Message, 2)
	for i := 0; i < 2; i++ {
		pollCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		go func() {
			defer cancel()
			if msg, ok := f.hub.TakeOutbox(pollCtx, "mac:amy"); ok {
				res <- msg
			} else {
				res <- Message{ID: "__none__"}
			}
		}()
	}
	// reject 掉其中一条 queued 的（已在 outbox）
	if err := f.hub.Decide(ids[1], "reject"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		msg := <-res
		if msg.ID != "__none__" {
			t.Fatalf("忙时被阻塞的长轮询不该拿到消息，实际 %+v", msg)
		}
	}
	if state := stateOf(t, f.hub, ids[1]); state != MsgRejected {
		t.Fatalf("ids[1] 应 rejected，实际 %s", state)
	}
	// 空闲后逐条取：rejected 的绝不出现，outbox 脏条目已被 reject 清掉
	for i := 0; i < 2; i++ {
		if err := f.hub.SetState("mac:amy", PeerIdle); err != nil {
			t.Fatal(err)
		}
		msg, ok := f.hub.TakeOutbox(ctx, "mac:amy")
		if !ok {
			t.Fatalf("空闲后第 %d 条没取到", i)
		}
		if msg.ID == ids[1] {
			t.Fatalf("被拒绝的消息被取走了！")
		}
		if err := f.hub.MarkDelivered(msg.ID); err != nil {
			t.Fatalf("MarkDelivered(%s): %v", msg.ID, err)
		}
	}
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, ok := f.hub.TakeOutbox(shortCtx, "mac:amy"); ok {
		t.Fatalf("outbox 应已清空")
	}
}

// 复核：auto 直接 queued 的 mac 消息一条 Pump 全部入队；Hub 淘汰时把已入队的
// 最早消息连带 outbox 条目一起清掉，等待/后续的 TakeOutbox 拿不到被淘汰的 ID。
func TestAutoEnqueuedAllAndTrimCleans(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	if err := f.hub.SetAuto(60); err != nil {
		t.Fatal(err)
	}
	m := okMsg()
	m.To = "mac:amy"
	ids := make([]string, 200)
	for i := range ids {
		m.Text = fmt.Sprintf("t%03d", i)
		id, err := f.hub.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		if state := stateOf(t, f.hub, id); state != MsgQueued {
			t.Fatalf("auto 模式应 queued，实际 %s", state)
		}
		ids[i] = id
	}
	f.hub.Pump(ctx) // 一条 Pump 全部入队
	// 先卡住一个长轮询（amy 忙），再触发淘汰
	if err := f.hub.SetState("mac:amy", PeerBusy); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan Message, 1)
	pollCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	go func() {
		msg, ok := f.hub.TakeOutbox(pollCtx, "mac:amy")
		if !ok {
			msg = Message{ID: "__none__"}
		}
		blocked <- msg
	}()
	// 第 201 条触发淘汰：没有已结束的消息，删最早的 queued（ids[0]，已在 outbox）
	m.Text = "t200"
	overflow, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	// 淘汰是整条删除，不是置 rejected——List 里不该再有
	for _, lm := range f.hub.List() {
		if lm.ID == ids[0] {
			t.Fatalf("最早的 queued 应被整条淘汰")
		}
	}
	msg := <-blocked
	if msg.ID != "__none__" {
		t.Fatalf("amy 忙时长轮询不该拿到，实际 %+v", msg)
	}
	// 放回空闲，把淘汰后剩下的 queued（含 overflow）全部 Pump 入队
	if err := f.hub.SetState("mac:amy", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	// 逐条取完：应恰好是 ids[1:] + overflow，共 200 条，且都能确认送达
	taken := map[string]bool{}
	for i := 0; i < 200; i++ {
		if i > 0 {
			if err := f.hub.SetState("mac:amy", PeerIdle); err != nil {
				t.Fatal(err)
			}
		}
		tm, ok := f.hub.TakeOutbox(ctx, "mac:amy")
		if !ok {
			t.Fatalf("第 %d 轮没取到", i)
		}
		taken[tm.ID] = true
		if err := f.hub.MarkDelivered(tm.ID); err != nil {
			t.Fatalf("MarkDelivered(%s): %v", tm.ID, err)
		}
	}
	if taken[ids[0]] {
		t.Errorf("被淘汰的消息不应被取到")
	}
	if !taken[overflow] {
		t.Errorf("第 201 条入队后应能被取到")
	}
	if len(taken) != 200 {
		t.Errorf("应取到 200 条，实际 %d", len(taken))
	}
	shortCtx, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	if _, ok := f.hub.TakeOutbox(shortCtx, "mac:amy"); ok {
		t.Errorf("全部取完后 outbox 应为空（auto 过期前发的都已处理）")
	}
	// auto 过期验证：到期后新消息不再 queued，也就不该进 outbox
	f.clock.Add(61 * time.Minute)
	late, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if state := stateOf(t, f.hub, late); state != MsgPending {
		t.Errorf("auto 过期后应回 pending，实际 %s", state)
	}
	f.hub.Pump(ctx)
	shortCtx2, cancel3 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel3()
	if _, ok := f.hub.TakeOutbox(shortCtx2, "mac:amy"); ok {
		t.Errorf("pending 的消息不该进 outbox")
	}
}

func TestNoDoubleSendAfterSync(t *testing.T) {
	var mu sync.Mutex
	var syncCalls, deliverCalls int
	syncStarted := make(chan struct{})
	syncRelease := make(chan struct{})
	deliverStarted := make(chan struct{})
	deliverRelease := make(chan struct{})
	h := NewHub(HubConfig{
		Sync: func(context.Context, Message) error {
			mu.Lock()
			syncCalls++
			mu.Unlock()
			close(syncStarted)
			<-syncRelease
			return nil
		},
		Deliver: func(_ context.Context, m Message) error {
			mu.Lock()
			deliverCalls++
			mu.Unlock()
			close(deliverStarted)
			<-deliverRelease
			return nil
		},
	})
	m := okMsg()
	m.To = "ubuntu:bob"
	m.Repo = "base"
	m.Branch = "feat/msg"
	id, err := h.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	pumpDone := make(chan struct{})
	go func() {
		h.Pump(context.Background())
		close(pumpDone)
	}()
	<-syncStarted                // pump1 卡在 Sync 里
	h.Pump(context.Background()) // 同步进行中：inflight 挡住，无事发生
	close(syncRelease)           // pump1 同步成功，走向 Deliver
	<-deliverStarted             // 此刻已进入"同步完成、尚未送完"的空档
	h.Pump(context.Background()) // 没有 inflight 的话这里就会第二次 Deliver
	close(deliverRelease)
	<-pumpDone
	mu.Lock()
	defer mu.Unlock()
	if syncCalls != 1 {
		t.Errorf("Sync 应只调 1 次，实际 %d", syncCalls)
	}
	if deliverCalls != 1 {
		t.Errorf("Deliver 应只调 1 次（双发！），实际 %d", deliverCalls)
	}
	if state := stateOf(t, h, id); state != MsgDelivered {
		t.Errorf("最终应 delivered，实际 %s", state)
	}
}

// 复核 2：同步期间用户 reject，回调回来后发现不是 queued 就收尾，不能再送达。
func TestRejectDuringSync(t *testing.T) {
	var mu sync.Mutex
	var deliverCalls int
	syncStarted := make(chan struct{})
	syncRelease := make(chan struct{})
	h := NewHub(HubConfig{
		Sync: func(context.Context, Message) error {
			close(syncStarted)
			<-syncRelease
			return nil
		},
		Deliver: func(context.Context, Message) error {
			mu.Lock()
			defer mu.Unlock()
			deliverCalls++
			return nil
		},
	})
	m := okMsg()
	m.To = "ubuntu:bob"
	m.Repo = "base"
	m.Branch = "feat/msg"
	id, err := h.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	pumpDone := make(chan struct{})
	go func() {
		h.Pump(context.Background())
		close(pumpDone)
	}()
	<-syncStarted
	if err := h.Decide(id, "reject"); err != nil {
		t.Fatal(err)
	}
	close(syncRelease)
	<-pumpDone
	mu.Lock()
	defer mu.Unlock()
	if deliverCalls != 0 {
		t.Errorf("同步期间被拒绝后不应送达，实际 Deliver 调了 %d 次", deliverCalls)
	}
	if state := stateOf(t, h, id); state != MsgRejected {
		t.Errorf("应保持 rejected，实际 %s", state)
	}
}

func TestDeliverFailKeepsQueued(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	boom := errors.New("tmux 满了")
	f.setDeliverErr(boom)
	m := okMsg()
	m.To = "ubuntu:bob"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if state := stateOf(t, f.hub, id); state != MsgQueued {
		t.Fatalf("Deliver 失败应保持 queued，实际 %s", state)
	}
	for _, lm := range f.hub.List() {
		if lm.ID == id && lm.Reason != boom.Error() {
			t.Errorf("Reason 应记错误原文，实际 %q", lm.Reason)
		}
	}
	if f.hub.Peers()["ubuntu:bob"] == PeerBusy {
		t.Errorf("送达失败不应置忙")
	}
	f.setDeliverErr(nil)
	f.hub.Pump(ctx)
	if state := stateOf(t, f.hub, id); state != MsgDelivered {
		t.Fatalf("第二次 Pump 应成功，实际 %s", state)
	}
	if n := len(f.delivered()); n != 2 {
		t.Errorf("Deliver 应被调 2 次，实际 %d", n)
	}
}

func TestCapacityEvicts(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	// 全 pending：超限时删最早的
	var ids []string
	for i := 0; i < 205; i++ {
		m := okMsg()
		m.To = "ubuntu:bob"
		m.Text = fmt.Sprintf("m%03d", i)
		id, err := f.hub.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	list := f.hub.List()
	if len(list) != 200 {
		t.Fatalf("应保留 200 条，实际 %d", len(list))
	}
	if list[0].Text != "m204" {
		t.Errorf("最新一条应是 m204，实际 %s", list[0].Text)
	}
	for _, lm := range list {
		if lm.Text == "m004" || lm.Text == "m000" {
			t.Errorf("最早的应被淘汰")
		}
	}

	// 全 delivered：超限时优先删最早的已结束消息
	// 走 mac: outbox：Pump 把 200 条 queued 全放进 outbox（不会互相置忙挡路），
	// 再逐条 TakeOutbox + MarkDelivered，得到 200 条 delivered。
	f2 := newMsgFixture()
	if err := f2.hub.SetAuto(60); err != nil {
		t.Fatal(err)
	}
	var first string
	for i := 0; i < 200; i++ {
		m := okMsg()
		m.Text = fmt.Sprintf("d%03d", i)
		id, err := f2.hub.Send(m)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = id
		}
	}
	f2.hub.Pump(ctx)
	for i := 0; i < 200; i++ {
		// 取走一条会把 alice 置忙，非 now 的下一条会被按住——先放回空闲
		if err := f2.hub.SetState("mac:alice", PeerIdle); err != nil {
			t.Fatal(err)
		}
		msg, ok := f2.hub.TakeOutbox(ctx, "mac:alice")
		if !ok {
			t.Fatalf("outbox 第 %d 条没取到", i)
		}
		if err := f2.hub.MarkDelivered(msg.ID); err != nil {
			t.Fatalf("MarkDelivered(%s): %v", msg.ID, err)
		}
	}
	if n := len(f2.hub.List()); n != 200 {
		t.Fatalf("200 条 delivered 未超限，不应淘汰，实际 %d", n)
	}
	id201, err := f2.hub.Send(okMsg())
	if err != nil {
		t.Fatal(err)
	}
	list = f2.hub.List()
	if len(list) != 200 {
		t.Fatalf("应保留 200 条，实际 %d", len(list))
	}
	for _, lm := range list {
		if lm.ID == first {
			t.Errorf("最早的 delivered 应被淘汰")
		}
	}
	if list[0].ID != id201 || stateOf(t, f2.hub, id201) != MsgQueued {
		t.Errorf("新消息应在且 queued，实际 %v %s", list[0].ID == id201, stateOf(t, f2.hub, id201))
	}
}

func TestDecideErrors(t *testing.T) {
	f := newMsgFixture()
	if err := f.hub.Decide("deadbeefdeadbeef", "approve"); !errors.Is(err, ErrMsgNotFound) {
		t.Errorf("不存在的 ID 应返回 ErrMsgNotFound，实际 %v", err)
	}
	id, err := f.hub.Send(okMsg())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "teleport"); err == nil {
		t.Errorf("未知 action 应报错")
	}
	if err := f.hub.Decide(id, "retry_sync"); err == nil {
		t.Errorf("pending 不能 retry_sync")
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err == nil {
		t.Errorf("queued 不能重复 approve")
	}
	if err := f.hub.Decide(id, "reject"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "now"); err == nil {
		t.Errorf("rejected 不能再 now")
	}
	if err := f.hub.MarkDelivered(id); err == nil {
		t.Errorf("rejected 不能 MarkDelivered")
	}
	if err := f.hub.SetState("bad:addr", PeerBusy); err == nil {
		t.Errorf("SetState 应校验地址")
	}
	if err := f.hub.SetState("mac:alice", PeerState("away")); err == nil {
		t.Errorf("SetState 应校验状态")
	}
}

func TestPeersCopy(t *testing.T) {
	f := newMsgFixture()
	if err := f.hub.SetState("mac:alice", PeerBusy); err != nil {
		t.Fatal(err)
	}
	p := f.hub.Peers()
	p["mac:bob"] = PeerBusy
	delete(p, "mac:alice")
	if f.hub.Peers()["mac:bob"] != "" {
		t.Errorf("Peers 应是拷贝")
	}
	if f.hub.Peers()["mac:alice"] != PeerBusy {
		t.Errorf("Peers 修改不应影响内部")
	}
	// 没上报过的地址不在表里（调用方按缺省 idle 处理）
	if _, ok := f.hub.Peers()["ubuntu:carol"]; ok {
		t.Errorf("没上报过的不应出现")
	}
}

func TestConcurrentAccess(t *testing.T) {
	f := newMsgFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				m := okMsg()
				m.To = fmt.Sprintf("ubuntu:peer%d", i%2)
				if i%2 == 1 {
					m.To = fmt.Sprintf("mac:peer%d", i%2)
					m.FileName = "f.txt"
					m.File = []byte("x")
				}
				id, err := f.hub.Send(m)
				if err == nil && n%3 == 0 {
					_ = f.hub.Decide(id, "approve")
				}
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				f.hub.Pump(ctx)
				time.Sleep(time.Millisecond)
			}
		}()
	}
	markErrs := make(chan error, 64)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
				if msg, ok := f.hub.TakeOutbox(c, fmt.Sprintf("mac:peer%d", i)); ok {
					// 取走的必然还是 queued：MarkDelivered 不能报状态冲突；
					// 只有"并发淘汰把整条删了"导致的 ErrMsgNotFound 是可接受的
					if err := f.hub.MarkDelivered(msg.ID); err != nil && !errors.Is(err, ErrMsgNotFound) {
						select {
						case markErrs <- fmt.Errorf("MarkDelivered(%s): %v", msg.ID, err):
						default:
						}
					}
				}
				cancel()
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = f.hub.List()
			_ = f.hub.Peers()
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	for len(markErrs) > 0 {
		t.Errorf("%v", <-markErrs)
	}
	if n := len(f.hub.List()); n > 200 {
		t.Errorf("并发下不得超过 200 条，实际 %d", n)
	}
}
