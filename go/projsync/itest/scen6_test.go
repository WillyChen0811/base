//go:build itest

// S22：项目直接放在工作目录第 1 层（rel 只有一段，没有年份层）的两机联调。
//
// XXYI 的「桌面上未同步」列表 2026-11 起也列工作目录第 1 层的项目，同步目录里镜像成 sync_dir/<名>，
// Ubuntu 上是 remote_root/<名>。剧本：
//  1. Mac 开始同步 260403J1401-测试项目庚（rel 只有一段）→ 一轮到 Ubuntu：两边一致，Ubuntu 的路径是 <root>/<名>；
//  2. Ubuntu 往里加一个文件、Mac 往里改一个文件 → 再一轮：两边都拿到对方的；
//  3. 取消同步（挪回原位）：桌面第 1 层是真实目录、同步目录里没有、同步目录本身还在；Ubuntu 那份不删。

package itest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/WillyChen0811/base/go/projsync"
)

const (
	relG  = "260403J1401-测试项目庚" // 只有一段：没有年份层
	codeG = "260403J1401"
)

func TestItestS22FirstLayerProject(t *testing.T) {
	c := newCluster(t)

	// 1) 开始同步（搬进同步目录 + 桌面留链接）→ 一轮到 Ubuntu。
	c.startUnit(relG, map[string][]byte{
		"需求.docx":   []byte("需求 v1\n"),
		"现场/记录.txt": []byte("记录 v1\n"),
	})
	link := filepath.Join(c.work, relG)
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("桌面第 1 层应留下链接 %s：%v", link, err)
	}
	if tgt, _ := os.Readlink(link); tgt != filepath.Join(c.sync, relG) {
		t.Errorf("链接应指向 <同步目录>/<名>：%q", tgt)
	}
	rep := c.round(true)
	c.assertAligned(relG, rep)
	if got := c.ubFilesOf(relG)["需求.docx"]; got != sha256Hex([]byte("需求 v1\n")) {
		t.Fatalf("Ubuntu 的 <root>/%s 下应有需求.docx：sha=%q 本轮=%s", relG, got, jsonOf(rep))
	}
	if res := unitResult(rep, codeG); res == nil {
		t.Fatalf("本轮报告里没有单元 %s：%s", codeG, jsonOf(rep))
	}

	// 2) 两边各改一处，再一轮。
	fromUb := []byte("Ubuntu 上新加的\n")
	c.writeUb(relG, map[string][]byte{"新到.txt": fromUb})
	edited := []byte("记录 v2（Mac 改的）\n")
	c.writeMacInSync(relG, map[string][]byte{"现场/记录.txt": edited})
	quietPast(t, filepath.Join(c.sync, filepath.FromSlash(relG)))
	c.sleepQuiet()
	rep2 := c.round(true)
	if got := c.macFilesOf(relG)["新到.txt"]; got != sha256Hex(fromUb) {
		t.Errorf("Mac 应拿到 Ubuntu 新加的文件：sha=%q 本轮=%s", got, jsonOf(rep2))
	}
	if got := c.ubFilesOf(relG)["现场/记录.txt"]; got != sha256Hex(edited) {
		t.Errorf("Ubuntu 应拿到 Mac 改的文件：sha=%q 本轮=%s", got, jsonOf(rep2))
	}
	c.assertAligned(relG, rep2)

	// 3) 取消同步，挪回原位。
	res, err := c.mob.StopSync(context.Background(), codeG)
	if err != nil || res.Status != projsync.PFUnitMoveDone {
		t.Fatalf("StopSync 没做成：%v status=%s reason=%s", err, res.Status, res.Reason)
	}
	if fi, err := os.Lstat(link); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("取消后桌面第 1 层应是真实目录：%v", err)
	}
	if _, err := os.Lstat(filepath.Join(c.sync, relG)); err == nil {
		t.Errorf("取消后同步目录里不该还有 %s", relG)
	}
	if _, err := os.Stat(c.sync); err != nil {
		t.Errorf("同步目录本身不能被删：%v", err)
	}
	if got := c.ubFilesOf(relG)["需求.docx"]; got != sha256Hex([]byte("需求 v1\n")) {
		t.Errorf("取消同步不删 Ubuntu 那份：sha=%q", got)
	}
	if b, err := os.ReadFile(filepath.Join(c.work, relG, "新到.txt")); err != nil || string(b) != string(fromUb) {
		t.Errorf("挪回桌面的目录里应有同步来的文件：%v", err)
	}
}

// S23：项目放在非年份的文件夹下一层（rel = 「客户资料/<项目>」），开始同步 → 一轮到 Ubuntu → 取消同步。
func TestItestS23GroupFolderProject(t *testing.T) {
	const rel = "客户资料/260403J1402-测试项目辛"
	c := newCluster(t)
	c.startUnit(rel, map[string][]byte{"需求.docx": []byte("需求 v1\n"), "现场/记录.txt": []byte("记录 v1\n")})
	rep := c.round(true)
	c.assertAligned(rel, rep)
	if got := c.ubFilesOf(rel)["需求.docx"]; got != sha256Hex([]byte("需求 v1\n")) {
		t.Fatalf("Ubuntu 的 <root>/%s 下应有需求.docx：sha=%q 本轮=%s", rel, got, jsonOf(rep))
	}
	res, err := c.mob.StopSync(context.Background(), "260403J1402")
	if err != nil || res.Status != projsync.PFUnitMoveDone {
		t.Fatalf("StopSync 没做成：%v status=%s reason=%s", err, res.Status, res.Reason)
	}
	if fi, err := os.Lstat(filepath.Join(c.work, rel)); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("取消后桌面应是真实目录：%v", err)
	}
	if _, err := os.Lstat(filepath.Join(c.work, "客户资料")); err != nil {
		t.Errorf("桌面上的「客户资料」文件夹不能被删：%v", err)
	}
}
