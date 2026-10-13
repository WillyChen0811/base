package support

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrMismatch 接收方暂存引用的提交号与期望值不一致（推送后被别处覆盖过）。
var ErrMismatch = errors.New("support: 暂存引用与期望提交号不一致")

// ErrNoRepoDir 目标目录不是 git 仓库（hosts.json 的 remote_dir 配错了）。
var ErrNoRepoDir = errors.New("support: 目标目录不是 git 仓库")

// ErrRepoBusy 分支正被占用：另一个工作区检出了它，或仓库处于 rebase 中。
var ErrRepoBusy = errors.New("support: 分支正被占用（其他工作区或 rebase 中）")

var fullCommit = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// shQuote 把任意字符串做成单引号包住的 shell 参数。
// 原理：' 不能直接被转义，改为断掉当前单引号段、插入转义单引号、再续上剩余部分。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// stagingRef 返回 Mac→Ubuntu 中转用的暂存引用名。
func stagingRef(branch string) string {
	return "refs/xxyi/mac/" + branch
}

// PushRef 把本地仓库 r.Dir 的 branch 推到 r.Remote 的 refs/xxyi/mac/<branch>（强制覆盖这个暂存引用），
// 返回推送的提交号（本地 refs/heads/<branch> 的完整 40 位）。不要求工作区干净（推的是已提交内容）。
func PushRef(ctx context.Context, r Repo, branch string, env []string) (string, error) {
	if err := ValidBranch(branch); err != nil {
		return "", err
	}
	// Remote 不能为空或以 - 开头，防止被 git 当成选项
	if r.Remote == "" || strings.HasPrefix(r.Remote, "-") {
		return "", fmt.Errorf("support: Remote 非法（不能为空或以 - 开头）: %q", r.Remote)
	}

	// 取本地分支的提交号（用完整引用名，避免被同名标签遮蔽）
	out, err := runGit(ctx, r, env, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if len(commit) != 40 {
		return "", fmt.Errorf("support: refs/heads/%s 不是 40 位提交号: %q", branch, commit)
	}

	// 强制覆盖暂存引用；src 用提交号本身，不受推送过程中本地分支移动影响
	if _, err := runGit(ctx, r, env, "push", "--force", r.Remote,
		commit+":"+stagingRef(branch)); err != nil {
		return "", err
	}
	return commit, nil
}

// FFScript 返回在接收方仓库 dir 里执行的 sh 脚本：把 refs/xxyi/mac/<branch> 快进到 refs/heads/<branch>。
// want 是期望的提交号（40 位十六进制，否则报错）；dir 用 shQuote 引起来。
// 输出约定：成功 "OK <提交号>" 退出 0；暂存引用不符 "ERR mismatch" 退出 3；
// 不能快进 "ERR notff" 退出 4；当前分支工作区脏（未跟踪文件不算）"ERR dirty" 退出 5；
// 目录不是 git 仓库 "ERR nodir" 退出 6；分支被其他工作区检出或 rebase 中 "ERR busy" 退出 7。
// 成功（含已一致）后删除暂存引用——中转用完就该没了，失败路径不删（留给排查）。
func FFScript(dir, branch, want string) (string, error) {
	if err := ValidBranch(branch); err != nil {
		return "", err
	}
	if !fullCommit.MatchString(want) {
		return "", fmt.Errorf("support: want 不是 40 位十六进制提交号: %q", want)
	}
	d := shQuote(dir)
	h := shQuote("refs/heads/" + branch)
	s := shQuote(stagingRef(branch))
	w := shQuote(want)
	// worktree list --porcelain 里检出该分支的行恰好是 "branch refs/heads/<branch>"
	wtLine := shQuote("branch refs/heads/" + branch)
	return strings.Join([]string{
		"set -e",
		"D=" + d,
		// 0. 目录必须是 git 仓库，否则后面每一步的报错都看不出真正原因
		`git -C "$D" rev-parse --git-dir >/dev/null 2>&1 || { echo 'ERR nodir'; exit 6; }`,
		// 1. 暂存引用必须是刚推上去的那个提交
		`if [ "$(git -C "$D" rev-parse --verify -q ` + s + ` || true)" != ` + w + ` ]; then echo 'ERR mismatch'; exit 3; fi`,
		`if ! git -C "$D" rev-parse --verify -q ` + h + ` >/dev/null 2>&1; then`,
		// 2. 分支不存在：直接建出来
		`  git -C "$D" update-ref ` + h + ` ` + w,
		`elif ! git -C "$D" merge-base --is-ancestor ` + h + ` ` + w + `; then`,
		// 3. 本地有独有提交：不能快进
		`  echo 'ERR notff'; exit 4`,
		`elif [ "$(git -C "$D" rev-parse ` + h + `)" = ` + w + ` ]; then`,
		// 4. 已一致：什么都不做——工作区脏不脏与此无关，不该拦同步
		`  :`,
		`elif [ "$(git -C "$D" symbolic-ref -q HEAD || true)" = ` + h + ` ]; then`,
		// 5. 正检出该分支：已跟踪文件必须干净（未跟踪不挡，撞上了 merge --ff-only 自己会拒），再 merge 快进（带动工作区）
		`  if [ -n "$(git -C "$D" status --porcelain --untracked-files=no)" ]; then echo 'ERR dirty'; exit 5; fi`,
		`  git -C "$D" merge --ff-only ` + w,
		`else`,
		// 6. 没检出该分支：先确认没被别的工作区检出、不在 rebase 中（rebase 时 HEAD 是 detached，走的就是这一支）
		`  if git -C "$D" worktree list --porcelain | grep -qxF ` + wtLine + `; then echo 'ERR busy'; exit 7; fi`,
		// rev-parse --git-path 可能返回相对路径，相对 "$D" 解析
		`  rp=$(git -C "$D" rev-parse --git-path rebase-merge)`,
		`  case $rp in /*) ;; *) rp="$D/$rp" ;; esac`,
		`  if [ -d "$rp" ]; then echo 'ERR busy'; exit 7; fi`,
		`  rp=$(git -C "$D" rev-parse --git-path rebase-apply)`,
		`  case $rp in /*) ;; *) rp="$D/$rp" ;; esac`,
		`  if [ -d "$rp" ]; then echo 'ERR busy'; exit 7; fi`,
		// 7. CAS 更新，旧值中途被人动过就会失败
		`  git -C "$D" update-ref ` + h + ` ` + w + ` "$(git -C "$D" rev-parse ` + h + `)"`,
		`fi`,
		// 8. 删除暂存引用（失败忽略），报告结果
		`git -C "$D" update-ref -d ` + s + ` 2>/dev/null || true`,
		`echo "OK $(git -C "$D" rev-parse ` + h + `)"`,
	}, "\n"), nil
}

// ParseFFResult 解析 FFScript 的输出和退出码：OK → 返回提交号；
// ERR dirty→ErrDirty，notff→ErrNotFastForward，mismatch→ErrMismatch，
// nodir→ErrNoRepoDir，busy→ErrRepoBusy；其他 → 带输出前 500 字节的错误。
// 从最后一行往前找结果行：merge 等命令的提示性输出会混在 OK 行之前，不能假设 OK 在第一行。
func ParseFFResult(out []byte, exitCode int) (string, error) {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "OK ") {
			if exitCode != 0 {
				break // 有 OK 行但退出码非 0，不可信，落到通用错误
			}
			commit := strings.TrimSpace(strings.TrimPrefix(l, "OK "))
			if !fullCommit.MatchString(commit) {
				return "", fmt.Errorf("support: OK 后面的不是 40 位提交号: %q", commit)
			}
			return commit, nil
		}
		if strings.HasPrefix(l, "ERR dirty") {
			return "", ErrDirty
		}
		if strings.HasPrefix(l, "ERR notff") {
			return "", ErrNotFastForward
		}
		if strings.HasPrefix(l, "ERR mismatch") {
			return "", ErrMismatch
		}
		if strings.HasPrefix(l, "ERR nodir") {
			return "", ErrNoRepoDir
		}
		if strings.HasPrefix(l, "ERR busy") {
			return "", ErrRepoBusy
		}
	}
	msg := out
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return "", fmt.Errorf("support: 快进失败（退出码 %d）: %s", exitCode, msg)
}
