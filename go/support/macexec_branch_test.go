package support

// /exec/run 的 branch 参数：用临时目录里的真实 git 仓库（裸仓当 Remote + 克隆当 xxyi 仓库）测专用 worktree 流程。
// 动作用 build_app（跑提交里的 mac/build.sh），不真跑 swift。

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// branchFixture 是 branch 测试的现场：dev 是往 remote 推分支的开发仓，xxyi 是 Mac 上的主工作区。
type branchFixture struct {
	base, token string
	dev, remote string
	xxyi        string // <home>/xxyi-work/xxyi
	worktree    string // <home>/xxyi-work/.xxyi-branch-build
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeIn(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// pushBranchVersion 在 dev 里把 branch（不存在就从 master 建）改成 VERSION=ver 并推到 remote。
func (f *branchFixture) pushBranchVersion(t *testing.T, branch, ver string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", f.dev, "rev-parse", "--verify", "-q", "refs/heads/"+branch).CombinedOutput(); err != nil {
		_ = out
		gitIn(t, f.dev, "checkout", "-b", branch, "master")
	} else {
		gitIn(t, f.dev, "checkout", branch)
	}
	writeIn(t, f.dev, "VERSION", ver+"\n")
	gitIn(t, f.dev, "add", "-A")
	gitIn(t, f.dev, "commit", "-m", "version "+ver)
	gitIn(t, f.dev, "push", f.remote, "HEAD")
	gitIn(t, f.dev, "checkout", "master")
}

func newBranchFixture(t *testing.T) *branchFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dev, remote := setupTestRepos(t)
	writeIn(t, dev, "mac/build.sh", "#!/bin/sh\necho built-$(cat ../VERSION)\n")
	writeIn(t, dev, "VERSION", "master\n")
	writeIn(t, dev, "README", "readme\n")
	writeIn(t, dev, ".gitignore", ".build/\n")
	gitIn(t, dev, "add", "-A")
	gitIn(t, dev, "commit", "-m", "init")
	pushDefault(t, dev, remote)

	xxyi := filepath.Join(home, "xxyi-work", "xxyi")
	if err := os.MkdirAll(filepath.Dir(xxyi), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "clone", remote, xxyi).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	s, fr := newTestService(t, Config{Repos: map[string]Repo{"xxyi": {Dir: xxyi, Remote: remote}}})
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return &branchFixture{
		base: "http://" + s.Status().RemoteAddr, token: fr.publishRec(0).token,
		dev: dev, remote: remote, xxyi: xxyi,
		worktree: filepath.Join(home, "xxyi-work", ".xxyi-branch-build"),
	}
}

// run 发 /exec/run，返回状态码、tail 与错误文字。
func (f *branchFixture) run(t *testing.T, body string) (int, string, string) {
	t.Helper()
	code, b := remoteDo(t, http.MethodPost, f.base+"/exec/run", f.token, []byte(body))
	var r struct {
		Tail  string `json:"tail"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("响应不是 JSON: %s", b)
	}
	return code, r.Tail, r.Error
}

func TestExecRunBranchWorktree(t *testing.T) {
	f := newBranchFixture(t)
	f.pushBranchVersion(t, "feat/a", "v1")

	// 主工作区有未提交改动（跟踪文件修改 + 未跟踪文件），编译别的分支照样能跑。
	writeIn(t, f.xxyi, "README", "dirty\n")
	writeIn(t, f.xxyi, "scratch.txt", "mine\n")
	statusBefore := gitIn(t, f.xxyi, "status", "--porcelain")
	headBefore := gitIn(t, f.xxyi, "rev-parse", "HEAD")

	// 首次：建 worktree
	code, tail, errMsg := f.run(t, `{"action":"build_app","branch":"feat/a"}`)
	if code != http.StatusOK {
		t.Fatalf("首次应 200，得 %d: %s", code, errMsg)
	}
	commit1 := strings.TrimSpace(gitIn(t, f.remote, "rev-parse", "feat/a"))
	wantHeader := "分支 feat/a @ " + commit1[:12] + "，目录 " + f.worktree
	if !strings.HasPrefix(tail, wantHeader+"\n") || !strings.Contains(tail, "built-v1") {
		t.Fatalf("输出应以说明行开头并含 built-v1：%q（期望首行 %q）", tail, wantHeader)
	}
	if got := strings.TrimSpace(gitIn(t, f.worktree, "rev-parse", "HEAD")); got != commit1 {
		t.Fatalf("worktree HEAD = %s，应为 %s", got, commit1)
	}

	// 第二次：复用同一个目录并切到新提交；被忽略的 .build 保留，未跟踪垃圾被清掉。
	writeIn(t, f.worktree, "mac/.build/cache", "keep\n")
	writeIn(t, f.worktree, "junk.txt", "x\n")
	f.pushBranchVersion(t, "feat/a", "v2")
	code, tail, errMsg = f.run(t, `{"action":"build_app","branch":"feat/a"}`)
	if code != http.StatusOK || !strings.Contains(tail, "built-v2") {
		t.Fatalf("第二次应 200 且含 built-v2，得 %d %q %s", code, tail, errMsg)
	}
	commit2 := strings.TrimSpace(gitIn(t, f.remote, "rev-parse", "feat/a"))
	if got := strings.TrimSpace(gitIn(t, f.worktree, "rev-parse", "HEAD")); got != commit2 || commit2 == commit1 {
		t.Fatalf("worktree HEAD = %s，应切到新提交 %s", got, commit2)
	}
	if _, err := os.Stat(filepath.Join(f.worktree, "mac", ".build", "cache")); err != nil {
		t.Errorf(".build 缓存应保留：%v", err)
	}
	if _, err := os.Stat(filepath.Join(f.worktree, "junk.txt")); !os.IsNotExist(err) {
		t.Errorf("未跟踪文件应被 clean 掉：%v", err)
	}

	// 主工作区：分支、HEAD、改动、文件内容都没动，也没有本地 feat/a 分支。
	if got := gitIn(t, f.xxyi, "status", "--porcelain"); got != statusBefore {
		t.Errorf("主工作区 status 变了：%q -> %q", statusBefore, got)
	}
	if got := gitIn(t, f.xxyi, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("主工作区 HEAD 变了")
	}
	if got := strings.TrimSpace(gitIn(t, f.xxyi, "symbolic-ref", "--short", "HEAD")); got != "master" {
		t.Errorf("主工作区分支变了：%s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(f.xxyi, "README")); string(b) != "dirty\n" {
		t.Errorf("主工作区未提交改动被动了：%q", b)
	}
	if out, err := exec.Command("git", "-C", f.xxyi, "rev-parse", "--verify", "-q", "refs/heads/feat/a").CombinedOutput(); err == nil {
		t.Errorf("不该创建本地分支 feat/a：%s", out)
	}

	// 不带 branch：行为不变，仍跑主工作区（README dirty 在，VERSION 是 master）。
	code, tail, errMsg = f.run(t, `{"action":"build_app"}`)
	if code != http.StatusOK || !strings.Contains(tail, "built-master") || strings.Contains(tail, "分支 ") {
		t.Fatalf("无 branch 应编主工作区且无说明行，得 %d %q %s", code, tail, errMsg)
	}
}

func TestExecRunBranchBadRequests(t *testing.T) {
	f := newBranchFixture(t)
	for _, br := range []string{"a..b", "-x", "feat/", "a b", "x@{1}"} {
		body, _ := json.Marshal(map[string]string{"action": "build_app", "branch": br})
		if code, _, errMsg := f.run(t, string(body)); code != http.StatusBadRequest {
			t.Errorf("非法分支名 %q 应 400，得 %d %s", br, code, errMsg)
		}
	}
	if code, _, errMsg := f.run(t, `{"action":"git_status","repo":"xxyi","branch":"master"}`); code != http.StatusBadRequest || !strings.Contains(errMsg, "branch") {
		t.Errorf("git_status 带 branch 应 400，得 %d %s", code, errMsg)
	}
	if code, _, _ := f.run(t, `{"action":"build_app","repo":"base","branch":"master"}`); code != http.StatusBadRequest {
		t.Errorf("branch 配别的仓库应 400，得 %d", code)
	}
	// 远端没有的分支：fetch 那一步失败，502，且不留 worktree。
	code, _, errMsg := f.run(t, `{"action":"build_app","branch":"nope"}`)
	if code != http.StatusBadGateway || !strings.Contains(errMsg, "fetch") {
		t.Errorf("不存在的分支应 502 并点明 fetch，得 %d %s", code, errMsg)
	}
	if _, err := os.Stat(f.worktree); !os.IsNotExist(err) {
		t.Errorf("fetch 失败不该建 worktree：%v", err)
	}
}

func TestExecRunBranchDirOccupied(t *testing.T) {
	f := newBranchFixture(t)
	f.pushBranchVersion(t, "feat/a", "v1")
	// 专用目录被残留的普通目录占着：报错，且不删里面的东西。
	writeIn(t, f.worktree, "precious.txt", "do not delete\n")
	code, _, errMsg := f.run(t, `{"action":"build_app","branch":"feat/a"}`)
	if code != http.StatusBadGateway || !strings.Contains(errMsg, "worktree") {
		t.Fatalf("目录被占用应 502 并点明 worktree，得 %d %s", code, errMsg)
	}
	if _, err := os.Stat(filepath.Join(f.worktree, "precious.txt")); err != nil {
		t.Errorf("不该动被占用目录里的文件：%v", err)
	}
}

// 专用 worktree 被人整个删掉后（登记还在）：prune 后重建，照常能编。
func TestExecRunBranchWorktreeDeleted(t *testing.T) {
	f := newBranchFixture(t)
	f.pushBranchVersion(t, "feat/a", "v1")
	if code, _, errMsg := f.run(t, `{"action":"build_app","branch":"feat/a"}`); code != http.StatusOK {
		t.Fatalf("首次应 200，得 %d %s", code, errMsg)
	}
	if err := os.RemoveAll(f.worktree); err != nil {
		t.Fatal(err)
	}
	code, tail, errMsg := f.run(t, `{"action":"build_app","branch":"feat/a"}`)
	if code != http.StatusOK || !strings.Contains(tail, "built-v1") {
		t.Fatalf("目录被删后应能重建，得 %d %q %s", code, tail, errMsg)
	}
}
