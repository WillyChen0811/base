package projsync

// T10 测试：历史版本台账第一版（读一对、装一对、锁、一轮编排）。
//
// 台账 sqlite 是假的（同步只当不透明字节搬）；档案索引 JSON 照 versions.py 的 _export 结构写，
// versions[].snapshot 指到「历史版本/文档/<file_id>/V###_<8>_<名>」的真文件上。
// 夹具沿用 T9a 的 pfrdFix（进程内假 Ubuntu）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const pfltHV = "历史版本"

// ── 夹具小工具 ──────────────────────────────────────────

func pfltApplier(t *testing.T, f *pfrdFix) *PFApplier {
	t.Helper()
	now := func() time.Time { return f.ub.now }
	rec, err := NewPFRecycle(PFRoundMacRecycleDir(f.meta), PFRecycleOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewPFApplier(PFApplyConfig{Root: f.root, UnitRel: pfrtRel, Staging: PFRoundMacStagingDir(f.meta),
		Recycle: rec, Round: "pflt", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// pfltRow 造一条版本行（字段照 versions.py 的导出）。
func pfltRow(id, fileID string, number int, snapRel, sha, content string) map[string]any {
	return map[string]any{"id": id, "file_id": fileID, "number": number, "sha256": sha, "size": len(content),
		"snapshot": snapRel, "created_at": "2026-10-20T09:00:00", "source": "test", "metadata": map[string]any{}, "state": "complete"}
}

// pfltIndex 造假档案索引 JSON（schema_version 必须是 1，装的时候要校验）。
func pfltIndex(archiveID string, rows ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"schema_version": 1, "archive_id": archiveID,
		"files": []any{map[string]any{"id": "f1", "path": "a.txt"}}, "versions": rows, "events": []any{}})
	return string(b)
}

// pfltStagePut 在 Mac 暂存区直接放一个内容寻址的 part，返回 spec。
func pfltStagePut(t *testing.T, f *pfrdFix, content string) pfldFileSpec {
	t.Helper()
	sha := pfrtSHA(content)
	p := filepath.Join(PFRoundMacStagingDir(f.meta), sha+".part")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return pfldFileSpec{SHA256: sha, Size: int64(len(content)), MtimeNs: pfrtOld.UnixNano()}
}

// pfltExpect 按磁盘现状 + 内容造判定期望值。
func pfltExpect(t *testing.T, p, content string) *PFExpect {
	t.Helper()
	st, err := PFLstat(p)
	if err != nil {
		t.Fatal(err)
	}
	e := PFExpectFile(st.Size, st.MtimeNs, st.Ino, pfrtSHA(content))
	return &e
}

// pfltLedger V1 台账（Mac 上一对 + 引用的快照都在）。
const (
	pfltSnap1 = pfltHV + "/文档/f1/V001_deadbeef_a.txt"
	pfltDB1   = "DB1"
)

func pfltPutLedgerV1(t *testing.T, f *pfrdFix) (js string) {
	t.Helper()
	f.macPut(pfltSnap1, "v1\n", pfrtOld)
	js = pfltIndex("ar1", pfltRow("v1", "f1", 1, pfltSnap1, pfrtSHA("v1\n"), "v1\n"))
	f.macPut(pfltHV+"/档案台账.sqlite3", pfltDB1, pfrtOld)
	f.macPut(pfltHV+"/档案索引.json", js, pfrtOld)
	return js
}

// ── 读一对 ──────────────────────────────────────────────

func TestPFLedgerStageRead(t *testing.T) {
	f := newPFRDFix(t)
	app := pfltApplier(t, f)
	js := pfltPutLedgerV1(t, f)

	staged, res := pflStage(context.Background(), app, pfltHV)
	if res.Status != PFApplyDone {
		t.Fatalf("读一对应当完成: %s", res)
	}
	if want := pfrtSHA(pfltDB1) + pfrtSHA(js); staged.Hash != want {
		t.Fatalf("合并哈希应当是 sqlite+json 两个哈希拼接: %s", staged.Hash)
	}
	if staged.SQLite.Size != int64(len(pfltDB1)) || staged.SQLite.SHA256 != pfrtSHA(pfltDB1) {
		t.Fatalf("sqlite 的 spec: %+v", staged.SQLite)
	}
	// 暂存区里有两个内容寻址的拷贝
	for _, s := range []pfldFileSpec{staged.SQLite, staged.JSON} {
		b, err := os.ReadFile(filepath.Join(PFRoundMacStagingDir(f.meta), s.SHA256+".part"))
		if err != nil {
			t.Fatalf("暂存区应有 %s.part: %v", s.SHA256[:12], err)
		}
		if int64(len(b)) != s.Size {
			t.Fatalf("%s.part 大小不对", s.SHA256[:12])
		}
	}
	// 放完锁了：再拿一次能马上拿到
	lk, ok, err := pfldLockFile(context.Background(), filepath.Join(f.root, pfltHV), time.Second)
	if err != nil || !ok {
		t.Fatalf("读完之后锁应当已放开: ok=%v err=%v", ok, err)
	}
	pfldUnlock(lk)
}

func TestPFLedgerStageJournalDefers(t *testing.T) {
	f := newPFRDFix(t)
	app := pfltApplier(t, f)
	pfltPutLedgerV1(t, f)
	// 应用正在写台账：旁边有 journal
	pfrdPutAt(t, f.root, pfltHV+"/档案台账.sqlite3-journal", "j", pfrtOld)
	if _, res := pflStage(context.Background(), app, pfltHV); res.Status != PFApplyDeferred || res.Code != pflCodeJournal {
		t.Fatalf("有 journal 应当推迟: %s", res)
	}
	if err := os.Remove(filepath.Join(f.root, pfltHV, "档案台账.sqlite3-journal")); err != nil {
		t.Fatal(err)
	}
	pfrdPutAt(t, f.root, pfltHV+"/档案台账.sqlite3-wal", "w", pfrtOld)
	if _, res := pflStage(context.Background(), app, pfltHV); res.Status != PFApplyDeferred || res.Code != pflCodeJournal {
		t.Fatalf("有 wal 应当推迟: %s", res)
	}
	// 推迟时暂存区干干净净，什么都没拷
	ents, err := os.ReadDir(PFRoundMacStagingDir(f.meta))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("推迟时不该往暂存区放东西: %v", ents)
	}
}

// ── 装一对 ──────────────────────────────────────────────

// pfltInstallSetup 摆好目标边的旧一对，并把新的一对（sqlite + 索引 + 快照）放进暂存区，返回安装请求。
func pfltInstallSetup(t *testing.T, f *pfrdFix, app *PFApplier) pfldInstallReq {
	t.Helper()
	f.macPut(pfltHV+"/档案台账.sqlite3", "DBold", pfrtOld)
	f.macPut(pfltHV+"/档案索引.json", `{"schema_version":1,"versions":[]}`, pfrtOld)
	snap2 := pfltHV + "/文档/f1/V002_cafe1234_a.txt"
	f.macPut(snap2, "v2\n", pfrtOld)
	js2 := pfltIndex("ar1", pfltRow("v2", "f1", 2, snap2, pfrtSHA("v2\n"), "v2\n"))
	return pfldInstallReq{
		SQLite:       pfltStagePut(t, f, "DBnew"),
		JSON:         pfltStagePut(t, f, js2),
		ExpectSQLite: pfltExpect(t, f.macAbs(pfltHV+"/档案台账.sqlite3"), "DBold"),
		ExpectJSON:   pfltExpect(t, f.macAbs(pfltHV+"/档案索引.json"), `{"schema_version":1,"versions":[]}`),
	}
}

func TestPFLedgerInstallPair(t *testing.T) {
	f := newPFRDFix(t)
	app := pfltApplier(t, f)
	req := pfltInstallSetup(t, f, app)

	res := pflInstallPair(context.Background(), app, pfltHV, req)
	if res.Status != PFApplyDone {
		t.Fatalf("应当装成: %s", res)
	}
	if f.macRead(pfltHV+"/档案台账.sqlite3") != "DBnew" {
		t.Fatal("台账 sqlite 应当是新的")
	}
	if b := f.macRead(pfltHV + "/档案索引.json"); b == "" || b[0] != '{' || len(b) < len("DBold") || !contains(b, "V002") {
		t.Fatalf("档案索引应当是新的: %s", b)
	}
	// 旧的一对进同步回收，原因「覆盖」
	idx := pfrdIndex(t, PFRoundMacRecycleDir(f.meta))
	var seenSq, seenJs bool
	for _, e := range idx {
		if e.Reason != string(PFReasonOverwrite) {
			continue
		}
		if filepath.Base(e.Path) == "档案台账.sqlite3" && e.SHA256 == pfrtSHA("DBold") {
			seenSq = true
		}
		if filepath.Base(e.Path) == "档案索引.json" {
			seenJs = true
		}
	}
	if !seenSq || !seenJs {
		t.Fatalf("旧的一对应当都进回收（覆盖）: %+v", idx)
	}
}

func TestPFLedgerInstallSnapshotsMissing(t *testing.T) {
	f := newPFRDFix(t)
	app := pfltApplier(t, f)
	req := pfltInstallSetup(t, f, app)
	// 快照没到齐
	if err := os.Remove(f.macAbs(pfltHV + "/文档/f1/V002_cafe1234_a.txt")); err != nil {
		t.Fatal(err)
	}
	res := pflInstallPair(context.Background(), app, pfltHV, req)
	if res.Status != PFApplyDeferred || res.Code != pflCodeSnapshots {
		t.Fatalf("快照不齐应当推迟不装: %s", res)
	}
	if f.macRead(pfltHV+"/档案台账.sqlite3") != "DBold" || contains(f.macRead(pfltHV+"/档案索引.json"), "V002") {
		t.Fatal("推迟时两边台账原样不动")
	}
	for _, s := range []pfldFileSpec{req.SQLite, req.JSON} {
		if _, err := os.Stat(filepath.Join(PFRoundMacStagingDir(f.meta), s.SHA256+".part")); err != nil {
			t.Fatalf("推迟时暂存区的 part 应当还在: %v", err)
		}
	}
}

func TestPFLedgerInstallExpectMismatch(t *testing.T) {
	f := newPFRDFix(t)
	app := pfltApplier(t, f)
	req := pfltInstallSetup(t, f, app)
	// 判定之后目标 sqlite 又被改了
	pfrdPutAt(t, f.root, pfltHV+"/档案台账.sqlite3", "别人改的", time.Now())
	res := pflInstallPair(context.Background(), app, pfltHV, req)
	if res.Status != PFApplyAbandoned || res.Code != pflCodeExpect {
		t.Fatalf("期望对不上应当放弃: %s", res)
	}
	if res.Uncertain {
		t.Fatal("什么都没动，不该是不确定")
	}
	// 两边原样：目标还是被改过的那份，暂存区 part 还在
	if f.macRead(pfltHV+"/档案台账.sqlite3") != "别人改的" {
		t.Fatal("放弃时不能覆盖目标")
	}
	if _, err := os.Stat(filepath.Join(PFRoundMacStagingDir(f.meta), req.SQLite.SHA256+".part")); err != nil {
		t.Fatalf("放弃时源侧 part 应当完好: %v", err)
	}
	if ents := pfrdRecycled(t, PFRoundMacRecycleDir(f.meta)); len(ents) != 0 {
		t.Fatalf("放弃时不该有东西进回收: %v", ents)
	}
}

func TestPFLedgerInstallFirstTime(t *testing.T) {
	f := newPFRDFix(t)
	app := pfltApplier(t, f)
	// 目标还没有台账：期望两个都不存在
	if err := os.MkdirAll(filepath.Join(f.root, filepath.FromSlash(pfltHV)), 0o755); err != nil {
		t.Fatal(err)
	}
	absent := PFExpectAbsent()
	snap2 := pfltHV + "/文档/f1/V002_cafe1234_a.txt"
	f.macPut(snap2, "v2\n", pfrtOld)
	js2 := pfltIndex("ar1", pfltRow("v2", "f1", 2, snap2, pfrtSHA("v2\n"), "v2\n"))
	req := pfldInstallReq{SQLite: pfltStagePut(t, f, "DBnew"), JSON: pfltStagePut(t, f, js2),
		ExpectSQLite: &absent, ExpectJSON: &absent}
	res := pflInstallPair(context.Background(), app, pfltHV, req)
	if res.Status != PFApplyDone {
		t.Fatalf("首次应当装成: %s", res)
	}
	if f.macRead(pfltHV+"/档案台账.sqlite3") != "DBnew" {
		t.Fatal("新的应当放上正位")
	}
	if ents := pfrdRecycled(t, PFRoundMacRecycleDir(f.meta)); len(ents) != 0 {
		t.Fatalf("首次没有旧文件，不该有回收: %v", ents)
	}
}

// ── 锁 ──────────────────────────────────────────────────

func TestPFLedgerLockWaitsThenProceeds(t *testing.T) {
	f := newPFRDFix(t)
	app := pfltApplier(t, f)
	pfltPutLedgerV1(t, f)
	dir := filepath.Join(f.root, pfltHV)
	lk, ok, err := pfldLockFile(context.Background(), dir, time.Second)
	if err != nil || !ok {
		t.Fatalf("拿锁失败: ok=%v err=%v", ok, err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		pfldUnlock(lk)
	}()
	t0 := time.Now()
	_, res := pflStage(context.Background(), app, pfltHV)
	if res.Status != PFApplyDone {
		t.Fatalf("等锁放开后应当读成: %s", res)
	}
	if d := time.Since(t0); d < 100*time.Millisecond {
		t.Fatalf("应当在锁上等待过（只等了 %v）", d)
	}
}

func TestPFLedgerLockTimeoutDefers(t *testing.T) {
	f := newPFRDFix(t)
	app := pfltApplier(t, f)
	pfltPutLedgerV1(t, f)
	dir := filepath.Join(f.root, pfltHV)
	lk, ok, err := pfldLockFile(context.Background(), dir, time.Second)
	if err != nil || !ok {
		t.Fatalf("拿锁失败: ok=%v err=%v", ok, err)
	}
	defer pfldUnlock(lk)
	old := pflLockWait
	pflLockWait = 120 * time.Millisecond
	defer func() { pflLockWait = old }()
	_, res := pflStage(context.Background(), app, pfltHV)
	if res.Status != PFApplyDeferred || res.Code != pflCodeLocked {
		t.Fatalf("等锁超时应当推迟: %s", res)
	}
	res = pflInstallPair(context.Background(), app, pfltHV, pfltInstallSetup(t, f, app))
	if res.Status != PFApplyDeferred || res.Code != pflCodeLocked {
		t.Fatalf("装的时候等锁超时也应当推迟: %s", res)
	}
}

// ── 一轮编排 ────────────────────────────────────────────

func pfltLedgerCopied(t *testing.T, r *PFRoundResult) {
	t.Helper()
	for _, d := range r.Done {
		if d.Kind == PFActLedgerCopy {
			return
		}
	}
	t.Fatalf("应当有 ledger_copy 做成了:\n%s", pfrdDump(r))
}

func TestPFRoundLedgerSync(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("a.txt", "v1\n", pfrtOld)
	js1 := pfltPutLedgerV1(t, f)
	r := f.mustDone(f.run())
	pfltLedgerCopied(t, r)
	if f.ub.read("历史版本/档案台账.sqlite3") != pfltDB1 {
		t.Fatal("Ubuntu 台账 sqlite 应当与 Mac 一样")
	}
	if f.ub.read("历史版本/档案索引.json") != js1 {
		t.Fatal("Ubuntu 档案索引应当与 Mac 一样")
	}
	if f.ub.read(pfltSnap1) != "v1\n" {
		t.Fatal("快照先于台账，Ubuntu 上也应当在")
	}
	h1 := pfrtSHA(pfltDB1) + pfrtSHA(js1)
	base := f.loadBase()
	if base.Ledger().Hash != h1 {
		t.Fatalf("base 的台账哈希应当记成合并哈希: %s", base.Ledger().Hash)
	}
	if exp, err := base.ReadLedgerExport(h1); err != nil || string(exp) != js1 {
		t.Fatalf("base 里应有这一版的索引导出: %v", err)
	}
	f.settled()

	// Mac 又存了一版：快照 + 台账都变了
	f.macPut("a.txt", "v2\n", pfrtOld)
	snap2 := pfltHV + "/文档/f1/V002_cafe1234_a.txt"
	f.macPut(snap2, "v2\n", pfrtOld)
	js2 := pfltIndex("ar1", pfltRow("v1", "f1", 1, pfltSnap1, pfrtSHA("v1\n"), "v1\n"),
		pfltRow("v2", "f1", 2, snap2, pfrtSHA("v2\n"), "v2\n"))
	f.macPut(pfltHV+"/档案台账.sqlite3", "DB2", pfrtOld)
	f.macPut(pfltHV+"/档案索引.json", js2, pfrtOld)
	r2 := f.mustDone(f.run())
	pfltLedgerCopied(t, r2)
	if f.ub.read("历史版本/档案台账.sqlite3") != "DB2" || f.ub.read("历史版本/档案索引.json") != js2 {
		t.Fatal("Ubuntu 的台账一对应当跟上新版")
	}
	// Ubuntu 上被覆盖的旧一对进了同步回收（原因「覆盖」）
	var overwrites int
	for _, e := range pfrdIndex(t, PFRemoteRecycleDir(f.ub.meta)) {
		if e.Reason == string(PFReasonOverwrite) && (filepath.Base(e.Path) == "档案台账.sqlite3" || filepath.Base(e.Path) == "档案索引.json") {
			overwrites++
		}
	}
	if overwrites != 2 {
		t.Fatalf("旧的一对应当进 Ubuntu 回收（覆盖）: %+v", pfrdIndex(t, PFRemoteRecycleDir(f.ub.meta)))
	}
	h2 := pfrtSHA("DB2") + pfrtSHA(js2)
	if base = f.loadBase(); base.Ledger().Hash != h2 {
		t.Fatalf("base 的台账哈希应当跟到新版: %s", base.Ledger().Hash)
	}
	if exp, err := base.ReadLedgerExport(h2); err != nil || string(exp) != js2 {
		t.Fatalf("base 导出应当是新的: %v", err)
	}
	f.mustSame()
	f.settled()
}

// 两边台账一样、只有 base 落后：只写 base（base_ledger），不动文件。
func TestPFRoundLedgerBaseOnly(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("a.txt", "v1\n", pfrtOld)
	js1 := pfltPutLedgerV1(t, f)
	f.mustDone(f.run())
	h1 := pfrtSHA(pfltDB1) + pfrtSHA(js1)
	// 把 base 的台账记录抹掉（模拟 base 落后）
	if err := f.loadBase().SetLedger(PFBaseLedger{}); err != nil {
		t.Fatal(err)
	}
	r := f.mustDone(f.run())
	found := false
	for _, d := range r.Done {
		if d.Kind == PFActBaseLedger {
			found = true
		}
	}
	if !found {
		t.Fatalf("应当有 base_ledger 做成了:\n%s", pfrdDump(r))
	}
	if r.BytesUp != 0 || r.BytesDown != 0 {
		t.Fatalf("base_ledger 不该传字节: up=%d down=%d", r.BytesUp, r.BytesDown)
	}
	if b := f.loadBase(); b.Ledger().Hash != h1 {
		t.Fatalf("base 的台账哈希应当补记上: %s", b.Ledger().Hash)
	}
	f.settled()
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
