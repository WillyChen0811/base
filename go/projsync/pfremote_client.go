package projsync

// 项目文件同步 T8：Mac 侧客户端——经 RemoteRunner 调 Ubuntu 的三个 projfiles-* 子命令（remote.go 的 remoteCall 模式）。
//
// 原则：
//   - 通信失败 = 结果不确定。RemoteRunner 报错、退出码非 0、响应不是合法 JSON、响应的字段组合不合法，一律返回
//     errors.Is(err, ErrPFRemoteUncertain) 的错误；客户端不重试（apply 可能已经执行，重放不安全）。
//     调用方停下这个单元本轮后续写入，下一轮重新采集（remote.go 的 RemoteApply 同一原则）。
//   - 对方明确说没做（响应里的 Error：越界、租约在别人手里、单元找不到……）不是通信失败：
//     返回 *PFRemoteError（Kind=PFRemoteErrRefused 或 PFRemoteErrLease），什么都没写。
//   - 请求在本机就不合格（步骤校验、请求过大）不会发出去（PFRemoteErrLocal）。
//   - 响应的每个字段组合都校验：版本、步骤结果与步骤一一对应、停止位置、结果种类与原因代码、安装结果的哈希……
//     对不上就当通信失败，不信半截结果。
//
// 不管：SSH 连接（RemoteRunner 的实现在 wgtund）、判定、传输队列。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// PFRemoteClient 是调用 Ubuntu 的客户端。
type PFRemoteClient struct {
	Runner    RemoteRunner
	XxyiMsg   string // Ubuntu 上 xxyi-msg 的路径（只许 [A-Za-z0-9._/~-]，见 remoteCommand）
	Meta      string // Ubuntu 的 meta 目录（绝对路径）
	Root      string // Ubuntu 的同步根（绝对路径，要在 Ubuntu 的 roots.json 里）
	CoordID   string // 协调者编号（这台 Mac 固定不变）
	CoordHost string // 给人看的主机名
}

// PFRemoteUnit 指定 Ubuntu 上的一个单元。
type PFRemoteUnit struct {
	Code string // 单元键（归一化编号）
	Rel  string // 相对 Ubuntu 根的路径
}

func (c *PFRemoteClient) err(sub string, kind PFRemoteErrKind, msg string, err error) *PFRemoteError {
	return &PFRemoteError{Sub: sub, Kind: kind, Msg: msg, Err: err}
}

// call 发一个请求：本机先量一下大小，再走 remoteCall。runner 层的任何失败都是「结果不确定」。
func (c *PFRemoteClient) call(ctx context.Context, sub string, req, resp any) error {
	if c.Runner == nil {
		return c.err(sub, PFRemoteErrLocal, "没有 RemoteRunner", nil)
	}
	if _, err := remoteCommand(c.XxyiMsg, sub); err != nil {
		return c.err(sub, PFRemoteErrLocal, "xxyi-msg 路径不合规", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return c.err(sub, PFRemoteErrLocal, "请求编码失败", err)
	}
	if len(body) > PFRemoteMaxInput {
		return c.err(sub, PFRemoteErrLocal, fmt.Sprintf("请求有 %d 字节，超过 %d 字节上限，请分批", len(body), PFRemoteMaxInput), nil)
	}
	if err := remoteCall(ctx, c.Runner, c.XxyiMsg, sub, req, resp); err != nil {
		return c.err(sub, PFRemoteErrComm, "调用失败，远端结果不确定", err)
	}
	return nil
}

func (c *PFRemoteClient) bad(sub, why string) *PFRemoteError {
	return c.err(sub, PFRemoteErrComm, "远程响应不合法（远端结果不确定，需重新确认现场）: "+why, nil)
}

// ── poll ────────────────────────────────────────────────

// PFRemotePollOptions 是 poll 的可调部分，零值可用。
type PFRemotePollOptions struct {
	LeaseSeconds int // 缺省 600
	QuietSeconds int // 缺省 10
}

// Poll 调 projfiles-poll：续租、列单元与指纹、请求清单、剩余空间。
// 租约在别人手里：返回响应（Lease 里有现任持有者）加 *PFRemoteError（Kind=PFRemoteErrLease，errors.Is(err, ErrPFRemoteLeaseHeld)）；
// 这时响应里没有单元等内容，这台 Mac 不该动 Ubuntu。
func (c *PFRemoteClient) Poll(ctx context.Context, opt PFRemotePollOptions) (PFRemotePollResponse, error) {
	req := PFRemotePollRequest{Version: PFRemoteVersion, Meta: c.Meta, Root: c.Root, CoordID: c.CoordID, CoordHost: c.CoordHost,
		LeaseSeconds: opt.LeaseSeconds, QuietSeconds: opt.QuietSeconds}
	var resp PFRemotePollResponse
	if err := c.call(ctx, PFRemoteSubPoll, req, &resp); err != nil {
		return PFRemotePollResponse{}, err
	}
	if resp.Version != PFRemoteVersion {
		return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, fmt.Sprintf("协议版本 %d", resp.Version))
	}
	if resp.Error != "" {
		return PFRemotePollResponse{}, c.err(PFRemoteSubPoll, PFRemoteErrRefused, resp.Error, nil)
	}
	if !resp.Lease.Granted {
		if resp.Lease.Reason == "" {
			return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, "没有授予租约却没说原因")
		}
		e := c.err(PFRemoteSubPoll, PFRemoteErrLease, resp.Lease.Reason, nil)
		if resp.Lease.Holder.ID != "" {
			h := resp.Lease.Holder
			e.Holder = &h
		}
		return resp, e
	}
	if resp.Lease.Holder.ID != c.CoordID {
		return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, "授予的租约不是我们的")
	}
	if resp.Units == nil || resp.Requests == nil {
		return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, "缺少 units 或 requests")
	}
	seen := map[string]bool{}
	for _, u := range resp.Units {
		if pfrCheckUnitKey(u.Code) != nil || validRel(u.Rel) != nil {
			return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, fmt.Sprintf("单元 %q（%q）不合法", u.Code, u.Rel))
		}
		if seen[u.Rel] {
			return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, fmt.Sprintf("单元路径 %q 重复", u.Rel))
		}
		seen[u.Rel] = true
		switch {
		case u.Fingerprint != "" && !pfValidSHA(u.Fingerprint):
			return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, fmt.Sprintf("单元 %s 的指纹格式不对", u.Code))
		case u.Fingerprint == "" && !u.Dup && u.Error == "":
			return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, fmt.Sprintf("单元 %s 既没有指纹也没说原因", u.Code))
		case u.Dup && len(u.DupRels) == 0:
			return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, fmt.Sprintf("单元 %s 标了重复却没有对方路径", u.Code))
		}
	}
	for _, r := range resp.Requests {
		if pfrReqNameRe.FindStringSubmatch(r.Name) == nil || r.Request.Validate() != nil {
			return PFRemotePollResponse{}, c.bad(PFRemoteSubPoll, fmt.Sprintf("请求 %q 不合法", r.Name))
		}
	}
	return resp, nil
}

// ── facts ───────────────────────────────────────────────

// PFRemoteFactsOptions 是 facts 的可调部分，零值可用。
type PFRemoteFactsOptions struct {
	QuietSeconds int           // 缺省 10
	HashBudget   time.Duration // 每一页的哈希预算，缺省 60 秒，最大 100 秒
	MaxItems     int           // 只能调小，缺省 50000
	PageBytes    int           // 缺省 4MB
}

// PFRemoteFacts 是一个单元在 Ubuntu 上的事实（几页拼起来的完整结果）。
type PFRemoteFacts struct {
	// Exists=false：Rel 处没有这个编号的单元（Ubuntu 上单元目录不存在，或那里认不出这个编号）。
	// Found 是同编号的其他目录（G4）；Incomplete 为真说明发现不完整，「没有」不可信，不能据此判定单元不见了。
	Exists     bool
	Found      []string
	Incomplete bool

	UnitPath string  // 单元目录在 Ubuntu 上的绝对路径
	DirID    PFDirID // 单元目录 dev+ino（G2）
	Dup      bool    // G4：同编号多个目录
	DupRels  []string

	// TooLarge：条目数超过上限（§4.2 第 12 条），整个单元超出范围；Scan、Names 为空。
	TooLarge    bool
	Fingerprint string
	TotalItems  int
	TotalNames  int
	Summary     PFRemoteSummary

	Scan  *PFScanResult // 已带哈希；Root 是 Ubuntu 上单元目录的路径；Items 按路径字节序
	Names []PFRawName   // 被排除的名字（含 mtime），给判定层的 UbuntuNames
	// NowNs 是 Ubuntu 回应时的时钟（Unix 纳秒），算两边时钟差（G6）：skew = Mac 的钟 − NowNs。
	NowNs     int64
	HashStats PFHashStats // 各页的合计（Elapsed 是各页耗时之和）
	Pages     int
	Notes     []string
}

// Facts 调 projfiles-facts 并把几页拼成完整结果。拼好后核对：几页的指纹、条目总数、目录 ID 一致，
// 且用收到的条目重算的指纹等于对方报的（没有转义路径时）；对不上返回 ErrPFRemoteUnstable（单元在变，没写过东西，本轮跳过这个单元）。
func (c *PFRemoteClient) Facts(ctx context.Context, u PFRemoteUnit, opt PFRemoteFactsOptions) (*PFRemoteFacts, error) {
	const sub = PFRemoteSubFacts
	req := PFRemoteFactsRequest{Version: PFRemoteVersion, Meta: c.Meta, Root: c.Root, Code: u.Code, Rel: u.Rel,
		QuietSeconds: opt.QuietSeconds, HashBudgetMs: int(opt.HashBudget / time.Millisecond), MaxItems: opt.MaxItems, PageBytes: opt.PageBytes}
	if err := pfrCheckUnitKey(u.Code); err != nil {
		return nil, c.err(sub, PFRemoteErrLocal, err.Error(), nil)
	}
	if err := pfrCheckFactsRequest(&req); err != nil {
		return nil, c.err(sub, PFRemoteErrLocal, err.Error(), nil)
	}
	var out *PFRemoteFacts
	var items []PFScanItem
	var names []PFRawName
	escaped := 0
	for page := 0; ; page++ {
		if page >= pfrMaxPages {
			return nil, c.bad(sub, fmt.Sprintf("翻了 %d 页还没完，游标没有前进", pfrMaxPages))
		}
		var resp PFRemoteFactsResponse
		if err := c.call(ctx, sub, req, &resp); err != nil {
			return nil, err
		}
		if resp.Version != PFRemoteVersion {
			return nil, c.bad(sub, fmt.Sprintf("协议版本 %d", resp.Version))
		}
		if resp.Error != "" {
			if page > 0 {
				return nil, c.err(sub, PFRemoteErrUnstable, "第 "+fmt.Sprint(page+1)+" 页采集失败: "+resp.Error, nil)
			}
			return nil, c.err(sub, PFRemoteErrRefused, resp.Error, nil)
		}
		if page == 0 {
			out = &PFRemoteFacts{Exists: resp.Exists, Found: resp.Found, Incomplete: resp.Incomplete, UnitPath: resp.UnitPath,
				DirID: resp.DirID, Dup: resp.Dup, DupRels: resp.DupRels, TooLarge: resp.TooLarge, Fingerprint: resp.Fingerprint,
				TotalItems: resp.TotalItems, TotalNames: resp.TotalNames, Summary: resp.Summary, NowNs: resp.NowNs}
			if !resp.Exists {
				if resp.Fingerprint != "" || len(resp.Items) > 0 || resp.More || resp.UnitPath != "" || resp.Dup {
					return nil, c.bad(sub, "单元不存在却带着内容")
				}
				return out, nil
			}
			if !pfValidSHA(resp.Fingerprint) {
				return nil, c.bad(sub, "缺少合法的指纹")
			}
			if resp.UnitPath == "" || (resp.Dup && len(resp.DupRels) == 0) {
				return nil, c.bad(sub, "缺少单元路径，或标了重复却没有对方路径")
			}
			if resp.TooLarge {
				if len(resp.Items) > 0 || resp.More {
					return nil, c.bad(sub, "标了单元过大却带着内容")
				}
				out.Pages = 1
				return out, nil
			}
		} else if !resp.Exists || resp.Fingerprint != out.Fingerprint || resp.TotalItems != out.TotalItems ||
			resp.DirID != out.DirID || resp.Dup != out.Dup || resp.TooLarge != out.TooLarge || resp.TotalNames != out.TotalNames {
			return nil, c.err(sub, PFRemoteErrUnstable, "几页的指纹、条目数或目录 ID 对不上", nil)
		}
		if page == 0 {
			out.Scan = &PFScanResult{Root: resp.UnitPath, Excluded: resp.Excluded, ScannedAt: time.Unix(0, resp.ScannedAtNs)}
		}
		for _, w := range resp.Items {
			it, err := pfrFromWire(w, out.DirID.Dev)
			if err != nil {
				return nil, c.bad(sub, err.Error())
			}
			items = append(items, it)
		}
		names = append(names, resp.Names...)
		escaped += resp.Escaped
		out.HashStats.CacheHits += resp.HashStats.CacheHits
		out.HashStats.Hashed += resp.HashStats.Hashed
		out.HashStats.HashedBytes += resp.HashStats.HashedBytes
		out.HashStats.NewUnknown += resp.HashStats.NewUnknown
		out.HashStats.Elapsed += time.Duration(resp.HashStats.ElapsedMs) * time.Millisecond
		out.Notes = append(out.Notes, resp.Notes...)
		out.Pages = page + 1
		if !resp.More {
			break
		}
		// 游标必须前进，且不能越过总数
		if resp.NextItem < req.AfterItem || resp.NextName < req.AfterName || (resp.NextItem == req.AfterItem && resp.NextName == req.AfterName) ||
			resp.NextItem != len(items) || resp.NextName != len(names) {
			return nil, c.bad(sub, "翻页游标没有前进或与收到的内容对不上")
		}
		req.AfterItem, req.AfterName = resp.NextItem, resp.NextName
	}
	if len(items) != out.TotalItems || len(names) != out.TotalNames {
		return nil, c.err(sub, PFRemoteErrUnstable, fmt.Sprintf("收到 %d 个条目、%d 个名字，与总数 %d、%d 对不上", len(items), len(names), out.TotalItems, out.TotalNames), nil)
	}
	out.Scan.Items = items
	if escaped == 0 {
		if !sort.SliceIsSorted(items, func(i, j int) bool { return items[i].Path < items[j].Path }) {
			return nil, c.bad(sub, "条目不是按路径排序的")
		}
		for i := 1; i < len(items); i++ {
			if items[i].Path == items[i-1].Path {
				return nil, c.bad(sub, fmt.Sprintf("条目 %q 重复", items[i].Path))
			}
		}
		if fp := out.Scan.Fingerprint(); fp != out.Fingerprint {
			return nil, c.err(sub, PFRemoteErrUnstable, "用收到的条目重算的指纹与对方报的不同（几页之间单元变了，或传输出错）", nil)
		}
	}
	out.Names = names
	out.Scan.HashStats = out.HashStats
	out.Summary = pfrSummary(out.Scan) // 哈希之后的计数（服务端头部的计数是哈希之前的）
	return out, nil
}

// ── apply ───────────────────────────────────────────────

// PFRemoteApplyOptions 是 apply 的批级参数。
type PFRemoteApplyOptions struct {
	Unit       PFRemoteUnit // 要动单元目录的步骤需要；只有 verify_part 等不动单元的批可以留空
	Round      string       // 轮次号，写进回收记录
	StopOnFail bool         // 任何一步没完成就停（有先后依赖的几步用）
}

// PFRemoteApplyResult 是一批步骤的执行结果。
type PFRemoteApplyResult struct {
	Results    []PFRemoteStepResult // 已执行的步骤的结果，与请求的前 len(Results) 步一一对应
	Stopped    bool                 // 是否中途停止；停止时后面的步骤没有执行
	StopAt     int                  // 停止的那一步的下标（它的结果是 Results 的最后一项）
	StopReason string
	NotRun     int // 没执行的步数
}

var pfrCodeRe = regexp.MustCompile(`^[a-z_]+$`)

// Apply 调 projfiles-apply 执行一批步骤。返回的 error：
//   - 通信类（errors.Is(err, ErrPFRemoteUncertain)）：远端可能已经执行了一部分，结果不确定，调用方停下这个单元本轮后续写入、
//     下一轮重新采集，不得重试；
//   - *PFRemoteError 其它类：对方整批拒绝（Refused/Lease）或本机校验不过（Local），一步都没执行。
//
// 没有 error 时检查 Stopped：为真说明批在中途停下（放弃且不确定、配置错误等），后面的步骤没有执行。
func (c *PFRemoteClient) Apply(ctx context.Context, opt PFRemoteApplyOptions, steps []PFRemoteStep) (*PFRemoteApplyResult, error) {
	const sub = PFRemoteSubApply
	req := PFRemoteApplyRequest{Version: PFRemoteVersion, Meta: c.Meta, Root: c.Root, CoordID: c.CoordID,
		Code: opt.Unit.Code, Rel: opt.Unit.Rel, Round: opt.Round, StopOnFail: opt.StopOnFail, Steps: steps}
	if err := pfrValidateApplyRequest(&req); err != nil {
		return nil, c.err(sub, PFRemoteErrLocal, err.Error(), nil)
	}
	var resp PFRemoteApplyResponse
	if err := c.call(ctx, sub, req, &resp); err != nil {
		return nil, err
	}
	if resp.Version != PFRemoteVersion {
		return nil, c.bad(sub, fmt.Sprintf("协议版本 %d", resp.Version))
	}
	if resp.Error != "" {
		if len(resp.Results) != 0 || resp.Stopped {
			return nil, c.bad(sub, "整批拒绝却带着执行结果")
		}
		return nil, c.err(sub, PFRemoteErrRefused, resp.Error, nil)
	}
	if err := pfrCheckApplyResponse(steps, &resp); err != nil {
		return nil, c.bad(sub, err.Error())
	}
	return &PFRemoteApplyResult{Results: resp.Results, Stopped: resp.Stopped, StopAt: resp.StopAt, StopReason: resp.StopReason,
		NotRun: len(steps) - len(resp.Results)}, nil
}

// pfrCheckApplyResponse 核对响应与请求的步骤一一对应、字段组合合法。
func pfrCheckApplyResponse(steps []PFRemoteStep, resp *PFRemoteApplyResponse) error {
	n := len(resp.Results)
	if n > len(steps) {
		return fmt.Errorf("结果有 %d 项，比步骤（%d）多", n, len(steps))
	}
	if resp.Stopped {
		if resp.StopAt != n-1 || resp.StopReason == "" {
			return fmt.Errorf("标了中途停止，但停止位置 %d 与结果数 %d 对不上，或没说原因", resp.StopAt, n)
		}
	} else if n != len(steps) || resp.StopAt != 0 || resp.StopReason != "" {
		return fmt.Errorf("没有中途停止，结果却是 %d 项（步骤 %d 个），或带着停止信息", n, len(steps))
	}
	for i := range resp.Results {
		r, st := &resp.Results[i], &steps[i]
		if r.Op != st.Op {
			return fmt.Errorf("第 %d 项结果的种类 %q 与步骤 %q 不同", i+1, r.Op, st.Op)
		}
		if err := pfrCheckStepResult(st, r); err != nil {
			return fmt.Errorf("第 %d 项（%s）：%w", i+1, st.Op, err)
		}
	}
	return nil
}

func pfrCheckStepResult(st *PFRemoteStep, r *PFRemoteStepResult) error {
	if r.Config && r.Error == "" {
		return errors.New("标了配置错误却没说原因")
	}
	if r.Error != "" {
		if r.Apply != nil || r.Sum != "" || r.Clean != nil || len(r.Taken) > 0 {
			return errors.New("出错的步骤不该带结果")
		}
		return nil
	}
	if _, isExt := pfrExtOps[st.Op]; isExt {
		return nil
	}
	switch st.Op {
	case PFRemoteOpVerifyPart:
		if !pfValidSHA(r.Sum) || r.Size < 0 {
			return errors.New("缺少合法的 sum 或 size")
		}
		return nil
	case PFRemoteOpWriteStatus:
		return nil
	case PFRemoteOpTakeRequests:
		if len(r.Taken)+len(r.Missing)+len(r.Bad) != len(st.Names) {
			return errors.New("取走、不在、读不懂的文件数加起来不等于请求的个数")
		}
		return nil
	case PFRemoteOpRecycleClean, PFRemoteOpStagingClean:
		if r.Clean == nil {
			return errors.New("缺少清理结果")
		}
		return nil
	}
	// 写原语类
	a := r.Apply
	if a == nil {
		return errors.New("缺少写原语的结果")
	}
	switch a.Status {
	case PFApplyDone:
		if a.Code != PFApplyCodeOK || a.Uncertain {
			return errors.New("完成的结果原因代码应是 ok、且不能是不确定")
		}
		switch st.Op {
		case PFRemoteOpInstallNew, PFRemoteOpInstallOver:
			if a.Installed == nil || a.Installed.SHA256 != st.SHA256 {
				return errors.New("安装完成却缺少安装结果，或哈希与 part 的不同")
			}
		case PFRemoteOpCopyNew, PFRemoteOpCopyOver:
			if a.Installed == nil || a.Installed.SHA256 != st.Src.SHA256 {
				return errors.New("复制安装完成却缺少安装结果，或哈希与来源的不同")
			}
		case PFRemoteOpRename, PFRemoteOpConflictName:
			if a.Installed == nil || a.Installed.SHA256 != st.Expect.SHA256 {
				return errors.New("改名完成却缺少结果，或哈希与前提的不同")
			}
		case PFRemoteOpDelete:
			if a.Recycled == nil || a.Recycled.SHA256 != st.Expect.SHA256 {
				return errors.New("删除完成却缺少回收记录，或哈希与前提的不同")
			}
		}
	case PFApplyAbandoned, PFApplyDeferred, PFApplyOutOfScope:
		if a.Code == "" || a.Code == PFApplyCodeOK || !pfrCodeRe.MatchString(a.Code) {
			return fmt.Errorf("没完成的结果缺少合法的原因代码（%q）", a.Code)
		}
		if a.Uncertain && a.Status != PFApplyAbandoned {
			return errors.New("只有放弃才可能是不确定")
		}
	default:
		return fmt.Errorf("结果种类 %d 不认识", int(a.Status))
	}
	return nil
}

// ── 目标端核验（给 T7 的 VerifyUp） ───────────────────────

// TargetVerify 返回满足 PFTargetVerify 的函数，给传输层的 VerifyUp 用：partPath 必须是 <Ubuntu 暂存区>/<sha256>.part
// （传输层用 PFRemoteStagingDir(Meta) 拼出来的），函数让 Ubuntu 本机算这个 part 的 sha256 和大小，只返回算出来的值
// （比较由传输层做）。算不出来（通信失败、对方拒绝、文件读不了）一律返回 error，传输层保留 part，不当作不符。
func (c *PFRemoteClient) TargetVerify() PFTargetVerify {
	return func(ctx context.Context, partPath string) (string, int64, error) {
		sha, err := c.partSHA(partPath)
		if err != nil {
			return "", 0, err
		}
		res, err := c.Apply(ctx, PFRemoteApplyOptions{}, []PFRemoteStep{{Op: PFRemoteOpVerifyPart, SHA256: sha}})
		if err != nil {
			return "", 0, err
		}
		if len(res.Results) != 1 {
			return "", 0, c.bad(PFRemoteSubApply, "核验批没有返回一项结果")
		}
		r := res.Results[0]
		if r.Error != "" {
			return "", 0, c.err(PFRemoteSubApply, PFRemoteErrRefused, "Ubuntu 上算 part 的哈希失败: "+r.Error, nil)
		}
		return r.Sum, r.Size, nil
	}
}

// partSHA 从传输层给的 part 路径取出 sha256，同时确认它在 Ubuntu 暂存区里。
func (c *PFRemoteClient) partSHA(partPath string) (string, error) {
	dir, name := filepath.Split(partPath)
	if filepath.Clean(dir) != filepath.Clean(PFRemoteStagingDir(c.Meta)) || !pfrStagingPartRe.MatchString(name) {
		return "", c.err(PFRemoteSubApply, PFRemoteErrLocal, fmt.Sprintf("%q 不是 Ubuntu 暂存区 %s 里的 <sha256>.part", partPath, PFRemoteStagingDir(c.Meta)), nil)
	}
	return name[:64], nil
}
