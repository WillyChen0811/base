package projsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

// setupSFTPFS 用 net.Pipe 把 sftp.NewServer 和 sftp.NewClientPipe 接起来；服务端根目录是 t.TempDir。
func setupSFTPFS(t *testing.T) (*SFTPFS, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	sc, cc := net.Pipe()
	srv, err := sftp.NewServer(sc, sftp.WithServerWorkingDirectory(tmpDir))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	served := make(chan struct{})
	go func() { srv.Serve(); close(served) }()
	client, err := sftp.NewClientPipe(cc, cc)
	if err != nil {
		t.Fatalf("NewClientPipe: %v", err)
	}
	fs := &SFTPFS{Client: client, Root: tmpDir, Run: runLocal(tmpDir)}
	return fs, func() {
		client.Close()
		sc.Close()
		<-served
	}
}

// runLocal 返回一个闭包，在 tmpDir 下用 sh -c 执行命令。
func runLocal(tmpDir string) func(ctx context.Context, cmd string) ([]byte, error) {
	return func(ctx context.Context, cmd string) ([]byte, error) {
		ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmdExec := exec.CommandContext(ctx2, "sh", "-c", cmd)
		cmdExec.Dir = tmpDir
		return cmdExec.CombinedOutput()
	}
}

// ── List ────────────────────────────────────────────────

func TestListNotExist(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	entries, err := fs.List(context.Background())
	if err != nil {
		t.Fatalf("List 应该返回 nil, nil 而不是报错: %v", err)
	}
	if entries != nil {
		t.Errorf("根不存在应返回 nil，得到 %v", entries)
	}
}

func TestListWithFiles(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	tmpDir := fs.Root
	os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("world!!!"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "sub"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "sub", "c.txt"), []byte("nested"), 0644)
	// 临时文件应被跳过
	os.WriteFile(filepath.Join(tmpDir, ".projsync-tmp-abcdef"), []byte("tmp"), 0644)
	// 符号链接应被跳过
	os.Symlink("a.txt", filepath.Join(tmpDir, "link.txt"))

	entries, err := fs.List(context.Background())
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("应列出 3 个文件（a.txt, b.txt, sub/c.txt），得到 %d 个: %v", len(entries), entries)
	}
	got := map[string]int64{}
	for _, e := range entries {
		got[e.Path] = e.Size
	}
	want := map[string]int64{"a.txt": 5, "b.txt": 8, "sub/c.txt": 6}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v，预期 %v", got, want)
	}
}

// ── Hash ────────────────────────────────────────────────

func TestHash(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	tmpDir := fs.Root
	data := []byte("hello, world!")
	os.WriteFile(filepath.Join(tmpDir, "hash.txt"), data, 0644)

	n := int64(len(data))
	hash, err := fs.Hash(context.Background(), "hash.txt", n)
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	expected := sha256.Sum256(data)
	expectedHex := hex.EncodeToString(expected[:])
	if hash != expectedHex {
		t.Errorf("hash = %q，预期 %q", hash, expectedHex)
	}

	half := n / 2
	hashHalf, err := fs.Hash(context.Background(), "hash.txt", half)
	if err != nil {
		t.Fatalf("Hash half 失败: %v", err)
	}
	expectedHalf := sha256.Sum256(data[:half])
	expectedHalfHex := hex.EncodeToString(expectedHalf[:])
	if hashHalf != expectedHalfHex {
		t.Errorf("half hash = %q，预期 %q", hashHalf, expectedHalfHex)
	}
}

func TestHashFileTooShort(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	tmpDir := fs.Root
	os.WriteFile(filepath.Join(tmpDir, "small.txt"), []byte("hi"), 0644)

	_, err := fs.Hash(context.Background(), "small.txt", 100)
	if err == nil {
		t.Fatal("文件不足 n 应报错")
	}
	if !strings.Contains(err.Error(), "不足") {
		t.Errorf("错误消息应提到不足: %v", err)
	}
}

func TestHashZeroN(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	hash, err := fs.Hash(context.Background(), "any.txt", 0)
	if err != nil {
		t.Fatalf("Hash(n=0) 应返回空串 sha256: %v", err)
	}
	expected := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if hash != expected {
		t.Errorf("hash = %q，预期 %q", hash, expected)
	}
}

// ── Read ────────────────────────────────────────────────

func TestRead(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	tmpDir := fs.Root
	data := []byte("hello, world! this is a test")
	os.WriteFile(filepath.Join(tmpDir, "readme.txt"), data, 0644)

	rc, err := fs.Read(context.Background(), "readme.txt", 11)
	if err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != "hello, worl" {
		t.Fatalf("读到 %q, %v，预期 %q", got, err, "hello, worl")
	}

	// n 超过文件长度：读到末尾报 io.ErrUnexpectedEOF
	rc2, err := fs.Read(context.Background(), "readme.txt", 999)
	if err != nil {
		t.Fatalf("Read 打开失败: %v", err)
	}
	got2, err := io.ReadAll(rc2)
	rc2.Close()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Read(n>file) 应返回 io.ErrUnexpectedEOF，得到 %v", err)
	}
	if string(got2) != string(data) {
		t.Errorf("不足时应先给出已有内容，得到 %q", got2)
	}

	// n==0 读到空
	rc3, err := fs.Read(context.Background(), "readme.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(rc3); err != nil || len(b) != 0 {
		t.Errorf("n=0 应读到空: %q %v", b, err)
	}
	rc3.Close()
}

// ── Write ───────────────────────────────────────────────

func TestWrite(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	data := []byte("new file content")
	mtime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	err := fs.Write(context.Background(), "newdir/sub/deep.txt", strings.NewReader(string(data)), mtime)
	if err != nil {
		t.Fatalf("Write 失败: %v", err)
	}

	rc, err := fs.Read(context.Background(), "newdir/sub/deep.txt", int64(len(data)))
	if err != nil {
		t.Fatalf("Write 后 Read 失败: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll 失败: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("写入内容不匹配: got %q, want %q", string(got), string(data))
	}

	fi, err := fs.Client.Stat(filepath.Join(fs.Root, "newdir/sub/deep.txt"))
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v，预期 %v", fi.ModTime(), mtime)
	}
	if fi.Mode()&0777 != 0600 {
		t.Errorf("mode & 0777 = %o，预期 600", fi.Mode()&0777)
	}

	// 验证没有残留临时文件
	entries, _ := fs.List(context.Background())
	for _, e := range entries {
		basename := filepath.Base(e.Path)
		if strings.HasPrefix(basename, ".projsync-tmp-") {
			t.Errorf("存在残留临时文件: %s", e.Path)
		}
	}

	// 覆盖写入
	overwrite := []byte("overwritten")
	mtime2 := mtime.Add(1 * time.Hour)
	err = fs.Write(context.Background(), "newdir/sub/deep.txt", strings.NewReader(string(overwrite)), mtime2)
	if err != nil {
		t.Fatalf("覆盖 Write 失败: %v", err)
	}

	rc3, err := fs.Read(context.Background(), "newdir/sub/deep.txt", int64(len(overwrite)))
	if err != nil {
		t.Fatalf("覆盖后 Read 失败: %v", err)
	}
	got3, err := io.ReadAll(rc3)
	rc3.Close()
	if err != nil {
		t.Fatalf("ReadAll 失败: %v", err)
	}
	if string(got3) != string(overwrite) {
		t.Errorf("覆盖内容不匹配: got %q, want %q", string(got3), string(overwrite))
	}
}

func TestWriteDoesNotLeaveTmpOnError(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	os.WriteFile(filepath.Join(fs.Root, "existing.txt"), []byte("existing"), 0644)
	err := fs.Write(context.Background(), "existing.txt", strings.NewReader("replaced"), time.Now())
	if err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}

	ents, _ := os.ReadDir(fs.Root)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".projsync-tmp-") {
			t.Errorf("存在残留临时文件: %s", e.Name())
		}
	}
}

func TestWriteFailureLeavesNoTmp(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()
	// 目标是个目录：改名会失败，临时文件必须被清掉。
	os.MkdirAll(filepath.Join(fs.Root, "d", "x"), 0755)
	if err := fs.Write(context.Background(), "d", strings.NewReader("zz"), time.Now()); err == nil {
		t.Fatal("目标是目录时应报错")
	}
	ents, _ := os.ReadDir(fs.Root)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".projsync-tmp-") {
			t.Errorf("失败后残留临时文件: %s", e.Name())
		}
	}
}

func TestWriteWithSpaces(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	tmpDir := fs.Root
	name := "file with spaces & 'quotes'.txt"
	data := []byte("special names work")
	os.MkdirAll(filepath.Join(tmpDir, "dir with space"), 0755)

	err := fs.Write(context.Background(), "dir with space/"+name, strings.NewReader(string(data)), time.Now())
	if err != nil {
		t.Fatalf("写入含空格文件名失败: %v", err)
	}

	rc, err := fs.Read(context.Background(), "dir with space/"+name, int64(len(data)))
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll 失败: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("内容不匹配: got %q", string(got))
	}
}

func TestHashWithSpacesAndQuotes(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	tmpDir := fs.Root
	name := "file with spaces & 'quotes'.txt"
	data := []byte("hash special names")
	os.WriteFile(filepath.Join(tmpDir, name), data, 0644)

	hash, err := fs.Hash(context.Background(), name, int64(len(data)))
	if err != nil {
		t.Fatalf("Hash 含空格文件名失败: %v", err)
	}
	expected := sha256.Sum256(data)
	expectedHex := hex.EncodeToString(expected[:])
	if hash != expectedHex {
		t.Errorf("hash = %q，预期 %q", hash, expectedHex)
	}
}

func TestInvalidPath(t *testing.T) {
	fs, done := setupSFTPFS(t)
	defer done()

	_, err := fs.Hash(context.Background(), "", 10)
	if err == nil {
		t.Fatal("空路径应报错")
	}

	_, err = fs.Hash(context.Background(), "/etc/passwd", 10)
	if err == nil {
		t.Fatal("以 / 开头应报错")
	}

	_, err = fs.Hash(context.Background(), "a\\b", 10)
	if err == nil {
		t.Fatal("含 \\ 应报错")
	}

	_, err = fs.Hash(context.Background(), "a/../b", 10)
	if err == nil {
		t.Fatal("含 .. 应报错")
	}

	_, err = fs.Hash(context.Background(), "a/./b", 10)
	if err == nil {
		t.Fatal("含 . 应报错")
	}
	if _, err = fs.Hash(context.Background(), "a//b", 10); err == nil {
		t.Fatal("空段应报错")
	}
	if _, err = fs.Read(context.Background(), "../x", 1); err == nil {
		t.Fatal("Read .. 应报错")
	}
	if err = fs.Write(context.Background(), "../x", strings.NewReader("x"), time.Now()); err == nil {
		t.Fatal("Write .. 应报错")
	}
}
