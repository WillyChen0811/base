package projsync

// T9a 测试（续）：改名、写到一半、暂缓、中途不确定、单元级关卡、删空目录、同内容安装串行、base 缺失/损坏、输入检查。
// 夹具和约定见 pfround_test.go。

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── S13 改名与目录改名：目标边本机改名，不重传 ────────────

func TestPFRoundS13Rename(t *testing.T) {
	f := newPFRDFix(t)
	f.seedMac()
	f.mustDone(f.run())

	// Mac 改名一个文件、改名一个带子目录的目录
	pfrdMust(t, os.Rename(f.macAbs("说明.txt"), f.macAbs("说明v2.txt")))
	pfrdMust(t, os.Rename(f.macAbs("报告"), f.macAbs("报告新")))
	r := f.mustDone(f.run())
	if r.BytesUp != 0 || r.BytesDown != 0 {
		t.Fatalf("改名不该传字节: 上传 %d 下载 %d\n%s", r.BytesUp, r.BytesDown, pfrdDump(r))
	}
	renames := 0
	for _, d := range r.Done {
		if d.Kind == PFActRename && d.Side == PFSideUbuntu {
			renames++
		}
	}
	if renames != 3 { // 说明.txt、报告/初稿.docx、报告/附件/清单.csv
		t.Fatalf("应在 Ubuntu 本机改名 3 个文件，实际 %d 个:\n%s", renames, pfrdDump(r))
	}
	f.mustSame()
	if _, err := os.Lstat(f.ub.abs("报告")); err == nil {
		t.Fatal("Ubuntu 上旧目录应已删掉")
	}
	if f.ub.read("报告新/附件/清单.csv") != "ID,名称,数量\n1,螺栓,200\n2,垫片,50\n" {
		t.Fatal("改名后的内容不对")
	}
	// 改名走的是本机改名，不是删了重建，所以旧文件没有进回收
	if n := len(pfrdRecycled(t, PFRemoteRecycleDir(f.ub.meta))); n != 0 {
		t.Fatalf("改名不该产生回收文件，却有 %d 个", n)
	}
	f.settled()

	// 反过来：Ubuntu 改名，Mac 跟着改，不传
	pfrdMust(t, os.Rename(f.ub.abs("现场照片"), f.ub.abs("现场照片2")))
	r = f.mustDone(f.run())
	if r.BytesUp != 0 || r.BytesDown != 0 {
		t.Fatalf("改名不该传字节: 上传 %d 下载 %d", r.BytesUp, r.BytesDown)
	}
	f.mustSame()
	if !f.macHas("现场照片2/IMG_0002.png") || f.macHas("现场照片") {
		t.Fatal("Mac 应跟着改名")
	}
	f.settled()
}

// ── S16 写到一半：不传，下一轮再传 ───────────────────────

func TestPFRoundS16WriteInProgress(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("好的.txt", "好的\n", pfrtOld)
	f.macPut("写一半.docx", "只写了前半", f.ub.now.Add(-2*time.Second)) // 距「现在」不到 10 秒
	r := f.mustDone(f.run())
	if _, err := os.Lstat(f.ub.abs("写一半.docx")); err == nil {
		t.Fatal("还在写的文件不该传")
	}
	if f.ub.read("好的.txt") != "好的\n" {
		t.Fatal("别的文件照常传")
	}
	var found bool
	for _, s := range r.Skipped {
		if s.Path == "写一半.docx" && s.Code == PFCodeNotQuiet && s.Source == PFRoundSrcDecide {
			found = true
		}
	}
	if !found {
		t.Fatalf("应记下「等待静止」:\n%s", pfrdDump(r))
	}
	if !strings.Contains(strings.Join(r.Status.Notes, "\n"), "写一半.docx") {
		t.Fatalf("状态里要能看到等待静止的文件名: %v", r.Status.Notes)
	}
	if _, ok := f.loadBase().Get("写一半.docx"); ok {
		t.Fatal("没传的文件不该进 base")
	}
	// 还在写：下一轮仍然不传，也不出新动作
	r2 := f.settled()
	if r2.Aligned() {
		t.Fatal("有文件在等待静止，不算已对齐")
	}
	// 写完了（mtime 变成过去）：这一轮传
	pfrdMust(t, os.Chtimes(f.macAbs("写一半.docx"), pfrtOld, pfrtOld))
	f.mustDone(f.run())
	f.mustSame()
	f.settled()
}

// ── 暂缓 ────────────────────────────────────────────────

func (f *pfrdFix) bigFile(rel string, mb int, mt time.Time, onUbuntu bool) {
	content := strings.Repeat("abcdefghijklmnop", mb<<16)
	if onUbuntu {
		f.ubPut(rel, content, mt)
	} else {
		f.macPut(rel, content, mt)
	}
}

func TestPFRoundDeferHotspot(t *testing.T) {
	f := newPFRDFix(t)
	f.net = PFRemoteNetExpensive
	f.tr.ResetSpeed(PFSpeedDefaultHotspot)
	f.macPut("小.txt", "小文件\n", pfrtOld)
	f.bigFile("大视频.mp4", 10, pfrtOld, false)

	r := f.mustDone(f.run())
	if f.ub.read("小.txt") != "小文件\n" {
		t.Fatal("小文件在热点上也该传")
	}
	if _, err := os.Lstat(f.ub.abs("大视频.mp4")); err == nil {
		t.Fatal("热点上 10MB（预计 35 秒）该暂缓，不该传")
	}
	if len(r.Deferred) != 1 || r.Deferred[0].Path != "大视频.mp4" || r.Deferred[0].Dir != PFRoundToUbuntu ||
		r.Deferred[0].EstSeconds < 30 || r.Deferred[0].Size != 10<<20 || r.Deferred[0].Reason == "" {
		t.Fatalf("暂缓清单: %+v", r.Deferred)
	}
	if _, ok := f.loadBase().Get("大视频.mp4"); ok {
		t.Fatal("暂缓的文件 base 不该动")
	}
	if r.BytesUp > 1<<10 {
		t.Fatalf("暂缓期间不该传大文件，上传了 %d 字节", r.BytesUp)
	}
	// 状态文件（Ubuntu 上的）里 pending 有它
	st, err := PFRemoteReadStatus(f.ub.meta, pfrtCode)
	if err != nil || len(st.PendingToUbuntu) != 1 {
		t.Fatalf("状态文件的待传: %v %+v", err, st.PendingToUbuntu)
	}
	p := st.PendingToUbuntu[0]
	if p.Path != "大视频.mp4" || p.Reason != PFRemoteReasonDeferred || p.Size != 10<<20 || p.Sha == "" || p.MacMtimeNs != pfrtOld.UnixNano() {
		t.Fatalf("待传记录: %+v", p)
	}
	if r.Aligned() {
		t.Fatal("有暂缓，不算已对齐")
	}
	// 再跑一轮仍是热点：还是暂缓，不出别的动作，base 仍不动
	r2 := f.run()
	f.mustDone(r2)
	if len(r2.Deferred) != 1 || r2.BytesUp != 0 {
		t.Fatalf("还在热点上，应继续暂缓: %+v", r2.Deferred)
	}
	// 网好了：下一轮补上
	f.net = PFRemoteNetGood
	f.tr.ResetSpeed(PFSpeedDefaultGood)
	r3 := f.mustDone(f.run())
	if len(r3.Deferred) != 0 || r3.BytesUp < 10<<20 {
		t.Fatalf("网好后应补传: 暂缓 %d 上传 %d", len(r3.Deferred), r3.BytesUp)
	}
	f.mustSame()
	st, _ = PFRemoteReadStatus(f.ub.meta, pfrtCode)
	if len(st.PendingToUbuntu) != 0 {
		t.Fatalf("补传后待传应清空: %+v", st.PendingToUbuntu)
	}
	f.settled()
}

func TestPFRoundDeferDownloadAndUserLimit(t *testing.T) {
	f := newPFRDFix(t)
	f.net = PFRemoteNetUnknown
	f.tr.ResetSpeed(PFSpeedDefaultHotspot)
	f.bigFile("大图集.zip", 10, pfrtOld, true)
	f.ubPut("文字.txt", "文字\n", pfrtOld)

	// 普通优先级：暂缓；方向是 Ubuntu → Mac
	r := f.mustDone(f.run())
	if len(r.Deferred) != 1 || r.Deferred[0].Dir != PFRoundToMac || f.macHas("大图集.zip") || !f.macHas("文字.txt") {
		t.Fatalf("下载方向的暂缓:\n%s", pfrdDump(r))
	}
	st, _ := PFRemoteReadStatus(f.ub.meta, pfrtCode)
	if len(st.PendingToMac) != 1 || st.PendingToMac[0].Reason != PFRemoteReasonDeferred || st.PendingToMac[0].UbMtimeNs != pfrtOld.UnixNano() {
		t.Fatalf("pending_to_mac: %+v", st.PendingToMac)
	}
	// 用户指令要的（只点了这个文件）：热点上 60 秒内放行，预计 35 秒
	f.prio, f.paths = PFRoundPrioUser, []string{"大图集.zip"}
	r = f.mustDone(f.run())
	if len(r.Deferred) != 0 || !f.macHas("大图集.zip") || r.BytesDown < 10<<20 {
		t.Fatalf("用户指令要的该放行:\n%s", pfrdDump(r))
	}
	f.mustSame()
	f.settled()
}

func TestPFRoundPriorityPathsOnlyWanted(t *testing.T) {
	f := newPFRDFix(t)
	f.net = PFRemoteNetExpensive
	f.tr.ResetSpeed(PFSpeedDefaultHotspot)
	f.bigFile("要的.bin", 10, pfrtOld, false)
	f.bigFile("不要的.bin", 10, pfrtOld, false)
	f.prio, f.paths = PFRoundPrioUser, []string{"要的.bin"}
	r := f.mustDone(f.run())
	if _, err := os.Lstat(f.ub.abs("要的.bin")); err != nil {
		t.Fatalf("点了名的该传: %v", err)
	}
	if _, err := os.Lstat(f.ub.abs("不要的.bin")); err == nil || len(r.Deferred) != 1 || r.Deferred[0].Path != "不要的.bin" || r.Deferred[0].Wanted {
		t.Fatalf("没点名的仍按普通优先级暂缓:\n%s", pfrdDump(r))
	}
}

func TestPFRoundOffline(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("a.txt", "A\n", pfrtOld)
	f.net = PFRemoteNetOffline
	r := f.run()
	if r.Outcome != PFRoundSkipped || r.StatusWritten || len(r.Done) != 0 {
		t.Fatalf("断网应整轮跳过:\n%s", pfrdDump(r))
	}
	if _, err := os.Lstat(f.ub.abs("")); err == nil {
		t.Fatal("断网不该碰 Ubuntu")
	}
	if len(f.ub.run.cmds) != 0 {
		t.Fatalf("断网不该调用 Ubuntu: %v", f.ub.run.cmds)
	}
}

// ── 中途不确定 ──────────────────────────────────────────

// pfrdRunner 包一层 RemoteRunner：对含 install_new 的 apply 调用可以在执行前或执行后让连接断掉。
type pfrdRunner struct {
	inner  RemoteRunner
	before bool // 执行前断：Ubuntu 没收到
	after  bool // 执行后断：Ubuntu 已经做了，Mac 不知道
	hits   int
}

func (r *pfrdRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	hit := strings.HasSuffix(cmd, "projfiles-apply") && bytes.Contains(stdin, []byte(`"install_new"`)) && !bytes.Contains(stdin, []byte(`"verify_part"`))
	if hit && r.before {
		r.hits++
		return nil, nil, -1, errors.New("连接断了")
	}
	out, errb, code, err := r.inner.Run(ctx, cmd, stdin)
	if hit && r.after {
		r.hits++
		return nil, nil, -1, errors.New("连接断了（执行之后）")
	}
	return out, errb, code, err
}

func TestPFRoundUncertainUbuntuComm(t *testing.T) {
	for _, mode := range []string{"执行前断", "执行后断"} {
		t.Run(mode, func(t *testing.T) {
			f := newPFRDFix(t)
			f.macPut("x.txt", "x\n", pfrtOld)
			f.mustDone(f.run())
			f.settled()

			// a_ub.txt 要下载到 Mac（路径靠前，先做）；z_mac.txt 要上传（路径靠后，在 Ubuntu 批里）
			f.ubPut("a_ub.txt", "Ubuntu 新增\n", pfrtOld)
			f.macPut("z_mac.txt", "Mac 新增\n", pfrtOld)
			rn := &pfrdRunner{inner: f.ub.run, before: mode == "执行前断", after: mode == "执行后断"}
			f.ub.cl.Runner = rn
			r := f.run()
			if r.Outcome != PFRoundUncertain || !r.Uncertain || rn.hits != 1 {
				t.Fatalf("应是「结果不确定」:\n%s hits=%d", pfrdDump(r), rn.hits)
			}
			if r.StatusWritten {
				t.Fatal("Ubuntu 通信不确定时不再往 Ubuntu 写状态文件")
			}
			// 已经做完的（下载到 Mac 的）进了 base；没确认的没进
			b := f.loadBase()
			if _, ok := b.Get("a_ub.txt"); !ok || f.macRead("a_ub.txt") != "Ubuntu 新增\n" {
				t.Fatal("已经做完的 a_ub.txt 应在 Mac 上，并写进了 base")
			}
			if _, ok := b.Get("z_mac.txt"); ok {
				t.Fatal("通信不确定的 z_mac.txt 不能写进 base")
			}
			_, ubErr := os.Lstat(f.ub.abs("z_mac.txt"))
			if (mode == "执行前断") != (ubErr != nil) {
				t.Fatalf("%s：Ubuntu 上 z_mac.txt 的情况不对: %v", mode, ubErr)
			}
			// 恢复连接：下一轮重新采集，正确收尾
			f.ub.cl.Runner = f.ub.run
			r2 := f.mustDone(f.run())
			if mode == "执行后断" && r2.BytesUp != 0 {
				t.Fatalf("Ubuntu 上已经有了，不该再传: %d", r2.BytesUp)
			}
			f.mustSame()
			f.settled()
		})
	}
}

func TestPFRoundUncertainOnFactsFailure(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("a.txt", "A\n", pfrtOld)
	f.ub.run.fail = errors.New("连不上")
	r := f.run()
	if r.Outcome != PFRoundSkipped || !r.Uncertain || len(r.Done) != 0 || r.StatusWritten {
		t.Fatalf("采集不到 Ubuntu 的事实，本轮应跳过:\n%s", pfrdDump(r))
	}
	if _, err := PFBaseLoad(f.meta, pfrtCode); err != nil {
		t.Fatal(err)
	}
	f.ub.run.fail = nil
	f.mustDone(f.run())
	f.mustSame()
	f.settled()
}

// 写原语「放弃且不确定」（这里用钩子模拟进程在安装中间死掉）：立刻停止后续写入，下一轮正确收尾。
func TestPFRoundUncertainFromPrimitive(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("x.txt", "x\n", pfrtOld)
	f.mustDone(f.run())
	f.ubPut("c1.txt", "一\n", pfrtOld)
	f.ubPut("c2.txt", "二\n", pfrtOld)
	f.ubPut("c3.txt", "三\n", pfrtOld)
	in := f.input()
	in.ApplyHook = func(w string) error {
		if w == "new:after-rename" {
			return errors.New("进程死了")
		}
		return nil
	}
	r, err := PFRoundRunUnit(context.Background(), in)
	pfrdMust(t, err)
	if r.Outcome != PFRoundUncertain {
		t.Fatalf("应是不确定:\n%s", pfrdDump(r))
	}
	// 第一个文件的安装停在改名之后：文件在 Mac 上了，但没写进 base；后面的没有做
	if !f.macHas("c1.txt") || f.macHas("c2.txt") || f.macHas("c3.txt") {
		t.Fatalf("只有第一个该装上: c1=%v c2=%v c3=%v", f.macHas("c1.txt"), f.macHas("c2.txt"), f.macHas("c3.txt"))
	}
	b := f.loadBase()
	if _, ok := b.Get("c1.txt"); ok {
		t.Fatal("不确定的那一步不能写进 base")
	}
	stopped := 0
	for _, s := range r.Skipped {
		if s.Source == PFRoundSrcStopped {
			stopped++
		}
	}
	if stopped != 2 {
		t.Fatalf("后面两个应记成「已停止没做」，实际 %d 个:\n%s", stopped, pfrdDump(r))
	}
	// 下一轮：c1 两边一样，只补 base；c2、c3 正常下载
	r2 := f.mustDone(f.run())
	f.mustSame()
	// c2、c3 在上一轮已经提前传进了暂存区（核验过的 part 保留），这一轮复用，最多再传它们两个
	if r2.BytesDown > int64(len("二\n")+len("三\n")) {
		t.Fatalf("下一轮最多只该传 c2、c3: 下载 %d", r2.BytesDown)
	}
	f.settled()
}

// ── G1 / G2 单元级关卡 ─────────────────────────────────

func TestPFRoundG1UnitMissing(t *testing.T) {
	for _, side := range []string{"Ubuntu", "Mac"} {
		t.Run(side+"的单元目录不见了", func(t *testing.T) {
			f := newPFRDFix(t)
			f.seedMac()
			f.mustDone(f.run())
			before := f.loadBase()
			nBase, genBase, metaBase := before.Len(), before.Gen(), before.Meta()
			macTree, ubTree := pfrdTree(t, f.root), pfrdTree(t, f.ub.abs(""))
			if side == "Ubuntu" {
				pfrdMust(t, os.RemoveAll(f.ub.abs("")))
			} else {
				pfrdMust(t, os.RemoveAll(f.root))
			}
			r := f.run()
			if r.Outcome != PFRoundStopped || len(r.Gates) == 0 || r.Gates[0].Code != "G1" || len(r.Done) != 0 || r.PlannedActions != 0 {
				t.Fatalf("应整单元停下:\n%s", pfrdDump(r))
			}
			if !r.StatusWritten || !strings.Contains(r.Status.Note, "G1") {
				t.Fatalf("状态里应说明停下的原因: written=%v note=%q", r.StatusWritten, r.Status.Note)
			}
			// 不删另一边的任何东西，也不把不见的一边重新建出来
			if side == "Ubuntu" {
				if _, err := os.Lstat(f.ub.abs("")); err == nil {
					t.Fatal("停下后 Ubuntu 的单元目录不该被重建")
				}
				if got := pfrdTree(t, f.root); len(got) != len(macTree) {
					t.Fatalf("Mac 的东西被动了: %d → %d", len(macTree), len(got))
				}
			} else {
				if _, err := os.Lstat(f.root); err == nil {
					t.Fatal("停下后 Mac 的单元目录不该被重建")
				}
				if got := pfrdTree(t, f.ub.abs("")); len(got) != len(ubTree) {
					t.Fatalf("Ubuntu 的东西被动了: %d → %d", len(ubTree), len(got))
				}
			}
			// base 原样（连元数据一起）
			after := f.loadBase()
			if after.Len() != nBase || after.Gen() != genBase || after.Meta() != metaBase {
				t.Fatalf("停下的轮次不该动 base: %d/%d gen %d/%d", after.Len(), nBase, after.Gen(), genBase)
			}
			// 再跑一轮，依然停着
			if r2 := f.run(); r2.Outcome != PFRoundStopped {
				t.Fatalf("问题没消除，应继续停着: %s", r2.Outcome)
			}
		})
	}
}

// G2：单元目录被整个换过（ino 变了）→ 本轮按首次规则，绝不推断删除。
func TestPFRoundG2DirReplaced(t *testing.T) {
	f := newPFRDFix(t)
	f.seedMac()
	f.mustDone(f.run())
	// Ubuntu 上删一个文件；同时 Mac 的单元目录被换成一份新拷贝（新 inode）
	pfrdMust(t, os.Remove(f.ub.abs("说明.txt")))
	old := f.root + ".old"
	pfrdMust(t, os.Rename(f.root, old))
	pfrdCopyFixtureAll(t, old, f.root)
	pfrdMust(t, os.RemoveAll(old))
	r := f.mustDone(f.run())
	if !r.ResetBase || !r.FirstTime {
		t.Fatalf("应按首次规则并清空旧 base:\n%s", pfrdDump(r))
	}
	if !f.macHas("说明.txt") || f.ub.read("说明.txt") != "项目说明\n" {
		t.Fatal("首次规则不推断删除：Ubuntu 上缺的文件该从 Mac 补回去，Mac 的不该删")
	}
	f.mustSame()
	f.settled()
}

// pfrdCopyFixtureAll 拷整个目录树（含 mtime），目录用新建的（新 inode）。
func pfrdCopyFixtureAll(t *testing.T, src, dst string) {
	t.Helper()
	pfrdMust(t, filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return err
		}
		return os.Chtimes(target, fi.ModTime(), fi.ModTime())
	}))
}

// ── 删空目录：只剩系统垃圾文件 ─────────────────────────

func TestPFRoundRmdirJunk(t *testing.T) {
	t.Run("Ubuntu 侧目录里只剩垃圾", func(t *testing.T) {
		f := newPFRDFix(t)
		f.seedMac()
		f.mustDone(f.run())
		f.ubPut("a/b/c/.DS_Store", "finder", pfrtOld)
		f.ubPut("a/b/c/Thumbs.db", "win", pfrtOld)
		f.ubPut("a/b/c/._深层.txt", "appledouble", pfrtOld)
		pfrdMust(t, os.RemoveAll(f.macAbs("a/b/c")))

		r := f.mustDone(f.run())
		f.mustSame()
		if _, err := os.Lstat(f.ub.abs("a/b/c")); err == nil {
			t.Fatal("垃圾文件清走后，空目录应被删掉")
		}
		files := pfrdRecycled(t, PFRemoteRecycleDir(f.ub.meta))
		for _, n := range []string{".DS_Store", "Thumbs.db", "._深层.txt", "深层.txt"} {
			if !pfrdHasRecycled(files, n) {
				t.Fatalf("回收里应有 %s: %v", n, files)
			}
		}
		es := pfrdIndex(t, PFRemoteRecycleDir(f.ub.meta))
		if len(es) != 4 {
			t.Fatalf("回收记录应有 4 条: %+v", es)
		}
		for _, e := range es {
			if e.Reason != string(PFReasonDelete) || e.SHA256 == "" || !strings.HasPrefix(e.Path, "a/b/c/") {
				t.Fatalf("回收记录: %+v", e)
			}
		}
		if r.Deletes.Ubuntu != 1 {
			t.Fatalf("删除数只算同步的文件: %+v", r.Deletes)
		}
		if _, ok := f.loadBase().Get("a/b/c"); ok {
			t.Fatal("base 里该删掉 a/b/c")
		}
		f.settled()
	})
	t.Run("Mac 侧目录里只剩垃圾", func(t *testing.T) {
		f := newPFRDFix(t)
		f.seedMac()
		f.mustDone(f.run())
		f.macPut("a/b/c/.DS_Store", "finder", pfrtOld)
		f.macPut("a/b/c/Icon\r", "icon", pfrtOld)
		pfrdMust(t, os.RemoveAll(f.ub.abs("a/b/c")))

		f.mustDone(f.run())
		f.mustSame()
		if f.macHas("a/b/c") {
			t.Fatal("垃圾文件清走后，空目录应被删掉")
		}
		files := pfrdRecycled(t, PFRoundMacRecycleDir(f.meta))
		for _, n := range []string{".DS_Store", "Icon\r", "深层.txt"} {
			if !pfrdHasRecycled(files, n) {
				t.Fatalf("回收里应有 %q: %v", n, files)
			}
		}
		f.settled()
	})
	t.Run("还有别的东西就保留并报告", func(t *testing.T) {
		for _, side := range []string{"Ubuntu", "Mac"} {
			f := newPFRDFix(t)
			f.seedMac()
			f.mustDone(f.run())
			if side == "Ubuntu" {
				f.ubPut("a/b/c/.hidden", "别人的隐藏文件", pfrtOld)
				f.ubPut("a/b/c/.DS_Store", "finder", pfrtOld)
				pfrdMust(t, os.RemoveAll(f.macAbs("a/b/c")))
			} else {
				f.macPut("a/b/c/.hidden", "别人的隐藏文件", pfrtOld)
				f.macPut("a/b/c/.DS_Store", "finder", pfrtOld)
				pfrdMust(t, os.RemoveAll(f.ub.abs("a/b/c")))
			}
			r := f.mustDone(f.run())
			kept := false
			for _, s := range r.Skipped {
				if s.Path == "a/b/c" && s.Code == PFSkipDirKept {
					kept = true
				}
			}
			if !kept {
				t.Fatalf("%s：应报「目录保留」:\n%s", side, pfrdDump(r))
			}
			var hidden string
			if side == "Ubuntu" {
				hidden = f.ub.abs("a/b/c/.hidden")
			} else {
				hidden = f.macAbs("a/b/c/.hidden")
			}
			if b, err := os.ReadFile(hidden); err != nil || string(b) != "别人的隐藏文件" {
				t.Fatalf("%s：别的东西一个也不能动: %v", side, err)
			}
			// 垃圾文件也没动（有别的东西时一个都不清）
			if side == "Ubuntu" {
				if _, err := os.Lstat(f.ub.abs("a/b/c/.DS_Store")); err != nil {
					t.Fatal(".DS_Store 不该被清")
				}
			}
			// 稳定：再跑一轮仍然保留，不出错
			if r2 := f.mustDone(f.run()); r2.Deletes.Mac+r2.Deletes.Ubuntu != 0 {
				t.Fatalf("%s：第二轮不该删东西", side)
			}
		}
	})
}

// ── 同一内容的 part，安装串行 ──────────────────────────

func TestPFRoundSameSHASerial(t *testing.T) {
	t.Run("上传三份同内容", func(t *testing.T) {
		f := newPFRDFix(t)
		for _, n := range []string{"同1.txt", "同2.txt", "同3.txt"} {
			f.macPut(n, "完全相同的内容\n", pfrtOld)
		}
		r := f.mustDone(f.run())
		f.mustSame()
		for _, s := range r.Skipped {
			t.Fatalf("不该有跳过（part 被前一个安装消费后要重新要一份）: %+v", s)
		}
		f.settled()
	})
	t.Run("下载三份同内容", func(t *testing.T) {
		f := newPFRDFix(t)
		for _, n := range []string{"同1.txt", "同2.txt", "同3.txt"} {
			f.ubPut(n, "完全相同的内容\n", pfrtOld)
		}
		f.mustDone(f.run())
		f.mustSame()
		f.settled()
	})
}

// ── base 缺失、损坏 ─────────────────────────────────────

func TestPFRoundBaseMissingOrCorrupt(t *testing.T) {
	for _, mode := range []string{"缺失", "损坏"} {
		t.Run(mode, func(t *testing.T) {
			f := newPFRDFix(t)
			f.seedMac()
			f.mustDone(f.run())
			snap := filepath.Join(f.meta, "base", pfrtCode+".json")
			if mode == "缺失" {
				pfrdMust(t, os.Remove(snap))
				_ = os.Remove(filepath.Join(f.meta, "base", pfrtCode+".journal"))
			} else {
				pfrdMust(t, os.WriteFile(snap, []byte("{坏了"), 0o600))
			}
			// 两边其实一致；再让 Ubuntu 删一个文件：没有 base 时绝不能推断成删除
			pfrdMust(t, os.Remove(f.ub.abs("说明.txt")))
			r := f.mustDone(f.run())
			if !r.FirstTime {
				t.Fatalf("应按首次规则:\n%s", pfrdDump(r))
			}
			if !f.macHas("说明.txt") || f.ub.read("说明.txt") != "项目说明\n" {
				t.Fatal("没有可信的 base，不能把「Ubuntu 上没有」当成删除")
			}
			if r.BytesUp != int64(len("项目说明\n")) || r.BytesDown != 0 {
				t.Fatalf("只该补传那一个文件: 上传 %d 下载 %d", r.BytesUp, r.BytesDown)
			}
			f.mustSame()
			if mode == "损坏" {
				ents, _ := os.ReadDir(filepath.Join(f.meta, "base", "坏"))
				if len(ents) == 0 || !strings.Contains(strings.Join(r.Notices, "\n"), "base 损坏") {
					t.Fatalf("损坏的原文件应移进 坏/ 并报告: %d 个 %v", len(ents), r.Notices)
				}
			}
			r2 := f.settled()
			if r2.FirstTime {
				t.Fatal("base 重建之后不该再按首次规则")
			}
		})
	}
}

func TestPFRoundHashCacheCorrupt(t *testing.T) {
	f := newPFRDFix(t)
	f.seedMac()
	f.mustDone(f.run())
	pfrdMust(t, os.WriteFile(filepath.Join(f.meta, PFRemoteCacheName, pfrtCode+".json"), []byte("不是 JSON"), 0o600))
	r := f.settled()
	if !strings.Contains(strings.Join(r.Notices, "\n"), "哈希缓存损坏") {
		t.Fatalf("应报哈希缓存损坏、已重算: %v", r.Notices)
	}
	// 重算之后写回了好的缓存
	raw, err := os.ReadFile(filepath.Join(f.meta, PFRemoteCacheName, pfrtCode+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := PFParseHashCache(raw); err != nil || c.Len() != 9 {
		t.Fatalf("写回的缓存: %v", err)
	}
}

// ── 台账、暂存区残留、输入检查 ─────────────────────────

func TestPFRoundLedgerSkipped(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("a.txt", "A\n", pfrtOld)
	f.macPut("历史版本/档案台账.sqlite3", "sqlite 内容", pfrtOld)
	f.macPut("历史版本/档案索引.json", `{"versions":[]}`, pfrtOld)
	r := f.mustDone(f.run())
	todo := 0
	for _, s := range r.Skipped {
		if s.Source == PFRoundSrcTodo && strings.Contains(s.Reason, "T10") {
			todo++
		}
	}
	if todo != 1 {
		t.Fatalf("台账动作应记成跳过「台账同步待做（T10）」:\n%s", pfrdDump(r))
	}
	if _, err := os.Lstat(f.ub.abs("历史版本/档案台账.sqlite3")); err == nil {
		t.Fatal("本任务不同步台账")
	}
	if f.ub.read("a.txt") != "A\n" {
		t.Fatal("别的文件照常同步")
	}
}

func TestPFRoundStagingLeftoversOnlyReported(t *testing.T) {
	f := newPFRDFix(t)
	f.macPut("a.txt", "A\n", pfrtOld)
	stg := PFRoundMacStagingDir(f.meta)
	pfrdMust(t, os.MkdirAll(stg, 0o700))
	swap := filepath.Join(stg, ".pfswap-abcd1234-ffff")
	pfrdMust(t, os.WriteFile(swap, []byte("用户内容"), 0o600))
	in := f.input()
	in.UbuntuLeftovers = []PFRemoteLeftover{{Path: ".pfswap-deadbeef-0001", Kind: "swap", Size: 9}}
	r, err := PFRoundRunUnit(context.Background(), in)
	pfrdMust(t, err)
	f.mustDone(r)
	if b, err := os.ReadFile(swap); err != nil || string(b) != "用户内容" {
		t.Fatalf("swap 残留绝不能删: %v", err)
	}
	if len(r.Leftovers) != 2 || !strings.Contains(strings.Join(r.Notices, "\n"), "交换文件") {
		t.Fatalf("应报告两边的残留: %+v", r.Leftovers)
	}
}

func TestPFRoundInputChecks(t *testing.T) {
	f := newPFRDFix(t)
	ctx := context.Background()
	in := f.input()
	in.Unit.State = PFUnitStopped
	if _, err := PFRoundRunUnit(ctx, in); err == nil {
		t.Fatal("已停止同步的单元不该跑")
	}
	in = f.input()
	in.Staging = filepath.Join(f.base, "别的暂存区")
	if _, err := PFRoundRunUnit(ctx, in); err == nil || !strings.Contains(err.Error(), "暂存区") {
		t.Fatalf("传输器的暂存区和 Mac 暂存区不一致应报错: %v", err)
	}
	in = f.input()
	in.Remote = nil
	if _, err := PFRoundRunUnit(ctx, in); err == nil {
		t.Fatal("缺客户端应报错")
	}
	if len(f.ub.run.cmds) != 0 {
		t.Fatal("输入不合格时不该调用 Ubuntu")
	}
}

func pfrdMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}


// Ubuntu 上新建的项目：Mac 这边连单元目录（和年份目录）都还没有，按首次规则建出来并全部取回。
func TestPFRoundUbuntuNewProject(t *testing.T) {
	f := newPFRDFix(t)
	f.ubPut("报告/甲.docx", "甲\n", pfrtOld)
	f.ubPut("乙.txt", "乙\n", pfrtOld)
	pfrdMust(t, os.MkdirAll(f.ub.abs("空目录"), 0o755))
	if _, err := os.Lstat(f.root); err == nil {
		t.Fatal("前提：Mac 上还没有这个单元目录")
	}
	r := f.mustDone(f.run())
	if !r.FirstTime || r.BytesDown == 0 || r.BytesUp != 0 {
		t.Fatalf("应全部取回:\n%s", pfrdDump(r))
	}
	f.mustSame()
	if fi, err := os.Stat(f.macAbs("空目录")); err != nil || !fi.IsDir() {
		t.Fatalf("空目录也该建出来: %v", err)
	}
	if m := f.loadBase().Meta(); m.MacDirID.Ino == 0 || m.UbuntuDirID.Ino == 0 {
		t.Fatalf("两边单元目录的 ino 都该记下，否则下一轮会误触发 G2: %+v", m)
	}
	if r2 := f.settled(); r2.FirstTime {
		t.Fatal("第二轮不该再按首次规则")
	}
}
