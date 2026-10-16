package support

// /exec/* 远端接口的最小测试（见 tasks-macexec/S3.md）：
// 令牌错 401、未知动作 400、忙 409、logs 名字不对 400。HOME 一律换临时目录。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// macexecRemote 把 HOME 换成临时目录、开一次协助，返回远端地址基串、令牌和 HOME。
func macexecRemote(t *testing.T) (base, token, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	s, fr := newTestService(t, Config{})
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return "http://" + s.Status().RemoteAddr, fr.publishRec(0).token, home
}

func TestExecRunBadToken(t *testing.T) {
	base, _, _ := macexecRemote(t)
	if code, _ := remoteDo(t, http.MethodPost, base+"/exec/run", "", []byte(`{"action":"git_status","repo":"xxyi"}`)); code != http.StatusUnauthorized {
		t.Fatalf("不带令牌应 401，得 %d", code)
	}
	if code, _ := remoteDo(t, http.MethodPost, base+"/exec/run", "nope", []byte(`{"action":"git_status","repo":"xxyi"}`)); code != http.StatusUnauthorized {
		t.Fatalf("错令牌应 401，得 %d", code)
	}
	if code, _ := remoteDo(t, http.MethodGet, base+"/exec/logs?name=service", "nope", nil); code != http.StatusUnauthorized {
		t.Fatalf("logs 错令牌应 401，得 %d", code)
	}
}

func TestExecRunUnknownAction(t *testing.T) {
	base, token, _ := macexecRemote(t)
	code, body := remoteDo(t, http.MethodPost, base+"/exec/run", token, []byte(`{"action":"rm_rf"}`))
	if code != http.StatusBadRequest {
		t.Fatalf("未知动作应 400，得 %d: %s", code, body)
	}
	var eb struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &eb); err != nil || !strings.Contains(eb.Error, "未知动作") {
		t.Fatalf("400 正文应带原因，得 %s", body)
	}
}

func TestExecRunBusy(t *testing.T) {
	base, token, home := macexecRemote(t)
	dir := filepath.Join(home, "xxyi-work", "xxyi", "mac")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// build_app 跑 <mac>/build.sh：先落一个 marker 再 sleep，让测试能确定 busy 已被占住。
	script := filepath.Join(dir, "build.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$HOME/marker\"\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, base+"/exec/run", bytes.NewReader([]byte(`{"action":"build_app"}`)))
		if err != nil {
			res <- -1
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			res <- -1
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		res <- resp.StatusCode
	}()
	waitFor(t, 10*time.Second, "build.sh 起跑", func() bool {
		_, err := os.Stat(filepath.Join(home, "marker"))
		return err == nil
	})
	// 此刻单任务锁必已被第一个请求持有：第二个请求应 409，而不是排队等。
	code, body := remoteDo(t, http.MethodPost, base+"/exec/run", token, []byte(`{"action":"git_status","repo":"xxyi"}`))
	if code != http.StatusConflict {
		t.Fatalf("忙时应 409，得 %d: %s", code, body)
	}
	if c := <-res; c != http.StatusOK {
		t.Fatalf("第一个任务应正常 200，得 %d", c)
	}
}

func TestExecLogsBadName(t *testing.T) {
	base, token, _ := macexecRemote(t)
	for _, q := range []string{"name=bogus", "name=ui", "name=", "name=service.log"} {
		code, body := remoteDo(t, http.MethodGet, base+"/exec/logs?"+q, token, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，得 %d: %s", q, code, body)
		}
	}
}
