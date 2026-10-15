package projsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fpOf(t *testing.T, dir string) (string, bool) {
	t.Helper()
	fp, d, err := WorktreeFingerprint(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fp, d
}

func qwrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newQuietRepo(t *testing.T) string {
	d := t.TempDir()
	initRepo(d, t)
	return d
}

func qbump(t *testing.T, p string, sec int) {
	t.Helper()
	tm := time.Now().Add(time.Duration(sec) * time.Second)
	if err := os.Chtimes(p, tm, tm); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintCleanAndStable(t *testing.T) {
	d := newQuietRepo(t)
	if fp, dirty := fpOf(t, d); fp != "clean" || dirty {
		t.Fatalf("got %q %v", fp, dirty)
	}
	qwrite(t, filepath.Join(d, "README.md"), "x")
	a, dirty := fpOf(t, d)
	b, _ := fpOf(t, d)
	if !dirty || a == "clean" || a != b {
		t.Fatalf("不动应不变: %q %q %v", a, b, dirty)
	}
}

func TestFingerprintChanges(t *testing.T) {
	d := newQuietRepo(t)
	f := filepath.Join(d, "README.md")
	qwrite(t, f, "one")
	a, _ := fpOf(t, d)
	// 继续改同一文件：status 仍是 .M，尺寸变
	qwrite(t, f, "two-longer")
	b, _ := fpOf(t, d)
	if a == b {
		t.Fatal("继续编辑应改变指纹")
	}
	// 同尺寸、只改 mtime
	qbump(t, f, 100)
	c, _ := fpOf(t, d)
	if c == b {
		t.Fatal("mtime 变应改变指纹")
	}
	// 同尺寸同 mtime 但内容不同（mtime 被还原）
	fi, _ := os.Stat(f)
	qwrite(t, f, "TWO-LONGER")
	os.Chtimes(f, fi.ModTime(), fi.ModTime())
	e, _ := fpOf(t, d)
	if e == c {
		t.Fatal("小文件内容变（mtime 还原）应改变指纹")
	}
	// 新增未跟踪
	qwrite(t, filepath.Join(d, "new.txt"), "n")
	g, _ := fpOf(t, d)
	if g == e {
		t.Fatal("新增未跟踪应改变指纹")
	}
	// 暂存
	git(d, "add", "new.txt")
	h, _ := fpOf(t, d)
	if h == g {
		t.Fatal("暂存应改变指纹")
	}
}

func TestFingerprintRename(t *testing.T) {
	d := newQuietRepo(t)
	qwrite(t, filepath.Join(d, "a.txt"), "content content content")
	git(d, "add", ".")
	git(d, "commit", "-m", "a")
	a, dirty := fpOf(t, d)
	if dirty || a != "clean" {
		t.Fatal("应干净")
	}
	git(d, "mv", "a.txt", "b.txt")
	b, dirty := fpOf(t, d)
	if !dirty || b == "clean" {
		t.Fatal("重命名应 dirty")
	}
	out := git(d, "status", "--porcelain=v2", "-z")
	if !strings.HasPrefix(out, "2 ") {
		t.Fatalf("预期 rename 记录: %q", out)
	}
	ps, err := parseStatusV2Paths([]byte(out))
	if err != nil || len(ps) != 2 || ps[0] != "b.txt" || ps[1] != "a.txt" {
		t.Fatalf("%v %v", ps, err)
	}
}

func TestFingerprintOddNames(t *testing.T) {
	d := newQuietRepo(t)
	names := []string{"中文 文件.txt", "a b.txt", "x　y.txt", "tail "}
	for _, n := range names {
		qwrite(t, filepath.Join(d, n), "1")
	}
	out := git(d, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	ps, err := parseStatusV2Paths([]byte(out))
	if err != nil || len(ps) != len(names) {
		t.Fatalf("%q %v", ps, err)
	}
	a, _ := fpOf(t, d)
	qwrite(t, filepath.Join(d, "中文 文件.txt"), "22")
	b, _ := fpOf(t, d)
	if a == b {
		t.Fatal("中文名文件改动应被检出")
	}
}

func TestFingerprintGoneAndSymlink(t *testing.T) {
	d := newQuietRepo(t)
	if err := os.Symlink("target1", filepath.Join(d, "ln")); err != nil {
		t.Fatal(err)
	}
	a, _ := fpOf(t, d)
	os.Remove(filepath.Join(d, "ln"))
	os.Symlink("target2", filepath.Join(d, "ln"))
	b, _ := fpOf(t, d)
	if a == b {
		t.Fatal("符号链接目标变应改变指纹")
	}
	// statDesc：不存在记为 gone，不是 error
	s, err := statDesc(filepath.Join(d, "nope"))
	if err != nil || s != "gone" {
		t.Fatalf("%q %v", s, err)
	}
}

func TestFingerprintErrors(t *testing.T) {
	if _, _, err := WorktreeFingerprint(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("非仓库应返回 error")
	}
	if _, err := parseStatusV2Paths([]byte("Z weird\x00")); err == nil {
		t.Fatal("未知记录应 error")
	}
	// lstat 失败（非 NotExist）：用普通文件当目录 → ENOTDIR
	d := t.TempDir()
	qwrite(t, filepath.Join(d, "f"), "x")
	if _, err := statDesc(filepath.Join(d, "f", "g")); err == nil {
		t.Fatal("ENOTDIR 应 error")
	}
}

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestTrackerObserve(t *testing.T) {
	tr, err := LoadQuietTracker(filepath.Join(t.TempDir(), "q.json"))
	if err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if m := tr.Observe(wt, "A", t0); m != 0 {
		t.Fatal(m)
	}
	// 每 5 分钟观察一次，25 分钟后为 25
	var m int
	for i := 1; i <= 5; i++ {
		m = tr.Observe(wt, "A", t0.Add(time.Duration(5*i)*time.Minute))
	}
	if m != 25 {
		t.Fatalf("got %d", m)
	}
	if m := tr.Observe(wt, "B", t0.Add(30*time.Minute)); m != 0 {
		t.Fatalf("指纹变应归 0: %d", m)
	}
	if m := tr.Observe(wt, "B", t0.Add(35*time.Minute)); m != 5 {
		t.Fatalf("got %d", m)
	}
	// 时钟回拨
	if m := tr.Observe(wt, "B", t0.Add(10*time.Minute)); m != 0 {
		t.Fatalf("回拨应归 0: %d", m)
	}
	// 干净
	if m := tr.Observe(wt, "clean", t0.Add(11*time.Minute)); m != 0 {
		t.Fatal(m)
	}
	if m := tr.Observe(wt, "clean", t0.Add(16*time.Minute)); m != 0 {
		t.Fatal("clean 恒 0")
	}
}

func TestTrackerGap(t *testing.T) {
	tr, _ := LoadQuietTracker(filepath.Join(t.TempDir(), "q.json"))
	wt := t.TempDir()
	tr.Observe(wt, "A", t0)
	tr.Observe(wt, "A", t0.Add(10*time.Minute))
	// 间隔恰为 20 分钟（=2 倍）不算超过
	if m := tr.Observe(wt, "A", t0.Add(30*time.Minute)); m != 30 {
		t.Fatalf("got %d", m)
	}
	// 间隔 21 分钟 > 20 分钟 → 归 0
	if m := tr.Observe(wt, "A", t0.Add(51*time.Minute)); m != 0 {
		t.Fatalf("got %d", m)
	}
	if m := tr.Observe(wt, "A", t0.Add(56*time.Minute)); m != 5 {
		t.Fatalf("重置后重新计时: %d", m)
	}
	// 自定义间隔
	tr.DetectInterval = time.Minute
	if m := tr.Observe(wt, "A", t0.Add(60*time.Minute)); m != 0 {
		t.Fatalf("got %d", m)
	}
}

func TestTrackerCanonicalPath(t *testing.T) {
	tr, _ := LoadQuietTracker(filepath.Join(t.TempDir(), "q.json"))
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	tr.Observe(real, "A", t0)
	if m := tr.Observe(link+"/.", "A", t0.Add(7*time.Minute)); m != 7 {
		t.Fatalf("同一目录不同写法应同一记录: %d", m)
	}
}

func TestTrackerSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "q.json")
	tr, err := LoadQuietTracker(p)
	if err != nil || len(tr.Entries) != 0 {
		t.Fatal(err)
	}
	wt := t.TempDir()
	tr.Observe(wt, "A", t0)
	tr.Observe(wt, "A", t0.Add(5*time.Minute))
	if err := tr.Save(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限 %v", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("不应残留临时文件: %v", ents)
	}
	tr2, err := LoadQuietTracker(p)
	if err != nil {
		t.Fatal(err)
	}
	e1, e2 := tr.Entries[canonWorktree(wt)], tr2.Entries[canonWorktree(wt)]
	if e1.Fingerprint != e2.Fingerprint || !e1.Since.Equal(e2.Since) {
		t.Fatalf("%v %v", e1, e2)
	}
	// 磁盘条目不参与静止判断：第一次 Observe 重新计时
	if m := tr2.Observe(wt, "A", t0.Add(10*time.Minute)); m != 0 {
		t.Fatalf("加载后第一次应归 0: %d", m)
	}
	if m := tr2.Observe(wt, "A", t0.Add(15*time.Minute)); m != 5 {
		t.Fatalf("之后正常计时: %d", m)
	}
}

func TestTrackerLoadedIgnoresOldSince(t *testing.T) {
	p := filepath.Join(t.TempDir(), "q.json")
	wt := t.TempDir()
	tr, _ := LoadQuietTracker(p)
	tr.Observe(wt, "A", t0)
	tr.Save()
	b, _ := os.ReadFile(p)
	b = []byte(strings.ReplaceAll(string(b), "2026-01-01T12:00:00Z", "2000-01-01T00:00:00Z"))
	os.WriteFile(p, b, 0o600)
	tr2, err := LoadQuietTracker(p)
	if err != nil {
		t.Fatal(err)
	}
	if e := tr2.Entries[canonWorktree(wt)]; e.Since.Year() != 2000 {
		t.Fatalf("改写未生效: %v", e)
	}
	if m := tr2.Observe(wt, "A", t0.Add(time.Hour)); m != 0 {
		t.Fatalf("很早的 since 不应沿用: %d", m)
	}
}

// 注入带单调读数的 now（time.Now() 为基准再 Add，Add 保留单调读数），验证进程内 25 分钟返回 25。
func TestTrackerMonotonicInProcess(t *testing.T) {
	tr, _ := LoadQuietTracker(filepath.Join(t.TempDir(), "q.json"))
	wt := t.TempDir()
	base := time.Now()
	tr.Observe(wt, "A", base)
	for i := 1; i <= 5; i++ {
		tr.Observe(wt, "A", base.Add(time.Duration(5*i)*time.Minute))
	}
	if m := tr.Observe(wt, "A", base.Add(25*time.Minute)); m != 25 {
		t.Fatalf("got %d", m)
	}
}

func TestFingerprintHugeFileUnverifiable(t *testing.T) {
	d := newQuietRepo(t)
	f, err := os.Create(filepath.Join(d, "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(fileHashLimit + 1); err != nil {
		t.Skip("无法造稀疏文件:", err)
	}
	f.Close()
	a, dirty := fpOf(t, d)
	b, _ := fpOf(t, d)
	if !dirty || a == b || !strings.HasPrefix(a, unverifiablePrefix) {
		t.Fatalf("超大文件指纹应每次不同: %q %q", a, b)
	}
	tr, _ := LoadQuietTracker(filepath.Join(t.TempDir(), "q.json"))
	base := time.Now()
	for i := 0; i < 5; i++ {
		fp, _ := fpOf(t, d)
		if m := tr.Observe(d, fp, base.Add(time.Duration(10*i)*time.Minute)); m != 0 {
			t.Fatalf("无法确认应恒为 0: %d", m)
		}
	}
	// 恰好等于上限的稀疏文件仍可哈希
	os.Truncate(filepath.Join(d, "huge.bin"), 1<<20)
	c, _ := fpOf(t, d)
	if strings.HasPrefix(c, unverifiablePrefix) {
		t.Fatal("小文件不应无法确认")
	}
}

func TestTrackerAtomicSave(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "q.json")
	tr, _ := LoadQuietTracker(p)
	tr.Observe(dir, "A", t0)
	if err := tr.Save(); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(p)
	// rename 目标是目录 → rename 失败，旧文件内容保持，临时文件被清理
	dir2 := t.TempDir()
	p2 := filepath.Join(dir2, "q.json")
	os.Mkdir(p2, 0o700)
	os.WriteFile(filepath.Join(p2, "keep"), []byte("1"), 0o600)
	tr.path = p2
	if err := tr.Save(); err == nil {
		t.Fatal("应失败")
	}
	ents, _ := os.ReadDir(dir2)
	if len(ents) != 1 {
		t.Fatalf("临时文件未清理: %v", ents)
	}
	now, _ := os.ReadFile(p)
	if string(now) != string(old) {
		t.Fatal("原文件被改动")
	}
}

func TestTrackerCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "q.json")
	for _, c := range []string{"{not json", "null", ""} {
		os.WriteFile(p, []byte(c), 0o600)
		if _, err := LoadQuietTracker(p); err == nil {
			t.Fatalf("%q 应返回 error", c)
		}
	}
}

func TestQuietFuncWithDecide(t *testing.T) {
	tr, _ := LoadQuietTracker(filepath.Join(t.TempDir(), "q.json"))
	wt := t.TempDir()
	fps := map[string]string{wt: "A"}
	f := tr.QuietFunc(t0, fps)
	if m, ok := f(wt); !ok || m != 0 {
		t.Fatal(m, ok)
	}
	if _, ok := f("/no/such"); ok {
		t.Fatal("map 里没有应 false")
	}
	decide := func(at time.Duration) ActionKind {
		m, _ := tr.QuietFunc(t0.Add(at), fps)(wt)
		as := DecideWorktrees(RepoFacts{}, []WorktreeFacts{{Side: SideMac, Path: wt, Branch: "b", Dirty: true, QuietMinutes: m}})
		return as[0].Kind
	}
	tr.Observe(wt, "A", t0.Add(10*time.Minute))
	if k := decide(19 * time.Minute); k != KindWaitDirty {
		t.Fatalf("19 分钟应 WaitDirty: %v", k)
	}
	if k := decide(20 * time.Minute); k != KindNeedCommit {
		t.Fatalf("20 分钟应 NeedCommit: %v", k)
	}
}
