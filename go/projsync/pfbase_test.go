package projsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const pfTU = "261001J0101"

// pfTE 造一个文件条目（虚构数据）。
func pfTE(i int) PFBaseEntry {
	sum := sha256.Sum256([]byte(fmt.Sprintf("内容-%d", i)))
	return PFBaseEntry{
		T: PFBaseTypeFile, Size: int64(i) * 10, SHA256: hex.EncodeToString(sum[:]),
		Mac: PFSideStat{MtimeNs: 1700000000000000000 + int64(i), Ino: uint64(1000 + i)},
		Ub:  PFSideStat{MtimeNs: 1700000000500000000 + int64(i), Ino: uint64(9000 + i)},
	}
}

func pfTDir() PFBaseEntry {
	return PFBaseEntry{T: PFBaseTypeDir, Mac: PFSideStat{MtimeNs: 1, Ino: 2}, Ub: PFSideStat{MtimeNs: 3, Ino: 4}}
}

func pfTMeta() PFBaseMeta {
	return PFBaseMeta{
		MacRel: "2026/261001J0101-测试项目甲", UbuntuRel: "2026/261001J0101-测试项目甲",
		MacDirID: PFDirID{Dev: 16777234, Ino: 4242}, UbuntuDirID: PFDirID{Dev: 2049, Ino: 9999},
	}
}

func pfNewBase(t *testing.T) (string, *PFBase) {
	t.Helper()
	meta := t.TempDir()
	b, err := PFBaseCreate(meta, pfTU, pfTMeta())
	if err != nil {
		t.Fatal(err)
	}
	return meta, b
}

func pfMustLoadOK(t *testing.T, meta string) *PFBase {
	t.Helper()
	r, err := PFBaseLoad(meta, pfTU)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != PFBaseOK || r.Base == nil {
		t.Fatalf("应载入成功，实际 %v：%s", r.Status, r.Reason)
	}
	return r.Base
}

func pfSnapP(meta string) string    { return filepath.Join(meta, "base", pfTU+".json") }
func pfJournalP(meta string) string { return filepath.Join(meta, "base", pfTU+".journal") }

func pfBadList(t *testing.T, meta string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(meta, "base", "坏"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func pfWant(t *testing.T, b PFBaseView, want map[string]PFBaseEntry) {
	t.Helper()
	got := b.Entries()
	if !reflect.DeepEqual(got, want) {
		gk, wk := pfKeys(got), pfKeys(want)
		t.Fatalf("条目不一致\n got(%d)=%v\nwant(%d)=%v", len(got), gk, len(want), wk)
	}
	if b.Len() != len(want) {
		t.Fatalf("Len=%d want %d", b.Len(), len(want))
	}
}

func pfKeys(m map[string]PFBaseEntry) []string {
	var k []string
	for p := range m {
		k = append(k, p)
	}
	sort.Strings(k)
	return k
}

func pfReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------- 正常路径 ----------

func TestPFBase_RoundTrip(t *testing.T) {
	meta, b := pfNewBase(t)
	if b.Gen() != 1 || b.Len() != 0 {
		t.Fatalf("新 base 应是 gen=1、空，实际 gen=%d len=%d", b.Gen(), b.Len())
	}
	want := map[string]PFBaseEntry{}
	set := func(p string, e PFBaseEntry) {
		t.Helper()
		if err := b.Set(p, e); err != nil {
			t.Fatal(err)
		}
		want[p] = e
	}
	set("01 来函/函件.docx", pfTE(1))
	set("01 来函", pfTDir())
	set("02 报告/终稿 v2（最终）.docx", pfTE(2))
	set("cafe\u0301/菜单.docx", pfTE(3)) // NFD 的 é，字节必须原样保留
	set("caf\u00e9/菜单.docx", pfTE(4))  // NFC 的 é，和上一条是两个不同的键
	set("😀/空 格 .txt", pfTE(5))
	pfWant(t, b, want)

	// 压实
	if err := b.SetMeta(pfTMeta()); err != nil {
		t.Fatal(err)
	}
	if err := b.Compact(); err != nil {
		t.Fatal(err)
	}
	if b.Gen() != 2 || b.JournalLines() != 0 {
		t.Fatalf("压实后 gen=%d journal=%d", b.Gen(), b.JournalLines())
	}
	if fi, err := os.Stat(pfJournalP(meta)); err != nil || fi.Size() != 0 {
		t.Fatalf("压实后日志应被截空: %v", err)
	}
	// 压实后再写
	set("03 新增/a.docx", pfTE(6))
	if err := b.Del("02 报告/终稿 v2（最终）.docx"); err != nil {
		t.Fatal(err)
	}
	delete(want, "02 报告/终稿 v2（最终）.docx")
	set("01 来函/函件.docx", pfTE(7)) // 改写已有条目
	pfWant(t, b, want)

	// 重新加载，一致
	b2 := pfMustLoadOK(t, meta)
	pfWant(t, b2, want)
	if b2.Gen() != 2 || b2.Unit() != pfTU || b2.JournalLines() != 4 {
		t.Fatalf("gen=%d unit=%s journal=%d", b2.Gen(), b2.Unit(), b2.JournalLines())
	}
	if !reflect.DeepEqual(b2.Meta(), pfTMeta()) {
		t.Fatalf("meta %+v", b2.Meta())
	}
	// 再压实、再加载，仍一致
	if err := b2.Compact(); err != nil {
		t.Fatal(err)
	}
	b3 := pfMustLoadOK(t, meta)
	pfWant(t, b3, want)
	if b3.Gen() != 3 {
		t.Fatalf("gen=%d", b3.Gen())
	}
	// 键的字节没被改写
	if _, ok := b3.Get("cafe\u0301/菜单.docx"); !ok {
		t.Fatal("NFD 键丢了")
	}
	if _, ok := b3.Get("caf\u00e9/菜单.docx"); !ok {
		t.Fatal("NFC 键丢了")
	}
}

func TestPFBase_SnapshotFormat(t *testing.T) {
	meta, b := pfNewBase(t)
	b.Set("a/b.docx", pfTE(1))
	b.Set("a", pfTDir())
	if err := b.SetLedger(PFBaseLedger{Hash: "h1h2", Export: pfTU + ".ledger/h1h2.json"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Compact(); err != nil {
		t.Fatal(err)
	}
	raw := pfReadFile(t, pfSnapP(meta))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "unit", "gen", "mac_rel", "ubuntu_rel", "mac_dir_id", "ubuntu_dir_id", "updated", "entries", "ledger", "checksum"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("快照缺字段 %s: %s", k, raw)
		}
	}
	if m["version"].(float64) != 1 || m["unit"] != pfTU || m["gen"].(float64) != 2 {
		t.Fatalf("字段值 %v", m)
	}
	ent := m["entries"].(map[string]any)["a/b.docx"].(map[string]any)
	for _, k := range []string{"t", "size", "sha256", "mac", "ub"} {
		if _, ok := ent[k]; !ok {
			t.Fatalf("条目缺字段 %s: %v", k, ent)
		}
	}
	if _, ok := ent["mac"].(map[string]any)["mtime_ns"]; !ok {
		t.Fatal("mac 缺 mtime_ns")
	}
	if _, ok := ent["ub"].(map[string]any)["ino"]; !ok {
		t.Fatal("ub 缺 ino")
	}
	if id := m["mac_dir_id"].(map[string]any); id["dev"].(float64) != 16777234 || id["ino"].(float64) != 4242 {
		t.Fatalf("mac_dir_id %v", id)
	}
	// 校验和 = 正文规范化 JSON 的 sha256
	body, cerr := pfParseSnapshot(raw, pfTU)
	if cerr != nil {
		t.Fatal(cerr)
	}
	sum, _ := pfBodyChecksum(body)
	if m["checksum"] != sum {
		t.Fatalf("checksum %v != %s", m["checksum"], sum)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(pfSnapP(meta)); fi.Mode().Perm() != 0o600 {
			t.Fatalf("快照权限 %v", fi.Mode().Perm())
		}
	}
	// 日志行格式
	b.Set("c.docx", pfTE(2))
	b.Del("a")
	lines := strings.Split(strings.TrimRight(string(pfReadFile(t, pfJournalP(meta))), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("日志行数 %d", len(lines))
	}
	var l1, l2 map[string]any
	json.Unmarshal([]byte(lines[0]), &l1)
	json.Unmarshal([]byte(lines[1]), &l2)
	if l1["op"] != "set" || l1["path"] != "c.docx" || l1["gen"].(float64) != 2 || l1["t"] == nil || l1["entry"] == nil {
		t.Fatalf("set 行 %v", l1)
	}
	if l2["op"] != "del" || l2["path"] != "a" || l2["entry"] != nil {
		t.Fatalf("del 行 %v", l2)
	}
}

func TestPFBase_Views(t *testing.T) {
	_, b := pfNewBase(t)
	var v PFBaseView = b
	for _, p := range []string{"b/2", "a/1", "b/10", "a"} {
		if err := b.Set(p, pfTE(len(p))); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := v.Get("nope"); ok {
		t.Fatal("不存在的路径不应查到")
	}
	if e, ok := v.Get("a/1"); !ok || e != pfTE(3) {
		t.Fatalf("Get a/1 = %+v %v", e, ok)
	}
	var order []string
	v.Range(func(p string, e PFBaseEntry) bool {
		order = append(order, p)
		// 回调里调用写方法不应死锁
		if p == "a" {
			if err := b.Set("zz", pfTE(99)); err != nil {
				t.Error(err)
			}
		}
		return true
	})
	if strings.Join(order, ",") != "a,a/1,b/10,b/2" {
		t.Fatalf("Range 应按字节序遍历调用时刻的拷贝: %v", order)
	}
	if v.Len() != 5 {
		t.Fatalf("Len=%d", v.Len())
	}
	n := 0
	v.Range(func(string, PFBaseEntry) bool { n++; return n < 2 })
	if n != 2 {
		t.Fatalf("提前结束失败 n=%d", n)
	}
	// Entries 是拷贝
	m := v.Entries()
	delete(m, "a")
	if _, ok := v.Get("a"); !ok {
		t.Fatal("改拷贝影响了 base")
	}
	// dir id 对比
	mt := pfTMeta()
	c := v.CompareDirIDs(mt.MacDirID, mt.UbuntuDirID)
	if !c.MacSame || !c.UbuntuSame {
		t.Fatalf("应相同 %+v", c)
	}
	c = v.CompareDirIDs(PFDirID{Dev: mt.MacDirID.Dev, Ino: mt.MacDirID.Ino + 1}, mt.UbuntuDirID)
	if c.MacSame || !c.UbuntuSame {
		t.Fatalf("Mac 的 ino 变了 %+v", c)
	}
	c = v.CompareDirIDs(mt.MacDirID, PFDirID{Dev: mt.UbuntuDirID.Dev + 1, Ino: mt.UbuntuDirID.Ino})
	if !c.MacSame || c.UbuntuSame {
		t.Fatalf("Ubuntu 的 dev 变了 %+v", c)
	}
	// base 里没记录 → 视为不同
	_, z, err := func() (string, *PFBase, error) {
		meta := t.TempDir()
		z, err := PFBaseCreate(meta, pfTU, PFBaseMeta{})
		return meta, z, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	if c := z.CompareDirIDs(PFDirID{}, PFDirID{}); c.MacSame || c.UbuntuSame {
		t.Fatalf("零值记录不应算相同 %+v", c)
	}
	// 元数据通过 SetMeta + Compact 更新
	mt2 := PFBaseMeta{MacRel: "x", UbuntuRel: "y", MacDirID: PFDirID{1, 2}, UbuntuDirID: PFDirID{3, 4}}
	if err := b.SetMeta(mt2); err != nil {
		t.Fatal(err)
	}
	if b.Meta() != mt2 {
		t.Fatal("SetMeta 没生效")
	}
}

func TestPFBase_NoOpWritesNothing(t *testing.T) {
	meta, b := pfNewBase(t)
	if err := b.Set("a", pfTE(1)); err != nil {
		t.Fatal(err)
	}
	size := func() int64 {
		fi, err := os.Stat(pfJournalP(meta))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	s0 := size()
	b.Set("a", pfTE(1))         // 相同
	b.Del("不存在")                // 本来就没有
	b.Apply(nil)                // 空
	b.SetLedger(PFBaseLedger{}) // 台账没变
	if size() != s0 {
		t.Fatal("没有变化时不应写日志")
	}
	// 批量里「先设后删」同一路径：净效果是不存在，日志里两行
	if err := b.Apply([]PFBaseOp{{Path: "x", Entry: pfTE(5)}, {Path: "x", Del: true}, {Path: "y", Entry: pfTE(6)}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Get("x"); ok {
		t.Fatal("x 应已删除")
	}
	b2 := pfMustLoadOK(t, meta)
	pfWant(t, b2, map[string]PFBaseEntry{"a": pfTE(1), "y": pfTE(6)})
}

// ---------- 拒绝非法输入 ----------

func TestPFBase_RejectsBadInput(t *testing.T) {
	meta, b := pfNewBase(t)
	good := pfTE(1)
	if err := b.Set("ok", good); err != nil {
		t.Fatal(err)
	}
	jsize := func() int64 { fi, _ := os.Stat(pfJournalP(meta)); return fi.Size() }
	s0 := jsize()

	bad := []struct {
		name string
		path string
		e    PFBaseEntry
	}{
		{"非法 UTF-8 路径", "文档/\xff\xfe.docx", good},
		{"截断的 UTF-8", "a/\xe4\xb8", good},
		{"空路径", "", good},
		{"绝对路径", "/etc/passwd", good},
		{"含 ..", "a/../b", good},
		{"含反斜杠", `a\b`, good},
		{"类型不认识", "p", PFBaseEntry{T: "l", Size: 1, SHA256: good.SHA256}},
		{"文件没有哈希", "p", PFBaseEntry{T: "f", Size: 1}},
		{"哈希大写", "p", PFBaseEntry{T: "f", Size: 1, SHA256: strings.ToUpper(good.SHA256)}},
		{"哈希太短", "p", PFBaseEntry{T: "f", Size: 1, SHA256: "abc"}},
		{"大小为负", "p", PFBaseEntry{T: "f", Size: -1, SHA256: good.SHA256}},
		{"目录带哈希", "p", PFBaseEntry{T: "d", SHA256: good.SHA256}},
		{"目录带大小", "p", PFBaseEntry{T: "d", Size: 3}},
	}
	for _, c := range bad {
		if err := b.Set(c.path, c.e); err == nil {
			t.Errorf("%s: Set 应拒绝", c.name)
		}
	}
	if err := b.Del("文档/\xff.docx"); err == nil {
		t.Error("Del 非法 UTF-8 应拒绝")
	}
	// 批量里有一个非法 → 整批不写
	err := b.Apply([]PFBaseOp{{Path: "g1", Entry: good}, {Path: "坏\xff", Entry: good}, {Path: "g2", Entry: good}})
	if err == nil {
		t.Error("批量里有非法路径应整批拒绝")
	}
	if _, ok := b.Get("g1"); ok {
		t.Error("整批拒绝后 g1 不应已写入")
	}
	if jsize() != s0 {
		t.Error("拒绝的写入不应动日志")
	}
	if b.Err() != nil {
		t.Errorf("校验失败不应让对象进入出错状态: %v", b.Err())
	}
	// 合法的 U+FFFD 字面字符是合法 UTF-8，可以写，并原样读回
	if err := b.Set("a/�.txt", good); err != nil {
		t.Errorf("字面 U+FFFD 是合法 UTF-8: %v", err)
	}
	b2 := pfMustLoadOK(t, meta)
	if _, ok := b2.Get("a/�.txt"); !ok {
		t.Error("U+FFFD 路径读回丢了")
	}
	// 元数据里的非法 UTF-8
	if err := b.SetMeta(PFBaseMeta{MacRel: "\xff"}); err == nil {
		t.Error("SetMeta 非法 UTF-8 应拒绝")
	}
	if _, err := PFBaseCreate(t.TempDir(), pfTU, PFBaseMeta{UbuntuRel: "x\xff"}); err == nil {
		t.Error("Create 非法 UTF-8 应拒绝")
	}
	// 单元键
	for _, k := range []string{"", "../x", "a/b", "a b", "编号", strings.Repeat("A", 65)} {
		if _, err := PFBaseLoad(t.TempDir(), k); err == nil {
			t.Errorf("单元键 %q 应拒绝", k)
		}
		if _, err := PFBaseCreate(t.TempDir(), k, PFBaseMeta{}); err == nil {
			t.Errorf("Create 单元键 %q 应拒绝", k)
		}
	}
}

// ---------- 无 base ----------

func TestPFBase_Missing(t *testing.T) {
	meta := t.TempDir()
	r, err := PFBaseLoad(meta, pfTU)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != PFBaseMissing || !r.FirstTime() || r.Base != nil || r.Reason == "" || len(r.Moved) != 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(meta, "base")); err == nil {
		t.Fatal("加载不应建目录")
	}
	// 只有孤立日志：移进 坏/
	os.MkdirAll(filepath.Join(meta, "base"), 0o700)
	orphan := []byte("{\"gen\":1,\"path\":\"a\",\"op\":\"del\",\"t\":\"x\"}\n")
	os.WriteFile(pfJournalP(meta), orphan, 0o600)
	r, err = PFBaseLoad(meta, pfTU)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != PFBaseMissing || len(r.Moved) != 1 || !strings.Contains(r.Reason, "日志") {
		t.Fatalf("%+v", r)
	}
	if got := pfReadFile(t, r.Moved[0]); string(got) != string(orphan) {
		t.Fatal("孤立日志内容被改了")
	}
	if _, err := os.Stat(pfJournalP(meta)); err == nil {
		t.Fatal("孤立日志应已移走")
	}
	// 空日志文件：不算孤立，不移
	os.WriteFile(pfJournalP(meta), nil, 0o600)
	r, _ = PFBaseLoad(meta, pfTU)
	if r.Status != PFBaseMissing || len(r.Moved) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestPFBase_CreateExistsAndOrphanJournal(t *testing.T) {
	meta, b := pfNewBase(t)
	b.Set("a", pfTE(1))
	if _, err := PFBaseCreate(meta, pfTU, pfTMeta()); !errors.Is(err, ErrPFBaseExists) {
		t.Fatalf("已有快照应返回 ErrPFBaseExists: %v", err)
	}
	// 删了快照、留下带 gen=1 行的日志：Create 必须先把它移走，否则旧行会被新一代 gen=1 当成自己的
	os.Remove(pfSnapP(meta))
	old := pfReadFile(t, pfJournalP(meta))
	b2, err := PFBaseCreate(meta, pfTU, pfTMeta())
	if err != nil {
		t.Fatal(err)
	}
	if b2.Len() != 0 {
		t.Fatal("新 base 应为空")
	}
	b3 := pfMustLoadOK(t, meta)
	pfWant(t, b3, map[string]PFBaseEntry{})
	bad := pfBadList(t, meta)
	if len(bad) != 1 || !strings.HasSuffix(bad[0], ".journal") {
		t.Fatalf("坏/: %v", bad)
	}
	if got := pfReadFile(t, filepath.Join(meta, "base", "坏", bad[0])); string(got) != string(old) {
		t.Fatal("移走的日志内容不一致")
	}
}

func TestPFBase_ReadErrorIsNotCorruption(t *testing.T) {
	meta := t.TempDir()
	// 快照路径是目录：读不了，返回 error，什么都不移
	if err := os.MkdirAll(pfSnapP(meta), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := PFBaseLoad(meta, pfTU)
	if err == nil {
		t.Fatalf("读不了应返回 error: %+v", r)
	}
	if len(pfBadList(t, meta)) != 0 {
		t.Fatal("读不了不应移动任何文件")
	}
}

func TestPFBase_StaleTmpRemoved(t *testing.T) {
	meta, b := pfNewBase(t)
	b.Set("a", pfTE(1))
	tmp := filepath.Join(meta, "base", pfTU+".json.tmp-12345")
	os.WriteFile(tmp, []byte("半截"), 0o600)
	pfMustLoadOK(t, meta)
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("上次崩溃留下的临时文件应在加载时清掉")
	}
}

// ---------- 日志尾行 ----------

func TestPFBase_TornJournalTail(t *testing.T) {
	cases := []struct {
		name string
		tail string
	}{
		{"半行（不完整 JSON）", `{"gen":1,"path":"c","op":"se`},
		{"完整 JSON 但没有换行结尾", `{"gen":1,"path":"c","op":"del","t":"x"}`},
		{"有换行但不是合规的一行", "garbage garbage\n"},
		{"只有半个多字节字符", "{\"gen\":1,\"path\":\"\xe4\xb8"},
		{"一串 NUL（延迟分配的撕裂尾巴）", strings.Repeat("\x00", 37)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta, b := pfNewBase(t)
			b.Set("a", pfTE(1))
			b.Set("b", pfTE(2))
			validLen := len(pfReadFile(t, pfJournalP(meta)))
			f, err := os.OpenFile(pfJournalP(meta), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString(c.tail)
			f.Close()

			r, err := PFBaseLoad(meta, pfTU)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != PFBaseOK {
				t.Fatalf("最后一行写了一半不是损坏: %v %s", r.Status, r.Reason)
			}
			pfWant(t, r.Base, map[string]PFBaseEntry{"a": pfTE(1), "b": pfTE(2)})
			if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "丢掉") {
				t.Fatalf("应报告丢掉了尾行: %v", r.Notes)
			}
			// 被丢掉的字节原样存进 坏/，不丢任何东西
			bad := pfBadList(t, meta)
			if len(bad) != 1 || !strings.HasSuffix(bad[0], ".journal-tail") {
				t.Fatalf("坏/: %v", bad)
			}
			if got := pfReadFile(t, filepath.Join(meta, "base", "坏", bad[0])); string(got) != c.tail {
				t.Fatalf("尾行内容 %q", got)
			}
			// 日志已截到最后一个完整行，后面的追加不会粘在半行后面
			if got := len(pfReadFile(t, pfJournalP(meta))); got != validLen {
				t.Fatalf("日志长度 %d，应为 %d", got, validLen)
			}
			if err := r.Base.Set("d", pfTE(4)); err != nil {
				t.Fatal(err)
			}
			b3 := pfMustLoadOK(t, meta)
			pfWant(t, b3, map[string]PFBaseEntry{"a": pfTE(1), "b": pfTE(2), "d": pfTE(4)})
		})
	}
	t.Run("日志只有半行", func(t *testing.T) {
		meta, _ := pfNewBase(t)
		os.WriteFile(pfJournalP(meta), []byte(`{"gen":1,"pa`), 0o600)
		r, err := PFBaseLoad(meta, pfTU)
		if err != nil || r.Status != PFBaseOK || r.Base.Len() != 0 {
			t.Fatalf("%v %+v", err, r)
		}
	})
}

// ---------- 损坏 ----------

func pfCorruptSetup(t *testing.T) (meta string, origSnap, origJournal []byte) {
	t.Helper()
	meta, b := pfNewBase(t)
	b.Set("a", pfTE(1))
	b.Set("b", pfTE(2))
	if err := b.Compact(); err != nil { // gen=2，日志清空
		t.Fatal(err)
	}
	b.Set("c", pfTE(3)) // 日志里一行 gen=2
	return meta, pfReadFile(t, pfSnapP(meta)), pfReadFile(t, pfJournalP(meta))
}

// pfCraftSnapshot 改快照正文并重新算出正确的校验和，用来构造「校验和对、内容却不合规」的快照。
func pfCraftSnapshot(t *testing.T, meta string, mutate func(*pfSnapBody)) {
	t.Helper()
	body, cerr := pfParseSnapshot(pfReadFile(t, pfSnapP(meta)), pfTU)
	if cerr != nil {
		t.Fatal(cerr)
	}
	mutate(body)
	sum, err := pfBodyChecksum(body)
	if err != nil {
		t.Fatal(err)
	}
	data, err := pfCanonJSON(pfSnapFile{pfSnapBody: *body, Checksum: sum})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pfSnapP(meta), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPFBase_Corrupt(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, meta string, snap, journal []byte)
		reason string // Reason 里必须出现的片段
	}{
		{"快照不是 JSON", func(t *testing.T, meta string, snap, journal []byte) {
			os.WriteFile(pfSnapP(meta), []byte("{not json"), 0o600)
		}, "JSON"},
		{"快照是空文件", func(t *testing.T, meta string, snap, journal []byte) {
			os.WriteFile(pfSnapP(meta), nil, 0o600)
		}, "JSON"},
		{"快照被截断一半", func(t *testing.T, meta string, snap, journal []byte) {
			os.WriteFile(pfSnapP(meta), snap[:len(snap)/2], 0o600)
		}, "JSON"},
		{"快照不是对象", func(t *testing.T, meta string, snap, journal []byte) {
			os.WriteFile(pfSnapP(meta), []byte("[1,2,3]"), 0o600)
		}, "JSON"},
		{"校验和不对", func(t *testing.T, meta string, snap, journal []byte) {
			s := strings.Replace(string(snap), `"gen":2`, `"gen":3`, 1)
			if s == string(snap) {
				t.Fatal("构造失败")
			}
			os.WriteFile(pfSnapP(meta), []byte(s), 0o600)
		}, "校验和"},
		{"某个条目被改了一位", func(t *testing.T, meta string, snap, journal []byte) {
			s := strings.Replace(string(snap), `"size":10`, `"size":11`, 1)
			if s == string(snap) {
				t.Fatal("构造失败")
			}
			os.WriteFile(pfSnapP(meta), []byte(s), 0o600)
		}, "校验和"},
		{"没有 version", func(t *testing.T, meta string, snap, journal []byte) {
			s := strings.Replace(string(snap), `"version":1,`, ``, 1)
			os.WriteFile(pfSnapP(meta), []byte(s), 0o600)
		}, "version"},
		{"version 不认识", func(t *testing.T, meta string, snap, journal []byte) {
			s := strings.Replace(string(snap), `"version":1`, `"version":2`, 1)
			if s == string(snap) {
				t.Fatal("构造失败")
			}
			os.WriteFile(pfSnapP(meta), []byte(s), 0o600)
		}, "版本 2"},
		{"快照末尾有多余内容", func(t *testing.T, meta string, snap, journal []byte) {
			os.WriteFile(pfSnapP(meta), append(append([]byte{}, snap...), []byte("{}")...), 0o600)
		}, "JSON"},
		{"校验和对但条目类型不认识", func(t *testing.T, meta string, snap, journal []byte) {
			pfCraftSnapshot(t, meta, func(b *pfSnapBody) {
				e := b.Entries["a"]
				e.T = "x"
				b.Entries["a"] = e
			})
		}, "不合规"},
		{"校验和对但路径不合法", func(t *testing.T, meta string, snap, journal []byte) {
			pfCraftSnapshot(t, meta, func(b *pfSnapBody) { b.Entries["../逃出去"] = pfTE(9) })
		}, "不合规"},
		{"快照属于别的单元", func(t *testing.T, meta string, snap, journal []byte) {
			pfCraftSnapshot(t, meta, func(b *pfSnapBody) { b.Unit = "OTHER9" })
		}, "OTHER9"},
		{"日志中间有坏行", func(t *testing.T, meta string, snap, journal []byte) {
			os.WriteFile(pfJournalP(meta), append([]byte("garbage\n"), journal...), 0o600)
		}, "日志第 1 行"},
		{"日志中间有空行", func(t *testing.T, meta string, snap, journal []byte) {
			os.WriteFile(pfJournalP(meta), append(append([]byte{}, journal...), append([]byte("\n"), journal...)...), 0o600)
		}, "日志第 2 行"},
		{"日志中间有未知 op", func(t *testing.T, meta string, snap, journal []byte) {
			bad := `{"gen":2,"path":"x","op":"zap","t":"x"}` + "\n"
			os.WriteFile(pfJournalP(meta), append([]byte(bad), journal...), 0o600)
		}, "日志第 1 行"},
		{"日志中间有未知字段", func(t *testing.T, meta string, snap, journal []byte) {
			bad := `{"gen":2,"path":"x","op":"del","t":"x","extra":1}` + "\n"
			os.WriteFile(pfJournalP(meta), append([]byte(bad), journal...), 0o600)
		}, "日志第 1 行"},
		{"日志中间有条目不合规", func(t *testing.T, meta string, snap, journal []byte) {
			bad := `{"gen":2,"path":"x","op":"set","entry":{"t":"f","size":1,"sha256":"zz","mac":{"mtime_ns":0,"ino":0},"ub":{"mtime_ns":0,"ino":0}},"t":"x"}` + "\n"
			os.WriteFile(pfJournalP(meta), append([]byte(bad), journal...), 0o600)
		}, "日志第 1 行"},
		{"日志来自更新的一代（快照被回退）", func(t *testing.T, meta string, snap, journal []byte) {
			os.WriteFile(pfJournalP(meta), []byte(strings.Replace(string(journal), `"gen":2`, `"gen":9`, 1)), 0o600)
		}, "第 9 代"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta, snap, journal := pfCorruptSetup(t)
			c.mutate(t, meta, snap, journal)
			// 变异之后文件的实际内容：移进 坏/ 之后必须原样保存
			curSnap := pfReadFile(t, pfSnapP(meta))
			curJournal := pfReadFile(t, pfJournalP(meta))

			r, err := PFBaseLoad(meta, pfTU)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != PFBaseCorrupt || !r.FirstTime() || r.Base != nil {
				t.Fatalf("应报损坏、按首次处理: %+v", r)
			}
			if !strings.Contains(r.Reason, c.reason) {
				t.Fatalf("原因里应含 %q: %s", c.reason, r.Reason)
			}
			for _, p := range []string{pfSnapP(meta), pfJournalP(meta)} {
				if _, err := os.Stat(p); err == nil {
					t.Fatalf("%s 应已被移走", p)
				}
			}
			if len(r.Moved) != 2 {
				t.Fatalf("快照和日志应一起移走: %v", r.Moved)
			}
			var gotSnap, gotJournal []byte
			for _, m := range r.Moved {
				if filepath.Dir(m) != filepath.Join(meta, "base", "坏") || !strings.HasPrefix(filepath.Base(m), pfTU+"-") {
					t.Fatalf("移到了不该去的地方: %s", m)
				}
				switch filepath.Ext(m) {
				case ".json":
					gotSnap = pfReadFile(t, m)
				case ".journal":
					gotJournal = pfReadFile(t, m)
				default:
					t.Fatalf("扩展名 %s", m)
				}
			}
			if string(gotSnap) != string(curSnap) || string(gotJournal) != string(curJournal) {
				t.Fatal("移进 坏/ 的内容和原文件不一致（不能丢任何字节）")
			}
			// 再次加载：无 base（不是再次损坏），也不会把旧数据当 base
			r2, err := PFBaseLoad(meta, pfTU)
			if err != nil || r2.Status != PFBaseMissing {
				t.Fatalf("%v %+v", err, r2)
			}
			// 按首次规则新建，从零开始
			nb, err := PFBaseCreate(meta, pfTU, pfTMeta())
			if err != nil {
				t.Fatal(err)
			}
			if nb.Len() != 0 || nb.Gen() != 1 {
				t.Fatalf("新 base gen=%d len=%d", nb.Gen(), nb.Len())
			}
			nb.Set("z", pfTE(26))
			pfWant(t, pfMustLoadOK(t, meta), map[string]PFBaseEntry{"z": pfTE(26)})
		})
	}
}

func TestPFBase_CorruptKeepsSnapshotWhenJournalOnlyIsBad(t *testing.T) {
	// 快照好、日志中间坏：日志和快照一起移走（日志不能单独留着，否则会和以后的新快照错配）
	meta, snap, journal := pfCorruptSetup(t)
	os.WriteFile(pfJournalP(meta), append([]byte("garbage\n"), journal...), 0o600)
	r, _ := PFBaseLoad(meta, pfTU)
	if r.Status != PFBaseCorrupt || len(r.Moved) != 2 {
		t.Fatalf("%+v", r)
	}
	var found bool
	for _, m := range r.Moved {
		if filepath.Ext(m) == ".json" && string(pfReadFile(t, m)) == string(snap) {
			found = true
		}
	}
	if !found {
		t.Fatal("好的快照也应原样存进 坏/")
	}
}

func TestPFBase_QuarantineNameCollision(t *testing.T) {
	// 同一时刻两次损坏：第二次不覆盖第一次
	fixed := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	env := pfBaseEnv{now: func() time.Time { return fixed }}
	meta := t.TempDir()
	var moved [][]string
	for i := 0; i < 3; i++ {
		os.MkdirAll(filepath.Join(meta, "base"), 0o700)
		os.WriteFile(pfSnapP(meta), []byte(fmt.Sprintf("坏 %d", i)), 0o600)
		r, err := pfBaseLoad(meta, pfTU, env)
		if err != nil || r.Status != PFBaseCorrupt {
			t.Fatalf("%v %+v", err, r)
		}
		moved = append(moved, r.Moved)
	}
	bad := pfBadList(t, meta)
	if len(bad) != 3 {
		t.Fatalf("三次损坏应有三个文件: %v", bad)
	}
	for i, m := range moved {
		if got := string(pfReadFile(t, m[0])); got != fmt.Sprintf("坏 %d", i) {
			t.Fatalf("第 %d 份被覆盖: %q", i, got)
		}
	}
	if !strings.Contains(bad[0], "20261026-120000.000") {
		t.Fatalf("文件名应带时间: %v", bad)
	}
}

// ---------- 台账 ----------

func TestPFBase_Ledger(t *testing.T) {
	meta, b := pfNewBase(t)
	hash := strings.Repeat("a", 64) + strings.Repeat("b", 64)
	ref, err := b.SaveLedgerExport(hash, []byte(`{"versions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if ref != pfTU+".ledger/"+hash+".json" {
		t.Fatalf("ref=%s", ref)
	}
	if got, err := b.ReadLedgerExport(hash); err != nil || string(got) != `{"versions":[]}` {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(meta, "base", pfTU+".ledger", hash+".json")); err != nil {
		t.Fatalf("导出副本应在 base 目录旁: %v", err)
	}
	for _, h := range []string{"", "../x", "a/b", "a.b", strings.Repeat("a", 257)} {
		if _, err := b.SaveLedgerExport(h, []byte("x")); err == nil {
			t.Errorf("哈希 %q 应拒绝", h)
		}
		if _, err := b.ReadLedgerExport(h); err == nil {
			t.Errorf("读哈希 %q 应拒绝", h)
		}
	}
	l := PFBaseLedger{Hash: hash, Export: ref}
	if err := b.SetLedger(l); err != nil {
		t.Fatal(err)
	}
	if b.Ledger() != l {
		t.Fatal("Ledger() 不对")
	}
	// 日志回放得到台账
	b2 := pfMustLoadOK(t, meta)
	if b2.Ledger() != l {
		t.Fatalf("回放后台账 %+v", b2.Ledger())
	}
	// 压实后仍在
	b2.Compact()
	if got := pfMustLoadOK(t, meta).Ledger(); got != l {
		t.Fatalf("压实后台账 %+v", got)
	}
	// 清空台账记录也要能回放
	b3 := pfMustLoadOK(t, meta)
	if err := b3.SetLedger(PFBaseLedger{}); err != nil {
		t.Fatal(err)
	}
	if got := pfMustLoadOK(t, meta).Ledger(); got != (PFBaseLedger{}) {
		t.Fatalf("清空后台账 %+v", got)
	}
}

func TestPFBase_Reset(t *testing.T) {
	meta, b := pfNewBase(t)
	b.Set("a", pfTE(1))
	b.Compact()
	b.Set("b", pfTE(2)) // 日志里留着旧一代的行
	b.SetLedger(PFBaseLedger{Hash: "h"})
	newMeta := PFBaseMeta{MacRel: "新/路径", UbuntuRel: "新/路径", MacDirID: PFDirID{7, 8}, UbuntuDirID: PFDirID{9, 10}}
	if err := b.Reset(newMeta); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 0 || b.Gen() != 3 || b.Ledger() != (PFBaseLedger{}) || b.Meta() != newMeta {
		t.Fatalf("Reset 后 len=%d gen=%d", b.Len(), b.Gen())
	}
	b.Set("c", pfTE(3))
	b2 := pfMustLoadOK(t, meta)
	pfWant(t, b2, map[string]PFBaseEntry{"c": pfTE(3)})
	if b2.Meta() != newMeta {
		t.Fatalf("meta %+v", b2.Meta())
	}
	if err := b.Reset(PFBaseMeta{MacRel: "\xff"}); err == nil {
		t.Fatal("非法 UTF-8 应拒绝")
	}
}

// ---------- 出错状态 ----------

func TestPFBase_BrokenAfterAppendFailure(t *testing.T) {
	meta, b := pfNewBase(t)
	b.Set("a", pfTE(1))
	c := &pfCrashHook{at: 1} // append:written 之后失败（fsync 没做）
	b.env.hook = c.fn
	err := b.Set("b", pfTE(2))
	if !errors.Is(err, errPFTestCrash) {
		t.Fatalf("应返回注入的错误: %v", err)
	}
	if !errors.Is(b.Err(), ErrPFBaseBroken) {
		t.Fatalf("应进入出错状态: %v", b.Err())
	}
	if _, ok := b.Get("b"); ok {
		t.Fatal("没确认提交的条目不应进内存")
	}
	b.env.hook = nil
	if err := b.Set("c", pfTE(3)); !errors.Is(err, ErrPFBaseBroken) {
		t.Fatalf("出错后写入应被拒绝: %v", err)
	}
	if err := b.Compact(); !errors.Is(err, ErrPFBaseBroken) {
		t.Fatalf("出错后压实应被拒绝: %v", err)
	}
	if err := b.Reset(pfTMeta()); !errors.Is(err, ErrPFBaseBroken) {
		t.Fatalf("出错后 Reset 应被拒绝: %v", err)
	}
	if err := b.SetLedger(PFBaseLedger{Hash: "x"}); !errors.Is(err, ErrPFBaseBroken) {
		t.Fatalf("出错后 SetLedger 应被拒绝: %v", err)
	}
	// 读仍可用；重新加载后恢复正常
	if _, ok := b.Get("a"); !ok {
		t.Fatal("读视图应仍可用")
	}
	b2 := pfMustLoadOK(t, meta)
	if b2.Err() != nil {
		t.Fatal("重新加载的对象应是好的")
	}
	if err := b2.Set("c", pfTE(3)); err != nil {
		t.Fatal(err)
	}
}

// ---------- 崩溃注入 ----------

// pfForEachCrash 先不崩溃跑一遍，记下钩子经过的每一步；再对每一步各跑一遍，在该步之后「进程中断」。
// scenario 自己建临时目录、做准备、挂钩子执行被测动作、重新加载并检查。baseline 是第一遍记下的步骤（第一遍本身传 nil）。
func pfForEachCrash(t *testing.T, scenario func(t *testing.T, c *pfCrashHook, baseline []string)) {
	t.Helper()
	rec := &pfCrashHook{at: -1}
	t.Run("不崩溃", func(t *testing.T) { scenario(t, rec, nil) })
	if len(rec.steps) == 0 {
		t.Fatal("没有记到任何步骤")
	}
	steps := append([]string(nil), rec.steps...)
	for i, s := range steps {
		t.Run(fmt.Sprintf("%02d-崩在%s", i, s), func(t *testing.T) {
			scenario(t, &pfCrashHook{at: i}, steps)
		})
	}
}

func pfIndex(steps []string, s string) int {
	for i, x := range steps {
		if x == s {
			return i
		}
	}
	return -1
}

func pfNoTmp(t *testing.T, meta string) {
	t.Helper()
	if left := pfTmpLeft(t, filepath.Join(meta, "base")); len(left) != 0 {
		t.Fatalf("加载后不应残留临时文件: %v", left)
	}
}

// 压实的每两步之间崩溃：已 fsync 的条目一条不丢；之后还能继续写、再压实、再加载。
func TestPFBase_CrashDuringCompact(t *testing.T) {
	for _, rounds := range []int{0, 1} { // 0：第一次压实（gen 1→2）；1：之前已压实过一次，日志里是第 2 代的行
		t.Run(fmt.Sprintf("此前压实%d次", rounds), func(t *testing.T) {
			pfForEachCrash(t, func(t *testing.T, c *pfCrashHook, baseline []string) {
				meta, b := pfNewBase(t)
				want := map[string]PFBaseEntry{}
				add := func(p string, e PFBaseEntry) {
					if err := b.Set(p, e); err != nil {
						t.Fatal(err)
					}
					want[p] = e
				}
				if rounds == 1 {
					add("early", pfTE(10))
					if err := b.Compact(); err != nil {
						t.Fatal(err)
					}
				}
				add("a", pfTE(1))
				add("b", pfTE(2))
				add("c", pfTE(3))
				if err := b.Del("a"); err != nil {
					t.Fatal(err)
				}
				delete(want, "a")

				b.env.hook = c.fn
				err := b.Compact()
				if c.at < 0 {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, errPFTestCrash) {
					t.Fatalf("应返回崩溃 error: %v", err)
				}
				// 进程死了：丢掉 b，只看磁盘
				r := pfMustLoadOK(t, meta)
				pfWant(t, r, want)
				pfNoTmp(t, meta)
				// 日志里的旧行（压实写完、没来得及截断）不能影响之后新写的行
				if err := r.Set("d", pfTE(4)); err != nil {
					t.Fatal(err)
				}
				want["d"] = pfTE(4)
				r2 := pfMustLoadOK(t, meta)
				pfWant(t, r2, want)
				if err := r2.Compact(); err != nil {
					t.Fatal(err)
				}
				pfWant(t, pfMustLoadOK(t, meta), want)
			})
		})
	}
}

// 追加日志的每两步之间崩溃：之前已 fsync 的不丢；本条要么完整在、要么完全不在，不会出现半条。
func TestPFBase_CrashDuringAppend(t *testing.T) {
	for _, fresh := range []bool{false, true} { // fresh：日志文件还不存在（新建）
		t.Run(fmt.Sprintf("日志文件新建=%v", fresh), func(t *testing.T) {
			pfForEachCrash(t, func(t *testing.T, c *pfCrashHook, baseline []string) {
				meta, b := pfNewBase(t)
				want := map[string]PFBaseEntry{}
				if !fresh {
					for i := 1; i <= 3; i++ {
						p := fmt.Sprintf("old%d", i)
						if err := b.Set(p, pfTE(i)); err != nil {
							t.Fatal(err)
						}
						want[p] = pfTE(i)
					}
				}
				b.env.hook = c.fn
				err := b.Apply([]PFBaseOp{{Path: "n1", Entry: pfTE(21)}, {Path: "n2", Entry: pfTE(22)}})
				if c.at < 0 {
					if err != nil {
						t.Fatal(err)
					}
					want["n1"], want["n2"] = pfTE(21), pfTE(22)
				} else {
					if !errors.Is(err, errPFTestCrash) {
						t.Fatalf("应返回崩溃 error: %v", err)
					}
					if _, ok := b.Get("n1"); ok {
						t.Fatal("没确认的条目不应进内存")
					}
					// 一次 write 写出整批：write 之前崩溃 → 都没有；之后 → 都有
					if c.at >= pfIndex(baseline, "append:written") {
						want["n1"], want["n2"] = pfTE(21), pfTE(22)
					}
				}
				r := pfMustLoadOK(t, meta)
				pfWant(t, r, want)
				if err := r.Set("after", pfTE(30)); err != nil {
					t.Fatal(err)
				}
				want["after"] = pfTE(30)
				pfWant(t, pfMustLoadOK(t, meta), want)
			})
		})
	}
}

// 新建 base 的每两步之间崩溃：重新加载要么「无 base」、要么是个空 base，绝不是损坏。
func TestPFBase_CrashDuringCreate(t *testing.T) {
	pfForEachCrash(t, func(t *testing.T, c *pfCrashHook, baseline []string) {
		meta := t.TempDir()
		_, err := pfBaseCreate(meta, pfTU, pfTMeta(), pfBaseEnv{hook: c.fn})
		if c.at < 0 {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, errPFTestCrash) {
			t.Fatalf("应返回崩溃 error: %v", err)
		}
		r, err := PFBaseLoad(meta, pfTU)
		if err != nil {
			t.Fatal(err)
		}
		pfNoTmp(t, meta)
		renamed := c.at < 0 || c.at >= pfIndex(baseline, "snap:renamed")
		if renamed {
			if r.Status != PFBaseOK {
				t.Fatalf("改名之后应能载入: %v %s", r.Status, r.Reason)
			}
			pfWant(t, r.Base, map[string]PFBaseEntry{})
			if _, err := PFBaseCreate(meta, pfTU, pfTMeta()); !errors.Is(err, ErrPFBaseExists) {
				t.Fatalf("%v", err)
			}
		} else {
			if r.Status != PFBaseMissing {
				t.Fatalf("改名之前崩溃应是无 base: %v %s", r.Status, r.Reason)
			}
			nb, err := PFBaseCreate(meta, pfTU, pfTMeta())
			if err != nil {
				t.Fatal(err)
			}
			nb.Set("x", pfTE(1))
			pfWant(t, pfMustLoadOK(t, meta), map[string]PFBaseEntry{"x": pfTE(1)})
		}
		if len(pfBadList(t, meta)) != 0 {
			t.Fatalf("不应有东西被当成损坏移走: %v", pfBadList(t, meta))
		}
	})
}

// Reset 的每两步之间崩溃：要么还是旧内容、要么是空；旧一代的日志行不会复活。
func TestPFBase_CrashDuringReset(t *testing.T) {
	pfForEachCrash(t, func(t *testing.T, c *pfCrashHook, baseline []string) {
		meta, b := pfNewBase(t)
		old := map[string]PFBaseEntry{}
		for i := 1; i <= 3; i++ {
			p := fmt.Sprintf("p%d", i)
			b.Set(p, pfTE(i))
			old[p] = pfTE(i)
		}
		b.Compact()
		b.Set("p4", pfTE(4)) // 压实后日志里还有一行
		old["p4"] = pfTE(4)

		b.env.hook = c.fn
		err := b.Reset(pfTMeta())
		if c.at >= 0 && !errors.Is(err, errPFTestCrash) {
			t.Fatalf("应返回崩溃 error: %v", err)
		}
		if c.at < 0 && err != nil {
			t.Fatal(err)
		}
		r := pfMustLoadOK(t, meta)
		pfNoTmp(t, meta)
		if c.at < 0 || c.at >= pfIndex(baseline, "snap:renamed") {
			pfWant(t, r, map[string]PFBaseEntry{})
		} else {
			pfWant(t, r, old)
		}
		r.Set("new", pfTE(50))
		want := map[string]PFBaseEntry{"new": pfTE(50)}
		if c.at >= 0 && c.at < pfIndex(baseline, "snap:renamed") {
			for p, e := range old {
				want[p] = e
			}
		}
		pfWant(t, pfMustLoadOK(t, meta), want)
	})
}

// 加载时丢尾行的每两步之间崩溃：再加载一次得到同样的结果。
func TestPFBase_CrashDuringTailRepair(t *testing.T) {
	pfForEachCrash(t, func(t *testing.T, c *pfCrashHook, baseline []string) {
		meta, b := pfNewBase(t)
		b.Set("a", pfTE(1))
		b.Set("b", pfTE(2))
		f, _ := os.OpenFile(pfJournalP(meta), os.O_WRONLY|os.O_APPEND, 0)
		f.WriteString(`{"gen":1,"path":"c","op":"se`)
		f.Close()

		r, err := pfBaseLoad(meta, pfTU, pfBaseEnv{hook: c.fn})
		if c.at >= 0 {
			if !errors.Is(err, errPFTestCrash) {
				t.Fatalf("应返回崩溃 error: %v", err)
			}
		} else if err != nil || r.Status != PFBaseOK {
			t.Fatalf("%v %+v", err, r)
		}
		want := map[string]PFBaseEntry{"a": pfTE(1), "b": pfTE(2)}
		r2 := pfMustLoadOK(t, meta)
		pfWant(t, r2, want)
		if len(pfBadList(t, meta)) == 0 {
			t.Fatal("尾行应已存进 坏/")
		}
		r2.Set("c", pfTE(3))
		want["c"] = pfTE(3)
		pfWant(t, pfMustLoadOK(t, meta), want)
	})
}

// 把损坏文件移进 坏/ 的每两步之间崩溃：再加载一次，剩下的也会被移走，原始字节一个不少。
func TestPFBase_CrashDuringQuarantine(t *testing.T) {
	pfForEachCrash(t, func(t *testing.T, c *pfCrashHook, baseline []string) {
		meta, b := pfNewBase(t)
		b.Set("a", pfTE(1))
		b.Compact()
		b.Set("b", pfTE(2))
		snap := []byte("{坏掉的快照")
		os.WriteFile(pfSnapP(meta), snap, 0o600)
		journal := pfReadFile(t, pfJournalP(meta))

		r, err := pfBaseLoad(meta, pfTU, pfBaseEnv{hook: c.fn})
		if c.at >= 0 {
			if !errors.Is(err, errPFTestCrash) {
				t.Fatalf("应返回崩溃 error: %v", err)
			}
		} else if err != nil || r.Status != PFBaseCorrupt {
			t.Fatalf("%v %+v", err, r)
		}
		r2, err := PFBaseLoad(meta, pfTU)
		if err != nil || r2.Status != PFBaseMissing {
			t.Fatalf("再加载应是无 base（残留的日志已被当孤立日志移走）: %v %+v", err, r2)
		}
		var gotSnap, gotJournal bool
		for _, n := range pfBadList(t, meta) {
			data := pfReadFile(t, filepath.Join(meta, "base", "坏", n))
			if strings.HasSuffix(n, ".json") && string(data) == string(snap) {
				gotSnap = true
			}
			if strings.HasSuffix(n, ".journal") && string(data) == string(journal) {
				gotJournal = true
			}
		}
		if !gotSnap || !gotJournal {
			t.Fatalf("快照和日志都应原样在 坏/ 里: snap=%v journal=%v %v", gotSnap, gotJournal, pfBadList(t, meta))
		}
		nb, err := PFBaseCreate(meta, pfTU, pfTMeta())
		if err != nil {
			t.Fatal(err)
		}
		pfWant(t, nb, map[string]PFBaseEntry{})
		pfWant(t, pfMustLoadOK(t, meta), map[string]PFBaseEntry{})
	})
}

// 追加时撕裂（write 只写了一半）之后再压实：中途崩溃也不丢已确认的条目。
func TestPFBase_CrashCompactAfterTornTail(t *testing.T) {
	pfForEachCrash(t, func(t *testing.T, c *pfCrashHook, baseline []string) {
		meta, b := pfNewBase(t)
		b.Set("a", pfTE(1))
		b.Set("b", pfTE(2))
		f, _ := os.OpenFile(pfJournalP(meta), os.O_WRONLY|os.O_APPEND, 0)
		f.WriteString(`{"gen":1,"path":"c","op":"set","entry":{"t":"f"`)
		f.Close()
		// 进程重启：加载（丢尾行）、写一条、压实时崩溃
		r := pfMustLoadOK(t, meta)
		if err := r.Set("d", pfTE(4)); err != nil {
			t.Fatal(err)
		}
		r.env.hook = c.fn
		err := r.Compact()
		if c.at >= 0 && !errors.Is(err, errPFTestCrash) {
			t.Fatalf("%v", err)
		}
		if c.at < 0 && err != nil {
			t.Fatal(err)
		}
		want := map[string]PFBaseEntry{"a": pfTE(1), "b": pfTE(2), "d": pfTE(4)}
		pfWant(t, pfMustLoadOK(t, meta), want)
	})
}

// ---------- 并发（同进程内读写） ----------

func TestPFBase_ConcurrentReadersDuringWrites(t *testing.T) {
	_, b := pfNewBase(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				n := 0
				b.Range(func(string, PFBaseEntry) bool { n++; return true })
				b.Get("p1")
				b.Len()
				b.Entries()
			}
		}()
	}
	for i := 0; i < 30; i++ {
		if err := b.Set(fmt.Sprintf("p%d", i), pfTE(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Compact(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	if b.Len() != 30 {
		t.Fatalf("Len=%d", b.Len())
	}
}

// ---------- 规模 ----------

// 5 万条的耗时只打印不断言：用 -v 才能看到 t.Logf 输出。
func TestPFBase_Scale50k(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试，-short 跳过")
	}
	const n = 50000
	meta := t.TempDir()
	b, err := PFBaseCreate(meta, pfTU, pfTMeta())
	if err != nil {
		t.Fatal(err)
	}
	path := func(i int) string {
		return fmt.Sprintf("03 设计文件/子目录%03d/第%02d批/测试项目甲-图纸-%05d（修订）.docx", i%200, i%37, i)
	}
	// 批量写日志（每批一次 fsync）
	t0 := time.Now()
	for s := 0; s < n; s += 1000 {
		ops := make([]PFBaseOp, 0, 1000)
		for i := s; i < s+1000 && i < n; i++ {
			ops = append(ops, PFBaseOp{Path: path(i), Entry: pfTE(i)})
		}
		if err := b.Apply(ops); err != nil {
			t.Fatal(err)
		}
	}
	dApply := time.Since(t0)
	jfi, _ := os.Stat(pfJournalP(meta))

	// 日志回放（5 万行）
	t0 = time.Now()
	rj := pfMustLoadOK(t, meta)
	dReplay := time.Since(t0)
	if rj.Len() != n {
		t.Fatalf("回放后条目数 %d", rj.Len())
	}

	// 压实 = 写 5 万条的快照
	t0 = time.Now()
	if err := b.Compact(); err != nil {
		t.Fatal(err)
	}
	dCompact := time.Since(t0)
	sfi, _ := os.Stat(pfSnapP(meta))

	// 只读快照加载
	t0 = time.Now()
	rs := pfMustLoadOK(t, meta)
	dLoad := time.Since(t0)
	if rs.Len() != n {
		t.Fatalf("加载后条目数 %d", rs.Len())
	}
	if e, ok := rs.Get(path(31337)); !ok || e != pfTE(31337) {
		t.Fatal("抽查条目不对")
	}

	// 逐条 Set（每条一次 fsync）
	const singles = 100
	t0 = time.Now()
	for i := 0; i < singles; i++ {
		if err := rs.Set(path(n+i), pfTE(n+i)); err != nil {
			t.Fatal(err)
		}
	}
	dSingle := time.Since(t0)

	// 5 万条快照 + 100 行日志的加载
	t0 = time.Now()
	rm := pfMustLoadOK(t, meta)
	dLoadMixed := time.Since(t0)
	if rm.Len() != n+singles {
		t.Fatalf("条目数 %d", rm.Len())
	}

	// 遍历
	t0 = time.Now()
	cnt := 0
	rm.Range(func(string, PFBaseEntry) bool { cnt++; return true })
	dRange := time.Since(t0)

	t.Logf("5 万条规模（%s/%s）：", runtime.GOOS, runtime.GOARCH)
	t.Logf("  批量写日志 5 万条（50 次 fsync）: %v，日志 %.1f MB", dApply, float64(jfi.Size())/1e6)
	t.Logf("  回放 5 万行日志加载:           %v", dReplay)
	t.Logf("  压实（写 5 万条快照）:          %v，快照 %.1f MB", dCompact, float64(sfi.Size())/1e6)
	t.Logf("  加载 5 万条快照:               %v", dLoad)
	t.Logf("  逐条 Set 100 次（每次 fsync）:   %v（平均 %v/条）", dSingle, dSingle/singles)
	t.Logf("  快照 + 100 行日志加载:          %v", dLoadMixed)
	t.Logf("  Range 遍历 5 万条（含排序）:     %v", dRange)
}
