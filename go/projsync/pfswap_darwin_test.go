//go:build darwin

package projsync

// macOS 专用测试：renamex_np 的不覆盖改名与原子交换、clonefile、uchg 标志。
// 只在 Mac Studio 跑（APFS）。字节名核对（Report/report、NFC/NFD）在 pfapply_test.go 里，
// 那里会探测文件系统是否真的不区分大小写。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPFDarwinRenameNoReplace(t *testing.T) {
	d := t.TempDir()
	a, b, c := filepath.Join(d, "a"), filepath.Join(d, "b"), filepath.Join(d, "c")
	os.WriteFile(a, []byte("A"), 0o644)
	os.WriteFile(b, []byte("B"), 0o644)
	if err := pfRenameNoReplace(a, b); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("目标已存在应报 EEXIST：%v", err)
	}
	if x, _ := os.ReadFile(a); string(x) != "A" {
		t.Error("a 被动了")
	}
	if x, _ := os.ReadFile(b); string(x) != "B" {
		t.Error("b 被盖了")
	}
	if err := pfRenameNoReplace(a, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a); err == nil {
		t.Error("改名后 a 应不在")
	}
	// APFS 不区分大小写：只差大小写的名字也算已存在
	if pfaCaseInsensitive(t, d) {
		os.WriteFile(filepath.Join(d, "Case"), []byte("1"), 0o644)
		os.WriteFile(filepath.Join(d, "src"), []byte("2"), 0o644)
		if err := pfRenameNoReplace(filepath.Join(d, "src"), filepath.Join(d, "case")); !errors.Is(err, syscall.EEXIST) {
			t.Errorf("大小写不敏感的盘上 case 应算已存在：%v", err)
		}
	}
}

func TestPFDarwinRenameSwap(t *testing.T) {
	d := t.TempDir()
	a, b := filepath.Join(d, "a"), filepath.Join(d, "b")
	os.WriteFile(a, []byte("A"), 0o644)
	os.WriteFile(b, []byte("BB"), 0o644)
	sa, _ := PFLstat(a)
	sb, _ := PFLstat(b)
	if err := pfRenameExchange(a, b); err != nil {
		t.Fatalf("APFS 应支持 RENAME_SWAP：%v", err)
	}
	if x, _ := os.ReadFile(a); string(x) != "BB" {
		t.Errorf("a = %q", x)
	}
	if x, _ := os.ReadFile(b); string(x) != "A" {
		t.Errorf("b = %q", x)
	}
	na, _ := PFLstat(a)
	nb, _ := PFLstat(b)
	if na.Ino != sb.Ino || nb.Ino != sa.Ino {
		t.Error("交换应只换名字，ino 跟着内容走")
	}
	if err := pfRenameExchange(a, filepath.Join(d, "nope")); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("一边不存在应报 ENOENT：%v", err)
	}
}

func TestPFDarwinClonefile(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	os.WriteFile(src, []byte("原内容"), 0o644)
	if err := pfCloneFile(src, dst); err != nil {
		t.Skipf("这个盘不支持 clonefile：%v", err)
	}
	if x, _ := os.ReadFile(dst); string(x) != "原内容" {
		t.Errorf("clone 内容 = %q", x)
	}
	// 写时复制：改 clone 不影响来源
	os.WriteFile(dst, []byte("改了"), 0o644)
	if x, _ := os.ReadFile(src); string(x) != "原内容" {
		t.Error("改 clone 影响了来源")
	}
	if err := pfCloneFile(src, dst); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("目标已存在应报 EEXIST：%v", err)
	}
	// 来源是符号链接：不跟随
	os.Symlink(src, filepath.Join(d, "lnk"))
	if err := pfCloneFile(filepath.Join(d, "lnk"), filepath.Join(d, "dst2")); err == nil {
		if st, _ := os.Lstat(filepath.Join(d, "dst2")); st.Mode()&os.ModeSymlink == 0 {
			t.Error("CLONE_NOFOLLOW 不该把链接当成目标文件复制")
		}
	}
}

func TestPFDarwinStageCopyUsesClone(t *testing.T) {
	e := newPFAEnv(t)
	e.put("src.docx", "共同内容")
	probe := filepath.Join(e.stg, "probe")
	if err := pfCloneFile(e.abs("src.docx"), probe); err != nil {
		t.Skipf("这个盘不支持 clonefile：%v", err)
	}
	os.Remove(probe)
	part, res, err := e.a.StageCopy(bg, PFCopySrc{Path: e.abs("src.docx"), SHA256: pfaSHA("共同内容"), Size: int64(len("共同内容"))})
	e.wantDone(res, err)
	if !strings.Contains(strings.Join(res.Notes, " "), "clonefile") {
		t.Errorf("darwin 上应走 clonefile：%v", res.Notes)
	}
	if b, _ := os.ReadFile(part.Path); string(b) != "共同内容" {
		t.Errorf("part 内容 %q", b)
	}
	// 先 clone 再改来源：part 仍是 clone 当时的内容，装上去用的是 clone 的内容
	pfaAppend(t, e.abs("src.docx"), "+之后改的")
	res, err = e.a.InstallNew(bg, "copy.docx", part, 3)
	e.wantDone(res, err)
	if e.read("copy.docx") != "共同内容" {
		t.Errorf("clone 应与来源后续修改无关：%q", e.read("copy.docx"))
	}
}

func TestPFDarwinUchgIsOutOfScope(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "原")
	e.put("locked/b.docx", "原b")
	want := e.expect("a.docx")
	wantB := e.expect("locked/b.docx")
	part := e.stage("新")
	setFlags := func(p string, f int) {
		if err := unix.Chflags(p, f); err != nil {
			t.Fatalf("chflags %s：%v", p, err)
		}
	}
	setFlags(e.abs("a.docx"), unix.UF_IMMUTABLE)
	setFlags(e.abs("locked"), unix.UF_IMMUTABLE)
	t.Cleanup(func() { unix.Chflags(e.abs("a.docx"), 0); unix.Chflags(e.abs("locked"), 0) })

	for name, run := range map[string]func() (PFApplyResult, error){
		"覆盖":   func() (PFApplyResult, error) { return e.a.InstallOver(bg, "a.docx", part, 1, want) },
		"删除":   func() (PFApplyResult, error) { return e.a.Delete(bg, "a.docx", want, PFReasonDelete) },
		"改名":   func() (PFApplyResult, error) { return e.a.Rename(bg, "a.docx", "z.docx", want) },
		"目录里删": func() (PFApplyResult, error) { return e.a.Delete(bg, "locked/b.docx", wantB, PFReasonDelete) },
		"目录里建": func() (PFApplyResult, error) { return e.a.InstallNew(bg, "locked/new.docx", part, 1) },
		"删目录":  func() (PFApplyResult, error) { return e.a.Rmdir(bg, "locked") },
	} {
		res, err := run()
		if err != nil || res.Status != PFApplyOutOfScope || res.Code != PFApplyCodeImmutable {
			t.Errorf("%s：%v err=%v，想要超出范围 immutable", name, res, err)
		}
	}
	if e.read("a.docx") != "原" || e.read("locked/b.docx") != "原b" {
		t.Error("带 uchg 的文件被动了")
	}
	e.partIntact(part, "新")
}

// 标志是核对之后才加的（stat 当时没看到）：不管系统调用拒绝还是放行，任何一份内容都不能丢。
func TestPFDarwinUchgAddedInWindow(t *testing.T) {
	e := newPFAEnv(t)
	e.put("a.docx", "原")
	want := e.expect("a.docx")
	part := e.stage("新")
	clearFlags := func() {
		filepath.WalkDir(e.base, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				unix.Chflags(p, 0)
			}
			return nil
		})
	}
	t.Cleanup(clearFlags)
	e.onHook("over:before-swap", func() {
		if err := unix.Chflags(e.abs("a.docx"), unix.UF_IMMUTABLE); err != nil {
			t.Fatal(err)
		}
	})
	res, err := e.a.InstallOver(bg, "a.docx", part, 1, want)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("目标在窗口里被加了 uchg，结果：%v", res)
	clearFlags()
	e.holds("原", "新")
}
