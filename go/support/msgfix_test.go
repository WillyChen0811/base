package support

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件补送达确认与重试计数改造后的行为（照改后的 msg.go 语义）：
// 确认只认钩子经 SetState 上报的 busy（记进 hookBusySeq），基准是注入**前**的
// peerSeq（ackBase：Ubuntu 在调 Deliver 前、Mac 在 TakeOutbox 置忙前记下）——
// 注入过程中到达的 busy 也算确认，落状态当场直接 delivered；
// idle 上报不算确认，超过 AckTimeout 由 CheckTimers/Pump 重回 queued 并记一次 Stage=ack；
// gone 上报把该地址的 injected 全部退回 queued，不记 Attempt、不耗预算；
// 重试预算按阶段各记各的（stageKey=id|stage），某阶段成功清该阶段记录，
// 早先阶段的失败不占后面阶段的时间窗；retry 重开时整个预算清零；
// ClientID 是同 From 的幂等 ID，重复 Send 还回原 ID；
// CheckTimers 不调 Pump 也能把确认超时的 injected 收回。
// 时钟与 fixture 复用 msg_test.go 的 fakeClock/newMsgFixture，
// 阶段预算的内部表复用 msgretry_test.go 的 peekStageFails/peekFirstFail。

// 1) 假 Deliver 里先上报 busy 再返回成功：ackBase 是调 Deliver **前**的版本号，
// 注入成功那一刻 hookBusySeq 已超过它，Pump 结束消息直接 delivered，
// 不再等"下一次上报"；推时钟一分钟再 Pump 也不该重投。
func TestAckBusyInsideDeliverConfirmsAtOnce(t *testing.T) {
	clock := newFakeClock()
	var hub *Hub
	var mu sync.Mutex
	var deliverCalls int
	hub = NewHub(HubConfig{
		Now: clock.Now,
		Deliver: func(_ context.Context, m Message) error {
			// 像钩子在注入完成前就被触发：先报 busy，再让 Deliver 成功返回。
			if err := hub.SetState(m.To, PeerBusy); err != nil {
				t.Errorf("注入中上报 busy: %v", err)
			}
			mu.Lock()
			deliverCalls++
			mu.Unlock()
			return nil
		},
	})
	id, err := hub.Send(Message{From: "ubuntu:macsrv", To: "ubuntu:bob", Text: "注入中就报忙"})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	hub.Pump(context.Background())
	if st := stateOf(t, hub, id); st != MsgDelivered {
		t.Fatalf("注入过程中钩子已报 busy，应直接 delivered，实际 %s", st)
	}
	if _, _, ok := peekAckProgress(t, hub, id); ok {
		t.Errorf("已 delivered，不该还留着等确认的进度")
	}
	clock.Add(time.Minute)
	hub.Pump(context.Background())
	mu.Lock()
	n := deliverCalls
	mu.Unlock()
	if n != 1 {
		t.Errorf("已送达不该重投，Deliver 实际被调 %d 次", n)
	}
	if st := stateOf(t, hub, id); st != MsgDelivered {
		t.Fatalf("再 Pump 后应仍是 delivered，实际 %s", st)
	}
}

// 2) Mac 路径：TakeOutbox 记下置忙**前**的 ackBase，MarkDelivered 之前钩子报 busy，
// MarkDelivered 当场直接落 delivered（不停在 injected）。
func TestAckBusyAfterTakeConfirmsOnMarkDelivered(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	id, err := f.hub.Send(okMsg()) // To=mac:alice
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
		t.Fatalf("应能取到消息，ok=%v got=%+v", ok, got)
	}
	ackPeer(t, f.hub, "mac:alice", PeerBusy) // 取走后、确认注入前报忙
	if err := f.hub.MarkDelivered(id); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("取走前基准更早、取走后报过忙，MarkDelivered 应直接 delivered，实际 %s", st)
	}
	if _, _, ok := peekAckProgress(t, f.hub, id); ok {
		t.Errorf("已 delivered，不该还留着 ackSeq/injectedAt")
	}
}

// 3) 注入后只报 idle：idle 不算确认，消息停在 injected；
// 推过 AckTimeout 由 CheckTimers 重回 queued，Attempts 多一条 Stage=ack。
func TestAckIdleNotAckedUntilTimeout(t *testing.T) {
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
		t.Fatalf("注入后应为 injected，实际 %s", st)
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle) // 只报 idle：空闲不代表接住了消息
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("idle 上报不该确认送达，应仍是 injected，实际 %s", st)
	}
	f.clock.Add(defaultAckTimeout + time.Second)
	f.hub.CheckTimers()
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("超过 AckTimeout 没报 busy 应回 queued，实际 %s", st)
	}
	lm := peekHubMsg(t, f.hub, id)
	if len(lm.Attempts) != 1 {
		t.Fatalf("确认超时应记 1 条 Attempt，实际 %d", len(lm.Attempts))
	}
	if lm.Attempts[0].Stage != "ack" {
		t.Errorf("Attempt.Stage 应为 ack，实际 %q", lm.Attempts[0].Stage)
	}
	if !strings.Contains(lm.Reason, "没开始处理") {
		t.Errorf("Reason 应写明确认超时，实际 %q", lm.Reason)
	}
}

// 4) 注入后上报 gone：injected 退回 queued，Reason 记「对方已退出」，
// 退回不算失败——不记 Attempt、不耗 ack 预算。
func TestAckGoneRequeuesWithoutAttempt(t *testing.T) {
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
		t.Fatalf("注入后应为 injected，实际 %s", st)
	}
	before := len(peekHubMsg(t, f.hub, id).Attempts)
	ackPeer(t, f.hub, "ubuntu:bob", PeerGone)
	lm := peekHubMsg(t, f.hub, id)
	if lm.State != MsgQueued {
		t.Fatalf("gone 上报后应退回 queued，实际 %s", lm.State)
	}
	if lm.Reason != peerGoneReason {
		t.Errorf("Reason 应为 %q，实际 %q", peerGoneReason, lm.Reason)
	}
	if len(lm.Attempts) != before {
		t.Errorf("gone 退回不该记 Attempt：%d → %d", before, len(lm.Attempts))
	}
	if n := peekStageFails(t, f.hub, id, "ack"); n != 0 {
		t.Errorf("gone 退回不该耗 ack 预算，stageFails[ack]=%d", n)
	}
	if _, _, ok := peekAckProgress(t, f.hub, id); ok {
		t.Errorf("退回 queued 后等确认的进度应清掉")
	}
}

// 5) ack 预算用完转 failed 后 Decide retry：预算清零重新给，
// 再注入、再超时一次只算新一轮的第 1 次失败——仍 queued 而不是立刻 failed。
func TestAckBudgetResetByRetry(t *testing.T) {
	f := newMsgFixture()
	f.hub.cfg.MaxAttempts = 2 // ack 阶段失败 2 次就转人工
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
		t.Fatalf("应注入为 injected，实际 %s", st)
	}

	// 第 1 次 ack 超时：queued，ack 预算用掉 1。
	f.clock.Add(defaultAckTimeout + time.Second)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("第 1 次确认超时应回 queued，实际 %s", st)
	}
	if n := peekStageFails(t, f.hub, id, "ack"); n != 1 {
		t.Fatalf("ack 预算应记 1 次失败，实际 %d", n)
	}

	// 第 2 次：重新注入又超时，预算用完转 failed（带 Stage=ack 报告）。
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.clock.Add(3 * time.Second) // 推过 2 秒退避
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("退避后应重新注入，实际 %s", st)
	}
	f.clock.Add(defaultAckTimeout + time.Second)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgFailed {
		t.Fatalf("ack 预算用完应转 failed，实际 %s", st)
	}
	lm := peekHubMsg(t, f.hub, id)
	if lm.Report == nil || lm.Report.Stage != "ack" {
		t.Errorf("转人工报告应是 Stage=ack，实际 %+v", lm.Report)
	}
	if len(lm.Attempts) != 2 {
		t.Errorf("转人工前 ack 失败应记 2 条，实际 %d", len(lm.Attempts))
	}

	// retry：整个预算清零。
	if err := f.hub.Decide(id, "retry"); err != nil {
		t.Fatal(err)
	}
	if n := peekStageFails(t, f.hub, id, "ack"); n != 0 {
		t.Errorf("retry 后 ack 预算应清零，实际 %d", n)
	}
	if _, ok := peekFirstFail(t, f.hub, id, "ack"); ok {
		t.Errorf("retry 后 ack 预算起点应清掉")
	}

	// 再注入、再超时一次：是新一轮的第 1 次失败，不该直接 failed。
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgInjected {
		t.Fatalf("retry 后应重新注入，实际 %s", st)
	}
	f.clock.Add(defaultAckTimeout + time.Second)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("retry 后新一轮第 1 次超时不该 failed，应 queued，实际 %s", st)
	}
	if n := peekStageFails(t, f.hub, id, "ack"); n != 1 {
		t.Errorf("新一轮 ack 预算应从 1 重计，实际 %d", n)
	}
	lm = peekHubMsg(t, f.hub, id)
	if len(lm.Attempts) != 3 {
		t.Errorf("Attempts 历史应保留并新增到 3 条，实际 %d", len(lm.Attempts))
	}
	for i, a := range lm.Attempts {
		if a.Stage != "ack" {
			t.Errorf("第 %d 条 Attempt 的 Stage 应为 ack，实际 %q", i+1, a.Stage)
		}
	}
}

// 6) 预算按阶段独立：同步失败 1 次的起点（firstFail[id|sync]）在同步成功后清掉；
// 对方 busy 40 分钟（远超 RetryWindow）再 idle，deliver 失败 1 次——
// deliver 阶段自己的起点是新记的，不能被 40 分钟前 sync 的首败拖进 failed。
func TestStageBudgetIndependentPerStage(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	boomSync := errors.New("同步：ssh 断了")
	boomDeliver := errors.New("注入：找不到窗格")
	f.setSyncErr(boomSync)
	m := Message{From: "mac:alice", To: "ubuntu:bob", Text: "先同步再送达", Repo: "base", Branch: "main"}
	id, err := f.hub.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("同步失败应留在 queued，实际 %s", st)
	}
	if n := peekStageFails(t, f.hub, id, "sync"); n != 1 {
		t.Fatalf("sync 阶段应记 1 次失败，实际 %d", n)
	}
	if _, ok := peekFirstFail(t, f.hub, id, "sync"); !ok {
		t.Fatalf("sync 阶段应记预算起点 firstFail")
	}
	if n := len(f.delivered()); n != 0 {
		t.Errorf("同步没过不该送达，实际 Deliver 调了 %d 次", n)
	}

	// busy 40 分钟（超 busyStale 也超 RetryWindow），然后重新 idle。
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy)
	f.clock.Add(40 * time.Minute)
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.setSyncErr(nil)
	f.setDeliverErr(boomDeliver)
	f.hub.Pump(ctx)

	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("deliver 阶段第 1 次失败应留在 queued（不被 sync 的旧起点拖成 failed），实际 %s", st)
	}
	if n := peekStageFails(t, f.hub, id, "sync"); n != 0 {
		t.Errorf("同步成功后 sync 阶段记录应清掉，实际 %d", n)
	}
	if _, ok := peekFirstFail(t, f.hub, id, "sync"); ok {
		t.Errorf("同步成功后 firstFail[id|sync] 应清掉")
	}
	ffDeliver, ok := peekFirstFail(t, f.hub, id, "deliver")
	if !ok {
		t.Fatalf("deliver 失败应记自己阶段的起点")
	}
	if !ffDeliver.Equal(f.clock.Now()) {
		t.Errorf("deliver 起点应是本次失败时刻（本次 Pump 的时间），实际 %v vs %v", ffDeliver, f.clock.Now())
	}
	if n := peekStageFails(t, f.hub, id, "deliver"); n != 1 {
		t.Errorf("deliver 阶段应记 1 次失败，实际 %d", n)
	}
	lm := peekHubMsg(t, f.hub, id)
	if len(lm.Attempts) != 2 || lm.Attempts[0].Stage != "sync" || lm.Attempts[1].Stage != "deliver" {
		t.Errorf("Attempts 应是 sync、deliver 各一条，实际 %+v", lm.Attempts)
	}
	if n := len(f.delivered()); n != 1 {
		t.Errorf("Deliver 应只试了 1 次，实际 %d", n)
	}
}

// 7) ClientID 幂等：同 From 同 ClientID 重复 Send 还回已有 ID、List 只有一条；
// 不同 From 同 ClientID 互不相干、各一条。
func TestClientIDIdempotent(t *testing.T) {
	f := newMsgFixture()
	id1, err := f.hub.Send(Message{From: "ubuntu:macsrv", To: "ubuntu:bob", Text: "第一次", ClientID: "cli-001"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := f.hub.Send(Message{From: "ubuntu:macsrv", To: "ubuntu:bob", Text: "客户端超时重发", ClientID: "cli-001"})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("同 From 同 ClientID 应还回原 ID，实际 %q vs %q", id1, id2)
	}
	msgs := f.hub.List()
	if len(msgs) != 1 {
		t.Fatalf("重复发送不该新建消息，List 应只有 1 条，实际 %d", len(msgs))
	}
	if msgs[0].Text != "第一次" {
		t.Errorf("重发不该覆盖原文，实际正文 %q", msgs[0].Text)
	}
	// 不同 From 同 ClientID：不是一回事，各自入库。
	id3, err := f.hub.Send(Message{From: "ubuntu:bob", To: "ubuntu:macsrv", Text: "换个发件人", ClientID: "cli-001"})
	if err != nil {
		t.Fatal(err)
	}
	if id3 == id1 {
		t.Errorf("不同 From 同 ClientID 不该撞 ID")
	}
	if n := len(f.hub.List()); n != 2 {
		t.Fatalf("List 应有 2 条，实际 %d", n)
	}
}

// 7b) ClientID 经 HTTP：非法字符应 400 且不入库；合法字符 200 且重发还回同一 ID。
func TestClientIDInvalidCharHTTP400(t *testing.T) {
	a := newMsgAPI(t, 30)
	bad, _ := json.Marshal(msgSendReq{From: "mac:frank", To: "ubuntu:bob", Text: "坏幂等号", ClientID: "bad id"})
	if code, body := post(t, a.lURL+"/send", bad); code != http.StatusBadRequest {
		t.Fatalf("client_id 含空格应 400，得 %d %s", code, body)
	}
	bad2, _ := json.Marshal(msgSendReq{From: "mac:frank", To: "ubuntu:bob", Text: "坏幂等号", ClientID: "中文id"})
	if code, body := post(t, a.lURL+"/send", bad2); code != http.StatusBadRequest {
		t.Fatalf("client_id 含中文应 400，得 %d %s", code, body)
	}
	if n := len(listMsg(t, a.lURL).Messages); n != 0 {
		t.Fatalf("非法 client_id 不该入库，List 实际 %d 条", n)
	}

	good, _ := json.Marshal(msgSendReq{From: "mac:frank", To: "ubuntu:bob", Text: "幂等重发", ClientID: "cli.A-1_2"})
	code, body := post(t, a.lURL+"/send", good)
	if code != http.StatusOK {
		t.Fatalf("合法 client_id 应 200，得 %d %s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil || resp["id"] == "" {
		t.Fatalf("响应应带 id: %s", body)
	}
	code, body = post(t, a.lURL+"/send", good)
	if code != http.StatusOK {
		t.Fatalf("重发应 200，得 %d %s", code, body)
	}
	var resp2 map[string]string
	if err := json.Unmarshal(body, &resp2); err != nil || resp2["id"] != resp["id"] {
		t.Fatalf("同 client_id 重发应还回同一 ID，得 %s（原 %q）", body, resp["id"])
	}
}

// 8) CheckTimers 独立生效：不调 Pump，也能把确认超时的 injected 收回 queued，
// 且它不碰投递（Deliver 次数不变）。
func TestCheckTimersRequeuesWithoutPump(t *testing.T) {
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
		t.Fatalf("应注入为 injected，实际 %s", st)
	}
	f.clock.Add(defaultAckTimeout + time.Second)
	f.hub.CheckTimers()
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("CheckTimers 应把确认超时的 injected 转回 queued，实际 %s", st)
	}
	if n := len(f.delivered()); n != 1 {
		t.Errorf("CheckTimers 不做注入，Deliver 应还是 1 次，实际 %d", n)
	}
	lm := peekHubMsg(t, f.hub, id)
	if len(lm.Attempts) != 1 || lm.Attempts[0].Stage != "ack" {
		t.Errorf("应记 1 条 Stage=ack 的 Attempt，实际 %+v", lm.Attempts)
	}
}
