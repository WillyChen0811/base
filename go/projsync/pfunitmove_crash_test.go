package projsync

// 本文件测 pfunitmove.go 的崩溃与失败：在每一步之间注入「进程死掉」（钩子返回 error），换一个新执行器调 RecoverMoves
// （相当于 wgtund 重启），验证
//   - 四种认得出的现场都补完或退回，项目文件一个不丢（逐个文件按内容比对、目录 ino 不变）；
//   - 任何时刻都不会出现桌面和同步目录里各有一个真实目录；
//   - 现场不认识时停下报告、不动任何东西；
//   - 失败回退：建链接失败目录回到原处，核对不符不删任何东西。
// 只在 Mac Studio 跑（见 pfunitmove_test.go 头部）。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func pfumCrashAt(target string) PFUnitMoveHook {
	return func(w string) error {
		if w == target {
			return errPFUMCrash
		}
		return nil
	}
}

// ── 通用的崩溃场景框架 ───────────────────────────────────

type pfumCrashScenario struct {
	name string
	// setup 建好前置状态，返回项目开始时的样子。
	setup func(t *testing.T, e *pfumEnv) pfumWant
	// op 执行被测操作（钩子由框架事先装在 e.hook 上）。
	op        func(e *pfumEnv) (PFUnitMoveResult, error)
	allowNone bool
	// wantWindows 是这个操作应该经过的全部窗口（防止窗口被改没了，测试悄悄变少）。
	wantWindows []string
	// final 说明在窗口 w 崩溃、恢复之后项目应在哪："desktop"、"link" 或 "none"。
	final func(w string) string
	// verify 对恢复后的状态查登记表、日志等。
	verify func(t *testing.T, e *pfumEnv, where string)
	// afterwards 恢复后继续正常使用。
	afterwards func(t *testing.T, e *pfumEnv, where string)
}

// windows 不崩溃跑一遍，记下操作经过的窗口。
func (sc pfumCrashScenario) windows(t *testing.T) []string {
	e := newPFUMEnv(t)
	sc.setup(t, e)
	var ws []string
	e.hook = func(w string) error { ws = append(ws, w); return nil }
	res, err := sc.op(e)
	if err != nil || res.Status != PFUnitMoveDone {
		t.Fatalf("不崩溃时应成功：%v %+v", err, res)
	}
	if strings.Join(ws, ",") != strings.Join(sc.wantWindows, ",") {
		t.Fatalf("操作经过的窗口变了：\n 实际 %v\n 预期 %v", ws, sc.wantWindows)
	}
	return ws
}

// crashed 装钩子在 w 崩溃，跑操作，断言它带着崩溃 error 返回、现场没有丢也没有两份。
func (sc pfumCrashScenario) crashed(t *testing.T, e *pfumEnv, want pfumWant, w string) {
	t.Helper()
	e.hook = pfumCrashAt(w)
	res, err := sc.op(e)
	if !errors.Is(err, errPFUMCrash) || res.Status != PFUnitMoveHalted || res.Code != PFUnitMoveCodeHookAbort {
		t.Fatalf("在 %s 应模拟崩溃：%v %+v", w, err, res)
	}
	e.assertOneCopy(t, pfumRel, want, sc.allowNone)
}

// recovered 重启（新执行器）恢复一次，断言没有停下，项目在预期的位置，并跑 verify、再恢复一次应无事可做。
func (sc pfumCrashScenario) recovered(t *testing.T, e *pfumEnv, want pfumWant, final string) {
	t.Helper()
	e.hook = nil
	rep, err := e.mover().RecoverMoves(bg)
	if err != nil {
		t.Fatalf("恢复报错：%v", err)
	}
	for _, r := range rep {
		if r.Status == PFUnitMoveHalted {
			t.Errorf("认得出的现场不该停下：%+v", r)
		}
	}
	where := e.assertOneCopy(t, pfumRel, want, sc.allowNone)
	if where != final {
		t.Errorf("恢复后项目应在 %s，实际 %s（%+v）", final, where, rep)
	}
	sc.verify(t, e, where)
	e.noTmp(t)
	rep2, err := e.mover().RecoverMoves(bg)
	if err != nil || len(rep2) != 0 {
		t.Errorf("第二次恢复应无事可做：%v %+v", err, rep2)
	}
	if again := e.assertOneCopy(t, pfumRel, want, sc.allowNone); again != where {
		t.Errorf("第二次恢复把现场改了：%s → %s", where, again)
	}
	sc.afterwards(t, e, where)
}

// runSingle：每个窗口崩溃一次，恢复。
func (sc pfumCrashScenario) runSingle(t *testing.T) {
	for _, w := range sc.windows(t) {
		t.Run(w, func(t *testing.T) {
			e := newPFUMEnv(t)
			want := sc.setup(t, e)
			sc.crashed(t, e, want, w)
			sc.recovered(t, e, want, sc.final(w))
		})
	}
}

// runDouble：在每个窗口崩溃之后，恢复自己也在每个窗口再崩一次，然后再恢复——恢复要能重入。
func (sc pfumCrashScenario) runDouble(t *testing.T) {
	for _, w1 := range sc.windows(t) {
		var rws []string
		func() { // 先看恢复会经过哪些窗口
			e := newPFUMEnv(t)
			want := sc.setup(t, e)
			sc.crashed(t, e, want, w1)
			e.hook = func(w string) error { rws = append(rws, w); return nil }
			if _, err := e.mover().RecoverMoves(bg); err != nil {
				t.Fatalf("恢复报错：%v", err)
			}
		}()
		for _, w2 := range rws {
			t.Run(w1+"/恢复时崩在"+w2, func(t *testing.T) {
				e := newPFUMEnv(t)
				want := sc.setup(t, e)
				sc.crashed(t, e, want, w1)
				e.hook = pfumCrashAt(w2)
				if _, err := e.mover().RecoverMoves(bg); !errors.Is(err, errPFUMCrash) {
					t.Fatalf("恢复在 %s 应模拟崩溃：%v", w2, err)
				}
				e.assertOneCopy(t, pfumRel, want, sc.allowNone)
				sc.recovered(t, e, want, sc.final(w1))
			})
		}
	}
}

func pfumStartScenario() pfumCrashScenario {
	return pfumCrashScenario{
		name: "开始同步",
		setup: func(t *testing.T, e *pfumEnv) pfumWant {
			return e.want(t, e.project(pfumRel))
		},
		op: func(e *pfumEnv) (PFUnitMoveResult, error) { return e.mover().StartSync(bg, pfumRel) },
		wantWindows: []string{"start:logged-begin", "start:after-mkdir", "start:logged-rename", "start:after-rename", "start:logged-link",
			"start:after-link", "start:after-verify", "start:logged-register", "start:after-register", "start:after-done"},
		final: func(w string) string {
			switch w {
			case "start:logged-begin", "start:after-mkdir", "start:logged-rename":
				return "desktop" // 还没挪
			}
			return "link"
		},
		verify: func(t *testing.T, e *pfumEnv, where string) {
			if where == "desktop" {
				e.noUnit(t, pfumCode)
				if st := e.logState(t, pfumCode); st != "" {
					t.Errorf("没挪成：日志应清掉，实际 %q", st)
				}
				if _, err := os.Lstat(e.syn); err == nil {
					t.Errorf("没挪成：这次建出来的同步目录和年份层应清掉")
				}
				return
			}
			e.wantUnit(t, pfumCode, PFUnitSyncing)
			if st := e.logState(t, pfumCode); st != PFUnitMoveLogDone {
				t.Errorf("挪成了：日志应是 done，实际 %q", st)
			}
		},
		afterwards: func(t *testing.T, e *pfumEnv, where string) {
			if where == "desktop" {
				e.mustStart(t, pfumRel)
				e.wantUnit(t, pfumCode, PFUnitSyncing)
				return
			}
			if cs, err := e.mover().CheckLinks(bg); err != nil || len(cs) != 1 || cs[0].Status != PFUnitMoveLinkOK {
				t.Errorf("链接检查：%v %+v", err, cs)
			}
			e.mustStop(t, pfumCode)
		},
	}
}

func pfumStopScenario() pfumCrashScenario {
	return pfumCrashScenario{
		name: "取消同步",
		setup: func(t *testing.T, e *pfumEnv) pfumWant {
			e.project(pfumRel)
			e.mustStart(t, pfumRel)
			return e.want(t, e.S(pfumRel))
		},
		op: func(e *pfumEnv) (PFUnitMoveResult, error) { return e.mover().StopSync(bg, pfumCode) },
		wantWindows: []string{"stop:after-stopping", "stop:logged-begin", "stop:logged-unlink", "stop:after-detach", "stop:after-unlink",
			"stop:logged-rename", "stop:after-rename", "stop:after-verify", "stop:logged-register", "stop:after-register", "stop:after-done"},
		final: func(w string) string {
			switch w {
			case "stop:after-stopping", "stop:logged-begin", "stop:logged-unlink":
				return "link" // 链接还没动：退回同步中
			}
			return "desktop" // after-detach 起链接已在临时位置：恢复补删它，接着挪回
		},
		verify: func(t *testing.T, e *pfumEnv, where string) {
			if where == "link" {
				e.wantUnit(t, pfumCode, PFUnitSyncing)
				if st := e.logState(t, pfumCode); st != "" && st != PFUnitMoveLogDone {
					t.Errorf("退回后不该留没做完的日志：%q", st)
				}
				return
			}
			u := e.wantUnit(t, pfumCode, PFUnitStopped)
			if u.LinkPath != "" || u.SyncPath != "" {
				t.Errorf("stopped 的登记不对：%+v", u)
			}
			if st := e.logState(t, pfumCode); st != PFUnitMoveLogDone {
				t.Errorf("取消完成：日志应是 done，实际 %q", st)
			}
			if _, err := os.Lstat(filepath.Join(e.syn, "2026")); err == nil {
				t.Errorf("取消完成：空了的年份层应删掉")
			}
		},
		afterwards: func(t *testing.T, e *pfumEnv, where string) {
			if where == "link" {
				e.mustStop(t, pfumCode)
				return
			}
			e.mustStart(t, pfumRel)
		},
	}
}

func pfumCreateScenario() pfumCrashScenario {
	return pfumCrashScenario{
		name: "新建同步项目",
		setup: func(t *testing.T, e *pfumEnv) pfumWant {
			return pfumWant{snap: map[string]string{}}
		},
		allowNone: true,
		op:        func(e *pfumEnv) (PFUnitMoveResult, error) { return e.mover().CreateSynced(bg, pfumRel) },
		wantWindows: []string{"create:logged-begin", "create:after-mkdir", "create:logged-link", "create:after-link",
			"create:after-verify", "create:logged-register", "create:after-register", "create:after-done"},
		final: func(w string) string {
			if w == "create:logged-begin" {
				return "none" // 什么都还没建
			}
			return "link"
		},
		verify: func(t *testing.T, e *pfumEnv, where string) {
			if where == "none" {
				e.noUnit(t, pfumCode)
				if st := e.logState(t, pfumCode); st != "" {
					t.Errorf("没建出来：日志应清掉，实际 %q", st)
				}
				return
			}
			e.wantUnit(t, pfumCode, PFUnitSyncing)
			if st := e.logState(t, pfumCode); st != PFUnitMoveLogDone {
				t.Errorf("建成了：日志应是 done，实际 %q", st)
			}
		},
		afterwards: func(t *testing.T, e *pfumEnv, where string) {
			if where == "none" {
				if res, err := e.mover().CreateSynced(bg, pfumRel); err != nil || res.Status != PFUnitMoveDone {
					t.Errorf("恢复后应能再新建：%v %+v", err, res)
				}
				return
			}
			if cs, err := e.mover().CheckLinks(bg); err != nil || len(cs) != 1 || cs[0].Status != PFUnitMoveLinkOK {
				t.Errorf("链接检查：%v %+v", err, cs)
			}
			e.mustStop(t, pfumCode)
		},
	}
}

func TestPFUnitMove_CrashStart(t *testing.T)  { pfumStartScenario().runSingle(t) }
func TestPFUnitMove_CrashStop(t *testing.T)   { pfumStopScenario().runSingle(t) }
func TestPFUnitMove_CrashCreate(t *testing.T) { pfumCreateScenario().runSingle(t) }

func TestPFUnitMove_CrashDuringRecoverStart(t *testing.T)  { pfumStartScenario().runDouble(t) }
func TestPFUnitMove_CrashDuringRecoverStop(t *testing.T)   { pfumStopScenario().runDouble(t) }
func TestPFUnitMove_CrashDuringRecoverCreate(t *testing.T) { pfumCreateScenario().runDouble(t) }

// ── 失败回退 ────────────────────────────────────────────

func TestPFUnitMove_StartLinkFailsRollsBack(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		name := "同步目录和年份层都是新建的"
		if preexisting {
			name = "同步目录和年份层原来就有"
		}
		t.Run(name, func(t *testing.T) {
			e := newPFUMEnv(t)
			p := e.project(pfumRel)
			want := e.want(t, p)
			if preexisting {
				e.put(filepath.Join(e.syn, "2026"), "别的项目/a.txt", "a")
			}
			e.symlink = func(target, link string) error { return syscall.EACCES }
			res, err := e.mover().StartSync(bg, pfumRel)
			if err != nil || res.Status != PFUnitMoveRolledBack || res.Code != PFUnitMoveCodeLinkFailed {
				t.Fatalf("建链接失败应回退：%v %+v", err, res)
			}
			if res.ErrText == "" {
				t.Errorf("回退结果应带底层原因")
			}
			// 目录回到原处：真实目录、ino 不变、内容不变，桌面上不是链接
			if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
				t.Fatalf("应回到桌面原处，实际 %s", where)
			}
			if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				t.Errorf("桌面位置应是真实目录：%v", err)
			}
			e.noUnit(t, pfumCode)
			if st := e.logState(t, pfumCode); st != "" {
				t.Errorf("日志应清掉：%q", st)
			}
			_, serr := os.Lstat(e.syn)
			_, yerr := os.Lstat(filepath.Join(e.syn, "2026"))
			if preexisting {
				if serr != nil || yerr != nil {
					t.Errorf("原来就有的同步目录和年份层不能删：%v %v", serr, yerr)
				}
				if data, _ := os.ReadFile(filepath.Join(e.syn, "2026", "别的项目", "a.txt")); string(data) != "a" {
					t.Errorf("同步目录里别的东西不能被动")
				}
			} else {
				if serr == nil {
					t.Errorf("这次新建的同步目录应清掉")
				}
				if yerr == nil {
					t.Errorf("这次新建的年份层应清掉")
				}
			}
			if rep, err := e.mover().RecoverMoves(bg); err != nil || len(rep) != 0 {
				t.Errorf("回退后没有要恢复的：%v %+v", err, rep)
			}
		})
	}
}

func TestPFUnitMove_StartRenameFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
		code  string
	}{{"目标刚被别人建了", syscall.EEXIST, PFUnitMoveCodeTargetExists}, {"跨卷", syscall.EXDEV, PFUnitMoveCodeCrossVolume}, {"没权限", syscall.EACCES, PFUnitMoveCodeRenameFailed}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPFUMEnv(t)
			p := e.project(pfumRel)
			want := e.want(t, p)
			e.rename = func(a, b string) error { return tc.errno }
			res, err := e.mover().StartSync(bg, pfumRel)
			if err != nil || res.Status != PFUnitMoveRolledBack || res.Code != tc.code {
				t.Fatalf("改名失败应回退（%s）：%v %+v", tc.code, err, res)
			}
			if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
				t.Fatalf("应还在桌面，实际 %s", where)
			}
			e.noUnit(t, pfumCode)
			if st := e.logState(t, pfumCode); st != "" {
				t.Errorf("日志应清掉：%q", st)
			}
			if _, err := os.Lstat(e.syn); err == nil {
				t.Errorf("这次新建的同步目录应清掉")
			}
		})
	}
	t.Run("报错但其实改名成功了，当成功往下做", func(t *testing.T) {
		e := newPFUMEnv(t)
		p := e.project(pfumRel)
		want := e.want(t, p)
		e.rename = func(a, b string) error {
			if err := pfRenameNoReplace(a, b); err != nil {
				return err
			}
			return syscall.EINTR
		}
		res, err := e.mover().StartSync(bg, pfumRel)
		if err != nil || res.Status != PFUnitMoveDone {
			t.Fatalf("实际挪成了应当成功：%v %+v", err, res)
		}
		if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
			t.Errorf("应是链接，实际 %s", where)
		}
	})
}

// 建链接失败、想改回去时桌面位置被别人占了：停下报告，项目数据在同步目录，占位的东西一个字节不动。
func TestPFUnitMove_StartRollbackBlockedHalts(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	want := e.want(t, p)
	e.symlink = func(target, link string) error { return syscall.EACCES }
	e.hook = func(w string) error {
		if w == "start:before-rollback" { // 就在这时，有人在桌面位置建了个目录
			os.MkdirAll(p, 0o755)
			os.WriteFile(filepath.Join(p, "占位者.txt"), []byte("mine"), 0o644)
		}
		return nil
	}
	res, err := e.mover().StartSync(bg, pfumRel)
	if err != nil || res.Status != PFUnitMoveHalted || res.Code != PFUnitMoveCodeLinkFailed || len(res.Residue) != 2 {
		t.Fatalf("改回去失败应停下报告：%v %+v", err, res)
	}
	s := e.S(pfumRel)
	pfumSameSnap(t, "同步目录里的项目", pfumSnap(t, s), want.snap)
	if pfumIno(t, s) != want.ino {
		t.Errorf("项目目录 ino 变了")
	}
	if data, _ := os.ReadFile(filepath.Join(p, "占位者.txt")); string(data) != "mine" {
		t.Errorf("桌面位置上别人放的东西不能被动")
	}
	if st := e.logState(t, pfumCode); st != PFUnitMoveLogMoving {
		t.Errorf("停下时日志应留着：%q", st)
	}
	e.noUnit(t, pfumCode)
	// 重启恢复：现场（P 是别的目录、S 是项目）不认识，停下，什么都不动
	e.hook, e.symlink = nil, nil
	home := pfumSnap(t, e.home)
	for i := 0; i < 2; i++ {
		rep, err := e.mover().RecoverMoves(bg)
		if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted || rep[0].Code != PFUnitMoveCodeUnknownState || len(rep[0].Residue) != 2 {
			t.Fatalf("恢复应停下报告：%v %+v", err, rep)
		}
		pfumSameSnap(t, "恢复停下后整个家目录", pfumSnap(t, e.home), home)
	}
	// 人把桌面上占位的东西挪走之后，恢复能补完
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	rep, err := e.mover().RecoverMoves(bg)
	if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveDone || rep[0].Code != PFUnitMoveCodeCompleted {
		t.Fatalf("现场清理后应补完：%v %+v", err, rep)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
		t.Errorf("补完后应是链接：%s", where)
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
}

// 建完链接核对不符：停下报告，不删任何东西（项目、那个不对的链接都原样留着）。
func TestPFUnitMove_StartVerifyMismatchHaltsWithoutDeleting(t *testing.T) {
	t.Run("链接被换成指向别处的链接", func(t *testing.T) {
		e := newPFUMEnv(t)
		p := e.project(pfumRel)
		want := e.want(t, p)
		other := filepath.Join(e.home, "别处")
		os.MkdirAll(other, 0o755)
		e.hook = func(w string) error {
			if w == "start:after-link" {
				os.Remove(p)
				os.Symlink(other, p)
			}
			return nil
		}
		res, err := e.mover().StartSync(bg, pfumRel)
		if err != nil || res.Status != PFUnitMoveHalted || res.Code != PFUnitMoveCodeVerifyFailed || len(res.Residue) != 2 {
			t.Fatalf("核对不符应停下报告：%v %+v", err, res)
		}
		if tgt, _ := os.Readlink(p); tgt != other {
			t.Errorf("那个不对的链接不能被删或改：%q", tgt)
		}
		pfumSameSnap(t, "同步目录里的项目", pfumSnap(t, e.S(pfumRel)), want.snap)
		e.noUnit(t, pfumCode)
		lg, _ := PFUnitMoveReadLog(e.meta, pfumCode)
		if lg == nil || lg.State != PFUnitMoveLogMoving || lg.Step != "verify-failed" {
			t.Errorf("日志应留着并记下核对失败：%+v", lg)
		}
		// 恢复也停下，不动
		e.hook = nil
		home := pfumSnap(t, e.home)
		rep, err := e.mover().RecoverMoves(bg)
		if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted {
			t.Fatalf("恢复应停下：%v %+v", err, rep)
		}
		pfumSameSnap(t, "恢复停下后整个家目录", pfumSnap(t, e.home), home)
		// 人删掉那个不对的链接后，恢复补完
		os.Remove(p)
		rep, err = e.mover().RecoverMoves(bg)
		if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveDone {
			t.Fatalf("现场清理后应补完：%v %+v", err, rep)
		}
		if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
			t.Errorf("补完后应是链接：%s", where)
		}
	})
	t.Run("ino 对不上", func(t *testing.T) {
		e := newPFUMEnv(t)
		p := e.project(pfumRel)
		want := e.want(t, p)
		skew := false
		e.lstatMod = func(path string, st *PFStat) {
			if skew && path == e.S(pfumRel) {
				st.Ino++
			}
		}
		e.hook = func(w string) error {
			if w == "start:after-link" {
				skew = true
			}
			return nil
		}
		res, err := e.mover().StartSync(bg, pfumRel)
		if err != nil || res.Status != PFUnitMoveHalted || res.Code != PFUnitMoveCodeVerifyFailed {
			t.Fatalf("ino 对不上应停下报告：%v %+v", err, res)
		}
		skew = false
		if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
			t.Errorf("什么都不该被删：%s", where)
		}
		e.noUnit(t, pfumCode)
		e.hook = nil
		rep, err := e.mover().RecoverMoves(bg)
		if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveDone {
			t.Fatalf("现场没问题了，恢复应补登记：%v %+v", err, rep)
		}
		e.wantUnit(t, pfumCode, PFUnitSyncing)
	})
}

// 登记 units.json 失败：项目已挪好、链接已建好，停下报告，恢复时补登记。
func TestPFUnitMove_StartRegisterFailsThenRecoverRegisters(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	want := e.want(t, p)
	e.hook = func(w string) error {
		if w == "start:logged-register" {
			os.MkdirAll(e.meta, 0o755)
			os.WriteFile(PFUnitMoveUnitsPath(e.meta), []byte("{坏掉了"), 0o600)
		}
		return nil
	}
	res, err := e.mover().StartSync(bg, pfumRel)
	if err != nil || res.Status != PFUnitMoveHalted || res.Code != PFUnitMoveCodeRegisterFailed {
		t.Fatalf("登记失败应停下报告：%v %+v", err, res)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
		t.Errorf("项目和链接应完好：%s", where)
	}
	if got, _ := os.ReadFile(PFUnitMoveUnitsPath(e.meta)); string(got) != "{坏掉了" {
		t.Errorf("坏的 units.json 不能被改：%q", got)
	}
	// 登记表还是坏的：恢复停下
	e.hook = nil
	rep, err := e.mover().RecoverMoves(bg)
	if err != nil || len(rep) < 1 || rep[0].Status != PFUnitMoveHalted {
		t.Fatalf("登记表坏着时恢复应停下：%v %+v", err, rep)
	}
	// 人修好（删掉坏文件）后补登记
	os.Remove(PFUnitMoveUnitsPath(e.meta))
	rep, err = e.mover().RecoverMoves(bg)
	if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveDone {
		t.Fatalf("修好后应补登记：%v %+v", err, rep)
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
}

// ── 取消同步的停下与失败 ─────────────────────────────────

func TestPFUnitMove_StopHaltsWhenPIsNotOurLink(t *testing.T) {
	for _, kind := range []string{"真实目录", "指向别处的链接", "普通文件"} {
		t.Run(kind, func(t *testing.T) {
			e := newPFUMEnv(t)
			p := e.project(pfumRel)
			want := e.want(t, p)
			e.mustStart(t, pfumRel)
			// 用户把链接删了，换成别的东西
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			elsewhere := filepath.Join(e.home, "别处")
			os.MkdirAll(elsewhere, 0o755)
			e.put(elsewhere, "别处.txt", "else")
			switch kind {
			case "真实目录":
				e.put(p, "用户的文件.txt", "user-data")
			case "指向别处的链接":
				os.Symlink(elsewhere, p)
			case "普通文件":
				os.WriteFile(p, []byte("a-file"), 0o644)
			}
			homeBefore := pfumSnap(t, e.home)
			res, err := e.mover().StopSync(bg, pfumCode)
			if err != nil || res.Status != PFUnitMoveHalted || res.Code != PFUnitMoveCodeNotOurLink || len(res.Residue) != 2 {
				t.Fatalf("P 不是我们建的链接应停下报告：%v %+v", err, res)
			}
			// 一个字节都没动：整个家目录（除了登记表在 stopping 和退回之间来回）内容不变
			pfumSameSnap(t, "停下后整个家目录", pfumSnap(t, e.home), homeBefore)
			pfumSameSnap(t, "同步目录里的项目", pfumSnap(t, e.S(pfumRel)), want.snap)
			switch kind {
			case "真实目录":
				if data, _ := os.ReadFile(filepath.Join(p, "用户的文件.txt")); string(data) != "user-data" {
					t.Errorf("用户换上的目录不能被动")
				}
			case "指向别处的链接":
				if tgt, _ := os.Readlink(p); tgt != elsewhere {
					t.Errorf("别处的链接不能被动：%q", tgt)
				}
			}
			e.wantUnit(t, pfumCode, PFUnitSyncing)
			if st := e.logState(t, pfumCode); st != PFUnitMoveLogDone {
				t.Errorf("没动手就不该留取消的日志：%q", st)
			}
		})
	}
}

func TestPFUnitMove_StopWhenLinkAlreadyGone(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	want := e.want(t, p)
	e.mustStart(t, pfumRel)
	os.Remove(p) // 链接没了，位置是空的
	e.mustStop(t, pfumCode)
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
		t.Errorf("应回到桌面：%s", where)
	}
	e.wantUnit(t, pfumCode, PFUnitStopped)
}

func TestPFUnitMove_StopRefusedWhileBusy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *pfumEnv, s string)
		code  string
	}{
		{"Office 锁", func(e *pfumEnv, s string) { e.put(s, "01 合同/~$合同.docx", "lock") }, PFUnitMoveCodeOfficeLock},
		{"最近有写入", func(e *pfumEnv, s string) {
			os.WriteFile(filepath.Join(s, "刚写的.txt"), []byte("now"), 0o644)
		}, PFUnitMoveCodeNotQuiet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPFUMEnv(t)
			p := e.project(pfumRel)
			e.mustStart(t, pfumRel)
			s := e.S(pfumRel)
			tc.setup(e, s)
			want := pfumWant{snap: pfumSnap(t, s), ino: pfumIno(t, s)}
			res, err := e.mover().StopSync(bg, pfumCode)
			if err != nil || res.Status != PFUnitMoveRefused || !pfumHas(pfumRefusalCodes(res), tc.code) {
				t.Fatalf("应被拒绝（%s）：%v %+v", tc.code, err, res)
			}
			if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
				t.Errorf("链接应原样：%s", where)
			}
			if tgt, _ := os.Readlink(p); tgt != s {
				t.Errorf("链接不能被动")
			}
			e.wantUnit(t, pfumCode, PFUnitSyncing)
		})
	}
	t.Run("残留的锁（超过 12 小时）不挡", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		e.mustStart(t, pfumRel)
		lock := e.put(e.S(pfumRel), "01 合同/~$合同.docx", "lock")
		old := time.Now().Add(-13 * time.Hour)
		os.Chtimes(lock, old, old)
		res := e.mustStop(t, pfumCode)
		if len(res.Notes) == 0 {
			t.Errorf("残留的锁应写进 Notes")
		}
	})
}

func TestPFUnitMove_StopErrors(t *testing.T) {
	e := newPFUMEnv(t)
	e.project(pfumRel)
	res, err := e.mover().StopSync(bg, "260403J9999")
	if err != nil || res.Status != PFUnitMoveRefused || res.Code != PFUnitMoveCodeNotRegistered {
		t.Errorf("没登记的应拒绝：%v %+v", err, res)
	}
	res, err = e.mover().StopSync(bg, "不是编号")
	if err != nil || res.Status != PFUnitMoveRefused || res.Code != PFUnitMoveCodeNoCode {
		t.Errorf("不是编号应拒绝：%v %+v", err, res)
	}
	// 编号大小写、空白归一化
	e.mustStart(t, pfumRel)
	e.mustStop(t, " 260403j1001 ")
	res, err = e.mover().StopSync(bg, pfumCode)
	if err != nil || res.Status != PFUnitMoveRefused || res.Code != PFUnitMoveCodeBadState {
		t.Errorf("已停止的应拒绝：%v %+v", err, res)
	}
	// 有没做完的日志
	e.mustStart(t, pfumRel)
	lg := PFUnitMoveLog{Version: 1, Code: pfumCode, Op: PFUnitMoveOpStop, State: PFUnitMoveLogMovingBack, Step: "rename", Rel: pfumRel,
		P: e.P(pfumRel), S: e.S(pfumRel), Ino: 1, Dev: 1, StartedAt: time.Now(), UpdatedAt: time.Now()}
	if err := e.mover().writeLog(&lg, lg.State, lg.Step); err != nil {
		t.Fatal(err)
	}
	res, err = e.mover().StopSync(bg, pfumCode)
	if err != nil || res.Status != PFUnitMoveRefused || res.Code != PFUnitMoveCodeMovePending {
		t.Errorf("有没做完的日志应拒绝：%v %+v", err, res)
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
}

func TestPFUnitMove_StopCanceled(t *testing.T) {
	e := newPFUMEnv(t)
	e.project(pfumRel)
	e.mustStart(t, pfumRel)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	res, err := e.mover().StopSync(ctx, pfumCode)
	if !errors.Is(err, context.Canceled) || res.Code != PFUnitMoveCodeCanceled {
		t.Fatalf("取消应返回 canceled：%v %+v", err, res)
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
	if tgt, err := os.Readlink(e.P(pfumRel)); err != nil || tgt != e.S(pfumRel) {
		t.Errorf("链接不能被动")
	}
}

// S→P 改名失败：把链接建回去，项目仍在同步目录里同步。
func TestPFUnitMove_StopRenameFailsRelinks(t *testing.T) {
	e := newPFUMEnv(t)
	e.project(pfumRel)
	e.mustStart(t, pfumRel)
	want := e.want(t, e.S(pfumRel))
	e.rename = pfumFailRenameFrom(e.S(pfumRel), syscall.EXDEV) // 只让 S→P 失败；桌面链接移到临时位置的改名要成功
	res, err := e.mover().StopSync(bg, pfumCode)
	if err != nil || res.Status != PFUnitMoveRolledBack || res.Code != PFUnitMoveCodeCrossVolume {
		t.Fatalf("改名失败应回退：%v %+v", err, res)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
		t.Errorf("应把链接建回去：%s", where)
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
	if st := e.logState(t, pfumCode); st != "" {
		t.Errorf("日志应清掉：%q", st)
	}
	if cs, err := e.mover().CheckLinks(bg); err != nil || len(cs) != 1 || cs[0].Status != PFUnitMoveLinkOK {
		t.Errorf("链接检查：%v %+v", err, cs)
	}
	// 之后条件好了可以再取消
	e.rename = nil
	e.mustStop(t, pfumCode)
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
		t.Errorf("应回到桌面：%s", where)
	}
}

// 改名失败、链接也建不回去：停下报告（项目在同步目录里完好），恢复在条件好了之后补完。
func TestPFUnitMove_StopRenameAndRelinkFailHalts(t *testing.T) {
	e := newPFUMEnv(t)
	e.project(pfumRel)
	e.mustStart(t, pfumRel)
	want := e.want(t, e.S(pfumRel))
	e.rename = pfumFailRenameFrom(e.S(pfumRel), syscall.EXDEV)
	e.symlink = func(target, link string) error { return syscall.EACCES }
	res, err := e.mover().StopSync(bg, pfumCode)
	if err != nil || res.Status != PFUnitMoveHalted || len(res.Residue) != 2 {
		t.Fatalf("应停下报告：%v %+v", err, res)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "sync-only" {
		t.Fatalf("项目应完好地在同步目录：%s", where)
	}
	e.wantUnit(t, pfumCode, PFUnitStopping)
	if st := e.logState(t, pfumCode); st != PFUnitMoveLogMovingBack {
		t.Errorf("日志应留着：%q", st)
	}
	// 条件还没好：恢复也停下，不动
	rep, err := e.mover().RecoverMoves(bg)
	if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted {
		t.Fatalf("条件没好时恢复应停下：%v %+v", err, rep)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "sync-only" {
		t.Fatalf("项目应完好：%s", where)
	}
	// 条件好了：补完取消
	e.rename, e.symlink = nil, nil
	rep, err = e.mover().RecoverMoves(bg)
	if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveDone || rep[0].Code != PFUnitMoveCodeCompleted {
		t.Fatalf("条件好了应补完：%v %+v", err, rep)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
		t.Errorf("应回到桌面：%s", where)
	}
	e.wantUnit(t, pfumCode, PFUnitStopped)
}

// pfumFailRenameFrom 返回一个改名函数：来源是 from 时失败（返回 errno），其余照常不覆盖地改名。
func pfumFailRenameFrom(from string, errno syscall.Errno) func(a, b string) error {
	return func(a, b string) error {
		if a == from {
			return errno
		}
		return pfRenameNoReplace(a, b)
	}
}

// unlinkTmps 返回 <meta>/moves/ 下取消同步暂放桌面链接的临时位置（<编号>.unlink-*）。
func (e *pfumEnv) unlinkTmps(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.meta, "moves"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range ents {
		if strings.Contains(en.Name(), ".unlink-") {
			out = append(out, filepath.Join(e.meta, "moves", en.Name()))
		}
	}
	return out
}

// ── 取消同步：桌面链接被换掉、先改名到临时位置再核对、再 unlink ──────────

// 桌面链接在「核对之后、改名之前」被换成别的东西（用户的普通文件、目录、指向别处的链接）：
// 换上的东西被不覆盖地移到临时位置，核对不是链接，原样放回桌面位置；一个字节不丢，停下报告，同步目录里的项目不动。
func TestPFUnitMove_StopLinkSwappedBeforeDetach(t *testing.T) {
	for _, kind := range []string{"普通文件", "目录", "指向别处的链接"} {
		t.Run(kind, func(t *testing.T) {
			e := newPFUMEnv(t)
			p := e.project(pfumRel)
			want := e.want(t, p)
			e.mustStart(t, pfumRel)
			elsewhere := filepath.Join(e.home, "别处")
			e.put(elsewhere, "别处.txt", "else")
			var swappedIno uint64
			e.hook = func(w string) error {
				if w != "stop:logged-unlink" { // 核对过「是链接」、日志也写了，改名之前
					return nil
				}
				if err := os.Remove(p); err != nil {
					t.Error(err)
					return nil
				}
				switch kind {
				case "普通文件":
					os.WriteFile(p, []byte("用户刚存的文件"), 0o644)
				case "目录":
					e.put(p, "用户的文件.txt", "user-data")
				case "指向别处的链接":
					os.Symlink(elsewhere, p)
				}
				swappedIno = pfumIno(t, p)
				return nil
			}
			res, err := e.mover().StopSync(bg, pfumCode)
			if err != nil || res.Status != PFUnitMoveHalted || res.Code != PFUnitMoveCodeNotOurLink || len(res.Residue) != 2 {
				t.Fatalf("桌面链接被换掉应停下报告：%v %+v", err, res)
			}
			// 换上的东西原样回到桌面位置（同一个 ino，一个字节不丢）
			if got := pfumIno(t, p); got != swappedIno {
				t.Errorf("换上的东西应原样放回（ino %d → %d）", swappedIno, got)
			}
			switch kind {
			case "普通文件":
				if data, _ := os.ReadFile(p); string(data) != "用户刚存的文件" {
					t.Errorf("用户的文件被动了：%q", data)
				}
			case "目录":
				if data, _ := os.ReadFile(filepath.Join(p, "用户的文件.txt")); string(data) != "user-data" {
					t.Errorf("用户的目录被动了")
				}
			case "指向别处的链接":
				if tgt, _ := os.Readlink(p); tgt != elsewhere {
					t.Errorf("别处的链接被动了：%q", tgt)
				}
			}
			pfumSameSnap(t, "同步目录里的项目", pfumSnap(t, e.S(pfumRel)), want.snap)
			if got := pfumIno(t, e.S(pfumRel)); got != want.ino {
				t.Errorf("项目目录 ino 变了：%d → %d", want.ino, got)
			}
			if tmps := e.unlinkTmps(t); len(tmps) != 0 {
				t.Errorf("临时位置不该留东西：%v", tmps)
			}
			e.wantUnit(t, pfumCode, PFUnitSyncing)
			if st := e.logState(t, pfumCode); st != "" {
				t.Errorf("放回去之后日志应清掉：%q", st)
			}
			if rep, err := e.mover().RecoverMoves(bg); err != nil || len(rep) != 0 {
				t.Errorf("没有要恢复的：%v %+v", err, rep)
			}
		})
	}
}

// 换上的东西移到临时位置之后，桌面位置又被占了：放不回去，留在临时位置，停下报告；日志和 stopping 都留着，
// 重启恢复也停下、不动任何东西；人把占位的拿走之后恢复把它放回去。
func TestPFUnitMove_StopLinkSwappedAndPOccupiedAgain(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	want := e.want(t, p)
	e.mustStart(t, pfumRel)
	e.hook = func(w string) error {
		switch w {
		case "stop:logged-unlink":
			os.Remove(p)
			os.WriteFile(p, []byte("文件甲"), 0o644)
		case "stop:after-detach": // 文件甲已被移到临时位置；这时桌面位置又被人放了个目录
			e.put(p, "乙.txt", "乙")
		}
		return nil
	}
	res, err := e.mover().StopSync(bg, pfumCode)
	if err != nil || res.Status != PFUnitMoveHalted || res.Code != PFUnitMoveCodeNotOurLink {
		t.Fatalf("放不回去应停下报告：%v %+v", err, res)
	}
	tmps := e.unlinkTmps(t)
	if len(tmps) != 1 {
		t.Fatalf("文件甲应留在临时位置：%v", tmps)
	}
	if data, _ := os.ReadFile(tmps[0]); string(data) != "文件甲" {
		t.Errorf("临时位置上的内容 = %q", data)
	}
	if !strings.Contains(strings.Join(res.Residue, "\n"), tmps[0]) {
		t.Errorf("Residue 应写明临时位置 %s：%v", tmps[0], res.Residue)
	}
	if data, _ := os.ReadFile(filepath.Join(p, "乙.txt")); string(data) != "乙" {
		t.Errorf("桌面位置上别人放的东西不能被动")
	}
	pfumSameSnap(t, "同步目录里的项目", pfumSnap(t, e.S(pfumRel)), want.snap)
	e.wantUnit(t, pfumCode, PFUnitStopping)
	if st := e.logState(t, pfumCode); st != PFUnitMoveLogMovingBack {
		t.Errorf("停下时日志应留着：%q", st)
	}
	// 再取消：有没做完的日志，拒绝
	e.hook = nil
	if r, err := e.mover().StopSync(bg, pfumCode); err != nil || r.Status != PFUnitMoveRefused || r.Code != PFUnitMoveCodeMovePending {
		t.Errorf("有没做完的日志应拒绝：%v %+v", err, r)
	}
	// 重启恢复：停下，整个家目录一个字节不动
	home := pfumSnap(t, e.home)
	for i := 0; i < 2; i++ {
		rep, err := e.mover().RecoverMoves(bg)
		if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted || rep[0].Code != PFUnitMoveCodeUnknownState {
			t.Fatalf("恢复应停下报告：%v %+v", err, rep)
		}
		pfumSameSnap(t, "恢复停下后整个家目录", pfumSnap(t, e.home), home)
	}
	// 人把桌面位置上占位的目录拿走：恢复把临时位置上的文件甲放回桌面位置，停下报告（桌面位置不是我们的链接）
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	rep, err := e.mover().RecoverMoves(bg)
	if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted || rep[0].Code != PFUnitMoveCodeNotOurLink {
		t.Fatalf("占位拿走后应把文件甲放回去并停下报告：%v %+v", err, rep)
	}
	if data, _ := os.ReadFile(p); string(data) != "文件甲" {
		t.Errorf("文件甲应回到桌面位置：%q", data)
	}
	if tmps := e.unlinkTmps(t); len(tmps) != 0 {
		t.Errorf("临时位置不该留东西：%v", tmps)
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
	if rep, err := e.mover().RecoverMoves(bg); err != nil || len(rep) != 0 {
		t.Errorf("没有要恢复的：%v %+v", err, rep)
	}
}

// 崩在「桌面链接已改名到临时位置、还没核对」：桌面位置空着，链接在临时位置，项目在同步目录；恢复补删链接、接着挪回桌面。
func TestPFUnitMove_StopCrashAfterDetachRecovers(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	want := e.want(t, p)
	e.mustStart(t, pfumRel)
	s := e.S(pfumRel)
	e.hook = pfumCrashAt("stop:after-detach")
	if _, err := e.mover().StopSync(bg, pfumCode); !errors.Is(err, errPFUMCrash) {
		t.Fatalf("应模拟崩溃：%v", err)
	}
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("桌面位置应空着：%v", err)
	}
	tmps := e.unlinkTmps(t)
	if len(tmps) != 1 {
		t.Fatalf("链接应在临时位置：%v", tmps)
	}
	if tgt, err := os.Readlink(tmps[0]); err != nil || tgt != s {
		t.Fatalf("临时位置上应是指向 %s 的链接：%q %v", s, tgt, err)
	}
	lg, err := PFUnitMoveReadLog(e.meta, pfumCode)
	if err != nil || lg == nil || lg.UnlinkTmp != tmps[0] || lg.State != PFUnitMoveLogMovingBack {
		t.Fatalf("日志里应记着临时位置：%v %+v", err, lg)
	}
	e.assertOneCopy(t, pfumRel, want, false)
	e.hook = nil
	rep, err := e.mover().RecoverMoves(bg)
	if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveDone || rep[0].Code != PFUnitMoveCodeCompleted {
		t.Fatalf("应补完取消同步：%v %+v", err, rep)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "desktop" {
		t.Errorf("应回到桌面：%s", where)
	}
	if tmps := e.unlinkTmps(t); len(tmps) != 0 {
		t.Errorf("临时位置上的链接应已删：%v", tmps)
	}
	e.wantUnit(t, pfumCode, PFUnitStopped)
}

// 崩在「桌面链接已改名到临时位置」，而移到临时位置的其实是用户在窗口里换上的普通文件：
// 恢复不能 unlink 它，要原样放回桌面位置并停下报告；桌面位置又被占了就不动、停下。
func TestPFUnitMove_StopCrashAfterDetachOfSwappedFile(t *testing.T) {
	setup := func(t *testing.T) (e *pfumEnv, p string, want pfumWant) {
		e = newPFUMEnv(t)
		p = e.project(pfumRel)
		want = e.want(t, p)
		e.mustStart(t, pfumRel)
		e.hook = func(w string) error {
			switch w {
			case "stop:logged-unlink":
				os.Remove(p)
				os.WriteFile(p, []byte("用户刚存的文件"), 0o644)
			case "stop:after-detach":
				return errPFUMCrash
			}
			return nil
		}
		if _, err := e.mover().StopSync(bg, pfumCode); !errors.Is(err, errPFUMCrash) {
			t.Fatalf("应模拟崩溃：%v", err)
		}
		e.hook = nil
		return
	}
	t.Run("桌面位置空着：放回去", func(t *testing.T) {
		e, p, want := setup(t)
		rep, err := e.mover().RecoverMoves(bg)
		if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted || rep[0].Code != PFUnitMoveCodeNotOurLink {
			t.Fatalf("应放回去并停下报告：%v %+v", err, rep)
		}
		if data, _ := os.ReadFile(p); string(data) != "用户刚存的文件" {
			t.Errorf("用户的文件应原样回到桌面位置：%q", data)
		}
		if tmps := e.unlinkTmps(t); len(tmps) != 0 {
			t.Errorf("临时位置不该留东西：%v", tmps)
		}
		pfumSameSnap(t, "同步目录里的项目", pfumSnap(t, e.S(pfumRel)), want.snap)
		e.wantUnit(t, pfumCode, PFUnitSyncing)
		if st := e.logState(t, pfumCode); st != "" {
			t.Errorf("放回去之后日志应清掉：%q", st)
		}
	})
	t.Run("桌面位置又被占了：不动", func(t *testing.T) {
		e, p, _ := setup(t)
		e.put(p, "占位.txt", "x")
		home := pfumSnap(t, e.home)
		for i := 0; i < 2; i++ {
			rep, err := e.mover().RecoverMoves(bg)
			if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted || rep[0].Code != PFUnitMoveCodeUnknownState {
				t.Fatalf("现场不认识应停下：%v %+v", err, rep)
			}
			pfumSameSnap(t, "停下后整个家目录", pfumSnap(t, e.home), home)
		}
		tmps := e.unlinkTmps(t)
		if len(tmps) != 1 {
			t.Fatalf("用户的文件应还在临时位置：%v", tmps)
		}
		if data, _ := os.ReadFile(tmps[0]); string(data) != "用户刚存的文件" {
			t.Errorf("临时位置上的内容 = %q", data)
		}
	})
}

// 日志里记的临时位置不在 <meta>/moves/ 下、或名字不是这个编号的：不认识，停下，什么都不动。
func TestPFUnitMove_RecoverStopUnknownUnlinkTmpHalts(t *testing.T) {
	for _, bad := range []string{"别处/x.unlink-1", "moves/260403J1001.json", "moves/别的编号.unlink-1"} {
		t.Run(bad, func(t *testing.T) {
			e := newPFUMEnv(t)
			e.project(pfumRel)
			e.mustStart(t, pfumRel)
			e.hook = pfumCrashAt("stop:logged-unlink")
			if _, err := e.mover().StopSync(bg, pfumCode); !errors.Is(err, errPFUMCrash) {
				t.Fatalf("应模拟崩溃：%v", err)
			}
			e.hook = nil
			lg, err := PFUnitMoveReadLog(e.meta, pfumCode)
			if err != nil || lg == nil || lg.UnlinkTmp == "" {
				t.Fatalf("日志里应有临时位置：%v %+v", err, lg)
			}
			if strings.HasPrefix(bad, "moves/") {
				lg.UnlinkTmp = filepath.Join(e.meta, bad)
			} else {
				lg.UnlinkTmp = filepath.Join(e.home, bad)
			}
			if err := e.mover().writeLog(lg, lg.State, lg.Step); err != nil {
				t.Fatal(err)
			}
			home := pfumSnap(t, e.home)
			for i := 0; i < 2; i++ {
				rep, err := e.mover().RecoverMoves(bg)
				if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted || rep[0].Code != PFUnitMoveCodeUnknownState {
					t.Fatalf("临时位置不认识应停下：%v %+v", err, rep)
				}
				pfumSameSnap(t, "停下后整个家目录", pfumSnap(t, e.home), home)
			}
		})
	}
}

// 桌面链接所在目录与 meta 不同卷：改名到临时位置做不了，取消同步开始前就拒绝，什么都不动。
func TestPFUnitMove_StopRefusedWhenMetaOnOtherVolume(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	want := e.want(t, p)
	e.mustStart(t, pfumRel)
	m := e.mover() // 先建执行器（建的时候要校验同步目录与 meta 同卷），再让 meta 下的东西「变成」另一个卷
	e.lstatMod = func(path string, st *PFStat) {
		if pfumUnder(path, e.meta) {
			st.Dev += 12345
		}
	}
	res, err := m.StopSync(bg, pfumCode)
	e.lstatMod = nil
	if err != nil || res.Status != PFUnitMoveRefused || !pfumHas(pfumRefusalCodes(res), PFUnitMoveCodeCrossVolume) {
		t.Fatalf("不同卷应拒绝：%v %+v", err, res)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
		t.Errorf("链接应原样：%s", where)
	}
	if tgt, _ := os.Readlink(e.P(pfumRel)); tgt != e.S(pfumRel) {
		t.Errorf("链接不能被动")
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
	if st := e.logState(t, pfumCode); st != PFUnitMoveLogDone {
		t.Errorf("没动手就不该留取消的日志：%q", st)
	}
	if tmps := e.unlinkTmps(t); len(tmps) != 0 {
		t.Errorf("临时位置不该留东西：%v", tmps)
	}
	e.mustStop(t, pfumCode) // 条件好了可以取消
}

// 桌面链接改名到临时位置失败（比如跨卷）：一切照旧，链接还在。
func TestPFUnitMove_StopDetachRenameFailsRollsBack(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(pfumRel)
	want := e.want(t, p)
	e.mustStart(t, pfumRel)
	e.rename = pfumFailRenameFrom(p, syscall.EXDEV)
	res, err := e.mover().StopSync(bg, pfumCode)
	if err != nil || res.Status != PFUnitMoveRolledBack || res.Code != PFUnitMoveCodeUnlinkFailed {
		t.Fatalf("改名失败应回退：%v %+v", err, res)
	}
	if where := e.assertOneCopy(t, pfumRel, want, false); where != "link" {
		t.Errorf("链接应原样：%s", where)
	}
	e.wantUnit(t, pfumCode, PFUnitSyncing)
	if st := e.logState(t, pfumCode); st != "" {
		t.Errorf("日志应清掉：%q", st)
	}
	if tmps := e.unlinkTmps(t); len(tmps) != 0 {
		t.Errorf("临时位置不该留东西：%v", tmps)
	}
	e.rename = nil
	e.mustStop(t, pfumCode)
}

// ── 恢复遇到不认识的现场：停下报告，一个字节都不动 ────────────

func TestPFUnitMove_RecoverUnknownScenesHalt(t *testing.T) {
	type scene struct {
		name  string
		start bool // true：开始同步的日志；false：取消同步的日志
		mut   func(e *pfumEnv)
	}
	P, S := func(e *pfumEnv) string { return e.P(pfumRel) }, func(e *pfumEnv) string { return e.S(pfumRel) }
	scenes := []scene{
		{"开始：桌面和同步目录里各有一个目录", true, func(e *pfumEnv) { e.put(S(e), "别人.txt", "x") }},
		{"开始：桌面目录被换成别的目录（ino 不同）", true, func(e *pfumEnv) {
			os.Rename(P(e), P(e)+".旧")
			e.put(P(e), "新的.txt", "n")
		}},
		{"开始：两边都没有", true, func(e *pfumEnv) { os.Rename(P(e), filepath.Join(e.home, "藏起来")) }},
		{"开始：桌面是指向别处的链接", true, func(e *pfumEnv) {
			os.Rename(P(e), filepath.Join(e.home, "藏起来"))
			os.Symlink(e.home, P(e))
		}},
		{"开始：桌面是普通文件", true, func(e *pfumEnv) {
			os.Rename(P(e), filepath.Join(e.home, "藏起来"))
			os.WriteFile(P(e), []byte("f"), 0o644)
		}},
		{"取消：桌面和同步目录里各有一个目录", false, func(e *pfumEnv) {
			os.Remove(P(e))
			e.put(P(e), "用户的.txt", "u")
		}},
		{"取消：两边都没有", false, func(e *pfumEnv) {
			os.Remove(P(e))
			os.Rename(S(e), filepath.Join(e.home, "藏起来"))
		}},
		{"取消：桌面是指向别处的链接", false, func(e *pfumEnv) {
			os.Remove(P(e))
			os.Symlink(e.home, P(e))
		}},
	}
	for _, sc := range scenes {
		t.Run(sc.name, func(t *testing.T) {
			e := newPFUMEnv(t)
			e.project(pfumRel)
			if sc.start {
				e.hook = pfumCrashAt("start:logged-begin")
				if _, err := e.mover().StartSync(bg, pfumRel); !errors.Is(err, errPFUMCrash) {
					t.Fatal(err)
				}
			} else {
				e.mustStart(t, pfumRel)
				e.hook = pfumCrashAt("stop:logged-begin")
				if _, err := e.mover().StopSync(bg, pfumCode); !errors.Is(err, errPFUMCrash) {
					t.Fatal(err)
				}
			}
			e.hook = nil
			sc.mut(e)
			before := pfumSnap(t, e.home)
			for i := 0; i < 2; i++ {
				rep, err := e.mover().RecoverMoves(bg)
				if err != nil || len(rep) < 1 {
					t.Fatalf("恢复应有报告：%v %+v", err, rep)
				}
				found := false
				for _, r := range rep {
					if r.Unit == pfumCode && r.Status == PFUnitMoveHalted && r.Code == PFUnitMoveCodeUnknownState && len(r.Residue) == 2 {
						found = true
					}
				}
				if !found {
					t.Fatalf("现场不认识应停下报告（unknown_state）：%+v", rep)
				}
				pfumSameSnap(t, "停下后整个家目录", pfumSnap(t, e.home), before)
			}
		})
	}
}

func TestPFUnitMove_RecoverStoppingWithoutLog(t *testing.T) {
	t.Run("链接完好：退回 syncing", func(t *testing.T) {
		e := newPFUMEnv(t)
		e.project(pfumRel)
		e.mustStart(t, pfumRel)
		if err := e.mover().setUnitState(pfumCode, PFUnitStopping); err != nil {
			t.Fatal(err)
		}
		rep, err := e.mover().RecoverMoves(bg)
		if err != nil || len(rep) != 1 || rep[0].Code != PFUnitMoveCodeReverted || rep[0].Status != PFUnitMoveDone {
			t.Fatalf("应退回同步中：%v %+v", err, rep)
		}
		e.wantUnit(t, pfumCode, PFUnitSyncing)
	})
	t.Run("链接被换成目录：停下，不动", func(t *testing.T) {
		e := newPFUMEnv(t)
		p := e.project(pfumRel)
		e.mustStart(t, pfumRel)
		if err := e.mover().setUnitState(pfumCode, PFUnitStopping); err != nil {
			t.Fatal(err)
		}
		os.Remove(p)
		e.put(p, "用户的.txt", "u")
		before := pfumSnap(t, e.home)
		rep, err := e.mover().RecoverMoves(bg)
		if err != nil || len(rep) != 1 || rep[0].Status != PFUnitMoveHalted {
			t.Fatalf("应停下：%v %+v", err, rep)
		}
		pfumSameSnap(t, "停下后整个家目录", pfumSnap(t, e.home), before)
	})
}

func TestPFUnitMove_RecoverCleansTempAndHandlesBadLog(t *testing.T) {
	e := newPFUMEnv(t)
	e.project(pfumRel)
	dir := filepath.Join(e.meta, "moves")
	os.MkdirAll(dir, 0o755)
	tmp := filepath.Join(dir, "260403J1001.json.tmp-12345")
	os.WriteFile(tmp, []byte("半截"), 0o600)
	os.WriteFile(filepath.Join(dir, "260403J1002.json"), []byte("{坏日志"), 0o600)
	rep, err := e.mover().RecoverMoves(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tmp); err == nil {
		t.Errorf("上次写日志崩溃留下的临时文件应删掉")
	}
	if len(rep) != 1 || rep[0].Unit != "260403J1002" || rep[0].Status != PFUnitMoveHalted {
		t.Errorf("坏日志应停下报告：%+v", rep)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "260403J1002.json")); string(data) != "{坏日志" {
		t.Errorf("坏日志不能被改")
	}
}
