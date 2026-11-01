//go:build itest

// S21：同编号两个目录、其中一个没配置（两机联调，§14.2）。
//
// 剧本（复核者定的）：
//  1. Mac 开始同步 2026/260403J1301-测试项目己 → 同步一轮到 Ubuntu；
//  2. 在 Ubuntu 上另建同编号、不同 rel 的目录 2026/260403J1301-测试项目己-副本，放一个文件（这个没配置）；
//     同时往已登记的目录里也加一个文件（证明登记的照常同步）；
//  3. 跑一轮 → 断言：
//     - 登记的单元照常同步、没被同编号副本拖停（Mac 拿到了登记目录里新加的文件）；
//     - 本轮报告里那个副本目录是「没配置」（Why=unconfigured，Note=PFNoteUnconfigured，只报告）；
//     - Mac 上不出现副本目录（没配置的内容不过来了），Ubuntu 上副本内容原样不动；
//     - 登记目录两侧一致。
//
// 比较一致性用 projsync 自己的 PFExclude 规则滤掉不同步的内部文件（沿用 S20 的 pfFilterTree）。
// CLI / 写操作一律经 runner / SFTP 在 Ubuntu 上做，不在 Ubuntu 本机跑 go test。

package itest

import (
	"os"
	"testing"

	"github.com/WillyChen0811/base/go/projsync"
)

const (
	relF     = "2026/260403J1301-测试项目己"
	relFCopy = "2026/260403J1301-测试项目己-副本" // Ubuntu 上另建、没配置
)

func TestItestS21SameCodeTwoDirs(t *testing.T) {
	c := newCluster(t)

	// 1) Mac 开始同步主目录，一轮到 Ubuntu。
	c.startUnit(relF, map[string][]byte{
		"需求.docx":   []byte("需求 v1\n"),
		"现场/记录.txt": []byte("记录 v1\n"),
	})
	rep := c.round(true)
	c.assertAligned(relF, rep)

	// 2) Ubuntu 上另建同编号、不同 rel 的副本目录（没配置）；并往登记目录加一个新文件。
	copyBody := []byte("副本里不该同步的内容\n")
	c.writeUb(relFCopy, map[string][]byte{"闲.txt": copyBody})
	newInReg := []byte("登记目录里新加的文件\n")
	c.writeUb(relF, map[string][]byte{"新到.txt": newInReg})
	c.sleepQuiet()

	// 3) 再跑一轮。
	rep2 := c.round(true)

	// 登记单元照常同步：Mac 拿到了登记目录里新加的文件（没被同编号副本拖停）。
	if got := c.macFilesOf(relF)["新到.txt"]; got != sha256Hex(newInReg) {
		t.Errorf("登记单元应照常同步新到.txt 到 Mac：sha=%s 应为 %s（本轮=%s）", got, sha256Hex(newInReg), jsonOf(rep2))
	}
	// 本轮没有任何单元因同编号重复被停下。
	for _, u := range rep2.Units {
		if u.Why == "duplicate" {
			t.Errorf("同编号有没配置的目录时不该再 duplicate 停整个项目：%+v", u)
		}
	}

	// 报告里副本目录是「没配置」。
	foundCopy := false
	for _, u := range rep2.Units {
		if u.Rel == relFCopy {
			foundCopy = true
			if u.Action != projsync.PFSchedActNotDone || u.Why != "unconfigured" || u.Note != projsync.PFNoteUnconfigured {
				t.Errorf("副本目录应只报告没配置：%+v", u)
			}
		}
	}
	if !foundCopy {
		t.Errorf("本轮报告没列出没配置的副本目录 %s：%s", relFCopy, jsonOf(rep2))
	}

	// 登记单元自己的结果里也只是一条「报告」级的 G4（同编号还有别的目录），不是停。
	if res := unitResult(rep2, "260403J1301"); res == nil {
		t.Errorf("本轮报告里没有登记单元 260403J1301 的结果：%s", jsonOf(rep2))
	} else {
		for _, g := range res.Gates {
			if g.Code == "G4" && g.Effect != projsync.PFGateReport {
				t.Errorf("同编号别的目录在登记单元里只该是报告级的 G4：%+v", g)
			}
		}
	}

	// Mac 上不出现副本目录（没配置的内容不过来了）。
	if _, err := os.Stat(filepathJoin(c.sync, relFCopy)); err == nil {
		t.Errorf("没配置的副本目录不该同步到 Mac：%s", filepathJoin(c.sync, relFCopy))
	}

	// Ubuntu 上副本内容原样不动。
	if got := c.ubFilesOf(relFCopy)["闲.txt"]; got != sha256Hex(copyBody) {
		t.Errorf("Ubuntu 副本内容不该被动：sha=%s 应为 %s", got, sha256Hex(copyBody))
	}

	// 登记目录两侧仍一致（滤掉不同步的内部文件后）。
	mac, ub := pfFilterTree(c.macFilesOf(relF)), pfFilterTree(c.ubFilesOf(relF))
	onlyMac, onlyUb, differ := cmpTree(mac, ub)
	if len(onlyMac)+len(onlyUb)+len(differ) > 0 {
		t.Errorf("登记目录两侧不一致：Mac 独有 %v，Ubuntu 独有 %v，内容不同 %v", onlyMac, onlyUb, differ)
		c.t.Logf("Mac=%s Ubuntu=%s", jsonOf(mapKeys(mac)), jsonOf(mapKeys(ub)))
	}
}
