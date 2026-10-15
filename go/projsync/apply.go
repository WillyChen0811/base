package projsync

// 执行器（PLAN-sync-v2.md 第 2 项，单机版，作用于一个本地仓库）。
//
// 本文件只提供「校验 + 写入」原语：CreateBranch / FastForwardBranch / AutoMergeCommit。
// 每个原语都先重新读取现场再写，任何读取失败、前提不符都返回带中文原因的 error 且不写。
//
// 第八节各条的归属：
//  1. 执行前提：ApplyPlan（本文件）。仓库标识=RepoDir；两边旧 OID 由调用方对两边各建一份 plan；
//     工作区路径不放进 plan，由执行时现场枚举（listWorktrees），不信任旧事实。
//  2. 不存在 vs 读取失败：readRef / isAncestor 三态返回；所有写函数遇 error 一律停止。
//  3. 引用更新：FastForwardBranch 先验后代再 update-ref <new> <old>；CreateBranch 用全零旧值。
//  4. 重查所有工作区：listWorktrees（含 linked、unborn、引用缺失仍被 symbolic HEAD 指向），
//     多个工作区检出同一分支拒绝。
//  5. 检出分支只在实际工作区 merge --ff-only：FastForwardBranch；检查→写入间的竞态用
//     「写前紧贴 merge 再读一次 symbolic-ref/HEAD/分支值」收窄，写后再读 HEAD 核对。
//     注意：同一命令序列内重读不是原子的，残余窗口无法在 git 层消除，
//     因此【调用方必须对同一仓库串行】（同步任务、提交 agent 任务、工作区操作互斥）。
//     「派提交后作废分支计划、任务在途禁止写、静止 ≥20 分钟、派活去重」属第 4 项触发器/调度层，本项不做。
//  6. 创建只建引用：CreateBranch。
//  7. 合并绑定精确父提交：AutoMergeCommit（只造提交、不动分支）；两边更新由调用方对两边分别调
//     FastForwardBranch（旧值各自是自己的父提交）；「一边成功另一边失败则保留成功结果、重新采集重试、
//     禁止回退覆盖」属调度层（本项没有任何回退写路径）。
//  8. 提交任务派发 / 9. 冲突只交控制中心：调度层与展示层，本项不做。
//  10. 集成测试：apply_test.go。
//
// 禁用：reset、checkout/switch、branch -d/-D、update-ref -d、--force、--autostash（merge 显式 --no-autostash、--no-overwrite-ignore）、stash。
// 所有待写分支必须是直接引用（符号引用/悬空符号引用一律拒绝），update-ref 带 --no-deref。
// 任何写动作前检查仓库全部非 bare 工作区的进行中标记（merge/rebase/cherry-pick/revert/sequencer/bisect）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ApplyPlan 是执行一个分支动作所需的完整前提。
type ApplyPlan struct {
	RepoDir string // 仓库任一工作区路径（用于运行 git）
	Branch  string // 短分支名，如 main
	// OldOID 是执行前预期的分支当前值；空串=预期不存在（仅 CreateBranch）。
	OldOID string
	NewOID string
	Kind   ActionKind
	// Parents 是 AutoMerge 的两个父提交，顺序固定：Parents[0] 是 mac 侧、Parents[1] 是 ubuntu 侧。
	Parents [2]string
	// FactsVersion 是采集事实的版本号，仅用于错误信息；是否过期由调用方（调度层）判断，
	// 本层以现场重读的 OID/HEAD 为准。
	FactsVersion int64
	// Env 追加给 git 的环境变量，可空。
	Env []string
}

// applyTestHook 仅测试使用：在 FastForwardBranch 通过检查、真正写入之前被调用。
var applyTestHook func(stage string)

// ErrCheckedOutDuringUpdate 表示更新引用期间该分支被某工作区检出：引用已改、工作区文件未随之更新。
// 只报告，不做任何回退或补救写；需人工处理。用 errors.As 取出详情。
type ErrCheckedOutDuringUpdate struct {
	Branch, Worktree, OldOID, NewOID string
}

func (e *ErrCheckedOutDuringUpdate) Error() string {
	return fmt.Sprintf("分支 %s：更新引用期间该分支被工作区 %s 检出，工作区文件未随之更新（旧 %s，新 %s），需人工处理；未做任何回退",
		e.Branch, e.Worktree, e.OldOID, e.NewOID)
}

// postCheckNotCheckedOut 在引用写入成功后复查：若此时有工作区检出该分支则报告（只能发现、不能消除竞态，
// 调度层必须对同一仓库串行执行同步与工作区操作）。
func (p ApplyPlan) postCheckNotCheckedOut(ctx context.Context) error {
	wts, err := listWorktrees(ctx, p.RepoDir, p.Env)
	if err != nil {
		return fmt.Errorf("%s：引用已更新，但事后复查工作区失败，需人工确认: %w", p.tag(), err)
	}
	if hs := checkedOutBy(wts, p.Branch); len(hs) > 0 {
		return &ErrCheckedOutDuringUpdate{Branch: p.Branch, Worktree: hs[0].Path, OldOID: p.OldOID, NewOID: p.NewOID}
	}
	return nil
}

var oidRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func zeroOID(like string) string { return strings.Repeat("0", len(like)) }

func (p ApplyPlan) tag() string {
	return fmt.Sprintf("分支 %s（事实版本 %d）", p.Branch, p.FactsVersion)
}

func (p ApplyPlan) validateCommon(ctx context.Context, kind ActionKind) error {
	if p.Kind != kind {
		return fmt.Errorf("%s：动作类型不符（期望 %s，实际 %s），停止", p.tag(), kind, p.Kind)
	}
	if p.RepoDir == "" {
		return fmt.Errorf("%s：缺少仓库路径，停止", p.tag())
	}
	if p.Branch == "" || strings.HasPrefix(p.Branch, "-") {
		return fmt.Errorf("%s：分支名不合法，停止", p.tag())
	}
	if _, err := runGit(ctx, p.RepoDir, p.Env, "check-ref-format", "refs/heads/"+p.Branch); err != nil {
		return fmt.Errorf("%s：分支名不合法，停止: %w", p.tag(), err)
	}
	return nil
}

// readRef 读取引用。exists=false 且 err==nil 才代表已确认引用不存在；
// 其它任何失败（目录不存在、不是仓库、权限、对象损坏）都返回 err。
func readRef(ctx context.Context, dir string, env []string, ref string) (oid string, exists bool, err error) {
	// for-each-ref 按前缀匹配，所以要精确比对 refname；它对不存在的引用返回空且退出码 0。
	out, err := runGit(ctx, dir, env, "for-each-ref", "--format=%(objectname) %(refname)", ref)
	if err != nil {
		return "", false, fmt.Errorf("读取引用 %s 失败: %w", ref, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, " ", 2)
		if len(f) == 2 && f[1] == ref {
			if !oidRe.MatchString(f[0]) {
				return "", false, fmt.Errorf("引用 %s 的值不是合法 OID: %q", ref, f[0])
			}
			return f[0], true, nil
		}
	}
	return "", false, nil
}

// isAncestor 判断 a 是否是 b 的祖先（含相等）。命令失败（对象缺失、OID 损坏等）是 error，不是 false。
func isAncestor(ctx context.Context, dir string, env []string, a, b string) (bool, error) {
	if !oidRe.MatchString(a) || !oidRe.MatchString(b) {
		return false, fmt.Errorf("祖先检测的 OID 不合法: %q %q", a, b)
	}
	_, code, err := runGitCode(ctx, dir, env, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("祖先检测失败: %w", err)
}

// isSymbolicRef 判断 ref 是否是符号引用（含悬空）。exit 1=不是；其它失败是 error。
func isSymbolicRef(ctx context.Context, dir string, env []string, ref string) (bool, error) {
	_, code, err := runGitCode(ctx, dir, env, "symbolic-ref", "-q", ref)
	if err == nil {
		return true, nil
	}
	if code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("检查 %s 是否符号引用失败: %w", ref, err)
}

// requireDirectRef 拒绝符号引用（update-ref 即使加 --no-deref 也不应去改别名）。
func (p ApplyPlan) requireDirectRef(ctx context.Context, ref string) error {
	sym, err := isSymbolicRef(ctx, p.RepoDir, p.Env, ref)
	if err != nil {
		return fmt.Errorf("%s：%w，停止", p.tag(), err)
	}
	if sym {
		return fmt.Errorf("%s：%s 是符号引用，拒绝更新", p.tag(), ref)
	}
	return nil
}

// requireAllIdle 逐个检查全部非 bare 工作区的进行中标记（各自的 git 目录）；任一 busy 或失败都停止。
func (p ApplyPlan) requireAllIdle(ctx context.Context, wts []WorktreeInfo) error {
	for _, w := range wts {
		if w.Bare {
			continue
		}
		gd, err := runGit(ctx, w.Path, p.Env, "rev-parse", "--absolute-git-dir")
		if err != nil {
			return fmt.Errorf("%s：检查工作区 %s 失败，停止: %w", p.tag(), w.Path, err)
		}
		gitDir := strings.TrimSuffix(string(gd), "\n") // 只去掉输出末尾一个换行，路径自身的空白必须保留
		for _, m := range busyMarkers {
			_, err := os.Lstat(filepath.Join(gitDir, m))
			if err == nil {
				return fmt.Errorf("%s：工作区 %s 正在进行 %s 对应的操作（merge/rebase/cherry-pick/revert/bisect），停止", p.tag(), w.Path, m)
			}
			if !os.IsNotExist(err) {
				return fmt.Errorf("%s：检查 %s 的 %s 失败，停止: %w", p.tag(), w.Path, m, err)
			}
		}
	}
	return nil
}

// requireCommit 确认 oid 在仓库里存在且是提交。
func requireCommit(ctx context.Context, dir string, env []string, oid string) error {
	if !oidRe.MatchString(oid) {
		return fmt.Errorf("OID 不合法: %q", oid)
	}
	if _, err := runGit(ctx, dir, env, "cat-file", "-e", oid+"^{commit}"); err != nil {
		return fmt.Errorf("提交 %s 不存在或不是提交: %w", oid, err)
	}
	return nil
}

// WorktreeInfo 是一个工作区的 HEAD 状态。
type WorktreeInfo struct {
	Path     string
	Bare     bool
	Detached bool
	// Branch 是 HEAD 作为 symbolic ref 指向的短分支名；Detached 时为空。
	Branch string
	// Unborn：HEAD 指向的分支引用不存在（含新仓库未提交、引用被删但 HEAD 仍指向）。
	Unborn  bool
	HeadOID string
}

// listWorktrees 列出仓库全部工作区（含 linked worktree）。
func listWorktrees(ctx context.Context, dir string, env []string) ([]WorktreeInfo, error) {
	out, err := runGit(ctx, dir, env, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, fmt.Errorf("列出工作区失败: %w", err)
	}
	var wts []WorktreeInfo
	var cur *WorktreeInfo
	flush := func() error {
		if cur == nil {
			return nil
		}
		if cur.Branch != "" && !cur.Bare {
			_, ex, err := readRef(ctx, dir, env, "refs/heads/"+cur.Branch)
			if err != nil {
				return err
			}
			cur.Unborn = !ex
		}
		wts = append(wts, *cur)
		cur = nil
		return nil
	}
	// -z：每个属性以 NUL 结尾，工作区之间多一个空记录；路径保持原始字节，不做 C 风格转义。
	for _, line := range strings.Split(string(out), "\x00") {
		switch {
		case line == "":
			if err := flush(); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "worktree "):
			if err := flush(); err != nil {
				return nil, err
			}
			cur = &WorktreeInfo{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			return nil, fmt.Errorf("工作区列表格式异常: %q", line)
		case strings.HasPrefix(line, "HEAD "):
			cur.HeadOID = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			if !strings.HasPrefix(ref, "refs/heads/") {
				return nil, fmt.Errorf("工作区 %s 的 HEAD 指向非分支引用 %s", cur.Path, ref)
			}
			cur.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case line == "detached":
			cur.Detached = true
		case line == "bare":
			cur.Bare = true
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(wts) == 0 {
		return nil, errors.New("工作区列表为空")
	}
	return wts, nil
}

// checkedOutBy 返回 HEAD 指向该分支名的全部工作区（含 unborn / 引用缺失）。
func checkedOutBy(wts []WorktreeInfo, branch string) []WorktreeInfo {
	var r []WorktreeInfo
	for _, w := range wts {
		if !w.Bare && !w.Detached && w.Branch == branch {
			r = append(r, w)
		}
	}
	return r
}

// CreateBranch 只建引用 refs/heads/<Branch> = NewOID，不碰任何文件/索引/HEAD。
func CreateBranch(ctx context.Context, p ApplyPlan) error {
	if err := p.validateCommon(ctx, KindCreateOnOther); err != nil {
		return err
	}
	if p.OldOID != "" {
		return fmt.Errorf("%s：创建分支要求旧值为空，停止", p.tag())
	}
	if err := requireCommit(ctx, p.RepoDir, p.Env, p.NewOID); err != nil {
		return fmt.Errorf("%s：目标提交不可用，停止: %w", p.tag(), err)
	}
	ref := "refs/heads/" + p.Branch
	if err := p.requireDirectRef(ctx, ref); err != nil { // 悬空符号引用也在此拒绝
		return err
	}
	_, ex, err := readRef(ctx, p.RepoDir, p.Env, ref)
	if err != nil {
		return fmt.Errorf("%s：读取失败，停止: %w", p.tag(), err)
	}
	if ex {
		return fmt.Errorf("%s：目标分支已存在，拒绝创建，需重新判定", p.tag())
	}
	wts, err := listWorktrees(ctx, p.RepoDir, p.Env)
	if err != nil {
		return fmt.Errorf("%s：%w，停止", p.tag(), err)
	}
	if hs := checkedOutBy(wts, p.Branch); len(hs) > 0 {
		return fmt.Errorf("%s：工作区 %s 的 HEAD 已指向该分支名，拒绝创建", p.tag(), hs[0].Path)
	}
	if err := p.requireAllIdle(ctx, wts); err != nil {
		return err
	}
	if applyTestHook != nil {
		applyTestHook("beforeUpdateRef")
	}
	// 旧值全零 = 必须不存在的比较交换。
	// 竞态：上面的检查与此处写入之间不是原子的，写后只能事后复查发现（见 postCheckNotCheckedOut），
	// 调度层必须对同一仓库串行。
	if _, err := runGit(ctx, p.RepoDir, p.Env, "update-ref", "--no-deref", ref, p.NewOID, zeroOID(p.NewOID)); err != nil {
		return fmt.Errorf("%s：创建引用失败（可能被抢先建立），需重新判定: %w", p.tag(), err)
	}
	return p.postCheckNotCheckedOut(ctx)
}

// FastForwardBranch 把分支从 OldOID 快进到 NewOID。
func FastForwardBranch(ctx context.Context, p ApplyPlan) error {
	if err := p.validateCommon(ctx, KindFastForward); err != nil {
		return err
	}
	if !oidRe.MatchString(p.OldOID) {
		return fmt.Errorf("%s：快进需要合法的旧值，停止", p.tag())
	}
	if err := requireCommit(ctx, p.RepoDir, p.Env, p.NewOID); err != nil {
		return fmt.Errorf("%s：目标提交不可用，停止: %w", p.tag(), err)
	}
	ref := "refs/heads/" + p.Branch
	if err := p.requireDirectRef(ctx, ref); err != nil {
		return err
	}
	cur, ex, err := readRef(ctx, p.RepoDir, p.Env, ref)
	if err != nil {
		return fmt.Errorf("%s：读取失败，停止: %w", p.tag(), err)
	}
	if !ex || cur != p.OldOID {
		return fmt.Errorf("%s：引用已变化（当前 %q，预期 %s），需重新判定", p.tag(), cur, p.OldOID)
	}
	isAnc, err := isAncestor(ctx, p.RepoDir, p.Env, p.OldOID, p.NewOID)
	if err != nil {
		return fmt.Errorf("%s：%w，停止", p.tag(), err)
	}
	if !isAnc {
		return fmt.Errorf("%s：目标不是当前值的后代，拒绝非快进更新", p.tag())
	}
	if p.OldOID == p.NewOID {
		return nil
	}
	wts, err := listWorktrees(ctx, p.RepoDir, p.Env)
	if err != nil {
		return fmt.Errorf("%s：%w，停止", p.tag(), err)
	}
	if err := p.requireAllIdle(ctx, wts); err != nil {
		return err
	}
	holders := checkedOutBy(wts, p.Branch)
	switch len(holders) {
	case 0:
		if applyTestHook != nil {
			applyTestHook("beforeUpdateRef")
		}
		// 未检出路径：listWorktrees 与 update-ref 之间不是原子的，写后复查只能发现、不能消除竞态，
		// 调度层必须对同一仓库串行执行同步与工作区操作。
		if _, err := runGit(ctx, p.RepoDir, p.Env, "update-ref", "--no-deref", ref, p.NewOID, p.OldOID); err != nil {
			return fmt.Errorf("%s：更新引用失败（引用可能已变化），需重新判定: %w", p.tag(), err)
		}
		if got, ex, err := readRef(ctx, p.RepoDir, p.Env, ref); err != nil || !ex || got != p.NewOID {
			return fmt.Errorf("%s：更新后复读引用不符（%q，%v），需人工确认", p.tag(), got, err)
		}
		return p.postCheckNotCheckedOut(ctx)
	case 1:
		return p.fastForwardCheckedOut(ctx, holders[0].Path, ref)
	default:
		return fmt.Errorf("%s：被 %d 个工作区同时检出，拒绝更新（%s 等）", p.tag(), len(holders), holders[0].Path)
	}
}

// busyMarkers 是 git 目录下表示操作进行中的文件/目录。
var busyMarkers = []string{"MERGE_HEAD", "REBASE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "REVERT_HEAD", "sequencer", "BISECT_LOG"}

func (p ApplyPlan) checkWorktreeHead(ctx context.Context, wt, ref string) error {
	sym, err := runGit(ctx, wt, p.Env, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return fmt.Errorf("工作区 %s 的 HEAD 已不是分支（或读取失败）: %w", wt, err)
	}
	if strings.TrimSuffix(string(sym), "\n") != ref {
		return fmt.Errorf("工作区 %s 已切换到 %s", wt, strings.TrimSuffix(string(sym), "\n"))
	}
	head, err := runGit(ctx, wt, p.Env, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("读取工作区 %s 的 HEAD 失败: %w", wt, err)
	}
	if strings.TrimSpace(string(head)) != p.OldOID {
		return fmt.Errorf("工作区 %s 的 HEAD 已变化为 %s", wt, strings.TrimSpace(string(head)))
	}
	cur, ex, err := readRef(ctx, wt, p.Env, ref)
	if err != nil {
		return err
	}
	if !ex || cur != p.OldOID {
		return fmt.Errorf("分支值已变化为 %q", cur)
	}
	return nil
}

// checkClean：含未跟踪文件（-uall，按 .gitignore 过滤）；GIT_OPTIONAL_LOCKS=0 避免 status 回写索引。
func (p ApplyPlan) checkClean(ctx context.Context, wt string) error {
	st, err := runGit(ctx, wt, append(append([]string{}, p.Env...), "GIT_OPTIONAL_LOCKS=0"),
		"status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(st))) > 0 {
		return fmt.Errorf("工作区 %s 有未提交改动或未跟踪文件", wt)
	}
	return nil
}

// checkNoOverwrite：被 .gitignore 忽略的未跟踪文件 status 看不见，但 merge 可能覆盖。
// 以 --no-renames 把重命名拆成删除+新增，检查新提交里所有将被写入的路径（新增/修改/类型变化）：
// 路径本身及其各级父路径在工作区若存在「Old 未跟踪」的同名文件则拒绝；
// 路径本身是目录时（文件/目录互换），目录内必须全是 Old 已跟踪文件。
func (p ApplyPlan) checkNoOverwrite(ctx context.Context, wt string) error {
	tr, err := runGit(ctx, wt, p.Env, "ls-tree", "-r", "-z", "--name-only", p.OldOID)
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, f := range strings.Split(string(tr), "\x00") {
		if f != "" {
			tracked[f] = true
		}
	}
	out, err := runGit(ctx, wt, p.Env, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=AMT", p.OldOID, p.NewOID)
	if err != nil {
		return err
	}
	for _, path := range strings.Split(string(out), "\x00") {
		if path == "" {
			continue
		}
		parts := strings.Split(path, "/")
		for k := range parts {
			q := strings.Join(parts[:k+1], "/")
			fi, err := os.Lstat(filepath.Join(wt, q))
			if os.IsNotExist(err) {
				break // 更深的层级也不会存在
			}
			if err != nil {
				return fmt.Errorf("检查路径 %s 失败: %w", q, err)
			}
			final := k == len(parts)-1
			if !fi.IsDir() {
				if !tracked[q] {
					return fmt.Errorf("路径 %s 在工作区已存在且未被跟踪（被忽略的文件），会被覆盖", q)
				}
				break // 已跟踪文件将由 git 替换，其下不会有内容
			}
			if !final {
				continue
			}
			// 最终路径在工作区是目录（新提交里它是文件）：目录内必须全是已跟踪文件。
			werr := filepath.WalkDir(filepath.Join(wt, q), func(fp string, d os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if d.IsDir() {
					return nil
				}
				rel, e := filepath.Rel(wt, fp)
				if e != nil {
					return e
				}
				if !tracked[filepath.ToSlash(rel)] {
					return fmt.Errorf("目录 %s 内有未跟踪文件 %s，会被覆盖", q, rel)
				}
				return nil
			})
			if werr != nil {
				return werr
			}
		}
	}
	return nil
}

func (p ApplyPlan) fastForwardCheckedOut(ctx context.Context, wt, ref string) error {
	fail := func(err error) error { return fmt.Errorf("%s：%w，停止，未写入", p.tag(), err) }
	if err := p.checkWorktreeHead(ctx, wt, ref); err != nil {
		return fail(err)
	}
	// 进行中标记已由 requireAllIdle 对全部工作区检查过。
	if err := p.checkClean(ctx, wt); err != nil {
		return fail(err)
	}
	if err := p.checkNoOverwrite(ctx, wt); err != nil {
		return fail(err)
	}

	if applyTestHook != nil {
		applyTestHook("beforeMerge")
	}
	// 紧贴写入再读一次（非原子，见文件头：调用方必须串行）。
	if err := p.checkWorktreeHead(ctx, wt, ref); err != nil {
		return fail(err)
	}
	if err := p.checkClean(ctx, wt); err != nil {
		return fail(err)
	}
	if _, err := runGit(ctx, wt, p.Env,
		// -c 清空该分支继承的 mergeOptions（如 --squash 会让内容进索引/工作区而 HEAD 不动）；
		// 显式 --no-squash。--ff-only 在命令行上优先于 merge.ff 等配置，不会被全局配置改变语义。
		"-c", "branch."+p.Branch+".mergeOptions=", "merge", "--ff-only", "--no-squash", "--no-autostash", "--no-overwrite-ignore", p.NewOID); err != nil {
		return fmt.Errorf("%s：merge --ff-only 失败，需重新判定: %w", p.tag(), err)
	}
	head, err := runGit(ctx, wt, p.Env, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("%s：快进后读取 HEAD 失败: %w", p.tag(), err)
	}
	if got := strings.TrimSpace(string(head)); got != p.NewOID {
		return fmt.Errorf("%s：快进后 HEAD 为 %s，不等于预期 %s，需人工查看", p.tag(), got, p.NewOID)
	}
	return nil
}

// AutoMergeCommit 不碰工作区地试合并两个父提交并生成合并提交，返回新提交 OID。
// 不更新任何分支：调用方对两边分别调用 FastForwardBranch（旧值各自是自己的父提交）。
func AutoMergeCommit(ctx context.Context, p ApplyPlan) (string, error) {
	if err := p.validateCommon(ctx, KindAutoMerge); err != nil {
		return "", err
	}
	p1, p2 := p.Parents[0], p.Parents[1]
	if p1 == p2 {
		return "", fmt.Errorf("%s：两个父提交相同，停止", p.tag())
	}
	for _, o := range []string{p1, p2} {
		if err := requireCommit(ctx, p.RepoDir, p.Env, o); err != nil {
			return "", fmt.Errorf("%s：父提交不可用，停止: %w", p.tag(), err)
		}
	}
	out, code, err := runGitCode(ctx, p.RepoDir, p.Env, "merge-tree", "--write-tree", p1, p2)
	if err != nil {
		if code == 1 {
			return "", fmt.Errorf("%s：试合并有冲突，不自动合并", p.tag())
		}
		return "", fmt.Errorf("%s：试合并命令失败，停止: %w", p.tag(), err)
	}
	tree := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if !oidRe.MatchString(tree) {
		return "", fmt.Errorf("%s：试合并输出无法解析: %q", p.tag(), tree)
	}
	env := append(append([]string{}, p.Env...),
		"GIT_AUTHOR_NAME=XXYI", "GIT_AUTHOR_EMAIL=xxyi@localhost",
		"GIT_COMMITTER_NAME=XXYI", "GIT_COMMITTER_EMAIL=xxyi@localhost")
	msg := fmt.Sprintf("XXYI 自动合并 %s：mac %s + ubuntu %s", p.Branch, p1, p2)
	co, err := runGit(ctx, p.RepoDir, env, "commit-tree", tree, "-p", p1, "-p", p2, "-m", msg)
	if err != nil {
		return "", fmt.Errorf("%s：生成合并提交失败，停止: %w", p.tag(), err)
	}
	merged := strings.TrimSpace(string(co))
	if !oidRe.MatchString(merged) {
		return "", fmt.Errorf("%s：合并提交输出无法解析: %q", p.tag(), merged)
	}
	for _, par := range []string{p1, p2} {
		ok, err := isAncestor(ctx, p.RepoDir, p.Env, par, merged)
		if err != nil {
			return "", fmt.Errorf("%s：校验父提交失败: %w", p.tag(), err)
		}
		if !ok {
			return "", fmt.Errorf("%s：父提交 %s 不是合并提交的祖先，丢弃", p.tag(), par)
		}
	}
	return merged, nil
}
