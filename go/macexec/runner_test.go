package macexec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestRunner 在临时目录里建一个 Runner。
func newTestRunner(t *testing.T, keep int) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := NewRunner(dir, keep)
	if err != nil {
		t.Fatalf("NewRunner(%q, %d) 失败: %v", dir, keep, err)
	}
	return r, dir
}

// waitForTerminal 轮询 Get 直到任务离开 running（默认最多 15 秒，防止卡死测试）。
func waitForTerminal(t *testing.T, r *Runner, id string) Job {
	t.Helper()
	return waitForTerminalMax(t, r, id, 15*time.Second)
}

func waitForTerminalMax(t *testing.T, r *Runner, id string, maxWait time.Duration) Job {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for {
		j, err := r.Get(id)
		if err != nil {
			t.Fatalf("Get(%q) 失败: %v", id, err)
		}
		if j.State != StateRunning {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待任务 %s 结束超时（当前仍为 %s）", id, j.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pgrepFound 用 pgrep -f 查有没有命令行含 pat 的进程（验证后台子进程是否被杀干净）。
func pgrepFound(pat string) bool {
	out, err := exec.Command("pgrep", "-f", pat).Output()
	if err != nil {
		return false // 没匹配到（退出码非 0）
	}
	return strings.TrimSpace(string(out)) != ""
}

// waitPgrepGone 等 pat 匹配的进程全部消失；超时则带现场报错。
func waitPgrepGone(t *testing.T, pat string, maxWait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for pgrepFound(pat) {
		if time.Now().After(deadline) {
			out, _ := exec.Command("pgrep", "-af", pat).Output()
			t.Errorf("清理超时：仍有进程匹配 %q:\n%s", pat, out)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// jobFileCount 数 runner 目录里属于 job 的文件（<id>.log + <id>.json）。
func jobFileCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") || strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

func readJobJSON(t *testing.T, path string) Job {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	return job
}

// startSimple 跑一条短命令并等它结束（默认 15 秒内）。
func startSimple(t *testing.T, r *Runner, action, script string) Job {
	t.Helper()
	job, err := r.Start(Spec{Action: action, Argv: []string{"sh", "-c", script}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Start(%s) 失败: %v", action, err)
	}
	return waitForTerminal(t, r, job.ID)
}

func TestNewRunnerValidation(t *testing.T) {
	// keep 必须为正。
	if _, err := NewRunner(t.TempDir(), 0); err == nil {
		t.Error("keep=0 应报错，实际通过")
	}
	if _, err := NewRunner(t.TempDir(), -3); err == nil {
		t.Error("keep=-3 应报错，实际通过")
	}
	// dir 不存在时自动创建。
	dir := filepath.Join(t.TempDir(), "missing", "runner")
	if _, err := NewRunner(dir, 1); err != nil {
		t.Fatalf("dir 不存在时应自动创建，实际报错: %v", err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Errorf("runner 目录未创建: %v", err)
	}
}

func TestJobNormalExit(t *testing.T) {
	r, dir := newTestRunner(t, 5)
	job, err := r.Start(Spec{
		Action:  "echo",
		Argv:    []string{"sh", "-c", `echo out-line; echo err-line 1>&2; echo foo=$FOO`},
		Env:     append(os.Environ(), "FOO=bar"),
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// Start 返回的就是 running 快照。
	if job.State != StateRunning {
		t.Errorf("Start 返回的 State = %q，应为 running", job.State)
	}

	final := waitForTerminal(t, r, job.ID)
	if final.State != StateDone {
		t.Errorf("State = %q（Note: %s），应为 done", final.State, final.Note)
	}
	if final.ExitCode != 0 {
		t.Errorf("ExitCode = %d，应为 0", final.ExitCode)
	}
	if final.Ended.IsZero() {
		t.Error("Ended 应已填，实际为零值")
	}

	// stdout 和 stderr 都要进同一个 log；Env 注入要生效。
	data, err := os.ReadFile(filepath.Join(dir, job.ID+".log"))
	if err != nil {
		t.Fatalf("读 log 失败: %v", err)
	}
	for _, want := range []string{"out-line", "err-line", "foo=bar"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log 缺少 %q，内容: %q", want, string(data))
		}
	}

	// json 落盘的内容与内存一致。
	persisted := readJobJSON(t, filepath.Join(dir, job.ID+".json"))
	if persisted.ID != job.ID || persisted.State != StateDone || persisted.ExitCode != 0 || persisted.Action != "echo" {
		t.Errorf("落盘 Job = %+v，与期望不符", persisted)
	}
}

func TestJobNonZeroExit(t *testing.T) {
	r, _ := newTestRunner(t, 5)
	final := startSimple(t, r, "exit3", "echo before-fail; exit 3")
	if final.State != StateFailed {
		t.Errorf("State = %q，应为 failed", final.State)
	}
	if final.ExitCode != 3 {
		t.Errorf("ExitCode = %d，应为 3", final.ExitCode)
	}
}

func TestStartBadArgv(t *testing.T) {
	r, _ := newTestRunner(t, 5)
	if _, err := r.Start(Spec{Action: "empty"}); err == nil {
		t.Error("Argv 为空应报错")
	}
	if _, err := r.Start(Spec{Action: "empty", Argv: []string{""}}); err == nil {
		t.Error("Argv[0] 为空应报错")
	}
	// 命令不存在：Start 报错，且落盘的记录是 failed。
	job, err := r.Start(Spec{Action: "ghost", Argv: []string{"definitely-no-such-cmd-xyz"}, Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("命令不存在应报错")
	}
	if job.State != StateFailed {
		t.Errorf("命令不存在时返回的 State = %q，应为 failed", job.State)
	}
	final := waitForTerminal(t, r, job.ID)
	if final.State != StateFailed {
		t.Errorf("最终 State = %q，应为 failed", final.State)
	}
	// 失败记录不能把单任务占用卡住。
	j2, err := r.Start(simpleSpec("after-bad-cmd"))
	if err != nil {
		t.Fatalf("上一条启动失败后应能继续接任务，实际: %v", err)
	}
	waitForTerminal(t, r, j2.ID)
}

func simpleSpec(script string) Spec {
	return Spec{Action: "echo", Argv: []string{"sh", "-c", script}, Timeout: 5 * time.Second}
}

func TestBusyWhileRunning(t *testing.T) {
	r, _ := newTestRunner(t, 5)
	job, err := r.Start(Spec{Action: "hold", Argv: []string{"sh", "-c", "sleep 2"}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// 忙时第二个任务必须被 ErrBusy 挡住，并带上正在跑的任务信息。
	dup, err := r.Start(Spec{Action: "second", Argv: []string{"sh", "-c", "echo hi"}, Timeout: 5 * time.Second})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("忙时应返回 ErrBusy，实际: %v", err)
	}
	if dup.ID != job.ID || dup.State != StateRunning {
		t.Errorf("ErrBusy 返回的 Job = %+v，应为正在跑的 %s(running)", dup, job.ID)
	}
	waitForTerminal(t, r, job.ID)
	// 结束后恢复可接。
	j3, err := r.Start(simpleSpec("after-idle"))
	if err != nil {
		t.Fatalf("空闲后 Start 失败: %v", err)
	}
	waitForTerminal(t, r, j3.ID)
}

func TestTimeoutStateAndKill(t *testing.T) {
	r, _ := newTestRunner(t, 5)
	start := time.Now()
	job, err := r.Start(Spec{Action: "hang", Argv: []string{"sh", "-c", "sleep 120"}, Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	final := waitForTerminal(t, r, job.ID)
	if final.State != StateTimeout {
		t.Errorf("State = %q，应为 timeout", final.State)
	}
	if final.Note != "超时被终止" {
		t.Errorf("Note = %q，应为「超时被终止」", final.Note)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("超时任务拖了 %v 才收尾，应贴近 500ms", elapsed)
	}
	// 超时后必须能接新任务。
	j2, err := r.Start(simpleSpec("next"))
	if err != nil {
		t.Fatalf("超时后 Start 失败: %v", err)
	}
	waitForTerminal(t, r, j2.ID)
}

func TestTimeoutKillsBackgroundChild(t *testing.T) {
	r, _ := newTestRunner(t, 5)
	// sh 起后台 sleep 456 后 wait：主进程活着，超时杀进程组时后台子进程必须一起死。
	if pgrepFound("sleep 456") {
		t.Fatal("测试开始前就有残留的 sleep 456，环境不干净")
	}
	job, err := r.Start(Spec{Action: "bg", Argv: []string{"sh", "-c", "sleep 456 & wait"}, Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// 确认后台子进程确实起过（在同一个 sleep 456 上）。
	seen := false
	for i := 0; i < 20 && !seen; i++ {
		seen = pgrepFound("sleep 456")
		time.Sleep(50 * time.Millisecond)
	}
	if !seen {
		t.Fatal("后台 sleep 456 没起来，测试无效")
	}
	final := waitForTerminal(t, r, job.ID)
	if final.State != StateTimeout {
		t.Errorf("State = %q，应为 timeout", final.State)
	}
	// 整个进程组（含后台 sleep）都要被清掉。
	waitPgrepGone(t, "sleep 456", 5*time.Second)
	// 占用已释放。
	j2, err := r.Start(simpleSpec("freed"))
	if err != nil {
		t.Fatalf("杀掉进程组后 Start 失败: %v", err)
	}
	waitForTerminal(t, r, j2.ID)
}

func TestBackgroundChildInheritsPipeUntilTimeout(t *testing.T) {
	r, _ := newTestRunner(t, 5)
	// 主进程立刻退出，后台 sleep 789 继承 stdout/stderr 占着管道：
	// 截止时间到点必须把进程组（含 sleep）杀掉、释放占用，而不是干等 789 秒。
	job, err := r.Start(Spec{Action: "orphan", Argv: []string{"sh", "-c", "echo parent-exit; sleep 789 & exit 0"}, Timeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	start := time.Now()
	final := waitForTerminalMax(t, r, job.ID, 20*time.Second)
	elapsed := time.Since(start)
	if final.State == StateRunning {
		t.Fatal("任务没有收尾")
	}
	if elapsed > 10*time.Second {
		t.Errorf("拖了 %v 才收尾；应≈超时阈值+清理（上限 waitDelay=5s）", elapsed)
	}
	t.Logf("后台继承管道场景: state=%s exit=%d note=%q elapsed=%v", final.State, final.ExitCode, final.Note, elapsed)
	waitPgrepGone(t, "sleep 789", 5*time.Second)
	j2, err := r.Start(simpleSpec("freed2"))
	if err != nil {
		t.Fatalf("管道场景收尾后 Start 失败: %v", err)
	}
	waitForTerminal(t, r, j2.ID)
}

func TestGetReadsDiskAfterReopen(t *testing.T) {
	r, _ := newTestRunner(t, 5)
	final := startSimple(t, r, "persist", "echo persisted-output; exit 7")
	if final.State != StateFailed || final.ExitCode != 7 {
		t.Fatalf("前置任务异常: %+v", final)
	}
	// 新建 Runner（模拟进程重启）后仍能 Get 到落盘的最终状态。
	r2, err := NewRunner(r.dir, 5)
	if err != nil {
		t.Fatalf("重开 Runner 失败: %v", err)
	}
	got, err := r2.Get(final.ID)
	if err != nil {
		t.Fatalf("重启后 Get 失败: %v", err)
	}
	if got.State != StateFailed || got.ExitCode != 7 || got.Action != "persist" {
		t.Errorf("重启后 Get = %+v，期望 failed/7/persist", got)
	}
	if s, err := r2.Get("nonexistent"); err == nil {
		t.Errorf("不存在的任务应报错，实际 %+v", s)
	}
}

func TestRunningVisibleThenRestartFlipsToFailed(t *testing.T) {
	r, dir := newTestRunner(t, 5)
	job, err := r.Start(Spec{Action: "stale", Argv: []string{"sh", "-c", "sleep 30"}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// Start 后立刻：内存和落盘 json 都应是 running。
	if j, err := r.Get(job.ID); err != nil || j.State != StateRunning {
		t.Fatalf("Start 后 Get = %+v err=%v，应为 running", j, err)
	}
	onDisk := readJobJSON(t, filepath.Join(dir, job.ID+".json"))
	if onDisk.State != StateRunning {
		t.Errorf("Start 后落盘 state = %q，应为 running", onDisk.State)
	}

	// 模拟执行器重启：新建 Runner 把 running 中的残留清算成 failed。
	r2, err := NewRunner(dir, 5)
	if err != nil {
		t.Fatalf("重启 NewRunner 失败: %v", err)
	}
	got, err := r2.Get(job.ID)
	if err != nil {
		t.Fatalf("重启后 Get 失败: %v", err)
	}
	if got.State != StateFailed {
		t.Errorf("重启后 state = %q，应为 failed", got.State)
	}
	if !strings.Contains(got.Note, "执行器重启") {
		t.Errorf("重启清算 Note = %q，应注明执行器重启", got.Note)
	}
	// 落盘也被改写。
	if j := readJobJSON(t, filepath.Join(dir, job.ID+".json")); j.State != StateFailed {
		t.Errorf("清算后落盘 state = %q，应为 failed", j.State)
	}
	// 收尾：原任务由自身超时清理，不留 sleep 30。
	final := waitForTerminal(t, r, job.ID)
	t.Logf("原 Runner 最终记录: state=%s note=%q", final.State, final.Note)
	waitPgrepGone(t, "sleep 30", 5*time.Second)
}

func TestKeepRotation(t *testing.T) {
	r, dir := newTestRunner(t, 2)
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		j := startSimple(t, r, "rot", "echo x")
		ids = append(ids, j.ID)
		if j.State != StateDone {
			t.Fatalf("轮转前置任务 %d 失败: %+v", i, j)
		}
		time.Sleep(30 * time.Millisecond) // 拉开 mtime，让轮转的"最旧"判定稳定
	}
	// 只留最近 2 个：最旧的 log/json 应被删，新的保留。
	for _, suffix := range []string{".log", ".json"} {
		if _, err := os.Stat(filepath.Join(dir, ids[0]+suffix)); !os.IsNotExist(err) {
			t.Errorf("最旧任务 %s%s 应被轮转删除（stat err=%v）", ids[0], suffix, err)
		}
		for _, keepID := range ids[1:] {
			if _, err := os.Stat(filepath.Join(dir, keepID+suffix)); err != nil {
				t.Errorf("近期任务 %s%s 不应被删: %v", keepID, suffix, err)
			}
		}
	}
	if n := jobFileCount(t, dir); n != 4 {
		t.Errorf("keep=2 跑完 3 个任务后目录里应有 4 个 job 文件，实际 %d", n)
	}
	if _, err := r.Get(ids[0]); err == nil {
		t.Error("被轮转掉的旧任务 Get 应报错")
	}
	if j, err := r.Get(ids[2]); err != nil || j.State != StateDone {
		t.Errorf("最新任务 Get = %+v err=%v", j, err)
	}
}

func TestRunningSurvivesRotation(t *testing.T) {
	// keep=1 时正在跑的任务不能被自己这轮轮转删掉。
	r, dir := newTestRunner(t, 1)
	prev := startSimple(t, r, "prev", "echo prev")
	if prev.State != StateDone {
		t.Fatalf("前置任务失败: %+v", prev)
	}
	job, err := r.Start(Spec{Action: "hold", Argv: []string{"sh", "-c", "sleep 1"}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, job.ID+".log")); err != nil {
		t.Errorf("running 中的 log 不应被轮转删除: %v", err)
	}
	final := waitForTerminal(t, r, job.ID)
	if final.State != StateDone {
		t.Errorf("State = %q（Note: %s），应为 done", final.State, final.Note)
	}
}

func TestOutputOverLimitFailsJob(t *testing.T) {
	orig := MaxOutputBytes
	defer func() { MaxOutputBytes = orig }()
	MaxOutputBytes = 4096

	r, dir := newTestRunner(t, 5)
	job, err := r.Start(Spec{Action: "flood", Argv: []string{"sh", "-c", "yes 0123456789"}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	final := waitForTerminal(t, r, job.ID)
	if final.State != StateFailed {
		t.Errorf("输出超限应标 failed，实际 state=%q note=%q", final.State, final.Note)
	}
	if !strings.Contains(final.Note, "输出超过上限") {
		t.Errorf("Note = %q，应注明输出超过上限", final.Note)
	}
	// log 不超过上限且非空；超限后不卡占用。
	st, err := os.Stat(filepath.Join(dir, job.ID+".log"))
	if err != nil {
		t.Fatalf("读超限 log 失败: %v", err)
	}
	if st.Size() == 0 || st.Size() > MaxOutputBytes {
		t.Errorf("超限 log 大小 = %d，应在 (0, %d]", st.Size(), MaxOutputBytes)
	}
	j2, err := r.Start(simpleSpec("after-flood"))
	if err != nil {
		t.Fatalf("超限任务收尾后 Start 失败: %v", err)
	}
	waitForTerminal(t, r, j2.ID)
}

func TestReadOnlyDirStartFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下 chmod 拦不住写入，只读目录场景不适用")
	}
	r, dir := newTestRunner(t, 5)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("设只读失败: %v", err)
	}
	defer func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("恢复目录权限失败: %v", err)
		}
	}()
	// 目录只读：落不了盘，Start 必须返回错误，且不能把 runner 卡成 busy。
	if _, err := r.Start(Spec{Action: "ro", Argv: []string{"sh", "-c", "echo hi"}, Timeout: 5 * time.Second}); err == nil {
		t.Error("目录只读时 Start 应报错，实际成功")
	}
	// 目录恢复后可以正常接任务。
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("恢复目录权限失败: %v", err)
	}
	j, err := r.Start(simpleSpec("rw-again"))
	if err != nil {
		t.Fatalf("恢复可写后 Start 失败: %v", err)
	}
	if j.State != StateRunning {
		t.Errorf("State = %q，应为 running", j.State)
	}
	waitForTerminal(t, r, j.ID)
}

// TestNewRunnerRecoverWriteFailure 按 B2 要求："恢复时改写失败 NewRunner 返回错误"。
// 当前实现 recoverStaleRunning 对 persistLocked 的失败是 _ = 吞掉的，
// 预期此测试暴露该实现缺口。
func TestNewRunnerRecoverWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下 chmod 拦不住写入，只读目录场景不适用")
	}
	r, dir := newTestRunner(t, 5)
	job, err := r.Start(Spec{Action: "stale2", Argv: []string{"sh", "-c", "sleep 30"}, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// 等 running 的 json 落定，再把目录设只读，让重启清算写不回去。
	if j := readJobJSON(t, filepath.Join(dir, job.ID+".json")); j.State != StateRunning {
		t.Fatalf("前置条件：落盘应为 running，实际 %q", j.State)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("设只读失败: %v", err)
	}
	defer func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("恢复目录权限失败: %v", err)
		}
	}()

	if _, err := NewRunner(dir, 5); err == nil {
		t.Errorf("【实现 bug】恢复时清算改写失败，NewRunner 应返回错误，实际返回成功——runner.go recoverStaleRunning 里 persistLocked 的错误被 _ = 吞掉")
	}
	// 收尾：原任务自己超时退出，不留孤儿。
	final := waitForTerminal(t, r, job.ID)
	t.Logf("原任务收尾: state=%s note=%q", final.State, final.Note)
	waitPgrepGone(t, "sleep 30", 5*time.Second)
}

func TestGetInvalidID(t *testing.T) {
	r, _ := newTestRunner(t, 5)
	for _, id := range []string{"", "nope", "20261016-100000-ZZZZZZZZ", "2026-1016-100000-a1b2c3d4"} {
		if _, err := r.Get(id); err == nil {
			t.Errorf("Get(%q) 非法 ID 应报错", id)
		}
	}
	// 合法格式但不存在。
	if _, err := r.Get("20261016-100000-a1b2c3d4"); err == nil {
		t.Error("不存在的合法 ID 应报错")
	}
}

func TestTailBasics(t *testing.T) {
	dir := t.TempDir()

	// 参数校验。
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Tail(p, 0, 100); err == nil {
		t.Error("lines=0 应报错")
	}
	if _, err := Tail(p, 5, 0); err == nil {
		t.Error("maxBytes=0 应报错")
	}
	if _, err := Tail(filepath.Join(dir, "missing.txt"), 5, 100); err == nil {
		t.Error("文件不存在应报错")
	}
	if _, err := Tail(dir, 5, 100); err == nil {
		t.Error("路径是目录应报错")
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err := Tail(filepath.Join(dir, "empty.txt"), 5, 100); err != nil || s != "" {
		t.Errorf("空文件 Tail = %q err=%v，期望空串", s, err)
	}

	// 行数上限：10 行取最后 3 行。
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "line-%02d\n", i)
	}
	p = filepath.Join(dir, "ten.txt")
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Tail(p, 3, 1<<20)
	if err != nil {
		t.Fatalf("Tail 失败: %v", err)
	}
	if want := "line-08\nline-09\nline-10"; got != want {
		t.Errorf("Tail(3 行) = %q，期望 %q", got, want)
	}
	// 文件比请求短：全量返回，去掉尾随换行。
	p2 := filepath.Join(dir, "two.txt")
	if err := os.WriteFile(p2, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := Tail(p2, 100, 1<<20); err != nil || got != "a\nb" {
		t.Errorf("Tail(短文件) = %q err=%v，期望 \"a\\nb\"", got, err)
	}
	// 无尾随换行。
	p3 := filepath.Join(dir, "nonl.txt")
	if err := os.WriteFile(p3, []byte("a\nb\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := Tail(p3, 2, 1<<20); err != nil || got != "b\nc" {
		t.Errorf("Tail(无尾换行) = %q err=%v，期望 \"b\\nc\"", got, err)
	}
}

func TestTailMaxBytesWindow(t *testing.T) {
	dir := t.TempDir()
	// 10 行，每行 9 字节（8 个字符 + 换行），共 90 字节。
	p := filepath.Join(dir, "window.txt")
	if err := os.WriteFile(p, []byte(strings.Repeat("aaaaaaaa\n", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	// 窗口 25 字节：从第 8 行行中开始，开头残段要丢掉，保留最后 2 个完整行。
	got, err := Tail(p, 100, 25)
	if err != nil {
		t.Fatalf("Tail 失败: %v", err)
	}
	if want := "aaaaaaaa\naaaaaaaa"; got != want {
		t.Errorf("Tail(25B窗口) = %q，期望 %q", got, want)
	}
	// 窗口从行边界开始（6 字节 = 最后两行整行）：不该丢完整行。
	p2 := filepath.Join(dir, "boundary.txt")
	if err := os.WriteFile(p2, []byte("aa\nbb\ncc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = Tail(p2, 100, 6)
	if err != nil {
		t.Fatalf("Tail 失败: %v", err)
	}
	if want := "bb\ncc"; got != want {
		t.Errorf("【实现 bug】窗口从行边界开始时 Tail = %q，期望 %q——runner.go Tail 无条件丢掉窗口首行，应先查窗口前一个字节是否为 '\\n' 再决定丢不丢", got, want)
	}
	// 窗口比文件小、且不足一行：整窗没有换行时原样返回窗口内容。
	p3 := filepath.Join(dir, "longline.txt")
	if err := os.WriteFile(p3, []byte("0123456789ABCDEFGHIJ"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = Tail(p3, 100, 5)
	if err != nil {
		t.Fatalf("Tail 失败: %v", err)
	}
	if want := "FGHIJ"; got != want {
		t.Errorf("Tail(超长行窗口) = %q，期望 %q", got, want)
	}
}
