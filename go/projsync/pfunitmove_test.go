package projsync

// 本文件测 pfunitmove.go 的开始/取消同步、新建、链接检查、同步目录校验（技术设计 §12.7 的 9.6）。
// 崩溃注入与恢复在 pfunitmove_crash_test.go。
//
// 只在 Mac Studio 跑：要用 macOS 真实文件系统的 renamex_np、符号链接、目录改名后 ino 不变。其他平台整体跳过。
// 工作目录、同步目录、meta 都在临时目录里，测试用的「家目录」也是临时目录；项目名、文件内容全部虚构。
//
// 核心断言是「项目文件一个不丢、不出现两份同时被当成项目」：每个场景结束时用 assertOneCopy 查——
// 项目目录的 ino 没变、全部文件按内容逐个比对一致，并且桌面和同步目录里不会各有一个真实目录。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	pfumRel  = "2026/260403J1001-测试项目"
	pfumCode = "260403J1001"
	// 夹具里会被同步的文件数、目录数、字节数（.DS_Store 是排除项，不算）
	pfumFixFiles = 5
	pfumFixDirs  = 5
	pfumFixBytes = 11 + 4 + 6 + 5 + 320000
)

var errPFUMCrash = errors.New("模拟进程崩溃")

// ── 夹具 ────────────────────────────────────────────────

type pfumEnv struct {
	t    *testing.T
	home string
	work string // 桌面 Projects
	syn  string // 同步目录
	meta string

	skipSnap  bool // expectRefused 不比对项目内容（钩子里会改文件）
	nowFn     func() time.Time
	lstatMod  func(path string, st *PFStat) // 改 Lstat 的结果（模拟占位、不同卷）
	symlink   func(target, link string) error
	rename    func(oldPath, newPath string) error
	sleep     func(ctx context.Context, d time.Duration) error
	hook      PFUnitMoveHook
	pathsList string // 非空 = 配置路径清单（§14.4 开始同步的闸）
}

func newPFUMEnv(t *testing.T) *pfumEnv {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("开始/取消同步只在 macOS 上做（renamex_np、iCloud 位置），其他平台跳过")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &pfumEnv{t: t, home: filepath.Join(base, "home")}
	e.work = filepath.Join(e.home, "Desktop", "Projects")
	e.syn = filepath.Join(e.home, "XXYI-sync")
	e.meta = filepath.Join(e.home, ".xxyi", "projfiles")
	for _, d := range []string{e.work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *pfumEnv) cfg() PFUnitMoveConfig {
	return PFUnitMoveConfig{
		WorkDir: e.work, SyncDir: e.syn, Meta: e.meta, Home: e.home,
		PathsList: e.pathsList,
		Now: func() time.Time {
			if e.nowFn != nil {
				return e.nowFn()
			}
			return time.Now()
		},
		Lstat: func(p string) (PFStat, error) {
			st, err := PFLstat(p)
			if err == nil && e.lstatMod != nil {
				e.lstatMod(p, &st)
			}
			return st, err
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			if e.sleep != nil {
				return e.sleep(ctx, d)
			}
			return nil
		},
		Symlink: func(target, link string) error {
			if e.symlink != nil {
				return e.symlink(target, link)
			}
			return os.Symlink(target, link)
		},
		Rename: func(a, b string) error {
			if e.rename != nil {
				return e.rename(a, b)
			}
			return pfRenameNoReplace(a, b)
		},
		Hook: func(w string) error {
			if e.hook != nil {
				return e.hook(w)
			}
			return nil
		},
	}
}

// mover 每次建一个新实例：相当于进程重启。
func (e *pfumEnv) mover() *PFUnitMover {
	e.t.Helper()
	m, err := NewPFUnitMover(e.cfg())
	if err != nil {
		e.t.Fatalf("建执行器失败: %v", err)
	}
	return m
}

func (e *pfumEnv) P(rel string) string { return filepath.Join(e.work, filepath.FromSlash(rel)) }
func (e *pfumEnv) S(rel string) string { return filepath.Join(e.syn, filepath.FromSlash(rel)) }

// put 写一个文件，mtime 设成 2 小时前（已静止）。
func (e *pfumEnv) put(root, rel, content string) string {
	e.t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// project 在桌面工作目录里建一个标准夹具项目，返回 P。
func (e *pfumEnv) project(rel string) string {
	e.t.Helper()
	root := e.P(rel)
	e.put(root, "01 合同/合同.docx", "contract-v1")
	e.put(root, "02 图纸/平面图.pdf", "plan")
	e.put(root, "02 图纸/详图/节点.dwg", "detail")
	e.put(root, "备注.txt", "notes")
	e.put(root, ".DS_Store", "ds")
	e.put(root, "视频/big.bin", strings.Repeat("0123456789abcdef", 20000))
	if err := os.MkdirAll(filepath.Join(root, "03 空目录"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return root
}

func pfumSnap(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", dir, err)
		}
		for _, en := range ents {
			r := path.Join(rel, en.Name())
			full := filepath.Join(dir, en.Name())
			switch {
			case en.Type()&fs.ModeSymlink != 0:
				tgt, _ := os.Readlink(full)
				out[r] = "l:" + tgt
			case en.IsDir():
				out[r] = "d"
				walk(full, r)
			default:
				data, err := os.ReadFile(full)
				if err != nil {
					t.Fatalf("读 %s 失败: %v", full, err)
				}
				h := sha256.Sum256(data)
				out[r] = "f:" + hex.EncodeToString(h[:])
			}
		}
	}
	walk(root, "")
	return out
}

func pfumSameSnap(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s：条目数 %d，应为 %d", what, len(got), len(want))
	}
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got[k] != want[k] {
			t.Errorf("%s：%q 现在是 %q，应为 %q", what, k, got[k], want[k])
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s：多出了 %q", what, k)
		}
	}
}

func pfumIno(t *testing.T, p string) uint64 {
	t.Helper()
	st, err := PFLstat(p)
	if err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	return st.Ino
}

// pfumWant 是项目开始时的样子：内容快照、目录 ino。
type pfumWant struct {
	snap map[string]string
	ino  uint64 // 0 表示不核对（新建的项目）
}

func (e *pfumEnv) want(t *testing.T, p string) pfumWant {
	t.Helper()
	return pfumWant{snap: pfumSnap(t, p), ino: pfumIno(t, p)}
}

// assertOneCopy 查项目现在在哪，并断言：不丢（内容逐个一致、目录 ino 没变）、不两份（桌面和同步目录不会各有一个真实目录）。
// 返回 "desktop"（桌面真实目录）、"link"（桌面是指向同步目录的链接）、"sync-only"（桌面位置空、项目在同步目录）、
// "none"（哪里都没有，只在 allowNone 时允许）。
func (e *pfumEnv) assertOneCopy(t *testing.T, rel string, want pfumWant, allowNone bool) string {
	t.Helper()
	p, s := e.P(rel), e.S(rel)
	pfi, perr := os.Lstat(p)
	sfi, serr := os.Lstat(s)
	pKind, sKind := "none", "none"
	switch {
	case perr == nil && pfi.Mode()&fs.ModeSymlink != 0:
		pKind = "link"
	case perr == nil && pfi.IsDir():
		pKind = "dir"
	case perr == nil:
		pKind = "other"
	case !errors.Is(perr, fs.ErrNotExist):
		t.Fatalf("lstat %s: %v", p, perr)
	}
	switch {
	case serr == nil && sfi.IsDir() && sfi.Mode()&fs.ModeSymlink == 0:
		sKind = "dir"
	case serr == nil:
		sKind = "other"
	case !errors.Is(serr, fs.ErrNotExist):
		t.Fatalf("lstat %s: %v", s, serr)
	}
	if pKind == "dir" && sKind == "dir" {
		t.Fatalf("桌面和同步目录里各有一个真实目录（两份同时被当成项目）：%s、%s", p, s)
	}
	check := func(where, root string) string {
		pfumSameSnap(t, "项目内容（"+where+"）", pfumSnap(t, root), want.snap)
		if want.ino != 0 {
			st, err := pfumStatFollow(root) // 跟随链接：经 P 看到的就是项目目录本身
			if err != nil {
				t.Fatalf("stat %s: %v", root, err)
			}
			if got := st.Ino; got != want.ino {
				t.Errorf("项目目录 ino 变了：%d → %d（%s）", want.ino, got, root)
			}
		}
		return where
	}
	switch {
	case pKind == "link" && sKind == "dir":
		if tgt, _ := os.Readlink(p); tgt != s {
			t.Errorf("桌面链接指向 %q，应为 %q", tgt, s)
		}
		check("link(经 P 读)", p)
		return check("link", s)
	case pKind == "dir" && sKind == "none":
		return check("desktop", p)
	case pKind == "none" && sKind == "dir":
		return check("sync-only", s)
	case pKind == "none" && sKind == "none" && allowNone:
		return "none"
	}
	t.Fatalf("项目不见了或现场不认识：P(%s)=%s S(%s)=%s", p, pKind, s, sKind)
	return ""
}

func (e *pfumEnv) units(t *testing.T) []PFUnitRecord {
	t.Helper()
	us, err := PFUnitMoveLoadUnits(e.meta)
	if err != nil {
		t.Fatalf("读登记表失败: %v", err)
	}
	return us
}

func (e *pfumEnv) wantUnit(t *testing.T, code string, state PFUnitState) PFUnitRecord {
	t.Helper()
	u, ok := PFFindUnit(e.units(t), code)
	if !ok {
		t.Fatalf("登记表里没有 %s", code)
	}
	if u.State != state {
		t.Fatalf("登记表里 %s 的状态是 %s，应为 %s", code, u.State, state)
	}
	return u
}

func (e *pfumEnv) noUnit(t *testing.T, code string) {
	t.Helper()
	if u, ok := PFFindUnit(e.units(t), code); ok {
		t.Fatalf("登记表里不该有 %s，现在有：%+v", code, u)
	}
}

// logState 返回移动日志的 state；没有日志返回 ""。
func (e *pfumEnv) logState(t *testing.T, code string) string {
	t.Helper()
	lg, err := PFUnitMoveReadLog(e.meta, code)
	if err != nil {
		t.Fatalf("读移动日志失败: %v", err)
	}
	if lg == nil {
		return ""
	}
	return lg.State
}

func (e *pfumEnv) noTmp(t *testing.T) {
	t.Helper()
	for _, d := range []string{e.meta, filepath.Join(e.meta, "moves")} {
		ents, _ := os.ReadDir(d)
		for _, en := range ents {
			if strings.Contains(en.Name(), ".tmp-") {
				t.Errorf("%s 里残留了临时文件 %s", d, en.Name())
			}
		}
	}
}

func (e *pfumEnv) mustStart(t *testing.T, rel string) PFUnitMoveResult {
	t.Helper()
	res, err := e.mover().StartSync(bg, rel)
	if err != nil || res.Status != PFUnitMoveDone {
		t.Fatalf("开始同步应成功：err=%v res=%+v", err, res)
	}
	return res
}

func (e *pfumEnv) mustStop(t *testing.T, code string) PFUnitMoveResult {
	t.Helper()
	res, err := e.mover().StopSync(bg, code)
	if err != nil || res.Status != PFUnitMoveDone {
		t.Fatalf("取消同步应成功：err=%v res=%+v", err, res)
	}
	return res
}

func pfumRefusalCodes(res PFUnitMoveResult) []string {
	var out []string
	for _, r := range res.Refusals {
		out = append(out, r.Code)
	}
	return out
}

func pfumHas(codes []string, c string) bool {
	for _, x := range codes {
		if x == c {
			return true
		}
	}
	return false
}

// ── 同步目录校验（§12.1）────────────────────────────────

func TestPFUnitMove_ValidateConfig(t *testing.T) {
	e := newPFUMEnv(t)
	if err := PFUnitMoveValidateConfig(e.cfg()); err != nil {
		t.Fatalf("正常配置应通过：%v", err)
	}
	// 同步目录还没建、上级也不存在的情况也应通过（开始同步时会建）
	if err := os.MkdirAll(e.meta, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PFUnitMoveValidateConfig(e.cfg()); err != nil {
		t.Fatalf("meta 已建时应通过：%v", err)
	}
	other := filepath.Join(e.home, "Work")
	real := filepath.Join(e.home, "real")
	for _, d := range []string{real, filepath.Join(e.home, "Desktop", "x")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(real, filepath.Join(e.home, "syncln")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.home, "Desktop", "x"), filepath.Join(e.home, "lnk")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mod  func(c *PFUnitMoveConfig)
		want string
	}{
		{"同步目录不是绝对路径", func(c *PFUnitMoveConfig) { c.SyncDir = "XXYI-sync" }, "绝对路径"},
		{"同步目录没设置", func(c *PFUnitMoveConfig) { c.SyncDir = "" }, "没有设置"},
		{"工作目录不是绝对路径", func(c *PFUnitMoveConfig) { c.WorkDir = "Desktop/Projects" }, "绝对路径"},
		{"meta 不是绝对路径", func(c *PFUnitMoveConfig) { c.Meta = ".xxyi" }, "绝对路径"},
		{"在 Desktop 下", func(c *PFUnitMoveConfig) { c.SyncDir = filepath.Join(c.Home, "Desktop", "sync") }, "iCloud"},
		{"就是 Desktop", func(c *PFUnitMoveConfig) { c.SyncDir = filepath.Join(c.Home, "Desktop") }, "iCloud"},
		{"在 Documents 下", func(c *PFUnitMoveConfig) { c.SyncDir = filepath.Join(c.Home, "Documents", "sync") }, "iCloud"},
		{"在 Mobile Documents 下", func(c *PFUnitMoveConfig) {
			c.SyncDir = filepath.Join(c.Home, "Library", "Mobile Documents", "com~apple~CloudDocs", "sync")
		}, "iCloud"},
		{"在 CloudStorage 下", func(c *PFUnitMoveConfig) {
			c.SyncDir = filepath.Join(c.Home, "Library", "CloudStorage", "Box", "sync")
		}, "iCloud"},
		{"Desktop 写成小写（APFS 不区分大小写）", func(c *PFUnitMoveConfig) { c.SyncDir = filepath.Join(c.Home, "desktop", "sync") }, "iCloud"},
		{"经符号链接进了 Desktop", func(c *PFUnitMoveConfig) { c.SyncDir = filepath.Join(c.Home, "lnk", "sync") }, "iCloud"},
		{"在工作目录里", func(c *PFUnitMoveConfig) { c.WorkDir = other; c.SyncDir = filepath.Join(other, "sync") }, "互相包含"},
		{"包含工作目录", func(c *PFUnitMoveConfig) { c.WorkDir = filepath.Join(other, "p"); c.SyncDir = other }, "互相包含"},
		{"与工作目录相同", func(c *PFUnitMoveConfig) { c.WorkDir = other; c.SyncDir = other }, "互相包含"},
		{"在 meta 里", func(c *PFUnitMoveConfig) { c.SyncDir = filepath.Join(c.Meta, "sync") }, "互相包含"},
		{"包含 meta", func(c *PFUnitMoveConfig) { c.SyncDir = filepath.Join(c.Home, ".xxyi") }, "互相包含"},
		{"同步目录是符号链接", func(c *PFUnitMoveConfig) { c.SyncDir = filepath.Join(c.Home, "syncln") }, "符号链接"},
		{"与 meta 不同卷", func(c *PFUnitMoveConfig) {
			orig := c.Lstat
			meta := c.Meta
			c.Lstat = func(p string) (PFStat, error) {
				st, err := orig(p)
				if err == nil && pfumUnder(p, meta) {
					st.Dev += 7
				}
				return st, err
			}
		}, "同一个卷"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := e.cfg()
			tc.mod(&c)
			err := PFUnitMoveValidateConfig(c)
			var ce *PFUnitMoveConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("应返回配置错误，实际：%v", err)
			}
			if !strings.Contains(ce.Msg, tc.want) {
				t.Errorf("错误信息应含 %q：%s", tc.want, ce.Msg)
			}
			if _, err := NewPFUnitMover(c); !errors.As(err, &ce) {
				t.Errorf("NewPFUnitMover 也应返回配置错误：%v", err)
			}
		})
	}
}

func TestPFUnitMove_UnsupportedPlatform(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("这条只在非 macOS 上有意义")
	}
	_, err := NewPFUnitMover(PFUnitMoveConfig{WorkDir: "/a", SyncDir: "/b", Meta: "/c", Home: "/h"})
	var ce *PFUnitMoveConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("非 macOS 应返回配置错误（不支持）：%v", err)
	}
}

// ── 正常来回 ────────────────────────────────────────────

func TestPFUnitMove_RoundTrip(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	want := e.want(t, p)
	s := e.S(pfumRel)
	// 同步目录里原来有别的项目的年份层：取消后不能被误删
	base := filepath.Join(e.meta, "base", pfumCode+".json")
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte(`{"unit":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	res := e.mustStart(t, pfumRel)
	if res.Unit != pfumCode || res.Files != pfumFixFiles || res.Dirs != pfumFixDirs || res.Bytes != pfumFixBytes {
		t.Errorf("开始同步应报告文件/目录/字节数 %d/%d/%d：%+v", pfumFixFiles, pfumFixDirs, pfumFixBytes, res)
	}
	if res.LinkPath != p || res.SyncPath != s || res.Code != PFUnitMoveCodeOK {
		t.Errorf("结果里的路径或代码不对：%+v", res)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
		t.Fatalf("开始同步后应是链接，实际 %s", where)
	}
	// 链接是指向 S 绝对路径的符号链接；经链接能读到文件、能写文件
	if tgt, err := os.Readlink(p); err != nil || tgt != s {
		t.Errorf("readlink(P)=%q,%v，应为 %q", tgt, err, s)
	}
	if data, err := os.ReadFile(filepath.Join(p, "备注.txt")); err != nil || string(data) != "notes" {
		t.Errorf("经链接读文件失败：%q %v", data, err)
	}
	u := e.wantUnit(t, pfumCode, PFUnitSyncing)
	if u.Rel != pfumRel || u.LinkPath != p || u.SyncPath != s || u.StartedAt.IsZero() {
		t.Errorf("登记不对：%+v", u)
	}
	if st := e.logState(t, pfumCode); st != PFUnitMoveLogDone {
		t.Errorf("移动日志应是 done，实际 %q", st)
	}
	e.noTmp(t)
	checks, err := e.mover().CheckLinks(bg)
	if err != nil || len(checks) != 1 || checks[0].Status != PFUnitMoveLinkOK {
		t.Errorf("链接检查应为 ok：%v %+v", err, checks)
	}
	// 同步中的项目不能再开始
	again, err := e.mover().StartSync(bg, pfumRel)
	if err != nil || again.Status != PFUnitMoveRefused {
		t.Errorf("已在同步的项目再点开始应被拒绝：%v %+v", err, again)
	}
	// 同步目录一侧加个文件（模拟同步进来的）：取消同步后跟着回到桌面
	e.put(s, "新来的.txt", "from-ubuntu")
	want.snap["新来的.txt"] = "f:" + func() string { h := sha256.Sum256([]byte("from-ubuntu")); return hex.EncodeToString(h[:]) }()

	stop := e.mustStop(t, pfumCode)
	if stop.Files != pfumFixFiles+1 || stop.Bytes != pfumFixBytes+int64(len("from-ubuntu")) {
		t.Errorf("取消同步应报告回桌面的文件数/字节数：%+v", stop)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
		t.Fatalf("取消同步后应回到桌面，实际 %s", where)
	}
	if _, err := os.Lstat(filepath.Join(e.syn, "2026")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("年份层空了应删掉：%v", err)
	}
	if _, err := os.Lstat(e.syn); err != nil {
		t.Errorf("同步目录本身不能被删：%v", err)
	}
	u = e.wantUnit(t, pfumCode, PFUnitStopped)
	if u.LinkPath != "" || u.SyncPath != "" || u.Rel != pfumRel {
		t.Errorf("stopped 的登记不对：%+v", u)
	}
	if st := e.logState(t, pfumCode); st != PFUnitMoveLogDone {
		t.Errorf("移动日志应是 done，实际 %q", st)
	}
	if data, _ := os.ReadFile(base); string(data) != `{"unit":"x"}` {
		t.Errorf("取消同步不能动 base：%q", data)
	}
	e.noTmp(t)

	// 取消后可以再开始（编号登记里是 stopped）
	e.mustStart(t, pfumRel)
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
		t.Fatalf("再次开始同步后应是链接，实际 %s", where)
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
	if len(e.units(t)) != 1 {
		t.Errorf("再次开始不能多出登记：%+v", e.units(t))
	}
}

// 项目里自带符号链接：随目录一起挪过去挪回来，链接本身原样（不跟随、不改写），提示里说明同步不处理它们。
func TestPFUnitMove_InnerSymlinkTravelsUnchanged(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	if err := os.Symlink("备注.txt", filepath.Join(p, "快捷方式")); err != nil {
		t.Fatal(err)
	}
	want := e.want(t, p)
	res := e.mustStart(t, pfumRel)
	if !strings.Contains(strings.Join(res.Notes, ""), "符号链接") {
		t.Errorf("应提示项目里有符号链接：%v", res.Notes)
	}
	e.assertOneCopy(t, pfumRel, want, false)
	e.mustStop(t, pfumCode)
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
		t.Errorf("应回到桌面：%s", where)
	}
	if tgt, err := os.Readlink(filepath.Join(p, "快捷方式")); err != nil || tgt != "备注.txt" {
		t.Errorf("项目里的链接应原样：%q %v", tgt, err)
	}
}

// 同一个年份层下的第二个项目：取消第一个时年份层不空，不能删；取消最后一个才删。
func TestPFUnitMove_YearLayerKeptUntilEmpty(t *testing.T) {
	e := newPFUMEnv(t)
	rel2 := "2026/260403J1002-另一个项目"
	e.project(pfumRel)
	e.project(rel2)
	e.mustStart(t, pfumRel)
	e.mustStart(t, rel2)
	e.mustStop(t, pfumCode)
	if _, err := os.Lstat(filepath.Join(e.syn, "2026", "260403J1002-另一个项目")); err != nil {
		t.Fatalf("另一个项目不能被动：%v", err)
	}
	// 年份层里有个不相干的文件：即使项目走光了也不删
	e.put(filepath.Join(e.syn, "2026"), "读我.txt", "x")
	e.mustStop(t, "260403J1002")
	if _, err := os.Lstat(filepath.Join(e.syn, "2026", "读我.txt")); err != nil {
		t.Fatalf("年份层里的无关文件不能被删：%v", err)
	}
}

// 只用了一层 rel（没有年份层）也能来回。
func TestPFUnitMove_NoYearLayer(t *testing.T) {
	e := newPFUMEnv(t)
	rel := "260403J1003-单层项目"
	p := e.project(rel)
	want := e.want(t, p)
	e.mustStart(t, rel)
	e.assertOneCopy(t, rel, want, false)
	e.mustStop(t, "260403J1003")
	if where := e.assertOneCopy(t, rel, want, false); where != "desktop" {
		t.Fatalf("应回到桌面：%s", where)
	}
	if _, err := os.Lstat(e.syn); err != nil {
		t.Errorf("同步目录本身不能被删：%v", err)
	}
}

// 移动日志的格式（T11、T17 依赖）：在建完上级目录之后读，字段齐全。
func TestPFUnitMove_LogFormat(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	ino := pfumIno(t, p)
	var raw []byte
	e.hook = func(w string) error {
		if w == "start:after-mkdir" {
			raw, _ = os.ReadFile(PFUnitMoveLogPath(e.meta, pfumCode))
		}
		return nil
	}
	e.mustStart(t, pfumRel)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("日志不是 JSON：%v\n%s", err, raw)
	}
	for k, want := range map[string]any{"version": float64(1), "code": pfumCode, "op": "start", "state": "moving", "step": "begin",
		"rel": pfumRel, "p": p, "s": e.S(pfumRel), "ino": float64(ino)} {
		if m[k] != want {
			t.Errorf("日志字段 %s=%v，应为 %v\n%s", k, m[k], want, raw)
		}
	}
	for _, k := range []string{"dev", "started_at", "updated_at", "created_dirs"} {
		if _, ok := m[k]; !ok {
			t.Errorf("日志缺字段 %s\n%s", k, raw)
		}
	}
	if dirs, _ := m["created_dirs"].([]any); len(dirs) != 2 || dirs[0] != e.syn || dirs[1] != filepath.Join(e.syn, "2026") {
		t.Errorf("created_dirs 应是同步目录和年份层（从上到下）：%v", m["created_dirs"])
	}
	lgs, err := PFUnitMoveListLogs(e.meta)
	if err != nil || len(lgs) != 1 || lgs[0].Code != pfumCode || lgs[0].State != PFUnitMoveLogDone || lgs[0].Op != PFUnitMoveOpStart {
		t.Errorf("列日志不对：%v %+v", err, lgs)
	}
	// 日志坏了：读报错，列表里不放它并返回错误
	if err := os.WriteFile(PFUnitMoveLogPath(e.meta, "260403J1009"), []byte("{坏的"), 0o600); err != nil {
		t.Fatal(err)
	}
	lgs, err = PFUnitMoveListLogs(e.meta)
	if err == nil || len(lgs) != 1 {
		t.Errorf("坏日志应返回错误且不进结果：%v %+v", err, lgs)
	}
}

// ── 拒绝（前置检查，全部只读）────────────────────────────

// expectRefused 调开始同步，断言被拒绝、原因里有 want 里的每个代码，并且现场一点没动。
func (e *pfumEnv) expectRefused(t *testing.T, rel string, want ...string) PFUnitMoveResult {
	t.Helper()
	p := e.P(rel)
	var before map[string]string
	var ino uint64
	if st, err := os.Lstat(p); err == nil && st.IsDir() && !e.skipSnap {
		before, ino = pfumSnap(t, p), pfumIno(t, p)
	}
	res, err := e.mover().StartSync(bg, rel)
	if err != nil {
		t.Fatalf("前置检查没过不该返回 error：%v", err)
	}
	if res.Status != PFUnitMoveRefused {
		t.Fatalf("应被拒绝，实际 %+v", res)
	}
	codes := pfumRefusalCodes(res)
	for _, c := range want {
		if !pfumHas(codes, c) {
			t.Errorf("拒绝原因里应有 %s，实际 %v（%s）", c, codes, res.Reason)
		}
	}
	if res.Reason == "" || len(res.Refusals) == 0 {
		t.Errorf("拒绝要带中文原因和原因列表：%+v", res)
	}
	if before != nil && !e.skipSnap {
		pfumSameSnap(t, "被拒绝后项目内容", pfumSnap(t, p), before)
		if pfumIno(t, p) != ino {
			t.Errorf("被拒绝后项目目录 ino 变了")
		}
	}
	if _, err := os.Lstat(e.S(rel)); !errors.Is(err, fs.ErrNotExist) {
		if !pfumHas(codes, PFUnitMoveCodeTargetExists) {
			t.Errorf("被拒绝后同步目录里不该有东西：%v", err)
		}
	}
	if _, err := os.Lstat(PFUnitMoveLogPath(e.meta, pfumCode)); err == nil {
		t.Errorf("前置检查没过不该写移动日志")
	}
	return res
}

func pfumRefusalFiles(res PFUnitMoveResult, code string) []string {
	for _, r := range res.Refusals {
		if r.Code == code {
			return r.Files
		}
	}
	return nil
}

func TestPFUnitMove_RefuseICloudPlaceholder(t *testing.T) {
	t.Run("SF_DATALESS", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		e.lstatMod = func(p string, st *PFStat) {
			if strings.HasSuffix(p, "平面图.pdf") {
				st.Flags |= PFSFDataless
			}
		}
		res := e.expectRefused(t, pfumRel, PFUnitMoveCodePlaceholder)
		if f := pfumRefusalFiles(res, PFUnitMoveCodePlaceholder); len(f) != 1 || f[0] != "02 图纸/平面图.pdf" {
			t.Errorf("应列出占位文件的相对路径：%v", f)
		}
		e.noUnit(t, pfumCode)
	})
	t.Run("项目目录本身是占位", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		root := e.P(pfumRel)
		e.lstatMod = func(p string, st *PFStat) {
			if p == root {
				st.Flags |= PFSFDataless
			}
		}
		res := e.expectRefused(t, pfumRel, PFUnitMoveCodePlaceholder)
		if f := pfumRefusalFiles(res, PFUnitMoveCodePlaceholder); len(f) != 1 || f[0] != "." {
			t.Errorf("应说明是项目目录本身：%v", f)
		}
	})
	t.Run("占位目录", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		e.lstatMod = func(p string, st *PFStat) {
			if strings.HasSuffix(p, "02 图纸") {
				st.Flags |= PFSFDataless
			}
		}
		e.expectRefused(t, pfumRel, PFUnitMoveCodePlaceholder)
	})
	t.Run("老式 .X.icloud，文件本身不在", func(t *testing.T) {
		e := newPFUMEnv(t)
		root := e.project(pfumRel)
		e.put(root, "02 图纸/.详图说明.pdf.icloud", "stub")
		res := e.expectRefused(t, pfumRel, PFUnitMoveCodePlaceholder)
		if f := pfumRefusalFiles(res, PFUnitMoveCodePlaceholder); len(f) != 1 || f[0] != "02 图纸/详图说明.pdf" {
			t.Errorf("应列出 X 的路径：%v", f)
		}
	})
	t.Run("占位在隐藏目录里也要拒绝", func(t *testing.T) {
		e := newPFUMEnv(t)
		root := e.project(pfumRel)
		e.put(root, ".隐藏/内容.bin", "x")
		e.lstatMod = func(p string, st *PFStat) {
			if strings.HasSuffix(p, "内容.bin") {
				st.Flags |= PFSFDataless
			}
		}
		e.expectRefused(t, pfumRel, PFUnitMoveCodePlaceholder)
	})
	t.Run("最多列 50 个", func(t *testing.T) {
		e := newPFUMEnv(t)
		root := e.project(pfumRel)
		for i := 0; i < 60; i++ {
			e.put(root, fmt.Sprintf("批量/占位-%02d.bin", i), "x")
		}
		e.lstatMod = func(p string, st *PFStat) {
			if strings.Contains(filepath.Base(p), "占位-") {
				st.Flags |= PFSFDataless
			}
		}
		res := e.expectRefused(t, pfumRel, PFUnitMoveCodePlaceholder)
		var r PFUnitMoveRefusal
		for _, x := range res.Refusals {
			if x.Code == PFUnitMoveCodePlaceholder {
				r = x
			}
		}
		if len(r.Files) != 50 || r.More != 10 || !sort.StringsAreSorted(r.Files) {
			t.Errorf("应列 50 个（字节序）并说明还有 10 个：%d 个，more=%d", len(r.Files), r.More)
		}
		if !strings.Contains(r.Reason, "60") {
			t.Errorf("原因里应有总数 60：%s", r.Reason)
		}
	})
}

func TestPFUnitMove_RefuseOfficeLock(t *testing.T) {
	for _, name := range []string{"01 合同/~$合同.docx", "01 合同/.~lock.合同.docx#"} {
		t.Run(name, func(t *testing.T) {
			e := newPFUMEnv(t)
			root := e.project(pfumRel)
			e.put(root, name, "lock")
			res := e.expectRefused(t, pfumRel, PFUnitMoveCodeOfficeLock)
			if f := pfumRefusalFiles(res, PFUnitMoveCodeOfficeLock); len(f) != 1 || f[0] != name {
				t.Errorf("应列出锁文件：%v", f)
			}
		})
	}
	t.Run("超过 12 小时的锁当残留，不挡，写进提示", func(t *testing.T) {
		e := newPFUMEnv(t)
		root := e.project(pfumRel)
		lock := e.put(root, "01 合同/~$合同.docx", "lock")
		old := time.Now().Add(-13 * time.Hour)
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
		res := e.mustStart(t, pfumRel)
		if len(res.Notes) == 0 || !strings.Contains(strings.Join(res.Notes, ""), "残留") {
			t.Errorf("残留的锁应写进 Notes：%v", res.Notes)
		}
	})
}

func TestPFUnitMove_RefuseRecentWrite(t *testing.T) {
	e := newPFUMEnv(t)
	root := e.project(pfumRel)
	if err := os.WriteFile(filepath.Join(root, "刚写的.txt"), []byte("now"), 0o644); err != nil { // mtime 是现在
		t.Fatal(err)
	}
	res := e.expectRefused(t, pfumRel, PFUnitMoveCodeNotQuiet)
	if f := pfumRefusalFiles(res, PFUnitMoveCodeNotQuiet); len(f) != 1 || f[0] != "刚写的.txt" {
		t.Errorf("应列出刚写的文件：%v", f)
	}
}

// 排除项（.DS_Store 等）最近被写不挡路——Finder 随时在改它。
func TestPFUnitMove_ExcludedNamesDoNotBlock(t *testing.T) {
	e := newPFUMEnv(t)
	root := e.project(pfumRel)
	now := time.Now()
	for _, n := range []string{".DS_Store", "x.tmp", "下载.crdownload", "Thumbs.db"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("fresh"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root, n), now, now); err != nil {
			t.Fatal(err)
		}
	}
	e.sleep = func(ctx context.Context, d time.Duration) error { // 两次扫描之间 .DS_Store 又被写了
		return os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("fresh-again!"), 0o644)
	}
	res := e.mustStart(t, pfumRel)
	if res.Files != pfumFixFiles {
		t.Errorf("排除项不算文件数：%d", res.Files)
	}
}

func TestPFUnitMove_RefuseUnstable(t *testing.T) {
	t.Run("两次扫描之间文件被改", func(t *testing.T) {
		e := newPFUMEnv(t)
		root := e.project(pfumRel)
		e.skipSnap = true
		var gap time.Duration
		e.sleep = func(ctx context.Context, d time.Duration) error {
			gap = d
			return os.WriteFile(filepath.Join(root, "备注.txt"), []byte("notes-changed"), 0o644)
		}
		res := e.expectRefused(t, pfumRel, PFUnitMoveCodeUnstable)
		if gap != 2*time.Second {
			t.Errorf("两次扫描间隔缺省应是 2 秒：%v", gap)
		}
		if f := pfumRefusalFiles(res, PFUnitMoveCodeUnstable); len(f) != 1 || f[0] != "备注.txt（变动）" {
			t.Errorf("应列出变动的文件：%v", f)
		}
	})
	t.Run("两次扫描之间多了文件", func(t *testing.T) {
		e := newPFUMEnv(t)
		root := e.project(pfumRel)
		e.skipSnap = true
		e.sleep = func(ctx context.Context, d time.Duration) error {
			return os.WriteFile(filepath.Join(root, "多出来的.txt"), []byte("x"), 0o644)
		}
		res := e.expectRefused(t, pfumRel, PFUnitMoveCodeUnstable)
		if f := pfumRefusalFiles(res, PFUnitMoveCodeUnstable); len(f) != 1 || f[0] != "多出来的.txt（新增）" {
			t.Errorf("应列出新增的文件：%v", f)
		}
	})
	t.Run("两次扫描之间文件消失", func(t *testing.T) {
		e := newPFUMEnv(t)
		root := e.project(pfumRel)
		e.skipSnap = true
		e.sleep = func(ctx context.Context, d time.Duration) error { return os.Remove(filepath.Join(root, "备注.txt")) }
		res := e.expectRefused(t, pfumRel, PFUnitMoveCodeUnstable)
		if f := pfumRefusalFiles(res, PFUnitMoveCodeUnstable); len(f) != 1 || f[0] != "备注.txt（消失）" {
			t.Errorf("应列出消失的文件：%v", f)
		}
	})
	t.Run("等待时 ctx 取消", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		ctx, cancel := context.WithCancel(bg)
		e.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
		res, err := e.mover().StartSync(ctx, pfumRel)
		if !errors.Is(err, context.Canceled) || res.Status != PFUnitMoveRefused || res.Code != PFUnitMoveCodeCanceled {
			t.Fatalf("取消应返回 canceled：%v %+v", err, res)
		}
		if _, err := os.Lstat(e.S(pfumRel)); err == nil {
			t.Errorf("取消后不该挪")
		}
	})
}

func TestPFUnitMove_RefuseTargetExists(t *testing.T) {
	for _, kind := range []string{"目录", "文件", "链接"} {
		t.Run(kind, func(t *testing.T) {
			e := newPFUMEnv(t)
			e.project(pfumRel)
			s := e.S(pfumRel)
			if err := os.MkdirAll(filepath.Dir(s), 0o755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "目录":
				os.Mkdir(s, 0o755)
				e.put(s, "别人的.txt", "keep")
			case "文件":
				os.WriteFile(s, []byte("keep"), 0o644)
			case "链接":
				os.Symlink(e.home, s)
			}
			e.expectRefused(t, pfumRel, PFUnitMoveCodeTargetExists)
			if kind == "目录" {
				if data, _ := os.ReadFile(filepath.Join(s, "别人的.txt")); string(data) != "keep" {
					t.Errorf("同步目录里已有的东西不能被动")
				}
			}
		})
	}
}

func TestPFUnitMove_RefuseCrossVolume(t *testing.T) {
	e := newPFUMEnv(t)
	e.project(pfumRel)
	year := filepath.Join(e.syn, "2026")
	if err := os.MkdirAll(year, 0o755); err != nil {
		t.Fatal(err)
	}
	e.lstatMod = func(p string, st *PFStat) {
		if pfumUnder(p, year) {
			st.Dev += 5 // 目标位置所在的卷与桌面项目不同（只改年份层以下，免得同步目录与 meta 的同卷校验先报错）
		}
	}
	e.expectRefused(t, pfumRel, PFUnitMoveCodeCrossVolume)
}

func TestPFUnitMove_RefuseNestedMount(t *testing.T) {
	e := newPFUMEnv(t)
	e.project(pfumRel)
	e.lstatMod = func(p string, st *PFStat) {
		if strings.HasSuffix(p, "视频") {
			st.Dev += 5 // 项目里的某个目录是别的卷挂载进来的
		}
	}
	e.expectRefused(t, pfumRel, PFUnitMoveCodeCrossVolume)
}

func TestPFUnitMove_RefuseUnreadableDir(t *testing.T) {
	pfaSkipIfRoot(t)
	e := newPFUMEnv(t)
	root := e.project(pfumRel)
	e.skipSnap = true // 读不了的目录没法做内容快照
	dir := filepath.Join(root, "03 空目录")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	res := e.expectRefused(t, pfumRel, PFUnitMoveCodeUnreadable)
	if f := pfumRefusalFiles(res, PFUnitMoveCodeUnreadable); len(f) != 1 || f[0] != "03 空目录" {
		t.Errorf("应列出读不了的目录：%v", f)
	}
}

// 被排除的名字（. 开头的隐藏目录）读不了也要拒绝：里面有没有 iCloud 占位无从确认，挪出去就拿不回来。
func TestPFUnitMove_RefuseUnreadableExcludedDir(t *testing.T) {
	pfaSkipIfRoot(t)
	e := newPFUMEnv(t)
	root := e.project(pfumRel)
	e.put(root, ".cache/x.bin", "hidden")
	e.skipSnap = true // 读不了的目录没法做内容快照
	dir := filepath.Join(root, ".cache")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	res := e.expectRefused(t, pfumRel, PFUnitMoveCodeUnreadable)
	if f := pfumRefusalFiles(res, PFUnitMoveCodeUnreadable); len(f) != 1 || f[0] != ".cache" {
		t.Errorf("应列出读不了的隐藏目录：%v", f)
	}
	// 现场一点没动：P 还是原来的真实目录，同步目录里没有东西，没有移动日志（expectRefused 已查后两项）
	if st, err := os.Lstat(root); err != nil || !st.IsDir() {
		t.Errorf("项目目录应原样留在桌面：%v", err)
	}
	// 权限改回来之后同样的项目可以开始同步（拒绝只因为读不了）
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.skipSnap = false
	e.mustStart(t, pfumRel)
}

// 被排除的隐藏目录嵌得太深、扫描没进去看，同样拒绝。
func TestPFUnitMove_RefuseTooDeepExcludedDir(t *testing.T) {
	e := newPFUMEnv(t)
	root := e.project(pfumRel)
	deep := filepath.Join(root, ".deep") + strings.Repeat("/d", pfMaxDepth+2)
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	e.expectRefused(t, pfumRel, PFUnitMoveCodeUnreadable)
}

func TestPFUnitMove_RefuseNotRealDir(t *testing.T) {
	t.Run("不存在", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.expectRefused(t, pfumRel, PFUnitMoveCodeNotRealDir)
	})
	t.Run("已经是链接", func(t *testing.T) {
		e := newPFUMEnv(t)
		other := filepath.Join(e.home, "别处", "项目")
		os.MkdirAll(other, 0o755)
		e.put(other, "a.txt", "a")
		os.MkdirAll(filepath.Dir(e.P(pfumRel)), 0o755)
		if err := os.Symlink(other, e.P(pfumRel)); err != nil {
			t.Fatal(err)
		}
		e.expectRefused(t, pfumRel, PFUnitMoveCodeNotRealDir)
		if data, _ := os.ReadFile(filepath.Join(other, "a.txt")); string(data) != "a" {
			t.Errorf("链接指向的东西不能被动")
		}
	})
	t.Run("是文件", func(t *testing.T) {
		e := newPFUMEnv(t)
		os.MkdirAll(filepath.Dir(e.P(pfumRel)), 0o755)
		os.WriteFile(e.P(pfumRel), []byte("f"), 0o644)
		e.expectRefused(t, pfumRel, PFUnitMoveCodeNotRealDir)
	})
	t.Run("年份层是符号链接", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project("2025/260403J1004-旧项目")
		real := filepath.Join(e.home, "别处的年份层")
		if err := os.Rename(filepath.Join(e.work, "2025"), real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(e.work, "2025")); err != nil {
			t.Fatal(err)
		}
		e.expectRefused(t, "2025/260403J1004-旧项目", PFUnitMoveCodePathLink)
	})
	t.Run("同步目录一侧的年份层是文件", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		os.MkdirAll(e.syn, 0o755)
		os.WriteFile(filepath.Join(e.syn, "2026"), []byte("f"), 0o644)
		e.expectRefused(t, pfumRel, PFUnitMoveCodePathLink)
	})
}

func TestPFUnitMove_RefuseBadRel(t *testing.T) {
	e := newPFUMEnv(t)
	e.project(pfumRel)
	for _, tc := range []struct{ rel, code string }{
		{"../逃出去/260403J1001-x", PFUnitMoveCodeBadRel},
		{"", PFUnitMoveCodeBadRel},
		{"2026//260403J1001-x", PFUnitMoveCodeBadRel},
		{"/2026/260403J1001-x", PFUnitMoveCodeBadRel},
		{"2026/没有编号的目录", PFUnitMoveCodeNoCode},
		{"2026/.隐藏/260403J1001-x", PFUnitMoveCodeBadRel},
		{"a/b/c/d/e/260403J1001-x", PFUnitMoveCodeBadRel},
		{"260403J1001-x/260403J1002-y", PFUnitMoveCodeBadRel}, // 上级目录名里已能认出编号
	} {
		res, err := e.mover().StartSync(bg, tc.rel)
		if err != nil || res.Status != PFUnitMoveRefused || res.Code != tc.code {
			t.Errorf("rel=%q 应被拒绝（%s）：%v %+v", tc.rel, tc.code, err, res)
		}
	}
	if _, err := os.Lstat(e.P(pfumRel)); err != nil {
		t.Errorf("项目不能被动：%v", err)
	}
}

func TestPFUnitMove_RefuseRegisteredAndPending(t *testing.T) {
	t.Run("编号已登记", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		rec := PFUnitRecord{Code: pfumCode, Rel: pfumRel, State: PFUnitSyncing, LinkPath: e.P(pfumRel), SyncPath: e.S(pfumRel), StartedAt: time.Now()}
		data, err := PFMarshalUnits([]PFUnitRecord{rec})
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(e.meta, 0o755)
		os.WriteFile(PFUnitMoveUnitsPath(e.meta), data, 0o600)
		e.expectRefused(t, pfumRel, PFUnitMoveCodeRegistered)
	})
	t.Run("有没做完的移动日志", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		lg := PFUnitMoveLog{Version: 1, Code: pfumCode, Op: PFUnitMoveOpStart, State: PFUnitMoveLogMoving, Step: "rename", Rel: pfumRel,
			P: e.P(pfumRel), S: e.S(pfumRel), Ino: 1, Dev: 1, StartedAt: time.Now(), UpdatedAt: time.Now()}
		data, _ := json.Marshal(lg)
		os.MkdirAll(filepath.Join(e.meta, "moves"), 0o755)
		os.WriteFile(PFUnitMoveLogPath(e.meta, pfumCode), data, 0o600)
		p := e.P(pfumRel)
		before := pfumSnap(t, p)
		res, err := e.mover().StartSync(bg, pfumRel)
		if err != nil || res.Status != PFUnitMoveRefused || !pfumHas(pfumRefusalCodes(res), PFUnitMoveCodeMovePending) {
			t.Fatalf("应因日志没做完被拒绝：%v %+v", err, res)
		}
		pfumSameSnap(t, "项目内容", pfumSnap(t, p), before)
		if after, _ := os.ReadFile(PFUnitMoveLogPath(e.meta, pfumCode)); !bytes.Equal(after, data) {
			t.Errorf("没做完的日志不能被覆盖")
		}
	})
	t.Run("同时有多个原因，全部列出", func(t *testing.T) {
		e := newPFUMEnv(t)
		root := e.project(pfumRel)
		e.put(root, "01 合同/~$合同.docx", "lock")
		e.lstatMod = func(p string, st *PFStat) {
			if strings.HasSuffix(p, "节点.dwg") {
				st.Flags |= PFSFDataless
			}
		}
		os.MkdirAll(e.syn, 0o755)
		os.MkdirAll(e.S(pfumRel), 0o755)
		e.expectRefused(t, pfumRel, PFUnitMoveCodeOfficeLock, PFUnitMoveCodePlaceholder, PFUnitMoveCodeTargetExists)
	})
}

// units.json 坏了：读到坏的就报错停下，不当成空表；文件一个字节不改。
func TestPFUnitMove_CorruptRegistryStops(t *testing.T) {
	for name, content := range map[string]string{"乱码": "{不是 JSON", "空文件": "", "不是数组": `{"a":1}`, "校验不过": `[{"code":"x","rel":"y","state":"syncing"}]`} {
		t.Run(name, func(t *testing.T) {
			e := newPFUMEnv(t)
			p := e.project(pfumRel)
			want := e.want(t, p)
			os.MkdirAll(e.meta, 0o755)
			os.WriteFile(PFUnitMoveUnitsPath(e.meta), []byte(content), 0o600)
			res, err := e.mover().StartSync(bg, pfumRel)
			if err == nil || res.Code != PFUnitMoveCodeRegistryBad || res.Status != PFUnitMoveRefused {
				t.Fatalf("开始同步应因登记表坏了停下：%v %+v", err, res)
			}
			if res, err = e.mover().CreateSynced(bg, "2026/260403J1005-新项目"); err == nil || res.Code != PFUnitMoveCodeRegistryBad {
				t.Errorf("新建应停下：%v %+v", err, res)
			}
			if res, err = e.mover().StopSync(bg, pfumCode); err == nil || res.Code != PFUnitMoveCodeRegistryBad {
				t.Errorf("取消应停下：%v %+v", err, res)
			}
			if _, err = e.mover().CheckLinks(bg); err == nil {
				t.Errorf("链接检查应报错")
			}
			rep, err := e.mover().RecoverMoves(bg)
			if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted || rep[0].Code != PFUnitMoveCodeRegistryBad {
				t.Errorf("恢复应报告登记表坏了：%v %+v", err, rep)
			}
			if got, _ := os.ReadFile(PFUnitMoveUnitsPath(e.meta)); string(got) != content {
				t.Errorf("坏的 units.json 不能被改：%q", got)
			}
			if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
				t.Errorf("项目应还在桌面：%s", where)
			}
			if _, err := os.Lstat(e.S("2026/260403J1005-新项目")); err == nil {
				t.Errorf("不该新建任何东西")
			}
			e.noTmp(t)
		})
	}
}

// ── 链接检查（§12.2）────────────────────────────────────

func TestPFUnitMove_CheckLinks(t *testing.T) {
	e := newPFUMEnv(t)
	relA, relB, relC, relD := "2026/260403J1001-甲", "2026/260403J1002-乙", "2026/260403J1003-丙", "2026/260403J1004-丁"
	for _, r := range []string{relA, relB, relC, relD} {
		e.project(r)
		e.mustStart(t, r)
	}
	statuses := func() map[string]PFUnitMoveLinkCheck {
		cs, err := e.mover().CheckLinks(bg)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]PFUnitMoveLinkCheck{}
		for _, c := range cs {
			out[c.Unit] = c
		}
		return out
	}
	for _, c := range statuses() {
		if c.Status != PFUnitMoveLinkOK {
			t.Fatalf("起点应全是 ok：%+v", c)
		}
	}
	// 甲：链接被删，原位置空 → 重建
	if err := os.Remove(e.P(relA)); err != nil {
		t.Fatal(err)
	}
	// 乙：链接被换成真实目录 → 只报告
	os.Remove(e.P(relB))
	os.Mkdir(e.P(relB), 0o755)
	e.put(e.P(relB), "用户放的.txt", "mine")
	// 丙：链接指向别处 → 只报告
	os.Remove(e.P(relC))
	os.Symlink(e.home, e.P(relC))
	// 丁：同步目录里的项目没了 → 只报告，不重建、不当删除
	sD := e.S(relD)
	hold := sD + ".藏起来"
	os.Rename(sD, hold)

	got := statuses()
	if got["260403J1001"].Status != PFUnitMoveLinkRebuilt {
		t.Errorf("甲应重建：%+v", got["260403J1001"])
	}
	if tgt, err := os.Readlink(e.P(relA)); err != nil || tgt != e.S(relA) {
		t.Errorf("甲的链接应指向 S：%q %v", tgt, err)
	}
	if got["260403J1002"].Status != PFUnitMoveLinkOccupied {
		t.Errorf("乙应是被占：%+v", got["260403J1002"])
	}
	if data, _ := os.ReadFile(filepath.Join(e.P(relB), "用户放的.txt")); string(data) != "mine" {
		t.Errorf("乙原位置的东西不能被动")
	}
	if got["260403J1003"].Status != PFUnitMoveLinkWrongTarget {
		t.Errorf("丙应是指向别处：%+v", got["260403J1003"])
	}
	if tgt, _ := os.Readlink(e.P(relC)); tgt != e.home {
		t.Errorf("丙的链接不能被改：%q", tgt)
	}
	if got["260403J1004"].Status != PFUnitMoveLinkSyncMissing {
		t.Errorf("丁应是同步目录里不见了：%+v", got["260403J1004"])
	}
	if _, err := os.Lstat(sD); err == nil {
		t.Errorf("丁的同步目录位置不能被凭空建出来")
	}
	// 年份层被删：重建时补上
	os.Rename(hold, sD)
	os.Remove(e.P(relD))
	os.Remove(e.P(relA))
	os.Remove(e.P(relB))
	os.RemoveAll(e.P(relB))
	os.Remove(e.P(relC))
	if err := os.Remove(filepath.Join(e.work, "2026")); err != nil {
		t.Fatalf("年份层应已空：%v", err)
	}
	got = statuses()
	for _, c := range got {
		if c.Status != PFUnitMoveLinkRebuilt {
			t.Errorf("年份层没了也应重建：%+v", c)
		}
	}
	// 非 syncing 的不检查；有没做完的日志的跳过
	e.mustStop(t, "260403J1002")
	os.Remove(e.P(relA))
	os.MkdirAll(filepath.Join(e.meta, "moves"), 0o755)
	lg := PFUnitMoveLog{Version: 1, Code: "260403J1001", Op: PFUnitMoveOpStop, State: PFUnitMoveLogMovingBack, Step: "rename", Rel: relA,
		P: e.P(relA), S: e.S(relA), Ino: 1, Dev: 1, StartedAt: time.Now(), UpdatedAt: time.Now()}
	data, _ := json.Marshal(lg)
	os.WriteFile(PFUnitMoveLogPath(e.meta, "260403J1001"), data, 0o600)
	got = statuses()
	if _, ok := got["260403J1002"]; ok {
		t.Errorf("stopped 的不该检查")
	}
	if got["260403J1001"].Status != PFUnitMoveLinkSkipped {
		t.Errorf("有没做完的日志应跳过：%+v", got["260403J1001"])
	}
	if _, err := os.Lstat(e.P(relA)); err == nil {
		t.Errorf("跳过的不该重建")
	}
}

// ── 新建同步项目（§12.5）────────────────────────────────

func TestPFUnitMove_CreateSynced(t *testing.T) {
	e := newPFUMEnv(t)
	res, err := e.mover().CreateSynced(bg, pfumRel)
	if err != nil || res.Status != PFUnitMoveDone {
		t.Fatalf("新建应成功：%v %+v", err, res)
	}
	p, s := e.P(pfumRel), e.S(pfumRel)
	if tgt, err := os.Readlink(p); err != nil || tgt != s {
		t.Fatalf("P 应是指向 S 的链接：%q %v", tgt, err)
	}
	if ents, err := os.ReadDir(s); err != nil || len(ents) != 0 {
		t.Fatalf("S 应是空目录：%v %v", ents, err)
	}
	u := e.wantUnit(t, pfumCode, PFUnitSyncing)
	if u.LinkPath != p || u.SyncPath != s {
		t.Errorf("登记不对：%+v", u)
	}
	if e.logState(t, pfumCode) != PFUnitMoveLogDone {
		t.Errorf("日志应是 done")
	}
	// 经 P 建子目录、放文件，落在 S 里
	if err := os.MkdirAll(filepath.Join(p, "01 合同"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.put(p, "01 合同/合同.docx", "v1")
	if data, err := os.ReadFile(filepath.Join(s, "01 合同", "合同.docx")); err != nil || string(data) != "v1" {
		t.Errorf("经链接写的文件应落在 S：%q %v", data, err)
	}
	if cs, err := e.mover().CheckLinks(bg); err != nil || len(cs) != 1 || cs[0].Status != PFUnitMoveLinkOK {
		t.Errorf("链接检查：%v %+v", err, cs)
	}
	e.noTmp(t)
	// 再建同一个：编号已登记、P 和 S 都已存在
	res, err = e.mover().CreateSynced(bg, pfumRel)
	if err != nil || res.Status != PFUnitMoveRefused {
		t.Fatalf("重复新建应被拒绝：%v %+v", err, res)
	}
	for _, c := range []string{PFUnitMoveCodeRegistered, PFUnitMoveCodeTargetExists} {
		if !pfumHas(pfumRefusalCodes(res), c) {
			t.Errorf("拒绝原因里应有 %s：%v", c, pfumRefusalCodes(res))
		}
	}
	if data, _ := os.ReadFile(filepath.Join(s, "01 合同", "合同.docx")); string(data) != "v1" {
		t.Errorf("重复新建不能动已有内容")
	}
	// 取消同步后，项目（含经链接放进去的文件）回到桌面
	e.mustStop(t, pfumCode)
	if data, err := os.ReadFile(filepath.Join(p, "01 合同", "合同.docx")); err != nil || string(data) != "v1" {
		t.Errorf("取消后文件应在桌面：%q %v", data, err)
	}
}

func TestPFUnitMove_CreateSyncedRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *pfumEnv)
		want  string
	}{
		{"桌面位置已有目录", func(e *pfumEnv) { e.put(e.P(pfumRel), "a.txt", "a") }, PFUnitMoveCodeTargetExists},
		{"桌面位置已有文件", func(e *pfumEnv) {
			os.MkdirAll(filepath.Dir(e.P(pfumRel)), 0o755)
			os.WriteFile(e.P(pfumRel), []byte("f"), 0o644)
		}, PFUnitMoveCodeTargetExists},
		{"桌面位置已有链接", func(e *pfumEnv) { os.MkdirAll(filepath.Dir(e.P(pfumRel)), 0o755); os.Symlink(e.home, e.P(pfumRel)) }, PFUnitMoveCodeTargetExists},
		{"同步目录位置已有目录", func(e *pfumEnv) { e.put(e.S(pfumRel), "a.txt", "a") }, PFUnitMoveCodeTargetExists},
		{"没有编号", nil, PFUnitMoveCodeNoCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPFUMEnv(t)
			rel := pfumRel
			if tc.name == "没有编号" {
				rel = "2026/没有编号"
			}
			if tc.setup != nil {
				tc.setup(e)
			}
			var beforeP, beforeS map[string]string
			if st, err := os.Lstat(e.P(rel)); err == nil && st.IsDir() {
				beforeP = pfumSnap(t, e.P(rel))
			}
			if st, err := os.Lstat(e.S(rel)); err == nil && st.IsDir() {
				beforeS = pfumSnap(t, e.S(rel))
			}
			res, err := e.mover().CreateSynced(bg, rel)
			if err != nil || res.Status != PFUnitMoveRefused || !pfumHas(pfumRefusalCodes(res), tc.want) {
				t.Fatalf("应被拒绝（%s）：%v %+v", tc.want, err, res)
			}
			if beforeP != nil {
				pfumSameSnap(t, "桌面原有内容", pfumSnap(t, e.P(rel)), beforeP)
			}
			if beforeS != nil {
				pfumSameSnap(t, "同步目录原有内容", pfumSnap(t, e.S(rel)), beforeS)
			}
			e.noUnit(t, pfumCode)
			if _, err := os.Lstat(PFUnitMoveLogPath(e.meta, pfumCode)); err == nil {
				t.Errorf("被拒绝不该写日志")
			}
		})
	}
}

func TestPFUnitMove_CreateSyncedRollsBackWhenLinkFails(t *testing.T) {
	e := newPFUMEnv(t)
	e.symlink = func(target, link string) error { return syscall.EACCES }
	res, err := e.mover().CreateSynced(bg, pfumRel)
	if err != nil || res.Status != PFUnitMoveRolledBack || res.Code != PFUnitMoveCodeLinkFailed {
		t.Fatalf("建链接失败应回退：%v %+v", err, res)
	}
	for _, p := range []string{e.S(pfumRel), filepath.Dir(e.S(pfumRel)), e.syn, e.P(pfumRel), filepath.Dir(e.P(pfumRel))} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("回退后不该留下 %s", p)
		}
	}
	if _, err := os.Lstat(e.work); err != nil {
		t.Errorf("桌面工作目录本身不能被删：%v", err)
	}
	e.noUnit(t, pfumCode)
	if st := e.logState(t, pfumCode); st != "" {
		t.Errorf("回退后日志应清掉：%q", st)
	}
	if rep, err := e.mover().RecoverMoves(bg); err != nil || len(rep) != 0 {
		t.Errorf("回退后没有要恢复的：%v %+v", err, rep)
	}
}

// ── 并发 ────────────────────────────────────────────────

// 同一个执行器上同时开始两个项目：登记表的读改写被串行，两条登记都在。
func TestPFUnitMove_ConcurrentStarts(t *testing.T) {
	e := newPFUMEnv(t)
	rels := []string{"2026/260403J1001-甲", "2026/260403J1002-乙", "2026/260403J1003-丙"}
	for _, r := range rels {
		e.project(r)
	}
	m := e.mover()
	var wg sync.WaitGroup
	for _, r := range rels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, err := m.StartSync(bg, r); err != nil || res.Status != PFUnitMoveDone {
				t.Errorf("%s: %v %+v", r, err, res)
			}
		}()
	}
	wg.Wait()
	if us := e.units(t); len(us) != 3 {
		t.Errorf("三个项目都应登记：%+v", us)
	}
}

// ── 小工具 ──────────────────────────────────────────────

// pfumUnlink 只删名字：对链接只删链接；对目录（空的、不空的）都失败且目录原样。
func TestPFUnitMove_UnlinkNeverRemovesDirs(t *testing.T) {
	e := newPFUMEnv(t)
	target := filepath.Join(e.home, "target")
	e.put(target, "a.txt", "a")
	link := filepath.Join(e.home, "ln")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := pfumUnlink(link); err != nil {
		t.Fatalf("删链接应成功：%v", err)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Errorf("链接应已删")
	}
	if data, _ := os.ReadFile(filepath.Join(target, "a.txt")); string(data) != "a" {
		t.Errorf("链接指向的东西不能被动")
	}
	if err := pfumUnlink(target); err == nil {
		t.Errorf("对不空的目录 unlink 应失败")
	}
	empty := filepath.Join(e.home, "empty")
	os.Mkdir(empty, 0o755)
	if err := pfumUnlink(empty); err == nil {
		t.Errorf("对空目录 unlink 也应失败（不能像 os.Remove 那样 rmdir）")
	}
	if _, err := os.Lstat(empty); err != nil {
		t.Errorf("空目录应还在：%v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(target, "a.txt")); string(data) != "a" {
		t.Errorf("目录里的东西不能被动")
	}
}

// ── 开始同步的闸（§14.4）───────────────────────────────

// pfumWritePathsList 写一份配置路径清单（内容为调用方拼好的 JSON）。
func pfumWritePathsList(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPFUnitMove_PathsGate(t *testing.T) {
	const code = "260403J1301"
	const relMain = "2026/260403J1301-测试项目己"
	const relCopy = "2026/260403J1301-测试项目己-副本"
	const relOther = "2026/260403J1301-没配置"
	e := newPFUMEnv(t)
	e.pathsList = filepath.Join(e.home, ".xxyi", "projfiles-paths.json")
	e.project(relMain)
	e.project(relCopy)

	// 1) 清单读不到 → 有配置与否判断不了，拒绝并写原因
	res := e.expectRefused(t, relMain, PFUnitMoveCodePathsBad)
	if !strings.Contains(res.Reason, "清单") {
		t.Errorf("原因要写明是清单的问题：%s", res.Reason)
	}

	// 2) 编号在清单里有配置、rel 是配置的那条 → 照常过，键 = 编号
	pfumWritePathsList(t, e.pathsList, `{"version":1,"units":[`+
		`{"key":"`+code+`","code":"`+code+`","serials":[1],"rel":"`+relMain+`"},`+
		`{"key":"`+code+`R2","code":"`+code+`","serials":[2],"rel":"`+relCopy+`"}]}`)
	r := e.mustStart(t, relMain)
	if r.Unit != code {
		t.Errorf("主目录单元的键应是编号，得 %q", r.Unit)
	}
	rec := e.wantUnit(t, code, PFUnitSyncing)
	if rec.ProjectCode != "" {
		t.Errorf("键 = 编号时 project_code 应留空，得 %q", rec.ProjectCode)
	}

	// 3) rel 不是清单里配置的那条 → 拒绝，原因列出配置的目录
	e.project(relOther)
	res = e.expectRefused(t, relOther, PFUnitMoveCodeNotConfigured)
	if !strings.Contains(res.Reason, relMain) || !strings.Contains(res.Reason, relCopy) {
		t.Errorf("原因要列出这个编号配置的目录：%s", res.Reason)
	}

	// 4) 另配目录单元：键取清单里的 R2，编号记进 project_code；取消同步按 R2 键也走得通
	r = e.mustStart(t, relCopy)
	if r.Unit != code+"R2" {
		t.Errorf("另配目录单元的键应是 %sR2，得 %q", code, r.Unit)
	}
	rec = e.wantUnit(t, code+"R2", PFUnitSyncing)
	if rec.ProjectCode != code || rec.Rel != relCopy {
		t.Errorf("登记应是编号 %s 配 %s，得 %+v", code, relCopy, rec)
	}
	if got := pfumSnap(t, e.S(relCopy)); len(got) == 0 {
		t.Errorf("取消前同步目录里应是完整项目")
	}

	// 编号完全不在清单里 → 照旧（键 = 编号），闸只挡配置过的编号
	const code2, rel2 = "260403J1302", "2026/260403J1302-另一个项目"
	e.project(rel2)
	r = e.mustStart(t, rel2)
	if r.Unit != code2 {
		t.Errorf("不在清单里的编号应照旧按编号登记，得 %q", r.Unit)
	}

	// CreateSynced 不受闸限制：把清单弄坏也照样能建（键 = 编号）
	pfumWritePathsList(t, e.pathsList, `{"version":1,"units": 坏 JSON`)
	const code3, rel3 = "260403J1303", "2026/260403J1303-桌面新建"
	res, err := e.mover().CreateSynced(bg, rel3)
	if err != nil || res.Status != PFUnitMoveDone || res.Unit != code3 {
		t.Fatalf("CreateSynced 不该被闸挡住：%+v %v", res, err)
	}

	// 取消 R2 单元：键检查放宽后按 R2 取消，目录搬回原位
	e.mustStop(t, code+"R2")
	if st, err := os.Lstat(e.P(relCopy)); err != nil || !st.IsDir() {
		t.Errorf("取消后 %s 应搬回桌面原位", relCopy)
	}
	e.wantUnit(t, code+"R2", PFUnitStopped) // 取消同步后登记留着，状态是 stopped（与主目录单元一样）
	e.noTmp(t)
}
