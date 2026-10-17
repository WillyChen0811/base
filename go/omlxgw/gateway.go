package omlxgw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Gateway 是 oMLX 网关。
type Gateway struct {
	path   string // 配置文件路径，PUT /gw/config 写回
	sched  *scheduler
	proxy  *httputil.ReverseProxy
	logger *log.Logger
	listen string
	up     string
}

// New 用已校验的配置创建网关。cfgPath 为空则 PUT /gw/config 只改内存。logw 为日志输出（nil 用 stderr）。
func New(cfg *Config, cfgPath string, logw io.Writer) (*Gateway, error) {
	target, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	if logw == nil {
		logw = os.Stderr
	}
	g := &Gateway{path: cfgPath, sched: newScheduler(cfg), logger: log.New(logw, "", 0), listen: cfg.Listen, up: cfg.Upstream}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
		FlushInterval: -1,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			DisableCompression:  true,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     60 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				if sw, ok := w.(*statusWriter); ok {
					sw.status = 499
				}
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "上游错误: " + err.Error()})
		},
	}
	return g, nil
}

// Handler 返回 HTTP 处理器。
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/gw/status", g.handleStatus)
	mux.HandleFunc("/gw/config", g.handleConfig)
	mux.HandleFunc("/gw/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "未知管理接口"})
	})
	mux.HandleFunc("/", g.handleProxy)
	return mux
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	var body []byte
	model := ""
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败: " + err.Error()})
			return
		}
		var m struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &m) == nil {
			model = m.Model
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	sw := &statusWriter{ResponseWriter: w}
	if model == "" { // 不带模型的请求不排队
		g.proxy.ServeHTTP(sw, r)
		return
	}
	cfg := g.sched.config()
	prio := cfg.DefaultPriority
	if v := r.Header.Get("X-Xxyi-Priority"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= 9 {
			prio = n
		}
	}
	client := r.Header.Get("X-Xxyi-Client")
	t0 := time.Now()
	e, release, err := g.sched.acquire(r.Context(), model, prio, client)
	if err != nil {
		g.logReq(model, prio, client, time.Since(t0), 0, 499)
		return
	}
	t1 := time.Now()
	defer func() {
		release()
		g.logReq(model, prio, client, t1.Sub(t0), time.Since(t1), sw.status)
	}()
	_ = e
	g.proxy.ServeHTTP(sw, r)
}

func (g *Gateway) logReq(model string, prio int, client string, wait, run time.Duration, status int) {
	if client == "" {
		client = "-"
	}
	g.logger.Printf("%s model=%s prio=%d client=%s wait=%.1fs run=%.1fs status=%d",
		time.Now().Format("2006-01-02 15:04:05"), model, prio, strings.ReplaceAll(client, " ", "_"), wait.Seconds(), run.Seconds(), status)
}

func (g *Gateway) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	run, q := g.sched.status()
	writeJSON(w, 200, map[string]any{"running": run, "queued": q})
}

func (g *Gateway) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, g.sched.config())
	case http.MethodPut:
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		cfg, err := ParseConfig(b)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if cfg.Listen != g.listen || cfg.Upstream != g.up {
			writeJSON(w, 400, map[string]string{"error": "listen、upstream 不允许修改"})
			return
		}
		if g.path != "" {
			out, _ := json.MarshalIndent(cfg, "", "  ")
			if err := writeFileAtomic(g.path, append(out, '\n')); err != nil {
				writeJSON(w, 500, map[string]string{"error": "写配置文件失败: " + err.Error()})
				return
			}
		}
		g.sched.setConfig(cfg)
		writeJSON(w, 200, cfg)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET、PUT"})
	}
}

// Run 读配置、监听并服务，直到 ctx 取消。
func Run(ctx context.Context, cfgPath string, logw io.Writer) error {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("配置: %w", err)
	}
	g, err := New(cfg, cfgPath, logw)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sc)
		srv.Close()
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
