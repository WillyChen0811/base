package support

// /exec/run 的可选 branch：编译类动作不切换 xxyi 主工作区，而是在固定的专用 worktree 里编译指定分支。
// fetch 只更新 FETCH_HEAD、不碰本地分支和主工作区文件；worktree 只有一个槽位，反复复用以保留 .build 缓存。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// xxyiRepoName 是编译类动作固定作用的仓库名（与 macexec 的 xxyiRepo、Config.Repos 的键一致）。
	xxyiRepoName = "xxyi"
	// branchWorktreeName 是专用 worktree 的目录名，放在 xxyi 仓库根的同级目录下。
	branchWorktreeName = ".xxyi-branch-build"
	// branchErrMax 是回给调用方的 git 错误文字上限（git 错误里带 stderr，截短避免刷屏）。
	branchErrMax = 600
)

// branchBuildMu 保证专用 worktree 同一时间只服务一个请求：准备阶段会 checkout 并 clean，
// 不能发生在上一个分支编译还在跑的时候。拿不到直接按"忙"处理，与 macexec.Run 的单任务约定一致。
var branchBuildMu sync.Mutex

// branchError 带 HTTP 状态码，供 execRun 直接映射。
type branchError struct {
	status int
	msg    string
}

func (e *branchError) Error() string { return e.msg }

// prepareBranchWorktree 把 Remote 的 branch 取下来并在专用 worktree 里检出，返回 worktree 目录和 40 位提交号。
// 与 /sync 共用 syncLocks["xxyi"]，避免同时 fetch/checkout。步骤失败的错误文字都写明是哪一步。
func (s *Service) prepareBranchWorktree(ctx context.Context, branch string) (string, string, error) {
	if err := ValidBranch(branch); err != nil {
		return "", "", &branchError{http.StatusBadRequest, err.Error()}
	}
	repo, ok := s.cfg.Repos[xxyiRepoName]
	lock := s.syncLocks[xxyiRepoName]
	if !ok || lock == nil {
		return "", "", &branchError{http.StatusNotFound, "未知仓库"}
	}
	// Remote 不能为空或以 - 开头，防止被 git 当成选项（与 Sync 同一条规则）
	if repo.Remote == "" || strings.HasPrefix(repo.Remote, "-") {
		return "", "", &branchError{http.StatusBadGateway, fmt.Sprintf("fetch 分支失败：Remote 非法（不能为空或以 - 开头）: %q", repo.Remote)}
	}
	lock.Lock() // 与 /sync 同一把锁：同一仓库的 fetch/checkout 串行
	defer lock.Unlock()
	if ctx.Err() != nil {
		return "", "", &branchError{http.StatusServiceUnavailable, "协助已结束"}
	}

	step := func(what string, err error) error {
		return &branchError{http.StatusBadGateway, what + "失败：" + truncRunes(err.Error(), branchErrMax)}
	}

	// fetch 不要求工作区干净，也不 checkout、不动本地分支。
	if _, err := runGit(ctx, repo, s.cfg.GitEnv, "fetch", "--no-tags", "--", repo.Remote, "refs/heads/"+branch); err != nil {
		return "", "", step("fetch 分支", err)
	}
	out, err := runGit(ctx, repo, s.cfg.GitEnv, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", "", step("取 FETCH_HEAD", err)
	}
	commit := strings.TrimSpace(string(out))
	if len(commit) != 40 {
		return "", "", &branchError{http.StatusBadGateway, fmt.Sprintf("取 FETCH_HEAD 失败：不是 40 位提交号: %q", commit)}
	}

	dir := filepath.Join(filepath.Dir(repo.Dir), branchWorktreeName)
	registered, err := branchWorktreeRegistered(ctx, repo, s.cfg.GitEnv, dir)
	if err != nil {
		return "", "", step("查看 worktree 列表", err)
	}
	if !registered {
		// 目录被删过/残留普通目录时登记可能已过期：先 prune 再看一次，仍不对就报错，绝不自己 rm。
		if _, err := runGit(ctx, repo, s.cfg.GitEnv, "worktree", "prune"); err != nil {
			return "", "", step("清理过期 worktree 记录", err)
		}
		if registered, err = branchWorktreeRegistered(ctx, repo, s.cfg.GitEnv, dir); err != nil {
			return "", "", step("查看 worktree 列表", err)
		}
	}
	wt := Repo{Dir: dir}
	switch {
	case registered:
		if _, err := runGit(ctx, wt, s.cfg.GitEnv, "checkout", "--detach", "--force", commit); err != nil {
			return "", "", step("在专用 worktree 检出 "+commit[:12], err)
		}
		// 不带 -x：被忽略的 .build 缓存要留着
		if _, err := runGit(ctx, wt, s.cfg.GitEnv, "clean", "-fd"); err != nil {
			return "", "", step("清理专用 worktree", err)
		}
	default:
		if _, err := os.Lstat(dir); err == nil {
			return "", "", &branchError{http.StatusBadGateway, fmt.Sprintf("准备专用 worktree 失败：%q 已存在但不是 xxyi 仓库的 worktree，请人工处理后重试", dir)}
		}
		if _, err := runGit(ctx, repo, s.cfg.GitEnv, "worktree", "add", "--detach", dir, commit); err != nil {
			return "", "", step("创建专用 worktree", err)
		}
	}
	return dir, commit, nil
}

// branchWorktreeRegistered 判断 dir 是否是 repo 登记在册、且目录里 .git 文件还在的 worktree。
// 路径两边都过 EvalSymlinks 再比（macOS 的 /var 与 /private/var 等）。
func branchWorktreeRegistered(ctx context.Context, repo Repo, env []string, dir string) (bool, error) {
	out, err := runGit(ctx, repo, env, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		path, ok := strings.CutPrefix(string(line), "worktree ")
		if !ok {
			continue
		}
		if got, err := filepath.EvalSymlinks(path); err == nil && got == want {
			// 登记在册但目录被换成普通目录（没有 .git 文件）不算
			st, err := os.Lstat(filepath.Join(dir, ".git"))
			return err == nil && st.Mode().IsRegular(), nil
		}
	}
	return false, nil
}
