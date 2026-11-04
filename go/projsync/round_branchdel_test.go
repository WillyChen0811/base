package projsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 分支删除同步的整轮测试：两个临时仓库（Mac 本地 + 进程内假 Ubuntu），基线目录在 t.TempDir()。

func (p rpair) envBase(r RemoteRunner, dir string) RoundEnv {
	e := p.env(r)
	e.BaseDir = dir
	return e
}

func roundOK(t *testing.T, env RoundEnv) RoundResult {
	t.Helper()
	res := RunRound(context.Background(), env)
	if res.Error != "" || res.Stopped {
		t.Fatalf("本轮出错: %+v", res)
	}
	return res
}

// loadBaseOf 像新进程一样从磁盘读基线。
func loadBaseOf(t *testing.T, p rpair, dir string) *branchBase {
	t.Helper()
	key, err := repoLockKey(context.Background(), p.mac)
	if err != nil {
		t.Fatal(err)
	}
	b, notes, err := loadBranchBase(dir, "proj", key, p.ubu, time.Now())
	if err != nil || len(notes) != 0 {
		t.Fatalf("%v %v", err, notes)
	}
	return b
}

func hasRef(t *testing.T, dir, branch string) bool {
	t.Helper()
	return !refGone(t, dir, branch)
}

// childCommit 在 parent 之上造一个新提交（不碰工作区），返回新 OID。
func childCommit(t *testing.T, dir, parent string) string {
	t.Helper()
	return ag(t, dir, "commit-tree", parent+"^{tree}", "-p", parent, "-m", "child of "+parent[:7])
}

func readDeleteLog(t *testing.T, p rpair, dir string) string {
	t.Helper()
	b, _ := os.ReadFile(loadBaseOf(t, p, dir).logPath)
	return string(b)
}

// 首次运行（没有基线文件）：两边相同的分支记成初始基线；升级前就只有一边有的分支按规则 1 新建，不删。
func TestRoundBranchBaseFirstRunInitializes(t *testing.T) {
	p := newPair(t)
	dir := filepath.Join(t.TempDir(), "base")
	c := rev(t, p.mac, "main")
	ag(t, p.mac, "branch", "old") // 升级前就只在 Mac 上
	res := roundOK(t, p.envBase(&ubuRunner{}, dir))
	if b := findBranch(t, res, "old"); b.Action != KindCreateOnOther || b.Result != ResultDone {
		t.Fatalf("%+v", b)
	}
	if rev(t, p.ubu, "old") != c {
		t.Fatal("只有一边有的分支应在另一边新建")
	}
	b := loadBaseOf(t, p, dir)
	if b.get("main") != c || b.get("old") != c {
		t.Fatalf("初始基线不对: %+v", b.file.Branches)
	}
}

// 规则 2（两个方向）+ 基线跨轮持久化：第一轮建立基线，用户在两边各删一个分支，第二轮把另一边也删掉。
func TestRoundBranchDeleteSyncsBothDirections(t *testing.T) {
	p := newPair(t)
	dir := t.TempDir()
	c := rev(t, p.mac, "main")
	ag(t, p.mac, "branch", "f")
	ag(t, p.mac, "branch", "g")
	roundOK(t, p.envBase(&ubuRunner{}, dir))
	if !hasRef(t, p.ubu, "f") || !hasRef(t, p.ubu, "g") || loadBaseOf(t, p, dir).get("f") != c {
		t.Fatal("第一轮应把 f、g 建到 Ubuntu 并记基线")
	}
	ag(t, p.ubu, "branch", "-D", "f") // Ubuntu 删了 f
	ag(t, p.mac, "branch", "-D", "g") // Mac 删了 g
	res := roundOK(t, p.envBase(&ubuRunner{}, dir))
	bf, bg := findBranch(t, res, "f"), findBranch(t, res, "g")
	if bf.Action != KindDeleteOnOther || bf.Result != ResultDone || bf.Deleted != c || bf.Sides[0] != SideMac {
		t.Fatalf("f: %+v", bf)
	}
	if bg.Action != KindDeleteOnOther || bg.Result != ResultDone || bg.Deleted != c || bg.Sides[0] != SideUbuntu {
		t.Fatalf("g: %+v", bg)
	}
	if hasRef(t, p.mac, "f") || hasRef(t, p.ubu, "g") {
		t.Fatal("另一边也应删掉")
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "git branch f "+c) {
		t.Fatalf("结果里应写明原提交与找回方法: %v", res.Notes)
	}
	log := readDeleteLog(t, p, dir)
	if strings.Count(log, `"state":"deleting"`) != 2 || strings.Count(log, `"state":"deleted"`) != 2 || !strings.Contains(log, c) {
		t.Fatalf("删除记录不对:\n%s", log)
	}
	b := loadBaseOf(t, p, dir)
	if b.get("f") != "" || b.get("g") != "" || b.get("main") != c {
		t.Fatalf("删除后基线应清掉: %+v", b.file.Branches)
	}
	// 第三轮：不会再建回来。
	res = roundOK(t, p.envBase(&ubuRunner{}, dir))
	if len(res.Branches) != 0 || hasRef(t, p.mac, "f") || hasRef(t, p.ubu, "g") {
		t.Fatalf("删掉的分支不应再出现: %+v", res.Branches)
	}
}

// 规则 3：Ubuntu 删了 f，但 Mac 上 f 在基线之后有新提交 → 在 Ubuntu 新建，指向新提交。
func TestRoundBranchDeletedButNewCommitsRecreated(t *testing.T) {
	p := newPair(t)
	dir := t.TempDir()
	c := rev(t, p.mac, "main")
	ag(t, p.mac, "branch", "f")
	roundOK(t, p.envBase(&ubuRunner{}, dir))
	ag(t, p.ubu, "branch", "-D", "f")
	y := childCommit(t, p.mac, c)
	ag(t, p.mac, "update-ref", "refs/heads/f", y)
	res := roundOK(t, p.envBase(&ubuRunner{}, dir))
	b := findBranch(t, res, "f")
	if b.Action != KindCreateOnOther || b.Result != ResultDone || !strings.Contains(b.Detail, "旧的删了，新的照样建") {
		t.Fatalf("%+v", b)
	}
	if rev(t, p.ubu, "f") != y || rev(t, p.mac, "f") != y {
		t.Fatal("新提交不能丢，Ubuntu 应新建到新提交")
	}
	if loadBaseOf(t, p, dir).get("f") != y {
		t.Fatal("新建成功后基线应记成新提交")
	}
}

// 规则 4：要删的那边该分支正被工作区检出 → 不删、不切分支，列需要用户处理的事项。
func TestRoundBranchDeleteBlockedWhenCheckedOut(t *testing.T) {
	p := newPair(t)
	dir := t.TempDir()
	ag(t, p.mac, "branch", "f")
	roundOK(t, p.envBase(&ubuRunner{}, dir))
	wt := filepath.Join(t.TempDir(), "wt-f")
	ag(t, p.mac, "worktree", "add", "-q", wt, "f")
	ag(t, p.ubu, "branch", "-D", "f")
	res := roundOK(t, p.envBase(&ubuRunner{}, dir))
	b := findBranch(t, res, "f")
	if b.Action != KindDeleteBlocked || b.Result != ResultSkipped {
		t.Fatalf("%+v", b)
	}
	a := hasAtt(res, AttDeleteBlocked)
	if a == nil || a.Branch != "f" || a.Side != SideMac || !strings.Contains(a.Message, "正被工作区检出") {
		t.Fatalf("应列需要用户处理的事项: %+v", res.Attention)
	}
	if !hasRef(t, p.mac, "f") || ag(t, wt, "symbolic-ref", "HEAD") != "refs/heads/f" {
		t.Fatal("不删、不切分支")
	}
	if hasRef(t, p.ubu, "f") {
		t.Fatal("也不能在 Ubuntu 建回来")
	}
}

// 规则 5：main、master 永远不自动删除，只列需要用户处理的事项。
func TestRoundProtectedBranchNotDeleted(t *testing.T) {
	p := newPair(t)
	dir := t.TempDir()
	c := rev(t, p.mac, "main")
	ag(t, p.mac, "branch", "master")
	roundOK(t, p.envBase(&ubuRunner{}, dir))
	ag(t, p.ubu, "branch", "-D", "master")
	for i := 0; i < 2; i++ { // 每轮都提示，基线保留
		res := roundOK(t, p.envBase(&ubuRunner{}, dir))
		b := findBranch(t, res, "master")
		if b.Action != KindDeleteBlocked || b.Result != ResultSkipped {
			t.Fatalf("%+v", b)
		}
		if a := hasAtt(res, AttDeleteBlocked); a == nil || !strings.Contains(a.Message, "永远不自动删除") {
			t.Fatalf("%+v", res.Attention)
		}
		if rev(t, p.mac, "master") != c || hasRef(t, p.ubu, "master") {
			t.Fatal("受保护分支两边都不能动")
		}
		if loadBaseOf(t, p, dir).get("master") != c {
			t.Fatal("基线保留")
		}
	}
}

// 规则 6：两边都没有了 → 清掉基线。
func TestRoundBranchBaseClearedWhenBothGone(t *testing.T) {
	p := newPair(t)
	dir := t.TempDir()
	ag(t, p.mac, "branch", "f")
	roundOK(t, p.envBase(&ubuRunner{}, dir))
	ag(t, p.mac, "branch", "-D", "f")
	ag(t, p.ubu, "branch", "-D", "f")
	res := roundOK(t, p.envBase(&ubuRunner{}, dir))
	for _, b := range res.Branches {
		if b.Name == "f" {
			t.Fatalf("两边都没有，不应有动作: %+v", b)
		}
	}
	if b := loadBaseOf(t, p, dir); b.get("f") != "" || b.get("main") == "" {
		t.Fatalf("%+v", b.file.Branches)
	}
}

// 删除时旧值不符（采集之后、删除之前 Mac 上 f 被并发改过）→ 比较交换失败，不删，基线不动。
func TestRoundBranchDeleteStaleOldValueStops(t *testing.T) {
	p := newPair(t)
	dir := t.TempDir()
	c := rev(t, p.mac, "main")
	ag(t, p.mac, "branch", "f")
	roundOK(t, p.envBase(&ubuRunner{}, dir))
	ag(t, p.ubu, "branch", "-D", "f")
	y := childCommit(t, p.mac, c)
	fired := false
	applyTestHook = func(stage string) {
		if stage == "beforeUpdateRef" && !fired {
			fired = true
			ag(t, p.mac, "update-ref", "refs/heads/f", y)
		}
	}
	defer func() { applyTestHook = nil }()
	res := RunRound(context.Background(), p.envBase(&ubuRunner{}, dir))
	b := findBranch(t, res, "f")
	if !fired || b.Action != KindDeleteOnOther || b.Result != ResultFailed || b.Deleted != c {
		t.Fatalf("fired=%v %+v", fired, b)
	}
	if rev(t, p.mac, "f") != y {
		t.Fatal("被并发改过的分支不能删")
	}
	if loadBaseOf(t, p, dir).get("f") != c {
		t.Fatal("删除失败基线不动")
	}
	if log := readDeleteLog(t, p, dir); !strings.Contains(log, `"state":"failed"`) {
		t.Fatalf("删除记录应记失败:\n%s", log)
	}
	// 下一轮：Mac 的 f 已不等于基线 → 按规则 3 在 Ubuntu 新建，不丢提交。
	res = roundOK(t, p.envBase(&ubuRunner{}, dir))
	if b := findBranch(t, res, "f"); b.Action != KindCreateOnOther || rev(t, p.ubu, "f") != y {
		t.Fatalf("%+v", b)
	}
}

// Ubuntu 一侧删除时通信失败 → 结果不确定，停止后续写，基线不动。
func TestRoundBranchDeleteRemoteCommFailureUncertain(t *testing.T) {
	p := newPair(t)
	dir := t.TempDir()
	c := rev(t, p.mac, "main")
	ag(t, p.mac, "branch", "g")
	roundOK(t, p.envBase(&ubuRunner{}, dir))
	ag(t, p.mac, "branch", "-D", "g")
	res := RunRound(context.Background(), p.envBase(&ubuRunner{failApplyAt: 1}, dir))
	b := findBranch(t, res, "g")
	if b.Result != ResultUncertain || !res.Stopped || b.Deleted != c || !strings.Contains(b.Detail, c) {
		t.Fatalf("%+v", b)
	}
	if hasAtt(res, AttNeedRecheck) == nil {
		t.Fatal("应提示需重新确认")
	}
	if loadBaseOf(t, p, dir).get("g") != c {
		t.Fatal("结果不确定，基线不动")
	}
}

// 没配基线目录：等同旧行为，删掉的分支会被建回来。
func TestRoundWithoutBaseDirKeepsCreating(t *testing.T) {
	p := newPair(t)
	ag(t, p.mac, "branch", "f")
	roundOK(t, p.env(&ubuRunner{}))
	ag(t, p.ubu, "branch", "-D", "f")
	res := roundOK(t, p.env(&ubuRunner{}))
	if b := findBranch(t, res, "f"); b.Action != KindCreateOnOther || !hasRef(t, p.ubu, "f") {
		t.Fatalf("%+v", b)
	}
}
