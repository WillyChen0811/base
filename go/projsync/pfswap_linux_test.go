//go:build linux

package projsync

// Linux 专用测试：renameat2 的不覆盖改名与原子交换、没有 clonefile 时的普通复制、
// 区分大小写的文件系统上「同名只差大小写的两个文件」。
// 这一组（名字都以 TestPFLinux 开头）可以在 Ubuntu 本机跑，只用 t.TempDir()，不碰任何服务：
//   cd go && env -u TMUX -u TMUX_PANE ~/.local/go/bin/go test -count=1 -run 'TestPFLinux' ./projsync/
// 其中 TestPFLinuxPrimitives* 把全部写原语在 Linux 上完整走一遍（正常路径与竞态窗口）。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPFLinuxRenameNoReplace(t *testing.T) {
	d := t.TempDir()
	a, b, c := filepath.Join(d, "a"), filepath.Join(d, "b"), filepath.Join(d, "c")
	os.WriteFile(a, []byte("A"), 0o644)
	os.WriteFile(b, []byte("B"), 0o644)
	err := pfRenameNoReplace(a, b)
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENOTSUP) {
		t.Skipf("这个文件系统不支持 RENAME_NOREPLACE：%v", err)
	}
	if !errors.Is(err, syscall.EEXIST) {
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
	if x, _ := os.ReadFile(c); string(x) != "A" {
		t.Errorf("c = %q", x)
	}
	// 目标是目录：同样不覆盖
	os.Mkdir(filepath.Join(d, "dir"), 0o755)
	os.WriteFile(filepath.Join(d, "f"), []byte("F"), 0o644)
	if err := pfRenameNoReplace(filepath.Join(d, "f"), filepath.Join(d, "dir")); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("目标是目录应报 EEXIST：%v", err)
	}
}

func TestPFLinuxRenameExchange(t *testing.T) {
	d := t.TempDir()
	a, b := filepath.Join(d, "a"), filepath.Join(d, "b")
	os.WriteFile(a, []byte("A"), 0o644)
	os.WriteFile(b, []byte("BB"), 0o644)
	sa, _ := PFLstat(a)
	sb, _ := PFLstat(b)
	err := pfRenameExchange(a, b)
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENOTSUP) {
		t.Skipf("这个文件系统不支持 RENAME_EXCHANGE：%v", err)
	}
	if err != nil {
		t.Fatal(err)
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

func TestPFLinuxNoClonefile(t *testing.T) {
	if pfCanClone {
		t.Fatal("linux 上 pfCanClone 应为 false")
	}
	if err := pfCloneFile("a", "b"); !errors.Is(err, errPFNoClone) {
		t.Errorf("pfCloneFile 应返回 errPFNoClone：%v", err)
	}
	e := newPFAEnv(t)
	e.put("src.docx", "共同内容")
	part, res, err := e.a.StageCopy(bg, PFCopySrc{Path: e.abs("src.docx"), SHA256: pfaSHA("共同内容"), Size: int64(len("共同内容"))})
	e.wantDone(res, err)
	if strings.Contains(strings.Join(res.Notes, " "), "clonefile") {
		t.Errorf("linux 上不该走 clonefile：%v", res.Notes)
	}
	if b, _ := os.ReadFile(part.Path); string(b) != "共同内容" {
		t.Errorf("part = %q", b)
	}
	st, _ := os.Lstat(part.Path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("复制品权限 %v", st.Mode().Perm())
	}
	if len(e.leftovers()) != 0 {
		t.Errorf("暂存区残留 %v", e.leftovers())
	}
	// 复制来源是符号链接：不读
	os.Symlink(e.abs("src.docx"), e.abs("lnk.docx"))
	_, res, err = e.a.StageCopy(bg, PFCopySrc{Path: e.abs("lnk.docx"), SHA256: pfaSHA("共同内容"), Size: int64(len("共同内容"))})
	e.wantStatus(res, err, PFApplyAbandoned, PFApplyCodeSourceChanged)
}

// 区分大小写的文件系统（ext4）上，Report.docx 和 report.docx 是两个不同的文件：
// 按字节核对，各管各的；新建第三个只差大小写的名字则判超出范围。
func TestPFLinuxSameNameTwoFiles(t *testing.T) {
	e := newPFAEnv(t)
	if pfaCaseInsensitive(t, e.root) {
		t.Skip("这个目录所在文件系统不区分大小写")
	}
	e.put("Report.docx", "大写那份")
	e.put("report.docx", "小写那份")
	wantLower := e.expect("report.docx")
	part := e.stage("新内容")

	res, err := e.a.InstallOver(bg, "report.docx", part, 1, wantLower)
	e.wantDone(res, err)
	if e.read("report.docx") != "新内容" || e.read("Report.docx") != "大写那份" {
		t.Errorf("只该换小写那份：%q %q", e.read("report.docx"), e.read("Report.docx"))
	}
	e.holds("小写那份")
	// 按大写名字删：只删大写那份
	res, err = e.a.Delete(bg, "Report.docx", e.expect("Report.docx"), PFReasonDelete)
	e.wantDone(res, err)
	if e.exists("Report.docx") || e.read("report.docx") != "新内容" {
		t.Error("删除只该动 Report.docx")
	}
	// 新建第三个只差大小写的名字：判超出范围
	e.put("Notes.txt", "n")
	res, err = e.a.InstallNew(bg, "NOTES.txt", e.stage("x"), 1)
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeNameClash)
	// 目录同理
	os.MkdirAll(e.abs("Docs"), 0o755)
	os.MkdirAll(e.abs("docs"), 0o755)
	e.put("Docs/a.docx", "D")
	e.put("docs/a.docx", "d")
	res, err = e.a.Delete(bg, "docs/a.docx", e.expect("docs/a.docx"), PFReasonDelete)
	e.wantDone(res, err)
	if e.read("Docs/a.docx") != "D" {
		t.Error("Docs/a.docx 不该被动")
	}
	// 改名成只差大小写：第一版超出范围
	res, err = e.a.Rename(bg, "Notes.txt", "notes.txt", e.expect("Notes.txt"))
	e.wantStatus(res, err, PFApplyOutOfScope, PFApplyCodeCaseOnlyRename)
}

// 原语在 Linux（RENAME_EXCHANGE / RENAME_NOREPLACE）上完整走一遍：把 pfapply_test.go、pfrecycle_test.go 里
// 与文件系统无关的测试逐个当子测试跑（依赖 macOS 行为的几个不在内）。

func TestPFLinuxPrimitives(t *testing.T) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"PFApplyInstallNewBasic", TestPFApplyInstallNewBasic},
		{"PFApplyInstallNewTargetExists", TestPFApplyInstallNewTargetExists},
		{"PFApplyInstallNewRaceCreate", TestPFApplyInstallNewRaceCreate},
		{"PFApplyInstallNewRaceCreateRemovesOwnDirs", TestPFApplyInstallNewRaceCreateRemovesOwnDirs},
		{"PFApplyInstallNewPartMismatchDeletesPart", TestPFApplyInstallNewPartMismatchDeletesPart},
		{"PFApplyInstallNewCrashWindows", TestPFApplyInstallNewCrashWindows},
		{"PFApplyInstallOverBasic", TestPFApplyInstallOverBasic},
		{"PFApplyInstallOverPreconditionMismatch", TestPFApplyInstallOverPreconditionMismatch},
		{"PFApplyInstallOverHashMismatchSwapsBack", TestPFApplyInstallOverHashMismatchSwapsBack},
		{"PFApplyInstallOverRaceBeforeSwap", TestPFApplyInstallOverRaceBeforeSwap},
		{"PFApplyInstallOverTargetDeletedBeforeSwap", TestPFApplyInstallOverTargetDeletedBeforeSwap},
		{"PFApplyInstallOverRaceAfterSwap", TestPFApplyInstallOverRaceAfterSwap},
		{"PFApplyInstallOverStrangerDuringSwap", TestPFApplyInstallOverStrangerDuringSwap},
		{"PFApplyInstallOverCrashWindows", TestPFApplyInstallOverCrashWindows},
		{"PFApplyInstallOverAfterStaleCleanDoesNotDeleteOldFile", TestPFApplyInstallOverAfterStaleCleanDoesNotDeleteOldFile},
		{"PFApplyInstallOverRecyclesWithoutDeletingExisting", TestPFApplyInstallOverRecyclesWithoutDeletingExisting},
		{"PFApplyCancelAfterSwapStillFinishes", TestPFApplyCancelAfterSwapStillFinishes},
		{"PFApplyDeleteBasic", TestPFApplyDeleteBasic},
		{"PFApplyDeletePreconditionMismatch", TestPFApplyDeletePreconditionMismatch},
		{"PFApplyDeleteRaceModifiedBeforeMove", TestPFApplyDeleteRaceModifiedBeforeMove},
		{"PFApplyDeleteRaceOriginalSpotOccupied", TestPFApplyDeleteRaceOriginalSpotOccupied},
		{"PFApplyDeleteCrashWindows", TestPFApplyDeleteCrashWindows},
		{"PFApplyRenameBasic", TestPFApplyRenameBasic},
		{"PFApplyRenameTargetExistsAndClash", TestPFApplyRenameTargetExistsAndClash},
		{"PFApplyRenameRaceModified", TestPFApplyRenameRaceModified},
		{"PFApplyConflictRename", TestPFApplyConflictRename},
		{"PFApplyOfficeLocks", TestPFApplyOfficeLocks},
		{"PFApplyOfficeLockLongName", TestPFApplyOfficeLockLongName},
		{"PFApplyReadOnly", TestPFApplyReadOnly},
		{"PFApplyDatalessIsDeferred", TestPFApplyDatalessIsDeferred},
		{"PFApplyNotRegularTargets", TestPFApplyNotRegularTargets},
		{"PFApplyDifferentVolumeIsConfigError", TestPFApplyDifferentVolumeIsConfigError},
		{"PFApplyInstallNewNameClash", TestPFApplyInstallNewNameClash},
		{"PFApplyInstallNewParentClash", TestPFApplyInstallNewParentClash},
		{"PFApplyInstallNewSymlinkAndFileInPath", TestPFApplyInstallNewSymlinkAndFileInPath},
		{"PFApplyInstallNewBadNames", TestPFApplyInstallNewBadNames},
		{"PFApplyInstallArgsAreConfigErrors", TestPFApplyInstallArgsAreConfigErrors},
		{"PFApplyInstallNewReadOnlyDir", TestPFApplyInstallNewReadOnlyDir},
		{"PFApplyInstallNewICloudStub", TestPFApplyInstallNewICloudStub},
		{"PFApplyMkdirRmdir", TestPFApplyMkdirRmdir},
		{"PFApplyRmdirRaceFileAdded", TestPFApplyRmdirRaceFileAdded},
		{"PFApplyRmdirSymlinkAndSiblings", TestPFApplyRmdirSymlinkAndSiblings},
		{"PFApplyStageCopyAndInstall", TestPFApplyStageCopyAndInstall},
		{"PFApplyStageCopyOver", TestPFApplyStageCopyOver},
		{"PFApplyStageCopySourceMismatch", TestPFApplyStageCopySourceMismatch},
		{"PFApplyStageCopyExistingPart", TestPFApplyStageCopyExistingPart},
		{"PFApplyCanceledBeforeStart", TestPFApplyCanceledBeforeStart},
		{"PFRecycleLayoutAndIndex", TestPFRecycleLayoutAndIndex},
		{"PFRecycleNameCollisions", TestPFRecycleNameCollisions},
		{"PFRecycleCaseInsensitiveCollision", TestPFRecycleCaseInsensitiveCollision},
		{"PFRecycleLongNameKeepsWithin255Bytes", TestPFRecycleLongNameKeepsWithin255Bytes},
		{"PFRecyclePathShapeConflict", TestPFRecyclePathShapeConflict},
		{"PFRecycleIndexSkipsPartialLine", TestPFRecycleIndexSkipsPartialLine},
		{"PFRecycleCleanDays", TestPFRecycleCleanDays},
		{"PFRecycleCleanKeepsFullDays", TestPFRecycleCleanKeepsFullDays},
		{"PFRecycleUsage", TestPFRecycleUsage},
		{"PFRecycleCheckDifferentVolume", TestPFRecycleCheckDifferentVolume},
		{"PFRecycleNeverDeletedByPrimitives", TestPFRecycleNeverDeletedByPrimitives},
	} {
		t.Run(c.name, c.fn)
	}
}
