//go:build itest

package itest

import (
	"os"
	"strings"
	"testing"
)

// 项目编号必须是真实格式（PFCodeFromName 认得出）：日期+字母+流水号。
const (
	relA  = "2026/260403J1001-测试项目甲"
	relB  = "2026/260403J1002-测试项目乙"
	codeA = "260403J1001"
	codeB = "260403J1002"
)

// S1 首次同步：Mac 两个项目（含子目录、一个 5MB 文件、中文名）一轮传齐；再跑一轮无变化不传。
func TestItestS1FirstSync(t *testing.T) {
	c := newCluster(t)
	const mb5 = 5 << 20
	filesA := map[string][]byte{
		"需求说明书.docx":    []byte("需求 v1\n"),
		"大纲.txt":       []byte("大纲\n"),
		"素材/录音.raw":    bigFile(mb5),
		"素材/图片/图一.png": []byte("png-stub-1\n"),
		"素材/图片/图二.png": []byte("png-stub-2\n"),
		"文档/会议纪要.md":   []byte("# 会议纪要\n"),
		"文档/草稿/初稿.txt": []byte("初稿\n"),
		"文档/草稿/二稿.txt": []byte("二稿\n"),
		"合同扫描件.pdf":    []byte("%PDF-1.4 stub\n"),
		"预算表.xlsx":     []byte("xlsx-stub\n"),
		"联系人名录.csv":    []byte("姓名,电话\n张三,13800000000\n"),
		"项目日志.log":     []byte("2026-10-27 建项\n"),
	}
	filesB := map[string][]byte{
		"备忘.txt":       []byte("备忘\n"),
		"素材/音频片段.wav": []byte("RIFF-stub\n"),
		"报告 v1.md":     []byte("# 报告\n"),
	}
	c.startUnit(relA, filesA)
	c.startUnit(relB, filesB)

	rep := c.round(true)
	if unitResult(rep, codeA) == nil || unitResult(rep, codeB) == nil {
		t.Fatalf("首轮两个项目都应该跑：units=%s", jsonOf(rep.Units))
	}
	c.assertAligned(relA, rep)
	c.assertAligned(relB, rep)
	if rep.Totals.BytesUp < mb5 {
		t.Errorf("首轮上行字节 %d，不应少于 5MB（录音.raw）", rep.Totals.BytesUp)
	}

	// 再跑一轮：没有变化 → 报告里没有传输。
	rep2 := c.round(true)
	if rep2.Totals.BytesUp != 0 || rep2.Totals.BytesDown != 0 {
		t.Errorf("第二轮不该有任何传输：bytes_up=%d bytes_down=%d", rep2.Totals.BytesUp, rep2.Totals.BytesDown)
		t.Logf("第二轮报告：%s", jsonOf(rep2))
		c.t2logTreeDiff(relA)
		c.t2logTreeDiff(relB)
	}
	for _, u := range rep2.Units {
		if u.Result != nil && len(u.Result.Done) > 0 {
			t.Errorf("第二轮单元 %s 不该有已做的动作：%s", u.Code, jsonOf(u.Result.Done))
		}
	}
}

// S2 双向改：对齐后 Mac 改 A、Ubuntu 改 B、Ubuntu 新建 C → 一轮后两边一致。
func TestItestS2BothChanged(t *testing.T) {
	c := newCluster(t)
	c.startUnit(relA, map[string][]byte{
		"大纲.txt":  []byte("大纲 v1\n"),
		"记录.md":   []byte("记录 v1\n"),
		"名单.csv":  []byte("a,b\n"),
		"素材/x.raw": []byte("x\n"),
	})
	rep := c.round(true)
	c.assertAligned(relA, rep)

	macV := []byte("大纲 v2（Mac 改的）\n")
	ubV := []byte("记录 v2（Ubuntu 改的）\n")
	newC := []byte("Ubuntu 新建的 C\n")
	c.writeMacInSync(relA, map[string][]byte{"大纲.txt": macV})
	c.writeUb(relA, map[string][]byte{"记录.md": ubV, "素材/新C.txt": newC})
	c.sleepQuiet()

	rep = c.round(true)
	c.assertAligned(relA, rep)
	mac, ub := c.macFilesOf(relA), c.ubFilesOf(relA)
	for _, side := range []struct {
		name string
		tree map[string]string
	}{{"Mac", mac}, {"Ubuntu", ub}} {
		if got := side.tree["大纲.txt"]; got != sha256Hex(macV) {
			t.Errorf("%s 的大纲.txt 不是 Mac 的新内容：sha=%s", side.name, got)
		}
		if got := side.tree["记录.md"]; got != sha256Hex(ubV) {
			t.Errorf("%s 的记录.md 不是 Ubuntu 的新内容：sha=%s", side.name, got)
		}
		if got := side.tree["素材/新C.txt"]; got != sha256Hex(newC) {
			t.Errorf("%s 上没有素材/新C.txt（Ubuntu 新建的没取回）", side.name)
		}
	}
	t.Logf("S2 done=%s", jsonOf(unitResult(rep, codeA).Done))
}

// S3 冲突：两边都改同一个文件 → 两边都留下原名 + 冲突副本，报告里有这条冲突，两边内容都不丢。
func TestItestS3Conflict(t *testing.T) {
	c := newCluster(t)
	c.startUnit(relA, map[string][]byte{"冲突文档.txt": []byte("base 版本\n"), "别的.txt": []byte("别的\n")})
	c.round(true)

	macV := []byte("Mac 上改的版本\n")
	ubV := []byte("Ubuntu 上改的版本\n")
	c.writeMacInSync(relA, map[string][]byte{"冲突文档.txt": macV})
	c.writeUb(relA, map[string][]byte{"冲突文档.txt": ubV})
	c.sleepQuiet()

	rep := c.round(true)
	res := unitResult(rep, codeA)
	hitConflict := false
	for _, p := range res.NewConflicts {
		if p == "冲突文档.txt" {
			hitConflict = true
		}
	}
	for _, cf := range res.Conflicts {
		if cf.Path == "冲突文档.txt" {
			hitConflict = true
		}
	}
	if !hitConflict {
		t.Errorf("报告里没有这条冲突：new_conflicts=%v conflicts=%s", res.NewConflicts, jsonOf(res.Conflicts))
	}

	macSha, ubSha := sha256Hex(macV), sha256Hex(ubV)
	for _, side := range []struct {
		name string
		tree map[string]string
	}{{"Mac", c.macFilesOf(relA)}, {"Ubuntu", c.ubFilesOf(relA)}} {
		// 只看冲突那组：原名 + 恰好一个冲突副本（项目里还有别的.txt，不参与计数）。
		var group []string
		for k := range side.tree {
			if k == "冲突文档.txt" || strings.Contains(strings.TrimSuffix(k, ".txt"), "冲突-") {
				group = append(group, k)
			}
		}
		if len(group) != 2 {
			t.Errorf("%s 冲突组应该是 2 个文件（原名+冲突副本），实际：%s", side.name, jsonOf(group))
			continue
		}
		orig, ok := side.tree["冲突文档.txt"]
		if !ok {
			t.Errorf("%s 上原名 冲突文档.txt 不见了：%s", side.name, jsonOf(mapKeys(side.tree)))
			continue
		}
		copyName := ""
		for _, k := range group {
			if k != "冲突文档.txt" {
				copyName = k
			}
		}
		if !strings.Contains(copyName, "冲突") {
			t.Errorf("%s 的冲突副本名不带「冲突」：%s", side.name, copyName)
		}
		cp := side.tree[copyName]
		if !((orig == macSha && cp == ubSha) || (orig == ubSha && cp == macSha)) {
			t.Errorf("%s 冲突文件内容不对（两边版本必须各留一份）：原名=%s 副本(%s)=%s", side.name, orig, copyName, cp)
		}
	}
}

// S3b 改 vs 删：一边删、一边改 → 改的那份留下（不被删）。
func TestItestS3bModifyVsDelete(t *testing.T) {
	c := newCluster(t)
	c.startUnit(relA, map[string][]byte{"笔记.txt": []byte("base\n"), "陪跑.txt": []byte("陪跑\n")})
	c.round(true)

	if err := os.Remove(filepathJoin(c.sync, relA, "笔记.txt")); err != nil {
		t.Fatalf("Mac 删笔记.txt 失败：%v", err)
	}
	ubV := []byte("Ubuntu 上改过的笔记\n")
	c.writeUb(relA, map[string][]byte{"笔记.txt": ubV})
	c.sleepQuiet()

	rep := c.round(true)
	c.assertAligned(relA, rep)
	mac, ub := c.macFilesOf(relA), c.ubFilesOf(relA)
	if got := mac["笔记.txt"]; got != sha256Hex(ubV) {
		t.Errorf("Mac 上笔记.txt 应该是 Ubuntu 改过的那份（改的赢），sha=%s", got)
	}
	if got := ub["笔记.txt"]; got != sha256Hex(ubV) {
		t.Errorf("Ubuntu 上笔记.txt 被 Mac 的删除带走了？sha=%s", got)
	}
	t.Logf("S3b done=%s", jsonOf(unitResult(rep, codeA).Done))
}

// S4 删除传播：Mac 删一个文件 → Ubuntu 上也没了，且在 Ubuntu 的回收区能找到。
func TestItestS4DeleteToRecycle(t *testing.T) {
	c := newCluster(t)
	c.startUnit(relA, map[string][]byte{
		"素材/旧笔记.txt": []byte("旧笔记，将被 Mac 删掉\n"),
		"主文档.txt":     []byte("主文档\n"),
	})
	c.round(true)

	if err := os.Remove(filepathJoin(c.sync, relA, "素材/旧笔记.txt")); err != nil {
		t.Fatalf("Mac 删素材/旧笔记.txt 失败：%v", err)
	}
	c.sleepQuiet()
	rep := c.round(true)

	ub := c.ubFilesOf(relA)
	if _, ok := ub["素材/旧笔记.txt"]; ok {
		t.Errorf("Mac 删了，Ubuntu 上素材/旧笔记.txt 还在：%s", jsonOf(ub))
	}
	c.assertAligned(relA, rep)

	recycle := c.env.meta + "/回收"
	paths := ubuntuPaths(t, c.env.sf, recycle)
	found := false
	for _, p := range paths {
		if strings.HasSuffix(p, "旧笔记.txt") {
			found = true
		}
	}
	idx := ubuntuCat(t, c.env.sf, recycle+"/index.jsonl")
	if !found && !strings.Contains(idx, "旧笔记.txt") {
		t.Errorf("Ubuntu 回收区里找不到旧笔记.txt：paths=%s\nindex=%s", jsonOf(paths), idx)
	}
	t.Logf("S4 回收区：%s", jsonOf(paths))
}

// S13 改名不重传：Mac 把 5MB 文件改名并挪到别的子目录 → Ubuntu 对上，且不重新传这 5MB。
func TestItestS13RenameNoRetransfer(t *testing.T) {
	c := newCluster(t)
	const mb5 = 5 << 20
	big := bigFile(mb5)
	c.startUnit(relA, map[string][]byte{"素材/母带.bin": big, "说明.txt": []byte("说明\n")})
	c.round(true)

	// Mac 侧改名 + 挪目录（内容不动）。
	if err := os.MkdirAll(filepathJoin(c.sync, relA, "导出"), 0o755); err != nil {
		t.Fatalf("建导出目录失败：%v", err)
	}
	if err := os.Rename(filepathJoin(c.sync, relA, "素材/母带.bin"), filepathJoin(c.sync, relA, "导出/母带v2.bin")); err != nil {
		t.Fatalf("改名失败：%v", err)
	}
	c.sleepQuiet()
	rep := c.round(true)

	ub := c.ubFilesOf(relA)
	if _, ok := ub["导出/母带v2.bin"]; !ok {
		t.Errorf("Ubuntu 上没有改名后的 导出/母带v2.bin：%s", jsonOf(ub))
	}
	if _, ok := ub["素材/母带.bin"]; ok {
		t.Errorf("Ubuntu 上旧路径 素材/母带.bin 还在：%s", jsonOf(ub))
	}
	c.assertAligned(relA, rep)
	res := unitResult(rep, codeA)
	t.Logf("S13 done=%s bytes_up=%d", jsonOf(res.Done), res.BytesUp)
	if res.BytesUp >= mb5 {
		t.Errorf("改名后重新传了 %d 字节（>=5MB），应该靠改名对上而不是重传", res.BytesUp)
	}
	viaRename := false
	for _, d := range res.Done {
		if d.From == "素材/母带.bin" || strings.Contains(string(d.Kind), "rename") {
			viaRename = true
		}
	}
	if !viaRename {
		t.Errorf("报告里没有「改名对上」的痕迹（Done 里没有 from=素材/母带.bin 或 rename 类动作）：%s", jsonOf(res.Done))
	}
}

// 边界名：Office 锁 ~$报告.docx、.DS_Store、结尾带空格的文件 → 不同步，排除有账可查。
func TestItestNamesBoundary(t *testing.T) {
	c := newCluster(t)
	doc := []byte("正式报告\n")
	body := []byte("正文\n")
	c.startUnit(relA, map[string][]byte{"报告.docx": doc, "正文.txt": body})
	c.round(true)

	junk := map[string][]byte{
		"~$报告.docx": []byte("office-lock\n"),
		".DS_Store":   []byte("ds\n"),
		"工作笔记 ":      []byte("带尾空格\n"), // 结尾一个空格
	}
	c.writeMacInSync(relA, junk)
	c.sleepQuiet()
	rep := c.round(true)

	ub := c.ubFilesOf(relA)
	for name := range junk {
		if _, ok := ub[name]; ok {
			t.Errorf("边界名 %q 被同步到了 Ubuntu：%s", name, jsonOf(ub))
		}
	}
	if got := ub["报告.docx"]; got != sha256Hex(doc) {
		t.Errorf("正常文件 报告.docx 反而没传对：sha=%s", got)
	}
	// 排除的账：Mac 侧用生产同一套扫描规则能看到这三个被排除（§4.1 排除=不同步、只计数，
	// 轮报告里没有逐条 Skipped，这是产品的既有口径，日志里给全）。
	excl := c.macExcluded(relA)
	for name := range junk {
		hit := false
		for _, p := range excl {
			if p == name {
				hit = true
			}
		}
		if !hit {
			t.Errorf("扫描排除清单里没有 %q：%s", name, jsonOf(excl))
		}
	}
	t.Logf("本轮报告：%s", jsonOf(rep))
	t.Logf("Mac 清单=%s Ubuntu 清单=%s", jsonOf(mapKeys(c.macFilesOf(relA))), jsonOf(mapKeys(ub)))
}

// t2logTreeDiff 打两边清单（失败现场用）。
func (c *cluster) t2logTreeDiff(rel string) {
	c.t.Helper()
	c.t.Logf("%s Mac=%s Ubuntu=%s", rel, jsonOf(mapKeys(c.macFilesOf(rel))), jsonOf(mapKeys(c.ubFilesOf(rel))))
}

// filepathJoin 拼本机路径（rel 用 / 写，这里转）。
func filepathJoin(base string, rels ...string) string {
	p := base
	for _, r := range rels {
		p += string(os.PathSeparator) + filepathFromSlash(r)
	}
	return p
}

func filepathFromSlash(s string) string { return strings.ReplaceAll(s, "/", string(os.PathSeparator)) }
