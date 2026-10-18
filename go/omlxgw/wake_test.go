package omlxgw

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sleepyStudio 模拟睡着的 Studio：上游地址一开始连不上，门口监听被连一下后才起上游。
type sleepyStudio struct {
	upAddr  string
	knocks  atomic.Int32
	knockLn net.Listener
	srv     *http.Server
	delay   time.Duration
}

func newSleepyStudio(t *testing.T, delay time.Duration) *sleepyStudio {
	s := &sleepyStudio{delay: delay}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	s.upAddr = l.Addr().String()
	l.Close() // 先关：连不上
	s.knockLn, _ = net.Listen("tcp", "127.0.0.1:0")
	go func() {
		for {
			c, err := s.knockLn.Accept()
			if err != nil {
				return
			}
			c.Close()
			if s.knocks.Add(1) == 1 && delay >= 0 {
				go func() {
					time.Sleep(s.delay)
					ln, err := net.Listen("tcp", s.upAddr)
					if err != nil {
						return
					}
					s.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/v1/models" {
							w.WriteHeader(401)
							return
						}
						io.WriteString(w, "hello")
					})}
					go s.srv.Serve(ln)
				}()
			}
		}
	}()
	t.Cleanup(func() {
		s.knockLn.Close()
		if s.srv != nil {
			s.srv.Close()
		}
	})
	return s
}

func (s *sleepyStudio) knockPort() int { return s.knockLn.Addr().(*net.TCPAddr).Port }

func newWakeGW(t *testing.T, s *sleepyStudio, maxWait int) (*Gateway, *httptest.Server) {
	cfg := &Config{Listen: "127.0.0.1:1", Upstream: "http://" + s.upAddr, DefaultPriority: 5,
		DefaultModel: ModelRule{MaxConcurrent: 8}, MaxTotal: 16,
		Wake: &WakeConfig{Host: "127.0.0.1", KnockPorts: []int{s.knockPort()}, MaxWaitSeconds: maxWait}}
	var logs strings.Builder
	g, err := New(cfg, "", &logs)
	if err != nil {
		t.Fatal(err)
	}
	g.waker.unit = time.Millisecond
	g.waker.poll = 20 * time.Millisecond
	g.waker.probeTimeout = 200 * time.Millisecond
	ts := httptest.NewServer(g.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(g.Close)
	return g, ts
}

func post(ts *httptest.Server) (int, string) {
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestWakeConcurrentSingleKnock(t *testing.T) {
	s := newSleepyStudio(t, 150*time.Millisecond)
	g, ts := newWakeGW(t, s, 5000)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := post(ts); code != 200 || body != "hello" {
				t.Errorf("code=%d body=%s", code, body)
			}
		}()
	}
	wg.Wait()
	if n := s.knocks.Load(); n != 1 {
		t.Fatalf("敲门次数 %d，期望 1", n)
	}
	if g.waker.status()["last_wake_seconds"] == nil {
		t.Fatal("last_wake_seconds 应有值")
	}
	// 刚成功过，不再探测/敲门
	if code, _ := post(ts); code != 200 || s.knocks.Load() != 1 {
		t.Fatal("醒后不应再敲门")
	}
}

func TestWakeTimeout503(t *testing.T) {
	s := newSleepyStudio(t, -1) // 永远叫不醒
	_, ts := newWakeGW(t, s, 200)
	code, body := post(ts)
	var m map[string]string
	json.Unmarshal([]byte(body), &m)
	if code != 503 || !strings.HasPrefix(m["error"], "Studio 没叫醒") {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if s.knocks.Load() != 1 {
		t.Fatalf("knocks=%d", s.knocks.Load())
	}
}

func TestWakeOnDialErrorRetry(t *testing.T) {
	s := newSleepyStudio(t, 50*time.Millisecond)
	g, ts := newWakeGW(t, s, 5000)
	g.waker.markOK() // 缓存显示「刚醒着」，转发时才发现连不上
	if code, body := post(ts); code != 200 || body != "hello" {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if s.knocks.Load() != 1 {
		t.Fatalf("knocks=%d", s.knocks.Load())
	}
}

func TestKeepAwake(t *testing.T) {
	up := newFakeUp(t)
	dir := t.TempDir()
	cnt := filepath.Join(dir, "cnt")
	cfg := &Config{Listen: "127.0.0.1:1", Upstream: up.srv.URL, DefaultPriority: 5,
		DefaultModel: ModelRule{MaxConcurrent: 8}, MaxTotal: 16,
		KeepAwake: &KeepAwakeConfig{Command: []string{"sh", "-c", "echo x >> " + cnt + "; sleep 30"}, RefreshSeconds: 80, IdleSeconds: 400}}
	g, err := New(cfg, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	g.keeper.unit = time.Millisecond
	g.keeper.tick = 5 * time.Millisecond
	g.keeper.start()
	ts := httptest.NewServer(g.Handler())
	defer ts.Close()
	lines := func() int {
		b, _ := os.ReadFile(cnt)
		return strings.Count(string(b), "x")
	}
	up.release("a")
	// 持续有活 ~350ms：应按 80ms 周期起多次
	end := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(end) {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Test", "a")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	n := lines()
	if n < 3 || n > 6 {
		t.Fatalf("有活期间起了 %d 次", n)
	}
	st := g.keeper.status()
	if st["running"].(int) < 1 || st["active"] != true {
		t.Fatalf("status=%v", st)
	}
	// 空闲超过 400ms 后不再起
	time.Sleep(600 * time.Millisecond)
	n1 := lines()
	time.Sleep(300 * time.Millisecond)
	if lines() != n1 || g.keeper.status()["active"] != false {
		t.Fatal("空闲后仍在起进程")
	}
	// 关闭网关：子进程被杀
	if g.keeper.status()["running"].(int) == 0 {
		t.Fatal("应还有活着的子进程")
	}
	done := make(chan struct{})
	go func() { g.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 没有杀掉子进程")
	}
	if g.keeper.status()["running"].(int) != 0 {
		t.Fatal("Close 后仍有子进程")
	}
}

func TestConfigWakeValidate(t *testing.T) {
	for _, js := range []string{
		`{"listen":"127.0.0.1:1","upstream":"http://x:1","wake":{"host":""}}`,
		`{"listen":"127.0.0.1:1","upstream":"http://x:1","wake":{"host":"h","knock_ports":[0]}}`,
		`{"listen":"127.0.0.1:1","upstream":"http://x:1","keepawake":{"command":[]}}`,
	} {
		if _, err := ParseConfig([]byte(js)); err == nil {
			t.Fatalf("应拒绝: %s", js)
		}
	}
	c, err := ParseConfig([]byte(`{"listen":"127.0.0.1:1","upstream":"http://x:1","wake":{"host":"h"},"keepawake":{"command":["a"]}}`))
	if err != nil || c.Wake.MaxWaitSeconds != 120 || c.KeepAwake.RefreshSeconds != 240 || c.KeepAwake.IdleSeconds != 600 {
		t.Fatalf("%v %+v", err, c)
	}
}

// 长请求在途期间，即使超过 idle 也要持续续期；结束后再给一个空闲窗口。
func TestKeepAwakeInflight(t *testing.T) {
	dir := t.TempDir()
	cnt := filepath.Join(dir, "cnt")
	kc := &KeepAwakeConfig{Command: []string{"sh", "-c", "echo x >> " + cnt + "; sleep 30"}, RefreshSeconds: 20, IdleSeconds: 60}
	k := newKeeper(func() *KeepAwakeConfig { return kc }, log.New(io.Discard, "", 0))
	k.unit = time.Millisecond
	k.tick = 5 * time.Millisecond
	k.start()
	defer k.close()
	lines := func() int {
		b, _ := os.ReadFile(cnt)
		return strings.Count(string(b), "x")
	}
	k.begin()
	time.Sleep(300 * time.Millisecond) // 远超 idle 60ms
	if n := lines(); n < 5 || k.status()["active"] != true {
		t.Fatalf("在途期间续期不足: %d 次 status=%v", n, k.status())
	}
	k.end()
	time.Sleep(250 * time.Millisecond)
	n1 := lines()
	time.Sleep(150 * time.Millisecond)
	if lines() != n1 || k.status()["active"] != false {
		t.Fatal("请求结束并空闲后仍在续期")
	}
}
