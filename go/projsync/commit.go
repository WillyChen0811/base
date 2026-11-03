package projsync

// 自动提交（PLAN-sync-v2.md 第十四、十五节）：agent 状态确定做完后，把工作区的未提交改动提交成一个正常提交。
//
// 与同步更新（apply.go）的区别：
//   - 用正常的 `git add -A` + `git commit`，**不禁用钩子**、不加 --no-verify，仓库自己的检查照常运行；
//   - 提交身份用仓库/用户自己的 git 配置，不固定 XXYI 身份；没配置导致失败就按失败返回，不自动补；
//   - 失败（含钩子拒绝）不重试、不绕过、不撤销已暂存内容（不 reset），完整输出交给调用方。
//
// 任何一步检查失败都不写。调用方必须对同一仓库串行（同 apply.go 第 5 条）。
// 本文件不做调度、不做状态接口。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// CommitPlan 是一次自动提交的完整前提。
type CommitPlan struct {
	RepoDir    string // 主工作区根（用于 CheckScope）
	Worktree   string // 要提交的工作区根，可以是 linked worktree
	Branch     string // 该工作区当前检出的分支（短名），用于核对
	ExpectHead string // 调用方采集时看到的 HEAD OID
	Message    string // 可选；为空时自动生成
	// Force：用户在界面点「提交并同步」时为 true，跳过 judge，其余检查照常。
	Force bool
}

// CommitJudge 判定 worktree 里的 agent 是否已做完（finished=true 才允许自动提交）；why 是判定依据。
type CommitJudge func(ctx context.Context, worktree string) (finished bool, why string)

var (
	// ErrNotFinished：agent 状态不是「已做完」，未写任何东西。
	ErrNotFinished = errors.New("agent 状态不是已做完，未提交")
	// ErrNothingToCommit：工作区没有未提交改动。不算失败，调用方可忽略。
	ErrNothingToCommit = errors.New("工作区没有未提交的改动")
)

// ErrCommittedUncertain：提交已经完成，但事后核对发现现场与预期不符（新 HEAD 读不到、父提交不符、
// HEAD 已不在该分支）。调用方必须停止该项目后续写动作，并报告「已提交、现场需确认」。
var ErrCommittedUncertain = errors.New("已提交、现场需确认")

// MaxCommitOutput 是 ErrCommitFailed.Output 的上限。
const MaxCommitOutput = 64 << 10

// ErrCommitFailed 表示 git add / git commit 失败（含钩子拒绝）。不重试；已暂存的内容不撤销。
type ErrCommitFailed struct {
	Stage  string // "add" 或 "commit"
	Code   int    // git 退出码，-1 表示未能得到
	Output string // stderr + stdout 完整原文，上限 MaxCommitOutput
}

func (e *ErrCommitFailed) Error() string {
	note := "改动已暂存但未提交（未撤销，需人工处理）"
	if e.Stage == "add" {
		note = "部分改动可能已暂存、未提交（未撤销，需人工处理）"
	}
	return fmt.Sprintf("自动提交失败（git %s，退出码 %d）：%s。git 输出：\n%s", e.Stage, e.Code, note, e.Output)
}

// runGitFull 与 runGitCode 的环境处理一致（清继承 GIT_*、GIT_TERMINAL_PROMPT=0、GIT_NO_REPLACE_OBJECTS=1），
// 但保留完整的 stderr+stdout（上限 MaxCommitOutput），用于把钩子/命令的完整输出交给用户。不禁用钩子。
func runGitFull(ctx context.Context, dir string, args ...string) (output string, code int, err error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cleanEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	all := append(append([]byte{}, stderr.Bytes()...), stdout.Bytes()...)
	if len(all) > MaxCommitOutput {
		total := len(all)
		all = append(all[:MaxCommitOutput:MaxCommitOutput], []byte(fmt.Sprintf("…（已截断，原文共 %d 字节）", total))...)
	}
	output = strings.ToValidUTF8(string(all), "�")
	if err == nil {
		return output, 0, nil
	}
	code = -1
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		code = ee.ExitCode()
	}
	return output, code, err
}

// statusEntries 解析 `git status --porcelain=v2 -z --untracked-files=all` 的原始字节，返回改动路径（按出现顺序）、
// 是否有未合并条目、是否有以 / 结尾的未跟踪条目（嵌套仓库，git add 会把它变成 gitlink）。
func statusEntries(raw []byte) (paths []string, unmerged bool, nested []string) {
	recs := strings.Split(string(raw), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		switch {
		case strings.HasPrefix(r, "1 "):
			if f := strings.SplitN(r, " ", 9); len(f) == 9 {
				paths = append(paths, f[8])
			}
		case strings.HasPrefix(r, "2 "):
			if f := strings.SplitN(r, " ", 10); len(f) == 10 {
				paths = append(paths, f[9])
			}
			i++ // 下一条记录是原路径
		case strings.HasPrefix(r, "u "):
			unmerged = true
			if f := strings.SplitN(r, " ", 11); len(f) == 11 {
				paths = append(paths, f[10])
			}
		case strings.HasPrefix(r, "? "):
			p := r[2:]
			if strings.HasSuffix(p, "/") {
				nested = append(nested, p)
			}
			paths = append(paths, p)
		}
	}
	return
}

// defaultCommitMessage：「projsync 自动提交：<最多 10 个文件名>」，超出写「等 N 个」。名字里的控制字符与非法 UTF-8 做替换。
func defaultCommitMessage(paths []string) string {
	const maxNames = 10
	show := paths
	if len(show) > maxNames {
		show = show[:maxNames]
	}
	clean := make([]string, len(show))
	for i, p := range show {
		p = strings.ToValidUTF8(p, "�")
		clean[i] = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return '?'
			}
			return r
		}, p)
	}
	msg := "projsync 自动提交：" + strings.Join(clean, "、")
	if len(paths) > maxNames {
		msg += fmt.Sprintf(" 等 %d 个", len(paths))
	}
	return msg
}

func sameDir(a, b string) bool {
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && filepath.Clean(ra) == filepath.Clean(rb)
}

// ValidateCommitPlan 做不碰仓库的字段校验。
func ValidateCommitPlan(p CommitPlan) error {
	for name, v := range map[string]string{"RepoDir": p.RepoDir, "Worktree": p.Worktree} {
		if v == "" || !filepath.IsAbs(v) || strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
			return fmt.Errorf("自动提交：%s 必须是合法的绝对路径: %q", name, v)
		}
	}
	if p.Branch == "" || strings.HasPrefix(p.Branch, "-") || !utf8.ValidString(p.Branch) || strings.ContainsRune(p.Branch, 0) {
		return fmt.Errorf("自动提交：Branch 不合法: %q", p.Branch)
	}
	if !oidRe.MatchString(p.ExpectHead) {
		return fmt.Errorf("自动提交：ExpectHead 不是合法 OID: %q", p.ExpectHead)
	}
	if len(p.Message) > 4096 || !utf8.ValidString(p.Message) || strings.ContainsRune(p.Message, 0) {
		return errors.New("自动提交：Message 过长（>4096 字节）或不是合法 UTF-8")
	}
	return nil
}

// AutoCommit 提交 p.Worktree 里的未提交改动，返回新提交 OID。
// 任何一步检查失败都不写；返回的 error 带中文原因，可用 errors.Is / errors.As 判断
// ErrNotFinished、ErrNothingToCommit、ErrRefsChanged、*ErrOutOfScope、*ErrCommitFailed。
func AutoCommit(ctx context.Context, p CommitPlan, judge CommitJudge) (newOID string, err error) {
	if err := ValidateCommitPlan(p); err != nil {
		return "", err
	}
	tag := fmt.Sprintf("自动提交 %s（分支 %s）", p.Worktree, p.Branch)

	// a) 范围：仓库 + 分支；工作区自己也按「工作区根」再检一次（常规布局、索引状态）
	if err := CheckScope(ctx, p.RepoDir, nil, []string{p.Branch}); err != nil {
		return "", fmt.Errorf("%s：%w", tag, err)
	}
	if !sameDir(p.RepoDir, p.Worktree) {
		if err := CheckScope(ctx, p.Worktree, nil, []string{p.Branch}); err != nil {
			return "", fmt.Errorf("%s：%w", tag, err)
		}
	}

	// b) 现场核对：Worktree 必须是该仓库的非 bare 工作区；HEAD 符号引用与 OID 与采集时一致；全部工作区无进行中操作
	wts, err := listWorktrees(ctx, p.RepoDir, nil)
	if err != nil {
		return "", fmt.Errorf("%s：%w，停止", tag, err)
	}
	var target *WorktreeInfo
	for i := range wts {
		if !wts[i].Bare && sameDir(wts[i].Path, p.Worktree) {
			target = &wts[i]
		}
	}
	if target == nil {
		return "", fmt.Errorf("%s：%s 不是该仓库的工作区，停止", tag, p.Worktree)
	}
	sym, err := runGit(ctx, p.Worktree, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return "", fmt.Errorf("%s：工作区 HEAD 不是分支（游离或读取失败），停止: %w", tag, err)
	}
	if got := strings.TrimSuffix(string(sym), "\n"); got != "refs/heads/"+p.Branch {
		return "", fmt.Errorf("%s：%w：工作区当前分支是 %s，预期 refs/heads/%s", tag, ErrRefsChanged, got, p.Branch)
	}
	head, err := runGit(ctx, p.Worktree, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s：读取 HEAD 失败，停止: %w", tag, err)
	}
	if got := strings.TrimSpace(string(head)); got != p.ExpectHead {
		return "", fmt.Errorf("%s：%w（HEAD 当前 %s，预期 %s），需重新判定", tag, ErrRefsChanged, got, p.ExpectHead)
	}
	if err := (ApplyPlan{RepoDir: p.RepoDir, Branch: p.Branch}).requireAllIdle(ctx, wts); err != nil {
		return "", err
	}

	// c) agent 状态
	if !p.Force {
		if judge == nil {
			return "", fmt.Errorf("%s：未提供 agent 状态判定，%w", tag, ErrNotFinished)
		}
		finished, why := judge(ctx, p.Worktree)
		if !finished {
			return "", fmt.Errorf("%s：%w：%s", tag, ErrNotFinished, why)
		}
	}

	// d) 有改动吗？同时取得改动文件名（原始字节，-z）
	raw, err := runGit(ctx, p.Worktree, []string{"GIT_OPTIONAL_LOCKS=0"},
		"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return "", fmt.Errorf("%s：读取工作区状态失败，停止: %w", tag, err)
	}
	if len(raw) == 0 {
		return "", ErrNothingToCommit
	}
	paths, unmerged, nested := statusEntries(raw)
	if unmerged {
		return "", fmt.Errorf("%s：工作区有未合并（冲突）条目，停止", tag)
	}
	if len(nested) > 0 {
		return "", fmt.Errorf("%s：%w", tag, &ErrOutOfScope{Repo: p.RepoDir, Branch: p.Branch,
			Reasons: []string{fmt.Sprintf("未跟踪目录 %q 是嵌套仓库，git add 会把它当子模块，超出自动同步范围", nested[0])}})
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("%s：状态非空但解析不出改动文件，停止: %q", tag, raw)
	}
	msg := p.Message
	if msg == "" {
		msg = defaultCommitMessage(paths)
	}

	// e)(f) 正常的 add + commit，钩子照常运行；失败不重试、不撤销
	if out, code, err := runGitFull(ctx, p.Worktree, "add", "-A"); err != nil {
		return "", &ErrCommitFailed{Stage: "add", Code: code, Output: out}
	}
	if out, code, err := runGitFull(ctx, p.Worktree, "commit", "-m", msg); err != nil {
		return "", &ErrCommitFailed{Stage: "commit", Code: code, Output: out}
	}

	// g) 核对新提交：HEAD 仍在该分支上、唯一父提交 == ExpectHead
	out, err := runGit(ctx, p.Worktree, nil, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		return "", fmt.Errorf("%s：提交已完成但读取新 HEAD 失败，%w: %w", tag, ErrCommittedUncertain, err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 || !oidRe.MatchString(f[0]) || f[1] != p.ExpectHead {
		return "", fmt.Errorf("%s：提交已完成，但新提交的父提交与预期 %s 不符（%q），%w", tag, p.ExpectHead, f, ErrCommittedUncertain)
	}
	// 提交后再读一次 HEAD 的符号引用：必须仍在该分支上（提交过程中钩子/他人切走分支则现场需确认）
	sym2, err := runGit(ctx, p.Worktree, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return "", fmt.Errorf("%s：提交已完成（%s），但提交后读取 HEAD 符号引用失败，%w: %w", tag, f[0], ErrCommittedUncertain, err)
	}
	if got := strings.TrimSuffix(string(sym2), "\n"); got != "refs/heads/"+p.Branch {
		return "", fmt.Errorf("%s：提交已完成（%s），但提交后工作区 HEAD 指向 %s，预期 refs/heads/%s，%w", tag, f[0], got, p.Branch, ErrCommittedUncertain)
	}
	return f[0], nil
}
