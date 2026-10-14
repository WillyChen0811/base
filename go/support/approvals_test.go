package support

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// approvalsFix 打包一个开了协助的服务 + 本机/远端访问入口；approvalWait 可调、测后还原。
type approvalsFix struct {
	s     *Service
	base  string // http://<远端监听地址>
	token string
	lbase string // 本机 LocalHandler 的根
}

func newApprovalsFix(t *testing.T) *approvalsFix {
	t.Helper()
	oldWait, oldMax := approvalWait, maxApprovalPending
	t.Cleanup(func() { approvalWait, maxApprovalPending = oldWait, oldMax })
	s, fr := newTestService(t, Config{})
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return &approvalsFix{
		s:     s,
		base:  "http://" + s.Status().RemoteAddr,
		token: fr.publishRec(0).token,
		lbase: newServer(t, s.LocalHandler()).URL,
	}
}

// hookBash 拼一条 Bash 的 PermissionRequest 钩子输入。
func hookBash(cmd string) string {
	q, _ := json.Marshal(cmd)
	return `{"session_id":"s1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":` +
		string(q) + `}}`
}

// approvalAskBody 拼 /approval/ask 的正文；hook 是钩子输入原样 JSON。
func approvalAskBody(addr, agent, cwd, hook string) []byte {
	b, _ := json.Marshal(map[string]any{"addr": addr, "agent": agent, "cwd": cwd, "hook": json.RawMessage(hook)})
	return b
}

// approvalAskOut 是一次后台 ask 的结果（在 goroutine 里跑，不能 t.Fatal，错误走这里）。
type approvalAskOut struct {
	code int
	body []byte
	err  error
}

func (o approvalAskOut) decision(t *testing.T) (decided, allow bool, message string) {
	t.Helper()
	if o.err != nil {
		t.Fatalf("ask 请求失败: %v", o.err)
	}
	if o.code != 200 {
		t.Fatalf("ask 应 200，得 %d %s", o.code, o.body)
	}
	var r struct {
		Decided bool   `json:"decided"`
		Allow   bool   `json:"allow"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(o.body, &r); err != nil {
		t.Fatalf("解 ask 响应: %v %s", err, o.body)
	}
	return r.Decided, r.Allow, r.Message
}

// approvalAskAsync 后台发一次 ask（阻塞接口）；token/certHdr 可空。
func approvalAskAsync(rawURL, token, certHdr string, body []byte) <-chan approvalAskOut {
	ch := make(chan approvalAskOut, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
		if err != nil {
			ch <- approvalAskOut{err: err}
			return
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if certHdr != "" {
			req.Header.Set("X-XXYI-Auth", certHdr)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			ch <- approvalAskOut{err: err}
			return
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		ch <- approvalAskOut{code: resp.StatusCode, body: buf.Bytes()}
	}()
	return ch
}

// approvalGET 拉 GET /approval 的待批列表。
func approvalGET(t *testing.T, lbase string) []ApprovalRequest {
	t.Helper()
	code, body := get(t, lbase+"/approval")
	if code != 200 {
		t.Fatalf("GET /approval: %d %s", code, body)
	}
	var r struct {
		Requests []ApprovalRequest `json:"requests"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("解 /approval: %v %s", err, body)
	}
	return r.Requests
}

// approvalWaitOne 等列表里恰好攒够 n 条并返回。
func approvalWaitOne(t *testing.T, lbase string, n int) []ApprovalRequest {
	t.Helper()
	var got []ApprovalRequest
	waitFor(t, 3*time.Second, "待批列表攒够", func() bool {
		got = approvalGET(t, lbase)
		return len(got) == n
	})
	return got
}

// approvalDecide 本机下决定，断言 200。
func approvalDecide(t *testing.T, lbase, id string, allow bool, message string) {
	t.Helper()
	q := url.Values{"id": {id}, "allow": {"0"}}
	if allow {
		q.Set("allow", "1")
	}
	if message != "" {
		q.Set("message", message)
	}
	code, body := post(t, lbase+"/approval/decide?"+q.Encode(), nil)
	if code != 200 {
		t.Fatalf("decide: %d %s", code, body)
	}
}

// TestApprovalRemoteAskDecideAllow 远端 ask（ubuntu 地址）阻塞 → 本机看得到 → 允许后放行。
func TestApprovalRemoteAskDecideAllow(t *testing.T) {
	f := newApprovalsFix(t)
	ch := approvalAskAsync(f.base+"/approval/ask", f.token, "",
		approvalAskBody("ubuntu:bob", "claude", "/srv/app", hookBash("rm -rf /tmp/x")))

	got := approvalWaitOne(t, f.lbase, 1)
	r := got[0]
	if r.ID == "" || r.Addr != "ubuntu:bob" || r.Agent != "claude" || r.Cwd != "/srv/app" {
		t.Fatalf("请求字段不对: %+v", r)
	}
	if r.Tool != "Bash" || r.Summary != "rm -rf /tmp/x" {
		t.Fatalf("Tool/Summary 不对: %+v", r)
	}
	if r.Created.IsZero() {
		t.Fatalf("Created 应有值: %+v", r)
	}
	// 没决定前 ask 不该返回。
	select {
	case out := <-ch:
		t.Fatalf("ask 不该提前返回: %d %s %v", out.code, out.body, out.err)
	default:
	}

	approvalDecide(t, f.lbase, r.ID, true, "")
	decided, allow, msg := (<-ch).decision(t)
	if !decided || !allow || msg != "" {
		t.Fatalf("允许应返回 decided+allow，得 decided=%v allow=%v msg=%q", decided, allow, msg)
	}
	if got := approvalGET(t, f.lbase); len(got) != 0 {
		t.Fatalf("决定后列表应空，得 %+v", got)
	}
	// 再决定同一条应 404。
	if code, _ := post(t, f.lbase+"/approval/decide?id="+r.ID+"&allow=1", nil); code != 404 {
		t.Fatalf("已决定的 id 应 404，得 %d", code)
	}
}

// TestApprovalDecideDenyWithMessage 拒绝带 message → ask 返回 allow=false 和原因。
func TestApprovalDecideDenyWithMessage(t *testing.T) {
	f := newApprovalsFix(t)
	ch := approvalAskAsync(f.base+"/approval/ask", f.token, "",
		approvalAskBody("ubuntu:bob", "codex", "/home/bob/x", hookBash("git push --force")))

	got := approvalWaitOne(t, f.lbase, 1)
	approvalDecide(t, f.lbase, got[0].ID, false, "不许强推")
	decided, allow, msg := (<-ch).decision(t)
	if !decided || allow || msg != "不许强推" {
		t.Fatalf("应返回 allow=false+原因，得 decided=%v allow=%v msg=%q", decided, allow, msg)
	}
}

// TestApprovalLocalAsk 本机 ask（mac: 侧）同样阻塞并出现在列表里。
func TestApprovalLocalAsk(t *testing.T) {
	f := newApprovalsFix(t)
	ch := approvalAskAsync(f.lbase+"/approval/ask", "", "",
		approvalAskBody("mac:frank", "claude", "/Users/frank", hookBash("ls")))

	got := approvalWaitOne(t, f.lbase, 1)
	if got[0].Addr != "mac:frank" || got[0].Tool != "Bash" || got[0].Summary != "ls" {
		t.Fatalf("本机请求字段不对: %+v", got[0])
	}
	approvalDecide(t, f.lbase, got[0].ID, true, "")
	if decided, allow, _ := (<-ch).decision(t); !decided || !allow {
		t.Fatalf("应放行，得 decided=%v allow=%v", decided, allow)
	}
}

// TestApprovalListOrder 两条 ask 按 Created 从旧到新排。
func TestApprovalListOrder(t *testing.T) {
	f := newApprovalsFix(t)
	ch1 := approvalAskAsync(f.base+"/approval/ask", f.token, "",
		approvalAskBody("ubuntu:a", "claude", "/1", hookBash("first")))
	one := approvalWaitOne(t, f.lbase, 1)
	ch2 := approvalAskAsync(f.base+"/approval/ask", f.token, "",
		approvalAskBody("ubuntu:b", "claude", "/2", hookBash("second")))
	got := approvalWaitOne(t, f.lbase, 2)
	if got[0].ID != one[0].ID || got[1].Addr != "ubuntu:b" {
		t.Fatalf("列表顺序应先到先列: %+v", got)
	}
	approvalDecide(t, f.lbase, got[0].ID, true, "")
	approvalDecide(t, f.lbase, got[1].ID, false, "no")
	(<-ch1).decision(t)
	<-ch2
}

// TestApprovalTimeout 超时（approvalWait 调小）→ decided:false，且已从簿中删除。
func TestApprovalTimeout(t *testing.T) {
	f := newApprovalsFix(t)
	approvalWait = 200 * time.Millisecond

	start := time.Now()
	ch := approvalAskAsync(f.base+"/approval/ask", f.token, "",
		approvalAskBody("ubuntu:bob", "claude", "/x", hookBash("sleep 1")))
	got := approvalWaitOne(t, f.lbase, 1)
	id := got[0].ID

	out := <-ch
	decided, _, _ := out.decision(t)
	if decided {
		t.Fatalf("没人决定应 decided:false，得 %s", out.body)
	}
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("应等满 approvalWait 才返回，实际 %v", el)
	}
	if got := approvalGET(t, f.lbase); len(got) != 0 {
		t.Fatalf("超时后列表应空，得 %+v", got)
	}
	// 超时后再 decide 应 404。
	if code, _ := post(t, f.lbase+"/approval/decide?id="+id+"&allow=1", nil); code != 404 {
		t.Fatalf("超时后的 id 再决定应 404，得 %d", code)
	}
}

// TestApprovalClientDisconnect 客户端 ctx cancel → 服务端把请求从簿里删掉。
func TestApprovalClientDisconnect(t *testing.T) {
	f := newApprovalsFix(t)
	ctx, cancel := context.WithCancel(context.Background())
	body := approvalAskBody("ubuntu:bob", "claude", "/x", hookBash("long-running"))
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.base+"/approval/ask", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.token)
		resp, err := httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	approvalWaitOne(t, f.lbase, 1) // 先确认已入簿再断开
	cancel()
	<-ch
	waitFor(t, 3*time.Second, "断开后清除请求", func() bool {
		return len(approvalGET(t, f.lbase)) == 0
	})
}

// TestApprovalSideAndBadInput 远端 mac: 地址 403、本机 ubuntu: 地址 403、坏地址 400、坏 hook 400、坏正文 400。
func TestApprovalSideAndBadInput(t *testing.T) {
	f := newApprovalsFix(t)

	// 远端只能 ubuntu: 一侧；阻塞校验都发生在入簿之前，可同步断言。
	if code, body := remoteDo(t, http.MethodPost, f.base+"/approval/ask", f.token,
		approvalAskBody("mac:frank", "claude", "/x", hookBash("ls"))); code != 403 {
		t.Fatalf("远端 mac: 地址应 403，得 %d %s", code, body)
	}
	if code, body := post(t, f.lbase+"/approval/ask", approvalAskBody("ubuntu:bob", "claude", "/x", hookBash("ls"))); code != 403 {
		t.Fatalf("本机 ubuntu: 地址应 403，得 %d %s", code, body)
	}
	if code, body := post(t, f.lbase+"/approval/ask", approvalAskBody("bob", "claude", "/x", hookBash("ls"))); code != 400 {
		t.Fatalf("坏地址应 400，得 %d %s", code, body)
	}
	// hook 事件不对 / 缺 tool_name / 不是 JSON → 都 400，且不入簿。
	badHooks := []string{
		`{"hook_event_name":"Other","tool_name":"Bash"}`,
		`{"hook_event_name":"PermissionRequest"}`,
		`not-json`,
	}
	for _, h := range badHooks {
		if code, body := post(t, f.lbase+"/approval/ask", approvalAskBody("mac:frank", "claude", "/x", h)); code != 400 {
			t.Fatalf("坏 hook %q 应 400，得 %d %s", h, code, body)
		}
	}
	if code, body := post(t, f.lbase+"/approval/ask", []byte("{")); code != 400 {
		t.Fatalf("坏正文应 400，得 %d %s", code, body)
	}
	if got := approvalGET(t, f.lbase); len(got) != 0 {
		t.Fatalf("被拒的请求不该入簿: %+v", got)
	}
	// Edit 摘要取 file_path；未知工具取 input 原文。
	ch := approvalAskAsync(f.lbase+"/approval/ask", "", "",
		approvalAskBody("mac:frank", "claude", "/x",
			`{"hook_event_name":"PermissionRequest","tool_name":"Edit","tool_input":{"file_path":"/a/b.txt"}}`))
	got := approvalWaitOne(t, f.lbase, 1)
	if got[0].Summary != "/a/b.txt" {
		t.Fatalf("Edit 摘要应是 file_path: %+v", got[0])
	}
	approvalDecide(t, f.lbase, got[0].ID, false, "")
	<-ch

	ch = approvalAskAsync(f.lbase+"/approval/ask", "", "",
		approvalAskBody("mac:frank", "mystery", "/x",
			`{"hook_event_name":"PermissionRequest","tool_name":"Mystery","tool_input":{"k":"v"}}`))
	got = approvalWaitOne(t, f.lbase, 1)
	if !strings.Contains(got[0].Summary, `"k":"v"`) {
		t.Fatalf("未知工具摘要应是 input 原文: %+v", got[0])
	}
	approvalDecide(t, f.lbase, got[0].ID, true, "")
	<-ch
}

// TestApprovalPendingLimit 待批满额后新的 ask 直接 503，已入簿的不受影响。
func TestApprovalPendingLimit(t *testing.T) {
	f := newApprovalsFix(t)
	maxApprovalPending = 2

	for i := 0; i < 2; i++ {
		if _, err := f.s.approvals.add(ApprovalRequest{
			Addr: "mac:frank", Agent: "claude", Tool: "Bash",
			Input: json.RawMessage(`{"command":"x"}`),
		}); err != nil {
			t.Fatalf("直接入簿 %d: %v", i, err)
		}
	}
	if got := approvalGET(t, f.lbase); len(got) != 2 {
		t.Fatalf("应能看到 2 条: %+v", got)
	}
	code, body := post(t, f.lbase+"/approval/ask", approvalAskBody("mac:frank", "claude", "/x", hookBash("ls")))
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "待批太多") {
		t.Fatalf("满额应 503 待批太多，得 %d %s", code, body)
	}
	// 腾出一个位置后又能入簿并放行。
	approvalDecide(t, f.lbase, approvalGET(t, f.lbase)[0].ID, true, "")
	ch := approvalAskAsync(f.lbase+"/approval/ask", "", "", approvalAskBody("mac:frank", "claude", "/x", hookBash("ls")))
	got := approvalWaitOne(t, f.lbase, 2)
	approvalDecide(t, f.lbase, got[1].ID, true, "")
	if decided, allow, _ := (<-ch).decision(t); !decided || !allow {
		t.Fatalf("腾位置后应能审批: decided=%v allow=%v", decided, allow)
	}
	// 清掉剩下一条，不然会话收尾时它一直挂着。
	approvalDecide(t, f.lbase, approvalGET(t, f.lbase)[0].ID, false, "收尾")
}

// TestApprovalDecideNotFound decide 不存在/allow 不对/方法不对。
func TestApprovalDecideNotFound(t *testing.T) {
	f := newApprovalsFix(t)
	if code, _ := post(t, f.lbase+"/approval/decide?id=nope&allow=1", nil); code != 404 {
		t.Fatalf("不存在的 id 应 404，得 %d", code)
	}
	if code, _ := post(t, f.lbase+"/approval/decide?id=nope&allow=2", nil); code != 400 {
		t.Fatalf("allow=2 应 400，得 %d", code)
	}
	if code, _ := get(t, f.lbase+"/approval/decide?id=nope&allow=1"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET decide 应 405，得 %d", code)
	}
	if code, _ := post(t, f.lbase+"/approval", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /approval 应 405，得 %d", code)
	}
}

// TestApprovalStopKeepsRequests 协助 Stop 不清通用审批的请求（与协助 Queue 的 CancelAll 无关）。
func TestApprovalStopKeepsRequests(t *testing.T) {
	f := newApprovalsFix(t)
	if _, err := f.s.approvals.add(ApprovalRequest{
		Addr: "mac:frank", Agent: "codex", Tool: "Bash",
		Input: json.RawMessage(`{"command":"x"}`),
	}); err != nil {
		t.Fatal(err)
	}
	f.s.Stop("测试收尾")
	got := approvalGET(t, f.lbase)
	if len(got) != 1 || got[0].Agent != "codex" {
		t.Fatalf("Stop 后通用审批请求应还在: %+v", got)
	}
	approvalDecide(t, f.lbase, got[0].ID, true, "")
}

// TestApprovalMsgAuthGating 配了 MsgAuth：/approval/ask 无证书一律 401（远端+本机）；
// ubuntu 证书远端 ask 正常走通；本机拿 ubuntu 证书发 mac: 地址 403；
// 内部接口 GET /approval、POST /approval/decide 不要证书。
func TestApprovalMsgAuthGating(t *testing.T) {
	f := newMsgAuthFix(t, true)
	body := approvalAskBody("ubuntu:bob", "claude", "/srv", hookBash("ls"))

	// 远端无证书（令牌有）→ 401。
	if code, b := authDo(t, http.MethodPost, f.base+"/approval/ask", f.token, "", body); code != 401 || !strings.Contains(string(b), "证书校验失败") {
		t.Fatalf("远端无证书应 401，得 %d %s", code, b)
	}
	// 本机无证书 → 401。
	if code, b := authDo(t, http.MethodPost, f.lbase+"/approval/ask", "", "", body); code != 401 {
		t.Fatalf("本机无证书应 401，得 %d %s", code, b)
	}
	// 本机 ubuntu 证书 + mac: 地址：证书能过、侧别/前缀不过 → 403。
	macBody := approvalAskBody("mac:frank", "claude", "/x", hookBash("ls"))
	if code, b := f.localAuth(t, http.MethodPost, "/approval/ask", f.ubiP, f.ubiC, macBody); code != 403 {
		t.Fatalf("ubuntu 证书发 mac: 地址应 403，得 %d %s", code, b)
	}

	// ubuntu 证书远端 ask：签名后异步请求，本机批准收尾。
	hdr := f.remoteSign(t, http.MethodPost, "/approval/ask", f.ubiP, f.ubiC, body)
	ch := approvalAskAsync(f.base+"/approval/ask", f.token, hdr, body)
	got := approvalWaitOne(t, f.lbase, 1)
	// 内部接口不带证书也放行（Origin 校验在 wgtund 路由层）。
	if code, _ := authDo(t, http.MethodPost, f.lbase+"/approval/decide?id="+got[0].ID+"&allow=1", "", "", nil); code != 200 {
		t.Fatalf("内部 decide 不该要证书，得 %d", code)
	}
	if decided, allow, _ := (<-ch).decision(t); !decided || !allow {
		t.Fatalf("应放行: %v %v", decided, allow)
	}
}
