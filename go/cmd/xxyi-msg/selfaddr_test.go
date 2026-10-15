package main

// 进程树自寻地址与钩子日志的测试：parentPID/listPanes 一律注入替换，绝不真执行 tmux。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WillyChen0811/base/go/xxyica"
)

// TestMain 把 HOME 统一指到临时目录：钩子日志不许写进真实家目录。
// 需要不同家目录布局的用例再 t.Setenv("HOME", t.TempDir()) 覆盖。
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "xxyi-msg-home")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// useFakeTree 注入进程链与 pane 列表，测完还原。
func useFakeTree(t *testing.T, parents map[int]int, panes string) {
	t.Helper()
	oldPP, oldLP := parentPID, listPanes
	t.Cleanup(func() { parentPID, listPanes = oldPP, oldLP })
	parentPID = func(pid int) (int, error) {
		if p, ok := parents[pid]; ok {
			return p, nil
		}
		return 0, errors.New("不该问到这个 pid")
	}
	listPanes = func() (string, error) { return panes, nil }
}

func TestParentPIDFromProcStat(t *testing.T) {
	cases := []struct {
		name string
		stat string
		want int
		err  bool
	}{
		{"普通", "1234 (node) S 99 1234 1234 0 -1 0 100 0 0 0 5 0 0 0 20 0 1 0 555\n", 99, false},
		{"进程名带空格和括号", "42 (my (weird) proc) R 7 42 6 0 -1\n", 7, false},
		{"名字里全是括号", "9 ((kworker/0:1-events)) S 8\n", 8, false},
		{"括号后字段不全", "1234 (node) S\n", 0, true},
		{"没有括号", "1234 node S 99", 0, true},
		{"父 pid 不是数字", "1234 (node) S xx 1", 0, true},
	}
	for _, c := range cases {
		got, err := parentPIDFromProcStat([]byte(c.stat))
		if c.err {
			if err == nil {
				t.Errorf("%s：该报错，得到 %d", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s：得到 %d,%v，want %d", c.name, got, err, c.want)
		}
	}
}

func TestFindPaneSessionWalk(t *testing.T) {
	oldPP := parentPID
	t.Cleanup(func() { parentPID = oldPP })
	parentPID = func(pid int) (int, error) {
		switch pid {
		case 100:
			return 90, nil
		case 90:
			return 80, nil
		case 80:
			return 1, nil
		}
		return 0, errors.New("链断了")
	}
	panes := map[int]string{80: "codex1", 55: "other"}
	name, err := findPaneSession(100, panes)
	if err != nil || name != "codex1" {
		t.Fatalf("沿 100→90→80 该命中 codex1，得到 %q,%v", name, err)
	}
	// 链上无命中：走到 init 停，空串不是错误
	name, err = findPaneSession(100, map[int]string{777: "nope"})
	if err != nil || name != "" {
		t.Fatalf("无命中该空串收场，得到 %q,%v", name, err)
	}
	// pid 环/无限上涨：到 maxProcTreeDepth 层必须自己停
	bumps := 0
	parentPID = func(pid int) (int, error) { bumps++; return pid + 1, nil }
	name, err = findPaneSession(100, map[int]string{999999: "远在天边"})
	if err != nil || name != "" {
		t.Fatalf("超层数该停，得到 %q,%v", name, err)
	}
	if bumps > maxProcTreeDepth {
		t.Errorf("上溯 %d 次，超过上限 %d", bumps, maxProcTreeDepth)
	}
	// 取父进程失败原样带出
	parentPID = func(int) (int, error) { return 0, errors.New("读不到") }
	if _, err := findPaneSession(100, panes); err == nil {
		t.Error("取父进程失败该报错")
	}
}

func TestPaneSessionsOf(t *testing.T) {
	m := paneSessionsOf("80 codex1\n55 other\n\n垃圾行\nabc x\n")
	if len(m) != 2 || m[80] != "codex1" || m[55] != "other" {
		t.Errorf("解析得到 %v", m)
	}
	// 会话名带空格：完整留着，让 checkSessionName 去拒
	m = paneSessionsOf("80 中文 会话\n")
	if m[80] != "中文 会话" {
		t.Errorf("带空格会话名 %q", m[80])
	}
}

// XXYI_ADDR 和 TMUX 都为空：沿进程树命中 pane → mac: 会话名（HOME 里没有 ~/.xxyi-support 的东西）。
func TestSelfAddrFromProcTreeMac(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 90, 90: 80, 80: 1}, "80 codex1\n55 other")
	addr, err := selfAddr()
	if err != nil || addr != "mac:codex1" {
		t.Fatalf("得到 %q,%v，want mac:codex1", addr, err)
	}
}

// 有 ~/.xxyi-support 端点或证书 ⇒ Ubuntu 一侧：前缀 ubuntu:。
func TestSelfAddrFromProcTreeUbuntuSide(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 80, 80: 1}, "80 codex1\n")
	for _, marker := range []string{"endpoint.json", "client.json"} {
		if err := os.MkdirAll(filepath.Join(home, ".xxyi-support"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".xxyi-support", marker), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		addr, err := selfAddr()
		if err != nil || addr != "ubuntu:codex1" {
			t.Fatalf("%s 在位时得到 %q,%v", marker, addr, err)
		}
		if err := os.Remove(filepath.Join(home, ".xxyi-support", marker)); err != nil {
			t.Fatal(err)
		}
	}
}

// 链上无命中：selfAddr 报 errNoSelfAddr（send 的文案不变），state 静默 0 且不发请求。
func TestSelfAddrProcTreeNoHit(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 90, 90: 80, 80: 1}, "55 other\n77 second")

	errOut := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d，stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "不知道自己的地址") {
		t.Errorf("stderr %q", errOut.String())
	}
	if code := run([]string{"state", "busy"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("state 退出码 %d", code)
	}
	if n := cap.count(); n != 0 {
		t.Errorf("没地址时不该发请求，已发 %d", n)
	}
}

// listPanes 失败：同样报错 / 静默。
func TestSelfAddrProcTreeListPanesFails(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	oldLP := listPanes
	t.Cleanup(func() { listPanes = oldLP })
	listPanes = func() (string, error) { return "", errors.New("找不到 tmux") }

	if _, err := selfAddr(); err == nil || !strings.Contains(err.Error(), "不知道自己的地址") {
		t.Errorf("selfAddr 得到 %v", err)
	}
	if _, err := selfAddr(); err == nil || !strings.Contains(err.Error(), "找不到 tmux") {
		t.Errorf("原因要带进错误里：%v", err)
	}
	if code := run([]string{"state", "busy"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("state 退出码 %d", code)
	}
	if n := cap.count(); n != 0 {
		t.Errorf("列 pane 失败时 state 不该发请求，已发 %d", n)
	}
}

// 兜底取到的会话名不合法：跟 TMUX 分支同一套文案。
func TestSelfAddrProcTreeIllegalName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 80, 80: 1}, "80 中文 会话\n")
	_, err := selfAddr()
	if err == nil || !strings.Contains(err.Error(), "tmux rename-session") {
		t.Errorf("得到 %v，该提示改名", err)
	}
}

// state 每次运行在 ~/.xxyi/msg-hook.log 追加一行：地址、结果。
func TestHookLogLine(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".xxyi"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XXYI_ADDR", "ubuntu:cw")
	if code := run([]string{"state", "busy"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".xxyi", "msg-hook.log"))
	if err != nil {
		t.Fatalf("日志读不了：%v", err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasSuffix(line, "state busy addr=ubuntu:cw 已上报 busy") {
		t.Errorf("日志行 %q", line)
	}
	// 取不到地址也要留下原因
	if code := run([]string{"state", "idle"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	raw, _ = os.ReadFile(filepath.Join(home, ".xxyi", "msg-hook.log"))
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 2 || !strings.Contains(lines[1], "state idle addr=ubuntu:cw") {
		t.Errorf("第二行 %q", string(raw))
	}
}

// 地址失败的原因也进日志（哪一步失败一目了然）。
func TestHookLogAddrFailure(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".xxyi"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	oldPP, oldLP := parentPID, listPanes
	t.Cleanup(func() { parentPID, listPanes = oldPP, oldLP })
	parentPID = func(int) (int, error) { return 1, nil }
	listPanes = func() (string, error) { return "55 other", nil }

	if code := run([]string{"state", "busy", "--check"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".xxyi", "msg-hook.log"))
	if err != nil {
		t.Fatalf("日志读不了：%v", err)
	}
	if !strings.Contains(string(raw), "state busy --check 取地址失败：不知道自己的地址") {
		t.Errorf("日志 %q", string(raw))
	}
}

// Ubuntu 一侧：日志写到 ~/.xxyi-support，Mac 侧目录不碰。
func TestHookLogUbuntuSideDir(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".xxyi"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".xxyi-support"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".xxyi-support", "client.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XXYI_ADDR", "ubuntu:cw")
	if code := run([]string{"state", "gone"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".xxyi-support", "msg-hook.log")); err != nil {
		t.Errorf("Ubuntu 侧日志：%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".xxyi", "msg-hook.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Mac 侧目录不该有日志：%v", err)
	}
}

// 目录不存在就不写；写了也不报错。
func TestHookLogNoDirNoWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hookLog([]string{"busy"}, "不该被写")
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("家目录里冒出了 %v", entries)
	}
}

// 超过 256KB：先整份改名 .1，再写新的。
func TestHookLogRotation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".xxyi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "msg-hook.log")
	old := strings.Repeat("x", maxHookLogBytes+1)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	hookLog([]string{"idle"}, "轮转验证")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 1024 {
		t.Errorf("新日志 %d 字节，该只有刚写的一行", len(raw))
	}
	if !strings.Contains(string(raw), "state idle 轮转验证") {
		t.Errorf("新日志 %q", string(raw))
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf(".1 读不了：%v", err)
	}
	if len(rotated) != len(old) {
		t.Errorf(".1 长 %d，want %d", len(rotated), len(old))
	}
}

// 选侧规则的四种文件布局（connect 与 selfAddr 共用 isUbuntuSideFiles，结论必须一致）：
// 只有 endpoint.json → ubuntu；只有 Ubuntu 证书（没有 Mac 的 config.json）→ ubuntu；
// 证书加 config.json 但没有 endpoint.json → mac；什么都没有 → mac。
func TestUbuntuSideRule(t *testing.T) {
	cases := []struct {
		name                  string
		endpoint, bundle, cfg bool
		wantUbuntu            bool
	}{
		{"只有 endpoint.json", true, false, false, true},
		{"只有 Ubuntu 证书没有 config.json", false, true, false, true},
		{"证书加 config.json 没有 endpoint.json", false, true, true, false},
		{"什么都没有", false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			sup := filepath.Join(home, ".xxyi-support")
			mac := filepath.Join(home, ".xxyi")
			for _, dir := range []string{sup, mac} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// 包级路径与家目录下的默认路径重合：connect 与 ubuntuSide 看的是同一组文件
			setPaths(t, filepath.Join(sup, "endpoint.json"), filepath.Join(mac, "config.json"))
			clientBundlePathUbuntu = filepath.Join(sup, "client.json") // setPaths 已登记还原
			for _, p := range []string{endpointPath, clientBundlePathUbuntu, xxyiConfigPath} {
				_ = os.Remove(p)
			}
			touch := func(p string) {
				if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if c.endpoint {
				touch(endpointPath)
			}
			if c.bundle {
				touch(clientBundlePathUbuntu)
			}
			if c.cfg {
				touch(xxyiConfigPath)
			}

			if got := isUbuntuSideFiles(endpointPath, clientBundlePathUbuntu, xxyiConfigPath); got != c.wantUbuntu {
				t.Errorf("isUbuntuSideFiles = %v，want %v", got, c.wantUbuntu)
			}
			if got := ubuntuSide(); got != c.wantUbuntu {
				t.Errorf("ubuntuSide() = %v，want %v", got, c.wantUbuntu)
			}
			// 进程树兜底的地址前缀跟着侧别走
			t.Setenv("XXYI_ADDR", "")
			t.Setenv("TMUX", "")
			useFakeTree(t, map[int]int{os.Getpid(): 80, 80: 1}, "80 codex1\n")
			want := "mac:codex1"
			if c.wantUbuntu {
				want = "ubuntu:codex1"
			}
			addr, err := selfAddr()
			if err != nil || addr != want {
				t.Errorf("selfAddr = %q,%v，want %q", addr, err, want)
			}
		})
	}
}

// P2 的触发场景：有 ~/.xxyi-support/client.json 和 ~/.xxyi/config.json、没有 endpoint.json。
// connect 走 Mac 本机接口，selfAddr 的兜底前缀也必须是 mac:（判成 ubuntu 会被本机接口 403 拒）。
func TestSideConnectAndSelfAddrAgree(t *testing.T) {
	var msgCap captured
	msgSrv := httptest.NewServer(msgCap.handler())
	defer msgSrv.Close()
	api := http.NewServeMux()
	api.HandleFunc("/api/terminal", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"http":%q,"token":"mtk"}`, authority(msgSrv.URL))
	})
	apiSrv := httptest.NewServer(api)
	defer apiSrv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	sup := filepath.Join(home, ".xxyi-support")
	if err := os.MkdirAll(sup, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, ".xxyi", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"http_port":%q}`, portOf(t, apiSrv.URL))), 0o600); err != nil {
		t.Fatal(err)
	}
	ca, err := xxyica.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(sup, "client.json")
	writeTestBundle(t, ca, bundle, "ubuntu")

	setPaths(t, filepath.Join(sup, "endpoint.json"), cfg) // endpoint.json 故意不写
	clientBundlePathUbuntu = bundle
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	useFakeTree(t, map[int]int{os.Getpid(): 80, 80: 1}, "80 codex1\n")

	if ubuntuSide() {
		t.Error("装了 Mac 配置又没有端点：该判成 Mac")
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	c, err := connect(ctx, httpTimeout)
	if err != nil {
		t.Fatalf("connect 失败：%v", err)
	}
	if !c.query || c.token != "mtk" {
		t.Errorf("connect 该走 Mac 侧（令牌在查询参数里）：query=%v token=%q", c.query, c.token)
	}
	if addr, err := selfAddr(); err != nil || addr != "mac:codex1" {
		t.Fatalf("selfAddr = %q,%v", addr, err)
	}

	out := &bytes.Buffer{}
	if code := run([]string{"send", "ubuntu:boss", "hi"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d，输出 %q", code, out.String())
	}
	got := msgCap.last(t)
	if got.body.From != "mac:codex1" || got.query != "token=mtk" {
		t.Errorf("from=%q query=%q，前缀该跟 connect 的侧别一致", got.body.From, got.query)
	}
}

// 兜底成功后 send 照常走网络（不只是 selfAddr 单测）。
func TestSendViaProcTreeFallback(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 90, 90: 80, 80: 1}, "80 codex1\n55 other")

	out := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if got := cap.last(t); got.body.From != "mac:codex1" {
		t.Errorf("from %q", got.body.From)
	}
}
