package projsync

// 支持范围检查（PLAN-sync-v2.md 第十一节：识别边界、超出就停）。
//
// 【本检查是「白名单」】仓库状态必须【完全】落在下列范围内才允许执行器写；
// 任何不认识、不确定、读取失败的状态一律判「超出自动同步范围：<原因>」，不写任何东西。
// 宁可误判为超出范围，也不能把超出范围的状态当成在范围内。
// 【以后发现的新边角，一律先在这里加一条「超出范围」规则，而不是在执行器里加专门处理。】
//
// 范围清单（CheckScope 一次收集全部原因再返回）：
//  a) Git 版本 ≥ 2.38（已验证范围）；版本串解析失败也算超出。
//  b) 分支名：合法 UTF-8、NFC、git check-ref-format --branch 通过、不含 '='（配置键歧义）、不以 '-' 开头。
//  c) 本仓库内没有「NFC 规范化后相同但字节不同」的两个分支（待写分支也计入）。
//     跨两台机器的比较由调用方把两边分支名 MergeBranchNames 后再 NormalizationClashes。
//  d) 分支是直接引用（非符号引用，含悬空符号引用）。
//  e) 分支没有 branch.<名>.mergeOptions / branch.<名>.rebase。仓库级 merge.* / pull.* 不查（执行器命令行已覆盖）。
//  f) 没有 sparse-checkout；索引没有 skip-worktree / assume-unchanged 条目（每个工作区各查）。
//  g) 常规布局：主工作区 git 目录是 <工作区>/.git 目录；linked worktree（git worktree add 的标准布局）允许；
//     --separate-git-dir、core.worktree、bare、调用方环境带 GIT_DIR/GIT_WORK_TREE 等 => 超出。工作区总数 ≤ maxWorktrees。
//  h) 子进程环境清掉继承的 GIT_*（cleanEnv；git.go 的 runGitCode 是所有 git 调用的唯一入口，读写命令都经过它）。
//     因此继承的 GIT_DIR 等对执行器无效（选择：清掉后按正确仓库工作，而非判超出）；
//     但调用方显式经 env 参数传入的布局类变量（GIT_DIR 等）无法确认意图，判超出。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxWorktrees：工作区总数（含主工作区）上限。超过说明仓库被用在预期之外的方式（大量并行工作区），
// 枚举与检查的假设不再可靠，故判超出范围。
const maxWorktrees = 8

// 最低 Git 版本。
const minGitMajor, minGitMinor = 2, 38

// ErrOutOfScope 表示仓库/分支状态超出自动同步范围，未写任何东西。
type ErrOutOfScope struct {
	Repo    string
	Branch  string // 本次检查涉及的分支（多个以逗号分隔）
	Reasons []string
}

func (e *ErrOutOfScope) Error() string {
	return "超出自动同步范围：" + strings.Join(e.Reasons, "；")
}

// cleanEnv 返回去掉全部 GIT_* 变量的 base 副本（执行器自己注入的变量在其后追加，不经过这里）。
func cleanEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

var gitVersionRe = regexp.MustCompile(`^git version (\d+)\.(\d+)(?:\.(\d+))?`)

// parseGitVersion 解析 `git version 2.39.3 (Apple Git-146)` 一类输出。
func parseGitVersion(s string) (major, minor int, err error) {
	m := gitVersionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, fmt.Errorf("无法解析 Git 版本: %q", s)
	}
	major, e1 := strconv.Atoi(m[1])
	minor, e2 := strconv.Atoi(m[2])
	if e1 != nil || e2 != nil {
		return 0, 0, fmt.Errorf("无法解析 Git 版本: %q", s)
	}
	return major, minor, nil
}

// layoutEnvKeys：调用方 env 里出现这些变量会改变仓库布局/对象来源，一律超出。
var layoutEnvKeys = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_REPLACE_REF_BASE", "GIT_NO_REPLACE_OBJECTS", "GIT_GRAFT_FILE", "GIT_CEILING_DIRECTORIES"}

// 保守的「无组合形式差异」区块（未引入 golang.org/x/text，见 isSafeRune）。
func isSafeRune(r rune) bool {
	switch {
	case r < 0x80:
		return true
	case r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意文字
		r >= 0x3400 && r <= 0x4DBF, // CJK 扩展 A
		r >= 0x3000 && r <= 0x303F, // 中日韩符号标点
		r >= 0xFF00 && r <= 0xFFEF: // 全角
		return true
	}
	return false
}

// nameNFCProblem 返回分支名在 UTF-8/NFC 方面的问题（空串=无）。
// go.mod 没有 golang.org/x/text、go.sum 与模块缓存里也没有，故不引入新依赖，
// 用保守白名单：非 ASCII 字符只允许落在 isSafeRune 的区块内（这些区块无 NFC/NFD 差异），其余一律判超出。
func nameNFCProblem(name string) string {
	if !utf8.ValidString(name) {
		return "不是合法 UTF-8"
	}
	for _, r := range name {
		if r == utf8.RuneError || !isSafeRune(r) {
			return fmt.Sprintf("含无法确认已是 NFC 形式的字符 U+%04X", r)
		}
	}
	return ""
}

// latin1Decomp：Latin-1 带音标大写字母的规范分解（基字母 + 组合符），小写由代码推导。
// 仅用于 NormalizationClashes 的键，使 é 与 e+U+0301 得到同一个键；其它字符原样。
var latin1Decomp = func() map[rune]string {
	up := map[rune]string{
		'À': "À", 'Á': "Á", 'Â': "Â", 'Ã': "Ã", 'Ä': "Ä", 'Å': "Å", 'Ç': "Ç",
		'È': "È", 'É': "É", 'Ê': "Ê", 'Ë': "Ë", 'Ì': "Ì", 'Í': "Í", 'Î': "Î", 'Ï': "Ï",
		'Ñ': "Ñ", 'Ò': "Ò", 'Ó': "Ó", 'Ô': "Ô", 'Õ': "Õ", 'Ö': "Ö",
		'Ù': "Ù", 'Ú': "Ú", 'Û': "Û", 'Ü': "Ü", 'Ý': "Ý",
	}
	m := map[rune]string{'ÿ': "ÿ"}
	for r, d := range up {
		m[r] = d
		m[unicode.ToLower(r)] = strings.ToLower(d[:1]) + d[1:]
	}
	return m
}()

// normKey 近似 NFD 键：覆盖 Latin-1 precomposed 字母；其余原样。
// 判定「键相同而字节不同」是 NFC 相等的近似（未覆盖的字符对不会被这里发现，
// 但它们含非白名单字符，作为待写分支时会被 nameNFCProblem 拦下）。
func normKey(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if d, ok := latin1Decomp[r]; ok {
			sb.WriteString(d)
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// MergeBranchNames 合并两台机器的分支名（去重、排序），供调用方做跨机器的 NormalizationClashes。
func MergeBranchNames(mac, ubuntu []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]string{mac, ubuntu} {
		for _, n := range l {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// NormalizationClashes 返回「规范化后相同但字节不同」的名字对（每对按字节序排列，整体有序）。
func NormalizationClashes(names []string) [][2]string {
	uniq := MergeBranchNames(names, nil)
	groups := map[string][]string{}
	var keys []string
	for _, n := range uniq {
		k := normKey(n)
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], n)
	}
	sort.Strings(keys)
	var out [][2]string
	for _, k := range keys {
		g := groups[k]
		for i := 0; i < len(g); i++ {
			for j := i + 1; j < len(g); j++ {
				out = append(out, [2]string{g[i], g[j]})
			}
		}
	}
	return out
}

// CheckScope 检查仓库与本次要写的分支是否都在支持范围内；不在则返回 *ErrOutOfScope（含全部原因）。
// 只做读取，不写。
func CheckScope(ctx context.Context, repoDir string, env []string, branches []string) error {
	var reasons []string
	add := func(f string, a ...any) { reasons = append(reasons, fmt.Sprintf(f, a...)) }
	done := func() error {
		if len(reasons) == 0 {
			return nil
		}
		return &ErrOutOfScope{Repo: repoDir, Branch: strings.Join(branches, ", "), Reasons: reasons}
	}

	// h) 调用方 env 里的布局类变量
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		for _, bad := range layoutEnvKeys {
			if k == bad {
				add("调用方环境设置了 %s（可能指向别处的仓库/布局），无法确认意图", k)
			}
		}
	}
	if len(reasons) > 0 {
		return done() // 在这种环境下继续读仓库可能读到别处，不再往下查
	}
	if repoDir == "" {
		add("缺少仓库路径")
		return done()
	}

	// a) Git 版本
	if out, err := runGit(ctx, repoDir, env, "version"); err != nil {
		add("读取 Git 版本失败: %v", err)
	} else if ma, mi, err := parseGitVersion(string(out)); err != nil {
		add("%v", err)
	} else if ma < minGitMajor || (ma == minGitMajor && mi < minGitMinor) {
		add("Git 版本 %d.%d 低于已验证的最低版本 %d.%d", ma, mi, minGitMajor, minGitMinor)
	}

	// g) 布局 + f) 逐工作区
	wts := checkLayout(ctx, repoDir, env, add)

	// b) 分支名
	for _, b := range branches {
		checkBranchName(ctx, repoDir, env, b, add)
	}

	// c) 规范化冲突（本仓库全部本地分支 + 待写分支）
	local, err := listLocalBranches(ctx, repoDir, env)
	if err != nil {
		add("列出本地分支失败: %v", err)
	} else {
		// 仓库里任何一个本地分支名含白名单外字符 => 无法确认规范化，整个仓库超出（NormalizationClashes 只是辅助）
		for _, n := range local {
			if nameNFCProblem(n) != "" {
				add("仓库里有无法确认规范化的分支名：%s", n)
			}
		}
		for _, pr := range NormalizationClashes(MergeBranchNames(local, branches)) {
			add("分支名 %q 与 %q 规范化后相同但字节不同", pr[0], pr[1])
		}
	}

	// d) 直接引用
	for _, b := range branches {
		if b == "" {
			continue
		}
		sym, err := isSymbolicRef(ctx, repoDir, env, "refs/heads/"+b)
		if err != nil {
			add("分支 %q：%v", b, err)
		} else if sym {
			add("分支 %q 是符号引用", b)
		}
	}

	// e) 改变合并行为的分支配置
	checkBranchConfig(ctx, repoDir, env, branches, add)

	// f) sparse-checkout / skip-worktree / assume-unchanged，每个工作区各查
	for _, w := range wts {
		if w.Bare {
			continue
		}
		checkWorktreeIndexState(ctx, w.Path, env, add)
	}
	return done()
}

func checkBranchName(ctx context.Context, repoDir string, env []string, b string, add func(string, ...any)) {
	if b == "" || strings.HasPrefix(b, "-") {
		add("分支名 %q 不合法（空或以 - 开头）", b)
		return
	}
	if p := nameNFCProblem(b); p != "" {
		add("分支名 %q %s", b, p)
	}
	if strings.Contains(b, "=") {
		add("分支名 %q 含 =（配置键歧义）", b)
	}
	if strings.ContainsRune(b, 0) {
		add("分支名 %q 含 NUL", b)
		return
	}
	out, err := runGit(ctx, repoDir, env, "check-ref-format", "--branch", b)
	if err != nil {
		add("分支名 %q 未通过 git check-ref-format --branch", b)
	} else if strings.TrimSuffix(string(out), "\n") != b {
		add("分支名 %q 经 check-ref-format --branch 展开为 %q（非普通分支名）", b, strings.TrimSuffix(string(out), "\n"))
	}
}

func checkBranchConfig(ctx context.Context, repoDir string, env []string, branches []string, add func(string, ...any)) {
	out, code, err := runGitCode(ctx, repoDir, env, "config", "--null", "--get-regexp", `^branch\.`)
	if err != nil {
		if code == 1 { // 没有任何 branch.* 配置
			return
		}
		add("读取 branch.* 配置失败: %v", err)
		return
	}
	for _, rec := range strings.Split(string(out), "\x00") {
		key, _, _ := strings.Cut(rec, "\n")
		if key == "" {
			continue
		}
		for _, b := range branches {
			pre := "branch." + b + "."
			if !strings.HasPrefix(key, pre) {
				continue
			}
			switch strings.ToLower(strings.TrimPrefix(key, pre)) {
			case "mergeoptions", "rebase":
				add("分支 %q 配置了 %s（改变合并行为）", b, key)
			}
		}
	}
}

func checkWorktreeIndexState(ctx context.Context, wt string, env []string, add func(string, ...any)) {
	out, code, err := runGitCode(ctx, wt, env, "config", "--type=bool", "--get", "core.sparseCheckout")
	if err != nil {
		if code != 1 {
			add("工作区 %s：读取 core.sparseCheckout 失败: %v", wt, err)
		}
	} else if strings.TrimSpace(string(out)) == "true" {
		add("工作区 %s 启用了 sparse-checkout", wt)
	}
	if p, err := runGit(ctx, wt, env, "rev-parse", "--git-path", "info/sparse-checkout"); err != nil {
		add("工作区 %s：定位 sparse-checkout 文件失败: %v", wt, err)
	} else {
		path := strings.TrimSuffix(string(p), "\n")
		if !filepath.IsAbs(path) {
			path = filepath.Join(wt, path)
		}
		if fi, err := os.Stat(path); err == nil {
			if fi.Size() > 0 || fi.IsDir() {
				add("工作区 %s 存在非空的 sparse-checkout 文件", wt)
			}
		} else if !os.IsNotExist(err) {
			add("工作区 %s：检查 sparse-checkout 文件失败: %v", wt, err)
		}
	}
	lv, err := runGit(ctx, wt, env, "ls-files", "-v", "-z")
	if err != nil {
		add("工作区 %s：读取索引标记失败: %v", wt, err)
		return
	}
	var skip, assume int
	for _, rec := range strings.Split(string(lv), "\x00") {
		if rec == "" {
			continue
		}
		c := rec[0]
		switch {
		case c == 'S':
			skip++
		case c >= 'a' && c <= 'z':
			assume++ // 小写 = assume-unchanged（s=同时 skip-worktree）
		}
	}
	if skip > 0 {
		add("工作区 %s 的索引有 %d 个 skip-worktree 条目", wt, skip)
	}
	if assume > 0 {
		add("工作区 %s 的索引有 %d 个 assume-unchanged 条目", wt, assume)
	}
}

// checkLayout 检查 g)：返回枚举到的工作区（失败时可能为 nil）。
func checkLayout(ctx context.Context, repoDir string, env []string, add func(string, ...any)) []WorktreeInfo {
	wts, err := listWorktrees(ctx, repoDir, env)
	if err != nil {
		add("无法枚举工作区: %v", err)
		return nil
	}
	if len(wts) > maxWorktrees {
		add("工作区共 %d 个，超过上限 %d（超出预期使用方式）", len(wts), maxWorktrees)
	}
	main := wts[0]
	if main.Bare {
		add("主工作区是 bare 仓库")
		return wts
	}
	real := func(p string) (string, error) { return filepath.EvalSymlinks(p) }
	mainGit := filepath.Join(main.Path, ".git")
	if fi, err := os.Lstat(mainGit); err != nil {
		add("主工作区的 .git 不存在或不可读（独立 git 目录等特殊布局）: %v", err)
		return wts
	} else if !fi.IsDir() {
		add("主工作区的 %s 不是 .git 目录（独立 git 目录或符号链接等特殊布局）", mainGit)
		return wts
	}
	wantCommon, err1 := real(mainGit)
	cm, err2 := runGit(ctx, main.Path, env, "rev-parse", "--path-format=absolute", "--git-common-dir")
	gotCommon, err3 := real(strings.TrimSuffix(string(cm), "\n"))
	if err1 != nil || err2 != nil || err3 != nil {
		add("无法确认共用 git 目录: %v %v %v", err1, err2, err3)
		return wts
	}
	if gotCommon != wantCommon {
		add("共用 git 目录 %s 不是主工作区的 .git（%s）", gotCommon, wantCommon)
	}
	if _, code, err := runGitCode(ctx, main.Path, env, "config", "--get", "core.worktree"); err == nil {
		add("配置了 core.worktree（非常规布局）")
	} else if code != 1 {
		add("读取 core.worktree 失败: %v", err)
	}
	for _, w := range wts[1:] {
		if w.Bare {
			add("工作区 %s 是 bare", w.Path)
			continue
		}
		if fi, err := os.Lstat(filepath.Join(w.Path, ".git")); err != nil || !fi.Mode().IsRegular() {
			add("linked 工作区 %s 的 .git 不是标准文件: %v", w.Path, err)
			continue
		}
		gd, err := runGit(ctx, w.Path, env, "rev-parse", "--absolute-git-dir")
		if err != nil {
			add("读取工作区 %s 的 git 目录失败: %v", w.Path, err)
			continue
		}
		g, err := real(strings.TrimSuffix(string(gd), "\n"))
		if err != nil || filepath.Dir(g) != filepath.Join(wantCommon, "worktrees") {
			add("linked 工作区 %s 的 git 目录 %s 不在 %s/worktrees 下", w.Path, g, wantCommon)
		}
	}
	// repoDir 必须是某个工作区的根
	if top, err := runGit(ctx, repoDir, env, "rev-parse", "--show-toplevel"); err != nil {
		add("无法确认 %s 的工作区根: %v", repoDir, err)
	} else {
		tp, e := real(strings.TrimSuffix(string(top), "\n"))
		found := false
		for _, w := range wts {
			if wp, e2 := real(w.Path); e == nil && e2 == nil && wp == tp {
				found = true
			}
		}
		if !found {
			add("%s 不是已枚举工作区的根目录", repoDir)
		}
	}
	return wts
}

// scopeCheckDisabledForTest 仅测试使用：让执行器自身的第二层防护（mergeOptions 清空等）仍可被单独测试。
var scopeCheckDisabledForTest bool

// checkScope 是 ApplyPlan 在写原语开头调用的统一入口。
func (p ApplyPlan) checkScope(ctx context.Context) error {
	if scopeCheckDisabledForTest {
		return nil
	}
	err := CheckScope(ctx, p.RepoDir, p.Env, []string{p.Branch})
	var oos *ErrOutOfScope
	if errors.As(err, &oos) {
		return fmt.Errorf("%s：%w", p.tag(), err)
	}
	return err
}
