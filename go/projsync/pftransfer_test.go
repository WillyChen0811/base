package projsync

// 传输层测试：用进程内 SFTP 服务（sftp.NewServer + net.Pipe），不连真机器。
// 进程内 SFTP 服务直接读写本机文件系统：「远端」的路径就是本机临时目录里的路径，
// 所以测试里能直接看远端暂存区里的文件、也能直接改它。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

// ── 掐断连接 ────────────────────────────────────────────

// pfCutConn 包住客户端一侧的连接：客户端累计写出超过 limit 字节就把连接两头都关掉（limit<0 不掐）。
type pfCutConn struct {
	net.Conn
	peer  net.Conn
	limit int64
	n     atomic.Int64
	cut   atomic.Bool
}

func (c *pfCutConn) Write(p []byte) (int, error) {
	if c.limit >= 0 && c.n.Add(int64(len(p))) > c.limit {
		c.cut.Store(true)
		c.Conn.Close()
		c.peer.Close()
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(p)
}

func pfTrDialSFTP(t *testing.T, cutAfter int64) (*sftp.Client, func()) {
	t.Helper()
	sc, cc := net.Pipe()
	srv, err := sftp.NewServer(sc)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	served := make(chan struct{})
	go func() { srv.Serve(); close(served) }()
	conn := &pfCutConn{Conn: cc, peer: sc, limit: cutAfter}
	client, err := sftp.NewClientPipe(conn, conn)
	if err != nil {
		t.Fatalf("NewClientPipe: %v", err)
	}
	return client, func() {
		client.Close()
		sc.Close()
		cc.Close()
		<-served
	}
}

// ── 观察「远端」写 part 的探针 ──────────────────────────

type pfTrSpy struct {
	PFTransferFS
	mu       sync.Mutex
	classify func(part string) string // part 路径 → 类别（统计并发用）
	active   map[string]int
	peak     map[string]int
	written  map[string]int64
	opened   []string
	gates    map[string]chan struct{} // part 路径 → 写之前要等的门
}

func newPFTrSpy(fs PFTransferFS) *pfTrSpy {
	return &pfTrSpy{PFTransferFS: fs, active: map[string]int{}, peak: map[string]int{}, written: map[string]int64{}, gates: map[string]chan struct{}{}}
}

func (s *pfTrSpy) OpenFile(p string, flag int) (PFTransferFile, error) {
	f, err := s.PFTransferFS.OpenFile(p, flag)
	if err != nil || !strings.HasSuffix(p, ".part") || flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		return f, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cls := ""
	if s.classify != nil {
		cls = s.classify(p)
	}
	s.active[cls]++
	s.peak[cls] = max(s.peak[cls], s.active[cls])
	seen := false
	for _, o := range s.opened {
		seen = seen || o == p
	}
	if !seen {
		s.opened = append(s.opened, p)
	}
	return &pfTrSpyFile{PFTransferFile: f, s: s, path: p, cls: cls}, nil
}

func (s *pfTrSpy) gate(part string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := make(chan struct{})
	s.gates[part] = g
	return g
}

func (s *pfTrSpy) writtenTotal() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, v := range s.written {
		n += v
	}
	return n
}

type pfTrSpyFile struct {
	PFTransferFile
	s      *pfTrSpy
	path   string
	cls    string
	closed bool
}

func (f *pfTrSpyFile) WriteAt(b []byte, off int64) (int, error) {
	f.s.mu.Lock()
	g := f.s.gates[f.path]
	f.s.mu.Unlock()
	if g != nil {
		<-g
	}
	n, err := f.PFTransferFile.WriteAt(b, off)
	f.s.mu.Lock()
	f.s.written[f.path] += int64(n)
	f.s.mu.Unlock()
	return n, err
}

func (f *pfTrSpyFile) Close() error {
	f.s.mu.Lock()
	if !f.closed {
		f.closed = true
		f.s.active[f.cls]--
	}
	f.s.mu.Unlock()
	return f.PFTransferFile.Close()
}

// ── 测试环境 ────────────────────────────────────────────

type pfTrEnv struct {
	t      *testing.T
	dir    string
	rstage string // 远端暂存区（上传的 part 在这里）
	lstage string // 本机暂存区（下载的 part 在这里）
	m      *PFTransfer
	spy    *pfTrSpy
	ctx    context.Context

	mu       sync.Mutex
	progress func(PFTransferProgress)
	order    []string // 核验顺序（sha）
	verify   func() error
	closers  []func()
}

const pfTrBlock = 1 << 20

func newPFTrEnv(t *testing.T, mod func(*PFTransferConfig)) *pfTrEnv {
	t.Helper()
	e := &pfTrEnv{t: t, dir: t.TempDir()}
	e.rstage = filepath.Join(e.dir, "rstage")
	e.lstage = filepath.Join(e.dir, "lstage")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	e.ctx = ctx
	t.Cleanup(cancel)

	cfg := PFTransferConfig{
		LocalStaging:  e.lstage,
		RemoteStaging: e.rstage,
		BlockSize:     pfTrBlock,
		IOChunk:       256 << 10,
		VerifyUp: func(ctx context.Context, p string) (string, int64, error) {
			e.mu.Lock()
			e.order = append(e.order, strings.TrimSuffix(filepath.Base(p), ".part"))
			vf := e.verify
			e.mu.Unlock()
			if vf != nil {
				if err := vf(); err != nil {
					return "", 0, err
				}
			}
			// 测试里「Ubuntu 本机」就是这台机器：直接算
			return PFHashLocalFile(ctx, p)
		},
		Progress: func(p PFTransferProgress) {
			e.mu.Lock()
			f := e.progress
			e.mu.Unlock()
			if f != nil {
				f(p)
			}
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	m, err := NewPFTransfer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	e.connect(-1)
	t.Cleanup(func() {
		m.Close()
		for _, c := range e.closers {
			c()
		}
	})
	return e
}

// connect 建一条新的「远端连接」（cutAfter>=0 时客户端写满这么多字节就被掐断）并换给传输层。
func (e *pfTrEnv) connect(cutAfter int64) *pfTrSpy {
	client, closer := pfTrDialSFTP(e.t, cutAfter)
	e.closers = append(e.closers, closer)
	e.spy = newPFTrSpy(PFSFTP(client))
	e.m.SetRemote(e.spy)
	return e.spy
}

func (e *pfTrEnv) setProgress(f func(PFTransferProgress)) {
	e.mu.Lock()
	e.progress = f
	e.mu.Unlock()
}

func (e *pfTrEnv) rpart(sha string) string { return filepath.Join(e.rstage, sha+".part") }
func (e *pfTrEnv) lpart(sha string) string { return filepath.Join(e.lstage, sha+".part") }

// file 在测试目录里写一个文件，返回路径。
func (e *pfTrEnv) file(name string, data []byte) string {
	p := filepath.Join(e.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func pfTrData(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func pfTrSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func pfTrUp(src string, data []byte, prio int) PFTransferJob {
	return PFTransferJob{Dir: PFUpload, Src: src, Size: int64(len(data)), SHA256: pfTrSHA(data), Priority: prio}
}

func pfTrDown(src string, data []byte, prio int) PFTransferJob {
	return PFTransferJob{Dir: PFDownload, Src: src, Size: int64(len(data)), SHA256: pfTrSHA(data), Priority: prio}
}

func pfTrKind(err error) PFErrKind {
	var pe *PFTransferError
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return 0
}

func pfTrMustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pfTrEqual(t *testing.T, p string, want []byte) {
	t.Helper()
	got := pfTrMustRead(t, p)
	if len(got) != len(want) || pfTrSHA(got) != pfTrSHA(want) {
		t.Fatalf("%s 的内容不对：大小 %d，期望 %d", p, len(got), len(want))
	}
}

func pfTrWaitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ── 基本传输 ────────────────────────────────────────────

func TestPFTransferUploadDownload(t *testing.T) {
	e := newPFTrEnv(t, nil)

	data := pfTrData(1, 3<<20+12345)
	src := e.file("src/a.bin", data)
	res, err := e.m.Transfer(e.ctx, pfTrUp(src, data, PFPrioNormal))
	if err != nil {
		t.Fatalf("上传: %v", err)
	}
	if res.PartPath != e.rpart(pfTrSHA(data)) {
		t.Errorf("PartPath = %s", res.PartPath)
	}
	pfTrEqual(t, res.PartPath, data)
	if res.Reused || res.SentBytes != int64(len(data)) || res.ResumedFrom != 0 || res.Attempts != 1 || res.Yields != 0 {
		t.Errorf("结果不对: %+v", res)
	}
	// meta：头 + 4 个块 + 核验行
	meta := parsePFTrMeta(pfTrMustRead(t, filepath.Join(e.rstage, pfTrSHA(data)+".meta")))
	if meta == nil || len(meta.blocks) != 4 || !meta.hasVer || meta.verSize != int64(len(data)) {
		t.Errorf("meta 不对: %+v", meta)
	}
	if meta != nil && (meta.blocks[0] != pfTrSHA(data[:pfTrBlock]) || meta.blocks[3] != pfTrSHA(data[3<<20:])) {
		t.Errorf("meta 里的块哈希不对")
	}

	// 下载：远端文件 → 本机暂存区
	data2 := pfTrData(2, 2<<20+777)
	rsrc := e.file("rsrc/b.bin", data2)
	res2, err := e.m.Transfer(e.ctx, pfTrDown(rsrc, data2, PFPrioNormal))
	if err != nil {
		t.Fatalf("下载: %v", err)
	}
	if res2.PartPath != e.lpart(pfTrSHA(data2)) {
		t.Errorf("PartPath = %s", res2.PartPath)
	}
	pfTrEqual(t, res2.PartPath, data2)
	if res2.SentBytes != int64(len(data2)) || res2.Attempts != 1 {
		t.Errorf("结果不对: %+v", res2)
	}
}

func TestPFTransferEmptyAndSmall(t *testing.T) {
	e := newPFTrEnv(t, nil)
	for i, data := range [][]byte{{}, []byte("x"), pfTrData(5, pfTrBlock), pfTrData(6, pfTrBlock+1)} {
		src := e.file("src/f"+string(rune('a'+i)), data)
		res, err := e.m.Transfer(e.ctx, pfTrUp(src, data, 0))
		if err != nil {
			t.Fatalf("大小 %d: %v", len(data), err)
		}
		pfTrEqual(t, res.PartPath, data)
	}
}

func TestPFTransferReuse(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(3, 2<<20+5)
	a := e.file("src/a.bin", data)
	b := e.file("src/copy-of-a.bin", data)

	if _, err := e.m.Transfer(e.ctx, pfTrUp(a, data, 0)); err != nil {
		t.Fatal(err)
	}
	before := e.spy.writtenTotal()
	res, err := e.m.Transfer(e.ctx, pfTrUp(b, data, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reused || res.SentBytes != 0 || e.spy.writtenTotal() != before || res.PartPath != e.rpart(pfTrSHA(data)) {
		t.Errorf("同内容第二次应直接复用: %+v，写入变化 %d", res, e.spy.writtenTotal()-before)
	}

	// part 被人动过（mtime 变了）：不能再当核验过的用，要重新核对，对不上就重传
	part := e.rpart(pfTrSHA(data))
	if err := os.WriteFile(part, pfTrData(99, len(data)), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(part, later, later)
	res, err = e.m.Transfer(e.ctx, pfTrUp(b, data, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused || res.ResumedFrom != 0 || res.SentBytes != int64(len(data)) {
		t.Errorf("part 被改过应从头重传: %+v", res)
	}
	pfTrEqual(t, part, data)
}

func TestPFTransferSameContentQueuedOnce(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(4, 3<<20)
	a := e.file("src/a.bin", data)
	b := e.file("src/b.bin", data)
	h1, err := e.m.Submit(e.ctx, pfTrUp(a, data, 0))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := e.m.Submit(e.ctx, pfTrUp(b, data, 0))
	if err != nil {
		t.Fatal(err)
	}
	r1, err1 := h1.Wait()
	r2, err2 := h2.Wait()
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if r1.SentBytes+r2.SentBytes != int64(len(data)) || (r1.Reused == r2.Reused) {
		t.Errorf("同一内容只能传一份: %+v / %+v", r1, r2)
	}
}

func TestPFTransferJobValidation(t *testing.T) {
	e := newPFTrEnv(t, nil)
	good := pfTrData(7, 10)
	cases := map[string]PFTransferJob{
		"sha 不对": {Dir: PFUpload, Src: "/x", Size: 1, SHA256: "ABC"},
		"大小为负":   {Dir: PFUpload, Src: "/x", Size: -1, SHA256: pfTrSHA(good)},
		"源路径为空":  {Dir: PFUpload, Src: "", Size: 1, SHA256: pfTrSHA(good)},
		"方向不对":   {Dir: 7, Src: "/x", Size: 1, SHA256: pfTrSHA(good)},
		"大写 sha": {Dir: PFUpload, Src: "/x", Size: 1, SHA256: strings.ToUpper(pfTrSHA(good))},
	}
	for name, job := range cases {
		if _, err := e.m.Submit(e.ctx, job); pfTrKind(err) != PFErrConfig {
			t.Errorf("%s：应报配置错误，得到 %v", name, err)
		}
	}
	// 上传没配核验函数
	m2, _ := NewPFTransfer(PFTransferConfig{RemoteStaging: e.rstage, LocalStaging: e.lstage, Remote: e.spy})
	defer m2.Close()
	if _, err := m2.Submit(e.ctx, pfTrUp("/x", good, 0)); pfTrKind(err) != PFErrConfig || !strings.Contains(err.Error(), "核验") {
		t.Errorf("没配 VerifyUp 应拒绝: %v", err)
	}
}

func TestPFTransferSourceMissing(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(8, 100)
	_, err := e.m.Transfer(e.ctx, pfTrUp(filepath.Join(e.dir, "nope"), data, 0))
	if pfTrKind(err) != PFErrSource {
		t.Errorf("上传源不存在应报源错误: %v", err)
	}
	_, err = e.m.Transfer(e.ctx, pfTrDown(filepath.Join(e.dir, "nope"), data, 0))
	if pfTrKind(err) != PFErrSource {
		t.Errorf("下载源不存在应报源错误: %v", err)
	}
}

// ── 断线续传 ────────────────────────────────────────────

// 在第 N 字节掐断连接，续传后实际重传的字节数小于 1 块（8MB 块）。
func TestPFTransferResumeAfterCut(t *testing.T) {
	e := newPFTrEnv(t, func(c *PFTransferConfig) { c.BlockSize = PFTransferDefaultBlock; c.IOChunk = 0 })
	const block = PFTransferDefaultBlock
	data := pfTrData(10, 40<<20+4321)
	src := e.file("src/big.bin", data)
	job := pfTrUp(src, data, 0)

	e.connect(21<<20 + 1000) // 客户端写出约 21MB 时掐断
	res1, err := e.m.Transfer(e.ctx, job)
	if err == nil {
		t.Fatal("连接被掐断，应该报错")
	}
	var pe *PFTransferError
	if !errors.As(err, &pe) || pe.Kind != PFErrComm || !pe.Uncertain() {
		t.Fatalf("应是通信错误（结果不确定）: %v", err)
	}
	t.Logf("掐断后的错误: %v", err)
	if res1.PartPath != "" {
		t.Errorf("出错时不应有 PartPath")
	}
	fi, err := os.Stat(e.rpart(job.SHA256))
	if err != nil {
		t.Fatal(err)
	}
	atCut := fi.Size()
	if atCut < 16<<20 || atCut > 22<<20 {
		t.Fatalf("掐断时 part 大小 %d 不在预期范围", atCut)
	}

	e.connect(-1) // 重连
	res2, err := e.m.Transfer(e.ctx, job)
	if err != nil {
		t.Fatalf("续传: %v", err)
	}
	pfTrEqual(t, res2.PartPath, data)
	if res2.ResumedFrom == 0 || res2.ResumedFrom%block != 0 {
		t.Errorf("应从块边界续传，实际 %d", res2.ResumedFrom)
	}
	resent := atCut - res2.ResumedFrom // 断线前已经写进 part、却要重传的字节
	if resent < 0 || resent >= block {
		t.Errorf("重传了 %d 字节，应小于 1 块（%d）", resent, block)
	}
	if res2.SentBytes != int64(len(data))-res2.ResumedFrom {
		t.Errorf("续传只应搬剩下的: SentBytes=%d", res2.SentBytes)
	}
	t.Logf("掐断时 part=%d，续传起点=%d，重传=%d 字节（块=%d）", atCut, res2.ResumedFrom, resent, block)
}

// 取消后再提交 = 续传（下载方向，part 在本机）。
func TestPFTransferDownloadResume(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(11, 5<<20+99)
	rsrc := e.file("rsrc/v.bin", data)
	job := pfTrDown(rsrc, data, 0)

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.setProgress(func(p PFTransferProgress) {
		if p.Block == 2 {
			cancel()
		}
	})
	res1, err := e.m.Transfer(ctx, job)
	if pfTrKind(err) != PFErrCanceled {
		t.Fatalf("应报已取消: %v", err)
	}
	if res1.SentBytes != 3<<20 {
		t.Errorf("取消前应搬了 3 块: %d", res1.SentBytes)
	}
	e.setProgress(nil)
	res2, err := e.m.Transfer(e.ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if res2.ResumedFrom != 3<<20 || res2.SentBytes != int64(len(data))-3<<20 {
		t.Errorf("应从 3MB 续传: %+v", res2)
	}
	pfTrEqual(t, res2.PartPath, data)
}

// 末块（最后一个已确认的块）被篡改：退一块后续传，结果正确。
func TestPFTransferTamperLastBlock(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(12, 6<<20+500)
	src := e.file("src/t.bin", data)
	job := pfTrUp(src, data, 0)

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.setProgress(func(p PFTransferProgress) {
		if p.Block == 3 { // 已确认 4 块
			cancel()
		}
	})
	if _, err := e.m.Transfer(ctx, job); pfTrKind(err) != PFErrCanceled {
		t.Fatalf("应报已取消: %v", err)
	}
	e.setProgress(nil)

	part := e.rpart(job.SHA256)
	f, err := os.OpenFile(part, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte{^data[3<<20+100]}, 3<<20+100) // 改第 4 块（下标 3）里的一个字节
	f.Close()

	res, err := e.m.Transfer(e.ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResumedFrom != 3<<20 {
		t.Errorf("末块对不上应退一块，从 3MB 续传，实际 %d", res.ResumedFrom)
	}
	if res.Attempts != 1 || res.SentBytes != int64(len(data))-3<<20 {
		t.Errorf("结果不对: %+v", res)
	}
	pfTrEqual(t, res.PartPath, data)
}

// 更早的块被篡改，末块检查发现不了：整体核验不符，丢掉重来一轮，最终正确。
func TestPFTransferTamperEarlierBlock(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(13, 6<<20)
	src := e.file("src/t.bin", data)
	job := pfTrUp(src, data, 0)

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.setProgress(func(p PFTransferProgress) {
		if p.Block == 3 {
			cancel()
		}
	})
	e.m.Transfer(ctx, job)
	e.setProgress(nil)

	f, _ := os.OpenFile(e.rpart(job.SHA256), os.O_WRONLY, 0)
	f.WriteAt([]byte{^data[50]}, 50) // 改第 1 块
	f.Close()

	res, err := e.m.Transfer(e.ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 || res.ResumedFrom != 4<<20 || res.SentBytes != int64(len(data))-4<<20+int64(len(data)) {
		t.Errorf("应续传一次、核验不符后从头重来一次: %+v", res)
	}
	pfTrEqual(t, res.PartPath, data)
}

// 源在传输中被改：核验失败、丢弃；连续两次则报错，不产出任何结果。
func TestPFTransferSourceChanged(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(14, 5<<20)
	src := e.file("src/s.bin", data)
	job := pfTrUp(src, data, 0)

	changed := append([]byte(nil), data...)
	for i := 3 << 20; i < len(changed); i += 4096 {
		changed[i] ^= 0xff // 后面的块全变，前面两块还是旧的：半新半旧
	}
	e.setProgress(func(p PFTransferProgress) {
		if p.Block == 1 {
			os.WriteFile(src, changed, 0o644)
		}
	})
	res, err := e.m.Transfer(e.ctx, job)
	if pfTrKind(err) != PFErrVerify {
		t.Fatalf("连续两次不符应报核验错误: %v", err)
	}
	t.Logf("错误: %v", err)
	if res.PartPath != "" || res.Attempts != 2 {
		t.Errorf("不应产出结果: %+v", res)
	}
	for _, ext := range []string{".part", ".meta"} {
		if _, err := os.Stat(filepath.Join(e.rstage, job.SHA256+ext)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 应已丢弃: %v", ext, err)
		}
	}

	// 第一轮被改、第二轮源恢复了：第二轮成功，结果就是期望内容
	os.WriteFile(src, data, 0o644)
	var round atomic.Int32
	e.setProgress(func(p PFTransferProgress) {
		if p.Attempt == 1 && p.Block == 1 {
			os.WriteFile(src, changed, 0o644)
		}
		if p.Attempt == 2 && p.Block == 0 {
			os.WriteFile(src, data, 0o644)
			round.Add(1)
		}
	})
	res, err = e.m.Transfer(e.ctx, job)
	if err != nil {
		t.Fatalf("第二轮源已恢复，应成功: %v", err)
	}
	if res.Attempts != 2 || round.Load() != 1 {
		t.Errorf("应重来一轮: %+v", res)
	}
	pfTrEqual(t, res.PartPath, data)
}

// 源在传输中变短：同样按核验不符处理，不产出结果。
func TestPFTransferSourceShrank(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(15, 4<<20)
	src := e.file("src/s.bin", data)
	e.setProgress(func(p PFTransferProgress) {
		if p.Block == 1 && p.Attempt == 1 {
			os.Truncate(src, 2<<20+10)
		}
	})
	res, err := e.m.Transfer(e.ctx, pfTrUp(src, data, 0))
	if pfTrKind(err) != PFErrVerify || res.PartPath != "" {
		t.Fatalf("应报核验错误: %v %+v", err, res)
	}
}

// 上传的大小与块边界算术（复核要求：只钉期望，不改生产代码）。
//
// 现状（grep 过 projsync：没有 MaxSize、没有 int32 窄化）：这一层的 Size/偏移/块大小全是 int64，
// 所以「offset+block 越过 4GB 上限」在这份代码里没有对应的场景；能真正碰到边界的是
// ① 提交时给的 Size 与源文件不符（包括谎报成 int64 上界附近），② 大小不是块整数倍时最后一块的长度/终点。
// 期望按生产现有语义（copyBlocks 前先比源大小；不符走 contentMismatch）：一块都不搬、
// 暂存区不落 part/meta，两次都不符就报 PFErrVerify——即「拒绝产出、报错、不落文件」。
func TestPFTransferUploadSizeAndBlockEdges(t *testing.T) {
	const huge = 1<<63 - 1
	e := newPFTrEnv(t, nil)
	data := pfTrData(21, 3*pfTrBlock+1234)
	src := e.file("src/lie.bin", data)
	for _, lie := range []int64{int64(len(data)) + 1, int64(len(data)) - 1, huge, huge - pfTrBlock} {
		job := PFTransferJob{Dir: PFUpload, Src: src, Size: lie, SHA256: pfTrSHA(data)}
		res, err := e.m.Transfer(e.ctx, job)
		if pfTrKind(err) != PFErrVerify {
			t.Errorf("Size=%d 与源 %d 字节不符，应报核验错误（拒绝产出）：%v", lie, len(data), err)
		}
		if res.PartPath != "" || res.SentBytes != 0 {
			t.Errorf("Size=%d：不该搬任何字节、不该有结果：%+v", lie, res)
		}
		for _, ext := range []string{".part", ".meta"} {
			p := filepath.Join(e.rstage, job.SHA256+ext)
			if _, serr := os.Stat(p); !errors.Is(serr, os.ErrNotExist) {
				t.Errorf("Size=%d：暂存区不该留下 %s：%v", lie, p, serr)
			}
		}
	}
	if got := e.spy.writtenTotal(); got != 0 {
		t.Errorf("大小不符时远端不该收到任何字节，实际 %d", got)
	}

	// 块边界：3 整块 + 半块 → 4 块，最后一块长度是剩下的半块，终点正好等于 size。
	e2 := newPFTrEnv(t, nil)
	edge := pfTrData(22, 3*pfTrBlock+4096)
	src2 := e2.file("src/edge.bin", edge)
	var lastBlock, lastDone int
	e2.setProgress(func(p PFTransferProgress) { lastBlock, lastDone = p.Block, int(p.Done) })
	res2, err := e2.m.Transfer(e2.ctx, pfTrUp(src2, edge, 0))
	if err != nil {
		t.Fatalf("正常大小应该传成：%v", err)
	}
	if res2.SentBytes != int64(len(edge)) {
		t.Errorf("SentBytes=%d，期望 %d", res2.SentBytes, len(edge))
	}
	if lastBlock != 3 || lastDone != len(edge) {
		t.Errorf("最后一块应是第 4 块（Block=3）落在文件尾：Block=%d Done=%d", lastBlock, lastDone)
	}
	pfTrEqual(t, res2.PartPath, edge)
	meta := pfTrMustRead(t, strings.TrimSuffix(res2.PartPath, ".part")+".meta")
	m := parsePFTrMeta(meta)
	if m == nil {
		t.Fatalf("meta 解析不出来：%q", meta)
	}
	if int64(len(m.blocks)) != pfTrNumBlocks(int64(len(edge)), m.hdr.Block) {
		t.Errorf("meta 记录了 %d 个块，期望 %d", len(m.blocks), pfTrNumBlocks(int64(len(edge)), m.hdr.Block))
	}
	if !m.hasVer || m.verSize != int64(len(edge)) || m.hdr.Size != int64(len(edge)) {
		t.Errorf("核验行/头里的大小应是 %d：hasVer=%v verSize=%d hdr.Size=%d",
			len(edge), m.hasVer, m.verSize, m.hdr.Size)
	}
	// 纯函数：整除与非整除的块数、最后一块的终点。
	if n := pfTrNumBlocks(3*pfTrBlock, pfTrBlock); n != 3 {
		t.Errorf("整除时块数 %d，期望 3", n)
	}
	if n := pfTrNumBlocks(3*pfTrBlock+1, pfTrBlock); n != 4 {
		t.Errorf("非整除时块数 %d，期望 4", n)
	}
	if n := pfTrBlockEnd(2, 3*pfTrBlock, pfTrBlock); n != 3*pfTrBlock {
		t.Errorf("整除时最后一块终点 %d，期望 %d", n, 3*pfTrBlock)
	}
	if n := pfTrBlockEnd(3, 3*pfTrBlock+1, pfTrBlock); n != 3*pfTrBlock+1 {
		t.Errorf("非整除时最后一块终点 %d，期望 %d", n, 3*pfTrBlock+1)
	}
	t.Logf("块=%d：3 整块+4096 共 4 块，SentBytes=%d part=%d", pfTrBlock, res2.SentBytes, len(edge))
}

// 目标端核验自己出通信错误：保留 part，原样报给调用方；下次直接核验，不重传。
func TestPFTransferVerifyCommError(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(16, 3<<20)
	src := e.file("src/v.bin", data)
	job := pfTrUp(src, data, 0)

	e.mu.Lock()
	e.verify = func() error { return errors.New("ssh: 连接中断") }
	e.mu.Unlock()
	_, err := e.m.Transfer(e.ctx, job)
	var pe *PFTransferError
	if !errors.As(err, &pe) || pe.Kind != PFErrComm || !strings.Contains(pe.Error(), "连接中断") {
		t.Fatalf("应带出核验的通信错误: %v", err)
	}
	if _, err := os.Stat(e.rpart(job.SHA256)); err != nil {
		t.Fatalf("part 应保留: %v", err)
	}

	e.mu.Lock()
	e.verify = nil
	e.mu.Unlock()
	res, err := e.m.Transfer(e.ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if res.SentBytes != 0 || res.ResumedFrom != int64(len(data)) || res.Reused {
		t.Errorf("part 已传完，只应重新核验: %+v", res)
	}
	pfTrEqual(t, res.PartPath, data)
}

// ── 抢占、顺序、并发 ────────────────────────────────────

// 低优先级大文件传输中，插入高优先级小文件：大文件让出，小文件先完成，大文件随后从断点续传完成。
func TestPFTransferPreempt(t *testing.T) {
	e := newPFTrEnv(t, nil)
	big := pfTrData(20, 6<<20)
	small := pfTrData(21, 100<<10)
	bsrc := e.file("src/big.bin", big)
	ssrc := e.file("src/small.bin", small)
	bjob, sjob := pfTrUp(bsrc, big, PFPrioNormal), pfTrUp(ssrc, small, PFPrioUser)

	gate := e.spy.gate(e.rpart(sjob.SHA256)) // 小文件的写先卡住，保证大文件让出时它还没传完
	var blocks []int
	var smallH *PFTransferHandle
	e.setProgress(func(p PFTransferProgress) {
		if p.Job.SHA256 != bjob.SHA256 {
			return
		}
		blocks = append(blocks, p.Block)
		if p.Block == 2 && smallH == nil {
			h, err := e.m.Submit(e.ctx, sjob)
			if err != nil {
				t.Error(err)
				return
			}
			smallH = h
		}
	})
	bh, err := e.m.Submit(e.ctx, bjob)
	if err != nil {
		t.Fatal(err)
	}
	pfTrWaitFor(t, 20*time.Second, "大文件让出、小文件在传", func() bool {
		s := e.m.Stats()
		return s.Pending == 1 && s.Running == 1
	})
	select {
	case <-bh.Done():
		t.Fatal("大文件不该在小文件之前完成")
	default:
	}
	close(gate)

	sres, err := smallH.Wait()
	if err != nil {
		t.Fatal(err)
	}
	bres, err := bh.Wait()
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	order := append([]string(nil), e.order...)
	e.mu.Unlock()
	if len(order) != 2 || order[0] != sjob.SHA256 || order[1] != bjob.SHA256 {
		t.Errorf("完成顺序应是小文件、大文件: %v", order)
	}
	if bres.Yields < 1 || bres.SentBytes != int64(len(big)) {
		t.Errorf("大文件应让出过、续传不重传: %+v", bres)
	}
	for i, b := range blocks {
		if b != i {
			t.Errorf("大文件的块应连续不重复: %v", blocks)
			break
		}
	}
	pfTrEqual(t, sres.PartPath, small)
	pfTrEqual(t, bres.PartPath, big)
}

// 排队顺序：优先级，再预计时间从小到大，再路径。
func TestPFTransferOrdering(t *testing.T) {
	e := newPFTrEnv(t, nil)
	blk := pfTrData(30, 3<<20)
	bjob := pfTrUp(e.file("src/blocker", blk), blk, PFPrioUser)
	gate := e.spy.gate(e.rpart(bjob.SHA256))
	bh, _ := e.m.Submit(e.ctx, bjob)
	pfTrWaitFor(t, 10*time.Second, "占位任务开始传", func() bool { return e.m.Stats().Running == 1 })

	mk := func(seed int64, size int, name string, prio int) PFTransferJob {
		d := pfTrData(seed, size)
		return pfTrUp(e.file("src/"+name, d), d, prio)
	}
	p1 := mk(31, 3<<20, "b", PFPrioNormal)
	p2 := mk(32, 2<<20, "z", PFPrioNormal)
	p3 := mk(33, 2<<20, "a", PFPrioNormal)
	p4 := mk(34, 5<<20, "c", PFPrioUbuntu)
	p5 := mk(35, 4<<20, "d", PFPrioUser)
	var hs []*PFTransferHandle
	for _, j := range []PFTransferJob{p1, p2, p3, p4, p5} {
		h, err := e.m.Submit(e.ctx, j)
		if err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
	}
	if s := e.m.Stats(); s.Pending != 5 || s.Running != 1 {
		t.Fatalf("队列状态不对: %+v", s)
	}
	close(gate)
	for _, h := range append(hs, bh) {
		if _, err := h.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{bjob.SHA256, p5.SHA256, p4.SHA256, p3.SHA256, p2.SHA256, p1.SHA256}
	e.mu.Lock()
	got := append([]string(nil), e.order...)
	e.mu.Unlock()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("顺序不对:\n got %v\nwant %v", got, want)
	}
}

func TestPFTransferCancelPending(t *testing.T) {
	e := newPFTrEnv(t, nil)
	a := pfTrData(40, 2<<20)
	b := pfTrData(41, 2<<20)
	ajob := pfTrUp(e.file("src/a", a), a, 0)
	gate := e.spy.gate(e.rpart(ajob.SHA256))
	ah, _ := e.m.Submit(e.ctx, ajob)
	pfTrWaitFor(t, 10*time.Second, "a 开始传", func() bool { return e.m.Stats().Running == 1 })
	bh, _ := e.m.Submit(e.ctx, pfTrUp(e.file("src/b", b), b, 0))
	if e.m.Stats().Pending != 1 {
		t.Fatalf("同方向大文件应只传一个: %+v", e.m.Stats())
	}
	bh.Cancel()
	if _, err := bh.Wait(); pfTrKind(err) != PFErrCanceled {
		t.Errorf("排队中取消应报已取消: %v", err)
	}
	close(gate)
	if _, err := ah.Wait(); err != nil {
		t.Fatal(err)
	}
}

// 同时提交 20 个小文件和 3 个大文件：并发不超过上限。
func TestPFTransferConcurrencyLimits(t *testing.T) {
	e := newPFTrEnv(t, nil)
	classes := map[string]string{}
	var hs []*PFTransferHandle
	var jobs []PFTransferJob
	var datas [][]byte
	for i := 0; i < 23; i++ {
		size, cls := 200<<10, "small"
		if i == 3 || i == 11 || i == 19 { // 3 个大文件夹在 20 个小文件里
			size, cls = 3<<20, "large"
		}
		d := pfTrData(int64(100+i), size)
		j := pfTrUp(e.file("src/f"+string(rune('A'+i)), d), d, 0)
		classes[e.rpart(j.SHA256)] = cls
		jobs, datas = append(jobs, j), append(datas, d)
	}
	e.spy.classify = func(p string) string { return classes[p] }
	e.setProgress(func(PFTransferProgress) { time.Sleep(3 * time.Millisecond) }) // 拉长每个任务，让并发看得出来
	for _, j := range jobs {
		h, err := e.m.Submit(e.ctx, j)
		if err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
	}
	for i, h := range hs {
		res, err := h.Wait()
		if err != nil {
			t.Fatalf("任务 %d: %v", i, err)
		}
		pfTrEqual(t, res.PartPath, datas[i])
	}
	e.spy.mu.Lock()
	defer e.spy.mu.Unlock()
	t.Logf("观察到的并发峰值: 小=%d 大=%d", e.spy.peak["small"], e.spy.peak["large"])
	if e.spy.peak["small"] > 8 || e.spy.peak["small"] < 2 {
		t.Errorf("小文件并发应在 2..8，实际 %d", e.spy.peak["small"])
	}
	if e.spy.peak["large"] != 1 {
		t.Errorf("大文件每方向只能 1 个，实际 %d", e.spy.peak["large"])
	}
}

// 上传和下载方向各传一个大文件可以同时进行（各 1 个）。
func TestPFTransferDirectionsIndependent(t *testing.T) {
	e := newPFTrEnv(t, nil)
	up := pfTrData(50, 3<<20)
	down := pfTrData(51, 3<<20)
	ujob := pfTrUp(e.file("src/up", up), up, 0)
	djob := pfTrDown(e.file("rsrc/down", down), down, 0)
	gate := e.spy.gate(e.rpart(ujob.SHA256))
	uh, _ := e.m.Submit(e.ctx, ujob)
	pfTrWaitFor(t, 10*time.Second, "上传开始", func() bool { return e.m.Stats().Running == 1 })
	// 上传卡着，下载不受影响（优先级低也不受上传影响）
	res, err := e.m.Transfer(e.ctx, djob)
	if err != nil {
		t.Fatal(err)
	}
	pfTrEqual(t, res.PartPath, down)
	close(gate)
	if _, err := uh.Wait(); err != nil {
		t.Fatal(err)
	}
}

// ── 限速 ────────────────────────────────────────────────

func TestPFTransferRateLimit(t *testing.T) {
	e := newPFTrEnv(t, nil)
	e.m.SetLimit(PFUpload, 1<<20) // 1MB/s
	data := pfTrData(60, 5<<20)
	src := e.file("src/r.bin", data)
	t0 := time.Now()
	res, err := e.m.Transfer(e.ctx, pfTrUp(src, data, 0))
	el := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	pfTrEqual(t, res.PartPath, data)
	t.Logf("1MB/s 传 5MB 用了 %v", el)
	if el < 3500*time.Millisecond || el > 12*time.Second {
		t.Errorf("限速 1MB/s 传 5MB 应约 5 秒，实际 %v", el)
	}
	bps, ok := e.m.Speed().Rate(PFUpload)
	if !ok || bps < 0.5*(1<<20) || bps > 2*(1<<20) {
		t.Errorf("测到的速度应接近限速: %.0f（有测量=%v）", bps, ok)
	}
	if e.m.Limit(PFUpload) != 1<<20 || e.m.Limit(PFDownload) != 0 {
		t.Errorf("Limit 读回不对")
	}
}

// 运行中改限速（改成不限）立即生效。
func TestPFTransferRateLimitChangedWhileRunning(t *testing.T) {
	e := newPFTrEnv(t, nil)
	e.m.SetLimit(PFUpload, 1<<20)
	data := pfTrData(61, 6<<20)
	src := e.file("src/r.bin", data)
	e.setProgress(func(p PFTransferProgress) {
		if p.Block == 0 {
			e.m.SetLimit(PFUpload, 0)
		}
	})
	t0 := time.Now()
	if _, err := e.m.Transfer(e.ctx, pfTrUp(src, data, 0)); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el > 4*time.Second {
		t.Errorf("中途改成不限速后应很快传完，实际 %v", el)
	}
}

// ── 测速与预计时间 ──────────────────────────────────────

func TestPFSpeedEstimate(t *testing.T) {
	s := NewPFSpeed(PFSpeedDefaultGood)
	if bps, ok := s.Rate(PFUpload); ok || bps != PFSpeedDefaultGood {
		t.Errorf("无测量值应给缺省: %v %v", bps, ok)
	}
	if got := s.EstimateSeconds(10<<20, PFUpload); got != 2 {
		t.Errorf("10MB ÷ 5MB/s = 2 秒，得到 %v", got)
	}
	if s.EstimateSeconds(0, PFUpload) != 0 || s.EstimateSeconds(-5, PFUpload) != 0 {
		t.Errorf("剩余 <=0 应为 0")
	}

	s.Observe(PFUpload, 1<<20, time.Second) // 第一个样本直接当估计
	if bps, ok := s.Rate(PFUpload); !ok || bps != 1<<20 {
		t.Errorf("第一个样本: %v %v", bps, ok)
	}
	s.Observe(PFUpload, 2<<20, time.Second) // 0.3×2MB + 0.7×1MB
	if bps, _ := s.Rate(PFUpload); bps < 1.3*(1<<20)-1 || bps > 1.3*(1<<20)+1 {
		t.Errorf("指数平均不对: %v", bps)
	}
	if bps, ok := s.Rate(PFDownload); ok || bps != PFSpeedDefaultGood {
		t.Errorf("另一个方向不受影响: %v %v", bps, ok)
	}
	s.Observe(PFUpload, 0, time.Second) // 无效样本忽略
	s.Observe(PFUpload, 10, 0)
	if bps, _ := s.Rate(PFUpload); bps < 1.3*(1<<20)-1 || bps > 1.3*(1<<20)+1 {
		t.Errorf("无效样本不应改变估计: %v", bps)
	}

	s.Reset(PFSpeedDefaultHotspot) // 网络状态变了：清掉测量，换缺省
	if bps, ok := s.Rate(PFUpload); ok || bps != PFSpeedDefaultHotspot {
		t.Errorf("重置后应回缺省: %v %v", bps, ok)
	}
	if got := s.EstimateSeconds(600_000, PFDownload); got != 2 {
		t.Errorf("600KB ÷ 0.3MB/s = 2 秒，得到 %v", got)
	}
	s.Reset(0) // <=0 沿用原缺省
	if bps, _ := s.Rate(PFDownload); bps != PFSpeedDefaultHotspot {
		t.Errorf("Reset(0) 应沿用原缺省: %v", bps)
	}
}

func TestPFTransferMeasuresSpeedAndReset(t *testing.T) {
	e := newPFTrEnv(t, func(c *PFTransferConfig) { c.DefaultBps = PFSpeedDefaultGood })
	data := pfTrData(70, 3<<20)
	if _, err := e.m.Transfer(e.ctx, pfTrUp(e.file("src/m", data), data, 0)); err != nil {
		t.Fatal(err)
	}
	if bps, ok := e.m.Speed().Rate(PFUpload); !ok || bps <= 0 {
		t.Errorf("传完 3 个 1MB 块后上传方向应有测量值: %v %v", bps, ok)
	}
	if _, ok := e.m.Speed().Rate(PFDownload); ok {
		t.Errorf("下载方向没传过，不该有测量值")
	}
	// 块小于 MinSampleBytes 不算样本
	small := pfTrData(71, 100<<10)
	e.m.ResetSpeed(0)
	if _, err := e.m.Transfer(e.ctx, pfTrUp(e.file("src/s", small), small, 0)); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.m.Speed().Rate(PFUpload); ok {
		t.Errorf("小于 1MB 的块不该计入测速")
	}
	e.m.ResetSpeed(PFSpeedDefaultHotspot)
	if got := e.m.EstimateSeconds(300_000, PFUpload); got != 1 {
		t.Errorf("热点缺省下 300KB 应预计 1 秒: %v", got)
	}
}

func TestPFTransferConfirmedBytesAndEstimate(t *testing.T) {
	e := newPFTrEnv(t, func(c *PFTransferConfig) { c.DefaultBps = 1 << 20 })
	data := pfTrData(72, 6<<20)
	job := pfTrUp(e.file("src/c", data), data, 0)
	if n, err := e.m.ConfirmedBytes(e.ctx, job); err != nil || n != 0 {
		t.Fatalf("没传过应为 0: %v %v", n, err)
	}
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.setProgress(func(p PFTransferProgress) {
		if p.Block == 1 {
			cancel()
		}
	})
	e.m.Transfer(ctx, job)
	e.setProgress(nil)
	e.m.ResetSpeed(1 << 20) // 前两块已测出本机的速度，这里要用固定的缺省来算
	if n, _ := e.m.ConfirmedBytes(e.ctx, job); n != 2<<20 {
		t.Errorf("传了 2 块应确认 2MB: %d", n)
	}
	if sec, _ := e.m.EstimateJob(e.ctx, job); sec != 4 {
		t.Errorf("剩 4MB、1MB/s 应预计 4 秒: %v", sec)
	}
	if _, err := e.m.Transfer(e.ctx, job); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.m.ConfirmedBytes(e.ctx, job); n != int64(len(data)) {
		t.Errorf("核验过应为全部: %d", n)
	}
}

// ── 清理 ────────────────────────────────────────────────

func TestPFCleanStaging(t *testing.T) {
	dir := t.TempDir()
	fs := PFLocalFS{}
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	mk := func(name string, size int, mt time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mt, mt)
	}
	sha := func(c string) string { return strings.Repeat(c, 64) }
	mk(sha("a")+".part", 100, old) // 过期，成对删
	mk(sha("a")+".meta", 10, old)
	mk(sha("b")+".part", 100, old) // part 旧但 meta 新（刚被引用）：保留
	mk(sha("b")+".meta", 10, now)
	mk(sha("c")+".part", 50, old) // 孤儿 part，过期
	mk(sha("d")+".part", 70, old) // 过期但正在用
	mk(sha("d")+".meta", 7, old)
	mk("keep.txt", 5, old) // 不认识的文件不碰
	mk(strings.ToUpper(sha("e"))+".part", 5, old)

	if _, err := PFCleanStaging(context.Background(), fs, dir, 0, now, nil); err == nil {
		t.Error("保留时长为 0 应拒绝")
	}
	res, err := PFCleanStaging(context.Background(), fs, dir, 7*24*time.Hour, now, func(s string) bool { return s == sha("d") })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 3 || res.Bytes != 160 || res.Kept != 2 {
		t.Errorf("清理结果不对: %+v", res)
	}
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }
	for n, want := range map[string]bool{
		sha("a") + ".part": false, sha("a") + ".meta": false, sha("b") + ".part": true, sha("b") + ".meta": true,
		sha("c") + ".part": false, sha("d") + ".part": true, sha("d") + ".meta": true, "keep.txt": true,
		strings.ToUpper(sha("e")) + ".part": true,
	} {
		if exists(n) != want {
			t.Errorf("%s 存在=%v，期望 %v", n, exists(n), want)
		}
	}
	// 目录不存在不算错
	if r, err := PFCleanStaging(context.Background(), fs, filepath.Join(dir, "无"), time.Hour, now, nil); err != nil || len(r.Removed) != 0 {
		t.Errorf("目录不存在应无事: %+v %v", r, err)
	}
}

// 复用 part 算一次引用：meta 的时间被刷新，清理按它算。
func TestPFTransferReuseRefreshesReference(t *testing.T) {
	e := newPFTrEnv(t, nil)
	data := pfTrData(80, 2<<20)
	job := pfTrUp(e.file("src/x", data), data, 0)
	if _, err := e.m.Transfer(e.ctx, job); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(e.rstage, job.SHA256+".meta")
	part := e.rpart(job.SHA256)
	oldT := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second) // SFTP 的时间只到秒
	// 把 part 和 meta 都弄成「30 天前」；meta 末行记的 part 时间同步改成新的，保证仍能复用
	age := func() {
		os.Chtimes(part, oldT, oldT)
		fi, _ := os.Stat(part)
		lines := strings.Split(strings.TrimSuffix(string(pfTrMustRead(t, meta)), "\n"), "\n")
		lines[len(lines)-1] = fmt.Sprintf("verified %d %d", fi.Size(), fi.ModTime().UnixNano())
		os.WriteFile(meta, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		os.Chtimes(meta, oldT, oldT)
	}
	age()
	if res, err := e.m.Transfer(e.ctx, job); err != nil || !res.Reused {
		t.Fatalf("应复用: %+v %v", res, err)
	}
	r, err := e.m.CleanStaging(e.ctx, PFUpload, 7*24*time.Hour)
	if err != nil || len(r.Removed) != 0 || r.Kept != 1 {
		t.Errorf("刚被引用的不该被清: %+v %v", r, err)
	}
	age() // 都老了才清
	r, err = e.m.CleanStaging(e.ctx, PFUpload, 7*24*time.Hour)
	if err != nil || len(r.Removed) != 2 {
		t.Errorf("过期应清掉 part 和 meta: %+v %v", r, err)
	}
}
