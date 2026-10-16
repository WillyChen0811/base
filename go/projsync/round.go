package projsync

// 项目同步第一版第③b项：一轮同步的编排（PLAN-sync-v2.md 第二、三、八、九、十一、十四、十五节）。
//
// RunRound 是纯编排：不知道 SSH，只通过 RoundEnv 里的函数/接口做事；所有写动作都复用已过审的原语
// （apply.go / commit.go / remote.go）。流程：
//
//	a) 交换引用（SyncGit） → b) 采集两边事实 + 范围检查 → c) 自动提交（状态确定才提交）
//	→ d) 判定 → e) 执行（同一项目内严格串行） → g) 汇总。
//
// 安全约束（来自第八、九、十一节）：
//   - 互斥按本地 `git rev-parse --git-common-dir` 的规范化路径；手动与定时共用同一把锁，拿不到即跳过；
//   - 通信失败（RemoteFacts/RemoteApply 返回 error）、写后复查失败（ErrCheckedOutDuringUpdate、提交后现场需确认）
//     ——立即停止本项目本轮所有后续写动作，标记「结果不确定，需重新确认」，不重试；
//   - 超出支持范围——写之前完整预检（两边分支名并集的 CheckScope、本轮涉及提交的树 CheckCommitsScope、
//     dirty 工作区当前 HEAD），任一不过则整轮不写任何东西；自动提交后重新采集会再预检一次；
//     若执行中途才因现场变化判超出，停止后续写并如实列出已完成的写入；
//   - 自动提交失败（含钩子拒绝）——记录完整输出，本轮不再写；
//   - AutoMerge：Parents[0]=mac、Parents[1]=ubuntu；先在 Mac 本地生成合并提交（只写对象）并传到 Ubuntu，
//     再分别快进 Mac、Ubuntu（先传后快进），禁止回退；任一步失败，已完成的保留，停止后续写。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 动作结果取值（BranchOutcome.Result）。
const (
	ResultDone      = "done"      // 已执行
	ResultSkipped   = "skipped"   // 本轮不动（冲突、未提交改动挡住、仓库忙等），原因在 Detail
	ResultFailed    = "failed"    // 执行失败，确认未写入
	ResultUncertain = "uncertain" // 结果不确定（通信失败/已写入现场需确认），本项目后续写已停止
	ResultNotRun    = "not_run"   // 因本轮已停止，没有执行
)

// 工作区动作取值（WorktreeOutcome.Action）。
const (
	WtCommitted    = "committed"     // 已自动提交
	WtUncertain    = "uncertain"     // 有未提交的改动，状态不确定，未提交
	WtCommitFailed = "commit_failed" // git commit/add 失败（含钩子拒绝）
	WtCommitError  = "commit_error"  // 提交前检查没过或结果不确定，详情见 Detail
	WtNothing      = "nothing"       // 要提交时发现已无改动
)

// 需要用户处理的事项类别（AttentionItem.Kind）。
const (
	AttConflict      = "conflict"        // 合并冲突，用户手动处理
	AttUncertainDirt = "uncertain_dirty" // 有未提交的改动，状态不确定（界面带「提交并同步」按钮）
	AttCommitFailed  = "commit_failed"   // 自动提交失败，附完整输出
	AttOutOfScope    = "out_of_scope"    // 超出自动同步范围
	AttNeedRecheck   = "need_recheck"    // 结果不确定，需重新确认
	AttApplyFailed   = "apply_failed"    // 执行出错（与真实冲突分开）
)

// ForceCommit 是用户在界面点「提交并同步」指定的工作区：本轮对它 AutoCommit(Force=true)。
type ForceCommit struct {
	Side     Side
	Worktree string
}

// RoundEnv 是一轮同步需要的一切外部依赖。
type RoundEnv struct {
	Project    string // 项目名（只用于结果展示）
	Repo       string // 本地（Mac）仓库顶层绝对路径
	RemoteRepo string // 对方（Ubuntu）仓库绝对路径
	RemoteNS   string // 对方分支在本地的命名空间（refs/remotes/<ns>/*），空则 "ubuntu"
	// Exchange 执行 v1 的引用交换（SyncGit 的闭包）：Mac refs/heads → Ubuntu refs/remotes/mac/*，
	// Ubuntu refs/heads → Mac refs/remotes/<RemoteNS>/*。
	Exchange   func(ctx context.Context) error
	Remote     RemoteRunner
	XxyiMsg    string     // 对方 xxyi-msg 路径
	LocalJudge AgentJudge // 本地 agent 状态判定；nil 一律按 unknown
	Now        func() time.Time
	// Force 是用户点「提交并同步」指定的工作区（可空）。
	Force []ForceCommit
}

// BranchOutcome 是一个分支本轮的动作与结果。两边相同的分支不记录（只计入 Unchanged）。
type BranchOutcome struct {
	Name   string     `json:"name"`
	Action ActionKind `json:"action"`
	Result string     `json:"result"`
	Sides  []Side     `json:"sides,omitempty"`
	// Target：目标提交（AutoMerge 为合并提交，失败时可能为空）。
	Target string `json:"target,omitempty"`
	Detail string `json:"detail"` // 中文原因/说明
}

// WorktreeOutcome 是一个有未提交改动的工作区本轮的处理结果。
type WorktreeOutcome struct {
	Side    Side   `json:"side"`
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Verdict string `json:"verdict"` // agent 结论（finished/busy/unknown/no_agent，或「用户指定」）
	Action  string `json:"action"`
	NewOID  string `json:"new_oid,omitempty"`
	Detail  string `json:"detail"`
}

// AttentionItem 是需要用户处理的事项。
type AttentionItem struct {
	Kind     string `json:"kind"`
	Side     Side   `json:"side,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Message  string `json:"message"`
	Detail   string `json:"detail,omitempty"` // 完整输出/冲突文件等
	// CanCommit：界面可显示「提交并同步」按钮（Side + Worktree 即 commit 接口参数）。
	CanCommit bool `json:"can_commit,omitempty"`
}

// RoundResult 是一轮同步的完整结果。
type RoundResult struct {
	Project  string    `json:"project"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Skipped：没拿到仓库锁（上一轮还在进行），什么都没做。
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	// Error：本轮提前结束的原因（交换失败、采集失败、超出范围等）；空表示正常走完。
	Error string `json:"error,omitempty"`
	// Stopped：本轮在中途停止了后续写动作；StopReason 为首个原因。
	Stopped    bool   `json:"stopped,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	// OutOfScope：本项目超出自动同步范围。预检阶段发现则本轮没有写任何东西；
	// 若执行中途才发现，已完成的写入见 Error/Attention 的说明，并已停止后续写。
	OutOfScope bool `json:"out_of_scope,omitempty"`
	// Recollected：自动提交成功后重新交换与采集过一次。
	Recollected bool              `json:"recollected,omitempty"`
	Branches    []BranchOutcome   `json:"branches"`
	Unchanged   int               `json:"unchanged"` // 两边相同、无事的分支数
	Worktrees   []WorktreeOutcome `json:"worktrees"`
	Attention   []AttentionItem   `json:"attention"`
	// NeedsAttention：Attention 非空。
	NeedsAttention bool     `json:"needs_attention"`
	Notes          []string `json:"notes,omitempty"`
	// Claude：本轮代码同步之后补做的对话记录同步结果（没做则为空）；
	// ClaudeError：对话记录同步失败的原因——只影响这两项，不改变代码同步的结论。
	Claude      *FileResult `json:"claude,omitempty"`
	ClaudeError string      `json:"claude_error,omitempty"`
	Summary     string      `json:"summary"`
}

// tmpMergePrefix 是 AutoMerge 传送合并提交用的临时分支前缀。它只是传送载体，不是用户分支：
// 采集、判定、范围预检时一律忽略（Mac 与 Ubuntu 两侧都忽略），以免中断后的残留被当成新分支同步。
const tmpMergePrefix = "xxyi-tmp-merge-"

func isTmpMerge(name string) bool { return strings.HasPrefix(name, tmpMergePrefix) }

// ── 互斥 ──────────────────────────────────────────

var repoLocks = struct {
	sync.Mutex
	held map[string]bool
}{held: map[string]bool{}}

// repoLockKey 返回仓库的规范化 Git common directory（符号链接已解开）。
func repoLockKey(ctx context.Context, repo string) (string, error) {
	out, err := runGit(ctx, repo, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("读取 git common dir 失败: %w", err)
	}
	p := strings.TrimSuffix(string(out), "\n")
	if p == "" {
		return "", errors.New("git common dir 为空")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(repo, p)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p), nil
}

func tryLockRepo(key string) bool {
	repoLocks.Lock()
	defer repoLocks.Unlock()
	if repoLocks.held[key] {
		return false
	}
	repoLocks.held[key] = true
	return true
}

func unlockRepo(key string) {
	repoLocks.Lock()
	delete(repoLocks.held, key)
	repoLocks.Unlock()
}

// ── 一轮 ──────────────────────────────────────────

type roundRun struct {
	env RoundEnv
	res *RoundResult

	localWts   []WorktreeFacts
	localAgent map[string]AgentVerdict
	remote     FactsResponse

	attSeen map[string]bool
	written []string // 本轮已完成的写入（中文描述），超出范围时如实报告
}

func (r *roundRun) wrote(s string) { r.written = append(r.written, s) }

func (r *roundRun) att(a AttentionItem) {
	key := a.Kind + "|" + string(a.Side) + "|" + a.Branch + "|" + a.Worktree
	if a.Kind == AttNeedRecheck || a.Kind == AttApplyFailed || a.Kind == AttCommitFailed {
		key += "|" + a.Message
	}
	if r.attSeen[key] {
		return
	}
	r.attSeen[key] = true
	r.res.Attention = append(r.res.Attention, a)
}

// stop 停止本项目本轮后续的所有写动作。needRecheck 时同时记「结果不确定，需重新确认」。
func (r *roundRun) stop(reason string, needRecheck bool, side Side, branch string) {
	if !r.res.Stopped {
		r.res.Stopped, r.res.StopReason = true, reason
	}
	if needRecheck {
		r.att(AttentionItem{Kind: AttNeedRecheck, Side: side, Branch: branch,
			Message: "结果不确定，需重新确认：" + reason})
	}
}

// RunRound 跑一轮同步。永不 panic 到调用方之外；拿不到仓库锁时返回 Skipped 结果。
func RunRound(ctx context.Context, env RoundEnv) (res RoundResult) {
	now := env.Now
	if now == nil {
		now = time.Now
	}
	res = RoundResult{Project: env.Project, Started: now(), Branches: []BranchOutcome{},
		Worktrees: []WorktreeOutcome{}, Attention: []AttentionItem{}}
	defer func() {
		if p := recover(); p != nil {
			res.Error = fmt.Sprintf("内部错误: %v", p)
			res.Stopped = true
			if res.StopReason == "" {
				res.StopReason = res.Error
			}
			res.Attention = append(res.Attention, AttentionItem{Kind: AttNeedRecheck,
				Message: "结果不确定，需重新确认：同步过程内部出错 " + fmt.Sprint(p)})
		}
		res.Finished = now()
		res.NeedsAttention = len(res.Attention) > 0
		res.Summary = summarize(&res)
	}()

	if env.RemoteNS == "" {
		env.RemoteNS = "ubuntu"
	}
	if env.Exchange == nil || env.Remote == nil || env.Repo == "" || env.RemoteRepo == "" {
		res.Error = "同步环境不完整（缺少仓库路径、交换函数或远端 runner）"
		return
	}
	key, err := repoLockKey(ctx, env.Repo)
	if err != nil {
		res.Error = err.Error()
		return
	}
	if !tryLockRepo(key) {
		res.Skipped, res.SkipReason = true, "上一轮还在进行"
		return
	}
	defer unlockRepo(key)

	r := &roundRun{env: env, res: &res, attSeen: map[string]bool{}}
	r.run(ctx)
	return
}

// cleanupTmpMerge 清理 Mac 本地遗留的临时分支（上一轮在删除前被中断）：只删引用（带旧值），不碰工作区；
// 正被某个工作区检出的不删，记 needs_attention。结果写进 Notes。
func (r *roundRun) cleanupTmpMerge(ctx context.Context) {
	out, err := runGit(ctx, r.env.Repo, nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/"+tmpMergePrefix+"*")
	if err != nil {
		r.res.Notes = append(r.res.Notes, "检查遗留临时分支失败: "+err.Error())
		return
	}
	lines := nameLines(out)
	if len(lines) == 0 {
		return
	}
	checked := map[string]bool{}
	if wo, err := runGit(ctx, r.env.Repo, nil, "worktree", "list", "--porcelain"); err == nil {
		for _, l := range strings.Split(string(wo), "\n") {
			if v, ok := strings.CutPrefix(l, "branch "); ok {
				checked[v] = true
			}
		}
	} else {
		r.res.Notes = append(r.res.Notes, "无法列出工作区，遗留临时分支未清理: "+err.Error())
		return
	}
	for _, line := range lines {
		oid, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if checked[ref] {
			r.res.Notes = append(r.res.Notes, "遗留临时分支 "+ref+" 正被工作区检出，未删除")
			r.att(AttentionItem{Kind: AttApplyFailed, Side: SideMac, Branch: strings.TrimPrefix(ref, "refs/heads/"),
				Message: "遗留的临时分支 " + ref + " 正被工作区检出，未清理，请手动处理"})
			continue
		}
		if _, err := runGit(ctx, r.env.Repo, nil, "update-ref", "-d", ref, oid); err != nil {
			r.res.Notes = append(r.res.Notes, "删除遗留临时分支 "+ref+" 失败: "+err.Error())
			continue
		}
		r.res.Notes = append(r.res.Notes, "已清理遗留临时分支 "+ref)
	}
}

func (r *roundRun) run(ctx context.Context) {
	r.cleanupTmpMerge(ctx)
	// a) 交换引用 b) 采集
	if !r.exchangeAndCollect(ctx) {
		return
	}
	// 写之前预检：dirty 工作区的当前 HEAD 提交树（自动提交会基于它们）。
	if !r.checkDirtyHeads(ctx) {
		return
	}
	// c) 自动提交
	committed := r.commitStep(ctx)
	if r.res.Stopped {
		return
	}
	if committed {
		// 有提交成功：重新交换与采集一次（只此一次，不再提交，防循环）
		r.res.Recollected = true
		if !r.exchangeAndCollect(ctx) {
			return
		}
		r.noteLeftoverDirty()
	}
	// d)(e) 判定并执行
	r.decideAndApply(ctx)
}

// checkDirtyHeads 对两边所有 dirty 工作区的当前 HEAD 调用提交树预检。
func (r *roundRun) checkDirtyHeads(ctx context.Context) bool {
	var oids []string
	for _, d := range r.dirtyWorktrees() {
		if d.branch == "" {
			continue
		}
		if head, err := r.expectHead(ctx, d); err == nil {
			oids = append(oids, head) // 读不到的交给 commitStep 记为状态不确定
		}
	}
	return r.checkCommits(ctx, oids...)
}

func (r *roundRun) exchangeAndCollect(ctx context.Context) bool {
	if err := r.env.Exchange(ctx); err != nil {
		r.res.Error = "交换引用失败，本轮结束: " + err.Error()
		return false
	}
	return r.collect(ctx)
}

// collect 采集两边工作区与 agent 结论，并对两侧做范围检查。返回 false 表示本轮应结束（原因已记录）。
func (r *roundRun) collect(ctx context.Context) bool {
	wts, err := CollectLocalWorktrees(ctx, r.env.Repo, nil, SideMac, nil)
	if err != nil {
		r.res.Error = "采集本地工作区失败，本轮结束: " + err.Error()
		return false
	}
	r.localWts = wts
	r.localAgent = map[string]AgentVerdict{}
	for _, w := range wts {
		if !w.Dirty {
			continue
		}
		av := AgentVerdict{Verdict: "unknown", Notes: []string{"未提供本地 agent 判定"}}
		if r.env.LocalJudge != nil {
			av = r.env.LocalJudge(ctx, w.Path)
		}
		r.localAgent[w.Path] = av
	}
	rf, err := RemoteFacts(ctx, r.env.Remote, r.env.XxyiMsg, FactsRequest{Repo: r.env.RemoteRepo, Side: SideUbuntu})
	if err != nil {
		r.res.Error = "采集 Ubuntu 事实失败: " + err.Error()
		r.stop("与 Ubuntu 通信失败（"+err.Error()+"）", true, SideUbuntu, "")
		return false
	}
	r.remote = rf
	if rf.ScopeError != "" {
		r.outOfScope(SideUbuntu, rf.ScopeError, rf.ScopeReasons)
		return false
	}
	// 两边分支名并集（Mac refs/heads + Ubuntu refs/heads）：本地 CheckScope 同时覆盖 Mac 独有分支的配置
	// （mergeOptions/rebase）与将要在 Mac 创建的 Ubuntu 独有分支名；Ubuntu 一侧已由 rf.ScopeError 覆盖。
	localNames, err := listLocalBranches(ctx, r.env.Repo, nil)
	if err != nil {
		r.res.Error = "列出本地分支失败，本轮结束: " + err.Error()
		return false
	}
	seen := map[string]bool{}
	var names []string
	for _, n := range localNames {
		if isTmpMerge(n) {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	for n := range rf.Branches {
		if !seen[n] && !isTmpMerge(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if err := CheckScope(ctx, r.env.Repo, nil, names); err != nil {
		var oos *ErrOutOfScope
		if errors.As(err, &oos) {
			r.outOfScope(SideMac, oos.Error(), oos.Reasons)
		} else {
			r.res.Error = "本地范围检查失败，本轮结束: " + err.Error()
		}
		return false
	}
	return true
}

// checkCommits 预检本轮涉及的提交树（含子模块即超出范围）。oids 可含空串与重复。返回 false 表示本轮应结束。
func (r *roundRun) checkCommits(ctx context.Context, oids ...string) bool {
	seen := map[string]bool{}
	var list []string
	for _, o := range oids {
		if o != "" && !seen[o] {
			seen[o] = true
			list = append(list, o)
		}
	}
	err := CheckCommitsScope(ctx, r.env.Repo, nil, list...)
	if err == nil {
		return true
	}
	var oos *ErrOutOfScope
	if errors.As(err, &oos) {
		r.outOfScope("", oos.Error(), oos.Reasons)
	} else {
		r.res.Error = "提交树预检失败，本轮结束: " + err.Error()
		r.stop("提交树预检失败", false, "", "")
	}
	return false
}

// outOfScope 记录超出范围。尚无写入则「本轮不写任何东西」；已有写入（自动提交、已执行的分支）则停止后续写，
// 并如实列出已完成的写入。side 为空表示不归属某一侧（提交树预检）。
func (r *roundRun) outOfScope(side Side, msg string, reasons []string) {
	r.res.OutOfScope = true
	who := ""
	if side != "" {
		who = sideNames([]Side{side}) + "侧"
	}
	var text string
	if len(r.written) == 0 {
		r.res.Error = "超出自动同步范围，本轮不写任何东西"
		text = who + "超出自动同步范围，本轮不写任何东西："
	} else {
		done := strings.Join(r.written, "；")
		r.res.Error = "超出自动同步范围，已停止后续写；本轮已完成的写入：" + done
		text = who + "超出自动同步范围，已停止后续写（本轮已完成的写入：" + done + "）："
		r.stop("超出自动同步范围", false, side, "")
	}
	r.att(AttentionItem{Kind: AttOutOfScope, Side: side, Message: text + strings.Join(reasons, "；"), Detail: msg})
}

// ── c) 自动提交 ──

type dirtyWt struct {
	side    Side
	path    string
	branch  string
	verdict AgentVerdict
}

func (r *roundRun) dirtyWorktrees() []dirtyWt {
	var out []dirtyWt
	for _, w := range r.localWts {
		if w.Dirty {
			out = append(out, dirtyWt{SideMac, w.Path, w.Branch, r.localAgent[w.Path]})
		}
	}
	for _, w := range r.remote.Worktrees {
		if w.Dirty {
			out = append(out, dirtyWt{SideUbuntu, w.Path, w.Branch, r.remote.Agents[w.Path]})
		}
	}
	return out
}

func (r *roundRun) forced(side Side, path string) bool {
	for _, f := range r.env.Force {
		if f.Side == side && filepath.Clean(f.Worktree) == filepath.Clean(path) {
			return true
		}
	}
	return false
}

func (r *roundRun) wtOutcome(d dirtyWt, force bool, action, detail, oid string) {
	v := d.verdict.Verdict
	if v == "" {
		v = "unknown"
	}
	if force {
		v = "用户指定"
	}
	r.res.Worktrees = append(r.res.Worktrees, WorktreeOutcome{Side: d.side, Path: d.path, Branch: d.branch,
		Verdict: v, Action: action, NewOID: oid, Detail: detail})
}

func (r *roundRun) uncertainDirty(d dirtyWt, why string) {
	msg := fmt.Sprintf("%s工作区 %s（分支 %s）有未提交的改动，状态不确定", sideNames([]Side{d.side}), d.path, d.branch)
	r.att(AttentionItem{Kind: AttUncertainDirt, Side: d.side, Branch: d.branch, Worktree: d.path,
		Message: msg, Detail: why, CanCommit: true})
}

// commitStep 对两边每个 dirty 工作区：agent 结论 Finished（或用户指定）→ AutoCommit，其它 → 记「状态不确定」。
// 返回是否有提交成功。
func (r *roundRun) commitStep(ctx context.Context) bool {
	committed := false
	dirty := r.dirtyWorktrees()
	forceHit := map[int]bool{}
	for i, d := range dirty {
		forceHit[i] = r.forced(d.side, d.path)
	}
	for _, f := range r.env.Force {
		found := false
		for _, d := range dirty {
			if d.side == f.Side && filepath.Clean(d.path) == filepath.Clean(f.Worktree) {
				found = true
			}
		}
		if !found {
			r.res.Notes = append(r.res.Notes, fmt.Sprintf("指定提交的%s工作区 %s 当前没有未提交改动或不在本仓库工作区列表中，未提交",
				sideNames([]Side{f.Side}), f.Worktree))
		}
	}
	for i, d := range dirty {
		if r.res.Stopped {
			break
		}
		force := forceHit[i]
		why := strings.Join(d.verdict.Notes, "；")
		if !force && d.verdict.Verdict != "finished" {
			r.wtOutcome(d, false, WtUncertain, fmt.Sprintf("有未提交的改动，状态不确定（agent 结论 %s），未提交", orStr(d.verdict.Verdict, "unknown")), "")
			r.uncertainDirty(d, fmt.Sprintf("agent 结论 %s：%s", orStr(d.verdict.Verdict, "unknown"), why))
			continue
		}
		if d.branch == "" {
			r.wtOutcome(d, force, WtUncertain, "工作区 HEAD 不是分支（游离），无法自动提交", "")
			r.uncertainDirty(d, "工作区 HEAD 不是分支（游离），无法自动提交")
			continue
		}
		head, err := r.expectHead(ctx, d)
		if err != nil {
			r.wtOutcome(d, force, WtUncertain, "读取工作区 HEAD 失败，未提交: "+err.Error(), "")
			r.uncertainDirty(d, "读取工作区 HEAD 失败: "+err.Error())
			continue
		}
		oid, ok := r.doCommit(ctx, d, head, force)
		if ok {
			committed = true
			r.wrote(fmt.Sprintf("%s工作区 %s 自动提交 %s", sideNames([]Side{d.side}), d.path, short(oid)))
			r.wtOutcome(d, force, WtCommitted, "已自动提交 "+oid, oid)
		}
	}
	return committed
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (r *roundRun) expectHead(ctx context.Context, d dirtyWt) (string, error) {
	if d.side == SideMac {
		out, err := runGit(ctx, d.path, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	oid := r.remote.Branches[d.branch]
	if oid == "" {
		return "", fmt.Errorf("分支 %s 在 Ubuntu 上没有对应提交（可能尚未有提交）", d.branch)
	}
	return oid, nil
}

// doCommit 执行一次提交并处理各种失败；返回新提交与是否成功。失败时已记录 outcome/attention 并按需停止。
func (r *roundRun) doCommit(ctx context.Context, d dirtyWt, head string, force bool) (string, bool) {
	plan := CommitPlan{RepoDir: r.env.Repo, Worktree: d.path, Branch: d.branch, ExpectHead: head, Force: force}
	var (
		oid string
		err error
	)
	if d.side == SideMac {
		judge := func(ctx context.Context, wt string) (bool, string) {
			if r.env.LocalJudge == nil {
				return false, "未提供本地 agent 判定"
			}
			av := r.env.LocalJudge(ctx, wt)
			return av.Verdict == "finished", fmt.Sprintf("agent 状态 %s；%s", av.Verdict, strings.Join(av.Notes, "；"))
		}
		oid, err = AutoCommit(ctx, plan, judge)
	} else {
		plan.RepoDir = r.env.RemoteRepo
		var resp ApplyResponse
		resp, err = RemoteApply(ctx, r.env.Remote, r.env.XxyiMsg, ApplyRequest{Repo: r.env.RemoteRepo, Kind: RequestAutoCommit, Commit: &plan})
		if err != nil {
			// 通信失败/响应不合法：远端可能已提交，结果不确定
			r.wtOutcome(d, force, WtCommitError, "提交结果不确定: "+err.Error(), "")
			r.stop("Ubuntu 自动提交时通信失败（"+err.Error()+"）", true, SideUbuntu, d.branch)
			return "", false
		}
		if resp.OK {
			return resp.NewOID, true
		}
		err = &remoteApplyError{kind: resp.ErrKind, msg: resp.Error}
	}
	if err == nil {
		return oid, true
	}
	r.handleCommitErr(d, force, err)
	return "", false
}

// remoteApplyError 把远端业务失败还原成本地错误分类可识别的形式。
type remoteApplyError struct{ kind, msg string }

func (e *remoteApplyError) Error() string { return e.msg }

func (r *roundRun) handleCommitErr(d dirtyWt, force bool, err error) {
	kind := ""
	var rae *remoteApplyError
	var cf *ErrCommitFailed
	var oos *ErrOutOfScope
	var co *ErrCheckedOutDuringUpdate
	switch {
	case errors.As(err, &rae):
		kind = rae.kind
	case errors.Is(err, ErrNotFinished):
		kind = ErrKindNotFinished
	case errors.Is(err, ErrNothingToCommit):
		kind = ErrKindNothingToCommit
	case errors.As(err, &cf):
		kind = ErrKindCommitFailed
	case errors.Is(err, ErrCommittedUncertain):
		kind = ErrKindCommittedUncertain
	case errors.As(err, &oos):
		kind = ErrKindOutOfScope
	case errors.As(err, &co):
		kind = ErrKindCheckedOut
	}
	text := err.Error()
	switch kind {
	case ErrKindNothingToCommit:
		r.wtOutcome(d, force, WtNothing, "已没有未提交的改动", "")
	case ErrKindNotFinished:
		r.wtOutcome(d, force, WtUncertain, "有未提交的改动，状态不确定（提交时复核未通过），未提交", "")
		r.uncertainDirty(d, text)
	case ErrKindCommitFailed:
		r.wtOutcome(d, force, WtCommitFailed, text, "")
		r.att(AttentionItem{Kind: AttCommitFailed, Side: d.side, Branch: d.branch, Worktree: d.path,
			Message: fmt.Sprintf("%s工作区 %s（分支 %s）自动提交失败（钩子拒绝或 git 出错），本轮不再写；改动已暂存但未提交", sideNames([]Side{d.side}), d.path, d.branch),
			Detail:  text})
		r.stop("自动提交失败，本轮不再写", false, d.side, d.branch)
	case ErrKindCommittedUncertain, ErrKindCheckedOut:
		r.wtOutcome(d, force, WtCommitError, "已提交、现场需确认: "+text, "")
		r.stop(fmt.Sprintf("%s工作区 %s 已提交、现场需确认（%s）", sideNames([]Side{d.side}), d.path, text), true, d.side, d.branch)
	case ErrKindOutOfScope:
		r.wtOutcome(d, force, WtCommitError, "超出自动同步范围，未提交: "+text, "")
		r.outOfScope(d.side, text, []string{text})
		r.stop("超出自动同步范围", false, d.side, d.branch)
	default: // refs_changed / other：提交前检查没过，没有写入
		r.wtOutcome(d, force, WtCommitError, "提交前检查没通过，未提交: "+text, "")
		r.uncertainDirty(d, "本轮未能自动提交："+text)
	}
}

// noteLeftoverDirty：重新采集后，仍然 dirty 且本轮没处理过的工作区（例如提交后钩子又改了文件），记为状态不确定，不再提交。
func (r *roundRun) noteLeftoverDirty() {
	for _, d := range r.dirtyWorktrees() {
		seen := false
		for _, o := range r.res.Worktrees {
			if o.Side == d.side && o.Path == d.path && (o.Action == WtUncertain || o.Action == WtCommitFailed || o.Action == WtCommitError) {
				seen = true
			}
		}
		if seen {
			continue
		}
		r.wtOutcome(d, false, WtUncertain, "提交后仍有未提交的改动，本轮不再提交", "")
		r.uncertainDirty(d, "提交后仍有未提交的改动，本轮不再提交")
	}
}

// ── d)(e) 判定与执行 ──

func (r *roundRun) decideAndApply(ctx context.Context) {
	bfs, repo, err := CollectBranchFacts(ctx, r.env.Repo, nil, r.env.RemoteNS, r.remote.Worktrees, r.remote.Repo, nil)
	if err != nil {
		r.res.Error = "采集分支事实失败，本轮结束: " + err.Error()
		return
	}
	facts := map[string]BranchFacts{}
	kept := bfs[:0]
	for _, b := range bfs {
		if isTmpMerge(b.Name) {
			continue
		}
		kept = append(kept, b)
	}
	bfs = kept
	for _, b := range bfs {
		facts[b.Name] = b
	}
	acts := DecideBranches(repo, bfs)
	// 写之前（含重新采集后的新事实）再预检一次：所有要写的动作涉及的提交（两边 OID、目标 OID）。
	var oids []string
	for _, act := range acts {
		switch act.Kind {
		case KindCreateOnOther, KindFastForward, KindAutoMerge:
			bf := facts[act.Name]
			oids = append(oids, bf.Mac, bf.Ubuntu, act.Target)
		}
	}
	if !r.checkCommits(ctx, oids...) {
		return
	}
	for _, act := range acts {
		bf := facts[act.Name]
		switch act.Kind {
		case KindNothing:
			r.res.Unchanged++
		case KindCreateOnOther, KindFastForward:
			r.execSimple(ctx, act, bf)
		case KindAutoMerge:
			r.execMerge(ctx, act, bf)
		case KindConflict:
			r.addBranch(act, ResultSkipped, act.Reason)
			r.att(AttentionItem{Kind: AttConflict, Branch: act.Name,
				Message: fmt.Sprintf("分支 %s 两边分叉且有冲突，两边都没有改动，请手动处理（Mac %s，Ubuntu %s）", act.Name, short(bf.Mac), short(bf.Ubuntu)),
				Detail:  strings.Join(bf.ConflictFiles, "\n")})
		case KindWaitDirty, KindBlockedDirty:
			r.addBranch(act, ResultSkipped, "有未提交的改动，状态不确定，本轮跳过该分支："+act.Reason)
		default: // InvalidFacts / SkipRepoBusy
			r.addBranch(act, ResultSkipped, act.Reason)
		}
	}
}

func short(oid string) string {
	if len(oid) > 10 {
		return oid[:10]
	}
	return oid
}

func (r *roundRun) addBranch(act BranchAction, result, detail string) {
	r.res.Branches = append(r.res.Branches, BranchOutcome{Name: act.Name, Action: act.Kind, Result: result,
		Sides: act.Sides, Target: act.Target, Detail: detail})
}

// applyState 是一次分支写动作的结果分类。
type applyState int

const (
	applyOK applyState = iota
	applyFailed
	applyUncertain
)

type applyOutcome struct {
	state  applyState
	detail string
	refs   bool // 引用变化（预期内的竞争，不算执行故障）
	oos    bool
}

// applyOn 在 side 上执行 FF/Create 计划，并把各种失败归类：未写入（failed）或结果不确定（uncertain）。
func (r *roundRun) applyOn(ctx context.Context, side Side, plan ApplyPlan) applyOutcome {
	if side == SideMac {
		plan.RepoDir = r.env.Repo
		var err error
		if plan.Kind == KindCreateOnOther {
			err = CreateBranch(ctx, plan)
		} else {
			err = FastForwardBranch(ctx, plan)
		}
		if err == nil {
			return applyOutcome{state: applyOK}
		}
		var co *ErrCheckedOutDuringUpdate
		if errors.As(err, &co) {
			return applyOutcome{state: applyUncertain, detail: "已写入、现场需确认: " + err.Error()}
		}
		var oos *ErrOutOfScope
		o := applyOutcome{state: applyFailed, detail: err.Error(), refs: errors.Is(err, ErrRefsChanged), oos: errors.As(err, &oos)}
		if o.oos || o.refs {
			return o
		}
		// 其它失败：看实际效果，引用若已是目标值则说明已写入
		oid, ex, rerr := readRef(ctx, plan.RepoDir, nil, "refs/heads/"+plan.Branch)
		if rerr != nil {
			return applyOutcome{state: applyUncertain, detail: "写入失败且无法复查现场: " + err.Error()}
		}
		if ex && oid == plan.NewOID {
			return applyOutcome{state: applyUncertain, detail: "已写入、现场需确认: " + err.Error()}
		}
		return o
	}
	// Ubuntu
	resp, err := RemoteApply(ctx, r.env.Remote, r.env.XxyiMsg, ApplyRequest{Repo: r.env.RemoteRepo, Plan: plan})
	if err != nil {
		return applyOutcome{state: applyUncertain, detail: "与 Ubuntu 通信失败，结果不确定: " + err.Error()}
	}
	if resp.OK {
		return applyOutcome{state: applyOK}
	}
	switch resp.ErrKind {
	case ErrKindCheckedOut, ErrKindCommittedUncertain:
		return applyOutcome{state: applyUncertain, detail: "已写入、现场需确认: " + resp.Error}
	case ErrKindOutOfScope:
		return applyOutcome{state: applyFailed, detail: resp.Error, oos: true}
	case ErrKindRefs:
		return applyOutcome{state: applyFailed, detail: resp.Error, refs: true}
	}
	// other：看实际效果
	rf, ferr := RemoteFacts(ctx, r.env.Remote, r.env.XxyiMsg, FactsRequest{Repo: r.env.RemoteRepo, Side: SideUbuntu})
	if ferr != nil {
		return applyOutcome{state: applyUncertain, detail: "写入失败且无法复查 Ubuntu 现场: " + resp.Error}
	}
	if rf.Branches[plan.Branch] == plan.NewOID {
		return applyOutcome{state: applyUncertain, detail: "已写入、现场需确认: " + resp.Error}
	}
	return applyOutcome{state: applyFailed, detail: resp.Error}
}

// finishApply 把一次写动作的结果记入分支结果与事项，返回是否成功。
func (r *roundRun) finishApply(act BranchAction, side Side, o applyOutcome) bool {
	switch o.state {
	case applyOK:
		return true
	case applyUncertain:
		r.addBranch(act, ResultUncertain, o.detail)
		r.stop(fmt.Sprintf("分支 %s 在%s：%s", act.Name, sideNames([]Side{side}), o.detail), true, side, act.Name)
	default:
		r.addBranch(act, ResultFailed, o.detail)
		switch {
		case o.oos:
			r.outOfScope(side, o.detail, []string{o.detail})
			r.stop("超出自动同步范围", false, side, act.Name)
		case !o.refs:
			r.att(AttentionItem{Kind: AttApplyFailed, Side: side, Branch: act.Name,
				Message: fmt.Sprintf("分支 %s 在%s更新失败（未写入），请查看原因", act.Name, sideNames([]Side{side})), Detail: o.detail})
		}
	}
	return false
}

func (r *roundRun) skipStopped(act BranchAction) bool {
	if r.res.Stopped {
		r.addBranch(act, ResultNotRun, "本轮已停止后续写动作（"+r.res.StopReason+"），未执行")
		return true
	}
	return false
}

func (r *roundRun) execSimple(ctx context.Context, act BranchAction, bf BranchFacts) {
	if r.skipStopped(act) {
		return
	}
	side := act.Sides[0]
	plan := ApplyPlan{Branch: act.Name, NewOID: act.Target, Kind: act.Kind}
	if act.Kind == KindFastForward {
		plan.OldOID = bf.Mac
		if side == SideUbuntu {
			plan.OldOID = bf.Ubuntu
		}
	}
	o := r.applyOn(ctx, side, plan)
	if r.finishApply(act, side, o) {
		r.wrote(fmt.Sprintf("分支 %s 在%s已更新到 %s", act.Name, sideNames([]Side{side}), short(act.Target)))
		r.addBranch(act, ResultDone, act.Reason)
	}
}

func (r *roundRun) execMerge(ctx context.Context, act BranchAction, bf BranchFacts) {
	if r.skipStopped(act) {
		return
	}
	// 1) Mac 本地生成合并提交（只写对象）；Parents[0]=mac、Parents[1]=ubuntu
	merged, err := AutoMergeCommit(ctx, ApplyPlan{RepoDir: r.env.Repo, Branch: act.Name, Kind: KindAutoMerge,
		Parents: [2]string{bf.Mac, bf.Ubuntu}})
	if err != nil {
		var oos *ErrOutOfScope
		r.addBranch(act, ResultFailed, "生成合并提交失败，两边都没有改动: "+err.Error())
		if errors.As(err, &oos) {
			r.outOfScope(SideMac, err.Error(), oos.Reasons)
			r.stop("超出自动同步范围", false, SideMac, act.Name)
		} else {
			r.att(AttentionItem{Kind: AttApplyFailed, Side: SideMac, Branch: act.Name,
				Message: fmt.Sprintf("分支 %s 自动合并失败（两边都没有改动），请查看原因", act.Name), Detail: err.Error()})
		}
		return
	}
	act.Target = merged
	if !r.checkCommits(ctx, merged) { // 合并提交的树也要过一遍（此时分支都没动）
		r.addBranch(act, ResultFailed, "合并提交的树超出自动同步范围，两边分支都未更新")
		return
	}
	// 2) 把合并提交传到 Ubuntu（先传后快进）。v1 的交换只推 refs/heads，所以临时在 Mac 本地建一个专用分支
	//    xxyi-tmp-merge-<合并提交>（指向合并提交，不碰用户分支），交换后合并对象随它落到 Ubuntu 的
	//    refs/remotes/mac/ 下；传完（无论成败）立刻在 Mac 删掉。Ubuntu 上那个远程跟踪引用由下一次交换
	//    （push --prune）自动清掉，不会出现在 Ubuntu 的 refs/heads。
	tmp := "refs/heads/xxyi-tmp-merge-" + merged
	if _, err := runGit(ctx, r.env.Repo, nil, "update-ref", tmp, merged, strings.Repeat("0", len(merged))); err != nil {
		r.addBranch(act, ResultFailed, "建立传送用的临时引用失败，两边分支都未更新: "+err.Error())
		r.att(AttentionItem{Kind: AttApplyFailed, Side: SideMac, Branch: act.Name,
			Message: fmt.Sprintf("分支 %s 自动合并失败（两边分支都未更新），请查看原因", act.Name), Detail: err.Error()})
		r.stop(fmt.Sprintf("分支 %s：无法建立传送用临时引用", act.Name), false, SideMac, act.Name)
		return
	}
	xerr := r.env.Exchange(ctx)
	if _, err := runGit(ctx, r.env.Repo, nil, "update-ref", "-d", tmp, merged); err != nil {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("临时引用 %s 删除失败，请手动删除: %v", tmp, err))
	}
	if xerr != nil {
		r.addBranch(act, ResultFailed, "把合并提交 "+merged+" 传送到 Ubuntu 失败，两边分支都未更新: "+xerr.Error())
		r.stop(fmt.Sprintf("分支 %s：传送合并对象到 Ubuntu 失败（%v），两边分支都未更新", act.Name, xerr), true, SideUbuntu, act.Name)
		return
	}
	// 3) Mac 本地快进到合并提交（旧值=mac 原提交）。对象已安全到达 Ubuntu；Ubuntu 一侧是否真有该对象，
	//    由下面 Ubuntu 快进原语写之前的 requireCommit 复查，缺失则拒绝写入。
	o := r.applyOn(ctx, SideMac, ApplyPlan{Branch: act.Name, OldOID: bf.Mac, NewOID: merged, Kind: KindFastForward})
	if !r.finishApply(act, SideMac, o) {
		if o.state == applyFailed && o.refs { // 竞争：没写入也要让用户知道本轮提前停了
			r.att(AttentionItem{Kind: AttApplyFailed, Side: SideMac, Branch: act.Name,
				Message: fmt.Sprintf("分支 %s 在 Mac 更新时现场已变化（未写入），本轮停止后续写", act.Name), Detail: o.detail})
		}
		r.stop(fmt.Sprintf("分支 %s 的合并在 Mac 一侧未完成", act.Name), false, SideMac, act.Name)
		return
	}
	r.wrote(fmt.Sprintf("分支 %s 在 Mac 已快进到合并提交 %s", act.Name, short(merged)))
	// 4) Ubuntu 快进到合并提交（旧值=ubuntu 原提交）
	o = r.applyOn(ctx, SideUbuntu, ApplyPlan{Branch: act.Name, OldOID: bf.Ubuntu, NewOID: merged, Kind: KindFastForward})
	switch o.state {
	case applyOK:
		r.wrote(fmt.Sprintf("分支 %s 在 Ubuntu 已快进到合并提交 %s", act.Name, short(merged)))
		r.addBranch(act, ResultDone, fmt.Sprintf("两边分叉但可干净合并，已生成合并提交 %s（父提交：mac %s、ubuntu %s），两边都已更新", merged, short(bf.Mac), short(bf.Ubuntu)))
	case applyUncertain:
		// 保留 Mac 的结果，不回退；finishApply 记不确定并停止
		o.detail = "Mac 已快进到合并提交 " + merged + "；" + o.detail
		r.finishApply(act, SideUbuntu, o)
	default:
		// 保留 Mac 的结果，不回退；停止后续写，下一轮重新采集后会把 Ubuntu 快进上来
		d := "Mac 已快进到合并提交 " + merged + "，Ubuntu 未更新（下一轮重新判定）: " + o.detail
		r.addBranch(act, ResultFailed, d)
		if o.oos {
			r.outOfScope(SideUbuntu, o.detail, []string{o.detail})
		}
		r.att(AttentionItem{Kind: AttApplyFailed, Side: SideUbuntu, Branch: act.Name,
			Message: fmt.Sprintf("分支 %s：Mac 已快进到合并提交，Ubuntu 更新失败（未写入），本轮停止后续写", act.Name), Detail: o.detail})
		r.stop(fmt.Sprintf("分支 %s：Mac 已更新、Ubuntu 快进失败", act.Name), false, SideUbuntu, act.Name)
	}
}

func summarize(res *RoundResult) string {
	if res.Skipped {
		return res.SkipReason
	}
	var done, failed, skipped int
	for _, b := range res.Branches {
		switch b.Result {
		case ResultDone:
			done++
		case ResultFailed, ResultUncertain:
			failed++
		case ResultSkipped, ResultNotRun:
			skipped++
		}
	}
	var parts []string
	if res.Error != "" {
		parts = append(parts, res.Error)
	}
	parts = append(parts, fmt.Sprintf("同步 %d 个分支，失败/不确定 %d，跳过 %d，无事 %d", done, failed, skipped, res.Unchanged))
	if n := len(res.Attention); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 项需要处理", n))
	}
	return strings.Join(parts, "；")
}
