package support

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件专门覆盖送达确认（injected）与请求欠回复超时（overdue）：
// 非 Now 消息注入成功只进 injected，对端下一次上报才落 delivered；
// AckTimeout 内没有任何上报按 ack 阶段失败重回 queued，预算（MaxAttempts）用完转 failed
// 并抓屏出报告；Now 消息注入成功直接 delivered；reply 只有送达才把原请求标 Answered；
// 请求送达后超过 ReplyTimeout 没回复标 overdue 并由 xxyi:hub 发系统通知（只发一次）；
// injected 进度经落盘/重启后还在。时钟用 msg_test.go 的 fakeClock，
// 写法照抄 msgretry_test.go（Pump 前把时钟推过退避点，退避 2 秒起翻倍）。

// peekAckProgress 读一条消息等确认的进度（injectedAt/ackSeq，不落盘的内存表）。
func peekAckProgress(t *testing.T, h *Hub, id string) (time.Time, uint64, bool) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	at, ok := h.injectedAt[id]
	return at, h.ackSeq[id], ok
}

// hubNotices 挑出 List 里所有 Hub 自己发的系统通知。
func hubNotices(h *Hub) []Message {
	var out []Message
	for _, m := range h.List() {
		if m.From == HubSenderAddr {
			out = append(out, m)
		}
	}
	return out
}

// 1) 发往 ubuntu: 的非 Now 消息：Deliver 成功只到 injected，
// 对端再上报（哪怕报的是 busy）才算 delivered。
func TestAckNonNowWaitsPeerReport(t *testing.T) {
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
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("Deliver 成功非 Now 应先进 injected，实际 %s", st)
	}
	if n := len(f.delivered()); n != 1 {
		t.Errorf("Deliver 应调 1 次，实际 %d", n)
	}
	if _, _, ok := peekAckProgress(t, f.hub, id); !ok {
		t.Errorf("injected 应记下等确认的进度（injectedAt）")
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy) // 任何一次新上报都算对方接住了
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("对端上报后应 delivered，实际 %s", st)
	}
	if _, _, ok := peekAckProgress(t, f.hub, id); ok {
		t.Errorf("delivered 后等确认进度应清掉")
	}
}

// 2) 注入后对端一直没上报：每超过 AckTimeout 记一次 Stage=ack 失败重回 queued 并置忙对端，
// 重新送达又要重新等；ack 预算（defaultMaxAttempts 次）用完转 failed，
// 报告 Stage=ack、画面来自 Capture，Capture 只在转人工时调一次。
func TestAckTimeoutConsumesBudgetToFailed(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	var mu sync.Mutex
	var captureCalls int
	var captureAddr string
	f.hub.cfg.Capture = func(_ context.Context, addr string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		captureCalls++
		captureAddr = addr
		return "注入后一直没动静的画面", nil
	}
	m := okMsg()
	m.To = "ubuntu:bob"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("首次注入成功应为 injected，实际 %s", st)
	}

	// 第 1 次 ack 超时：回 queued，Attempts 多一条 Stage=ack，对端被置 busy。
	f.clock.Add(defaultAckTimeout + time.Second)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("注入后超过 AckTimeout 没上报应回 queued，实际 %s", st)
	}
	lm := peekHubMsg(t, f.hub, id)
	if len(lm.Attempts) != 1 {
		t.Fatalf("第一次确认超时应记 1 条 Attempt，实际 %d", len(lm.Attempts))
	}
	if lm.Attempts[0].Stage != "ack" {
		t.Errorf("Attempt.Stage 应为 ack，实际 %q", lm.Attempts[0].Stage)
	}
	if !strings.Contains(lm.Reason, "没开始处理") {
		t.Errorf("Reason 应写明确认超时原因，实际 %q", lm.Reason)
	}
	if got := f.hub.Peers()["ubuntu:bob"]; got != PeerBusy {
		t.Errorf("确认超时后对端应被置 busy 按住重投，实际 %s", got)
	}

	// 第 2..5 次：上报 idle 恢复可送 → 推过退避（2,4,8,16 秒）重新注入 → 再等满 AckTimeout 又超时。
	for k := 2; k <= defaultMaxAttempts; k++ {
		ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
		f.clock.Add((2 << uint(k-2)) * time.Second)
		f.hub.Pump(ctx)
		if st := stateOf(t, f.hub, id); st != MsgInjected {
			t.Fatalf("第 %d 轮退避后应重新注入为 injected，实际 %s", k, st)
		}
		if n := len(f.delivered()); n != k {
			t.Fatalf("第 %d 轮应累计调 Deliver %d 次，实际 %d", k, k, n)
		}
		f.clock.Add(defaultAckTimeout + time.Second)
		f.hub.Pump(ctx)
		want := MsgQueued
		if k == defaultMaxAttempts {
			want = MsgFailed
		}
		if st := stateOf(t, f.hub, id); st != want {
			t.Fatalf("第 %d 次确认超时状态应为 %s，实际 %s", k, want, st)
		}
		lm = peekHubMsg(t, f.hub, id)
		if len(lm.Attempts) != k {
			t.Fatalf("第 %d 次确认后 Attempts 应记 %d 条，实际 %d", k, k, len(lm.Attempts))
		}
		for i, a := range lm.Attempts {
			if a.Stage != "ack" {
				t.Errorf("第 %d 条 Attempt 的 Stage 应为 ack，实际 %q", i+1, a.Stage)
			}
		}
	}
	lm = peekHubMsg(t, f.hub, id)
	if lm.Report == nil {
		t.Fatalf("ack 预算用完转人工应带 FailReport")
	}
	if lm.Report.Stage != "ack" {
		t.Errorf("Report.Stage 应为 ack，实际 %q", lm.Report.Stage)
	}
	if lm.Report.Screen != "注入后一直没动静的画面" {
		t.Errorf("Report.Screen 应为 Capture 的返回值，实际 %q", lm.Report.Screen)
	}
	if lm.Report.PeerState != string(PeerBusy) {
		t.Errorf("确认超时期间对端是 busy，Report.PeerState 应为 busy，实际 %q", lm.Report.PeerState)
	}
	mu.Lock()
	calls, addr := captureCalls, captureAddr
	mu.Unlock()
	if calls != 1 {
		t.Errorf("Capture 应只在转人工时调 1 次，实际 %d", calls)
	}
	if addr != "ubuntu:bob" {
		t.Errorf("Capture 应抓接收方地址，实际 %q", addr)
	}
	// failed 之后彻底收尾：再推时钟 Pump 不重投也不重抓。
	f.clock.Add(time.Hour)
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != defaultMaxAttempts {
		t.Errorf("failed 后不该再投递，实际共调了 %d 次", n)
	}
	mu.Lock()
	calls = captureCalls
	mu.Unlock()
	if calls != 1 {
		t.Errorf("failed 后不该再抓屏，实际 Capture 调了 %d 次", calls)
	}
}

// 3) Now 消息（用户强送）不做确认这一步：Deliver 成功直接 delivered，
// 不进 injected、不记等确认的进度。
func TestAckNowSkipsInjected(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "ubuntu:bob"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "now"); err != nil { // Now 只能经 decide now 强送
		t.Fatal(err)
	}
	f.hub.Pump(ctx) // 没上报过对端也照送（Now 绕过忙闲闸门）
	if n := len(f.delivered()); n != 1 {
		t.Fatalf("Now 应直接投递，实际 Deliver 调了 %d 次", n)
	}
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("Now 注入成功应直接 delivered，实际 %s", st)
	}
	if _, _, ok := peekAckProgress(t, f.hub, id); ok {
		t.Errorf("Now 不该记 injected 的等确认进度")
	}
}

// 4) mac: 走 outbox：取走并 MarkDelivered 后同样只进 injected，
// 收件端钩子再上报才落 delivered。
func TestAckMacOutboxWaitsPeerReport(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	m := okMsg() // To=mac:alice
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f.hub, "mac:alice", PeerIdle)
	f.hub.Pump(ctx)
	got, ok := f.hub.TakeOutbox(ctx, "mac:alice")
	if !ok || got.ID != id {
		t.Fatalf("Pump 后应能取到消息，ok=%v got=%+v", ok, got)
	}
	if err := f.hub.MarkDelivered(id); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("非 Now 的 outbox 消息 MarkDelivered 后应为 injected，实际 %s", st)
	}
	ackPeer(t, f.hub, "mac:alice", PeerBusy)
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("收件端上报后应 delivered，实际 %s", st)
	}
}

// 5) 回复只有真正送达才算回了：reply 还在 injected 时原请求 Answered 保持 false，
// 对端确认送达后才标成 true。
func TestAckReplyAnswersRequestOnlyOnDelivery(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	reqID, err := f.hub.Send(Message{From: "ubuntu:macsrv", To: "ubuntu:bob", Kind: KindRequest, Text: "帮忙看看"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(reqID, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(ctx)
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy) // 请求先送达
	if st := stateOf(t, f.hub, reqID); st != MsgDelivered {
		t.Fatalf("请求应先 delivered，实际 %s", st)
	}

	replyID, err := f.hub.Send(Message{From: "ubuntu:bob", To: "ubuntu:macsrv", Kind: KindReply, ReplyTo: reqID, Text: "好的"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(replyID, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f.hub, "ubuntu:macsrv", PeerIdle)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, replyID); st != MsgInjected {
		t.Fatalf("回复注入后应为 injected，实际 %s", st)
	}
	if peekHubMsg(t, f.hub, reqID).Answered {
		t.Errorf("回复还在 injected，原请求不该算已回复")
	}
	ackPeer(t, f.hub, "ubuntu:macsrv", PeerBusy)
	if st := stateOf(t, f.hub, replyID); st != MsgDelivered {
		t.Fatalf("对端上报后回复应 delivered，实际 %s", st)
	}
	if !peekHubMsg(t, f.hub, reqID).Answered {
		t.Errorf("回复送达后原请求应标 Answered")
	}
}

// 6) 请求送达后超过 ReplyTimeout 没回复：标 overdue（只标一次）、
// 由 xxyi:hub 给原发件人发一条 queued 的系统通知（Display 头部是系统通知格式）；
// 再 Pump 不重复标也不重复发。对照组：已有一条 pending 的回复（没被拒绝就算回了）不标超时。
func TestReplyTimeoutMarksOverdueOnce(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	reqID, err := f.hub.Send(Message{From: "mac:alice", To: "ubuntu:bob", Kind: KindRequest, Text: "请求"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(reqID, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(ctx)
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy) // 送达确认完成，欠回复计时开始
	if st := stateOf(t, f.hub, reqID); st != MsgDelivered {
		t.Fatalf("请求应先 delivered，实际 %s", st)
	}

	f.clock.Add(defaultReplyTimeout + time.Second)
	f.hub.Pump(ctx)
	req := peekHubMsg(t, f.hub, reqID)
	if !req.Overdue {
		t.Fatalf("送达后超过 ReplyTimeout 没回复应标 overdue")
	}
	notices := hubNotices(f.hub)
	if len(notices) != 1 {
		t.Fatalf("应只有一条系统通知，实际 %d 条", len(notices))
	}
	n := notices[0]
	if n.To != "mac:alice" || n.Kind != KindNotice || n.State != MsgQueued {
		t.Errorf("系统通知应 queued 发给原发件人，实际 To=%s Kind=%s State=%s", n.To, n.Kind, n.State)
	}
	if !strings.Contains(n.Text, reqID) {
		t.Errorf("通知正文应带上超时的请求编号 %s，实际 %q", reqID, n.Text)
	}
	if head := Display(n, ""); !strings.HasPrefix(head, "[XXYI 系统通知 #") {
		t.Errorf("系统通知的 Display 头部应以「[XXYI 系统通知 #」开头，实际 %q", head)
	}
	// 再 Pump 不重复：overdue 只标一次，通知只发一条。
	f.clock.Add(defaultReplyTimeout + time.Second)
	f.hub.Pump(ctx)
	if len(hubNotices(f.hub)) != 1 {
		t.Errorf("再 Pump 不该再生成第二条系统通知")
	}

	// 对照组：请求已有一条 pending 的回复（还没批准），超时后不标 overdue、不发通知。
	f2 := newMsgFixture()
	reqID2, err := f2.hub.Send(Message{From: "mac:alice", To: "ubuntu:bob", Kind: KindRequest, Text: "请求"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f2.hub.Decide(reqID2, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f2.hub, "ubuntu:bob", PeerIdle)
	f2.hub.Pump(ctx)
	ackPeer(t, f2.hub, "ubuntu:bob", PeerBusy)
	if _, err := f2.hub.Send(Message{From: "ubuntu:bob", To: "mac:alice", Kind: KindReply, ReplyTo: reqID2, Text: "草稿回复"}); err != nil {
		t.Fatal(err) // 保持 pending，不 Decide
	}
	f2.clock.Add(defaultReplyTimeout + time.Second)
	f2.hub.Pump(ctx)
	if peekHubMsg(t, f2.hub, reqID2).Overdue {
		t.Errorf("已有 pending 回复的请求不该标 overdue")
	}
	if len(hubNotices(f2.hub)) != 0 {
		t.Errorf("没超时不该发系统通知，实际 %d 条", len(hubNotices(f2.hub)))
	}
}

// 7) 落盘/重启：injected 的进度经快照还在——新 Hub（同一个 fake 时钟）里消息仍是 injected；
// ackSeq 归零，所以之后任何一次上报都算确认（delivered）；
// 一直没上报的那条在新 Hub 里重新等满 AckTimeout 后照样回 queued。
func TestStoreInjectedProgressSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	clock := newFakeClock()
	deliver := func(_ context.Context, _ Message) error { return nil }
	ctx := context.Background()

	h1 := NewHub(HubConfig{Now: clock.Now, Deliver: deliver, StorePath: path})
	idA, err := h1.Send(Message{From: "ubuntu:macsrv", To: "ubuntu:bob", Kind: KindNotice, Text: "A 等确认"})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := h1.Send(Message{From: "ubuntu:macsrv", To: "ubuntu:carol", Kind: KindNotice, Text: "B 等确认"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{idA, idB} {
		if err := h1.Decide(id, "approve"); err != nil {
			t.Fatal(err)
		}
	}
	ackPeer(t, h1, "ubuntu:bob", PeerIdle)
	ackPeer(t, h1, "ubuntu:carol", PeerIdle)
	h1.Pump(ctx)
	if st := stateOf(t, h1, idA); st != MsgInjected {
		t.Fatalf("A 注入后应为 injected，实际 %s", st)
	}
	if st := stateOf(t, h1, idB); st != MsgInjected {
		t.Fatalf("B 注入后应为 injected，实际 %s", st)
	}

	// 同路径同钟新建 Hub：injected 还在（快照里的状态原样恢复）。
	h2 := NewHub(HubConfig{Now: clock.Now, Deliver: deliver, StorePath: path})
	if st := stateOf(t, h2, idA); st != MsgInjected {
		t.Fatalf("重启后 A 应仍是 injected，实际 %s", st)
	}
	if st := stateOf(t, h2, idB); st != MsgInjected {
		t.Fatalf("重启后 B 应仍是 injected，实际 %s", st)
	}
	// 重启期间有没有上报无从判断：ackSeq 归零，新 Hub 上钩子报忙就算确认。
	ackPeer(t, h2, "ubuntu:bob", PeerBusy)
	if st := stateOf(t, h2, idA); st != MsgDelivered {
		t.Fatalf("新 Hub 上 bob 上报后 A 应 delivered，实际 %s", st)
	}
	// carol 一直没上报：在新 Hub 里重新等满 AckTimeout 后按 ack 失败回 queued。
	clock.Add(defaultAckTimeout + time.Second)
	h2.Pump(ctx)
	if st := stateOf(t, h2, idB); st != MsgQueued {
		t.Fatalf("carol 没上报，B 重新等满 AckTimeout 后应回 queued，实际 %s", st)
	}
	lm := peekHubMsg(t, h2, idB)
	if len(lm.Attempts) == 0 || lm.Attempts[len(lm.Attempts)-1].Stage != "ack" {
		t.Errorf("B 回 queued 应记 Stage=ack 的 Attempt，实际 %+v", lm.Attempts)
	}
}
