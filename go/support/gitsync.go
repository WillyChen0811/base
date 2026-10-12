// Package support 提供 Mac 协助服务所需的基础能力。
package support

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ErrDirty 工作区有未提交的改动（含未跟踪文件）。
var ErrDirty = errors.New("support: 工作区有未提交的改动")

// ErrNotFastForward 本地分支有远端没有的提交，无法快进。
var ErrNotFastForward = errors.New("support: 本地分支不能快进到远端（有本地独有的提交）")

// Repo 表示一个本地 git 仓库。
type Repo struct {
	Dir    string // 本地仓库目录（已存在的 git 工作区）
	Remote string // 远端地址（URL 或本地路径），只用来 fetch
}

var branchIllegal = regexp.MustCompile(`[^A-Za-z0-9._/\-]`)

// ValidBranch 校验分支名。规则：
//   - 长度 1..200 字节
//   - 只允许 ASCII 字母数字和 . _ / -
//   - 不能以 - 或 / 开头，不能以 / 或 .lock 结尾
//   - 不能含 ".."、"//"、"@{"
//
// 不合规返回带中文描述的错误。
func ValidBranch(name string) error {
	if len(name) == 0 || len(name) > 200 {
		return fmt.Errorf("support: 分支名长度必须 1..200 字节，实际 %d", len(name))
	}
	// 只允许 ASCII 字母数字和 . _ / -
	if m := branchIllegal.FindString(name); m != "" {
		return fmt.Errorf("support: 分支名包含非法字符 %q，只允许 ASCII 字母数字和 . _ / -", m)
	}
	// 不能以 - 或 / 开头
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("support: 分支名不能以 - 或 / 开头")
	}
	// 不能以 / 或 .lock 结尾
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("support: 分支名不能以 / 或 .lock 结尾")
	}
	// 不能含 ".."、"//"、"@{"
	if strings.Contains(name, "..") {
		return fmt.Errorf("support: 分支名不能包含 ..")
	}
	if strings.Contains(name, "//") {
		return fmt.Errorf("support: 分支名不能包含 //")
	}
	if strings.Contains(name, "@{") {
		return fmt.Errorf("support: 分支名不能包含 @{")
	}
	return nil
}

// Sync 把 Remote 的 branch 同步到本地同名分支并切过去，返回同步后的 HEAD 提交号（完整 40 位）。
// 步骤：
//  1. 校验分支名；检查工作区是否干净。
//  2. fetch 远端分支，取 FETCH_HEAD 的提交号。
//  3. 本地分支不存在：checkout 创建。
//  4. 存在：判断能否快进，不能 → ErrNotFastForward；能快进则合并。
func Sync(ctx context.Context, r Repo, branch string, env []string) (string, error) {
	// 1. 校验分支名
	if err := ValidBranch(branch); err != nil {
		return "", err
	}

	// Remote 不能为空或以 - 开头，防止被 git 当成选项
	if r.Remote == "" || strings.HasPrefix(r.Remote, "-") {
		return "", fmt.Errorf("support: Remote 非法（不能为空或以 - 开头）: %q", r.Remote)
	}

	// 1b. 检查工作区干净
	statusOut, err := runGit(ctx, r, env, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(statusOut)) > 0 {
		return "", ErrDirty
	}

	// 2. fetch 远端分支
	_, err = runGit(ctx, r, env, "fetch", "--no-tags", "--", r.Remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}

	// 取 FETCH_HEAD 的提交号
	fetchHeadOut, err := runGit(ctx, r, env, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(fetchHeadOut))
	if len(commit) != 40 {
		return "", fmt.Errorf("support: FETCH_HEAD 不是 40 位提交号: %q", commit)
	}

	// 检查本地分支是否存在（退出码 1 才算不存在，其余错误原样返回）
	_, code, err := runGitCode(ctx, r, env, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		if code != 1 {
			return "", err
		}
		// 本地分支不存在 → 步骤 3：checkout -b
		if _, _, err := runGitCode(ctx, r, env, "checkout", "-b", branch, commit, "--"); err != nil {
			return "", err
		}
		return commit, nil
	}

	// 4. 判断能否快进（用完整引用名，避免被同名标签遮蔽）
	_, code, err = runGitCode(ctx, r, env, "merge-base", "--is-ancestor", "refs/heads/"+branch, commit)
	if err != nil {
		if code == 1 {
			return "", ErrNotFastForward
		}
		return "", err
	}

	// 能快进：检查当前是否在目标分支上（用完整引用名比较；游离 HEAD 时退出码 1）
	symOut, code, err := runGitCode(ctx, r, env, "symbolic-ref", "-q", "HEAD")
	if err != nil && code != 1 {
		return "", err
	}
	if strings.TrimSpace(string(symOut)) == "refs/heads/"+branch {
		if _, _, err := runGitCode(ctx, r, env, "merge", "--ff-only", commit); err != nil {
			return "", err
		}
	} else {
		if _, _, err := runGitCode(ctx, r, env, "branch", "-f", branch, commit); err != nil {
			return "", err
		}
		if _, _, err := runGitCode(ctx, r, env, "checkout", branch, "--"); err != nil {
			return "", err
		}
	}

	headOut, err := runGit(ctx, r, env, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(headOut)), nil
}

// runGit 执行 git 并只返回 stdout。
func runGit(ctx context.Context, r Repo, env []string, args ...string) ([]byte, error) {
	out, _, err := runGitCode(ctx, r, env, args...)
	return out, err
}

// runGitCode 执行 git -C Dir ...，不经 shell。env 追加在 os.Environ() 之后，
// 末尾固定 GIT_TERMINAL_PROMPT=0。stdout 与 stderr 分开收集，失败时错误带 stderr（截前 2KB）。
// 返回退出码；非 git 退出类错误（ctx 取消、找不到 git 等）退出码为 -1。
func runGitCode(ctx context.Context, r Repo, env []string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
	cmd.Env = append(append(os.Environ(), env...), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil {
			code = ee.ExitCode()
		}
		msg := stderr.Bytes()
		if len(msg) > 2048 {
			msg = msg[:2048]
		}
		return nil, code, fmt.Errorf("git %s 失败: %w, stderr: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), 0, nil
}
