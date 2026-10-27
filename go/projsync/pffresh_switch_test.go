package projsync

// pffresh_switch_test.go：评审2 修复 3/4 的行为测试——
//   3. 取最新的预算用完时，那一轮（含正在远程读文件的搬运）被取消并**等它真正停下**才返回「未能确认」；
//      返回之后不再有安装发生（旧代码会在预算之后把新版装进 Mac，调用方按旧文件写回就会盖掉新版）。
//   4. 取最新 / 本轮跑（runOne）拿到单元锁之后重读登记表：拿锁前刚点了取消同步（或 Rel/SyncPath 变了），
//      这一轮不跑，报「项目刚开始/取消同步，这一轮跳过」。
// 「取最新持锁期间调 unit/stop 等锁」的接口侧互斥在 wgtund 的 TestProjfilesUnitWaitsUnitLock 里验
// （两边用的是调度器导出的同一把单元锁）。

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pffsSlow 包一层本机 FS：Open 到匹配路径的文件后，第一次读拖 delay——
// 用来造「一轮真的在传、比预算久」的场景；拖完记下时间，好证明返回时它确实停下了。
type pffsSlow struct {
	PFLocalFS
	mu     sync.Mutex
	delay  time.Duration
	match  func(p string) bool
	opens  int
	reads  int
	seenAt []time.Time
}

func (f *pffsSlow) Open(p string) (PFTransferFile, error) {
	raw, err := f.PFLocalFS.Open(p)
	if err != nil {
		return nil, err
	}
	if f.match == nil || !f.match(p) {
		return raw, nil
	}
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return &pffsSlowFile{PFTransferFile: raw, slow: f}, nil
}

func (f *pffsSlow) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens, f.reads
}

type pffsSlowFile struct {
	PFTransferFile
	slow *pffsSlow
	once sync.Once
}

func (f *pffsSlowFile) ReadAt(b []byte, off int64) (int, error) {
	f.once.Do(func() {
		time.Sleep(f.slow.delay)
		f.slow.mu.Lock()
		f.slow.reads++
		f.slow.seenAt = append(f.slow.seenAt, time.Now())
		f.slow.mu.Unlock()
	})
	return f.PFTransferFile.ReadAt(b, off)
}

// 修复 3：预算用完 → 取消那一轮并等它停下才答「未能确认」；之后不再有安装、不再有远程读。
func TestPFFreshBudgetStopsRound(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.tr.Close()
	slow := &pffsSlow{delay: 2500 * time.Millisecond, match: func(p string) bool { return strings.HasSuffix(p, "slow.bin") }}
	var err error
	f.tr, err = NewPFTransfer(PFTransferConfig{Local: PFLocalFS{}, Remote: slow,
		LocalStaging: PFRoundMacStagingDir(f.meta), RemoteStaging: PFRemoteStagingDir(f.ub.meta),
		VerifyUp: f.ub.cl.TargetVerify()})
	if err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	f.s, err = NewPFSched(PFSchedConfig{MetaDir: f.meta, WorkDir: f.work, Remote: f.ub.cl, Transfer: f.tr,
		Settings: PFSchedSettings{Net: PFRemoteNetGood, Host: "测试Mac"}, Now: func() time.Time { return f.ub.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.s.Close)
	f.fr, err = NewPFFresh(f.s, PFFreshConfig{SyncDir: f.syn})
	if err != nil {
		t.Fatal(err)
	}
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"slow.bin": "旧"})
	f.ubPut(pfscA, "slow.bin", "旧", pfrtOld)
	f.settle()
	// Ubuntu 出了新版：30KB，估时极短（限速默认 5MB/s），但假 FS 第一次读要拖 2.5 秒 → 一定超 1 秒预算
	big := strings.Repeat("X", 30000)
	f.ubPut(pfscA, "slow.bin", big, pfrdOld(5))
	bp := filepath.Join(f.link(pfscA), "slow.bin")

	t0 := time.Now()
	res := f.fr.Paths(context.Background(), []string{bp}, 1*time.Second)
	el := time.Since(t0)
	if res.Items[0].State != PFFreshStateUnknown {
		t.Fatalf("预算内没跑完不能装懂: %+v note=%s", res.Items[0], res.Note)
	}
	if !strings.Contains(res.Note, "没能确认") || !strings.Contains(res.Note, "没取完，已停下") {
		t.Fatalf("note 要如实说明已停下、用的是本机现有文件: %q", res.Note)
	}
	if el > 15*time.Second {
		t.Fatalf("等它停下也该有界，不应挂 %v", el)
	}
	// 那一轮确实已经跑到传输上了（不是没开始就取消），而且返回时已经停下（读只发生过预算内那一次）
	if opens, reads := slow.counts(); opens == 0 || reads == 0 {
		t.Fatalf("预算内那一轮应已在远程读文件: opens=%d reads=%d", opens, reads)
	}
	if c, _ := f.macRead(pfscA, "slow.bin"); c != "旧" {
		t.Fatalf("答未确认时本机必须还是旧文件: %q", c[:min(8, len(c))])
	}
	// 过了「没取消就会装完」的窗口：不再有安装、也不再有多余的读
	o1, r1 := slow.counts()
	time.Sleep(4 * time.Second)
	if c, _ := f.macRead(pfscA, "slow.bin"); c != "旧" {
		t.Fatal("返回之后不许再有安装发生")
	}
	if o2, r2 := slow.counts(); o2 != o1 || r2 != r1 {
		t.Fatalf("返回之后那一轮必须已停下，不再碰远程文件: opens %d→%d reads %d→%d", o1, o2, r1, r2)
	}
}

// 修复 4（取最新侧）：被别的持锁者挡住时老实等；等到锁后重读登记表，
// 期间项目被取消了就这一轮不跑，答「项目刚开始/取消同步，这一轮跳过」，不装文件。
func TestPFFreshSkipsAfterRegistryChange(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"a.txt": "A"})
	f.ubPut(pfscA, "a.txt", "A", pfrtOld)
	f.settle()
	f.ubPut(pfscA, "a.txt", "Ubuntu 新版", pfrdOld(5))

	unlock, ok := f.s.LockUnit(context.Background(), pfscA, 0)
	if !ok {
		t.Fatal("拿单元锁")
	}
	type one struct{ res PFFreshResult }
	ch := make(chan one, 1)
	p := filepath.Join(f.link(pfscA), "a.txt")
	go func() { ch <- one{f.fr.Paths(context.Background(), []string{p}, 30*time.Second)} }()
	select {
	case got := <-ch:
		unlock()
		t.Fatalf("锁被占着时取最新不该已经给出答案: %+v note=%s", got.res.Items[0], got.res.Note)
	case <-time.After(150 * time.Millisecond):
	}
	// 等锁期间模拟 unit/stop 把登记改成已取消
	f.recs[0].State = PFUnitStopped
	f.saveUnits()
	unlock()
	var got one
	select {
	case got = <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("松锁后取最新必须收尾")
	}
	if got.res.Items[0].State != PFFreshStateUnknown || !strings.Contains(got.res.Note, PFSchedSkipReasonRegChanged) {
		t.Fatalf("拿锁后重读登记发现已取消，要如实报跳过: %+v note=%s", got.res.Items[0], got.res.Note)
	}
	if c, _ := f.macRead(pfscA, "a.txt"); c != "A" {
		t.Fatal("跳过的那一轮不许装任何文件")
	}
}

// 修复 4（本轮跑侧）：runOne 排队等锁，拿到锁后重读登记表发现项目刚被取消 → 这一轮跳过。
func TestPFSchedRunOneRereadsRegistry(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"a.txt": "A"})
	// Ubuntu 有更新：不跳过的话这一轮会把它装进 Mac
	f.ubPut(pfscA, "a.txt", "Ubuntu 新版", pfrdOld(5))

	unlock, ok := f.s.LockUnit(context.Background(), pfscA, 0)
	if !ok {
		t.Fatal("拿单元锁")
	}
	ch := make(chan *PFSchedReport, 1)
	go func() { ch <- f.round(PFSchedRoundOptions{}) }()
	time.Sleep(300 * time.Millisecond) // 让本轮跑到 runOne，卡在单元锁上
	f.recs[0].State = PFUnitStopped
	f.saveUnits()
	unlock()

	var rep *PFSchedReport
	select {
	case rep = <-ch:
	case <-time.After(30 * time.Second):
		t.Fatal("松锁后本轮必须收尾")
	}
	u := rep.unit(pfscA)
	if u == nil || u.Result == nil {
		t.Fatalf("排队的那一轮要在报告里: %s", pfscDump(rep))
	}
	if u.Result.Outcome != PFRoundSkipped || !strings.Contains(u.Result.Reason, PFSchedSkipReasonRegChanged) {
		t.Fatalf("拿锁后重读登记，已取消就该跳过并报原因: action=%v result=%s reason=%s",
			u.Action, u.Result.Outcome, u.Result.Reason)
	}
	if c, _ := f.macRead(pfscA, "a.txt"); c != "A" {
		t.Fatal("跳过不许装文件")
	}
}
