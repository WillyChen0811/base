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
	"strconv"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
	"github.com/WillyChen0811/base/go/tmux"
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
	Support        *supportConf      `json:"support"`
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
	prep, err := prepareSupport(conf, dir)
	if err != nil {
		return err
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
	// 每台主机共用一条 SSH 连接，第一次用到时才拨号
	pool := newSSHPool(knownHostsCb)
	closers = append(closers, pool)
	for _, h := range conf.Hosts {
		var d sshterm.Dialer = &net.Dialer{Timeout: dialTimeout}
		if h.Tunnel != "" {
			d = tunnels[h.Tunnel]
		}
		pool.add(h, d)
	}

	open := func(r *http.Request, cols, rows int) (*sshterm.Session, error) {
		q := r.URL.Query()
		name := q.Get("host")
		if _, ok := hosts[name]; !ok {
			return nil, errors.New("未知主机")
		}
		// /term?session=名：在共用连接上附加 tmux 会话；
		// 没有 session 时执行主机配置的 command（空则登录 shell），同样走共用连接
		command := hosts[name].Command
		if s := q.Get("session"); s != "" {
			c, err := tmux.AttachCommand(s)
			if err != nil { // ValidTarget 校验：控制字符等在这里拒绝
				return nil, err
			}
			command = c
		}
		return pool.session(r.Context(), name, command, cols, rows)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	closers = append(closers, ln)

	// 协助服务（可选）：本机 HTTP 地址和钩子令牌都要在写合并设置文件前拿到
	var supportLocal, supportHook http.Handler
	if prep != nil {
		svc, err := startSupport(prep, dir, expand(conf.KnownHosts), ln.Addr().String(), socksAddrs, pool)
		if err != nil {
			return err
		}
		closers = append(closers, closerFunc(svc.Close))
		supportLocal, supportHook = svc.LocalHandler(), svc.HookHandler()
	}

	mux := http.NewServeMux()
	supportRoute := func(w http.ResponseWriter, r *http.Request) {
		if supportLocal == nil {
			errJSON(w, http.StatusNotFound, "没有配置协助")
			return
		}
		// 开始/停止/批准只许从 xxyi 页面发起：要求带白名单内的 Origin（curl 等没有 Origin 的调用拿主令牌也不行）
		if r.Method == http.MethodPost {
			switch r.URL.Path {
			case "/support/start", "/support/stop", "/support/decide":
				if o := r.Header.Get("Origin"); o == "" || !originAllowed(o, conf.AllowedOrigins) {
					errJSON(w, http.StatusForbidden, "需要从 xxyi 页面操作")
					return
				}
			}
		}
		supportLocal.ServeHTTP(w, r)
	}
	mux.HandleFunc("/support", supportRoute)
	mux.HandleFunc("/support/", supportRoute)
	mux.HandleFunc("/hosts", func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			Name      string `json:"name"`
			Tunnel    string `json:"tunnel"`
			Connected bool   `json:"connected"`
			Error     string `json:"error"`
		}
		list := []item{}
		for _, h := range conf.Hosts {
			connected, lastErr := pool.info(h.Name)
			list = append(list, item{h.Name, h.Tunnel, connected, lastErr})
		}
		writeJSON(w, list)
	})

	// tmux 会话管理。写操作用 POST 但不带请求体和自定义头，
	// 这样浏览器跨域时属于"简单请求"，不会先发预检（OPTIONS）。
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			errJSON(w, http.StatusMethodNotAllowed, "只支持 GET")
			return
		}
		if _, ok := hostOf(w, r, hosts); !ok {
			return
		}
		out, err := pool.run(r.Context(), r.URL.Query().Get("host"), tmux.ListCommand())
		if err != nil {
			var ee *sshterm.ExitError
			if errors.As(err, &ee) && tmux.NoServer(ee.Stderr) {
				writeJSON(w, map[string]any{"sessions": []any{}}) // 没有 tmux 服务：空列表
				return
			}
			errJSON(w, http.StatusBadGateway, "列会话失败: "+err.Error())
			return
		}
		list, err := tmux.ParseList(out)
		if err != nil {
			errJSON(w, http.StatusBadGateway, "解析会话列表失败: "+err.Error())
			return
		}
		type item struct {
			Name     string `json:"name"`
			Attached int    `json:"attached"`
			Windows  int    `json:"windows"`
			Created  int64  `json:"created"`
			Activity int64  `json:"activity"`
		}
		items := []item{}
		for _, s := range list {
			items = append(items, item{s.Name, s.Attached, s.Windows, s.Created.Unix(), s.Activity.Unix()})
		}
		writeJSON(w, map[string]any{"sessions": items})
	})

	mux.HandleFunc("/sessions/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			errJSON(w, http.StatusMethodNotAllowed, "请用 POST")
			return
		}
		hname := r.URL.Query().Get("host")
		if _, ok := hosts[hname]; !ok {
			errJSON(w, http.StatusNotFound, "未知主机")
			return
		}
		name := r.URL.Query().Get("name")
		cmd, err := tmux.NewCommand(name) // 内含 ValidNewName
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := pool.run(r.Context(), hname, cmd); err != nil {
			var ee *sshterm.ExitError
			if errors.As(err, &ee) && tmux.Duplicate(ee.Stderr) {
				errJSON(w, http.StatusConflict, "会话已存在: "+name)
				return
			}
			errJSON(w, http.StatusBadGateway, "新建会话失败: "+err.Error())
			return
		}
		writeJSON(w, map[string]any{"name": name})
	})

	mux.HandleFunc("/sessions/kill", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			errJSON(w, http.StatusMethodNotAllowed, "请用 POST")
			return
		}
		hname := r.URL.Query().Get("host")
		if _, ok := hosts[hname]; !ok {
			errJSON(w, http.StatusNotFound, "未知主机")
			return
		}
		cmd, err := tmux.KillCommand(r.URL.Query().Get("name")) // 内含 ValidTarget
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := pool.run(r.Context(), hname, cmd); err != nil {
			var ee *sshterm.ExitError
			if errors.As(err, &ee) && tmux.NotFound(ee.Stderr) {
				errJSON(w, http.StatusNotFound, "会话不存在")
				return
			}
			errJSON(w, http.StatusBadGateway, "结束会话失败: "+err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// 抓取会话窗格内容（给以后任务中心读会话输出用）
	mux.HandleFunc("/capture", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			errJSON(w, http.StatusMethodNotAllowed, "只支持 GET")
			return
		}
		hname := r.URL.Query().Get("host")
		if _, ok := hosts[hname]; !ok {
			errJSON(w, http.StatusNotFound, "未知主机")
			return
		}
		lines := 200
		if v := r.URL.Query().Get("lines"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errJSON(w, http.StatusBadRequest, "lines 要是整数")
				return
			}
			lines = n
		}
		cmd, err := tmux.CaptureCommand(r.URL.Query().Get("name"), lines) // 内含 ValidTarget 与 1..5000
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		out, err := pool.run(r.Context(), hname, cmd)
		if err != nil {
			var ee *sshterm.ExitError
			if errors.As(err, &ee) && tmux.NotFound(ee.Stderr) {
				errJSON(w, http.StatusNotFound, "会话不存在")
				return
			}
			if errors.Is(err, sshterm.ErrOutputTooLarge) {
				errJSON(w, http.StatusBadGateway, "输出过大")
				return
			}
			errJSON(w, http.StatusBadGateway, "抓取会话失败: "+err.Error())
			return
		}
		writeJSON(w, map[string]any{"text": string(out)})
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
		// 钩子入口由 support 自己校验钩子令牌，不走主令牌；只放行这一个精确路径
		if supportHook != nil && r.URL.Path == "/support/hook" {
			supportHook.ServeHTTP(w, r)
			return
		}
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

// closerFunc 让普通函数当 io.Closer 用。
type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// errJSON 返回带中文原因的错误 JSON。
func errJSON(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// hostOf 校验 host 参数；未知主机回 404。
func hostOf(w http.ResponseWriter, r *http.Request, hosts map[string]hostConf) (hostConf, bool) {
	h, ok := hosts[r.URL.Query().Get("host")]
	if !ok {
		errJSON(w, http.StatusNotFound, "未知主机")
	}
	return h, ok
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
