package projsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 两个临时仓库分别扮演 Mac（本地）与 Ubuntu（经进程内 RemoteRunner 调 ServeFacts / ServeApplyWith）。

type rpair struct{ mac, ubu string }

func newPair(t *testing.T) rpair {
	t.Helper()
	isolateGitConfig(t)
	ubu := newRepo(t)
	mac := filepath.Join(t.TempDir(), "mac")
	if out, err := exec.Command("git", "clone", "-q", ubu, mac).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	ag(t, mac, "config", "user.name", "test")
	ag(t, mac, "config", "user.email", "t@t")
	ag(t, mac, "config", "commit.gpgsign", "false")
	return rpair{mac: mac, ubu: ubu}
}

// ubuRunner 在进程内模拟 Ubuntu 的 xxyi-msg；可注入「第 N 次 apply 执行后通信失败」。
type ubuRunner struct {
	mu          sync.Mutex
	verdict     string // 所有工作区的 agent 结论，默认 unknown
	applies     int
	failApplyAt int // >0：第 N 次 apply 照常执行，但返回通信错误
	factsErr    bool
}

func (u *ubuRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f := strings.Fields(cmd)
	v := u.verdict
	if v == "" {
		v = "unknown"
	}
	switch f[1] {
	case SubcmdFacts:
		if u.factsErr {
			return nil, nil, -1, fmt.Errorf("注入：连接断开")
		}
		out, err := ServeFacts(ctx, stdin, func(ctx context.Context, wt string) AgentVerdict {
			return AgentVerdict{Verdict: v, Notes: []string{"假 judge"}}
		})
		if err != nil {
			return nil, []byte(err.Error()), 2, nil
		}
		return out, nil, 0, nil
	case SubcmdApply:
		u.applies++
		out, err := ServeApplyWith(ctx, stdin, func(ctx context.Context, wt string) (bool, string) {
			return v == "finished", "假 judge"
		})
		if u.failApplyAt > 0 && u.applies == u.failApplyAt {
			return nil, nil, -1, fmt.Errorf("注入：连接中断")
		}
		if err != nil {
			return nil, []byte(err.Error()), 2, nil
		}
		return out, nil, 0, nil
	}
	return nil, nil, -1, fmt.Errorf("未知子命令")
}

func (p rpair) env(r RemoteRunner) RoundEnv {
	return RoundEnv{Project: "proj", Repo: p.mac, RemoteRepo: p.ubu, XxyiMsg: "/x/xxyi-msg", Remote: r,
		Exchange: func(ctx context.Context) error {
			_, err := SyncGit(ctx, p.mac, p.ubu, nil, "mac", "ubuntu")
			return err
		}}
}

func findBranch(t *testing.T, res RoundResult, name string) BranchOutcome {
	t.Helper()
	for _, b := range res.Branches {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("结果里没有分支 %s: %+v", name, res.Branches)
	return BranchOutcome{}
}

func hasAtt(res RoundResult, kind string) *AttentionItem {
	for i := range res.Attention {
		if res.Attention[i].Kind == kind {
			return &res.Attention[i]
		}
	}
	return nil
}

func TestRoundMacAheadFastForwardsUbuntu(t *testing.T) {
	p := newPair(t)
	c := commitFile(t, p.mac, "m.txt", "m\n")
	res := RunRound(context.Background(), p.env(&ubuRunner{}))
	if res.Error != "" || res.Stopped || res.NeedsAttention {
		t.Fatalf("%+v", res)
	}
	if rev(t, p.ubu, "main") != c {
		t.Fatal("Ubuntu 应快进到 Mac 的提交")
	}
	if _, err := os.Stat(filepath.Join(p.ubu, "m.txt")); err != nil {
		t.Fatal("Ubuntu 检出的工作区文件应随之更新")
	}
	if b := findBranch(t, res, "main"); b.Result != ResultDone || b.Action != KindFastForward {
		t.Fatalf("%+v", b)
	}
	if res.Finished.Before(res.Started) || res.Started.IsZero() {
		t.Fatal("时间戳")
	}
}

func TestRoundUbuntuAheadFastForwardsMac(t *testing.T) {
	p := newPair(t)
	c := commitFile(t, p.ubu, "u.txt", "u\n")
	res := RunRound(context.Background(), p.env(&ubuRunner{}))
	if res.Error != "" || res.NeedsAttention {
		t.Fatalf("%+v", res)
	}
	if rev(t, p.mac, "main") != c {
		t.Fatal("Mac 应快进")
	}
	if _, err := os.Stat(filepath.Join(p.mac, "u.txt")); err != nil {
		t.Fatal("Mac 工作区文件应更新")
	}
}

func TestRoundAutoMergeBothSides(t *testing.T) {
	p := newPair(t)
	m := commitFile(t, p.mac, "a.txt", "a\n")
	u := commitFile(t, p.ubu, "b.txt", "b\n")
	res := RunRound(context.Background(), p.env(&ubuRunner{}))
	if res.Error != "" || res.Stopped || res.NeedsAttention {
		t.Fatalf("%+v", res)
	}
	mm, uu := rev(t, p.mac, "main"), rev(t, p.ubu, "main")
	if mm != uu || mm == m || mm == u {
		t.Fatalf("两边都应到合并提交: mac %s ubuntu %s", mm, uu)
	}
	if rev(t, p.mac, "main^1") != m || rev(t, p.mac, "main^2") != u {
		t.Fatal("父提交顺序应为 mac 在前、ubuntu 在后")
	}
	if rev(t, p.ubu, "main^1") != m {
		t.Fatal("Ubuntu 上父提交相同")
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		for _, d := range []string{p.mac, p.ubu} {
			if _, err := os.Stat(filepath.Join(d, f)); err != nil {
				t.Fatalf("%s 缺 %s", d, f)
			}
		}
	}
	if b := findBranch(t, res, "main"); b.Result != ResultDone || b.Action != KindAutoMerge || b.Target != mm {
		t.Fatalf("%+v", b)
	}
}

func TestRoundConflictLeavesBothUntouched(t *testing.T) {
	p := newPair(t)
	m := commitFile(t, p.mac, "README.md", "mac side\n")
	u := commitFile(t, p.ubu, "README.md", "ubuntu side\n")
	res := RunRound(context.Background(), p.env(&ubuRunner{}))
	if rev(t, p.mac, "main") != m || rev(t, p.ubu, "main") != u {
		t.Fatal("冲突时两边都不应动")
	}
	a := hasAtt(res, AttConflict)
	if a == nil || a.Branch != "main" || !strings.Contains(a.Detail, "README.md") || !res.NeedsAttention {
		t.Fatalf("应有冲突事项: %+v", res.Attention)
	}
	if b := findBranch(t, res, "main"); b.Result != ResultSkipped || b.Action != KindConflict {
		t.Fatalf("%+v", b)
	}
	if hasAtt(res, AttApplyFailed) != nil {
		t.Fatal("冲突不是执行故障")
	}
}

func TestRoundCommitsFinishedUbuntuThenSyncs(t *testing.T) {
	p := newPair(t)
	os.WriteFile(filepath.Join(p.ubu, "x.txt"), []byte("x\n"), 0o644)
	res := RunRound(context.Background(), p.env(&ubuRunner{verdict: "finished"}))
	if res.Error != "" || res.Stopped || res.NeedsAttention || !res.Recollected {
		t.Fatalf("%+v", res)
	}
	if len(res.Worktrees) != 1 || res.Worktrees[0].Action != WtCommitted || res.Worktrees[0].Side != SideUbuntu {
		t.Fatalf("%+v", res.Worktrees)
	}
	head := rev(t, p.ubu, "main")
	if head == rev(t, p.ubu, "main~1") || !strings.Contains(ag(t, p.ubu, "log", "-1", "--format=%s"), "XXYI 自动提交") {
		t.Fatal("Ubuntu 应已自动提交")
	}
	if rev(t, p.mac, "main") != head {
		t.Fatal("提交后应同步到 Mac")
	}
	if _, err := os.Stat(filepath.Join(p.mac, "x.txt")); err != nil {
		t.Fatal("Mac 工作区应有 x.txt")
	}
}

func TestRoundCommitsFinishedMacLocal(t *testing.T) {
	p := newPair(t)
	os.WriteFile(filepath.Join(p.mac, "y.txt"), []byte("y\n"), 0o644)
	env := p.env(&ubuRunner{})
	env.LocalJudge = func(ctx context.Context, wt string) AgentVerdict { return AgentVerdict{Verdict: "finished"} }
	res := RunRound(context.Background(), env)
	if res.Error != "" || res.NeedsAttention || len(res.Worktrees) != 1 || res.Worktrees[0].Action != WtCommitted || res.Worktrees[0].Side != SideMac {
		t.Fatalf("%+v", res)
	}
	if rev(t, p.ubu, "main") != rev(t, p.mac, "main") {
		t.Fatal("应同步到 Ubuntu")
	}
}

func TestRoundUncertainDirtySkipsOnlyThatBranch(t *testing.T) {
	p := newPair(t)
	os.WriteFile(filepath.Join(p.ubu, "wip.txt"), []byte("wip\n"), 0o644) // verdict unknown
	c := commitFile(t, p.mac, "m.txt", "m\n")
	ag(t, p.mac, "branch", "feat", c)
	before := rev(t, p.ubu, "main")
	res := RunRound(context.Background(), p.env(&ubuRunner{}))
	if rev(t, p.ubu, "main") != before {
		t.Fatal("状态不确定：不得提交、不得更新被挡住的分支")
	}
	if ag(t, p.ubu, "status", "--porcelain") != "?? wip.txt" {
		t.Fatal("未提交改动应原样保留")
	}
	a := hasAtt(res, AttUncertainDirt)
	if a == nil || !a.CanCommit || a.Side != SideUbuntu || a.Worktree != p.ubu || !strings.Contains(a.Message, "状态不确定") {
		t.Fatalf("%+v", res.Attention)
	}
	if b := findBranch(t, res, "main"); b.Result != ResultSkipped || !strings.Contains(b.Detail, "状态不确定") {
		t.Fatalf("%+v", b)
	}
	// 其它分支不受影响：feat 在 Ubuntu 被创建
	if rev(t, p.ubu, "feat") != c || findBranch(t, res, "feat").Result != ResultDone {
		t.Fatal("没被挡住的分支应照常同步")
	}
	if res.Stopped {
		t.Fatal("状态不确定不是停止原因")
	}
}

func TestRoundHookRejectRecordsAndStopsWrites(t *testing.T) {
	p := newPair(t)
	hook := filepath.Join(p.ubu, ".git", "hooks", "pre-commit")
	write(t, hook, "#!/bin/sh\necho HOOK-REJECT-MSG >&2\nexit 1\n")
	os.Chmod(hook, 0o755)
	os.WriteFile(filepath.Join(p.ubu, "x.txt"), []byte("x\n"), 0o644)
	c := commitFile(t, p.mac, "m.txt", "m\n")
	ag(t, p.mac, "branch", "feat", c)
	res := RunRound(context.Background(), p.env(&ubuRunner{verdict: "finished"}))
	a := hasAtt(res, AttCommitFailed)
	if a == nil || !strings.Contains(a.Detail, "HOOK-REJECT-MSG") || !res.Stopped || !res.NeedsAttention {
		t.Fatalf("应记录提交失败: %+v", res)
	}
	if len(res.Worktrees) != 1 || res.Worktrees[0].Action != WtCommitFailed {
		t.Fatalf("%+v", res.Worktrees)
	}
	if exec.Command("git", "-C", p.ubu, "rev-parse", "--verify", "-q", "refs/heads/feat").Run() == nil {
		t.Fatal("提交失败后本项目不应继续写（feat 不应被创建）")
	}
	if rev(t, p.ubu, "main") == rev(t, p.mac, "main") {
		t.Fatal("main 也不应被更新")
	}
}

func TestRoundRemoteCommFailureStopsFurtherWrites(t *testing.T) {
	p := newPair(t)
	c := commitFile(t, p.mac, "m.txt", "m\n")
	ag(t, p.mac, "branch", "b1", c)
	ag(t, p.mac, "branch", "b2", c)
	u := &ubuRunner{failApplyAt: 1}
	res := RunRound(context.Background(), p.env(u))
	if !res.Stopped || !res.NeedsAttention {
		t.Fatalf("%+v", res)
	}
	a := hasAtt(res, AttNeedRecheck)
	if a == nil || !strings.Contains(a.Message, "结果不确定，需重新确认") {
		t.Fatalf("%+v", res.Attention)
	}
	if u.applies != 1 {
		t.Fatalf("通信失败后不得再发写请求，实际 %d 次", u.applies)
	}
	var uncertain, notRun int
	for _, b := range res.Branches {
		switch b.Result {
		case ResultUncertain:
			uncertain++
		case ResultNotRun:
			notRun++
		}
	}
	if uncertain != 1 || notRun < 2 {
		t.Fatalf("应有 1 个不确定、其余未执行: %+v", res.Branches)
	}
	if exec.Command("git", "-C", p.ubu, "rev-parse", "--verify", "-q", "refs/heads/b2").Run() == nil {
		t.Fatal("b2 不应被创建")
	}
}

func TestRoundRemoteFactsFailureIsUncertain(t *testing.T) {
	p := newPair(t)
	c := commitFile(t, p.mac, "m.txt", "m\n")
	res := RunRound(context.Background(), p.env(&ubuRunner{factsErr: true}))
	if !res.Stopped || hasAtt(res, AttNeedRecheck) == nil || res.Error == "" {
		t.Fatalf("%+v", res)
	}
	if rev(t, p.ubu, "main") == c {
		t.Fatal("不应写")
	}
}

func TestRoundOutOfScopeWritesNothing(t *testing.T) {
	p := newPair(t)
	ag(t, p.ubu, "branch", "a=b")
	commitFile(t, p.mac, "m.txt", "m\n")
	before := rev(t, p.ubu, "main")
	res := RunRound(context.Background(), p.env(&ubuRunner{}))
	if !res.OutOfScope || hasAtt(res, AttOutOfScope) == nil || len(res.Branches) != 0 {
		t.Fatalf("%+v", res)
	}
	if rev(t, p.ubu, "main") != before {
		t.Fatal("超出范围不得写")
	}
	if exec.Command("git", "-C", p.mac, "rev-parse", "--verify", "-q", "refs/heads/a=b").Run() == nil {
		t.Fatal("Mac 不得创建 a=b")
	}
}

func TestRoundForceCommitIgnoresVerdict(t *testing.T) {
	p := newPair(t)
	os.WriteFile(filepath.Join(p.ubu, "x.txt"), []byte("x\n"), 0o644)
	env := p.env(&ubuRunner{}) // verdict unknown
	env.Force = []ForceCommit{{Side: SideUbuntu, Worktree: p.ubu}}
	res := RunRound(context.Background(), env)
	if len(res.Worktrees) != 1 || res.Worktrees[0].Action != WtCommitted || res.Worktrees[0].Verdict != "用户指定" {
		t.Fatalf("%+v", res)
	}
	if rev(t, p.mac, "main") != rev(t, p.ubu, "main") || res.NeedsAttention {
		t.Fatalf("提交后应同步: %+v", res)
	}
}

func TestRoundForceUnknownWorktreeNoted(t *testing.T) {
	p := newPair(t)
	env := p.env(&ubuRunner{})
	env.Force = []ForceCommit{{Side: SideUbuntu, Worktree: "/nonexistent"}}
	res := RunRound(context.Background(), env)
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "未提交") {
		t.Fatalf("%+v", res.Notes)
	}
}

func TestRoundSecondConcurrentRoundIsLocked(t *testing.T) {
	p := newPair(t)
	entered, release := make(chan struct{}), make(chan struct{})
	env1 := p.env(&ubuRunner{})
	env1.Exchange = func(ctx context.Context) error {
		close(entered)
		<-release
		return nil
	}
	done := make(chan RoundResult)
	go func() { done <- RunRound(context.Background(), env1) }()
	<-entered
	// 用同仓库的另一个入口（linked worktree 路径）也应拿同一把锁
	wt := p.mac + "-wt"
	ag(t, p.mac, "worktree", "add", "-q", "-b", "side", wt)
	env2 := p.env(&ubuRunner{})
	env2.Repo = wt
	res2 := RunRound(context.Background(), env2)
	if !res2.Skipped || !strings.Contains(res2.SkipReason, "上一轮还在进行") {
		t.Fatalf("第二轮应被挡住: %+v", res2)
	}
	res3 := RunRound(context.Background(), p.env(&ubuRunner{}))
	if !res3.Skipped {
		t.Fatal("同路径第二轮也应被挡住")
	}
	close(release)
	if r1 := <-done; r1.Skipped {
		t.Fatal("第一轮不应被跳过")
	}
	// 锁已释放
	if r := RunRound(context.Background(), p.env(&ubuRunner{})); r.Skipped {
		t.Fatalf("释放后应能再跑: %+v", r)
	}
}

func TestRoundNowInjected(t *testing.T) {
	p := newPair(t)
	fixed := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	env := p.env(&ubuRunner{})
	env.Now = func() time.Time { return fixed }
	res := RunRound(context.Background(), env)
	if !res.Started.Equal(fixed) || !res.Finished.Equal(fixed) {
		t.Fatalf("%v %v", res.Started, res.Finished)
	}
}
