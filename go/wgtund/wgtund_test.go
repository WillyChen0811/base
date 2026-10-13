package wgtund

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/internal/sshtest"
	"github.com/WillyChen0811/base/go/wgtunnel"
	"github.com/coder/websocket"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/net/proxy"
)

func wgKey(t *testing.T) (priv, pub string) {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	p, _ := curve25519.X25519(k, curve25519.Basepoint)
	return base64.StdEncoding.EncodeToString(k), base64.StdEncoding.EncodeToString(p)
}

// fixture：测试这边起一条隧道 A（扮演"远端网络"，里面有 SSH 服务和回显服务），
// 配置目录里写好让助手连向 A 的隧道 home、主机清单、known_hosts、私钥文件。
type fixture struct {
	dir    string
	remote *wgtunnel.Tunnel
	ssh    *sshtest.Server
}

func newFixture(t *testing.T, mutate func(cfg map[string]any)) *fixture {
	t.Helper()
	return newFixtureExec(t, mutate, nil)
}

// newFixtureExec 同 newFixture，但给隧道内的 SSH 服务器配 exec 行为（假 tmux 用）。
func newFixtureExec(t *testing.T, mutate func(cfg map[string]any), exec func(string) (string, string, int)) *fixture {
	t.Helper()
	return newFixtureOpts(t, mutate, sshtest.Options{Exec: exec})
}

// newFixtureOpts 同 newFixtureExec，但可完整指定 SSH 服务器选项（如 Forward）。
func newFixtureOpts(t *testing.T, mutate func(cfg map[string]any), opts sshtest.Options) *fixture {
	t.Helper()
	dir := t.TempDir()
	aPriv, aPub := wgKey(t)
	bPriv, bPub := wgKey(t)
	ca, err := wgtunnel.ParseConfig([]byte(fmt.Sprintf("[Interface]\nPrivateKey=%s\nAddress=10.99.0.1/32\n[Peer]\nPublicKey=%s\nAllowedIPs=10.99.0.2/32\n", aPriv, bPub)))
	if err != nil {
		t.Fatal(err)
	}
	a, err := wgtunnel.Open(ca, wgtunnel.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	os.MkdirAll(filepath.Join(dir, "tunnels"), 0o700)
	os.WriteFile(filepath.Join(dir, "tunnels", "home.conf"), []byte(fmt.Sprintf(
		"[Interface]\nPrivateKey=%s\nAddress=10.99.0.2/32\n[Peer]\nPublicKey=%s\nAllowedIPs=10.99.0.0/24\nEndpoint=127.0.0.1:%d\nPersistentKeepalive=1\n",
		bPriv, aPub, a.ListenPort())), 0o600)

	// 远端 SSH 服务（在隧道 A 里监听 22）
	clientKey, clientRaw := sshtest.NewKey()
	ln, err := a.Listen("tcp", "10.99.0.1:22")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := sshtest.ServeWith(ln, clientKey.PublicKey(), opts)

	// 远端回显服务（在隧道 A 里监听 7000），给 SOCKS 测试用
	eln, err := a.Listen("tcp", "10.99.0.1:7000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eln.Close() })
	go func() {
		for {
			c, err := eln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				l, _ := bufio.NewReader(c).ReadString('\n')
				io.WriteString(c, "remote:"+l)
			}()
		}
	}()

	// 私钥文件（OpenSSH 格式）和 known_hosts
	pemBlock, err := ssh.MarshalPrivateKey(clientRaw, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "id_test")
	os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600)
	khPath := filepath.Join(dir, "known_hosts")
	os.WriteFile(khPath, []byte(knownhosts.Line([]string{knownhosts.Normalize("10.99.0.1:22")}, srv.HostKey.PublicKey())+"\n"), 0o600)

	cfg := map[string]any{
		"known_hosts":     khPath,
		"allowed_origins": []string{"127.0.0.1:8765"},
		"socks":           map[string]string{"home": "127.0.0.1:0"},
		"hosts": []map[string]any{{
			"name": "ubuntu", "tunnel": "home", "addr": "10.99.0.1:22", "user": "chenwy",
			"command": "tmux new -A -s main", "identity_files": []string{keyPath},
		}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(filepath.Join(dir, "hosts.json"), b, 0o600)
	return &fixture{dir: dir, remote: a, ssh: srv}
}

type started struct {
	HTTP  string            `json:"http"`
	Token string            `json:"token"`
	Socks map[string]string `json:"socks"`
}

// run 启动助手，返回启动信息、关掉 stdin 的函数、Run 的返回值通道
func run(t *testing.T, dir string) (started, func(), chan error) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { done <- Run(ctx, dir, inR, outW); outW.Close() }()
	lineCh := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(outR).ReadString('\n')
		lineCh <- l
		io.Copy(io.Discard, outR)
	}()
	var st started
	select {
	case l := <-lineCh:
		if err := json.Unmarshal([]byte(l), &st); err != nil {
			select {
			case err := <-done:
				t.Fatalf("Run 提前退出: %v", err)
			default:
			}
			t.Fatalf("第一行应是启动信息 JSON，得到 %q", l)
		}
	case err := <-done:
		t.Fatalf("Run 提前退出: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("等不到启动信息")
	}
	t.Cleanup(func() { inW.Close() })
	return st, func() { inW.Close() }, done
}

func get(t *testing.T, st started, path string, withToken bool) (*http.Response, []byte) {
	t.Helper()
	u := "http://" + st.HTTP + path
	if withToken {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		u += sep + "token=" + st.Token
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestStartupInfo(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	if !strings.HasPrefix(st.HTTP, "127.0.0.1:") || len(st.Token) < 32 {
		t.Errorf("启动信息 %+v：应只监听 127.0.0.1，令牌至少 32 个字符", st)
	}
	if !strings.HasPrefix(st.Socks["home"], "127.0.0.1:") || strings.HasSuffix(st.Socks["home"], ":0") {
		t.Errorf("socks 应报告实际端口: %v", st.Socks)
	}
}

func TestTokenRequired(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	if resp, _ := get(t, st, "/hosts", false); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无令牌应 401，得到 %d", resp.StatusCode)
	}
	resp, b := get(t, st, "/hosts", true)
	if resp.StatusCode != 200 {
		t.Fatalf("有令牌应 200，得到 %d", resp.StatusCode)
	}
	var hosts []struct{ Name, Tunnel string }
	json.Unmarshal(b, &hosts)
	if len(hosts) != 1 || hosts[0].Name != "ubuntu" || hosts[0].Tunnel != "home" {
		t.Errorf("/hosts = %s", b)
	}
	if strings.Contains(string(b), "id_test") || strings.Contains(string(b), "PRIVATE") {
		t.Errorf("/hosts 不应暴露私钥路径或内容: %s", b)
	}
}

func dialTerm(t *testing.T, st started, query string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Origin", "http://127.0.0.1:8765")
	c, _, err := websocket.Dial(ctx, "ws://"+st.HTTP+"/term?token="+st.Token+"&"+query, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func TestTerminalThroughTunnel(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	c := dialTerm(t, st, "host=ubuntu&cols=100&rows=30")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c.Write(ctx, websocket.MessageBinary, []byte("hi\n"))
	var buf strings.Builder
	for !strings.Contains(buf.String(), "\n") {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("读终端输出失败: %v（已读 %q）", err, buf.String())
		}
		if typ == websocket.MessageText {
			t.Fatalf("意外的控制消息: %s", b)
		}
		buf.Write(b)
	}
	if strings.TrimSpace(buf.String()) != "echo:hi" {
		t.Errorf("got %q", buf.String())
	}
	if cmds := f.ssh.Commands(); len(cmds) != 1 || cmds[0] != "exec:tmux new -A -s main" {
		t.Errorf("远端命令 %v", cmds)
	}
}

func readCtrl(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("没收到控制消息: %v", err)
		}
		if typ == websocket.MessageText {
			var m map[string]any
			json.Unmarshal(b, &m)
			return m
		}
	}
}

func TestUnknownHost(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	c := dialTerm(t, st, "host=nope&cols=80&rows=24")
	if m := readCtrl(t, c); m["type"] != "error" {
		t.Errorf("未知主机应回 error 消息: %v", m)
	}
}

// known_hosts 里登记的不是这台主机的密钥：必须拒绝
func TestHostKeyMismatch(t *testing.T) {
	f := newFixture(t, nil)
	other := sshtest.NewSigner()
	os.WriteFile(filepath.Join(f.dir, "known_hosts"), []byte(knownhosts.Line([]string{knownhosts.Normalize("10.99.0.1:22")}, other.PublicKey())+"\n"), 0o600)
	st, _, _ := run(t, f.dir)
	c := dialTerm(t, st, "host=ubuntu&cols=80&rows=24")
	if m := readCtrl(t, c); m["type"] != "error" {
		t.Errorf("主机密钥不符应回 error: %v", m)
	}
	if len(f.ssh.Commands()) != 0 {
		t.Error("主机密钥不符时不应执行任何命令")
	}
}

func TestSocksThroughTunnel(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	p, err := proxy.SOCKS5("tcp", st.Socks["home"], nil, &net.Dialer{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Dial("tcp", "10.99.0.1:7000")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	io.WriteString(c, "ping\n")
	l, _ := bufio.NewReader(c).ReadString('\n')
	if strings.TrimSpace(l) != "remote:ping" {
		t.Errorf("got %q", l)
	}
}

func TestStatus(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	// 先走一次流量，确保握手完成
	warmTraffic(t, st)
	resp, b := get(t, st, "/status", true)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var s struct {
		Tunnels map[string]struct {
			Peers []struct {
				PublicKey     string `json:"public_key"`
				LastHandshake string `json:"last_handshake"`
				Rx            uint64 `json:"rx_bytes"`
				Tx            uint64 `json:"tx_bytes"`
			} `json:"peers"`
		} `json:"tunnels"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	home, ok := s.Tunnels["home"]
	if !ok || len(home.Peers) != 1 || home.Peers[0].LastHandshake == "" || home.Peers[0].Rx == 0 {
		t.Errorf("/status = %s", b)
	}
	if strings.Contains(string(b), "PrivateKey") || strings.Contains(string(b), "private") {
		t.Errorf("/status 不应含私钥: %s", b)
	}
}

func warmTraffic(t *testing.T, st started) {
	t.Helper()
	p, _ := proxy.SOCKS5("tcp", st.Socks["home"], nil, &net.Dialer{Timeout: 10 * time.Second})
	c, err := p.Dial("tcp", "10.99.0.1:7000")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	io.WriteString(c, "x\n")
	bufio.NewReader(c).ReadString('\n')
}

// stdin 关闭（父进程没了）时，Run 退出并关掉所有端口
func TestExitOnStdinClose(t *testing.T) {
	f := newFixture(t, nil)
	st, closeIn, done := run(t, f.dir)
	closeIn()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run 返回 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stdin 关闭后 Run 没有退出")
	}
	if c, err := net.DialTimeout("tcp", st.HTTP, time.Second); err == nil {
		c.Close()
		t.Error("退出后 HTTP 端口应关闭")
	}
}

// 配置错误：在打印启动信息之前就返回错误
func TestBadConfig(t *testing.T) {
	cases := map[string]func(map[string]any){
		"主机引用了不存在的隧道": func(c map[string]any) {
			c["hosts"].([]map[string]any)[0]["tunnel"] = "nope"
		},
		"socks 引用了不存在的隧道": func(c map[string]any) {
			c["socks"] = map[string]string{"nope": "127.0.0.1:0"}
		},
		"socks 监听非回环地址": func(c map[string]any) {
			c["socks"] = map[string]string{"home": "0.0.0.0:0"}
		},
		"主机名为空": func(c map[string]any) {
			c["hosts"].([]map[string]any)[0]["name"] = ""
		},
		"主机名重复": func(c map[string]any) {
			h := c["hosts"].([]map[string]any)
			c["hosts"] = append(h, h[0])
		},
		"主机地址没端口": func(c map[string]any) {
			c["hosts"].([]map[string]any)[0]["addr"] = "10.99.0.1"
		},
		"缺 known_hosts": func(c map[string]any) {
			delete(c, "known_hosts")
		},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, m)
			var out strings.Builder
			err := Run(context.Background(), f.dir, strings.NewReader(""), &out)
			if err == nil {
				t.Fatal("应报错")
			}
			if out.Len() != 0 {
				t.Errorf("出错时不应打印启动信息: %q", out.String())
			}
		})
	}
}

// 第二条隧道配置坏了，也要整体报错并指出是哪个文件
func TestBadTunnelFile(t *testing.T) {
	f := newFixture(t, nil)
	os.WriteFile(filepath.Join(f.dir, "tunnels", "broken.conf"), []byte("[Interface]\nPrivateKey=AAAA\n"), 0o600)
	err := Run(context.Background(), f.dir, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "broken.conf") {
		t.Errorf("应报错并带文件名，得到 %v", err)
	}
}

// 页面和助手端口不同：白名单来源要拿到 CORS 头，其他来源不给
func TestCORS(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	for origin, want := range map[string]string{"http://127.0.0.1:8765": "http://127.0.0.1:8765", "http://evil.example": ""} {
		req, _ := http.NewRequest("GET", "http://"+st.HTTP+"/hosts?token="+st.Token, nil)
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("Origin %s: ACAO = %q, want %q", origin, got, want)
		}
	}
}
