package projsync

// pfmerge_test.go：§14.3 合并（只在 Mac 真做）——用临时目录 fixture 覆盖四种处置各一次、
// same-size+异 MD5→rename、rename 撞名→-2、超长→ask、Office 锁→ask、指纹变了拒绝、
// run 后空目录进回收区且 same 那份在回收区找得到、目标是已同步单元时拿住单元锁、
// 注入目标已存在的竞态→那条失败其余照常不覆盖。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pfMergeFix 是一棵临时目录树：work（工作目录）、sync（同步目录）、meta（回收区在其下）。
type pfMergeFix struct {
	t    *testing.T
	dir  string
	work string
	sync string
	meta string
	now  time.Time
}

func pfMergeSetup(t *testing.T) *pfMergeFix {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	sync := filepath.Join(dir, "sync")
	meta := filepath.Join(dir, "meta")
	for _, d := range []string{work, sync, meta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &pfMergeFix{t: t, dir: dir, work: work, sync: sync, meta: meta, now: time.Now()}
}

// put 在工作目录 work 下按 rel 建文件，mtime 设成很久以前（让它静止、能挪）。
func (f *pfMergeFix) put(base, rel, content string) string {
	f.t.Helper()
	p := filepath.Join(base, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	old := f.now.Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// cfg 拼一份合并配置（静止 10 秒、Now 固定）。
func (f *pfMergeFix) cfg() PFMergeConfig {
	return PFMergeConfig{
		WorkDir: f.work, SyncDir: f.sync, MetaDir: f.meta,
		QuietSeconds: 10, Now: func() time.Time { return f.now },
	}
}

func pfMergeHas(items []PFMergeItem, rel string) (PFMergeItem, bool) {
	for _, it := range items {
		if it.Rel == rel {
			return it, true
		}
	}
	return PFMergeItem{}, false
}

// walkFiles 列出目录树里所有普通文件（相对 root，/ 分隔）。
func pfWalkFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			r, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	return out
}

// 四种处置各一次 + 空目录进回收区。
func TestPFMergePlanAndRunFourActions(t *testing.T) {
	f := pfMergeSetup(t)
	// 来源：new.txt(→move)、same.txt(→same)、diff.txt(→rename)、sub/deep.txt(→move，sub 会空)
	f.put(f.work, "src/new.txt", "全新内容")
	f.put(f.work, "src/same.txt", "一样的内容")
	f.put(f.work, "src/diff.txt", "来源版本AAA")
	f.put(f.work, "src/sub/deep.txt", "深层文件")
	// 目标：same.txt(同内容)、diff.txt(不同内容但同字节数)
	f.put(f.work, "dst/same.txt", "一样的内容")
	f.put(f.work, "dst/diff.txt", "目标版本BBB")

	plan, err := PFMergePlan(f.cfg(), "src", "dst")
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	want := map[string]PFMergeAction{
		"new.txt": PFMergeMove, "same.txt": PFMergeSame, "diff.txt": PFMergeRename, "sub/deep.txt": PFMergeMove,
	}
	for rel, wa := range want {
		it, ok := pfMergeHas(plan.Items, rel)
		if !ok {
			t.Fatalf("计划里缺 %s: %+v", rel, plan.Items)
		}
		if it.Action != wa {
			t.Errorf("%s 处置应为 %s，实为 %s（reason %s）", rel, wa, it.Action, it.Reason)
		}
		if it.Reason == "" {
			t.Errorf("%s 没有 reason", rel)
		}
	}
	// rename 的名字应带「（合并-src）」
	if it, _ := pfMergeHas(plan.Items, "diff.txt"); !strings.Contains(it.DestRel, "（合并-src）") {
		t.Errorf("rename 落名应带合并标记: %s", it.DestRel)
	}
	// 会变空的目录含 sub
	if !hasStr(plan.EmptyDirs, "sub") {
		t.Errorf("空目录应含 sub: %+v", plan.EmptyDirs)
	}

	res, err := PFMergeRun(context.Background(), f.cfg(), "src", "dst", plan.Fingerprint)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Rejected {
		t.Fatalf("不该被拒: %s", res.Reason)
	}
	// 来源里该挪的都没了
	for _, rel := range []string{"new.txt", "same.txt", "diff.txt", "sub/deep.txt"} {
		if _, err := os.Stat(filepath.Join(f.work, "src", filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s 应已从来源挪走: %v", rel, err)
		}
	}
	// 目标里 move/rename 的都在，且 same 的目标原样没被动过
	if got := readFile(t, filepath.Join(f.work, "dst", "new.txt")); got != "全新内容" {
		t.Errorf("new.txt 没挪过去: %q", got)
	}
	if got := readFile(t, filepath.Join(f.work, "dst", "same.txt")); got != "一样的内容" {
		t.Errorf("same 不该改目标: %q", got)
	}
	if len(res.Failed) != 0 {
		t.Errorf("不该有失败: %+v", res.Failed)
	}

	// 回收区里能找到 same 那份，原因「合并时内容相同」
	rc, err := NewPFRecycle(PFRoundMacRecycleDir(f.meta), PFRecycleOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, day := range mustDays(t, rc) {
		for _, e := range mustReadIndex(t, rc, day) {
			if e.Path == "same.txt" && e.Reason == PFMergeReasonSameRecycle {
				found = true
				if _, err := os.Stat(rc.Path(e)); err != nil {
					t.Errorf("回收区里该有 same 那份: %v", err)
				}
			}
		}
	}
	if !found {
		t.Error("回收区 index 里没找到 same 那份（原因应为「合并时内容相同」）")
	}
	// 空目录 sub 进了回收区（改名挪走），来源里 src/sub 不复存在
	if _, err := os.Stat(filepath.Join(f.work, "src", "sub")); !os.IsNotExist(err) {
		t.Errorf("空的 sub 目录应已进回收区: %v", err)
	}
	if len(res.RecycleDirs) == 0 {
		t.Error("应报告回收过的空目录")
	}
}

// same-size + 不同 MD5 → rename；rename 撞名 → -2。
func TestPFMergeRenameCollisionSuffix(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "src/a.docx", "来源AAA")        // 250? no, short. 目标已有同 rel 不同内容
	f.put(f.work, "dst/a.docx", "目标BBB")        // 同长度不同内容 → 本条 rename
	f.put(f.work, "dst/a（合并-src）.docx", "占位") // 把第一个候选名占掉 → 落到 -2

	plan, err := PFMergePlan(f.cfg(), "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	it, ok := pfMergeHas(plan.Items, "a.docx")
	if !ok || it.Action != PFMergeRename {
		t.Fatalf("a.docx 应为 rename: %+v", plan.Items)
	}
	if !strings.HasSuffix(it.DestRel, "（合并-src）-2.docx") {
		t.Errorf("撞名应加 -2，实为 %s", it.DestRel)
	}
}

// 改名后单段超 255 字节 → ask（留在原处）。
func TestPFMergeTooLongAsk(t *testing.T) {
	f := pfMergeSetup(t)
	long := strings.Repeat("x", 250) + ".docx" // 原名就 255 字节，加合并标记必超
	f.put(f.work, "src/"+long, "来源AAA")
	f.put(f.work, "dst/"+long, "目标BBB") // 同长度不同内容 → 走 rename → 超长 → ask

	plan, err := PFMergePlan(f.cfg(), "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	it, ok := pfMergeHas(plan.Items, long)
	if !ok || it.Action != PFMergeAsk {
		t.Fatalf("超长的应 ask: %+v", plan.Items)
	}
	if !plan.HasAsk {
		t.Error("HasAsk 应为真")
	}
	// run 之后仍原地不动
	res, err := PFMergeRun(context.Background(), f.cfg(), "src", "dst", plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.work, "src", long)); err != nil {
		t.Errorf("ask 的超长文件必须留在原处: %v", err)
	}
	if len(res.Skipped) == 0 {
		t.Error("ask 应进 Skipped")
	}
}

// Office 锁：自身是 ~$ 锁、以及有保护它的兄弟锁 → 都 ask。
func TestPFMergeOfficeLockAsk(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "src/报告.docx", "正文")
	f.put(f.work, "src/~$报告.docx", "锁") // 自身是锁
	f.put(f.work, "src/only-lock.docx", "")
	// only-lock.docx 本身被自身锁覆盖，这里再造一个有兄弟锁的：
	f.put(f.work, "src/doc.docx", "内容")
	f.put(f.work, "src/~$doc.docx", "兄弟锁")

	plan, err := PFMergePlan(f.cfg(), "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"~$报告.docx", "报告.docx", "doc.docx"} {
		it, ok := pfMergeHas(plan.Items, rel)
		if !ok {
			t.Fatalf("缺 %s", rel)
		}
		if it.Action != PFMergeAsk {
			t.Errorf("%s 应 ask（锁/有兄弟锁），实为 %s：%s", rel, it.Action, it.Reason)
		}
	}
}

// 计划之后文件有变动 → run 拒绝、退回新计划、不动任何文件。
func TestPFMergeFingerprintChangedReject(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "src/a.txt", "A")
	f.put(f.work, "dst/b.txt", "B")

	plan, err := PFMergePlan(f.cfg(), "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	// 计划之后动一下来源里的文件（mtime 变），指纹就变了
	time.Sleep(2 * time.Millisecond)
	now := time.Now()
	if err := os.Chtimes(filepath.Join(f.work, "src", "a.txt"), now, now); err != nil {
		t.Fatal(err)
	}
	res, err := PFMergeRun(context.Background(), f.cfg(), "src", "dst", plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rejected {
		t.Fatalf("指纹变了必须拒绝")
	}
	if !strings.Contains(res.Reason, "计划之后文件有变动") {
		t.Errorf("原因不对: %s", res.Reason)
	}
	if res.Fingerprint == plan.Fingerprint {
		t.Error("退回的新计划指纹应与旧的不同")
	}
	// 来源文件还在、没动
	if got := readFile(t, filepath.Join(f.work, "src", "a.txt")); got != "A" {
		t.Errorf("拒绝时不该动文件: %q", got)
	}
}

// 来源是已登记的同步单元 → 直接拒绝。
func TestPFMergeSourceIsRegisteredUnit(t *testing.T) {
	f := pfMergeSetup(t)
	// 把 src 登记成一个 syncing 单元（路径要合法以过校验）
	code := "260403J0812"
	rel := "2026/" + code + "-来源"
	f.put(f.work, rel+"/a.txt", "A")
	rec := PFUnitRecord{Code: code, Rel: rel, State: PFUnitSyncing,
		LinkPath: f.work + "/" + rel, SyncPath: f.sync + "/" + rel, StartedAt: f.now}
	pfWriteUnits(t, f.meta, []PFUnitRecord{rec})

	_, err := PFMergePlan(f.cfg(), rel, "dst")
	if err == nil || !strings.Contains(err.Error(), "正在同步") {
		t.Fatalf("来源是同步单元应被拒绝，实得: %v", err)
	}
}

// 目标是已登记的同步单元（工作目录下是指向同步目录的链接）→ 按计划解析到同步目录里算，
// 执行期间拿住单元锁：占着锁时 run 会等，松锁后才继续。
func TestPFMergeTargetSyncedUnitHoldsLock(t *testing.T) {
	f := pfMergeSetup(t)
	code := "260403J0812"
	rel := "2026/" + code + "-目标"
	// 同步目录里放真实的目标目录（带一个同名文件 → same）
	f.put(f.sync, rel+"/a.txt", "一样")
	// 工作目录下建一个指向同步目录的链接（正位）
	link := filepath.Join(f.work, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.sync, filepath.FromSlash(rel)), link); err != nil {
		t.Fatal(err)
	}
	rec := PFUnitRecord{Code: code, Rel: rel, State: PFUnitSyncing,
		LinkPath: link, SyncPath: filepath.Join(f.sync, filepath.FromSlash(rel)), StartedAt: f.now}
	pfWriteUnits(t, f.meta, []PFUnitRecord{rec})
	// 来源（未登记的同号目录）
	f.put(f.work, "src-copy/a.txt", "一样") // 与目标同 rel(a.txt) 同内容 → same

	tr, err := NewPFTransfer(PFTransferConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := NewPFSched(PFSchedConfig{MetaDir: f.meta, WorkDir: f.work, Remote: &PFRemoteClient{}, Transfer: tr})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	cfg := f.cfg()
	cfg.Sched = sc

	plan, err := PFMergePlan(cfg, "src-copy", rel)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TargetUnit != code || !plan.TargetSync {
		t.Fatalf("目标应认出是同步中的单元 %s: %+v", code, plan)
	}

	// 先占住单元锁，再并发跑 run——它应当卡在拿锁上
	unlock, ok := sc.LockUnit(context.Background(), code, 0)
	if !ok {
		t.Fatal("拿单元锁")
	}
	done := make(chan PFMergeRunResult, 1)
	go func() {
		r, _ := PFMergeRun(context.Background(), cfg, "src-copy", rel, plan.Fingerprint)
		done <- r
	}()
	select {
	case <-done:
		t.Fatal("单元锁还被占着，run 不该已经完成（会同时动同一个单元）")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case r := <-done:
		if r.Rejected {
			t.Fatalf("松锁后 run 不该再被拒: %s", r.Reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("松锁后 run 必须往下走")
	}
}

// 注入「目标凭空出现」的竞态：那一条失败、其余照常，绝不覆盖。
func TestPFMergeRaceNoOverwrite(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "src/m1.txt", "挪一")
	f.put(f.work, "src/m2.txt", "挪二")

	cfg := f.cfg()
	// 在挪 m1.txt 之前，凭空把目标造出来（模拟并发），模拟重命名的老对手抢先
	cfg.moveHook = func(it PFMergeItem) {
		if it.Rel == "m1.txt" {
			_ = os.WriteFile(filepath.Join(f.work, "dst", "m1.txt"), []byte("别人抢先放的"), 0o644)
		}
	}
	plan, err := PFMergePlan(cfg, "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	res, err := PFMergeRun(context.Background(), cfg, "src", "dst", plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	// m1 失败、m2 成功
	if len(res.Failed) != 1 || res.Failed[0].Rel != "m1.txt" {
		t.Errorf("m1 应失败、其余不该: %+v", res.Failed)
	}
	if moved := pfWalkFiles(t, filepath.Join(f.work, "dst")); !hasStr(moved, "m2.txt") {
		t.Errorf("m2 应照常挪过去: %+v", moved)
	}
	// 覆盖没发生：目标里 m1 还是别人抢先放的内容
	if got := readFile(t, filepath.Join(f.work, "dst", "m1.txt")); got != "别人抢先放的" {
		t.Errorf("不该覆盖别人的文件: %q", got)
	}
	// 失败那条来源原地还在（没删）
	if _, err := os.Stat(filepath.Join(f.work, "src", "m1.txt")); err != nil {
		t.Errorf("失败的来源必须原地不动: %v", err)
	}
}

// ── 小工具 ──────────────────────────────────────────────

func hasStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return string(b)
}

func pfWriteUnits(t *testing.T, meta string, us []PFUnitRecord) {
	t.Helper()
	b, err := PFMarshalUnits(us)
	if err != nil {
		t.Fatalf("序列化登记表: %v", err)
	}
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PFUnitMoveUnitsPath(meta), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustDays(t *testing.T, rc *PFRecycle) []string {
	t.Helper()
	d, err := rc.Days()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustReadIndex(t *testing.T, rc *PFRecycle, day string) []PFRecycleEntry {
	t.Helper()
	e, err := rc.ReadIndex(day)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
