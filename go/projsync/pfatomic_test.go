package projsync

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var errPFTestCrash = errors.New("模拟进程在此处中断")

// pfCrashHook 记录每一步的名字，并在第 at 次调用（从 0 数）返回「崩溃」；at<0 表示只记录不崩溃。
type pfCrashHook struct {
	at    int
	n     int
	steps []string
}

func (c *pfCrashHook) fn(step string) error {
	c.steps = append(c.steps, step)
	c.n++
	if c.at >= 0 && c.n-1 == c.at {
		return errPFTestCrash
	}
	return nil
}

func pfTmpLeft(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestWriteFileAtomic_Basic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b", "x.json") // 父目录不存在，要自动建
	if err := writeFileAtomic(p, []byte("one"), 0); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "one" {
		t.Fatalf("内容 %q", b)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("缺省权限应为 0600，实际 %v", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Join(dir, "a"))
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("新建目录权限应为 0700，实际 %v", di.Mode().Perm())
		}
	}
	// 覆盖，并换权限
	if err := writeFileAtomic(p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "two" {
		t.Fatalf("内容 %q", b)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o644 {
			t.Fatalf("权限应为 0644，实际 %v", fi.Mode().Perm())
		}
	}
	if left := pfTmpLeft(t, filepath.Dir(p)); len(left) != 0 {
		t.Fatalf("残留临时文件 %v", left)
	}
	// 空内容也能写
	if err := writeFileAtomic(p, nil, 0); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Size() != 0 {
		t.Fatalf("应为空文件")
	}
}

func TestWriteFileAtomic_FailureLeavesOldAndNoTmp(t *testing.T) {
	dir := t.TempDir()
	// 目标是个目录：改名必败。
	target := filepath.Join(dir, "d")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("x"), 0); err == nil {
		t.Fatal("目标是目录应报错")
	}
	if left := pfTmpLeft(t, dir); len(left) != 0 {
		t.Fatalf("失败后不应残留临时文件 %v", left)
	}
	// 父路径是个普通文件
	f := filepath.Join(dir, "f")
	os.WriteFile(f, []byte("x"), 0o600)
	if err := writeFileAtomic(filepath.Join(f, "y"), []byte("x"), 0); err == nil {
		t.Fatal("父路径是文件应报错")
	}
}

// 每一步之间「崩溃」：目标要么是旧内容、要么是新内容，绝不是半截；崩溃不清理临时文件（和真崩溃一致）。
func TestWriteFileAtomic_CrashEveryStep(t *testing.T) {
	rec := &pfCrashHook{at: -1}
	{
		p := filepath.Join(t.TempDir(), "x")
		if _, err := pfWriteAtomic(p, []byte("new"), 0, rec.fn); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"tmp-created", "tmp-written", "tmp-synced", "renamed", "dir-synced"}
	if strings.Join(rec.steps, ",") != strings.Join(want, ",") {
		t.Fatalf("步骤 %v", rec.steps)
	}
	for i, step := range rec.steps {
		dir := t.TempDir()
		p := filepath.Join(dir, "x")
		os.WriteFile(p, []byte("old"), 0o600)
		c := &pfCrashHook{at: i}
		renamed, err := pfWriteAtomic(p, []byte("new"), 0, c.fn)
		if !errors.Is(err, errPFTestCrash) {
			t.Fatalf("步骤 %s: 应返回崩溃 error，得到 %v", step, err)
		}
		b, _ := os.ReadFile(p)
		afterRename := i >= 3
		if renamed != afterRename {
			t.Fatalf("步骤 %s: renamed=%v", step, renamed)
		}
		if afterRename && string(b) != "new" || !afterRename && string(b) != "old" {
			t.Fatalf("步骤 %s: 目标内容 %q", step, b)
		}
		left := pfTmpLeft(t, dir)
		if !afterRename && len(left) != 1 {
			t.Fatalf("步骤 %s: 崩溃应原样留下临时文件，实际 %v", step, left)
		}
		if afterRename && len(left) != 0 {
			t.Fatalf("步骤 %s: 改名后不应有临时文件 %v", step, left)
		}
	}
}

func TestAppendLineSync(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "j.journal")
	if err := appendLineSync(p, []byte(`{"a":1}`), 0); err != nil {
		t.Fatal(err)
	}
	if err := appendLineSync(p, []byte("second"), 0); err != nil {
		t.Fatal(err)
	}
	if err := appendLinesSync(p, [][]byte{[]byte("x"), []byte("y")}, 0, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "{\"a\":1}\nsecond\nx\ny\n" {
		t.Fatalf("内容 %q", b)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("权限 %v", fi.Mode().Perm())
		}
	}
	// 行内含换行：拒绝，且文件不变
	if err := appendLineSync(p, []byte("a\nb"), 0); err == nil {
		t.Fatal("行内换行应拒绝")
	}
	b2, _ := os.ReadFile(p)
	if string(b2) != string(b) {
		t.Fatal("拒绝后文件被改了")
	}
	// 没有行：什么也不做
	if err := appendLinesSync(p, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAppendLinesSync_CrashSteps(t *testing.T) {
	// 新建文件：步骤含 dir-synced；已存在：不含。
	rec := &pfCrashHook{at: -1}
	p := filepath.Join(t.TempDir(), "j")
	if err := appendLinesSync(p, [][]byte{[]byte("a")}, 0, rec.fn); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.steps, ","); got != "append:opened,append:written,append:synced,append:dir-synced" {
		t.Fatalf("新建文件步骤 %s", got)
	}
	rec2 := &pfCrashHook{at: -1}
	if err := appendLinesSync(p, [][]byte{[]byte("b")}, 0, rec2.fn); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec2.steps, ","); got != "append:opened,append:written,append:synced" {
		t.Fatalf("已有文件步骤 %s", got)
	}
	// 崩溃：已写入的行不丢，没写的不出现
	for i := 0; i < 3; i++ { // 文件已存在时只有前三步
		pp := filepath.Join(t.TempDir(), "j")
		os.WriteFile(pp, []byte("old\n"), 0o600)
		c := &pfCrashHook{at: i}
		err := appendLinesSync(pp, [][]byte{[]byte("new")}, 0, c.fn)
		if !errors.Is(err, errPFTestCrash) {
			t.Fatalf("步骤 %d: %v", i, err)
		}
		b, _ := os.ReadFile(pp)
		if i == 0 {
			if string(b) != "old\n" {
				t.Fatalf("写之前崩溃，内容 %q", b)
			}
		} else if string(b) != "old\nnew\n" {
			t.Fatalf("步骤 %d: 内容 %q", i, b)
		}
	}
}

func TestPFSyncDir(t *testing.T) {
	if err := pfSyncDir(t.TempDir()); err != nil {
		t.Fatalf("对普通目录 fsync 应成功: %v", err)
	}
	if err := pfSyncDir(filepath.Join(t.TempDir(), "不存在")); err == nil {
		t.Fatal("目录不存在应报错")
	}
}

func TestPFMkdirAllSync(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b", "c")
	if err := pfMkdirAllSync(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(deep); err != nil || !fi.IsDir() {
		t.Fatalf("没建出来: %v", err)
	}
	// 已存在：幂等
	if err := pfMkdirAllSync(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	// 中间是文件：报错
	f := filepath.Join(dir, "file")
	os.WriteFile(f, []byte("x"), 0o600)
	if err := pfMkdirAllSync(filepath.Join(f, "sub"), 0o700); err == nil {
		t.Fatal("父路径是文件应报错")
	}
}
