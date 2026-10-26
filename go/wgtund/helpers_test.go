package wgtund

// 多个测试文件共用的小辅助。原先放在 support_test.go 里；2026-10-26 拆协助时那个文件删了，这几个还有人用。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// testOrigin 是测试里冒充 xxyi 页面的 Origin（夹具的 allowed_origins 里有它）。
const testOrigin = "http://127.0.0.1:8765"

// gitInit 在 dir 建一个空 git 仓库。
func gitInit(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

// runErr 直接调用 Run，返回它的错误（配置不合规时应很快返回）。
func runErr(t *testing.T, dir string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Run(ctx, dir, pr, &out) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run 没有因配置错误退出")
		return nil
	}
}

// postOrigin 带主令牌 POST；origin 非空时带上 Origin 头。
func postOrigin(t *testing.T, st started, path, origin string) (*http.Response, []byte) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := http.NewRequest("POST", "http://"+st.HTTP+path+sep+"token="+st.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}
