package projsync

// T9a 测试：单元一轮同步 PFRoundRunUnit 的单机全流程模拟（技术设计 §9.3 的 S1–S16 在单机上的对应）。
//
// 做法同 T8 的测试：「Ubuntu」的根和 meta 在临时目录里，假 RemoteRunner 在进程内直接调 xxyi-msg 的处理函数（PFRemoteServer）；
// 传输器的远端文件系统用本机文件系统（PFLocalFS）；Mac 的同步目录和 meta 也在临时目录。
// 时钟是固定的假时钟（2026-10-27 12:00），文件的 mtime 都设在过去，所以默认都已静止。
// 不依赖 Linux 专有系统调用，Studio（darwin）上能跑。
//
// 每个场景末尾都再跑一轮，断言「没有新动作」（两边已一致时一轮是空的）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── 夹具 ────────────────────────────────────────────────

// pfrdSerial 把假 RemoteRunner 串行化：传输器的几个任务会同时调 Ubuntu 的核验，T8 夹具里的 pfrtRunner 自己不加锁
// （真实的 SSH 连接池是并发安全的）。
type pfrdSerial struct {
	mu    sync.Mutex
	inner RemoteRunner
}

func (s *pfrdSerial) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Run(ctx, cmd, stdin)
}

type pfrdFix struct {
	t      *testing.T
	ub     *pfrtFix
	runner RemoteRunner // 串行化之后的 Ubuntu 连接
	base   string
	sync   string
	meta   string
	root   string // Mac 侧单元根
	unit   PFUnitRecord
	tr     *PFTransfer
	net    string
	limits PFRoundLimits
	prio   PFRoundPriority
	paths  []string
	n      int
	last   *PFRoundResult
}

func newPFRDFix(t *testing.T) *pfrdFix {
	t.Helper()
	ub := newPFRTFix(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ub.cl.Runner = &pfrdSerial{inner: ub.run}
	f := &pfrdFix{t: t, ub: ub, runner: ub.cl.Runner, base: base, sync: filepath.Join(base, "XXYI-sync"), meta: filepath.Join(base, "macmeta"), net: PFRemoteNetGood}
	f.root = filepath.Join(f.sync, filepath.FromSlash(pfrtRel))
	for _, d := range []string{f.sync, f.meta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.unit = PFUnitRecord{Code: pfrtCode, Rel: pfrtRel, State: PFUnitSyncing,
		LinkPath: filepath.Join(base, "Projects", filepath.FromSlash(pfrtRel)), SyncPath: f.root, StartedAt: ub.now}
	f.tr, err = NewPFTransfer(PFTransferConfig{
		Local: PFLocalFS{}, Remote: PFLocalFS{},
		LocalStaging: PFRoundMacStagingDir(f.meta), RemoteStaging: PFRemoteStagingDir(ub.meta),
		VerifyUp: ub.cl.TargetVerify(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.tr.Close)
	return f
}

func (f *pfrdFix) input() PFRoundInput {
	f.n++
	return PFRoundInput{
		Unit: f.unit, MetaDir: f.meta, Remote: f.ub.cl, Transfer: f.tr,
		Net: f.net, Limits: f.limits, Priority: f.prio, PriorityPaths: f.paths,
		Round: fmt.Sprintf("r%d", f.n), Now: func() time.Time { return f.ub.now },
	}
}

// run 跑一轮。
func (f *pfrdFix) run() *PFRoundResult {
	f.t.Helper()
	res, err := PFRoundRunUnit(context.Background(), f.input())
	if err != nil {
		f.t.Fatalf("PFRoundRunUnit 输入不合格: %v", err)
	}
	f.last = res
	return res
}

func pfrdDump(r *PFRoundResult) string {
	b, _ := json.MarshalIndent(map[string]any{
		"outcome": r.Outcome, "reason": r.Reason, "planned": r.PlannedActions, "gates": r.Gates, "done": r.Done,
		"skipped": r.Skipped, "deferred": r.Deferred, "errors": r.Errors, "notices": r.Notices, "conflicts": r.Conflicts,
	}, "", " ")
	return string(b)
}

// mustDone：一轮正常跑完且没有出错。
func (f *pfrdFix) mustDone(r *PFRoundResult) *PFRoundResult {
	f.t.Helper()
	if r.Outcome != PFRoundDone || len(r.Errors) > 0 || r.Uncertain {
		f.t.Fatalf("应当正常跑完:\n%s", pfrdDump(r))
	}
	return r
}

// settled 再跑一轮，断言没有新动作、没有传输。
func (f *pfrdFix) settled() *PFRoundResult {
	f.t.Helper()
	r := f.run()
	f.mustDone(r)
	if r.PlannedActions != 0 || len(r.Done) != 0 || r.BytesUp != 0 || r.BytesDown != 0 {
		f.t.Fatalf("两边已一致，再跑一轮应当什么都不做:\n%s\n上传 %d 下载 %d", pfrdDump(r), r.BytesUp, r.BytesDown)
	}
	return r
}

func (f *pfrdFix) macAbs(rel string) string { return filepath.Join(f.root, filepath.FromSlash(rel)) }

func pfrdOld(h int) time.Time { return pfrtOld.Add(time.Duration(h) * time.Hour) }

func pfrdPutAt(t *testing.T, root, rel, content string, mt time.Time) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func (f *pfrdFix) macPut(rel, content string, mt time.Time) { f.t.Helper(); pfrdPutAt(f.t, f.root, rel, content, mt) }
func (f *pfrdFix) ubPut(rel, content string, mt time.Time) {
	f.t.Helper()
	pfrdPutAt(f.t, f.ub.abs(""), rel, content, mt)
}

func (f *pfrdFix) macRead(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(f.macAbs(rel))
	if err != nil {
		f.t.Fatalf("读 Mac 的 %s 失败: %v", rel, err)
	}
	return string(b)
}

func (f *pfrdFix) macHas(rel string) bool {
	_, err := os.Lstat(f.macAbs(rel))
	return err == nil
}

// copyTree 把 testdata 里的夹具拷到 dst，mtime 统一设成过去（git 检出的 mtime 是现在，会被当成还在写）。
func pfrdCopyFixture(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return err
		}
		return os.Chtimes(target, pfrtOld, pfrtOld)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// tree 列出目录树：相对路径 → "目录" 或文件的 sha256；排除项（.DS_Store 等）不算，和同步的口径一致。
func pfrdTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		parent := ""
		if i := strings.LastIndexByte(rel, '/'); i >= 0 {
			parent = rel[:i]
		}
		if _, ex := PFExclude(d.Name(), d.IsDir(), parent); ex {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			out[rel] = "目录"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(h[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// diff 比对两边的目录树（等价于 synctest/compare.py 的逐文件比对：路径按字节、内容用 sha256）。
func (f *pfrdFix) diff() []string {
	f.t.Helper()
	m, u := pfrdTree(f.t, f.root), pfrdTree(f.t, f.ub.abs(""))
	var out []string
	for p, h := range m {
		if uh, ok := u[p]; !ok {
			out = append(out, "只有 Mac 有: "+p)
		} else if uh != h {
			out = append(out, "内容不同: "+p)
		}
	}
	for p := range u {
		if _, ok := m[p]; !ok {
			out = append(out, "只有 Ubuntu 有: "+p)
		}
	}
	sort.Strings(out)
	return out
}

func (f *pfrdFix) mustSame() {
	f.t.Helper()
	if d := f.diff(); len(d) > 0 {
		f.t.Fatalf("两边不一致:\n%s", strings.Join(d, "\n"))
	}
}

// loadBase 从磁盘读 base。
func (f *pfrdFix) loadBase() *PFBase {
	f.t.Helper()
	lr, err := PFBaseLoad(f.meta, pfrtCode)
	if err != nil || lr.Status != PFBaseOK {
		f.t.Fatalf("读 base: %v status=%v %s", err, lr.Status, lr.Reason)
	}
	return lr.Base
}

// recycled 返回某一边同步回收里所有文件的路径（相对回收区，不含 index.jsonl）。
func pfrdRecycled(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != "index.jsonl" {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func pfrdIndex(t *testing.T, dir string) []PFRecycleEntry {
	t.Helper()
	rc, err := NewPFRecycle(dir, PFRecycleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	days, _ := rc.Days()
	var out []PFRecycleEntry
	for _, d := range days {
		es, _ := rc.ReadIndex(d)
		out = append(out, es...)
	}
	return out
}

func pfrdHasRecycled(files []string, name string) bool {
	for _, p := range files {
		if filepath.Base(p) == name {
			return true
		}
	}
	return false
}

// 首次同步的夹具：testdata 里的小目录树 + 空目录 + 3MB 的大一点的文件 + 会被排除的系统文件。
func (f *pfrdFix) seedMac() {
	f.t.Helper()
	pfrdCopyFixture(f.t, filepath.Join("testdata", "pfround", "s1"), f.root)
	if err := os.MkdirAll(f.macAbs("空目录"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.macPut(".DS_Store", "finder", pfrtOld)
	f.macPut("报告/.DS_Store", "finder", pfrtOld)
	f.macPut("大文件.bin", strings.Repeat("0123456789abcdef", 3<<16), pfrtOld) // 3MB
}

// ── 暂缓的判定 ──────────────────────────────────────────

func TestPFRoundAdmit(t *testing.T) {
	lim := PFRoundLimits{MaxExpensive: 20 * time.Second, MaxUser: 60 * time.Second}
	cases := []struct {
		name   string
		net    string
		wanted bool
		est    float64
		ok     bool
		limit  float64
	}{
		{"网好全放行", PFRemoteNetGood, false, 3600, true, 0},
		{"网好用户要的也放行", PFRemoteNetGood, true, 3600, true, 0},
		{"热点 20 秒内放行", PFRemoteNetExpensive, false, 20, true, 20},
		{"热点超过 20 秒暂缓", PFRemoteNetExpensive, false, 20.5, false, 20},
		{"未知按热点：超过暂缓", PFRemoteNetUnknown, false, 30, false, 20},
		{"空串按未知", "", false, 10, true, 20},
		{"热点用户要的 60 秒内放行", PFRemoteNetExpensive, true, 59, true, 60},
		{"热点用户要的超过 60 秒暂缓", PFRemoteNetExpensive, true, 61, false, 60},
		{"断网全暂缓", PFRemoteNetOffline, true, 0, false, 0},
	}
	for _, c := range cases {
		ok, why, limit := pfRoundAdmit(c.net, lim, c.wanted, c.est)
		if ok != c.ok || limit != c.limit || (!ok && why == "") {
			t.Errorf("%s: ok=%v why=%q limit=%v", c.name, ok, why, limit)
		}
	}
	// 网好但设了上限
	if ok, _, _ := pfRoundAdmit(PFRemoteNetGood, PFRoundLimits{MaxGood: 5 * time.Second}, false, 6); ok {
		t.Error("网好设了 5 秒上限，6 秒应暂缓")
	}
}

// ── S1 首次 ─────────────────────────────────────────────

func TestPFRoundS1First(t *testing.T) {
	f := newPFRDFix(t)
	f.seedMac()

	r := f.mustDone(f.run())
	if !r.FirstTime || r.PlannedActions == 0 {
		t.Fatalf("首次应按首次规则:\n%s", pfrdDump(r))
	}
	f.mustSame()
	// 排除项没有传过去
	if _, err := os.Lstat(f.ub.abs(".DS_Store")); err == nil {
		t.Fatal(".DS_Store 不该同步")
	}
	if _, err := os.Lstat(f.ub.abs("报告/.DS_Store")); err == nil {
		t.Fatal("报告/.DS_Store 不该同步")
	}
	if !f.last.StatusWritten || r.BytesUp < 3<<20 || r.BytesDown != 0 {
		t.Fatalf("上传 %d 下载 %d 状态写了=%v", r.BytesUp, r.BytesDown, r.StatusWritten)
	}
	if _, err := os.Lstat(f.ub.abs("空目录")); err != nil {
		t.Fatalf("空目录应建到 Ubuntu: %v", err)
	}
	if fi, err := os.Stat(f.ub.abs("空文件.txt")); err != nil || fi.Size() != 0 {
		t.Fatalf("空文件应传过去: %v", err)
	}
	// mtime 照源设置
	if st, err := PFLstat(f.ub.abs("报告/初稿.docx")); err != nil || st.MtimeNs != pfrtOld.UnixNano() {
		t.Fatalf("Ubuntu 上的 mtime 应照源: %v %+v", err, st)
	}

	// base：每个文件和目录都进去了，两边 ino 都填了真实值，单元目录 dev+ino 两边都记了
	b := f.loadBase()
	want := pfrdTree(t, f.root)
	if b.Len() != len(want) {
		t.Fatalf("base 有 %d 条，两边共有 %d 个文件和目录", b.Len(), len(want))
	}
	for p, h := range want {
		e, ok := b.Get(p)
		if !ok {
			t.Fatalf("base 里没有 %s", p)
		}
		if h != "目录" && (e.T != PFBaseTypeFile || e.SHA256 != h) {
			t.Fatalf("base 里 %s 的内容不对: %+v", p, e)
		}
		if h != "目录" && (e.Mac.Ino == 0 || e.Ub.Ino == 0 || e.Mac.MtimeNs != pfrtOld.UnixNano()) {
			t.Fatalf("base 里 %s 的两边 stat 应是真实值: %+v", p, e)
		}
	}
	m := b.Meta()
	if m.MacDirID.Ino == 0 || m.UbuntuDirID.Ino == 0 || m.MacRel != pfrtRel || m.UbuntuRel != pfrtRel {
		t.Fatalf("单元级元数据: %+v", m)
	}

	// 状态文件：Ubuntu 上能读到，没有待传
	st, err := PFRemoteReadStatus(f.ub.meta, pfrtCode)
	if err != nil || st.RoundAt == "" || st.MacSeenAt == "" || len(st.PendingToUbuntu) != 0 || len(st.PendingToMac) != 0 || st.UbuntuRel != pfrtRel {
		t.Fatalf("Ubuntu 上的状态文件: %v %+v", err, st)
	}
	// Mac 本机状态文件
	ms, err := PFRoundLoadState(f.meta, pfrtCode)
	if err != nil || ms == nil || ms.Outcome != PFRoundDone {
		t.Fatalf("Mac 本机状态: %v %+v", err, ms)
	}
	// 哈希缓存：写回了，第二轮不用重读
	raw, err := os.ReadFile(filepath.Join(f.meta, PFRemoteCacheName, pfrtCode+".json"))
	if err != nil {
		t.Fatal(err)
	}
	cache, err := PFParseHashCache(raw)
	if err != nil || cache.Len() != 9 {
		t.Fatalf("哈希缓存: %v len=%d", err, cache.Len())
	}

	// 再跑一轮：什么都不做，且这是真正的「已对齐」
	r2 := f.settled()
	if r2.FirstTime || !r2.Aligned() {
		t.Fatalf("第二轮不该再按首次规则: first=%v aligned=%v", r2.FirstTime, r2.Aligned())
	}
}

// 两边各有一份一样的内容（首次）：只写 base，不传。
func TestPFRoundFirstSameBothSides(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("a.txt", "一样\n", pfrtOld)
	f.ubPut("a.txt", "一样\n", pfrtOld)
	r := f.mustDone(f.run())
	if r.BytesUp != 0 || r.BytesDown != 0 || len(r.Done) == 0 {
		t.Fatalf("一样的内容不该传:\n%s", pfrdDump(r))
	}
	f.mustSame()
	f.settled()
}

// 两边各有一份同名但不同的内容（首次）：冲突，两份都留。
func TestPFRoundFirstDifferentBothSides(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("a.txt", "Mac 的\n", pfrdOld(1))
	f.ubPut("a.txt", "Ubuntu 的\n", pfrdOld(2))
	r := f.mustDone(f.run())
	if len(r.NewConflicts) != 1 {
		t.Fatalf("应出一个冲突:\n%s", pfrdDump(r))
	}
	f.mustSame()
	if f.macRead("a.txt") != "Ubuntu 的\n" {
		t.Fatal("Ubuntu 较新，应占原名")
	}
	f.settled()
}

// ── S2 双向修改 ─────────────────────────────────────────

func TestPFRoundS2TwoWay(t *testing.T) {
	f := newPFRDFix(t)
	f.seedMac()
	f.mustDone(f.run())

	f.macPut("说明.txt", "Mac 改过的说明\n", pfrdOld(1))
	f.ubPut("报告/初稿.docx", "Ubuntu 改过的初稿\n", pfrdOld(1))
	f.macPut("新增/mac.txt", "Mac 新增\n", pfrdOld(1))
	f.ubPut("新增/ub.txt", "Ubuntu 新增\n", pfrdOld(1))

	r := f.mustDone(f.run())
	f.mustSame()
	if f.ub.read("说明.txt") != "Mac 改过的说明\n" || f.macRead("报告/初稿.docx") != "Ubuntu 改过的初稿\n" {
		t.Fatal("改动没有传到对方")
	}
	if f.macRead("新增/ub.txt") != "Ubuntu 新增\n" || f.ub.read("新增/mac.txt") != "Mac 新增\n" {
		t.Fatal("新文件没有传到对方")
	}
	if r.BytesUp == 0 || r.BytesDown == 0 {
		t.Fatalf("两个方向都该传了: 上传 %d 下载 %d", r.BytesUp, r.BytesDown)
	}
	// 被覆盖的旧版本都进了对应那一边的同步回收
	if !pfrdHasRecycled(pfrdRecycled(t, PFRemoteRecycleDir(f.ub.meta)), "说明.txt") {
		t.Fatal("Ubuntu 被覆盖的旧说明.txt 应在回收里")
	}
	if !pfrdHasRecycled(pfrdRecycled(t, PFRoundMacRecycleDir(f.meta)), "初稿.docx") {
		t.Fatal("Mac 被覆盖的旧初稿.docx 应在回收里")
	}
	for _, e := range pfrdIndex(t, PFRemoteRecycleDir(f.ub.meta)) {
		if e.Reason != string(PFReasonOverwrite) {
			t.Fatalf("回收原因应是覆盖: %+v", e)
		}
	}
	// base 记的是新内容
	b := f.loadBase()
	if e, _ := b.Get("说明.txt"); e.SHA256 != pfrtSHA("Mac 改过的说明\n") {
		t.Fatalf("base 里的说明.txt: %+v", e)
	}
	f.settled()
}

// ── S3 冲突 ─────────────────────────────────────────────

func TestPFRoundS3Conflict(t *testing.T) {
	cases := []struct {
		name         string
		macH, ubH    int
		winner, lose string // 谁占原名；落败的机器名
		winText      string
		loseText     string
	}{
		{"Ubuntu 较新", 1, 2, "ubuntu", "Mac", "Ubuntu版\n", "Mac版\n"},
		{"Mac 较新", 2, 1, "mac", "Ubuntu", "Mac版\n", "Ubuntu版\n"},
		{"相差不到 2 秒 Mac 占原名", 1, 1, "mac", "Ubuntu", "Mac版\n", "Ubuntu版\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPFRDFix(t)
			f.macPut("冲突.txt", "原始\n", pfrtOld)
			f.macPut("别的.txt", "别的\n", pfrtOld)
			f.mustDone(f.run())
			f.settled()

			f.macPut("冲突.txt", "Mac版\n", pfrdOld(c.macH))
			f.ubPut("冲突.txt", "Ubuntu版\n", pfrdOld(c.ubH))
			if c.macH == c.ubH { // 同一时刻
				f.ubPut("冲突.txt", "Ubuntu版\n", pfrdOld(c.macH).Add(time.Second))
			}
			r := f.mustDone(f.run())
			if len(r.NewConflicts) != 1 || len(r.Conflicts) != 1 {
				t.Fatalf("应出一个冲突:\n%s", pfrdDump(r))
			}
			cf := r.Conflicts[0]
			loserM := pfrdOld(c.macH)
			if c.lose == "Ubuntu" {
				loserM = pfrdOld(c.ubH)
				if c.macH == c.ubH {
					loserM = loserM.Add(time.Second)
				}
			}
			wantName := fmt.Sprintf("冲突（冲突-%s-%s）.txt", c.lose, loserM.In(time.Local).Format("0102-1504"))
			if cf.Path != "冲突.txt" || cf.ConflictPath != wantName || cf.Winner != c.winner {
				t.Fatalf("冲突记录 %+v，应为 冲突.txt / %s / %s", cf, wantName, c.winner)
			}
			// 两边都是：原名 = 胜出版本，冲突名 = 落败版本
			for side, read := range map[string]func(string) string{"Mac": f.macRead, "Ubuntu": f.ub.read} {
				if got := read("冲突.txt"); got != c.winText {
					t.Fatalf("%s 的原名内容 %q，应为 %q", side, got, c.winText)
				}
				if got := read(wantName); got != c.loseText {
					t.Fatalf("%s 的冲突副本内容 %q，应为 %q", side, got, c.loseText)
				}
			}
			f.mustSame()
			// base：原名记胜出，冲突名记落败，两边 stat 都是真实值
			b := f.loadBase()
			e1, ok1 := b.Get("冲突.txt")
			e2, ok2 := b.Get(wantName)
			if !ok1 || !ok2 || e1.SHA256 != pfrtSHA(c.winText) || e2.SHA256 != pfrtSHA(c.loseText) {
				t.Fatalf("base: %+v %+v", e1, e2)
			}
			for _, e := range []PFBaseEntry{e1, e2} {
				if e.Mac.Ino == 0 || e.Ub.Ino == 0 {
					t.Fatalf("两边的 stat 应是真实值: %+v", e)
				}
			}
			// 冲突里被改名的那份没有进回收（两份都留）
			if n := len(pfrdRecycled(t, PFRoundMacRecycleDir(f.meta))) + len(pfrdRecycled(t, PFRemoteRecycleDir(f.ub.meta))); n != 0 {
				t.Fatalf("冲突不该产生回收文件，却有 %d 个", n)
			}
			// 再跑一轮：没有新动作，冲突仍然报着（还没解决）
			r2 := f.settled()
			if len(r2.Conflicts) != 1 || r2.Aligned() {
				t.Fatalf("冲突没解决前应一直报着: %+v aligned=%v", r2.Conflicts, r2.Aligned())
			}
			st, err := PFRemoteReadStatus(f.ub.meta, pfrtCode)
			if err != nil || len(st.Conflicts) != 1 {
				t.Fatalf("Ubuntu 状态里的冲突: %v %+v", err, st.Conflicts)
			}
			// 用户把冲突副本删了（Mac 上），传播后冲突就不再报了
			if err := os.Remove(f.macAbs(wantName)); err != nil {
				t.Fatal(err)
			}
			f.mustDone(f.run())
			f.mustSame()
			r4 := f.settled()
			if len(r4.Conflicts) != 0 {
				t.Fatalf("冲突副本删了之后不该再报: %+v", r4.Conflicts)
			}
		})
	}
}

// ── S3b 改 vs 删：改的赢 ────────────────────────────────

func TestPFRoundS3bModifyVsDelete(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("甲.txt", "甲原始\n", pfrtOld)
	f.macPut("乙.txt", "乙原始\n", pfrtOld)
	f.mustDone(f.run())
	f.settled()

	// 甲：Mac 改、Ubuntu 删 → Ubuntu 恢复；乙：Ubuntu 改、Mac 删 → Mac 恢复
	f.macPut("甲.txt", "甲 Mac 改\n", pfrdOld(1))
	if err := os.Remove(f.ub.abs("甲.txt")); err != nil {
		t.Fatal(err)
	}
	f.ubPut("乙.txt", "乙 Ubuntu 改\n", pfrdOld(1))
	if err := os.Remove(f.macAbs("乙.txt")); err != nil {
		t.Fatal(err)
	}
	r := f.mustDone(f.run())
	f.mustSame()
	if f.ub.read("甲.txt") != "甲 Mac 改\n" || f.macRead("乙.txt") != "乙 Ubuntu 改\n" {
		t.Fatal("改的应当赢，被删的回来")
	}
	if r.Deletes.Mac+r.Deletes.Ubuntu != 0 {
		t.Fatalf("不该删任何东西: %+v", r.Deletes)
	}
	joined := strings.Join(r.Notices, "\n")
	if !strings.Contains(joined, "已恢复到 Ubuntu") || !strings.Contains(joined, "已恢复到 Mac") {
		t.Fatalf("应提示被删的文件恢复了:\n%s", joined)
	}
	f.settled()
}

// ── S4 删除传播 ─────────────────────────────────────────

func TestPFRoundS4DeletePropagation(t *testing.T) {
	f := newPFRDFix(t)
	f.seedMac()
	f.mustDone(f.run())

	if err := os.Remove(f.macAbs("现场照片/IMG_0001.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.ub.abs("说明.txt")); err != nil {
		t.Fatal(err)
	}
	r := f.mustDone(f.run())
	f.mustSame()
	if f.macHas("说明.txt") {
		t.Fatal("Ubuntu 删了的，Mac 也该删")
	}
	if _, err := os.Lstat(f.ub.abs("现场照片/IMG_0001.png")); err == nil {
		t.Fatal("Mac 删了的，Ubuntu 也该删")
	}
	if r.Deletes.Mac != 1 || r.Deletes.Ubuntu != 1 || r.BytesUp != 0 || r.BytesDown != 0 {
		t.Fatalf("删除数 %+v 上传 %d 下载 %d", r.Deletes, r.BytesUp, r.BytesDown)
	}
	// 都进了同步回收，回收里能找到，原因是「删除」
	if !pfrdHasRecycled(pfrdRecycled(t, PFRemoteRecycleDir(f.ub.meta)), "IMG_0001.png") {
		t.Fatal("Ubuntu 回收里应有 IMG_0001.png")
	}
	if !pfrdHasRecycled(pfrdRecycled(t, PFRoundMacRecycleDir(f.meta)), "说明.txt") {
		t.Fatal("Mac 回收里应有 说明.txt")
	}
	for _, dir := range []string{PFRemoteRecycleDir(f.ub.meta), PFRoundMacRecycleDir(f.meta)} {
		es := pfrdIndex(t, dir)
		if len(es) != 1 || es[0].Reason != string(PFReasonDelete) || es[0].SHA256 == "" {
			t.Fatalf("回收记录: %+v", es)
		}
	}
	if !strings.Contains(strings.Join(f.last.Status.Notes, "\n"), "删了 2 个") {
		t.Fatalf("状态里应报删除数: %v", f.last.Status.Notes)
	}
	b := f.loadBase()
	if _, ok := b.Get("说明.txt"); ok {
		t.Fatal("base 里该删掉 说明.txt")
	}
	f.settled()
}


