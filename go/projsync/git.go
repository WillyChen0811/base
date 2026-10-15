package projsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// BranchState 描述一个本地分支相对于对方同名分支的领先/落后。
type BranchState struct {
	Branch string `json:"branch"` // 本机分支名
	Ahead  int    `json:"ahead"`  // 本机比对方同名分支多的提交数
	Behind int    `json:"behind"` // 对方比本机多的提交数
	Remote bool   `json:"remote"` // 对方有没有同名分支
}

// GitResult 是 SyncGit 的返回结果。
type GitResult struct {
	Branches   []BranchState `json:"branches"`    // 本机所有分支，按名字排序
	RemoteOnly []string      `json:"remote_only"` // 对方有、本机没有的分支名，排序；空时 []
}

var namespaceIllegal = regexp.MustCompile(`[^a-z0-9\-]`)

// validateNamespace 校验命名空间名：只允许 [a-z0-9-]，1..32 字节。
func validateNamespace(name string) error {
	if len(name) == 0 || len(name) > 32 {
		return fmt.Errorf("projsync: 命名空间名长度必须 1..32 字节，实际 %d", len(name))
	}
	if m := namespaceIllegal.FindString(name); m != "" {
		return fmt.Errorf("projsync: 命名空间名 %q 包含非法字符 %q，只允许 [a-z0-9-]", name, m)
	}
	return nil
}

// SyncGit 将 dir 仓库的代码与远端仓库 <remoteURL> 交换已提交的代码，返回每个本地分支的领先/落后信息。
//
// 步骤：
//  1. git push --prune +refs/heads/*:refs/remotes/<localName>/* — 本机推送到远端，落盘到 localName 命名空间
//  2. git fetch --prune --no-tags +refs/heads/*:refs/remotes/<remoteName>/* — 从远端拉取，落盘到 remoteName 命名空间
//  3. 列出本机所有分支，逐个计算 Ahead/Behind
//  4. 找出对方有但本机没有的分支（RemoteOnly）
//
// 绝不修改本机或对方的 HEAD/refs/heads/工作区。
// 继承的 os.Environ() 先经 cleanEnv 清掉全部 GIT_*（读写命令统一），env（执行器/调用方注入）追加其后， caller 可用它注入 git config 等。
func SyncGit(ctx context.Context, dir, remoteURL string, env []string, localName, remoteName string) (GitResult, error) {
	// 校验参数
	if err := validateNamespace(localName); err != nil {
		return GitResult{}, err
	}
	if err := validateNamespace(remoteName); err != nil {
		return GitResult{}, err
	}
	if remoteURL == "" || strings.HasPrefix(remoteURL, "-") {
		return GitResult{}, fmt.Errorf("projsync: 远端地址不能为空或以 - 开头")
	}

	// 前置：dir 必须是存在的目录（快速检查，git 命令会再确认）
	if _, err := os.Stat(dir); err != nil {
		return GitResult{}, fmt.Errorf("projsync: 目录 %q 不存在: %w", dir, err)
	}

	pushSpec := fmt.Sprintf("+refs/heads/*:refs/remotes/%s/*", localName)
	fetchSpec := fmt.Sprintf("+refs/heads/*:refs/remotes/%s/*", remoteName)

	// 步骤 1: push --prune
	if _, err := runGit(ctx, dir, env, "push", "--prune", "--no-verify", "--", remoteURL, pushSpec); err != nil {
		return GitResult{}, fmt.Errorf("projsync: push 失败: %w", err)
	}

	// 步骤 2: fetch --prune --no-tags
	if _, err := runGit(ctx, dir, env, "fetch", "--prune", "--no-tags", "--", remoteURL, fetchSpec); err != nil {
		return GitResult{}, fmt.Errorf("projsync: fetch 失败: %w", err)
	}

	// 步骤 3: 列出本机分支
	localRefs, err := listLocalBranches(ctx, dir, env)
	if err != nil {
		return GitResult{}, fmt.Errorf("projsync: 列出本地分支失败: %w", err)
	}

	result := GitResult{
		Branches:   make([]BranchState, 0, len(localRefs)),
		RemoteOnly: make([]string, 0),
	}

	// 对每个本地分支计算 ahead/behind
	for _, branch := range localRefs {
		remoteRef := fmt.Sprintf("refs/remotes/%s/%s", remoteName, branch)

		// 检查对方是否有同名分支
		remoteExists := refExists(ctx, dir, env, remoteRef)

		if remoteExists {
			// 用 rev-list --count 算 ahead/behind
			countOut, err := runGit(ctx, dir, env, "rev-list", "--left-right", "--count",
				fmt.Sprintf("refs/heads/%s...%s", branch, remoteRef))
			if err != nil {
				return GitResult{}, fmt.Errorf("projsync: 计算 %s 领先/落后失败: %w", branch, err)
			}
			f := strings.Fields(string(countOut))
			if len(f) != 2 {
				return GitResult{}, fmt.Errorf("projsync: 无法解析 rev-list 输出 %q", countOut)
			}
			ahead, err1 := strconv.Atoi(f[0])
			behind, err2 := strconv.Atoi(f[1])
			if err1 != nil || err2 != nil {
				return GitResult{}, fmt.Errorf("projsync: 无法解析 rev-list 输出 %q", countOut)
			}
			result.Branches = append(result.Branches, BranchState{
				Branch: branch, Ahead: ahead, Behind: behind, Remote: true,
			})
		} else {
			result.Branches = append(result.Branches, BranchState{
				Branch: branch, Remote: false,
			})
		}
	}

	// 步骤 4: 找出 RemoteOnly — 对方有但本机没有的分支
	remoteBranchRefs, err := listRemoteRefs(ctx, dir, env, remoteName)
	if err != nil {
		return GitResult{}, fmt.Errorf("projsync: 列出远端引用失败: %w", err)
	}

	localSet := make(map[string]struct{}, len(localRefs))
	for _, b := range localRefs {
		localSet[b] = struct{}{}
	}

	for _, ref := range remoteBranchRefs {
		if _, ok := localSet[ref]; !ok {
			result.RemoteOnly = append(result.RemoteOnly, ref)
		}
	}

	// 排序
	sort.Strings(result.RemoteOnly)
	// Branches 已经按本地分支字典序（listLocalBranches 已排序），确保
	sort.Slice(result.Branches, func(i, j int) bool {
		return result.Branches[i].Branch < result.Branches[j].Branch
	})

	return result, nil
}

// listLocalBranches 返回本机所有分支名，按字典序排序。
func listLocalBranches(ctx context.Context, dir string, env []string) ([]string, error) {
	out, err := runGit(ctx, dir, env, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range nameLines(out) {
		if line != "" {
			branches = append(branches, line)
		}
	}
	sort.Strings(branches)
	return branches, nil
}

// refExists 检查某个引用是否存在（rev-parse 成功即存在）。
func refExists(ctx context.Context, dir string, env []string, ref string) bool {
	_, _, err := runGitCode(ctx, dir, env, "rev-parse", "--verify", "-q", ref)
	return err == nil
}

// listRemoteRefs 列出 refs/remotes/<nsName>/* 下的所有引用名（不含 refs/remotes/<nsName>/ 前缀）。
func listRemoteRefs(ctx context.Context, dir string, env []string, nsName string) ([]string, error) {
	remoteRefBase := fmt.Sprintf("refs/remotes/%s/", nsName)
	out, err := runGit(ctx, dir, env, "for-each-ref", "--format=%(refname:lstrip=3)", remoteRefBase)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range nameLines(out) {
		if line != "" {
			refs = append(refs, line)
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// runGit 执行 git 并返回 stdout，stderr 弃掉。
func runGit(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	out, _, err := runGitCode(ctx, dir, env, args...)
	return out, err
}

// runGitCode 执行 git -C Dir ...，不经 shell。
// 继承的 os.Environ() 先经 cleanEnv 清掉全部 GIT_*（读写命令统一），env（执行器/调用方注入）追加其后，末尾固定 GIT_TERMINAL_PROMPT=0。
// stdout 与 stderr 分开收集，失败时错误带 stderr（截前 2KB）。
// 返回退出码；非 git 退出类错误（ctx 取消、找不到 git 等）退出码为 -1。
func runGitCode(ctx context.Context, dir string, env []string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// GIT_NO_REPLACE_OBJECTS=1 放在最后、调用方不能覆盖：refs/replace 会让 Git 看到被替换后的历史/对象，必须关掉。
	cmd.Env = append(append(cleanEnv(os.Environ()), env...), "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
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

// nameLines 把 git 输出按行拆开，只去掉记录分隔的 "\n"：名字/路径必须保留原始字节，
// 绝不能 TrimSpace（Go 的 TrimSpace 会裁掉 U+00A0、U+3000 等，把真实名字变成另一个名字）。OID 才可以 TrimSpace。
func nameLines(out []byte) []string {
	s := strings.TrimSuffix(string(out), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
