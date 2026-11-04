package projsync

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func delPlan(dir, branch, old string) ApplyPlan {
	return ApplyPlan{RepoDir: dir, Branch: branch, OldOID: old, Kind: KindDeleteOnOther}
}

func refGone(t *testing.T, dir, branch string) bool {
	t.Helper()
	_, ex, err := readRef(context.Background(), dir, nil, "refs/heads/"+branch)
	if err != nil {
		t.Fatal(err)
	}
	return !ex
}

func TestDeleteBranchDeletesWithOldValue(t *testing.T) {
	isolateGitConfig(t)
	dir := newRepo(t)
	c := rev(t, dir, "HEAD")
	ag(t, dir, "branch", "f")
	if err := DeleteBranch(context.Background(), delPlan(dir, "f", c)); err != nil {
		t.Fatal(err)
	}
	if !refGone(t, dir, "f") {
		t.Fatal("应已删除")
	}
	if rev(t, dir, "main") != c {
		t.Fatal("别的分支不能动")
	}
}

func TestDeleteBranchRefusesStaleOldValue(t *testing.T) {
	isolateGitConfig(t)
	dir := newRepo(t)
	c0 := rev(t, dir, "HEAD")
	ag(t, dir, "branch", "f")
	c1 := commitFile(t, dir, "x.txt", "x\n")
	ag(t, dir, "update-ref", "refs/heads/f", c1) // 判定之后被并发改过
	err := DeleteBranch(context.Background(), delPlan(dir, "f", c0))
	if !errors.Is(err, ErrRefsChanged) {
		t.Fatalf("旧值不符应报 ErrRefsChanged: %v", err)
	}
	if rev(t, dir, "f") != c1 {
		t.Fatal("旧值不符不能删")
	}
	// 引用已不存在也算现场变了。
	ag(t, dir, "branch", "-D", "f")
	if err := DeleteBranch(context.Background(), delPlan(dir, "f", c0)); !errors.Is(err, ErrRefsChanged) {
		t.Fatalf("%v", err)
	}
}

// 检查通过之后、update-ref 之前被改：update-ref -d 的比较交换挡住，不删。
func TestDeleteBranchCASRaceBeforeWrite(t *testing.T) {
	isolateGitConfig(t)
	dir := newRepo(t)
	c0 := rev(t, dir, "HEAD")
	ag(t, dir, "branch", "f")
	c1 := commitFile(t, dir, "x.txt", "x\n")
	applyTestHook = func(stage string) {
		if stage == "beforeUpdateRef" {
			ag(t, dir, "update-ref", "refs/heads/f", c1)
		}
	}
	defer func() { applyTestHook = nil }()
	if err := DeleteBranch(context.Background(), delPlan(dir, "f", c0)); err == nil {
		t.Fatal("比较交换应失败")
	}
	if rev(t, dir, "f") != c1 {
		t.Fatal("被并发改过的分支不能删")
	}
}

func TestDeleteBranchRefusesCheckedOutAndProtected(t *testing.T) {
	isolateGitConfig(t)
	dir := newRepo(t)
	c := rev(t, dir, "HEAD")
	wt := filepath.Join(t.TempDir(), "wt")
	ag(t, dir, "worktree", "add", "-q", "-b", "f", wt)
	if err := DeleteBranch(context.Background(), delPlan(dir, "f", c)); err == nil || !strings.Contains(err.Error(), "正检出") {
		t.Fatalf("被检出的分支不能删: %v", err)
	}
	if refGone(t, dir, "f") {
		t.Fatal("不应删除")
	}
	ag(t, dir, "branch", "master")
	for _, b := range []string{"main", "master"} {
		if err := DeleteBranch(context.Background(), delPlan(dir, b, c)); err == nil || !strings.Contains(err.Error(), "受保护") {
			t.Fatalf("%s 受保护: %v", b, err)
		}
		if refGone(t, dir, b) {
			t.Fatalf("%s 不能删", b)
		}
	}
	if err := DeleteBranch(context.Background(), ApplyPlan{RepoDir: dir, Branch: "f", OldOID: c, NewOID: c, Kind: KindDeleteOnOther}); err == nil {
		t.Fatal("带目标值应拒绝")
	}
}

func TestRemoteApplyRequestDeleteValidation(t *testing.T) {
	ok := ApplyRequest{Version: RemoteVersion, Repo: "/r", Plan: ApplyPlan{Branch: "f", OldOID: tOID1, Kind: KindDeleteOnOther}}
	if err := validateApplyRequest(&ok); err != nil {
		t.Fatal(err)
	}
	bad := ApplyRequest{Version: RemoteVersion, Repo: "/r", Plan: ApplyPlan{Branch: "f", OldOID: tOID1, NewOID: tOID2, Kind: KindDeleteOnOther}}
	if err := validateApplyRequest(&bad); err == nil {
		t.Fatal("删除带 NewOID 应拒绝")
	}
	bad2 := ApplyRequest{Version: RemoteVersion, Repo: "/r", Plan: ApplyPlan{Branch: "f", Kind: KindDeleteOnOther}}
	if err := validateApplyRequest(&bad2); err == nil {
		t.Fatal("删除缺旧值应拒绝")
	}
}
