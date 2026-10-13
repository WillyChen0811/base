package wgtund

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/support"
	"github.com/WillyChen0811/base/go/tmux"
)

func TestRemoteDirOf(t *testing.T) {
	cases := []struct {
		remote string
		want   string
		bad    bool
	}{
		{"ssh://chenwy@ubuntu.example:2222/home/chenwy/repos/xxyi", "/home/chenwy/repos/xxyi", false},
		{"ssh://chenwy@ubuntu.example/home/chenwy/repos/xxyi", "/home/chenwy/repos/xxyi", false},
		{"chenwy@ubuntu.example:/home/chenwy/repos/xxyi", "/home/chenwy/repos/xxyi", false},
		{"chenwy@ubuntu.example:repos/xxyi", "", true}, // 相对路径：FFScript 会引号包住，~ 和相对解析都失效
		{"/local/path/repo", "/local/path/repo", false},
		{"", "", true},
		{"gitolite", "", true},
	}
	for _, c := range cases {
		got, err := remoteDirOf(c.remote)
		if c.bad {
			if err == nil {
				t.Errorf("remoteDirOf(%q) 应报错，得到 %q", c.remote, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("remoteDirOf(%q): %v", c.remote, err)
			continue
		}
		if got != c.want {
			t.Errorf("remoteDirOf(%q) = %q， want %q", c.remote, got, c.want)
		}
	}
	// 相对路径的报错要提示 remote_dir
	if _, err := remoteDirOf("chenwy@h:repos/xxyi"); err == nil || !strings.Contains(err.Error(), "remote_dir") {
		t.Errorf("相对路径报错要提到 remote_dir: %v", err)
	}
}

// deliverRecorder 记录 bridge 的 write/run 调用（只在测试 goroutine 里用）。
type deliverRecorder struct {
	writes map[string][]byte
	order  []string
	cmds   []string
	out    []byte
	code   int
}

func TestMsgBridgeDeliver(t *testing.T) {
	rec := &deliverRecorder{writes: map[string][]byte{}}
	b := &msgBridge{
		run: func(ctx context.Context, cmd string) ([]byte, int, error) {
			rec.cmds = append(rec.cmds, cmd)
			return rec.out, rec.code, nil
		},
		write: func(_ context.Context, p string, data []byte) error {
			rec.writes[p] = data
			rec.order = append(rec.order, p)
			return nil
		},
	}
	m := support.Message{
		ID: "abc123", From: "mac:frank", To: "ubuntu:claude",
		Text: "看下第三行", FileName: "note.txt", File: []byte("patch-data"),
	}
	if err := b.Deliver(context.Background(), m); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	inbox := "xxyi-work/inbox/abc123-note.txt"
	msgPath := ".xxyi-support/msg/abc123.txt"
	if len(rec.order) != 2 || rec.order[0] != inbox || rec.order[1] != msgPath {
		t.Errorf("应依次写附件和正文，得到 %v", rec.order)
	}
	if string(rec.writes[inbox]) != "patch-data" {
		t.Errorf("附件内容不对: %q", rec.writes[inbox])
	}
	wantText := "[来自 mac:frank] 看下第三行（附件：~/" + inbox + "）"
	if got := string(rec.writes[msgPath]); got != wantText {
		t.Errorf("注入文字 = %q，want %q", got, wantText)
	}
	paste, err := tmux.PasteCommand("claude", "xxyi-abc123")
	if err != nil {
		t.Fatal(err)
	}
	wantCmd := "(" + paste + ") < " + tmux.Quote(msgPath) +
		"; rc=$?; rm -f " + tmux.Quote(msgPath) + "; exit $rc"
	if len(rec.cmds) != 1 || rec.cmds[0] != wantCmd {
		t.Errorf("注入命令 = %q\nwant %q", rec.cmds, wantCmd)
	}

	// 没附件：只写正文一个文件，文字不带附件提示
	rec2 := &deliverRecorder{writes: map[string][]byte{}}
	b2 := &msgBridge{
		run: func(ctx context.Context, cmd string) ([]byte, int, error) {
			rec2.cmds = append(rec2.cmds, cmd)
			return nil, 0, nil
		},
		write: func(_ context.Context, p string, data []byte) error {
			rec2.writes[p] = data
			rec2.order = append(rec2.order, p)
			return nil
		},
	}
	if err := b2.Deliver(context.Background(), support.Message{
		ID: "def456", From: "mac:frank", To: "ubuntu:claude", Text: "hi",
	}); err != nil {
		t.Fatalf("Deliver 无附件: %v", err)
	}
	if len(rec2.order) != 1 || rec2.order[0] != ".xxyi-support/msg/def456.txt" {
		t.Errorf("无附件应只写正文: %v", rec2.order)
	}
	if got := string(rec2.writes[".xxyi-support/msg/def456.txt"]); got != "[来自 mac:frank] hi" {
		t.Errorf("无附件文字 = %q", got)
	}

	// 远端非零退出：报错带上地址和输出（会话不存在时 tmux 的报错原样带上）
	rec3 := &deliverRecorder{writes: map[string][]byte{}, out: []byte("can't find session: claude"), code: 1}
	b3 := &msgBridge{
		run: func(ctx context.Context, cmd string) ([]byte, int, error) { return rec3.out, rec3.code, nil },
		write: func(_ context.Context, p string, data []byte) error {
			rec3.writes[p] = data
			return nil
		},
	}
	err = b3.Deliver(context.Background(), support.Message{
		ID: "ghi789", From: "mac:frank", To: "ubuntu:claude", Text: "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "注入 ubuntu:claude 失败") || !strings.Contains(err.Error(), "can't find session") {
		t.Fatalf("非零退出应报注入失败并带输出，得到 %v", err)
	}
}

// ── 真 git 仓库的 Sync ──────────────────────────────

func gitSh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Tester", "-c", "user.email=tester@example.com"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitCommit(t *testing.T, dir, msg string) string {
	t.Helper()
	gitSh(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return gitSh(t, dir, "rev-parse", "HEAD")
}

// newRepoPair 建 mac 仓库（main 一个提交）和它的 clone（ubuntu），两边同起点。
func newRepoPair(t *testing.T) (mac, ubuntu string) {
	t.Helper()
	tmp := t.TempDir()
	mac = filepath.Join(tmp, "mac")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", mac).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	gitCommit(t, mac, "c1")
	ubuntu = filepath.Join(tmp, "ubuntu")
	if out, err := exec.Command("git", "clone", "-q", mac, ubuntu).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v %s", err, out)
	}
	return mac, ubuntu
}

// localShRun 在本机执行命令并取退出码（测试里扮演 Ubuntu）。
func localShRun(ctx context.Context, cmd string) ([]byte, int, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return out, -1, err
		}
	}
	return out, code, nil
}

func TestMsgBridgeSyncMacToUbuntu(t *testing.T) {
	mac, ubuntu := newRepoPair(t)
	want := gitCommit(t, mac, "c2")
	b := &msgBridge{
		repos:     map[string]support.Repo{"xxyi": {Dir: mac, Remote: ubuntu}},
		remoteDir: map[string]string{"xxyi": ubuntu},
		run:       localShRun,
		write:     func(context.Context, string, []byte) error { t.Fatal("Sync 不该写文件"); return nil },
	}
	m := support.Message{ID: "1", From: "mac:frank", To: "ubuntu:claude", Text: "x", Repo: "xxyi", Branch: "main"}
	if err := b.Sync(context.Background(), m); err != nil {
		t.Fatalf("Sync mac→ubuntu: %v", err)
	}
	if got := gitSh(t, ubuntu, "rev-parse", "refs/heads/main"); got != want {
		t.Errorf("Ubuntu main 应快进到 %s，得到 %s", want, got)
	}
	// 同步成功后暂存引用应已被删掉（中转用完就该没了）
	if out, err := exec.Command("git", "-C", ubuntu, "rev-parse", "--verify", "-q", "refs/xxyi/mac/main").CombinedOutput(); err == nil {
		t.Errorf("成功后暂存引用还在: %s", out)
	}

	// mac 写个文件再同步一次，把它带进 ubuntu 工作区（成为已跟踪文件）
	if err := os.WriteFile(filepath.Join(mac, "f.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitSh(t, mac, "add", "f.txt")
	gitSh(t, mac, "commit", "-q", "-m", "add f")
	gitSh(t, mac, "commit", "-q", "--allow-empty", "-m", "c3")
	if err := b.Sync(context.Background(), m); err != nil {
		t.Fatalf("干净时 Sync 不该报错: %v", err)
	}
	// 未跟踪文件不挡同步
	if err := os.WriteFile(filepath.Join(ubuntu, "scratch.txt"), []byte("scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	newWant := gitCommit(t, mac, "c4")
	if err := b.Sync(context.Background(), m); err != nil {
		t.Fatalf("只有未跟踪文件时不该挡同步: %v", err)
	}

	// Ubuntu 脏（改已跟踪文件）：FFScript 报 ERR dirty，Sync 要转成给人看的中文
	if err := os.WriteFile(filepath.Join(ubuntu, "f.txt"), []byte("oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	newWant = gitCommit(t, mac, "c5")
	err := b.Sync(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "Ubuntu") || !strings.Contains(err.Error(), "未提交") {
		t.Fatalf("Ubuntu 脏应报中文错误，得到 %v", err)
	}
	if got := gitSh(t, ubuntu, "rev-parse", "refs/heads/main"); got == newWant {
		t.Errorf("脏时不该快进")
	}
}

func TestMsgBridgeSyncBusyAndNoRepo(t *testing.T) {
	mac, ubuntu := newRepoPair(t)
	// ubuntu 主工作区让出 main，改检出别的分支
	gitSh(t, ubuntu, "checkout", "-q", "-b", "other")
	wt := filepath.Join(t.TempDir(), "wt")
	gitSh(t, ubuntu, "worktree", "add", "-q", wt, "main")

	b := &msgBridge{
		repos:     map[string]support.Repo{"xxyi": {Dir: mac, Remote: ubuntu}},
		remoteDir: map[string]string{"xxyi": ubuntu},
		run:       localShRun,
		write:     func(context.Context, string, []byte) error { t.Fatal("Sync 不该写文件"); return nil },
	}
	m := support.Message{ID: "5", From: "mac:frank", To: "ubuntu:claude", Text: "x", Repo: "xxyi", Branch: "main"}
	gitCommit(t, mac, "c2")
	err := b.Sync(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "main") || !strings.Contains(err.Error(), "工作区") {
		t.Fatalf("main 被另一工作区检出应报占用，得到 %v", err)
	}

	// remote_dir 配错指向非仓库目录 → 报「不是 git 仓库」并提示查 remote_dir
	b2 := &msgBridge{
		repos:     map[string]support.Repo{"xxyi": {Dir: mac, Remote: ubuntu}},
		remoteDir: map[string]string{"xxyi": filepath.Join(t.TempDir(), "nothing")},
		run:       localShRun,
		write:     func(context.Context, string, []byte) error { t.Fatal("Sync 不该写文件"); return nil },
	}
	if err := b2.Sync(context.Background(), m); err == nil ||
		!strings.Contains(err.Error(), "不是 git 仓库") || !strings.Contains(err.Error(), "remote_dir") {
		t.Fatalf("非仓库目录应报查 remote_dir，得到 %v", err)
	}
}

func TestMsgBridgeSyncUbuntuToMac(t *testing.T) {
	mac, ubuntu := newRepoPair(t)
	want := gitCommit(t, ubuntu, "c2")
	b := &msgBridge{
		repos:     map[string]support.Repo{"xxyi": {Dir: mac, Remote: ubuntu}},
		remoteDir: map[string]string{"xxyi": ubuntu},
		run:       localShRun,
		write:     func(context.Context, string, []byte) error { t.Fatal("Sync 不该写文件"); return nil },
	}
	m := support.Message{ID: "2", From: "ubuntu:claude", To: "mac:frank", Text: "x", Repo: "xxyi", Branch: "main"}
	if err := b.Sync(context.Background(), m); err != nil {
		t.Fatalf("Sync ubuntu→mac: %v", err)
	}
	if got := gitSh(t, mac, "rev-parse", "refs/heads/main"); got != want {
		t.Errorf("Mac main 应快进到 %s，得到 %s", want, got)
	}
}

func TestMsgBridgeSyncUnknownRepo(t *testing.T) {
	mac, ubuntu := newRepoPair(t)
	b := &msgBridge{
		repos:     map[string]support.Repo{"xxyi": {Dir: mac, Remote: ubuntu}},
		remoteDir: map[string]string{"xxyi": ubuntu},
		run:       localShRun,
	}
	err := b.Sync(context.Background(), support.Message{
		ID: "3", From: "mac:frank", To: "ubuntu:claude", Text: "x", Repo: "nope", Branch: "main",
	})
	if err == nil || !strings.Contains(err.Error(), "未知仓库 nope") {
		t.Fatalf("应报未知仓库，得到 %v", err)
	}
	// 仓库在但推不出 Ubuntu 目录（remote_dir 没写、remote 是相对 scp 式）→ 明确报错，不让它静默
	b2 := &msgBridge{
		repos: map[string]support.Repo{"xxyi": {Dir: mac, Remote: "chenwy@h:repos/xxyi"}},
		run:   localShRun,
	}
	err = b2.Sync(context.Background(), support.Message{
		ID: "4", From: "mac:frank", To: "ubuntu:claude", Text: "x", Repo: "xxyi", Branch: "main",
	})
	if err == nil || !strings.Contains(err.Error(), "没有 Ubuntu 目录") {
		t.Fatalf("缺 Ubuntu 目录应报错，得到 %v", err)
	}
}

func TestMsgRoutesWired(t *testing.T) {
	f, _, _ := supportFixture(t, nil)
	st, _, _ := run(t, f.dir)
	// 用户操作不带 Origin 一律 403
	for _, p := range []string{"/msg/decide?id=x&action=approve", "/msg/auto?minutes=10"} {
		if resp, b := supportPost(t, st, p, ""); resp.StatusCode != 403 || !strings.Contains(string(b), "需要从 xxyi 页面操作") {
			t.Errorf("无 Origin 的 POST %s 应 403: %d %s", p, resp.StatusCode, b)
		}
	}
	// 带白名单 Origin 进入业务逻辑：decide 查无此条 404；auto 开 10 分钟 200
	if resp, _ := supportPost(t, st, "/msg/decide?id=x&action=approve", testOrigin); resp.StatusCode != 404 {
		t.Errorf("带 Origin 的 /msg/decide 应进业务得 404，得到 %d", resp.StatusCode)
	}
	if resp, b := supportPost(t, st, "/msg/auto?minutes=10", testOrigin); resp.StatusCode != 200 {
		t.Errorf("带 Origin 的 /msg/auto 应 200: %d %s", resp.StatusCode, b)
	}
	// GET /msg 列消息（不要 Origin）
	if resp, b := get(t, st, "/msg", true); resp.StatusCode != 200 || !strings.Contains(string(b), "messages") {
		t.Errorf("GET /msg 应 200 带 messages: %d %s", resp.StatusCode, b)
	}
	// 没配协助：/msg 一律 404
	f2 := newFixture(t, nil)
	st2, _, _ := run(t, f2.dir)
	if resp, b := get(t, st2, "/msg", true); resp.StatusCode != 404 || !strings.Contains(string(b), "没有配置协助") {
		t.Errorf("没配协助的 GET /msg 应 404: %d %s", resp.StatusCode, b)
	}
}

// Sync 跟别的同仓库同步串行：锁被别人拿着就等到 ctx 超时，解锁后才干活。
func TestMsgBridgeSyncWaitsRepoLock(t *testing.T) {
	mac, ubuntu := newRepoPair(t)
	gitCommit(t, ubuntu, "c2") // ubuntu→mac 方向有东西可同步
	mu := &sync.Mutex{}
	mu.Lock()
	b := &msgBridge{
		repos:     map[string]support.Repo{"xxyi": {Dir: mac, Remote: ubuntu}},
		remoteDir: map[string]string{"xxyi": ubuntu},
		lock:      func(string) *sync.Mutex { return mu },
		run:       localShRun,
		write:     func(context.Context, string, []byte) error { t.Fatal("Sync 不该写文件"); return nil },
	}
	m := support.Message{ID: "lk1", From: "ubuntu:x", To: "mac:y", Text: "x", Repo: "xxyi", Branch: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := b.Sync(ctx, m); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("锁被占时应等到 ctx 超时，得到 %v", err)
	}
	mu.Unlock()
	// 解锁后（新 ctx）再调：不该再因锁报 ctx 错误（别的原因失败由别的测试管，这里不断言成败）
	if err := b.Sync(context.Background(), m); errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("解锁后再调不该因锁报 ctx 错误，得到 %v", err)
	}
}

// Deliver 写文件阻塞时必须按 msgRunTimeout 返回，不能挂死（协助 Stop 等在这里）。
func TestMsgBridgeDeliverWriteBlocksUntilTimeout(t *testing.T) {
	old := msgRunTimeout
	t.Cleanup(func() { msgRunTimeout = old })
	msgRunTimeout = 200 * time.Millisecond

	b := &msgBridge{
		run: func(context.Context, string) ([]byte, int, error) {
			t.Fatal("写正文都没写完，不该走到执行命令")
			return nil, 0, nil
		},
		write: func(ctx context.Context, _ string, _ []byte) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	start := time.Now()
	err := b.Deliver(context.Background(), support.Message{
		ID: "to1", From: "mac:frank", To: "ubuntu:claude", Text: "hi",
	})
	if err == nil {
		t.Fatal("写文件阻塞时 Deliver 应按超时返回错误")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("耗时 %v，应在 1 秒内按超时返回", d)
	}
	if !strings.Contains(err.Error(), "写到 Ubuntu 失败") {
		t.Errorf("错误应说清卡在写文件：%v", err)
	}
}

// remoteDir 配不出来但记了原因（remoteDirErr）：报错要把原因带上，让人知道去哪儿修。
func TestMsgBridgeSyncRemoteDirErr(t *testing.T) {
	mac, ubuntu := newRepoPair(t)
	b := &msgBridge{
		repos:        map[string]support.Repo{"xxyi": {Dir: mac, Remote: ubuntu}},
		remoteDirErr: map[string]string{"xxyi": "xyz"},
		run: func(context.Context, string) ([]byte, int, error) {
			t.Fatal("推不出 Ubuntu 目录就不该执行远端命令")
			return nil, 0, nil
		},
		write: func(context.Context, string, []byte) error {
			t.Fatal("推不出 Ubuntu 目录就不该写文件")
			return nil
		},
	}
	err := b.Sync(context.Background(), support.Message{
		ID: "rd1", From: "mac:x", To: "ubuntu:y", Text: "x", Repo: "xxyi", Branch: "main",
	})
	if err == nil || !strings.Contains(err.Error(), "xyz") || !strings.Contains(err.Error(), "没有 Ubuntu 目录") {
		t.Fatalf("报错要带上 remoteDirErr 里的原因，得到 %v", err)
	}
}

// wrapRC 包装后的命令在本机 sh 下执行：非零退出也拿得到 stdout 和真实退出码。
func TestWrapRC(t *testing.T) {
	cases := []struct {
		cmd      string
		wantOut  string
		wantCode int
	}{
		{"echo OK abc", "OK abc", 0},
		{"echo 'ERR dirty'; exit 5", "ERR dirty", 5},
		{"echo to-stderr >&2; exit 3", "to-stderr", 3},
		{"printf 'a\\nb'", "a\nb", 0},
	}
	for _, c := range cases {
		out, err := exec.Command("sh", "-c", wrapRC(c.cmd)).Output()
		if err != nil {
			t.Fatalf("%q: %v", c.cmd, err)
		}
		got, code := splitRC(out)
		if strings.TrimSpace(string(got)) != c.wantOut || code != c.wantCode {
			t.Errorf("%q: 得到 (%q, %d)，期望 (%q, %d)", c.cmd, got, code, c.wantOut, c.wantCode)
		}
	}
	if _, code := splitRC([]byte("no mark")); code != -1 {
		t.Errorf("没有回报行应返回 -1，得到 %d", code)
	}
}
