package projsync

// pfnames.go 的测试：全是纯函数，不建真实文件，所以在 APFS（不区分大小写、对 NFC/NFD 不敏感）上也能跑。
// 金样来自 testdata/pfnames_edge.json（synctest 虚构数据 manifest 里的 edge/lock/system/symlink 路径，
// 名字原始字节用 \u 转义保存，NFC/NFD 不会被编辑器改掉）。
// 下面源码里写 NFC/NFD 的地方一律用 \u 转义，不直接写带组合符的字面量。

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	pfCafeNFC = "café.txt"
	pfCafeNFD = "café.txt"
	pfGaNFC   = "が.txt"  // が
	pfGaNFD   = "が.txt" // か + 濁点
)

// ---------------------------------------------------------------------------------------------
// §4.1 排除
// ---------------------------------------------------------------------------------------------

func TestPFExclude(t *testing.T) {
	const hist = "项目/6-其他/历史版本"
	cases := []struct {
		name   string
		isDir  bool
		parent string
		want   bool
		reason string // 原因里应含的关键词（want 为 true 时）
	}{
		// . 开头：文件和目录
		{".DS_Store", false, "", true, "隐藏"},
		{"._report.docx", false, "a/b", true, "隐藏"},
		{".~lock.需求表.xlsx#", false, "", true, "隐藏"},
		{".Spotlight-V100", true, "", true, "隐藏"},
		{".Trashes", true, "", true, "隐藏"},
		{".fseventsd", true, "", true, "隐藏"},
		{".TemporaryItems", true, "", true, "隐藏"},
		{".peerctl-state", false, "", true, "隐藏"},
		{".projsync-tmp-abc123", false, "x", true, "隐藏"},
		{".lock", false, "x", true, "隐藏"},   // 历史版本之外，.lock 也是 . 开头
		{"..icloud", false, "", true, "隐藏"}, // X 为空，不是占位文件
		// iCloud 占位：排除，原因单独计数
		{".report.docx.icloud", false, "x", true, "iCloud"},
		{".报告.pages.icloud", false, "", true, "iCloud"},
		// Office 与系统垃圾
		{"~$report.docx", false, "", true, "Office 锁"},
		{"~$cument.doc", false, "", true, "Office 锁"},
		{"~$委托单.docx", false, "", true, "Office 锁"},
		{"~WRL0001.tmp", false, "", true, "~WRL"},
		{"~wrl1234.TMP", false, "", true, "~WRL"},
		{"Thumbs.db", false, "", true, "Thumbs.db"},
		{"thumbs.db", false, "", true, "Thumbs.db"},
		{"desktop.ini", false, "", true, "desktop.ini"},
		{"Desktop.ini", false, "", true, "desktop.ini"},
		{"Icon\r", false, "", true, "Icon"},
		// 下载与临时文件
		{"大文件.mp4.part", false, "", true, ".part"},
		{"a.PART", false, "", true, ".part"},
		{"x.tmp", false, "", true, ".tmp"},
		{"x.TMP", false, "", true, ".tmp"},
		{"video.crdownload", false, "", true, ".crdownload"},
		{"file.download", false, "", true, ".download"},
		// 历史版本内部文件
		{".lock", false, hist, true, "历史版本内部"},
		{".index-3f2a9c", false, hist, true, "历史版本内部"},
		{"档案台账.sqlite3-journal", false, hist, true, "sqlite3-journal"},
		{"档案台账.sqlite3-wal", false, hist, true, "-wal"},
		{"档案台账.sqlite3-shm", false, hist, true, "-shm"},
		{"档案台账.sqlite3-journal", false, "历史版本", true, "sqlite3-journal"},         // 历史版本在根
		{"x.sqlite3-journal", false, hist + "/文档/f001", true, "sqlite3-journal"}, // 更深处也算
		{".index-1", false, "a/历史版本/b", true, "历史版本内部"},

		// ---- 反例：不排除 ----
		{"报告.docx", false, "", false, ""},
		{"report.docx", false, "a/b", false, ""},
		{"Report.docx", false, "", false, ""},
		{"Icon", false, "", false, ""},  // 没有 \r
		{"Icon\r", true, "", false, ""}, // 目录不按名字规则排除
		{"~$x", true, "", false, ""},    // 同上
		{"备份.tmp", true, "", false, ""}, // 目录里可能有用户资料
		{"a~$b.docx", false, "", false, ""},
		{"~report.docx", false, "", false, ""},
		{"~WRL0001.doc", false, "", false, ""},
		{"thumbs.db.bak", false, "", false, ""},
		{"desktop.ini.txt", false, "", false, ""},
		{"x.partial", false, "", false, ""},
		{"x.tmpl", false, "", false, ""},
		{"tmp", false, "", false, ""},
		{"part", false, "", false, ""},
		{"x.download.docx", false, "", false, ""},
		// 历史版本内的正常文件：台账一对和快照要同步（§7）
		{"档案台账.sqlite3", false, hist, false, ""},
		{"档案索引.json", false, hist, false, ""},
		{"V001_ab12cd34_报告.docx", false, hist + "/文档/f001", false, ""},
		// 历史版本之外：这三个后缀不排除
		{"x.sqlite3-journal", false, "项目/6-其他", false, ""},
		{"x-wal", false, "项目", false, ""},
		{"x-shm", false, "", false, ""},
		{"x-wal", false, "项目/历史版本备份", false, ""},    // 段不是正好等于「历史版本」
		{"x-wal", false, "项目/历史版本/x", true, "-wal"}, // 路径里有段等于历史版本
		// 名字本身等于「历史版本」的目录不排除（它的内容才受规则管）
		{"历史版本", true, "项目/6-其他", false, ""},
	}
	for _, c := range cases {
		reason, got := PFExclude(c.name, c.isDir, c.parent)
		if got != c.want {
			t.Errorf("PFExclude(%q, dir=%v, parent=%q) = %v（原因 %q），期望 %v", c.name, c.isDir, c.parent, got, reason, c.want)
			continue
		}
		if got && !strings.Contains(reason, c.reason) {
			t.Errorf("PFExclude(%q, dir=%v, parent=%q) 原因 %q 不含 %q", c.name, c.isDir, c.parent, reason, c.reason)
		}
		if !got && reason != "" {
			t.Errorf("PFExclude(%q) 不排除时原因应为空，得到 %q", c.name, reason)
		}
	}
}

func TestPFICloudPlaceholder(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{".report.docx.icloud", "report.docx", true},
		{".报告.pages.icloud", "报告.pages", true},
		{".x.icloud", "x", true},
		{"..x.icloud", ".x", true},
		{".icloud.icloud", "icloud", true},
		{".a b.icloud", "a b", true},
		{"..icloud", "", false}, // X 为空
		{".icloud", "", false},
		{"report.icloud", "", false}, // 没有开头的点
		{".report.docx", "", false},
		{".report.docx.icloud.bak", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := PFICloudPlaceholder(c.name)
		if got != c.want || ok != c.ok {
			t.Errorf("PFICloudPlaceholder(%q) = (%q, %v)，期望 (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// §4.2 第 1、2、6 条：名字检查
// ---------------------------------------------------------------------------------------------

func TestPFCheckPath(t *testing.T) {
	seg := func(n int) string { return strings.Repeat("a", n) }
	cjk := func(n int) string { return strings.Repeat("字", n) } // 每个 3 字节
	// 总长恰好 1000 与 1001 字节的路径
	p1000 := strings.Join([]string{seg(250), seg(250), seg(250), seg(247)}, "/")
	p1001 := strings.Join([]string{seg(250), seg(250), seg(250), seg(248)}, "/")

	cases := []struct {
		name   string
		rel    string
		isDir  bool
		reason string // 为空表示应在范围内；否则原因应含此关键词
	}{
		// 第 1 条：UTF-8 与控制字符
		{"非法 UTF-8", "a\xffb.txt", false, "UTF-8"},
		{"非法 UTF-8（目录段）", "x/\xc3\x28/y.txt", false, "UTF-8"},
		{"截断的多字节", "报\xe5\x91.txt", false, "UTF-8"},
		{"NUL", "a\x00b.txt", false, "控制字符"},
		{"换行", "a\nb.txt", false, "控制字符"},
		{"制表符", "a\tb.txt", false, "控制字符"},
		{"U+001F", "a\x1fb", false, "控制字符"},
		{"DEL", "a\x7fb.txt", false, "控制字符"},
		{"目录段含控制字符", "x\ry/b.txt", false, "控制字符"},
		{"正常：空格", "a b.txt", false, ""},
		{"正常：全角括号", "（报告）.docx", false, ""},
		{"正常：全角标点【】、，", "需求表（第二版）【终稿】、确认，定稿.xlsx", false, ""},
		{"正常：带圈数字", "①②③.txt", false, ""},
		{"正常：罗马数字", "第Ⅱ章.docx", false, ""},
		{"正常：emoji", "📎附件.txt", false, ""},
		{"正常：日文假名 NFC", pfGaNFC, false, ""},
		{"正常：日文假名 NFD", pfGaNFD, false, ""},
		{"正常：café NFD", pfCafeNFD, false, ""},
		{"正常：全角英文", "ＡＢＣ．ｔｘｔ", false, ""},
		{"正常：假名与片假名", "ひらがなカタカナ.txt", false, ""},
		{"正常：前后空格", " 前面有空格.txt", false, ""},
		{"正常：以点结尾", "以点结尾.", false, ""},
		{"正常：多级路径", "Projects/2026/261001J0101-测试项目甲/1-客户资料提供/a.docx", false, ""},
		// 第 2 条：长度
		{"单段 255 字节", seg(251) + ".txt", false, ""},
		{"单段 256 字节", seg(252) + ".txt", false, "255"},
		{"85 个汉字 = 255 字节", cjk(85), false, ""},
		{"86 个汉字 = 258 字节（不足 255 字符但超 255 字节）", cjk(86), false, "255"},
		{"目录段 256 字节", "x/" + seg(256) + "/b.txt", false, "255"},
		{"整条路径 1000 字节", p1000, false, ""},
		{"整条路径 1001 字节", p1001, false, "1000"},
		{"6 段各 200 字节", strings.Join([]string{seg(200), seg(200), seg(200), seg(200), seg(200), seg(200)}, "/"), false, "1000"},
		// 第 6 条：Mac 包目录（只对目录）
		{"Foo.app 目录", "Foo.app", true, ".app"},
		{"Foo.app 是文件时正常", "Foo.app", false, ""},
		{"穿过 .app 目录的文件", "x/Foo.app/Contents/Info.plist", false, ".app"},
		{"大写扩展名", "演示.KEY", true, ".key"},
		{".pages 目录", "报告.pages", true, ".pages"},
		{".numbers 目录", "表.numbers", true, ".numbers"},
		{".key 目录", "演示.key", true, ".key"},
		{".rtfd 目录", "文.rtfd", true, ".rtfd"},
		{".photoslibrary 目录", "照片.photoslibrary", true, ".photoslibrary"},
		{".bundle 目录", "x.bundle", true, ".bundle"},
		{".fcpbundle 目录", "剪辑.fcpbundle", true, ".fcpbundle"},
		{".imovielibrary 目录", "x.imovielibrary", true, ".imovielibrary"},
		{".pages 文件（新版 Pages 是单文件）正常", "报告.pages", false, ""},
		{".key 文件正常", "演示.key", false, ""},
		{"类似扩展名的目录正常：.application", "x.application", true, ""},
		{"没有扩展名的 app 目录正常", "app", true, ""},
		{"普通目录正常", "6-其他", true, ""},
		// 路径结构（对应 validRel）
		{"空路径", "", false, "为空"},
		{"以 / 开头", "/a/b", false, "/"},
		{"含反斜杠", "a\\b.txt", false, "反斜杠"},
		{"空段", "a//b", false, "非法段"},
		{"末尾斜杠", "a/b/", false, "非法段"},
		{"点段", "a/./b", false, "非法段"},
		{"点点段", "a/../b", false, "非法段"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PFCheckPath(c.rel, c.isDir)
			if c.reason == "" {
				if got != nil {
					t.Fatalf("PFCheckPath(%q, dir=%v) 应在范围内，得到 %v", c.rel, c.isDir, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("PFCheckPath(%q, dir=%v) 应超出范围（含 %q），得到 nil", c.rel, c.isDir, c.reason)
			}
			if got.Path != c.rel {
				t.Errorf("Path = %q，期望 %q", got.Path, c.rel)
			}
			if !strings.Contains(got.Reason, c.reason) {
				t.Errorf("原因 %q 不含 %q", got.Reason, c.reason)
			}
			var err error = got // 实现 error
			if !strings.Contains(err.Error(), "超出同步范围") {
				t.Errorf("Error() = %q，应含「超出同步范围」", err.Error())
			}
		})
	}
}

func TestPFCheckName(t *testing.T) {
	ok := []struct {
		name  string
		isDir bool
	}{{"报告.docx", false}, {"Foo.app", false}, {"6-其他", true}, {"📎", false}}
	for _, c := range ok {
		if got := PFCheckName(c.name, c.isDir); got != nil {
			t.Errorf("PFCheckName(%q, %v) 应在范围内，得到 %v", c.name, c.isDir, got)
		}
	}
	bad := []struct {
		name   string
		isDir  bool
		reason string
	}{
		{"a/b", false, "/"},
		{"", false, "为空"},
		{".", true, "为空"},
		{"..", true, "为空"},
		{"a\x01", false, "控制字符"},
		{"Foo.app", true, ".app"},
	}
	for _, c := range bad {
		got := PFCheckName(c.name, c.isDir)
		if got == nil || !strings.Contains(got.Reason, c.reason) || got.Path != c.name {
			t.Errorf("PFCheckName(%q, %v) = %v，期望超出范围（含 %q）", c.name, c.isDir, got, c.reason)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// 折叠键、撞名
// ---------------------------------------------------------------------------------------------

func TestPFFoldKey(t *testing.T) {
	same := [][2]string{
		{"Report.docx", "report.docx"},
		{"REPORT.DOCX", "report.docx"},
		{pfCafeNFC, pfCafeNFD},
		{pfGaNFC, pfGaNFD},
		{"CAF\u00c9.txt", pfCafeNFD},  // 大写 + NFC 对小写 + NFD
		{"Ｒｅｐｏｒｔ.txt", "ｒｅｐｏｒｔ.txt"},  // 全角大小写也折叠
		{"第Ⅱ章", "第ⅱ章"},                // 罗马数字大小写
		{"Straße.txt", "STRASSE.txt"}, // 完整折叠：比 APFS 宽，宁可多判
		{"Σ", "ς"},
	}
	for _, c := range same {
		if a, b := PFFoldKey(c[0]), PFFoldKey(c[1]); a != b {
			t.Errorf("PFFoldKey(%q)=%q 与 PFFoldKey(%q)=%q 应相同", c[0], a, c[1], b)
		}
	}
	diff := [][2]string{
		{"a.txt", "b.txt"},
		{"Ａ.txt", "A.txt"}, // 全角和半角不是同一个字（NFC 不做兼容分解）
		{"①.txt", "1.txt"}, // 带圈数字
		{"Ⅱ.txt", "II.txt"},
		{"（报告）.docx", "(报告).docx"}, // 全角括号
		{"报告.docx", "报告 .docx"},
		{"报告.docx", "报告.doc"},
		{"📎.txt", "📌.txt"},
		{"\xff", "\xfe"}, // 非法 UTF-8 不合并
		{"\xff", "�"},
	}
	for _, c := range diff {
		if a, b := PFFoldKey(c[0]), PFFoldKey(c[1]); a == b {
			t.Errorf("PFFoldKey(%q) 与 PFFoldKey(%q) 都是 %q，应不同", c[0], c[1], a)
		}
	}
	// 折叠键本身是 NFC 小写：不依赖输入的写法
	if got, want := PFFoldKey(pfCafeNFD), pfCafeNFC; got != want {
		t.Errorf("PFFoldKey(%q) = %q，期望 NFC 小写 %q", pfCafeNFD, got, want)
	}
}

func TestPFSameSideClashes(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  [][]string
	}{
		{"没有名字", nil, nil},
		{"没有撞名", []string{"a.docx", "b.docx", "报告.docx"}, nil},
		{"完全相同的重复名字不算", []string{"a.txt", "a.txt"}, nil},
		{"只差大小写", []string{"Report.docx", "report.docx", "other.docx"}, [][]string{{"Report.docx", "report.docx"}}},
		{"NFC 对 NFD（组内按字节序，NFD 的 e 在前）", []string{pfCafeNFC, pfCafeNFD}, [][]string{{pfCafeNFD, pfCafeNFC}}},
		{"假名 NFC 对 NFD（组内按字节序，NFD 的 か 在前）", []string{pfGaNFC, pfGaNFD}, [][]string{{pfGaNFD, pfGaNFC}}},
		{"一组三个", []string{"a.txt", "A.txt", "a.TXT"}, [][]string{{"A.txt", "a.TXT", "a.txt"}}},
		{"两组，按第一个名字排序", []string{"b.txt", "B.TXT", "a.txt", "A.txt"}, [][]string{{"A.txt", "a.txt"}, {"B.TXT", "b.txt"}}},
		{"目录和文件也撞", []string{"Docs", "docs"}, [][]string{{"Docs", "docs"}}},
		{"全角与半角不撞", []string{"Ａ.txt", "A.txt"}, nil},
		{"带圈数字与数字不撞", []string{"①.txt", "1.txt"}, nil},
		{"全角括号与半角不撞", []string{"（报告）.docx", "(报告).docx"}, nil},
		{"emoji 不撞", []string{"📎.txt", "📌.txt"}, nil},
		{"非法 UTF-8 各自独立", []string{"\xff.txt", "\xfe.txt"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PFSameSideClashes(c.names)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("PFSameSideClashes(%q) = %q，期望 %q", c.names, got, c.want)
			}
		})
	}
}

func TestPFCrossSideClashes(t *testing.T) {
	cases := []struct {
		name        string
		mac, ubuntu []string
		want        []PFCrossClash
	}{
		{"两边都空", nil, nil, nil},
		{"完全一致", []string{"a.docx", "报告.docx"}, []string{"a.docx", "报告.docx"}, nil},
		{"只在一边", []string{"a.docx"}, []string{"b.docx"}, nil},
		{"只差大小写", []string{"Report.docx"}, []string{"report.docx"}, []PFCrossClash{{"Report.docx", "report.docx"}}},
		{"Mac NFC、Ubuntu NFD", []string{pfCafeNFC}, []string{pfCafeNFD}, []PFCrossClash{{pfCafeNFC, pfCafeNFD}}},
		{"Mac NFD、Ubuntu NFC（假名）", []string{pfGaNFD}, []string{pfGaNFC}, []PFCrossClash{{pfGaNFD, pfGaNFC}}},
		{"目录段只差大小写", []string{"Docs"}, []string{"docs"}, []PFCrossClash{{"Docs", "docs"}}},
		{"其余照常，只报撞的那对", []string{"a.txt", "B.txt"}, []string{"a.txt", "b.txt", "c.txt"}, []PFCrossClash{{"B.txt", "b.txt"}}},
		{"Mac 有两种写法、Ubuntu 一种", []string{pfCafeNFD, pfCafeNFC}, []string{pfCafeNFC}, []PFCrossClash{{pfCafeNFD, pfCafeNFC}}},
		{"全角与半角不算", []string{"Ａ.txt"}, []string{"A.txt"}, nil},
		{"空格不同不算", []string{"报告.docx"}, []string{"报告 .docx"}, nil},
		{"按 Mac 名字再按 Ubuntu 名字排序", []string{"b.txt", "A.txt"}, []string{"B.TXT", "a.txt"}, []PFCrossClash{{"A.txt", "a.txt"}, {"b.txt", "B.TXT"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PFCrossSideClashes(c.mac, c.ubuntu)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("PFCrossSideClashes(%q, %q) = %v，期望 %v", c.mac, c.ubuntu, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Office 锁
// ---------------------------------------------------------------------------------------------

func TestPFOfficeLockCandidates(t *testing.T) {
	cases := []struct {
		x    string
		want []string
	}{
		// 主名 ≤6 个字符：只加 ~$
		{"report.docx", []string{"~$report.docx", ".~lock.report.docx#"}},
		{"file12.docx", []string{"~$file12.docx", ".~lock.file12.docx#"}},
		{"委托单.docx", []string{"~$委托单.docx", ".~lock.委托单.docx#"}},          // 按字符数，3 个字符（9 字节）
		{"报告报告报告.docx", []string{"~$报告报告报告.docx", ".~lock.报告报告报告.docx#"}}, // 6 个字符（18 字节）仍是 0
		{"README", []string{"~$README", ".~lock.README#"}},
		// 主名 ≥7 个字符：去掉 0/1/2 个字符都当候选
		{"file123.docx", []string{"~$file123.docx", "~$ile123.docx", "~$le123.docx", ".~lock.file123.docx#"}},
		{"Document.doc", []string{"~$Document.doc", "~$ocument.doc", "~$cument.doc", ".~lock.Document.doc#"}}, // 微软 KB 313472 的例子
		{"委托 单 副本.docx", []string{"~$委托 单 副本.docx", "~$托 单 副本.docx", "~$ 单 副本.docx", ".~lock.委托 单 副本.docx#"}}, // 7 个字符，按字符去掉
		// 只取最后一个扩展名
		{"data.backup.2024.xlsx", []string{"~$data.backup.2024.xlsx", "~$ata.backup.2024.xlsx", "~$ta.backup.2024.xlsx", ".~lock.data.backup.2024.xlsx#"}},
		// BMP 之外的字符按 UTF-16 码元算长度：4 个 emoji = 8 个码元
		{"📎📎📎.docx", []string{"~$📎📎📎.docx", ".~lock.📎📎📎.docx#"}},
		{"📎📎📎📎.docx", []string{"~$📎📎📎📎.docx", "~$📎📎📎.docx", "~$📎📎.docx", ".~lock.📎📎📎📎.docx#"}},
		// 输入是 NFD 时，~$ 名按 NFC 算，LibreOffice 名原样
		{"café.docx", []string{"~$café.docx", ".~lock.café.docx#"}},
	}
	for _, c := range cases {
		if got := PFOfficeLockCandidates(c.x); !reflect.DeepEqual(got, c.want) {
			t.Errorf("PFOfficeLockCandidates(%q) = %q，期望 %q", c.x, got, c.want)
		}
	}
}

func TestPFOfficeLocks(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		x     string
		want  []string
	}{
		{"Word 短名", []string{"report.docx", "~$report.docx"}, "report.docx", []string{"~$report.docx"}},
		{"Word 长名（KB 例子）", []string{"Document.doc", "~$cument.doc"}, "Document.doc", []string{"~$cument.doc"}},
		{"7 个字符，去 1 个", []string{"~$ile123.docx"}, "file123.docx", []string{"~$ile123.docx"}},
		{"7 个字符，去 2 个", []string{"~$le123.docx"}, "file123.docx", []string{"~$le123.docx"}},
		{"Excel 长名不去", []string{"~$file12345678.xlsx"}, "file12345678.xlsx", []string{"~$file12345678.xlsx"}},
		{"LibreOffice", []string{".~lock.需求表.xlsx#"}, "需求表.xlsx", []string{".~lock.需求表.xlsx#"}},
		{"两种锁都有，按 names 顺序", []string{".~lock.a.docx#", "a.docx", "~$a.docx"}, "a.docx", []string{".~lock.a.docx#", "~$a.docx"}},
		{"大小写不同也算", []string{"~$REPORT.DOCX"}, "report.docx", []string{"~$REPORT.DOCX"}},
		{"NFD 的锁对 NFC 的文件", []string{"~$café.docx"}, "café.docx", []string{"~$café.docx"}},
		{"无锁", []string{"report.docx", "other.docx", "~$other.docx"}, "report.docx", nil},
		{"LibreOffice 名缺 #", []string{".~lock.report.docx"}, "report.docx", nil},
		{"多了后缀不算", []string{"~$report.docx.bak", "report.docx.~lock"}, "report.docx", nil},
		{"名字列表为空", nil, "report.docx", nil},
		{"另一个文档的锁不算（短名）", []string{"~$委托单.docx"}, "委托 单 副本.docx", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PFOfficeLocks(c.names, c.x); !reflect.DeepEqual(got, c.want) {
				t.Errorf("PFOfficeLocks(%q, %q) = %q，期望 %q", c.names, c.x, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// §3.4 冲突副本命名
// ---------------------------------------------------------------------------------------------

func TestPFConflictName(t *testing.T) {
	ts := time.Date(2026, 10, 26, 15, 30, 45, 0, time.UTC)
	none := func(string) bool { return false }
	set := func(names ...string) func(string) bool { return NewPFNameSet(names).Has }

	long80 := strings.Repeat("字", 80)

	t.Run("基本格式", func(t *testing.T) {
		got, err := PFConflictName("报告.docx", "Mac", ts, none)
		if err != nil || got != "报告（冲突-Mac-1026-1530）.docx" {
			t.Fatalf("得到 (%q, %v)", got, err)
		}
	})
	t.Run("时间用 t 自己的时区，补零", func(t *testing.T) {
		tt := time.Date(2026, 1, 2, 3, 4, 59, 0, time.FixedZone("CST", 8*3600))
		got, err := PFConflictName("a.txt", "Ubuntu", tt, none)
		if err != nil || got != "a（冲突-Ubuntu-0102-0304）.txt" {
			t.Fatalf("得到 (%q, %v)", got, err)
		}
	})
	t.Run("重名加 -2、-3", func(t *testing.T) {
		n1 := "报告（冲突-Mac-1026-1530）.docx"
		n2 := "报告（冲突-Mac-1026-1530-2）.docx"
		n3 := "报告（冲突-Mac-1026-1530-3）.docx"
		if got, _ := PFConflictName("报告.docx", "Mac", ts, set(n1)); got != n2 {
			t.Errorf("一个已存在：得到 %q，期望 %q", got, n2)
		}
		if got, _ := PFConflictName("报告.docx", "Mac", ts, set(n1, n2)); got != n3 {
			t.Errorf("两个已存在：得到 %q，期望 %q", got, n3)
		}
		if got, _ := PFConflictName("报告.docx", "Mac", ts, set(n1, n3)); got != n2 {
			t.Errorf("-2 空着时用 -2：得到 %q，期望 %q", got, n2)
		}
		// 按折叠键判断：目标边大小写不同的同名也算已存在
		if got, _ := PFConflictName("report.docx", "Mac", ts, set("REPORT（冲突-MAC-1026-1530）.DOCX")); got != "report（冲突-Mac-1026-1530-2）.docx" {
			t.Errorf("折叠键判断：得到 %q", got)
		}
	})
	t.Run("多重扩展名只保留最后一个", func(t *testing.T) {
		got, _ := PFConflictName("a.tar.gz", "Mac", ts, none)
		if got != "a.tar（冲突-Mac-1026-1530）.gz" {
			t.Errorf("得到 %q", got)
		}
	})
	t.Run("没有扩展名、点结尾、点开头", func(t *testing.T) {
		for in, want := range map[string]string{
			"README":   "README（冲突-Mac-1026-1530）",
			"以点结尾.":    "以点结尾（冲突-Mac-1026-1530）.",
			".bashrc":  ".bashrc（冲突-Mac-1026-1530）",
			"a b .txt": "a b （冲突-Mac-1026-1530）.txt",
		} {
			if got, _ := PFConflictName(in, "Mac", ts, none); got != want {
				t.Errorf("PFConflictName(%q) = %q，期望 %q", in, got, want)
			}
		}
	})
	t.Run("超 255 字节按字符截短主名，保留扩展名", func(t *testing.T) {
		in := long80 + ".txt" // 244 字节
		got, err := PFConflictName(in, "Mac", ts, none)
		if err != nil {
			t.Fatal(err)
		}
		// 标记「（冲突-Mac-1026-1530）」26 字节 + ".txt" 4 字节 → 主名最多 225 字节 = 75 个字
		want := strings.Repeat("字", 75) + "（冲突-Mac-1026-1530）.txt"
		if got != want {
			t.Errorf("得到 %q（%d 字节），期望 %q", got, len(got), want)
		}
		got2, _ := PFConflictName(in, "Mac", ts, set(want))
		want2 := strings.Repeat("字", 74) + "（冲突-Mac-1026-1530-2）.txt"
		if got2 != want2 || len(got2) > 255 {
			t.Errorf("-2 版本：得到 %q（%d 字节），期望 %q", got2, len(got2), want2)
		}
	})
	t.Run("截短不切开多字节字符", func(t *testing.T) {
		in := strings.Repeat("📎", 70) + ".docx" // 285 字节，每个 emoji 4 字节
		got, err := PFConflictName(in, "Ubuntu", ts, none)
		if err != nil || !utf8.ValidString(got) || len(got) > 255 || !strings.HasSuffix(got, "（冲突-Ubuntu-1026-1530）.docx") {
			t.Fatalf("得到 (%q, %v) %d 字节", got, err, len(got))
		}
		if main, _, _ := strings.Cut(got, "（"); main != strings.Repeat("📎", len([]rune(main))) {
			t.Errorf("截短后的主名不是原名的前缀：%q", main)
		}
	})
	t.Run("扩展名长得放不下时放弃保留", func(t *testing.T) {
		in := "a." + strings.Repeat("x", 300)
		got, err := PFConflictName(in, "Mac", ts, none)
		if err != nil || len(got) > 255 || !utf8.ValidString(got) || !strings.Contains(got, "（冲突-Mac-1026-1530）") {
			t.Fatalf("得到 (%q, %v) %d 字节", got, err, len(got))
		}
	})
	t.Run("机器名清理", func(t *testing.T) {
		for machine, want := range map[string]string{
			"Mac/Book\n": "报告（冲突-Mac_Book_-1026-1530）.docx",
			"":           "报告（冲突-未知-1026-1530）.docx",
			"一二三四五六七八九十一二三四五六七八九十甲乙": "报告（冲突-一二三四五六七八九十一二三四五六七八九十-1026-1530）.docx",
		} {
			if got, _ := PFConflictName("报告.docx", machine, ts, none); got != want {
				t.Errorf("机器 %q：得到 %q，期望 %q", machine, got, want)
			}
		}
	})
	t.Run("结果通过名字检查且不被排除", func(t *testing.T) {
		for _, in := range []string{"报告.docx", long80 + ".txt", "a.tar.gz", "README"} {
			got, err := PFConflictName(in, "Mac", ts, none)
			if err != nil {
				t.Fatal(err)
			}
			if oos := PFCheckName(got, false); oos != nil {
				t.Errorf("%q 的冲突名 %q 超出范围：%v", in, got, oos)
			}
			if r, ex := PFExclude(got, false, ""); ex {
				t.Errorf("%q 的冲突名 %q 被排除：%s", in, got, r)
			}
		}
	})
	t.Run("exists 恒真时报错，不死循环", func(t *testing.T) {
		if got, err := PFConflictName("a.txt", "Mac", ts, func(string) bool { return true }); err == nil {
			t.Errorf("应报错，得到 %q", got)
		}
	})
	t.Run("名字不合法时报错", func(t *testing.T) {
		for _, in := range []string{"", "a/b.txt", "a\xff.txt"} {
			if got, err := PFConflictName(in, "Mac", ts, none); err == nil {
				t.Errorf("PFConflictName(%q) 应报错，得到 %q", in, got)
			}
		}
	})
	t.Run("exists 为 nil 当作都不存在", func(t *testing.T) {
		if got, err := PFConflictName("a.txt", "Mac", ts, nil); err != nil || got != "a（冲突-Mac-1026-1530）.txt" {
			t.Errorf("得到 (%q, %v)", got, err)
		}
	})
}

func TestPFNameSet(t *testing.T) {
	s := NewPFNameSet([]string{"Report.docx", pfCafeNFC})
	for _, n := range []string{"Report.docx", "report.docx", "REPORT.DOCX", pfCafeNFC, pfCafeNFD} {
		if !s.Has(n) {
			t.Errorf("Has(%q) 应为 true", n)
		}
	}
	for _, n := range []string{"report.doc", "other.txt", ""} {
		if s.Has(n) {
			t.Errorf("Has(%q) 应为 false", n)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// 金样：testdata/pfnames_edge.json
// ---------------------------------------------------------------------------------------------

type pfEdgeEntry struct {
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	Desc          string `json:"desc"`
	Excluded      bool   `json:"excluded"`
	ExcludeReason string `json:"exclude_reason"`
	OutOfScope    bool   `json:"out_of_scope"`
	FoldKey       string `json:"fold_key"`
}

type pfEdgeFile struct {
	Parent         string        `json:"parent"`
	Entries        []pfEdgeEntry `json:"entries"`
	DirNames       []string      `json:"dir_names"`
	SameSideGroups [][]string    `json:"same_side_groups"`
	LockCases      []struct {
		X    string   `json:"x"`
		Want []string `json:"want"`
		Desc string   `json:"desc"`
	} `json:"lock_cases"`
}

func pfLoadEdge(t *testing.T) *pfEdgeFile {
	t.Helper()
	b, err := os.ReadFile("testdata/pfnames_edge.json")
	if err != nil {
		t.Fatal(err)
	}
	var f pfEdgeFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) != 20 {
		t.Fatalf("金样应有 20 条（manifest 里 edge/lock/system/symlink 共 20 条），得到 %d", len(f.Entries))
	}
	return &f
}

func TestPFEdgeGolden(t *testing.T) {
	f := pfLoadEdge(t)

	// 先确认 JSON 里的字节没被改：NFC 与 NFD 的 café、が 各一条，且字节不同
	have := map[string]int{}
	for _, e := range f.Entries {
		name := e.Path[strings.LastIndex(e.Path, "/")+1:]
		have[name]++
	}
	for _, n := range []string{pfCafeNFC, pfCafeNFD, pfGaNFC, pfGaNFD, "Report.docx", "report.docx", "📎附件.txt"} {
		if have[n] != 1 {
			t.Fatalf("金样里名字 %q 应恰好 1 条，得到 %d（JSON 里的 \\u 转义被改动了？）", n, have[n])
		}
	}

	for _, e := range f.Entries {
		slash := strings.LastIndex(e.Path, "/")
		parent, name := e.Path[:slash], e.Path[slash+1:]
		t.Run(e.Kind+"/"+strings.ToValidUTF8(name, "?"), func(t *testing.T) {
			reason, ex := PFExclude(name, false, parent)
			if ex != e.Excluded {
				t.Fatalf("PFExclude(%q) = %v（%s），期望 %v（%s）", name, ex, reason, e.Excluded, e.Desc)
			}
			if ex {
				if !strings.Contains(reason, e.ExcludeReason) {
					t.Errorf("排除原因 %q 不含 %q", reason, e.ExcludeReason)
				}
				return // 排除的名字不再检查
			}
			oos := PFCheckPath(e.Path, false)
			if (oos != nil) != e.OutOfScope {
				t.Errorf("PFCheckPath(%q) = %v，期望超出范围=%v（%s）", e.Path, oos, e.OutOfScope, e.Desc)
			}
			if got := PFFoldKey(name); got != e.FoldKey {
				t.Errorf("PFFoldKey(%q) = %q，期望 %q", name, got, e.FoldKey)
			}
		})
	}

	// 目录 parent 下的名字：撞名组、两边对不上、锁
	var live []string // 非排除的名字
	for _, n := range f.DirNames {
		if _, ex := PFExclude(n, false, f.Parent); !ex {
			live = append(live, n)
		}
	}
	if got := PFSameSideClashes(live); !reflect.DeepEqual(got, f.SameSideGroups) {
		t.Errorf("PFSameSideClashes(目录名字) = %q，期望 %q", got, f.SameSideGroups)
	}

	// Ubuntu 只有 Report.docx、café（NFC）、が（NFC），没有 report.docx 和两个 NFD 写法，其余与 Mac 相同
	// → Mac 多出来的三个名字各自对上 Ubuntu 的那一个，按 Mac 名字的字节序排列。
	var ubuntu []string
	for _, n := range live {
		if n != "report.docx" && n != pfCafeNFD && n != pfGaNFD {
			ubuntu = append(ubuntu, n)
		}
	}
	wantCross := []PFCrossClash{
		{Mac: pfCafeNFD, Ubuntu: pfCafeNFC},
		{Mac: "report.docx", Ubuntu: "Report.docx"},
		{Mac: pfGaNFD, Ubuntu: pfGaNFC},
	}
	if got := PFCrossSideClashes(live, ubuntu); !reflect.DeepEqual(got, wantCross) {
		t.Errorf("PFCrossSideClashes = %v，期望 %v", got, wantCross)
	}
	// 两边完全一样（且各边内部没有撞名）→ 没有跨边差异
	if got := PFCrossSideClashes(ubuntu, ubuntu); got != nil {
		t.Errorf("两边一样时应没有跨边差异，得到 %v", got)
	}

	for _, c := range f.LockCases {
		want := c.Want
		if len(want) == 0 {
			want = nil // JSON 里的 [] 解出来是空切片，函数没找到时返回 nil
		}
		if got := PFOfficeLocks(f.DirNames, c.X); !reflect.DeepEqual(got, want) {
			t.Errorf("PFOfficeLocks(目录名字, %q) = %q，期望 %q（%s）", c.X, got, want, c.Desc)
		}
	}
}
