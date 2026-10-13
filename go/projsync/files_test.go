package projsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sha256Hex 算小写十六进制 SHA-256。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeFile 在 root 下按 / 分隔的相对路径写文件，mtime 非零时设修改时间。
func writeFile(t *testing.T, root, path, content string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// mustRead 读 root 下的相对路径文件全文。
func mustRead(t *testing.T, root, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

func TestValidRel(t *testing.T) {
	valid := []string{"a", "a/b", "a/b/c.jsonl", "x-y.z", "docs/README.md", "a/..b"}
	for _, p := range valid {
		if err := validRel(p); err != nil {
			t.Errorf("validRel(%q) 应合法，得 %v", p, err)
		}
	}
	// "..b" 只是普通文件名，不是 .. 段，合法；下面的才是非法的
	invalid := []string{"", "../x", "a//b", "/abs", "a/./b", `a\b`, "a/b/", ".", "..", "a/b/..", "./a"}
	for _, p := range invalid {
		if err := validRel(p); err == nil {
			t.Errorf("validRel(%q) 应报错", p)
		}
	}
}

func TestLocalFSList(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "A", time.Time{})
	writeFile(t, root, "a/b/c.jsonl", "C", time.Time{})
	writeFile(t, root, "realdir/in.txt", "IN", time.Time{})
	// 符号链接（文件和目录）都要跳过，且不能跟进目录链接
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "realdir"), filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	// 临时文件要跳过
	writeFile(t, root, ".projsync-tmp-123", "TMP", time.Time{})

	entries, err := (&LocalFS{Root: root}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, e := range entries {
		got[e.Path] = e
	}
	want := map[string]string{
		"a.txt":          "A",
		"a/b/c.jsonl":    "C",
		"realdir/in.txt": "IN",
	}
	if len(got) != len(want) {
		t.Fatalf("List 返回 %d 个条目 %v，期望 %d 个 %v", len(got), got, len(want), want)
	}
	for p, content := range want {
		e, ok := got[p]
		if !ok {
			t.Errorf("List 缺少 %q", p)
			continue
		}
		if e.Size != int64(len(content)) {
			t.Errorf("%q Size = %d，期望 %d", p, e.Size, len(content))
		}
	}
	if _, ok := got["link.txt"]; ok {
		t.Error("List 不应包含符号链接文件")
	}
	if _, ok := got[".projsync-tmp-123"]; ok {
		t.Error("List 不应包含临时文件")
	}
}

func TestLocalFSListRootMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "不存在")
	entries, err := (&LocalFS{Root: root}).List(context.Background())
	if err != nil {
		t.Fatalf("根目录不存在不应报错，得 %v", err)
	}
	if entries != nil {
		t.Errorf("根目录不存在应返回 nil，得 %v", entries)
	}
}

func TestLocalFSHash(t *testing.T) {
	root := t.TempDir()
	content := "hello world"
	writeFile(t, root, "f.txt", content, time.Time{})
	fs := &LocalFS{Root: root}
	ctx := context.Background()

	if got, err := fs.Hash(ctx, "f.txt", 5); err != nil {
		t.Fatal(err)
	} else if got != sha256Hex([]byte("hello")) {
		t.Errorf("前 5 字节哈希 = %q，期望 %q", got, sha256Hex([]byte("hello")))
	}
	if got, err := fs.Hash(ctx, "f.txt", int64(len(content))); err != nil {
		t.Fatal(err)
	} else if got != sha256Hex([]byte(content)) {
		t.Errorf("全文件哈希 = %q，期望 %q", got, sha256Hex([]byte(content)))
	}
	if got, err := fs.Hash(ctx, "f.txt", 0); err != nil {
		t.Fatal(err)
	} else if got != sha256Hex(nil) {
		t.Errorf("n==0 时应返回空串哈希，得 %q", got)
	}
	if _, err := fs.Hash(ctx, "f.txt", int64(len(content))+1); err == nil {
		t.Error("文件不足 n 字节应报错")
	}
	if _, err := fs.Hash(ctx, "../f.txt", 1); err == nil {
		t.Error("越界路径应报错")
	}
}

func TestLocalFSRead(t *testing.T) {
	root := t.TempDir()
	content := "0123456789"
	writeFile(t, root, "f.txt", content, time.Time{})
	fs := &LocalFS{Root: root}
	ctx := context.Background()

	// 只读前 n 字节
	r, err := fs.Read(ctx, "f.txt", 4)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatalf("读足 n 字节不应报错: %v", err)
	}
	if string(b) != "0123" {
		t.Errorf("读前 4 字节 = %q，期望 %q", b, "0123")
	}

	// 文件不足 n 字节：读到末尾报 io.ErrUnexpectedEOF
	r, err = fs.Read(ctx, "f.txt", 15)
	if err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(r)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("不足 n 字节应报 ErrUnexpectedEOF，得 %v", err)
	}
	if string(b) != content {
		t.Errorf("读到末尾前应有全部内容，得 %q", b)
	}
}

func TestLocalFSWrite(t *testing.T) {
	root := t.TempDir()
	fs := &LocalFS{Root: root}
	ctx := context.Background()
	mtime := time.Date(2024, 3, 1, 10, 30, 0, 0, time.UTC)

	// 建父目录、写入
	if err := fs.Write(ctx, "a/b/c.jsonl", strings.NewReader("内容"), mtime); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(root, "a/b/c.jsonl")
	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("文件权限 = %v，期望 0600", st.Mode().Perm())
	}
	if !st.ModTime().Equal(mtime) {
		t.Errorf("修改时间 = %v，期望 %v", st.ModTime(), mtime)
	}
	if got := mustRead(t, root, "a/b/c.jsonl"); got != "内容" {
		t.Errorf("内容 = %q，期望 %q", got, "内容")
	}
	entries, err := fs.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Path, ".projsync-tmp-") {
			t.Errorf("残留临时文件 %s", e.Path)
		}
	}

	// 覆盖已有文件
	if err := fs.Write(ctx, "a/b/c.jsonl", strings.NewReader("新内容"), mtime); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, root, "a/b/c.jsonl"); got != "新内容" {
		t.Errorf("覆盖后内容 = %q，期望 %q", got, "新内容")
	}

	// 越界路径报错且不落地
	if err := fs.Write(ctx, "../x", strings.NewReader("x"), mtime); err == nil {
		t.Error("越界路径应报错")
	}
	if _, err := os.Stat(filepath.Join(root, "..", "x")); err == nil {
		t.Error("越界写入不应产生文件")
	}
}

// failWriteFS 包装一个 FS，让指定路径的 Write 报错。
type failWriteFS struct {
	FS
	failPath string
}

func (f *failWriteFS) Write(ctx context.Context, path string, r io.Reader, mtime time.Time) error {
	if path == f.failPath {
		return errors.New("模拟写失败")
	}
	return f.FS.Write(ctx, path, r, mtime)
}

func TestSyncDirsOneSideEach(t *testing.T) {
	// 只本机有（子目录 a/b/c.jsonl）→ 上传；只远端有 → 下载
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	t0 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	writeFile(t, localRoot, "a/b/c.jsonl", "本地子目录", t0)
	writeFile(t, remoteRoot, "top.txt", "远端文件", t0)

	res, err := SyncDirs(context.Background(), &LocalFS{Root: localRoot}, &LocalFS{Root: remoteRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Uploaded) != 1 || res.Uploaded[0] != "a/b/c.jsonl" {
		t.Errorf("Uploaded = %v，期望 [a/b/c.jsonl]", res.Uploaded)
	}
	if len(res.Downloaded) != 1 || res.Downloaded[0] != "top.txt" {
		t.Errorf("Downloaded = %v，期望 [top.txt]", res.Downloaded)
	}
	if got := mustRead(t, remoteRoot, "a/b/c.jsonl"); got != "本地子目录" {
		t.Errorf("远端 a/b/c.jsonl = %q", got)
	}
	if got := mustRead(t, localRoot, "top.txt"); got != "远端文件" {
		t.Errorf("本机 top.txt = %q", got)
	}
	// 修改时间保留源文件的
	st, err := os.Stat(filepath.Join(localRoot, "top.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(t0) {
		t.Errorf("下载的修改时间 = %v，期望 %v", st.ModTime(), t0)
	}
	if len(res.Conflicts) != 0 || len(res.Errors) != 0 || res.Same != 0 {
		t.Errorf("不应有冲突/错误/Same: %+v", res)
	}
}

func TestSyncDirsSame(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	t0 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	writeFile(t, localRoot, "same.txt", "一样", t0)
	writeFile(t, remoteRoot, "same.txt", "一样", t0)

	res, err := SyncDirs(context.Background(), &LocalFS{Root: localRoot}, &LocalFS{Root: remoteRoot})
	if err != nil {
		t.Fatal(err)
	}
	if res.Same != 1 {
		t.Errorf("Same = %d，期望 1", res.Same)
	}
	if len(res.Uploaded) != 0 || len(res.Downloaded) != 0 || len(res.Conflicts) != 0 || len(res.Errors) != 0 {
		t.Errorf("相同文件只应计 Same: %+v", res)
	}
}

func TestSyncDirsLocalPrefixOfRemote(t *testing.T) {
	// 本机是远端前缀（会话只追加）→ 用远端覆盖本机，记 Downloaded
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	tLocal := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	tRemote := time.Date(2024, 6, 7, 8, 9, 10, 0, time.UTC)
	writeFile(t, localRoot, "s.jsonl", "abc", tLocal)
	writeFile(t, remoteRoot, "s.jsonl", "abcdef", tRemote)

	res, err := SyncDirs(context.Background(), &LocalFS{Root: localRoot}, &LocalFS{Root: remoteRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Downloaded) != 1 || res.Downloaded[0] != "s.jsonl" {
		t.Errorf("Downloaded = %v，期望 [s.jsonl]", res.Downloaded)
	}
	if len(res.Uploaded) != 0 || res.Same != 0 || len(res.Conflicts) != 0 || len(res.Errors) != 0 {
		t.Errorf("前缀覆盖只应记 Downloaded: %+v", res)
	}
	if got := mustRead(t, localRoot, "s.jsonl"); got != "abcdef" {
		t.Errorf("本机应被远端覆盖，得 %q", got)
	}
	if got := mustRead(t, remoteRoot, "s.jsonl"); got != "abcdef" {
		t.Errorf("远端不应变，得 %q", got)
	}
	st, err := os.Stat(filepath.Join(localRoot, "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(tRemote) {
		t.Errorf("本机修改时间应取远端的 %v，得 %v", tRemote, st.ModTime())
	}
}

func TestSyncDirsRemotePrefixOfLocal(t *testing.T) {
	// 远端是本机前缀 → 用本机覆盖远端，记 Uploaded
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	tLocal := time.Date(2024, 6, 7, 8, 9, 10, 0, time.UTC)
	writeFile(t, localRoot, "s.jsonl", "abcdef", tLocal)
	writeFile(t, remoteRoot, "s.jsonl", "abc", time.Time{})

	res, err := SyncDirs(context.Background(), &LocalFS{Root: localRoot}, &LocalFS{Root: remoteRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Uploaded) != 1 || res.Uploaded[0] != "s.jsonl" {
		t.Errorf("Uploaded = %v，期望 [s.jsonl]", res.Uploaded)
	}
	if len(res.Downloaded) != 0 || res.Same != 0 || len(res.Conflicts) != 0 || len(res.Errors) != 0 {
		t.Errorf("前缀覆盖只应记 Uploaded: %+v", res)
	}
	if got := mustRead(t, remoteRoot, "s.jsonl"); got != "abcdef" {
		t.Errorf("远端应被本机覆盖，得 %q", got)
	}
	if got := mustRead(t, localRoot, "s.jsonl"); got != "abcdef" {
		t.Errorf("本机不应变，得 %q", got)
	}
}

func TestSyncDirsConflictSameSize(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	writeFile(t, localRoot, "s.txt", "aaa", time.Time{})
	writeFile(t, remoteRoot, "s.txt", "bbb", time.Time{})

	res, err := SyncDirs(context.Background(), &LocalFS{Root: localRoot}, &LocalFS{Root: remoteRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("Conflicts = %v，期望 1 个", res.Conflicts)
	}
	c := res.Conflicts[0]
	if c.Path != "s.txt" || c.LocalSize != 3 || c.RemoteSize != 3 {
		t.Errorf("冲突条目 = %+v", c)
	}
	if c.Reason != "两边大小相同但内容不同" {
		t.Errorf("Reason = %q", c.Reason)
	}
	if len(res.Uploaded) != 0 || len(res.Downloaded) != 0 || res.Same != 0 || len(res.Errors) != 0 {
		t.Errorf("冲突不应改动任何一边: %+v", res)
	}
	if got := mustRead(t, localRoot, "s.txt"); got != "aaa" {
		t.Errorf("本机不应变，得 %q", got)
	}
	if got := mustRead(t, remoteRoot, "s.txt"); got != "bbb" {
		t.Errorf("远端不应变，得 %q", got)
	}
}

func TestSyncDirsConflictDifferentSize(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	writeFile(t, localRoot, "s.txt", "abcdef", time.Time{})
	writeFile(t, remoteRoot, "s.txt", "abce", time.Time{}) // 不同长且不是前缀

	res, err := SyncDirs(context.Background(), &LocalFS{Root: localRoot}, &LocalFS{Root: remoteRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("Conflicts = %v，期望 1 个", res.Conflicts)
	}
	if c := res.Conflicts[0]; c.Reason != "两边都有对方没有的改动" {
		t.Errorf("Reason = %q", c.Reason)
	}
	if len(res.Uploaded) != 0 || len(res.Downloaded) != 0 || res.Same != 0 || len(res.Errors) != 0 {
		t.Errorf("冲突不应改动任何一边: %+v", res)
	}
	if got := mustRead(t, localRoot, "s.txt"); got != "abcdef" {
		t.Errorf("本机不应变，得 %q", got)
	}
	if got := mustRead(t, remoteRoot, "s.txt"); got != "abce" {
		t.Errorf("远端不应变，得 %q", got)
	}
}

func TestSyncDirsSecondRunAllSame(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	t0 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	writeFile(t, localRoot, "a/b/c.jsonl", "hello", t0)
	writeFile(t, remoteRoot, "d.txt", "world", t0)
	local := &LocalFS{Root: localRoot}
	remote := &LocalFS{Root: remoteRoot}

	first, err := SyncDirs(context.Background(), local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Uploaded) != 1 || first.Uploaded[0] != "a/b/c.jsonl" || len(first.Downloaded) != 1 || first.Downloaded[0] != "d.txt" {
		t.Fatalf("第一次 = %+v", first)
	}

	second, err := SyncDirs(context.Background(), local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if second.Same != 2 {
		t.Errorf("第二次 Same = %d，期望 2", second.Same)
	}
	if len(second.Uploaded) != 0 || len(second.Downloaded) != 0 || len(second.Conflicts) != 0 || len(second.Errors) != 0 {
		t.Errorf("第二次不应再改动: %+v", second)
	}
}

func TestSyncDirsWriteError(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	writeFile(t, localRoot, "a.txt", "甲", time.Time{})
	writeFile(t, localRoot, "x.txt", "乙", time.Time{})
	remote := &failWriteFS{FS: &LocalFS{Root: remoteRoot}, failPath: "x.txt"}

	res, err := SyncDirs(context.Background(), &LocalFS{Root: localRoot}, remote)
	if err != nil {
		t.Fatal(err)
	}
	// a.txt 照常上传，x.txt 的失败记入 Errors，不中断整体
	if len(res.Uploaded) != 1 || res.Uploaded[0] != "a.txt" {
		t.Errorf("Uploaded = %v，期望 [a.txt]", res.Uploaded)
	}
	if len(res.Errors) != 1 || !strings.HasPrefix(res.Errors[0], "x.txt: ") {
		t.Errorf("Errors = %v，期望 1 条 \"x.txt: ...\"", res.Errors)
	}
	if got := mustRead(t, remoteRoot, "a.txt"); got != "甲" {
		t.Errorf("远端 a.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "x.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("写失败的 x.txt 不应出现在远端")
	}
}

func TestSyncDirsEmptyJSON(t *testing.T) {
	res, err := SyncDirs(context.Background(), &LocalFS{Root: t.TempDir()}, &LocalFS{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"uploaded":[],"downloaded":[],"same":0,"conflicts":[],"errors":[]}`
	if string(b) != want {
		t.Errorf("JSON = %s，期望 %s", b, want)
	}
}

func TestSyncDirsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := SyncDirs(ctx, &LocalFS{Root: t.TempDir()}, &LocalFS{Root: t.TempDir()})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("已取消的 ctx 应返回 context.Canceled，得 %v", err)
	}
}
