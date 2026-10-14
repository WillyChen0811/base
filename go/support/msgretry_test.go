package support

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件专门覆盖重试预算（MaxAttempts/RetryWindow）、线路暂停（ErrLinkDown）与转人工：
// 转人工的时机（次数用完 / 时间窗先到）、报告内容（Stage/Screen/PeerState）、
// retry/now 重开预算、Redirect 改送、以及 failed 连同报告落盘后重启还在。
// 时钟一律用 msg_test.go 的 fakeClock：退避 2 秒起翻倍，每次 Pump 前把时钟推过退避点。

// pumpDeliverAttempt 做第 round+1 次投递尝试：先推过时钟推到当轮退避点
// （第 1 次不用等，之后 2,4,8,16… 秒），再 Pump。调用方逐轮调用并自己断言状态。
func pumpDeliverAttempt(t *testing.T, f *msgFixture, ctx context.Context, round int) {
	t.Helper()
	if round > 0 {
		f.clock.Add((2 << uint(round-1)) * time.Second)
	}
	f.hub.Pump(ctx)
}

// peekStageFails 读某阶段的预算计数（stageFails，不落盘）。
func peekStageFails(t *testing.T, h *Hub, id, stage string) int {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stageFails[id+"|"+stage]
}

// peekFirstFail 读预算起点：没记过（或已被 retry/成功清掉）返回零值 false。
func peekFirstFail(t *testing.T, h *Hub, id string) (time.Time, bool) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.firstFail[id]
	return v, ok
}

// 预算 1：Deliver 一直普通失败，第 defaultMaxAttempts 次转 failed，
// 报告 Stage=deliver、画面是 Capture 给的、对端状态是当时上报的 idle。
func TestDeliverBudgetExhaustedToFailed(t *testing.T) {
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
		return "对方终端最后画面", nil
	}
	boom := errors.New("tmux 注入失败")
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
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err) // 上报过 idle 才会真去投递
	}
	for k := 0; k < defaultMaxAttempts; k++ {
		pumpDeliverAttempt(t, f, ctx, k)
		want := MsgQueued
		if k == defaultMaxAttempts-1 {
			want = MsgFailed
		}
		if st := stateOf(t, f.hub, id); st != want {
			t.Fatalf("送达失败第 %d 次应为 %s，实际 %s", k+1, want, st)
		}
		if lm := peekHubMsg(t, f.hub, id); len(lm.Attempts) != k+1 {
			t.Errorf("第 %d 次失败后 Attempts 应记 %d 条，实际 %d", k+1, k+1, len(lm.Attempts))
		}
	}
	if _, ok := peekFirstFail(t, f.hub, id); !ok {
		t.Errorf("首次失败应已记预算起点 firstFail")
	}
	lm := peekHubMsg(t, f.hub, id)
	if lm.Reason != boom.Error() {
		t.Errorf("Reason 应为错误原文，实际 %q", lm.Reason)
	}
	if len(lm.Attempts) != defaultMaxAttempts {
		t.Errorf("Attempts 应有 %d 条，实际 %d", defaultMaxAttempts, len(lm.Attempts))
	}
	for i, a := range lm.Attempts {
		if a.Stage != "deliver" {
			t.Errorf("第 %d 条 Attempt 的 Stage 应为 deliver，实际 %q", i+1, a.Stage)
		}
		if a.Err != boom.Error() {
			t.Errorf("Attempt 应记错误原文，实际 %q", a.Err)
		}
	}
	if lm.Report == nil {
		t.Fatalf("转人工应带 FailReport")
	}
	if lm.Report.Stage != "deliver" {
		t.Errorf("Report.Stage 应为 deliver，实际 %q", lm.Report.Stage)
	}
	if lm.Report.Screen != "对方终端最后画面" {
		t.Errorf("Report.Screen 应为 Capture 的返回值，实际 %q", lm.Report.Screen)
	}
	if lm.Report.PeerState != string(PeerIdle) {
		t.Errorf("转人工时对端上报过 idle，PeerState 应为 idle，实际 %q", lm.Report.PeerState)
	}
	if n := len(f.delivered()); n != defaultMaxAttempts {
		t.Errorf("Deliver 应被调 %d 次，实际 %d", defaultMaxAttempts, n)
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
	// 已转人工：再推时钟 Pump 也不该碰它
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

// 预算 2：次数预算很大，时间窗先到（RetryWindow=1 分钟）也转人工；
// 退避没到点的 Pump 不碰线路、不推进时间窗判定。
func TestRetryWindowTriggersFailed(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	f.hub.cfg.MaxAttempts = 100
	f.hub.cfg.RetryWindow = time.Minute
	boom := errors.New("tmux 注入失败")
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
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx) // 首次失败：记下预算起点
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("首次失败应留在 queued 自动重试，实际 %s", st)
	}
	// 退避（2 秒）没到点：Pump 不该调 Deliver
	f.clock.Add(time.Second)
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 1 {
		t.Errorf("退避中不该重试投递，实际调了 %d 次", n)
	}
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("退避中应仍 queued，实际 %s", st)
	}
	// 推过退避并越过时间窗：第二次失败时窗口先到 → failed（次数远未到）
	f.clock.Add(2 * time.Minute)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgFailed {
		t.Fatalf("超过 RetryWindow=1 分钟应转 failed，实际 %s", st)
	}
	lm := peekHubMsg(t, f.hub, id)
	if lm.Report == nil || lm.Report.Stage != "deliver" {
		t.Errorf("应带 Stage=deliver 的报告，实际 %+v", lm.Report)
	}
	if len(lm.Attempts) != 2 {
		t.Errorf("Attempts 应为 2 条（次数预算没用完就被时间窗拦停），实际 %d", len(lm.Attempts))
	}
	if n := len(f.delivered()); n != 2 {
		t.Errorf("Deliver 应调 2 次，实际 %d", n)
	}
}

// 线路不通：不记 Attempt、不计次数、不退避，Hub 整体暂停；
// 暂停期间只有越过探测间隔（linkProbeEvery）的那一条 Pump 允许碰线路；
// 探测成功即解除暂停并把消息送出。
func TestLinkDownPauseProbeAndRecover(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	down := fmt.Errorf("ssh 断了: %w", ErrLinkDown)
	f.setDeliverErr(down)
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
	if !f.hub.LinkDown() {
		t.Fatalf("撞上线路不通应进入暂停")
	}
	lm := peekHubMsg(t, f.hub, id)
	if len(lm.Attempts) != 0 {
		t.Errorf("线路不通不该记 Attempt，实际 %d 条", len(lm.Attempts))
	}
	if !strings.Contains(lm.Reason, "线路不通") {
		t.Errorf("Reason 应含「线路不通」，实际 %q", lm.Reason)
	}
	if st := lm.State; st != MsgQueued {
		t.Errorf("应留在 queued 等线路恢复，实际 %s", st)
	}
	if next, fails := peekBackoff(t, f.hub, id); fails != 0 || !next.IsZero() {
		t.Errorf("线路不通不该退避计数，实际 fails=%d next=%v", fails, next)
	}
	if _, ok := peekFirstFail(t, f.hub, id); ok {
		t.Errorf("线路不通不该开预算窗，实际 firstFail 已记")
	}
	if n := len(f.delivered()); n != 1 {
		t.Fatalf("首次投递应调 1 次，实际 %d", n)
	}
	// 上一条 Pump 已经放过探测（记下 lastProbe）：间隔内再 Pump 必须按住不碰线路
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 2 {
		t.Fatalf("进暂停后的第一条 Pump 应放过探测（此刻 lastProbe 还是零值），实际调了 %d 次", n)
	}
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 2 {
		t.Errorf("探测间隔内不该再碰线路，实际调了 %d 次", n)
	}
	if !f.hub.LinkDown() {
		t.Errorf("探测没成功前应保持暂停")
	}
	// 越过探测间隔：放一条探测，仍不通
	f.clock.Add(linkProbeEvery)
	f.hub.Pump(ctx)
	if n := len(f.delivered()); n != 3 {
		t.Errorf("越过探测间隔应放探测，实际 Deliver 调了 %d 次", n)
	}
	if !f.hub.LinkDown() {
		t.Errorf("探测仍不通时应保持暂停")
	}
	if lm := peekHubMsg(t, f.hub, id); len(lm.Attempts) != 0 {
		t.Errorf("暂停期间的探测不该记 Attempt，实际 %d 条", len(lm.Attempts))
	}
	// 线路恢复：探测成功 → 解除暂停、消息送出
	f.clock.Add(time.Hour)
	f.setDeliverErr(nil)
	f.hub.Pump(ctx)
	if f.hub.LinkDown() {
		t.Errorf("探测成功后应解除暂停")
	}
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("恢复后消息应送出，实际 %s", st)
	}
	if lm := peekHubMsg(t, f.hub, id); len(lm.Attempts) != 0 {
		t.Errorf("整条线路不通的消息不该留下 Attempt，实际 %d 条", len(lm.Attempts))
	}
}

// 时间窗不计算暂停时间：预算窗口从首次真失败起算，但线路暂停的那段要扣掉。
// RetryWindow=30 分钟、次数预算放到很大：暂停 1 小时后恢复，
// 同一条消息第一次普通失败不该转 failed（墙钟已过 1 小时）；
// 之后累计的有效时间过窗才转人工。
func TestRetryWindowExcludesLinkDownPause(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	f.hub.cfg.MaxAttempts = 100
	f.hub.cfg.RetryWindow = 30 * time.Minute
	boom := errors.New("tmux 注入失败")
	down := fmt.Errorf("ssh 断了: %w", ErrLinkDown)
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
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 2; k++ { // 两次普通失败：firstFail 记下，暂停时长从这之后才扣
		pumpDeliverAttempt(t, f, ctx, k)
	}
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("两次失败远未到预算，应仍 queued，实际 %s", st)
	}
	first, ok := peekFirstFail(t, f.hub, id)
	if !ok {
		t.Fatalf("应已记预算起点 firstFail")
	}
	// 线路不通 → 暂停（不记 Attempt）；先推过上一轮的退避（4 秒）才会真投递
	f.setDeliverErr(down)
	f.clock.Add(4 * time.Second)
	f.hub.Pump(ctx)
	if !f.hub.LinkDown() {
		t.Fatalf("应进入线路暂停")
	}
	if lm := peekHubMsg(t, f.hub, id); len(lm.Attempts) != 2 {
		t.Errorf("暂停不该加 Attempt，实际 %d 条", len(lm.Attempts))
	}
	// 暂停 1 小时：期间的探测都撞回线路不通
	f.clock.Add(time.Hour)
	f.hub.Pump(ctx)
	f.clock.Add(linkProbeEvery)
	f.hub.Pump(ctx)
	if !f.hub.LinkDown() {
		t.Fatalf("探测仍不通时应保持暂停")
	}
	if lm := peekHubMsg(t, f.hub, id); len(lm.Attempts) != 2 {
		t.Errorf("暂停期间的探测不该加 Attempt，实际 %d 条", len(lm.Attempts))
	}
	// 探测间隔内先撞一次普通错误：应被按住，既不解除暂停也不计数
	f.setDeliverErr(boom)
	f.hub.Pump(ctx)
	if !f.hub.LinkDown() {
		t.Errorf("探测间隔内没碰线路，不该解除暂停")
	}
	if lm := peekHubMsg(t, f.hub, id); len(lm.Attempts) != 2 {
		t.Errorf("被按住的 Pump 不该加 Attempt，实际 %d 条", len(lm.Attempts))
	}
	// 越过探测间隔：普通错误撞回来——线路是通的（解除暂停），
	// 这条失败计入预算，但有效经过时间只有几分钟，不该转 failed
	f.clock.Add(linkProbeEvery)
	f.hub.Pump(ctx)
	if f.hub.LinkDown() {
		t.Fatalf("撞回普通错误说明线路已恢复，应解除暂停")
	}
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("恢复后第一次普通失败不该转 failed（暂停的 1 小时不算），实际 %s", st)
	}
	lm := peekHubMsg(t, f.hub, id)
	if len(lm.Attempts) != 3 {
		t.Errorf("恢复后的这次失败应记 Attempt，实际 %d 条", len(lm.Attempts))
	}
	if lm.Reason != boom.Error() {
		t.Errorf("Reason 应为错误原文，实际 %q", lm.Reason)
	}
	// 之后累计有效时间越过 30 分钟：时间窗到，转人工
	f.clock.Add(31 * time.Minute)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgFailed {
		t.Fatalf("有效时间越过 RetryWindow 应转 failed，实际 %s", st)
	}
	lm = peekHubMsg(t, f.hub, id)
	if lm.Report == nil || lm.Report.Stage != "deliver" {
		t.Errorf("应带 Stage=deliver 的报告，实际 %+v", lm.Report)
	}
	// 真失败只有 4 次（暂停期间的探测都不算），窗口却是从暂停前那次失败起算的
	if got := peekStageFails(t, f.hub, id, "deliver"); got != 4 {
		t.Errorf("deliver 阶段应累计 4 次真失败，实际 %d", got)
	}
	if ff, ok := peekFirstFail(t, f.hub, id); !ok || !ff.Equal(first) {
		t.Errorf("firstFail 应保持暂停前首次失败的时间 %v，实际 %v ok=%v", first, ff, ok)
	}
}

// 同步阶段同样按预算：前 4 次保持 queued 自动重试，第 5 次才 sync_failed，报告 Stage=sync。
func TestSyncBudgetFiveAttemptsToSyncFailed(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	boom := errors.New("ssh 断了")
	f.setSyncErr(boom)
	m := okMsg()
	m.From = "mac:srv" // 跨侧才允许带 repo
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
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < defaultMaxAttempts-1; k++ {
		if k > 0 {
			f.clock.Add((2 << uint(k-1)) * time.Second)
		}
		f.hub.Pump(ctx)
		if st := stateOf(t, f.hub, id); st != MsgQueued {
			t.Fatalf("同步失败第 %d 次应留在 queued 自动重试，实际 %s", k+1, st)
		}
	}
	if n := f.syncCount(); n != defaultMaxAttempts-1 {
		t.Errorf("Sync 应被调 %d 次，实际 %d", defaultMaxAttempts-1, n)
	}
	f.clock.Add((2 << uint(defaultMaxAttempts-2)) * time.Second)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgSyncFailed {
		t.Fatalf("同步失败 %d 次应转 sync_failed，实际 %s", defaultMaxAttempts, st)
	}
	if n := len(f.delivered()); n != 0 {
		t.Errorf("同步失败不该送达，实际 %d 条", n)
	}
	lm := peekHubMsg(t, f.hub, id)
	if lm.Report == nil || lm.Report.Stage != "sync" {
		t.Errorf("应带 Stage=sync 的报告，实际 %+v", lm.Report)
	}
	if len(lm.Attempts) != defaultMaxAttempts {
		t.Fatalf("Attempts 应有 %d 条，实际 %d", defaultMaxAttempts, len(lm.Attempts))
	}
	for i, a := range lm.Attempts {
		if a.Stage != "sync" {
			t.Errorf("第 %d 条 Attempt 的 Stage 应为 sync，实际 %q", i+1, a.Stage)
		}
	}
	if n := f.syncCount(); n != defaultMaxAttempts {
		t.Errorf("Sync 应共调 %d 次，实际 %d", defaultMaxAttempts, n)
	}
	if got := peekStageFails(t, f.hub, id, "sync"); got != defaultMaxAttempts {
		t.Errorf("sync 阶段预算计数应为 %d，实际 %d", defaultMaxAttempts, got)
	}
	// sync_failed 不再自动重试：推很久时钟也不动
	f.clock.Add(time.Hour)
	f.hub.Pump(ctx)
	if n := f.syncCount(); n != defaultMaxAttempts {
		t.Errorf("sync_failed 后不该再自动同步，实际调了 %d 次", n)
	}
}

// retry 重开预算：回 queued、报告清掉、Attempts 历史保留；
// 再失败要重新满 5 次才转人工。
func TestRetryResetsBudgetKeepsAttemptsHistory(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	boom := errors.New("tmux 注入失败")
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
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < defaultMaxAttempts; k++ { // 撞满送达预算转人工
		pumpDeliverAttempt(t, f, ctx, k)
	}
	if st := stateOf(t, f.hub, id); st != MsgFailed {
		t.Fatalf("应先转 failed，实际 %s", st)
	}
	if err := f.hub.Decide(id, "retry"); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("retry 后应回 queued，实际 %s", st)
	}
	lm := peekHubMsg(t, f.hub, id)
	if lm.Report != nil {
		t.Errorf("retry 后报告应清掉，实际 %+v", lm.Report)
	}
	if len(lm.Attempts) != defaultMaxAttempts {
		t.Errorf("retry 应保留 Attempts 历史（%d 条），实际 %d", defaultMaxAttempts, len(lm.Attempts))
	}
	if lm.Reason != "" {
		t.Errorf("retry 应清 Reason，实际 %q", lm.Reason)
	}
	if got := peekStageFails(t, f.hub, id, "deliver"); got != 0 {
		t.Errorf("retry 应清阶段预算，实际 %d", got)
	}
	if _, ok := peekFirstFail(t, f.hub, id); ok {
		t.Errorf("retry 应清预算起点，实际 firstFail 还在")
	}
	// 重新满 5 次才转人工：前 4 次仍 queued
	for k := 0; k < defaultMaxAttempts; k++ {
		pumpDeliverAttempt(t, f, ctx, k)
		want := MsgQueued
		if k == defaultMaxAttempts-1 {
			want = MsgFailed
		}
		if st := stateOf(t, f.hub, id); st != want {
			t.Fatalf("重开后第 %d 次失败应为 %s，实际 %s", k+1, want, st)
		}
	}
	if lm := peekHubMsg(t, f.hub, id); len(lm.Attempts) != 2*defaultMaxAttempts {
		t.Errorf("Attempts 历史应累计到 %d 条，实际 %d", 2*defaultMaxAttempts, len(lm.Attempts))
	}
}

// now 对 failed 可用：回 queued、Now=true，预算清零重开，能立刻再试到成功。
func TestDecideNowOnFailedMessage(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	boom := errors.New("tmux 注入失败")
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
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < defaultMaxAttempts; k++ { // 撞满送达预算转人工
		pumpDeliverAttempt(t, f, ctx, k)
	}
	if st := stateOf(t, f.hub, id); st != MsgFailed {
		t.Fatalf("应先转 failed，实际 %s", st)
	}
	if err := f.hub.Decide(id, "now"); err != nil {
		t.Fatalf("failed 应能 now: %v", err)
	}
	lm := peekHubMsg(t, f.hub, id)
	if lm.State != MsgQueued || !lm.Now {
		t.Errorf("now 后应 queued 且 Now=true，实际 state=%s now=%v", lm.State, lm.Now)
	}
	if lm.Report != nil {
		t.Errorf("now 等于 retry，报告应清掉，实际 %+v", lm.Report)
	}
	// 预算已清零：这次 Pump 撞一次普通失败也不该立刻转 failed
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgQueued {
		t.Fatalf("now 重开后失败一次不该转 failed，实际 %s", st)
	}
	// 恢复后 now 无视退避直接送
	f.setDeliverErr(nil)
	f.clock.Add(time.Hour)
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("now 应立刻送达，实际 %s", st)
	}
}

// Redirect：failed 消息改送给别人——新消息按正常消息入库，
// 原消息 rejected、Reason 写「已改送 #<新id>」并清掉预算；已送达的不能改送。
func TestRedirectFailedMessage(t *testing.T) {
	f := newMsgFixture()
	ctx := context.Background()
	boom := errors.New("tmux 注入失败")
	f.setDeliverErr(boom)
	m := okMsg() // ubuntu:macsrv -> mac:alice
	m.To = "ubuntu:bob"
	m.Text = "原消息正文"
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
	for k := 0; k < defaultMaxAttempts; k++ { // 撞满送达预算转人工
		pumpDeliverAttempt(t, f, ctx, k)
	}
	if st := stateOf(t, f.hub, id); st != MsgFailed {
		t.Fatalf("应先转 failed 再改送，实际 %s", st)
	}
	newID, err := f.hub.Redirect(id, "ubuntu:carol")
	if err != nil {
		t.Fatalf("failed 应能改送: %v", err)
	}
	if newID == id || !regexp16(newID) {
		t.Errorf("应返回新的 16 位十六进制 ID，实际 %q（原 %q）", newID, id)
	}
	nlm := peekHubMsg(t, f.hub, newID)
	if nlm.To != "ubuntu:carol" {
		t.Errorf("新消息 To 应为 ubuntu:carol，实际 %q", nlm.To)
	}
	if nlm.From != "ubuntu:macsrv" || nlm.Text != "原消息正文" {
		t.Errorf("新消息应照抄原消息的发件人与正文，实际 from=%q text=%q", nlm.From, nlm.Text)
	}
	if nlm.State != MsgPending {
		t.Errorf("手动模式下新消息应 pending 等新收件人批准，实际 %s", nlm.State)
	}
	lm := peekHubMsg(t, f.hub, id)
	if lm.State != MsgRejected {
		t.Errorf("原消息应 rejected，实际 %s", lm.State)
	}
	if want := fmt.Sprintf("已改送 #%s", newID); lm.Reason != want {
		t.Errorf("原消息 Reason 应为 %q，实际 %q", want, lm.Reason)
	}
	if next, fails := peekBackoff(t, f.hub, id); fails != 0 || !next.IsZero() {
		t.Errorf("改送后原消息退避应清零，实际 fails=%d next=%v", fails, next)
	}
	if _, ok := peekFirstFail(t, f.hub, id); ok {
		t.Errorf("改送后原消息预算起点应清掉")
	}
	if got := peekStageFails(t, f.hub, id, "deliver"); got != 0 {
		t.Errorf("改送后原消息阶段预算应清零，实际 %d", got)
	}
	// 已送达的消息不能改送
	f.setDeliverErr(nil)
	m2 := okMsg()
	m2.To = "ubuntu:carol"
	id2, err := f.hub.Send(m2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Decide(id2, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.SetState("ubuntu:carol", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id2); st != MsgDelivered {
		t.Fatalf("前置条件：应 delivered，实际 %s", st)
	}
	if _, err := f.hub.Redirect(id2, "ubuntu:dave"); err == nil {
		t.Errorf("已送达的消息不能改送")
	}
	// 非法收件人：报错且原消息不动
	m3 := okMsg()
	m3.To = "ubuntu:bob"
	id3, err := f.hub.Send(m3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Redirect(id3, "bad:addr"); err == nil {
		t.Errorf("非法收件人应报错")
	}
	if st := stateOf(t, f.hub, id3); st != MsgPending {
		t.Errorf("改送失败后原消息状态不该变，实际 %s", st)
	}
}

// ErrPeerNotClaude 不占预算：不记 Attempt、不开时间窗，
// 反复撞也不算失败；对面变回 Claude 后正常送达。
func TestPeerNotClaudeDoesNotConsumeBudget(t *testing.T) {
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
	// 反复撞：gone 闸门会把后续 Pump 按住，放开 idle 再撞也不计数
	for k := 0; k < defaultMaxAttempts+2; k++ {
		if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
			t.Fatal(err)
		}
		f.clock.Add(time.Minute)
		f.hub.Pump(ctx)
		if st := stateOf(t, f.hub, id); st != MsgQueued {
			t.Fatalf("撞上不是 Claude 不该转人工，第 %d 轮实际 %s", k+1, st)
		}
	}
	lm := peekHubMsg(t, f.hub, id)
	if len(lm.Attempts) != 0 {
		t.Errorf("ErrPeerNotClaude 不该记 Attempt，实际 %d 条", len(lm.Attempts))
	}
	if lm.Reason != notClaude.Error() {
		t.Errorf("Reason 应记错误原文，实际 %q", lm.Reason)
	}
	if _, ok := peekFirstFail(t, f.hub, id); ok {
		t.Errorf("不该开预算窗，实际 firstFail 已记")
	}
	if got := peekStageFails(t, f.hub, id, "deliver"); got != 0 {
		t.Errorf("deliver 阶段预算计数应为 0，实际 %d", got)
	}
	if n := len(f.delivered()); n != defaultMaxAttempts+2 {
		t.Errorf("每轮上报 idle 后都应撞一次投递，实际调了 %d 次", n)
	}
	// 对面变回 Claude：立刻送达，预算仍然干净
	f.setDeliverErr(nil)
	if err := f.hub.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	f.hub.Pump(ctx)
	if st := stateOf(t, f.hub, id); st != MsgDelivered {
		t.Fatalf("恢复后应送达，实际 %s", st)
	}
	if lm := peekHubMsg(t, f.hub, id); len(lm.Attempts) != 0 || lm.Report != nil {
		t.Errorf("送达后不该有 Attempt/报告，实际 attempts=%d report=%+v", len(lm.Attempts), lm.Report)
	}
}

// 落盘：failed 连同报告、Attempt、预算起点一起写进快照，
// 同路径新建 Hub 后状态/报告/次数都还在，且不会自动重开预算。
func TestFailedReportPersistedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "hub.json")
	clock := newFakeClock()
	ctx := context.Background()
	boom := errors.New("tmux 注入失败")
	var mu sync.Mutex
	var captureCalls int
	newCfg := func() HubConfig {
		return HubConfig{
			Now: clock.Now,
			Deliver: func(context.Context, Message) error {
				return boom
			},
			Capture: func(context.Context, string) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				captureCalls++
				return "重启前抓的画面", nil
			},
			StorePath: storePath,
		}
	}
	h := NewHub(newCfg())
	m := okMsg()
	m.To = "ubuntu:bob"
	id, err := h.Send(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := h.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < defaultMaxAttempts; k++ {
		if k > 0 {
			clock.Add((2 << uint(k-1)) * time.Second)
		}
		h.Pump(ctx)
	}
	if st := stateOf(t, h, id); st != MsgFailed {
		t.Fatalf("应转 failed，实际 %s", st)
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("落盘文件应存在: %v", err)
	}
	mu.Lock()
	callsBefore := captureCalls
	mu.Unlock()
	if callsBefore != 1 {
		t.Errorf("Capture 应调 1 次，实际 %d", callsBefore)
	}

	h2 := NewHub(newCfg())
	if st := stateOf(t, h2, id); st != MsgFailed {
		t.Fatalf("重启后应保持 failed，实际 %s", st)
	}
	lm := peekHubMsg(t, h2, id)
	if lm.Report == nil {
		t.Fatalf("重启后报告应还在")
	}
	if lm.Report.Stage != "deliver" || lm.Report.Screen != "重启前抓的画面" {
		t.Errorf("报告内容不对: %+v", lm.Report)
	}
	if lm.Reason != boom.Error() {
		t.Errorf("Reason 应还在，实际 %q", lm.Reason)
	}
	if len(lm.Attempts) != defaultMaxAttempts {
		t.Errorf("Attempts 应还在（%d 条），实际 %d", defaultMaxAttempts, len(lm.Attempts))
	}
	if _, ok := peekFirstFail(t, h2, id); !ok {
		t.Errorf("预算起点应随快照恢复")
	}
	// 重启不重开预算：failed 的消息推多久时钟都不会自己动
	clock.Add(time.Hour)
	h2.Pump(ctx)
	if st := stateOf(t, h2, id); st != MsgFailed {
		t.Fatalf("failed 不该被 Pump 推进，实际 %s", st)
	}
	mu.Lock()
	callsAfter := captureCalls
	mu.Unlock()
	if callsAfter != callsBefore {
		t.Errorf("failed 消息不该再抓屏，Capture 从 %d 变 %d", callsBefore, callsAfter)
	}
	// 人工 retry 才重新给预算
	if err := h2.Decide(id, "retry"); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, h2, id); st != MsgQueued {
		t.Fatalf("retry 后应 queued，实际 %s", st)
	}
	if got := peekStageFails(t, h2, id, "deliver"); got != 0 {
		t.Errorf("retry 后阶段预算应清零，实际 %d", got)
	}
	if lm := peekHubMsg(t, h2, id); lm.Report != nil {
		t.Errorf("retry 后报告应清掉，实际 %+v", lm.Report)
	}
}
