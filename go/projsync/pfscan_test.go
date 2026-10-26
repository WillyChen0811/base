package projsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 项目文件同步 T2 的测试：扫描、静止、指纹、iCloud 占位。
// 平台相关：权限类用例在 root 下跳过；SF_DATALESS 全靠注入的 stat 模拟（Studio 多半没开 iCloud 桌面）。

// ---- 测试夹具 ----

func pfstSHA(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// pfstWrite 写文件（自动建父目录），mtime 设为一小时前（已静止）。
func pfstWrite(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	pfstAge(t, p)
	return p
}

func pfstAge(t *testing.T, p string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

func pfstMkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func pfstSkipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root 不受文件权限限制，跳过权限类用例")
	}
}

// pfstStat 包一层 PFLstat：rel 是相对 root 的路径（/ 分隔，根自己是 "."），mod 可改返回值或报错。
func pfstStat(root string, mod func(rel string, st *PFStat) error) PFStatFunc {
	return func(p string) (PFStat, error) {
		st, err := PFLstat(p)
		if err != nil {
			return st, err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return st, rerr
		}
		if err := mod(filepath.ToSlash(rel), &st); err != nil {
			return PFStat{}, err
		}
		return st, nil
	}
}

// pfstOpens 记录哪些文件被打开读过。
type pfstOpens struct {
	mu    sync.Mutex
	paths []string
}

func (o *pfstOpens) open(p string) (io.ReadCloser, error) {
	o.mu.Lock()
	o.paths = append(o.paths, filepath.ToSlash(p))
	o.mu.Unlock()
	return os.Open(p)
}

func (o *pfstOpens) count(suffix string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, p := range o.paths {
		if strings.HasSuffix(p, suffix) {
			n++
		}
	}
	return n
}

func (o *pfstOpens) total() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.paths)
}

// pfstFilter 模拟 T1 的名字规则：点开头排除，.app 目录判超出范围。
func pfstFilter(rel string, isDir bool) PFScanVerdict {
	if strings.HasPrefix(pfBase(rel), ".") {
		return PFScanVerdict{Kind: PFScanExclude}
	}
	if isDir && strings.HasSuffix(rel, ".app") {
		return PFScanVerdict{Kind: PFScanOutOfScope, Reason: "Mac 包目录"}
	}
	if strings.HasSuffix(rel, ".bad") {
		return PFScanVerdict{Kind: PFScanOutOfScope, Reason: "名字不合规"}
	}
	return PFScanVerdict{}
}

func pfstItem(t *testing.T, res *PFScanResult, path string) PFScanItem {
	t.Helper()
	it, ok := res.Find(path)
	if !ok {
		var all []string
		for _, i := range res.Items {
			all = append(all, i.Path)
		}
		t.Fatalf("结果里没有 %q（有: %v）", path, all)
	}
	return it
}

func pfstWantState(t *testing.T, res *PFScanResult, path string, state PFState, code string) PFScanItem {
	t.Helper()
	it := pfstItem(t, res, path)
	if it.State != state || it.Code != code {
		t.Fatalf("%s: 状态 %s/%s（%s），想要 %s/%s", path, it.State, it.Code, it.Reason, state, code)
	}
	return it
}

func pfstPaths(res *PFScanResult) []string {
	var out []string
	for _, it := range res.Items {
		out = append(out, it.Path)
	}
	return out
}

// ---- 基本扫描 ----

func TestPFScan_Basic(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.txt", "hello")
	pfstWrite(t, root, "d1/b.txt", "world")
	pfstMkdir(t, root, "empty")
	deepDir := "n1/n2/n3/n4/n5/n6/n7/n8/n9/n10"
	pfstWrite(t, root, deepDir+"/deep.txt", "deep")

	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "d1", "d1/b.txt", "empty"}
	parts := strings.Split(deepDir, "/")
	for i := range parts {
		want = append(want, strings.Join(parts[:i+1], "/"))
	}
	want = append(want, deepDir+"/deep.txt")
	sort.Strings(want)
	got := pfstPaths(res)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("条目不对\n got  %v\n want %v", got, want)
	}
	for _, p := range []string{"a.txt", "d1/b.txt", deepDir + "/deep.txt"} {
		it := pfstWantState(t, res, p, PFStateOK, "")
		if it.Kind != PFKindFile || !it.HasStat || it.Stat.Ino == 0 || it.Stat.Dev == 0 || it.Stat.CtimeNs == 0 || it.Stat.MtimeNs == 0 {
			t.Errorf("%s: 字段不全 %+v", p, it)
		}
		if strings.Contains(p, "\\") {
			t.Errorf("%s: 路径不是 / 分隔", p)
		}
	}
	if it := pfstItem(t, res, "a.txt"); it.Hash != pfstSHA("hello") || it.Stat.Size != 5 {
		t.Errorf("a.txt: hash=%s size=%d", it.Hash, it.Stat.Size)
	}
	if it := pfstItem(t, res, deepDir+"/deep.txt"); it.Hash != pfstSHA("deep") {
		t.Errorf("deep.txt 哈希不对")
	}
	for _, d := range []string{"d1", "empty", deepDir} {
		if it := pfstWantState(t, res, d, PFStateOK, ""); it.Kind != PFKindDir {
			t.Errorf("%s 应是目录", d)
		}
	}
	s := res.Summary()
	if s.Files != 3 || s.HashedFiles != 3 || s.Dirs != 2+10 || s.Unknown != 0 || s.OutOfScope != 0 {
		t.Errorf("Summary 不对: %+v", s)
	}
	if s.Bytes != int64(len("hello")+len("world")+len("deep")) {
		t.Errorf("Bytes 不对: %d", s.Bytes)
	}
}

func TestPFScan_RootProblems(t *testing.T) {
	base := t.TempDir()
	pfstWrite(t, base, "real/f.txt", "x")
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "rootlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := PFScan(context.Background(), filepath.Join(base, "rootlink"), PFScanOptions{}); !errors.Is(err, ErrPFRootNotDir) {
		t.Errorf("根是符号链接应报 ErrPFRootNotDir，得到 %v", err)
	}
	if _, err := PFScan(context.Background(), filepath.Join(base, "real", "f.txt"), PFScanOptions{}); !errors.Is(err, ErrPFRootNotDir) {
		t.Errorf("根是文件应报 ErrPFRootNotDir，得到 %v", err)
	}
	if _, err := PFScan(context.Background(), filepath.Join(base, "nope"), PFScanOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("根不存在应报 ErrNotExist，得到 %v", err)
	}
	st := pfstStat(base, func(rel string, st *PFStat) error {
		if rel == "real" {
			st.Flags |= PFSFDataless
		}
		return nil
	})
	if _, err := PFScan(context.Background(), filepath.Join(base, "real"), PFScanOptions{Stat: st}); !errors.Is(err, ErrPFRootNotDir) {
		t.Errorf("根是占位目录应报 ErrPFRootNotDir，得到 %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PFScan(ctx, filepath.Join(base, "real"), PFScanOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("取消应返回 context.Canceled，得到 %v", err)
	}
}

// ---- 符号链接、特殊文件：超出范围，不进入 ----

func TestPFScan_SymlinksAndFIFO(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.txt", "A")
	pfstWrite(t, root, "d1/b.txt", "B")
	pfstWrite(t, root, "d1/inner/c.txt", "C")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink("a.txt", filepath.Join(root, "link-file")))
	must(os.Symlink("d1", filepath.Join(root, "link-dir")))
	must(os.Symlink("no-such-target", filepath.Join(root, "link-dangling")))
	must(os.Symlink("../d1", filepath.Join(root, "d1", "inner", "link-up")))
	must(syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600))

	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{})
	if err != nil {
		t.Fatal(err) // 若哈希阶段去 open 了 FIFO，这里会挂住；能返回就说明没碰它
	}
	for _, p := range []string{"link-file", "link-dir", "link-dangling", "d1/inner/link-up"} {
		it := pfstWantState(t, res, p, PFStateOutOfScope, PFCodeSymlink)
		if it.Kind != PFKindSymlink || it.Hash != "" {
			t.Errorf("%s: %+v", p, it)
		}
	}
	if it := pfstWantState(t, res, "pipe", PFStateOutOfScope, PFCodeSpecialFile); it.Kind != PFKindOther || !strings.Contains(it.Reason, "FIFO") {
		t.Errorf("pipe: %+v", it)
	}
	for _, p := range pfstPaths(res) {
		if strings.HasPrefix(p, "link-dir/") || strings.HasPrefix(p, "d1/inner/link-up/") {
			t.Errorf("进入了符号链接目录: %s", p)
		}
	}
	// 真实目录照常
	pfstWantState(t, res, "d1/inner/c.txt", PFStateOK, "")
	if anc, ok := res.SubtreeSkipped("link-dir/b.txt"); !ok || anc != "link-dir" {
		t.Errorf("SubtreeSkipped(link-dir/b.txt) = %q,%v", anc, ok)
	}
	if _, ok := res.SubtreeSkipped("d1/b.txt"); ok {
		t.Errorf("真实目录下的路径不该算子树跳过")
	}
}

// ---- 读不了的目录/文件：报出来，不当成「没有」 ----

func TestPFScan_UnreadableDir(t *testing.T) {
	pfstSkipIfRoot(t)
	root := t.TempDir()
	pfstWrite(t, root, "ok/a.txt", "A")
	pfstWrite(t, root, "secret/s1.txt", "S1")
	pfstWrite(t, root, "secret/sub/s2.txt", "S2")
	secret := filepath.Join(root, "secret")
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(secret, 0o755) })

	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	it := pfstWantState(t, res, "secret", PFStateOutOfScope, PFCodeUnreadable)
	if it.Kind != PFKindDir || it.Reason == "" {
		t.Errorf("secret: %+v", it)
	}
	pfstWantState(t, res, "ok/a.txt", PFStateOK, "")
	for _, p := range pfstPaths(res) {
		if strings.HasPrefix(p, "secret/") {
			t.Errorf("不该有 secret 下面的条目: %s", p)
		}
	}
	if anc, ok := res.SubtreeSkipped("secret/s1.txt"); !ok || anc != "secret" {
		t.Errorf("读不了的目录下的路径必须算「没看」: %q %v", anc, ok)
	}
	// 根下的读不了目录不影响整体：只有它一个超出范围
	if s := res.Summary(); s.OutOfScope != 1 {
		t.Errorf("Summary: %+v", s)
	}
}

func TestPFScan_UnreadableFile(t *testing.T) {
	pfstSkipIfRoot(t)
	root := t.TempDir()
	pfstWrite(t, root, "good.txt", "good")
	locked := pfstWrite(t, root, "locked.txt", "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o644) })

	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "locked.txt", PFStateOutOfScope, PFCodeUnreadable)
	if it := pfstWantState(t, res, "good.txt", PFStateOK, ""); it.Hash != pfstSHA("good") {
		t.Errorf("好文件应照常哈希")
	}
	if res.HashStats.NewUnknown != 1 || res.HashStats.Hashed != 1 {
		t.Errorf("HashStats: %+v", res.HashStats)
	}
}

func TestPFScan_StatErrors(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "deny.txt", "d")
	pfstWrite(t, root, "gone.txt", "g")
	pfstWrite(t, root, "fine.txt", "f")
	st := pfstStat(root, func(rel string, st *PFStat) error {
		switch rel {
		case "deny.txt":
			return &fs.PathError{Op: "lstat", Path: rel, Err: syscall.EACCES}
		case "gone.txt":
			return &fs.PathError{Op: "lstat", Path: rel, Err: syscall.ENOENT}
		}
		return nil
	})
	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{Stat: st})
	if err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "deny.txt", PFStateOutOfScope, PFCodeUnreadable)
	// 扫描时消失：不明，不是「没有」
	pfstWantState(t, res, "gone.txt", PFStateUnknown, PFCodeVanished)
	pfstWantState(t, res, "fine.txt", PFStateOK, "")
}

// ---- 过滤器：排除计数、超出范围、不进入 ----

func TestPFScan_Filter(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "doc.txt", "doc")
	pfstWrite(t, root, ".DS_Store", "junk")
	pfstWrite(t, root, ".cache/x.bin", "x")
	pfstWrite(t, root, "sub/.hidden", "h")
	pfstWrite(t, root, "sub/shown.txt", "s")
	pfstWrite(t, root, "Thing.app/Contents/Info.plist", "plist")

	type call struct {
		rel   string
		isDir bool
	}
	var mu sync.Mutex
	var calls []call
	var excluded []string
	opt := PFScanOptions{
		Filter: func(rel string, isDir bool) PFScanVerdict {
			mu.Lock()
			calls = append(calls, call{rel, isDir})
			mu.Unlock()
			return pfstFilter(rel, isDir)
		},
		OnExcluded: func(rel string, isDir bool) { excluded = append(excluded, fmt.Sprintf("%s:%v", rel, isDir)) },
	}
	res, err := PFScanAndHash(context.Background(), root, nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Excluded != 3 {
		t.Errorf("Excluded=%d，想要 3（.DS_Store、.cache 目录、sub/.hidden）", res.Excluded)
	}
	sort.Strings(excluded)
	if strings.Join(excluded, ",") != ".DS_Store:false,.cache:true,sub/.hidden:false" {
		t.Errorf("OnExcluded: %v", excluded)
	}
	app := pfstWantState(t, res, "Thing.app", PFStateOutOfScope, PFCodeFilter)
	if app.Kind != PFKindDir || app.Reason != "Mac 包目录" {
		t.Errorf("Thing.app: %+v", app)
	}
	want := []string{"Thing.app", "doc.txt", "sub", "sub/shown.txt"}
	if got := pfstPaths(res); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("条目 %v，想要 %v", got, want)
	}
	for _, c := range calls {
		if strings.HasPrefix(c.rel, ".cache/") || strings.HasPrefix(c.rel, "Thing.app/") {
			t.Errorf("被排除/超出范围的目录不该进入: %v", c)
		}
	}
	if anc, ok := res.SubtreeSkipped("Thing.app/Contents/Info.plist"); !ok || anc != "Thing.app" {
		t.Errorf("包目录下的路径应算子树跳过: %q %v", anc, ok)
	}
}

// ---- iCloud：.X.icloud 占位 ----

func TestPFScan_DotIcloudStub(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "docs/other.txt", "other")
	pfstWrite(t, root, "docs/.report.docx.icloud", "")    // 只剩占位，本地没有 report.docx
	pfstWrite(t, root, "docs/both.txt", "both")           // X 和占位同时在
	pfstWrite(t, root, "docs/.both.txt.icloud", "")       //
	pfstWrite(t, root, "docs/.DS_Store", "junk")          // 普通排除
	pfstWrite(t, root, "docs/sub/.nested.pdf.icloud", "") // 子目录里的占位
	pfstWrite(t, root, "docs/..hidden.icloud", "")      // 占位的 X（.hidden，本地没有）本身会被排除
	pfstWrite(t, root, "docs/.pack.bad.icloud", "")       // 占位的 X 被名字规则判超出范围
	opens := &pfstOpens{}
	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{Filter: pfstFilter, Open: opens.open})
	if err != nil {
		t.Fatal(err)
	}
	// 本地没有的 X：补出「不明」条目，不是「没有」
	it := pfstWantState(t, res, "docs/report.docx", PFStateUnknown, PFCodeICloudPlaceholder)
	if !it.Stub || it.HasStat || it.Kind != PFKindFile || it.Reason == "" {
		t.Errorf("report.docx: %+v", it)
	}
	pfstWantState(t, res, "docs/sub/nested.pdf", PFStateUnknown, PFCodeICloudPlaceholder)
	// X 也在：照样标不明，不读内容
	it = pfstWantState(t, res, "docs/both.txt", PFStateUnknown, PFCodeICloudPlaceholder)
	if it.Hash != "" || !it.Stub {
		t.Errorf("both.txt: %+v", it)
	}
	if opens.count("both.txt") != 0 {
		t.Errorf("读了有 .X.icloud 的文件")
	}
	pfstWantState(t, res, "docs/other.txt", PFStateOK, "")
	// 占位文件本身不单独成条目；X 被排除就只计数；X 被判超出范围就报超出范围
	for _, p := range pfstPaths(res) {
		if strings.HasSuffix(p, ".icloud") {
			t.Errorf("占位文件本身不该成条目: %s", p)
		}
	}
	if _, ok := res.Find("docs/.DS_Store"); ok {
		t.Errorf("被排除的不该成条目")
	}
	if res.Excluded != 2 {
		t.Errorf("Excluded=%d，想要 2（.DS_Store 和 ..hidden.icloud 的 X）", res.Excluded)
	}
	pfstWantState(t, res, "docs/pack.bad", PFStateOutOfScope, PFCodeFilter)
	if s := res.Summary(); s.UnknownPlaceholder != 3 {
		t.Errorf("Summary: %+v", s)
	}
}

// ---- iCloud：SF_DATALESS（注入 stat 模拟） ----

func TestPFScan_DatalessNeverRead(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.bin", "AAAA")
	pfstWrite(t, root, "b.bin", "BBBB")
	pfstWrite(t, root, "cloud/inner.txt", "inner")
	dataless := map[string]bool{"a.bin": true, "cloud": true}
	st := pfstStat(root, func(rel string, st *PFStat) error {
		if dataless[rel] {
			st.Flags |= PFSFDataless
		}
		return nil
	})

	// 没有缓存：占位文件不读、标不明；条目还在（绝不是「没有」）
	opens := &pfstOpens{}
	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{Stat: st, Open: opens.open})
	if err != nil {
		t.Fatal(err)
	}
	it := pfstWantState(t, res, "a.bin", PFStateUnknown, PFCodeICloudPlaceholder)
	if it.Hash != "" || !it.HasStat || !it.Stat.Dataless() {
		t.Errorf("a.bin: %+v", it)
	}
	if opens.count("a.bin") != 0 {
		t.Fatalf("读了 iCloud 占位文件的内容")
	}
	pfstWantState(t, res, "b.bin", PFStateOK, "")
	if opens.count("b.bin") != 1 {
		t.Errorf("b.bin 应读一次")
	}
	// 占位目录：不进入
	pfstWantState(t, res, "cloud", PFStateUnknown, PFCodeICloudPlaceholder)
	if _, ok := res.Find("cloud/inner.txt"); ok {
		t.Errorf("进入了占位目录")
	}
	if anc, ok := res.SubtreeSkipped("cloud/inner.txt"); !ok || anc != "cloud" {
		t.Errorf("占位目录下的路径应算「没看」: %q %v", anc, ok)
	}
}

func TestPFScan_DatalessHitsCache(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.bin", "AAAA")
	pfstWrite(t, root, "b.bin", "BBBB")
	cache := NewPFHashCache()
	// 第一轮：文件在本地，正常算哈希进缓存
	if _, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if cache.Len() != 2 {
		t.Fatalf("缓存应有 2 条，实际 %d", cache.Len())
	}
	// 第二轮：a.bin 被驱逐成占位——iCloud 驱逐只改 ctime 和 flags，size/mtime/ino 不变
	evicted := pfstStat(root, func(rel string, st *PFStat) error {
		if rel == "a.bin" {
			st.Flags |= PFSFDataless
			st.CtimeNs += 5e9
		}
		return nil
	})
	opens := &pfstOpens{}
	res, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{Stat: evicted, Open: opens.open})
	if err != nil {
		t.Fatal(err)
	}
	it := pfstWantState(t, res, "a.bin", PFStateOK, "")
	if it.Hash != pfstSHA("AAAA") || !it.FromCache {
		t.Errorf("驱逐后的占位文件应命中缓存用缓存哈希: %+v", it)
	}
	if opens.total() != 0 {
		t.Errorf("全部命中缓存，不该读任何文件，实际读了 %v", opens.paths)
	}
	if res.HashStats.CacheHits != 2 || res.HashStats.Hashed != 0 {
		t.Errorf("HashStats: %+v", res.HashStats)
	}

	// 第三轮：占位文件的 mtime 变了（云端别处改过）→ 对不上缓存 → 不明，仍不读
	changed := pfstStat(root, func(rel string, st *PFStat) error {
		if rel == "a.bin" {
			st.Flags |= PFSFDataless
			st.MtimeNs -= 1e9
		}
		return nil
	})
	opens = &pfstOpens{}
	res, err = PFScanAndHash(context.Background(), root, cache, PFScanOptions{Stat: changed, Open: opens.open})
	if err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "a.bin", PFStateUnknown, PFCodeICloudPlaceholder)
	if opens.count("a.bin") != 0 {
		t.Errorf("对不上缓存的占位文件也不能读")
	}
	// size 变了、ino 变了同理
	for name, f := range map[string]func(*PFStat){"size": func(s *PFStat) { s.Size++ }, "ino": func(s *PFStat) { s.Ino++ }} {
		f := f
		stx := pfstStat(root, func(rel string, st *PFStat) error {
			if rel == "a.bin" {
				st.Flags |= PFSFDataless
				f(st)
			}
			return nil
		})
		res, err = PFScanAndHash(context.Background(), root, cache, PFScanOptions{Stat: stx})
		if err != nil {
			t.Fatal(err)
		}
		if it := pfstItem(t, res, "a.bin"); it.State != PFStateUnknown || it.Hash != "" {
			t.Errorf("%s 变了不该命中: %+v", name, it)
		}
	}
}

// 扫描时在本地、哈希前被驱逐：重新 stat 发现是占位，不读。
func TestPFHash_EvictedAfterScan(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.bin", "AAAA")
	res, err := PFScan(context.Background(), root, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "a.bin", PFStateOK, "")
	evicted := pfstStat(root, func(rel string, st *PFStat) error {
		if rel == "a.bin" {
			st.Flags |= PFSFDataless
			st.CtimeNs++
		}
		return nil
	})
	opens := &pfstOpens{}
	if err := PFHashItems(context.Background(), root, res, nil, PFScanOptions{Stat: evicted, Open: opens.open}); err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "a.bin", PFStateUnknown, PFCodeICloudPlaceholder)
	if opens.total() != 0 {
		t.Errorf("读了被驱逐的文件")
	}
}

// ---- 静止判断 ----

func TestPFIsQuiet(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	ns := func(d time.Duration) int64 { return now.Add(-d).UnixNano() }
	cases := []struct {
		name  string
		mtime int64
		quiet int
		want  bool
	}{
		{"整好 10 秒", ns(10 * time.Second), 10, true},
		{"差 1 纳秒", ns(10*time.Second) + 1, 10, false},
		{"刚写完", ns(0), 10, false},
		{"一小时前", ns(time.Hour), 10, true},
		{"未来", ns(-time.Minute), 10, false},
		{"quiet=0 取缺省 10", ns(9 * time.Second), 0, false},
		{"quiet=0 取缺省 10（够了）", ns(10 * time.Second), 0, true},
		{"quiet=30", ns(20 * time.Second), 30, false},
	}
	for _, c := range cases {
		if got := PFIsQuiet(c.mtime, now, c.quiet); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestPFScan_NotQuiet(t *testing.T) {
	root := t.TempDir()
	p := pfstWrite(t, root, "f.txt", "x")
	t0 := time.Unix(2_000_000, 0)
	if err := os.Chtimes(p, t0, t0); err != nil {
		t.Fatal(err)
	}
	scan := func(now time.Time) *PFScanResult {
		res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{Now: func() time.Time { return now }, QuietSeconds: 10})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := scan(t0.Add(9 * time.Second))
	it := pfstWantState(t, res, "f.txt", PFStateUnknown, PFCodeNotQuiet)
	if it.Hash != "" || res.Summary().UnknownNotQuiet != 1 {
		t.Errorf("未静止的文件不该算哈希: %+v", it)
	}
	pfstWantState(t, scan(t0.Add(10*time.Second)), "f.txt", PFStateOK, "")
	// 时钟比文件还早（mtime 在未来）
	it = pfstWantState(t, scan(t0.Add(-time.Hour)), "f.txt", PFStateUnknown, PFCodeNotQuiet)
	if !strings.Contains(it.Reason, "晚于当前时间") {
		t.Errorf("未来 mtime 的原因应说清: %s", it.Reason)
	}
}

// 正在被写的文件：另起 goroutine 持续写。
func TestPFScan_FileBeingWritten(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "calm.txt", "calm")
	live := filepath.Join(root, "live.log")
	f, err := os.OpenFile(live, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	stop, done, started := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer f.Close()
		first := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.WriteString("x")
			if first {
				close(started)
				first = false
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	defer func() { close(stop); <-done }()
	<-started

	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if it := pfstWantState(t, res, "live.log", PFStateUnknown, PFCodeNotQuiet); it.Hash != "" {
		t.Errorf("正在写的文件不该有哈希")
	}
	pfstWantState(t, res, "calm.txt", PFStateOK, "")
}

// 扫描时静止、哈希前被写（goroutine 持续写）：重新 stat 发现变了，不明，不入缓存。
func TestPFHash_WrittenAfterScan(t *testing.T) {
	root := t.TempDir()
	big := pfstWrite(t, root, "big.bin", strings.Repeat("0123456789", 100000))
	res, err := PFScan(context.Background(), root, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "big.bin", PFStateOK, "")

	f, err := os.OpenFile(big, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stop, done, started := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer f.Close()
		first := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.WriteAt([]byte("Z"), 0)
			if first {
				close(started)
				first = false
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() { close(stop); <-done }()
	<-started

	cache := NewPFHashCache()
	if err := PFHashItems(context.Background(), root, res, cache, PFScanOptions{}); err != nil {
		t.Fatal(err)
	}
	it := pfstItem(t, res, "big.bin")
	if it.State != PFStateUnknown || it.Hash != "" || (it.Code != PFCodeChangedSinceScan && it.Code != PFCodeChangedWhileRead) {
		t.Errorf("被写的文件应是不明: %+v", it)
	}
	if cache.Len() != 0 {
		t.Errorf("不明的文件不能入缓存")
	}
}

// ---- 指纹与两次扫描比较 ----

func TestPFScan_FingerprintAndStable(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.txt", "aaa")
	pfstWrite(t, root, "d/b.txt", "bbb")
	opt := PFScanOptions{Filter: pfstFilter}
	scan := func() *PFScanResult {
		res, err := PFScan(context.Background(), root, opt)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	s1, s2 := scan(), scan()
	if s1.Fingerprint() != s2.Fingerprint() || s1.QuietFingerprint() != s2.QuietFingerprint() {
		t.Fatalf("没变化指纹应相同")
	}
	if ok, diffs := PFScansStable(s1, s2); !ok || len(diffs) != 0 {
		t.Fatalf("没变化应稳定: %v", diffs)
	}

	// 排除的文件（.DS_Store）出现，不影响指纹（目录的 mtime/ctime 不入指纹）
	pfstWrite(t, root, ".DS_Store", "junk")
	pfstWrite(t, root, "d/.DS_Store", "junk")
	s3 := scan()
	if s1.Fingerprint() != s3.Fingerprint() {
		t.Errorf("排除文件出现不该改变指纹")
	}
	if ok, _ := PFScansStable(s1, s3); !ok {
		t.Errorf("排除文件出现不该算不稳定")
	}

	// 改内容（size + mtime）
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	s4 := scan()
	if s1.Fingerprint() == s4.Fingerprint() || s1.QuietFingerprint() == s4.QuietFingerprint() {
		t.Errorf("改了文件指纹应变")
	}
	ok, diffs := PFScansStable(s1, s4)
	if ok || len(diffs) != 1 || diffs[0] != (PFScanDiff{"a.txt", "变动"}) {
		t.Errorf("diffs=%v", diffs)
	}
	// 新增、消失
	pfstWrite(t, root, "new.txt", "n")
	if err := os.Remove(filepath.Join(root, "d", "b.txt")); err != nil {
		t.Fatal(err)
	}
	ok, diffs = PFScansStable(s1, scan())
	got := fmt.Sprint(diffs)
	if ok || !strings.Contains(got, "{a.txt 变动}") || !strings.Contains(got, "{d/b.txt 消失}") || !strings.Contains(got, "{new.txt 新增}") {
		t.Errorf("diffs=%v", diffs)
	}
}

// 指纹逐字段：size、mtime、ctime、ino 各变一项都要变；dev、flags、mode（文件）不入指纹。
func TestPFScan_FingerprintFields(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.txt", "aaa")
	fp := func(mod func(*PFStat)) (string, string) {
		st := pfstStat(root, func(rel string, st *PFStat) error {
			if rel == "a.txt" && mod != nil {
				mod(st)
			}
			return nil
		})
		res, err := PFScan(context.Background(), root, PFScanOptions{Stat: st})
		if err != nil {
			t.Fatal(err)
		}
		return res.Fingerprint(), res.QuietFingerprint()
	}
	base, baseQ := fp(nil)
	for name, f := range map[string]func(*PFStat){
		"size":  func(s *PFStat) { s.Size++ },
		"mtime": func(s *PFStat) { s.MtimeNs -= 1e9 },
		"ino":   func(s *PFStat) { s.Ino++ },
	} {
		if a, q := fp(f); a == base || q == baseQ {
			t.Errorf("%s 变了指纹应变", name)
		}
	}
	// ctime 只入单元指纹，不入「静止比较」指纹（§4.2 第 8 条是 size、mtime_ns、ino）
	if a, q := fp(func(s *PFStat) { s.CtimeNs++ }); a == base || q != baseQ {
		t.Errorf("ctime 变化：单元指纹应变、静止指纹不变")
	}
	if a, q := fp(func(s *PFStat) { s.Dev++ }); a != base || q != baseQ {
		t.Errorf("dev 不入指纹")
	}
	// 占位标记变化也不入指纹（驱逐会同时改 ctime，那一项会让单元指纹变，这是预期内的多扫一轮）
	if _, q := fp(func(s *PFStat) { s.Flags |= PFSFDataless }); q != baseQ {
		t.Errorf("flags 不入静止指纹")
	}
}

// ---- 规模 ----

func TestPFScan_Scale4000(t *testing.T) {
	root := t.TempDir()
	const dirs, perDir = 40, 100
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, fmt.Sprintf("proj%02d", d), "sub")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < perDir; i++ {
			p := filepath.Join(dir, fmt.Sprintf("file%03d.txt", i))
			if err := os.WriteFile(p, []byte(fmt.Sprintf("content of %d/%d, padded %s", d, i, strings.Repeat("x", 80))), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 一次性把 mtime 调到一小时前，让它们都已静止
	old := time.Now().Add(-time.Hour)
	filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			os.Chtimes(p, old, old)
		}
		return nil
	})

	cache := NewPFHashCache()
	ctx := context.Background()
	t0 := time.Now()
	res, err := PFScan(ctx, root, PFScanOptions{Filter: pfstFilter})
	if err != nil {
		t.Fatal(err)
	}
	scanDur := time.Since(t0)
	fp1 := res.Fingerprint()
	t1 := time.Now()
	if err := PFHashItems(ctx, root, res, cache, PFScanOptions{Filter: pfstFilter}); err != nil {
		t.Fatal(err)
	}
	coldHash := time.Since(t1)
	s := res.Summary()
	if s.Files != dirs*perDir || s.HashedFiles != dirs*perDir || s.Unknown != 0 {
		t.Fatalf("Summary: %+v", s)
	}
	if res.HashStats.Hashed != dirs*perDir || res.HashStats.CacheHits != 0 {
		t.Errorf("冷哈希 HashStats: %+v", res.HashStats)
	}

	opens := &pfstOpens{}
	t2 := time.Now()
	res2, err := PFScanAndHash(ctx, root, cache, PFScanOptions{Filter: pfstFilter, Open: opens.open})
	if err != nil {
		t.Fatal(err)
	}
	warm := time.Since(t2)
	if res2.HashStats.CacheHits != dirs*perDir || res2.HashStats.Hashed != 0 || opens.total() != 0 {
		t.Errorf("热缓存应 100%% 命中、不读文件: %+v 读了 %d 个", res2.HashStats, opens.total())
	}
	if res2.Fingerprint() != fp1 {
		t.Errorf("没变化指纹应相同")
	}
	t.Logf("规模 %d 个文件 + %d 个目录：扫描 %v；首次哈希（冷）%v；再扫一次含全部命中缓存 %v", dirs*perDir, s.Dirs, scanDur, coldHash, warm)
}
