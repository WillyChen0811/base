package macexec

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunNormalExit：正常结束拿到退出码、用时与 stdout+stderr 合并的尾部；非零退出码不算错误。
func TestRunNormalExit(t *testing.T) {
	res, err := Run(context.Background(), Spec{
		Action:  "test_exit",
		Argv:    []string{"sh", "-c", "echo to-stdout; echo to-stderr >&2; exit 3"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("非零退出码是结果不是错误，err=%v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode=%d，期望 3", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("TimedOut 应为 false")
	}
	if !strings.Contains(res.Tail, "to-stdout") || !strings.Contains(res.Tail, "to-stderr") {
		t.Errorf("Tail 应同时含 stdout 与 stderr：%q", res.Tail)
	}
	if res.Seconds < 0 || res.Seconds > 10 {
		t.Errorf("Seconds 不合理：%v", res.Seconds)
	}
}

// TestRunTimeoutKillsProcessGroup：超时时后台子进程（`&` 起的 sleep）必须随整个进程组一起被杀，
// TimedOut=true，且超时后 promptly 返回。
func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	// 用独一份的时长做标记，避免误伤别处的 sleep；到点前进程必须还活着，才能被超时杀掉。
	const marker = "sleep 82111"
	start := time.Now()
	res, err := Run(context.Background(), Spec{
		Action:  "test_timeout",
		Argv:    []string{"sh", "-c", marker + " & " + marker},
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("超时是结果不是错误，err=%v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut 应为 true：%+v", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("超时后应 promptly 返回（WaitDelay 5s 内），实际 %v", elapsed)
	}
	// SIGKILL 之后进程消失还要一点时间：轮询到没有为止，等满仍有残留即失败。
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", marker).Output()
		if pids := strings.TrimSpace(string(out)); pids != "" {
			if time.Now().After(deadline) {
				_ = exec.Command("pkill", "-f", marker).Run() // 失败路径兜底，别把孤儿留给整机
				t.Fatalf("进程组没被杀干净，残留 pid：%s", pids)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		break
	}
}

// TestRunBusySecondCall：第一个任务在跑时，第二个调用立刻拿到 ErrBusy，不排队等。
func TestRunBusySecondCall(t *testing.T) {
	running := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(running)
		_, err := Run(context.Background(), Spec{
			Action:  "busy_hold",
			Argv:    []string{"sleep", "2"},
			Timeout: 30 * time.Second,
		})
		done <- err
	}()
	<-running
	time.Sleep(100 * time.Millisecond) // 让 goroutine 真正把锁拿到

	began := time.Now()
	_, secondErr := Run(context.Background(), Spec{
		Action:  "busy_try",
		Argv:    []string{"true"},
		Timeout: 5 * time.Second,
	})
	busyElapsed := time.Since(began) // 计时在这里取：<-done 会等第一个任务跑完，不能算进 ErrBusy 的响应
	firstErr := <-done               // 先收回第一个任务，保证锁一定释放，不影响后面的测试

	if !errors.Is(secondErr, ErrBusy) {
		t.Fatalf("第二个调用应拿 ErrBusy，实际 err=%v", secondErr)
	}
	if busyElapsed > time.Second {
		t.Errorf("ErrBusy 应立刻返回，实际等了 %v", busyElapsed)
	}
	if firstErr != nil {
		t.Fatalf("第一个任务失败：%v", firstErr)
	}
}
