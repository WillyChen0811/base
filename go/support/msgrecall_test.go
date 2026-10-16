package support

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 把一条 ubuntu:bob 收件的消息发出去并送达（delivered）。
func recallDeliver(t *testing.T, f *msgFixture, m Message) string {
	t.Helper()
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(context.Background())
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy)
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("应 delivered，实际 %s", st)
	}
	return id
}

// 未送达（pending / queued）撤回：状态 recalled，对方永远收不到，也没有系统通知。
func TestRecallUndeliveredNeverReaches(t *testing.T) {
	f := newMsgFixture()
	m := okMsg()
	m.To = "ubuntu:bob"
	pend, _ := f.hub.Send(m)
	que, _ := f.hub.Send(m)
	if err := f.hub.Decide(que, "approve"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pend, que} {
		if err := f.hub.Recall(id, m.From); err != nil {
			t.Fatalf("撤回 %s: %v", id, err)
		}
		if st := stateOf(t, f.hub, id); st != MsgRecalled {
			t.Fatalf("应 recalled，实际 %s", st)
		}
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(context.Background())
	if n := len(f.delivered()); n != 0 {
		t.Fatalf("撤回的消息不应送达，实际 %d 条", n)
	}
	if n := len(hubNotices(f.hub)); n != 0 {
		t.Fatalf("未送达撤回不该有系统通知，实际 %d", n)
	}
}

// 发往 mac: 且还躺在 outbox 里的消息撤回后取不到。
func TestRecallRemovesFromOutbox(t *testing.T) {
	f := newMsgFixture()
	id, _ := f.hub.Send(Message{From: "ubuntu:bob", To: "mac:alice", Text: "x"})
	if err := f.hub.Decide(id, "now"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Recall(id, "ubuntu:bob"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m, ok := f.hub.TakeOutbox(ctx, "mac:alice"); ok {
		t.Fatalf("撤回后不应能取到: %+v", m)
	}
}

// 已送达撤回：状态 recalled、原文保留，收件人收到系统通知。
func TestRecallDeliveredNotifiesRecipient(t *testing.T) {
	f := newMsgFixture()
	m := okMsg()
	m.To = "ubuntu:bob"
	id := recallDeliver(t, f, m)
	if err := f.hub.Recall(id, m.From); err != nil {
		t.Fatal(err)
	}
	got := peekHubMsg(t, f.hub, id)
	if got.State != MsgRecalled || got.Text != "hello" {
		t.Fatalf("应 recalled 且保留原文，实际 %s %q", got.State, got.Text)
	}
	ns := hubNotices(f.hub)
	if len(ns) != 1 {
		t.Fatalf("应有一条系统通知，实际 %d", len(ns))
	}
	if ns[0].To != "ubuntu:bob" || ns[0].State != MsgQueued || !strings.Contains(ns[0].Text, "#"+id+" 已被发件人撤回") {
		t.Errorf("通知不对: %+v", ns[0])
	}
	// 通知能照常投递给收件人
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(context.Background())
	d := f.delivered()
	if len(d) != 2 || d[1].From != HubSenderAddr {
		t.Fatalf("收件人应再收到系统通知，实际 %+v", d)
	}
}

// injected（已进终端未确认）撤回也算已送达：补通知。
func TestRecallInjectedNotifies(t *testing.T) {
	f := newMsgFixture()
	m := okMsg()
	m.To = "ubuntu:bob"
	id, _ := f.hub.Send(m)
	_ = f.hub.Decide(id, "approve")
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(context.Background())
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("应 injected，实际 %s", st)
	}
	if err := f.hub.Recall(id, m.From); err != nil {
		t.Fatal(err)
	}
	if len(hubNotices(f.hub)) != 1 {
		t.Fatalf("injected 撤回应补通知")
	}
	// 之后对端上报，不会把 recalled 又改回 delivered
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy)
	if st := stateOf(t, f.hub, id); st != MsgRecalled {
		t.Fatalf("应保持 recalled，实际 %s", st)
	}
}

func TestRecallErrors(t *testing.T) {
	f := newMsgFixture()
	m := okMsg()
	m.To = "ubuntu:bob"
	id, _ := f.hub.Send(m)
	if err := f.hub.Recall(id, "ubuntu:other"); !errors.Is(err, ErrNotSender) {
		t.Fatalf("非发件人应 ErrNotSender，实际 %v", err)
	}
	if err := f.hub.Recall(id, "mac:alice"); !errors.Is(err, ErrNotSender) {
		t.Fatalf("收件人不能撤回对方发的消息: %v", err)
	}
	if err := f.hub.Recall("nope", m.From); !errors.Is(err, ErrMsgNotFound) {
		t.Fatalf("查无此条: %v", err)
	}
	if err := f.hub.Recall(id, m.From); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Recall(id, m.From); err == nil || !strings.Contains(err.Error(), "recalled") {
		t.Fatalf("重复撤回应报错，实际 %v", err)
	}
	rej, _ := f.hub.Send(m)
	_ = f.hub.Decide(rej, "reject")
	if err := f.hub.Recall(rej, m.From); err == nil {
		t.Fatalf("已拒绝不能撤回")
	}
	if n := len(hubNotices(f.hub)); n != 0 {
		t.Fatalf("失败的撤回不该产生通知")
	}
}

// 撤回的请求不再计入超时未回，也不进 Unanswered。
func TestRecallRequestNotOverdue(t *testing.T) {
	f := newMsgFixture()
	id := recallDeliver(t, f, Message{From: "mac:alice", To: "ubuntu:bob", Kind: KindRequest, Text: "请求"})
	if err := f.hub.Recall(id, "mac:alice"); err != nil {
		t.Fatal(err)
	}
	n0 := len(hubNotices(f.hub))
	f.clock.Add(defaultReplyTimeout + 1)
	f.hub.Pump(context.Background())
	if peekHubMsg(t, f.hub, id).Overdue {
		t.Fatalf("撤回的请求不该标 overdue")
	}
	if len(hubNotices(f.hub)) != n0 {
		t.Fatalf("不该再产生超时通知")
	}
	if len(f.hub.Unanswered("ubuntu:bob")) != 0 {
		t.Fatalf("撤回的请求不该算欠回复")
	}
}

// 撤回的回复：原请求重新算未回复，且可以再回复。
func TestRecallReplyAllowsReReply(t *testing.T) {
	f := newMsgFixture()
	reqID := recallDeliver(t, f, Message{From: "mac:alice", To: "ubuntu:bob", Kind: KindRequest, Text: "请求"})
	rep, err := f.hub.Send(Message{From: "ubuntu:bob", Kind: KindReply, ReplyTo: reqID, Text: "答"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Recall(rep, "ubuntu:bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Send(Message{From: "ubuntu:bob", Kind: KindReply, ReplyTo: reqID, Text: "重答"}); err != nil {
		t.Fatalf("撤回后应能重新回复: %v", err)
	}
}

// API：本机/远端都挂了 /msg/recall，远端只能撤回 ubuntu: 的消息。
func TestMsgRecallAPI(t *testing.T) {
	a := newMsgAPI(t, 30) // 自动模式开着，发出即 queued
	_, body := post(t, a.lURL+"/send", msgBody("mac:frank", "ubuntu:bob", "x", nil, ""))
	var r map[string]string
	_ = json.Unmarshal(body, &r)
	id := r["id"]
	rb := func(id, from string) []byte {
		b, _ := json.Marshal(msgRecallReq{ID: id, From: from})
		return b
	}
	// 远端不能撤回 mac 的消息（身份校验：from 必须 ubuntu:）
	if code, _ := remoteDo(t, "POST", a.base+"/msg/recall", a.token, rb(id, "mac:frank")); code != 403 {
		t.Fatalf("远端冒充 mac 应 403，实际 %d", code)
	}
	if code, _ := remoteDo(t, "POST", a.base+"/msg/recall", a.token, rb(id, "ubuntu:bob")); code != 403 {
		t.Fatalf("远端非发件人应 403，实际 %d", code)
	}
	if code, _ := post(t, a.lURL+"/recall", rb(id, "mac:frank")); code != 200 {
		t.Fatalf("本机撤回应 200，实际 %d", code)
	}
	if st := msgStateOf(t, a.lURL, id); st != MsgRecalled {
		t.Fatalf("列表里应 recalled，实际 %s", st)
	}
	if code, _ := post(t, a.lURL+"/recall", rb(id, "mac:frank")); code != 409 {
		t.Fatalf("重复撤回应 409，实际 %d", code)
	}
	if code, _ := post(t, a.lURL+"/recall", rb("nope", "mac:frank")); code != 404 {
		t.Fatalf("查无此条应 404，实际 %d", code)
	}
	// 远端撤回自己发的
	_, body = remoteDo(t, "POST", a.base+"/msg/send", a.token, msgBody("ubuntu:bob", "mac:frank", "y", nil, ""))
	_ = json.Unmarshal(body, &r)
	if code, b := remoteDo(t, "POST", a.base+"/msg/recall", a.token, rb(r["id"], "ubuntu:bob")); code != 200 {
		t.Fatalf("远端撤回自己的应 200，实际 %d %s", code, b)
	}
}
