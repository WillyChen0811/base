package projsync

// T8 apply 测试：各步骤、整批拒绝、中途停止、unit_mkdir、verify_part、状态/请求文件、清理、扩展点。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (f *pfrtFix) apply(steps ...PFRemoteStep) (*PFRemoteApplyResult, error) {
	f.t.Helper()
	return f.cl.Apply(context.Background(), PFRemoteApplyOptions{Unit: f.unit(), Round: "r-001"}, steps)
}

func (f *pfrtFix) mustApply(steps ...PFRemoteStep) *PFRemoteApplyResult {
	f.t.Helper()
	res, err := f.apply(steps...)
	if err != nil {
		f.t.Fatalf("apply 失败: %v", err)
	}
	return res
}

func pfrtDone(t *testing.T, r PFRemoteStepResult) {
	t.Helper()
	if r.Error != "" || r.Apply == nil || r.Apply.Status != PFApplyDone {
		t.Fatalf("应完成: %+v apply=%v", r, r.Apply)
	}
}

var pfrtMt = pfrtOld.UnixNano()

func TestPFRemoteApplyMkdirAndInstallNew(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	sha, size := f.stage("新内容")
	res := f.mustApply(
		PFRemoteStep{Op: PFRemoteOpMkdir, Path: "新目录/子目录"},
		PFRemoteStep{Op: PFRemoteOpInstallNew, Path: "新目录/子目录/n.txt", SHA256: sha, Size: size, MtimeNs: pfrtMt},
	)
	if res.Stopped || res.NotRun != 0 || len(res.Results) != 2 {
		t.Fatalf("%+v", res)
	}
	pfrtDone(t, res.Results[0])
	pfrtDone(t, res.Results[1])
	inst := res.Results[1].Apply.Installed
	if inst == nil || inst.SHA256 != sha || inst.Size != size {
		t.Fatalf("安装结果: %+v", inst)
	}
	if f.read("新目录/子目录/n.txt") != "新内容" {
		t.Fatal("内容不对")
	}
	// 再装一次：目标已存在 → 放弃，不覆盖，后面的步骤照常
	sha2, size2 := f.stage("别的")
	res = f.mustApply(
		PFRemoteStep{Op: PFRemoteOpInstallNew, Path: "新目录/子目录/n.txt", SHA256: sha2, Size: size2, MtimeNs: pfrtMt},
		PFRemoteStep{Op: PFRemoteOpMkdir, Path: "另一个"},
	)
	if res.Stopped || res.Results[0].Apply.Status != PFApplyAbandoned || res.Results[0].Apply.Code != PFApplyCodeTargetExists {
		t.Fatalf("目标已存在应放弃: %+v", res.Results[0])
	}
	pfrtDone(t, res.Results[1])
	if f.read("新目录/子目录/n.txt") != "新内容" {
		t.Fatal("放弃的安装不该改目标")
	}
}

func TestPFRemoteApplyInstallOverAndDelete(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "旧内容")
	f.put("b.txt", "要删的")
	sha, size := f.stage("新内容")
	expA, expB := f.expect("a.txt"), f.expect("b.txt")
	res := f.mustApply(
		PFRemoteStep{Op: PFRemoteOpInstallOver, Path: "a.txt", SHA256: sha, Size: size, MtimeNs: pfrtMt, Expect: &expA},
		PFRemoteStep{Op: PFRemoteOpDelete, Path: "b.txt", Expect: &expB, Reason: string(PFReasonDelete)},
	)
	pfrtDone(t, res.Results[0])
	pfrtDone(t, res.Results[1])
	if f.read("a.txt") != "新内容" || f.exists("b.txt") {
		t.Fatal("覆盖或删除没生效")
	}
	// 被换下来的和被删的都在 Ubuntu 的同步回收里，路径在 meta 下
	rec, err := NewPFRecycle(PFRemoteRecycleDir(f.meta), PFRecycleOptions{})
	pfrtNoErr(t, err)
	over, del := res.Results[0].Apply.Recycled, res.Results[1].Apply.Recycled
	if over == nil || del == nil || over.Reason != string(PFReasonOverwrite) || del.Reason != string(PFReasonDelete) {
		t.Fatalf("回收记录: %+v %+v", over, del)
	}
	for e, want := range map[*PFRecycleEntry]string{over: "旧内容", del: "要删的"} {
		b, err := os.ReadFile(rec.Path(*e))
		if err != nil || string(b) != want {
			t.Fatalf("回收里 %s 的内容: %q %v", e.Path, b, err)
		}
		if e.Round != "r-001" || e.Unit != pfrtRel {
			t.Fatalf("回收记录的轮次或单元: %+v", e)
		}
	}
	day := f.now.Format("2006-01-02")
	if idx, err := rec.ReadIndex(day); err != nil || len(idx) != 2 {
		t.Fatalf("回收 index: %v %v", idx, err)
	}
	// 前提不符（文件被人改了）→ 放弃，不动
	f.put("a.txt", "又被改了")
	sha3, size3 := f.stage("第三版")
	res = f.mustApply(PFRemoteStep{Op: PFRemoteOpInstallOver, Path: "a.txt", SHA256: sha3, Size: size3, MtimeNs: pfrtMt, Expect: &expA})
	if res.Results[0].Apply.Status != PFApplyAbandoned || f.read("a.txt") != "又被改了" {
		t.Fatalf("前提不符应放弃: %+v", res.Results[0].Apply)
	}
}

func TestPFRemoteApplyRenameConflictCopy(t *testing.T) {
	f := newPFRTFix(t)
	f.put("x/old.txt", "旧名")
	f.put("r.docx", "冲突原名")
	f.put("src.txt", "来源")
	expOld, expR := f.expect("x/old.txt"), f.expect("r.docx")
	srcSHA, srcSize := pfrtSHA("来源"), int64(len("来源"))
	conflict := "r（冲突-Mac-1027-1200）.docx"
	res := f.mustApply(
		PFRemoteStep{Op: PFRemoteOpRename, Path: "x/old.txt", To: "y/new.txt", Expect: &expOld},
		PFRemoteStep{Op: PFRemoteOpConflictName, Path: "r.docx", To: conflict, Expect: &expR},
		PFRemoteStep{Op: PFRemoteOpCopyNew, Path: "copy.txt", MtimeNs: pfrtMt, Src: &PFRemoteCopySrc{Where: PFRemoteSrcUnit, Path: "src.txt", SHA256: srcSHA, Size: srcSize}},
	)
	for _, r := range res.Results {
		pfrtDone(t, r)
	}
	if f.exists("x/old.txt") || f.read("y/new.txt") != "旧名" || f.exists("r.docx") || f.read(conflict) != "冲突原名" || f.read("copy.txt") != "来源" || f.read("src.txt") != "来源" {
		t.Fatal("改名、冲突改名或复制没生效")
	}
	// 从暂存区复制来覆盖
	sha, size := f.stage("暂存的新版")
	exp := f.expect("copy.txt")
	res = f.mustApply(PFRemoteStep{Op: PFRemoteOpCopyOver, Path: "copy.txt", MtimeNs: pfrtMt, Expect: &exp,
		Src: &PFRemoteCopySrc{Where: PFRemoteSrcStaging, Path: sha + ".part", SHA256: sha, Size: size}})
	pfrtDone(t, res.Results[0])
	if f.read("copy.txt") != "暂存的新版" {
		t.Fatal("copy_over 没生效")
	}
	// 删除后从同步回收里复制回来（恢复）
	expC := f.expect("copy.txt")
	res = f.mustApply(PFRemoteStep{Op: PFRemoteOpDelete, Path: "copy.txt", Expect: &expC, Reason: string(PFReasonConflict)})
	pfrtDone(t, res.Results[0])
	rec := res.Results[0].Apply.Recycled
	res = f.mustApply(PFRemoteStep{Op: PFRemoteOpCopyNew, Path: "恢复.txt", MtimeNs: pfrtMt,
		Src: &PFRemoteCopySrc{Where: PFRemoteSrcRecycle, Path: rec.Stored, SHA256: rec.SHA256, Size: rec.Size}})
	pfrtDone(t, res.Results[0])
	if f.read("恢复.txt") != "暂存的新版" {
		t.Fatal("从回收复制回来失败")
	}
	// rmdir：空目录删掉，不空的保留（放弃，not_empty）
	pfrtNoErr(t, os.MkdirAll(f.abs("空目录"), 0o755))
	res = f.mustApply(PFRemoteStep{Op: PFRemoteOpRmdir, Path: "空目录"}, PFRemoteStep{Op: PFRemoteOpRmdir, Path: "y"})
	pfrtDone(t, res.Results[0])
	if res.Results[1].Apply.Status != PFApplyAbandoned || res.Results[1].Apply.Code != PFApplyCodeNotEmpty || res.Stopped {
		t.Fatalf("不空的目录应放弃且不停: %+v", res.Results[1].Apply)
	}
}

// ── unit_mkdir ──────────────────────────────────────────

func TestPFRemoteApplyUnitMkdir(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	newU := PFRemoteUnit{Code: "260403J1002", Rel: "2026/260403J1002-新项目"}
	sha, size := f.stage("首次内容")
	res, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: newU, Round: "r-1"}, []PFRemoteStep{
		{Op: PFRemoteOpUnitMkdir},
		{Op: PFRemoteOpMkdir, Path: "资料"},
		{Op: PFRemoteOpInstallNew, Path: "资料/n.txt", SHA256: sha, Size: size, MtimeNs: pfrtMt},
	})
	pfrtNoErr(t, err)
	for _, r := range res.Results {
		pfrtDone(t, r)
	}
	if res.Results[0].Apply.Existed {
		t.Fatal("新建的单元目录不该标 Existed")
	}
	if b, err := os.ReadFile(filepath.Join(f.root, "2026/260403J1002-新项目/资料/n.txt")); err != nil || string(b) != "首次内容" {
		t.Fatalf("新单元里的文件: %q %v", b, err)
	}
	// poll 看得到这个新单元
	pr, err := f.cl.Poll(ctx, PFRemotePollOptions{})
	pfrtNoErr(t, err)
	if len(pr.Units) != 2 || pr.Units[1].Code != "260403J1002" {
		t.Fatalf("poll 单元: %+v", pr.Units)
	}
	// 幂等：单元已存在，unit_mkdir 算完成（Existed）
	res, err = f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: newU}, []PFRemoteStep{{Op: PFRemoteOpUnitMkdir}})
	pfrtNoErr(t, err)
	pfrtDone(t, res.Results[0])
	if !res.Results[0].Apply.Existed {
		t.Fatal("已存在应标 Existed")
	}
}

func TestPFRemoteApplyUnitMkdirRefusals(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	outside := filepath.Join(f.base, "外面")
	pfrtNoErr(t, os.MkdirAll(outside, 0o755))
	pfrtNoErr(t, os.Symlink(outside, filepath.Join(f.root, "link2026")))
	pfrtNoErr(t, os.MkdirAll(filepath.Join(f.root, "Archive"), 0o755))
	cases := []struct {
		name string
		unit PFRemoteUnit
		want string
	}{
		{"编号对不上", PFRemoteUnit{Code: "260403J1003", Rel: "2026/260403J1004-x"}, "与 260403J1003 不符"},
		{"别处已有同编号", PFRemoteUnit{Code: pfrtCode, Rel: "2025/260403J1001-又一个"}, "已有目录"},
		{"建在另一个单元里面", PFRemoteUnit{Code: "260403J1006", Rel: "2026/260403J1005-外层/260403J1006-内层"}, "不能建在另一个单元里面"},
		{"以点开头", PFRemoteUnit{Code: "260403J1007", Rel: ".hidden/260403J1007-x"}, "以 . 开头"},
		{"经过符号链接", PFRemoteUnit{Code: "260403J1008", Rel: "link2026/260403J1008-x"}, "不是真实目录"},
		{"只差大小写", PFRemoteUnit{Code: "260403J1009", Rel: "archive/260403J1009-x"}, "只差大小写"},
		{"层数太多", PFRemoteUnit{Code: "260403J1010", Rel: "a/b/c/d/e/260403J1010-x"}, "层数"},
	}
	for _, c := range cases {
		before := listDir(t, f.root)
		res, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: c.unit}, []PFRemoteStep{{Op: PFRemoteOpUnitMkdir}, {Op: PFRemoteOpMkdir, Path: "子"}})
		pfrtNoErr(t, err)
		r := res.Results[0]
		if !res.Stopped || res.NotRun != 1 || !r.Config || !strings.Contains(r.Error, c.want) {
			t.Errorf("%s: 应以配置错误停下（含 %q）: %+v stopped=%v", c.name, c.want, r, res.Stopped)
		}
		if after := listDir(t, f.root); after != before {
			t.Errorf("%s: 被拒绝却改了根目录:\n前 %s\n后 %s", c.name, before, after)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("符号链接指向的目录里被建了东西")
	}
}

func listDir(t *testing.T, root string) string {
	t.Helper()
	var out []string
	pfrtNoErr(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	}))
	return strings.Join(out, "|")
}

// ── 整批拒绝 ────────────────────────────────────────────

func TestPFRemoteApplyRefusedWholeBatch(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	mk := func(extra ...PFRemoteStep) []PFRemoteStep {
		return append([]PFRemoteStep{{Op: PFRemoteOpMkdir, Path: "不该被建"}}, extra...)
	}
	req := func(steps []PFRemoteStep) PFRemoteApplyRequest {
		return PFRemoteApplyRequest{Version: 1, Meta: f.meta, Root: f.root, CoordID: "mac-A", Code: pfrtCode, Rel: pfrtRel, Steps: steps}
	}
	sha := pfrtSHA("x")
	goodExp := &PFExpect{Size: 1, MtimeNs: 1, SHA256: sha}
	bad := map[string][]PFRemoteStep{
		"未知种类":             mk(PFRemoteStep{Op: "format_disk"}),
		"多余字段":             mk(PFRemoteStep{Op: PFRemoteOpMkdir, Path: "x", SHA256: sha}),
		"缺字段":              mk(PFRemoteStep{Op: PFRemoteOpInstallNew, Path: "x", SHA256: sha}),
		"路径含 ..":           mk(PFRemoteStep{Op: PFRemoteOpMkdir, Path: "../逃出去"}),
		"绝对路径":             mk(PFRemoteStep{Op: PFRemoteOpMkdir, Path: "/etc/x"}),
		"sha 格式不对":         mk(PFRemoteStep{Op: PFRemoteOpInstallNew, Path: "x", SHA256: "ZZ", MtimeNs: 1}),
		"前提是不存在":           mk(PFRemoteStep{Op: PFRemoteOpDelete, Path: "x", Expect: &PFExpect{Absent: true}, Reason: "删除"}),
		"前提是目录":            mk(PFRemoteStep{Op: PFRemoteOpDelete, Path: "x", Expect: &PFExpect{T: "d", SHA256: sha}, Reason: "删除"}),
		"回收原因不对":           mk(PFRemoteStep{Op: PFRemoteOpDelete, Path: "x", Expect: goodExp, Reason: "覆盖"}),
		"冲突名带目录":           mk(PFRemoteStep{Op: PFRemoteOpConflictName, Path: "x", To: "d/y", Expect: goodExp}),
		"复制来源位置不对":         mk(PFRemoteStep{Op: PFRemoteOpCopyNew, Path: "x", MtimeNs: 1, Src: &PFRemoteCopySrc{Where: "磁盘", Path: "y", SHA256: sha}}),
		"暂存区来源不是part":      mk(PFRemoteStep{Op: PFRemoteOpCopyNew, Path: "x", MtimeNs: 1, Src: &PFRemoteCopySrc{Where: PFRemoteSrcStaging, Path: "../../etc/passwd", SHA256: sha}}),
		"unit_mkdir 不在第一步": mk(PFRemoteStep{Op: PFRemoteOpUnitMkdir}),
		"状态给了两个":           mk(PFRemoteStep{Op: PFRemoteOpWriteStatus, Status: &PFRemoteStatus{Version: 1, Unit: pfrtCode}, MacStatus: &PFRemoteMacStatus{Version: 1, MacSeenAt: "2026-10-27T12:00:00Z", Network: "good"}}),
		"状态不合格":            mk(PFRemoteStep{Op: PFRemoteOpWriteStatus, Status: &PFRemoteStatus{Version: 1, Unit: "小写"}}),
		"请求名不对":            mk(PFRemoteStep{Op: PFRemoteOpTakeRequests, Names: []string{"../x.json"}}),
		"天数为零":             mk(PFRemoteStep{Op: PFRemoteOpRecycleClean}),
		"keep 不是哈希":        mk(PFRemoteStep{Op: PFRemoteOpStagingClean, Keep: []string{"abc"}}),
		"没有步骤":             nil,
	}
	for name, steps := range bad {
		// 服务端：什么都不做，响应里 Error 说明
		out, err := f.rawServe(PFRemoteSubApply, req(steps))
		pfrtNoErr(t, err)
		var resp PFRemoteApplyResponse
		pfrtNoErr(t, json.Unmarshal(out, &resp))
		if resp.Error == "" || len(resp.Results) != 0 || resp.Stopped {
			t.Errorf("%s: 服务端应整批拒绝: %+v", name, resp)
		}
		if f.exists("不该被建") {
			t.Fatalf("%s: 整批拒绝却执行了前面的步骤", name)
		}
		// 客户端：本机校验就拦住，不发出去
		calls := f.run.calls
		_, err = f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: f.unit()}, steps)
		var re *PFRemoteError
		if !errors.As(err, &re) || re.Kind != PFRemoteErrLocal || f.run.calls != calls {
			t.Errorf("%s: 客户端应在本机拦住: %v calls %d→%d", name, err, calls, f.run.calls)
		}
	}
	// 步骤太多
	many := make([]PFRemoteStep, PFRemoteMaxSteps+1)
	for i := range many {
		many[i] = PFRemoteStep{Op: PFRemoteOpMkdir, Path: fmt.Sprintf("d%d", i)}
	}
	if _, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: f.unit()}, many); err == nil || PFRemoteIsUncertain(err) {
		t.Fatalf("步骤太多应在本机拦住: %v", err)
	}
	// 单元找不到 / 编号对不上 / 重复
	unitless := []PFRemoteStep{{Op: PFRemoteOpMkdir, Path: "不该被建"}}
	for name, u := range map[string]PFRemoteUnit{
		"单元不存在":   {Code: "260403J9999", Rel: "2026/260403J9999-无"},
		"路径对不上编号": {Code: "260403J1002", Rel: pfrtRel},
	} {
		_, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: u}, unitless)
		var re *PFRemoteError
		if !errors.As(err, &re) || re.Kind != PFRemoteErrRefused {
			t.Errorf("%s: 应被拒绝: %v", name, err)
		}
	}
	f.putIn("2025/260403J1001-副本", "b.txt", "B")
	_, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: f.unit()}, unitless)
	var re *PFRemoteError
	if !errors.As(err, &re) || re.Kind != PFRemoteErrRefused || !strings.Contains(re.Msg, "多个目录") {
		t.Fatalf("同编号多个目录应拒绝: %v", err)
	}
	if f.exists("不该被建") {
		t.Fatal("被拒绝的批不该有任何写入")
	}
}

func TestPFRemoteApplyLease(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	steps := []PFRemoteStep{{Op: PFRemoteOpMkdir, Path: "新"}}
	// 别人持有且未过期：整批拒绝，一步不做
	pfrtNoErr(t, pfrWriteLease(f.meta, PFRemoteLease{Version: 1, ID: "mac-B", Host: "另一台", Expires: f.now.Add(time.Minute).UTC().Format(time.RFC3339)}))
	_, err := f.apply(steps...)
	var re *PFRemoteError
	if !errors.As(err, &re) || re.Kind != PFRemoteErrRefused || !strings.Contains(re.Msg, "mac-B") || PFRemoteIsUncertain(err) {
		t.Fatalf("租约被占应整批拒绝: %v", err)
	}
	if f.exists("新") {
		t.Fatal("被拒绝的批写了东西")
	}
	// 过期了：放行
	f.now = f.now.Add(2 * time.Minute)
	res := f.mustApply(steps...)
	pfrtDone(t, res.Results[0])
	// 自己持有：放行
	pfrtNoErr(t, pfrWriteLease(f.meta, PFRemoteLease{Version: 1, ID: "mac-A", Host: "我", Expires: f.now.Add(time.Minute).UTC().Format(time.RFC3339)}))
	res = f.mustApply(PFRemoteStep{Op: PFRemoteOpMkdir, Path: "新2"})
	pfrtDone(t, res.Results[0])
	// 租约文件读不懂：拒绝
	pfrtNoErr(t, os.WriteFile(f.leaseFile(), []byte("坏"), 0o644))
	if _, err := f.apply(PFRemoteStep{Op: PFRemoteOpMkdir, Path: "新3"}); err == nil || f.exists("新3") {
		t.Fatalf("租约文件读不懂应拒绝: %v", err)
	}
}

// ── 中途停止 ────────────────────────────────────────────

func TestPFRemoteApplyStopsOnUncertain(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	shaA, sizeA := f.stage("内容甲")
	shaB, sizeB := f.stage("内容乙")
	shaC, sizeC := f.stage("内容丙")
	installs := 0
	f.srv.ApplyHook = func(w string) error {
		if w == "new:before-rename" {
			installs++
			if installs == 2 {
				return errors.New("模拟进程死在改名之前")
			}
		}
		return nil
	}
	res := f.mustApply(
		PFRemoteStep{Op: PFRemoteOpInstallNew, Path: "甲.txt", SHA256: shaA, Size: sizeA, MtimeNs: pfrtMt},
		PFRemoteStep{Op: PFRemoteOpInstallNew, Path: "乙.txt", SHA256: shaB, Size: sizeB, MtimeNs: pfrtMt},
		PFRemoteStep{Op: PFRemoteOpInstallNew, Path: "丙.txt", SHA256: shaC, Size: sizeC, MtimeNs: pfrtMt},
	)
	if !res.Stopped || res.StopAt != 1 || res.NotRun != 1 || len(res.Results) != 2 || !strings.Contains(res.StopReason, "不确定") {
		t.Fatalf("应停在第 2 步: %+v", res)
	}
	pfrtDone(t, res.Results[0])
	r := res.Results[1].Apply
	if r.Status != PFApplyAbandoned || !r.Uncertain {
		t.Fatalf("第 2 步应是放弃且不确定: %+v", r)
	}
	if f.read("甲.txt") != "内容甲" || f.exists("丙.txt") {
		t.Fatal("第 1 步应已完成、第 3 步不该执行")
	}
}

func TestPFRemoteApplyStopsOnConfigError(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	// 注入：回收区与根不在同一个文件系统
	f.srv.Stat = func(p string) (PFStat, error) {
		st, err := PFLstat(p)
		if err == nil && p == PFRemoteRecycleDir(f.meta) {
			st.Dev++
		}
		return st, err
	}
	res := f.mustApply(
		PFRemoteStep{Op: PFRemoteOpMkdir, Path: "新"},
		PFRemoteStep{Op: PFRemoteOpMkdir, Path: "新2"},
	)
	if !res.Stopped || res.StopAt != 0 || res.NotRun != 1 || !res.Results[0].Config || !strings.Contains(res.Results[0].Error, "不在同一个文件系统") {
		t.Fatalf("应以配置错误停在第 1 步: %+v", res)
	}
	if f.exists("新") || f.exists("新2") {
		t.Fatal("配置错误时不该写东西")
	}
}

func TestPFRemoteApplyStopOnFail(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	missing := pfrtSHA("暂存区里没有这个")
	steps := []PFRemoteStep{
		{Op: PFRemoteOpInstallNew, Path: "x.txt", SHA256: missing, Size: 3, MtimeNs: pfrtMt},
		{Op: PFRemoteOpMkdir, Path: "后面的"},
	}
	// 缺省：part 不在 → 放弃（不确定=否），后面的照常
	res := f.mustApply(steps...)
	if res.Stopped || res.Results[0].Apply.Code != PFApplyCodePartMissing || !f.exists("后面的") {
		t.Fatalf("缺省不该停: %+v", res)
	}
	pfrtNoErr(t, os.Remove(f.abs("后面的")))
	// StopOnFail：第一步没完成就停
	res, err := f.cl.Apply(context.Background(), PFRemoteApplyOptions{Unit: f.unit(), StopOnFail: true}, steps)
	pfrtNoErr(t, err)
	if !res.Stopped || res.StopAt != 0 || res.NotRun != 1 || f.exists("后面的") {
		t.Fatalf("StopOnFail 应停在第 1 步: %+v", res)
	}
}

// ── verify_part ────────────────────────────────────────

func TestPFRemoteVerifyPartAndTargetVerify(t *testing.T) {
	f := newPFRTFix(t)
	ctx := context.Background()
	sha, size := f.stage("要核验的内容")
	// 直接用步骤：不带单元
	res, err := f.cl.Apply(ctx, PFRemoteApplyOptions{}, []PFRemoteStep{{Op: PFRemoteOpVerifyPart, SHA256: sha}})
	pfrtNoErr(t, err)
	if r := res.Results[0]; r.Sum != sha || r.Size != size || r.Error != "" {
		t.Fatalf("%+v", r)
	}
	// 给传输层用的 PFTargetVerify
	verify := f.cl.TargetVerify()
	part := filepath.Join(PFRemoteStagingDir(f.meta), sha+".part")
	sum, n, err := verify(ctx, part)
	if err != nil || sum != sha || n != size {
		t.Fatalf("核验: %q %d %v", sum, n, err)
	}
	// part 内容与文件名不符：只报告算出来的值，不比较
	other, _ := f.stage("另一份")
	pfrtNoErr(t, os.Rename(filepath.Join(PFRemoteStagingDir(f.meta), other+".part"), filepath.Join(PFRemoteStagingDir(f.meta), sha+".part")))
	sum, n, err = verify(ctx, part)
	if err != nil || sum != other || n != int64(len("另一份")) {
		t.Fatalf("应报告实际算出的值: %q %d %v", sum, n, err)
	}
	// 文件不存在：error（不是不符）
	if _, _, err := verify(ctx, filepath.Join(PFRemoteStagingDir(f.meta), pfrtSHA("没有")+".part")); err == nil || PFRemoteIsUncertain(err) {
		t.Fatalf("文件不存在应是 error: %v", err)
	}
	// 不在 Ubuntu 暂存区里的路径：本机就拒绝，不发请求
	calls := f.run.calls
	for _, p := range []string{part + "x", filepath.Join(f.meta, "别处", sha+".part"), filepath.Join(PFRemoteStagingDir(f.meta), "x", sha+".part"), "/etc/passwd"} {
		if _, _, err := verify(ctx, p); err == nil {
			t.Errorf("%q 应被拒绝", p)
		}
	}
	if f.run.calls != calls {
		t.Fatal("不合格的路径不该发出请求")
	}
	// 通信失败：error 且是不确定
	f.run.fail = errors.New("连接断了")
	if _, _, err := verify(ctx, part); !PFRemoteIsUncertain(err) {
		t.Fatalf("通信失败应是不确定: %v", err)
	}
	// 符号链接不算：暂存区里的 <sha>.part 是链接就不算
	f.run.fail = nil
	link := pfrtSHA("链接")
	pfrtNoErr(t, os.Symlink(f.root, filepath.Join(PFRemoteStagingDir(f.meta), link+".part")))
	if _, _, err := verify(ctx, filepath.Join(PFRemoteStagingDir(f.meta), link+".part")); err == nil {
		t.Fatal("符号链接不该被核验")
	}
}

// ── 状态文件、请求文件 ──────────────────────────────────

func TestPFRemoteApplyStatusAndRequests(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	ub := int64(10)
	st := PFRemoteStatus{Version: 1, Unit: pfrtCode, RoundAt: "2026-10-27T12:00:00Z", MacSeenAt: "2026-10-27T12:00:00Z", UbuntuRel: pfrtRel,
		PendingToUbuntu: []PFRemotePending{{Path: "报告/大.mp4", Size: 100, Sha: pfrtSHA("x"), MacMtimeNs: 5, UbSize: &ub, UbMtimeNs: 4, Reason: PFRemoteReasonDeferred}},
		Conflicts:       []PFRemoteConflict{{Path: "a.docx", ConflictPath: "a（冲突-Mac-1027-1200）.docx", Winner: "ubuntu", At: "2026-10-27T12:00:00Z"}},
		Notes:           []string{"这一轮删了 3 个，都在同步回收里"}}
	mac := PFRemoteMacStatus{Version: 1, MacSeenAt: "2026-10-27T12:00:00Z", Network: PFRemoteNetExpensive, Host: "测试Mac"}
	// 请求文件：写两个
	n1, err := PFRemoteWriteRequest(f.meta, PFRemoteRequest{Unit: pfrtCode, Paths: []string{"a.txt"}, Priority: PFRemotePrioUser}, f.now)
	pfrtNoErr(t, err)
	n2, err := PFRemoteWriteRequest(f.meta, PFRemoteRequest{Kind: PFRemoteReqTouched, Unit: pfrtCode, Priority: PFRemotePrioUser}, f.now.Add(time.Second))
	pfrtNoErr(t, err)
	pr, err := f.cl.Poll(ctx, PFRemotePollOptions{})
	pfrtNoErr(t, err)
	if len(pr.Requests) != 2 || pr.Requests[0].Name != n1 || pr.Requests[1].Name != n2 {
		t.Fatalf("poll 的请求: %+v", pr.Requests)
	}

	res, err := f.cl.Apply(ctx, PFRemoteApplyOptions{}, []PFRemoteStep{
		{Op: PFRemoteOpWriteStatus, Status: &st},
		{Op: PFRemoteOpWriteStatus, MacStatus: &mac},
		{Op: PFRemoteOpTakeRequests, Names: []string{n1, "260403J1001-20250101T000000Z-00000000.json"}},
	})
	pfrtNoErr(t, err)
	if res.Stopped || len(res.Results) != 3 {
		t.Fatalf("%+v", res)
	}
	// 状态文件往返（Python 读的同一份）
	got, err := PFRemoteReadStatus(f.meta, pfrtCode)
	pfrtNoErr(t, err)
	if got.UbuntuRel != pfrtRel || len(got.PendingToUbuntu) != 1 || got.PendingToUbuntu[0].Path != "报告/大.mp4" || *got.PendingToUbuntu[0].UbSize != 10 ||
		got.PendingToMac == nil || len(got.PendingToMac) != 0 || len(got.Conflicts) != 1 || got.Notes[0] != st.Notes[0] {
		t.Fatalf("状态文件: %+v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(f.meta, PFRemoteStatusName, pfrtCode+".json"))
	if !strings.Contains(string(raw), `"pending_to_mac":[]`) {
		t.Fatalf("空列表应写成 [] 而不是 null: %s", raw)
	}
	gm, err := PFRemoteReadMacStatus(f.meta)
	pfrtNoErr(t, err)
	if gm != mac {
		t.Fatalf("_mac.json: %+v", gm)
	}
	// 取走请求：n1 拿到内容并删除，不存在的记 missing；n2 还在
	tk := res.Results[2]
	if len(tk.Taken) != 1 || tk.Taken[0].Name != n1 || tk.Taken[0].Request.Paths[0] != "a.txt" || len(tk.Missing) != 1 {
		t.Fatalf("take_requests: %+v", tk)
	}
	if _, err := os.Lstat(filepath.Join(f.meta, PFRemoteRequestName, n1)); !os.IsNotExist(err) {
		t.Fatal("取走的请求文件应已删除")
	}
	pr, err = f.cl.Poll(ctx, PFRemotePollOptions{})
	pfrtNoErr(t, err)
	if len(pr.Requests) != 1 || pr.Requests[0].Name != n2 {
		t.Fatalf("取走后 poll 的请求: %+v", pr.Requests)
	}
	// 读不懂的请求文件：点名 take 就删掉并报告
	badName := pfrtCode + "-20261027T020000Z-deadbeef.json"
	pfrtNoErr(t, os.WriteFile(filepath.Join(f.meta, PFRemoteRequestName, badName), []byte(`{"kind":"fetch"}`), 0o644))
	res, err = f.cl.Apply(ctx, PFRemoteApplyOptions{}, []PFRemoteStep{{Op: PFRemoteOpTakeRequests, Names: []string{badName}}})
	pfrtNoErr(t, err)
	if len(res.Results[0].Bad) != 1 || res.Results[0].Bad[0].Name != badName {
		t.Fatalf("%+v", res.Results[0])
	}
	if _, err := os.Lstat(filepath.Join(f.meta, PFRemoteRequestName, badName)); !os.IsNotExist(err) {
		t.Fatal("点名取走的坏文件应已删除")
	}
}

// ── 清理 ────────────────────────────────────────────────

func TestPFRemoteApplyCleaners(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	// 回收：一个 40 天前的日期目录、一个 3 天前的
	rec := PFRemoteRecycleDir(f.meta)
	for _, d := range []string{"2026-09-01", "2026-10-24"} {
		pfrtNoErr(t, os.MkdirAll(filepath.Join(rec, d, "u"), 0o755))
		pfrtNoErr(t, os.WriteFile(filepath.Join(rec, d, "u", "f.txt"), []byte("旧"), 0o644))
	}
	// 暂存区：一份 10 天前的、一份刚写的、一份 10 天前但在 keep 里、一个不相干的名字
	stg := PFRemoteStagingDir(f.meta)
	pfrtNoErr(t, os.MkdirAll(stg, 0o700))
	oldSHA, keepSHA, newSHA := pfrtSHA("旧"), pfrtSHA("留"), pfrtSHA("新")
	oldT := f.now.Add(-10 * 24 * time.Hour)
	for _, s := range []string{oldSHA, keepSHA} {
		p := filepath.Join(stg, s+".part")
		pfrtNoErr(t, os.WriteFile(p, []byte("x"), 0o600))
		pfrtNoErr(t, os.Chtimes(p, oldT, oldT))
	}
	pfrtNoErr(t, os.WriteFile(filepath.Join(stg, newSHA+".part"), []byte("x"), 0o600))
	pfrtNoErr(t, os.Chtimes(filepath.Join(stg, newSHA+".part"), f.now, f.now))
	pfrtNoErr(t, os.WriteFile(filepath.Join(stg, "别的文件.txt"), []byte("x"), 0o600))
	pfrtNoErr(t, os.Chtimes(filepath.Join(stg, "别的文件.txt"), oldT, oldT))

	res, err := f.cl.Apply(ctx, PFRemoteApplyOptions{}, []PFRemoteStep{
		{Op: PFRemoteOpRecycleClean, Days: 30},
		{Op: PFRemoteOpStagingClean, Keep: []string{keepSHA}},
	})
	pfrtNoErr(t, err)
	rc := res.Results[0].Clean
	if rc == nil || rc.Days != 30 || len(rc.Removed) != 1 || rc.Removed[0] != "2026-09-01" || rc.Kept != 1 {
		t.Fatalf("回收清理: %+v", rc)
	}
	if _, err := os.Lstat(filepath.Join(rec, "2026-09-01")); !os.IsNotExist(err) {
		t.Fatal("超期的日期目录应被删")
	}
	if _, err := os.Lstat(filepath.Join(rec, "2026-10-24")); err != nil {
		t.Fatal("没超期的不该删")
	}
	sc := res.Results[1].Clean
	if sc == nil || len(sc.Removed) != 1 || sc.Removed[0] != oldSHA+".part" || sc.Kept != 2 {
		t.Fatalf("暂存区清理: %+v", sc)
	}
	for name, want := range map[string]bool{oldSHA + ".part": false, keepSHA + ".part": true, newSHA + ".part": true, "别的文件.txt": true} {
		if _, err := os.Lstat(filepath.Join(stg, name)); (err == nil) != want {
			t.Errorf("暂存区里 %s 存在=%v，应为 %v", name, err == nil, want)
		}
	}
	// 天数不足 7 天按 7 天算（回收层的保底）
	res, err = f.cl.Apply(ctx, PFRemoteApplyOptions{}, []PFRemoteStep{{Op: PFRemoteOpRecycleClean, Days: 1}})
	pfrtNoErr(t, err)
	if res.Results[0].Clean.Days != PFRecycleMinDays {
		t.Fatalf("%+v", res.Results[0].Clean)
	}
}

// ── 扩展点（T10 的 ledger_stage / ledger_install 用） ─────

func TestPFRemoteApplyExtensionPoint(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	called := 0
	pfrRegisterOp("test_ext_op", pfrExtOp{
		unit: true,
		validate: func(st *PFRemoteStep) error {
			if len(st.Ext) == 0 {
				return errors.New("缺少 ext")
			}
			return nil
		},
		run: func(ctx context.Context, rc *pfrApplyRun, st *PFRemoteStep) PFRemoteStepResult {
			called++
			a, err := rc.applier() // 扩展步骤可以拿到同一个 PFApplier
			if err != nil {
				return pfrCfgErr(err)
			}
			r, err := a.Mkdir(ctx, "由扩展步骤建")
			res := pfrPrim(r, err)
			res.Ext = st.Ext
			return res
		},
	})
	defer delete(pfrExtOps, "test_ext_op")
	res := f.mustApply(PFRemoteStep{Op: "test_ext_op", Ext: json.RawMessage(`{"k":1}`)}, PFRemoteStep{Op: PFRemoteOpMkdir, Path: "后"})
	pfrtDone(t, res.Results[0])
	if called != 1 || string(res.Results[0].Ext) != `{"k":1}` || !f.exists("由扩展步骤建") || !f.exists("后") {
		t.Fatalf("扩展步骤: %+v called=%d", res.Results[0], called)
	}
	// 扩展步骤自己的校验
	if _, err := f.apply(PFRemoteStep{Op: "test_ext_op"}); err == nil {
		t.Fatal("扩展步骤的校验应生效")
	}
	// 内置步骤不能带 ext
	if _, err := f.apply(PFRemoteStep{Op: PFRemoteOpMkdir, Path: "x", Ext: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("内置步骤不该带 ext")
	}
	// 不重复注册、不与内置冲突
	for _, name := range []string{"test_ext_op", PFRemoteOpMkdir, ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("注册 %q 应 panic", name)
				}
			}()
			pfrRegisterOp(name, pfrExtOp{validate: func(*PFRemoteStep) error { return nil }, run: func(context.Context, *pfrApplyRun, *PFRemoteStep) PFRemoteStepResult { return PFRemoteStepResult{} }})
		}()
	}
	delete(pfrExtOps, "test_ext_op")
	if _, err := f.apply(PFRemoteStep{Op: "test_ext_op", Ext: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("注销后应不认识")
	}
}
