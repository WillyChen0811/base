package projsync

// 写原语（pfapply.go）的测试。测试只在 Mac Studio 跑（darwin：APFS 不区分大小写、对 NFC/NFD 不敏感）；
// 与文件系统无关的部分在 Linux 上也能跑，依赖文件系统行为的用 runtime.GOOS 或探测跳过并注明。
//
// 核心断言是「没有任何一份内容丢失」：每个竞态窗口注入「被改」之后，原内容要么在原位、要么在回收区，
// 新内容要么在原位、要么在暂存区——用 holds 在单元目录、暂存区、回收区三处按 sha256 找内容。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// ── 夹具 ────────────────────────────────────────────────

type pfaEnv struct {
	t      *testing.T
	base   string
	unit   string // 单元 rel
	root   string // 单元目录
	stg    string
	recDir string
	rec    *PFRecycle
	a      *PFApplier
	now    time.Time
	hook   PFApplyHook // 测试设置；通过 cfg.Hook 间接调用，可以随时替换
}

func pfaSHA(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// newPFAEnv 建一个单元目录、暂存区、回收区都在同一个临时目录下的环境（同一个文件系统）。
func newPFAEnv(t *testing.T) *pfaEnv {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &pfaEnv{t: t, base: base, unit: "2026/260403J1001-测试项目",
		now: time.Date(2026, 10, 26, 15, 30, 0, 0, time.Local)}
	e.root = filepath.Join(base, "sync", filepath.FromSlash(e.unit))
	e.stg = filepath.Join(base, "meta", "staging")
	e.recDir = filepath.Join(base, "meta", "回收")
	for _, d := range []string{e.root, e.stg} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.rec, err = NewPFRecycle(e.recDir, PFRecycleOptions{Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	e.a, err = NewPFApplier(PFApplyConfig{Root: e.root, UnitRel: e.unit, Staging: e.stg, Recycle: e.rec, Round: "r-001",
		Now:  func() time.Time { return e.now },
		Hook: func(w string) error { return e.callHook(w) }})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *pfaEnv) callHook(w string) error {
	if e.hook == nil {
		return nil
	}
	return e.hook(w)
}

// onHook 只在窗口 window 第一次出现时执行 f。
func (e *pfaEnv) onHook(window string, f func()) {
	done := false
	e.hook = func(w string) error {
		if w == window && !done {
			done = true
			f()
		}
		return nil
	}
}

// crashAt 让钩子在窗口 window 返回 error（模拟进程死在这里）。
func (e *pfaEnv) crashAt(window string) {
	e.hook = func(w string) error {
		if w == window {
			return errors.New("模拟崩溃")
		}
		return nil
	}
}

func (e *pfaEnv) abs(rel string) string { return filepath.Join(e.root, filepath.FromSlash(rel)) }

var pfaOldTime = time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)

// put 在单元里写一个文件（mtime 固定在过去，之后任何真实写入都会改变 mtime）。
func (e *pfaEnv) put(rel, content string) {
	e.t.Helper()
	pfaWriteFile(e.t, e.abs(rel), content, pfaOldTime)
}

func pfaWriteFile(t *testing.T, p, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// expect 读出单元里某个文件的现状作为前提（用真实 Lstat 和哈希）。
func (e *pfaEnv) expect(rel string) PFExpect {
	e.t.Helper()
	return pfaExpect(e.t, e.abs(rel))
}

func pfaExpect(t *testing.T, p string) PFExpect {
	t.Helper()
	st, err := PFLstat(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return PFExpectFile(st.Size, st.MtimeNs, st.Ino, pfaSHA(string(b)))
}

// stage 在暂存区放一份 part（0600，带一份 meta），返回 PFPart。
func (e *pfaEnv) stage(content string) PFPart {
	e.t.Helper()
	sha := pfaSHA(content)
	p := filepath.Join(e.stg, sha+".part")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.stg, sha+".meta"), []byte("meta\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return PFPart{Path: p, SHA256: sha, Size: int64(len(content))}
}

func (e *pfaEnv) read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(e.abs(rel))
	if err != nil {
		e.t.Fatalf("读 %s 失败：%v", rel, err)
	}
	return string(b)
}

func (e *pfaEnv) exists(rel string) bool {
	_, err := os.Lstat(e.abs(rel))
	return err == nil
}

// allHashes 返回单元目录、暂存区、回收区里所有普通文件的 sha256 → 路径列表。
func (e *pfaEnv) allHashes() map[string][]string {
	out := map[string][]string{}
	for _, dir := range []string{e.root, e.stg, e.recDir} {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			if filepath.Base(p) == pfRecycleIndexName || strings.HasSuffix(p, ".meta") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			out[pfaSHA(string(b))] = append(out[pfaSHA(string(b))], p)
			return nil
		})
	}
	return out
}

// holds 断言这些内容每一份都还在（单元目录、暂存区、回收区之一）。
func (e *pfaEnv) holds(contents ...string) {
	e.t.Helper()
	got := e.allHashes()
	for _, c := range contents {
		if len(got[pfaSHA(c)]) == 0 {
			var all []string
			for _, ps := range got {
				all = append(all, ps...)
			}
			sort.Strings(all)
			e.t.Errorf("内容 %q 丢了；现有文件：%v", c, all)
		}
	}
}

// leftovers 返回暂存区里的 .pfswap-*、.pfcopy-* 文件。
func (e *pfaEnv) leftovers() []PFLeftover {
	e.t.Helper()
	l, err := PFStagingLeftovers(e.stg)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *pfaEnv) wantStatus(res PFApplyResult, err error, st PFApplyStatus, code string) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("不该返回 error：%v（结果 %v）", err, res)
	}
	if res.Status != st || (code != "" && res.Code != code) {
		e.t.Fatalf("结果 = %v，想要 %v[%s]", res, st, code)
	}
}

func (e *pfaEnv) wantDone(res PFApplyResult, err error) {
	e.t.Helper()
	e.wantStatus(res, err, PFApplyDone, PFApplyCodeOK)
}

func (e *pfaEnv) noPartLeft(p PFPart) {
	e.t.Helper()
	if _, err := os.Lstat(p.Path); err == nil {
		e.t.Errorf("part %s 还在暂存区", p.Path)
	}
}

func (e *pfaEnv) partIntact(p PFPart, content string) {
	e.t.Helper()
	b, err := os.ReadFile(p.Path)
	if err != nil || string(b) != content {
		e.t.Errorf("part 应完好地留在暂存区：%v", err)
		return
	}
	st, _ := os.Lstat(p.Path)
	if st.Mode().Perm() != 0o600 {
		e.t.Errorf("part 的权限应改回 0600，实际 %v", st.Mode().Perm())
	}
	if st.ModTime().Before(time.Now().Add(-time.Hour)) {
		e.t.Errorf("part 的 mtime 应改回原来的（刚写入时的）值，实际 %v", st.ModTime())
	}
}

// appendTo 在原地往文件末尾追加（同一个 ino，大小和 mtime 都变）。
func pfaAppend(t *testing.T, p, extra string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// replaceByRename 用「写临时文件再改名盖上」的方式换掉文件（新 ino）。
func pfaReplace(t *testing.T, p, content string) {
	t.Helper()
	tmp := p + ".swap-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
}

// caseInsensitiveFS 探测 dir 所在文件系统是否不区分大小写。
func pfaCaseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	a := filepath.Join(dir, "PfaProbe")
	if err := os.WriteFile(a, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(a)
	_, err := os.Lstat(filepath.Join(dir, "pfaprobe"))
	return err == nil
}

func pfaSkipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root 用户不受权限位限制，跳过只读类测试")
	}
}

var bg = context.Background()

// ── 安装（新建）──────────────────────────────────────────

func TestPFApplyInstallNewBasic(t *testing.T) {
	e := newPFAEnv(t)
	part := e.stage("新内容")
	mtime := time.Date(2026, 9, 1, 8, 30, 15, 123456789, time.Local)
	res, err := e.a.InstallNew(bg, "报告/资料/新.docx", part, mtime.UnixNano())
	e.wantDone(res, err)
	if got := e.read("报告/资料/新.docx"); got != "新内容" {
		t.Fatalf("内容 = %q", got)
	}
	st, _ := os.Lstat(e.abs("报告/资料/新.docx"))
	if st.Mode().Perm() != 0o644 {
		t.Errorf("权限 = %v，想要 0644", st.Mode().Perm())
	}
	if st.ModTime().UnixNano() != mtime.UnixNano() {
		t.Errorf("mtime = %d，想要 %d", st.ModTime().UnixNano(), mtime.UnixNano())
	}
	for _, d := range []string{"报告", "报告/资料"} {
		dst, err := os.Lstat(e.abs(d))
		if err != nil || !dst.IsDir() || dst.Mode().Perm() != 0o755 {
			t.Errorf("上级目录 %s 应是 0755：%v %v", d, err, dst)
		}
	}
	e.noPartLeft(part)
	if res.Installed == nil || res.Installed.SHA256 != part.SHA256 || res.Installed.Size != int64(len("新内容")) || res.Installed.MtimeNs != mtime.UnixNano() {
		t.Errorf("Installed = %+v", res.Installed)
	}
	if ls := e.leftovers(); len(ls) != 0 {
		t.Errorf("暂存区不该有残留：%v", ls)
	}
	// 装好的文件与 part 是同一个 inode（改名，不是复制）
	if cur, _ := PFLstat(e.abs("报告/资料/新.docx")); cur.Ino != res.Installed.Ino {
		t.Errorf("Installed.Ino 对不上")
	}
}

func TestPFApplyInstallNewPartMismatchDeletesPart(t *testing.T) {
	e := newPFAEnv(t)
	// 暂存区里叫 <sha(B)>.part 的文件，实际内容是 A（传输层按已核验复用会出事）
	shaB := pfaSHA("B")
	p := filepath.Join(e.stg, shaB+".part")
	os.WriteFile(p, []byte("A"), 0o600)
	os.WriteFile(filepath.Join(e.stg, shaB+".meta"), []byte("meta\n"), 0o600)
	res, err := e.a.InstallNew(bg, "a.docx", PFPart{Path: p, SHA256: shaB, Size: 1}, 0)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodePartMismatch)
	if e.exists("a.docx") {
		t.Error("内容不符的 part 不该被装上")
	}
	for _, f := range []string{p, filepath.Join(e.stg, shaB+".meta")} {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("内容不符的暂存文件 %s 应该被删掉，免得传输层复用", f)
		}
	}
}

func TestPFApplyInstallNewTargetExists(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "别人的")
	part := e.stage("新")
	res, err := e.a.InstallNew(bg, "a.docx", part, 1)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetExists)
	if e.read("a.docx") != "别人的" {
		t.Error("已有文件被动了")
	}
	e.partIntact(part, "新")
}

func TestPFApplyInstallNewRaceCreate(t *testing.T) {
	e := newPFAEnv(t)
	part := e.stage("新")
	// 核对和建父目录都做完、改名之前，别人建了同名文件
	e.onHook("new:before-rename", func() { e.put("新目录/a.docx", "抢先的") })
	res, err := e.a.InstallNew(bg, "新目录/a.docx", part, 1)
	// 别人在我们刚建好的目录里建了文件：EEXIST，放弃
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetExists)
	if e.read("新目录/a.docx") != "抢先的" {
		t.Error("抢先的文件被覆盖了")
	}
	e.partIntact(part, "新")
	e.holds("抢先的", "新")
}

func TestPFApplyInstallNewRaceCreateRemovesOwnDirs(t *testing.T) {
	e := newPFAEnv(t)
	part := e.stage("新")
	// 钩子在 new:before-rename 之前上级目录已建好；这里让钩子把整个目标位置换成别人的文件，
	// 然后我们的改名失败，我们建的空目录要删掉（这里目录里有别人的文件，所以删不掉也不该删）。
	e.onHook("new:before-rename", func() { os.WriteFile(e.abs("d1/d2/a.docx"), []byte("抢先的"), 0o644) })
	res, err := e.a.InstallNew(bg, "d1/d2/a.docx", part, 1)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetExists)
	e.holds("抢先的", "新")

	// 没人抢时放弃（别的原因）要把自己建的空目录删掉：part 在安装前被换掉
	e2 := newPFAEnv(t)
	part2 := e2.stage("新2")
	e2.onHook("new:before-rename", func() {
		os.Remove(part2.Path)
		os.WriteFile(part2.Path, []byte("新2"), 0o600) // 内容相同，但 ino 变了
	})
	res, err = e2.a.InstallNew(bg, "x1/x2/a.docx", part2, 1)
	e2.wantStatus(res, err, PFApplyAbandoned, PFApplyCodePartChanged)
	if e2.exists("x1") {
		t.Error("放弃后自己建的空目录应该删掉")
	}
	e2.partIntact(part2, "新2")
}

func TestPFApplyInstallNewNameClash(t *testing.T) {
	e := newPFAEnv(t)
	e.put("Report.docx", "已有")
	part := e.stage("新")
	// Report 与 report 折叠键相同：在不区分大小写的盘上会打到它，两种系统上都不新建
	res, err := e.a.InstallNew(bg, "report.docx", part, 1)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameClash)
	if e.read("Report.docx") != "已有" {
		t.Error("Report.docx 被动了")
	}
	e.partIntact(part, "新")
	// NFC 与 NFD
	e.put("café.docx", "nfc版")
	res, err = e.a.InstallNew(bg, "café.docx", part, 1)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameClash)
}

func TestPFApplyInstallNewParentClash(t *testing.T) {
	e := newPFAEnv(t)
	e.put("Docs/x.docx", "x")
	part := e.stage("新")
	res, err := e.a.InstallNew(bg, "docs/a.docx", part, 1)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameClash)
	if _, err := os.Lstat(filepath.Join(e.root, "Docs", "a.docx")); err == nil {
		t.Error("不该写进只差大小写的目录")
	}
	e.partIntact(part, "新")
}

func TestPFApplyInstallNewSymlinkAndFileInPath(t *testing.T) {
	e := newPFAEnv(t)
	outside := filepath.Join(e.base, "outside")
	os.MkdirAll(outside, 0o755)
	if err := os.Symlink(outside, e.abs("link")); err != nil {
		t.Fatal(err)
	}
	part := e.stage("新")
	res, err := e.a.InstallNew(bg, "link/a.docx", part, 1)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeSymlinkInPath)
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Error("不该穿过符号链接写到外面")
	}
	e.put("afile", "我是文件")
	res, err = e.a.InstallNew(bg, "afile/b.docx", part, 1)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNotDir)
}

func TestPFApplyInstallNewBadNames(t *testing.T) {
	e := newPFAEnv(t)
	part := e.stage("新")
	for _, rel := range []string{".DS_Store", "d/~$x.docx", "a\x01b.docx", "dir.app/x.txt", "x.tmp", "../escape", "/abs", "a//b"} {
		res, err := e.a.InstallNew(bg, rel, part, 1)
		if err != nil || res.Status != PFApplyOutOfScope || res.Code != PFApplyCodeBadName {
			t.Errorf("%q：结果 %v err=%v，想要超出范围 bad_name", rel, res, err)
		}
	}
	e.partIntact(part, "新")
}

func TestPFApplyInstallArgsAreConfigErrors(t *testing.T) {
	e := newPFAEnv(t)
	part := e.stage("新")
	bad := []PFPart{
		{Path: filepath.Join(e.base, "x.part"), SHA256: part.SHA256, Size: part.Size},    // 不在暂存区
		{Path: part.Path, SHA256: "xyz", Size: part.Size},                                // sha 不合法
		{Path: filepath.Join(e.stg, "a.part"), SHA256: part.SHA256, Size: part.Size},     // 名字不是 <sha>.part
		{Path: filepath.Join(e.stg, pfaSHA("别的")+".part"), SHA256: part.SHA256, Size: 1}, // sha 与文件名不一致
	}
	for i, p := range bad {
		var ce *PFApplyConfigError
		if _, err := e.a.InstallNew(bg, "a.docx", p, 1); !errors.As(err, &ce) {
			t.Errorf("参数 %d 应返回配置错误，得到 %v", i, err)
		}
	}
	if _, err := e.a.InstallOver(bg, "a.docx", part, 1, PFExpectAbsent()); err == nil {
		t.Error("InstallOver 的前提是 Absent 应报错")
	}
}

func TestPFApplyInstallNewReadOnlyDir(t *testing.T) {
	pfaSkipIfRoot(t)
	e := newPFAEnv(t)
	os.MkdirAll(e.abs("ro"), 0o755)
	os.Chmod(e.abs("ro"), 0o555)
	t.Cleanup(func() { os.Chmod(e.abs("ro"), 0o755) })
	part := e.stage("新")
	res, err := e.a.InstallNew(bg, "ro/a.docx", part, 1)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeReadOnly)
	e.partIntact(part, "新")
}

func TestPFApplyInstallNewICloudStub(t *testing.T) {
	e := newPFAEnv(t)
	e.put(".a.docx.icloud", "占位")
	part := e.stage("新")
	res, err := e.a.InstallNew(bg, "a.docx", part, 1)
	e.wantStatus(res, err, PFApplyDeferred, PFApplyCodeICloud)
	if e.exists("a.docx") {
		t.Error("有 iCloud 占位时不该新建同名文件")
	}
}

func TestPFApplyInstallNewCanceled(t *testing.T) {
	e := newPFAEnv(t)
	part := e.stage("新")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	res, err := e.a.InstallNew(ctx, "a.docx", part, 1)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeCanceled)
	e.partIntact(part, "新")
}

func TestPFApplyInstallNewCrashWindows(t *testing.T) {
	for _, w := range []string{"new:before-rename", "new:after-rename"} {
		e := newPFAEnv(t)
		part := e.stage("新")
		e.crashAt(w)
		res, err := e.a.InstallNew(bg, "d/a.docx", part, 1)
		e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeHookAbort)
		if !res.Uncertain {
			t.Errorf("%s：崩溃应标不确定", w)
		}
		e.holds("新")
	}
}

// ── 安装（覆盖）──────────────────────────────────────────

func TestPFApplyInstallOverBasic(t *testing.T) {
	e := newPFAEnv(t)
	e.put("d/a.docx", "旧内容")
	os.Chmod(e.abs("d/a.docx"), 0o640)
	want := e.expect("d/a.docx")
	part := e.stage("新内容")
	mtime := time.Date(2026, 9, 2, 8, 0, 0, 5, time.Local)
	res, err := e.a.InstallOver(bg, "d/a.docx", part, mtime.UnixNano(), want)
	e.wantDone(res, err)
	if e.read("d/a.docx") != "新内容" {
		t.Fatalf("内容 = %q", e.read("d/a.docx"))
	}
	st, _ := os.Lstat(e.abs("d/a.docx"))
	if st.Mode().Perm() != 0o640 {
		t.Errorf("覆盖应保留目标原来的权限位 0640，实际 %v", st.Mode().Perm())
	}
	if st.ModTime().UnixNano() != mtime.UnixNano() {
		t.Errorf("mtime 不对")
	}
	e.noPartLeft(part)
	if len(e.leftovers()) != 0 {
		t.Errorf("暂存区残留 %v", e.leftovers())
	}
	// 旧内容进了回收，index 有一条
	if res.Recycled == nil {
		t.Fatal("没有回收记录")
	}
	r := res.Recycled
	if r.Reason != "覆盖" || r.SHA256 != pfaSHA("旧内容") || r.Path != "d/a.docx" || r.Unit != e.unit || r.Round != "r-001" || r.Size != int64(len("旧内容")) || r.MtimeNs != want.MtimeNs {
		t.Errorf("回收记录 = %+v", r)
	}
	b, err := os.ReadFile(e.rec.Path(*r))
	if err != nil || string(b) != "旧内容" {
		t.Errorf("回收区里的内容 = %q %v", b, err)
	}
	if !strings.HasPrefix(r.Stored, "2026-10-26/"+e.unit+"/d/a.docx") {
		t.Errorf("回收路径 = %q", r.Stored)
	}
	idx, err := e.rec.ReadIndex("2026-10-26")
	if err != nil || len(idx) != 1 || idx[0] != *r {
		t.Errorf("index = %+v %v", idx, err)
	}
	if res.Installed == nil || res.Installed.SHA256 != part.SHA256 {
		t.Errorf("Installed = %+v", res.Installed)
	}
}

func TestPFApplyInstallOverPreconditionMismatch(t *testing.T) {
	cases := []struct {
		name string
		mut  func(w PFExpect) PFExpect
	}{
		{"大小", func(w PFExpect) PFExpect { w.Size++; return w }},
		{"mtime", func(w PFExpect) PFExpect { w.MtimeNs += 1e9; return w }},
		{"ino", func(w PFExpect) PFExpect { w.Ino++; return w }},
	}
	for _, c := range cases {
		e := newPFAEnv(t)
		e.put("a.docx", "旧")
		part := e.stage("新")
		res, err := e.a.InstallOver(bg, "a.docx", part, 1, c.mut(e.expect("a.docx")))
		e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetChanged)
		if e.read("a.docx") != "旧" {
			t.Errorf("%s：目标被动了", c.name)
		}
		e.partIntact(part, "新")
		if e.rec != nil {
			if u, _ := e.rec.Usage(); u.Files != 0 {
				t.Errorf("%s：回收区不该有东西", c.name)
			}
		}
	}
}

// 前提里的哈希与目标实际内容不同，但大小、mtime、ino 都对（「改了内容又把 mtime 改回去」）：
// 执行前的 stat 核对看不出来，交换后读内容核对发现，换回去。
func TestPFApplyInstallOverHashMismatchSwapsBack(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "目标现在的内容")
	want := e.expect("a.docx")
	want.SHA256 = pfaSHA("判定时以为的内容")
	part := e.stage("新")
	res, err := e.a.InstallOver(bg, "a.docx", part, 1, want)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeChangedInWindow)
	if res.Uncertain {
		t.Errorf("换回去成功，不该标不确定：%v", res)
	}
	if e.read("a.docx") != "目标现在的内容" {
		t.Error("目标内容应原样留在原位")
	}
	e.partIntact(part, "新")
	if len(e.leftovers()) != 0 {
		t.Errorf("换回去后暂存区不该有残留：%v", e.leftovers())
	}
	if u, _ := e.rec.Usage(); u.Files != 0 {
		t.Error("没覆盖成，回收区不该有东西")
	}
	// 目标文件的 ino、mtime 也该和换之前一样
	if cur := e.expect("a.docx"); cur.Ino != want.Ino || cur.MtimeNs != want.MtimeNs {
		t.Errorf("换回去之后文件的 ino/mtime 变了")
	}
}

func TestPFApplyInstallOverRaceBeforeSwap(t *testing.T) {
	type tc struct {
		name string
		mut  func(e *pfaEnv)
		keep string // 目标位置最后应有的内容
	}
	cases := []tc{
		{"原地追加", func(e *pfaEnv) { pfaAppend(t, e.abs("a.docx"), "+用户新写的") }, "旧+用户新写的"},
		{"改名替换", func(e *pfaEnv) { pfaReplace(t, e.abs("a.docx"), "别人整个换了") }, "别人整个换了"},
		{"原地改写并还原大小与mtime", func(e *pfaEnv) {
			p := e.abs("a.docx")
			os.WriteFile(p, []byte("改"), 0o644)
			os.Chtimes(p, pfaOldTime, pfaOldTime)
		}, "改"},
	}
	for _, c := range cases {
		e := newPFAEnv(t)
		e.put("a.docx", "旧")
		want := e.expect("a.docx")
		part := e.stage("新")
		e.onHook("over:before-swap", func() { c.mut(e) })
		res, err := e.a.InstallOver(bg, "a.docx", part, 1, want)
		e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeChangedInWindow)
		if res.Uncertain {
			t.Errorf("%s：不该标不确定 %v", c.name, res)
		}
		if got := e.read("a.docx"); got != c.keep {
			t.Errorf("%s：目标内容 = %q，想要 %q（被改的内容必须留在原位）", c.name, got, c.keep)
		}
		e.partIntact(part, "新")
		e.holds(c.keep, "新")
		if u, _ := e.rec.Usage(); u.Files != 0 {
			t.Errorf("%s：没覆盖成，回收区不该有东西", c.name)
		}
		if len(e.leftovers()) != 0 {
			t.Errorf("%s：暂存区残留 %v", c.name, e.leftovers())
		}
	}
}

func TestPFApplyInstallOverTargetDeletedBeforeSwap(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "旧")
	want := e.expect("a.docx")
	part := e.stage("新")
	e.onHook("over:before-swap", func() { os.Remove(e.abs("a.docx")) })
	res, err := e.a.InstallOver(bg, "a.docx", part, 1, want)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetMissing)
	if e.exists("a.docx") {
		t.Error("不该凭空建出目标")
	}
	e.partIntact(part, "新")
	if len(e.leftovers()) != 0 {
		t.Errorf("暂存区残留 %v", e.leftovers())
	}
}

func TestPFApplyInstallOverRaceAfterSwap(t *testing.T) {
	// 交换之后、核对之前，换下来的文件（用户的编辑器还开着它）又被写了一下
	e := newPFAEnv(t)
	e.put("a.docx", "旧")
	want := e.expect("a.docx")
	part := e.stage("新")
	e.onHook("over:after-swap", func() {
		ls := e.leftovers()
		if len(ls) != 1 || ls[0].Kind != "swap" {
			t.Errorf("交换之后暂存区应有一个 swap 文件：%v", ls)
			return
		}
		pfaAppend(t, ls[0].Path, "+编辑器又写了")
	})
	res, err := e.a.InstallOver(bg, "a.docx", part, 1, want)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeChangedInWindow)
	if res.Uncertain {
		t.Errorf("换回去成功，不该标不确定：%v", res)
	}
	if got := e.read("a.docx"); got != "旧+编辑器又写了" {
		t.Errorf("目标内容 = %q", got)
	}
	e.partIntact(part, "新")
	e.holds("旧+编辑器又写了", "新")
	if len(e.leftovers()) != 0 {
		t.Errorf("暂存区残留 %v", e.leftovers())
	}
}

func TestPFApplyInstallOverStrangerDuringSwap(t *testing.T) {
	// 交换之后，换下来的文件被改（要换回去），同时目标位置又被第三方换成了另一个文件：
	// 换回去之后 slot 上是第三方的文件。两份有人写过的内容都必须留着，新内容的暂存文件没了要重传。
	e := newPFAEnv(t)
	e.put("a.docx", "旧")
	want := e.expect("a.docx")
	part := e.stage("新")
	e.onHook("over:after-swap", func() {
		ls := e.leftovers()
		if len(ls) != 1 {
			t.Errorf("残留 %v", ls)
			return
		}
		pfaAppend(t, ls[0].Path, "+被改过")
		pfaReplace(t, e.abs("a.docx"), "第三方")
	})
	res, err := e.a.InstallOver(bg, "a.docx", part, 1, want)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeChangedInWindow)
	if !res.Uncertain || len(res.Residue) == 0 {
		t.Errorf("应标不确定并说明残留：%v", res)
	}
	e.holds("旧+被改过", "第三方")
	if got := e.read("a.docx"); got != "旧+被改过" {
		t.Errorf("目标位置应是被改过的那份，实际 %q", got)
	}
}

func TestPFApplyInstallOverCrashWindows(t *testing.T) {
	for _, w := range []string{"over:before-swap", "over:after-swap", "over:before-recycle", "over:after-recycle"} {
		e := newPFAEnv(t)
		e.put("a.docx", "旧")
		want := e.expect("a.docx")
		part := e.stage("新")
		e.crashAt(w)
		res, err := e.a.InstallOver(bg, "a.docx", part, 1, want)
		e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeHookAbort)
		if !res.Uncertain {
			t.Errorf("%s：崩溃应标不确定", w)
		}
		// 崩在任何一步，新旧两份内容都还在（单元目录、暂存区、回收区之一）
		e.holds("旧", "新")
		// 崩溃留下的暂存区私有文件不会被暂存区清理当成过期 part 删掉
		for _, l := range e.leftovers() {
			if pfTrStageName.MatchString(filepath.Base(l.Path)) {
				t.Errorf("%s：私有名字不该匹配暂存区清理的文件名规则：%s", w, l.Path)
			}
		}
	}
}

func TestPFApplyInstallOverAfterStaleCleanDoesNotDeleteOldFile(t *testing.T) {
	// 崩在交换之后：被换下来的用户文件在暂存区。PFCleanStaging 过了 7 天也不能把它删了。
	e := newPFAEnv(t)
	e.put("a.docx", "旧")
	want := e.expect("a.docx")
	part := e.stage("新")
	e.crashAt("over:after-swap")
	_, _ = e.a.InstallOver(bg, "a.docx", part, 1, want)
	future := time.Now().Add(30 * 24 * time.Hour)
	// 给暂存区里所有文件一个很老的 mtime，再清理
	filepath.WalkDir(e.stg, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			os.Chtimes(p, time.Now().Add(-60*24*time.Hour), time.Now().Add(-60*24*time.Hour))
		}
		return nil
	})
	if _, err := PFCleanStaging(bg, PFLocalFS{}, e.stg, 7*24*time.Hour, future, nil); err != nil {
		t.Fatal(err)
	}
	e.holds("旧")
}

func TestPFApplyInstallOverRecyclesWithoutDeletingExisting(t *testing.T) {
	// 同一路径连续覆盖两次：两份旧内容都在回收区里，后一份换名，不盖掉前一份
	e := newPFAEnv(t)
	e.put("a.docx", "v1")
	res, err := e.a.InstallOver(bg, "a.docx", e.stage("v2"), 5, e.expect("a.docx"))
	e.wantDone(res, err)
	res, err = e.a.InstallOver(bg, "a.docx", e.stage("v3"), 6, e.expect("a.docx"))
	e.wantDone(res, err)
	e.holds("v1", "v2", "v3")
	idx, _ := e.rec.ReadIndex("2026-10-26")
	if len(idx) != 2 || idx[0].Stored == idx[1].Stored {
		t.Fatalf("index = %+v", idx)
	}
	if !strings.Contains(idx[1].Stored, "~153000-") {
		t.Errorf("重名应加 ~HHMMSS-xxxx：%q", idx[1].Stored)
	}
}

// ── 删除 ────────────────────────────────────────────────

func TestPFApplyDeleteBasic(t *testing.T) {
	e := newPFAEnv(t)
	e.put("d/a.docx", "要删的")
	want := e.expect("d/a.docx")
	res, err := e.a.Delete(bg, "d/a.docx", want, PFReasonDelete)
	e.wantDone(res, err)
	if e.exists("d/a.docx") {
		t.Error("原位置应已空")
	}
	r := res.Recycled
	if r == nil || r.Reason != "删除" || r.SHA256 != pfaSHA("要删的") || r.Path != "d/a.docx" || r.MtimeNs != want.MtimeNs {
		t.Fatalf("回收记录 = %+v", r)
	}
	if b, err := os.ReadFile(e.rec.Path(*r)); err != nil || string(b) != "要删的" {
		t.Errorf("回收区内容 = %q %v", b, err)
	}
	idx, _ := e.rec.ReadIndex("2026-10-26")
	if len(idx) != 1 || idx[0] != *r {
		t.Errorf("index = %+v", idx)
	}
	// 冲突原因
	e.put("c.docx", "落选的")
	res, err = e.a.Delete(bg, "c.docx", e.expect("c.docx"), PFReasonConflict)
	e.wantDone(res, err)
	if res.Recycled.Reason != "冲突" {
		t.Errorf("原因 = %q", res.Recycled.Reason)
	}
	// 「覆盖」不能用于删除
	var ce *PFApplyConfigError
	if _, err := e.a.Delete(bg, "x", want, PFReasonOverwrite); !errors.As(err, &ce) {
		t.Error("删除不该接受「覆盖」原因")
	}
}

func TestPFApplyDeletePreconditionMismatch(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "内容")
	want := e.expect("a.docx")
	want.Size++
	res, err := e.a.Delete(bg, "a.docx", want, PFReasonDelete)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetChanged)
	if e.read("a.docx") != "内容" {
		t.Error("被动了")
	}
	res, err = e.a.Delete(bg, "nope.docx", want, PFReasonDelete)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetMissing)
}

func TestPFApplyDeleteRaceModifiedBeforeMove(t *testing.T) {
	cases := map[string]func(e *pfaEnv){
		"原地追加": func(e *pfaEnv) { pfaAppend(t, e.abs("a.docx"), "+新写的") },
		"改名替换": func(e *pfaEnv) { pfaReplace(t, e.abs("a.docx"), "别人换的") },
	}
	keep := map[string]string{"原地追加": "旧+新写的", "改名替换": "别人换的"}
	for name, mut := range cases {
		e := newPFAEnv(t)
		e.put("a.docx", "旧")
		want := e.expect("a.docx")
		e.onHook("del:before-move", func() { mut(e) })
		res, err := e.a.Delete(bg, "a.docx", want, PFReasonDelete)
		e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeChangedInWindow)
		if res.Uncertain {
			t.Errorf("%s：移回去成功，不该标不确定 %v", name, res)
		}
		if got := e.read("a.docx"); got != keep[name] {
			t.Errorf("%s：原位应是被改过的内容 %q，实际 %q", name, keep[name], got)
		}
		if u, _ := e.rec.Usage(); u.Files != 0 {
			t.Errorf("%s：没删成，回收区不该留文件", name)
		}
		if idx, _ := e.rec.ReadIndex("2026-10-26"); len(idx) != 0 {
			t.Errorf("%s：没删成，index 不该有记录：%v", name, idx)
		}
		e.holds(keep[name])
	}
}

func TestPFApplyDeleteRaceOriginalSpotOccupied(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "旧")
	want := e.expect("a.docx")
	e.onHook("del:before-move", func() { pfaAppend(t, e.abs("a.docx"), "+新写的") })
	// 移走并核对不符之后、移回去之前，原位置又被别人建了文件
	hookedRestore := false
	prev := e.hook
	e.hook = func(w string) error {
		if w == "del:before-restore" && !hookedRestore {
			hookedRestore = true
			e.put("a.docx", "占位的新文件")
		}
		return prev(w)
	}
	res, err := e.a.Delete(bg, "a.docx", want, PFReasonDelete)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeChangedInWindow)
	if !res.Uncertain || len(res.Residue) == 0 || res.Recycled == nil {
		t.Errorf("应标不确定并说明内容留在回收：%v", res)
	}
	if e.read("a.docx") != "占位的新文件" {
		t.Error("原位置新文件被动了")
	}
	e.holds("旧+新写的", "占位的新文件")
	idx, _ := e.rec.ReadIndex("2026-10-26")
	if len(idx) != 1 || idx[0].Note == "" || idx[0].SHA256 != pfaSHA("旧+新写的") {
		t.Errorf("留在回收里的应记账并注明：%+v", idx)
	}
}

func TestPFApplyDeleteCrashWindows(t *testing.T) {
	for _, w := range []string{"del:before-move", "del:after-move", "del:after-verify"} {
		e := newPFAEnv(t)
		e.put("a.docx", "旧")
		e.crashAt(w)
		res, err := e.a.Delete(bg, "a.docx", e.expect("a.docx"), PFReasonDelete)
		e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeHookAbort)
		e.holds("旧")
	}
}

// ── 改名 ────────────────────────────────────────────────

func TestPFApplyRenameBasic(t *testing.T) {
	e := newPFAEnv(t)
	e.put("旧目录/a.mp4", "视频内容")
	want := e.expect("旧目录/a.mp4")
	res, err := e.a.Rename(bg, "旧目录/a.mp4", "新目录/子/b.mp4", want)
	e.wantDone(res, err)
	if e.exists("旧目录/a.mp4") || e.read("新目录/子/b.mp4") != "视频内容" {
		t.Error("改名没生效")
	}
	st, _ := PFLstat(e.abs("新目录/子/b.mp4"))
	if st.Ino != want.Ino || st.MtimeNs != want.MtimeNs {
		t.Error("改名应保持同一个文件（ino、mtime 不变）")
	}
	if res.Installed == nil || res.Installed.SHA256 != pfaSHA("视频内容") {
		t.Errorf("Installed = %+v", res.Installed)
	}
	if u, _ := e.rec.Usage(); u.Files != 0 {
		t.Error("改名不进回收")
	}
}

func TestPFApplyRenameTargetExistsAndClash(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "A")
	e.put("b.docx", "B")
	res, err := e.a.Rename(bg, "a.docx", "b.docx", e.expect("a.docx"))
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetExists)
	res, err = e.a.Rename(bg, "a.docx", "B.docx", e.expect("a.docx"))
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameClash)
	if e.read("a.docx") != "A" || e.read("b.docx") != "B" {
		t.Error("内容被动了")
	}
	// 只差大小写的改名第一版不做
	res, err = e.a.Rename(bg, "a.docx", "A.docx", e.expect("a.docx"))
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeCaseOnlyRename)
}

func TestPFApplyRenameRaceModified(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "旧")
	want := e.expect("a.docx")
	e.onHook("ren:before-rename", func() { pfaAppend(t, e.abs("a.docx"), "+新写的") })
	res, err := e.a.Rename(bg, "a.docx", "新/b.docx", want)
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeChangedInWindow)
	if res.Uncertain {
		t.Errorf("改回去成功不该标不确定：%v", res)
	}
	if e.read("a.docx") != "旧+新写的" || e.exists("新/b.docx") || e.exists("新") {
		t.Error("应改回原名，并删掉自己建的空目录")
	}
	// 改回去时原位被占 → 留在新位置
	e2 := newPFAEnv(t)
	e2.put("a.docx", "旧")
	want = e2.expect("a.docx")
	e2.onHook("ren:before-rename", func() { pfaAppend(t, e2.abs("a.docx"), "+新写的") })
	prev := e2.hook
	e2.hook = func(w string) error {
		if w == "ren:before-restore" {
			e2.put("a.docx", "占位")
		}
		return prev(w)
	}
	res, err = e2.a.Rename(bg, "a.docx", "b.docx", want)
	e2.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeChangedInWindow)
	if !res.Uncertain || len(res.Residue) == 0 {
		t.Errorf("原位被占应标不确定：%v", res)
	}
	e2.holds("旧+新写的", "占位")
}

func TestPFApplyConflictRename(t *testing.T) {
	e := newPFAEnv(t)
	e.put("d/报告.docx", "落败的")
	name, err := PFConflictName("报告.docx", "Mac", time.Date(2026, 10, 26, 15, 30, 0, 0, time.Local), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.a.ConflictRename(bg, "d/报告.docx", name, e.expect("d/报告.docx"))
	e.wantDone(res, err)
	if e.exists("d/报告.docx") || e.read("d/"+name) != "落败的" {
		t.Error("冲突改名没生效")
	}
	var ce *PFApplyConfigError
	if _, err := e.a.ConflictRename(bg, "d/x", "a/b", e.expect("d/"+name)); !errors.As(err, &ce) {
		t.Error("冲突名里有 / 应报配置错误")
	}
}

// ── Office 锁 ───────────────────────────────────────────

func TestPFApplyOfficeLocks(t *testing.T) {
	type op struct {
		name string
		run  func(e *pfaEnv, want PFExpect) (PFApplyResult, error)
	}
	ops := []op{
		{"覆盖", func(e *pfaEnv, w PFExpect) (PFApplyResult, error) {
			return e.a.InstallOver(bg, "d/x.docx", e.stage("新"), 1, w)
		}},
		{"删除", func(e *pfaEnv, w PFExpect) (PFApplyResult, error) {
			return e.a.Delete(bg, "d/x.docx", w, PFReasonDelete)
		}},
		{"改名", func(e *pfaEnv, w PFExpect) (PFApplyResult, error) { return e.a.Rename(bg, "d/x.docx", "d/y.docx", w) }},
		{"冲突改名", func(e *pfaEnv, w PFExpect) (PFApplyResult, error) {
			return e.a.ConflictRename(bg, "d/x.docx", "x（冲突）.docx", w)
		}},
	}
	locks := []string{"d/~$x.docx", "d/.~lock.x.docx#"}
	for _, o := range ops {
		for _, lk := range locks {
			// 有效的锁 → 推迟，什么都不动
			e := newPFAEnv(t)
			e.put("d/x.docx", "原")
			pfaWriteFile(t, e.abs(lk), "lock", e.now.Add(-time.Hour))
			res, err := o.run(e, e.expect("d/x.docx"))
			e.wantStatus(res, err, PFApplyDeferred, PFApplyCodeOfficeLock)
			if e.read("d/x.docx") != "原" || e.exists("d/y.docx") {
				t.Errorf("%s/%s：推迟时不该动文件", o.name, lk)
			}
			// 11 小时 59 分的锁还是锁
			e2 := newPFAEnv(t)
			e2.put("d/x.docx", "原")
			pfaWriteFile(t, e2.abs(lk), "lock", e2.now.Add(-12*time.Hour+time.Minute))
			res, err = o.run(e2, e2.expect("d/x.docx"))
			e2.wantStatus(res, err, PFApplyDeferred, PFApplyCodeOfficeLock)
			// 超过 12 小时的当残留：照常做，并报告
			e3 := newPFAEnv(t)
			e3.put("d/x.docx", "原")
			pfaWriteFile(t, e3.abs(lk), "lock", e3.now.Add(-13*time.Hour))
			res, err = o.run(e3, e3.expect("d/x.docx"))
			e3.wantDone(res, err)
			if len(res.StaleLocks) != 1 || res.StaleLocks[0] != filepath.Base(lk) {
				t.Errorf("%s/%s：StaleLocks = %v", o.name, lk, res.StaleLocks)
			}
			if _, err := os.Lstat(e3.abs(lk)); err != nil {
				t.Errorf("%s/%s：不该动锁文件本身", o.name, lk)
			}
		}
	}
}

func TestPFApplyOfficeLockLongName(t *testing.T) {
	// 主名 ≥8 个字符时锁名被「~$」顶掉开头两个字符
	e := newPFAEnv(t)
	e.put("report2026.docx", "原")
	pfaWriteFile(t, e.abs("~$port2026.docx"), "lock", e.now.Add(-time.Minute))
	res, err := e.a.Delete(bg, "report2026.docx", e.expect("report2026.docx"), PFReasonDelete)
	e.wantStatus(res, err, PFApplyDeferred, PFApplyCodeOfficeLock)
	// 新建不看锁（目标不存在）
	part := e.stage("新")
	res, err = e.a.InstallNew(bg, "other.docx", part, 1)
	e.wantDone(res, err)
}

// ── 只读、iCloud、前提里的类型 ──────────────────────────

func TestPFApplyReadOnly(t *testing.T) {
	pfaSkipIfRoot(t)
	e := newPFAEnv(t)
	e.put("a.docx", "原")
	os.Chmod(e.abs("a.docx"), 0o444)
	part := e.stage("新")
	for _, run := range []func() (PFApplyResult, error){
		func() (PFApplyResult, error) { return e.a.InstallOver(bg, "a.docx", part, 1, e.expect("a.docx")) },
		func() (PFApplyResult, error) { return e.a.Delete(bg, "a.docx", e.expect("a.docx"), PFReasonDelete) },
		func() (PFApplyResult, error) { return e.a.Rename(bg, "a.docx", "b.docx", e.expect("a.docx")) },
	} {
		res, err := run()
		e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeReadOnly)
	}
	if e.read("a.docx") != "原" || e.exists("b.docx") {
		t.Error("只读文件被动了")
	}
	// 所在目录不可写
	e.put("ro/b.docx", "原b")
	os.MkdirAll(e.abs("ro/sub"), 0o755)
	os.Chmod(e.abs("ro"), 0o555)
	t.Cleanup(func() { os.Chmod(e.abs("ro"), 0o755) })
	res, err := e.a.Delete(bg, "ro/b.docx", e.expect("ro/b.docx"), PFReasonDelete)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeReadOnly)
	res, err = e.a.Rmdir(bg, "ro/sub")
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeReadOnly)
	e.holds("原", "原b")
}

func TestPFApplyDatalessIsDeferred(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "原")
	want := e.expect("a.docx")
	// 注入 stat：目标带 iCloud 占位标志
	e.a.cfg.Stat = func(p string) (PFStat, error) {
		st, err := PFLstat(p)
		if p == e.abs("a.docx") {
			st.Flags |= PFSFDataless
		}
		return st, err
	}
	part := e.stage("新")
	for _, run := range []func() (PFApplyResult, error){
		func() (PFApplyResult, error) { return e.a.InstallOver(bg, "a.docx", part, 1, want) },
		func() (PFApplyResult, error) { return e.a.Delete(bg, "a.docx", want, PFReasonDelete) },
		func() (PFApplyResult, error) { return e.a.Rename(bg, "a.docx", "b.docx", want) },
	} {
		res, err := run()
		e.wantStatus(res, err, PFApplyDeferred, PFApplyCodeICloud)
	}
	// 复制来源是占位文件：不读
	_, res, err := e.a.StageCopy(bg, PFCopySrc{Path: e.abs("a.docx"), SHA256: want.SHA256, Size: want.Size})
	e.wantStatus(res, err, PFApplyDeferred, PFApplyCodeICloud)
	if e.read("a.docx") != "原" {
		t.Error("被动了")
	}
	// .X.icloud 占位：X 当不明
	e.a.cfg.Stat = PFLstat
	e.put(".c.docx.icloud", "占位")
	e.put("c.docx", "本地也有一份旧的")
	res, err = e.a.Delete(bg, "c.docx", e.expect("c.docx"), PFReasonDelete)
	e.wantStatus(res, err, PFApplyDeferred, PFApplyCodeICloud)
}

func TestPFApplyNotRegularTargets(t *testing.T) {
	e := newPFAEnv(t)
	os.MkdirAll(e.abs("adir"), 0o755)
	os.Symlink("adir", e.abs("alink"))
	want := PFExpectFile(0, 0, 0, pfaSHA(""))
	res, err := e.a.Delete(bg, "adir", want, PFReasonDelete)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNotRegular)
	res, err = e.a.Delete(bg, "alink", want, PFReasonDelete)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNotRegular)
	part := e.stage("新")
	res, err = e.a.InstallNew(bg, "adir", part, 1)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNotRegular)
}

// ── 不同卷 ──────────────────────────────────────────────

func TestPFApplyDifferentVolumeIsConfigError(t *testing.T) {
	e := newPFAEnv(t)
	// 回收区在另一个卷
	otherRec, err := NewPFRecycle(filepath.Join(e.base, "meta", "回收2"), PFRecycleOptions{
		Stat: func(p string) (PFStat, error) {
			st, err := PFLstat(p)
			if strings.Contains(p, "回收2") {
				st.Dev += 7
			}
			return st, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	var ce *PFApplyConfigError
	_, err = NewPFApplier(PFApplyConfig{Root: e.root, UnitRel: e.unit, Staging: e.stg, Recycle: otherRec})
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "文件系统") {
		t.Errorf("回收区不同卷应报配置错误：%v", err)
	}
	// 暂存区在另一个卷
	_, err = NewPFApplier(PFApplyConfig{Root: e.root, UnitRel: e.unit, Staging: e.stg, Recycle: e.rec,
		Stat: func(p string) (PFStat, error) {
			st, err := PFLstat(p)
			if p == e.stg {
				st.Dev += 3
			}
			return st, err
		}})
	if !errors.As(err, &ce) {
		t.Errorf("暂存区不同卷应报配置错误：%v", err)
	}
	// 运行中挂载变了：回收区的 st_dev 变得不同 → 覆盖和删除都报配置错误，什么都不动
	e.put("a.docx", "原")
	want := e.expect("a.docx")
	e.rec.stat = func(p string) (PFStat, error) {
		st, err := PFLstat(p)
		if p == e.recDir {
			st.Dev += 9
		}
		return st, err
	}
	part := e.stage("新")
	if _, err := e.a.InstallOver(bg, "a.docx", part, 1, want); !errors.As(err, &ce) {
		t.Errorf("InstallOver：%v", err)
	}
	if _, err := e.a.Delete(bg, "a.docx", want, PFReasonDelete); !errors.As(err, &ce) {
		t.Errorf("Delete：%v", err)
	}
	if e.read("a.docx") != "原" {
		t.Error("不同卷时不该动文件")
	}
	e.partIntact(part, "新")
	// part 与单元目录不同卷
	e.rec.stat = PFLstat
	e.a.cfg.Stat = func(p string) (PFStat, error) {
		st, err := PFLstat(p)
		if p == part.Path {
			st.Dev += 5
		}
		return st, err
	}
	if _, err := e.a.InstallNew(bg, "n.docx", part, 1); !errors.As(err, &ce) {
		t.Errorf("part 不同卷应报配置错误：%v", err)
	}
}

// ── 复制复用 ────────────────────────────────────────────

func TestPFApplyStageCopyAndInstall(t *testing.T) {
	e := newPFAEnv(t)
	e.put("老位置/大视频.mp4", "视频字节")
	sha := pfaSHA("视频字节")
	// 来源在项目里别处
	res, err := e.a.CopyInstallNew(bg, PFCopySrc{Path: e.abs("老位置/大视频.mp4"), SHA256: sha, Size: int64(len("视频字节"))}, "新位置/大视频.mp4", pfaOldTime.UnixNano())
	e.wantDone(res, err)
	if e.read("新位置/大视频.mp4") != "视频字节" || e.read("老位置/大视频.mp4") != "视频字节" {
		t.Error("复制复用后两份都应在")
	}
	// 来源在回收区：先删一个文件进回收，再从回收里复制出来
	e.put("b.docx", "回收里的")
	dres, err := e.a.Delete(bg, "b.docx", e.expect("b.docx"), PFReasonDelete)
	e.wantDone(dres, err)
	src := PFCopySrc{Path: e.rec.Path(*dres.Recycled), SHA256: pfaSHA("回收里的"), Size: int64(len("回收里的"))}
	res, err = e.a.CopyInstallNew(bg, src, "恢复/b.docx", 7)
	e.wantDone(res, err)
	if e.read("恢复/b.docx") != "回收里的" {
		t.Error("从回收区复制失败")
	}
	b, _ := os.ReadFile(e.rec.Path(*dres.Recycled))
	if string(b) != "回收里的" {
		t.Error("回收区里的原件不该被动")
	}
	// 来源在暂存区（别的 part）
	part := e.stage("暂存的")
	res, err = e.a.CopyInstallNew(bg, PFCopySrc{Path: part.Path, SHA256: part.SHA256, Size: part.Size}, "c.docx", 8)
	// 同一个 part：目标 part 路径与来源相同，走「暂存区里已有同内容，直接用」
	e.wantDone(res, err)
	if e.read("c.docx") != "暂存的" {
		t.Error("从暂存区复制失败")
	}
	if len(e.leftovers()) != 0 {
		t.Errorf("暂存区残留 %v", e.leftovers())
	}
}

func TestPFApplyStageCopyOver(t *testing.T) {
	e := newPFAEnv(t)
	e.put("src.docx", "共同内容")
	e.put("dst.docx", "旧的")
	res, err := e.a.CopyInstallOver(bg, PFCopySrc{Path: e.abs("src.docx"), SHA256: pfaSHA("共同内容"), Size: int64(len("共同内容"))}, "dst.docx", 9, e.expect("dst.docx"))
	e.wantDone(res, err)
	if e.read("dst.docx") != "共同内容" || e.read("src.docx") != "共同内容" {
		t.Error("复制覆盖失败")
	}
	e.holds("旧的")
}

func TestPFApplyStageCopySourceMismatch(t *testing.T) {
	e := newPFAEnv(t)
	e.put("src.docx", "来源现在的内容")
	// 期望的哈希与来源实际内容不同（来源已被改）：删掉复制品，放弃
	_, res, err := e.a.StageCopy(bg, PFCopySrc{Path: e.abs("src.docx"), SHA256: pfaSHA("来源改之前的内容"), Size: int64(len("来源现在的内容"))})
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeSourceChanged)
	if len(e.leftovers()) != 0 {
		t.Errorf("放弃后不该留复制品：%v", e.leftovers())
	}
	if _, err := os.Lstat(filepath.Join(e.stg, pfaSHA("来源改之前的内容")+".part")); err == nil {
		t.Error("不该产生 part")
	}
	// 大小不对
	_, res, err = e.a.StageCopy(bg, PFCopySrc{Path: e.abs("src.docx"), SHA256: pfaSHA("x"), Size: 1})
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeSourceChanged)
	// 来源不在允许范围
	outside := filepath.Join(e.base, "outside.txt")
	os.WriteFile(outside, []byte("外面"), 0o644)
	var ce *PFApplyConfigError
	if _, _, err := e.a.StageCopy(bg, PFCopySrc{Path: outside, SHA256: pfaSHA("外面"), Size: 6}); !errors.As(err, &ce) {
		t.Errorf("来源在允许范围外应报配置错误：%v", err)
	}
}

func TestPFApplyStageCopyExistingPart(t *testing.T) {
	e := newPFAEnv(t)
	e.put("src.docx", "内容")
	sha := pfaSHA("内容")
	// 暂存区里已有同名但内容不全（可能正在传输）：不碰它，放弃
	p := filepath.Join(e.stg, sha+".part")
	os.WriteFile(p, []byte("内"), 0o600)
	_, res, err := e.a.StageCopy(bg, PFCopySrc{Path: e.abs("src.docx"), SHA256: sha, Size: int64(len("内容"))})
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodePartChanged)
	if b, _ := os.ReadFile(p); string(b) != "内" {
		t.Error("不该动暂存区里已有的文件")
	}
}

// ── 目录 ────────────────────────────────────────────────

func TestPFApplyMkdirRmdir(t *testing.T) {
	e := newPFAEnv(t)
	res, err := e.a.Mkdir(bg, "a/b/c")
	e.wantDone(res, err)
	for _, d := range []string{"a", "a/b", "a/b/c"} {
		st, err := os.Lstat(e.abs(d))
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o755 {
			t.Errorf("%s 应是 0755 目录：%v", d, err)
		}
	}
	res, err = e.a.Mkdir(bg, "a/b/c")
	e.wantDone(res, err)
	if !res.Existed {
		t.Error("已存在的目录应标 Existed")
	}
	// 同名是文件、折叠键相同
	e.put("f", "文件")
	res, err = e.a.Mkdir(bg, "f")
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNotDir)
	res, err = e.a.Mkdir(bg, "A/x")
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameClash)
	res, err = e.a.Mkdir(bg, "a/B")
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameClash)

	// Rmdir：空目录删掉；不空保留
	res, err = e.a.Rmdir(bg, "a/b/c")
	e.wantDone(res, err)
	if e.exists("a/b/c") {
		t.Error("空目录应被删")
	}
	e.put("a/b/x.docx", "里面有东西")
	res, err = e.a.Rmdir(bg, "a/b")
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeNotEmpty)
	e.holds("里面有东西")
	// 不存在、是文件
	res, err = e.a.Rmdir(bg, "nope")
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeTargetMissing)
	res, err = e.a.Rmdir(bg, "f")
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNotDir)
	if e.read("f") != "文件" {
		t.Error("文件不该被当目录删")
	}
}

func TestPFApplyRmdirRaceFileAdded(t *testing.T) {
	e := newPFAEnv(t)
	os.MkdirAll(e.abs("d"), 0o755)
	// 核对之后、rmdir 之前有人往目录里放了文件：rmdir 由系统确认为空，所以会失败，文件不丢
	e.onHook("rmdir:before-rmdir", func() { e.put("d/新来的.docx", "刚放进来的") })
	res, err := e.a.Rmdir(bg, "d")
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeNotEmpty)
	e.holds("刚放进来的")
}

func TestPFApplyRmdirSymlinkAndSiblings(t *testing.T) {
	e := newPFAEnv(t)
	os.MkdirAll(e.base+"/outside", 0o755)
	os.Symlink(e.base+"/outside", e.abs("lnk"))
	res, err := e.a.Rmdir(bg, "lnk")
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNotDir)
	if _, err := os.Lstat(e.abs("lnk")); err != nil {
		t.Error("符号链接不该被删")
	}
	// 目录名只差大小写（字节核对）
	os.MkdirAll(e.abs("Docs"), 0o755)
	res, err = e.a.Rmdir(bg, "docs")
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameMismatch)
	if !e.exists("Docs") {
		t.Error("Docs 不该被删")
	}
}

// ── 字节名核对（APFS 不区分大小写、对 NFC/NFD 不敏感）────────

func TestPFApplyByteExactNames(t *testing.T) {
	e := newPFAEnv(t)
	if !pfaCaseInsensitive(t, e.root) {
		t.Skip("这个文件系统区分大小写：按名字打不开别的文件，字节名核对不会被触发（见 pfswap_linux_test.go 的同名两文件测试）")
	}
	e.put("Report.docx", "原")
	want := e.expect("Report.docx")
	part := e.stage("新")
	// 按小写名字能 lstat 到，但必须不被当成同一个文件
	if _, err := os.Lstat(e.abs("report.docx")); err != nil {
		t.Fatalf("前提不成立：%v", err)
	}
	for name, run := range map[string]func() (PFApplyResult, error){
		"覆盖": func() (PFApplyResult, error) { return e.a.InstallOver(bg, "report.docx", part, 1, want) },
		"删除": func() (PFApplyResult, error) { return e.a.Delete(bg, "report.docx", want, PFReasonDelete) },
		"改名": func() (PFApplyResult, error) { return e.a.Rename(bg, "report.docx", "other.docx", want) },
		"冲突改名": func() (PFApplyResult, error) {
			return e.a.ConflictRename(bg, "report.docx", "report（冲突）.docx", want)
		},
	} {
		res, err := run()
		if err != nil || res.Status != PFApplyOutOfScope || res.Code != PFApplyCodeNameMismatch {
			t.Errorf("%s：结果 %v err=%v，想要超出范围 name_mismatch", name, res, err)
		}
	}
	if e.read("Report.docx") != "原" {
		t.Error("Report.docx 被动了")
	}
	if ents, _ := os.ReadDir(e.root); len(ents) != 1 || ents[0].Name() != "Report.docx" {
		t.Errorf("单元目录里应只有 Report.docx：%v", ents)
	}
	e.partIntact(part, "新")
	// 字节完全相同的名字照常工作
	res, err := e.a.InstallOver(bg, "Report.docx", part, 1, want)
	e.wantDone(res, err)
	// 目录段大小写不同
	e.put("Docs/a.docx", "a")
	res, err = e.a.Delete(bg, "docs/a.docx", e.expect("Docs/a.docx"), PFReasonDelete)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameClash)
	if e.read("Docs/a.docx") != "a" {
		t.Error("Docs/a.docx 被动了")
	}
}

func TestPFApplyByteExactNamesNFD(t *testing.T) {
	e := newPFAEnv(t)
	nfc, nfd := "café.docx", "café.docx"
	e.put(nfc, "nfc")
	if _, err := os.Lstat(e.abs(nfd)); err != nil {
		t.Skip("这个文件系统区分 NFC/NFD，字节名核对不会被触发")
	}
	res, err := e.a.Delete(bg, nfd, e.expect(nfc), PFReasonDelete)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameMismatch)
	// 磁盘上存的 NFD 名字：按 NFD 字节可以操作，按 NFC 不行
	e.put("nfd文件́.docx", "nfd")
	res, err = e.a.Delete(bg, "nfd文件́.docx", e.expect("nfd文件́.docx"), PFReasonDelete)
	e.wantDone(res, err)
	if !e.exists(nfc) {
		t.Error("NFC 文件不该被动")
	}
}

// ── 取消 ────────────────────────────────────────────────

func TestPFApplyCanceledBeforeStart(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "原")
	want := e.expect("a.docx")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	part := e.stage("新")
	for name, run := range map[string]func() (PFApplyResult, error){
		"覆盖":  func() (PFApplyResult, error) { return e.a.InstallOver(ctx, "a.docx", part, 1, want) },
		"删除":  func() (PFApplyResult, error) { return e.a.Delete(ctx, "a.docx", want, PFReasonDelete) },
		"改名":  func() (PFApplyResult, error) { return e.a.Rename(ctx, "a.docx", "b.docx", want) },
		"建目录": func() (PFApplyResult, error) { return e.a.Mkdir(ctx, "d") },
	} {
		res, err := run()
		if err != nil || res.Status != PFApplyAbandoned || res.Code != PFApplyCodeCanceled {
			t.Errorf("%s：%v %v", name, res, err)
		}
	}
	if e.read("a.docx") != "原" {
		t.Error("取消后文件被动了")
	}
}

// 交换之后的核对不响应取消：取消发生在交换之后，仍要完成核对并把旧文件放进回收。
func TestPFApplyCancelAfterSwapStillFinishes(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "旧")
	want := e.expect("a.docx")
	part := e.stage("新")
	ctx, cancel := context.WithCancel(bg)
	e.onHook("over:after-swap", cancel)
	res, err := e.a.InstallOver(ctx, "a.docx", part, 1, want)
	e.wantDone(res, err)
	if e.read("a.docx") != "新" || res.Recycled == nil {
		t.Errorf("取消发生在交换之后，仍应完成：%v", res)
	}
}

func TestPFApplyResultStrings(t *testing.T) {
	for st, s := range map[PFApplyStatus]string{PFApplyDone: "完成", PFApplyAbandoned: "放弃", PFApplyDeferred: "推迟", PFApplyOutOfScope: "超出范围"} {
		if st.String() != s {
			t.Errorf("%d 的名字 = %q", st, st.String())
		}
	}
	if runtime.GOOS == "" {
		t.Skip()
	}
}
