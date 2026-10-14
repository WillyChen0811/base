package support

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// —— Hub 层：对端上报、消息类型、渲染、收件箱查询 ——

// 没上报过忙闲的对端等同于 busy/gone：自动模式放行了也停在 queued，Deliver 一次都不调；
// 上报 idle 才送出；gone 时按住；被点了 now 则无论什么状态都送。
func TestUnreportedPeerHoldsQueued(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	if err := f.hub.SetAuto(10); err != nil {
		t.Fatal(err)
	}
	m := okMsg()
	m.To = "ubuntu:bob"
	m.Kind = KindRequest
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("自动模式 Send 应 queued，实际 %s", st)
	}
	if _, ok := f.hub.Peers()["ubuntu:bob"]; ok {
		t.Fatalf("前提：ubuntu:bob 没上报过忙闲")
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 0 {
		t.Fatalf("没上报过 idle 不该调 Deliver，实际调了 %d 次", n)
	}
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("应停在 queued，实际 %s", st)
	}
	// gone 也按住
	if err := f.hub.SetState("ubuntu:bob", PeerGone); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 0 {
		t.Fatalf("gone 时不该送达，实际调了 %d 次", n)
	}
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("gone 时应仍 queued，实际 %s", st)
	}
	// 上报 idle 后送出
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("上报 idle 后应注入并 injected，实际 %s", st)
	}
	// 再上报一次才算对方接收了 → delivered
	if err := f.hub.SetState("ubuntu:bob", PeerBusy); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("再次上报后应 delivered，实际 %s", st)
	}
	if n := len(f.delivered()); n != 1 {
		t.Fatalf("应送达 1 条，实际 %d", n)
	}
	// now：gone 也照送
	m2 := okMsg()
	m2.To = "ubuntu:bob"
	m2.Text = "急事"
	id2, err := f.hub.Send(m2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState("ubuntu:bob", PeerGone); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 1 {
		t.Fatalf("gone 且非 now 不该送达，实际共 %d 条", n)
	}
	if err := f.hub.Decide(id2, "now"); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id2); st != MsgDelivered {
		t.Fatalf("now 应无视 gone 送达，实际 %s", st)
	}
	if n := len(f.delivered()); n != 2 {
		t.Fatalf("now 后应送达第二条，实际共 %d 条", n)
	}
}

// 发往 mac 的消息：对端没上报过就进不了 outbox，取件只能超时返回 false；
// 上报 idle 后由 Pump 放进 outbox 才取得到。
func TestTakeOutboxHoldsUnreportedPeer(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	id, err := f.hub.Send(okMsg()) // → mac:alice
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.hub.Peers()["mac:alice"]; ok {
		t.Fatalf("前提：mac:alice 没上报过忙闲")
	}
	f.hub.Pump(ctx) // 入队闸门按着：进不了 outbox
	shortCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if got, ok := f.hub.TakeOutbox(shortCtx, "mac:alice"); ok {
		t.Fatalf("没上报过 idle 不该取到，实际 got=%+v", got)
	}
	if err := f.hub.SetState("mac:alice", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx) // 上报过 idle 之后这条 Pump 才放进 outbox
	got, ok := f.hub.TakeOutbox(ctx, "mac:alice")
	if !ok || got.ID != id {
		t.Fatalf("上报 idle 后应取到 %s，实际 ok=%v got=%+v", id, ok, got)
	}
	if err := f.hub.MarkDelivered(id); err != nil {
		t.Fatal(err)
	}
}

// 撞上 ErrPeerNotClaude：对端记成 gone、消息仍 queued、Reason 说明原因；
// 这不算投递失败——不累计失败次数、不设退避，对方重新上报 idle 后下一轮 Pump 立刻重试。
func TestDeliverPeerNotClaudeNoBackoff(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	notClaude := fmt.Errorf("注入失败: %w", ErrPeerNotClaude)
	f.setDeliverErr(notClaude)
	m := okMsg()
	m.To = "ubuntu:bob"
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if got := f.hub.Peers()["ubuntu:bob"]; got != PeerGone {
		t.Fatalf("撞上不是 Claude 应记 gone，实际 %s", got)
	}
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("应仍 queued，实际 %s", st)
	}
	if r := peekHubMsg(t, f.hub, id).Reason; !strings.Contains(r, "不是 Claude") {
		t.Errorf("Reason 应含「不是 Claude」，实际 %q", r)
	}
	if next, fails := peekBackoff(t, f.hub, id); fails != 0 || !next.IsZero() {
		t.Errorf("不该计失败次数和退避，实际 fails=%d next=%v", fails, next)
	}
	// gone 期间不重试（这是 gone 闸门挡的，不是退避）
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 1 {
		t.Fatalf("gone 期间不该重试投递，实际调了 %d 次", n)
	}
	// 重新上报 idle：下一次 Pump 立刻再撞上，仍不被退避挡住
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 2 {
		t.Fatalf("重新上报 idle 后应立刻重试（第 2 次调用），实际调了 %d 次", n)
	}
	if next, fails := peekBackoff(t, f.hub, id); fails != 0 || !next.IsZero() {
		t.Errorf("重复撞上仍不该退避，实际 fails=%d next=%v", fails, next)
	}
	// 对面变回 Claude：立刻送达
	f.setDeliverErr(nil)
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy) // 对端钩子下一次上报，确认 injected → delivered
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("恢复正常后应 delivered，实际 %s", st)
	}
	if n := len(f.delivered()); n != 3 {
		t.Errorf("应共调 3 次，实际 %d", n)
	}
}

// 消息类型与回复指向的校验：类型缺省/未知、code 缺仓库分支、reply 缺/指错/换人，
// 以及正常 reply 自动填收件人并把原请求标成已回复。
func TestSendKindAndReplyValidation(t *testing.T) {
	f := newMsgFixture()

	// Kind 空 → 存成 notice
	id, err := f.hub.Send(okMsg())
	if err != nil {
		t.Fatal(err)
	}
	if got := peekHubMsg(t, f.hub, id).Kind; got != KindNotice {
		t.Fatalf("Kind 空应存成 notice，实际 %q", got)
	}
	// 未知 kind
	bad := Message{From: "ubuntu:bob", To: "mac:alice", Text: "x", Kind: "memo"}
	if _, err := f.hub.Send(bad); err == nil || !strings.Contains(err.Error(), "未知消息类型") {
		t.Errorf("未知 kind 应报错，实际 %v", err)
	}
	// code 必须带仓库和分支
	noBranch := Message{From: "ubuntu:bob", To: "mac:alice", Text: "交活儿", Kind: KindCode, Repo: "base"}
	if _, err := f.hub.Send(noBranch); err == nil || !strings.Contains(err.Error(), "仓库和分支") {
		t.Errorf("code 缺 branch 应报错，实际 %v", err)
	}
	bare := noBranch
	bare.Repo = ""
	if _, err := f.hub.Send(bare); err == nil {
		t.Errorf("code 不带仓库分支应报错")
	}
	goodCode := noBranch
	goodCode.Branch = "feat/msg"
	if _, err := f.hub.Send(goodCode); err != nil {
		t.Errorf("code 带齐仓库分支应通过: %v", err)
	}

	// 原请求：mac:sue → ubuntu:bob
	reqID, err := f.hub.Send(Message{From: "mac:sue", To: "ubuntu:bob", Text: "做点事", Kind: KindRequest})
	if err != nil {
		t.Fatal(err)
	}

	// reply 缺 reply_to
	if _, err := f.hub.Send(Message{From: "ubuntu:bob", Text: "做完了", Kind: KindReply}); err == nil ||
		!strings.Contains(err.Error(), "reply_to") {
		t.Errorf("reply 缺 reply_to 应报错，实际 %v", err)
	}
	// reply 指向不存在的消息
	missing := Message{From: "ubuntu:bob", Text: "做完了", Kind: KindReply, ReplyTo: "deadbeefdeadbeef"}
	if _, err := f.hub.Send(missing); !errors.Is(err, ErrMsgNotFound) {
		t.Errorf("reply 指向不存在的消息应 ErrMsgNotFound，实际 %v", err)
	}
	// reply 指向 notice（上面第一条就是 notice）
	toNotice := Message{From: "ubuntu:bob", Text: "做完了", Kind: KindReply, ReplyTo: id}
	if _, err := f.hub.Send(toNotice); err == nil || !strings.Contains(err.Error(), "不是请求") {
		t.Errorf("回复 notice 应报错，实际 %v", err)
	}
	// reply 指向发给别人的请求：reqID 是发给 ubuntu:bob 的
	toOther := Message{From: "mac:carol", Text: "做完了", Kind: KindReply, ReplyTo: reqID}
	if _, err := f.hub.Send(toOther); err == nil || !strings.Contains(err.Error(), "不能回复") {
		t.Errorf("回复发给别人的请求应报错，实际 %v", err)
	}
	// 原请求还没送达（pending）：不能回复
	pendingReply := Message{From: "ubuntu:bob", Text: "做完了", Kind: KindReply, ReplyTo: reqID}
	if _, err := f.hub.Send(pendingReply); err == nil || !strings.Contains(err.Error(), "还没送达") {
		t.Errorf("回复 pending 的请求应报错，实际 %v", err)
	}
	// approve 后 queued 仍未送达：照样不能回复
	if err := f.hub.Decide(reqID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Send(pendingReply); err == nil || !strings.Contains(err.Error(), "还没送达") {
		t.Errorf("回复 queued 的请求应报错，实际 %v", err)
	}
	// 上报 idle 送达后，才允许回复
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(context.Background())
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy) // 对端钩子下一次上报，确认 injected → delivered
	if st := stateOf(t, f.hub, reqID); st != MsgDelivered {
		t.Fatalf("原请求应已送达，实际 %s", st)
	}
	// 回复不许带仓库和分支（交代码走 code）
	repoReply := pendingReply
	repoReply.Repo = "base"
	repoReply.Branch = "feat/msg"
	if _, err := f.hub.Send(repoReply); err == nil || !strings.Contains(err.Error(), "回复不能带仓库和分支") {
		t.Errorf("reply 带 repo/branch 应报错，实际 %v", err)
	}
	// 正常 reply：To 自动填成原请求的发件人；刚发出还不能算回复
	replyID, err := f.hub.Send(Message{From: "ubuntu:bob", Text: "做完了", Kind: KindReply, ReplyTo: reqID})
	if err != nil {
		t.Fatalf("正常 reply 应通过: %v", err)
	}
	if got := peekHubMsg(t, f.hub, replyID); got.To != "mac:sue" || got.Kind != KindReply {
		t.Errorf("reply 的 To 应自动填原请求发件人，实际 %+v", got)
	}
	if peekHubMsg(t, f.hub, reqID).Answered {
		t.Errorf("回复刚发出（未送达）不该标 Answered")
	}
	// 回复真正送达（mac 侧取走并确认）后，原请求才标 Answered
	if err := f.hub.Decide(replyID, "approve"); err != nil {
		t.Fatal(err)
	}
	if peekHubMsg(t, f.hub, reqID).Answered {
		t.Errorf("回复还在队列里不该标 Answered")
	}
	if err := f.hub.SetState("mac:sue", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(context.Background())
	if got, ok := f.hub.TakeOutbox(context.Background(), "mac:sue"); !ok || got.ID != replyID {
		t.Fatalf("应取到回复 %s，实际 ok=%v got=%+v", replyID, ok, got)
	}
	if err := f.hub.MarkDelivered(replyID); err != nil {
		t.Fatal(err)
	}
	// 注入确认只到 injected：mac 侧钩子再上报一次这条回复才算真正送达，原请求这时才标 Answered
	ackPeer(t, f.hub, "mac:sue", PeerBusy)
	if !peekHubMsg(t, f.hub, reqID).Answered {
		t.Errorf("回复送达后原请求应标 Answered")
	}
	// To 填了别人：凭空换人，拒收
	wrongTo := Message{From: "ubuntu:bob", To: "mac:mallory", Text: "做完了", Kind: KindReply, ReplyTo: reqID}
	if _, err := f.hub.Send(wrongTo); err == nil || !strings.Contains(err.Error(), "原请求的发件人") {
		t.Errorf("reply 的 To 填别人应报错，实际 %v", err)
	}
	// notice 不许带 reply_to
	noticeWithReply := Message{From: "ubuntu:bob", To: "mac:alice", Text: "打声招呼", Kind: KindNotice, ReplyTo: reqID}
	if _, err := f.hub.Send(noticeWithReply); err == nil || !strings.Contains(err.Error(), "只有回复") {
		t.Errorf("notice 带 reply_to 应报错，实际 %v", err)
	}
}

// AskUser 的消息即使自动模式开着也要停下等用户。
func TestAskUserStopsAuto(t *testing.T) {
	f := newMsgFixture()
	if err := f.hub.SetAuto(10); err != nil {
		t.Fatal(err)
	}
	m := okMsg()
	m.AskUser = true
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.hub, id); st != MsgPending {
		t.Fatalf("AskUser 的消息自动模式下也应 pending，实际 %s", st)
	}
	// 对照：同样的消息不标 AskUser 就自动放行
	m.AskUser = false
	id2, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.hub, id2); st != MsgQueued {
		t.Fatalf("对照消息应 queued，实际 %s", st)
	}
}

// 终端渲染：各类型的头部、代码同步提示、附件行、回复提示行、「要用户定」行的完整文本与顺序。
func TestDisplayAndMacInboxPath(t *testing.T) {
	const (
		id16    = "aaaaaaaaaaaaaaaa"
		replyTo = "1234567890abcdef"
	)
	cases := []struct {
		name   string
		m      Message
		attach string
		want   string
	}{
		{
			name: "请求含回复提示行",
			m:    Message{ID: id16, From: "ubuntu:bob", To: "mac:alice", Text: "帮我看看这个", Kind: KindRequest},
			want: "[XXYI 消息 #" + id16 + " · 请求 · 来自 ubuntu:bob]\n帮我看看这个" +
				"\n（这是请求：用 xxyi_reply 回复 #" + id16 + "；做不了也要回复说明原因）",
		},
		{
			name: "回复头部指向原请求",
			m:    Message{ID: id16, From: "mac:alice", To: "ubuntu:bob", Text: "看完了", Kind: KindReply, ReplyTo: replyTo},
			want: "[XXYI 消息 #" + id16 + " · 回复 #" + replyTo + " · 来自 mac:alice]\n看完了",
		},
		{
			name: "通知",
			m:    Message{ID: id16, From: "ubuntu:bob", To: "mac:alice", Text: "水开了", Kind: KindNotice},
			want: "[XXYI 消息 #" + id16 + " · 通知 · 来自 ubuntu:bob]\n水开了",
		},
		{
			name: "交代码已同步",
			m: Message{ID: id16, From: "ubuntu:bob", To: "mac:alice", Text: "代码给你",
				Kind: KindCode, Repo: "base", Branch: "feat/msg", Synced: true},
			want: "[XXYI 消息 #" + id16 + " · 交代码 · 来自 ubuntu:bob]\n代码给你" +
				"\n（代码已同步：仓库 base 分支 feat/msg）",
		},
		{
			name: "交代码未同步要说明",
			m: Message{ID: id16, From: "ubuntu:bob", To: "mac:alice", Text: "代码给你",
				Kind: KindCode, Repo: "base", Branch: "feat/msg"},
			want: "[XXYI 消息 #" + id16 + " · 交代码 · 来自 ubuntu:bob]\n代码给你" +
				"\n（注意：代码没有同步，仓库 base 分支 feat/msg）",
		},
		{
			name:   "带附件",
			m:      Message{ID: id16, From: "ubuntu:bob", To: "mac:alice", Text: "带附件", Kind: KindNotice},
			attach: MacInboxPath(Message{ID: id16, FileName: "report.txt"}),
			want: "[XXYI 消息 #" + id16 + " · 通知 · 来自 ubuntu:bob]\n带附件" +
				"\n（附件：~/xxyi-work/inbox/" + id16 + "-report.txt）",
		},
		{
			name: "请求标了要用户定",
			m: Message{ID: id16, From: "ubuntu:bob", To: "mac:alice", Text: "要不要删库",
				Kind: KindRequest, AskUser: true},
			want: "[XXYI 消息 #" + id16 + " · 请求 · 来自 ubuntu:bob]\n要不要删库" +
				"\n（这是请求：用 xxyi_reply 回复 #" + id16 + "；做不了也要回复说明原因）" +
				"\n（对方标了「要用户定」：涉及决定的部分请交给用户，不要自己拍板）",
		},
		{
			name: "交代码+附件+要用户定的行序",
			m: Message{ID: id16, From: "ubuntu:bob", To: "mac:alice", Text: "一起给你",
				Kind: KindCode, Repo: "base", Branch: "feat/msg", AskUser: true},
			attach: "~/xxyi-work/inbox/" + id16 + "-patch.txt",
			want: "[XXYI 消息 #" + id16 + " · 交代码 · 来自 ubuntu:bob]\n一起给你" +
				"\n（注意：代码没有同步，仓库 base 分支 feat/msg）" +
				"\n（附件：~/xxyi-work/inbox/" + id16 + "-patch.txt）" +
				"\n（对方标了「要用户定」：涉及决定的部分请交给用户，不要自己拍板）",
		},
	}
	for _, c := range cases {
		if got := Display(c.m, c.attach); got != c.want {
			t.Errorf("%s:\nwant %q\ngot  %q", c.name, c.want, got)
		}
	}

	// MacInboxPath：没附件空串，有附件带消息 ID 前缀
	if got := MacInboxPath(Message{ID: id16}); got != "" {
		t.Errorf("没附件应为空串，实际 %q", got)
	}
	if got := MacInboxPath(Message{ID: id16, FileName: "note.md"}); got != "~/xxyi-work/inbox/"+id16+"-note.md" {
		t.Errorf("MacInboxPath 不对，实际 %q", got)
	}
}

// protoDeliverTo 走批准 → 上报空闲 → Pump，把 ubuntu 侧的消息真送达（返回 ID）。
func protoDeliverTo(t *testing.T, f *msgFixture, m Message) string {
	t.Helper()
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatalf("Send(%q): %v", m.Text, err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState(m.To, PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(context.Background())
	// 注入成功后等收件对端钩子报忙才算送达（idle 不算确认）；后面还要送下一条，确认完放回空闲
	ackPeer(t, f.hub, m.To, PeerBusy)
	if err := f.hub.SetState(m.To, PeerIdle); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("%q 应送达，实际 %s", m.Text, st)
	}
	return id
}

// Unanswered 只收「已送达 + 请求 + 发给这个地址 + 没回复过」，旧的在前；
// Inbox 返回发给这个地址的最近 n 条，新的在前，都不带附件内容。
func TestUnansweredAndInbox(t *testing.T) {
	f := newMsgFixture()
	const bob = "ubuntu:bob"

	r1 := protoDeliverTo(t, f, Message{From: "mac:sue", To: bob, Text: "第一件事", Kind: KindRequest,
		File: []byte("payload"), FileName: "a.txt"})
	r2 := protoDeliverTo(t, f, Message{From: "mac:sue", To: bob, Text: "第二件事", Kind: KindRequest})
	r3 := protoDeliverTo(t, f, Message{From: "mac:sue", To: bob, Text: "第三件事", Kind: KindRequest})
	n1 := protoDeliverTo(t, f, Message{From: "mac:sue", To: bob, Text: "只是通知", Kind: KindNotice})
	// 回复被拒 → 不算回复过，第三条仍欠
	rejID, err := f.hub.Send(Message{From: bob, Text: "第三件做不了", Kind: KindReply, ReplyTo: r3})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(rejID, "reject"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(idsOf(f.hub.Unanswered(bob)), ","), r3) {
		t.Errorf("回复被拒后原请求应仍欠回复，实际 %v", idsOf(f.hub.Unanswered(bob)))
	}
	// 回复真正送达（mac 侧取走并确认）→ 第三条才不再算欠回复
	rpID, err := f.hub.Send(Message{From: bob, Text: "第三件做完了", Kind: KindReply, ReplyTo: r3})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(rpID, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState("mac:sue", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(context.Background())
	if got, ok := f.hub.TakeOutbox(context.Background(), "mac:sue"); !ok || got.ID != rpID {
		t.Fatalf("应取到回复 %s，实际 ok=%v got=%+v", rpID, ok, got)
	}
	if err := f.hub.MarkDelivered(rpID); err != nil {
		t.Fatal(err)
	}
	// 发给别人的已送达请求不算在 bob 头上
	protoDeliverTo(t, f, Message{From: "mac:sue", To: "ubuntu:carol", Text: "别人的事", Kind: KindRequest})
	// 未送达（pending / queued）的请求都不算
	pendID, err := f.hub.Send(Message{From: "mac:sue", To: bob, Text: "还没批", Kind: KindRequest})
	if err != nil {
		t.Fatal(err)
	}
	queID, err := f.hub.Send(Message{From: "mac:sue", To: bob, Text: "批了没送", Kind: KindRequest})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(queID, "approve"); err != nil {
		t.Fatal(err)
	}

	un := f.hub.Unanswered(bob)
	if got := idsOf(un); strings.Join(got, ",") != r1+","+r2 {
		t.Errorf("Unanswered 应为旧到新的 %s,%s，实际 %v", r1, r2, got)
	}
	for _, m := range un {
		if m.File != nil {
			t.Errorf("Unanswered 不该带出附件内容，实际 %q", m.File)
		}
	}

	in := f.hub.Inbox(bob, 2)
	if got := idsOf(in); strings.Join(got, ",") != queID+","+pendID {
		t.Errorf("Inbox(n=2) 应最新在前两条，实际 %v", got)
	}
	in = f.hub.Inbox(bob, 100)
	if got := strings.Join(idsOf(in), ","); got != strings.Join([]string{queID, pendID, n1, r3, r2, r1}, ",") {
		t.Errorf("Inbox 应最新在前且只含发给 %s 的消息，实际 %s", bob, got)
	}
	for _, m := range in {
		if m.File != nil {
			t.Errorf("Inbox 不该带出附件内容，实际 %q", m.File)
		}
	}
}

func idsOf(ms []Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// —— HTTP 层 ——

// protoMsgBody 拼带 kind/reply_to 的 /msg/send 正文。
func protoMsgBody(t *testing.T, req msgSendReq) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("拼正文: %v", err)
	}
	return b
}

// /msg/state?state=idle&check=1：还有没回复的请求时不许装空闲，把欠的列回去、状态不动；
// 补齐回复后才真的变 idle。
func TestMsgAPIStateCheckUnanswered(t *testing.T) {
	a := newMsgAPI(t, 30)

	code, body := post(t, a.lURL+"/send", protoMsgBody(t, msgSendReq{
		From: "mac:frank", To: "ubuntu:bob", Text: "帮我看看\n第二行不该进摘要", Kind: KindRequest,
	}))
	if code != 200 {
		t.Fatalf("发请求: %d %s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil || resp["id"] == "" {
		t.Fatalf("响应应带 id: %s", body)
	}
	id := resp["id"]
	if code, body = post(t, a.lURL+"/decide?id="+id+"&action=approve", nil); code != 200 {
		t.Fatalf("approve: %d %s", code, body)
	}

	// 收件方上报空闲（不带 check）→ 经 Deliver 注入；注入只到 injected，
	// 等收件方钩子再上报一次（这里报 busy）才落 delivered
	if code, body = remoteDo(t, http.MethodPost, a.base+"/msg/state?addr=ubuntu:bob&state=idle", a.token, nil); code != 200 {
		t.Fatalf("上报空闲: %d %s", code, body)
	}
	waitFor(t, 3*time.Second, "注入", func() bool {
		st := msgStateOf(t, a.lURL, id)
		return st == MsgInjected || st == MsgDelivered
	})
	if code, body = remoteDo(t, http.MethodPost, a.base+"/msg/state?addr=ubuntu:bob&state=busy", a.token, nil); code != 200 {
		t.Fatalf("再次上报置忙: %d %s", code, body)
	}
	waitFor(t, 3*time.Second, "送达", func() bool {
		return msgStateOf(t, a.lURL, id) == MsgDelivered
	})

	// 还欠着回复：check=1 把欠的列回去，状态不变（仍 busy，不是 idle）
	code, body = remoteDo(t, http.MethodPost, a.base+"/msg/state?addr=ubuntu:bob&state=idle&check=1", a.token, nil)
	if code != 200 {
		t.Fatalf("check=1: %d %s", code, body)
	}
	var st struct {
		OK         bool             `json:"ok"`
		Unanswered []unansweredItem `json:"unanswered"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("解 check=1 响应: %v %s", err, body)
	}
	if !st.OK || len(st.Unanswered) != 1 || st.Unanswered[0].ID != id {
		t.Fatalf("应列出欠回复的那条请求，实际 %s", body)
	}
	if st.Unanswered[0].From != "mac:frank" || st.Unanswered[0].Text != "帮我看看" {
		t.Errorf("摘要应是发件人和正文首行，实际 %+v", st.Unanswered[0])
	}
	if got := a.s.Hub().Peers()["ubuntu:bob"]; got == PeerIdle {
		t.Errorf("欠回复时不该变 idle，实际 %s", got)
	}

	// 补上回复：远端代发（回给 mac:frank，走 outbox）；发出去就不再算欠回复，
	// 取走并确认送达只是把这条回复自己走完投递流程
	if code, body = remoteDo(t, http.MethodPost, a.base+"/msg/send", a.token, protoMsgBody(t, msgSendReq{
		From: "ubuntu:bob", Text: "看完了", Kind: KindReply, ReplyTo: id,
	})); code != 200 {
		t.Fatalf("回复: %d %s", code, body)
	}
	var rr map[string]string
	if err := json.Unmarshal(body, &rr); err != nil || rr["id"] == "" {
		t.Fatalf("回复响应应带 id: %s", body)
	}
	replyID := rr["id"]
	if len(a.s.Hub().Unanswered("ubuntu:bob")) != 0 {
		t.Fatalf("回复已发出（哪怕还没送达）就不该再算欠回复")
	}
	if code, body = post(t, a.lURL+"/decide?id="+replyID+"&action=approve", nil); code != 200 {
		t.Fatalf("approve 回复: %d %s", code, body)
	}
	if code, body = post(t, a.lURL+"/state?addr=mac:frank&state=idle", nil); code != 200 {
		t.Fatalf("上报 mac 侧空闲: %d %s", code, body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, _ = get(t, a.lURL+"/outbox?to=mac:frank")
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("取不到回复消息，最后状态码 %d", code)
		}
	}
	if code, body = post(t, a.lURL+"/delivered?id="+replyID, nil); code != 200 {
		t.Fatalf("确认回复送达: %d %s", code, body)
	}
	// 回复送达后才没有欠的了，再报空闲才真的变 idle
	if len(a.s.Hub().Unanswered("ubuntu:bob")) != 0 {
		t.Fatalf("回复确认后不该再有欠回复")
	}
	code, body = remoteDo(t, http.MethodPost, a.base+"/msg/state?addr=ubuntu:bob&state=idle&check=1", a.token, nil)
	if code != 200 {
		t.Fatalf("补齐后上报空闲: %d %s", code, body)
	}
	st.Unanswered = nil // json.Unmarshal 不清空缺失字段，手动清掉上一轮的结果
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("解响应: %v %s", err, body)
	}
	if !st.OK || len(st.Unanswered) != 0 {
		t.Fatalf("不该再有 unanswered，实际 %s", body)
	}
	if got := a.s.Hub().Peers()["ubuntu:bob"]; got != PeerIdle {
		t.Fatalf("没有欠回复后应变 idle，实际 %s", got)
	}
}

// /msg/inbox 只许读本侧：远端读 mac 的收件箱 403，本机读 ubuntu 的也 403。
func TestMsgAPIInboxSideEnforced(t *testing.T) {
	a := newMsgAPI(t, 30)

	// 远端有自己的一条收件消息，读自己的没问题
	if code, body := post(t, a.lURL+"/send", protoMsgBody(t, msgSendReq{
		From: "mac:frank", To: "ubuntu:bob", Text: "收件箱里有货", Kind: KindRequest,
	})); code != 200 {
		t.Fatalf("本机发消息: %d %s", code, body)
	}
	code, body := remoteDo(t, http.MethodGet, a.base+"/msg/inbox?addr=ubuntu:bob", a.token, nil)
	if code != 200 {
		t.Fatalf("远端读自己收件箱: %d %s", code, body)
	}
	var box struct {
		Messages   []Message `json:"messages"`
		Unanswered []Message `json:"unanswered"`
	}
	if err := json.Unmarshal(body, &box); err != nil {
		t.Fatalf("解 inbox: %v %s", err, body)
	}
	if len(box.Messages) != 1 || box.Messages[0].Text != "收件箱里有货" {
		t.Errorf("远端收件箱内容不对: %+v", box.Messages)
	}
	// 冒充另一侧一律 403
	if code, _ := remoteDo(t, http.MethodGet, a.base+"/msg/inbox?addr=mac:frank", a.token, nil); code != http.StatusForbidden {
		t.Fatalf("远端读 mac 收件箱应 403，得 %d", code)
	}
	if code, _ := get(t, a.lURL+"/inbox?addr=ubuntu:bob"); code != http.StatusForbidden {
		t.Fatalf("本机读 ubuntu 收件箱应 403，得 %d", code)
	}
	if code, _ := get(t, a.lURL+"/inbox?addr=mac:frank"); code != 200 {
		t.Fatalf("本机读自己收件箱应 200，得 %d", code)
	}
	// n 越界应 400
	if code, _ := get(t, a.lURL+"/inbox?addr=mac:frank&n=0"); code != http.StatusBadRequest {
		t.Fatalf("n=0 应 400，得 %d", code)
	}
}

// 取件响应的 display 必须等于 Display(m, MacInboxPath(m))——取件端直接注入，不自己拼格式。
func TestMsgAPIOutboxDisplay(t *testing.T) {
	a := newMsgAPI(t, 30)

	code, body := remoteDo(t, http.MethodPost, a.base+"/msg/send", a.token, protoMsgBody(t, msgSendReq{
		From: "ubuntu:bob", To: "mac:frank", Text: "带附件的请求",
		FileName: "/tmp/report.txt", File: []byte("payload"), Kind: KindRequest, AskUser: true,
	}))
	if code != 200 {
		t.Fatalf("远端发带附件消息: %d %s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil || resp["id"] == "" {
		t.Fatalf("响应应带 id: %s", body)
	}
	id := resp["id"]
	if code, body = post(t, a.lURL+"/decide?id="+id+"&action=approve", nil); code != 200 {
		t.Fatalf("approve: %d %s", code, body)
	}
	if code, body = post(t, a.lURL+"/state?addr=mac:frank&state=idle", nil); code != 200 {
		t.Fatalf("上报空闲: %d %s", code, body)
	}

	var env struct {
		Message
		Display string `json:"display"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, body = get(t, a.lURL+"/outbox?to=mac:frank")
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("取不到出站消息，最后状态码 %d %s", code, body)
		}
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解取件响应: %v %s", err, body)
	}
	if env.ID != id || env.Kind != KindRequest || env.FileName != "report.txt" {
		t.Fatalf("取件响应不对: %+v", env)
	}
	if want := Display(env.Message, MacInboxPath(env.Message)); env.Display != want {
		t.Errorf("display 应等于 Display(m, MacInboxPath(m)):\nwant %q\ngot  %q", want, env.Display)
	}
	if !strings.Contains(env.Display, "（附件：~/xxyi-work/inbox/"+id+"-report.txt）") {
		t.Errorf("display 应含附件行，实际 %q", env.Display)
	}
	if code, body = post(t, a.lURL+"/delivered?id="+id, nil); code != 200 {
		t.Errorf("确认送达应 200，得 %d %s", code, body)
	}
}

// NagUnanswered：同一条欠回复最多提醒 nagMax 次；创建超过 nagWindow 的不再打扰；
// Unanswered 本身不受提醒次数影响。
func TestNagUnansweredCapAndWindow(t *testing.T) {
	f := newMsgFixture()
	r1 := protoDeliverTo(t, f, Message{From: "mac:sue", To: "ubuntu:bob", Text: "第一件事", Kind: KindRequest})
	for i := 0; i < nagMax; i++ {
		nag := f.hub.NagUnanswered("ubuntu:bob")
		if len(nag) != 1 || nag[0].ID != r1 {
			t.Fatalf("第 %d 次提醒应返回 %s，实际 %v", i+1, r1, idsOf(nag))
		}
	}
	if nag := f.hub.NagUnanswered("ubuntu:bob"); len(nag) != 0 {
		t.Errorf("提醒满 %d 次后不该再返回，实际 %v", nagMax, idsOf(nag))
	}
	if ids := idsOf(f.hub.Unanswered("ubuntu:bob")); len(ids) != 1 || ids[0] != r1 {
		t.Errorf("Unanswered 不该受提醒次数影响，实际 %v", ids)
	}
	// 再送一条新的，推进时钟超过窗口：两条都不该再提醒
	protoDeliverTo(t, f, Message{From: "mac:sue", To: "ubuntu:bob", Text: "第二件事", Kind: KindRequest})
	f.clock.Add(nagWindow + time.Minute)
	if nag := f.hub.NagUnanswered("ubuntu:bob"); len(nag) != 0 {
		t.Errorf("超过 %v 的欠回复不该再提醒，实际 %v", nagWindow, idsOf(nag))
	}
}

// busy 卡住的兜底：busy 超过 busyStale 按可送处理（Deliver 与 outbox 两条路径都是）；
// 恰好等于不算（必须严格大于）；gone 放多久都照按。
func TestBusyStaleTreatedAsReady(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()

	// Deliver 路径
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
		t.Fatalf("fresh busy 不该送达，实际 %d", n)
	}
	f.clock.Add(busyStale)
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 0 {
		t.Fatalf("恰好 %v 不算失联，不该送达，实际 %d", busyStale, n)
	}
	f.clock.Add(time.Second)
	f.hub.Pump(ctx)
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy) // 对端钩子下一次上报，确认 injected → delivered
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("busy 失联后应送达，实际 %s", st)
	}

	// mac outbox 路径
	m2 := okMsg() // → mac:alice
	m2.Text = "失联补送"
	id2, err := f.hub.Send(m2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id2, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState("mac:alice", PeerBusy); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, ok := f.hub.TakeOutbox(shortCtx, "mac:alice"); ok {
		t.Fatalf("fresh busy 不该取得到")
	}
	f.clock.Add(busyStale + time.Second)
	f.hub.Pump(ctx)
	if got, ok := f.hub.TakeOutbox(ctx, "mac:alice"); !ok || got.ID != id2 {
		t.Fatalf("busy 失联后应能取到，实际 ok=%v got=%+v", ok, got)
	}
	if err := f.hub.MarkDelivered(id2); err != nil {
		t.Fatal(err)
	}

	// gone 不算失联：放多久都按住
	m3 := okMsg()
	m3.Text = "gone 按住"
	id3, err := f.hub.Send(m3)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id3, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState("mac:alice", PeerGone); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(48 * time.Hour)
	f.hub.Pump(ctx)
	shortCtx2, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	if _, ok := f.hub.TakeOutbox(shortCtx2, "mac:alice"); ok {
		t.Fatalf("gone 再久也不该可送，实际取到了 %s", id3)
	}
}

// 回复只要发出去了（pending/queued/delivered/sync_failed，只要没被拒绝）就算"已经回复过了"：
// 原请求不再出现在 Unanswered（NagUnanswered 也就不催），重复回复当场拦下；
// 上一条被拒绝后请求重新变回欠着，才允许再回复。
func TestLiveReplyCountsAsAnswered(t *testing.T) {
	f := newMsgFixture()
	const bob = "ubuntu:bob"
	r := protoDeliverTo(t, f, Message{From: "mac:sue", To: bob, Text: "第一件事", Kind: KindRequest})

	replyID, err := f.hub.Send(Message{From: bob, Text: "做完了", Kind: KindReply, ReplyTo: r})
	if err != nil {
		t.Fatalf("回复应通过: %v", err)
	}
	if ids := idsOf(f.hub.Unanswered(bob)); len(ids) != 0 {
		t.Fatalf("回复 pending（还没送达）就该算已回复，实际欠 %v", ids)
	}
	if nag := f.hub.NagUnanswered(bob); len(nag) != 0 {
		t.Errorf("没有欠回复不该催，实际 %v", idsOf(nag))
	}
	// 重复回复：拦下，报错带上上一条回复的 ID
	if _, err := f.hub.Send(Message{From: bob, Text: "再说一遍", Kind: KindReply, ReplyTo: r}); err == nil ||
		!strings.Contains(err.Error(), "已经回复过了") || !strings.Contains(err.Error(), replyID) {
		t.Fatalf("重复回复应报「已经回复过了」并带上 #%s，实际 %v", replyID, err)
	}
	// 上一条被拒绝：请求重新欠着，可以再回复
	if err := f.hub.Decide(replyID, "reject"); err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(f.hub.Unanswered(bob)); len(ids) != 1 || ids[0] != r {
		t.Fatalf("回复被拒后应重新欠回复，实际 %v", ids)
	}
	if _, err := f.hub.Send(Message{From: bob, Text: "重新做完了", Kind: KindReply, ReplyTo: r}); err != nil {
		t.Fatalf("上一条被拒后应能再回复: %v", err)
	}
}

// TakeOutbox 定时重查：对端 busy 之后再无任何事件（不再 SetState、不再 Pump），
// 注入时钟推过 busyStale（busy 失联）后，等待中的 TakeOutbox 靠 outboxRecheck
// 定时器自己醒来取走，不会干等到 ctx 结束。
func TestTakeOutboxRechecksPeriodically(t *testing.T) {
	old := outboxRecheck
	t.Cleanup(func() { outboxRecheck = old })
	outboxRecheck = 50 * time.Millisecond

	f := newMsgFixture()
	id, err := f.hub.Send(okMsg()) // → mac:alice
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState("mac:alice", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(context.Background()) // idle 时进 outbox
	if err := f.hub.SetState("mac:alice", PeerBusy); err != nil {
		t.Fatal(err)
	}
	// 之后 hub 不再有任何调用——取走只能靠 TakeOutbox 的定时重查
	go func() {
		time.Sleep(100 * time.Millisecond) // 让 TakeOutbox 先进入等待（此刻 busy 还没过期）
		f.clock.Add(busyStale + time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, ok := f.hub.TakeOutbox(ctx, "mac:alice")
	if !ok || got.ID != id {
		t.Fatalf("busy 失联后无任何事件也该按时钟重查取到，实际 ok=%v got=%+v", ok, got)
	}
}

// ErrPeerNotClaude 与送达成功置 busy 同一条规矩：送达期间对端有新上报就不动状态。
// 假 Deliver 里先上报 idle（版本号变了）再报不是 Claude——对端应停在 idle，
// 不能被 gone 按住（否则刚报的空闲被吞，后续消息全卡住）。
func TestPeerNotClaudeNotOverwriteIdleDuringDeliver(t *testing.T) {
	f := newMsgFixture()
	f.hub.cfg.Deliver = func(_ context.Context, m Message) error {
		if err := f.hub.SetState(m.To, PeerIdle); err != nil {
			t.Errorf("送达期间上报 idle: %v", err)
		}
		return ErrPeerNotClaude
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
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(context.Background())
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Errorf("没送达应仍 queued，实际 %s", st)
	}
	if got := f.hub.Peers()["ubuntu:bob"]; got != PeerIdle {
		t.Errorf("送达期间报的 idle 不该被 gone 覆盖，实际 %s", got)
	}
	if r := peekHubMsg(t, f.hub, id).Reason; !strings.Contains(r, "不是 Claude") {
		t.Errorf("Reason 应含「不是 Claude」，实际 %q", r)
	}
}

// 送达过程中对端钩子报的 idle 不能被送达成功的置 busy 覆盖：
// pumpOne 调 Deliver 前记下对端状态版本号，回来发现版本变了就不动状态。
func TestIdleDuringDeliverNotOverwritten(t *testing.T) {
	f := newMsgFixture()
	f.hub.cfg.Deliver = func(_ context.Context, m Message) error {
		// 模拟注入过程中接收方上报空闲：版本号变了，送达后不得覆盖
		return f.hub.SetState(m.To, PeerIdle)
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
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(context.Background())
	if got := f.hub.Peers()["ubuntu:bob"]; got != PeerIdle {
		t.Errorf("送达期间报的 idle 不该被 busy 覆盖，实际 %s", got)
	}
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("idle 上报不算确认，注入后应 injected，实际 %s", st)
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy) // 钩子报忙才确认送达
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("应送达，实际 %s", st)
	}
}
