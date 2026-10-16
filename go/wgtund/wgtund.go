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
	Name          string      `json:"name"`
	Tunnel        string      `json:"tunnel"`
	Addr          string      `json:"addr"`
	Routes        []routeConf `json:"routes"`
	User          string      `json:"user"`
	Command       string      `json:"command"`
	IdentityFiles []string    `json:"identity_files"`
}

// routeConf 是主机的一条线路：经哪条隧道（空为直连）到主机的哪个地址。
type routeConf struct {
	Label  string `json:"label"`
	Tunnel string `json:"tunnel"`
	Addr   string `json:"addr"`
}

// hostRoutes 返回主机规范化后的线路列表：写了 routes 就用它，否则把 tunnel/addr 当作唯一一条线路。
// label 为空时补全：先用隧道名，隧道名也空则用「直连」。
func hostRoutes(h hostConf) []routeConf {
	rs := h.Routes
	if len(rs) == 0 {
		rs = []routeConf{{Tunnel: h.Tunnel, Addr: h.Addr}}
	}
	out := make([]routeConf, len(rs))
	for i, r := range rs {
		if r.Label == "" {
			if r.Tunnel != "" {
				r.Label = r.Tunnel
			} else {
				r.Label = "直连"
			}
		}
		out[i] = r
	}
	return out
}

type fileConf struct {
	KnownHosts     string            `json:"known_hosts"`
	AllowedOrigins []string          `json:"allowed_origins"`
	Socks          map[string]string `json:"socks"`
	Hosts          []hostConf        `json:"hosts"`
	Support        *supportConf      `json:"support"`
	Projsync       *projsyncConf     `json:"projsync"`
	Forwards       []forwardConf     `json:"forwards"`
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
		if len(h.Routes) > 0 {
			if h.Tunnel != "" || h.Addr != "" {
				return fmt.Errorf("hosts.json: 主机 %q 的 routes 与 tunnel/addr 只能写一种", h.Name)
			}
			for i, r := range h.Routes {
				if _, _, err := net.SplitHostPort(r.Addr); err != nil {
					return fmt.Errorf("hosts.json: 主机 %q 第 %d 条线路的地址 %q 要写成 主机:端口", h.Name, i+1, r.Addr)
				}
				if r.Tunnel != "" && cfgs[r.Tunnel] == nil {
					return fmt.Errorf("hosts.json: 主机 %q 第 %d 条线路引用了不存在的隧道 %q", h.Name, i+1, r.Tunnel)
				}
			}
			continue
		}
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
	fwdSeen := map[string]bool{}
	for _, fw := range conf.Forwards {
		if fwdSeen[fw.Name] {
			return fmt.Errorf("hosts.json: 转发名 %q 重复", fw.Name)
		}
		fwdSeen[fw.Name] = true
		if err := fw.validate(cfgs, seen); err != nil {
			return err
		}
	}
	prep, err := prepareSupport(conf, dir)
	if err != nil {
		return err
	}
	pjPrep, err := prepareProjsync(conf)
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

	// 端口转发（可选）：本机固定端口，进来的拨到当前线路的远端地址；监听失败算启动失败
	forwardMap := map[string]*forwardSvc{}
	forwardOrder := []string{}
	for _, fw := range conf.Forwards {
		fwd := newForward(fw, tunnels)
		if err := fwd.start(); err != nil {
			return fmt.Errorf("转发 %q 开监听失败: %w", fw.Name, err)
		}
		closers = append(closers, fwd)
		forwardMap[fw.Name] = fwd
		forwardOrder = append(forwardOrder, fw.Name)
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
		var ds []sshterm.Dialer
		for _, r := range hostRoutes(h) {
			var d sshterm.Dialer = &net.Dialer{Timeout: dialTimeout}
			if r.Tunnel != "" {
				d = tunnels[r.Tunnel]
			}
			ds = append(ds, d)
		}
		pool.add(h, ds)
	}

	// 项目同步（可选）：给目标主机开一条本机线路转发口（git 走它、跟随当前线路），
	// 启动时开一次，退出时一起关（照协助多线路对 fwd 的处理）
	var projV2 *projsyncV2Svc
	if pjPrep != nil {
		pjFwd, err := routeForwarder(pool, pjPrep.host)
		if err != nil {
			return fmt.Errorf("projsync: 开线路转发口失败: %w", err)
		}
		closers = append(closers, pjFwd)
		projSvc := newProjsyncSvc(pjPrep, pool, pjFwd.Addr().String())
		// v2：定时调度（缺省关闭）+ 手动接口；退出时取消并等当前一轮收尾
		projV2 = newProjsyncV2Svc(projSvc, pool)
		v2ctx, v2cancel := context.WithCancel(ctx)
		v2done := make(chan struct{})
		go func() { defer close(v2done); projV2.loop(v2ctx) }()
		closers = append(closers, closerFunc(func() { v2cancel(); <-v2done }))
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
	// （projV2 必须在这之前建好：协助的 ProjSync 要直接用它）
	var supportLocal, supportHook http.Handler
	var autostart *autostarter // 未启用自动开时为 nil（markManual 对 nil 安全）
	if prep != nil {
		svc, fwd, err := startSupport(prep, dir, expand(conf.KnownHosts), ln.Addr().String(), socksAddrs, pool, projV2)
		if err != nil {
			return err
		}
		closers = append(closers, closerFunc(svc.Close))
		if fwd != nil {
			closers = append(closers, fwd)
		}
		supportLocal, supportHook = svc.LocalHandler(), svc.HookHandler()
		if prep.conf.autostartOn() {
			autostart = startAutostart(svc, autostartRetry, prep.conf.AutostartMinutes)
			closers = append(closers, autostart) // 在 svc.Close 之前关（closers 逆序执行）
		}
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
			case "/support/start", "/support/stop", "/support/decide", "/msg/decide", "/msg/auto", "/approval/decide":
				if o := r.Header.Get("Origin"); o == "" || !originAllowed(o, conf.AllowedOrigins) {
					errJSON(w, http.StatusForbidden, "需要从 xxyi 页面操作")
					return
				}
				if r.URL.Path == "/support/start" || r.URL.Path == "/support/stop" {
					autostart.markManual() // 用户手动开/关过，本次运行不再自动开
				}
			}
		}
		supportLocal.ServeHTTP(w, r)
	}
	mux.HandleFunc("/support", supportRoute)
	mux.HandleFunc("/support/", supportRoute)
	mux.HandleFunc("/msg", supportRoute)
	mux.HandleFunc("/msg/", supportRoute)
	// 通用审批（任意会话的 PermissionRequest）：同样整段转给 support；decide 的 Origin 校验在上面
	mux.HandleFunc("/approval", supportRoute)
	mux.HandleFunc("/approval/", supportRoute)
	type hostItem struct {
		Name      string `json:"name"`
		Tunnel    string `json:"tunnel"`
		Connected bool   `json:"connected"`
		Error     string `json:"error"`
		routeState
	}
	itemOf := func(h hostConf) hostItem {
		connected, lastErr := pool.info(h.Name)
		rs := pool.routeInfo(h.Name)
		tunnel := h.Tunnel
		if rs.Active >= 0 && rs.Active < len(rs.Routes) {
			tunnel = rs.Routes[rs.Active].Tunnel // 当前线路的隧道
		}
		return hostItem{h.Name, tunnel, connected, lastErr, rs}
	}
	mux.HandleFunc("/hosts", func(w http.ResponseWriter, r *http.Request) {
		list := []hostItem{}
		for _, h := range conf.Hosts {
			list = append(list, itemOf(h))
		}
		writeJSON(w, list)
	})
	// 切换线路：用户在页面上同意后才切；和协助的开始/停止一样，只许从 xxyi 页面发起
	mux.HandleFunc("/hosts/route", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			errJSON(w, http.StatusMethodNotAllowed, "请用 POST")
			return
		}
		if o := r.Header.Get("Origin"); o == "" || !originAllowed(o, conf.AllowedOrigins) {
			errJSON(w, http.StatusForbidden, "需要从 xxyi 页面操作")
			return
		}
		h, ok := hostOf(w, r, hosts)
		if !ok {
			return
		}
		idx, err := strconv.Atoi(r.URL.Query().Get("route"))
		if err != nil {
			errJSON(w, http.StatusBadRequest, "route 要是整数")
			return
		}
		if err := pool.switchRoute(h.Name, idx); err != nil {
			var re *routeError
			if errors.As(err, &re) && re.notFound {
				errJSON(w, http.StatusNotFound, "未知主机")
			} else {
				errJSON(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		writeJSON(w, itemOf(h))
	})

	// 端口转发：每项字段与 /hosts 一致（外加 listen），网页按同一种线路条渲染；切换同样只许从 xxyi 页面发起
	mux.HandleFunc("/forwards", func(w http.ResponseWriter, r *http.Request) {
		list := []forwardItem{}
		for _, n := range forwardOrder {
			list = append(list, forwardMap[n].item())
		}
		writeJSON(w, list)
	})
	mux.HandleFunc("/forwards/route", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			errJSON(w, http.StatusMethodNotAllowed, "请用 POST")
			return
		}
		if o := r.Header.Get("Origin"); o == "" || !originAllowed(o, conf.AllowedOrigins) {
			errJSON(w, http.StatusForbidden, "需要从 xxyi 页面操作")
			return
		}
		fwd, ok := forwardMap[r.URL.Query().Get("name")]
		if !ok {
			errJSON(w, http.StatusNotFound, "未知转发")
			return
		}
		idx, err := strconv.Atoi(r.URL.Query().Get("route"))
		if err != nil {
			errJSON(w, http.StatusBadRequest, "route 要是整数")
			return
		}
		if err := fwd.switchRoute(idx); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, fwd.item())
	})

	// 项目同步 v2（可选）：/projsync/v2*；没配置一律 404（v1 的 /projsync、/projsync/run 已移除）
	if projV2 != nil {
		h2 := projV2.localHandler(ctx, conf.AllowedOrigins)
		mux.Handle("/projsync/v2", h2)
		mux.Handle("/projsync/v2/run", h2)
		mux.Handle("/projsync/v2/commit", h2)
	} else {
		noProj := func(w http.ResponseWriter, r *http.Request) {
			errJSON(w, http.StatusNotFound, "没有配置项目同步")
		}
		mux.HandleFunc("/projsync/v2", noProj)
		mux.HandleFunc("/projsync/v2/run", noProj)
		mux.HandleFunc("/projsync/v2/commit", noProj)
	}

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
