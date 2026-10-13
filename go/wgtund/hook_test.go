package wgtund

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func hookRun(args []string, stdin string, env map[string]string) (int, string) {
	var out bytes.Buffer
	code := RunHook(args, strings.NewReader(stdin), &out, func(k string) string { return env[k] })
	return code, out.String()
}

func TestRunHook(t *testing.T) {
	const allow = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
	var gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT = string(b), r.Header.Get("Content-Type")
		switch r.URL.Path {
		case "/allow":
			io.WriteString(w, allow)
		case "/deny":
			io.WriteString(w, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"不行"}}}`)
		case "/bad":
			io.WriteString(w, "not json")
		default:
			http.Error(w, "boom", 500)
		}
	}))
	defer srv.Close()

	if code, out := hookRun([]string{srv.URL + "/allow"}, `{"x":1}`, nil); code != 0 || out != allow {
		t.Errorf("allow: %d %q", code, out)
	} else if gotBody != `{"x":1}` || gotCT != "application/json" {
		t.Errorf("转发的正文/类型不对: %q %q", gotBody, gotCT)
	}
	if _, out := hookRun([]string{srv.URL + "/deny"}, "{}", nil); !strings.Contains(out, `"deny"`) || !strings.Contains(out, "不行") {
		t.Errorf("deny 应原样转出: %s", out)
	}
	for name, args := range map[string][]string{
		"500":    {srv.URL + "/err"},
		"非JSON":  {srv.URL + "/bad"},
		"连不上":    {"http://127.0.0.1:1/x"},
		"没地址":    nil,
		"不是本机":   {"http://example.com:80/x"},
		"伪装本机域名": {"http://127.0.0.1.evil.com/x"},
		"用户名伪装":  {"http://127.0.0.1:1@evil.com/x"},
		"https":  {"https://127.0.0.1:1/"},
	} {
		code, out := hookRun(args, "{}", nil)
		if code != 0 || !strings.Contains(out, `"deny"`) || !strings.Contains(out, "协助服务不可用") {
			t.Errorf("%s: 应输出拒绝且退出码 0: %d %s", name, code, out)
		}
	}
	// 环境变量兜底；参数优先
	if _, out := hookRun(nil, "{}", map[string]string{"XXYI_HOOK_URL": srv.URL + "/allow"}); out != allow {
		t.Errorf("环境变量应生效: %s", out)
	}
	if _, out := hookRun([]string{srv.URL + "/allow"}, "{}", map[string]string{"XXYI_HOOK_URL": srv.URL + "/deny"}); out != allow {
		t.Errorf("参数应优先于环境变量: %s", out)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRunHookWriteFailure(t *testing.T) {
	code := RunHook(nil, strings.NewReader("{}"), failWriter{}, func(string) string { return "" })
	if code != 1 {
		t.Errorf("标准输出写失败应返回 1，得到 %d", code)
	}
}
