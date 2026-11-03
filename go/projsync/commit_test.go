package projsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateGitConfig 把 HOME / XDG_CONFIG_HOME 指到临时目录，隔离用户自己的 git 配置（cleanEnv 会清 GIT_*，所以不用 GIT_CONFIG_GLOBAL）。
func isolateGitConfig(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, "xdg"))
}

func commitRepo(t *testing.T) (dir, head string) {
	t.Helper()
	isolateGitConfig(t)
	dir = newRepo(t)
	commitFile(t, dir, ".gitignore", "*.log\nbuild/\n")
	return dir, rev(t, dir, "HEAD")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func finishedJudge(called *int) CommitJudge {
	return func(ctx context.Context, wt string) (bool, string) {
		if called != nil {
			*called++
		}
		return true, "测试：已完成"
	}
}

func busyJudge(ctx context.Context, wt string) (bool, string) { return false, "测试：仍在忙" }

func basePlan(dir, head string) CommitPlan {
	return CommitPlan{RepoDir: dir, Worktree: dir, Branch: "main", ExpectHead: head}
}

func TestAutoCommitSuccess(t *testing.T) {
	dir, head := commitRepo(t)
	write(t, filepath.Join(dir, "README.md"), "changed\n")
	write(t, filepath.Join(dir, "new.txt"), "new\n")
	write(t, filepath.Join(dir, "debug.log"), "ignored\n")
	write(t, filepath.Join(dir, "build", "out.bin"), "ignored\n")
	oid, err := AutoCommit(context.Background(), basePlan(dir, head), finishedJudge(nil))
	if err != nil {
		t.Fatal(err)
	}
	if rev(t, dir, "HEAD") != oid || rev(t, dir, "HEAD^") != head {
		t.Fatalf("新提交 %s 父应为 %s", oid, head)
	}
	files := ag(t, dir, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(files, "new.txt") || strings.Contains(files, "debug.log") || strings.Contains(files, "build/") {
		t.Fatalf("提交内容: %s", files)
	}
	if st := ag(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("提交后仍有改动: %s", st)
	}
	if msg := ag(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, "projsync 自动提交：") || !strings.Contains(msg, "new.txt") {
		t.Fatalf("默认提交信息: %q", msg)
	}
	// 自定义信息
	write(t, filepath.Join(dir, "b.txt"), "b\n")
	p := basePlan(dir, oid)
	p.Message = "我的信息"
	if _, err := AutoCommit(context.Background(), p, finishedJudge(nil)); err != nil || ag(t, dir, "log", "-1", "--format=%s") != "我的信息" {
		t.Fatalf("自定义信息: %v", err)
	}
}

func TestAutoCommitJudgeAndForce(t *testing.T) {
	dir, head := commitRepo(t)
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	before := snapshot(t, dir)
	_, err := AutoCommit(context.Background(), basePlan(dir, head), busyJudge)
	if !errors.Is(err, ErrNotFinished) || !strings.Contains(err.Error(), "仍在忙") || snapshot(t, dir) != before {
		t.Fatalf("%v", err)
	}
	if _, err := AutoCommit(context.Background(), basePlan(dir, head), nil); !errors.Is(err, ErrNotFinished) {
		t.Fatalf("nil judge: %v", err)
	}
	p := basePlan(dir, head)
	p.Force = true
	called := 0
	if _, err := AutoCommit(context.Background(), p, func(context.Context, string) (bool, string) { called++; return false, "" }); err != nil || called != 0 {
		t.Fatalf("Force 应跳过 judge: %v called=%d", err, called)
	}
}

func TestAutoCommitNothing(t *testing.T) {
	dir, head := commitRepo(t)
	write(t, filepath.Join(dir, "x.log"), "ignored only\n")
	_, err := AutoCommit(context.Background(), basePlan(dir, head), finishedJudge(nil))
	if !errors.Is(err, ErrNothingToCommit) || rev(t, dir, "HEAD") != head {
		t.Fatalf("%v", err)
	}
}

func TestAutoCommitRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("HEAD 不符", func(t *testing.T) {
		dir, head := commitRepo(t)
		write(t, filepath.Join(dir, "a.txt"), "a\n")
		before := snapshot(t, dir)
		p := basePlan(dir, head)
		p.ExpectHead = strings.Repeat("a", 40)
		_, err := AutoCommit(ctx, p, finishedJudge(nil))
		if !errors.Is(err, ErrRefsChanged) || snapshot(t, dir) != before {
			t.Fatalf("%v", err)
		}
	})
	t.Run("分支不符", func(t *testing.T) {
		dir, head := commitRepo(t)
		ag(t, dir, "branch", "other")
		write(t, filepath.Join(dir, "a.txt"), "a\n")
		before := snapshot(t, dir)
		p := basePlan(dir, head)
		p.Branch = "other"
		_, err := AutoCommit(ctx, p, finishedJudge(nil))
		if !errors.Is(err, ErrRefsChanged) || snapshot(t, dir) != before {
			t.Fatalf("%v", err)
		}
	})
	t.Run("进行中的 merge", func(t *testing.T) {
		dir, head := commitRepo(t)
		write(t, filepath.Join(dir, "a.txt"), "a\n")
		write(t, filepath.Join(dir, ".git", "MERGE_HEAD"), head+"\n")
		before := snapshot(t, dir)
		_, err := AutoCommit(ctx, basePlan(dir, head), finishedJudge(nil))
		if err == nil || !strings.Contains(err.Error(), "MERGE_HEAD") || snapshot(t, dir) != before {
			t.Fatalf("%v", err)
		}
	})
	t.Run("超出范围：分支名含=", func(t *testing.T) {
		dir, _ := commitRepo(t)
		ag(t, dir, "checkout", "-q", "-b", "a=b")
		head := rev(t, dir, "HEAD")
		write(t, filepath.Join(dir, "a.txt"), "a\n")
		before := snapshot(t, dir)
		p := basePlan(dir, head)
		p.Branch = "a=b"
		_, err := AutoCommit(ctx, p, finishedJudge(nil))
		var oos *ErrOutOfScope
		if !errors.As(err, &oos) || snapshot(t, dir) != before {
			t.Fatalf("%v", err)
		}
	})
	t.Run("嵌套仓库", func(t *testing.T) {
		dir, head := commitRepo(t)
		inner := filepath.Join(dir, "inner")
		os.MkdirAll(inner, 0o755)
		ag(t, inner, "init", "-q", "-b", "main")
		write(t, filepath.Join(inner, "f"), "f")
		ag(t, inner, "add", "f")
		ag(t, inner, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-qm", "i")
		_, err := AutoCommit(ctx, basePlan(dir, head), finishedJudge(nil))
		var oos *ErrOutOfScope
		if !errors.As(err, &oos) || rev(t, dir, "HEAD") != head || ag(t, dir, "diff", "--cached", "--name-only") != "" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("字段不合法", func(t *testing.T) {
		dir, head := commitRepo(t)
		p := basePlan(dir, head)
		p.ExpectHead = "zzz"
		if _, err := AutoCommit(ctx, p, nil); err == nil {
			t.Fatal("应拒绝")
		}
		p = basePlan(dir, head)
		p.Worktree = "relative"
		if _, err := AutoCommit(ctx, p, nil); err == nil {
			t.Fatal("应拒绝")
		}
	})
}

func TestAutoCommitHookRejects(t *testing.T) {
	dir, head := commitRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	write(t, hook, "#!/bin/sh\ntouch '"+marker+"'\necho HOOK-REJECT-MSG >&2\nexit 1\n")
	os.Chmod(hook, 0o755)
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	_, err := AutoCommit(context.Background(), basePlan(dir, head), finishedJudge(nil))
	var cf *ErrCommitFailed
	if !errors.As(err, &cf) || cf.Stage != "commit" || !strings.Contains(cf.Output, "HOOK-REJECT-MSG") || !strings.Contains(err.Error(), "已暂存但未提交") {
		t.Fatalf("%v", err)
	}
	if _, e := os.Stat(marker); e != nil {
		t.Fatal("钩子没有被执行")
	}
	if rev(t, dir, "HEAD") != head || ag(t, dir, "diff", "--cached", "--name-only") != "a.txt" {
		t.Fatal("HEAD 应不变、改动应仍在暂存区")
	}
}

func TestAutoCommitNoIdentity(t *testing.T) {
	dir, head := commitRepo(t)
	ag(t, dir, "config", "--unset", "user.name")
	ag(t, dir, "config", "--unset", "user.email")
	ag(t, dir, "config", "user.useConfigOnly", "true")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	_, err := AutoCommit(context.Background(), basePlan(dir, head), finishedJudge(nil))
	var cf *ErrCommitFailed
	if !errors.As(err, &cf) || cf.Stage != "commit" || rev(t, dir, "HEAD") != head {
		t.Fatalf("%v", err)
	}
}

func TestAutoCommitLinkedWorktree(t *testing.T) {
	dir, head := commitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	ag(t, dir, "worktree", "add", "-q", "-b", "feat", wt)
	write(t, filepath.Join(wt, "f.txt"), "f\n")
	write(t, filepath.Join(dir, "main-dirty.txt"), "m\n") // 主工作区的改动不能被带进去
	p := CommitPlan{RepoDir: dir, Worktree: wt, Branch: "feat", ExpectHead: head}
	oid, err := AutoCommit(context.Background(), p, finishedJudge(nil))
	if err != nil {
		t.Fatal(err)
	}
	if rev(t, wt, "HEAD") != oid || rev(t, wt, "HEAD^") != head || rev(t, dir, "main") != head {
		t.Fatal("提交位置不对")
	}
	if files := ag(t, dir, "ls-tree", "-r", "--name-only", oid); !strings.Contains(files, "f.txt") || strings.Contains(files, "main-dirty") {
		t.Fatalf("内容: %s", files)
	}
	// Worktree 不属于该仓库
	other := t.TempDir()
	if _, err := AutoCommit(context.Background(), CommitPlan{RepoDir: dir, Worktree: other, Branch: "main", ExpectHead: head}, nil); err == nil {
		t.Fatal("应拒绝")
	}
}

func TestDefaultCommitMessage(t *testing.T) {
	if m := defaultCommitMessage([]string{"a", "b"}); m != "projsync 自动提交：a、b" {
		t.Fatal(m)
	}
	var ps []string
	for i := 0; i < 12; i++ {
		ps = append(ps, fmt.Sprintf("f%02d", i))
	}
	m := defaultCommitMessage(ps)
	if !strings.Contains(m, "f09") || strings.Contains(m, "f10") || !strings.HasSuffix(m, "等 12 个") {
		t.Fatal(m)
	}
	if m := defaultCommitMessage([]string{"a\nb"}); strings.Contains(m, "\n") {
		t.Fatal(m)
	}
	// 解析：重命名、含空格路径
	raw := "1 .M N... 100644 100644 100644 aaa bbb dir/with space.txt\x002 R. N... 100644 100644 100644 aaa bbb R100 new.txt\x00old.txt\x00? u.txt\x00"
	ps, un, ne := statusEntries([]byte(raw))
	if len(ps) != 3 || ps[0] != "dir/with space.txt" || ps[1] != "new.txt" || ps[2] != "u.txt" || un || len(ne) != 0 {
		t.Fatalf("%q %v %v", ps, un, ne)
	}
}

// ── 远程往返 ─────────────────────────────────────────

type commitRunner struct {
	judge CommitJudge
	reply []byte // 非空则直接回这个响应（伪造）
}

func (r *commitRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	if r.reply != nil {
		return r.reply, nil, 0, nil
	}
	out, err := ServeApplyWith(ctx, stdin, r.judge)
	if err != nil {
		return nil, []byte(err.Error()), 2, nil
	}
	return out, nil, 0, nil
}

func TestRemoteAutoCommit(t *testing.T) {
	ctx := context.Background()
	dir, head := commitRepo(t)
	req := func(c CommitPlan) ApplyRequest {
		c.RepoDir = dir
		return ApplyRequest{Repo: dir, Kind: RequestAutoCommit, Commit: &c}
	}
	r := &commitRunner{judge: finishedJudge(nil)}

	// nothing_to_commit
	resp, err := RemoteApply(ctx, r, testBin, req(CommitPlan{Worktree: dir, Branch: "main", ExpectHead: head}))
	if err != nil || resp.OK || resp.ErrKind != ErrKindNothingToCommit {
		t.Fatalf("%v %+v", err, resp)
	}
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	// not_finished
	r.judge = busyJudge
	resp, err = RemoteApply(ctx, r, testBin, req(CommitPlan{Worktree: dir, Branch: "main", ExpectHead: head}))
	if err != nil || resp.OK || resp.ErrKind != ErrKindNotFinished || !strings.Contains(resp.Error, "仍在忙") {
		t.Fatalf("%v %+v", err, resp)
	}
	// refs_changed（HEAD 不符）
	r.judge = finishedJudge(nil)
	resp, err = RemoteApply(ctx, r, testBin, req(CommitPlan{Worktree: dir, Branch: "main", ExpectHead: strings.Repeat("b", 40)}))
	if err != nil || resp.OK || resp.ErrKind != ErrKindRefs {
		t.Fatalf("%v %+v", err, resp)
	}
	// out_of_scope（分支不是合法范围：用不存在含 = 的分支名）
	resp, err = RemoteApply(ctx, r, testBin, req(CommitPlan{Worktree: dir, Branch: "x=y", ExpectHead: head}))
	if err != nil || resp.OK || resp.ErrKind != ErrKindOutOfScope {
		t.Fatalf("%v %+v", err, resp)
	}
	// commit_failed（钩子拒绝）
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	write(t, hook, "#!/bin/sh\necho NOPE >&2\nexit 1\n")
	os.Chmod(hook, 0o755)
	resp, err = RemoteApply(ctx, r, testBin, req(CommitPlan{Worktree: dir, Branch: "main", ExpectHead: head}))
	if err != nil || resp.OK || resp.ErrKind != ErrKindCommitFailed || !strings.Contains(resp.Error, "NOPE") {
		t.Fatalf("%v %+v", err, resp)
	}
	os.Remove(hook)
	// 成功（用 Force，judge 不会被调用）
	r.judge = nil
	resp, err = RemoteApply(ctx, r, testBin, req(CommitPlan{Worktree: dir, Branch: "main", ExpectHead: head, Force: true}))
	if err != nil || !resp.OK || resp.NewOID != rev(t, dir, "HEAD") || rev(t, dir, "HEAD^") != head {
		t.Fatalf("%v %+v", err, resp)
	}
	// 没有 judge 也没 Force → not_finished
	write(t, filepath.Join(dir, "b.txt"), "b\n")
	resp, err = RemoteApply(ctx, r, testBin, req(CommitPlan{Worktree: dir, Branch: "main", ExpectHead: resp.NewOID}))
	if err != nil || resp.ErrKind != ErrKindNotFinished {
		t.Fatalf("%v %+v", err, resp)
	}
}

func TestRemoteAutoCommitRequestValidation(t *testing.T) {
	ctx := context.Background()
	dir, head := commitRepo(t)
	good := CommitPlan{Worktree: dir, Branch: "main", ExpectHead: head}
	bad := map[string]ApplyRequest{
		"Plan.Env":       {Repo: dir, Kind: RequestAutoCommit, Commit: &good, Plan: ApplyPlan{Env: []string{"GIT_DIR=/x"}}},
		"缺 Commit":       {Repo: dir, Kind: RequestAutoCommit},
		"Kind 空带 Commit": {Repo: dir, Commit: &good, Plan: ApplyPlan{Branch: "main", Kind: KindFastForward}},
		"未知 Kind":        {Repo: dir, Kind: "weird", Commit: &good},
	}
	for name, rq := range bad {
		rq.Version = RemoteVersion
		b, _ := json.Marshal(rq)
		if _, err := ServeApplyWith(ctx, b, nil); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
}

func TestClassifyRefsBySentinel(t *testing.T) {
	if classifyApplyErr(fmt.Errorf("随便改的措辞：%w", ErrRefsChanged)) != ErrKindRefs {
		t.Fatal("哨兵错误应分类为 refs_changed")
	}
	if classifyApplyErr(errors.New("引用已变化 只是文案")) != ErrKindOther {
		t.Fatal("不应再按文案匹配")
	}
}

func TestRemoteApplyResponseValidation(t *testing.T) {
	ctx := context.Background()
	oid := strings.Repeat("c", 40)
	commitReq := ApplyRequest{Repo: "/r", Kind: RequestAutoCommit, Commit: &CommitPlan{Worktree: "/r", Branch: "main", ExpectHead: oid}}
	ffReq := ApplyRequest{Repo: "/r", Plan: ApplyPlan{Branch: "main", Kind: KindFastForward, OldOID: oid, NewOID: oid}}
	mergeReq := ApplyRequest{Repo: "/r", Plan: ApplyPlan{Branch: "main", Kind: KindAutoMerge, Parents: [2]string{oid, oid}}}
	cases := []struct {
		name  string
		req   ApplyRequest
		reply string
		ok    bool
	}{
		{"OK 且 ErrKind 非空", ffReq, `{"Version":1,"OK":true,"ErrKind":"other"}`, false},
		{"OK 且 Error 非空", ffReq, `{"Version":1,"OK":true,"Error":"x"}`, false},
		{"快进成功正常", ffReq, `{"Version":1,"OK":true}`, true},
		{"快进成功带 NewOID", ffReq, `{"Version":1,"OK":true,"NewOID":"` + oid + `"}`, false},
		{"合并成功无 NewOID", mergeReq, `{"Version":1,"OK":true}`, false},
		{"合并成功 NewOID 非法", mergeReq, `{"Version":1,"OK":true,"NewOID":"zz"}`, false},
		{"合并成功正常", mergeReq, `{"Version":1,"OK":true,"NewOID":"` + oid + `"}`, true},
		{"提交成功无 NewOID", commitReq, `{"Version":1,"OK":true}`, false},
		{"提交成功正常", commitReq, `{"Version":1,"OK":true,"NewOID":"` + oid + `"}`, true},
		{"失败无 ErrKind", ffReq, `{"Version":1,"OK":false,"Error":"x"}`, false},
		{"失败未知 ErrKind", ffReq, `{"Version":1,"OK":false,"ErrKind":"???","Error":"x"}`, false},
		{"失败无 Error", ffReq, `{"Version":1,"OK":false,"ErrKind":"other"}`, false},
		{"失败带 NewOID", ffReq, `{"Version":1,"OK":false,"ErrKind":"other","Error":"x","NewOID":"` + oid + `"}`, false},
		{"失败正常", commitReq, `{"Version":1,"OK":false,"ErrKind":"commit_failed","Error":"x"}`, true},
	}
	for _, c := range cases {
		_, err := RemoteApply(ctx, &commitRunner{reply: []byte(c.reply)}, testBin, c.req)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v 期望 ok=%v", c.name, err, c.ok)
		}
	}
}

func TestAutoCommitHeadMovedAfterCommit(t *testing.T) {
	dir, head := commitRepo(t)
	hook := filepath.Join(dir, ".git", "hooks", "post-commit")
	write(t, hook, "#!/bin/sh\ngit checkout -q -b other\n")
	os.Chmod(hook, 0o755)
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	_, err := AutoCommit(context.Background(), basePlan(dir, head), finishedJudge(nil))
	if !errors.Is(err, ErrCommittedUncertain) || !strings.Contains(err.Error(), "已提交、现场需确认") {
		t.Fatalf("提交后 HEAD 被切走应报现场需确认: %v", err)
	}
	if classifyApplyErr(err) != ErrKindCommittedUncertain {
		t.Fatal("远程分类")
	}
}

func TestCommitFailedOutputTruncatedNote(t *testing.T) {
	dir, head := commitRepo(t)
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	// 输出远超上限
	write(t, hook, "#!/bin/sh\nhead -c 100000 /dev/zero | tr '\\0' 'x' >&2\nexit 1\n")
	os.Chmod(hook, 0o755)
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	_, err := AutoCommit(context.Background(), basePlan(dir, head), finishedJudge(nil))
	var cf *ErrCommitFailed
	if !errors.As(err, &cf) || !strings.HasSuffix(cf.Output, "字节）") || !strings.Contains(cf.Output, "…（已截断，原文共 ") {
		t.Fatalf("应带截断说明: %v", err)
	}
}
