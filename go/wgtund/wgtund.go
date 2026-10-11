// Package wgtund 是助手程序的实现：按配置目录起 WireGuard 隧道、SOCKS 口，
// 并在 127.0.0.1 上提供带令牌的 HTTP 接口（主机清单、状态、SSH 终端桥）。
package wgtund

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
	"github.com/WillyChen0811/base/go/wgtunnel"
	"github.com/WillyChen0811/base/go/wgtunnel/socks"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type hostConf struct {
	Name          string   `json:"name"`
	Tunnel        string   `json:"tunnel"`
	Addr          string   `json:"addr"`
	User          string   `json:"user"`
	Command       string   `json:"command"`
	IdentityFiles []string `json:"identity_files"`
}

type fileConf struct {
	KnownHosts     string            `json:"known_hosts"`
	AllowedOrigins []string          `json:"allowed_origins"`
	Socks          map[string]string `json:"socks"`
	Hosts          []hostConf        `json:"hosts"`
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

func loadConf(dir string) (*fileConf, error) {
	b, err := os.ReadFile(filepath.Join(dir, "hosts.json"))
	if err != nil {
		return nil, err
	}
	var c fileConf
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("hosts.json: %w", err)
	}
	if c.KnownHosts == "" {
		return nil, errors.New("hosts.json: 缺少 known_hosts")
	}
	return &c, nil
}

// Run 读配置目录，起隧道、SOCKS 口和 HTTP 服务；就绪后往 stdout 写一行启动信息 JSON。
// stdin 读到 EOF 或 ctx 取消时关掉一切并返回 nil。
func Run(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer) (err error) {
	conf, err := loadConf(dir)
	if err != nil {
		return err
	}

	var closers []io.Closer
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i].Close()
		}
	}()

	files, err := filepath.Glob(filepath.Join(dir, "tunnels", "*.conf"))
	if err != nil {
		return err
	}
	cfgs := map[string]*wgtunnel.Config{}
	for _, f := range files {
		base := filepath.Base(f)
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("%s: %w", base, err)
		}
		cfg, err := wgtunnel.ParseConfig(b)
		if err != nil {
			return fmt.Errorf("%s: %w", base, err)
		}
		cfgs[strings.TrimSuffix(base, ".conf")] = cfg
	}
	seen := map[string]bool{}
	for _, h := range conf.Hosts {
		if h.Name == "" || seen[h.Name] {
			return fmt.Errorf("hosts.json: 主机名 %q 为空或重复", h.Name)
		}
		seen[h.Name] = true
		if _, _, err := net.SplitHostPort(h.Addr); err != nil {
			return fmt.Errorf("hosts.json: 主机 %q 的地址 %q 要写成 主机:端口", h.Name, h.Addr)
		}
		if h.Tunnel != "" && cfgs[h.Tunnel] == nil {
			return fmt.Errorf("hosts.json: 主机 %q 引用了不存在的隧道 %q", h.Name, h.Tunnel)
		}
	}
	for name, addr := range conf.Socks {
		if cfgs[name] == nil {
			return fmt.Errorf("hosts.json: socks 引用了不存在的隧道 %q", name)
		}
		if host, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("hosts.json: socks 地址 %q: %w", addr, err)
		} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("hosts.json: socks 地址 %q 必须是回环地址", addr)
		}
	}
	knownHostsCb, err := knownhosts.New(expand(conf.KnownHosts))
	if err != nil {
		return fmt.Errorf("known_hosts: %w", err)
	}

	tunnels := map[string]*wgtunnel.Tunnel{}
	for name, cfg := range cfgs {
		t, err := wgtunnel.Open(cfg, wgtunnel.Options{})
		if err != nil {
			return fmt.Errorf("%s.conf: %w", name, err)
		}
		closers = append(closers, t)
		tunnels[name] = t
	}

	socksAddrs := map[string]string{}
	for name, addr := range conf.Socks {
		s, err := socks.Listen(addr, tunnels[name], socks.Options{})
		if err != nil {
			return fmt.Errorf("socks %s: %w", name, err)
		}
		closers = append(closers, s)
		socksAddrs[name] = s.Addr().String()
	}

	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	token := hex.EncodeToString(tok)

	hosts := map[string]hostConf{}
	for _, h := range conf.Hosts {
		hosts[h.Name] = h
	}
	open := func(r *http.Request, cols, rows int) (*sshterm.Session, error) {
		h, ok := hosts[r.URL.Query().Get("host")]
		if !ok {
			return nil, errors.New("未知主机")
		}
		var d sshterm.Dialer = &net.Dialer{Timeout: 15 * time.Second}
		if h.Tunnel != "" {
			d = tunnels[h.Tunnel]
		}
		signers, agentConn, keyErrs := loadSigners(h.IdentityFiles)
		if agentConn != nil {
			defer agentConn.Close()
		}
		if len(signers) == 0 {
			if len(keyErrs) > 0 {
				return nil, fmt.Errorf("没有可用的私钥：%s", strings.Join(keyErrs, "；"))
			}
			return nil, errors.New("没有可用的私钥（identity_files 为空，ssh-agent 也没有密钥）")
		}
		// ssh.Client 握手期间就会用完 signer，Open 返回后 agent 连接不再需要
		return sshterm.Open(r.Context(), d,
			sshterm.Target{Addr: h.Addr, User: h.User, Command: h.Command},
			sshterm.Auth{Signers: signers, HostKeyCallback: knownHostsCb}, cols, rows)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hosts", func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			Name   string `json:"name"`
			Tunnel string `json:"tunnel"`
		}
		list := []item{}
		for _, h := range conf.Hosts {
			list = append(list, item{h.Name, h.Tunnel})
		}
		writeJSON(w, list)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		type peer struct {
			PublicKey     string `json:"public_key"`
			Endpoint      string `json:"endpoint"`
			LastHandshake string `json:"last_handshake"`
			Rx            uint64 `json:"rx_bytes"`
			Tx            uint64 `json:"tx_bytes"`
		}
		type tun struct {
			Peers []peer `json:"peers"`
		}
		out := map[string]tun{}
		for name, t := range tunnels {
			ps := []peer{}
			for _, p := range t.Stats().Peers {
				hs := ""
				if !p.LastHandshake.IsZero() {
					hs = p.LastHandshake.UTC().Format(time.RFC3339)
				}
				ps = append(ps, peer{p.PublicKey, p.Endpoint, hs, p.RxBytes, p.TxBytes})
			}
			out[name] = tun{ps}
		}
		writeJSON(w, map[string]any{"tunnels": out})
	})
	mux.Handle("/term", sshterm.Handler(open, sshterm.BridgeOptions{OriginPatterns: conf.AllowedOrigins}))

	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 页面（如 xxyi 的 127.0.0.1:8765）和本服务端口不同，fetch /hosts、/status 属于跨域：
		// 只对 allowed_origins 里的来源放行；WebSocket 不受 CORS 管，由 sshterm 自己校验 Origin
		if o := r.Header.Get("Origin"); o != "" && originAllowed(o, conf.AllowedOrigins) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Add("Vary", "Origin")
		}
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: guarded}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	closers = append(closers, srv)

	info, _ := json.Marshal(map[string]any{"http": ln.Addr().String(), "token": token, "socks": socksAddrs})
	if _, err := fmt.Fprintf(stdout, "%s\n", info); err != nil {
		return err
	}

	eof := make(chan struct{})
	go func() {
		io.Copy(io.Discard, stdin)
		close(eof)
	}()
	// 能关的话退出时关掉 stdin，让上面的读取协程结束
	if c, ok := stdin.(io.Closer); ok {
		defer c.Close()
	}
	select {
	case <-eof:
	case <-ctx.Done():
	case err := <-served:
		// HTTP 服务意外停了：整体退出，让父进程看到并重启
		return fmt.Errorf("HTTP 服务停止: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// loadSigners 先读私钥文件，再追加 ssh-agent 里的密钥。读不了的文件跳过，
// 但把原因（只含文件名，不含内容）收进 errs，没有任何可用密钥时报给用户。
func loadSigners(files []string) (signers []ssh.Signer, agentConn io.Closer, errs []string) {
	for _, f := range files {
		b, err := os.ReadFile(expand(f))
		if err == nil {
			var s ssh.Signer
			if s, err = ssh.ParsePrivateKey(b); err == nil {
				signers = append(signers, s)
				continue
			}
		}
		errs = append(errs, fmt.Sprintf("%s: %v", filepath.Base(f), err))
	}
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return signers, nil, errs
	}
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return signers, nil, errs
	}
	as, err := agent.NewClient(c).Signers()
	if err != nil {
		c.Close()
		return signers, nil, errs
	}
	return append(signers, as...), c, errs
}

// originAllowed：Origin 的主机部分（含端口）必须正好是白名单里的某一项
func originAllowed(origin string, allowed []string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, a := range allowed {
		if strings.EqualFold(u.Host, a) {
			return true
		}
	}
	return false
}
