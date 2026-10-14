package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// clientIDPat：消息正文的去重 id 是 16 字节随机 hex（服务端接受 [A-Za-z0-9._-] ≤64 字节）。
var clientIDPat = regexp.MustCompile(`^[0-9a-f]{32}$`)

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

// dirNames 返回目录里全部条目的名字（含 .sending/.tmp/.reason，用来看认领与归档状态）；目录不存在返回空。
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// sendCount 数假服务收到的 /msg/send 请求条数。
func sendCount(c *captured) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.reqs {
		if r.path == "/msg/send" {
			n++
		}
	}
	return n
}

// Ubuntu 侧 send 到没人听的地址：退出 0 并暂存（正文带 client_id）；通道恢复后任何命令都先补发
// （带合法签名、正文原样），发出去就删文件。
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
	if !clientIDPat.MatchString(staged.ClientID) {
		t.Errorf("暂存正文的 client_id %q", staged.ClientID)
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
	if !bytes.Equal(reqs[0].raw, entry.Body) {
		t.Errorf("补发正文不是暂存原样：%s", reqs[0].raw)
	}
	if len(spoolList(spoolDirUbuntu)) != 0 {
		t.Errorf("补发成功后暂存该清空，还在：%v", spoolList(spoolDirUbuntu))
	}
	if len(dirNames(t, filepath.Join(spoolDirUbuntu, spoolRejectedSub))) != 0 {
		t.Errorf("没被拒过却进了 rejected/")
	}
}

// 命令行与 MCP 的每次 send/reply 都带新的随机 client_id（16 字节 hex，互不相同）。
func TestSendCarriesFreshClientIDs(t *testing.T) {
	var cap captured
	addr := listenFreeAddr(t)
	startServerAt(t, addr, cap.handler())
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	for _, text := range []string{"第一条", "第二条"} {
		if code := run([]string{"send", "mac:boss", text}, strings.NewReader(""), &bytes.Buffer{}, io.Discard); code != exitOK {
			t.Fatalf("send %q 退出码 %d", text, code)
		}
	}
	cap.mu.Lock()
	reqs := append([]capturedReq(nil), cap.reqs...)
	cap.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("收到 %d 个请求", len(reqs))
	}
	for i, r := range reqs {
		if !clientIDPat.MatchString(r.body.ClientID) {
			t.Errorf("第 %d 条 client_id %q", i+1, r.body.ClientID)
		}
	}
	if reqs[0].body.ClientID == reqs[1].body.ClientID {
		t.Errorf("两次 send 的 client_id 不该相同：%q", reqs[0].body.ClientID)
	}
}

// 响应丢失（假服务收下正文后掐断连接）：消息进了暂存，补发的正文与暂存原样一致——
// 同一个 client_id 原样重发，服务端按它去重，不会出两条。
func TestSpoolResendKeepsSameClientID(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	var mu sync.Mutex
	var lost []byte
	srv1 := startServerAt(t, addr, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/msg/send" {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			lost = b
			mu.Unlock()
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("假服务不支持 Hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("Hijack 失败：%v", err)
				return
			}
			conn.Close() // 正文已收到，响应直接掐掉
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	})

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "响应丢了也要算"}, strings.NewReader(""), out, errOut); code != exitOK {
		t.Fatalf("send 退出码 %d，stderr %q", code, errOut.String())
	}
	mu.Lock()
	sentBody := append([]byte(nil), lost...)
	mu.Unlock()
	if len(sentBody) == 0 {
		t.Fatal("假服务没收到正文")
	}
	var staged sendBody
	if err := json.Unmarshal(sentBody, &staged); err != nil || !clientIDPat.MatchString(staged.ClientID) {
		t.Fatalf("第一次发送的正文 %s err=%v", sentBody, err)
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
		t.Fatal(err)
	}
	if !bytes.Equal(entry.Body, sentBody) {
		t.Errorf("暂存正文不是原样保存")
	}

	srv1.Close()
	var cap captured
	cap.respID = "dup1"
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
	if !bytes.Equal(reqs[0].raw, sentBody) {
		t.Errorf("补发正文和原发送不一致：\n%s\n%s", reqs[0].raw, sentBody)
	}
	if len(spoolList(spoolDirUbuntu)) != 0 {
		t.Errorf("补发后暂存没清空")
	}
}

// 原子写：spoolAdd 只留下最终 .json，目录里不会有 .tmp/.sending 之类半截文件，内容原样。
func TestSpoolAddOnlyWritesFinalFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	body := []byte(`{"from":"ubuntu:cw","to":"mac:boss","text":"原子写","client_id":"aa"}`)
	name, n, err := spoolAdd(dir, http.MethodPost, "/msg/send", body)
	if err != nil || n != 1 {
		t.Fatalf("spoolAdd (%d, %v)", n, err)
	}
	names := dirNames(t, dir)
	if len(names) != 1 || names[0] != name {
		t.Fatalf("目录里 %v，只该有最终文件 %s", names, name)
	}
	if strings.Contains(name, spoolTmpSuffix) || strings.Contains(name, sendingSuffix) {
		t.Errorf("最终文件名不该带临时后缀：%s", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	var entry spoolEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("最终文件不是合法 JSON：%v", err)
	}
	if !bytes.Equal(entry.Body, body) || entry.Method != http.MethodPost || entry.Path != "/msg/send" {
		t.Errorf("条目 %+v", entry)
	}
}

// 两个 goroutine 同时 flushSpool：靠 rename 认领，同一条消息只发出去一次。
func TestSpoolConcurrentFlushSendsOnce(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	stageOffline(t, addr, 1)

	var cap captured
	startServerAt(t, addr, cap.handler())

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			flushSpool(context.Background())
		}()
	}
	wg.Wait()
	if n := sendCount(&cap); n != 1 {
		t.Fatalf("同一条消息补发了 %d 次", n)
	}
	if left := dirNames(t, spoolDirUbuntu); len(left) != 0 {
		t.Errorf("补发后目录里还有 %v", left)
	}
}

// 可重试与永久拒绝的分界：
// ① 401：留在暂存（rename 回原名）、就地停，下一条不越过它；
// ② 之后 400：第一条移入 rejected/（同名 json + .reason），第二条继续补发成功；
// ③ 单独跑 inbox 列出一次（正文第一行 + 原因），④ 再跑不再显示。
func TestSpoolRetryableStaysAndPermanentRejectedShownOnce(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	stageOffline(t, addr, 2)
	files := spoolList(spoolDirUbuntu)
	if len(files) != 2 {
		t.Fatalf("暂存 %d 条", len(files))
	}
	first, second := filepath.Base(files[0]), filepath.Base(files[1])

	var mu sync.Mutex
	var sends int
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/msg/send":
			io.Copy(io.Discard, r.Body)
			mu.Lock()
			sends++
			n := sends
			mu.Unlock()
			switch n {
			case 1:
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":"令牌过期"}`)
			case 2:
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"收件人不存在"}`)
			default:
				fmt.Fprint(w, `{"id":"ok7"}`)
			}
		case "/msg/inbox":
			fmt.Fprint(w, `{"messages":[],"unanswered":[]}`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}
	startServerAt(t, addr, handler)

	// ① 401：可重试——这条 rename 回原名，补发就地停
	sent, err := flushSpool(context.Background())
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusUnauthorized {
		t.Fatalf("401 阶段错误 %v", err)
	}
	if len(sent) != 0 {
		t.Errorf("401 阶段 sent %v", sent)
	}
	left := spoolList(spoolDirUbuntu)
	if len(left) != 2 || filepath.Base(left[0]) != first {
		t.Fatalf("401 后暂存 %v，该原样留 2 条", left)
	}
	if names := dirNames(t, spoolDirUbuntu); len(names) != 2 {
		t.Errorf("401 后目录里 %v，不该有认领残留", names)
	}
	mu.Lock()
	n1 := sends
	mu.Unlock()
	if n1 != 1 {
		t.Fatalf("401 后不该越过第二条，已发 %d 条", n1)
	}

	// ② 400：永久拒绝——归档；第二条继续补发成功
	sent, err = flushSpool(context.Background())
	if err != nil {
		t.Fatalf("400 阶段 flushSpool：%v", err)
	}
	if sent[second] != "ok7" {
		t.Errorf("第二条没补发成功：%v", sent)
	}
	rejected := dirNames(t, filepath.Join(spoolDirUbuntu, spoolRejectedSub))
	if len(rejected) != 2 || rejected[0] != first || rejected[1] != first+reasonSuffix {
		t.Fatalf("rejected/ 里 %v", rejected)
	}
	raw, err := os.ReadFile(filepath.Join(spoolDirUbuntu, spoolRejectedSub, first))
	if err != nil {
		t.Fatal(err)
	}
	var kept spoolEntry
	if err := json.Unmarshal(raw, &kept); err != nil {
		t.Fatal(err)
	}
	if kept.RejectReason != "收件人不存在" || kept.RejectStatus != http.StatusBadRequest || kept.RejectedAt == nil {
		t.Errorf("归档条目 %+v", kept)
	}
	if len(spoolList(spoolDirUbuntu)) != 0 {
		t.Errorf("处理完暂存该清空")
	}

	// ③ 单独跑 inbox：顶部列一次
	out := &bytes.Buffer{}
	if code := run([]string{"inbox"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("inbox 退出码 %d", code)
	}
	note, _, _ := strings.Cut(strings.TrimRight(out.String(), "\n"), "\n")
	if !strings.Contains(note, "有 1 条暂存消息被拒") ||
		!strings.Contains(note, "第 1 条") || !strings.Contains(note, "收件人不存在") {
		t.Fatalf("顶注 %q", note)
	}
	after := dirNames(t, filepath.Join(spoolDirUbuntu, spoolRejectedSub))
	if len(after) != 2 || after[1] != first+reasonSuffix+seenSuffix {
		t.Errorf("列出后 rejected/ 里 %v，.reason 该改名为 .reason.seen", after)
	}

	// ④ 再跑不再显示
	out.Reset()
	if code := run([]string{"inbox"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("第二次 inbox 退出码 %d", code)
	}
	if strings.Contains(out.String(), "被拒") {
		t.Errorf("列过一次还挂着顶注：%q", out.String())
	}
}

// 只有 400/403/404/409 算永久拒绝（归档 rejected/）；401/408/429/5xx 都可重试（留在暂存）。
func TestSpoolStatusClassification(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))

	for _, c := range []struct {
		status    int
		permanent bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true},
		{http.StatusConflict, true},
		{http.StatusUnauthorized, false},
		{http.StatusRequestTimeout, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusServiceUnavailable, false},
	} {
		spoolDir := filepath.Join(dir, fmt.Sprintf("spool-%d", c.status))
		old := spoolDirUbuntu
		t.Cleanup(func() { spoolDirUbuntu = old })
		spoolDirUbuntu = spoolDir

		srv := startServerAt(t, addr, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/msg/send" {
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(c.status)
				fmt.Fprintf(w, `{"error":"状态码 %d 的答复"}`, c.status)
				return
			}
			fmt.Fprint(w, `{"ok":true}`)
		})
		body := []byte(`{"from":"ubuntu:cw","to":"mac:boss","text":"hi","client_id":"cid1"}`)
		name, _, err := spoolAdd(spoolDir, http.MethodPost, "/msg/send", body)
		if err != nil {
			t.Fatalf("%d：spoolAdd %v", c.status, err)
		}
		sent, ferr := flushSpool(context.Background())
		if c.permanent {
			if ferr != nil || len(sent) != 0 {
				t.Errorf("%d：永久拒绝不该报错/补发成功：%v %v", c.status, sent, ferr)
			}
			if left := spoolList(spoolDir); len(left) != 0 {
				t.Errorf("%d：被拒的还留在暂存：%v", c.status, left)
			}
			raw, err := os.ReadFile(filepath.Join(spoolDir, spoolRejectedSub, name))
			if err != nil {
				t.Errorf("%d：没归档成 %v", c.status, err)
			} else {
				var kept spoolEntry
				if json.Unmarshal(raw, &kept) != nil || kept.RejectStatus != c.status {
					t.Errorf("%d：归档条目 %+v", c.status, kept)
				}
			}
		} else {
			var se *statusError
			if !errors.As(ferr, &se) || se.status != c.status {
				t.Errorf("%d：可重试错误没原样返回：%v", c.status, ferr)
			}
			if len(sent) != 0 {
				t.Errorf("%d：可重试不该算发出去：%v", c.status, sent)
			}
			left := spoolList(spoolDir)
			if len(left) != 1 || filepath.Base(left[0]) != name {
				t.Errorf("%d：可重试该留在暂存（原名），实际 %v", c.status, left)
			}
			if names := dirNames(t, filepath.Join(spoolDir, spoolRejectedSub)); len(names) != 0 {
				t.Errorf("%d：可重试不该进 rejected/：%v", c.status, names)
			}
		}
		srv.Close()
	}
}

// 补发时创建超过 24 小时的条目不再发：移入 rejected/，原因"暂存超过 24 小时，已放弃"。
func TestSpoolExpiredDropped(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))

	old := time.Now().Add(-25 * time.Hour)
	if err := os.MkdirAll(spoolDirUbuntu, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"from":"ubuntu:cw","to":"mac:boss","text":"压箱底的旧消息","client_id":"old1"}`
	raw, err := json.Marshal(spoolEntry{Created: old, Method: http.MethodPost, Path: "/msg/send", Body: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%019d-old.json", old.UnixNano())
	if err := os.WriteFile(filepath.Join(spoolDirUbuntu, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var cap captured
	startServerAt(t, addr, cap.handler())
	sent, ferr := flushSpool(context.Background())
	if ferr != nil || len(sent) != 0 {
		t.Fatalf("超期条目不该发出：%v %v", sent, ferr)
	}
	if n := sendCount(&cap); n != 0 {
		t.Fatalf("超期条目发了 %d 个请求", n)
	}
	rejected := dirNames(t, filepath.Join(spoolDirUbuntu, spoolRejectedSub))
	if len(rejected) != 2 || rejected[0] != name || rejected[1] != name+reasonSuffix {
		t.Fatalf("rejected/ 里 %v", rejected)
	}
	rawRej, err := os.ReadFile(filepath.Join(spoolDirUbuntu, spoolRejectedSub, name))
	if err != nil {
		t.Fatal(err)
	}
	var kept spoolEntry
	if err := json.Unmarshal(rawRej, &kept); err != nil {
		t.Fatal(err)
	}
	if kept.RejectReason != "暂存超过 24 小时，已放弃" || kept.RejectedAt == nil {
		t.Errorf("归档条目 %+v", kept)
	}
}

// 认领超时回收：超过 sendingStaleAfter 的 .sending 改回原名并照常补发；刚认领的不许动。
func TestSpoolStaleSendingReclaimed(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	body := func(text string) []byte {
		return []byte(`{"from":"ubuntu:cw","to":"mac:boss","text":"` + text + `","client_id":"cid"}`)
	}
	zombie, _, err := spoolAdd(spoolDirUbuntu, http.MethodPost, "/msg/send", body("认领它的进程死了"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(spoolDirUbuntu, zombie), filepath.Join(spoolDirUbuntu, zombie+sendingSuffix)); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(filepath.Join(spoolDirUbuntu, zombie+sendingSuffix), stale, stale); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := spoolAdd(spoolDirUbuntu, http.MethodPost, "/msg/send", body("刚被别人认领"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(spoolDirUbuntu, fresh), filepath.Join(spoolDirUbuntu, fresh+sendingSuffix)); err != nil {
		t.Fatal(err)
	}

	var cap captured
	cap.respID = "rec1"
	startServerAt(t, addr, cap.handler())
	sent, ferr := flushSpool(context.Background())
	if ferr != nil {
		t.Fatalf("flushSpool %v", ferr)
	}
	if sent[zombie] != "rec1" {
		t.Errorf("陈旧认领的条目没被回收补发：%v", sent)
	}
	if n := sendCount(&cap); n != 1 {
		t.Fatalf("补发请求 %d 条", n)
	}
	if names := dirNames(t, spoolDirUbuntu); len(names) != 1 || names[0] != fresh+sendingSuffix {
		t.Errorf("目录里 %v：新鲜的 .sending 不该被动过", names)
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
		if _, _, err := spoolAdd(spoolDirUbuntu, http.MethodPost, "/msg/send",
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

// 暂存里压着老消息 A 时发新消息 B：B 先进暂存、随队列按序补发——A 先到服务端，B 绝不插队。
// 假服务对第一个 /msg/send 回 500（可重试）：run 开头的预补发把 A 放回暂存就地停；
// doSendSpool 于是让 B 先进队列再补发，A 重发成功后才轮到 B。两次 A 的正文（含 client_id）逐字节一致。
func TestSpoolSendFlushesOldFirst(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	stageOffline(t, addr, 1) // A：第 1 条

	var mu sync.Mutex
	var sends int
	var rec []string // 「正文|client_id」按到达顺序
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/msg/send":
			b, _ := io.ReadAll(r.Body)
			var sd sendBody
			_ = json.Unmarshal(b, &sd)
			mu.Lock()
			sends++
			first := sends == 1
			rec = append(rec, sd.Text+"|"+sd.ClientID)
			mu.Unlock()
			if first {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":"服务器打了个盹"}`)
				return
			}
			fmt.Fprint(w, `{"id":"new9"}`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}
	startServerAt(t, addr, handler)

	out := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "后来的 B"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("send 退出码 %d", code)
	}
	if out.String() != "已排队 new9\n" {
		t.Errorf("输出 %q", out.String())
	}
	mu.Lock()
	got := append([]string(nil), rec...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("服务端收到 %v，该是 A(500)、A(200)、B(200)", got)
	}
	if got[0] != got[1] {
		t.Errorf("A 的两次重发正文不一致：%q %q", got[0], got[1])
	}
	bText, bCID, _ := strings.Cut(got[2], "|")
	if bText != "后来的 B" || !clientIDPat.MatchString(bCID) {
		t.Errorf("B 没排在 A 之后：%q", got[2])
	}
	if left := dirNames(t, spoolDirUbuntu); len(left) != 0 {
		t.Errorf("补发完目录里还有 %v", left)
	}
}

// state 钩子不补发：状态上报是送达确认的唯一依据，不能被补发挤掉。
// 暂存压着两条时跑 state：假服务只收到 /msg/state，一条 /msg/send 都没有，暂存原样留着。
func TestStateDoesNotFlushSpool(t *testing.T) {
	addr := listenFreeAddr(t)
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	writeEndpoint(t, ep, addr)
	setPaths(t, ep, filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	stageOffline(t, addr, 2)

	var cap captured
	startServerAt(t, addr, cap.handler())
	if code := run([]string{"state", "busy"}, strings.NewReader(""), &bytes.Buffer{}, io.Discard); code != exitOK {
		t.Fatalf("state 退出码 %d", code)
	}
	cap.mu.Lock()
	paths := make([]string, 0, len(cap.reqs))
	for _, r := range cap.reqs {
		paths = append(paths, r.path)
	}
	cap.mu.Unlock()
	if len(paths) != 1 || paths[0] != "/msg/state" {
		t.Fatalf("state 发了 %v，不该有任何 /msg/send", paths)
	}
	if n := len(spoolList(spoolDirUbuntu)); n != 2 {
		t.Errorf("state 后暂存剩 %d 条，该原样留 2 条", n)
	}
}
