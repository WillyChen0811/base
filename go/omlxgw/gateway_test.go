package omlxgw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const flash = "Qwen3.8-Flash-Next-oQ4e-mtp"

type fakeUp struct {
	srv     *httptest.Server
	started chan string
	mu      sync.Mutex
	gates   map[string]chan struct{}
	auth    chan string
}

func (f *fakeUp) gate(tag string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gates[tag] == nil {
		f.gates[tag] = make(chan struct{})
	}
	return f.gates[tag]
}
func (f *fakeUp) release(tag string) { close(f.gate(tag)) }

func newFakeUp(t *testing.T) *fakeUp {
	f := &fakeUp{started: make(chan string, 100), gates: map[string]chan struct{}{}, auth: make(chan string, 100)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[]}`)
			return
		}
		tag := r.Header.Get("X-Test")
		f.auth <- r.Header.Get("Authorization")
		f.started <- tag
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: one\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-f.gate(tag):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte("data: two\n\n"))
	}))
	t.Cleanup(func() {
		f.mu.Lock()
		for _, g := range f.gates {
			select {
			case <-g:
			default:
				close(g)
			}
		}
		f.mu.Unlock()
		f.srv.Close()
	})
	return f
}

func newGW(t *testing.T, up *fakeUp, mod func(*Config)) (*Gateway, *httptest.Server, string) {
	cfg := &Config{Listen: "127.0.0.1:1", Upstream: up.srv.URL, DefaultPriority: 5,
		Models:       map[string]ModelRule{flash: {MaxConcurrent: 1, Exclusive: true}},
		DefaultModel: ModelRule{MaxConcurrent: 8}, MaxTotal: 16}
	if mod != nil {
		mod(cfg)
	}
	path := filepath.Join(t.TempDir(), "gw.json")
	b, _ := json.Marshal(cfg)
	os.WriteFile(path, b, 0o644)
	var logs bytes.Buffer
	g, err := New(cfg, path, &logs)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(g.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(g.Close)
	return g, ts, path
}

// send 在 goroutine 里发请求并读完响应，完成时关闭 done。
func send(ctx context.Context, ts *httptest.Server, model string, prio, tag string) chan string {
	done := make(chan string, 1)
	go func() {
		body := `{"model":"` + model + `"}`
		req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/messages", strings.NewReader(body))
		req.Header.Set("X-Test", tag)
		req.Header.Set("Authorization", "Bearer k-"+tag)
		if prio != "" {
			req.Header.Set("X-Xxyi-Priority", prio)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- "err"
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- string(b)
	}()
	return done
}

func expectStart(t *testing.T, f *fakeUp, tag string) {
	t.Helper()
	select {
	case got := <-f.started:
		if got != tag {
			t.Fatalf("先开始的是 %s，期望 %s", got, tag)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s 没有开始", tag)
	}
}

func expectNoStart(t *testing.T, f *fakeUp) {
	t.Helper()
	select {
	case got := <-f.started:
		t.Fatalf("不该开始: %s", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func waitQueued(t *testing.T, g *Gateway, n int) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if _, q := g.sched.status(); len(q) == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("排队数没到 %d", n)
}

func TestStreamingPassthrough(t *testing.T) {
	up := newFakeUp(t)
	_, ts, _ := newGW(t, up, nil)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("X-Test", "s")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf) // 第二块还没发，第一块必须先到
	if string(buf[:n]) != "data: one\n\n" {
		t.Fatalf("首块: %q", buf[:n])
	}
	if a := <-up.auth; a != "Bearer secret" {
		t.Fatalf("Authorization 没透传: %q", a)
	}
	up.release("s")
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "data: two\n\n" {
		t.Fatalf("后续: %q", rest)
	}
}

func TestNoModelNotQueued(t *testing.T) {
	up := newFakeUp(t)
	g, ts, _ := newGW(t, up, nil)
	d := send(context.Background(), ts, flash, "", "f")
	expectStart(t, up, "f")
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("models: %v %v", resp, err)
	}
	resp.Body.Close()
	if _, q := g.sched.status(); len(q) != 0 {
		t.Fatal("不该排队")
	}
	up.release("f")
	<-d
}

func TestExclusive(t *testing.T) {
	up := newFakeUp(t)
	g, ts, _ := newGW(t, up, nil)
	a := send(context.Background(), ts, "A", "", "a")
	expectStart(t, up, "a")
	f := send(context.Background(), ts, flash, "", "f")
	waitQueued(t, g, 1)
	expectNoStart(t, up)
	up.release("a")
	<-a
	expectStart(t, up, "f")
	b := send(context.Background(), ts, "B", "", "b")
	waitQueued(t, g, 1)
	expectNoStart(t, up)
	up.release("f")
	<-f
	expectStart(t, up, "b")
	up.release("b")
	<-b
}

func TestConcurrencyLimit(t *testing.T) {
	up := newFakeUp(t)
	g, ts, _ := newGW(t, up, func(c *Config) { c.DefaultModel.MaxConcurrent = 2 })
	var ds []chan string
	for _, tag := range []string{"1", "2"} {
		ds = append(ds, send(context.Background(), ts, "A", "", tag))
		expectStart(t, up, tag)
	}
	ds = append(ds, send(context.Background(), ts, "A", "", "3"))
	waitQueued(t, g, 1)
	expectNoStart(t, up)
	// 另一个模型有自己的名额
	d := send(context.Background(), ts, "B", "", "b")
	expectStart(t, up, "b")
	up.release("b")
	<-d
	up.release("1")
	expectStart(t, up, "3")
	up.release("2")
	up.release("3")
	for _, d := range ds {
		<-d
	}
}

func TestPriorityOrder(t *testing.T) {
	up := newFakeUp(t)
	g, ts, _ := newGW(t, up, func(c *Config) { c.DefaultModel.MaxConcurrent = 1 })
	run := send(context.Background(), ts, "A", "", "run")
	expectStart(t, up, "run")
	d5 := send(context.Background(), ts, "A", "5", "p5")
	waitQueued(t, g, 1)
	d9 := send(context.Background(), ts, "A", "9", "p9")
	waitQueued(t, g, 2)
	d5b := send(context.Background(), ts, "A", "5", "p5b")
	waitQueued(t, g, 3)
	up.release("run")
	<-run
	expectStart(t, up, "p9")
	up.release("p9")
	<-d9
	expectStart(t, up, "p5") // 同级先到先得
	up.release("p5")
	<-d5
	expectStart(t, up, "p5b")
	up.release("p5b")
	<-d5b
}

func TestExclusiveNotStarved(t *testing.T) {
	up := newFakeUp(t)
	g, ts, _ := newGW(t, up, nil)
	a := send(context.Background(), ts, "A", "", "a")
	expectStart(t, up, "a")
	f := send(context.Background(), ts, flash, "", "f")
	waitQueued(t, g, 1)
	// 同优先级的新请求虽有名额也不放进去
	b := send(context.Background(), ts, "B", "", "b")
	waitQueued(t, g, 2)
	expectNoStart(t, up)
	// 优先级更高的仍可先走
	c := send(context.Background(), ts, "C", "9", "c")
	expectStart(t, up, "c")
	up.release("c")
	<-c
	up.release("a")
	<-a
	expectStart(t, up, "f")
	up.release("f")
	<-f
	expectStart(t, up, "b")
	up.release("b")
	<-b
}

func TestQueuedClientDisconnect(t *testing.T) {
	up := newFakeUp(t)
	g, ts, _ := newGW(t, up, nil)
	f := send(context.Background(), ts, flash, "", "f")
	expectStart(t, up, "f")
	ctx, cancel := context.WithCancel(context.Background())
	d := send(ctx, ts, "A", "", "a")
	waitQueued(t, g, 1)
	cancel()
	<-d
	waitQueued(t, g, 0)
	up.release("f")
	<-f
	expectNoStart(t, up) // 断开的请求不会被转发
}

func put(t *testing.T, ts *httptest.Server, body string) (int, string) {
	req, _ := http.NewRequest("PUT", ts.URL+"/gw/config", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPutConfig(t *testing.T) {
	up := newFakeUp(t)
	g, ts, path := newGW(t, up, nil)
	cfg := *g.sched.config()
	cfg.DefaultModel.MaxConcurrent = 1
	cfg.DefaultPriority = 3
	b, _ := json.Marshal(cfg)
	if code, body := put(t, ts, string(b)); code != 200 {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if c := g.sched.config(); c.DefaultModel.MaxConcurrent != 1 || c.DefaultPriority != 3 {
		t.Fatal("没有生效")
	}
	saved, err := LoadConfig(path)
	if err != nil || saved.DefaultModel.MaxConcurrent != 1 {
		t.Fatalf("没写回文件: %v %+v", err, saved)
	}
	// 新限制立即生效
	a := send(context.Background(), ts, "A", "", "a")
	expectStart(t, up, "a")
	a2 := send(context.Background(), ts, "A", "", "a2")
	waitQueued(t, g, 1)
	expectNoStart(t, up)
	// 放宽后排队中的立即放行
	cfg.DefaultModel.MaxConcurrent = 4
	b, _ = json.Marshal(cfg)
	put(t, ts, string(b))
	expectStart(t, up, "a2")
	up.release("a")
	up.release("a2")
	<-a
	<-a2
	resp, _ := http.Get(ts.URL + "/gw/config")
	var got Config
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.DefaultModel.MaxConcurrent != 4 {
		t.Fatalf("GET config: %+v", got)
	}
}

func TestPutConfigInvalid(t *testing.T) {
	up := newFakeUp(t)
	g, ts, path := newGW(t, up, nil)
	before, _ := os.ReadFile(path)
	cases := map[string]string{
		"改listen":   `{"listen":"127.0.0.1:2","upstream":"` + up.srv.URL + `","default_priority":5}`,
		"改upstream": `{"listen":"127.0.0.1:1","upstream":"http://10.0.0.1:1","default_priority":5}`,
		"优先级越界":     `{"listen":"127.0.0.1:1","upstream":"` + up.srv.URL + `","default_priority":10}`,
		"并发为0":      `{"listen":"127.0.0.1:1","upstream":"` + up.srv.URL + `","default_priority":5,"models":{"x":{"max_concurrent":0}}}`,
		"不是JSON":    `nope`,
		"未知字段":      `{"listen":"127.0.0.1:1","upstream":"` + up.srv.URL + `","foo":1}`,
	}
	for name, body := range cases {
		code, resp := put(t, ts, body)
		var e struct{ Error string }
		if code != 400 || json.Unmarshal([]byte(resp), &e) != nil || e.Error == "" {
			t.Errorf("%s: %d %s", name, code, resp)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || g.sched.config().DefaultPriority != 5 {
		t.Fatal("非法配置不应改动")
	}
}

func TestNonV1PathForbidden(t *testing.T) {
	up := newFakeUp(t)
	_, ts, _ := newGW(t, up, nil)
	for _, p := range []string{"/admin/api/stats", "/admin/", "/", "/v1", "/health", "/v1/../admin/api/stats"} {
		req, _ := http.NewRequest("GET", ts.URL+p, nil)
		req.Header.Set("Authorization", "Bearer fake-agent-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 || !strings.Contains(string(b), "网关不转发此路径") {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, b)
		}
	}
	select {
	case a := <-up.auth:
		t.Fatalf("请求不该到上游: %s", a)
	default:
	}
}
