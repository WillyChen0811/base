package projsync

// 第 4a 项：事实采集（只读，不写任何引用、索引、工作区、stash）。
//
// 本文件把 v1 SyncGit 已交换好的两边引用（本地 refs/heads/*、对方 refs/remotes/<ns>/*）
// 以及工作区状态整理成 decide.go 的输入（BranchFacts / RepoFacts / WorktreeFacts）。
// Ubuntu 侧工作区状态由调用方采集（CollectLocalWorktrees 以后会在 Ubuntu 上经命令行调用）后作为参数传入。
//
// 重要：本包采集函数不调用 CheckScope（CheckScope 是写前检查）。调度层必须在「写」之前，
// 对要写的分支调用 CheckScope（见 PLAN-sync-v2.md 第十一节）；采集的事实只用于判定，不构成写入许可。
//
// 区分「不存在」与「读取失败」：空 OID 只代表已确认引用不存在，任何读取失败整体返回 error。
//
// 关于 merge-tree 是否写对象：`git merge-tree --write-tree` 会把合并结果的 tree（以及需要内容合并时的新 blob）
// 写入对象库（不可达对象，不改引用/索引/工作区，之后由 gc 回收）。为了让采集真正「零写入」，本文件把对象写入
// 重定向到一个临时目录：GIT_OBJECT_DIRECTORY=<临时目录>，GIT_ALTERNATE_OBJECT_DIRECTORIES=<真实对象库>（读）；
// 命令结束即删除临时目录，真实仓库的 .git 一个字节都不变。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CollectBranchFacts 采集每个分支的事实。
//   - 本地（Mac 侧）分支：refs/heads/*；对方（Ubuntu 侧）：refs/remotes/<remoteNS>/*；两边分支名取并集（字节精确，不规范化）。
//   - remoteWorktrees：对方设备上各工作区的事实（按 Branch 匹配；Side 字段不看）。
//   - localQuiet：返回本地某工作区 dirty 改动的静止分钟数；nil 或 ok=false 视为 0 分钟（指纹记录是 4b）。
//   - remoteRepo：对方设备的仓库级事实，只取 UbuntuBusy / UbuntuBusyReason 并入返回的 RepoFacts
//     （选「传一个 RepoFacts 半结构」的做法：调用方在 Ubuntu 侧采集后直接填 UbuntuBusy*，Mac 侧字段忽略）。
//
// 多个工作区检出同一分支时：checkedOut=true，dirty 取「任一 dirty」，静止分钟取 dirty 工作区中的最小值（保守）。
func CollectBranchFacts(ctx context.Context, repoDir string, env []string, remoteNS string,
	remoteWorktrees []WorktreeFacts, remoteRepo RepoFacts,
	localQuiet func(path string) (minutes int, ok bool)) ([]BranchFacts, RepoFacts, error) {

	if err := validateNamespace(remoteNS); err != nil {
		return nil, RepoFacts{}, err
	}
	localNames, err := listLocalBranches(ctx, repoDir, env)
	if err != nil {
		return nil, RepoFacts{}, fmt.Errorf("列出本地分支失败: %w", err)
	}
	remoteNames, err := listRemoteRefs(ctx, repoDir, env, remoteNS)
	if err != nil {
		return nil, RepoFacts{}, fmt.Errorf("列出对方分支失败: %w", err)
	}
	seen := map[string]bool{}
	var names []string
	for _, n := range append(append([]string{}, localNames...), remoteNames...) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)

	wts, err := listWorktrees(ctx, repoDir, env)
	if err != nil {
		return nil, RepoFacts{}, err
	}
	// 每个工作区的 dirty 只算一次。
	dirty := map[string]bool{}
	for _, w := range wts {
		if w.Bare {
			continue
		}
		d, err := worktreeDirty(ctx, w.Path, env)
		if err != nil {
			return nil, RepoFacts{}, err
		}
		dirty[w.Path] = d
	}

	repo := RepoFacts{UbuntuBusy: remoteRepo.UbuntuBusy, UbuntuBusyReason: remoteRepo.UbuntuBusyReason}
	for _, w := range wts {
		if w.Bare {
			continue
		}
		marker, err := busyMarkerOf(ctx, w.Path, env)
		if err != nil {
			return nil, RepoFacts{}, err
		}
		if marker != "" && !repo.MacBusy {
			repo.MacBusy, repo.MacBusyReason = true, marker
		}
	}

	var out []BranchFacts
	for _, name := range names {
		bf := BranchFacts{Name: name}
		var ex bool
		if bf.Mac, ex, err = readRef(ctx, repoDir, env, "refs/heads/"+name); err != nil {
			return nil, RepoFacts{}, err
		} else if !ex {
			bf.Mac = ""
		}
		if bf.Ubuntu, ex, err = readRef(ctx, repoDir, env, "refs/remotes/"+remoteNS+"/"+name); err != nil {
			return nil, RepoFacts{}, err
		} else if !ex {
			bf.Ubuntu = ""
		}
		if bf.Mac != "" && bf.Ubuntu != "" && bf.Mac != bf.Ubuntu {
			if bf.MacIsAncestorOfUbuntu, err = isAncestor(ctx, repoDir, env, bf.Mac, bf.Ubuntu); err != nil {
				return nil, RepoFacts{}, fmt.Errorf("分支 %s：%w", name, err)
			}
			if bf.UbuntuIsAncestorOfMac, err = isAncestor(ctx, repoDir, env, bf.Ubuntu, bf.Mac); err != nil {
				return nil, RepoFacts{}, fmt.Errorf("分支 %s：%w", name, err)
			}
			if !bf.MacIsAncestorOfUbuntu && !bf.UbuntuIsAncestorOfMac {
				if bf.MergeClean, bf.ConflictFiles, err = tryMerge(ctx, repoDir, env, bf.Mac, bf.Ubuntu); err != nil {
					return nil, RepoFacts{}, fmt.Errorf("分支 %s：%w", name, err)
				}
			}
		}
		for _, w := range checkedOutBy(wts, name) {
			bf.MacCheckedOut = true
			if dirty[w.Path] {
				m := 0
				if localQuiet != nil {
					if v, ok := localQuiet(w.Path); ok {
						m = v
					}
				}
				if !bf.MacDirty || m < bf.MacQuietMinutes {
					bf.MacQuietMinutes = m
				}
				bf.MacDirty = true
			}
		}
		for _, w := range remoteWorktrees {
			if w.Branch != name {
				continue
			}
			bf.UbuntuCheckedOut = true
			if w.Dirty {
				if !bf.UbuntuDirty || w.QuietMinutes < bf.UbuntuQuietMinutes {
					bf.UbuntuQuietMinutes = w.QuietMinutes
				}
				bf.UbuntuDirty = true
			}
		}
		out = append(out, bf)
	}
	return out, repo, nil
}

// CollectLocalWorktrees 列出本仓库每个非 bare 工作区的事实（Branch 在 detached 时为空）。
// side 由调用方指定（在 Ubuntu 上调用时传 SideUbuntu）。quiet 为 nil 或 ok=false 视为 0 分钟。
func CollectLocalWorktrees(ctx context.Context, repoDir string, env []string, side Side,
	quiet func(path string) (int, bool)) ([]WorktreeFacts, error) {
	wts, err := listWorktrees(ctx, repoDir, env)
	if err != nil {
		return nil, err
	}
	var out []WorktreeFacts
	for _, w := range wts {
		if w.Bare {
			continue
		}
		d, err := worktreeDirty(ctx, w.Path, env)
		if err != nil {
			return nil, err
		}
		wf := WorktreeFacts{Side: side, Path: w.Path, Branch: w.Branch, Dirty: d}
		if d && quiet != nil {
			if m, ok := quiet(w.Path); ok {
				wf.QuietMinutes = m
			}
		}
		out = append(out, wf)
	}
	return out, nil
}

// worktreeDirty：git status --porcelain --untracked-files=all 非空即 dirty。
// GIT_OPTIONAL_LOCKS=0 避免 status 为刷新索引而写 index.lock。输出按原始字节判空，不 TrimSpace。
func worktreeDirty(ctx context.Context, wt string, env []string) (bool, error) {
	e := append(append([]string{}, env...), "GIT_OPTIONAL_LOCKS=0")
	out, err := runGit(ctx, wt, e, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return false, fmt.Errorf("读取工作区 %s 状态失败: %w", wt, err)
	}
	return len(out) > 0, nil
}

// busyMarkerOf 返回该工作区 git 目录下第一个存在的进行中标记名；没有则空串。检查失败是 error。
func busyMarkerOf(ctx context.Context, wt string, env []string) (string, error) {
	gd, err := runGit(ctx, wt, env, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("检查工作区 %s 失败: %w", wt, err)
	}
	gitDir := strings.TrimSuffix(string(gd), "\n")
	for _, m := range busyMarkers {
		_, err := os.Lstat(filepath.Join(gitDir, m))
		if err == nil {
			return m, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("检查 %s 的 %s 失败: %w", wt, m, err)
		}
	}
	return "", nil
}

// tryMerge 用 merge-tree 试合并（对象写入重定向到临时目录，见文件头说明）。
// 退出码 0=干净；1=有冲突（解析冲突文件）；其它=命令失败，返回 error。
//
// 注意：runGitCode 失败时丢弃 stdout，而冲突文件在退出码 1 的 stdout 里，所以这里用一个
// 与 runGitCode 同样环境处理（cleanEnv + env + GIT_TERMINAL_PROMPT=0）的本地执行函数保留 stdout。
func tryMerge(ctx context.Context, repoDir string, env []string, mac, ubuntu string) (clean bool, conflicts []string, err error) {
	for _, o := range []string{mac, ubuntu} {
		if !oidRe.MatchString(o) {
			return false, nil, fmt.Errorf("试合并的 OID 不合法: %q", o)
		}
	}
	od, err := runGit(ctx, repoDir, env, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return false, nil, fmt.Errorf("定位对象库失败: %w", err)
	}
	realObjs := strings.TrimSuffix(string(od), "\n")
	if strings.ContainsRune(realObjs, os.PathListSeparator) {
		return false, nil, fmt.Errorf("对象库路径含 %q，无法安全重定向: %s", string(os.PathListSeparator), realObjs)
	}
	tmp, err := os.MkdirTemp("", "xxyi-mergetree-")
	if err != nil {
		return false, nil, err
	}
	defer os.RemoveAll(tmp)

	e := append(append([]string{}, env...), "GIT_OBJECT_DIRECTORY="+tmp, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+realObjs)
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "merge-tree", "--write-tree", "--name-only", "-z", mac, ubuntu)
	cmd.Env = append(append(cleanEnv(os.Environ()), e...), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) || ctx.Err() != nil {
			return false, nil, fmt.Errorf("试合并命令失败: %w", runErr)
		}
		code = ee.ExitCode()
		if code != 1 {
			msg := stderr.Bytes()
			if len(msg) > 2048 {
				msg = msg[:2048]
			}
			return false, nil, fmt.Errorf("试合并命令失败(退出码 %d): %s", code, msg)
		}
	}
	// 输出：<tree>\0 [冲突文件\0 ...] \0 [提示信息\0 ...]；文件名保持原始字节。
	fields := strings.Split(stdout.String(), "\x00")
	if len(fields) == 0 || !oidRe.MatchString(fields[0]) {
		return false, nil, fmt.Errorf("试合并输出无法解析: %q", stdout.String())
	}
	if code == 0 {
		return true, nil, nil
	}
	terminated := false
	for _, f := range fields[1:] {
		if f == "" {
			terminated = true
			break
		}
		conflicts = append(conflicts, f)
	}
	if !terminated || len(conflicts) == 0 {
		return false, nil, fmt.Errorf("试合并报告冲突但无法解析冲突文件: %q", stdout.String())
	}
	return false, conflicts, nil
}
