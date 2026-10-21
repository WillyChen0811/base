package support

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeProbe 是 PaneProbe 的替身：按地址返回预设结果，记录调用次数。
type fakeProbe struct {
	mu    sync.Mutex
	info  PaneInfo
	err   error
	calls int
}

func (p *fakeProbe) set(info PaneInfo, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.info, p.err = info, err
}

func (p *fakeProbe) probe(_ context.Context, _ string) (PaneInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.info, p.err
}

func (p *fakeProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newProbeFixture() (*msgFixture, *fakeProbe) {
	f := newMsgFixture()
	p := &fakeProbe{info: PaneInfo{Exists: true, Prog: "claude"}}
	f.hub.cfg.PaneProbe = p.probe
	return f, p
}

// 会话不存在：记 gone，正等确认的 injected 消息退回 queued（Reason 同 SetState(gone)），不记 Attempt。
func TestProbeSessionMissingGoneRequeues(t *testing.T) {
	f, p := newProbeFixture()
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
		t.Fatalf("应为 injected，实际 %s", st)
	}
	p.set(PaneInfo{Exists: false}, nil)
	f.clock.Add(probeThrottle + time.Second)
	peers := f.hub.PeersChecked(ctx)
	if peers["ubuntu:bob"] != PeerGone {
		t.Fatalf("会话不存在应记 gone，实际 %q", peers["ubuntu:bob"])
	}
	lm := peekHubMsg(t, f.hub, id)
	if lm.State != MsgQueued || lm.Reason != peerGoneReason {
		t.Fatalf("injected 应退回 queued 并写原因，实际 %s %q", lm.State, lm.Reason)
	}
	if len(lm.Attempts) != 0 {
		t.Errorf("退回不该记 Attempt：%v", lm.Attempts)
	}
}

// Pump 送之前核对：一直 busy 的地址会话没了，Pump 一轮后记 gone，消息不会被送出。
func TestProbePumpMarksGoneBeforeDeliver(t *testing.T) {
	f, p := newProbeFixture()
	ctx := context.Background()
	m := okMsg()
	m.To = "ubuntu:bob"
	id, _ := f.hub.Send(m)
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy)
	p.set(PaneInfo{Exists: false}, nil)
	f.hub.Pump(ctx)
	if got := f.hub.Peers()["ubuntu:bob"]; got != PeerGone {
		t.Fatalf("Pump 应先核对并记 gone，实际 %q", got)
	}
	if n := len(f.delivered()); n != 0 {
		t.Errorf("会话不存在不该送出，送了 %d 次", n)
	}
}

// 会话在、前台不是 Claude：busy/idle 改成 other；gone 不动；other 下 Claude 回来改 busy。
func TestProbeNotClaudeOther(t *testing.T) {
	f, p := newProbeFixture()
	ctx := context.Background()
	ackPeer(t, f.hub, "mac:codex1", PeerBusy)
	ackPeer(t, f.hub, "mac:shell1", PeerGone)
	p.set(PaneInfo{Exists: true, Prog: "codex"}, nil)
	peers := f.hub.PeersChecked(ctx)
	if peers["mac:codex1"] != PeerOther {
		t.Errorf("前台 codex 应为 other，实际 %q", peers["mac:codex1"])
	}
	if peers["mac:shell1"] != PeerGone {
		t.Errorf("gone 不该被改成 other，实际 %q", peers["mac:shell1"])
	}
	// codex 可送，shell 不可送
	f.hub.mu.Lock()
	if !f.hub.peerReadyLocked("mac:codex1") {
		t.Errorf("other + codex 应可送")
	}
	f.hub.peerProg["mac:codex1"] = "zsh"
	if f.hub.peerReadyLocked("mac:codex1") {
		t.Errorf("other + shell 不该可送")
	}
	f.hub.mu.Unlock()
	// Claude 回来
	p.set(PaneInfo{Exists: true, Prog: "2.1.141"}, nil)
	f.clock.Add(probeThrottle + time.Second)
	if got := f.hub.PeersChecked(ctx)["mac:codex1"]; got != PeerBusy {
		t.Errorf("前台回到 Claude 应改 busy 等钩子校正，实际 %q", got)
	}
}

// 钩子不能上报 other。
func TestSetStateRejectsOther(t *testing.T) {
	f := newMsgFixture()
	if err := f.hub.SetState("mac:x", PeerOther); err == nil {
		t.Fatal("SetState 不该接受 other")
	}
}

// 查询失败：状态不变（不管是线路不通还是别的错）。
func TestProbeFailureKeepsState(t *testing.T) {
	f, p := newProbeFixture()
	ctx := context.Background()
	ackPeer(t, f.hub, "ubuntu:bob", PeerBusy)
	for _, e := range []error{ErrLinkDown, errors.New("tmux 莫名其妙")} {
		p.set(PaneInfo{Exists: false}, e) // 即使 Exists=false，有 error 也不能当不存在
		f.clock.Add(probeThrottle + time.Second)
		if got := f.hub.PeersChecked(ctx)["ubuntu:bob"]; got != PeerBusy {
			t.Fatalf("查询失败(%v)状态应不变，实际 %q", e, got)
		}
	}
}

// 节流：5 秒内同一地址只查一次；过了 5 秒再查。
func TestProbeThrottle(t *testing.T) {
	f, p := newProbeFixture()
	ctx := context.Background()
	ackPeer(t, f.hub, "ubuntu:bob", PeerIdle)
	f.hub.PeersChecked(ctx)
	f.hub.PeersChecked(ctx)
	f.clock.Add(probeThrottle - time.Second)
	f.hub.PeersChecked(ctx)
	if n := p.count(); n != 1 {
		t.Fatalf("节流内应只查 1 次，实际 %d", n)
	}
	f.clock.Add(2 * time.Second)
	f.hub.PeersChecked(ctx)
	if n := p.count(); n != 2 {
		t.Fatalf("过节流后应再查，累计应 2 次，实际 %d", n)
	}
}

// 过期清理：gone 且会话不存在超过 24 小时、没有待送消息才清掉；有待送消息或不够久都留着。
func TestProbeGoneExpire(t *testing.T) {
	f, p := newProbeFixture()
	ctx := context.Background()
	p.set(PaneInfo{Exists: false}, nil)
	ackPeer(t, f.hub, "mac:old", PeerGone)
	ackPeer(t, f.hub, "mac:pending", PeerGone)
	m := okMsg()
	m.To = "mac:pending"
	id, _ := f.hub.Send(m)
	if err := f.hub.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}

	f.clock.Add(goneExpire - time.Hour)
	peers := f.hub.PeersChecked(ctx)
	if _, ok := peers["mac:old"]; !ok {
		t.Fatal("不到 24 小时不该清")
	}
	f.clock.Add(2 * time.Hour)
	peers = f.hub.PeersChecked(ctx)
	if _, ok := peers["mac:old"]; ok {
		t.Error("gone 超过 24 小时且会话不存在应清掉")
	}
	if _, ok := peers["mac:pending"]; !ok {
		t.Error("还有待送消息的地址不能清")
	}
	// 会话还在（shell）的 gone 也不清
	ackPeer(t, f.hub, "mac:shell", PeerGone)
	p.set(PaneInfo{Exists: true, Prog: "zsh"}, nil)
	f.clock.Add(goneExpire + time.Hour)
	if _, ok := f.hub.PeersChecked(ctx)["mac:shell"]; !ok {
		t.Error("会话还在的 gone 不该清")
	}
}

// PeersKick（界面轮询用）不等核对：探测卡住时立即返回当前状态，核对完成后的结果下一次可见。
func TestPeersKickDoesNotWait(t *testing.T) {
	f, _ := newProbeFixture()
	release := make(chan struct{})
	f.hub.cfg.PaneProbe = func(context.Context, string) (PaneInfo, error) {
		<-release
		return PaneInfo{Exists: false}, nil
	}
	ackPeer(t, f.hub, "mac:codex1", PeerBusy)

	done := make(chan map[string]PeerState, 1)
	go func() { done <- f.hub.PeersKick() }()
	select {
	case peers := <-done:
		if peers["mac:codex1"] != PeerBusy {
			t.Fatalf("核对没完成前应返回原状态，实际 %q", peers["mac:codex1"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PeersKick 不该等探测")
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for f.hub.Peers()["mac:codex1"] != PeerGone {
		if time.Now().After(deadline) {
			t.Fatalf("后台核对完成后应记 gone，实际 %q", f.hub.Peers()["mac:codex1"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
