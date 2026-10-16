package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakeMacReq 是假 Mac 远端收到的一条请求。
type fakeMacReq struct {
	method string
	path   string
	query  string
	body   []byte
}

// fakeMac 是 /exec/* 的假远端：收到的请求排队记下，响应由每个测试自己挂 handler。
type fakeMac struct {
	mu   sync.Mutex
	reqs []fakeMacReq
}

func (f *fakeMac) add(method, path, query string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, fakeMacReq{method: method, path: path, query: query, body: body})
}

func (f *fakeMac) reqList() []fakeMacReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeMacReq(nil), f.reqs...)
}

func (f *fakeMac) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// macCallTool 走 MCP 全链路（tools/call），取回 content[0] 与 isError。
func macCallTool(t *testing.T, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	resps := mcpRound(t, mcpCall(1, "tools/call", map[string]any{"name": name, "arguments": args}))
	if len(resps) != 1 {
		t.Fatalf("%v 回应条数 %d", name, len(resps))
	}
	if e, ok := resps[0]["error"]; ok {
		t.Fatalf("%v 不该用 JSON-RPC error，得到 %v", name, e)
	}
	result, ok := resps[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("%v 回应里没有 result：%v", name, resps[0])
	}
	isErr, _ := result["isError"].(bool)
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("%v result.content 不对：%v", name, result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("%v content[0] 不是对象：%v", name, content[0])
	}
	return first, isErr
}

// macTextResult 取文字型工具结果的 text（不是文字就直接失败）。
func macTextResult(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	c, isErr := macCallTool(t, name, args)
	if c["type"] != "text" {
		t.Fatalf("%v 该返回文字：%v", name, c)
	}
	text, _ := c["text"].(string)
	return text, isErr
}

// macPNG 生成一张能整图解码的小 PNG（截图响应用）。
func macPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.NRGBA{R: byte(x * 60), G: byte(y * 60), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// macRunHandler 回应 POST /exec/run：runStatus 与正文由各测试设定。
func macRunHandler(f *fakeMac, runStatus int, runBody func(r *http.Request) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		f.add(r.Method, r.URL.Path, r.URL.RawQuery, body)
		w.WriteHeader(runStatus)
		_ = json.NewEncoder(w).Encode(runBody(r))
	}
}

// mac_build 同步返回文字：含退出码、用时与输出尾部；构建失败（非零退出码）也是数据不是报错。
func TestMacToolBuildText(t *testing.T) {
	var f fakeMac
	srv := httptest.NewServer(macRunHandler(&f, http.StatusOK, func(r *http.Request) any {
		return map[string]any{"exit_code": 1, "timed_out": false, "seconds": 12.3,
			"tail": "error: cannot find 'foo'\nBuild failed"}
	}))
	defer srv.Close()
	useEndpoint(t, srv.URL)

	text, isErr := macTextResult(t, "mac_build", map[string]any{})
	if isErr {
		t.Fatalf("构建失败也该是文字结果：%q", text)
	}
	for _, want := range []string{"退出码 1", "用时 12.3s", "Build failed", "cannot find"} {
		if !strings.Contains(text, want) {
			t.Errorf("结果文字缺 %q：%q", want, text)
		}
	}
	reqs := f.reqList()
	if len(reqs) != 1 {
		t.Fatalf("假远端收到 %d 个请求：%+v", len(reqs), reqs)
	}
	got := reqs[0]
	if got.method != http.MethodPost || got.path != "/exec/run" {
		t.Errorf("%s %s", got.method, got.path)
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("正文不是 JSON：%q：%v", got.body, err)
	}
	if body["action"] != "swift_build" {
		t.Errorf("action %v", body["action"])
	}
	// 超时没跑完是 timed_out 文字，不是 409/报错。
}

// timed_out 要在文字里点出来（超时被终止），退出码与尾部照常返回。
func TestMacToolBuildTimedOutText(t *testing.T) {
	var f fakeMac
	srv := httptest.NewServer(macRunHandler(&f, http.StatusOK, func(r *http.Request) any {
		return map[string]any{"exit_code": -1, "timed_out": true, "seconds": 900,
			"tail": "Compiling 42 modules"}
	}))
	defer srv.Close()
	useEndpoint(t, srv.URL)

	text, isErr := macTextResult(t, "mac_selftest", map[string]any{})
	if isErr {
		t.Fatalf("超时是结果不是工具报错：%q", text)
	}
	for _, want := range []string{"退出码 -1", "超时", "900.0s", "Compiling"} {
		if !strings.Contains(text, want) {
			t.Errorf("结果文字缺 %q：%q", want, text)
		}
	}
}

// mac_screenshot 走图片通道：content[0] 是 base64 的 image/png，解出来与远端给的字节一致。
func TestMacToolScreenshotImage(t *testing.T) {
	var f fakeMac
	pngBytes := macPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		f.add(r.Method, r.URL.Path, r.URL.RawQuery, body)
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()
	useEndpoint(t, srv.URL)

	c, isErr := macCallTool(t, "mac_screenshot", map[string]any{})
	if isErr {
		t.Fatalf("截图失败：%v", c)
	}
	if c["type"] != "image" || c["mimeType"] != "image/png" {
		t.Fatalf("content[0] 不是 image/png：%v", c)
	}
	data, err := base64.StdEncoding.DecodeString(fmt.Sprint(c["data"]))
	if err != nil {
		t.Fatalf("data 不是合法 base64：%v", err)
	}
	if !bytes.Equal(data, pngBytes) {
		t.Errorf("截图字节不一致：%d vs %d", len(data), len(pngBytes))
	}
	reqs := f.reqList()
	if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].path != "/exec/screenshot" {
		t.Errorf("请求 %+v", reqs)
	}

	// 半截 PNG（正文完整但内容断了）必须当场报错，不能当成功交出去：
	// 报错走 isError + 文字通道，不再是 image 内容。
	broken := pngBytes[:len(pngBytes)/2]
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(broken)
	}))
	defer srv2.Close()
	useEndpoint(t, srv2.URL)
	c2, isErr := macCallTool(t, "mac_screenshot", map[string]any{})
	if !isErr {
		t.Errorf("半截 PNG 该报错：%v", c2)
	}
	if c2["type"] != "text" || !strings.Contains(fmt.Sprint(c2["text"]), "PNG") {
		t.Errorf("半截 PNG 的报错文字 %v", c2)
	}
}

// 忙（409）：服务端原因原样带出并标 isError，不重试（远端只收到一次请求）。
func TestMacToolBusyError(t *testing.T) {
	var f fakeMac
	srv := httptest.NewServer(macRunHandler(&f, http.StatusConflict, func(r *http.Request) any {
		return map[string]any{"error": "Mac 上正有任务在跑"}
	}))
	defer srv.Close()
	useEndpoint(t, srv.URL)

	text, isErr := macTextResult(t, "mac_build", map[string]any{})
	if !isErr {
		t.Fatalf("409 该 isError：%q", text)
	}
	if !strings.Contains(text, "Mac 上正有任务在跑") || !strings.Contains(text, "409") {
		t.Errorf("忙时文字 %q，期望含原因与状态码", text)
	}
	if n := f.count(); n != 1 {
		t.Errorf("忙不重试，只该收到 1 个请求，收到 %d", n)
	}
}

// 令牌不对（401）：重读一次入口再试；换新令牌后第二次成功；仍不对则第二次仍报 401。
func TestMacToolTokenStaleReread(t *testing.T) {
	var f1, f2 fakeMac
	srv1 := httptest.NewServer(macRunHandler(&f1, http.StatusUnauthorized, func(r *http.Request) any {
		return map[string]any{"error": "令牌不对"}
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(macRunHandler(&f2, http.StatusOK, func(r *http.Request) any {
		return map[string]any{"exit_code": 0, "timed_out": false, "seconds": 0.4, "tail": "ok"}
	}))
	defer srv2.Close()
	useEndpoint(t, srv1.URL)

	// endpoint.json 一直写着旧地址：第一次 401，重读之前把入口换成新地址——
	// 第二次调用必须读到新入口并成功（模拟 wgtund 重启后 endpoint.json 换新）。
	ep := endpointPath
	// 第一次：旧入口 401；工具重试前重读 endpoint.json，此刻仍是旧地址 ⇒ 仍 401。
	text, isErr := macTextResult(t, "mac_git_status", map[string]any{"repo": "base"})
	if !isErr || !strings.Contains(text, "令牌不对") || !strings.Contains(text, "401") {
		t.Fatalf("旧令牌该报 401：%q isError=%v", text, isErr)
	}
	if n := f1.count(); n != 2 {
		t.Errorf("401 该重读入口重试一次（共 2 次），收到 %d", n)
	}

	// 入口文件换成新地址：每次调用现读，这次应当直接成功且旧远端不再收到请求。
	if err := os.WriteFile(ep,
		[]byte(fmt.Sprintf(`{"addr":%q,"token":"tk2"}`, authority(srv2.URL))), 0o600); err != nil {
		t.Fatal(err)
	}
	text, isErr = macTextResult(t, "mac_git_status", map[string]any{"repo": "base"})
	if isErr {
		t.Fatalf("换入口后该成功：%q", text)
	}
	if !strings.Contains(text, "退出码 0") || !strings.Contains(text, "ok") {
		t.Errorf("结果 %q", text)
	}
	if n := f1.count(); n != 2 {
		t.Errorf("换新入口后旧远端不该再收到请求，现有 %d", n)
	}
	if n := f2.count(); n != 1 {
		t.Errorf("新远端该收到 1 个请求，收到 %d", n)
	}
}

// mac_logs：参数校验在本地先拒（name 名单、lines 1..500），合法才发 GET 并带上查询串。
func TestMacToolLogs(t *testing.T) {
	var f fakeMac
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.add(r.Method, r.URL.Path, r.URL.RawQuery, nil)
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "line1\nline2"})
	}))
	defer srv.Close()
	useEndpoint(t, srv.URL)

	text, isErr := macTextResult(t, "mac_logs", map[string]any{"name": "wgtund", "lines": 50})
	if isErr || text != "line1\nline2" {
		t.Fatalf("logs %q isError=%v", text, isErr)
	}
	if q := f.reqList()[0].query; !strings.Contains(q, "name=wgtund") || !strings.Contains(q, "lines=50") {
		t.Errorf("查询串 %q", q)
	}

	for _, args := range []map[string]any{
		{"name": "ui"}, // ui 已从名单去掉
		{"name": "service", "lines": 0},
		{"name": "service", "lines": 501},
	} {
		text, isErr = macTextResult(t, "mac_logs", args)
		if !isErr {
			t.Errorf("%v 该本地拒绝：%q", args, text)
		}
	}
	if n := f.count(); n != 1 {
		t.Errorf("本地拒绝不该发请求，远端收到 %d", n)
	}
}

// mac_install_app：只确认起手，立即返回文字；不轮询（远端只收到一次 POST /exec/install）。
func TestMacToolInstallStarted(t *testing.T) {
	var f fakeMac
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		f.add(r.Method, r.URL.Path, r.URL.RawQuery, body)
		_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
	}))
	defer srv.Close()
	useEndpoint(t, srv.URL)

	text, isErr := macTextResult(t, "mac_install_app", map[string]any{})
	if isErr {
		t.Fatalf("install 失败：%q", text)
	}
	if !strings.Contains(text, "已") || !strings.Contains(text, "mac_screenshot") {
		t.Errorf("install 文字 %q，期望含提示", text)
	}
	reqs := f.reqList()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].path != "/exec/install" {
		t.Errorf("请求 %+v", reqs)
	}
}

// 假远端不可达（服务已关）：报"连不上"，不无限重试。
func TestMacToolConnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	useEndpoint(t, url)

	text, isErr := macTextResult(t, "mac_screenshot", map[string]any{})
	if !isErr || !strings.Contains(text, "连不上") {
		t.Errorf("关掉的远端该报连不上：%q isError=%v", text, isErr)
	}
}
