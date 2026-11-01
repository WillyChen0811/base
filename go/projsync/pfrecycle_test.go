package projsync

// 同步回收（pfrecycle.go）的测试：命名、index、路径结构冲突、清理天数、统计、不同卷。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// pfrPlace 把 e 里 rel 文件放进回收区并记账，返回记录。
func pfrPlace(t *testing.T, e *pfaEnv, rel, content string) PFRecycleEntry {
	t.Helper()
	e.put(rel, content)
	res, err := e.a.Delete(bg, rel, e.expect(rel), PFReasonDelete)
	e.wantDone(res, err)
	return *res.Recycled
}

func TestPFRecycleLayoutAndIndex(t *testing.T) {
	e := newPFAEnv(t)
	r := pfrPlace(t, e, "资料/报告.docx", "内容一")
	if r.Stored != "2026-10-26/"+e.unit+"/资料/报告.docx" {
		t.Errorf("Stored = %q", r.Stored)
	}
	if r.Time != e.now.Format(time.RFC3339) {
		t.Errorf("Time = %q", r.Time)
	}
	// index.jsonl 每行一个 JSON 对象，字段齐全
	raw, err := os.ReadFile(filepath.Join(e.recDir, "2026-10-26", "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &m); err != nil {
		t.Fatalf("index 不是 JSON：%v\n%s", err, raw)
	}
	for _, k := range []string{"unit", "path", "sha256", "size", "mtime_ns", "reason", "round", "time", "stored"} {
		if _, ok := m[k]; !ok {
			t.Errorf("index 缺字段 %s：%s", k, raw)
		}
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("index 每行要以换行结尾")
	}
}

func TestPFRecycleNameCollisions(t *testing.T) {
	e := newPFAEnv(t)
	a := pfrPlace(t, e, "x/r.docx", "A")
	b := pfrPlace(t, e, "x/r.docx", "B")
	c := pfrPlace(t, e, "x/r.docx", "C")
	if a.Stored == b.Stored || b.Stored == c.Stored || a.Stored == c.Stored {
		t.Fatalf("重名不该互相覆盖：%q %q %q", a.Stored, b.Stored, c.Stored)
	}
	for _, r := range []PFRecycleEntry{b, c} {
		base := filepath.Base(r.Stored)
		if !strings.HasPrefix(base, "r~153000-") || !strings.HasSuffix(base, ".docx") || len(base) != len("r~153000-abcd.docx") {
			t.Errorf("重名后缀格式 %q", base)
		}
	}
	e.holds("A", "B", "C")
	// 无扩展名的文件
	pfrPlace(t, e, "README", "1")
	n2 := pfrPlace(t, e, "README", "2")
	if base := filepath.Base(n2.Stored); !strings.HasPrefix(base, "README~153000-") || strings.Contains(base, ".") {
		t.Errorf("无扩展名的重名 %q", base)
	}
}

func TestPFRecycleCaseInsensitiveCollision(t *testing.T) {
	// 回收区里已有 Report.docx，再放 report.docx：在不区分大小写的盘上算同名，换名而不是覆盖
	e := newPFAEnv(t)
	a := pfrPlace(t, e, "Report.docx", "大写")
	b := pfrPlace(t, e, "report.docx", "小写")
	if a.Stored == b.Stored {
		t.Fatal("不该覆盖")
	}
	e.holds("大写", "小写")
}

func TestPFRecycleLongNameKeepsWithin255Bytes(t *testing.T) {
	e := newPFAEnv(t)
	name := strings.Repeat("长", 83) + ".docx" // 249+5 = 254 字节
	if len(name) > 255 {
		t.Fatalf("夹具名字 %d 字节", len(name))
	}
	pfrPlace(t, e, name, "1")
	r := pfrPlace(t, e, name, "2")
	base := filepath.Base(r.Stored)
	if len(base) > 255 || !utf8.ValidString(base) || !strings.HasSuffix(base, ".docx") || !strings.Contains(base, "~153000-") {
		t.Errorf("加后缀后的名字 %d 字节：%q", len(base), base)
	}
	e.holds("1", "2")
}

func TestPFRecyclePathShapeConflict(t *testing.T) {
	e := newPFAEnv(t)
	// 先回收一个叫 a 的文件，再回收 a/b（回收区里 a 是文件，建不了目录 a）
	pfrPlace(t, e, "a", "文件a")
	r := pfrPlace(t, e, "a/b.docx", "a下的b")
	if !strings.Contains(r.Stored, "_散放") || r.Path != "a/b.docx" {
		t.Errorf("路径结构冲突应放进散放目录并在 index 里记原路径：%+v", r)
	}
	e.holds("文件a", "a下的b")
	// 反过来：先回收目录下的文件，再回收同名文件（回收区里 d 是目录）
	pfrPlace(t, e, "d/x.docx", "d下的x")
	os.Remove(e.abs("d")) // 项目里的 d 目录已空，删掉才能再放一个叫 d 的文件
	r2 := pfrPlace(t, e, "d", "文件d")
	if filepath.Base(r2.Stored) == "d" {
		t.Errorf("d 已是目录，文件应换名：%q", r2.Stored)
	}
	e.holds("d下的x", "文件d")
	idx, _ := e.rec.ReadIndex("2026-10-26")
	if len(idx) != 4 {
		t.Errorf("index 应有 4 条：%+v", idx)
	}
}

func TestPFRecycleIndexSkipsPartialLine(t *testing.T) {
	e := newPFAEnv(t)
	pfrPlace(t, e, "a.docx", "A")
	p := filepath.Join(e.recDir, "2026-10-26", "index.jsonl")
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"unit":"u","path":"写了一半`) // 崩溃时最后一行没写完
	f.Close()
	idx, err := e.rec.ReadIndex("2026-10-26")
	if err != nil || len(idx) != 1 {
		t.Fatalf("应只读到完整的一条：%+v %v", idx, err)
	}
	if idx, err := e.rec.ReadIndex("2026-01-01"); err != nil || len(idx) != 0 {
		t.Errorf("没有的日期应返回空：%v %v", idx, err)
	}
	if _, err := e.rec.ReadIndex("../x"); err == nil {
		t.Error("日期格式不对应报错")
	}
}

func TestPFRecycleCleanDays(t *testing.T) {
	e := newPFAEnv(t)
	// 今天 2026-10-26；造一批日期目录，每个放一个文件
	mk := func(day, content string) {
		d := filepath.Join(e.recDir, day, "u")
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "f.txt"), []byte(content), 0o644)
		os.WriteFile(filepath.Join(e.recDir, day, "index.jsonl"), []byte("{}\n"), 0o600)
	}
	days := []string{"2026-09-01", "2026-09-26", "2026-09-27", "2026-10-18", "2026-10-19", "2026-10-20", "2026-10-26", "2026-10-27"}
	for _, d := range days {
		mk(d, "内容"+d)
	}
	os.WriteFile(filepath.Join(e.recDir, "notes.txt"), []byte("不是日期目录"), 0o644)
	os.MkdirAll(filepath.Join(e.recDir, "2026-1-1"), 0o755)
	os.MkdirAll(filepath.Join(e.recDir, "备份"), 0o755)
	outside := filepath.Join(e.base, "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "keep"), []byte("k"), 0o644)
	os.Symlink(outside, filepath.Join(e.recDir, "2020-01-01")) // 名字像日期的符号链接

	// 保留 30 天：今天往前推 30 天 = 09-26，早于 09-26 的删（09-01）
	res, err := e.rec.Clean(30)
	if err != nil {
		t.Fatal(err)
	}
	if res.Days != 30 || len(res.Removed) != 1 || res.Removed[0] != "2026-09-01" || res.Files != 1 {
		t.Errorf("保留 30 天：%+v", res)
	}
	for _, d := range days[1:] {
		if _, err := os.Stat(filepath.Join(e.recDir, d)); err != nil {
			t.Errorf("%s 不该被删", d)
		}
	}
	// 保留 3 天（小于下限）按 7 天算：09-26 之前删完后，10-19 之前的（早于 10-19）都删：09-26、09-27、10-18
	res, err = e.rec.Clean(3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Days != PFRecycleMinDays || strings.Join(res.Removed, ",") != "2026-09-26,2026-09-27,2026-10-18" {
		t.Errorf("保留 3 天应按 7 天：%+v", res)
	}
	for _, d := range []string{"2026-10-19", "2026-10-20", "2026-10-26", "2026-10-27"} {
		if _, err := os.Stat(filepath.Join(e.recDir, d)); err != nil {
			t.Errorf("%s 不该被删", d)
		}
	}
	// 非日期名字、符号链接及其指向的内容都没动
	for _, p := range []string{"notes.txt", "2026-1-1", "备份", "2020-01-01"} {
		if _, err := os.Lstat(filepath.Join(e.recDir, p)); err != nil {
			t.Errorf("%s 不该被清理动到", p)
		}
	}
	if b, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(b) != "k" {
		t.Error("符号链接指向的外部内容被动了")
	}
	// 不存在的目录不算错
	nr, _ := NewPFRecycle(filepath.Join(e.base, "新回收"), PFRecycleOptions{})
	os.RemoveAll(nr.Dir())
	if _, err := nr.Clean(30); err != nil {
		t.Errorf("目录不存在不算错：%v", err)
	}
}

func TestPFRecycleCleanKeepsFullDays(t *testing.T) {
	// 任何一份东西至少留满 N 天：当天 23:59 放进去的，在第 N 天 0 点前不会被清掉
	e := newPFAEnv(t)
	e.now = time.Date(2026, 10, 1, 23, 59, 0, 0, time.Local)
	pfrPlace(t, e, "late.docx", "晚上放的")
	e.now = time.Date(2026, 10, 8, 23, 59, 0, 0, time.Local) // 7 天后的同一时刻，还不到
	res, _ := e.rec.Clean(7)
	if len(res.Removed) != 0 {
		t.Errorf("还没满 7 天不该清：%+v", res)
	}
	e.now = time.Date(2026, 10, 9, 0, 0, 0, 0, time.Local) // 满 7 天
	res, _ = e.rec.Clean(7)
	if len(res.Removed) != 1 {
		t.Errorf("满 7 天应清：%+v", res)
	}
}

func TestPFRecycleUsage(t *testing.T) {
	e := newPFAEnv(t)
	pfrPlace(t, e, "a.docx", "12345")
	e.now = e.now.AddDate(0, 0, 1)
	pfrPlace(t, e, "b.docx", "1234567")
	u, err := e.rec.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if u.Files != 2 || len(u.Days) != 2 || u.Days[0].Day != "2026-10-26" || u.Days[0].Files != 1 || u.Days[1].Files != 1 {
		t.Errorf("Usage = %+v", u)
	}
	if u.Bytes < 12 || u.Days[0].Bytes <= 5 { // 文件内容 + index
		t.Errorf("字节数 %+v", u)
	}
}

func TestPFRecycleCheckDifferentVolume(t *testing.T) {
	e := newPFAEnv(t)
	if err := e.rec.Check(e.root); err != nil {
		t.Fatalf("同卷不该报错：%v", err)
	}
	r, err := NewPFRecycle(filepath.Join(e.base, "r2"), PFRecycleOptions{Stat: func(p string) (PFStat, error) {
		st, err := PFLstat(p)
		if strings.Contains(p, "r2") {
			st.Dev ^= 0xff
		}
		return st, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Check(e.root)
	var ce *PFApplyConfigError
	if !asConfigErr(err, &ce) || !strings.Contains(err.Error(), "st_dev 不同") {
		t.Errorf("不同卷应报配置错误：%v", err)
	}
	if _, err := NewPFRecycle("相对路径", PFRecycleOptions{}); err == nil {
		t.Error("相对路径应报错")
	}
}

func asConfigErr(err error, target **PFApplyConfigError) bool {
	ce, ok := err.(*PFApplyConfigError)
	if ok {
		*target = ce
	}
	return ok
}

func TestPFRecycleNeverDeletedByPrimitives(t *testing.T) {
	// 连续做覆盖、删除、冲突、放弃，回收区里已有的东西一份都不少
	e := newPFAEnv(t)
	var kept []string
	e.put("a.docx", "v1")
	res, err := e.a.InstallOver(bg, "a.docx", e.stage("v2"), 5, e.expect("a.docx"))
	e.wantDone(res, err)
	kept = append(kept, "v1")
	e.put("b.docx", "b1")
	res, err = e.a.Delete(bg, "b.docx", e.expect("b.docx"), PFReasonConflict)
	e.wantDone(res, err)
	kept = append(kept, "b1")
	// 之后一系列放弃的操作
	e.a.Delete(bg, "nope", PFExpectFile(1, 1, 0, pfaSHA("x")), PFReasonDelete)
	e.a.InstallOver(bg, "a.docx", e.stage("v3"), 6, PFExpectFile(1, 1, 0, pfaSHA("x")))
	e.a.Rmdir(bg, "nodir")
	e.holds(append(kept, "v2")...)
	u, _ := e.rec.Usage()
	if u.Files != 2 {
		t.Errorf("回收区文件数 = %d", u.Files)
	}
}
