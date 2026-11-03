package projsync

// T8 协议测试：夹具、poll、facts、线上编码、严格解码、白名单。
// 全部在进程内直接调用 PFRemoteServer（经一个假的 RemoteRunner，不走 SSH），在临时目录里搭 Ubuntu 的根和 meta。
// 不依赖 Linux 专有系统调用，Studio（darwin）上能跑。
// 测试里的「当前时间」是假的（2026-10-27 12:00），文件的 mtime 固定在 10/20，所以都已静止。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	pfrtCode = "260403J1001"
	pfrtRel  = "2026/260403J1001-测试项目"
	pfrtBin  = "/usr/local/bin/peerctl"
)

var pfrtOld = time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)

// pfrtRunner 是假的 RemoteRunner：在进程内直接调用服务端，可注入连接层失败、改响应、在每次调用后做点事。
type pfrtRunner struct {
	srv    *PFRemoteServer
	calls  int
	cmds   []string
	stdins [][]byte
	fail   error                               // 非 nil：每次调用都返回这个连接层错误（不调用服务端）
	exit   int                                 // 非 0：每次调用都返回这个退出码（不调用服务端）
	mutate func(sub string, out []byte) []byte // 改服务端的响应
	after  func(n int)                         // 服务端处理完第 n 次调用之后（返回给调用方之前）
}

func (r *pfrtRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	r.calls++
	r.cmds = append(r.cmds, cmd)
	r.stdins = append(r.stdins, stdin)
	f := strings.Fields(cmd)
	if len(f) != 2 {
		return nil, nil, -1, fmt.Errorf("命令串不是「路径 子命令」: %q", cmd)
	}
	if r.fail != nil {
		return nil, nil, -1, r.fail
	}
	if r.exit != 0 {
		return nil, []byte("boom"), r.exit, nil
	}
	out, err := r.srv.Serve(ctx, f[1], stdin)
	if r.after != nil {
		r.after(r.calls)
	}
	if err != nil {
		return nil, []byte(err.Error()), 2, nil
	}
	if r.mutate != nil {
		out = r.mutate(f[1], out)
	}
	return out, nil, 0, nil
}

type pfrtFix struct {
	t    *testing.T
	base string
	root string
	meta string
	now  time.Time
	srv  *PFRemoteServer
	run  *pfrtRunner
	cl   *PFRemoteClient
}

func newPFRTFix(t *testing.T) *pfrtFix {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &pfrtFix{t: t, base: base, root: filepath.Join(base, "xxyi-projects"), meta: filepath.Join(base, "meta"),
		now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.Local)}
	for _, d := range []string{f.root, f.meta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := PFRemoteWriteRoots(f.meta, []string{f.root}); err != nil {
		t.Fatal(err)
	}
	f.srv = &PFRemoteServer{Now: func() time.Time { return f.now }}
	f.run = &pfrtRunner{srv: f.srv}
	f.cl = &PFRemoteClient{Runner: f.run, RemoteCmd: pfrtBin, Meta: f.meta, Root: f.root, CoordID: "mac-A", CoordHost: "测试Mac"}
	return f
}

func (f *pfrtFix) unit() PFRemoteUnit { return PFRemoteUnit{Code: pfrtCode, Rel: pfrtRel} }

// abs 返回单元 pfrtRel 里 rel 的绝对路径。
func (f *pfrtFix) abs(rel string) string {
	return filepath.Join(f.root, filepath.FromSlash(pfrtRel), filepath.FromSlash(rel))
}

// put 在单元里写一个文件（mtime 固定在过去）。
func (f *pfrtFix) put(rel, content string) string {
	f.t.Helper()
	return f.putIn(pfrtRel, rel, content)
}

func (f *pfrtFix) putIn(unitRel, rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(unitRel), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(p, pfrtOld, pfrtOld); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *pfrtFix) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(f.abs(rel))
	if err != nil {
		f.t.Fatalf("读 %s 失败: %v", rel, err)
	}
	return string(b)
}

func (f *pfrtFix) exists(rel string) bool {
	_, err := os.Lstat(f.abs(rel))
	return err == nil
}

func pfrtSHA(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// stage 在 Ubuntu 暂存区放一份 part，返回 sha256 和大小。
func (f *pfrtFix) stage(content string) (string, int64) {
	f.t.Helper()
	sha := pfrtSHA(content)
	dir := PFRemoteStagingDir(f.meta)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sha+".part"), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return sha, int64(len(content))
}

// expect 读出单元里某个文件的现状作为前提。
func (f *pfrtFix) expect(rel string) PFExpect {
	f.t.Helper()
	p := f.abs(rel)
	st, err := PFLstat(p)
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	return PFExpectFile(st.Size, st.MtimeNs, st.Ino, pfrtSHA(string(b)))
}

func (f *pfrtFix) leaseFile() string { return filepath.Join(f.meta, PFRemoteLeaseFile) }

// rawServe 直接给服务端一个原始请求（绕过客户端的本机校验）。
func (f *pfrtFix) rawServe(sub string, req any) ([]byte, error) {
	f.t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.srv.Serve(context.Background(), sub, b)
}

func pfrtNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ── poll ────────────────────────────────────────────────

func TestPFRemotePollNormal(t *testing.T) {
	f := newPFRTFix(t)
	f.put("报告/a.docx", "A")
	f.put("b.txt", "BB")
	f.put(".DS_Store", "x")
	other := "2026/260403J1002-另一个项目"
	f.putIn(other, "c.txt", "CCC")
	// 请求：一个好的、一个读不懂的、一个名字不符合格式的（忽略）
	name, err := PFRemoteWriteRequest(f.meta, PFRemoteRequest{Kind: PFRemoteReqTouched, Unit: pfrtCode, Priority: PFRemotePrioUser}, f.now)
	pfrtNoErr(t, err)
	badName := "260403J1002-20261027T020000Z-deadbeef.json"
	pfrtNoErr(t, os.WriteFile(filepath.Join(f.meta, PFRemoteRequestName, badName), []byte("{"), 0o644))
	pfrtNoErr(t, os.WriteFile(filepath.Join(f.meta, PFRemoteRequestName, "随便.txt"), []byte("x"), 0o644))
	// 暂存区残留
	pfrtNoErr(t, os.MkdirAll(PFRemoteStagingDir(f.meta), 0o700))
	pfrtNoErr(t, os.WriteFile(filepath.Join(PFRemoteStagingDir(f.meta), ".pfswap-abcd1234-ffff"), []byte("用户内容"), 0o600))

	resp, err := f.cl.Poll(context.Background(), PFRemotePollOptions{})
	pfrtNoErr(t, err)
	if !resp.Lease.Granted || resp.Lease.Holder.ID != "mac-A" || resp.Lease.Holder.Host != "测试Mac" {
		t.Fatalf("租约: %+v", resp.Lease)
	}
	if want := f.now.Add(PFRemoteDefaultLease).UTC().Format(time.RFC3339); resp.Lease.Holder.Expires != want {
		t.Fatalf("到期时间 %s，应为 %s", resp.Lease.Holder.Expires, want)
	}
	if resp.NowNs != f.now.UnixNano() {
		t.Fatalf("NowNs %d", resp.NowNs)
	}
	if len(resp.Units) != 2 || resp.Units[0].Rel != pfrtRel || resp.Units[1].Rel != other || resp.Incomplete {
		t.Fatalf("单元: %+v incomplete=%v", resp.Units, resp.Incomplete)
	}
	u := resp.Units[0]
	st, err := PFLstat(f.abs(""))
	pfrtNoErr(t, err)
	if u.Code != pfrtCode || u.Dup || u.DirID != (PFDirID{Dev: st.Dev, Ino: st.Ino}) || u.Error != "" {
		t.Fatalf("单元 1: %+v", u)
	}
	scan, err := PFScan(context.Background(), f.abs(""), PFScanOptions{Filter: PFRemoteScanFilter(), Now: func() time.Time { return f.now }})
	pfrtNoErr(t, err)
	if u.Fingerprint != scan.Fingerprint() {
		t.Fatalf("指纹 %s，直接扫描是 %s", u.Fingerprint, scan.Fingerprint())
	}
	if u.Summary.Files != 2 || u.Summary.Dirs != 1 || u.Summary.Excluded != 1 || u.Summary.Items != 3 || u.Summary.Unknown != 0 || u.Summary.Bytes != 3 {
		t.Fatalf("摘要: %+v", u.Summary)
	}
	if resp.Units[1].Fingerprint == "" || resp.Units[1].Fingerprint == u.Fingerprint {
		t.Fatalf("第二个单元的指纹: %q", resp.Units[1].Fingerprint)
	}
	if resp.FreeBytes <= 0 || resp.TotalBytes < resp.FreeBytes {
		t.Fatalf("空间: free=%d total=%d", resp.FreeBytes, resp.TotalBytes)
	}
	if len(resp.Requests) != 1 || resp.Requests[0].Name != name || resp.Requests[0].Request.Kind != PFRemoteReqTouched {
		t.Fatalf("请求: %+v", resp.Requests)
	}
	if len(resp.BadRequests) != 1 || resp.BadRequests[0].Name != badName {
		t.Fatalf("读不懂的请求: %+v", resp.BadRequests)
	}
	if len(resp.StagingLeftovers) != 1 || resp.StagingLeftovers[0].Kind != "swap" {
		t.Fatalf("暂存区残留: %+v", resp.StagingLeftovers)
	}
	// 租约落盘
	l, err := pfrReadLease(f.meta)
	pfrtNoErr(t, err)
	if l == nil || l.ID != "mac-A" || l.Expires != resp.Lease.Holder.Expires {
		t.Fatalf("租约文件: %+v", l)
	}
	// 命令串固定：路径 + 子命令名
	if f.run.cmds[0] != pfrtBin+" projfiles-poll" {
		t.Fatalf("命令串: %q", f.run.cmds[0])
	}
}

func TestPFRemotePollDupAndSkipped(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	f.putIn("2025/260403J1001-副本", "b.txt", "B")
	pfrtNoErr(t, os.Symlink(f.abs(""), filepath.Join(f.root, "链接")))
	resp, err := f.cl.Poll(context.Background(), PFRemotePollOptions{})
	pfrtNoErr(t, err)
	if len(resp.Units) != 2 {
		t.Fatalf("单元: %+v", resp.Units)
	}
	for _, u := range resp.Units {
		// §14.2：同编号多个目录不再停，每个目录照常扫描、有指纹（登记单元靠它跳过没变化的轮次）。
		if !u.Dup || len(u.DupRels) != 1 || !pfValidSHA(u.Fingerprint) || u.Error != "" {
			t.Fatalf("重复单元: %+v", u)
		}
	}
	if !resp.Incomplete || len(resp.Skipped) != 1 || resp.Skipped[0].Rel != "链接" {
		t.Fatalf("符号链接应报进 skipped 且发现不完整: %+v incomplete=%v", resp.Skipped, resp.Incomplete)
	}
}

func TestPFRemotePollLease(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	// 别的协调者持有且未过期：拒绝，只回租约信息，租约文件不动
	other := PFRemoteLease{Version: 1, ID: "mac-B", Host: "另一台", Expires: f.now.Add(5 * time.Minute).UTC().Format(time.RFC3339)}
	pfrtNoErr(t, pfrWriteLease(f.meta, other))
	before, _ := os.ReadFile(f.leaseFile())
	resp, err := f.cl.Poll(ctx, PFRemotePollOptions{})
	if !errors.Is(err, ErrPFRemoteLeaseHeld) || PFRemoteIsUncertain(err) {
		t.Fatalf("应是租约被占的错误（不是通信失败）: %v", err)
	}
	var re *PFRemoteError
	if !errors.As(err, &re) || re.Holder == nil || re.Holder.ID != "mac-B" || !strings.Contains(re.Msg, "mac-B") {
		t.Fatalf("错误里应带现任持有者和说明: %+v", re)
	}
	if resp.Lease.Granted || len(resp.Units) != 0 || len(resp.Requests) != 0 {
		t.Fatalf("被占时不该回单元和请求: %+v", resp)
	}
	after, _ := os.ReadFile(f.leaseFile())
	if string(before) != string(after) {
		t.Fatal("被占时不该改租约文件")
	}
	// 过期后接管
	f.now = f.now.Add(6 * time.Minute)
	resp, err = f.cl.Poll(ctx, PFRemotePollOptions{LeaseSeconds: 120})
	pfrtNoErr(t, err)
	l, _ := pfrReadLease(f.meta)
	if l.ID != "mac-A" || l.Expires != f.now.Add(2*time.Minute).UTC().Format(time.RFC3339) || len(resp.Units) != 1 {
		t.Fatalf("接管: %+v", l)
	}
	// 自己续租：到期时间顺延
	f.now = f.now.Add(time.Minute)
	_, err = f.cl.Poll(ctx, PFRemotePollOptions{LeaseSeconds: 120})
	pfrtNoErr(t, err)
	l, _ = pfrReadLease(f.meta)
	if l.Expires != f.now.Add(2*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("续租后到期时间: %s", l.Expires)
	}
	// 租约文件读不懂：不覆盖、不授予
	pfrtNoErr(t, os.WriteFile(f.leaseFile(), []byte("坏的"), 0o644))
	_, err = f.cl.Poll(ctx, PFRemotePollOptions{})
	if !errors.Is(err, ErrPFRemoteLeaseHeld) {
		t.Fatalf("租约文件读不懂应拒绝: %v", err)
	}
	if b, _ := os.ReadFile(f.leaseFile()); string(b) != "坏的" {
		t.Fatal("读不懂的租约文件不该被覆盖")
	}
}

func TestPFRemoteWhitelist(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	refused := func(name string, mod func(c *PFRemoteClient), want string) {
		t.Helper()
		c := *f.cl
		mod(&c)
		_, err := c.Poll(ctx, PFRemotePollOptions{})
		var re *PFRemoteError
		if !errors.As(err, &re) || re.Kind != PFRemoteErrRefused || PFRemoteIsUncertain(err) || !strings.Contains(re.Msg, want) {
			t.Fatalf("%s: 应被拒绝（含 %q），得到 %v", name, want, err)
		}
	}
	// 根不在白名单
	other := filepath.Join(f.base, "别的目录")
	pfrtNoErr(t, os.MkdirAll(other, 0o755))
	refused("不在白名单", func(c *PFRemoteClient) { c.Root = other }, "白名单")
	if _, err := os.Lstat(f.leaseFile()); !os.IsNotExist(err) {
		t.Fatal("被拒绝的请求不该写租约")
	}
	// 根是符号链接
	link := filepath.Join(f.base, "link")
	pfrtNoErr(t, os.Symlink(f.root, link))
	refused("根是链接", func(c *PFRemoteClient) { c.Root = link }, "不是真实目录")
	// 根的路径上有符号链接
	refused("路径上有链接", func(c *PFRemoteClient) { c.Root = filepath.Join(link, "x") }, "读不了")
	// 不干净的路径
	refused("不干净", func(c *PFRemoteClient) { c.Root = f.root + "/./" }, "不是干净的路径")
	refused("相对路径", func(c *PFRemoteClient) { c.Root = "xxyi-projects" }, "不是绝对路径")
	refused("meta 相对", func(c *PFRemoteClient) { c.Meta = "meta" }, "不是绝对路径")
	// meta 在根里面 / 根在 meta 里面
	inner := filepath.Join(f.root, "meta2")
	pfrtNoErr(t, os.MkdirAll(inner, 0o755))
	pfrtNoErr(t, PFRemoteWriteRoots(inner, []string{f.root}))
	refused("meta 在根里", func(c *PFRemoteClient) { c.Meta = inner }, "互相包含")
	refused("meta 等于根", func(c *PFRemoteClient) { c.Meta = f.root }, "互相包含")
	// 没有 roots.json
	empty := filepath.Join(f.base, "空meta")
	pfrtNoErr(t, os.MkdirAll(empty, 0o755))
	refused("没部署", func(c *PFRemoteClient) { c.Meta = empty }, "没有部署白名单")
	// roots.json 读不懂
	bad := filepath.Join(f.base, "坏meta")
	pfrtNoErr(t, os.MkdirAll(bad, 0o755))
	pfrtNoErr(t, os.WriteFile(filepath.Join(bad, PFRemoteRootsFile), []byte(`{"version":1,"roots":["/a"],"extra":1}`), 0o644))
	refused("白名单坏", func(c *PFRemoteClient) { c.Meta = bad }, "读不懂")
	// facts、apply 同样
	c := *f.cl
	c.Root = other
	if _, err := c.Facts(ctx, f.unit(), PFRemoteFactsOptions{}); err == nil || PFRemoteIsUncertain(err) {
		t.Fatalf("facts 应被拒绝: %v", err)
	}
	if _, err := c.Apply(ctx, PFRemoteApplyOptions{Unit: f.unit()}, []PFRemoteStep{{Op: PFRemoteOpMkdir, Path: "新"}}); err == nil || PFRemoteIsUncertain(err) {
		t.Fatalf("apply 应被拒绝: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(other, "2026")); err == nil {
		t.Fatal("白名单外的目录里不该被建任何东西")
	}
}

// ── 严格解码 ────────────────────────────────────────────

func TestPFRemoteStrictDecode(t *testing.T) {
	f := newPFRTFix(t)
	ctx := context.Background()
	good := map[string]string{
		PFRemoteSubPoll:  fmt.Sprintf(`{"version":1,"meta":%q,"root":%q,"coord_id":"mac-A"}`, f.meta, f.root),
		PFRemoteSubFacts: fmt.Sprintf(`{"version":1,"meta":%q,"root":%q,"code":%q,"rel":%q}`, f.meta, f.root, pfrtCode, pfrtRel),
		PFRemoteSubApply: fmt.Sprintf(`{"version":1,"meta":%q,"root":%q,"coord_id":"mac-A","steps":[{"op":"take_requests","names":["%s-20261027T020000Z-deadbeef.json"]}]}`, f.meta, f.root, pfrtCode),
	}
	for sub, body := range good {
		if _, err := f.srv.Serve(ctx, sub, []byte(body)); err != nil {
			t.Fatalf("%s 正常请求被拒绝: %v", sub, err)
		}
		bads := map[string]string{
			"未知字段":    strings.Replace(body, `"version":1`, `"version":1,"bogus":1`, 1),
			"版本不对":    strings.Replace(body, `"version":1`, `"version":2`, 1),
			"没有版本":    strings.Replace(body, `"version":1,`, ``, 1),
			"后面有多余内容": body + ` {}`,
			"不是 JSON": "{",
			"空":       "",
			"数组":      "[]",
		}
		for name, in := range bads {
			out, err := f.srv.Serve(ctx, sub, []byte(in))
			if err == nil || out != nil {
				t.Errorf("%s / %s: 应是协议层失败，得到 out=%q err=%v", sub, name, out, err)
			}
		}
		big := make([]byte, PFRemoteMaxInput+1)
		if _, err := f.srv.Serve(ctx, sub, big); err == nil || !strings.Contains(err.Error(), "上限") {
			t.Errorf("%s: 超长的请求应拒绝: %v", sub, err)
		}
	}
	if _, err := f.srv.Serve(ctx, "projfiles-nope", []byte("{}")); err == nil {
		t.Error("未知子命令应失败")
	}
	// poll 的字段取值
	for name, mod := range map[string]string{
		"协调者编号带空格": `"coord_id":"a b"`,
		"租约太短":     `"coord_id":"mac-A","lease_seconds":5`,
		"租约太长":     `"coord_id":"mac-A","lease_seconds":99999`,
		"静止秒数为负":   `"coord_id":"mac-A","quiet_seconds":-1`,
	} {
		in := fmt.Sprintf(`{"version":1,"meta":%q,"root":%q,%s}`, f.meta, f.root, mod)
		if _, err := f.srv.Serve(ctx, PFRemoteSubPoll, []byte(in)); err == nil {
			t.Errorf("poll %s: 应失败", name)
		}
	}
	// facts 的字段取值
	for name, mod := range map[string]string{
		"单元键小写":    `"code":"260403j1001","rel":"a"`,
		"rel 含 ..": `"code":"260403J1001","rel":"../a"`,
		"预算太大":     `"code":"260403J1001","rel":"a","hash_budget_ms":1000000`,
		"条目上限太大":   `"code":"260403J1001","rel":"a","max_items":50001`,
		"页太小":      `"code":"260403J1001","rel":"a","page_bytes":10`,
		"游标为负":     `"code":"260403J1001","rel":"a","after_item":-1`,
	} {
		in := fmt.Sprintf(`{"version":1,"meta":%q,"root":%q,%s}`, f.meta, f.root, mod)
		if _, err := f.srv.Serve(ctx, PFRemoteSubFacts, []byte(in)); err == nil {
			t.Errorf("facts %s: 应失败", name)
		}
	}
}

func TestPFRemoteOutputLimit(t *testing.T) {
	s := &PFRemoteServer{}
	if _, err := s.encode(map[string]string{"x": strings.Repeat("a", pfrOutputLimit)}); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超过输出上限应报错而不是截断: %v", err)
	}
	if b, err := s.encode(map[string]string{"x": "ok"}); err != nil || len(b) == 0 {
		t.Fatalf("%v", err)
	}
}

// ── 线上编码 ────────────────────────────────────────────

func TestPFRemoteWireRoundTrip(t *testing.T) {
	sha := pfrtSHA("x")
	const dirDev = 9
	items := []PFScanItem{
		{Path: "a.txt", Kind: PFKindFile, HasStat: true, Stat: PFStat{Kind: PFKindFile, Size: 5, MtimeNs: 1, CtimeNs: 2, Ino: 3, Dev: dirDev, Mode: 0o644}, State: PFStateOK, Hash: sha, FromCache: true},
		{Path: "目录/子", Kind: PFKindDir, HasStat: true, Stat: PFStat{Kind: PFKindDir, MtimeNs: 4, CtimeNs: 5, Ino: 6, Dev: dirDev, Mode: 0o755 | 1<<31}, State: PFStateOK},
		{Path: "挂载点/f", Kind: PFKindFile, HasStat: true, Stat: PFStat{Kind: PFKindFile, Size: 1, Ino: 7, Dev: 77, Mode: 0o600}, State: PFStateOK},
		{Path: "零设备", Kind: PFKindFile, HasStat: true, Stat: PFStat{Kind: PFKindFile, Ino: 8, Dev: 0}, State: PFStateOK},
		{Path: "新.docx", Kind: PFKindFile, HasStat: true, Stat: PFStat{Kind: PFKindFile, Size: 9, MtimeNs: 100, Dev: dirDev, Flags: PFSFDataless}, State: PFStateUnknown, Code: PFCodeNotQuiet, Reason: "未静止"},
		{Path: "读不了", Kind: PFKindDir, State: PFStateOutOfScope, Code: PFCodeUnreadable, Reason: "读不了目录"},
		{Path: "链接", Kind: PFKindSymlink, HasStat: true, Stat: PFStat{Kind: PFKindSymlink, Ino: 10, Dev: dirDev}, State: PFStateOutOfScope, Code: PFCodeSymlink},
		{Path: "kind不同", Kind: PFKindFile, HasStat: true, Stat: PFStat{Kind: PFKindOther, Ino: 11, Dev: dirDev}, State: PFStateOK},
		{Path: "x", Kind: PFKindFile, State: PFStateUnknown, Code: PFCodeICloudPlaceholder, Stub: true},
	}
	for _, it := range items {
		esc := 0
		w := pfrToWire(it, dirDev, &esc)
		b, err := json.Marshal(w)
		pfrtNoErr(t, err)
		var w2 PFRemoteItem
		pfrtNoErr(t, json.Unmarshal(b, &w2))
		got, err := pfrFromWire(w2, dirDev)
		pfrtNoErr(t, err)
		if esc != 0 || !reflect.DeepEqual(got, it) {
			t.Errorf("编码往返不一致 %q:\n 原 %+v\n 得 %+v\n json %s", it.Path, it, got, b)
		}
	}
	// 不合法的内容不猜
	for _, w := range []PFRemoteItem{
		{Path: "a", Kind: "z"},
		{Path: "a", Kind: "f", State: "x"},
		{Path: "a", Kind: "f", Size: 3}, // 没有 stat 却带 stat 字段
		{Path: "a", Kind: "f", HasStat: true, StatKind: "z"},
		{Path: "a", Kind: "f", Hash: "xyz"},
	} {
		if _, err := pfrFromWire(w, dirDev); err == nil {
			t.Errorf("%+v 应被拒绝", w)
		}
	}
	// 非 UTF-8 的路径：换成 \xNN 的显示形式并计数
	esc := 0
	w := pfrToWire(PFScanItem{Path: "目录/a\xffb\xfe.txt", Kind: PFKindFile, State: PFStateOutOfScope, Code: PFCodeFilter}, dirDev, &esc)
	if w.Path != `目录/a\xffb\xfe.txt` || esc != 1 {
		t.Fatalf("转义: %q esc=%d", w.Path, esc)
	}
	if _, err := json.Marshal(w); err != nil {
		t.Fatal(err)
	}
}

func TestPFRemoteWireCompact(t *testing.T) {
	it := PFScanItem{Path: "2026项目/报告/第三章 设计说明-终稿.docx", Kind: PFKindFile, HasStat: true, State: PFStateOK, Hash: pfrtSHA("x"),
		Stat: PFStat{Kind: PFKindFile, Size: 123456, MtimeNs: 1790000000123456789, CtimeNs: 1790000000123456789, Ino: 12345678, Dev: 66306, Mode: 0o644}}
	n := pfrWireSize(pfrToWire(it, 66306, new(int)))
	// 路径约 50 字节：每条应在 250 字节以内，这样 5 万个条目分页后每页远小于 8MB
	if n > 250 {
		t.Fatalf("一个典型条目编码后有 %d 字节，太大", n)
	}
}

// ── facts ───────────────────────────────────────────────

func TestPFRemoteFactsNormal(t *testing.T) {
	f := newPFRTFix(t)
	ctx := context.Background()
	f.put("报告/a.docx", "AAA")
	f.put("b.txt", "BBBB")
	f.put("c/d/e.txt", "")
	f.put(".DS_Store", "x")
	f.put("报告/~$a.docx", "lock")
	pfrtNoErr(t, os.Symlink("b.txt", f.abs("link")))

	facts, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	st, err := PFLstat(f.abs(""))
	pfrtNoErr(t, err)
	if !facts.Exists || facts.UnitPath != f.abs("") || facts.DirID != (PFDirID{Dev: st.Dev, Ino: st.Ino}) || facts.Dup || facts.Pages != 1 {
		t.Fatalf("头部: %+v", facts)
	}
	it, ok := facts.Scan.Find("报告/a.docx")
	if !ok || it.State != PFStateOK || it.Hash != pfrtSHA("AAA") || it.Stat.Size != 3 || it.Stat.Dev != st.Dev || it.FromCache {
		t.Fatalf("a.docx: %+v", it)
	}
	if it, ok := facts.Scan.Find("c/d/e.txt"); !ok || it.Hash != pfrtSHA("") {
		t.Fatalf("空文件应有哈希: %+v", it)
	}
	if it, ok := facts.Scan.Find("link"); !ok || it.State != PFStateOutOfScope || it.Code != PFCodeSymlink {
		t.Fatalf("符号链接: %+v", it)
	}
	if _, ok := facts.Scan.Find(".DS_Store"); ok {
		t.Fatal("被排除的名字不该在条目里")
	}
	names := map[string]PFRawName{}
	for _, n := range facts.Names {
		names[n.Path] = n
	}
	if len(names) != 2 || names[".DS_Store"].MtimeNs == 0 || names["报告/~$a.docx"].MtimeNs != pfrtOld.UnixNano() {
		t.Fatalf("被排除的名字（带 mtime）: %+v", facts.Names)
	}
	if facts.Scan.Excluded != 2 || facts.Summary.Excluded != 2 {
		t.Fatalf("排除计数: %d", facts.Scan.Excluded)
	}
	if facts.Scan.Fingerprint() != facts.Fingerprint || facts.TotalItems != len(facts.Scan.Items) {
		t.Fatal("指纹或总数对不上")
	}
	if facts.HashStats.Hashed != 3 || facts.HashStats.CacheHits != 0 || facts.HashStats.HashedBytes != 7 {
		t.Fatalf("第一次哈希统计: %+v", facts.HashStats)
	}
	if facts.NowNs != f.now.UnixNano() {
		t.Fatalf("NowNs %d", facts.NowNs)
	}
	// poll 与 facts 的指纹一致（T9 拿它判断单元有没有变）
	pr, err := f.cl.Poll(ctx, PFRemotePollOptions{})
	pfrtNoErr(t, err)
	if pr.Units[0].Fingerprint != facts.Fingerprint {
		t.Fatalf("poll 指纹 %s，facts 指纹 %s", pr.Units[0].Fingerprint, facts.Fingerprint)
	}

	// 哈希缓存写在 Ubuntu 的 meta 里；第二次全部命中
	cachePath := filepath.Join(f.meta, PFRemoteCacheName, pfrtCode+".json")
	b, err := os.ReadFile(cachePath)
	pfrtNoErr(t, err)
	c, err := PFParseHashCache(b)
	pfrtNoErr(t, err)
	if c.Len() != 3 {
		t.Fatalf("缓存条目 %d", c.Len())
	}
	facts2, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if facts2.HashStats.CacheHits != 3 || facts2.HashStats.Hashed != 0 {
		t.Fatalf("第二次应全部命中缓存: %+v", facts2.HashStats)
	}
	if it, _ := facts2.Scan.Find("b.txt"); !it.FromCache || it.Hash != pfrtSHA("BBBB") {
		t.Fatalf("缓存命中的条目: %+v", it)
	}
	// 改一个、删一个：只重算改的，删掉的从缓存里清走
	f.put("b.txt", "CCCCC")
	pfrtNoErr(t, os.Remove(f.abs("报告/a.docx")))
	facts3, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if facts3.HashStats.Hashed != 1 || facts3.HashStats.CacheHits != 1 {
		t.Fatalf("第三次: %+v", facts3.HashStats)
	}
	if it, _ := facts3.Scan.Find("b.txt"); it.Hash != pfrtSHA("CCCCC") {
		t.Fatalf("改过的文件哈希: %+v", it)
	}
	b, _ = os.ReadFile(cachePath)
	c, err = PFParseHashCache(b)
	pfrtNoErr(t, err)
	if c.Len() != 2 {
		t.Fatalf("清理后缓存条目 %d，应为 2", c.Len())
	}
}

func TestPFRemoteFactsCorruptCache(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	cachePath := filepath.Join(f.meta, PFRemoteCacheName, pfrtCode+".json")
	pfrtNoErr(t, os.MkdirAll(filepath.Dir(cachePath), 0o755))
	pfrtNoErr(t, os.WriteFile(cachePath, []byte(`{"version":1,"entries":{"a.txt":{"size":1}}}`), 0o644))
	facts, err := f.cl.Facts(context.Background(), f.unit(), PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if facts.HashStats.Hashed != 1 || len(facts.Notes) != 1 || !strings.Contains(facts.Notes[0], "损坏") {
		t.Fatalf("缓存损坏应重算并说明: %+v notes=%v", facts.HashStats, facts.Notes)
	}
	b, _ := os.ReadFile(cachePath)
	if c, err := PFParseHashCache(b); err != nil || c.Len() != 1 {
		t.Fatalf("损坏的缓存应被换成新的: %v", err)
	}
}

func TestPFRemoteFactsHashBudget(t *testing.T) {
	f := newPFRTFix(t)
	for i := 0; i < 5; i++ {
		f.put(fmt.Sprintf("f%d.txt", i), strings.Repeat("x", 100+i))
	}
	f.srv.Open = func(p string) (io.ReadCloser, error) { time.Sleep(40 * time.Millisecond); return os.Open(p) }
	facts, err := f.cl.Facts(context.Background(), f.unit(), PFRemoteFactsOptions{HashBudget: 5 * time.Millisecond})
	pfrtNoErr(t, err)
	if facts.Summary.Unknown != 5 || facts.HashStats.NewUnknown != 5 {
		t.Fatalf("预算用完应全部标不明: %+v %+v", facts.Summary, facts.HashStats)
	}
	for _, it := range facts.Scan.Items {
		if it.State != PFStateUnknown || it.Code != PFCodeHashTimeout || it.Hash != "" {
			t.Fatalf("%+v", it)
		}
	}
	// 没算完的不进缓存；预算够了就都算出来
	f.srv.Open = nil
	facts, err = f.cl.Facts(context.Background(), f.unit(), PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if facts.Summary.Unknown != 0 || facts.HashStats.Hashed != 5 {
		t.Fatalf("预算够了应全部算出: %+v %+v", facts.Summary, facts.HashStats)
	}
}

func TestPFRemoteFactsNotExists(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	// 同编号但路径不同：Exists=false，Found 说明在哪
	facts, err := f.cl.Facts(ctx, PFRemoteUnit{Code: pfrtCode, Rel: "2026/260403J1001-改了名"}, PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if facts.Exists || !reflect.DeepEqual(facts.Found, []string{pfrtRel}) || facts.Scan != nil {
		t.Fatalf("%+v", facts)
	}
	// 完全没有这个编号
	facts, err = f.cl.Facts(ctx, PFRemoteUnit{Code: "260403J9999", Rel: "2026/260403J9999-无"}, PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if facts.Exists || len(facts.Found) != 0 || facts.Incomplete {
		t.Fatalf("%+v", facts)
	}
	// 发现不完整时要说出来（不能据此判定不见了）
	pfrtNoErr(t, os.Symlink(f.root, filepath.Join(f.root, "环")))
	facts, err = f.cl.Facts(ctx, PFRemoteUnit{Code: "260403J9999", Rel: "2026/260403J9999-无"}, PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if facts.Exists || !facts.Incomplete {
		t.Fatalf("应标发现不完整: %+v", facts)
	}
}

func TestPFRemoteFactsDup(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	f.putIn("2025/260403J1001-副本", "b.txt", "B")
	facts, err := f.cl.Facts(context.Background(), f.unit(), PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if !facts.Exists || !facts.Dup || !reflect.DeepEqual(facts.DupRels, []string{"2025/260403J1001-副本"}) {
		t.Fatalf("%+v", facts)
	}
}

// §14.4：单元键带 R（另配目录单元）时，Ubuntu 按 rel 配对、比对编号用键的前段；
// 同编号的别的目录（这里是主目录）进 Found，不影响这个单元。
func TestPFRemoteFactsUnitKeyR(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "主")
	const relR2 = "2026/260403J1001-另配"
	f.putIn(relR2, "r.txt", "副")
	u := PFRemoteUnit{Code: pfrtCode + "R2", Rel: relR2}
	facts, err := f.cl.Facts(context.Background(), u, PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if !facts.Exists || facts.UnitPath != filepath.Join(f.root, filepath.FromSlash(relR2)) {
		t.Fatalf("R2 键应按 rel 配上另配目录: %+v", facts)
	}
	if !facts.Dup || !reflect.DeepEqual(facts.DupRels, []string{pfrtRel}) || !reflect.DeepEqual(facts.Found, []string{pfrtRel}) {
		t.Fatalf("主目录应作为同编号的别的目录报出: %+v", facts)
	}
	if facts.Scan == nil || len(facts.Scan.Items) != 1 || facts.Scan.Items[0].Path != "r.txt" {
		t.Fatalf("扫描的应是另配目录自己的内容: %+v", facts.Scan)
	}
	// rel 在 Ubuntu 上没有（编号的别处有）：Exists=false，Found 带出同编号的目录
	none, err := f.cl.Facts(context.Background(), PFRemoteUnit{Code: pfrtCode + "R3", Rel: "2026/260403J1001-不存在"}, PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if none.Exists || len(none.Found) != 2 {
		t.Fatalf("rel 不在应 Exists=false 且报出同编号的两个目录: %+v", none)
	}
}

func TestPFRemoteFactsTooLarge(t *testing.T) {
	f := newPFRTFix(t)
	for i := 0; i < 10; i++ {
		f.put(fmt.Sprintf("f%d.txt", i), "x")
	}
	facts, err := f.cl.Facts(context.Background(), f.unit(), PFRemoteFactsOptions{MaxItems: 5})
	pfrtNoErr(t, err)
	if !facts.Exists || !facts.TooLarge || facts.Scan != nil || facts.TotalItems != 10 || !pfValidSHA(facts.Fingerprint) || facts.Summary.Files != 10 {
		t.Fatalf("单元过大应只报数量、不带内容: %+v", facts)
	}
	// 没超就正常
	facts, err = f.cl.Facts(context.Background(), f.unit(), PFRemoteFactsOptions{MaxItems: 10})
	pfrtNoErr(t, err)
	if facts.TooLarge || len(facts.Scan.Items) != 10 {
		t.Fatalf("%+v", facts)
	}
}

func TestPFRemoteFactsPaging(t *testing.T) {
	f := newPFRTFix(t)
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		f.put(fmt.Sprintf("目录%d/文件%03d.txt", i%7, i), fmt.Sprintf("内容%d", i))
	}
	for i := 0; i < 7; i++ {
		f.put(fmt.Sprintf("目录%d/.DS_Store", i), "x")
	}
	f.put("目录3/~$文件003.txt", "lock")

	paged, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{PageBytes: 2048})
	pfrtNoErr(t, err)
	if paged.Pages < 10 {
		t.Fatalf("应翻很多页，实际 %d 页", paged.Pages)
	}
	if len(paged.Scan.Items) != 307 || paged.TotalItems != 307 || len(paged.Names) != 8 {
		t.Fatalf("条目 %d 名字 %d", len(paged.Scan.Items), len(paged.Names))
	}
	full, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if full.Pages != 1 {
		t.Fatalf("不分页应只有 1 页: %d", full.Pages)
	}
	if paged.Fingerprint != full.Fingerprint || !reflect.DeepEqual(paged.Names, full.Names) || len(paged.Scan.Items) != len(full.Scan.Items) {
		t.Fatal("分页与不分页的结果不一致")
	}
	for i := range full.Scan.Items {
		a, b := paged.Scan.Items[i], full.Scan.Items[i]
		a.FromCache, b.FromCache = false, false
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("条目 %d 不同:\n %+v\n %+v", i, a, b)
		}
		if b.Kind == PFKindFile && b.Hash == "" {
			t.Fatalf("%s 没有哈希", b.Path)
		}
	}
	// 每页的哈希统计加起来：300 个文件各算一次（后面的页不重算前面的）
	if paged.HashStats.Hashed != 300 {
		t.Fatalf("各页合计应哈希 300 个文件: %+v", paged.HashStats)
	}
}

func TestPFRemoteFactsPagingUnstable(t *testing.T) {
	f := newPFRTFix(t)
	for i := 0; i < 100; i++ {
		f.put(fmt.Sprintf("f%03d.txt", i), fmt.Sprintf("内容%d", i))
	}
	f.run.after = func(n int) {
		if n == 1 { // 第一页之后，单元里的文件变了
			f.put("f050.txt", "变了变了变了")
		}
	}
	_, err := f.cl.Facts(context.Background(), f.unit(), PFRemoteFactsOptions{PageBytes: 2048})
	if !errors.Is(err, ErrPFRemoteUnstable) || PFRemoteIsUncertain(err) {
		t.Fatalf("翻页期间单元变了应报对不上（不是通信失败）: %v", err)
	}
}

// 翻页游标越界（单元缩小了）也按对不上处理
func TestPFRemoteFactsCursorOutOfRange(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	out, err := f.rawServe(PFRemoteSubFacts, PFRemoteFactsRequest{Version: 1, Meta: f.meta, Root: f.root, Code: pfrtCode, Rel: pfrtRel, AfterItem: 50})
	pfrtNoErr(t, err)
	var resp PFRemoteFactsResponse
	pfrtNoErr(t, json.Unmarshal(out, &resp))
	if resp.Error == "" || len(resp.Items) != 0 {
		t.Fatalf("%+v", resp)
	}
}
