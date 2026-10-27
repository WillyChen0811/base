package projsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 项目文件同步 T2 的测试：带预算的哈希、哈希缓存。

// ---- 预算 ----

func pfstManyFiles(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		pfstWrite(t, root, fmt.Sprintf("d%02d/f%04d.txt", i%10, i), fmt.Sprintf("content-%d-%s", i, strings.Repeat("z", i%50)))
	}
}

// 极小预算：几乎全部「不明」，不报错。（1 纳秒的超时是异步触发的，第一个文件可能已经算完，所以只断言其余的都超时。）
func TestPFHash_TinyBudget(t *testing.T) {
	root := t.TempDir()
	pfstManyFiles(t, root, 300)
	slowOpen := func(p string) (io.ReadCloser, error) {
		time.Sleep(2 * time.Millisecond)
		return os.Open(p)
	}
	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{Open: slowOpen, HashBudget: time.Nanosecond})
	if err != nil {
		t.Fatalf("预算用完不是错误: %v", err)
	}
	s := res.Summary()
	if s.Files > 1 || s.Files+s.Unknown != 300 || res.HashStats.Hashed != s.Files || res.HashStats.NewUnknown != s.Unknown {
		t.Fatalf("Summary %+v HashStats %+v", s, res.HashStats)
	}
	for _, it := range res.Items {
		if it.Kind != PFKindFile {
			continue
		}
		if it.State == PFStateOK && it.Hash != "" {
			continue
		}
		if it.State != PFStateUnknown || it.Code != PFCodeHashTimeout || it.Hash != "" {
			t.Fatalf("%s: %+v", it.Path, it)
		}
	}
}

// 慢读 + 小预算：一部分算完、一部分「不明」；小文件先算；算完的进缓存、没算完的不进。
func TestPFHash_PartialBudget(t *testing.T) {
	root := t.TempDir()
	pfstManyFiles(t, root, 400)
	opens := &pfstOpens{}
	slowOpen := func(p string) (io.ReadCloser, error) {
		time.Sleep(5 * time.Millisecond)
		return opens.open(p)
	}
	cache := NewPFHashCache()
	res, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{Open: slowOpen, HashBudget: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("预算用完不是错误: %v", err)
	}
	hashed, timedOut := 0, 0
	for _, it := range res.Items {
		if it.Kind != PFKindFile {
			continue
		}
		switch {
		case it.State == PFStateOK && it.Hash != "":
			hashed++
		case it.State == PFStateUnknown && it.Code == PFCodeHashTimeout && it.Hash == "":
			timedOut++
		default:
			t.Fatalf("%s: 既不是有哈希也不是超时不明: %+v", it.Path, it)
		}
	}
	if hashed == 0 || timedOut == 0 || hashed+timedOut != 400 {
		t.Fatalf("应一部分算完、一部分超时: hashed=%d timedOut=%d", hashed, timedOut)
	}
	if res.HashStats.Hashed != hashed || res.HashStats.NewUnknown != timedOut {
		t.Errorf("HashStats %+v", res.HashStats)
	}
	if cache.Len() != hashed {
		t.Errorf("缓存条数 %d，应等于算完的 %d", cache.Len(), hashed)
	}
	// 小文件先算：已打开的文件大小不降
	var last int64 = -1
	for _, p := range opens.paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() < last {
			t.Fatalf("哈希顺序应按文件大小从小到大，%s 比前一个小", p)
		}
		last = fi.Size()
	}
	// 下一轮（不限预算）：已算完的命中缓存，只读剩下的
	opens2 := &pfstOpens{}
	res2, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{Open: opens2.open})
	if err != nil {
		t.Fatal(err)
	}
	if res2.HashStats.CacheHits != hashed || res2.HashStats.Hashed != timedOut || opens2.total() != timedOut {
		t.Errorf("第二轮 %+v，读了 %d 个；想要命中 %d、新算 %d", res2.HashStats, opens2.total(), hashed, timedOut)
	}
	if s := res2.Summary(); s.HashedFiles != 400 || s.Unknown != 0 {
		t.Errorf("第二轮应全部有哈希: %+v", s)
	}
}

// 调用方取消：返回 ctx 错误，没算的标 canceled。
func TestPFHash_CallerCancel(t *testing.T) {
	root := t.TempDir()
	pfstManyFiles(t, root, 20)
	res, err := PFScan(context.Background(), root, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = PFHashItems(ctx, root, res, nil, PFScanOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，得到 %v", err)
	}
	for _, it := range res.Items {
		if it.Kind == PFKindFile && (it.State != PFStateUnknown || it.Code != PFCodeCanceled) {
			t.Fatalf("%s: %+v", it.Path, it)
		}
	}
}

// ---- 读的过程中被改 ----

// mutatingReader 在第一次 Read 时先改文件（追加一字节），模拟「读的过程中被改」。
type pfstMutatingReader struct {
	f      *os.File
	path   string
	mutate func()
	did    bool
}

func (r *pfstMutatingReader) Read(b []byte) (int, error) {
	if !r.did {
		r.did = true
		r.mutate()
	}
	return r.f.Read(b)
}
func (r *pfstMutatingReader) Close() error { return r.f.Close() }

func TestPFHash_ChangedWhileReading(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "calm.txt", "calm")
	mover := pfstWrite(t, root, "moving.txt", "moving")
	cache := NewPFHashCache()
	open := func(p string) (io.ReadCloser, error) {
		f, err := os.Open(p)
		if err != nil || !strings.HasSuffix(p, "moving.txt") {
			return f, err
		}
		return &pfstMutatingReader{f: f, path: p, mutate: func() {
			w, err := os.OpenFile(mover, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Error(err)
				return
			}
			w.WriteString("!")
			w.Close()
		}}, nil
	}
	res, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{Open: open})
	if err != nil {
		t.Fatal(err)
	}
	it := pfstWantState(t, res, "moving.txt", PFStateUnknown, PFCodeChangedWhileRead)
	if it.Hash != "" {
		t.Errorf("读的过程中被改的不能有哈希")
	}
	pfstWantState(t, res, "calm.txt", PFStateOK, "")
	if cache.Len() != 1 {
		t.Errorf("只有 calm.txt 进缓存，实际 %d 条", cache.Len())
	}
}

// 读前后两次 stat 不同（只有 ctime 变，文件内容和 mtime 没动，例如被 chmod）也判不明。
func TestPFHash_StatDiffersAfterRead(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.txt", "aaa")
	pfstWrite(t, root, "b.txt", "bbb")
	var calls atomic.Int32
	st := pfstStat(root, func(rel string, st *PFStat) error {
		if rel == "a.txt" && calls.Add(1) >= 3 { // 1: 扫描；2: 读前；3: 读后
			st.CtimeNs += 7
		}
		return nil
	})
	cache := NewPFHashCache()
	res, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{Stat: st})
	if err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "a.txt", PFStateUnknown, PFCodeChangedWhileRead)
	pfstWantState(t, res, "b.txt", PFStateOK, "")
	if cache.Len() != 1 {
		t.Errorf("缓存条数 %d", cache.Len())
	}
}

// 读的过程中文件被截短：读到的字节数不等于读前 size → 不明（即便 stat 恰好没变）。
func TestPFHash_ShortRead(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "a.txt", "0123456789")
	open := func(p string) (io.ReadCloser, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{io.LimitReader(f, 4), f}, nil
	}
	res, err := PFScanAndHash(context.Background(), root, nil, PFScanOptions{Open: open})
	if err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "a.txt", PFStateUnknown, PFCodeChangedWhileRead)
}

// 文件扫描后被改名/删除：不明（vanished），不是删除。
func TestPFHash_VanishedAfterScan(t *testing.T) {
	root := t.TempDir()
	p := pfstWrite(t, root, "a.txt", "aaa")
	res, err := PFScan(context.Background(), root, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := PFHashItems(context.Background(), root, res, nil, PFScanOptions{}); err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "a.txt", PFStateUnknown, PFCodeVanished)
}

// 文件改了内容但缓存命中条件不满足 → 重算；缓存里最新的是新值。
func TestPFHash_ModifiedFileRehashed(t *testing.T) {
	root := t.TempDir()
	p := pfstWrite(t, root, "a.txt", "old")
	cache := NewPFHashCache()
	res, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pfstItem(t, res, "a.txt").Hash != pfstSHA("old") {
		t.Fatal("首次哈希不对")
	}
	if err := os.WriteFile(p, []byte("newer content"), 0o644); err != nil {
		t.Fatal(err)
	}
	pfstAge(t, p)
	res, err = PFScanAndHash(context.Background(), root, cache, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	it := pfstItem(t, res, "a.txt")
	if it.Hash != pfstSHA("newer content") || it.FromCache || res.HashStats.Hashed != 1 {
		t.Errorf("改过的文件应重算: %+v %+v", it, res.HashStats)
	}
	if h, ok := cache.Lookup("a.txt", it.Stat); !ok || h != pfstSHA("newer content") {
		t.Errorf("缓存应更新成新值")
	}
}

// 未静止的文件不算哈希、不进缓存。
func TestPFHash_RecentFileNotCached(t *testing.T) {
	root := t.TempDir()
	pfstWrite(t, root, "old.txt", "old")
	if err := os.WriteFile(root+"/recent.txt", []byte("recent"), 0o644); err != nil { // mtime = 刚刚
		t.Fatal(err)
	}
	cache := NewPFHashCache()
	res, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pfstWantState(t, res, "recent.txt", PFStateUnknown, PFCodeNotQuiet)
	if cache.Len() != 1 {
		t.Errorf("只有静止的文件进缓存，实际 %d 条", cache.Len())
	}
}

// ---- 缓存 ----

func pfstFileStat() PFStat {
	return PFStat{Kind: PFKindFile, Size: 100, MtimeNs: time.Unix(1_000, 0).UnixNano(), CtimeNs: 5, Ino: 42, Dev: 1, Flags: 0}
}

func TestPFCache_LookupConditions(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	sha := pfstSHA("x")
	c := NewPFHashCache()
	st := pfstFileStat()
	if !c.Put("a/b.txt", st, sha, now, 10) {
		t.Fatal("Put 应成功")
	}
	if h, ok := c.Lookup("a/b.txt", st); !ok || h != sha {
		t.Fatalf("原样应命中")
	}
	cases := []struct {
		name string
		mod  func(*PFStat)
		hit  bool
	}{
		{"size 变", func(s *PFStat) { s.Size++ }, false},
		{"mtime 变", func(s *PFStat) { s.MtimeNs++ }, false},
		{"ino 变", func(s *PFStat) { s.Ino++ }, false},
		{"ctime 变（不看）", func(s *PFStat) { s.CtimeNs += 1e9 }, true},
		{"flags 变（不看，iCloud 驱逐）", func(s *PFStat) { s.Flags |= PFSFDataless }, true},
		{"dev 变（不看）", func(s *PFStat) { s.Dev++ }, true},
		{"mode 变（不看）", func(s *PFStat) { s.Mode = 0o600 }, true},
		{"驱逐：ctime 和 flags 同时变", func(s *PFStat) { s.CtimeNs += 1e9; s.Flags |= PFSFDataless }, true},
		{"变成目录", func(s *PFStat) { s.Kind = PFKindDir }, false},
	}
	for _, tc := range cases {
		s := st
		tc.mod(&s)
		if _, ok := c.Lookup("a/b.txt", s); ok != tc.hit {
			t.Errorf("%s: hit=%v 想要 %v", tc.name, ok, tc.hit)
		}
	}
	if _, ok := c.Lookup("other.txt", st); ok {
		t.Errorf("别的路径不该命中")
	}
	var nilCache *PFHashCache
	if _, ok := nilCache.Lookup("a/b.txt", st); ok || nilCache.Len() != 0 || nilCache.Put("x", st, sha, now, 10) {
		t.Errorf("nil 缓存恒不命中、不记")
	}
}

func TestPFCache_PutRules(t *testing.T) {
	sha := pfstSHA("x")
	st := pfstFileStat() // mtime = 1000 秒
	c := NewPFHashCache()
	mtime := time.Unix(0, st.MtimeNs)
	if c.Put("a", st, sha, mtime.Add(9*time.Second), 10) {
		t.Errorf("mtime 距今 9 秒（<10）不该进缓存")
	}
	if !c.Put("a", st, sha, mtime.Add(10*time.Second), 10) {
		t.Errorf("mtime 距今 10 秒应进缓存")
	}
	if c.Put("fut", st, sha, mtime.Add(-time.Hour), 10) {
		t.Errorf("mtime 在未来不该进缓存")
	}
	now := mtime.Add(time.Hour)
	bad := []struct {
		name, path, sha string
		st              PFStat
	}{
		{"空路径", "", sha, st},
		{"绝对路径", "/a", sha, st},
		{"含 ..", "a/../b", sha, st},
		{"空段", "a//b", sha, st},
		{"非 UTF-8", "a/\xff\xfe.txt", sha, st},
		{"哈希太短", "ok1", "abc", st},
		{"哈希大写", "ok2", strings.ToUpper(sha), st},
		{"目录", "ok3", sha, PFStat{Kind: PFKindDir, MtimeNs: st.MtimeNs}},
	}
	for _, b := range bad {
		if c.Put(b.path, b.st, b.sha, now, 10) {
			t.Errorf("%s: 不该记", b.name)
		}
	}
	if c.Len() != 1 {
		t.Errorf("缓存应只有 a 一条，实际 %d", c.Len())
	}
	// quietSeconds <= 0 取缺省 10
	if c.Put("def", st, sha, mtime.Add(5*time.Second), 0) {
		t.Errorf("quiet=0 应取缺省 10")
	}
}

func TestPFCache_Prune(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := NewPFHashCache()
	for _, p := range []string{"keep/a", "keep/b", "drop/c"} {
		c.Put(p, pfstFileStat(), pfstSHA(p), now, 10)
	}
	if n := c.Prune(func(p string) bool { return strings.HasPrefix(p, "keep/") }); n != 1 || c.Len() != 2 {
		t.Errorf("Prune 删了 %d，剩 %d", n, c.Len())
	}
	c.Delete("keep/a")
	if c.Len() != 1 {
		t.Errorf("Delete 后应剩 1")
	}
}

func TestPFCache_RoundTrip(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := NewPFHashCache()
	st := pfstFileStat()
	c.Put("b/中文 文件.docx", st, pfstSHA("1"), now, 10)
	st2 := st
	st2.Size, st2.Ino = 0, math.MaxUint64 // 大 inode 不能被 JSON 弄丢精度；空文件
	c.Put("a.txt", st2, pfstSHA("2"), now, 10)
	st3 := st
	st3.MtimeNs = -5_000_000_000 // 1970 年以前的 mtime
	c.Put("c.txt", st3, pfstSHA("3"), now, 10)

	b1, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := c.Marshal()
	if string(b1) != string(b2) {
		t.Errorf("输出应确定")
	}
	back, err := PFParseHashCache(b1)
	if err != nil {
		t.Fatalf("往返失败: %v\n%s", err, b1)
	}
	if back.Len() != 3 {
		t.Fatalf("往返后 %d 条", back.Len())
	}
	if h, ok := back.Lookup("a.txt", st2); !ok || h != pfstSHA("2") {
		t.Errorf("大 inode 往返后应命中")
	}
	if h, ok := back.Lookup("b/中文 文件.docx", st); !ok || h != pfstSHA("1") {
		t.Errorf("中文路径往返后应命中")
	}
	if h, ok := back.Lookup("c.txt", st3); !ok || h != pfstSHA("3") {
		t.Errorf("负 mtime 往返后应命中")
	}
	b3, _ := back.Marshal()
	if string(b1) != string(b3) {
		t.Errorf("往返后再序列化应一致")
	}
	// 空缓存也能往返
	e, err := NewPFHashCache().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if back, err := PFParseHashCache(e); err != nil || back.Len() != 0 {
		t.Errorf("空缓存往返: %v", err)
	}
}

// 坏数据一律报错，不静默当成空缓存。
func TestPFCache_ParseBad(t *testing.T) {
	good := `{"version":1,"entries":{"a.txt":{"size":1,"mtime_ns":2,"ino":3,"sha256":"` + pfstSHA("x") + `"}}}`
	if _, err := PFParseHashCache([]byte(good)); err != nil {
		t.Fatalf("基准样本应能解析: %v", err)
	}
	sha := pfstSHA("x")
	entry := func(body string) string {
		return `{"version":1,"entries":{"a.txt":` + body + `}}`
	}
	bad := map[string]string{
		"空文件":            ``,
		"只有空白":           "  \n",
		"不是 JSON":        `hello`,
		"截断":             good[:len(good)-7],
		"null":           `null`,
		"数组":             `[]`,
		"缺版本":            `{"entries":{}}`,
		"版本不认识":          `{"version":2,"entries":{}}`,
		"版本 0":           `{"version":0,"entries":{}}`,
		"缺 entries":      `{"version":1}`,
		"entries 为 null": `{"version":1,"entries":null}`,
		"多余字段":           `{"version":1,"entries":{},"extra":1}`,
		"后面有多余内容":        good + ` trailing`,
		"后面又一个对象":        good + good,
		"记录缺 size":       entry(`{"mtime_ns":2,"ino":3,"sha256":"` + sha + `"}`),
		"记录缺 mtime":      entry(`{"size":1,"ino":3,"sha256":"` + sha + `"}`),
		"记录缺 ino":        entry(`{"size":1,"mtime_ns":2,"sha256":"` + sha + `"}`),
		"记录缺 sha":        entry(`{"size":1,"mtime_ns":2,"ino":3}`),
		"记录为 null":       entry(`null`),
		"记录多余字段":         entry(`{"size":1,"mtime_ns":2,"ino":3,"sha256":"` + sha + `","ctime_ns":9}`),
		"size 为负":        entry(`{"size":-1,"mtime_ns":2,"ino":3,"sha256":"` + sha + `"}`),
		"size 不是数字":      entry(`{"size":"1","mtime_ns":2,"ino":3,"sha256":"` + sha + `"}`),
		"ino 为负":         entry(`{"size":1,"mtime_ns":2,"ino":-3,"sha256":"` + sha + `"}`),
		"哈希太短":           entry(`{"size":1,"mtime_ns":2,"ino":3,"sha256":"abcd"}`),
		"哈希大写":           entry(`{"size":1,"mtime_ns":2,"ino":3,"sha256":"` + strings.ToUpper(sha) + `"}`),
		"哈希非十六进制":        entry(`{"size":1,"mtime_ns":2,"ino":3,"sha256":"` + strings.Repeat("g", 64) + `"}`),
		"路径含 ..":         `{"version":1,"entries":{"../a":{"size":1,"mtime_ns":2,"ino":3,"sha256":"` + sha + `"}}}`,
		"路径为空":           `{"version":1,"entries":{"":{"size":1,"mtime_ns":2,"ino":3,"sha256":"` + sha + `"}}}`,
		"路径绝对":           `{"version":1,"entries":{"/a":{"size":1,"mtime_ns":2,"ino":3,"sha256":"` + sha + `"}}}`,
	}
	for name, in := range bad {
		c, err := PFParseHashCache([]byte(in))
		if err == nil {
			t.Errorf("%s: 应报错", name)
		} else if c != nil {
			t.Errorf("%s: 报错时不该同时返回缓存", name)
		}
	}
}

// Marshal 出来的东西确实是合法 JSON 且字段名符合约定（别的任务按这个读写文件）。
func TestPFCache_JSONShape(t *testing.T) {
	c := NewPFHashCache()
	c.Put("x/y.txt", pfstFileStat(), pfstSHA("q"), time.Unix(1_000_000, 0), 10)
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["version"].(float64) != 1 {
		t.Errorf("version: %v", m["version"])
	}
	e := m["entries"].(map[string]any)["x/y.txt"].(map[string]any)
	for _, k := range []string{"size", "mtime_ns", "ino", "sha256"} {
		if _, ok := e[k]; !ok {
			t.Errorf("缺字段 %s: %s", k, b)
		}
	}
	if len(e) != 4 {
		t.Errorf("字段数 %d，应只有 size、mtime_ns、ino、sha256: %s", len(e), b)
	}
}

// 完整往返：扫描哈希 → 序列化 → 反序列化 → 再扫描，100% 命中、不读文件。
func TestPFCache_PersistAndReuse(t *testing.T) {
	root := t.TempDir()
	pfstManyFiles(t, root, 50)
	cache := NewPFHashCache()
	res1, err := PFScanAndHash(context.Background(), root, cache, PFScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := cache.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := PFParseHashCache(b)
	if err != nil {
		t.Fatal(err)
	}
	opens := &pfstOpens{}
	res2, err := PFScanAndHash(context.Background(), root, loaded, PFScanOptions{Open: opens.open})
	if err != nil {
		t.Fatal(err)
	}
	if res2.HashStats.CacheHits != 50 || res2.HashStats.Hashed != 0 || opens.total() != 0 {
		t.Errorf("HashStats %+v，读了 %d 个", res2.HashStats, opens.total())
	}
	for i, it := range res1.Items {
		if it.Hash != res2.Items[i].Hash {
			t.Errorf("%s: 两次哈希不同", it.Path)
		}
	}
}
