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
		RecycleDir: PFRoundMacRecycleDir(f.meta),
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
	// 目标目录本身要存在（合并进的是已配置的目录）；这里只关心 src 的锁判断
	if err := os.MkdirAll(filepath.Join(f.work, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	done := make(chan struct {
		res PFMergeRunResult
		err error
	}, 1)
	go func() {
		r, err := PFMergeRun(context.Background(), cfg, "src-copy", rel, plan.Fingerprint)
		done <- struct {
			res PFMergeRunResult
			err error
		}{r, err}
	}()
	select {
	case <-done:
		t.Fatal("单元锁还被占着，run 不该已经完成（会同时动同一个单元）")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("松锁后 run 应正常完成: %v", out.err)
		}
		if out.res.Rejected {
			t.Fatalf("松锁后 run 不该再被拒: %s", out.res.Reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("松锁后 run 必须往下走")
	}
}

// 注入「目标凭空出现」的竞态：那一条失败、其余照常，绝不覆盖。
func TestPFMergeRaceNoOverwrite(t *testing.T) {
	f := pfMergeSetup(t)
	// dst 目录先建好（凭空出现的是文件，不是目录）
	if err := os.MkdirAll(filepath.Join(f.work, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
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

// ── 评审 20-1：拿到锁后重做计划；same 回收前再核两边；回收后核对不符要挪回 ──────────

// pfMergeSyncedTarget 搭一个「目标是同步中的单元」：同步目录里有真实目标，工作目录下是指向它的链接，已登记；
// 同步目录里放一个 a.txt，返回带调度器的配置。
func pfMergeSyncedTarget(t *testing.T, f *pfMergeFix, aContent string) (cfg PFMergeConfig, sc *PFSched, code, rel string) {
	t.Helper()
	code = "260403J0812"
	rel = "2026/" + code + "-目标"
	f.put(f.sync, rel+"/a.txt", aContent)
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
	tr, err := NewPFTransfer(PFTransferConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sc, err = NewPFSched(PFSchedConfig{MetaDir: f.meta, WorkDir: f.work, Remote: &PFRemoteClient{}, Transfer: tr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sc.Close)
	cfg = f.cfg()
	cfg.Sched = sc
	return cfg, sc, code, rel
}

// pfMergeRecycledPaths 返回回收区 index 里各天记过的原路径（按「合并时内容相同」过滤）。
func pfMergeRecycledPaths(t *testing.T, f *pfMergeFix) []string {
	t.Helper()
	rc, err := NewPFRecycle(PFRoundMacRecycleDir(f.meta), PFRecycleOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, day := range mustDays(t, rc) {
		for _, e := range mustReadIndex(t, rc, day) {
			if e.Reason == PFMergeReasonSameRecycle {
				out = append(out, e.Path)
			}
		}
	}
	return out
}

// 等锁期间来源被改成别的内容（大小不变、mtime 还原）：拿到锁后必须基于新状态重做计划，
// 来源这份新内容绝不能被当成重复件回收（要么拒绝、要么按新计划当不同内容保留）。
func TestPFMergeLockedReplanAfterWait(t *testing.T) {
	f := pfMergeSetup(t)
	cfg, sc, code, rel := pfMergeSyncedTarget(t, f, "一样的内容")
	src := f.put(f.work, "src-copy/a.txt", "一样的内容") // 同内容 → 计划是 same
	plan, err := PFMergePlan(cfg, "src-copy", rel)
	if err != nil {
		t.Fatal(err)
	}
	if it, ok := pfMergeHas(plan.Items, "a.txt"); !ok || it.Action != PFMergeSame {
		t.Fatalf("起始计划 a.txt 应是 same: %+v", plan.Items)
	}
	unlock, ok := sc.LockUnit(context.Background(), code, 0)
	if !ok {
		t.Fatal("拿单元锁")
	}
	type out struct {
		res PFMergeRunResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := PFMergeRun(context.Background(), cfg, "src-copy", rel, plan.Fingerprint)
		done <- out{r, err}
	}()
	time.Sleep(200 * time.Millisecond)
	// 等锁期间：来源改成同样长度的别的内容，mtime 还原成原来的
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("新的内容！"), 0o644); err != nil { // 同字节数
		t.Fatal(err)
	}
	if err := os.Chtimes(src, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	unlock()
	var o out
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("松锁后 run 必须往下走")
	}
	if o.err != nil {
		t.Fatal(o.err)
	}
	if hasStr(pfMergeRecycledPaths(t, f), "a.txt") {
		t.Fatalf("改过的来源不能被当重复件回收：%+v", o.res)
	}
	// 新内容必须还在：要么留在来源（被拒绝/没动），要么已按不同内容改名挪进目标
	found := false
	for _, root := range []string{filepath.Join(f.work, "src-copy"), filepath.Join(f.sync, filepath.FromSlash(rel))} {
		for _, rp := range pfWalkFiles(t, root) {
			if readFile(t, filepath.Join(root, filepath.FromSlash(rp))) == "新的内容！" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("来源的新内容丢了：%+v", o.res)
	}
}

// same 回收前再核对：计划之后（指纹通过之后）来源或目标变了，这一条不做、留在原处、列为要你定。
func TestPFMergeSameVerifyBeforeRecycle(t *testing.T) {
	type tc struct {
		name string
		hook func(f *pfMergeFix)
	}
	cases := []tc{
		{"来源被改成别的内容", func(f *pfMergeFix) {
			p := filepath.Join(f.work, "src", "a.txt")
			_ = os.WriteFile(p, []byte("别的内容AB"), 0o644) // 大小也变
		}},
		{"来源被改成同样长度的别的内容且 mtime 还原", func(f *pfMergeFix) {
			p := filepath.Join(f.work, "src", "a.txt")
			st, _ := os.Stat(p)
			_ = os.WriteFile(p, []byte("别样的内容"), 0o644) // 同样 15 字节
			_ = os.Chtimes(p, st.ModTime(), st.ModTime())
		}},
		{"目标被改成别的内容", func(f *pfMergeFix) {
			_ = os.WriteFile(filepath.Join(f.work, "dst", "a.txt"), []byte("别的"), 0o644)
		}},
		{"目标被删", func(f *pfMergeFix) { _ = os.Remove(filepath.Join(f.work, "dst", "a.txt")) }},
		{"来源被换成链接", func(f *pfMergeFix) {
			p := filepath.Join(f.work, "src", "a.txt")
			_ = os.Remove(p)
			_ = os.Symlink(filepath.Join(f.work, "dst", "a.txt"), p)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := pfMergeSetup(t)
			f.put(f.work, "src/a.txt", "一样的内容")
			f.put(f.work, "src/b.txt", "另一个")
			f.put(f.work, "dst/a.txt", "一样的内容")
			cfg := f.cfg()
			plan, err := PFMergePlan(cfg, "src", "dst")
			if err != nil {
				t.Fatal(err)
			}
			if it, ok := pfMergeHas(plan.Items, "a.txt"); !ok || it.Action != PFMergeSame {
				t.Fatalf("起始计划 a.txt 应是 same: %+v", plan.Items)
			}
			fired := false
			cfg.stepHook = func(stage, arg string) {
				if stage == "same:before-verify" && !fired {
					fired = true
					c.hook(f)
				}
			}
			res, err := PFMergeRun(context.Background(), cfg, "src", "dst", plan.Fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			if !fired {
				t.Fatalf("钩子没触发: %+v", res)
			}
			if hasStr(pfMergeRecycledPaths(t, f), "a.txt") || len(res.Recycled) != 0 {
				t.Fatalf("核对不符不能回收: %+v", res.Recycled)
			}
			it, ok := pfMergeHas(res.Skipped, "a.txt")
			if !ok || it.Action != PFMergeAsk || !strings.Contains(it.Reason, "回收前再核对") {
				t.Errorf("a.txt 应列为要你定（ask），原因带「回收前再核对」: skipped=%+v failed=%+v", res.Skipped, res.Failed)
			}
			if !hasStr(res.RemainingSrc, "a.txt") {
				t.Errorf("来源 a.txt 应列在没挪走的里面: %+v", res.RemainingSrc)
			}
			if _, err := os.Lstat(filepath.Join(f.work, "src", "a.txt")); err != nil {
				t.Errorf("来源必须留在原处: %v", err)
			}
			// 其它条目照常（b.txt move）
			if got := readFile(t, filepath.Join(f.work, "dst", "b.txt")); got != "另一个" {
				t.Errorf("其它条目应照常执行: %q", got)
			}
		})
	}
}

// 回收后核对：放进回收区的内容与核对过的不一样（核对和回收之间来源被改）→ 不覆盖地挪回原处，不留在回收区。
func TestPFMergeSamePostCheckPutsBack(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "src/a.txt", "一样的内容")
	f.put(f.work, "dst/a.txt", "一样的内容")
	cfg := f.cfg()
	plan, err := PFMergePlan(cfg, "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	cfg.stepHook = func(stage, arg string) {
		if stage == "same:before-place" { // 核对通过之后、放进回收区之前：来源被改（同长度、mtime 还原）
			st, _ := os.Stat(arg)
			_ = os.WriteFile(arg, []byte("新的内容！"), 0o644)
			_ = os.Chtimes(arg, st.ModTime(), st.ModTime())
		}
	}
	res, err := PFMergeRun(context.Background(), cfg, "src", "dst", plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recycled) != 0 || hasStr(pfMergeRecycledPaths(t, f), "a.txt") {
		t.Fatalf("不该记成回收了: %+v", res)
	}
	if got := readFile(t, filepath.Join(f.work, "src", "a.txt")); got != "新的内容！" {
		t.Errorf("被改的来源应原样挪回原处: %q", got)
	}
	if it, ok := pfMergeHas(res.Skipped, "a.txt"); !ok || !strings.Contains(it.Reason, "回收后核对") {
		t.Errorf("应列为要你定并说明是回收后核对: %+v / %+v", res.Skipped, res.Failed)
	}
	if got := readFile(t, filepath.Join(f.work, "dst", "a.txt")); got != "一样的内容" {
		t.Errorf("目标不能被动: %q", got)
	}
}

// 回收后核对不符、原位又被占了：转存到回收区根下的 _不自动清理（到期清理碰不到），记账、报告；
// 连转存都做不到：明确报告东西仍在回收区。
func TestPFMergeSamePostCheckKeepsWhenCannotPutBack(t *testing.T) {
	setup := func(t *testing.T, blockKeep bool) (*pfMergeFix, PFMergeRunResult) {
		f := pfMergeSetup(t)
		f.put(f.work, "src/a.txt", "一样的内容")
		f.put(f.work, "dst/a.txt", "一样的内容")
		cfg := f.cfg()
		plan, err := PFMergePlan(cfg, "src", "dst")
		if err != nil {
			t.Fatal(err)
		}
		cfg.stepHook = func(stage, arg string) {
			switch stage {
			case "same:before-place":
				st, _ := os.Stat(arg)
				_ = os.WriteFile(arg, []byte("新的内容！"), 0o644)
				_ = os.Chtimes(arg, st.ModTime(), st.ModTime())
			case "same:after-place": // 原位被别人新存了一个同名文件
				_ = os.WriteFile(arg, []byte("别人刚存的"), 0o644)
				if blockKeep {
					_ = os.WriteFile(filepath.Join(PFRoundMacRecycleDir(f.meta), pfRecycleKeepDir), []byte("占位"), 0o644)
				}
			}
		}
		res, err := PFMergeRun(context.Background(), cfg, "src", "dst", plan.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		return f, res
	}
	t.Run("转存到 _不自动清理", func(t *testing.T) {
		f, res := setup(t, false)
		if got := readFile(t, filepath.Join(f.work, "src", "a.txt")); got != "别人刚存的" {
			t.Errorf("原位别人新存的文件不能被覆盖: %q", got)
		}
		kept := filepath.Join(PFRoundMacRecycleDir(f.meta), pfRecycleKeepDir, "src", "a.txt")
		if got := readFile(t, kept); got != "新的内容！" {
			t.Fatalf("被改的内容应转存到 %s: %q（%+v）", kept, got, res)
		}
		it, ok := pfMergeHas(res.Failed, "a.txt")
		if !ok || !strings.Contains(it.Reason, pfRecycleKeepDir) {
			t.Errorf("失败原因应说明转存到 %s: %+v", pfRecycleKeepDir, res.Failed)
		}
		if len(res.Residue) == 0 {
			t.Error("应写进 Residue")
		}
		if hasStr(pfMergeRecycledPaths(t, f), "a.txt") {
			t.Error("不该有「合并时内容相同」的账")
		}
		// 到期清理（把时间拨到很久以后）不碰 _不自动清理
		rc, _ := NewPFRecycle(PFRoundMacRecycleDir(f.meta), PFRecycleOptions{Now: func() time.Time { return f.now.AddDate(0, 0, 400) }})
		if _, err := rc.Clean(7); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, kept); got != "新的内容！" {
			t.Errorf("到期清理不该删 %s: %q", pfRecycleKeepDir, got)
		}
	})
	t.Run("转存也做不到", func(t *testing.T) {
		f, res := setup(t, true)
		it, ok := pfMergeHas(res.Failed, "a.txt")
		if !ok || !strings.Contains(it.Reason, "到期会被自动清理") {
			t.Fatalf("应明确报告东西仍在回收区、会被清理: %+v", res.Failed)
		}
		if len(res.Residue) == 0 || !strings.Contains(strings.Join(res.Residue, "\n"), "尽快取走") {
			t.Errorf("Residue 应提醒尽快取走: %+v", res.Residue)
		}
		// 东西确实还在回收区里（没丢）
		found := false
		rc, _ := NewPFRecycle(PFRoundMacRecycleDir(f.meta), PFRecycleOptions{})
		for _, day := range mustDays(t, rc) {
			for _, rp := range pfWalkFiles(t, filepath.Join(rc.Dir(), day)) {
				if readFile(t, filepath.Join(rc.Dir(), day, filepath.FromSlash(rp))) == "新的内容！" {
					found = true
				}
			}
		}
		if !found {
			t.Error("被改的内容应仍在回收区里，不能丢")
		}
	})
}

// ── 评审 20-2：路径中间目录是符号链接时不穿过去 ──────────────────────

// 目标树里的中间目录是链接（指向另一个项目）：对完整路径 stat 会穿过去判成 move，文件就进了别的项目。
// 现在这一条 ask，别的项目一个字不动。
func TestPFMergeTargetIntermediateSymlinkAsk(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "src/资料/报告.pdf", "来源报告")
	f.put(f.work, "src/普通.txt", "普通")
	f.put(f.work, "other/占位.txt", "别的项目")
	if err := os.MkdirAll(filepath.Join(f.work, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.work, "other"), filepath.Join(f.work, "dst", "资料")); err != nil {
		t.Fatal(err)
	}
	plan, err := PFMergePlan(f.cfg(), "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	it, ok := pfMergeHas(plan.Items, "资料/报告.pdf")
	if !ok || it.Action != PFMergeAsk || !strings.Contains(it.Reason, "符号链接") {
		t.Fatalf("目标中间目录是链接，这一条应 ask 并说明链接: %+v", plan.Items)
	}
	if it, ok := pfMergeHas(plan.Items, "普通.txt"); !ok || it.Action != PFMergeMove {
		t.Errorf("不受影响的文件仍应 move: %+v", plan.Items)
	}
	res, err := PFMergeRun(context.Background(), f.cfg(), "src", "dst", plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.work, "other", "报告.pdf")); err == nil {
		t.Fatal("文件不能穿过链接写进别的项目")
	}
	if got := readFile(t, filepath.Join(f.work, "src", "资料", "报告.pdf")); got != "来源报告" {
		t.Errorf("来源必须留在原处: %q", got)
	}
	if got := readFile(t, filepath.Join(f.work, "dst", "普通.txt")); got != "普通" {
		t.Errorf("其它条目照常: %q", got)
	}
	if _, ok := pfMergeHas(res.Skipped, "资料/报告.pdf"); !ok {
		t.Errorf("应列进 Skipped: %+v", res)
	}
}

// 执行时现查：计划时目录是真实的，计划之后（挪这一条之前）目标里的目录被换成链接 → 这一条 ask，不穿过去。
func TestPFMergeRunRecheckChainAtMoveTime(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "src/资料/报告.pdf", "来源报告")
	f.put(f.work, "dst/资料/已有.txt", "目标已有")
	f.put(f.work, "other/占位.txt", "别的项目")
	cfg := f.cfg()
	plan, err := PFMergePlan(cfg, "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	if it, ok := pfMergeHas(plan.Items, "资料/报告.pdf"); !ok || it.Action != PFMergeMove {
		t.Fatalf("起始计划应是 move: %+v", plan.Items)
	}
	cfg.stepHook = func(stage, arg string) {
		if stage == "move:start" {
			dir := filepath.Join(f.work, "dst", "资料")
			_ = os.RemoveAll(dir)
			_ = os.Symlink(filepath.Join(f.work, "other"), dir)
		}
	}
	res, err := PFMergeRun(context.Background(), cfg, "src", "dst", plan.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.work, "other", "报告.pdf")); err == nil {
		t.Fatal("执行时目录变成链接，文件不能穿过去")
	}
	if _, ok := pfMergeHas(res.Skipped, "资料/报告.pdf"); !ok || len(res.Applied) != 0 {
		t.Errorf("这一条应 ask 不动: %+v", res)
	}
	if got := readFile(t, filepath.Join(f.work, "src", "资料", "报告.pdf")); got != "来源报告" {
		t.Errorf("来源必须留在原处: %q", got)
	}
}

// 来源、目标路径在工作目录下的上级目录是链接：整个合并不做。
func TestPFMergeRootAncestorSymlinkRejected(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "real-year/260403J0001-来源/a.txt", "A")
	f.put(f.work, "real-year/260403J0001-目标/b.txt", "B")
	if err := os.Symlink(filepath.Join(f.work, "real-year"), filepath.Join(f.work, "2026")); err != nil {
		t.Fatal(err)
	}
	if _, err := PFMergePlan(f.cfg(), "2026/260403J0001-来源", "real-year/260403J0001-目标"); err == nil || !strings.Contains(err.Error(), "上级路径不安全") {
		t.Errorf("来源上级是链接应拒绝: %v", err)
	}
	if _, err := PFMergePlan(f.cfg(), "real-year/260403J0001-来源", "2026/260403J0001-目标"); err == nil || !strings.Contains(err.Error(), "上级路径不安全") {
		t.Errorf("目标上级是链接应拒绝: %v", err)
	}
	// 来源根本身是链接：不是目录，拒绝
	if err := os.Symlink(filepath.Join(f.work, "real-year", "260403J0001-来源"), filepath.Join(f.work, "link-src")); err != nil {
		t.Fatal(err)
	}
	if _, err := PFMergePlan(f.cfg(), "link-src", "real-year/260403J0001-目标"); err == nil {
		t.Error("来源根是链接应拒绝")
	}
}

// 目标根是链接：没登记的链接（指向别的项目）一律拒绝；登记了但链接指向的不是登记的同步位置也拒绝；
// 指向登记位置的才接受。
func TestPFMergeTargetRootSymlinkOnlyRegisteredSyncPath(t *testing.T) {
	f := pfMergeSetup(t)
	f.put(f.work, "src/a.txt", "A")
	f.put(f.work, "other-project/x.txt", "别的项目")
	// 没登记的链接
	if err := os.Symlink(filepath.Join(f.work, "other-project"), filepath.Join(f.work, "dst-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := PFMergePlan(f.cfg(), "src", "dst-link"); err == nil || !strings.Contains(err.Error(), "不是已登记的同步单元") {
		t.Fatalf("没登记的目标链接应拒绝: %v", err)
	}

	// 登记了，但链接指向别处（不是登记的 SyncPath）
	code := "260403J0812"
	rel := "2026/" + code + "-目标"
	f.put(f.sync, rel+"/a.txt", "同步位置")
	link := filepath.Join(f.work, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.work, "other-project"), link); err != nil {
		t.Fatal(err)
	}
	rec := PFUnitRecord{Code: code, Rel: rel, State: PFUnitSyncing,
		LinkPath: link, SyncPath: filepath.Join(f.sync, filepath.FromSlash(rel)), StartedAt: f.now}
	pfWriteUnits(t, f.meta, []PFUnitRecord{rec})
	if _, err := PFMergePlan(f.cfg(), "src", rel); err == nil || !strings.Contains(err.Error(), "与登记的同步位置") {
		t.Fatalf("链接指向别处应拒绝: %v", err)
	}

	// 链接改成指向登记位置：接受
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.sync, filepath.FromSlash(rel)), link); err != nil {
		t.Fatal(err)
	}
	plan, err := PFMergePlan(f.cfg(), "src", rel)
	if err != nil {
		t.Fatalf("指向登记位置的链接应接受: %v", err)
	}
	if !plan.TargetSync || plan.TargetUnit != code {
		t.Errorf("应认出同步单元: %+v", plan)
	}
	// 登记状态是 stopped 时不该再有链接：当链接拒绝
	rec.State = PFUnitStopped
	pfWriteUnits(t, f.meta, []PFUnitRecord{rec})
	if _, err := PFMergePlan(f.cfg(), "src", rel); err == nil {
		t.Error("单元已 stopped 还是链接，应拒绝")
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
