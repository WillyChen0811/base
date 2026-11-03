//go:build itest

// Package itest：项目文件同步的两机联调（T14a），只在 -tags itest 下编译。
//
// Studio 测试机扮演 MacBook：work_dir（桌面）/sync_dir/meta 全在 t.TempDir() 下；
// 经 SSH+SFTP 真连 Ubuntu 测试机（地址、账号、钥匙、主机公钥、同步根、meta、peerctl 路径全部由
// 环境变量 PROJFILES_ITEST_* 注入，缺任何一个整体 t.Skip）。
// 主机校验固定用注入的公钥（FixedHostKey），绝不 InsecureIgnoreHostKey；连不上/校验不过如实失败。
//
// 接线照 wgtund/projfiles.go 的 build()：PFRemoteClient(Runner+RemoteCmd+Meta+Root+CoordID) →
// PFTransfer(LocalStaging/RemoteStaging/Remote=PFTransferFS(SFTP)/VerifyUp=remote.TargetVerify()) →
// PFSched(MetaDir/WorkDir/Remote/Transfer/Mover/Settings)。
// RemoteRunner 每次 Run 新开一条 SSH 连接和会话（不共享长连接，联调要的是每次都走真握手）。
package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/time/rate"
)

// itestAllowedPrefix 是 Ubuntu 端允许清场的路径前缀：rm 目标必须以此开头且不含 ".."，否则 t.Fatal。
const itestAllowedPrefix = "/home/chenwy/projfiles-itest/"

// ── 连接与执行 ──────────────────────────────────────────

// itRunner 实现 projsync.RemoteRunner：每次 Run 新建 SSH 连接、新开会话，stdin 原样送入。
type itRunner struct {
	addr string
	cfg  *ssh.ClientConfig
}

func (r *itRunner) Run(ctx context.Context, cmd string, stdin []byte) (stdout, stderr []byte, exit int, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, -1, err
	}
	c, err := ssh.Dial("tcp", r.addr, r.cfg)
	if err != nil {
		return nil, nil, -1, fmt.Errorf("SSH 连 %s 失败: %w", r.addr, err)
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		return nil, nil, -1, err
	}
	defer s.Close()
	var ob, eb bytes.Buffer
	if stdin != nil {
		s.Stdin = bytes.NewReader(stdin)
	}
	s.Stdout, s.Stderr = &ob, &eb
	err = s.Run(cmd)
	if ee, ok := err.(*ssh.ExitError); ok {
		return ob.Bytes(), eb.Bytes(), ee.ExitStatus(), nil
	}
	if err != nil {
		return ob.Bytes(), eb.Bytes(), -1, err
	}
	return ob.Bytes(), eb.Bytes(), 0, nil
}

// ── 环境 ────────────────────────────────────────────────

// itEnv 是一次联调的 Ubuntu 侧现场：连接、同步根、meta、peerctl。
type itEnv struct {
	t      *testing.T
	addr   string
	ssh    *ssh.Client
	sf     *sftp.Client
	runner *itRunner
	root   string // Ubuntu 同步根（roots.json 已包含）
	meta   string // Ubuntu meta（除 roots.json 外每场景清空）
	msg    string // Ubuntu 上的 linux 版 peerctl
}

func envReq(t *testing.T, name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Skipf("环境变量 %s 缺失：跳过两机联调", name)
	}
	return v
}

// newEnv 读环境变量并真连 Ubuntu。主机公钥必须与 PROJFILES_ITEST_HOSTKEY 一致。
func newEnv(t *testing.T) *itEnv {
	t.Helper()
	return newEnvAddr(t, envReq(t, "PROJFILES_ITEST_ADDR"))
}

// newEnvAddr 连指定地址（真机直连，或经 itProxy 本机代理转发到同一台 Ubuntu）。
// 其余连接要素（用户/钥匙/主机公钥）与环境变量注入的一致，主机校验不放宽——代理只搬 TCP 字节。
func newEnvAddr(t *testing.T, addr string) *itEnv {
	t.Helper()
	e := &itEnv{
		t:    t,
		addr: addr,
		root: envReq(t, "PROJFILES_ITEST_ROOT"),
		meta: envReq(t, "PROJFILES_ITEST_META"),
		msg:  envReq(t, "PROJFILES_ITEST_MSG"),
	}
	user := envReq(t, "PROJFILES_ITEST_USER")
	keyPEM, err := base64.StdEncoding.DecodeString(envReq(t, "PROJFILES_ITEST_KEY_B64"))
	if err != nil {
		t.Fatalf("PROJFILES_ITEST_KEY_B64 不是合法 base64：%v", err)
	}
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err != nil {
		t.Fatalf("解析 SSH 私钥失败：%v", err)
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(envReq(t, "PROJFILES_ITEST_HOSTKEY")))
	if err != nil {
		t.Fatalf("PROJFILES_ITEST_HOSTKEY 不是合法授权公钥行：%v", err)
	}
	e.runner = &itRunner{addr: e.addr, cfg: &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(hostKey), // 必须校验主机；不匹配直接失败
		// 诊断（r1027123628）：Ubuntu 默认协商出示 ecdsa-sha2-nistp256，注入的是 ed25519 公钥。
		// 钉住 ed25519 让对方出示同型钥匙，FixedHostKey 仍逐字节比对——只对齐算法协商，不放宽校验。
		HostKeyAlgorithms: []string{"ssh-ed25519"},
		Timeout:           15 * time.Second,
	}}
	c, err := ssh.Dial("tcp", e.addr, e.runner.cfg)
	if err != nil {
		t.Fatalf("SSH 连 %s 失败（主机校验/网络如实报告，不绕过）：%v", e.addr, err)
	}
	e.ssh = c
	sf, err := sftp.NewClient(c)
	if err != nil {
		t.Fatalf("开 SFTP 会话失败：%v", err)
	}
	e.sf = sf
	for _, p := range []string{e.root, e.meta} {
		if err := itestSafePath(p); err != nil {
			t.Fatalf("注入的 Ubuntu 路径不合规：%v", err)
		}
	}
	t.Cleanup(func() { sf.Close(); c.Close() })
	return e
}

// itestSafePath 校验 Ubuntu 端清理目标：必须落在联调专用前缀下、不含 ".."。
func itestSafePath(p string) error {
	if !strings.HasPrefix(p, itestAllowedPrefix) {
		return fmt.Errorf("路径 %q 不在联调前缀 %s 下", p, itestAllowedPrefix)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("路径 %q 含 ..", p)
	}
	return nil
}

// sh 在 Ubuntu 上跑一条 shell 命令（失败即 t.Fatal，带 stdout/stderr）。
func (e *itEnv) sh(ctx context.Context, cmd string) {
	e.t.Helper()
	stdout, stderr, exit, err := e.runner.Run(ctx, cmd, nil)
	if err != nil || exit != 0 {
		e.t.Fatalf("Ubuntu 命令失败（exit=%d err=%v）：%s\nstdout:%s\nstderr:%s", exit, err, cmd, stdout, stderr)
	}
}

// cleanUbuntu 场景开始前清场：root 清空；meta 清空但 roots.json 原样保留（不删不改）。
func (e *itEnv) cleanUbuntu(ctx context.Context) {
	e.t.Helper()
	if err := itestSafePath(e.root); err != nil {
		e.t.Fatalf("清场目标不合规：%v", err)
	}
	if err := itestSafePath(e.meta); err != nil {
		e.t.Fatalf("清场目标不合规：%v", err)
	}
	// .[^.]* / ..?* 补上 * 盖不到的隐藏项；不存在的通配字面量 rm -rf 静默跳过。
	e.sh(ctx, fmt.Sprintf(`rm -rf -- %s/* %s/.[^.]* %s/..?*`, quoteShell(e.root), quoteShell(e.root), quoteShell(e.root)))
	e.sh(ctx, fmt.Sprintf(`find %s -mindepth 1 -maxdepth 1 ! -name roots.json -exec rm -rf {} +`, quoteShell(e.meta)))
}

// quoteShell 单引号包住（联调路径都是固定 ASCII 白名单字符，出现单引号直接拒）。
func quoteShell(s string) string {
	if strings.ContainsAny(s, "'\n") {
		panic(fmt.Sprintf("联调路径含不允许的字符: %q", s))
	}
	return "'" + s + "'"
}

// ── PFTransferFS 的 SFTP 实现（照 wgtund/projfiles.go 的 projfilesRemoteFS 抄） ──

type sftpFS struct{ c *sftp.Client }

func (fs *sftpFS) Open(p string) (projsync.PFTransferFile, error) { return fs.c.Open(p) }

func (fs *sftpFS) OpenFile(p string, flag int) (projsync.PFTransferFile, error) {
	return fs.c.OpenFile(p, flag)
}

func (fs *sftpFS) Stat(p string) (os.FileInfo, error) { return fs.c.Stat(p) }

func (fs *sftpFS) Remove(p string) error { return fs.c.Remove(p) }

func (fs *sftpFS) MkdirAll(p string) error { return fs.c.MkdirAll(p) }

func (fs *sftpFS) ReadDir(p string) ([]os.FileInfo, error) { return fs.c.ReadDir(p) }

func (fs *sftpFS) Chtimes(p string, atime, mtime time.Time) error {
	return fs.c.Chtimes(p, atime, mtime)
}

func (fs *sftpFS) Join(elem ...string) string { return filepath.ToSlash(filepath.Join(elem...)) }

// ── 两边文件清单与 sha256 ──────────────────────────────

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// macTree 列本机目录树里全部普通文件：相对路径（/ 连接）→ sha256。符号链接跳过。
func macTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = sha256Hex(b)
		return nil
	})
	if err != nil {
		t.Fatalf("列本机 %s 失败：%v", root, err)
	}
	return out
}

// ubuntuTree 经 SFTP 列 Ubuntu 目录树：相对路径 → sha256。
func ubuntuTree(t *testing.T, sf *sftp.Client, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := sf.ReadDir(dir)
		if err != nil {
			t.Fatalf("列 Ubuntu %s 失败：%v", dir, err)
		}
		for _, e := range entries {
			p, r := dir+"/"+e.Name(), rel+e.Name()
			switch {
			case e.IsDir():
				walk(p, r+"/")
			case e.Mode().IsRegular():
				f, err := sf.Open(p)
				if err != nil {
					t.Fatalf("打开 Ubuntu %s 失败：%v", p, err)
				}
				h := sha256.New()
				if _, err := io.Copy(h, f); err != nil {
					f.Close()
					t.Fatalf("读 Ubuntu %s 失败：%v", p, err)
				}
				f.Close()
				out[r] = hex.EncodeToString(h.Sum(nil))
			}
		}
	}
	if _, err := sf.Stat(root); err != nil {
		t.Fatalf("Ubuntu 上没有 %s：%v", root, err)
	}
	walk(strings.TrimRight(root, "/"), "")
	return out
}

// ubuntuPaths 递归列目录里所有条目的相对路径（含目录，不读内容）；目录不存在返回空。
func ubuntuPaths(t *testing.T, sf *sftp.Client, root string) []string {
	t.Helper()
	var out []string
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := sf.ReadDir(dir)
		if err != nil {
			return // 回收区可能还没建
		}
		for _, e := range entries {
			p, r := dir+"/"+e.Name(), rel+e.Name()
			out = append(out, r)
			if e.IsDir() {
				walk(p, r+"/")
			}
		}
	}
	walk(root, "")
	sort.Strings(out)
	return out
}

// ubuntuCat 经 SFTP 读一个文件的文本；不存在返回 ""。
func ubuntuCat(t *testing.T, sf *sftp.Client, p string) string {
	t.Helper()
	f, err := sf.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("读 Ubuntu %s 失败：%v", p, err)
	}
	return string(b)
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cmpTree 比较两边清单：A 有 B 没有 / B 有 A 没有 / 都有但内容不同。
func cmpTree(a, b map[string]string) (onlyA, onlyB, differ []string) {
	for k, v := range a {
		if w, ok := b[k]; !ok {
			onlyA = append(onlyA, k)
		} else if w != v {
			differ = append(differ, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			onlyB = append(onlyB, k)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	sort.Strings(differ)
	return
}

// ── cluster：一台「Mac」（本机临时目录） + Ubuntu 的完整一轮 ──

type cluster struct {
	t    *testing.T
	env  *itEnv
	work string // 桌面（Mac）
	sync string // 同步目录（Mac）
	meta string // Mac meta
	mob  *projsync.PFUnitMover
	tr   *projsync.PFTransfer
	sch  *projsync.PFSched
}

// itOpt 是建 cluster 时对调度器参数的场景化微调（只动测试注入的 Settings，不碰生产行为）。
type itOpt func(*projsync.PFSchedSettings)

// withMaxExpensive 把热点/未知网络的暂缓阈值调小（缺省 20 秒≈100MB，联调不想造 100MB 文件）。
func withMaxExpensive(d time.Duration) itOpt {
	return func(s *projsync.PFSchedSettings) { s.Limits.MaxExpensive = d }
}

// withQuietSeconds 改「最近写入」门窗口（S16「写到一半」要几十秒级窗口才不靠赛跑）。
func withQuietSeconds(n int) itOpt {
	return func(s *projsync.PFSchedSettings) { s.QuietSeconds = n }
}

func newCluster(t *testing.T, opts ...itOpt) *cluster {
	t.Helper()
	return newClusterAddr(t, envReq(t, "PROJFILES_ITEST_ADDR"), opts...)
}

// newClusterAddr 同 newCluster，但把 SSH/SFTP 的连接地址指给调用方（经 itProxy 时用）。
func newClusterAddr(t *testing.T, addr string, opts ...itOpt) *cluster {
	t.Helper()
	e := newEnvAddr(t, addr)
	tdir := t.TempDir()
	c := &cluster{t: t, env: e,
		work: filepath.Join(tdir, "work"),
		sync: filepath.Join(tdir, "sync"),
		meta: filepath.Join(tdir, "meta")}
	for _, d := range []string{c.work, c.sync, c.meta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("建 %s 失败：%v", d, err)
		}
	}
	// 开始同步的闸一律要读清单（路径为空构造就失败）：联调里谁都没配置，给一份空清单
	pathsList := filepath.Join(tdir, "projfiles-paths.json")
	if err := os.WriteFile(pathsList, []byte(`{"version":1,"units":[]}`), 0o644); err != nil {
		t.Fatalf("写配置路径清单失败：%v", err)
	}
	mob, err := projsync.NewPFUnitMover(projsync.PFUnitMoveConfig{
		WorkDir: c.work, SyncDir: c.sync, Meta: c.meta, PathsList: pathsList,
		QuietSeconds: 1, StableGap: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("建 PFUnitMover 失败：%v", err)
	}
	remote := &projsync.PFRemoteClient{
		Runner: e.runner, RemoteCmd: e.msg, Meta: e.meta, Root: e.root,
		CoordID: "itest-mac", CoordHost: "studio-itest",
	}
	transfer, err := projsync.NewPFTransfer(projsync.PFTransferConfig{
		LocalStaging:  filepath.Join(c.meta, "staging"),
		RemoteStaging: projsync.PFRemoteStagingDir(e.meta),
		Remote:        &sftpFS{c: e.sf},
		VerifyUp:      remote.TargetVerify(),
	})
	if err != nil {
		t.Fatalf("建 PFTransfer 失败：%v", err)
	}
	set := projsync.PFSchedSettings{Net: projsync.PFRemoteNetGood, QuietSeconds: 1, Host: "studio-itest"}
	for _, o := range opts {
		o(&set)
	}
	sch, err := projsync.NewPFSched(projsync.PFSchedConfig{
		MetaDir: c.meta, WorkDir: c.work, Remote: remote, Transfer: transfer, Mover: mob,
		Settings: set,
	})
	if err != nil {
		t.Fatalf("建 PFSched 失败：%v", err)
	}
	t.Cleanup(sch.Close)
	c.mob, c.tr, c.sch = mob, transfer, sch
	c.cleanUbuntu(context.Background())
	return c
}

// redialRemote 是「掐断后续传」场景的现场恢复：重开 SSH 长连接和 SFTP 会话交给传输层
// （生产 SetRemote 的语义：只对之后开始的传输生效，所以必须在跑续传轮之前调用）。
// 旧连接就地关闭；newEnvAddr 里登记的 t.Cleanup 会对旧连接再关一次，无害。
func (c *cluster) redialRemote() {
	c.t.Helper()
	sshC, err := ssh.Dial("tcp", c.env.addr, c.env.runner.cfg)
	if err != nil {
		c.t.Fatalf("重连 SSH %s 失败：%v", c.env.addr, err)
	}
	sf, err := sftp.NewClient(sshC)
	if err != nil {
		sshC.Close()
		c.t.Fatalf("重连后开 SFTP 会话失败：%v", err)
	}
	c.env.sf.Close()
	c.env.ssh.Close()
	c.env.ssh, c.env.sf = sshC, sf
	c.tr.SetRemote(&sftpFS{c: sf})
	c.t.Logf("已重连 SSH+SFTP 并换进传输层（%s）", c.env.addr)
}

func (c *cluster) cleanUbuntu(ctx context.Context) { c.env.cleanUbuntu(ctx) }

// startUnit 按 PFUnitMover 的规矩立项：在 work_dir 写好多半真实文件 → mtime 拨到两小时前（免「最近写入」）→
// StartSync 整挪进 sync_dir 并在桌面留链接。
func (c *cluster) startUnit(rel string, files map[string][]byte) {
	c.t.Helper()
	ctx := context.Background()
	writeTree(c.t, filepath.Join(c.work, filepath.FromSlash(rel)), files)
	quietPast(c.t, filepath.Join(c.work, filepath.FromSlash(rel)))
	res, err := c.mob.StartSync(ctx, rel)
	if err != nil {
		c.t.Fatalf("StartSync(%s) 出错：%v", rel, err)
	}
	if res.Status != projsync.PFUnitMoveDone {
		c.t.Fatalf("StartSync(%s) 没做成：status=%s reason=%s refusals=%s", rel, res.Status, res.Reason, jsonOf(res.Refusals))
	}
}

// round 跑一整轮并打完整报告；outcome 不是 done 直接失败（带单元明细）。
func (c *cluster) round(full bool) *projsync.PFSchedReport {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	rep := c.sch.RunRound(ctx, projsync.PFSchedRoundOptions{Full: full})
	c.t.Logf("本轮报告：%s", jsonOf(rep))
	if rep.Outcome != projsync.PFSchedDone {
		c.t.Fatalf("本轮 outcome=%s reason=%s", rep.Outcome, rep.Reason)
	}
	for _, u := range rep.Units {
		if u.Result != nil && len(u.Result.Errors) > 0 {
			c.t.Fatalf("单元 %s 有错误：%s", u.Code, jsonOf(u.Result.Errors))
		}
	}
	return rep
}

// unitResult 取报告里某编号的单元结果。
func unitResult(rep *projsync.PFSchedReport, code string) *projsync.PFRoundResult {
	for _, u := range rep.Units {
		if u.Code == code && u.Result != nil {
			return u.Result
		}
	}
	return nil
}

// macFilesOf 列 Mac 上 <sync>/<rel> 全部文件。
func (c *cluster) macFilesOf(rel string) map[string]string {
	return macTree(c.t, filepath.Join(c.sync, filepath.FromSlash(rel)))
}

// ubFilesOf 列 Ubuntu 上 <root>/<rel> 全部文件。
func (c *cluster) ubFilesOf(rel string) map[string]string {
	return ubuntuTree(c.t, c.env.sf, c.env.root+"/"+rel)
}

// writeMacInSync 往 Mac 的同步目录（桌面链接背后那份）写/改文件，mtime 拨回过去以躲「最近写入」门。
func (c *cluster) writeMacInSync(rel string, files map[string][]byte) {
	c.t.Helper()
	writeTree(c.t, filepath.Join(c.sync, filepath.FromSlash(rel)), files)
}

// writeUb 经 SFTP 往 Ubuntu <root>/<rel> 下写文件（mtime 即当前，之后场景 sleep 过安静期）。
func (c *cluster) writeUb(rel string, files map[string][]byte) {
	c.t.Helper()
	for r, b := range files {
		p := c.env.root + "/" + rel + "/" + r
		if i := strings.LastIndex(p, "/"); i > 0 {
			if err := c.env.sf.MkdirAll(p[:i]); err != nil {
				c.t.Fatalf("Ubuntu MkdirAll %s 失败：%v", p[:i], err)
			}
		}
		f, err := c.env.sf.Create(p)
		if err != nil {
			c.t.Fatalf("Ubuntu Create %s 失败：%v", p, err)
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			c.t.Fatalf("Ubuntu 写 %s 失败：%v", p, err)
		}
		f.Close()
	}
}

// sleepQuiet 过安静期（Settings.QuietSeconds=1，等 1.5 秒）。
func (c *cluster) sleepQuiet() { time.Sleep(1500 * time.Millisecond) }

// macExcluded 用生产同一套扫描+排除规则扫 Mac 侧单元，返回被排除（不同步、只计数）的路径。
func (c *cluster) macExcluded(rel string) []string {
	c.t.Helper()
	var out []string
	opt := projsync.PFScanOptions{Filter: projsync.PFRemoteScanFilter(), Now: time.Now, QuietSeconds: 60}
	opt.OnExcluded = func(p string, isDir bool) { out = append(out, p) }
	if _, err := projsync.PFScan(context.Background(), filepath.Join(c.sync, filepath.FromSlash(rel)), opt); err != nil {
		c.t.Fatalf("扫 Mac 侧排除清单失败：%v", err)
	}
	sort.Strings(out)
	return out
}

// assertAligned 断言两边树一致；不一致时把两边清单打进日志。
func (c *cluster) assertAligned(rel string, rep *projsync.PFSchedReport) {
	c.t.Helper()
	mac, ub := c.macFilesOf(rel), c.ubFilesOf(rel)
	onlyA, onlyB, differ := cmpTree(mac, ub)
	if len(onlyA)+len(onlyB)+len(differ) > 0 {
		c.t.Errorf("%s 两边不一致：Mac 独有 %v，Ubuntu 独有 %v，内容不同 %v", rel, onlyA, onlyB, differ)
		c.t.Logf("Mac 清单：%s", jsonOf(mapKeys(mac)))
		c.t.Logf("Ubuntu 清单：%s", jsonOf(mapKeys(ub)))
		c.t.Logf("本轮报告：%s", jsonOf(rep))
	}
}

// ── 小工具 ──────────────────────────────────────────────

func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, b := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll %s 失败：%v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatalf("写 %s 失败：%v", p, err)
		}
	}
}

// quietPast 把整棵树的 mtime 拨到两小时前（StartSync/轮内「最近写入」门都认 mtime）。
func quietPast(t *testing.T, root string) {
	t.Helper()
	past := time.Now().Add(-2 * time.Hour)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, past, past)
	})
	if err != nil {
		t.Fatalf("拨 mtime 失败：%v", err)
	}
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<序列化失败 %v>", err)
	}
	return string(b)
}

func bigFile(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

// ── itProxy：本机 TCP 代理（T14c 场景的网络现场）─────────────────
//
// Studio 本机监听 127.0.0.1:<随机端口>，透明转发到 target（真实 Ubuntu SSH）。
// 只搬 TCP 字节、不碰 SSH/SFTP 语义，主机公钥校验照常。SSH/SFTP 改连 px.Addr() 即可。
//   - SetLimits(up, down)：两个方向分别限每秒字节上限（0 = 不限），令牌桶所有连接共享（同一条"网线"）；
//   - SetDelay(d)：每搬一块固定加延迟；
//   - Cut()：掐断当前所有连接，之后新连接照常（断线续传场景用）；
//   - SetReject(true)：接受后立刻关闭，握手瞬间失败——模拟"对方不在线"，不让调用方干等拨号超时；
//   - Up()/Down()：已转发的字节计数（up = 客户端→Ubuntu，down = Ubuntu→客户端）。
//
// 只在测试代码里，生产不感知（生产限速/测速见 PFTransfer.SetLimit / PFSpeed）。
const itProxyChunk = 64 << 10 // 代理搬运块 = 限速粒度（与生产 pfTrLimiterBurst 同量级）

type itProxy struct {
	t      *testing.T
	target string
	ln     net.Listener

	up, down       atomic.Int64
	limitUp, limDn atomic.Int64 // 每秒字节上限，0 = 不限（计数与限速各自独立）
	delayMs        atomic.Int64
	reject         atomic.Bool

	lims  [2]*rate.Limiter // [0]=up [1]=down
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func newITProxy(t *testing.T, target string) *itProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起本机 TCP 代理失败：%v", err)
	}
	p := &itProxy{t: t, target: target, ln: ln, conns: map[net.Conn]struct{}{}}
	for i := range p.lims {
		p.lims[i] = rate.NewLimiter(rate.Inf, itProxyChunk)
	}
	p.wg.Add(1)
	go p.acceptLoop()
	t.Cleanup(p.Close)
	t.Logf("TCP 代理就绪：%s → %s", ln.Addr(), target)
	return p
}

// Addr 是 SSH/SFTP 该连的地址（host:port）。
func (p *itProxy) Addr() string { return p.ln.Addr().String() }

// SetLimits 设两方向每秒字节上限（0 = 不限）；进行中的连接下一次搬运起生效。
func (p *itProxy) SetLimits(up, down int64) {
	p.limitUp.Store(up)
	p.limDn.Store(down)
	p.apply(0, up)
	p.apply(1, down)
}

func (p *itProxy) apply(i int, bps int64) {
	if bps <= 0 {
		p.lims[i].SetLimit(rate.Inf)
	} else {
		p.lims[i].SetLimit(rate.Limit(bps))
	}
	p.lims[i].SetBurst(itProxyChunk)
}

// SetDelay 每搬一块固定加延迟（0 = 不加）。
func (p *itProxy) SetDelay(d time.Duration) { p.delayMs.Store(int64(d / time.Millisecond)) }

// SetReject 开关"拒绝新连接"：开时接受后立刻关闭，握手快速失败。
func (p *itProxy) SetReject(b bool) { p.reject.Store(b) }

// Up/Down 是两方向已转发字节；ResetCounts 清零（场景分段计量用）。
func (p *itProxy) Up() int64   { return p.up.Load() }
func (p *itProxy) Down() int64 { return p.down.Load() }
func (p *itProxy) ResetCounts() { p.up.Store(0); p.down.Store(0) }

// Cut 掐断当前所有连接（计数和暂存区状态都保留；之后新连接照常）。
func (p *itProxy) Cut() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	p.t.Logf("代理掐断 %d 条连接（up=%d down=%d 字节）", len(conns), p.up.Load(), p.down.Load())
}

func (p *itProxy) track(c net.Conn) {
	p.mu.Lock()
	p.conns[c] = struct{}{}
	p.mu.Unlock()
}

func (p *itProxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

func (p *itProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		cli, err := p.ln.Accept()
		if err != nil {
			return // listener 已关（Close）
		}
		if p.reject.Load() {
			cli.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			// 代理连不上目标：如实让客户端也连不上（不缓存不重连）
			p.t.Logf("代理连目标 %s 失败：%v", p.target, err)
			cli.Close()
			continue
		}
		p.track(cli)
		p.track(up)
		p.wg.Add(2)
		go func() { defer p.wg.Done(); p.pump(up, cli, p.lims[0], &p.up); cli.Close(); up.Close(); p.untrack(cli); p.untrack(up) }()
		go func() { defer p.wg.Done(); p.pump(cli, up, p.lims[1], &p.down); up.Close(); cli.Close(); p.untrack(up); p.untrack(cli) }()
	}
}

// pump 把 src 搬到 dst：每块过令牌桶、可加固定延迟、按方向计数。任何一侧出错即止。
func (p *itProxy) pump(dst, src net.Conn, lim *rate.Limiter, cnt *atomic.Int64) {
	buf := make([]byte, itProxyChunk)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if err := lim.WaitN(context.Background(), n); err != nil {
				return // n ≤ burst，只有桶被改坏才到这
			}
			if d := p.delayMs.Load(); d > 0 {
				time.Sleep(time.Duration(d) * time.Millisecond)
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
			cnt.Add(int64(n))
		}
		if rerr != nil {
			return
		}
	}
}

// Close 停代理：不再接受新连接，掐掉在途的，等搬运 goroutine 退干净。
func (p *itProxy) Close() {
	p.ln.Close()
	p.Cut()
	p.wg.Wait()
}
