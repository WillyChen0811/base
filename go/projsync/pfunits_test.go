package projsync

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// 本文件测 pfunits.go：编号识别（金样）、单元发现、登记表、新单元判定、工作目录检查。
// 名字、路径全部虚构。

// ---------------------------------------------------------------------------
// 编号识别：金样由 Python 跑 xxyi 真正的正则生成（testdata/pfunits_codes.json）
// ---------------------------------------------------------------------------

type pfCodesGoldenCase struct {
	Name     string   `json:"name"`
	First    *string  `json:"first"`
	FirstRaw *string  `json:"first_raw"`
	All      []string `json:"all"`
	AllRaw   []string `json:"all_raw"`
	Note     string   `json:"note"`
}

type pfCodesGolden struct {
	Pattern   string              `json:"pattern"`
	Cases     []pfCodesGoldenCase `json:"cases"`
	NormCases []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"norm_cases"`
}

func loadPFCodesGolden(t *testing.T) pfCodesGolden {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "pfunits_codes.json"))
	if err != nil {
		t.Fatalf("读金样失败: %v", err)
	}
	var g pfCodesGolden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("解析金样失败: %v", err)
	}
	return g
}

func TestPFCodesGolden(t *testing.T) {
	g := loadPFCodesGolden(t)
	// 防止金样被截断或字段对不上（全部解析成空也会「全部一致」）
	hits, multi := 0, 0
	for _, c := range g.Cases {
		if c.First != nil {
			hits++
		}
		if len(c.All) > 1 {
			multi++
		}
	}
	if len(g.Cases) < 60 || hits < 40 || multi < 3 {
		t.Fatalf("金样太少或没解析出来: 共 %d 个，命中 %d 个，多编号 %d 个", len(g.Cases), hits, multi)
	}
	if !strings.Contains(g.Pattern, "(?<![0-9A-Za-z])") {
		t.Fatalf("金样里没有记正则源码: %q", g.Pattern)
	}
	bad := 0
	for _, c := range g.Cases {
		gotFirst, ok := PFCodeFromName(c.Name)
		switch {
		case c.First == nil && ok:
			t.Errorf("[%s] %q: Python 认不出，Go 认出了 %q", c.Note, c.Name, gotFirst)
			bad++
		case c.First != nil && !ok:
			t.Errorf("[%s] %q: Python 认出 %q，Go 认不出", c.Note, c.Name, *c.First)
			bad++
		case c.First != nil && gotFirst != *c.First:
			t.Errorf("[%s] %q: 第一个编号 Python %q，Go %q", c.Note, c.Name, *c.First, gotFirst)
			bad++
		}
		gotAll := PFCodesInName(c.Name)
		if len(gotAll) != len(c.All) || (len(c.All) > 0 && !reflect.DeepEqual(gotAll, c.All)) {
			t.Errorf("[%s] %q: 全部编号 Python %q，Go %q", c.Note, c.Name, c.All, gotAll)
			bad++
		}
		gotRaw := pfFindCodes(c.Name, false)
		if len(gotRaw) != len(c.AllRaw) || (len(c.AllRaw) > 0 && !reflect.DeepEqual(gotRaw, c.AllRaw)) {
			t.Errorf("[%s] %q: 归一化前的全部匹配 Python %q，Go %q", c.Note, c.Name, c.AllRaw, gotRaw)
			bad++
		}
		if bad > 30 {
			t.Fatal("不一致太多，不再往下报")
		}
	}
}

func TestPFNormCodeGolden(t *testing.T) {
	g := loadPFCodesGolden(t)
	if len(g.NormCases) < 10 {
		t.Fatalf("归一化金样太少: %d", len(g.NormCases))
	}
	for _, c := range g.NormCases {
		if got := PFNormCode(c.In); got != c.Out {
			t.Errorf("PFNormCode(%q) = %q，Python _norm 是 %q", c.In, got, c.Out)
		}
	}
}

func TestPFCodeFromNameMisc(t *testing.T) {
	// 名字里有不合法的 UTF-8 字节：不崩，不影响两侧「挨着」的判断
	if c, ok := PFCodeFromName("\xff260403J0812\xfe"); !ok || c != "260403J0812" {
		t.Errorf("坏字节两侧的编号: %q %v", c, ok)
	}
	if c, ok := PFCodeFromName("CCIC XMST 202605052-JB01"); !ok || c != "CCICXMST202605052" {
		t.Errorf("带空格旧格式: %q %v", c, ok)
	}
	if _, ok := PFCodeFromName(""); ok {
		t.Error("空名字不该认出编号")
	}
}

// ---------------------------------------------------------------------------
// 单元发现
// ---------------------------------------------------------------------------

func pfMkdirs(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, r := range rels {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(r)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func pfWriteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pfPairs(us []PFFoundUnit) []string {
	var out []string
	for _, u := range us {
		out = append(out, u.Code+" @ "+u.Rel)
	}
	return out
}

func pfSkipRels(ss []PFDiscoverSkip) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Rel)
	}
	sort.Strings(out)
	return out
}

func TestPFDiscoverUnitsTree(t *testing.T) {
	root := t.TempDir()
	pfMkdirs(t, root,
		"2026/260403J0812-甲项目/1-客户资料提供",
		"2026/260403J0812-甲项目/260403J0813-内层不该被发现",
		"2025/CCICXMST202605022 乙项目",
		"2026/杂项/260403Q0901-深层",
		"a/b/c/d/260403P0101-第五层",
		"a/b/c/d/e/260403P0102-第六层",
		"2024/TMP2026090201-临时",
		"2023/CCIC XMST 202605052 带空格",
		"2023/无编号的目录/资料",
		".回收/260403N0101-已删",
	)
	pfWriteFile(t, filepath.Join(root, "260403Y0101.docx"))
	pfWriteFile(t, filepath.Join(root, "2026", "说明.txt"))

	res, err := PFDiscoverUnits(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CCICXMST202605052 @ 2023/CCIC XMST 202605052 带空格",
		"TMP2026090201 @ 2024/TMP2026090201-临时",
		"CCICXMST202605022 @ 2025/CCICXMST202605022 乙项目",
		"260403J0812 @ 2026/260403J0812-甲项目",
		"260403Q0901 @ 2026/杂项/260403Q0901-深层",
		"260403P0101 @ a/b/c/d/260403P0101-第五层",
	}
	if got := pfPairs(res.Units); !reflect.DeepEqual(got, want) {
		t.Fatalf("单元不对:\n got %q\nwant %q", got, want)
	}
	for _, u := range res.Units {
		if u.Dup || len(u.DupRels) != 0 {
			t.Errorf("不该标重复: %+v", u)
		}
	}
	// 第 5 层的 e 认不出编号：报告「到顶」，里面的第六层不找
	if got := pfSkipRels(res.Skipped); !reflect.DeepEqual(got, []string{"a/b/c/d/e"}) {
		t.Errorf("Skipped = %q", got)
	}
	if !strings.Contains(res.Skipped[0].Reason, "第 5 层") || res.Skipped[0].Unreadable {
		t.Errorf("到顶原因不对: %+v", res.Skipped[0])
	}
	if !res.Incomplete {
		t.Error("有目录没往里看，Incomplete 应为真")
	}
	if res.Excluded != 1 {
		t.Errorf("「.」开头的目录应计 1 个，得 %d", res.Excluded)
	}
}

func TestPFDiscoverUnitsClean(t *testing.T) {
	root := t.TempDir()
	pfMkdirs(t, root, "2026/260403J0812-甲", "2026/260403J0813-乙", "2025/其他")
	res, err := PFDiscoverUnits(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Units) != 2 || len(res.Skipped) != 0 || res.Incomplete || res.Excluded != 0 {
		t.Fatalf("干净的树不该有跳过: %+v", res)
	}
	// 空根：没有单元，也不是错误
	res, err = PFDiscoverUnits(t.TempDir())
	if err != nil || len(res.Units) != 0 || res.Incomplete {
		t.Fatalf("空根: %+v %v", res, err)
	}
}

func TestPFDiscoverUnitsDuplicates(t *testing.T) {
	root := t.TempDir()
	pfMkdirs(t, root,
		"2026/260403J0812-甲",
		"2025/260403j0812-补（小写，归一化后同编号）",
		"2026/260403J0812-同一层的另一个",
		"2026/CCICXMST202605022-不重复",
		"2026/260403Q0901-另一个编号",
	)
	res, err := PFDiscoverUnits(root)
	if err != nil {
		t.Fatal(err)
	}
	dup := map[string]PFFoundUnit{}
	for _, u := range res.Units {
		if u.Dup {
			dup[u.Rel] = u
		} else if u.Code == "260403J0812" {
			t.Errorf("同编号的目录没标重复: %+v", u)
		}
	}
	if len(dup) != 3 {
		t.Fatalf("应有 3 个重复目录，得 %d: %+v", len(dup), res.Units)
	}
	a := dup["2026/260403J0812-甲"]
	if !reflect.DeepEqual(a.DupRels, []string{"2025/260403j0812-补（小写，归一化后同编号）", "2026/260403J0812-同一层的另一个"}) {
		t.Errorf("DupRels = %q", a.DupRels)
	}
	for _, u := range res.Units {
		if u.Code != "260403J0812" && (u.Dup || len(u.DupRels) != 0) {
			t.Errorf("不重复的被标了重复: %+v", u)
		}
	}
}

func TestPFDiscoverUnitsOthers(t *testing.T) {
	root := t.TempDir()
	pfMkdirs(t, root, "2026/260403J0812-260403Q0901合并-260403J0812")
	res, err := PFDiscoverUnits(root)
	if err != nil || len(res.Units) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	u := res.Units[0]
	if u.Code != "260403J0812" || !reflect.DeepEqual(u.Others, []string{"260403Q0901"}) {
		t.Errorf("取第一个编号为单元键，其余记 Others: %+v", u)
	}
}

func TestPFDiscoverUnitsSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	ext := filepath.Join(base, "ext")
	pfMkdirs(t, root, "2026/260403J0812-真实")
	pfMkdirs(t, ext, "260403Z0301-外部真实", "2020/260403Z0302-链接年份里")
	link := func(target, path string) {
		t.Helper()
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("建不了符号链接: %v", err)
		}
	}
	link(filepath.Join(ext, "260403Z0301-外部真实"), filepath.Join(root, "2026", "260403Z0301-链接"))
	link(filepath.Join(ext, "2020"), filepath.Join(root, "链接的年份"))
	link(filepath.Join(ext, "2020"), filepath.Join(root, ".隐藏链接"))

	res, err := PFDiscoverUnits(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := pfPairs(res.Units); !reflect.DeepEqual(got, []string{"260403J0812 @ 2026/260403J0812-真实"}) {
		t.Fatalf("不该跟随链接找出单元: %q", got)
	}
	if got := pfSkipRels(res.Skipped); !reflect.DeepEqual(got, []string{"2026/260403Z0301-链接", "链接的年份"}) {
		t.Errorf("Skipped = %q", got)
	}
	for _, s := range res.Skipped {
		if s.Reason != pfReasonSymlink || s.Unreadable {
			t.Errorf("跳过原因不对: %+v", s)
		}
	}
	if !res.Incomplete || res.Excluded != 1 {
		t.Errorf("Incomplete=%v Excluded=%d", res.Incomplete, res.Excluded)
	}
}

func TestPFDiscoverUnitsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 用户读得了 0 权限的目录")
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	pfMkdirs(t, root, "2026/260403J0812-甲", "锁住/260403W0101-读不到")
	locked := filepath.Join(root, "锁住")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("这个环境下 0 权限目录仍可读")
	}

	res, err := PFDiscoverUnits(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := pfPairs(res.Units); !reflect.DeepEqual(got, []string{"260403J0812 @ 2026/260403J0812-甲"}) {
		t.Errorf("其余单元照常: %q", got)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Rel != "锁住" || !res.Skipped[0].Unreadable || !res.Incomplete {
		t.Errorf("读不了的目录要报出来: %+v incomplete=%v", res.Skipped, res.Incomplete)
	}

	// 根本身读不了：返回 error，不能当成「一个单元都没有」
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o755) })
	if _, err := PFDiscoverUnits(root); err == nil {
		t.Error("根读不了应返回 error")
	}
}

func TestPFDiscoverUnitsRootErrors(t *testing.T) {
	if _, err := PFDiscoverUnits(""); err == nil {
		t.Error("空根应报错")
	}
	if _, err := PFDiscoverUnits(filepath.Join(t.TempDir(), "不存在")); err == nil {
		t.Error("根不存在应报错")
	}
	f := filepath.Join(t.TempDir(), "文件")
	pfWriteFile(t, f)
	if _, err := PFDiscoverUnits(f); err == nil {
		t.Error("根是文件应报错")
	}
}

// pfFakeEntry、pfFakeFS 用来注入读目录的结果，覆盖真实文件系统造不出来的情况（坏字节、反斜杠、IO 错误）。
type pfFakeEntry struct {
	name string
	mode fs.FileMode
}

func (e pfFakeEntry) Name() string               { return e.name }
func (e pfFakeEntry) IsDir() bool                { return e.mode.IsDir() }
func (e pfFakeEntry) Type() fs.FileMode          { return e.mode.Type() }
func (e pfFakeEntry) Info() (fs.FileInfo, error) { return nil, errors.New("不需要") }

func pfFakeReadDir(tree map[string][]fs.DirEntry, errs map[string]error) func(string) ([]fs.DirEntry, error) {
	return func(p string) ([]fs.DirEntry, error) {
		if err, ok := errs[p]; ok {
			return nil, err
		}
		if es, ok := tree[p]; ok {
			return es, nil
		}
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
}

func TestPFDiscoverInjected(t *testing.T) {
	d := func(n string) fs.DirEntry { return pfFakeEntry{n, fs.ModeDir} }
	l := func(n string) fs.DirEntry { return pfFakeEntry{n, fs.ModeSymlink} }
	f := func(n string) fs.DirEntry { return pfFakeEntry{n, 0} }
	tree := map[string][]fs.DirEntry{
		"/r":      {d("2026"), d("坏盘"), f("260403Y0101"), l("260403Z0301-链"), d("并列")},
		"/r/2026": {d("260403J0812-甲"), d("\xff260403Q0901-坏字节"), d("260403N0101\\反斜杠"), d("２６0403J０８１２-全角数字")},
		"/r/并列":   {d("260403T0101-ａ"), d("260403T0101-ｂ")},
	}
	errs := map[string]error{"/r/坏盘": errors.New("input/output error")}
	res, err := pfDiscover("/r", 5, pfFakeReadDir(tree, errs))
	if err != nil {
		t.Fatal(err)
	}
	if got := pfPairs(res.Units); !reflect.DeepEqual(got, []string{
		"260403J0812 @ 2026/260403J0812-甲",
		"260403T0101 @ 并列/260403T0101-ａ",
		"260403T0101 @ 并列/260403T0101-ｂ",
	}) {
		t.Errorf("单元 = %q", got)
	}
	reasons := map[string]string{}
	for _, s := range res.Skipped {
		reasons[s.Rel] = s.Reason
	}
	for rel, sub := range map[string]string{
		"坏盘":                       "input/output error",
		"260403Z0301-链":            "符号链接",
		"2026/\xff260403Q0901-坏字节": "UTF-8",
		"2026/260403N0101\\反斜杠":    "路径不合法",
		"2026/２６0403J０８１２-全角数字":    "非 ASCII",
	} {
		if r, ok := reasons[rel]; !ok || !strings.Contains(r, sub) {
			t.Errorf("Skipped[%q] = %q，应含 %q", rel, r, sub)
		}
	}
	if len(res.Skipped) != 5 || !res.Incomplete {
		t.Errorf("Skipped 共 %d 条: %+v", len(res.Skipped), res.Skipped)
	}
	for _, s := range res.Skipped {
		if s.Rel == "坏盘" != s.Unreadable {
			t.Errorf("Unreadable 标记不对: %+v", s)
		}
	}
	// 同编号两个目录：都标重复
	dups := 0
	for _, u := range res.Units {
		if u.Dup {
			dups++
		}
	}
	if dups != 2 {
		t.Errorf("应有 2 个重复，得 %d", dups)
	}

	// 根读不了
	if _, err := pfDiscover("/r", 5, pfFakeReadDir(nil, map[string]error{"/r": errors.New("boom")})); err == nil {
		t.Error("根读不了应返回 error")
	}
	// 层数：maxDepth=1 时根下认不出编号的目录都到顶
	res, err = pfDiscover("/r", 1, pfFakeReadDir(tree, errs))
	if err != nil || len(res.Units) != 0 || !res.Incomplete {
		t.Errorf("maxDepth=1: %+v %v", res, err)
	}
}

// ---------------------------------------------------------------------------
// 登记表
// ---------------------------------------------------------------------------

var pfTestStart = time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)

func pfUnit(code, name string, state PFUnitState) PFUnitRecord {
	rel := "2026/" + code + "-" + name
	return PFUnitRecord{
		Code:      code,
		Rel:       rel,
		State:     state,
		LinkPath:  "/Users/test/Desktop/Projects/" + rel,
		SyncPath:  "/Users/test/XXYI-sync/" + rel,
		StartedAt: pfTestStart,
	}
}

func pfSampleUnits() []PFUnitRecord {
	stopped := pfUnit("CCICXMST202605022", "乙项目", PFUnitStopped)
	stopped.LinkPath, stopped.SyncPath = "", ""
	return []PFUnitRecord{
		pfUnit("260403J0812", "甲项目", PFUnitSyncing),
		stopped,
		pfUnit("TMP2026090201", "临时", PFUnitStarting),
		pfUnit("260403Q0901", "丙", PFUnitStopping),
	}
}

func pfUnitsEqual(a, b []PFUnitRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Code != y.Code || x.Rel != y.Rel || x.State != y.State || x.LinkPath != y.LinkPath ||
			x.SyncPath != y.SyncPath || !x.StartedAt.Equal(y.StartedAt) {
			return false
		}
	}
	return true
}

func TestPFUnitsRoundTrip(t *testing.T) {
	in := pfSampleUnits()
	data, err := PFMarshalUnits(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.HasPrefix(s, "[\n  {") || !strings.HasSuffix(s, "}\n]\n") {
		t.Errorf("格式不对:\n%s", s)
	}
	for _, k := range []string{`"code"`, `"rel"`, `"state": "syncing"`, `"link_path"`, `"sync_path"`, `"started_at": "2026-10-26T09:30:00Z"`} {
		if !strings.Contains(s, k) {
			t.Errorf("缺 %s:\n%s", k, s)
		}
	}
	out, err := PFParseUnits(data)
	if err != nil {
		t.Fatalf("往返解析失败: %v\n%s", err, s)
	}
	if !pfUnitsEqual(in, out) {
		t.Errorf("往返不一致:\n in %+v\nout %+v", in, out)
	}
	// 再序列化一次，字节不变
	data2, err := PFMarshalUnits(out)
	if err != nil || string(data2) != s {
		t.Errorf("二次序列化不稳定: %v", err)
	}

	// 非 UTC 的时间也能往返（Equal 比较瞬间）
	u := pfUnit("260403J0812", "甲", PFUnitSyncing)
	u.StartedAt = time.Date(2026, 10, 26, 17, 30, 0, 123456789, time.FixedZone("CST", 8*3600))
	data, err = PFMarshalUnits([]PFUnitRecord{u})
	if err != nil {
		t.Fatal(err)
	}
	back, err := PFParseUnits(data)
	if err != nil || !pfUnitsEqual([]PFUnitRecord{u}, back) {
		t.Errorf("带时区往返: %v %+v", err, back)
	}

	// 空表
	for _, in := range [][]PFUnitRecord{nil, {}} {
		data, err := PFMarshalUnits(in)
		if err != nil || string(data) != "[]\n" {
			t.Errorf("空表序列化: %q %v", data, err)
		}
	}
	out, err = PFParseUnits([]byte("[]\n"))
	if err != nil || out == nil || len(out) != 0 {
		t.Errorf("空数组解析: %v %v", out, err)
	}
	// 路径里的 & < > 不转义成 \u0026
	u = pfUnit("260403J0812", "甲&乙", PFUnitSyncing)
	data, _ = PFMarshalUnits([]PFUnitRecord{u})
	if strings.Contains(string(data), `\u0026`) || !strings.Contains(string(data), "甲&乙") {
		t.Errorf("不该做 HTML 转义: %s", data)
	}
}

func TestPFValidateUnitRejects(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(u *PFUnitRecord)
		expect string
	}{
		{"编号为空", func(u *PFUnitRecord) { u.Code = "" }, "缺少编号"},
		{"编号小写", func(u *PFUnitRecord) { u.Code = "260403j0812" }, "归一化"},
		{"编号带空格", func(u *PFUnitRecord) { u.Code = "260403 J0812" }, "归一化"},
		{"编号全角数字", func(u *PFUnitRecord) { u.Code = "２６0403J０８１２"; u.Rel = "2026/２６0403J０８１２-甲" }, "非 ASCII"},
		{"编号认不出", func(u *PFUnitRecord) { u.Code = "ABC123" }, "认不出"},
		{"编号与 rel 对不上", func(u *PFUnitRecord) { u.Code = "260403J0813" }, "不一致"},
		{"rel 最后一段认不出", func(u *PFUnitRecord) {
			u.Rel = "2026/甲项目"
			u.LinkPath, u.SyncPath = "/a/2026/甲项目", "/b/2026/甲项目"
		}, "不一致"},
		{"状态为空", func(u *PFUnitRecord) { u.State = "" }, "状态"},
		{"状态不认识", func(u *PFUnitRecord) { u.State = "paused" }, "状态"},
		{"状态大小写不对", func(u *PFUnitRecord) { u.State = "SYNCING" }, "状态"},
		{"rel 为空", func(u *PFUnitRecord) { u.Rel = "" }, "rel"},
		{"rel 绝对路径", func(u *PFUnitRecord) { u.Rel = "/2026/260403J0812-甲项目" }, "rel"},
		{"rel 含 ..", func(u *PFUnitRecord) { u.Rel = "2026/../260403J0812-甲项目" }, "rel"},
		{"rel 含空段", func(u *PFUnitRecord) { u.Rel = "2026//260403J0812-甲项目" }, "rel"},
		{"rel 含反斜杠", func(u *PFUnitRecord) { u.Rel = `2026\260403J0812-甲项目` }, "rel"},
		{"rel 含 .", func(u *PFUnitRecord) { u.Rel = "./260403J0812-甲项目" }, "rel"},
		{"rel 超过 5 层", func(u *PFUnitRecord) {
			u.Rel = "a/b/c/d/e/260403J0812-甲"
			u.LinkPath, u.SyncPath = "/a/"+u.Rel, "/b/"+u.Rel
		}, "超过"},
		{"rel 有点开头的段", func(u *PFUnitRecord) {
			u.Rel = ".隐藏/260403J0812-甲"
			u.LinkPath, u.SyncPath = "/a/"+u.Rel, "/b/"+u.Rel
		}, "「.」开头"},
		{"rel 上级目录名里已有编号", func(u *PFUnitRecord) {
			u.Rel = "260403Q0901-外层/260403J0812-甲"
			u.LinkPath, u.SyncPath = "/a/"+u.Rel, "/b/"+u.Rel
		}, "上级目录"},
		{"link_path 为空", func(u *PFUnitRecord) { u.LinkPath = "" }, "link_path"},
		{"sync_path 为空", func(u *PFUnitRecord) { u.SyncPath = "" }, "sync_path"},
		{"link_path 相对路径", func(u *PFUnitRecord) { u.LinkPath = "Desktop/Projects/" + u.Rel }, "绝对路径"},
		{"sync_path 相对路径", func(u *PFUnitRecord) { u.SyncPath = "XXYI-sync/" + u.Rel }, "绝对路径"},
		{"link_path 含 ..", func(u *PFUnitRecord) { u.LinkPath = "/Users/test/../test/Desktop/Projects/" + u.Rel }, "干净"},
		{"sync_path 末尾斜杠", func(u *PFUnitRecord) { u.SyncPath += "/" }, "干净"},
		{"link_path 含 NUL", func(u *PFUnitRecord) { u.LinkPath += "\x00" }, "不是合法路径"},
		{"link_path 没以 rel 结尾", func(u *PFUnitRecord) { u.LinkPath = "/Users/test/Desktop/Projects/2025/260403J0812-甲项目" }, "结尾"},
		{"sync_path 没以 rel 结尾", func(u *PFUnitRecord) { u.SyncPath = "/Users/test/XXYI-sync" }, "结尾"},
		{"两个路径相同", func(u *PFUnitRecord) { u.SyncPath = u.LinkPath }, "相同"},
		{"started_at 缺失", func(u *PFUnitRecord) { u.StartedAt = time.Time{} }, "started_at"},
		{"stopped 但路径写了相对路径", func(u *PFUnitRecord) { u.State = PFUnitStopped; u.LinkPath = "Desktop/x" }, "绝对路径"},
	}
	for _, c := range cases {
		u := pfUnit("260403J0812", "甲项目", PFUnitSyncing)
		c.mut(&u)
		err := PFValidateUnit(u)
		if err == nil {
			t.Errorf("%s: 应被拒绝: %+v", c.name, u)
			continue
		}
		if !strings.Contains(err.Error(), c.expect) {
			t.Errorf("%s: 错误信息应含 %q，得 %v", c.name, c.expect, err)
		}
		// 整张表、序列化也一样拒绝
		if _, err := PFMarshalUnits([]PFUnitRecord{u}); err == nil {
			t.Errorf("%s: 坏数据不该能序列化", c.name)
		}
	}

	// 合格的几种
	for _, u := range pfSampleUnits() {
		if err := PFValidateUnit(u); err != nil {
			t.Errorf("%+v 应合格: %v", u, err)
		}
	}
	// 新、旧、临时编号，rel 深度 1 与 5
	for _, rel := range []string{"260403J0812-甲", "a/b/c/d/260403J0812-甲", "2026/CCIC XMST 202605052 空格", "2026/TMP2026090201"} {
		code, _ := PFCodeFromName(rel[strings.LastIndex(rel, "/")+1:])
		u := PFUnitRecord{Code: code, Rel: rel, State: PFUnitSyncing, LinkPath: "/a/" + rel, SyncPath: "/b/" + rel, StartedAt: pfTestStart}
		if err := PFValidateUnit(u); err != nil {
			t.Errorf("rel %q 应合格: %v", rel, err)
		}
	}
}

func TestPFValidateUnitsRejects(t *testing.T) {
	a := pfUnit("260403J0812", "甲项目", PFUnitSyncing)
	// 同编号两次（rel 不同）
	b := pfUnit("260403J0812", "别名", PFUnitStopped)
	if err := PFValidateUnits([]PFUnitRecord{a, b}); err == nil || !strings.Contains(err.Error(), "两次") {
		t.Errorf("同编号两条应被拒绝: %v", err)
	}
	// 同编号两次（完全相同）
	if err := PFValidateUnits([]PFUnitRecord{a, a}); err == nil || !strings.Contains(err.Error(), "两次") {
		t.Errorf("同一条重复应被拒绝: %v", err)
	}
	// 其中一条坏：报第几条
	bad := pfUnit("260403Q0901", "乙", "oops")
	err := PFValidateUnits([]PFUnitRecord{a, bad})
	if err == nil || !strings.Contains(err.Error(), "第 2 条") {
		t.Errorf("应指出第 2 条: %v", err)
	}
	// 单元套单元：里面那个的上级目录名认得出编号，被单条校验拦住
	inner := pfUnit("260403Q0901", "里面", PFUnitSyncing)
	inner.Rel = a.Rel + "/" + "260403Q0901-里面"
	inner.LinkPath, inner.SyncPath = "/x/"+inner.Rel, "/y/"+inner.Rel
	if err := PFValidateUnits([]PFUnitRecord{a, inner}); err == nil {
		t.Error("单元不能套在单元里")
	}
	if err := PFValidateUnits([]PFUnitRecord{a, pfUnit("260403Q0901", "乙", PFUnitSyncing)}); err != nil {
		t.Errorf("两个不同编号应合格: %v", err)
	}
}

func TestPFParseUnitsRejects(t *testing.T) {
	good := `{"code":"260403J0812","rel":"2026/260403J0812-甲","state":"syncing","link_path":"/a/2026/260403J0812-甲","sync_path":"/b/2026/260403J0812-甲","started_at":"2026-10-26T09:30:00Z"}`
	if us, err := PFParseUnits([]byte("[" + good + "]")); err != nil || len(us) != 1 {
		t.Fatalf("基准应能解析: %v", err)
	}
	cases := []struct{ name, in, expect string }{
		{"空内容", "", "空的"},
		{"只有空白", " \n\t ", "空的"},
		{"null", "null", "数组"},
		{"对象", "{}", "数组"},
		{"字符串", `"x"`, "数组"},
		{"数组里是 null", "[null]", "缺少编号"},
		{"数组里是数字", "[1]", "不合法"},
		{"JSON 没写完", "[{", "不合法"},
		{"不认识的字段", "[" + strings.Replace(good, `"code"`, `"extra":1,"code"`, 1) + "]", "extra"},
		{"结尾多余内容", "[" + good + "] []", "多余"},
		{"结尾多余垃圾", "[" + good + "] x", "多余"},
		{"state 写成数字", "[" + strings.Replace(good, `"syncing"`, "1", 1) + "]", "不合法"},
		{"state 不认识", "[" + strings.Replace(good, `"syncing"`, `"paused"`, 1) + "]", "状态"},
		{"started_at 格式不对", "[" + strings.Replace(good, "2026-10-26T09:30:00Z", "昨天", 1) + "]", "不合法"},
		{"缺 started_at", "[" + strings.Replace(good, `,"started_at":"2026-10-26T09:30:00Z"`, "", 1) + "]", "started_at"},
		{"缺 sync_path", "[" + strings.Replace(good, `"sync_path":"/b/2026/260403J0812-甲",`, "", 1) + "]", "sync_path"},
		{"编号重复", "[" + good + "," + good + "]", "两次"},
		{"link_path 相对路径", "[" + strings.Replace(good, `"/a/2026`, `"a/2026`, 1) + "]", "绝对路径"},
	}
	for _, c := range cases {
		us, err := PFParseUnits([]byte(c.in))
		if err == nil {
			t.Errorf("%s: 应被拒绝，得 %+v", c.name, us)
			continue
		}
		if us != nil {
			t.Errorf("%s: 出错时不该返回部分数据: %+v", c.name, us)
		}
		if !strings.Contains(err.Error(), c.expect) {
			t.Errorf("%s: 错误信息应含 %q，得 %v", c.name, c.expect, err)
		}
	}
}

func TestPFFindAndUpsertUnit(t *testing.T) {
	us := pfSampleUnits()
	if u, ok := PFFindUnit(us, "ccicxmst 202605022"); !ok || u.State != PFUnitStopped {
		t.Errorf("按归一化编号找: %+v %v", u, ok)
	}
	if _, ok := PFFindUnit(us, "260403J9999"); ok {
		t.Error("不存在的编号不该找到")
	}
	// 追加
	n := pfUnit("260403N0101", "新", PFUnitStarting)
	out, err := PFUpsertUnit(us, n)
	if err != nil || len(out) != len(us)+1 || out[len(out)-1].Code != "260403N0101" {
		t.Fatalf("追加: %v %+v", err, out)
	}
	if len(us) != 4 {
		t.Error("不该改原表")
	}
	// 替换
	r := us[0]
	r.State = PFUnitStopping
	out, err = PFUpsertUnit(us, r)
	if err != nil || len(out) != len(us) || out[0].State != PFUnitStopping || us[0].State != PFUnitSyncing {
		t.Fatalf("替换: %v %+v", err, out)
	}
	// 不合格的不接受
	r.Rel = "不对"
	if out, err = PFUpsertUnit(us, r); err == nil || out != nil {
		t.Errorf("不合格的应拒绝: %v %v", out, err)
	}
}

// ---------------------------------------------------------------------------
// 新单元判定
// ---------------------------------------------------------------------------

func pfFound(code, rel string) PFFoundUnit { return PFFoundUnit{Code: code, Rel: rel} }

func TestPFClassifyUnits(t *testing.T) {
	syncing := pfUnit("260403J0812", "甲", PFUnitSyncing)
	stopped := pfUnit("260403Q0901", "乙", PFUnitStopped)
	starting := pfUnit("260403P0101", "丙", PFUnitStarting)
	stopping := pfUnit("260403Y0101", "丁", PFUnitStopping)
	dupReg := pfUnit("260403N0101", "戊", PFUnitSyncing)
	missing := pfUnit("260403Z0101", "己", PFUnitSyncing)
	stoppedGone := pfUnit("260403W0101", "庚", PFUnitStopped)
	startingGone := pfUnit("260403C0101", "辛", PFUnitStarting)
	reg := []PFUnitRecord{syncing, stopped, starting, stopping, dupReg, missing, stoppedGone, startingGone}

	found := PFDiscoverResult{Units: []PFFoundUnit{
		pfFound("260403Y0101", "2026/260403Y0101-丁"),
		pfFound("260403J0812", "2026/260403J0812-甲"),
		pfFound("260403Q0901", "2026/260403Q0901-乙"),
		pfFound("260403P0101", "2026/260403P0101-丙"),
		pfFound("260403T0101", "2026/260403T0101-新项目二"),
		pfFound("260403T0100", "2026/260403T0100-新项目一"),
		pfFound("260403N0101", "2026/260403N0101-戊"),    // 已登记且重复
		pfFound("260403N0101", "2025/260403N0101-戊的副本"), // 已登记且重复
		pfFound("260403X0101", "2026/260403X0101-新重复a"), // 未登记且重复
		pfFound("260403X0101", "2026/260403X0101-新重复b"),
	}}
	got, err := PFClassifyUnits(found, reg)
	if err != nil {
		t.Fatal(err)
	}
	codesOf := func(ms []PFUnitMatch) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Reg.Code+"="+m.Found.Rel)
		}
		return out
	}
	// §14.4：按 rel 配对——戊登记的 rel 找得到就照常 Syncing，同编号的副本目录只报告（Unconfigured），不再连坐。
	if g := codesOf(got.Syncing); !reflect.DeepEqual(g, []string{"260403J0812=2026/260403J0812-甲", "260403N0101=2026/260403N0101-戊"}) {
		t.Errorf("Syncing = %q", g)
	}
	if g := pfPairs(got.Unconfigured); !reflect.DeepEqual(g, []string{"260403N0101 @ 2025/260403N0101-戊的副本"}) {
		t.Errorf("Unconfigured = %q", g)
	}
	if g := codesOf(got.Stopped); !reflect.DeepEqual(g, []string{"260403Q0901=2026/260403Q0901-乙"}) {
		t.Errorf("Stopped = %q", g)
	}
	if g := codesOf(got.Transitional); !reflect.DeepEqual(g, []string{"260403P0101=2026/260403P0101-丙", "260403Y0101=2026/260403Y0101-丁"}) {
		t.Errorf("Transitional = %q", g)
	}
	if g := pfPairs(got.New); !reflect.DeepEqual(g, []string{
		"260403T0100 @ 2026/260403T0100-新项目一",
		"260403T0101 @ 2026/260403T0101-新项目二",
	}) {
		t.Errorf("New = %q", g)
	}
	// Duplicates 只剩「编号完全没登记过、同编号多个目录」的（X0101）；已登记编号的多目录不再 Dup（§14.2）
	if g := pfPairs(got.Duplicates); !reflect.DeepEqual(g, []string{
		"260403X0101 @ 2026/260403X0101-新重复a",
		"260403X0101 @ 2026/260403X0101-新重复b",
	}) {
		t.Errorf("Duplicates = %q", g)
	}
	for _, d := range got.Duplicates {
		if !d.Dup || len(d.DupRels) != 1 {
			t.Errorf("重复项应带 Dup 和对方路径: %+v", d)
		}
	}
	// 登记 syncing 但没发现的才进 Missing；已停止、开始中没发现的不进；重复的（找到了）不进
	if len(got.Missing) != 1 || got.Missing[0].Code != "260403Z0101" {
		t.Errorf("Missing = %+v", got.Missing)
	}
	if got.Incomplete {
		t.Error("发现完整时 Incomplete 应为假")
	}

	// Incomplete 透传
	found.Incomplete = true
	got, _ = PFClassifyUnits(found, reg)
	if !got.Incomplete {
		t.Error("Incomplete 应透传")
	}

	// 空登记表：全是新项目
	got, err = PFClassifyUnits(PFDiscoverResult{Units: []PFFoundUnit{pfFound("260403T0100", "2026/260403T0100-新")}}, nil)
	if err != nil || len(got.New) != 1 || len(got.Syncing)+len(got.Stopped)+len(got.Missing)+len(got.Duplicates) != 0 {
		t.Errorf("空登记表: %+v %v", got, err)
	}
	// 什么都没发现：登记 syncing 的全进 Missing
	got, err = PFClassifyUnits(PFDiscoverResult{}, reg)
	// 登记里 syncing 的有 3 个（甲、戊、己）；stopped、starting、stopping 的不进 Missing
	if err != nil || len(got.Missing) != 3 || got.Missing[0].Code != "260403J0812" ||
		got.Missing[1].Code != "260403N0101" || got.Missing[2].Code != "260403Z0101" {
		t.Errorf("发现为空: %+v %v", got, err)
	}
	// 登记表不合格：报错
	if _, err := PFClassifyUnits(found, []PFUnitRecord{syncing, syncing}); err == nil {
		t.Error("登记表有重复应报错")
	}
}

func TestPFClassifyIgnoresDupFlag(t *testing.T) {
	// §14.4：配对按 rel 重算，不信 Dup 标——两个 found 的 rel 都不是登记的那条：
	// 编号已登记 → 都进 Unconfigured（只报告）；登记的 rel 没找到 → Missing。
	found := PFDiscoverResult{Units: []PFFoundUnit{pfFound("260403J0812", "a/260403J0812-1"), pfFound("260403J0812", "b/260403J0812-2")}}
	got, err := PFClassifyUnits(found, []PFUnitRecord{pfUnit("260403J0812", "甲", PFUnitSyncing)})
	if err != nil || len(got.Duplicates) != 0 || len(got.Syncing) != 0 ||
		len(got.Unconfigured) != 2 || len(got.Missing) != 1 || got.Missing[0].Code != "260403J0812" {
		t.Errorf("%+v %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// Mac 工作目录里是否已有同编号目录
// ---------------------------------------------------------------------------

func TestPFCheckWorkDirForUnit(t *testing.T) {
	found := pfFound("260403J0812", "2026/260403J0812-甲项目")

	t.Run("空位", func(t *testing.T) {
		work := t.TempDir()
		pfMkdirs(t, work, "2026/260403Q0901-别的项目", "2025/无编号")
		got, err := PFCheckWorkDirForUnit(work, found, PFDirOps{})
		if err != nil || got.Slot != PFSlotFree || got.Rel != "" {
			t.Errorf("%+v %v", got, err)
		}
	})
	t.Run("同名真实目录", func(t *testing.T) {
		work := t.TempDir()
		pfMkdirs(t, work, "2026/260403J0812-甲项目")
		got, err := PFCheckWorkDirForUnit(work, found, PFDirOps{})
		if err != nil || got.Slot != PFSlotRealDir || got.Rel != found.Rel {
			t.Errorf("%+v %v", got, err)
		}
	})
	t.Run("同编号不同名字", func(t *testing.T) {
		work := t.TempDir()
		pfMkdirs(t, work, "2026/260403J0812-桌面上的另一个名字")
		got, err := PFCheckWorkDirForUnit(work, found, PFDirOps{})
		if err != nil || got.Slot != PFSlotRealDir || got.Rel != "2026/260403J0812-桌面上的另一个名字" {
			t.Errorf("%+v %v", got, err)
		}
	})
	t.Run("同编号在别的年份目录", func(t *testing.T) {
		work := t.TempDir()
		pfMkdirs(t, work, "2025/260403j0812-小写")
		got, err := PFCheckWorkDirForUnit(work, found, PFDirOps{})
		if err != nil || got.Slot != PFSlotRealDir || got.Rel != "2025/260403j0812-小写" {
			t.Errorf("%+v %v", got, err)
		}
	})
	t.Run("同名位置是符号链接", func(t *testing.T) {
		work := t.TempDir()
		pfMkdirs(t, work, "2026")
		if err := os.Symlink(t.TempDir(), filepath.Join(work, "2026", "260403J0812-甲项目")); err != nil {
			t.Skipf("建不了符号链接: %v", err)
		}
		got, err := PFCheckWorkDirForUnit(work, found, PFDirOps{})
		if err != nil || got.Slot != PFSlotOccupied || got.Rel != found.Rel {
			t.Errorf("%+v %v", got, err)
		}
	})
	t.Run("同名位置是文件", func(t *testing.T) {
		work := t.TempDir()
		pfMkdirs(t, work, "2026")
		pfWriteFile(t, filepath.Join(work, "2026", "260403J0812-甲项目"))
		got, err := PFCheckWorkDirForUnit(work, found, PFDirOps{})
		if err != nil || got.Slot != PFSlotOccupied {
			t.Errorf("%+v %v", got, err)
		}
	})
	t.Run("同编号不同名字的符号链接", func(t *testing.T) {
		work := t.TempDir()
		pfMkdirs(t, work, "2026")
		if err := os.Symlink(t.TempDir(), filepath.Join(work, "2026", "260403J0812-链接名")); err != nil {
			t.Skipf("建不了符号链接: %v", err)
		}
		got, err := PFCheckWorkDirForUnit(work, found, PFDirOps{})
		if err != nil || got.Slot != PFSlotOccupied || got.Rel != "2026/260403J0812-链接名" {
			t.Errorf("%+v %v", got, err)
		}
		// 别的编号的链接不影响
		if err := os.Symlink(t.TempDir(), filepath.Join(work, "2026", "260403Q0901-别人的链接")); err != nil {
			t.Fatal(err)
		}
		other := pfFound("260403N0101", "2026/260403N0101-丙")
		got, err = PFCheckWorkDirForUnit(work, other, PFDirOps{})
		if err != nil || got.Slot != PFSlotFree {
			t.Errorf("别的编号: %+v %v", got, err)
		}
	})
	t.Run("同编号既有真实目录又有链接", func(t *testing.T) {
		work := t.TempDir()
		pfMkdirs(t, work, "2026/260403J0812-真")
		if err := os.Symlink(t.TempDir(), filepath.Join(work, "2026", "260403J0812-链")); err != nil {
			t.Skipf("建不了符号链接: %v", err)
		}
		got, err := PFCheckWorkDirForUnit(work, found, PFDirOps{})
		if err != nil || got.Slot != PFSlotRealDir || got.Rel != "2026/260403J0812-真" {
			t.Errorf("真实目录优先: %+v %v", got, err)
		}
	})
	t.Run("工作目录不存在", func(t *testing.T) {
		if _, err := PFCheckWorkDirForUnit(filepath.Join(t.TempDir(), "没有"), found, PFDirOps{}); err == nil {
			t.Error("工作目录不存在应报错，不能当成空位")
		}
	})
	t.Run("工作目录是相对路径", func(t *testing.T) {
		if _, err := PFCheckWorkDirForUnit("Projects", found, PFDirOps{}); err == nil {
			t.Error("相对路径应报错")
		}
	})
	t.Run("注入：子目录读不了", func(t *testing.T) {
		d := func(n string) fs.DirEntry { return pfFakeEntry{n, fs.ModeDir} }
		tree := map[string][]fs.DirEntry{"/w": {d("2026"), d("2025")}, "/w/2025": {}}
		ops := PFDirOps{
			ReadDir: pfFakeReadDir(tree, map[string]error{"/w/2026": &fs.PathError{Op: "open", Path: "/w/2026", Err: fs.ErrPermission}}),
			Lstat:   func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist },
		}
		if got, err := PFCheckWorkDirForUnit("/w", found, ops); err == nil {
			t.Errorf("读不了的子目录里可能有同编号目录，应报错: %+v", got)
		}
	})
	t.Run("注入：Lstat 报权限错误", func(t *testing.T) {
		d := func(n string) fs.DirEntry { return pfFakeEntry{n, fs.ModeDir} }
		ops := PFDirOps{
			ReadDir: pfFakeReadDir(map[string][]fs.DirEntry{"/w": {d("2026")}, "/w/2026": {}}, nil),
			Lstat: func(p string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrPermission}
			},
		}
		if got, err := PFCheckWorkDirForUnit("/w", found, ops); err == nil {
			t.Errorf("Lstat 出错应报错而不是当成空位: %+v", got)
		}
	})
	t.Run("注入：Lstat 报不存在", func(t *testing.T) {
		d := func(n string) fs.DirEntry { return pfFakeEntry{n, fs.ModeDir} }
		ops := PFDirOps{
			ReadDir: pfFakeReadDir(map[string][]fs.DirEntry{"/w": {d("2026")}, "/w/2026": {}}, nil),
			Lstat: func(p string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
			},
		}
		if got, err := PFCheckWorkDirForUnit("/w", found, ops); err != nil || got.Slot != PFSlotFree {
			t.Errorf("%+v %v", got, err)
		}
	})
}

func TestPFNotesNotEmpty(t *testing.T) {
	for _, s := range []string{PFNoteStopped, PFNoteNewOnUbuntu, PFNoteDuplicate, PFNoteStartOnMacFirst, PFNoteSlotOccupied} {
		if s == "" {
			t.Fatal("状态说法不能为空")
		}
	}
}
