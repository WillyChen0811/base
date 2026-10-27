package projsync

// T9b 测试：全部项目的一轮（PFSched.RunRound）和取最新（PFFresh）的单机模拟。
//
// 做法同 T9a：「Ubuntu」的根和 meta 在临时目录里，假 RemoteRunner 在进程内直接调 xxyi-msg 的处理函数；传输器两端都用本机文件系统。
// Mac 侧：home/Desktop/Projects 是工作目录（里面是指向同步目录的链接），home/XXYI-sync 是同步目录，home/.xxyi/projfiles 是 meta。
// 时钟是固定的假时钟（可手动拨），文件的 mtime 都设在过去。需要 PFUnitMover（建新项目、检查链接）的场景只在 macOS 上跑。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pfscFlaky：可以一键断开的 Ubuntu 连接。
type pfscFlaky struct {
	inner RemoteRunner
	down  atomic.Bool
}

func (f *pfscFlaky) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	if f.down.Load() {
		return nil, nil, -1, errors.New("连不上 Ubuntu")
	}
	return f.inner.Run(ctx, cmd, stdin)
}

type pfscFix struct {
	t     *testing.T
	ub    *pfrtFix
	home  string
	work  string
	syn   string
	meta  string
	tr    *PFTransfer
	flaky *pfscFlaky
	s     *PFSched
	fr    *PFFresh
	recs  []PFUnitRecord
}

type pfscOpt struct {
	mover bool
	bps   float64
	net   string
}

const (
	pfscA = "260403J1001"
	pfscB = "260403J1002"
	pfscC = "260403J1003"
	pfscD = "260403J1004"
	pfscE = "260403J1005"
)

func pfscRel(code string) string { return "2026/" + code + "-项目" + code[len(code)-2:] }

func newPFSchedFix(t *testing.T, o pfscOpt) *pfscFix {
	t.Helper()
	if o.mover && runtime.GOOS != "darwin" {
		t.Skip("建新项目、检查链接只在 macOS 上做，其他平台跳过")
	}
	ub := newPFRTFix(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &pfscFix{t: t, ub: ub, home: filepath.Join(base, "home")}
	f.work = filepath.Join(f.home, "Desktop", "Projects")
	f.syn = filepath.Join(f.home, "XXYI-sync")
	f.meta = filepath.Join(f.home, ".xxyi", "projfiles")
	for _, d := range []string{f.work, f.syn, f.meta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.flaky = &pfscFlaky{inner: &pfrdSerial{inner: ub.run}}
	ub.cl.Runner = f.flaky
	f.tr, err = NewPFTransfer(PFTransferConfig{Local: PFLocalFS{}, Remote: PFLocalFS{},
		LocalStaging: PFRoundMacStagingDir(f.meta), RemoteStaging: PFRemoteStagingDir(ub.meta),
		VerifyUp: ub.cl.TargetVerify(), DefaultBps: o.bps})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.tr.Close)
	cfg := PFSchedConfig{MetaDir: f.meta, WorkDir: f.work, Remote: ub.cl, Transfer: f.tr,
		Settings: PFSchedSettings{Net: o.net, Host: "测试Mac"}, Now: func() time.Time { return ub.now }}
	if o.net == "" {
		cfg.Settings.Net = PFRemoteNetGood
	}
	if o.mover {
		m, err := NewPFUnitMover(PFUnitMoveConfig{WorkDir: f.work, SyncDir: f.syn, Meta: f.meta, Home: f.home})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Mover = m
	}
	f.s, err = NewPFSched(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.s.Close)
	f.fr, err = NewPFFresh(f.s, PFFreshConfig{SyncDir: f.syn})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *pfscFix) macRoot(code string) string {
	return filepath.Join(f.syn, filepath.FromSlash(pfscRel(code)))
}
func (f *pfscFix) link(code string) string {
	return filepath.Join(f.work, filepath.FromSlash(pfscRel(code)))
}

// addUnit 登记一个单元：Mac 侧目录（和桌面链接）总是建好；files 是 Mac 侧初始文件。
func (f *pfscFix) addUnit(code string, state PFUnitState, macFiles map[string]string) {
	f.t.Helper()
	root := f.macRoot(code)
	if err := os.MkdirAll(root, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for rel, c := range macFiles {
		pfrdPutAt(f.t, root, rel, c, pfrtOld)
	}
	if err := os.MkdirAll(filepath.Dir(f.link(code)), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(root, f.link(code)); err != nil {
		f.t.Fatal(err)
	}
	f.recs = append(f.recs, PFUnitRecord{Code: code, Rel: pfscRel(code), State: state, LinkPath: f.link(code), SyncPath: root, StartedAt: f.ub.now.Add(-time.Hour)})
	f.saveUnits()
}

func (f *pfscFix) saveUnits() {
	f.t.Helper()
	b, err := PFMarshalUnits(f.recs)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(PFUnitMoveUnitsPath(f.meta), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *pfscFix) ubPut(code, rel, content string, mt time.Time) {
	f.t.Helper()
	pfrdPutAt(f.t, filepath.Join(f.ub.root, filepath.FromSlash(pfscRel(code))), rel, content, mt)
}
func (f *pfscFix) macPut(code, rel, content string, mt time.Time) {
	f.t.Helper()
	pfrdPutAt(f.t, f.macRoot(code), rel, content, mt)
}
func (f *pfscFix) ubRead(code, rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.ub.root, filepath.FromSlash(pfscRel(code)), filepath.FromSlash(rel)))
	return string(b), err == nil
}
func (f *pfscFix) macRead(code, rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.macRoot(code), filepath.FromSlash(rel)))
	return string(b), err == nil
}

func (f *pfscFix) round(opt PFSchedRoundOptions) *PFSchedReport {
	f.t.Helper()
	return f.s.RunRound(context.Background(), opt)
}

func (r *PFSchedReport) unit(code string) *PFSchedUnitReport {
	for i := range r.Units {
		if r.Units[i].Code == code {
			return &r.Units[i]
		}
	}
	return nil
}

func pfscDump(r *PFSchedReport) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "outcome=%s reason=%s notices=%v\n", r.Outcome, r.Reason, r.Notices)
	for _, u := range r.Units {
		fmt.Fprintf(&sb, "  %s action=%s why=%s note=%s", u.Code, u.Action, u.Why, u.Note)
		if u.Result != nil {
			fmt.Fprintf(&sb, " result=%s", pfrdDump(u.Result))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func (f *pfscFix) mustRan(r *PFSchedReport, code string) *PFRoundResult {
	f.t.Helper()
	u := r.unit(code)
	if u == nil || u.Result == nil {
		f.t.Fatalf("单元 %s 应当跑过一轮:\n%s", code, pfscDump(r))
	}
	if u.Result.Outcome != PFRoundDone || len(u.Result.Errors) > 0 {
		f.t.Fatalf("单元 %s 应当正常跑完:\n%s", code, pfscDump(r))
	}
	return u.Result
}

// snapshot：目录树的全部内容（路径 → 类型、大小、mtime、内容哈希），用来断言「一个字节都没写」。
func pfscSnapshot(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			v := fmt.Sprintf("%v|%d|%d", fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
			if fi.Mode().IsRegular() {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				h := sha256.Sum256(b)
				v += "|" + hex.EncodeToString(h[:8])
			}
			out[p] = v
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func pfscSameSnapshot(t *testing.T, a, b map[string]string) {
	t.Helper()
	for p, v := range a {
		if b[p] != v {
			t.Errorf("%s 变了：%q → %q", p, v, b[p])
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			t.Errorf("多出了 %s", p)
		}
	}
}

// ── 全部项目一轮 ────────────────────────────────────────

func TestPFSchedMultiUnit(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{mover: true})
	// A：已同步（两边各有文件）；D：stopped；E：Missing（登记 syncing，Ubuntu 没有，Mac 有文件）
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"报告/m.txt": "来自Mac"})
	f.ubPut(pfscA, "u.txt", "来自Ubuntu", pfrtOld)
	f.addUnit(pfscD, PFUnitStopped, map[string]string{"d.txt": "D的Mac版"})
	f.ubPut(pfscD, "d.txt", "D的Ubuntu版", pfrtOld)
	f.addUnit(pfscE, PFUnitSyncing, map[string]string{"e.txt": "只在Mac"})
	// B：Ubuntu 新项目，桌面位置空着
	f.ubPut(pfscB, "b/b.txt", "新项目内容", pfrtOld)
	// C：Ubuntu 新项目，桌面上已有同编号的真实目录
	f.ubPut(pfscC, "c.txt", "C在Ubuntu", pfrtOld)
	realC := filepath.Join(f.work, "2026", pfscC+"-桌面上原有的")
	pfrdPutAt(t, realC, "own.txt", "桌面原有", pfrtOld)

	r := f.round(PFSchedRoundOptions{})
	if r.Outcome != PFSchedDone {
		t.Fatalf("应跑完:\n%s", pfscDump(r))
	}
	// A：双向对齐
	f.mustRan(r, pfscA)
	if c, ok := f.ubRead(pfscA, "报告/m.txt"); !ok || c != "来自Mac" {
		t.Errorf("A 的 Mac 文件应传到 Ubuntu: %q %v", c, ok)
	}
	if c, ok := f.macRead(pfscA, "u.txt"); !ok || c != "来自Ubuntu" {
		t.Errorf("A 的 Ubuntu 文件应传到 Mac: %q %v", c, ok)
	}
	// B：自动在 Mac 建好、桌面放链接，并同步
	ub := r.unit(pfscB)
	if ub == nil || ub.Action != PFSchedActCreated || ub.Result == nil || ub.Result.Outcome != PFRoundDone {
		t.Fatalf("B 应自动建好并同步:\n%s", pfscDump(r))
	}
	if c, ok := f.macRead(pfscB, "b/b.txt"); !ok || c != "新项目内容" {
		t.Errorf("B 的文件应出现在 Mac 同步目录: %q %v", c, ok)
	}
	if tgt, err := os.Readlink(f.link(pfscB)); err != nil || tgt != f.macRoot(pfscB) {
		t.Errorf("B 的桌面链接: %q %v", tgt, err)
	}
	us, err := PFUnitMoveLoadUnits(f.meta)
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := PFFindUnit(us, pfscB); !ok || rec.State != PFUnitSyncing {
		t.Errorf("B 应登记为 syncing: %+v", rec)
	}
	// C：只报告，不建
	uc := r.unit(pfscC)
	if uc == nil || uc.Action != PFSchedActNotDone || uc.Slot != PFSlotRealDir || !strings.Contains(uc.Note, PFNoteStartOnMacFirst) {
		t.Fatalf("C 应只报告请先开始同步:\n%s", pfscDump(r))
	}
	if _, err := os.Lstat(f.macRoot(pfscC)); err == nil {
		t.Error("C 不该在同步目录里建出来")
	}
	if c, _ := os.ReadFile(filepath.Join(realC, "own.txt")); string(c) != "桌面原有" {
		t.Error("桌面原有目录不该被动")
	}
	if _, ok := f.macRead(pfscC, "c.txt"); ok {
		t.Error("C 的文件不该出现在 Mac")
	}
	// D：stopped，不处理，Ubuntu 状态里标已停止
	ud := r.unit(pfscD)
	if ud == nil || ud.Action != PFSchedActNotDone || ud.Note != PFNoteStopped || ud.Result != nil {
		t.Fatalf("D 应不处理:\n%s", pfscDump(r))
	}
	if c, _ := f.macRead(pfscD, "d.txt"); c != "D的Mac版" {
		t.Error("D 的 Mac 文件不该被动")
	}
	if c, _ := f.ubRead(pfscD, "d.txt"); c != "D的Ubuntu版" {
		t.Error("D 的 Ubuntu 文件不该被动")
	}
	if st, err := PFRemoteReadStatus(f.ub.meta, pfscD); err != nil || !st.Stopped {
		t.Errorf("D 的 Ubuntu 状态应标已停止同步: %+v %v", st, err)
	}
	// E：Missing 交给一轮（首次规则把 Mac 的推过去）
	f.mustRan(r, pfscE)
	if c, ok := f.ubRead(pfscE, "e.txt"); !ok || c != "只在Mac" {
		t.Errorf("E 的文件应推到 Ubuntu: %q %v", c, ok)
	}
	// status/_mac.json
	ms, err := PFRemoteReadMacStatus(f.ub.meta)
	if err != nil || ms.Network != PFRemoteNetGood || ms.Host != "测试Mac" {
		t.Errorf("_mac.json: %+v %v", ms, err)
	}
	if r.LinksChecked < 3 {
		t.Errorf("轮前应检查了链接: %d", r.LinksChecked)
	}
	if r.Totals.Ran != 3 {
		t.Errorf("应跑了 A、B、E 三个: %+v", r.Totals)
	}

	// 第二轮：刚做完活，指纹变了，再确认一遍；第三轮才可以全部跳过
	r2 := f.round(PFSchedRoundOptions{})
	for _, c := range []string{pfscA, pfscB, pfscE} {
		if res := f.mustRan(r2, c); res.PlannedActions != 0 {
			t.Errorf("第二轮 %s 不该再有动作", c)
		}
	}
	r3 := f.round(PFSchedRoundOptions{})
	for _, c := range []string{pfscA, pfscB, pfscE} {
		if u := r3.unit(c); u == nil || u.Action != PFSchedActSkipped {
			t.Errorf("第三轮 %s 指纹没变应跳过:\n%s", c, pfscDump(r3))
		}
	}
	// Full：不看指纹
	r4 := f.round(PFSchedRoundOptions{Full: true})
	if u := r4.unit(pfscA); u == nil || u.Action != PFSchedActRan || u.Why != PFSchedWhyFull {
		t.Errorf("Full 应全跑:\n%s", pfscDump(r4))
	}
}

func TestPFSchedLeaseHeldWritesNothing(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"m.txt": "M"})
	f.ubPut(pfscA, "u.txt", "U", pfrtOld)
	other := PFRemoteLease{Version: 1, ID: "mac-B", Host: "另一台", Expires: f.ub.now.Add(5 * time.Minute).UTC().Format(time.RFC3339)}
	if err := pfrWriteLease(f.ub.meta, other); err != nil {
		t.Fatal(err)
	}
	before := pfscSnapshot(t, f.ub.base, f.home)
	r := f.round(PFSchedRoundOptions{})
	if r.Outcome != PFSchedLeaseHeld || r.LeaseHolder == nil || r.LeaseHolder.ID != "mac-B" || len(r.Units) != 0 {
		t.Fatalf("应报告租约被占:\n%s holder=%+v", pfscDump(r), r.LeaseHolder)
	}
	pfscSameSnapshot(t, before, pfscSnapshot(t, f.ub.base, f.home))
	// 租约过期后正常
	f.ub.now = f.ub.now.Add(10 * time.Minute)
	r = f.round(PFSchedRoundOptions{})
	if r.Outcome != PFSchedDone || r.unit(pfscA) == nil || r.unit(pfscA).Result == nil {
		t.Fatalf("过期后应正常:\n%s", pfscDump(r))
	}
}

func TestPFSchedOfflineAndUnreachable(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{net: PFRemoteNetOffline})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"m.txt": "M"})
	before := pfscSnapshot(t, f.ub.base, f.home)
	if r := f.round(PFSchedRoundOptions{}); r.Outcome != PFSchedOffline {
		t.Fatalf("断网: %s", pfscDump(r))
	}
	f.s.SetNet(PFRemoteNetGood, f.ub.now)
	f.flaky.down.Store(true)
	if r := f.round(PFSchedRoundOptions{}); r.Outcome != PFSchedUnreachable {
		t.Fatalf("连不上: %s", pfscDump(r))
	}
	pfscSameSnapshot(t, before, pfscSnapshot(t, f.ub.base, f.home))
	// 网络状态过期按未知（热点）对待
	f.s.SetNet(PFRemoteNetGood, f.ub.now.Add(-10*time.Minute))
	if got := f.s.netNow(); got != PFRemoteNetUnknown {
		t.Errorf("过期应按未知: %s", got)
	}
}

func TestPFSchedUbuntuRequests(t *testing.T) {
	// 热点：普通文件预计 30 秒 > 20 秒暂缓；请求要的文件用 60 秒的上限放行
	f := newPFSchedFix(t, pfscOpt{bps: 1000, net: PFRemoteNetExpensive})
	big := strings.Repeat("x", 30000)
	f.addUnit(pfscA, PFUnitSyncing, nil)
	f.ubPut(pfscA, "X.bin", big, pfrtOld)
	f.ubPut(pfscA, "Y.bin", big+"y", pfrtOld)
	f.addUnit(pfscB, PFUnitSyncing, map[string]string{"b.txt": "B"})
	f.ubPut(pfscB, "b.txt", "B", pfrtOld)
	// B 先对齐两轮，使它的指纹稳定
	f.round(PFSchedRoundOptions{Only: []string{pfscB}})
	f.round(PFSchedRoundOptions{Only: []string{pfscB}})
	// 现在 A 有 Ubuntu 请求，B 的 Mac 文件被改（指纹变了）：A 排在前面
	f.macPut(pfscB, "b2.txt", "改了", pfrtOld)
	name, err := PFRemoteWriteRequest(f.ub.meta, PFRemoteRequest{Kind: PFRemoteReqFetch, Unit: pfscA, Paths: []string{"X.bin"}, Priority: PFRemotePrioUser}, f.ub.now)
	if err != nil {
		t.Fatal(err)
	}
	r := f.round(PFSchedRoundOptions{})
	if r.Outcome != PFSchedDone || len(r.Units) < 2 || r.Units[0].Code != pfscA || r.Units[0].Why != PFSchedWhyRequest || r.Units[1].Code != pfscB || r.Units[1].Why != PFSchedWhyFingerprint {
		t.Fatalf("A（请求）应排在 B（指纹变了）前面:\n%s", pfscDump(r))
	}
	if c, ok := f.macRead(pfscA, "X.bin"); !ok || c != big {
		t.Error("请求的 X.bin 应以优先级 80 取回来")
	}
	if _, ok := f.macRead(pfscA, "Y.bin"); ok {
		t.Error("没被请求的 Y.bin 热点下应暂缓")
	}
	if len(r.unit(pfscA).Result.Deferred) != 1 || r.unit(pfscA).Result.Deferred[0].Path != "Y.bin" {
		t.Errorf("Y.bin 应在暂缓清单里: %+v", r.unit(pfscA).Result.Deferred)
	}
	if r.RequestsSeen != 1 || r.RequestsTaken != 1 {
		t.Errorf("请求应被取走: seen=%d taken=%d", r.RequestsSeen, r.RequestsTaken)
	}
	if _, err := os.Lstat(filepath.Join(f.ub.meta, PFRemoteRequestName, name)); err == nil {
		t.Error("请求文件应已被取走")
	}

	// touched 请求：单元指纹没变也必跑
	f.s.SetNet(PFRemoteNetGood, f.ub.now)
	f.round(PFSchedRoundOptions{})
	f.round(PFSchedRoundOptions{})
	if u := f.round(PFSchedRoundOptions{}).unit(pfscB); u == nil || u.Action != PFSchedActSkipped {
		t.Fatal("先确认 B 现在是跳过的")
	}
	if _, err := PFRemoteWriteRequest(f.ub.meta, PFRemoteRequest{Kind: PFRemoteReqTouched, Unit: pfscB, Priority: PFRemotePrioUser}, f.ub.now); err != nil {
		t.Fatal(err)
	}
	r = f.round(PFSchedRoundOptions{})
	if u := r.unit(pfscB); u == nil || u.Action != PFSchedActRan || u.Why != PFSchedWhyRequest {
		t.Fatalf("touched 请求应让 B 本轮必跑:\n%s", pfscDump(r))
	}
	if r.RequestsTaken != 1 {
		t.Errorf("touched 请求应被取走: %d", r.RequestsTaken)
	}
}

func TestPFSchedSkipRules(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"a.txt": "A"})
	f.round(PFSchedRoundOptions{})
	f.round(PFSchedRoundOptions{})
	if u := f.round(PFSchedRoundOptions{}).unit(pfscA); u == nil || u.Action != PFSchedActSkipped {
		t.Fatal("稳定后应跳过")
	}
	// Mac 上出现一个还在被写的文件（mtime = 现在）：指纹变 → 跑，判定里它是「不明」
	f.macPut(pfscA, "writing.bin", "半截", f.ub.now)
	r := f.round(PFSchedRoundOptions{})
	if u := r.unit(pfscA); u == nil || u.Action != PFSchedActRan || u.Why != PFSchedWhyFingerprint {
		t.Fatalf("新文件应让指纹变化而跑:\n%s", pfscDump(r))
	}
	if _, ok := f.ubRead(pfscA, "writing.bin"); ok {
		t.Fatal("还在被写的文件本轮不该传")
	}
	// 指纹不再变，但有不明条目：不能只靠指纹跳过
	r = f.round(PFSchedRoundOptions{})
	if u := r.unit(pfscA); u == nil || u.Action != PFSchedActRan || u.Why != PFSchedWhyPending {
		t.Fatalf("有不明条目时不该跳过:\n%s", pfscDump(r))
	}
	// 拨钟让它静止：同一份指纹，下一轮把它传过去
	f.ub.now = f.ub.now.Add(time.Minute)
	r = f.round(PFSchedRoundOptions{})
	if u := r.unit(pfscA); u == nil || u.Action != PFSchedActRan {
		t.Fatalf("静止后应跑:\n%s", pfscDump(r))
	}
	if c, ok := f.ubRead(pfscA, "writing.bin"); !ok || c != "半截" {
		t.Error("静止后应传到 Ubuntu")
	}
	// 定期全量核对：到间隔就跑
	f.round(PFSchedRoundOptions{})
	if u := f.round(PFSchedRoundOptions{}).unit(pfscA); u == nil || u.Action != PFSchedActSkipped {
		t.Fatal("稳定后应跳过")
	}
	f.ub.now = f.ub.now.Add(7 * time.Hour)
	if u := f.round(PFSchedRoundOptions{}).unit(pfscA); u == nil || u.Why != PFSchedWhyFull {
		t.Fatal("过了 6 小时应全量核对")
	}
}

func TestPFSchedTouchedDebounce(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"a.txt": "A"})
	f.round(PFSchedRoundOptions{})
	f.round(PFSchedRoundOptions{})
	p := filepath.Join(f.link(pfscA), "a.txt")
	if codes := f.fr.Touched([]string{p, "/不在/同步/里面"}); len(codes) != 1 || codes[0] != pfscA {
		t.Fatalf("Touched: %v", codes)
	}
	if len(f.s.PendingTouched()) != 0 {
		t.Error("去抖没到期不该返回")
	}
	if u := f.round(PFSchedRoundOptions{}).unit(pfscA); u == nil || u.Action != PFSchedActSkipped {
		t.Error("去抖没到期，这一轮不该为它特意跑")
	}
	f.ub.now = f.ub.now.Add(11 * time.Second)
	if due := f.s.PendingTouched(); len(due) != 1 {
		t.Fatalf("到期应返回: %v", due)
	}
	if u := f.round(PFSchedRoundOptions{Only: []string{pfscA}}).unit(pfscA); u == nil || u.Why != PFSchedWhyTouched {
		t.Error("到期后应让它必跑")
	}
	if len(f.s.PendingTouched()) != 0 {
		t.Error("跑过之后应清掉")
	}
}

func TestPFSchedTouchedCallback(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	got := make(chan []string, 1)
	f.s.cfg.OnTouchedDue = func(c []string) { got <- c }
	f.s.cfg.Now = time.Now // 真实时钟，去抖用真计时器
	f.s.SetSettings(PFSchedSettings{Net: PFRemoteNetGood, TouchedDebounce: 30 * time.Millisecond})
	f.addUnit(pfscA, PFUnitSyncing, nil)
	f.fr.Touched([]string{filepath.Join(f.link(pfscA), "x.txt")})
	f.fr.Touched([]string{filepath.Join(f.link(pfscA), "y.txt")})
	select {
	case c := <-got:
		if len(c) != 1 || c[0] != pfscA {
			t.Errorf("回调: %v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("去抖到期没有回调")
	}
}

// ── 取最新 ──────────────────────────────────────────────

func (f *pfscFix) settle(codes ...string) {
	f.t.Helper()
	for i := 0; i < 2; i++ {
		f.round(PFSchedRoundOptions{})
	}
}

func TestPFFreshFetchThroughLink(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"报告/m.txt": "v1", "n.txt": "N1"})
	f.ubPut(pfscA, "报告/m.txt", "v1", pfrtOld)
	f.ubPut(pfscA, "n.txt", "N1", pfrtOld)
	f.settle()
	// Ubuntu 上改了 m.txt，并新增了 Mac 没有的 new.txt
	f.ubPut(pfscA, "报告/m.txt", "v2-更新", pfrdOld(5))
	f.ubPut(pfscA, "new.txt", "新文件", pfrdOld(5))
	mp, np := filepath.Join(f.link(pfscA), "报告", "m.txt"), filepath.Join(f.link(pfscA), "new.txt")
	outside := filepath.Join(t.TempDir(), "别处.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	realDesk := filepath.Join(f.work, "2026", "260403J1009-桌面真目录")
	pfrdPutAt(t, realDesk, "a.txt", "x", pfrtOld)
	stopped := filepath.Join(f.syn, "2026", "260403J1004-停了的", "s.txt")
	pfrdPutAt(t, filepath.Dir(stopped), "s.txt", "x", pfrtOld)

	res := f.fr.Paths(context.Background(), []string{mp, np, filepath.Join(f.link(pfscA), "n.txt"), outside, filepath.Join(realDesk, "a.txt"), "相对/路径.txt", stopped}, 30*time.Second)
	want := []string{PFFreshStateFetched, PFFreshStateFetched, PFFreshStateLatest, PFFreshStateOutScope, PFFreshStateOutScope, PFFreshStateOutScope, PFFreshStateOutScope}
	for i, w := range want {
		if res.Items[i].State != w {
			t.Errorf("第 %d 项 %s：%q，应为 %q（note=%s）", i, res.Items[i].Path, res.Items[i].State, w, res.Note)
		}
	}
	if res.Items[0].Unit != pfscA || res.Items[0].Rel != "报告/m.txt" || res.Items[0].Size == 0 || res.Items[0].OtherMtime == "" {
		t.Errorf("第一项应带单元、单元内路径、大小、时间: %+v", res.Items[0])
	}
	if c, _ := f.macRead(pfscA, "报告/m.txt"); c != "v2-更新" {
		t.Errorf("m.txt 应取到最新: %q", c)
	}
	if c, _ := f.macRead(pfscA, "new.txt"); c != "新文件" {
		t.Errorf("Mac 上原本没有的 new.txt 也应取到: %q", c)
	}
	if res.Note != "" {
		t.Errorf("都顺利时不该有 note: %q", res.Note)
	}
	// 整个单元（链接本身）也认
	res = f.fr.Paths(context.Background(), []string{f.link(pfscA)}, 30*time.Second)
	if res.Items[0].State != PFFreshStateLatest || res.Items[0].Rel != "" {
		t.Errorf("整个项目: %+v", res.Items[0])
	}
}

func TestPFFreshNewerOverBudget(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{bps: 1000}) // 1000 字节/秒
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"big.bin": "旧"})
	f.ubPut(pfscA, "big.bin", "旧", pfrtOld)
	f.addUnit(pfscB, PFUnitSyncing, map[string]string{"s.txt": "s1"})
	f.ubPut(pfscB, "s.txt", "s1", pfrtOld)
	f.settle()
	big := strings.Repeat("大", 20000) // 60000 字节，预计 60 秒
	f.ubPut(pfscA, "big.bin", big, pfrdOld(5))
	f.ubPut(pfscB, "s.txt", "s2", pfrdOld(5))
	bp, sp := filepath.Join(f.link(pfscA), "big.bin"), filepath.Join(f.link(pfscB), "s.txt")
	res := f.fr.Paths(context.Background(), []string{bp, sp}, 5*time.Second)
	if res.Items[0].State != PFFreshStateNewer {
		t.Fatalf("超预算应答对方较新未取: %+v note=%s", res.Items[0], res.Note)
	}
	if res.Items[0].Size != int64(len(big)) || res.Items[0].OtherMtime == "" {
		t.Errorf("应带对方的大小和时间: %+v", res.Items[0])
	}
	if res.Items[1].State != PFFreshStateFetched {
		t.Errorf("小的应取到: %+v", res.Items[1])
	}
	if !strings.Contains(res.Note, "big.bin") {
		t.Errorf("note 应说明哪个文件没取: %q", res.Note)
	}
	if c, _ := f.macRead(pfscA, "big.bin"); c != "旧" {
		t.Error("超预算的不该被取")
	}
	if c, _ := f.macRead(pfscB, "s.txt"); c != "s2" {
		t.Error("小的应已取到")
	}

	// Ubuntu 连不上：读上一轮本机状态
	f.flaky.down.Store(true)
	res = f.fr.Paths(context.Background(), []string{bp, sp}, 5*time.Second)
	if res.Items[0].State != PFFreshStateNewer || !strings.Contains(res.Note, "big.bin") {
		t.Errorf("已知对方较新，不在线时仍应说明: %+v note=%s", res.Items[0], res.Note)
	}
	if res.Items[1].State != PFFreshStateOffline {
		t.Errorf("没有已知更新的应答对方不在线: %+v", res.Items[1])
	}
	if strings.Contains(res.Note, "上次与 Ubuntu 对齐") {
		t.Errorf("刚对齐过不该附对齐时间: %q", res.Note)
	}
	// 过了一个多小时
	f.ub.now = f.ub.now.Add(90 * time.Minute)
	res = f.fr.Paths(context.Background(), []string{sp}, 5*time.Second)
	if res.Items[0].State != PFFreshStateOffline || !strings.Contains(res.Note, "上次与 Ubuntu 对齐是") || !strings.Contains(res.Note, "之后的改动未知") {
		t.Errorf("超过 1 小时应附对齐时间: %+v note=%s", res.Items[0], res.Note)
	}
	// 断网同样不在线
	f.flaky.down.Store(false)
	f.s.SetNet(PFRemoteNetOffline, f.ub.now)
	res = f.fr.Paths(context.Background(), []string{sp}, 5*time.Second)
	if res.Items[0].State != PFFreshStateOffline {
		t.Errorf("断网: %+v", res.Items[0])
	}
}

func TestPFFreshConflict(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"c.txt": "原"})
	f.ubPut(pfscA, "c.txt", "原", pfrtOld)
	f.settle()
	f.macPut(pfscA, "c.txt", "Mac改的", pfrdOld(4))
	f.ubPut(pfscA, "c.txt", "Ubuntu改的", pfrdOld(5))
	res := f.fr.Paths(context.Background(), []string{filepath.Join(f.link(pfscA), "c.txt")}, 30*time.Second)
	if res.Items[0].State != PFFreshStateConflict || !strings.Contains(res.Note, "c.txt") {
		t.Fatalf("两边都改应答冲突: %+v note=%s", res.Items[0], res.Note)
	}
}

func TestPFFreshBusyUnitTimesOut(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	f.addUnit(pfscA, PFUnitSyncing, map[string]string{"a.txt": "A"})
	unlock, ok := f.s.lockUnit(context.Background(), pfscA, 0)
	if !ok {
		t.Fatal("拿锁")
	}
	defer unlock()
	start := time.Now()
	res := f.fr.Paths(context.Background(), []string{filepath.Join(f.link(pfscA), "a.txt")}, 1200*time.Millisecond)
	if res.Items[0].State != PFFreshStateUnknown || !strings.Contains(res.Note, "没能确认") {
		t.Errorf("单元正忙等不到：不能说成最新: %+v note=%s", res.Items[0], res.Note)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("不该超过预算太多")
	}
}

func TestPFSchedUnitLockSerial(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{})
	u1, ok := f.s.lockUnit(context.Background(), pfscA, 0)
	if !ok {
		t.Fatal("第一次应拿到")
	}
	if _, ok := f.s.lockUnit(context.Background(), pfscA, 50*time.Millisecond); ok {
		t.Fatal("同一单元不该并发")
	}
	u2, ok := f.s.lockUnit(context.Background(), pfscB, 50*time.Millisecond)
	if !ok {
		t.Fatal("别的单元不受影响")
	}
	u2()
	u1()
	if u3, ok := f.s.lockUnit(context.Background(), pfscA, 50*time.Millisecond); !ok {
		t.Fatal("释放后应能拿到")
	} else {
		u3()
	}
}

func TestPFSchedStartupRecover(t *testing.T) {
	f := newPFSchedFix(t, pfscOpt{mover: true})
	res, err := f.s.PFSchedStartup(context.Background())
	if err != nil || len(res) != 0 {
		t.Fatalf("没有未完成的移动: %v %v", res, err)
	}
}
