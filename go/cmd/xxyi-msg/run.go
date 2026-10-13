package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 超时与上限，测试里可调小。
var (
	httpTimeout   = 10 * time.Second // send/who 的 HTTP 超时
	stateDeadline = 2 * time.Second  // state 的总耗时上限（含 HTTP 客户端超时）：钩子在等，超了直接放弃
	tmuxTimeout   = 2 * time.Second  // 取 tmux 会话名的超时
)

// 退出码与上限。
const (
	exitOK         = 0
	exitErr        = 2
	maxAttachBytes = 1 << 20 // 附件上限 1MB
)

// defaultXxyiPort 是 xxyi 页面服务端的缺省端口（config.json 没写 http_port 时用）。
const defaultXxyiPort = 8765

// 配置文件路径做成包级变量，测试指到 t.TempDir。
var (
	endpointPath   = defaultEndpointPath()
	xxyiConfigPath = defaultXxyiConfigPath()
)

// tmuxSession 取当前 tmux 会话名；测试里替换，绝不真执行 tmux。
var tmuxSession = func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

const usage = "用法：xxyi-msg send <收件人> [文字|-] [--file 路径] [--repo 名 --branch 名] | xxyi-msg who | xxyi-msg state busy|idle"

// errNoSelfAddr 是 send 找不到自己地址时的提示。
var errNoSelfAddr = errors.New("不知道自己的地址：请在 XXYI 的终端里运行，或设 XXYI_ADDR")

// connError 表示连不上服务；调用方原样打印（提示里已带「协助开着吗」），不加「发送失败」前缀。
type connError struct{ err error }

func (e *connError) Error() string {
	return "连不上 XXYI：" + e.err.Error() + "（协助开着吗？）"
}

func (e *connError) Unwrap() error { return e.err }

type endpoint struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	Until int64  `json:"until"` // 协助截止时间（Unix 秒）；0 或缺省表示未知
}

// client 到消息服务端的连接；query 为 true 时令牌放查询参数（Mac 侧），否则放 Authorization 头（Ubuntu 侧）。
type client struct {
	base  string
	token string
	query bool
	hc    *http.Client
}

// path 拼请求路径；Mac 侧把令牌追加为查询参数。
func (c *client) path(p string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	if c.query && c.token != "" {
		q.Set("token", c.token)
	}
	if enc := q.Encode(); enc != "" {
		return p + "?" + enc
	}
	return p
}

// do 发请求；连接失败包装成 connError，非 200 取响应 JSON 的 error 字段。
func (c *client) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if !c.query && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &connError{err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s（HTTP %d）", e.Error, resp.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return b, nil
}

func defaultEndpointPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi-support", "endpoint.json")
}

func defaultXxyiConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi", "config.json")
}

// connect 按配置文件选侧：Ubuntu 一侧有 endpoint.json 就用它；否则走 Mac 的 xxyi 本机接口拿端口和令牌。
func connect(ctx context.Context, timeout time.Duration) (*client, error) {
	raw, err := os.ReadFile(endpointPath)
	if errors.Is(err, fs.ErrNotExist) {
		return connectMac(ctx, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("端点文件读不了：%s：%w", endpointPath, err)
	}
	var ep endpoint
	if json.Unmarshal(raw, &ep) != nil || ep.Addr == "" {
		return nil, fmt.Errorf("端点文件格式不对：%s", endpointPath)
	}
	if ep.Until > 0 && ep.Until < time.Now().Unix() {
		return nil, fmt.Errorf("协助已到期（%s 里的 until）", endpointPath)
	}
	return &client{base: "http://" + ep.Addr, token: ep.Token, hc: &http.Client{Timeout: timeout}}, nil
}

// connectMac 读 ~/.xxyi/config.json 的 http_port，再从 /api/terminal 拿消息入口地址和令牌。
func connectMac(ctx context.Context, timeout time.Duration) (*client, error) {
	port, err := xxyiPort()
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: timeout}
	b, err := (&client{base: "http://127.0.0.1:" + port, hc: hc}).do(ctx, http.MethodGet, "/api/terminal", nil)
	if err != nil {
		return nil, err
	}
	var t struct {
		HTTP  string `json:"http"`
		Token string `json:"token"`
	}
	if json.Unmarshal(b, &t) != nil || t.HTTP == "" {
		return nil, fmt.Errorf("xxyi /api/terminal 返回格式不对")
	}
	return &client{base: "http://" + t.HTTP, token: t.Token, query: true, hc: hc}, nil
}

// xxyiPort 读 config.json 的 http_port（数字或字符串都可能），缺省 8765。
func xxyiPort() (string, error) {
	raw, err := os.ReadFile(xxyiConfigPath)
	if errors.Is(err, fs.ErrNotExist) || len(bytes.TrimSpace(raw)) == 0 {
		return strconv.Itoa(defaultXxyiPort), nil
	}
	if err != nil {
		return "", fmt.Errorf("配置文件读不了：%s：%w", xxyiConfigPath, err)
	}
	var cfg struct {
		HTTPPort any `json:"http_port"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("配置文件格式不对：%s", xxyiConfigPath)
	}
	port := ""
	switch v := cfg.HTTPPort.(type) {
	case float64:
		if v > 0 {
			port = strconv.Itoa(int(v))
		}
	case string:
		port = strings.TrimSpace(v)
	}
	if port == "" {
		return strconv.Itoa(defaultXxyiPort), nil
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%s 的 http_port 不是合法端口：%v", xxyiConfigPath, cfg.HTTPPort)
	}
	return port, nil
}

// selfAddr 定自己的地址：XXYI_ADDR 优先，其次 tmux 会话名（ubuntu: 一侧）。
func selfAddr() (string, error) {
	if a := strings.TrimSpace(os.Getenv("XXYI_ADDR")); a != "" {
		return a, nil
	}
	if os.Getenv("TMUX") != "" {
		name, err := tmuxSession()
		if err != nil {
			return "", fmt.Errorf("取 tmux 会话名失败：%w", err)
		}
		if name == "" {
			return "", errors.New("tmux 会话名为空")
		}
		return "ubuntu:" + name, nil
	}
	return "", errNoSelfAddr
}

// sendOpts 是 send 的可选参数。
type sendOpts struct {
	file   string
	repo   string
	branch string
}

// parseSendArgs 允许选项写在位置参数前后；text 为空串表示要从标准输入读。
func parseSendArgs(args []string) (to, text string, o sendOpts, err error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			var dst *string
			switch a {
			case "--file":
				dst = &o.file
			case "--repo":
				dst = &o.repo
			case "--branch":
				dst = &o.branch
			default:
				return "", "", o, fmt.Errorf("未知选项 %s", a)
			}
			if i+1 >= len(args) {
				return "", "", o, fmt.Errorf("%s 后面要跟值", a)
			}
			*dst = args[i+1]
			i++
			continue
		}
		pos = append(pos, a)
	}
	switch len(pos) {
	case 0:
		return "", "", o, errors.New("缺少收件人（形如 mac:<名> 或 ubuntu:<名>）")
	case 1:
		to = pos[0]
	case 2:
		to, text = pos[0], pos[1]
	default:
		return "", "", o, fmt.Errorf("参数过多：%v", pos)
	}
	host, name, ok := strings.Cut(to, ":")
	if !ok || (host != "mac" && host != "ubuntu") || name == "" {
		return "", "", o, fmt.Errorf("收件人 %q 必须形如 mac:<名> 或 ubuntu:<名>", to)
	}
	if (o.repo == "") != (o.branch == "") {
		return "", "", o, errors.New("--repo 和 --branch 必须一起给")
	}
	return to, text, o, nil
}

// sendBody 是 POST /msg/send 的正文（File 由 encoding/json 自动 base64）。
type sendBody struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Text     string `json:"text"`
	FileName string `json:"file_name,omitempty"`
	File     []byte `json:"file,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

// sendFail：连不上原样报，其它（含服务端 error 字段）加「发送失败」前缀。
func sendFail(err error) error {
	var ce *connError
	if errors.As(err, &ce) {
		return err
	}
	return fmt.Errorf("发送失败：%w", err)
}

func cmdSend(args []string, stdin io.Reader, out io.Writer) error {
	to, text, o, err := parseSendArgs(args)
	if err != nil {
		return err
	}
	if o.file != "" {
		st, err := os.Stat(o.file)
		if err != nil {
			return fmt.Errorf("附件读不了：%w", err)
		}
		if st.Size() > maxAttachBytes {
			return fmt.Errorf("附件超过 1MB：%s（%d 字节）", o.file, st.Size())
		}
	}
	if text == "" || text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("读标准输入失败：%w", err)
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("消息文字为空")
	}
	from, err := selfAddr()
	if err != nil {
		return err
	}
	body := sendBody{From: from, To: to, Text: text, Repo: o.repo, Branch: o.branch}
	if o.file != "" {
		data, err := os.ReadFile(o.file)
		if err != nil {
			return fmt.Errorf("附件读不了：%w", err)
		}
		body.FileName = filepath.Base(o.file)
		body.File = data
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx := context.Background()
	c, err := connect(ctx, httpTimeout)
	if err != nil {
		return err
	}
	b, err := c.do(ctx, http.MethodPost, c.path("/msg/send", nil), bytes.NewReader(raw))
	if err != nil {
		return sendFail(err)
	}
	var r struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &r) != nil || r.ID == "" {
		return errors.New("服务端响应里没有消息 id")
	}
	fmt.Fprintf(out, "已排队 %s\n", r.ID)
	return nil
}

func cmdWho(out io.Writer) error {
	ctx := context.Background()
	c, err := connect(ctx, httpTimeout)
	if err != nil {
		return err
	}
	p := "/msg/who" // Ubuntu 一侧
	if c.query {
		p = "/msg" // Mac 一侧：忙闲表在消息列表响应里的 peers
	}
	b, err := c.do(ctx, http.MethodGet, c.path(p, nil), nil)
	if err != nil {
		return err
	}
	var r struct {
		Peers map[string]string `json:"peers"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("服务端响应格式不对：%w", err)
	}
	addrs := make([]string, 0, len(r.Peers))
	for a := range r.Peers {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	for _, a := range addrs {
		st := r.Peers[a]
		if st != "busy" {
			st = "idle" // 没上报过的地址视为 idle
		}
		fmt.Fprintf(out, "%s %s\n", a, st)
	}
	return nil
}

// cmdState 给 Claude Code 钩子用：任何错误都静默退出 0，总耗时不超过 stateDeadline；
// 钩子会往标准输入塞 Claude 的 JSON，这里不读，免得等 EOF 卡住。
func cmdState(args []string) int {
	if len(args) != 1 || (args[0] != "busy" && args[0] != "idle") {
		return exitOK
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateDeadline)
	defer cancel()
	from, err := selfAddr()
	if err != nil {
		return exitOK
	}
	c, err := connect(ctx, stateDeadline)
	if err != nil {
		return exitOK
	}
	q := url.Values{"addr": {from}, "state": {args[0]}}
	_, _ = c.do(ctx, http.MethodPost, c.path("/msg/state", q), nil)
	return exitOK
}

// run 是程序主体，返回退出码。
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitErr
	}
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	var err error
	switch args[0] {
	case "send":
		err = cmdSend(args[1:], stdin, stdout)
	case "who":
		err = cmdWho(stdout)
	case "state":
		return cmdState(args[1:])
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n%s\n", args[0], usage)
		return exitErr
	}
	if err != nil {
		fmt.Fprintln(stderr, "错误："+err.Error())
		return exitErr
	}
	return exitOK
}
