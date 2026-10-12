package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "tok123"

// fakeMac 模拟远端接口。
type fakeMac struct {
	mu      sync.Mutex
	batches [][]string // 每批事件的 data JSON；按 /events 请求依次释放
	next    int
	seq     int64
	status  string
	task    string
	statusN int
	syncErr bool
	afters  []int64 // 非探测的 /events 请求的 after
	failN   int     // 前 N 次轮询（非探测）的 /events 返回 500
}

func (f *fakeMac) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.statusN++
		s := f.status
		if s == "" {
			s = `{"active":true,"until":0,"claude":{},"pending":[],"repos":[]}`
		}
		// 第二次起 status 里带一条待批请求（始终同一个 id）
		if strings.Contains(s, "%PENDING%") {
			p := `[]`
			if f.statusN >= 2 {
				p = `[{"id":"p1","tool":"Bash","summary":"rm -rf x"}]`
			}
			s = strings.Replace(s, "%PENDING%", p, 1)
		}
		fmt.Fprint(w, s)
	})
	mux.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
		b := new(bytes.Buffer)
		b.ReadFrom(r.Body)
		f.mu.Lock()
		f.task = b.String()
		f.mu.Unlock()
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if after == 9223372036854775807 {
			fmt.Fprintf(w, `{"events":[],"last":%d}`, f.seq)
			return
		}
		f.afters = append(f.afters, after)
		if f.failN > 0 {
			f.failN--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var evs []string
		if f.next < len(f.batches) {
			for _, d := range f.batches[f.next] {
				f.seq++
				evs = append(evs, fmt.Sprintf(`{"seq":%d,"time":1,"data":%s}`, f.seq, d))
			}
			f.next++
		}
		fmt.Fprintf(w, `{"events":[%s],"last":%d}`, strings.Join(evs, ","), f.seq)
	})
	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if f.syncErr {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"工作区有未提交改动"}`)
			return
		}
		fmt.Fprintf(w, `{"head":"abc123-%s-%s"}`, r.URL.Query().Get("repo"), r.URL.Query().Get("branch"))
	})
	mux.HandleFunc("/stop-claude", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"令牌不对"}`)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// setup 起服务并写端点文件，返回端点文件路径。
func setup(t *testing.T, f *fakeMac, token string) string {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	oldWait, oldRetry := eventsWait, retryDelay
	eventsWait, retryDelay = 0, time.Millisecond
	t.Cleanup(func() { eventsWait, retryDelay = oldWait, oldRetry })
	p := filepath.Join(t.TempDir(), "endpoint.json")
	b, _ := json.Marshal(endpoint{Addr: strings.TrimPrefix(srv.URL, "http://"), Token: token})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runCmd(args []string, stdin string) (int, string, string) {
	var o, e bytes.Buffer
	code := run(args, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func TestStatus(t *testing.T) {
	until := time.Now().Add(30*time.Minute + 30*time.Second).Unix()
	f := &fakeMac{status: fmt.Sprintf(`{"active":true,"until":%d,"claude":{"running":true,"busy":true,"session_id":"s9","turns":3},
		"pending":[{"id":"1","tool":"Edit","summary":"a.go"}],"repos":["base","ai-browser"]}`, until)}
	ep := setup(t, f, testToken)
	code, out, errs := runCmd([]string{"-endpoint", ep, "status"}, "")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errs)
	}
	for _, w := range []string{"剩余 31 分钟", "忙碌", "s9", "Edit a.go", "base、ai-browser"} {
		if !strings.Contains(out, w) {
			t.Errorf("缺少 %q：\n%s", w, out)
		}
	}
}

func TestBadToken(t *testing.T) {
	ep := setup(t, &fakeMac{}, "wrong")
	code, _, errs := runCmd([]string{"-endpoint", ep, "status"}, "")
	if code != 2 || !strings.Contains(errs, "令牌不对") {
		t.Fatalf("code=%d err=%s", code, errs)
	}
}

func TestSync(t *testing.T) {
	f := &fakeMac{}
	ep := setup(t, f, testToken)
	code, out, _ := runCmd([]string{"-endpoint", ep, "sync", "base", "feat/x"}, "")
	if code != 0 || strings.TrimSpace(out) != "abc123-base-feat/x" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	f.syncErr = true
	code, _, errs := runCmd([]string{"-endpoint", ep, "sync", "base", "x"}, "")
	if code != 2 || !strings.Contains(errs, "工作区有未提交改动") {
		t.Fatalf("code=%d err=%s", code, errs)
	}
	if code, _, _ := runCmd([]string{"-endpoint", ep, "sync", "base"}, ""); code != 2 {
		t.Fatalf("参数不足应退出 2，得 %d", code)
	}
}

func TestMissingEndpoint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "none.json")
	code, _, errs := runCmd([]string{"-endpoint", p, "status"}, "")
	if code != 2 || !strings.Contains(errs, "Mac 没有开协助（找不到 "+p+"）") {
		t.Fatalf("code=%d err=%s", code, errs)
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	p := filepath.Join(t.TempDir(), "e.json")
	b, _ := json.Marshal(endpoint{Addr: addr, Token: "x"})
	os.WriteFile(p, b, 0o600)
	if code, _, _ := runCmd([]string{"-endpoint", p, "status"}, ""); code != 2 {
		t.Fatalf("code=%d", code)
	}
}

func TestAskFlow(t *testing.T) {
	f := &fakeMac{
		seq:    5,
		status: `{"active":true,"until":0,"claude":{},"pending":%PENDING%,"repos":[]}`,
		batches: [][]string{
			{`{"type":"system","subtype":"init","session_id":"S","model":"opus"}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"开始"}]}}`},
			{}, // 空批：此时 status 仍带待批请求，应只打印一次
			{`{"type":"result","subtype":"success","duration_ms":2500,"total_cost_usd":0.12345,"result":"完成"}`},
		},
	}
	ep := setup(t, f, testToken)
	code, out, errs := runCmd([]string{"-endpoint", ep, "ask", "-"}, "做点事\n")
	if code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out, errs)
	}
	if f.task != "做点事\n" {
		t.Errorf("task=%q", f.task)
	}
	if n := strings.Count(out, "⏸ 等 Mac 上的用户授权：Bash rm -rf x"); n != 1 {
		t.Errorf("待批提示出现 %d 次：\n%s", n, out)
	}
	for _, w := range []string{"== 会话 S，模型 opus", "开始", "== 本轮结束：success，2.5 秒，$0.1235", "完成"} {
		if !strings.Contains(out, w) {
			t.Errorf("缺少 %q：\n%s", w, out)
		}
	}
	// 游标：首轮 after=5，之后每轮等于上一轮的 last（init+开始 -> 7，空批 -> 7）
	want := []int64{5, 7, 7}
	if fmt.Sprint(f.afters) != fmt.Sprint(want) {
		t.Errorf("after 序列 %v，期望 %v", f.afters, want)
	}
}

func TestAskRetry(t *testing.T) {
	ev := `{"type":"result","subtype":"success","duration_ms":1,"total_cost_usd":0}`
	f := &fakeMac{failN: 2, batches: [][]string{{ev}}}
	ep := setup(t, f, testToken)
	if code, out, e := runCmd([]string{"-endpoint", ep, "ask", "x"}, ""); code != 0 {
		t.Fatalf("两次失败后应成功：code=%d out=%s err=%s", code, out, e)
	}
	f = &fakeMac{failN: 3, batches: [][]string{{ev}}}
	ep = setup(t, f, testToken)
	if code, _, _ := runCmd([]string{"-endpoint", ep, "ask", "x"}, ""); code != 2 {
		t.Fatalf("连续 3 次失败应退出 2，得 %d", code)
	}
}

func TestAskBusyHint(t *testing.T) {
	ev := `{"type":"result","subtype":"success","duration_ms":1,"total_cost_usd":0}`
	f := &fakeMac{status: `{"active":true,"claude":{"busy":true},"pending":[],"repos":[]}`, batches: [][]string{{ev}}}
	ep := setup(t, f, testToken)
	_, out, _ := runCmd([]string{"-endpoint", ep, "ask", "x"}, "")
	if !strings.Contains(out, "Mac 上的 Claude 还在处理上一轮，本轮输出可能混在一起") {
		t.Errorf("缺少忙碌提示：%s", out)
	}
}

func TestUnknownCmdBeforeEndpoint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "none.json")
	code, _, errs := runCmd([]string{"-endpoint", p, "typo"}, "")
	if code != 2 || !strings.Contains(errs, "未知命令") {
		t.Fatalf("code=%d err=%s", code, errs)
	}
}

func TestUnreadableEndpoint(t *testing.T) {
	code, _, errs := runCmd([]string{"-endpoint", t.TempDir(), "status"}, "")
	if code != 2 || !strings.Contains(errs, "端点文件读不了") {
		t.Fatalf("code=%d err=%s", code, errs)
	}
}

func TestAskErrorExits(t *testing.T) {
	cases := map[string]string{
		"is_error": `{"type":"result","subtype":"error","is_error":true,"duration_ms":1,"total_cost_usd":0}`,
		"exited":   `{"type":"exited","code":1,"stderr":"a\nboom"}`,
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			ep := setup(t, &fakeMac{batches: [][]string{{ev}}}, testToken)
			code, out, _ := runCmd([]string{"-endpoint", ep, "ask", "x"}, "")
			if code != 1 {
				t.Fatalf("code=%d out=%s", code, out)
			}
			if name == "exited" && !strings.Contains(out, "code=1 boom") {
				t.Errorf("out=%s", out)
			}
		})
	}
}

func TestAskTimeout(t *testing.T) {
	ep := setup(t, &fakeMac{}, testToken)
	code, _, errs := runCmd([]string{"-endpoint", ep, "-timeout", "50ms", "ask", "x"}, "")
	if code != 2 || !strings.Contains(errs, "等待超过") {
		t.Fatalf("code=%d err=%s", code, errs)
	}
}

func TestEventsCmd(t *testing.T) {
	f := &fakeMac{batches: [][]string{{`{"type":"raw","text":"hi"}`, `{"type":"weird"}`}}}
	ep := setup(t, f, testToken)
	code, out, _ := runCmd([]string{"-endpoint", ep, "events", "-after", "0"}, "")
	if code != 0 || out != "hi\nlast=2\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestStopClaude(t *testing.T) {
	ep := setup(t, &fakeMac{}, testToken)
	if code, _, e := runCmd([]string{"-endpoint", ep, "stop-claude"}, ""); code != 0 {
		t.Fatalf("code=%d %s", code, e)
	}
}

func TestFormatEvent(t *testing.T) {
	long := strings.Repeat("长", 300)
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"init", `{"type":"system","subtype":"init","session_id":"s","model":"m"}`, []string{"== 会话 s，模型 m"}},
		{"system其他", `{"type":"system","subtype":"x"}`, []string{}},
		{"文本", `{"type":"assistant","message":{"content":[{"type":"text","text":"你好"}]}}`, []string{"你好"}},
		{"Bash", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls -l"}}]}}`, []string{"→ Bash ls -l"}},
		{"文件", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/a/b"}}]}}`, []string{"→ Read /a/b"}},
		{"其他工具", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"x"}}]}}`, []string{`→ Grep {"pattern":"x"}`}},
		{"结果字符串", `{"type":"user","message":{"content":[{"type":"tool_result","content":"一二三"}]}}`, []string{"  ✓ (3 字)"}},
		{"结果数组", `{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"ab"},{"type":"text","text":"c"}]}]}}`, []string{"  ✓ (3 字)"}},
		{"错误首行", `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"第一行\n第二行"}]}}`, []string{"  ✗ 第一行"}},
		{"错误截断", `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"` + long + `"}]}}`, []string{"  ✗ " + strings.Repeat("长", 200)}},
		{"result", `{"type":"result","subtype":"success","duration_ms":1500,"total_cost_usd":0.5,"result":"ok"}`, []string{"== 本轮结束：success，1.5 秒，$0.5000", "ok"}},
		{"result无文字", `{"type":"result","subtype":"s","duration_ms":0,"total_cost_usd":0}`, []string{"== 本轮结束：s，0 秒，$0.0000"}},
		{"exited", `{"type":"exited","code":2,"stderr":"x\ny\n"}`, []string{"== Mac 上的 Claude 已退出：code=2 y"}},
		{"raw", `{"type":"raw","text":"原文"}`, []string{"原文"}},
		{"raw无text", `{"type":"raw"}`, []string{}},
		{"结果含图片块", `{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"ab"},{"type":"image"},{"type":"text","text":"c"}]}]}}`, []string{"  ✓ (3 字)"}},
		{"工具不转义", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"WebFetch","input":{"url":"http://x/a?p=1&q=2"}}]}}`, []string{`→ WebFetch {"url":"http://x/a?p=1&q=2"}`}},
		{"未知", `{"type":"stream_event"}`, []string{}},
		{"畸形JSON", `{{{`, []string{}},
		{"非对象", `[1,2]`, []string{}},
		{"null", `null`, []string{}},
		{"content类型错", `{"type":"assistant","message":{"content":5}}`, []string{}},
		{"message缺失", `{"type":"user"}`, []string{}},
		{"块类型错", `{"type":"assistant","message":{"content":[1,"a",{"type":"tool_use"}]}}`, nil},
		{"result字段类型错", `{"type":"result","duration_ms":"x","total_cost_usd":[]}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatEvent(json.RawMessage(c.in))
			if got == nil {
				t.Fatal("不应返回 nil")
			}
			if c.want == nil {
				return // 只要求不 panic
			}
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}
