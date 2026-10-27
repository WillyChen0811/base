package projsync

// 项目文件同步 T8：projfiles-apply 服务端（技术设计 §0「写入」、§3.5、§5.2 第 4–6 步、§5.6、§6.2、§7）。
//
// 一批步骤按顺序在 Ubuntu 本机执行：写原语类步骤交给 T6 的 PFApplier（路径、前提核对、回收都由它负责），
// 其余是 Ubuntu 本机的小动作（算暂存区 part 的哈希、写状态文件、取走请求文件、清理回收和暂存区）。
// 每一步的结果原样放进响应（PFApplyResult 已可 JSON 编码）。
//
// 【整批拒绝（Error，一步都没做）】请求读得懂但这次不做：根不在白名单、租约在别的协调者手里或读不懂、步骤不合法
// （未知种类、字段不对、路径不合法、sha256 格式不对……所有步骤都先校验完才开始执行）、单元找不到（按 rel 配不上，§14.4）
// 或 rel 上的编号与请求对不上。同编号另有目录不再整批拒绝（§14.2，没配置的只报告）。
//
// 【中途停止】按顺序执行，出现下面任一情况就不再执行后面的步骤，响应里写明停在第几步：
//   - 某一步结果是「放弃且 Uncertain」（现场没能完全恢复原状）；
//   - 某一步出现配置错误（参数不合法、暂存区或回收区与根不在同一文件系统……，PFApplier 返回的 error）；
//   - 请求带了 StopOnFail，而某一步没完成（放弃、推迟、超出范围、出错）；
//   - 请求被取消。
//     其余「放弃/推迟/超出范围」只影响那一个文件，后面的步骤照常执行（每个写原语自己核对前提，互不依赖）；
//     有先后依赖的几步（例如冲突：改名 → 新建）让调用方带 StopOnFail。
//
// 【单元目录】PFApplier 要求单元目录已存在。新项目首次传到 Ubuntu 时第一步用 unit_mkdir 建单元目录本身：
// 只在 Rel 的每一级都过得了名字规则、最后一级的编号等于 Code、Ubuntu 上没有别的目录用这个编号、路径上没有符号链接时才建。
//
// 【扩展点】后续任务（T10 台账：ledger_stage / ledger_install）用 pfrRegisterOp 注册新的步骤种类，带自己的校验和执行，
// 请求里放在 PFRemoteStep.Ext、结果放在 PFRemoteStepResult.Ext；本文件不需要再改。
//
// 边界：本文件不判断同步规则，也不更新 base；暂存区里只动 <sha256>.part（由 PFApplier 和 PFCleanStaging 负责）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ── 步骤校验 ────────────────────────────────────────────

type pfrOpSpec struct {
	unit  bool     // 要动单元目录
	allow []string // 允许出现的字段
	need  []string // 必须出现的字段
}

var pfrOps = map[string]pfrOpSpec{
	PFRemoteOpUnitMkdir:    {unit: true},
	PFRemoteOpMkdir:        {unit: true, allow: []string{"path"}, need: []string{"path"}},
	PFRemoteOpRmdir:        {unit: true, allow: []string{"path"}, need: []string{"path"}},
	PFRemoteOpInstallNew:   {unit: true, allow: []string{"path", "sha256", "size", "mtime_ns"}, need: []string{"path", "sha256", "mtime_ns"}},
	PFRemoteOpInstallOver:  {unit: true, allow: []string{"path", "sha256", "size", "mtime_ns", "expect"}, need: []string{"path", "sha256", "mtime_ns", "expect"}},
	PFRemoteOpDelete:       {unit: true, allow: []string{"path", "expect", "reason"}, need: []string{"path", "expect", "reason"}},
	PFRemoteOpRename:       {unit: true, allow: []string{"path", "to", "expect"}, need: []string{"path", "to", "expect"}},
	PFRemoteOpConflictName: {unit: true, allow: []string{"path", "to", "expect"}, need: []string{"path", "to", "expect"}},
	PFRemoteOpCopyNew:      {unit: true, allow: []string{"src", "path", "mtime_ns"}, need: []string{"src", "path", "mtime_ns"}},
	PFRemoteOpCopyOver:     {unit: true, allow: []string{"src", "path", "mtime_ns", "expect"}, need: []string{"src", "path", "mtime_ns", "expect"}},
	PFRemoteOpVerifyPart:   {allow: []string{"sha256"}, need: []string{"sha256"}},
	PFRemoteOpWriteStatus:  {allow: []string{"status", "mac_status"}},
	PFRemoteOpTakeRequests: {allow: []string{"names"}, need: []string{"names"}},
	PFRemoteOpRecycleClean: {allow: []string{"days"}, need: []string{"days"}},
	PFRemoteOpStagingClean: {allow: []string{"max_age_hours", "keep"}},
}

// fields 列出步骤里出现（非零）的字段名。
func (st *PFRemoteStep) fields() map[string]bool {
	m := map[string]bool{}
	for k, ok := range map[string]bool{
		"path": st.Path != "", "to": st.To != "", "sha256": st.SHA256 != "", "size": st.Size != 0, "mtime_ns": st.MtimeNs != 0,
		"expect": st.Expect != nil, "reason": st.Reason != "", "src": st.Src != nil, "status": st.Status != nil,
		"mac_status": st.MacStatus != nil, "names": len(st.Names) > 0, "days": st.Days != 0,
		"max_age_hours": st.MaxAgeHours != 0, "keep": len(st.Keep) > 0, "ext": len(st.Ext) > 0,
	} {
		if ok {
			m[k] = true
		}
	}
	return m
}

var pfrStagingPartRe = regexp.MustCompile(`^[0-9a-f]{64}\.part$`)

func pfrCheckFileExpect(e *PFExpect) error {
	switch {
	case e == nil:
		return errors.New("缺少前提 expect")
	case e.Absent:
		return errors.New("前提不能是「目标不存在」")
	case e.T != "" && e.T != PFBaseTypeFile:
		return fmt.Errorf("前提的类型 %q 不是文件", e.T)
	case !pfValidSHA(e.SHA256):
		return fmt.Errorf("前提的 sha256 %q 不合法", e.SHA256)
	case e.Size < 0:
		return errors.New("前提的大小为负")
	}
	return nil
}

// pfrValidateStep 校验一步的字段（不碰磁盘）。
func pfrValidateStep(st *PFRemoteStep) error {
	if ext, ok := pfrExtOps[st.Op]; ok {
		return ext.validate(st)
	}
	spec, ok := pfrOps[st.Op]
	if !ok {
		return fmt.Errorf("不认识的步骤种类 %q", st.Op)
	}
	present := st.fields()
	allow := map[string]bool{}
	for _, f := range spec.allow {
		allow[f] = true
	}
	for f := range present {
		if !allow[f] {
			return fmt.Errorf("%s 步骤不该带字段 %s", st.Op, f)
		}
	}
	for _, f := range spec.need {
		if !present[f] {
			return fmt.Errorf("%s 步骤缺少字段 %s", st.Op, f)
		}
	}
	checkPath := func(field, p string) error {
		if err := validRel(p); err != nil {
			return fmt.Errorf("%s 步骤的 %s：%v", st.Op, field, err)
		}
		if !utf8.ValidString(p) {
			return fmt.Errorf("%s 步骤的 %s 不是合法 UTF-8", st.Op, field)
		}
		return nil
	}
	if present["path"] {
		if err := checkPath("path", st.Path); err != nil {
			return err
		}
	}
	if present["to"] {
		if st.Op == PFRemoteOpConflictName {
			if strings.ContainsRune(st.To, '/') || !utf8.ValidString(st.To) || st.To == "." || st.To == ".." {
				return fmt.Errorf("conflict_rename 的 to 应是冲突副本名（不含目录）：%q", st.To)
			}
		} else if err := checkPath("to", st.To); err != nil {
			return err
		}
	}
	if present["sha256"] && !pfValidSHA(st.SHA256) {
		return fmt.Errorf("%s 步骤的 sha256 %q 不合法", st.Op, st.SHA256)
	}
	if st.Size < 0 {
		return fmt.Errorf("%s 步骤的 size 为负", st.Op)
	}
	if present["expect"] {
		if err := pfrCheckFileExpect(st.Expect); err != nil {
			return fmt.Errorf("%s 步骤：%v", st.Op, err)
		}
	}
	if present["reason"] && PFRecycleReason(st.Reason) != PFReasonDelete && PFRecycleReason(st.Reason) != PFReasonConflict {
		return fmt.Errorf("delete 的回收原因 %q 不合法（只能是「删除」或「冲突」）", st.Reason)
	}
	if present["src"] {
		s := st.Src
		switch s.Where {
		case PFRemoteSrcUnit, PFRemoteSrcRecycle:
			if err := checkPath("src.path", s.Path); err != nil {
				return err
			}
		case PFRemoteSrcStaging:
			if !pfrStagingPartRe.MatchString(s.Path) {
				return fmt.Errorf("来自暂存区的复制来源应是 <sha256>.part：%q", s.Path)
			}
		default:
			return fmt.Errorf("复制来源的位置 %q 不认识", s.Where)
		}
		if !pfValidSHA(s.SHA256) || s.Size < 0 {
			return errors.New("复制来源的 sha256 或大小不合法")
		}
	}
	switch st.Op {
	case PFRemoteOpWriteStatus:
		if (st.Status == nil) == (st.MacStatus == nil) {
			return errors.New("write_status 要在 status 和 mac_status 里恰好给一个")
		}
		if st.Status != nil {
			c := *st.Status
			c.norm()
			if err := c.Validate(); err != nil {
				return fmt.Errorf("write_status：%v", err)
			}
		} else if err := st.MacStatus.Validate(); err != nil {
			return fmt.Errorf("write_status：%v", err)
		}
	case PFRemoteOpTakeRequests:
		if len(st.Names) > pfrMaxRequestFiles {
			return fmt.Errorf("take_requests 一次最多 %d 个文件", pfrMaxRequestFiles)
		}
		for _, n := range st.Names {
			if !pfrReqNameRe.MatchString(n) {
				return fmt.Errorf("take_requests：%q 不是请求文件名", n)
			}
		}
	case PFRemoteOpRecycleClean:
		if st.Days < 1 || st.Days > 3650 {
			return fmt.Errorf("recycle_clean 的 days %d 超出 1–3650", st.Days)
		}
	case PFRemoteOpStagingClean:
		if st.MaxAgeHours < 0 || st.MaxAgeHours > 24*3650 {
			return fmt.Errorf("staging_clean 的 max_age_hours %d 超出 0–%d", st.MaxAgeHours, 24*3650)
		}
		if len(st.Keep) > 100000 {
			return errors.New("staging_clean 的 keep 太多")
		}
		for _, k := range st.Keep {
			if !pfValidSHA(k) {
				return fmt.Errorf("staging_clean 的 keep 里 %q 不是 sha256", k)
			}
		}
	}
	return nil
}

// pfrValidateApplyRequest 校验整个 apply 请求（不碰磁盘）；服务端执行前、客户端发送前各调一次。
func pfrValidateApplyRequest(req *PFRemoteApplyRequest) error {
	if err := pfrCheckAbsDir("meta", req.Meta); err != nil {
		return err
	}
	if err := pfrCheckAbsDir("根", req.Root); err != nil {
		return err
	}
	if !pfrCoordIDRe.MatchString(req.CoordID) {
		return fmt.Errorf("coord_id %q 不合法", req.CoordID)
	}
	if len(req.Round) > 64 || !utf8.ValidString(req.Round) || strings.ContainsAny(req.Round, "\x00\n\r") {
		return errors.New("round 不合法")
	}
	if len(req.Steps) == 0 {
		return errors.New("没有步骤")
	}
	if len(req.Steps) > PFRemoteMaxSteps {
		return fmt.Errorf("一批最多 %d 步，收到 %d 步", PFRemoteMaxSteps, len(req.Steps))
	}
	needUnit := false
	for i := range req.Steps {
		st := &req.Steps[i]
		if err := pfrValidateStep(st); err != nil {
			return fmt.Errorf("第 %d 步（%s）：%w", i+1, st.Op, err)
		}
		if st.Op == PFRemoteOpUnitMkdir && i != 0 {
			return fmt.Errorf("第 %d 步：unit_mkdir 只能是第一步", i+1)
		}
		if spec, ok := pfrOps[st.Op]; ok && spec.unit {
			needUnit = true
		} else if ext, ok := pfrExtOps[st.Op]; ok && ext.unit {
			needUnit = true
		}
	}
	if needUnit {
		if err := pfrCheckUnitKey(req.Code); err != nil {
			return err
		}
		if err := validRel(req.Rel); err != nil {
			return err
		}
	} else if req.Code != "" || req.Rel != "" {
		if req.Code != "" {
			if err := pfrCheckUnitKey(req.Code); err != nil {
				return err
			}
		}
		if req.Rel != "" {
			if err := validRel(req.Rel); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── 扩展点 ──────────────────────────────────────────────

// pfrExtOp 是后续任务加的步骤种类。unit=true 表示要动单元目录（请求里必须带 Code、Rel，服务端先确认单元存在）。
// validate 只看字段；run 在 Ubuntu 本机执行并返回结果（Op 由调用方填）。run 可以用 rc.applier() 拿 PFApplier、
// rc.unitPath()、rc.env.staging() 等。结果的 Config / Apply 字段遵守同样的停止规则。
type pfrExtOp struct {
	unit     bool
	validate func(st *PFRemoteStep) error
	run      func(ctx context.Context, rc *pfrApplyRun, st *PFRemoteStep) PFRemoteStepResult
}

var pfrExtOps = map[string]pfrExtOp{}

// pfrRegisterOp 注册一个扩展步骤（包初始化时调用，名字不能和内置的重复）。
func pfrRegisterOp(name string, op pfrExtOp) {
	if _, dup := pfrOps[name]; dup || name == "" || op.validate == nil || op.run == nil {
		panic("pfrRegisterOp: 步骤种类不合法或与内置的重复: " + name)
	}
	if _, dup := pfrExtOps[name]; dup {
		panic("pfrRegisterOp: 步骤种类重复注册: " + name)
	}
	pfrExtOps[name] = op
}

// ── 执行 ────────────────────────────────────────────────

// pfrApplyRun 是一批 apply 的执行现场。
type pfrApplyRun struct {
	s    *PFRemoteServer
	env  *pfrEnv
	req  *PFRemoteApplyRequest
	disc *PFDiscoverResult
	unit *PFFoundUnit // 开始时就存在的单元；unit_mkdir 之后换成新建的
	app  *PFApplier
	rec  *PFRecycle
}

func (rc *pfrApplyRun) unitPath() string { return rc.env.unitAbs(rc.req.Rel) }

func (rc *pfrApplyRun) recycle() (*PFRecycle, error) {
	if rc.rec != nil {
		return rc.rec, nil
	}
	r, err := NewPFRecycle(rc.env.recycleDir(), PFRecycleOptions{Stat: rc.s.Stat, Now: rc.s.Now})
	if err != nil {
		return nil, err
	}
	rc.rec = r
	return r, nil
}

// applier 建（或取已建的）PFApplier；暂存区、回收区与根不在同一文件系统等配置错误从这里出来。
func (rc *pfrApplyRun) applier() (*PFApplier, error) {
	if rc.app != nil {
		return rc.app, nil
	}
	rec, err := rc.recycle()
	if err != nil {
		return nil, err
	}
	a, err := NewPFApplier(PFApplyConfig{Root: rc.unitPath(), UnitRel: rc.req.Rel, Staging: rc.env.staging(), Recycle: rec,
		Round: rc.req.Round, Now: rc.s.Now, Stat: rc.s.Stat, Hook: rc.s.ApplyHook})
	if err != nil {
		return nil, err
	}
	rc.app = a
	return a, nil
}

func (s *PFRemoteServer) apply(ctx context.Context, req *PFRemoteApplyRequest) PFRemoteApplyResponse {
	resp := PFRemoteApplyResponse{Version: PFRemoteVersion, Results: []PFRemoteStepResult{}}
	fail := func(f string, a ...any) PFRemoteApplyResponse {
		resp.Error = fmt.Sprintf(f, a...)
		return resp
	}
	if err := pfrValidateApplyRequest(req); err != nil {
		return fail("请求不合法，一步都没执行: %v", err)
	}
	env, err := s.openEnv(req.Meta, req.Root)
	if err != nil {
		return fail("%v", err)
	}
	if why := env.leaseBlocks(req.CoordID); why != "" {
		return fail("%s", why)
	}
	rc := &pfrApplyRun{s: s, env: env, req: req}
	needUnit := false
	for _, st := range req.Steps {
		if spec, ok := pfrOps[st.Op]; ok && spec.unit {
			needUnit = true
		} else if ext, ok := pfrExtOps[st.Op]; ok && ext.unit {
			needUnit = true
		}
	}
	if needUnit {
		disc, err := PFDiscoverUnits(env.root)
		if err != nil {
			return fail("发现单元失败: %v", err)
		}
		rc.disc = &disc
		// §14.4：按 rel 配对（不再按编号）；req.Code 是单元键，编号取键的前段。
		// 同编号多个目录已合法（没配置的只报告），不再 Dup 即停（取代 G4）。
		pc, _ := PFUnitKeyProjectCode(req.Code)
		for i := range disc.Units {
			if u := &disc.Units[i]; u.Rel == req.Rel {
				rc.unit = u
			}
		}
		switch {
		case rc.unit == nil && req.Steps[0].Op != PFRemoteOpUnitMkdir:
			return fail("Ubuntu 上没有单元 %s（%s），或那里的目录名认不出这个编号，一步都没执行", req.Code, req.Rel)
		case rc.unit != nil && rc.unit.Code != pc:
			return fail("单元 %s 登记的目录 %s 在 Ubuntu 上认出的编号是 %s，跟请求对不上，一步都没执行", req.Code, req.Rel, rc.unit.Code)
		}
	}

	for i := range req.Steps {
		if ctx.Err() != nil {
			resp.Stopped, resp.StopAt, resp.StopReason = true, i-1, "请求被取消"
			return resp
		}
		st := &req.Steps[i]
		r := rc.exec(ctx, st)
		r.Op = st.Op
		resp.Results = append(resp.Results, r)
		if why := pfrStopReason(&r, req.StopOnFail); why != "" {
			resp.Stopped, resp.StopAt, resp.StopReason = true, i, fmt.Sprintf("第 %d 步（%s）：%s", i+1, st.Op, why)
			return resp
		}
	}
	return resp
}

// pfrStopReason 判断这一步之后要不要停；不停返回空串。
func pfrStopReason(r *PFRemoteStepResult, stopOnFail bool) string {
	switch {
	case r.Config:
		return "配置错误：" + r.Error
	case r.Apply != nil && r.Apply.Status == PFApplyAbandoned && r.Apply.Uncertain:
		return "放弃且结果不确定（现场没能完全恢复原状）：" + r.Apply.Reason
	case stopOnFail && r.Error != "":
		return "没能执行：" + r.Error
	case stopOnFail && r.Apply != nil && r.Apply.Status != PFApplyDone:
		return fmt.Sprintf("没有完成（%s）：%s", r.Apply.Status, r.Apply.Reason)
	}
	return ""
}

func pfrCfgErr(err error) PFRemoteStepResult {
	return PFRemoteStepResult{Error: err.Error(), Config: true}
}

func pfrErr(f string, a ...any) PFRemoteStepResult {
	return PFRemoteStepResult{Error: fmt.Sprintf(f, a...)}
}

// prim 把写原语的 (结果, error) 包成步骤结果：error 只会是配置错误。
func pfrPrim(res PFApplyResult, err error) PFRemoteStepResult {
	if err != nil {
		return pfrCfgErr(err)
	}
	return PFRemoteStepResult{Apply: &res}
}

func (rc *pfrApplyRun) exec(ctx context.Context, st *PFRemoteStep) PFRemoteStepResult {
	if ext, ok := pfrExtOps[st.Op]; ok {
		return ext.run(ctx, rc, st)
	}
	staging := rc.env.staging()
	part := func() PFPart {
		return PFPart{Path: filepath.Join(staging, st.SHA256+".part"), SHA256: st.SHA256, Size: st.Size}
	}
	switch st.Op {
	case PFRemoteOpUnitMkdir:
		return rc.unitMkdir()
	case PFRemoteOpVerifyPart:
		return rc.verifyPart(ctx, st.SHA256)
	case PFRemoteOpWriteStatus:
		return rc.writeStatus(st)
	case PFRemoteOpTakeRequests:
		taken, missing, bad, err := PFRemoteTakeRequests(rc.env.meta, st.Names)
		r := PFRemoteStepResult{Taken: taken, Missing: missing, Bad: bad}
		if err != nil {
			r.Error = err.Error()
		}
		return r
	case PFRemoteOpRecycleClean:
		rec, err := rc.recycle()
		if err != nil {
			return pfrCfgErr(err)
		}
		cr, cerr := rec.Clean(st.Days)
		r := PFRemoteStepResult{Clean: &PFRemoteCleanInfo{Days: cr.Days, Removed: cr.Removed, Files: cr.Files, Bytes: cr.Bytes, Kept: cr.Kept}}
		if cerr != nil {
			r.Error = cerr.Error()
		}
		return r
	case PFRemoteOpStagingClean:
		hours := st.MaxAgeHours
		if hours == 0 {
			hours = 24 * 7
		}
		keep := map[string]bool{}
		for _, k := range st.Keep {
			keep[k] = true
		}
		cr, cerr := PFCleanStaging(ctx, PFLocalFS{}, staging, time.Duration(hours)*time.Hour, rc.s.now(), func(sha string) bool { return keep[sha] })
		r := PFRemoteStepResult{Clean: &PFRemoteCleanInfo{Removed: cr.Removed, Bytes: cr.Bytes, Kept: cr.Kept}}
		if cerr != nil {
			r.Error = cerr.Error()
		}
		return r
	}

	// 以下都要 PFApplier
	a, err := rc.applier()
	if err != nil {
		return pfrCfgErr(err)
	}
	copySrc := func() PFCopySrc {
		var base string
		switch st.Src.Where {
		case PFRemoteSrcUnit:
			base = rc.unitPath()
		case PFRemoteSrcRecycle:
			base = rc.env.recycleDir()
		default:
			base = staging
		}
		return PFCopySrc{Path: filepath.Join(base, filepath.FromSlash(st.Src.Path)), SHA256: st.Src.SHA256, Size: st.Src.Size}
	}
	switch st.Op {
	case PFRemoteOpMkdir:
		return pfrPrim(a.Mkdir(ctx, st.Path))
	case PFRemoteOpRmdir:
		return pfrPrim(a.Rmdir(ctx, st.Path))
	case PFRemoteOpInstallNew:
		return pfrPrim(a.InstallNew(ctx, st.Path, part(), st.MtimeNs))
	case PFRemoteOpInstallOver:
		return pfrPrim(a.InstallOver(ctx, st.Path, part(), st.MtimeNs, *st.Expect))
	case PFRemoteOpDelete:
		return pfrPrim(a.Delete(ctx, st.Path, *st.Expect, PFRecycleReason(st.Reason)))
	case PFRemoteOpRename:
		return pfrPrim(a.Rename(ctx, st.Path, st.To, *st.Expect))
	case PFRemoteOpConflictName:
		return pfrPrim(a.ConflictRename(ctx, st.Path, st.To, *st.Expect))
	case PFRemoteOpCopyNew:
		return pfrPrim(a.CopyInstallNew(ctx, copySrc(), st.Path, st.MtimeNs))
	case PFRemoteOpCopyOver:
		return pfrPrim(a.CopyInstallOver(ctx, copySrc(), st.Path, st.MtimeNs, *st.Expect))
	}
	return pfrCfgErr(fmt.Errorf("不认识的步骤种类 %q", st.Op))
}

// verifyPart 在 Ubuntu 本机算暂存区 <sha256>.part 的 sha256 和大小，只报告算出来的值（比较由传输层做）。
func (rc *pfrApplyRun) verifyPart(ctx context.Context, sha string) PFRemoteStepResult {
	p := filepath.Join(rc.env.staging(), sha+".part")
	fi, err := os.Lstat(p)
	if err != nil {
		return pfrErr("读暂存文件 %s.part 失败: %v", sha, err)
	}
	if !fi.Mode().IsRegular() {
		return pfrErr("暂存文件 %s.part 不是普通文件", sha)
	}
	sum, n, err := PFHashLocalFile(ctx, p)
	if err != nil {
		return pfrErr("算暂存文件 %s.part 的哈希失败: %v", sha, err)
	}
	return PFRemoteStepResult{Sum: sum, Size: n}
}

func (rc *pfrApplyRun) writeStatus(st *PFRemoteStep) PFRemoteStepResult {
	var err error
	if st.Status != nil {
		err = PFRemoteWriteStatus(rc.env.meta, *st.Status)
	} else {
		err = PFRemoteWriteMacStatus(rc.env.meta, *st.MacStatus)
	}
	if err != nil {
		return pfrErr("写状态文件失败: %v", err)
	}
	return PFRemoteStepResult{}
}

// unitMkdir 建单元目录本身（见文件头「单元目录」）。
func (rc *pfrApplyRun) unitMkdir() PFRemoteStepResult {
	rel, code := rc.req.Rel, rc.req.Code
	res := PFApplyResult{Rel: rel}
	if rc.unit != nil {
		res.Status, res.Code, res.Existed = PFApplyDone, PFApplyCodeOK, true
		return PFRemoteStepResult{Apply: &res}
	}
	segs := strings.Split(rel, "/")
	bad := func(f string, a ...any) PFRemoteStepResult {
		return pfrCfgErr(&PFApplyConfigError{Msg: fmt.Sprintf("建单元目录 %q：", rel) + fmt.Sprintf(f, a...)})
	}
	if len(segs) > PFUnitMaxDepth {
		return bad("层数超过 %d", PFUnitMaxDepth)
	}
	if o := PFCheckPath(rel, true); o != nil {
		return bad("路径不合格：%s", o.Reason)
	}
	// §14.4：req.Code 是单元键；目录名里认的编号 = 键的前段（另配目录单元 CODER2 的目录仍叫编号开头）。
	pc, _ := PFUnitKeyProjectCode(code)
	for i, seg := range segs {
		if strings.HasPrefix(seg, ".") {
			return bad("%q 以 . 开头，发现单元时会被跳过", seg)
		}
		c, ok := PFCodeFromName(seg)
		if i < len(segs)-1 && ok {
			return bad("上级目录 %q 里认得出编号 %s，单元不能建在另一个单元里面", seg, c)
		}
		if i == len(segs)-1 && (!ok || c != pc || !pfIsASCII(c)) {
			return bad("最后一级 %q 认出的编号是 %q，与 %s 的编号 %s 不符", seg, c, code, pc)
		}
	}
	// 同编号多个目录已合法（§14.2），不再「已有同编号目录就不建」；rel 相同的情况 rc.unit 已非空、上面直接回已存在。
	cur := rc.env.root
	created := false
	for _, seg := range segs {
		// 按字节核对父目录里的名字（区分大小写与否的文件系统上结果一样）：有字节相同的就是已存在，
		// 只有折叠键相同的（只差大小写或 NFC/NFD）不建。
		ents, rerr := os.ReadDir(cur)
		if rerr != nil {
			return bad("读目录 %s 失败: %v", cur, rerr)
		}
		exact := false
		for _, e := range ents {
			switch {
			case e.Name() == seg:
				exact = true
			case PFFoldKey(e.Name()) == PFFoldKey(seg):
				return bad("%s 里已有 %q，与要建的 %q 只差大小写或 NFC/NFD", cur, e.Name(), seg)
			}
		}
		next := filepath.Join(cur, seg)
		if exact {
			fi, err := os.Lstat(next)
			if err != nil {
				return bad("读 %s 失败: %v", next, err)
			}
			if !fi.IsDir() {
				return bad("%s 已存在但不是真实目录（符号链接或文件）", next)
			}
		} else {
			if merr := os.Mkdir(next, 0o755); merr != nil {
				return bad("建目录 %s 失败: %v", next, merr)
			}
			_ = pfSyncDir(cur)
			created = true
		}
		cur = next
	}
	res.Status, res.Code, res.Existed = PFApplyDone, PFApplyCodeOK, !created
	rc.unit = &PFFoundUnit{Code: code, Rel: rel}
	return PFRemoteStepResult{Apply: &res}
}
