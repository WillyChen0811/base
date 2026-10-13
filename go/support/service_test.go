package support

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 假 Remote 与测试辅助 ----------

type pubRec struct {
	addr, token string
	until       int64
}

// fakeRemote 在本机 127.0.0.1:0 起监听；可注入 Open/Publish 失败，
// 可强行关掉当前 listener 模拟断线。
type fakeRemote struct {
	mu           sync.Mutex
	openErr      error
	publishErr   error
	addrs        []string // 每次 Open 成功的地址
	publishes    []pubRec // Publish 调用记录
	unpublishCnt int
	listeners    []net.Listener
	gate         chan struct{} // 非 nil 时 Publish 先在此阻塞（不看 ctx），用来制造交错
	entered      chan struct{} // Publish 进入阻塞时发一个信号（缓冲 8）
}

func (f *fakeRemote) Open(ctx context.Context) (net.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.listeners = append(f.listeners, ln)
	f.addrs = append(f.addrs, ln.Addr().String())
	return ln, nil
}

func (f *fakeRemote) Publish(ctx context.Context, addr, token string, until int64) error {
	f.mu.Lock()
	g, ent := f.gate, f.entered
	f.mu.Unlock()
	if g != nil {
		select {
		case ent <- struct{}{}:
		default:
		}
		<-g
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return f.publishErr
	}
	f.publishes = append(f.publishes, pubRec{addr, token, until})
	return nil
}

func (f *fakeRemote) Unpublish(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpublishCnt++
	return nil
}

func (f *fakeRemote) setGate(g chan struct{}) {
	f.mu.Lock()
	f.gate = g
	f.entered = make(chan struct{}, 8)
	f.mu.Unlock()
}
func (f *fakeRemote) enteredCh() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entered
}
func (f *fakeRemote) lastPublish() pubRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishes[len(f.publishes)-1]
}
func (f *fakeRemote) setOpenErr(err error) { f.mu.Lock(); f.openErr = err; f.mu.Unlock() }
func (f *fakeRemote) setPublishErr(err error) {
	f.mu.Lock()
	f.publishErr = err
	f.mu.Unlock()
}
func (f *fakeRemote) publishCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.publishes)
}
func (f *fakeRemote) unpublishCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unpublishCnt
}
func (f *fakeRemote) publishRec(i int) pubRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishes[i]
}
func (f *fakeRemote) closeLast() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.listeners) > 0 {
		_ = f.listeners[len(f.listeners)-1].Close()
	}
}

// newTestService 建一个带假 claude 的服务；Claude 进程复用 claude_test.go 的假 claude 机制。
func newTestService(t *testing.T, cfg Config) (*Service, *fakeRemote) {
	t.Helper()
	if cfg.Runner.Bin == "" {
		cfg.Runner.Bin = os.Args[0]
		cfg.Runner.Dir = t.TempDir()
		cfg.Runner.Env = append(cfg.Runner.Env, "SUPPORT_FAKE_CLAUDE=1", "GORACE=atexit_sleep_ms=0")
	}
	fr := &fakeRemote{}
	s := NewService(cfg, fr)
	t.Cleanup(s.Close)
	return s, fr
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// remoteDo 直接访问远端 listener 上的地址。
func remoteDo(t *testing.T, method, rawURL, token string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(t, resp)
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("读响应: %v", err)
	}
	return buf.Bytes()
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待%s超时", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hookPost 发一次钩子请求，返回状态码和正文。
func hookPost(t *testing.T, url, token string, body string) (int, []byte) {
	t.Helper()
	return remoteDo(t, http.MethodPost, url+"?token="+token, "", []byte(body))
}

// hookDecisionOf 解出钩子输出的 behavior/message。
func hookDecisionOf(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var o struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(body, &o); err != nil {
		t.Fatalf("钩子输出不是 JSON: %v 原文 %q", err, body)
	}
	if o.HookSpecificOutput.HookEventName != "PermissionRequest" {
		t.Fatalf("hookEventName = %q", o.HookSpecificOutput.HookEventName)
	}
	return o.HookSpecificOutput.Decision.Behavior, o.HookSpecificOutput.Decision.Message
}

const hookBody = `{"session_id":"sess-9","hook_event_name":"PermissionRequest","tool_name":"Bash",` +
	`"tool_input":{"command":"ls -l"}}`

// ---------- 测试 ----------

func TestStartValidationAndPublish(t *testing.T) {
	s, fr := newTestService(t, Config{})

	for _, bad := range []int{0, 241, -1} {
		if err := s.Start(bad); err == nil {
			t.Fatalf("Start(%d) 应报错", bad)
		}
	}
	if st := s.Status(); st.Active || st.Until != 0 || st.RemoteAddr != "" {
		t.Fatalf("未开启的 Status = %+v", st)
	}
	if fr.publishCount() != 0 {
		t.Fatal("未开启不该 Publish")
	}

	// Open 失败：保持未开启
	fr.setOpenErr(errors.New("隧道断了"))
	if err := s.Start(10); err == nil || !strings.Contains(err.Error(), "隧道断了") {
		t.Fatalf("Open 失败应原样报错: %v", err)
	}
	if st := s.Status(); st.Active {
		t.Fatal("Open 失败后不该是开启状态")
	}
	fr.setOpenErr(nil)

	// Publish 失败：关掉已开的 listener，保持未开启
	fr.setPublishErr(errors.New("写不进远端"))
	if err := s.Start(10); err == nil {
		t.Fatal("Publish 失败应报错")
	}
	if st := s.Status(); st.Active || st.RemoteAddr != "" {
		t.Fatalf("Publish 失败后 Status = %+v", st)
	}
	if fr.publishCount() != 0 {
		t.Fatal("失败的 Publish 不该记账")
	}
	if c, err := net.Dial("tcp", fr.addrs[len(fr.addrs)-1]); err == nil {
		c.Close()
		t.Fatal("Publish 失败后应已关掉刚开的 listener")
	}
	fr.setPublishErr(nil)

	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec := fr.publishRec(0)
	if len(rec.token) != 64 || strings.TrimLeft(rec.token, "0123456789abcdef") != "" {
		t.Fatalf("远端令牌不是 64 位十六进制: %q", rec.token)
	}
	st := s.Status()
	if !st.Active || st.RemoteAddr != rec.addr {
		t.Fatalf("Status = %+v，Published = %+v", st, rec)
	}
	now := time.Now().Unix()
	if st.Until < now+29*60 || st.Until > now+31*60 {
		t.Fatalf("Until = %d，要在现在±30分钟附近", st.Until)
	}

	// 远端鉴权
	code, _ := remoteDo(t, http.MethodGet, "http://"+rec.addr+"/status", "", nil)
	if code != 401 {
		t.Fatalf("不带令牌应 401，得 %d", code)
	}
	code, _ = remoteDo(t, http.MethodGet, "http://"+rec.addr+"/status", "wrong-token", nil)
	if code != 401 {
		t.Fatalf("错令牌应 401，得 %d", code)
	}
	code, body := remoteDo(t, http.MethodGet, "http://"+rec.addr+"/status", rec.token, nil)
	if code != 200 {
		t.Fatalf("对令牌应 200，得 %d: %s", code, body)
	}
	var got Status
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解 Status: %v", err)
	}
	if !got.Active || got.Pending == nil || got.Repos == nil {
		t.Fatalf("远端 Status 正文不对: %s", body)
	}

	// 延长时长：令牌、地址不变，重新 Publish 一次新的截止时间
	if err := s.Start(60); err != nil {
		t.Fatalf("延长 Start: %v", err)
	}
	if fr.publishCount() != 2 {
		t.Fatalf("延长应重新 Publish 一次，count=%d", fr.publishCount())
	}
	rec2 := fr.publishRec(1)
	if rec2.token != rec.token || rec2.addr != rec.addr {
		t.Fatalf("延长应保持原令牌和地址: %+v vs %+v", rec2, rec)
	}
	if rec.until < now+29*60 || rec.until > now+31*60 {
		t.Fatalf("首次发布的 until = %d，要在现在±30分钟附近", rec.until)
	}
	if rec2.until < time.Now().Unix()+59*60 {
		t.Fatalf("延长后发布的 until = %d，要到 60 分钟后", rec2.until)
	}
	if st2 := s.Status(); st2.Until < time.Now().Unix()+59*60 {
		t.Fatalf("延长后 Until = %d，要到 60 分钟后", st2.Until)
	}

	// 延长时发布失败：只记日志，不报错
	fr.setPublishErr(errors.New("写不进远端"))
	if err := s.Start(90); err != nil {
		t.Fatalf("延长时发布失败不该报错: %v", err)
	}
	fr.setPublishErr(nil)
	found := false
	for _, e := range s.Log() {
		found = found || (e.Kind == "remote" && e.Detail["event"] == "republish_failed")
	}
	if !found {
		t.Fatal("延长时发布失败应记 republish_failed 日志")
	}
}

func TestRemoteTaskAndEvents(t *testing.T) {
	s, fr := newTestService(t, Config{})
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := s.Status().RemoteAddr
	token := fr.publishRec(0).token
	base := "http://" + addr

	// 还没发任务：events 是 [] 不是 null
	code, body := remoteDo(t, http.MethodGet, base+"/events?after=0", token, nil)
	if code != 200 || !strings.Contains(string(body), `"events":[]`) {
		t.Fatalf("空事件应带 \"events\":[]: %d %s", code, body)
	}

	code, body = remoteDo(t, http.MethodPost, base+"/task", token, []byte("你好"))
	if code != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("发任务: %d %s", code, body)
	}
	if code, _ := remoteDo(t, http.MethodPost, base+"/task", token, []byte("   ")); code != 400 {
		t.Fatalf("空白任务应 400，得 %d", code)
	}
	if code, _ := remoteDo(t, http.MethodPost, base+"/task", token, []byte(strings.Repeat("x", 64<<10+1))); code != 413 {
		t.Fatalf("超长任务应 413，得 %d", code)
	}
	if code, _ := remoteDo(t, http.MethodGet, base+"/task", token, nil); code != 405 {
		t.Fatalf("GET /task 应 405，得 %d", code)
	}
	if code, _ := remoteDo(t, http.MethodPost, base+"/task", "", []byte("x")); code != 401 {
		t.Fatalf("不带令牌应 401，得 %d", code)
	}

	// 轮询事件直到看到这一轮的 result
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, body = remoteDo(t, http.MethodGet, base+"/events?after=0&wait=2", token, nil)
		if code != 200 {
			t.Fatalf("取事件: %d %s", code, body)
		}
		var er struct {
			Events []Event `json:"events"`
			Last   int64   `json:"last"`
		}
		if err := json.Unmarshal(body, &er); err != nil {
			t.Fatalf("解事件: %v %s", err, body)
		}
		for _, e := range er.Events {
			m := dataOf(t, e)
			if m["type"] == "result" && m["result"] == "echo: 你好" {
				break
			}
		}
		if len(er.Events) > 0 && dataOf(t, er.Events[len(er.Events)-1])["type"] == "result" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 result 超时，最后响应: %s", body)
		}
	}

	// wait 参数范围
	for _, q := range []string{"wait=61", "wait=-1", "wait=abc"} {
		if code, _ := remoteDo(t, http.MethodGet, base+"/events?"+q, token, nil); code != 400 {
			t.Fatalf("%s 应 400，得 %d", q, code)
		}
	}

	// 停掉 Claude
	if code, body = remoteDo(t, http.MethodPost, base+"/stop-claude", token, nil); code != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("stop-claude: %d %s", code, body)
	}
	waitFor(t, 5*time.Second, "Claude 停止", func() bool { return !s.Status().Claude.Running })
}

func TestRemoteSync(t *testing.T) {
	local, remote := setupTestRepos(t)
	makeCommit(t, local, "file1")
	pushDefault(t, local, remote)
	wantHead := headCommit(t, remote, "master")

	s, fr := newTestService(t, Config{
		Repos: map[string]Repo{"app": {Dir: local, Remote: remote}},
	})
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := s.Status().RemoteAddr
	token := fr.publishRec(0).token
	post := func(query string) (int, []byte) {
		return remoteDo(t, http.MethodPost, "http://"+addr+"/sync?"+query, token, nil)
	}

	code, body := post("repo=app&branch=master")
	if code != 200 {
		t.Fatalf("同步成功应 200: %d %s", code, body)
	}
	var got struct {
		Head string `json:"head"`
	}
	if err := json.Unmarshal(body, &got); err != nil || got.Head != wantHead {
		t.Fatalf("head = %q，要 %s", got.Head, wantHead)
	}

	if code, _ := post("repo=nope&branch=master"); code != 404 {
		t.Fatalf("未知仓库应 404，得 %d", code)
	}
	if code, _ := post("repo=app&branch=" + url.QueryEscape("bad branch")); code != 400 {
		t.Fatalf("坏分支应 400，得 %d", code)
	}
	// 远端没有的分支 → git 失败 → 502
	if code, _ := post("repo=app&branch=feat%2Fnonexist"); code != 502 {
		t.Fatalf("远端没有的分支应 502，得 %d", code)
	}
	// 脏工作区 → 409
	if err := os.WriteFile(filepath.Join(local, "untracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body = post("repo=app&branch=master")
	if code != 409 {
		t.Fatalf("脏工作区应 409，得 %d: %s", code, body)
	}
	var eb struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &eb); err != nil || !strings.Contains(eb.Error, "工作区") {
		t.Fatalf("409 正文应带中文原因: %s", body)
	}
}

func TestRemoteHasNoControlEndpoints(t *testing.T) {
	s, fr := newTestService(t, Config{})
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := s.Status().RemoteAddr
	token := fr.publishRec(0).token

	if code, _ := remoteDo(t, http.MethodGet, "http://"+addr+"/support", "", nil); code != 401 {
		t.Fatalf("远端 /support 不带令牌应 401，得 %d", code)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/support"},
		{http.MethodPost, "/support/start?minutes=5"},
		{http.MethodPost, "/support/stop"},
		{http.MethodPost, "/support/decide?id=x&allow=1"},
		{http.MethodPost, "/support/hook?token=" + s.HookToken()},
	} {
		code, _ := remoteDo(t, c.method, "http://"+addr+c.path, token, []byte("{}"))
		if code != 404 {
			t.Fatalf("远端 %s %s 应 404，得 %d", c.method, c.path, code)
		}
	}
	// 确认这些请求确实没生效
	if st := s.Status(); !st.Active {
		t.Fatal("远端请求不该影响协助状态")
	}
}

func TestHookDecideFlow(t *testing.T) {
	s, fr := newTestService(t, Config{HookWait: 10 * time.Second})
	hsrv := newServer(t, s.HookHandler())
	lsrv := newServer(t, s.LocalHandler())
	hookURL := hsrv.URL + "/support/hook"

	// 协助未开：立即拒绝
	t0 := time.Now()
	code, body := hookPost(t, hookURL, s.HookToken(), hookBody)
	if code != 200 || time.Since(t0) > 2*time.Second {
		t.Fatalf("未开启应立刻 200: %d 用了 %v", code, time.Since(t0))
	}
	if b, m := hookDecisionOf(t, body); b != "deny" || m != "协助未开启" {
		t.Fatalf("未开启应拒绝协助未开启: %s %s", b, m)
	}

	// 令牌校验（未开也要先过令牌这一关）
	if code, _ := hookPost(t, hookURL, "wrong", hookBody); code != 401 {
		t.Fatalf("错令牌应 401，得 %d", code)
	}

	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fr.publishCount() != 1 {
		t.Fatalf("Start 后应 Publish 一次，count=%d", fr.publishCount())
	}

	// 开启后坏 JSON → 400 + 拒绝
	code, body = hookPost(t, hookURL, s.HookToken(), `{"broken":`)
	if code != 400 {
		t.Fatalf("坏 JSON 应 400，得 %d", code)
	}
	if b, m := hookDecisionOf(t, body); b != "deny" || m != "钩子输入无法解析" {
		t.Fatalf("坏 JSON 正文: %s %s", b, m)
	}

	// 允许
	type hookResp struct {
		code int
		body []byte
	}
	ch := make(chan hookResp, 1)
	go func() { c, b := hookPost(t, hookURL, s.HookToken(), hookBody); ch <- hookResp{c, b} }()
	waitFor(t, 5*time.Second, "待批出现", func() bool { return len(s.Status().Pending) == 1 })
	p := s.Status().Pending[0]
	if p.Tool != "Bash" || p.Summary != "ls -l" {
		t.Fatalf("待批内容: %+v", p)
	}
	code, body = remoteDo(t, http.MethodPost, lsrv.URL+"/support/decide?id="+p.ID+"&allow=1", "", nil)
	if code != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("decide: %d %s", code, body)
	}
	select {
	case hr := <-ch:
		if hr.code != 200 {
			t.Fatalf("钩子应 200，得 %d", hr.code)
		}
		if b, m := hookDecisionOf(t, hr.body); b != "allow" || m != "" {
			t.Fatalf("钩子决定 = %s %q", b, m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等钩子返回超时")
	}
	waitFor(t, 5*time.Second, "待批清空", func() bool { return len(s.Status().Pending) == 0 })

	// 拒绝带原因
	go func() { c, b := hookPost(t, hookURL, s.HookToken(), hookBody); ch <- hookResp{c, b} }()
	waitFor(t, 5*time.Second, "第二条待批", func() bool { return len(s.Status().Pending) == 1 })
	id := s.Status().Pending[0].ID
	q := url.Values{"id": {id}, "allow": {"0"}, "message": {"不行"}}
	code, body = remoteDo(t, http.MethodPost, lsrv.URL+"/support/decide?"+q.Encode(), "", nil)
	if code != 200 {
		t.Fatalf("decide deny: %d %s", code, body)
	}
	select {
	case hr := <-ch:
		if b, m := hookDecisionOf(t, hr.body); b != "deny" || m != "不行" {
			t.Fatalf("拒绝决定 = %s %q", b, m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等钩子返回超时")
	}

	// 本机 decide 的参数与不存在的 id
	if code, _ := remoteDo(t, http.MethodPost, lsrv.URL+"/support/decide?id="+id+"&allow=2", "", nil); code != 400 {
		t.Fatalf("allow=2 应 400，得 %d", code)
	}
	if code, _ := remoteDo(t, http.MethodPost, lsrv.URL+"/support/decide?id=nope&allow=1", "", nil); code != 404 {
		t.Fatalf("不存在的 id 应 404，得 %d", code)
	}
}

func TestHookTimeout(t *testing.T) {
	s, _ := newTestService(t, Config{HookWait: 200 * time.Millisecond})
	hsrv := newServer(t, s.HookHandler())
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t0 := time.Now()
	code, body := hookPost(t, hsrv.URL+"/support/hook", s.HookToken(), hookBody)
	el := time.Since(t0)
	if code != 200 {
		t.Fatalf("超时应 200 带拒绝: %d", code)
	}
	if el < 150*time.Millisecond || el > 5*time.Second {
		t.Fatalf("超时用了 %v，不像等过 200ms", el)
	}
	if b, m := hookDecisionOf(t, body); b != "deny" || m != "等待授权超时" {
		t.Fatalf("超时正文 = %s %q", b, m)
	}
	waitFor(t, 5*time.Second, "timeout 日志", func() bool {
		for _, e := range s.Log() {
			if e.Kind == "timeout" {
				return true
			}
		}
		return false
	})
}

func TestStopKillsEverything(t *testing.T) {
	s, fr := newTestService(t, Config{HookWait: 30 * time.Second})
	hsrv := newServer(t, s.HookHandler())
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := s.Status().RemoteAddr
	token := fr.publishRec(0).token

	if code, _ := remoteDo(t, http.MethodPost, "http://"+addr+"/task", token, []byte("hello")); code != 200 {
		t.Fatalf("发任务: %d", code)
	}
	waitFor(t, 15*time.Second, "一轮完成", func() bool { return s.Status().Claude.Turns >= 1 })

	// 让一个钩子挂住
	ch := make(chan int, 1)
	hbody := make(chan []byte, 1)
	go func() {
		c, b := hookPost(t, hsrv.URL+"/support/hook", s.HookToken(), hookBody)
		ch <- c
		hbody <- b
	}()
	waitFor(t, 5*time.Second, "钩子挂起", func() bool { return len(s.Status().Pending) == 1 })

	s.Stop("用户停止")

	select {
	case c := <-ch:
		b := <-hbody
		if c != 200 {
			t.Fatalf("Stop 后钩子应 200 拒绝，得 %d", c)
		}
		if bd, m := hookDecisionOf(t, b); bd != "deny" || m != "协助已结束" {
			t.Fatalf("Stop 后钩子 = %s %q", bd, m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 后挂着的钩子没被叫醒")
	}
	if !hasLog(s, "cancelled", func(d map[string]string) bool { return d["reason"] == "协助已结束" && d["id"] != "" }) {
		t.Fatal("Stop 时挂着的请求应记 cancelled 日志")
	}

	if fr.unpublishCount() != 1 {
		t.Fatalf("Unpublish 调用 %d 次，要 1", fr.unpublishCount())
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("Stop 后远端地址不该还能连")
	}
	st := s.Status()
	if st.Active || st.Until != 0 || st.RemoteAddr != "" || st.Claude.Running {
		t.Fatalf("Stop 后 Status = %+v", st)
	}

	s.Stop("再来一次") // 幂等
	if fr.unpublishCount() != 1 {
		t.Fatalf("第二次 Stop 不该再 Unpublish，count=%d", fr.unpublishCount())
	}
}

func TestAutoExpire(t *testing.T) {
	old := minuteUnit
	minuteUnit = 50 * time.Millisecond
	t.Cleanup(func() { minuteUnit = old })

	s, fr := newTestService(t, Config{})
	if err := s.Start(2); err != nil { // 2 个短分钟 = 100ms
		t.Fatalf("Start: %v", err)
	}
	if st := s.Status(); !st.Active {
		t.Fatal("刚 Start 应该在协助中")
	}
	waitFor(t, 5*time.Second, "到时自动结束", func() bool { return !s.Status().Active })
	waitFor(t, 5*time.Second, "Unpublish", func() bool { return fr.unpublishCount() >= 1 })
	waitFor(t, 5*time.Second, "stop 日志", func() bool {
		for _, e := range s.Log() {
			if e.Kind == "stop" && e.Detail["reason"] == "到时自动结束" {
				return true
			}
		}
		return false
	})
}

func TestRemoteReconnect(t *testing.T) {
	old := reconnectDelay
	reconnectDelay = 200 * time.Millisecond
	t.Cleanup(func() { reconnectDelay = old })

	s, fr := newTestService(t, Config{})
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr1 := s.Status().RemoteAddr
	token := fr.publishRec(0).token
	if code, _ := remoteDo(t, http.MethodGet, "http://"+addr1+"/status", token, nil); code != 200 {
		t.Fatalf("首个地址应先能服务: %d", code)
	}

	// 强行断掉当前 listener → 地址变空 → 重连后再 Publish
	fr.closeLast()
	waitFor(t, 5*time.Second, "地址变空", func() bool { return s.Status().RemoteAddr == "" })
	waitFor(t, 5*time.Second, "重连 Publish", func() bool { return fr.publishCount() >= 2 })

	rec2 := fr.publishRec(1)
	addr2 := rec2.addr
	if addr2 == addr1 {
		t.Fatalf("重连地址和原来一样: %s", addr2)
	}
	if rec2.token != token {
		t.Fatal("重连应保持原令牌")
	}
	waitFor(t, 5*time.Second, "新地址可服务", func() bool {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr2+"/status", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	})
	if got := s.Status().RemoteAddr; got != addr2 {
		t.Fatalf("RemoteAddr = %q，要 %q", got, addr2)
	}

	s.Stop("用户停止")
	if _, err := net.Dial("tcp", addr2); err == nil {
		t.Fatal("Stop 后新地址不该还能连")
	}
}

func TestLocalSupportStatusAndLogFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "support-log.jsonl")
	s, _ := newTestService(t, Config{LogPath: logPath})
	lsrv := newServer(t, s.LocalHandler())

	// 参数校验
	if code, _ := remoteDo(t, http.MethodPost, lsrv.URL+"/support/start", "", nil); code != 400 {
		t.Fatalf("缺 minutes 应 400，得 %d", code)
	}
	if code, _ := remoteDo(t, http.MethodPost, lsrv.URL+"/support/start?minutes=0", "", nil); code != 400 {
		t.Fatalf("minutes=0 应 400，得 %d", code)
	}

	code, body := remoteDo(t, http.MethodPost, lsrv.URL+"/support/start?minutes=5", "", nil)
	if code != 200 {
		t.Fatalf("本机 start: %d %s", code, body)
	}

	code, body = remoteDo(t, http.MethodGet, lsrv.URL+"/support", "", nil)
	if code != 200 {
		t.Fatalf("本机 status: %d %s", code, body)
	}
	var ls struct {
		Active  bool       `json:"active"`
		Until   int64      `json:"until"`
		Pending []Request  `json:"pending"`
		Log     []LogEntry `json:"log"`
	}
	if err := json.Unmarshal(body, &ls); err != nil {
		t.Fatalf("解本机 status: %v %s", err, body)
	}
	if !ls.Active || ls.Until == 0 || ls.Pending == nil || ls.Log == nil {
		t.Fatalf("本机 status 正文: %s", body)
	}
	sawStart := false
	for _, e := range ls.Log {
		if e.Kind == "start" {
			sawStart = true
		}
	}
	if !sawStart {
		t.Fatalf("log 里应有 start: %s", body)
	}

	s.Stop("用户停止")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读日志文件: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("日志文件才 %d 行: %q", len(lines), lines)
	}
	hasStart, hasStop := false, false
	for _, ln := range lines {
		var e LogEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("日志行不是 JSON: %q", ln)
		}
		if e.Kind == "start" {
			hasStart = true
		}
		if e.Kind == "stop" {
			hasStop = true
		}
	}
	if !hasStart || !hasStop {
		t.Fatalf("日志文件缺 start/stop: %q", lines)
	}
	if fi, err := os.Stat(logPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("日志文件权限不对: %v %v", fi, err)
	}
}

func hasLog(s *Service, kind string, match func(map[string]string) bool) bool {
	for _, e := range s.Log() {
		if e.Kind == kind && (match == nil || match(e.Detail)) {
			return true
		}
	}
	return false
}

// Stop 与在途 /task 交错：Stop 返回后 Claude 不在跑，且之后不会被重新拉起。
func TestStopVsInflightTask(t *testing.T) {
	for round := 0; round < 3; round++ {
		s, fr := newTestService(t, Config{})
		if err := s.Start(30); err != nil {
			t.Fatalf("Start: %v", err)
		}
		addr := s.Status().RemoteAddr
		token := fr.publishRec(0).token
		var wg sync.WaitGroup
		stopSend := make(chan struct{})
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stopSend:
						return
					default:
					}
					req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/task", strings.NewReader("hi"))
					req.Header.Set("Authorization", "Bearer "+token)
					if resp, err := httpClient.Do(req); err == nil {
						resp.Body.Close()
					}
				}
			}()
		}
		waitFor(t, 15*time.Second, "Claude 跑起来", func() bool { return s.Status().Claude.Running })
		s.Stop("用户停止")
		if s.Status().Claude.Running {
			t.Fatal("Stop 返回后 Claude 还在跑")
		}
		time.Sleep(500 * time.Millisecond)
		if s.Status().Claude.Running {
			t.Fatal("Stop 之后 Claude 被在途 /task 重新拉起")
		}
		close(stopSend)
		wg.Wait()
	}
}

// Stop 之后到达、以及 Stop 进行中到达的钩子都立即被拒绝，不进 Pending。
func TestHookDuringAndAfterStop(t *testing.T) {
	old := reconnectDelay
	reconnectDelay = 20 * time.Millisecond
	t.Cleanup(func() { reconnectDelay = old })

	s, fr := newTestService(t, Config{HookWait: 30 * time.Second})
	hsrv := newServer(t, s.HookHandler())
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 断线后重连的 Publish 卡住（不看 ctx），Stop 就会卡在等 goroutine 退出上。
	gate := make(chan struct{})
	fr.setGate(gate)
	fr.closeLast()
	select {
	case <-fr.enteredCh():
	case <-time.After(5 * time.Second):
		t.Fatal("重连没进 Publish")
	}
	stopped := make(chan struct{})
	go func() { s.Stop("用户停止"); close(stopped) }()
	waitFor(t, 5*time.Second, "Stop 开始", func() bool { return !s.Status().Active })

	t0 := time.Now()
	code, body := hookPost(t, hsrv.URL+"/support/hook", s.HookToken(), hookBody)
	if code != 200 || time.Since(t0) > 3*time.Second {
		t.Fatalf("Stop 进行中的钩子应很快拒绝: %d 用时 %v", code, time.Since(t0))
	}
	if b, _ := hookDecisionOf(t, body); b != "deny" {
		t.Fatalf("Stop 进行中的钩子 = %s", body)
	}
	if n := len(s.Status().Pending); n != 0 {
		t.Fatalf("Pending 应为空，得 %d", n)
	}
	select {
	case <-stopped:
		t.Fatal("Publish 还卡着，Stop 不该已返回")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("放开后 Stop 没返回")
	}

	t0 = time.Now()
	code, body = hookPost(t, hsrv.URL+"/support/hook", s.HookToken(), hookBody)
	if b, _ := hookDecisionOf(t, body); code != 200 || b != "deny" || time.Since(t0) > 3*time.Second {
		t.Fatalf("Stop 后的钩子: %d %s", code, body)
	}
	if n := len(s.Status().Pending); n != 0 {
		t.Fatalf("Stop 后 Pending 应为空，得 %d", n)
	}
}

// 断线重连中 Stop，随后立即 Start：新入口是新令牌，且没被旧 goroutine Unpublish / 覆盖。
func TestReconnectStopThenStart(t *testing.T) {
	old := reconnectDelay
	reconnectDelay = 20 * time.Millisecond
	t.Cleanup(func() { reconnectDelay = old })

	s, fr := newTestService(t, Config{})
	lsrv := newServer(t, s.LocalHandler())
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	oldTok := fr.publishRec(0).token
	gate := make(chan struct{})
	fr.setGate(gate)
	fr.closeLast()
	select {
	case <-fr.enteredCh():
	case <-time.After(5 * time.Second):
		t.Fatal("重连没进 Publish")
	}
	// 重连期间 RemoteAddr 为空（本机接口看到的也是空）
	code, body := remoteDo(t, http.MethodGet, lsrv.URL+"/support", "", nil)
	var ls struct {
		RemoteAddr string `json:"remote_addr"`
	}
	if code != 200 || json.Unmarshal(body, &ls) != nil || ls.RemoteAddr != "" {
		t.Fatalf("重连期间 remote_addr 应为空: %d %s", code, body)
	}

	stopped := make(chan struct{})
	go func() { s.Stop("用户停止"); close(stopped) }()
	time.Sleep(50 * time.Millisecond)
	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(30) }() // 排在 Stop 后面
	time.Sleep(50 * time.Millisecond)
	close(gate)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop 没返回")
	}
	if err := <-startErr; err != nil {
		t.Fatalf("再 Start: %v", err)
	}

	st := s.Status()
	if !st.Active || st.RemoteAddr == "" {
		t.Fatalf("新会话应在线: %+v", st)
	}
	last := fr.lastPublish()
	if last.token == oldTok || last.addr != st.RemoteAddr {
		t.Fatalf("最终入口应是新令牌新地址: %+v 旧令牌 %s 当前 %s", last, oldTok, st.RemoteAddr)
	}
	if n := fr.unpublishCount(); n != 1 {
		t.Fatalf("Unpublish 应只有 Stop 的那 1 次，得 %d", n)
	}
	time.Sleep(200 * time.Millisecond) // 旧 goroutine 若还活着，这里会有动静
	if fr.lastPublish() != last || fr.unpublishCount() != 1 || s.Status().RemoteAddr != last.addr {
		t.Fatal("新会话入口被旧会话的 goroutine 动了")
	}
	if c, _ := remoteDo(t, http.MethodGet, "http://"+last.addr+"/status", last.token, nil); c != 200 {
		t.Fatalf("新入口应可用: %d", c)
	}
	if c, _ := remoteDo(t, http.MethodGet, "http://"+last.addr+"/status", oldTok, nil); c != 401 {
		t.Fatalf("旧令牌应 401: %d", c)
	}
}

// 到点前 Start 延长，过了原到点时间仍在协助中。
func TestExtendBeatsTimer(t *testing.T) {
	old := minuteUnit
	minuteUnit = 100 * time.Millisecond
	t.Cleanup(func() { minuteUnit = old })

	s, _ := newTestService(t, Config{})
	if err := s.Start(3); err != nil { // 300ms
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := s.Start(10); err != nil { // 延长到约 1.15s
		t.Fatalf("延长: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // 已过原到点
	if !s.Status().Active {
		t.Fatal("延长后过了原到点仍应在协助中")
	}
	waitFor(t, 5*time.Second, "延长后的到点结束", func() bool { return !s.Status().Active })
}

// 两种令牌不能互换。
func TestTokensNotInterchangeable(t *testing.T) {
	s, fr := newTestService(t, Config{})
	hsrv := newServer(t, s.HookHandler())
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := s.Status().RemoteAddr
	remoteTok := fr.publishRec(0).token
	if code, _ := remoteDo(t, http.MethodGet, "http://"+addr+"/status", s.HookToken(), nil); code != 401 {
		t.Fatalf("钩子令牌打远端应 401，得 %d", code)
	}
	if code, _ := hookPost(t, hsrv.URL+"/support/hook", remoteTok, hookBody); code != 401 {
		t.Fatalf("远端令牌打钩子应 401，得 %d", code)
	}
}

func TestHookBodyTooLarge(t *testing.T) {
	s, _ := newTestService(t, Config{})
	hsrv := newServer(t, s.HookHandler())
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	big := `{"tool_name":"Bash","tool_input":{"command":"` + strings.Repeat("a", 1<<20) + `"}}`
	code, body := hookPost(t, hsrv.URL+"/support/hook", s.HookToken(), big)
	if code != 400 {
		t.Fatalf("超 1MB 应 400，得 %d", code)
	}
	if b, _ := hookDecisionOf(t, body); b != "deny" {
		t.Fatalf("超大正文应 deny: %s", body)
	}
	if len(s.Status().Pending) != 0 {
		t.Fatal("超大正文不该进 Pending")
	}
}

// 日志各类条目、ask/decide 带 id、cancelled 与 timeout 区分、0644 日志文件被收紧、Close 后不再开文件。
func TestAuditLogKinds(t *testing.T) {
	local, remote := setupTestRepos(t)
	makeCommit(t, local, "file1")
	pushDefault(t, local, remote)

	logPath := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, fr := newTestService(t, Config{
		LogPath:  logPath,
		HookWait: 400 * time.Millisecond,
		Repos:    map[string]Repo{"app": {Dir: local, Remote: remote}},
	})
	hsrv := newServer(t, s.HookHandler())
	lsrv := newServer(t, s.LocalHandler())
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fi, err := os.Stat(logPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("已存在的 0644 日志应被收紧为 0600: %v %v", fi, err)
	}
	addr := s.Status().RemoteAddr
	token := fr.publishRec(0).token

	remoteDo(t, http.MethodPost, "http://"+addr+"/task", token, []byte("hello"))
	remoteDo(t, http.MethodPost, "http://"+addr+"/sync?repo=app&branch=master", token, nil)

	// ask + decide(带 id、tool、result)
	done := make(chan struct{})
	go func() { hookPost(t, hsrv.URL+"/support/hook", s.HookToken(), hookBody); close(done) }()
	p := waitPending(t, s, 1)
	remoteDo(t, http.MethodPost, lsrv.URL+"/support/decide?id="+p[0].ID+"&allow=1", "", nil)
	<-done
	remoteDo(t, http.MethodPost, lsrv.URL+"/support/decide?id=nope&allow=1", "", nil)
	long := strings.Repeat("字", 800)
	remoteDo(t, http.MethodPost, lsrv.URL+"/support/decide?id=nope&allow=0&message="+url.QueryEscape(long), "", nil)

	// timeout
	hookPost(t, hsrv.URL+"/support/hook", s.HookToken(), hookBody)

	// cancelled：客户端断开
	cctx, ccancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(cctx, http.MethodPost, hsrv.URL+"/support/hook?token="+s.HookToken(), strings.NewReader(hookBody))
	cdone := make(chan struct{})
	go func() {
		if resp, err := httpClient.Do(req); err == nil {
			resp.Body.Close()
		}
		close(cdone)
	}()
	waitPending(t, s, 1)
	ccancel()
	<-cdone

	remoteDo(t, http.MethodGet, "http://"+addr+"/status", token, nil)
	fr.closeLast()
	waitFor(t, 5*time.Second, "disconnected 日志", func() bool {
		return hasLog(s, "remote", func(d map[string]string) bool { return d["event"] == "disconnected" })
	})

	waitFor(t, 5*time.Second, "cancelled 日志", func() bool { return hasLog(s, "cancelled", nil) })
	hasID := func(d map[string]string) bool { return len(d["id"]) == 16 }
	if !hasLog(s, "ask", hasID) {
		t.Fatal("ask 日志缺 id")
	}
	if !hasLog(s, "decide", func(d map[string]string) bool {
		return d["id"] == p[0].ID && d["result"] == "ok" && d["tool"] == "Bash" && d["summary"] == "ls -l"
	}) {
		t.Fatal("decide 日志缺 id/result/tool/summary")
	}
	if !hasLog(s, "decide", func(d map[string]string) bool {
		return d["result"] == "not_found" && len([]rune(d["message"])) == 500
	}) {
		t.Fatal("decide not_found 日志缺失或附言没截到 500 字")
	}
	for _, k := range []string{"start", "task", "sync", "timeout"} {
		if !hasLog(s, k, nil) {
			t.Fatalf("缺 %s 日志", k)
		}
	}
	if !hasLog(s, "timeout", func(d map[string]string) bool { return d["id"] != "" }) {
		t.Fatal("timeout 日志缺 id")
	}

	s.Close()
	fi1, _ := os.Stat(logPath)
	s.logEvent("task", map[string]string{"x": "after close"})
	fi2, _ := os.Stat(logPath)
	if fi1.Size() != fi2.Size() {
		t.Fatal("Close 后不该再写日志文件")
	}
}

func waitPending(t *testing.T, s *Service, n int) []Request {
	t.Helper()
	return waitForPending(t, s.queue, n)
}

// newServer 起一个绑回环的 httptest 服务器，测试结束自动关。
func newServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// 待批达到上限后，新的钩子请求直接拒绝并记 ask_rejected。
func TestHookPendingLimit(t *testing.T) {
	old := maxPending
	maxPending = 2
	t.Cleanup(func() { maxPending = old })

	s, _ := newTestService(t, Config{HookWait: 10 * time.Second})
	hsrv := newServer(t, s.HookHandler())
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	hookURL := hsrv.URL + "/support/hook"
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); hookPost(t, hookURL, s.HookToken(), hookBody) }()
	}
	defer wg.Wait()
	defer s.Stop("测试结束") // 放掉挂着的两条钩子
	waitFor(t, 5*time.Second, "两条待批出现", func() bool { return len(s.Status().Pending) == 2 })

	code, body := hookPost(t, hookURL, s.HookToken(), hookBody)
	if code != 200 {
		t.Fatalf("超限应返回 200 + 拒绝: %d %s", code, body)
	}
	if b, m := hookDecisionOf(t, body); b != "deny" || m != "待批请求太多" {
		t.Fatalf("超限应拒绝「待批请求太多」: %s %s", b, m)
	}
	if n := len(s.Status().Pending); n != 2 {
		t.Fatalf("被拒的请求不该进待批，Pending=%d", n)
	}
	found := false
	for _, e := range s.Log() {
		found = found || e.Kind == "ask_rejected"
	}
	if !found {
		t.Fatal("应记 ask_rejected 日志")
	}
}

// Claude 正在工作时拒绝同步。
func TestRemoteSyncWhileBusy(t *testing.T) {
	old := stopGrace
	stopGrace = 200 * time.Millisecond
	t.Cleanup(func() { stopGrace = old })

	local, remote := setupTestRepos(t)
	makeCommit(t, local, "file1")
	pushDefault(t, local, remote)
	s, fr := newTestService(t, Config{Repos: map[string]Repo{"app": {Dir: local, Remote: remote}}})
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	base := "http://" + s.Status().RemoteAddr
	token := fr.publishRec(0).token
	if code, b := remoteDo(t, http.MethodPost, base+"/task", token, []byte("hang")); code != 200 {
		t.Fatalf("发任务: %d %s", code, b)
	}
	waitFor(t, 5*time.Second, "Claude 忙碌", func() bool { return s.Status().Claude.Busy })
	code, body := remoteDo(t, http.MethodPost, base+"/sync?repo=app&branch=master", token, nil)
	if code != 409 || !strings.Contains(string(body), "Mac 上的 Claude 正在工作，稍后再同步") {
		t.Fatalf("忙碌时同步应 409: %d %s", code, body)
	}
}
