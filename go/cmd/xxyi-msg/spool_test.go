package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeEndpoint 写一个指向 addr 的 endpoint.json（Ubuntu 一侧）。
func writeEndpoint(t *testing.T, path, addr string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"addr":%q,"token":"tk"}`, addr)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stageOffline 在通道断着（addr 没人听）的时候发 n 条消息，要求全部落进暂存。
func stageOffline(t *testing.T, addr string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
		if code := run([]string{"send", "mac:boss", fmt.Sprintf("第 %d 条", i+1)}, strings.NewReader(""), out, errOut); code != exitOK {
			t.Fatalf("第 %d 条 send 退出码 %d，stderr %q", i+1, code, errOut.String())
		}
		if !strings.Contains(out.String(), "已暂存") {
			t.Fatalf("第 %d 条输出 %q，该是已暂存", i+1, out.String())
		}
	}
}

// Ubuntu 侧 send 到没人听的地址：退出 0 并暂存；通道恢复后任何命令都先补发（带合法签名），发出去就删文件。
func TestSpoolStagesThenFlushes(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	ca := setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "线路断了也要发出去"}, strings.NewReader(""), out, errOut); code != exitOK {
		t.Fatalf("send 退出码 %d，stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "已暂存") {
		t.Fatalf("输出 %q", out.String())
	}
	files := spoolList(spoolDirUbuntu)
	if len(files) != 1 {
		t.Fatalf("暂存 %d 条", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var entry spoolEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("暂存内容不是合法 JSON：%v", err)
	}
	if entry.Method != http.MethodPost || entry.Path != "/msg/send" {
		t.Errorf("暂存条目 %s %s", entry.Method, entry.Path)
	}
	var staged sendBody
	if err := json.Unmarshal(entry.Body, &staged); err != nil || staged.Text != "线路断了也要发出去" {
		t.Errorf("暂存正文 %q err=%v", staged.Text, err)
	}

	// 通道恢复：同一地址重开假服务，跑 who 时顺手把上一条补发掉
	var cap captured
	startServerAt(t, addr, cap.handler())
	out.Reset()
	if code := run([]string{"who"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("who 退出码 %d", code)
	}
	cap.mu.Lock()
	reqs := append([]capturedReq(nil), cap.reqs...)
	cap.mu.Unlock()
	if len(reqs) != 2 || reqs[0].path != "/msg/send" || reqs[1].path != "/msg/who" {
		t.Fatalf("补发顺序不对：%v", cap.queries())
	}
	verifyHdr(t, ca, reqs[0]) // 补发每次现签新头
	if len(spoolList(spoolDirUbuntu)) != 0 {
		t.Errorf("补发成功后暂存该清空，还在：%v", spoolList(spoolDirUbuntu))
	}
	if len(spoolList(filepath.Join(spoolDirUbuntu, spoolRejectedSub))) != 0 {
		t.Errorf("没被拒过却进了 rejected/")
	}
}

// 服务端明确拒绝补发：文件移进 rejected/ 并记下原因，收件箱顶注说清几条被拒；下一轮不再重复提示。
func TestSpoolRejectedArchivedAndNoted(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	stageOffline(t, addr, 1)

	var cap captured
	cap.respSendErr = "收件人不存在"
	startServerAt(t, addr, cap.handler())

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := run([]string{"inbox"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("inbox 退出码 %d，stderr %q", code, errOut.String())
	}
	first, _, _ := strings.Cut(strings.TrimRight(out.String(), "\n"), "\n")
	if !strings.Contains(first, "有 1 条暂存消息被拒") || !strings.Contains(first, "收件人不存在") {
		t.Fatalf("顶注 %q", first)
	}
	if n := len(spoolList(spoolDirUbuntu)); n != 0 {
		t.Errorf("被拒的还留在暂存里：%d 条", n)
	}
	rejected := spoolList(filepath.Join(spoolDirUbuntu, spoolRejectedSub))
	if len(rejected) != 1 {
		t.Fatalf("rejected/ 里 %d 个文件", len(rejected))
	}
	raw, err := os.ReadFile(rejected[0])
	if err != nil {
		t.Fatal(err)
	}
	var kept spoolEntry
	if err := json.Unmarshal(raw, &kept); err != nil {
		t.Fatal(err)
	}
	if kept.RejectReason != "收件人不存在" || kept.RejectedAt == nil {
		t.Errorf("归档条目 %+v", kept)
	}

	// 下一轮没有新的被拒：顶注不该再出现
	out.Reset()
	if code := run([]string{"inbox"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("第二次 inbox 退出码 %d", code)
	}
	if strings.Contains(out.String(), "被拒") {
		t.Errorf("第二轮还挂着顶注：%q", out.String())
	}
}

// 暂存满 50 条就不再暂存：连不上原样报错，也不该把已有的挤掉。
func TestSpoolRefusesWhenFull(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	for i := 0; i < spoolMaxCount; i++ {
		if _, err := spoolAdd(spoolDirUbuntu, http.MethodPost, "/msg/send",
			[]byte(`{"from":"ubuntu:cw","to":"mac:boss","text":"占位"}`)); err != nil {
			t.Fatalf("预置第 %d 条失败：%v", i+1, err)
		}
	}
	if n := len(spoolList(spoolDirUbuntu)); n != spoolMaxCount {
		t.Fatalf("预置后暂存 %d 条", n)
	}

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "挤不进去了"}, strings.NewReader(""), out, errOut); code != exitErr {
		t.Fatalf("满了还退出 %d，stdout %q", code, out.String())
	}
	if strings.Contains(out.String(), "已暂存") {
		t.Errorf("满了不该说已暂存：%q", out.String())
	}
	if !strings.Contains(errOut.String(), "连不上") {
		t.Errorf("stderr %q", errOut.String())
	}
	if n := len(spoolList(spoolDirUbuntu)); n != spoolMaxCount {
		t.Errorf("暂存条数变成 %d", n)
	}
}

// state 的补发只有 stateDeadline 这一份预算：慢服务补不完就放下次的，命令本身按时返回。
func TestSpoolFlushHonorsStateDeadline(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	stageOffline(t, addr, 3)

	old := stateDeadline
	t.Cleanup(func() { stateDeadline = old })
	stateDeadline = 300 * time.Millisecond

	startServerAt(t, addr, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // 每个请求都慢，3 条补发加状态上报跑不进预算
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/msg/send":
			fmt.Fprint(w, `{"id":"sp1"}`)
		case "/msg/who", "/msg":
			fmt.Fprint(w, `{"peers":{"mac:a":"idle"}}`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	})

	start := time.Now()
	out := &bytes.Buffer{}
	if code := run([]string{"state", "busy"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("state 退出码 %d", code)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("state 耗时 %v，超出预算", d)
	}
	// 每条要 200ms、预算只有 300ms：3 条不可能补完，剩下的留给下次
	if left := len(spoolList(spoolDirUbuntu)); left == 0 {
		t.Errorf("预算内不该把 3 条全补完")
	}
}
