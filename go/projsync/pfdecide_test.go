package projsync

// 项目文件同步 T5「三方判定」的测试（纯函数，不碰文件系统，除了一个用真 PFBase 的兼容性测试）。
// 全部是虚构数据；表格驱动，判定表第 1–17 行每一行至少一个用例。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------------------------

var pfdtNow = time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)

const pfdtT0 = int64(1700000000) // 默认修改时间（秒）：早就静止了

func pfdtAt(h, m, s int) int64 { return time.Date(2026, 10, 26, h, m, s, 0, time.UTC).Unix() }

var (
	pfdtMacDir = PFDirID{Dev: 1, Ino: 100}
	pfdtUbDir  = PFDirID{Dev: 2, Ino: 200}
)

const pfdtRel = "2026/261001J0101-测试项目甲"

func pfdtSHA(content string) string {
	s := sha256.Sum256([]byte(content))
	return hex.EncodeToString(s[:])
}

func pfdtIno(path string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(path))
	return h.Sum64()>>1 | 1
}

// pfdtF 造一个已哈希的文件条目；mtime 缺省 pfdtT0。
func pfdtF(path, content string, mtime ...int64) PFScanItem {
	mt := pfdtT0
	if len(mtime) > 0 {
		mt = mtime[0]
	}
	return PFScanItem{Path: path, Kind: PFKindFile, HasStat: true, State: PFStateOK, Hash: pfdtSHA(content),
		Stat: PFStat{Kind: PFKindFile, Size: int64(len(content)), MtimeNs: mt * int64(time.Second), Ino: pfdtIno(path)}}
}

func pfdtD(path string) PFScanItem {
	return PFScanItem{Path: path, Kind: PFKindDir, HasStat: true, State: PFStateOK,
		Stat: PFStat{Kind: PFKindDir, MtimeNs: pfdtT0 * int64(time.Second), Ino: pfdtIno(path)}}
}

// pfdtUnk 造一个「不明」的文件条目（没有哈希）。
func pfdtUnk(path, code, reason string) PFScanItem {
	return PFScanItem{Path: path, Kind: PFKindFile, HasStat: true, State: PFStateUnknown, Code: code, Reason: reason,
		Stat: PFStat{Kind: PFKindFile, Size: 5, MtimeNs: pfdtT0 * int64(time.Second), Ino: pfdtIno(path)}}
}

func pfdtNotQuiet(path string) PFScanItem { return pfdtUnk(path, PFCodeNotQuiet, "未静止") }

func pfdtPlaceholder(path string) PFScanItem {
	return pfdtUnk(path, PFCodeICloudPlaceholder, "iCloud 占位文件，内容只在云端，不读取")
}

// pfdtLink 造一个符号链接条目（超出范围，没进去看）。
func pfdtLink(path string) PFScanItem {
	return PFScanItem{Path: path, Kind: PFKindSymlink, HasStat: true, State: PFStateOutOfScope, Code: PFCodeSymlink,
		Reason: "符号链接：不跟随、不进入", Stat: PFStat{Kind: PFKindSymlink, Ino: pfdtIno(path)}}
}

// pfdtBadDir 造一个读不了的目录条目（没进去看）。
func pfdtBadDir(path string) PFScanItem {
	return PFScanItem{Path: path, Kind: PFKindDir, HasStat: true, State: PFStateOutOfScope, Code: PFCodeUnreadable,
		Reason: "读不了目录: permission denied", Stat: PFStat{Kind: PFKindDir, Ino: pfdtIno(path)}}
}

// pfdtPlaceholderDir 造一个 iCloud 占位目录（不明，没进去看）。
func pfdtPlaceholderDir(path string) PFScanItem {
	return PFScanItem{Path: path, Kind: PFKindDir, HasStat: true, State: PFStateUnknown, Code: PFCodeICloudPlaceholder,
		Reason: "iCloud 占位目录，内容只在云端，不进入", Stat: PFStat{Kind: PFKindDir, Ino: pfdtIno(path)}}
}

// pfdtBase 是内存版的 PFBaseView。
type pfdtBase struct {
	entries map[string]PFBaseEntry
	meta    PFBaseMeta
	ledger  PFBaseLedger
}

var _ PFBaseView = (*pfdtBase)(nil)

// pfdtNewBase 造 base：files 是 路径→内容（两边 mtime 都是 pfdtT0），dirs 是目录。
func pfdtNewBase(files map[string]string, dirs ...string) *pfdtBase {
	b := &pfdtBase{entries: map[string]PFBaseEntry{}, meta: PFBaseMeta{MacRel: pfdtRel, UbuntuRel: pfdtRel, MacDirID: pfdtMacDir, UbuntuDirID: pfdtUbDir}}
	for p, c := range files {
		st := PFSideStat{MtimeNs: pfdtT0 * int64(time.Second), Ino: pfdtIno(p)}
		b.entries[p] = PFBaseEntry{T: PFBaseTypeFile, Size: int64(len(c)), SHA256: pfdtSHA(c), Mac: st, Ub: st}
	}
	for _, d := range dirs {
		st := PFSideStat{MtimeNs: pfdtT0 * int64(time.Second), Ino: pfdtIno(d)}
		b.entries[d] = PFBaseEntry{T: PFBaseTypeDir, Mac: st, Ub: st}
	}
	return b
}

func (b *pfdtBase) Unit() string                     { return "261001J0101" }
func (b *pfdtBase) Gen() uint64                      { return 1 }
func (b *pfdtBase) Meta() PFBaseMeta                 { return b.meta }
func (b *pfdtBase) Get(p string) (PFBaseEntry, bool) { e, ok := b.entries[p]; return e, ok }
func (b *pfdtBase) Len() int                         { return len(b.entries) }
func (b *pfdtBase) Ledger() PFBaseLedger             { return b.ledger }
func (b *pfdtBase) Entries() map[string]PFBaseEntry {
	out := make(map[string]PFBaseEntry, len(b.entries))
	for k, v := range b.entries {
		out[k] = v
	}
	return out
}
func (b *pfdtBase) Range(fn func(string, PFBaseEntry) bool) {
	keys := make([]string, 0, len(b.entries))
	for k := range b.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !fn(k, b.entries[k]) {
			return
		}
	}
}
func (b *pfdtBase) CompareDirIDs(mac, ub PFDirID) PFDirIDCheck {
	return PFDirIDCheck{MacSame: b.meta.MacDirID == mac, UbuntuSame: b.meta.UbuntuDirID == ub}
}

func pfdtSorted(items []PFScanItem) []PFScanItem {
	out := append([]PFScanItem(nil), items...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// pfdtIn 造输入：base 可以是 nil（首次）；两边单元目录 ID 与 base 里记的相同，时钟没差。
func pfdtIn(b *pfdtBase, mac, ub []PFScanItem) PFDecideInput {
	in := PFDecideInput{
		Unit: "261001J0101",
		Mac:  &PFScanResult{Items: mac}, Ubuntu: &PFScanResult{Items: ub},
		MacDirID: pfdtMacDir, UbuntuDirID: pfdtUbDir, MacRel: pfdtRel, UbuntuRel: pfdtRel,
		Now: pfdtNow, Loc: time.UTC,
	}
	if b != nil {
		in.Base = b
	}
	return in
}

// pfdtLines 把动作清单压成一行一个，便于逐条比较：
//
//	"种类 路径 源>目标"（拷贝类）、"种类 路径 @目标边"（单边）、改名/复制再加 " from=来源路径"。
func pfdtLines(pl *PFPlan) []string {
	var out []string
	for _, a := range pl.Actions {
		s := fmt.Sprintf("%s %s", a.Kind, a.Path)
		switch {
		case a.Src != "" && a.Dst != "":
			s += fmt.Sprintf(" %s>%s", a.Src, a.Dst)
		case a.Dst != "":
			s += fmt.Sprintf(" @%s", a.Dst)
		}
		if a.From != "" {
			s += " from=" + a.From
		}
		out = append(out, s)
	}
	return out
}

func pfdtWantLines(t *testing.T, pl *PFPlan, want ...string) {
	t.Helper()
	got := pfdtLines(pl)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("动作清单不对\n得到:\n  %s\n想要:\n  %s\n跳过: %v", strings.Join(got, "\n  "), strings.Join(want, "\n  "), pfdtSkipLines(pl))
	}
}

func pfdtSkipLines(pl *PFPlan) []string {
	var out []string
	for _, s := range pl.Skips {
		out = append(out, fmt.Sprintf("%s[%s/%s]", s.Path, s.Code, s.Side))
	}
	return out
}

func pfdtSkip(t *testing.T, pl *PFPlan, path, code string) PFSkip {
	t.Helper()
	for _, s := range pl.Skips {
		if s.Path == path {
			if code != "" && s.Code != code {
				t.Fatalf("%s 的跳过原因是 %s（%s），想要 %s", path, s.Code, s.Reason, code)
			}
			return s
		}
	}
	t.Fatalf("%s 不在跳过清单里：%v", path, pfdtSkipLines(pl))
	return PFSkip{}
}

func pfdtNoSkip(t *testing.T, pl *PFPlan, path string) {
	t.Helper()
	for _, s := range pl.Skips {
		if s.Path == path {
			t.Fatalf("%s 不该被跳过：%s（%s）", path, s.Code, s.Reason)
		}
	}
}

func pfdtAct(t *testing.T, pl *PFPlan, path string) PFAction {
	t.Helper()
	var found []PFAction
	for _, a := range pl.Actions {
		if a.Path == path {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		t.Fatalf("路径 %q 的动作有 %d 个，想要 1 个：%v", path, len(found), pfdtLines(pl))
	}
	return found[0]
}

func pfdtNoActionsFor(t *testing.T, pl *PFPlan, paths ...string) {
	t.Helper()
	for _, a := range pl.Actions {
		for _, p := range paths {
			if a.Path == p || a.From == p || (a.Conflict != nil && a.Conflict.ConflictPath == p) {
				t.Fatalf("路径 %q 不该出现在动作里：%s %s", p, a.Kind, a.Path)
			}
		}
	}
}

func pfdtMustNotStop(t *testing.T, pl *PFPlan) {
	t.Helper()
	if pl.Verdict.Stopped {
		t.Fatalf("单元不该停下：%+v", pl.Verdict.Gates)
	}
}

func pfdtGate(pl *PFPlan, code string) (PFGate, bool) {
	for _, g := range pl.Verdict.Gates {
		if g.Code == code {
			return g, true
		}
	}
	return PFGate{}, false
}

func pfdtJSON(t *testing.T, pl *PFPlan) string {
	t.Helper()
	b, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------------------------
// 判定表第 1–17 行
// ---------------------------------------------------------------------------------------------

func TestPFDecideTableRows(t *testing.T) {
	const p = "a.txt"
	base1 := map[string]string{p: "v1"}
	cases := []struct {
		name   string
		base   map[string]string // nil = 没有这个路径的 base 条目（但有一份别的都空的 base）
		mac    []PFScanItem
		ub     []PFScanItem
		want   []string
		row    int    // 动作的表行号（无动作为 0）
		skip   string // 期望 a.txt 的跳过原因代码（空 = 不该被跳过）
		notice bool   // 动作是否带要报告的 Notice
	}{
		{"第1行 两边都没改", base1, []PFScanItem{pfdtF(p, "v1")}, []PFScanItem{pfdtF(p, "v1")}, nil, 0, "", false},
		{"第2行 Ubuntu 改 → 覆盖 Mac", base1, []PFScanItem{pfdtF(p, "v1")}, []PFScanItem{pfdtF(p, "v2")}, []string{"copy_over a.txt ubuntu>mac"}, 2, "", false},
		{"第3行 Mac 改 → 覆盖 Ubuntu", base1, []PFScanItem{pfdtF(p, "v2")}, []PFScanItem{pfdtF(p, "v1")}, []string{"copy_over a.txt mac>ubuntu"}, 3, "", false},
		{"第4行 Ubuntu 删、Mac 没改 → 删 Mac", base1, []PFScanItem{pfdtF(p, "v1")}, nil, []string{"delete a.txt @mac"}, 4, "", false},
		{"第5行 Mac 删、Ubuntu 没改 → 删 Ubuntu", base1, nil, []PFScanItem{pfdtF(p, "v1")}, []string{"delete a.txt @ubuntu"}, 5, "", false},
		{"第6行 两边都删 → 只删 base", base1, nil, nil, []string{"base_del a.txt"}, 6, "", false},
		{"第7行 两边改成一样 → 只更新 base", base1, []PFScanItem{pfdtF(p, "v2")}, []PFScanItem{pfdtF(p, "v2")}, []string{"base_set a.txt"}, 7, "", false},
		{"第8行 两边改得不同 → 冲突（Ubuntu 较新占原名）", base1,
			[]PFScanItem{pfdtF(p, "v2", pfdtAt(10, 0, 0))}, []PFScanItem{pfdtF(p, "v3", pfdtAt(11, 0, 0))},
			[]string{"conflict a.txt ubuntu>mac"}, 8, "", true},
		{"第9行 Mac 改、Ubuntu 删 → 改的赢，恢复到 Ubuntu", base1, []PFScanItem{pfdtF(p, "v2")}, nil, []string{"copy_new a.txt mac>ubuntu"}, 9, "", true},
		{"第10行 Mac 删、Ubuntu 改 → 改的赢，恢复到 Mac", base1, nil, []PFScanItem{pfdtF(p, "v3")}, []string{"copy_new a.txt ubuntu>mac"}, 10, "", true},
		{"第11行 只有 Mac 新建", nil, []PFScanItem{pfdtF(p, "v1")}, nil, []string{"copy_new a.txt mac>ubuntu"}, 11, "", false},
		{"第12行 只有 Ubuntu 新建", nil, nil, []PFScanItem{pfdtF(p, "v1")}, []string{"copy_new a.txt ubuntu>mac"}, 12, "", false},
		{"第13行 两边都新建且一样 → 只写 base", nil, []PFScanItem{pfdtF(p, "v1")}, []PFScanItem{pfdtF(p, "v1")}, []string{"base_set a.txt"}, 13, "", false},
		{"第14行 两边都新建但不同 → 冲突（Mac 较新占原名）", nil,
			[]PFScanItem{pfdtF(p, "v1", pfdtAt(11, 0, 0))}, []PFScanItem{pfdtF(p, "v2", pfdtAt(10, 0, 0))},
			[]string{"conflict a.txt mac>ubuntu"}, 14, "", true},
		{"第15行 Mac 不明（未静止）、Ubuntu 改了 → 跳过", base1, []PFScanItem{pfdtNotQuiet(p)}, []PFScanItem{pfdtF(p, "v2")}, nil, 0, PFSkipUnknown, false},
		{"第15行 Mac 是 iCloud 占位、Ubuntu 删了 → 跳过，不当删除", base1, []PFScanItem{pfdtPlaceholder(p)}, nil, nil, 0, PFSkipUnknown, false},
		{"第15行 Mac 超出范围（符号链接）→ 跳过", base1, []PFScanItem{pfdtLink(p)}, []PFScanItem{pfdtF(p, "v1")}, nil, 0, PFSkipOutOfScope, false},
		{"第16行 Ubuntu 不明、Mac 改了 → 跳过", base1, []PFScanItem{pfdtF(p, "v2")}, []PFScanItem{pfdtNotQuiet(p)}, nil, 0, PFSkipUnknown, false},
		{"第16行 Ubuntu 不明、Mac 删了 → 跳过，不当删除", base1, nil, []PFScanItem{pfdtNotQuiet(p)}, nil, 0, PFSkipUnknown, false},
		{"第16行 没有 base 时一边不明 → 跳过，另一边的新文件也不传", nil, []PFScanItem{pfdtF(p, "v1")}, []PFScanItem{pfdtNotQuiet(p)}, nil, 0, PFSkipUnknown, false},
		{"哈希没算出来的条目按不明处理", base1, []PFScanItem{func() PFScanItem { it := pfdtF(p, "v1"); it.Hash = ""; return it }()}, []PFScanItem{pfdtF(p, "v2")}, nil, 0, PFSkipUnknown, false},
		{"第17行 一边文件一边目录 → 跳过", base1, []PFScanItem{pfdtF(p, "v1")}, []PFScanItem{pfdtD(p)}, nil, 0, PFSkipKindMismatch, false},
		{"第17行 base 记的是文件、一边变成目录 → 跳过", base1, []PFScanItem{pfdtD(p)}, []PFScanItem{pfdtF(p, "v1")}, nil, 0, PFSkipKindMismatch, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := pfdtNewBase(c.base)
			pl := PFDecide(pfdtIn(b, c.mac, c.ub))
			pfdtMustNotStop(t, pl)
			if pl.Verdict.FirstTime {
				t.Fatalf("不该是首次：%+v", pl.Verdict.Gates)
			}
			pfdtWantLines(t, pl, c.want...)
			if c.skip == "" {
				if len(pl.Skips) != 0 {
					t.Fatalf("不该有跳过：%v", pfdtSkipLines(pl))
				}
			} else {
				pfdtSkip(t, pl, p, c.skip)
				if len(pl.Actions) != 0 {
					t.Fatalf("跳过的路径两边都不动，却有动作：%v", pfdtLines(pl))
				}
			}
			for _, a := range pl.Actions {
				if a.Row != c.row {
					t.Errorf("动作的表行号 %d，想要 %d", a.Row, c.row)
				}
				if (a.Notice != "") != c.notice {
					t.Errorf("Notice=%q，想要有无=%v", a.Notice, c.notice)
				}
			}
		})
	}
}

// 跳过的目录里面也不能有任何动作；第 17 行连子树一起跳过。
func TestPFDecideKindMismatchSkipsSubtree(t *testing.T) {
	b := pfdtNewBase(nil)
	mac := []PFScanItem{pfdtF("x", "一个文件")}
	ub := []PFScanItem{pfdtD("x"), pfdtF("x/a.txt", "里面的文件"), pfdtD("x/s"), pfdtF("x/s/b.txt", "更里面")}
	pl := PFDecide(pfdtIn(b, mac, ub))
	pfdtWantLines(t, pl)
	pfdtSkip(t, pl, "x", PFSkipKindMismatch)
	for _, p := range []string{"x/a.txt", "x/s", "x/s/b.txt"} {
		pfdtSkip(t, pl, p, PFSkipAncestor)
	}
}

// 动作带的「判定时看到的前提」要和扫描条目一一对应。
func TestPFDecideActionExpectations(t *testing.T) {
	b := pfdtNewBase(map[string]string{"a.txt": "v1", "gone.txt": "g1"})
	mac := []PFScanItem{pfdtF("a.txt", "v1", pfdtAt(9, 0, 0)), pfdtF("gone.txt", "g1", pfdtAt(9, 1, 0)), pfdtF("new.txt", "新文件", pfdtAt(9, 2, 0))}
	ub := []PFScanItem{pfdtF("a.txt", "v2版本更长", pfdtAt(9, 30, 0))}
	pl := PFDecide(pfdtIn(b, mac, ub))
	pfdtMustNotStop(t, pl)
	pfdtWantLines(t, pl, "copy_over a.txt ubuntu>mac", "delete gone.txt @mac", "copy_new new.txt mac>ubuntu")

	over := pfdtAct(t, pl, "a.txt")
	u, m := ub[0], mac[0]
	if over.SrcExpect == nil || over.SrcExpect.SHA256 != u.Hash || over.SrcExpect.Size != u.Stat.Size ||
		over.SrcExpect.MtimeNs != u.Stat.MtimeNs || over.SrcExpect.Ino != u.Stat.Ino || over.SrcExpect.T != "f" {
		t.Errorf("覆盖的源前提不对：%+v", over.SrcExpect)
	}
	if over.DstExpect == nil || over.DstExpect.SHA256 != m.Hash || over.DstExpect.Size != m.Stat.Size ||
		over.DstExpect.MtimeNs != m.Stat.MtimeNs || over.DstExpect.Ino != m.Stat.Ino || over.DstExpect.Absent {
		t.Errorf("覆盖的目标前提不对（要是判定时那份）：%+v", over.DstExpect)
	}
	if len(over.Base) != 1 || over.Base[0].Path != "a.txt" || over.Base[0].Del || over.Base[0].Entry.SHA256 != u.Hash ||
		over.Base[0].Entry.Ub.Ino != u.Stat.Ino || !over.Base[0].FillMac || over.Base[0].FillUb {
		t.Errorf("覆盖成功后的 base 变更不对：%+v", over.Base)
	}

	del := pfdtAct(t, pl, "gone.txt")
	if del.DstExpect == nil || del.DstExpect.SHA256 != mac[1].Hash || del.DstExpect.Ino != mac[1].Stat.Ino || del.SrcExpect != nil {
		t.Errorf("删除的前提不对：%+v", del)
	}
	if len(del.Base) != 1 || !del.Base[0].Del || del.Base[0].Path != "gone.txt" {
		t.Errorf("删除成功后应删 base 条目：%+v", del.Base)
	}

	nw := pfdtAct(t, pl, "new.txt")
	if nw.DstExpect == nil || !nw.DstExpect.Absent || nw.SrcExpect == nil || nw.SrcExpect.SHA256 != mac[2].Hash {
		t.Errorf("新建的前提不对（目标必须不存在）：%+v", nw)
	}
	if nw.Base[0].Entry.Mac.Ino != mac[2].Stat.Ino || !nw.Base[0].FillUb || nw.Base[0].Entry.Ub.MtimeNs != mac[2].Stat.MtimeNs {
		t.Errorf("新建的 base 变更不对（目标边 mtime 照源、ino 待填）：%+v", nw.Base[0])
	}
	for _, a := range pl.Actions {
		if a.Phase != PFPhaseFiles {
			t.Errorf("%s 的阶段 %d，想要 %d", a.Path, a.Phase, PFPhaseFiles)
		}
	}
}

// 行 7、13 的 base 条目两边都是真实 stat，没有待填的。
func TestPFDecideBaseOnlyActions(t *testing.T) {
	b := pfdtNewBase(map[string]string{"s.txt": "v1"})
	mac := []PFScanItem{pfdtF("s.txt", "v2", pfdtAt(9, 0, 0)), pfdtF("n.txt", "same", pfdtAt(9, 1, 0))}
	ub := []PFScanItem{pfdtF("s.txt", "v2", pfdtAt(9, 5, 0)), pfdtF("n.txt", "same", pfdtAt(9, 6, 0))}
	pl := PFDecide(pfdtIn(b, mac, ub))
	pfdtWantLines(t, pl, "base_set n.txt", "base_set s.txt")
	for _, a := range pl.Actions {
		if a.Src != "" || a.Dst != "" || a.SrcExpect != nil || a.DstExpect != nil {
			t.Errorf("只改 base 的动作不该带源和目标：%+v", a)
		}
		c := a.Base[0]
		if c.FillMac || c.FillUb || c.Entry.Mac.MtimeNs == c.Entry.Ub.MtimeNs || c.Entry.Mac.Ino == 0 || c.Entry.Ub.Ino == 0 {
			t.Errorf("%s：两边的 stat 都应是真实值：%+v", a.Path, c)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// 单元级关卡 G1–G6
// ---------------------------------------------------------------------------------------------

func TestPFDecideG1UnitDirMissing(t *testing.T) {
	b := pfdtNewBase(map[string]string{"a.txt": "v1", "b.txt": "v1"})
	items := []PFScanItem{pfdtF("a.txt", "v1"), pfdtF("b.txt", "v1")}
	for _, missing := range []PFSide{PFSideMac, PFSideUbuntu} {
		in := pfdtIn(b, items, items)
		if missing == PFSideMac {
			in.Mac = nil
		} else {
			in.Ubuntu = nil
		}
		pl := PFDecide(in)
		g, ok := pfdtGate(pl, "G1")
		if !ok || g.Effect != PFGateStop || !pl.Verdict.Stopped {
			t.Fatalf("%s 目录不见了应触发 G1 并停下：%+v", missing, pl.Verdict)
		}
		if len(pl.Actions) != 0 {
			t.Fatalf("G1 命中时整单元不出动作（尤其不能删另一边）：%v", pfdtLines(pl))
		}
	}
	// 两边都不在也停下。
	in := pfdtIn(b, nil, nil)
	in.Mac, in.Ubuntu = nil, nil
	if pl := PFDecide(in); !pl.Verdict.Stopped || len(pl.Actions) != 0 {
		t.Fatalf("两边都不在应停下：%+v", pl.Verdict)
	}
}

// 没有 base 的新单元，对方还没有目录：不是 G1，先建单元根目录，再按首次规则推过去。
func TestPFDecideRootMissingFirstTime(t *testing.T) {
	mac := []PFScanItem{pfdtD("d"), pfdtF("d/a.txt", "内容"), pfdtF("b.txt", "乙")}
	in := pfdtIn(nil, mac, nil)
	in.Ubuntu = nil
	in.UbuntuDirID = PFDirID{}
	pl := PFDecide(in)
	pfdtMustNotStop(t, pl)
	if !pl.Verdict.FirstTime {
		t.Fatalf("没有 base 应按首次规则")
	}
	pfdtWantLines(t, pl, "mkdir  @ubuntu", "mkdir d mac>ubuntu", "copy_new b.txt mac>ubuntu", "copy_new d/a.txt mac>ubuntu")
	if a := pl.Actions[0]; a.Path != "" || a.Dst != PFSideUbuntu || a.Phase != PFPhaseMkdir || a.DstExpect == nil || !a.DstExpect.Absent {
		t.Errorf("第一个动作应是建单元根目录：%+v", a)
	}
}

func TestPFDecideG2DirIDChanged(t *testing.T) {
	b := pfdtNewBase(map[string]string{"a.txt": "v1", "gone.txt": "g"})
	mac := []PFScanItem{pfdtF("a.txt", "v1"), pfdtF("gone.txt", "g")} // Ubuntu 没有 gone.txt：正常是第 4 行删 Mac
	ub := []PFScanItem{pfdtF("a.txt", "v1")}

	// 只有 dev 变了不算（macOS 重启或重新挂载后 dev 会变）：照常判定，第 4 行出删除。
	in := pfdtIn(b, mac, ub)
	in.MacDirID = PFDirID{Dev: 999, Ino: pfdtMacDir.Ino}
	in.UbuntuDirID = PFDirID{Dev: 888, Ino: pfdtUbDir.Ino}
	pl := PFDecide(in)
	if pl.Verdict.FirstTime {
		t.Fatalf("只有 dev 变了不该按首次规则：%+v", pl.Verdict.Gates)
	}
	pfdtWantLines(t, pl, "delete gone.txt @mac")

	// ino 变了：本轮按首次规则，不推断删除；gone.txt 当成 Mac 新文件传给 Ubuntu。
	for _, side := range []PFSide{PFSideMac, PFSideUbuntu} {
		in := pfdtIn(b, mac, ub)
		if side == PFSideMac {
			in.MacDirID = PFDirID{Dev: pfdtMacDir.Dev, Ino: pfdtMacDir.Ino + 1}
		} else {
			in.UbuntuDirID = PFDirID{Dev: pfdtUbDir.Dev, Ino: pfdtUbDir.Ino + 1}
		}
		pl := PFDecide(in)
		g, ok := pfdtGate(pl, "G2")
		if !ok || g.Effect != PFGateFirstTime || !pl.Verdict.FirstTime || !pl.Verdict.ResetBase {
			t.Fatalf("%s ino 变了应触发 G2（首次规则、要重置 base）：%+v", side, pl.Verdict)
		}
		pfdtWantLines(t, pl, "base_set a.txt", "copy_new gone.txt mac>ubuntu")
		if pl.Meta.MacDirID != in.MacDirID || pl.Meta.UbuntuDirID != in.UbuntuDirID {
			t.Errorf("计划里的 Meta 应是本轮看到的目录 ID：%+v", pl.Meta)
		}
	}
	// base 没记过目录 ID（零值）也按 G2。
	b2 := pfdtNewBase(map[string]string{"a.txt": "v1"})
	b2.meta.MacDirID = PFDirID{}
	if pl := PFDecide(pfdtIn(b2, mac, ub)); !pl.Verdict.FirstTime {
		t.Errorf("base 里目录 ID 为零值应按首次规则")
	}
}

func TestPFDecideG4(t *testing.T) {
	b := pfdtNewBase(map[string]string{"a.txt": "v1"})
	items := []PFScanItem{pfdtF("a.txt", "v1")}

	in := pfdtIn(b, items, items)
	in.MacDup = []string{"2025/261001J0101-旧名字"}
	pl := PFDecide(in)
	if g, ok := pfdtGate(pl, "G4"); !ok || g.Effect != PFGateStop || !pl.Verdict.Stopped || len(pl.Actions) != 0 {
		t.Fatalf("同编号两个目录应停下：%+v", pl.Verdict)
	}
	if g, _ := pfdtGate(pl, "G4"); !strings.Contains(g.Reason, "2025/261001J0101-旧名字") || !strings.Contains(g.Reason, pfdtRel) {
		t.Errorf("要报两边路径：%s", g.Reason)
	}

	for _, side := range []PFSide{PFSideMac, PFSideUbuntu} {
		in := pfdtIn(b, items, items)
		if side == PFSideMac {
			in.MacRel = "2026/261001J0101-新名字"
		} else {
			in.UbuntuRel = "2026/261001J0101-新名字"
		}
		pl := PFDecide(in)
		if g, ok := pfdtGate(pl, "G4"); !ok || !pl.Verdict.Stopped || len(pl.Actions) != 0 {
			t.Fatalf("%s 上文件夹改名/挪位置应停下：%+v", side, pl.Verdict)
		} else if !strings.Contains(g.Reason, "新名字") || !strings.Contains(g.Reason, pfdtRel) {
			t.Errorf("要报新旧路径：%s", g.Reason)
		}
	}
	// 没有 base（首次）时两边 rel 本来可以不同，不算 G4。
	in = pfdtIn(nil, items, nil)
	in.UbuntuRel = "261001J0101-别的布局"
	if pl := PFDecide(in); pl.Verdict.Stopped {
		t.Errorf("首次时两边 rel 不同不该停下")
	}
}

// §14.2：Ubuntu 上同编号还有别的目录（没配置）只报告，不停整个单元，文案不承诺合并。
func TestPFDecideUbuntuDupReportOnly(t *testing.T) {
	b := pfdtNewBase(map[string]string{"a.txt": "v1"})
	items := []PFScanItem{pfdtF("a.txt", "v1")}
	in := pfdtIn(b, items, items)
	in.UbuntuDup = []string{"2025/261001J0101-旧名字", "2025/261001J0101-另一个"}
	pl := PFDecide(in)
	g, ok := pfdtGate(pl, "G4")
	if !ok || g.Effect != PFGateReport || pl.Verdict.Stopped {
		t.Fatalf("Ubuntu 上同编号别的目录应只报告、不停：%+v", pl.Verdict)
	}
	for _, want := range []string{"Ubuntu 上同一编号还有别的目录", "2025/261001J0101-旧名字", "2025/261001J0101-另一个", "没配置，不同步"} {
		if !strings.Contains(g.Reason, want) {
			t.Errorf("报告文案应含 %q：%s", want, g.Reason)
		}
	}
	if strings.Contains(g.Reason, "合并") {
		t.Errorf("Ubuntu 上的目录第一版合并不了，文案不该承诺合并：%s", g.Reason)
	}
}

func TestPFDecideG5BaseUntrusted(t *testing.T) {
	mac := []PFScanItem{pfdtF("a.txt", "v1")}
	ub := []PFScanItem{pfdtF("b.txt", "v2")}
	in := pfdtIn(nil, mac, ub)
	in.BaseProblem = "快照校验和不对"
	pl := PFDecide(in)
	g, ok := pfdtGate(pl, "G5")
	if !ok || g.Effect != PFGateFirstTime || !pl.Verdict.FirstTime || pl.Verdict.ResetBase || !strings.Contains(g.Reason, "快照校验和不对") {
		t.Fatalf("base 不可信应触发 G5：%+v", pl.Verdict)
	}
	pfdtWantLines(t, pl, "copy_new a.txt mac>ubuntu", "copy_new b.txt ubuntu>mac")

	// 本来就没有 base：首次，但不是 G5。
	in.BaseProblem = ""
	pl = PFDecide(in)
	if _, ok := pfdtGate(pl, "G5"); ok {
		t.Errorf("没有 base 不是 G5")
	}
	if g, ok := pfdtGate(pl, "no_base"); !ok || !pl.Verdict.FirstTime || g.Effect != PFGateFirstTime {
		t.Errorf("没有 base 应是首次：%+v", pl.Verdict)
	}
}

func TestPFDecideG6ClockSkew(t *testing.T) {
	b := pfdtNewBase(map[string]string{"c.txt": "v1"})
	// Ubuntu 比 Mac 新一小时。
	mac := []PFScanItem{pfdtF("c.txt", "m", pfdtAt(10, 0, 0))}
	ub := []PFScanItem{pfdtF("c.txt", "u", pfdtAt(11, 0, 0))}
	run := func(skew time.Duration, unknown bool) *PFPlan {
		in := pfdtIn(b, mac, ub)
		in.ClockSkew, in.ClockUnknown = skew, unknown
		return PFDecide(in)
	}
	// 差 4 分钟：可信，较新的 Ubuntu 占原名。
	pl := run(4*time.Minute, false)
	if pl.Verdict.ClockUntrusted || pfdtAct(t, pl, "c.txt").Src != PFSideUbuntu {
		t.Fatalf("差 4 分钟应按 mtime 判，Ubuntu 占原名：%+v", pl.Verdict)
	}
	// 差 5 分钟整：不超过，可信。
	if pl := run(5*time.Minute, false); pl.Verdict.ClockUntrusted {
		t.Errorf("正好 5 分钟不算超过")
	}
	// 差超过 5 分钟（正负都算）：Mac 占原名，报告 G6，不拦。
	for _, skew := range []time.Duration{5*time.Minute + time.Second, -10 * time.Minute} {
		pl := run(skew, false)
		g, ok := pfdtGate(pl, "G6")
		if !ok || g.Effect != PFGateReport || !pl.Verdict.ClockUntrusted || pl.Verdict.Stopped {
			t.Fatalf("skew=%v 应触发 G6：%+v", skew, pl.Verdict)
		}
		a := pfdtAct(t, pl, "c.txt")
		if a.Kind != PFActConflict || a.Src != PFSideMac || a.Conflict.Winner != PFSideMac {
			t.Fatalf("G6 时 Mac 占原名：%+v", a)
		}
	}
	// 测不出时钟差，同样按 G6。
	pl = run(0, true)
	if !pl.Verdict.ClockUntrusted || pfdtAct(t, pl, "c.txt").Src != PFSideMac {
		t.Errorf("测不出时钟差应按 G6")
	}
}

// G3 只报告，不拦。
func TestPFDecideG3ReportOnly(t *testing.T) {
	files := map[string]string{}
	var mac, ub []PFScanItem
	for i := 0; i < 6; i++ {
		p := fmt.Sprintf("f%d.txt", i)
		files[p] = "内容" + p
		mac = append(mac, pfdtF(p, "内容"+p))
		if i >= 3 { // Ubuntu 只留后三个：Mac 上 f0..f2 要被删
			ub = append(ub, pfdtF(p, "内容"+p))
		}
	}
	// 另外 Ubuntu 上有两个 Mac 已删的：Ubuntu 要删 2 个。
	for _, p := range []string{"x1.txt", "x2.txt"} {
		files[p] = "x"
		ub = append(ub, pfdtF(p, "x"))
	}
	pl := PFDecide(pfdtIn(pfdtNewBase(files), mac, ub))
	pfdtMustNotStop(t, pl)
	if pl.Verdict.MacDeletes != 3 || pl.Verdict.UbuntuDeletes != 2 {
		t.Fatalf("删除数不对：%+v", pl.Verdict)
	}
	g, ok := pfdtGate(pl, "G3")
	if !ok || g.Effect != PFGateReport || !strings.Contains(g.Reason, "3") || !strings.Contains(g.Reason, "2") || !strings.Contains(g.Reason, "回收") {
		t.Fatalf("应报告 G3：%+v", g)
	}
	n := 0
	for _, a := range pl.Actions {
		if a.Kind == PFActDelete {
			n++
		}
	}
	if n != 5 {
		t.Fatalf("G3 不拦，5 个删除动作都该在：%v", pfdtLines(pl))
	}
}

// ---------------------------------------------------------------------------------------------
// 首次规则
// ---------------------------------------------------------------------------------------------

func TestPFDecideFirstTimeNeverDeletes(t *testing.T) {
	mac := []PFScanItem{
		pfdtD("d"), pfdtF("d/only-mac.txt", "只在 Mac"), pfdtF("same.txt", "一样"),
		pfdtF("diff.txt", "Mac 版本", pfdtAt(10, 0, 0)), pfdtF("m.txt", "m"),
	}
	ub := []PFScanItem{
		pfdtD("e"), pfdtF("e/only-ub.txt", "只在 Ubuntu"), pfdtF("same.txt", "一样"),
		pfdtF("diff.txt", "Ubuntu 版本", pfdtAt(11, 0, 0)), pfdtF("u.txt", "u"),
	}
	pl := PFDecide(pfdtIn(nil, mac, ub))
	pfdtMustNotStop(t, pl)
	if !pl.Verdict.FirstTime {
		t.Fatal("没有 base 应按首次规则")
	}
	pfdtWantLines(t, pl,
		"mkdir d mac>ubuntu", "mkdir e ubuntu>mac",
		"copy_new d/only-mac.txt mac>ubuntu", "conflict diff.txt ubuntu>mac", "copy_new e/only-ub.txt ubuntu>mac",
		"copy_new m.txt mac>ubuntu", "base_set same.txt", "copy_new u.txt ubuntu>mac")
	for _, a := range pl.Actions {
		switch a.Kind {
		case PFActDelete, PFActRmdir, PFActCopyOver, PFActRename:
			t.Errorf("首次规则不该有 %s：%s", a.Kind, a.Path)
		}
		if a.Row != 0 && (a.Row < 11 || a.Row > 14) {
			t.Errorf("首次规则只用表里第 11–14 行：%s 用了第 %d 行", a.Path, a.Row)
		}
	}
	if pl.Verdict.MacDeletes+pl.Verdict.UbuntuDeletes != 0 {
		t.Errorf("首次不该有删除：%+v", pl.Verdict)
	}
}

// G2 触发时旧 base 里的条目一概不信：本来是「另一边删了」的情形，改成新文件处理，不删。
func TestPFDecideFirstTimeIgnoresOldBaseEntries(t *testing.T) {
	b := pfdtNewBase(map[string]string{"a.txt": "v1", "b.txt": "v1", "c.txt": "v1", "d.txt": "v1"}, "dir")
	mac := []PFScanItem{pfdtF("a.txt", "v1"), pfdtF("b.txt", "改了"), pfdtD("dir")}
	ub := []PFScanItem{pfdtF("c.txt", "v9")}
	in := pfdtIn(b, mac, ub)
	in.UbuntuDirID = PFDirID{Dev: 2, Ino: 12345}
	pl := PFDecide(in)
	if !pl.Verdict.ResetBase {
		t.Fatal("应要求重置 base")
	}
	pfdtWantLines(t, pl, "mkdir dir mac>ubuntu", "copy_new a.txt mac>ubuntu", "copy_new b.txt mac>ubuntu", "copy_new c.txt ubuntu>mac")
}

// ---------------------------------------------------------------------------------------------
// 没看的子树
// ---------------------------------------------------------------------------------------------

func TestPFDecideSubtreeNotSeenIsNotDeletion(t *testing.T) {
	files := map[string]string{"link/x.txt": "x", "link/s/y.txt": "y", "other.txt": "o"}
	b := pfdtNewBase(files, "link", "link/s")
	uItems := []PFScanItem{pfdtD("link"), pfdtF("link/x.txt", "x"), pfdtD("link/s"), pfdtF("link/s/y.txt", "y"), pfdtF("other.txt", "o")}

	cases := []struct {
		name string
		dir  PFScanItem
		code string // 目录自己的跳过代码
	}{
		{"符号链接", pfdtLink("link"), PFSkipOutOfScope},
		{"读不了的目录", pfdtBadDir("link"), PFSkipOutOfScope},
		{"iCloud 占位目录", pfdtPlaceholderDir("link"), PFSkipUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mac := []PFScanItem{c.dir, pfdtF("other.txt", "o")} // base 里 link 下的东西 Mac 扫描里都没有
			pl := PFDecide(pfdtIn(b, mac, uItems))
			pfdtMustNotStop(t, pl)
			pfdtWantLines(t, pl) // 不出任何动作：既不删 Ubuntu 的，也不往 Mac 的链接里写
			pfdtSkip(t, pl, "link", c.code)
			for _, p := range []string{"link/x.txt", "link/s", "link/s/y.txt"} {
				s := pfdtSkip(t, pl, p, PFSkipSubtree)
				if s.Side != PFSideMac {
					t.Errorf("%s 没看的是 Mac 这边：%+v", p, s)
				}
			}
			pfdtNoSkip(t, pl, "other.txt")
		})
	}

	// 对称：Ubuntu 没看，Mac 有。
	mac := []PFScanItem{pfdtD("link"), pfdtF("link/x.txt", "x"), pfdtD("link/s"), pfdtF("link/s/y.txt", "y"), pfdtF("other.txt", "o")}
	ub := []PFScanItem{pfdtBadDir("link"), pfdtF("other.txt", "o")}
	pl := PFDecide(pfdtIn(b, mac, ub))
	pfdtWantLines(t, pl)
	if s := pfdtSkip(t, pl, "link/x.txt", PFSkipSubtree); s.Side != PFSideUbuntu {
		t.Errorf("没看的是 Ubuntu：%+v", s)
	}

	// 没看的那一边的子树下，另一边新增、修改的东西也不传（那一边根本写不进去）。
	b2 := pfdtNewBase(map[string]string{"link/x.txt": "x"}, "link")
	mac = []PFScanItem{pfdtLink("link")}
	ub = []PFScanItem{pfdtD("link"), pfdtF("link/x.txt", "x改了"), pfdtF("link/new.txt", "新")}
	pl = PFDecide(pfdtIn(b2, mac, ub))
	pfdtWantLines(t, pl)
	pfdtSkip(t, pl, "link/new.txt", PFSkipSubtree)
	pfdtSkip(t, pl, "link/x.txt", PFSkipSubtree)
}

// 重复出现在扫描里的路径当不明。
func TestPFDecideDuplicateItemIsUnknown(t *testing.T) {
	b := pfdtNewBase(map[string]string{"a.txt": "v1"})
	mac := []PFScanItem{pfdtF("a.txt", "v1"), pfdtF("a.txt", "v2")}
	ub := []PFScanItem{pfdtF("a.txt", "v1")}
	pl := PFDecide(pfdtIn(b, mac, ub))
	pfdtWantLines(t, pl)
	pfdtSkip(t, pl, "a.txt", PFSkipUnknown)
}

// ---------------------------------------------------------------------------------------------
// 目录
// ---------------------------------------------------------------------------------------------

func TestPFDecideDirs(t *testing.T) {
	t.Run("一边新建目录另一边建", func(t *testing.T) {
		b := pfdtNewBase(nil)
		pl := PFDecide(pfdtIn(b, []PFScanItem{pfdtD("a"), pfdtD("a/b")}, []PFScanItem{pfdtD("z")}))
		pfdtWantLines(t, pl, "mkdir a mac>ubuntu", "mkdir a/b mac>ubuntu", "mkdir z ubuntu>mac")
		for _, a := range pl.Actions {
			if a.Phase != PFPhaseMkdir || a.DstExpect == nil || !a.DstExpect.Absent || a.SrcExpect == nil || a.SrcExpect.T != "d" || len(a.Base) != 1 || a.Base[0].Entry.T != "d" {
				t.Errorf("建目录动作的前提或 base 不对：%+v", a)
			}
		}
	})
	t.Run("两边都新建同一个目录只写 base", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), []PFScanItem{pfdtD("a")}, []PFScanItem{pfdtD("a")}))
		pfdtWantLines(t, pl, "base_set a")
	})
	t.Run("两边都没有了的目录只删 base", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtNewBase(nil, "a"), nil, nil))
		pfdtWantLines(t, pl, "base_del a")
	})
	t.Run("一边删了整棵目录，另一边没改：先删文件，空了才 rmdir，深的先删", func(t *testing.T) {
		b := pfdtNewBase(map[string]string{"a/b/f.txt": "F", "a/g.txt": "G"}, "a", "a/b")
		ub := []PFScanItem{pfdtD("a"), pfdtD("a/b"), pfdtF("a/b/f.txt", "F"), pfdtF("a/g.txt", "G")}
		pl := PFDecide(pfdtIn(b, nil, ub))
		pfdtWantLines(t, pl, "delete a/b/f.txt @ubuntu", "delete a/g.txt @ubuntu", "rmdir a/b @ubuntu", "rmdir a @ubuntu")
		for _, a := range pl.Actions {
			if a.Kind == PFActRmdir && (!a.IfEmpty || a.Phase != PFPhaseRmdir || a.DstExpect == nil || a.DstExpect.T != "d") {
				t.Errorf("rmdir 要带「执行时再判空」的条件：%+v", a)
			}
		}
		if pl.Verdict.UbuntuDeletes != 2 {
			t.Errorf("删除数：%+v", pl.Verdict)
		}
	})
	t.Run("一边删了目录，另一边里面有新文件：目录要回来，不删", func(t *testing.T) {
		b := pfdtNewBase(map[string]string{"d/f": "F"}, "d")
		ub := []PFScanItem{pfdtD("d"), pfdtF("d/f", "F"), pfdtF("d/new", "新")}
		pl := PFDecide(pfdtIn(b, nil, ub))
		pfdtWantLines(t, pl, "mkdir d ubuntu>mac", "delete d/f @ubuntu", "copy_new d/new ubuntu>mac")
	})
	t.Run("目录里还有不明的东西：保留，不删", func(t *testing.T) {
		b := pfdtNewBase(map[string]string{"d/f": "F"}, "d")
		ub := []PFScanItem{pfdtD("d"), pfdtF("d/f", "F"), pfdtNotQuiet("d/writing.bin")}
		pl := PFDecide(pfdtIn(b, nil, ub))
		pfdtWantLines(t, pl, "delete d/f @ubuntu")
		pfdtSkip(t, pl, "d", PFSkipDirKept)
		pfdtSkip(t, pl, "d/writing.bin", PFSkipUnknown)
	})
	t.Run("目录里的文件有 Office 锁推迟删除：目录保留", func(t *testing.T) {
		b := pfdtNewBase(map[string]string{"d/report.docx": "F"}, "d")
		ub := []PFScanItem{pfdtD("d"), pfdtF("d/report.docx", "F")}
		in := pfdtIn(b, nil, ub)
		in.UbuntuNames = []PFRawName{{Path: "d/~$report.docx"}}
		pl := PFDecide(in)
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, "d/report.docx", PFSkipOfficeLock)
		pfdtSkip(t, pl, "d", PFSkipDirKept)
	})
	t.Run("目录深处新建：上级目录在目标边不在时先建", func(t *testing.T) {
		b := pfdtNewBase(map[string]string{"p/q/f": "F"}, "p", "p/q")
		// Mac 把 p 删了，Ubuntu 在 p/q 下新建了东西。
		ub := []PFScanItem{pfdtD("p"), pfdtD("p/q"), pfdtF("p/q/f", "F"), pfdtD("p/q/r"), pfdtF("p/q/r/new", "新")}
		pl := PFDecide(pfdtIn(b, nil, ub))
		pfdtWantLines(t, pl, "mkdir p ubuntu>mac", "mkdir p/q ubuntu>mac", "mkdir p/q/r ubuntu>mac", "delete p/q/f @ubuntu", "copy_new p/q/r/new ubuntu>mac")
	})
}

// ---------------------------------------------------------------------------------------------
// 改名、复制复用
// ---------------------------------------------------------------------------------------------

func TestPFDecideDirRenameReusesLocalFiles(t *testing.T) {
	b := pfdtNewBase(map[string]string{"old/a.mp4": "AAAA", "old/b.mp4": "BBBBBB", "old/s/c.mp4": "C"}, "old", "old/s")
	// Mac 没动；Ubuntu 把 old 改名成 new。
	mac := []PFScanItem{pfdtD("old"), pfdtF("old/a.mp4", "AAAA"), pfdtF("old/b.mp4", "BBBBBB"), pfdtD("old/s"), pfdtF("old/s/c.mp4", "C")}
	ub := []PFScanItem{pfdtD("new"), pfdtF("new/a.mp4", "AAAA"), pfdtF("new/b.mp4", "BBBBBB"), pfdtD("new/s"), pfdtF("new/s/c.mp4", "C")}
	pl := PFDecide(pfdtIn(b, mac, ub))
	pfdtMustNotStop(t, pl)
	pfdtWantLines(t, pl,
		"mkdir new ubuntu>mac", "mkdir new/s ubuntu>mac",
		"rename_local new/a.mp4 ubuntu>mac from=old/a.mp4",
		"rename_local new/b.mp4 ubuntu>mac from=old/b.mp4",
		"rename_local new/s/c.mp4 ubuntu>mac from=old/s/c.mp4",
		"rmdir old/s @mac", "rmdir old @mac")
	for _, a := range pl.Actions {
		if a.Kind == PFActCopyNew || a.Kind == PFActDelete || a.Kind == PFActCopyOver {
			t.Errorf("目录改名不该有传输或删除：%s %s", a.Kind, a.Path)
		}
	}
	if pl.Verdict.MacDeletes != 0 {
		t.Errorf("改名不算删除：%+v", pl.Verdict)
	}
	a := pfdtAct(t, pl, "new/a.mp4")
	if a.Dst != PFSideMac || a.From != "old/a.mp4" || a.DstExpect == nil || !a.DstExpect.Absent ||
		a.FromExpect == nil || a.FromExpect.SHA256 != pfdtSHA("AAAA") || a.FromExpect.Ino != mac[1].Stat.Ino || a.FromExpect.MtimeNs != mac[1].Stat.MtimeNs ||
		a.SrcExpect == nil || a.SrcExpect.SHA256 != pfdtSHA("AAAA") {
		t.Errorf("改名动作的前提不对：%+v", a)
	}
	// 成功后：base 删旧路径、写新路径；Mac 一边的 stat 是旧文件的（改名不改 mtime、ino），不用再填。
	if len(a.Base) != 2 || !a.Base[0].Del || a.Base[0].Path != "old/a.mp4" || a.Base[1].Path != "new/a.mp4" || a.Base[1].Del {
		t.Fatalf("改名的 base 变更不对：%+v", a.Base)
	}
	e := a.Base[1]
	if e.FillMac || e.FillUb || e.Entry.Mac.Ino != mac[1].Stat.Ino || e.Entry.Ub.Ino != ub[1].Stat.Ino || e.Entry.SHA256 != pfdtSHA("AAAA") {
		t.Errorf("改名的 base 条目不对：%+v", e)
	}
}

func TestPFDecideRenameSingleFileAndSameContentChoice(t *testing.T) {
	// 两个内容相同的文件在 Ubuntu 上互换了名字：每个新名字配一个要删的旧文件，文件名相同的优先
	// （内容一样，配谁都对；这里只锁住「各配一个、不重传、结果确定」）。
	b := pfdtNewBase(map[string]string{"x/same.bin": "SAME", "y/same2.bin": "SAME"}, "x", "y")
	mac := []PFScanItem{pfdtD("x"), pfdtD("y"), pfdtF("x/same.bin", "SAME"), pfdtF("y/same2.bin", "SAME")}
	ub := []PFScanItem{pfdtD("x"), pfdtD("y"), pfdtF("x/same2.bin", "SAME"), pfdtF("y/same.bin", "SAME")}
	pl := PFDecide(pfdtIn(b, mac, ub))
	pfdtWantLines(t, pl,
		"rename_local x/same2.bin ubuntu>mac from=y/same2.bin",
		"rename_local y/same.bin ubuntu>mac from=x/same.bin")
}

func TestPFDecideCloneLocalReuse(t *testing.T) {
	b := pfdtNewBase(map[string]string{"keep/orig.bin": "BIGBIGBIG"}, "keep")
	mac := []PFScanItem{pfdtD("keep"), pfdtF("keep/orig.bin", "BIGBIGBIG"), pfdtD("copy"), pfdtF("copy/new.bin", "BIGBIGBIG")}
	ub := []PFScanItem{pfdtD("keep"), pfdtF("keep/orig.bin", "BIGBIGBIG")}
	pl := PFDecide(pfdtIn(b, mac, ub))
	pfdtWantLines(t, pl, "mkdir copy mac>ubuntu", "clone_local copy/new.bin mac>ubuntu from=keep/orig.bin")
	a := pfdtAct(t, pl, "copy/new.bin")
	if a.FromExpect == nil || a.FromExpect.SHA256 != pfdtSHA("BIGBIGBIG") || a.FromExpect.Ino != ub[1].Stat.Ino ||
		a.SrcExpect == nil || a.SrcExpect.SHA256 != pfdtSHA("BIGBIGBIG") || a.DstExpect == nil || !a.DstExpect.Absent {
		t.Errorf("复制动作的前提不对：%+v", a)
	}
	if len(a.Base) != 1 || a.Base[0].Path != "copy/new.bin" || !a.Base[0].FillUb || a.Base[0].FillMac {
		t.Errorf("复制的 base 变更不对（目标边新文件 ino 待填）：%+v", a.Base)
	}

	// 来源文件本轮要被覆盖 → 不能当来源，老老实实传输。
	mac2 := []PFScanItem{pfdtD("keep"), pfdtF("keep/orig.bin", "NEWER"), pfdtD("copy"), pfdtF("copy/new.bin", "BIGBIGBIG")}
	pl = PFDecide(pfdtIn(b, mac2, ub))
	pfdtWantLines(t, pl, "mkdir copy mac>ubuntu", "copy_new copy/new.bin mac>ubuntu", "copy_over keep/orig.bin mac>ubuntu")

	// 来源文件是不明的 → 不能当来源。
	ub3 := []PFScanItem{pfdtD("keep"), pfdtNotQuiet("keep/orig.bin")}
	pl = PFDecide(pfdtIn(b, mac, ub3))
	pfdtNoActionsFor(t, pl, "keep/orig.bin")
	if got := pfdtAct(t, pl, "copy/new.bin"); got.Kind != PFActCopyNew {
		t.Errorf("来源不明时应照常传输：%s", got.Kind)
	}
}

func TestPFDecideFirstTimeCloneBothWays(t *testing.T) {
	mac := []PFScanItem{pfdtD("a"), pfdtF("a/x.bin", "X内容"), pfdtF("m.bin", "只有Mac")}
	ub := []PFScanItem{pfdtD("b"), pfdtF("b/x.bin", "X内容")}
	pl := PFDecide(pfdtIn(nil, mac, ub))
	pfdtWantLines(t, pl,
		"mkdir a mac>ubuntu", "mkdir b ubuntu>mac",
		"clone_local a/x.bin mac>ubuntu from=b/x.bin",
		"clone_local b/x.bin ubuntu>mac from=a/x.bin",
		"copy_new m.bin mac>ubuntu")
}

// ---------------------------------------------------------------------------------------------
// 冲突
// ---------------------------------------------------------------------------------------------

func TestPFDecideConflictNamingAndWinner(t *testing.T) {
	b := pfdtNewBase(map[string]string{"报告.docx": "v1", "子/报告.docx": "v1"}, "子")
	mkIn := func(mMt, uMt int64) PFDecideInput {
		return pfdtIn(b,
			[]PFScanItem{pfdtD("子"), pfdtF("报告.docx", "mac版", mMt), pfdtF("子/报告.docx", "mac版", mMt)},
			[]PFScanItem{pfdtD("子"), pfdtF("报告.docx", "ub版", uMt), pfdtF("子/报告.docx", "ub版", uMt)})
	}

	t.Run("Ubuntu 较新：Ubuntu 占原名，Mac 的版本改名", func(t *testing.T) {
		pl := PFDecide(mkIn(pfdtAt(15, 30, 0), pfdtAt(15, 45, 0)))
		a := pfdtAct(t, pl, "报告.docx")
		if a.Kind != PFActConflict || a.Row != 8 || a.Src != PFSideUbuntu || a.Dst != PFSideMac || a.Conflict == nil ||
			a.Conflict.Winner != PFSideUbuntu || a.Conflict.Loser != PFSideMac {
			t.Fatalf("胜负不对：%+v", a)
		}
		// 名字里的机器和时间指被改名的那份（Mac 的，15:30）。
		if a.Conflict.ConflictPath != "报告（冲突-Mac-1026-1530）.docx" {
			t.Errorf("冲突名不对：%q", a.Conflict.ConflictPath)
		}
		if sub := pfdtAct(t, pl, "子/报告.docx"); sub.Conflict.ConflictPath != "子/报告（冲突-Mac-1026-1530）.docx" {
			t.Errorf("子目录里的冲突名不对：%q", sub.Conflict.ConflictPath)
		}
		// 前提：胜出边、落败边原名文件的现状。
		if a.SrcExpect.SHA256 != pfdtSHA("ub版") || a.DstExpect.SHA256 != pfdtSHA("mac版") {
			t.Errorf("前提不对：src=%+v dst=%+v", a.SrcExpect, a.DstExpect)
		}
		// base：原名记胜出哈希，冲突名记落败哈希。
		if len(a.Base) != 2 || a.Base[0].Path != "报告.docx" || a.Base[0].Entry.SHA256 != pfdtSHA("ub版") ||
			a.Base[1].Path != a.Conflict.ConflictPath || a.Base[1].Entry.SHA256 != pfdtSHA("mac版") {
			t.Fatalf("冲突的 base 变更不对：%+v", a.Base)
		}
		// 原名：胜出边（Ubuntu）是真实 stat，落败边（Mac）装好胜出版本后待填；冲突名反过来。
		if a.Base[0].FillMac != true || a.Base[0].FillUb || a.Base[1].FillUb != true || a.Base[1].FillMac {
			t.Errorf("待填的边不对：%+v", a.Base)
		}
		if a.Base[1].Entry.Mac.Ino != pfdtIno("报告.docx") {
			t.Errorf("冲突名那条里落败边的 stat 应是原文件改名前的：%+v", a.Base[1].Entry)
		}
		if a.Notice == "" {
			t.Errorf("冲突要报告给用户")
		}
	})
	t.Run("Mac 较新：Mac 占原名，Ubuntu 的版本改名", func(t *testing.T) {
		pl := PFDecide(mkIn(pfdtAt(15, 45, 0), pfdtAt(9, 7, 0)))
		a := pfdtAct(t, pl, "报告.docx")
		if a.Src != PFSideMac || a.Conflict.ConflictPath != "报告（冲突-Ubuntu-1026-0907）.docx" {
			t.Fatalf("%+v", a.Conflict)
		}
	})
	t.Run("相差 2 秒以内 Mac 占原名（含正好 2 秒），超过 2 秒按新旧", func(t *testing.T) {
		for _, c := range []struct {
			diff int64 // Ubuntu 比 Mac 新几秒
			want PFSide
		}{{1, PFSideMac}, {2, PFSideMac}, {3, PFSideUbuntu}, {-1, PFSideMac}, {-2, PFSideMac}, {-3, PFSideMac}} {
			base := pfdtAt(12, 0, 10)
			pl := PFDecide(mkIn(base, base+c.diff))
			if got := pfdtAct(t, pl, "报告.docx").Src; got != c.want {
				t.Errorf("Ubuntu 比 Mac 新 %d 秒：占原名的是 %s，想要 %s", c.diff, got, c.want)
			}
		}
	})
	t.Run("同名冲突副本已存在：加 -2、-3", func(t *testing.T) {
		in := pfdtIn(b,
			[]PFScanItem{pfdtF("报告.docx", "mac版", pfdtAt(15, 30, 0))},
			[]PFScanItem{
				pfdtF("报告.docx", "ub版", pfdtAt(15, 45, 0)),
				pfdtF("报告（冲突-Mac-1026-1530）.docx", "旧冲突", pfdtAt(8, 0, 0)),
				pfdtF("报告（冲突-Mac-1026-1530-2）.docx", "旧冲突2", pfdtAt(8, 0, 0)),
			})
		in.MacNames = []PFRawName{{Path: "报告（冲突-Mac-1026-1530-3）.docx"}} // 被排除或别处看到的名字也算存在
		pl := PFDecide(in)
		if got := pfdtAct(t, pl, "报告.docx").Conflict.ConflictPath; got != "报告（冲突-Mac-1026-1530-4）.docx" {
			t.Errorf("应加 -4：%q", got)
		}
	})
	t.Run("同一目录两个冲突各用各的名字", func(t *testing.T) {
		b2 := pfdtNewBase(map[string]string{"a.docx": "v1", "b.docx": "v1"})
		pl := PFDecide(pfdtIn(b2,
			[]PFScanItem{pfdtF("a.docx", "m1", pfdtAt(9, 0, 0)), pfdtF("b.docx", "m2", pfdtAt(9, 0, 0))},
			[]PFScanItem{pfdtF("a.docx", "u1", pfdtAt(10, 0, 0)), pfdtF("b.docx", "u2", pfdtAt(10, 0, 0))}))
		an, bn := pfdtAct(t, pl, "a.docx").Conflict.ConflictPath, pfdtAct(t, pl, "b.docx").Conflict.ConflictPath
		if an == bn || an != "a（冲突-Mac-1026-0900）.docx" || bn != "b（冲突-Mac-1026-0900）.docx" {
			t.Errorf("%q %q", an, bn)
		}
	})
	t.Run("冲突副本名超长时截短主名", func(t *testing.T) {
		long := strings.Repeat("长", 80) + ".docx" // 240 + 5 = 245 字节
		b2 := pfdtNewBase(map[string]string{long: "v1"})
		pl := PFDecide(pfdtIn(b2, []PFScanItem{pfdtF(long, "m", pfdtAt(9, 0, 0))}, []PFScanItem{pfdtF(long, "u", pfdtAt(10, 0, 0))}))
		a := pfdtAct(t, pl, long)
		if len(a.Conflict.ConflictPath) > 255 || !strings.HasSuffix(a.Conflict.ConflictPath, "（冲突-Mac-1026-0900）.docx") {
			t.Errorf("超长名字没处理好：%q（%d 字节）", a.Conflict.ConflictPath, len(a.Conflict.ConflictPath))
		}
	})
	t.Run("首次两边都有且不同：同样冲突，也有冲突名", func(t *testing.T) {
		pl := PFDecide(pfdtIn(nil, []PFScanItem{pfdtF("n.txt", "m", pfdtAt(9, 0, 0))}, []PFScanItem{pfdtF("n.txt", "u", pfdtAt(9, 0, 1))}))
		a := pfdtAct(t, pl, "n.txt")
		if a.Kind != PFActConflict || a.Row != 14 || a.Src != PFSideMac || a.Conflict.ConflictPath != "n（冲突-Ubuntu-1026-0900）.txt" {
			t.Errorf("%+v %+v", a, a.Conflict)
		}
	})
}

// ---------------------------------------------------------------------------------------------
// 撞名、两边对不上
// ---------------------------------------------------------------------------------------------

func TestPFDecideNameClashes(t *testing.T) {
	t.Run("同一边撞名：两个都跳过，另一边的同名文件也不动", func(t *testing.T) {
		b := pfdtNewBase(map[string]string{"Report.docx": "v1"})
		mac := []PFScanItem{pfdtF("Report.docx", "v1")}
		ub := []PFScanItem{pfdtF("Report.docx", "v1"), pfdtF("report.docx", "v5")}
		pl := PFDecide(pfdtIn(b, mac, ub))
		pfdtWantLines(t, pl)
		for _, p := range []string{"Report.docx", "report.docx"} {
			if s := pfdtSkip(t, pl, p, PFSkipSameSideClash); s.Side != PFSideUbuntu {
				t.Errorf("%s 撞名在 Ubuntu 这边：%+v", p, s)
			}
		}
	})
	t.Run("同一边撞名：NFC 与 NFD", func(t *testing.T) {
		nfc, nfd := "café.txt", "café.txt"
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), nil, []PFScanItem{pfdtF(nfc, "1"), pfdtF(nfd, "2")}))
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, nfc, PFSkipSameSideClash)
		pfdtSkip(t, pl, nfd, PFSkipSameSideClash)
	})
	t.Run("两边对不上：Mac Report.docx、Ubuntu report.docx，两边都不动", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), []PFScanItem{pfdtF("Report.docx", "v1")}, []PFScanItem{pfdtF("report.docx", "v1")}))
		pfdtWantLines(t, pl)
		if s := pfdtSkip(t, pl, "Report.docx", PFSkipCrossSideClash); s.Side != PFSideMac {
			t.Errorf("%+v", s)
		}
		if s := pfdtSkip(t, pl, "report.docx", PFSkipCrossSideClash); s.Side != PFSideUbuntu {
			t.Errorf("%+v", s)
		}
	})
	t.Run("只差大小写的改名：Ubuntu 把 Report 改成 report，不能把 Mac 的 Report 当成「被删」删掉", func(t *testing.T) {
		b := pfdtNewBase(map[string]string{"Report.docx": "v1"})
		pl := PFDecide(pfdtIn(b, []PFScanItem{pfdtF("Report.docx", "v1")}, []PFScanItem{pfdtF("report.docx", "v1")}))
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, "Report.docx", PFSkipCrossSideClash)
		pfdtSkip(t, pl, "report.docx", PFSkipCrossSideClash)
	})
	t.Run("NFC/NFD 两边对不上", func(t *testing.T) {
		nfc, nfd := "café.txt", "café.txt"
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), []PFScanItem{pfdtF(nfc, "1")}, []PFScanItem{pfdtF(nfd, "1")}))
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, nfc, PFSkipCrossSideClash)
		pfdtSkip(t, pl, nfd, PFSkipCrossSideClash)
	})
	t.Run("目录段只差大小写：整个子树都跳过", func(t *testing.T) {
		b := pfdtNewBase(map[string]string{"Docs/a.txt": "v1"}, "Docs")
		mac := []PFScanItem{pfdtD("Docs"), pfdtF("Docs/a.txt", "v1")}
		ub := []PFScanItem{pfdtD("docs"), pfdtF("docs/a.txt", "v1"), pfdtF("docs/new.txt", "新")}
		pl := PFDecide(pfdtIn(b, mac, ub))
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, "Docs", PFSkipCrossSideClash)
		pfdtSkip(t, pl, "docs", PFSkipCrossSideClash)
		for _, p := range []string{"Docs/a.txt", "docs/a.txt", "docs/new.txt"} {
			pfdtSkip(t, pl, p, PFSkipAncestor)
		}
	})
	t.Run("两边字节相同的名字不算撞名", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), []PFScanItem{pfdtF("Same.txt", "v1")}, []PFScanItem{pfdtF("Same.txt", "v1")}))
		pfdtWantLines(t, pl, "base_set Same.txt")
		if len(pl.Skips) != 0 {
			t.Errorf("%v", pfdtSkipLines(pl))
		}
	})
	t.Run("别的目录里的同名不撞", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), []PFScanItem{pfdtD("a"), pfdtD("b"), pfdtF("a/x.txt", "1")}, []PFScanItem{pfdtD("a"), pfdtD("b"), pfdtF("b/X.txt", "1")}))
		if len(pl.Skips) != 0 {
			t.Errorf("不同目录里的 x.txt 与 X.txt 不撞：%v", pfdtSkipLines(pl))
		}
	})
	t.Run("路径不合规（整条超过 1000 字节）：跳过", func(t *testing.T) {
		long := strings.Repeat("目录/", 150) + "f.txt" // 1350 字节
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), []PFScanItem{pfdtF(long, "1")}, nil))
		pfdtSkip(t, pl, long, PFSkipOutOfScope)
		pfdtWantLines(t, pl)
	})
}

// ---------------------------------------------------------------------------------------------
// Office 锁
// ---------------------------------------------------------------------------------------------

func TestPFDecideOfficeLocks(t *testing.T) {
	const doc = "report.docx"
	b := pfdtNewBase(map[string]string{doc: "v1"})
	same, changed := []PFScanItem{pfdtF(doc, "v1")}, []PFScanItem{pfdtF(doc, "v2")}

	t.Run("锁在目标边：不覆盖，推迟", func(t *testing.T) {
		in := pfdtIn(b, same, changed) // 第 2 行，目标是 Mac
		in.MacNames = []PFRawName{{Path: "~$report.docx"}}
		pl := PFDecide(in)
		pfdtWantLines(t, pl)
		if s := pfdtSkip(t, pl, doc, PFSkipOfficeLock); s.Side != PFSideMac {
			t.Errorf("%+v", s)
		}
	})
	t.Run("锁在源边：照常发送，带一条提示", func(t *testing.T) {
		in := pfdtIn(b, same, changed)
		in.UbuntuNames = []PFRawName{{Path: "~$report.docx"}}
		pl := PFDecide(in)
		pfdtWantLines(t, pl, "copy_over report.docx ubuntu>mac")
		if a := pfdtAct(t, pl, doc); !strings.Contains(a.Notice, "Office 锁") {
			t.Errorf("应提示源边有锁：%q", a.Notice)
		}
	})
	t.Run("锁在目标边：不删", func(t *testing.T) {
		in := pfdtIn(b, same, nil) // 第 4 行，删 Mac
		in.MacNames = []PFRawName{{Path: ".~lock.report.docx#"}}
		pl := PFDecide(in)
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, doc, PFSkipOfficeLock)
	})
	t.Run("锁在别的目录里：不相干", func(t *testing.T) {
		in := pfdtIn(b, same, changed)
		in.MacNames = []PFRawName{{Path: "sub/~$report.docx"}}
		pfdtWantLines(t, PFDecide(in), "copy_over report.docx ubuntu>mac")
	})
	t.Run("长名字的锁（名字被 ~$ 顶掉几个字符）", func(t *testing.T) {
		b2 := pfdtNewBase(map[string]string{"projectplan.docx": "v1"})
		for _, lock := range []string{"~$projectplan.docx", "~$rojectplan.docx", "~$ojectplan.docx"} {
			in := pfdtIn(b2, []PFScanItem{pfdtF("projectplan.docx", "v1")}, []PFScanItem{pfdtF("projectplan.docx", "v2")})
			in.MacNames = []PFRawName{{Path: lock}}
			pl := PFDecide(in)
			pfdtSkip(t, pl, "projectplan.docx", PFSkipOfficeLock)
		}
	})
	t.Run("冲突时任一边有锁都推迟", func(t *testing.T) {
		b2 := pfdtNewBase(map[string]string{doc: "v1"})
		for _, side := range []PFSide{PFSideMac, PFSideUbuntu} {
			in := pfdtIn(b2, []PFScanItem{pfdtF(doc, "m", pfdtAt(9, 0, 0))}, []PFScanItem{pfdtF(doc, "u", pfdtAt(10, 0, 0))})
			if side == PFSideMac {
				in.MacNames = []PFRawName{{Path: "~$report.docx"}}
			} else {
				in.UbuntuNames = []PFRawName{{Path: "~$report.docx"}}
			}
			pl := PFDecide(in)
			pfdtWantLines(t, pl)
			if s := pfdtSkip(t, pl, doc, PFSkipOfficeLock); s.Side != side {
				t.Errorf("%s 有锁：%+v", side, s)
			}
		}
	})
	t.Run("锁文件超过 12 小时当残留：不再当锁，报告", func(t *testing.T) {
		in := pfdtIn(b, same, changed)
		old := pfdtNow.Add(-13 * time.Hour).UnixNano()
		in.MacNames = []PFRawName{{Path: "~$report.docx", MtimeNs: old}}
		pl := PFDecide(in)
		pfdtWantLines(t, pl, "copy_over report.docx ubuntu>mac")
		if len(pl.Notes) != 1 || pl.Notes[0].Code != "stale_lock" || pl.Notes[0].Path != "~$report.docx" {
			t.Errorf("应报告残留的锁：%+v", pl.Notes)
		}
		// 刚改过的锁仍是锁；不知道 mtime 的也当锁。
		for _, mt := range []int64{pfdtNow.Add(-1 * time.Hour).UnixNano(), 0} {
			in.MacNames = []PFRawName{{Path: "~$report.docx", MtimeNs: mt}}
			pfdtSkip(t, PFDecide(in), doc, PFSkipOfficeLock)
		}
	})
	t.Run("被扫描条目里的锁文件名也认（调用方没把它当排除项）", func(t *testing.T) {
		in := pfdtIn(b, []PFScanItem{pfdtF(doc, "v1"), pfdtF("~$report.docx", "锁")}, changed)
		pfdtSkip(t, PFDecide(in), doc, PFSkipOfficeLock)
	})
}

// ---------------------------------------------------------------------------------------------
// 历史版本与台账
// ---------------------------------------------------------------------------------------------

const (
	pfdtHV  = "6-其他/历史版本"
	pfdtSQL = pfdtHV + "/档案台账.sqlite3"
	pfdtJS  = pfdtHV + "/档案索引.json"
	pfdtSnp = pfdtHV + "/文档/f1/V001_aaaaaaaa_合同.docx"
)

// pfdtHVDirs 是历史版本那几层目录（两边都有，免得被 mkdir 干扰）。
func pfdtHVDirs() []PFScanItem {
	return []PFScanItem{pfdtD("6-其他"), pfdtD(pfdtHV), pfdtD(pfdtHV + "/文档"), pfdtD(pfdtHV + "/文档/f1")}
}

func pfdtHVBase(files map[string]string) *pfdtBase {
	return pfdtNewBase(files, "6-其他", pfdtHV, pfdtHV+"/文档", pfdtHV+"/文档/f1")
}

func pfdtLedger(db, js string) []PFScanItem {
	return []PFScanItem{pfdtF(pfdtSQL, db), pfdtF(pfdtJS, js)}
}

func pfdtLedgerHash(db, js string) string { return PFLedgerPairHash(pfdtSHA(db), pfdtSHA(js)) }

func TestPFDecideHistorySnapshots(t *testing.T) {
	dirs := pfdtHVDirs()
	with := func(extra ...PFScanItem) []PFScanItem { return append(append([]PFScanItem(nil), dirs...), extra...) }

	t.Run("快照只增：新快照拷过去", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtHVBase(nil), with(pfdtF(pfdtSnp, "快照1")), dirs))
		pfdtWantLines(t, pl, "copy_new "+pfdtSnp+" mac>ubuntu")
	})
	t.Run("同名不同内容（首次）：跳过，不覆盖、不做冲突副本", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtHVBase(nil), with(pfdtF(pfdtSnp, "快照甲")), with(pfdtF(pfdtSnp, "快照乙"))))
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, pfdtSnp, PFSkipHistoryDiffers)
	})
	t.Run("同名不同内容（一边改了快照）：跳过，不覆盖", func(t *testing.T) {
		b := pfdtHVBase(map[string]string{pfdtSnp: "快照1"})
		pl := PFDecide(pfdtIn(b, with(pfdtF(pfdtSnp, "快照1")), with(pfdtF(pfdtSnp, "被改过的快照"))))
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, pfdtSnp, PFSkipHistoryDiffers)
		pl = PFDecide(pfdtIn(b, with(pfdtF(pfdtSnp, "被改过的快照")), with(pfdtF(pfdtSnp, "快照1"))))
		pfdtWantLines(t, pl)
	})
	t.Run("同名不同内容（两边都改、改的对删的）：跳过", func(t *testing.T) {
		b := pfdtHVBase(map[string]string{pfdtSnp: "快照1"})
		pl := PFDecide(pfdtIn(b, with(pfdtF(pfdtSnp, "甲")), with(pfdtF(pfdtSnp, "乙"))))
		pfdtSkip(t, pl, pfdtSnp, PFSkipHistoryDiffers)
		pl = PFDecide(pfdtIn(b, with(pfdtF(pfdtSnp, "甲")), dirs))
		pfdtSkip(t, pl, pfdtSnp, PFSkipHistoryDiffers)
		pfdtWantLines(t, pl)
	})
	t.Run("delete_version 删的快照：照常进回收", func(t *testing.T) {
		b := pfdtHVBase(map[string]string{pfdtSnp: "快照1"})
		pl := PFDecide(pfdtIn(b, with(pfdtF(pfdtSnp, "快照1")), dirs))
		pfdtWantLines(t, pl, "delete "+pfdtSnp+" @mac")
	})
	t.Run("两边都有同样的快照：只写 base", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtHVBase(nil), with(pfdtF(pfdtSnp, "快照1")), with(pfdtF(pfdtSnp, "快照1"))))
		pfdtWantLines(t, pl, "base_set "+pfdtSnp)
	})
	t.Run("非历史版本目录里同名不同内容仍是普通冲突", func(t *testing.T) {
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), []PFScanItem{pfdtF("历史.docx", "甲")}, []PFScanItem{pfdtF("历史.docx", "乙")}))
		if a := pfdtAct(t, pl, "历史.docx"); a.Kind != PFActConflict {
			t.Errorf("%s", a.Kind)
		}
	})
}

func TestPFDecideLedger(t *testing.T) {
	dirs := pfdtHVDirs()
	scan := func(db, js string, extra ...PFScanItem) []PFScanItem {
		out := append(append([]PFScanItem(nil), dirs...), pfdtLedger(db, js)...)
		return append(out, extra...)
	}
	baseWith := func(db, js string) *pfdtBase {
		b := pfdtHVBase(nil)
		b.ledger = PFBaseLedger{Hash: pfdtLedgerHash(db, js)}
		return b
	}

	t.Run("两边都没变：无事", func(t *testing.T) {
		pl := PFDecide(pfdtIn(baseWith("DB0", "J0"), scan("DB0", "J0"), scan("DB0", "J0")))
		pfdtWantLines(t, pl)
		if len(pl.Skips) != 0 {
			t.Errorf("%v", pfdtSkipLines(pl))
		}
	})
	t.Run("只有 Mac 变了：整对拷到 Ubuntu，排在快照动作之后", func(t *testing.T) {
		mac := scan("DB1", "J1", pfdtF(pfdtSnp, "新快照"))
		pl := PFDecide(pfdtIn(baseWith("DB0", "J0"), mac, scan("DB0", "J0")))
		pfdtWantLines(t, pl, "copy_new "+pfdtSnp+" mac>ubuntu", "ledger_copy "+pfdtSQL+" mac>ubuntu")
		a := pl.Actions[1]
		if a.Phase != PFPhaseLedger || a.Ledger == nil || a.Ledger.Hash != pfdtLedgerHash("DB1", "J1") || a.BaseLedgerHash != a.Ledger.Hash ||
			a.Ledger.SQLite != pfdtSQL || a.Ledger.JSON != pfdtJS || a.Ledger.Dir != pfdtHV {
			t.Fatalf("台账动作不对：%+v %+v", a, a.Ledger)
		}
		// 前提：源两个文件是 Mac 的现状，目标两个文件是 Ubuntu 判定时的那一对。
		if a.Ledger.SrcSQLite.SHA256 != pfdtSHA("DB1") || a.Ledger.SrcJSON.SHA256 != pfdtSHA("J1") ||
			a.Ledger.DstSQLite.SHA256 != pfdtSHA("DB0") || a.Ledger.DstJSON.SHA256 != pfdtSHA("J0") || a.Ledger.DstSQLite.Absent {
			t.Errorf("台账前提不对：%+v", a.Ledger)
		}
	})
	t.Run("只有 Ubuntu 变了：拷到 Mac", func(t *testing.T) {
		pl := PFDecide(pfdtIn(baseWith("DB0", "J0"), scan("DB0", "J0"), scan("DB0", "J2")))
		pfdtWantLines(t, pl, "ledger_copy "+pfdtSQL+" ubuntu>mac")
	})
	t.Run("只变了一个文件也算整对变了", func(t *testing.T) {
		pl := PFDecide(pfdtIn(baseWith("DB0", "J0"), scan("DB0", "J1"), scan("DB0", "J0")))
		pfdtWantLines(t, pl, "ledger_copy "+pfdtSQL+" mac>ubuntu")
	})
	t.Run("两边都变且不同：跳过，待合并", func(t *testing.T) {
		pl := PFDecide(pfdtIn(baseWith("DB0", "J0"), scan("DB1", "J1"), scan("DB2", "J2")))
		pfdtWantLines(t, pl)
		for _, p := range []string{pfdtSQL, pfdtJS} {
			s := pfdtSkip(t, pl, p, PFSkipLedgerBoth)
			if !strings.Contains(s.Reason, "台账待合并") {
				t.Errorf("原因：%s", s.Reason)
			}
		}
	})
	t.Run("两边都变成一样：只更新 base 的台账记录", func(t *testing.T) {
		pl := PFDecide(pfdtIn(baseWith("DB0", "J0"), scan("DB1", "J1"), scan("DB1", "J1")))
		pfdtWantLines(t, pl, "base_ledger "+pfdtSQL)
		if a := pl.Actions[0]; a.BaseLedgerHash != pfdtLedgerHash("DB1", "J1") || a.Phase != PFPhaseLedger {
			t.Errorf("%+v", a)
		}
	})
	t.Run("首次：两边各有不同的台账也算两边都变，跳过", func(t *testing.T) {
		pl := PFDecide(pfdtIn(nil, scan("DB1", "J1"), scan("DB2", "J2")))
		pfdtSkip(t, pl, pfdtSQL, PFSkipLedgerBoth)
		pfdtSkip(t, pl, pfdtJS, PFSkipLedgerBoth)
		for _, a := range pl.Actions {
			if a.Kind == PFActLedgerCopy {
				t.Errorf("不该拷台账")
			}
		}
	})
	t.Run("首次：只有一边有台账，拷过去", func(t *testing.T) {
		pl := PFDecide(pfdtIn(nil, scan("DB1", "J1"), dirs))
		pfdtWantLines(t, pl, "base_set 6-其他", "base_set "+pfdtHV, "base_set "+pfdtHV+"/文档", "base_set "+pfdtHV+"/文档/f1", "ledger_copy "+pfdtSQL+" mac>ubuntu")
		a := pl.Actions[len(pl.Actions)-1]
		if !a.Ledger.DstSQLite.Absent || !a.Ledger.DstJSON.Absent {
			t.Errorf("目标那一对应要求不存在：%+v", a.Ledger)
		}
	})
	t.Run("首次：两边台账一样，只写 base", func(t *testing.T) {
		pl := PFDecide(pfdtIn(nil, scan("DB1", "J1"), scan("DB1", "J1")))
		last := pl.Actions[len(pl.Actions)-1]
		if last.Kind != PFActBaseLedger || last.BaseLedgerHash != pfdtLedgerHash("DB1", "J1") {
			t.Errorf("%+v", last)
		}
	})
	t.Run("一边的台账丢了一个文件：不完整，跳过，不动另一边", func(t *testing.T) {
		mac := append(append([]PFScanItem(nil), dirs...), pfdtF(pfdtSQL, "DB0")) // 少了 json
		pl := PFDecide(pfdtIn(baseWith("DB0", "J0"), mac, scan("DB0", "J0")))
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, pfdtSQL, PFSkipLedgerUnknown)
	})
	t.Run("台账文件不明（未静止）：跳过", func(t *testing.T) {
		mac := append(append([]PFScanItem(nil), dirs...), pfdtNotQuiet(pfdtSQL), pfdtF(pfdtJS, "J1"))
		pl := PFDecide(pfdtIn(baseWith("DB0", "J0"), mac, scan("DB0", "J0")))
		pfdtWantLines(t, pl)
		pfdtSkip(t, pl, pfdtSQL, PFSkipLedgerUnknown)
	})
	t.Run("旁边有 journal（正在写）：推迟", func(t *testing.T) {
		for _, side := range []PFSide{PFSideMac, PFSideUbuntu} {
			in := pfdtIn(baseWith("DB0", "J0"), scan("DB1", "J1"), scan("DB0", "J0"))
			raw := []PFRawName{{Path: pfdtSQL + "-journal"}}
			if side == PFSideMac {
				in.MacNames = raw
			} else {
				in.UbuntuNames = raw
			}
			pl := PFDecide(in)
			pfdtWantLines(t, pl)
			pfdtSkip(t, pl, pfdtSQL, PFSkipLedgerJournal)
		}
	})
	t.Run("一边没有台账（目录在）：拷过去，只增不删", func(t *testing.T) {
		b := baseWith("DB0", "J0")
		pl := PFDecide(pfdtIn(b, dirs, scan("DB0", "J0")))
		pfdtWantLines(t, pl, "ledger_copy "+pfdtSQL+" ubuntu>mac")
	})
	t.Run("台账不走普通文件规则：不会被当成删除或冲突", func(t *testing.T) {
		b := pfdtHVBase(map[string]string{pfdtSQL: "旧"}) // 万一 base 条目里有，也不理
		pl := PFDecide(pfdtIn(b, scan("DB1", "J1"), dirs))
		for _, a := range pl.Actions {
			if a.Kind == PFActDelete || a.Kind == PFActConflict || a.Kind == PFActCopyOver {
				t.Errorf("%s %s", a.Kind, a.Path)
			}
		}
	})
	t.Run("多处台账：都不自动同步", func(t *testing.T) {
		other := "别的项目/历史版本"
		mac := append(scan("DB1", "J1"), pfdtD("别的项目"), pfdtD(other), pfdtF(other+"/档案台账.sqlite3", "x"), pfdtF(other+"/档案索引.json", "y"))
		pl := PFDecide(pfdtIn(pfdtNewBase(nil), mac, dirs))
		pfdtSkip(t, pl, pfdtSQL, PFSkipLedgerMulti)
		pfdtSkip(t, pl, other+"/档案台账.sqlite3", PFSkipLedgerMulti)
		for _, a := range pl.Actions {
			if a.Kind == PFActLedgerCopy {
				t.Errorf("不该拷台账")
			}
		}
	})
	t.Run("历史版本目录被跳过时台账一起跳过", func(t *testing.T) {
		mac := []PFScanItem{pfdtD("6-其他"), pfdtBadDir(pfdtHV)}
		pl := PFDecide(pfdtIn(pfdtHVBase(nil), mac, append(append([]PFScanItem(nil), dirs...), pfdtLedger("DB0", "J0")...)))
		for _, a := range pl.Actions {
			if a.Kind == PFActLedgerCopy {
				t.Errorf("不该拷台账")
			}
		}
	})
}

// ---------------------------------------------------------------------------------------------
// 排序与结果确定
// ---------------------------------------------------------------------------------------------

// pfdtRich 造一个什么都有一点的场景。
func pfdtRich() PFDecideInput {
	b := pfdtHVBase(map[string]string{
		"a.txt": "v1", "gone.txt": "g", "c.docx": "v1", "old/x.mp4": "XX", "old/y.mp4": "YY", "keep/k.bin": "KK",
		"link/z.txt": "z", pfdtSnp: "快照1",
	})
	b.entries["old"] = pfdtNewBase(nil, "old").entries["old"]
	b.entries["keep"] = pfdtNewBase(nil, "keep").entries["keep"]
	b.entries["link"] = pfdtNewBase(nil, "link").entries["link"]
	b.ledger = PFBaseLedger{Hash: pfdtLedgerHash("DB0", "J0")}
	mac := append(pfdtHVDirs(),
		pfdtF("a.txt", "v2", pfdtAt(9, 0, 0)), pfdtF("gone.txt", "g"), pfdtF("c.docx", "mc", pfdtAt(9, 0, 0)),
		pfdtD("old"), pfdtF("old/x.mp4", "XX"), pfdtF("old/y.mp4", "YY"),
		pfdtD("keep"), pfdtF("keep/k.bin", "KK"), pfdtD("copy"), pfdtF("copy/k2.bin", "KK"),
		pfdtLink("link"), pfdtF("new.txt", "新"), pfdtNotQuiet("writing.bin"), pfdtF("Same.txt", "s"),
		pfdtF(pfdtSnp, "快照1"), pfdtF(pfdtHV+"/文档/f1/V002_bbbbbbbb_附件.docx", "快照2"),
		pfdtF(pfdtSQL, "DB1"), pfdtF(pfdtJS, "J1"))
	ub := append(pfdtHVDirs(),
		pfdtF("a.txt", "v1"), pfdtF("c.docx", "uc", pfdtAt(10, 0, 0)),
		pfdtD("new"), pfdtF("new/x.mp4", "XX"), pfdtF("new/y.mp4", "YY"),
		pfdtD("keep"), pfdtF("keep/k.bin", "KK"), pfdtD("link"), pfdtF("link/z.txt", "z"),
		pfdtF("same.txt", "s"), pfdtF("only-ub.txt", "乙"),
		pfdtF(pfdtSnp, "快照1"),
		pfdtF(pfdtSQL, "DB0"), pfdtF(pfdtJS, "J0"))
	in := pfdtIn(b, mac, ub)
	in.MacNames = []PFRawName{{Path: "~$c.docx"}, {Path: ".DS_Store"}, {Path: "old/.DS_Store"}}
	in.UbuntuNames = []PFRawName{{Path: "~$a.docx"}}
	return in
}

func TestPFDecideRichScenario(t *testing.T) {
	pl := PFDecide(pfdtRich())
	pfdtMustNotStop(t, pl)
	// 这个场景的预期，逐条核一遍：
	pfdtWantLines(t, pl,
		"mkdir copy mac>ubuntu",
		"mkdir new ubuntu>mac",
		"copy_new "+pfdtHV+"/文档/f1/V002_bbbbbbbb_附件.docx mac>ubuntu",
		"copy_over a.txt mac>ubuntu",
		"clone_local copy/k2.bin mac>ubuntu from=keep/k.bin",
		"delete gone.txt @mac",
		"copy_new new.txt mac>ubuntu",
		"rename_local new/x.mp4 ubuntu>mac from=old/x.mp4",
		"rename_local new/y.mp4 ubuntu>mac from=old/y.mp4",
		"copy_new only-ub.txt ubuntu>mac",
		"rmdir old @mac",
		"ledger_copy "+pfdtSQL+" mac>ubuntu")
	pfdtSkip(t, pl, "c.docx", PFSkipOfficeLock) // Mac 上 c.docx 有锁，且两边都改了
	pfdtSkip(t, pl, "link", PFSkipOutOfScope)
	pfdtSkip(t, pl, "link/z.txt", PFSkipSubtree)
	pfdtSkip(t, pl, "writing.bin", PFSkipUnknown)
	pfdtSkip(t, pl, "Same.txt", PFSkipCrossSideClash)
	pfdtSkip(t, pl, "same.txt", PFSkipCrossSideClash)
}

func TestPFDecideDeterministicUnderShuffle(t *testing.T) {
	in := pfdtRich()
	want := pfdtJSON(t, PFDecide(in))
	for i := 0; i < 50; i++ {
		r := rand.New(rand.NewSource(int64(i)))
		shuf := in
		shuf.Mac = &PFScanResult{Items: pfdtShuffled(r, in.Mac.Items)}
		shuf.Ubuntu = &PFScanResult{Items: pfdtShuffled(r, in.Ubuntu.Items)}
		shuf.MacNames = pfdtShuffledNames(r, in.MacNames)
		shuf.UbuntuNames = pfdtShuffledNames(r, in.UbuntuNames)
		if got := pfdtJSON(t, PFDecide(shuf)); got != want {
			t.Fatalf("第 %d 次打乱后输出不同\n原:%s\n新:%s", i, want, got)
		}
	}
	if got := pfdtJSON(t, PFDecide(in)); got != want {
		t.Fatal("同一输入两次输出不同")
	}
}

func pfdtShuffled(r *rand.Rand, items []PFScanItem) []PFScanItem {
	out := append([]PFScanItem(nil), items...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func pfdtShuffledNames(r *rand.Rand, names []PFRawName) []PFRawName {
	out := append([]PFRawName(nil), names...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// 动作按 (阶段, 路径) 排序：建目录最先，rmdir 深的先，台账最后。
func TestPFDecideActionOrder(t *testing.T) {
	pl := PFDecide(PFDecideInput{})
	if !pl.Verdict.Stopped {
		t.Fatal("两边都没有目录应停下")
	}
	pl = PFDecide(pfdtRich())
	last := -1
	var lastPath string
	for _, a := range pl.Actions {
		if a.Phase < last {
			t.Fatalf("阶段不是升序：%v", pfdtLines(pl))
		}
		if a.Phase == last && a.Phase != PFPhaseRmdir && a.Path < lastPath {
			t.Fatalf("同一阶段路径不是升序：%v", pfdtLines(pl))
		}
		if a.Phase == last && a.Phase == PFPhaseRmdir && a.Path > lastPath {
			t.Fatalf("rmdir 应深的先：%v", pfdtLines(pl))
		}
		last, lastPath = a.Phase, a.Path
	}
	// 台账在所有快照文件动作之后。
	li, si := -1, -1
	for i, a := range pl.Actions {
		if a.Kind == PFActLedgerCopy {
			li = i
		}
		if strings.Contains(a.Path, "/文档/f1/") {
			si = i
		}
	}
	if li < 0 || si < 0 || li < si {
		t.Fatalf("台账要排在快照动作之后：ledger=%d snapshot=%d", li, si)
	}
}

// ---------------------------------------------------------------------------------------------
// 随机性质测试
// ---------------------------------------------------------------------------------------------

type pfdtSpec struct {
	path  string
	isDir bool
}

var pfdtPool = []pfdtSpec{
	{"a.txt", false}, {"b.txt", false}, {"c.docx", false}, {"j.docx", false},
	{"d1", true}, {"d1/f1.txt", false}, {"d1/f2.txt", false}, {"d1/s", true}, {"d1/s/g.txt", false},
	{"d2", true}, {"d2/h.txt", false}, {"d2/i.txt", false},
	{"历史版本", true}, {"历史版本/V1.txt", false}, {"历史版本/V2.txt", false},
	{pfdtSQL2, false}, {pfdtJS2, false},
}

const (
	pfdtSQL2 = "历史版本/档案台账.sqlite3"
	pfdtJS2  = "历史版本/档案索引.json"
)

var pfdtContents = []string{"v1", "v2", "v3", "v4"}

// pfdtGenSide 按 base 随机造一边的扫描结果。okDir 记这一边哪些目录是「在范围内的目录」，没进去看的目录下不放条目。
func pfdtGenSide(r *rand.Rand, base *pfdtBase, withBase bool) []PFScanItem {
	var items []PFScanItem
	okDir := map[string]bool{"": true}
	for _, sp := range pfdtPool { // 池里父目录都排在子项前面
		parent := pfdParent(sp.path)
		if !okDir[parent] {
			continue // 父目录不存在或没看：这里不放东西（不明/超出范围的目录下面本来就扫不到）
		}
		be, inBase := base.entries[sp.path]
		inBase = inBase && withBase
		if sp.isDir {
			switch roll := r.Intn(10); {
			case inBase && roll < 6, !inBase && roll < 3:
				items = append(items, pfdtD(sp.path))
				okDir[sp.path] = true
			case roll == 9:
				if r.Intn(2) == 0 {
					items = append(items, pfdtLink(sp.path))
				} else {
					items = append(items, pfdtBadDir(sp.path))
				}
			}
			continue
		}
		var content string
		switch roll := r.Intn(20); {
		case inBase && roll < 8: // 没改
			content = ""
			for _, c := range pfdtContents {
				if pfdtSHA(c) == be.SHA256 {
					content = c
				}
			}
		case inBase && roll < 12: // 改了
			content = pfdtContents[2+r.Intn(2)]
		case inBase && roll < 17: // 删了
			continue
		case !inBase && roll < 7: // 新建
			content = pfdtContents[r.Intn(len(pfdtContents))]
		case roll == 19: // 不明
			if r.Intn(3) == 0 {
				it := pfdtF(sp.path, "x")
				it.Hash = ""
				items = append(items, it)
			} else if r.Intn(2) == 0 {
				items = append(items, pfdtNotQuiet(sp.path))
			} else {
				items = append(items, pfdtPlaceholder(sp.path))
			}
			continue
		default:
			continue
		}
		if content == "" {
			continue
		}
		items = append(items, pfdtF(sp.path, content, pfdtAt(8+r.Intn(8), r.Intn(60), r.Intn(4))))
	}
	return items
}

func pfdtGenBase(r *rand.Rand) *pfdtBase {
	b := pfdtNewBase(nil)
	inB := map[string]bool{"": true}
	for _, sp := range pfdtPool {
		if sp.path == pfdtSQL2 || sp.path == pfdtJS2 || !inB[pfdtParent(sp.path)] || r.Intn(10) >= 7 {
			continue
		}
		if sp.isDir {
			b.entries[sp.path] = pfdtNewBase(nil, sp.path).entries[sp.path]
		} else {
			b.entries[sp.path] = pfdtNewBase(map[string]string{sp.path: pfdtContents[r.Intn(2)]}).entries[sp.path]
		}
		inB[sp.path] = true
	}
	return b
}

func pfdtParent(p string) string { return pfdParent(p) }

func pfdtItemMap(items []PFScanItem) map[string]PFScanItem {
	m := map[string]PFScanItem{}
	for _, it := range items {
		m[it.Path] = it
	}
	return m
}

// pfdtCheckInvariants 检查任何计划都必须满足的性质。
func pfdtCheckInvariants(t *testing.T, tag string, in PFDecideInput, pl *PFPlan) {
	t.Helper()
	fail := func(format string, a ...any) {
		t.Helper()
		t.Fatalf("%s：%s\n动作：%v\n跳过：%v", tag, fmt.Sprintf(format, a...), pfdtLines(pl), pfdtSkipLines(pl))
	}
	if pl.Verdict.Stopped {
		if len(pl.Actions) != 0 {
			fail("停下的单元不该有动作")
		}
		return
	}
	scans := map[PFSide]*PFScanResult{PFSideMac: in.Mac, PFSideUbuntu: in.Ubuntu}
	items := map[PFSide]map[string]PFScanItem{PFSideMac: pfdtItemMap(in.Mac.Items), PFSideUbuntu: pfdtItemMap(in.Ubuntu.Items)}
	var base PFBaseView
	if !pl.Verdict.FirstTime {
		base = in.Base
	}
	skipped := map[string]bool{}
	for _, s := range pl.Skips {
		skipped[s.Path] = true
	}
	absent := func(side PFSide, path string) bool {
		if _, ok := items[side][path]; ok {
			return false
		}
		_, sub := scans[side].SubtreeSkipped(path)
		return !sub
	}
	okFile := func(side PFSide, path string) (PFScanItem, bool) {
		it, ok := items[side][path]
		return it, ok && it.State == PFStateOK && it.Kind == PFKindFile && pfIsHex64(it.Hash)
	}
	same := func(e *PFExpect, it PFScanItem) bool {
		if it.Kind == PFKindDir {
			return e != nil && e.T == "d" && e.Ino == it.Stat.Ino
		}
		return e != nil && !e.Absent && e.T == "f" && e.SHA256 == it.Hash && e.Size == it.Stat.Size && e.MtimeNs == it.Stat.MtimeNs && e.Ino == it.Stat.Ino
	}
	// 不明、超出范围的路径不出现在任何动作里（含 From、冲突名）。
	check := func(a PFAction, path string) {
		if path == "" {
			return
		}
		for _, side := range []PFSide{PFSideMac, PFSideUbuntu} {
			if it, ok := items[side][path]; ok {
				if it.State != PFStateOK {
					fail("%s %s：%s 上这个路径是 %s，不该出现在动作里", a.Kind, a.Path, side, it.State)
				}
				if it.Kind == PFKindFile && !pfIsHex64(it.Hash) {
					fail("%s %s：%s 上这个文件没有哈希，不该出现在动作里", a.Kind, a.Path, side)
				}
			}
			if _, sub := scans[side].SubtreeSkipped(path); sub {
				fail("%s %s：%s 上这个路径在没看的子树里", a.Kind, a.Path, side)
			}
		}
		if skipped[path] {
			fail("%s %s：路径在跳过清单里却出现在动作里", a.Kind, a.Path)
		}
	}
	for _, a := range pl.Actions {
		check(a, a.Path)
		check(a, a.From)
		if a.Kind == PFActLedgerCopy || a.Kind == PFActBaseLedger {
			continue
		}
		var be PFBaseEntry
		var inBase bool
		if base != nil {
			be, inBase = base.Get(a.Path)
		}
		switch a.Kind {
		case PFActDelete:
			if !inBase || be.T != "f" {
				fail("删除 %s：base 里没有它", a.Path)
			}
			it, ok := okFile(a.Dst, a.Path)
			if !ok || it.Hash != be.SHA256 || it.Stat.Size != be.Size {
				fail("删除 %s：%s 那一边不是 OK 或改过了", a.Path, a.Dst)
			}
			if !same(a.DstExpect, it) {
				fail("删除 %s：前提和扫描不符", a.Path)
			}
			if !absent(a.Dst.Other(), a.Path) {
				fail("删除 %s：另一边并没有删", a.Path)
			}
		case PFActCopyOver:
			if !inBase {
				fail("覆盖 %s：base 里没有", a.Path)
			}
			it, ok := okFile(a.Dst, a.Path)
			if !ok || it.Hash != be.SHA256 || !same(a.DstExpect, it) {
				fail("覆盖 %s：被覆盖的一边不等于 base", a.Path)
			}
			if src, ok := okFile(a.Src, a.Path); !ok || !same(a.SrcExpect, src) {
				fail("覆盖 %s：源前提和扫描不符", a.Path)
			}
		case PFActCopyNew:
			if !absent(a.Dst, a.Path) || a.DstExpect == nil || !a.DstExpect.Absent {
				fail("新建 %s：目标边已有", a.Path)
			}
			if src, ok := okFile(a.Src, a.Path); !ok || !same(a.SrcExpect, src) {
				fail("新建 %s：源前提和扫描不符", a.Path)
			}
		case PFActMkdir:
			if a.Path != "" {
				if !absent(a.Dst, a.Path) {
					fail("mkdir %s：目标边已有", a.Path)
				}
				if src, ok := items[a.Src][a.Path]; !ok || src.Kind != PFKindDir || src.State != PFStateOK {
					fail("mkdir %s：源边不是目录", a.Path)
				}
			}
		case PFActRmdir:
			it, ok := items[a.Dst][a.Path]
			if !inBase || be.T != "d" || !ok || it.Kind != PFKindDir || it.State != PFStateOK || !a.IfEmpty {
				fail("rmdir %s 不满足条件", a.Path)
			}
			if !absent(a.Dst.Other(), a.Path) {
				fail("rmdir %s：另一边并没有删", a.Path)
			}
		case PFActConflict:
			m, ok1 := okFile(PFSideMac, a.Path)
			u, ok2 := okFile(PFSideUbuntu, a.Path)
			if !ok1 || !ok2 || m.Hash == u.Hash || !same(a.SrcExpect, items[a.Src][a.Path]) || !same(a.DstExpect, items[a.Dst][a.Path]) {
				fail("冲突 %s 的前提不对", a.Path)
			}
			for _, s := range []PFSide{PFSideMac, PFSideUbuntu} {
				if _, ok := items[s][a.Conflict.ConflictPath]; ok {
					fail("冲突名 %s 已存在", a.Conflict.ConflictPath)
				}
			}
		case PFActRename:
			it, ok := okFile(a.Dst, a.From)
			if !ok || !same(a.FromExpect, it) || it.Hash != a.SrcExpect.SHA256 || !absent(a.Dst, a.Path) {
				fail("改名 %s ← %s 的前提不对", a.Path, a.From)
			}
		case PFActClone:
			it, ok := okFile(a.Dst, a.From)
			if !ok || !same(a.FromExpect, it) || it.Hash != a.SrcExpect.SHA256 || !absent(a.Dst, a.Path) {
				fail("复制 %s ← %s 的前提不对", a.Path, a.From)
			}
		}
		if pl.Verdict.FirstTime {
			switch a.Kind {
			case PFActDelete, PFActRmdir, PFActCopyOver, PFActRename:
				fail("首次规则不该有 %s", a.Kind)
			}
		}
	}
	// 凡是不是 OK 的路径，都不出现在动作里（已在 check 里查）；再确认 Unknown 的整棵都在跳过里或没被动。
	for _, side := range []PFSide{PFSideMac, PFSideUbuntu} {
		for _, it := range scans[side].Items {
			if it.State != PFStateOK {
				if !skipped[it.Path] {
					// 不明/超出范围的条目，要么在跳过清单里，要么是台账那一对（另判）。
					if n := pfBase(it.Path); n != pfLedgerSQLite && n != pfLedgerJSON {
						fail("%s 上 %s 是 %s，却不在跳过清单里", side, it.Path, it.State)
					}
				}
			}
		}
	}
	// 动作之间：同一路径只有一个动作（台账、根目录除外）。
	seen := map[string]bool{}
	for _, a := range pl.Actions {
		if a.Kind == PFActLedgerCopy || a.Kind == PFActBaseLedger {
			continue
		}
		key := string(a.Kind) + "|" + a.Path + "|" + string(a.Dst)
		if seen[key] {
			fail("重复的动作 %s", key)
		}
		seen[key] = true
	}
}

func TestPFDecideRandomProperties(t *testing.T) {
	n := 600
	if testing.Short() {
		n = 100
	}
	seen := map[string]int{}
	firstTimes := 0
	for seed := int64(1); seed <= int64(n); seed++ {
		r := rand.New(rand.NewSource(seed))
		base := pfdtGenBase(r)
		withBase := r.Intn(6) != 0 // 偶尔当没有 base（首次）
		mac := pfdtSorted(pfdtGenSide(r, base, withBase))
		ub := pfdtSorted(pfdtGenSide(r, base, withBase))
		var b *pfdtBase
		if withBase {
			b = base
		}
		in := pfdtIn(b, mac, ub)
		if r.Intn(5) == 0 {
			in.ClockSkew = 10 * time.Minute
		}
		if r.Intn(7) == 0 { // 偶尔 G2
			in.UbuntuDirID = PFDirID{Dev: 2, Ino: 5}
		}
		for _, lock := range []string{"c.docx", "j.docx"} {
			if r.Intn(4) == 0 {
				in.MacNames = append(in.MacNames, PFRawName{Path: "~$" + lock})
			}
			if r.Intn(4) == 0 {
				in.UbuntuNames = append(in.UbuntuNames, PFRawName{Path: "~$" + lock})
			}
		}
		pl := PFDecide(in)
		tag := fmt.Sprintf("seed=%d", seed)
		pfdtCheckInvariants(t, tag, in, pl)
		for _, a := range pl.Actions {
			seen[string(a.Kind)]++
		}
		for _, s := range pl.Skips {
			seen["skip:"+s.Code]++
		}
		if pl.Verdict.FirstTime {
			firstTimes++
		}

		// 打乱条目顺序，输出逐字节相同。
		want := pfdtJSON(t, pl)
		shuf := in
		shuf.Mac = &PFScanResult{Items: pfdtShuffled(r, mac)}
		shuf.Ubuntu = &PFScanResult{Items: pfdtShuffled(r, ub)}
		if got := pfdtJSON(t, PFDecide(shuf)); got != want {
			t.Fatalf("%s：打乱后输出不同\n原:%s\n新:%s", tag, want, got)
		}
	}
	// 别让随机测试空转：各种动作、各种跳过原因、首次规则都要真的出现过。
	if !testing.Short() {
		for _, k := range []string{"copy_new", "copy_over", "delete", "conflict", "mkdir", "rmdir", "rename_local", "clone_local",
			"base_set", "base_del", "ledger_copy", "skip:unknown", "skip:out_of_scope", "skip:subtree_not_seen", "skip:office_lock_target", "skip:history_snapshot_differs"} {
			if seen[k] == 0 {
				t.Errorf("%d 组随机输入里一次都没出现过 %s（出现次数：%v）", n, k, seen)
			}
		}
		if firstTimes == 0 {
			t.Errorf("一次首次规则都没出现过")
		}
	}
	t.Logf("%d 组随机输入，各类出现次数：%v，首次规则 %d 次", n, seen, firstTimes)
}

// 和真的 PFBase（T4）对得上：存盘、重新载入后直接当视图用。
func TestPFDecideWithRealBase(t *testing.T) {
	meta := t.TempDir()
	b, err := PFBaseCreate(meta, pfTU, PFBaseMeta{MacRel: pfdtRel, UbuntuRel: pfdtRel, MacDirID: pfdtMacDir, UbuntuDirID: pfdtUbDir})
	if err != nil {
		t.Fatal(err)
	}
	st := PFSideStat{MtimeNs: pfdtT0 * int64(time.Second), Ino: 1}
	if err := b.Apply([]PFBaseOp{
		{Path: "a.txt", Entry: PFBaseEntry{T: PFBaseTypeFile, Size: 2, SHA256: pfdtSHA("v1"), Mac: st, Ub: st}},
		{Path: "gone.txt", Entry: PFBaseEntry{T: PFBaseTypeFile, Size: 1, SHA256: pfdtSHA("g"), Mac: st, Ub: st}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Compact(); err != nil {
		t.Fatal(err)
	}
	r, err := PFBaseLoad(meta, pfTU)
	if err != nil || r.FirstTime() {
		t.Fatalf("载入失败：%v %+v", err, r)
	}
	in := pfdtIn(nil, []PFScanItem{pfdtF("a.txt", "v1"), pfdtF("gone.txt", "g")}, []PFScanItem{pfdtF("a.txt", "v2")})
	in.Base = r.Base
	pl := PFDecide(in)
	pfdtWantLines(t, pl, "copy_over a.txt ubuntu>mac", "delete gone.txt @mac")

	// 载入失败（r.FirstTime()）的情形：Base 不传。
	in.Base = nil
	in.BaseProblem = "测试"
	pl = PFDecide(in)
	pfdtWantLines(t, pl, "conflict a.txt mac>ubuntu", "copy_new gone.txt mac>ubuntu")
}
