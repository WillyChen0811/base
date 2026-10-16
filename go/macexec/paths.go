// Package macexec 提供 Mac 执行端的纯逻辑：仓库路径校验、命令任务执行与动作映射，
// 不依赖 HTTP，也不依赖 support 包。
package macexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRepos 是默认的仓库名单；调用方也可以给 RepoRoot 传自己的名单。
var DefaultRepos = []string{"xxyi", "base", "websubmit", "ai-browser"}

// RepoRoot 只认 allowed 里的名字，返回 <home>/xxyi-work/<name>（目录须存在）。
func RepoRoot(home, name string, allowed []string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("仓库名不合法: %q", name)
	}
	ok := false
	for _, a := range allowed {
		if a == name {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("仓库 %q 不在允许名单里（允许: %s）", name, strings.Join(allowed, ", "))
	}
	root := filepath.Join(home, "xxyi-work", name)
	st, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("仓库目录不可用 %q: %w", root, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("仓库路径不是目录: %q", root)
	}
	return root, nil
}

// ResolveInRepo 把仓库内相对路径解析成绝对路径：拒绝空、绝对路径、含 ".." 段、含 ".git" 段；
// filepath.EvalSymlinks 之后仍须在 root（同样 EvalSymlinks 过）之内，防符号链接逃逸。
func ResolveInRepo(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("只接受仓库内相对路径，收到绝对路径: %q", rel)
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == ".." {
			return "", fmt.Errorf("路径不允许包含 \"..\" 段: %q", rel)
		}
		if seg == ".git" {
			return "", fmt.Errorf("路径不允许访问 .git: %q", rel)
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("仓库根目录无法转成绝对路径 %q: %w", root, err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("仓库根目录解析失败 %q: %w", absRoot, err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(absRoot, rel))
	if err != nil {
		return "", fmt.Errorf("路径解析失败（不存在或断链）%q: %w", rel, err)
	}
	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越出仓库: %q", rel)
	}
	return resolved, nil
}
