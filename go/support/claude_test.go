package support

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain：带 SUPPORT_FAKE_CLAUDE=1 时本二进制当"假 claude"跑，否则正常跑测试。
func TestMain(m *testing.M) {
	if os.Getenv("SUPPORT_FAKE_CLAUDE") == "1" {
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}

// fakeClaude 模拟 claude stream-json 协议，返回进程退出码。
func fakeClaude() int {
	if f := os.Getenv("SUPPORT_FAKE_ARGS"); f != "" {
		if fh, err := os.OpenFile(f, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			fmt.Fprintf(fh, "%s\n", strings.Join(os.Args[1:], " "))
			fh.Close()
		}
	}
	cwd, _ := os.Getwd()
	init, _ := json.Marshal(map[string]string{
		"type": "system", "subtype": "init",
		"session_id": "sess-1", "cwd": cwd,
	})
	os.Stdout.Write(append(init, '\n'))
	os.Stdout.WriteString("hello plain\n")

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT)
	ignoring := false

	// 同步读 stdin：hang 时不再读，管道才会写满；
	// EOF 时正常模式退 0，ignore-int 模式挂住等 SIGKILL——
	// Stop 先关标准输入再发信号，不这样 hang 住就测不到升级 SIGKILL 的路径。
	reader := bufio.NewReader(os.Stdin)
	for {
		// 消息之间处理挂着的信号：正常模式收到 SIGINT 干净退 0
		select {
		case <-interrupt:
			if !ignoring {
				return 0
			}
		default:
		}

		line, err := reader.ReadString('\n')
		if line != "" {
			switch content := fakeContent(strings.TrimSpace(line)); content {
			case "crash":
				os.Stderr.WriteString("boom\n")
				return 3
			case "slow":
				time.Sleep(300 * time.Millisecond)
				fakeReply(content)
			case "ignore-int":
				signal.Ignore(syscall.SIGINT)
				ignoring = true
				fakeReply(content)
			case "big":
				// 一行 9MB 非 JSON，再来一条正常 result——测超长行截断
				os.Stdout.WriteString(strings.Repeat("b", 9<<20) + "\n")
				fakeResult(content)
			case "hang":
				// 不再读 stdin 且保持 stdin 开着，卡住等 SIGKILL——测写满管道时 Stop 还能推进
				select {}
			default:
				fakeReply(content)
			}
		}
		if err != nil { // 标准输入关闭
			if ignoring {
				select {}
			}
			return 0
		}
	}
}

func fakeContent(line string) string {
	var m struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	json.Unmarshal([]byte(line), &m)
	return m.Message.Content
}

func fakeReply(text string) {
	echo := "echo: " + text
	a, _ := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": echo}},
		},
	})
	os.Stdout.Write(append(a, '\n'))
	fakeResult(text)
}

func fakeResult(text string) {
	res, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "result": "echo: " + text,
	})
	os.Stdout.Write(append(res, '\n'))
}

// ---------- 测试辅助 ----------

func newTestRunner(t *testing.T, cfg RunnerConfig) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "fake-args.txt")
	cfg.Bin = os.Args[0]
	cfg.Dir = dir
	cfg.Env = append(cfg.Env, "SUPPORT_FAKE_CLAUDE=1", "SUPPORT_FAKE_ARGS="+argsFile, "GORACE=atexit_sleep_ms=0")
	r := NewRunner(cfg)
	t.Cleanup(func() { _ = r.Stop() })
	return r, argsFile
}

// waitEvents 轮询事件直到 pred 满足或超时，返回累计的全部事件。
func waitEvents(t *testing.T, r *Runner, pred func([]Event) bool) []Event {
	t.Helper()
	var all []Event
	var after int64
	deadline := time.Now().Add(10 * time.Second)
	for {
		ev, _ := r.Events(context.Background(), after, 0, 300*time.Millisecond)
		all = append(all, ev...)
		if len(ev) > 0 {
			after = ev[len(ev)-1].Seq
		}
		if pred(all) {
			return all
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待事件超时，已收到 %d 条", len(all))
		}
	}
}

func waitBusy(t *testing.T, r *Runner, want bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r.State().Busy == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 Busy=%v 超时，当前 %+v", want, r.State())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readArgsFile 返回参数文件的各行。
func readArgsFile(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读参数文件失败: %v", err)
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func dataOf(t *testing.T, e Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("事件 #%d 不是 JSON: %v", e.Seq, err)
	}
	return m
}

// ---------- 测试 ----------

func TestSendAndEvents(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{})
	if err := r.Send("你好"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := waitEvents(t, r, func(all []Event) bool {
		return len(all) >= 4
	})

	kinds := []string{}
	for _, e := range ev[:4] {
		kinds = append(kinds, fmt.Sprint(dataOf(t, e)["type"]))
	}
	if got := strings.Join(kinds, ","); got != "system,raw,assistant,result" {
		t.Fatalf("前四条事件类型 = %s，要 system,raw,assistant,result", got)
	}
	if txt := dataOf(t, ev[1])["text"]; txt != "hello plain" {
		t.Fatalf("raw 文本 = %v", txt)
	}
	cwd := filepath.Clean(fmt.Sprint(dataOf(t, ev[0])["cwd"]))
	dir := filepath.Clean(r.cfg.Dir)
	if ec, _ := filepath.EvalSymlinks(cwd); ec != "" {
		cwd = ec
	}
	if ed, _ := filepath.EvalSymlinks(dir); ed != "" {
		dir = ed
	}
	if cwd != dir {
		t.Fatalf("init 的 cwd = %s，Dir = %s", cwd, dir)
	}

	st := r.State()
	if st.SessionID != "sess-1" || st.Turns != 1 || !st.Running {
		t.Fatalf("State = %+v", st)
	}
	if st.Busy {
		t.Fatalf("result 已到，Busy 应为 false: %+v", st)
	}
}

// Busy 的"先真后假"用 slow 验：快速 echo 可能 result 先到，Busy 观测不到真。
func TestBusyTrueThenFalse(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{})
	if err := r.Send("slow"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitBusy(t, r, true) // slow 要睡 300ms，Busy 必为真
	waitEvents(t, r, func(all []Event) bool {
		for _, e := range all {
			if dataOf(t, e)["type"] == "result" {
				return true
			}
		}
		return false
	})
	waitBusy(t, r, false)
}

func TestTwoSendsOneProcess(t *testing.T) {
	r, argsFile := newTestRunner(t, RunnerConfig{Args: []string{"--extra", "x"}})
	if err := r.Send("a"); err != nil {
		t.Fatalf("Send a: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })
	if err := r.Send("b"); err != nil {
		t.Fatalf("Send b: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 2 })

	lines := readArgsFile(t, argsFile)
	if len(lines) != 1 {
		t.Fatalf("参数文件 %d 行，要 1 行: %q", len(lines), lines)
	}
	for _, want := range []string{"-p", "--input-format stream-json", "--output-format stream-json", "--verbose", "--extra x"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("启动参数缺 %q: %s", want, lines[0])
		}
	}
}

func TestCrashAndResume(t *testing.T) {
	r, argsFile := newTestRunner(t, RunnerConfig{})
	if err := r.Send("hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })
	if err := r.Send("crash"); err != nil {
		t.Fatalf("Send crash: %v", err)
	}
	exited := waitEvents(t, r, func(all []Event) bool {
		for _, e := range all {
			if dataOf(t, e)["type"] == "exited" {
				return true
			}
		}
		return false
	})
	var ex map[string]any
	for _, e := range exited {
		if m := dataOf(t, e); m["type"] == "exited" {
			ex = m
		}
	}
	if code, _ := ex["code"].(float64); code != 3 {
		t.Fatalf("exited.code = %v，要 3", ex["code"])
	}
	if s, _ := ex["stderr"].(string); !strings.Contains(s, "boom") {
		t.Fatalf("exited.stderr = %q，要含 boom", s)
	}
	st := r.State()
	if st.Running {
		t.Fatal("崩溃后 Running 应为 false")
	}
	if !strings.Contains(st.LastError, "boom") || st.LastError == "" {
		t.Fatalf("LastError = %q，要含 boom", st.LastError)
	}

	if err := r.Send("again"); err != nil {
		t.Fatalf("崩溃后 Send: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		lines := readArgsFile(t, argsFile)
		if len(lines) >= 2 && strings.Contains(lines[len(lines)-1], "--resume sess-1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("重启参数里没见到 --resume sess-1: %q", readArgsFile(t, argsFile))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := r.State(); !st.Running || st.LastError != "" {
		t.Fatalf("重启后 State = %+v", st)
	}
}

func TestEventsWait(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{})
	if err := r.Send("hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })
	_, latest := r.Events(context.Background(), 0, 0, 0)

	type res struct {
		ev  []Event
		seq int64
	}
	ch := make(chan res, 1)
	go func() {
		ev, seq := r.Events(context.Background(), latest, 0, 3*time.Second)
		ch <- res{ev, seq}
	}()
	time.Sleep(100 * time.Millisecond) // 让 Events 真的进入等待
	if err := r.Send("ping"); err != nil {
		t.Fatalf("Send ping: %v", err)
	}
	select {
	case got := <-ch:
		if len(got.ev) == 0 {
			t.Fatal("Events 等待后应带新事件返回")
		}
		if got.seq <= latest {
			t.Fatalf("最新 Seq = %d，应大于 %d", got.seq, latest)
		}
	case <-time.After(2900 * time.Millisecond): // 3 秒超时兜底
		t.Fatal("Events 应被 Send 叫醒，不该拖到超时")
	}
}

func TestMaxEvents(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{MaxEvents: 5})
	if err := r.Send("a"); err != nil {
		t.Fatalf("Send a: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })
	if err := r.Send("b"); err != nil {
		t.Fatalf("Send b: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 2 })

	ev, latest := r.Events(context.Background(), 0, 0, 0)
	// 总共 6 条（init/raw/2x assistant/result），只留最后 5 条
	if len(ev) != 5 {
		t.Fatalf("缓冲里 %d 条，要 5 条", len(ev))
	}
	if latest != 6 || ev[0].Seq != 2 || ev[4].Seq != 6 {
		t.Fatalf("latest=%d 首尾 Seq=%d..%d，要 6 和 2..6", latest, ev[0].Seq, ev[4].Seq)
	}
	for i := 1; i < len(ev); i++ {
		if ev[i].Seq != ev[i-1].Seq+1 {
			t.Fatalf("Seq 不递增: %v", ev)
		}
	}
}

func TestStopAndRepeat(t *testing.T) {
	r, argsFile := newTestRunner(t, RunnerConfig{})
	if err := r.Send("hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })

	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := r.State(); st.Running || st.Busy {
		t.Fatalf("Stop 后 State = %+v", st)
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("重复 Stop: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool {
		for _, e := range all {
			if m := dataOf(t, e); m["type"] == "exited" {
				return true
			}
		}
		return false
	})

	// Stop 之后还能再 Send，带 --resume 重启
	if err := r.Send("again"); err != nil {
		t.Fatalf("Stop 后 Send: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 2 })
	lines := readArgsFile(t, argsFile)
	if len(lines) != 2 || !strings.Contains(lines[1], "--resume sess-1") {
		t.Fatalf("重启参数不对: %q", lines)
	}
}

func TestStopEscalatesToKill(t *testing.T) {
	old := stopGrace
	stopGrace = 300 * time.Millisecond
	t.Cleanup(func() { stopGrace = old })

	r, _ := newTestRunner(t, RunnerConfig{})
	if err := r.Send("ignore-int"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })

	start := time.Now()
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed < stopGrace {
		t.Fatalf("Stop 只用了 %v，宽限期 %v 没生效", elapsed, stopGrace)
	}
	if st := r.State(); st.Running {
		t.Fatal("SIGKILL 后 Running 应为 false")
	}
	// 主动 Stop 杀的不算异常，LastError 应为空
	if st := r.State(); st.LastError != "" {
		t.Fatalf("主动 Stop 后 LastError 应为空，得到 %q", st.LastError)
	}
	ev := waitEvents(t, r, func(all []Event) bool {
		for _, e := range all {
			if m := dataOf(t, e); m["type"] == "exited" {
				return true
			}
		}
		return false
	})
	var code float64 = -2
	for _, e := range ev {
		if m := dataOf(t, e); m["type"] == "exited" {
			code, _ = m["code"].(float64)
		}
	}
	if want := float64(128 + 9); code != want {
		t.Fatalf("被 SIGKILL 的退出码 = %v，要 %v", code, want)
	}
}

func TestEmptySend(t *testing.T) {
	r, argsFile := newTestRunner(t, RunnerConfig{})
	if err := r.Send(""); err == nil {
		t.Fatal("空文字应报错")
	}
	if st := r.State(); st.Running {
		t.Fatal("空 Send 不该启动进程")
	}
	if lines := readArgsFile(t, argsFile); len(lines) != 0 {
		t.Fatalf("不该有进程起来: %q", lines)
	}
}

func TestMissingBin(t *testing.T) {
	r := NewRunner(RunnerConfig{
		Bin: filepath.Join(t.TempDir(), "no-such-claude"),
		Dir: t.TempDir(),
	})
	if err := r.Send("hi"); err == nil {
		t.Fatal("Bin 不存在应报错")
	}
	if st := r.State(); st.Running || st.LastError == "" {
		t.Fatalf("State = %+v，LastError 应非空且 Running=false", st)
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("没跑过的 Stop: %v", err)
	}
}

// M1：连发多轮，result 与 busy 置位赛跑时 busy 不能永远卡在 true。
func TestBusyFalseAfterManySends(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{})
	for i := 0; i < 50; i++ {
		if err := r.Send("x"); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for r.State().Turns <= i {
			if time.Now().After(deadline) {
				t.Fatalf("第 %d 轮 result 迟迟没到: %+v", i, r.State())
			}
			time.Sleep(2 * time.Millisecond)
		}
		if st := r.State(); st.Busy {
			t.Fatalf("第 %d 轮 result 已到，Busy 应为 false: %+v", i, st)
		}
	}
}

// M2：子进程不读 stdin、管道写满把 Send 的 Write 卡住时，Stop 必须照样能推进。
func TestStopWithBlockedSend(t *testing.T) {
	old := stopGrace
	stopGrace = 300 * time.Millisecond // 假 claude 无视 SIGINT，会走满宽限期，调小省时间
	t.Cleanup(func() { stopGrace = old })

	r, _ := newTestRunner(t, RunnerConfig{})
	if err := r.Send("hang"); err != nil {
		t.Fatalf("Send hang: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // 等假 claude 进入挂起（不再读 stdin）

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- r.Send(strings.Repeat("x", 128<<10)) // 管道满，Write 会阻塞
	}()
	time.Sleep(100 * time.Millisecond) // 让 Write 真的堵上

	stopped := make(chan error, 1)
	go func() { stopped <- r.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(stopGrace + 2*time.Second):
		t.Fatal("Stop 被阻塞的 Send 拖住了")
	}
	select {
	case err := <-sendDone:
		if err == nil {
			t.Fatal("stdin 被关后阻塞的 Send 应报错返回")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("阻塞的 Send 没随 Stop 结束")
	}
}

// M3：超过 8MB 的单行截断成 raw 事件，后面的正常行照常处理。
func TestBigLineTruncated(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{})
	if err := r.Send("big"); err != nil {
		t.Fatalf("Send big: %v", err)
	}
	ev := waitEvents(t, r, func(all []Event) bool {
		for _, e := range all {
			if bytes.Contains(e.Data, []byte(`"type":"result"`)) {
				return true
			}
		}
		return false
	})
	rawIdx, resIdx := -1, -1
	for i, e := range ev {
		m := dataOf(t, e)
		switch m["type"] {
		case "raw":
			// 跳过第一条 "hello plain"，找截断的那条超长行
			if tr, _ := m["truncated"].(bool); tr && rawIdx < 0 {
				rawIdx = i
			}
		case "result":
			if resIdx < 0 {
				resIdx = i
			}
		}
	}
	if rawIdx < 0 || resIdx < 0 || rawIdx >= resIdx {
		t.Fatalf("要先有 raw 再有 result: raw=%d result=%d", rawIdx, resIdx)
	}
	m := dataOf(t, ev[rawIdx])
	if tr, _ := m["truncated"].(bool); !tr {
		t.Fatalf("raw 事件应标 truncated: %v", m["type"])
	}
	if txt, _ := m["text"].(string); len(txt) != maxLineBytes {
		t.Fatalf("截断后 text 长度 = %d，要 %d", len(txt), maxLineBytes)
	}
	if st := r.State(); st.Turns != 1 {
		t.Fatalf("Turns = %d，要 1", st.Turns)
	}
}

// S6：ctx 取消的 Events 立即返回。
func TestEventsCtxCancel(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{})
	if err := r.Send("hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })
	_, latest := r.Events(context.Background(), 0, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t0 := time.Now()
	ev, seq := r.Events(ctx, latest, 0, 3*time.Second)
	if len(ev) != 0 || seq != latest {
		t.Fatalf("取消后应立刻空手而归: n=%d seq=%d latest=%d", len(ev), seq, latest)
	}
	if time.Since(t0) > time.Second {
		t.Fatalf("ctx 取消后拖了 %v 才返回", time.Since(t0))
	}
}

// S6：wait 到点返回空切片和当前最新 Seq。
func TestEventsWaitTimeout(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{})
	if err := r.Send("hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })
	_, latest := r.Events(context.Background(), 0, 0, 0)

	t0 := time.Now()
	ev, seq := r.Events(context.Background(), latest, 0, 100*time.Millisecond)
	if len(ev) != 0 || seq != latest {
		t.Fatalf("超时应返回空和 latest: n=%d seq=%d latest=%d", len(ev), seq, latest)
	}
	if d := time.Since(t0); d < 50*time.Millisecond {
		t.Fatalf("没等够 wait 就返回了: %v", d)
	}
}

// S6：after 早于缓冲最旧事件时从最旧的给，Seq 跳号可见。
func TestEventsAfterBeforeOldest(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{MaxEvents: 5})
	if err := r.Send("a"); err != nil {
		t.Fatalf("Send a: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 1 })
	if err := r.Send("b"); err != nil {
		t.Fatalf("Send b: %v", err)
	}
	waitEvents(t, r, func(all []Event) bool { return r.State().Turns >= 2 })

	ev, latest := r.Events(context.Background(), 0, 0, 0)
	// 总共 6 条只留 5 条，最旧 Seq=2 而不是 1——跳号能让调用方看出丢了
	if len(ev) != 5 || ev[0].Seq != 2 || ev[len(ev)-1].Seq != latest {
		t.Fatalf("fromOldest: n=%d first=%d last=%d latest=%d", len(ev), ev[0].Seq, ev[len(ev)-1].Seq, latest)
	}
	// after 超过最新：不返回事件，latest 照样给
	if ev2, seq := r.Events(context.Background(), latest+100, 0, 50*time.Millisecond); len(ev2) != 0 || seq != latest {
		t.Fatalf("after 超前: n=%d seq=%d", len(ev2), seq)
	}
}

// S6：Stop 与 Send 并发跑，不挂不竞态。
func TestConcurrentStopSend(t *testing.T) {
	r, _ := newTestRunner(t, RunnerConfig{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = r.Send(fmt.Sprintf("m%d", i)) // Stop 插队时写失败是允许的
		}(i)
		go func() {
			defer wg.Done()
			if err := r.Stop(); err != nil {
				t.Errorf("并发 Stop: %v", err)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("并发 Send/Stop 没收敛")
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("最终 Stop: %v", err)
	}
	if st := r.State(); st.Running {
		t.Fatalf("最终 Stop 后不应在跑: %+v", st)
	}
}

// Claude 子进程只继承白名单环境变量，不继承其他（如密钥）。
func TestClaudeEnvWhitelist(t *testing.T) {
	t.Setenv("FOO_SECRET", "s3cret")
	t.Setenv("CLAUDE_TEST_KEEP", "yes")
	t.Setenv("LC_ALL", "C")
	t.Setenv("https_proxy", "http://p:1")
	var keys []string
	for _, kv := range claudeEnv(os.Environ()) {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	has := func(k string) bool {
		for _, x := range keys {
			if x == k {
				return true
			}
		}
		return false
	}
	if has("FOO_SECRET") {
		t.Error("FOO_SECRET 不该传给 claude")
	}
	for _, k := range []string{"PATH", "CLAUDE_TEST_KEEP", "LC_ALL", "https_proxy"} {
		if !has(k) {
			t.Errorf("%s 应传给 claude", k)
		}
	}

	// 真实启动：额外的 cfg.Env 仍会追加
	r, _ := newTestRunner(t, RunnerConfig{Env: []string{"EXTRA_OK=1"}})
	if err := r.Send("hi"); err != nil {
		t.Fatal(err)
	}
	waitEvents(t, r, func(ev []Event) bool { return len(ev) >= 3 })
}
