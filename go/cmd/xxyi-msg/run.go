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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 超时与上限，测试里可调小。
var (
	httpTimeout   = 10 * time.Second       // send/who/inbox 的 HTTP 超时
	stateDeadline = 2 * time.Second        // state 的总耗时上限（含 HTTP 客户端超时）：钩子在等，超了直接放弃
	tmuxTimeout   = 2 * time.Second        // 取 tmux 会话名的超时
	stopHookWait  = 800 * time.Millisecond // state --check 等标准输入里钩子 JSON 的上限，留出时间把状态报出去
)

// 消息类型，与服务端 support 的 Kind* 同一套字面量（命令行不接受 reply，回复走 reply 子命令）。
const (
	kindRequest = "request"
	kindReply   = "reply"
	kindNotice  = "notice"
	kindCode    = "code"
)

// mcpInstructions 是 MCP initialize 里给 Claude 看的规则文字。
const mcpInstructions = "XXYI 消息通道：和另一台机器上的 Claude 通信只能用这些工具。规则：" +
	"1) 收到「请求」必须用 xxyi_reply 回复，做不了也要回复说明原因；" +
	"2) 只是告知用 kind=notice；要对方做事用 kind=request；交代码用 kind=code 并带 repo 和 branch（代码由 XXYI 自动同步，不要贴补丁）；" +
	"3) 涉及需要用户决定的事，设 ask_user=true，并且不要自己拍板；" +
	"4) 发之前可用 xxyi_peers 看对方状态，busy 时消息会排队等它空闲；" +
	"5) 消息正文要自包含：写清背景、要做什么、做完怎么回复。"

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
// 在 tmux 里时指定 $TMUX_PANE：同一服务器上有别的客户端/会话也不取错。
var tmuxSession = func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	args := []string{"display-message", "-p", "#S"}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		args = append(args, "-t", pane)
	}
	out, err := exec.CommandContext(ctx, "tmux", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

const usage = "用法：xxyi-msg send <收件人> [文字|-] [--file 路径] [--repo 名 --branch 名] [--kind request|notice|code] [--ask-user]" +
	" | xxyi-msg reply <消息id> [文字|-] [--file 路径]" +
	" | xxyi-msg inbox [n] | xxyi-msg who" +
	" | xxyi-msg state busy|idle|gone [--check] | xxyi-msg mcp"

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

// addrNameIllegal 与服务端 support.ValidAddr 同一套字符集：名字只许 ASCII 字母数字和 . _ -。
var addrNameIllegal = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// selfAddr 定自己的地址：XXYI_ADDR 优先，其次 tmux 会话名（ubuntu: 一侧）。
// 会话名先按服务端规则校验：不合法现在就说清，别让 send 到服务端才碰一脑袋 400。
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
		if len(name) > 64 || addrNameIllegal.FindString(name) != "" {
			return "", fmt.Errorf("tmux 会话名 %q 含不支持的字符，只允许字母数字 . _ -，请改名（tmux rename-session）或设 XXYI_ADDR", name)
		}
		return "ubuntu:" + name, nil
	}
	return "", errNoSelfAddr
}

// sendOpts 是 send 的可选参数；kind 为空按 notice 处理。
type sendOpts struct {
	file    string
	repo    string
	branch  string
	kind    string
	askUser bool
}

// parseSendArgs 允许选项写在位置参数前后；text 为空串表示要从标准输入读。
// 单独的 "--" 终止选项解析：之后的都以 "--" 开头的文字（如 "--help me"）按位置参数收。
func parseSendArgs(args []string) (to, text string, o sendOpts, err error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if a == "--ask-user" {
			o.askUser = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			var dst *string
			switch a {
			case "--file":
				dst = &o.file
			case "--repo":
				dst = &o.repo
			case "--branch":
				dst = &o.branch
			case "--kind":
				dst = &o.kind
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
	if err := checkRepoBranch(o.repo, o.branch); err != nil {
		return "", "", o, err
	}
	if err := checkSendKind(o.kind); err != nil {
		return "", "", o, err
	}
	if err := checkCodeRepoBranch(o.kind, o.repo, o.branch); err != nil {
		return "", "", o, err
	}
	return to, text, o, nil
}

// checkRepoBranch：repo 与 branch 要么都给要么都不给。
func checkRepoBranch(repo, branch string) error {
	if (repo == "") != (branch == "") {
		return errors.New("--repo 和 --branch 必须一起给")
	}
	return nil
}

// checkSendKind 校验 send 的消息类型；空按 notice，reply 得用 reply 子命令。
func checkSendKind(kind string) error {
	switch kind {
	case "", kindNotice, kindRequest, kindCode:
		return nil
	case kindReply:
		return errors.New("--kind 不能是 reply，回复请用 xxyi-msg reply <消息id> \"…\"")
	}
	return fmt.Errorf("--kind 只能是 request/notice/code，实际是 %q", kind)
}

// checkCodeRepoBranch：交代码必须说清是哪份代码（命令行与 MCP 共用）。
func checkCodeRepoBranch(kind, repo, branch string) error {
	if kind == kindCode && (repo == "" || branch == "") {
		return errors.New("交代码（--kind code）必须带 --repo 和 --branch")
	}
	return nil
}

// sendBody 是 POST /msg/send 的正文（File 由 encoding/json 自动 base64）。
// 回复的 To 留空，由服务端填成原请求的发件人。
type sendBody struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Text     string `json:"text"`
	FileName string `json:"file_name,omitempty"`
	File     []byte `json:"file,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Kind     string `json:"kind,omitempty"`
	ReplyTo  string `json:"reply_to,omitempty"`
	AskUser  bool   `json:"ask_user,omitempty"`
}

// inboxMessage 是 /msg/inbox 里我们关心的那几个字段（服务端 Message 的字段远不止这些）。
type inboxMessage struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Text    string `json:"text"`
	State   string `json:"state"`
	Kind    string `json:"kind"`
	ReplyTo string `json:"reply_to,omitempty"`
	AskUser bool   `json:"ask_user,omitempty"`
}

// inboxResp 是 GET /msg/inbox 的响应。
type inboxResp struct {
	Messages   []inboxMessage `json:"messages"`
	Unanswered []inboxMessage `json:"unanswered"`
}

// stateResp 是 POST /msg/state?check=1 的响应；unanswered 非空表示这次状态上报被按住。
type stateResp struct {
	OK         bool           `json:"ok"`
	Unanswered []inboxMessage `json:"unanswered"`
}

// firstLine 取正文第一行，界面摘要用。
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// formatInbox 把收件箱渲染成终端文本：先欠回复的请求，再最近消息（服务端已按新到旧排）。
func formatInbox(in inboxResp) string {
	var b strings.Builder
	for _, m := range in.Unanswered {
		fmt.Fprintf(&b, "未回复 #%s 来自 %s：%s\n", m.ID, m.From, firstLine(m.Text))
	}
	for _, m := range in.Messages {
		fmt.Fprintf(&b, "#%s %s %s → %s %s：%s\n", m.ID, m.Kind, m.From, m.To, m.State, firstLine(m.Text))
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatPeers 渲染忙闲表：地址排序，状态原样（busy/idle/gone）。
func formatPeers(peers map[string]string) string {
	addrs := make([]string, 0, len(peers))
	for a := range peers {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	lines := make([]string, 0, len(addrs))
	for _, a := range addrs {
		lines = append(lines, a+" "+peers[a])
	}
	return strings.Join(lines, "\n")
}

// loadAttach 读附件并限长，返回文件名和内容；path 为空表示没有附件。
func loadAttach(path string) (string, []byte, error) {
	if path == "" {
		return "", nil, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", nil, fmt.Errorf("附件读不了：%w", err)
	}
	if st.Size() > maxAttachBytes {
		return "", nil, fmt.Errorf("附件超过 1MB：%s（%d 字节）", path, st.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("附件读不了：%w", err)
	}
	return filepath.Base(path), data, nil
}

// doSend 发到 /msg/send，返回新消息 id；命令行和 MCP 共用。
func doSend(ctx context.Context, body sendBody) (string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	c, err := connect(ctx, httpTimeout)
	if err != nil {
		return "", err
	}
	b, err := c.do(ctx, http.MethodPost, c.path("/msg/send", nil), bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	var r struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &r) != nil || r.ID == "" {
		return "", errors.New("服务端响应里没有消息 id")
	}
	return r.ID, nil
}

// doPeers 取对端忙闲表：Ubuntu 一侧用 /msg/who，Mac 一侧的表在 /msg 的 peers 里。
func doPeers(ctx context.Context) (map[string]string, error) {
	c, err := connect(ctx, httpTimeout)
	if err != nil {
		return nil, err
	}
	p := "/msg/who"
	if c.query {
		p = "/msg"
	}
	b, err := c.do(ctx, http.MethodGet, c.path(p, nil), nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Peers map[string]string `json:"peers"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("服务端响应格式不对：%w", err)
	}
	return r.Peers, nil
}

// doInbox 读收件箱（最近 n 条 + 欠回复的请求）；命令行和 MCP 共用。
func doInbox(ctx context.Context, addr string, n int) (inboxResp, error) {
	var in inboxResp
	c, err := connect(ctx, httpTimeout)
	if err != nil {
		return in, err
	}
	q := url.Values{"addr": {addr}}
	if n > 0 {
		q.Set("n", strconv.Itoa(n))
	}
	b, err := c.do(ctx, http.MethodGet, c.path("/msg/inbox", q), nil)
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, fmt.Errorf("服务端响应格式不对：%w", err)
	}
	return in, nil
}

// inboxText 是收件箱的展示文本；命令行与 MCP 用同一份，保证两边看到的一样。
func inboxText(ctx context.Context, addr string, n int) (string, error) {
	in, err := doInbox(ctx, addr, n)
	if err != nil {
		return "", err
	}
	text := formatInbox(in)
	if text == "" {
		text = "收件箱是空的"
	}
	return text, nil
}

// checkInboxN 校验条数：服务端只收 1..100，本地先说清。
func checkInboxN(n int) error {
	if n < 1 || n > inboxMaxN {
		return fmt.Errorf("条数 n 必须 1..%d，实际 %d", inboxMaxN, n)
	}
	return nil
}

// 收件箱条数：默认 20，上限 100（与服务端 msgapi.go 一致）。
const (
	inboxDefaultN = 20
	inboxMaxN     = 100
)

// sendFail：连不上原样报，其它（含服务端 error 字段）加「发送失败」前缀。
func sendFail(err error) error {
	var ce *connError
	if errors.As(err, &ce) {
		return err
	}
	return fmt.Errorf("发送失败：%w", err)
}

// readText：文字为空或 "-" 时从标准输入读；读完必须非空。send 和 reply 共用。
func readText(stdin io.Reader, text string) (string, error) {
	if text == "" || text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("读标准输入失败：%w", err)
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("消息文字为空")
	}
	return text, nil
}

func cmdSend(args []string, stdin io.Reader, out io.Writer) error {
	to, text, o, err := parseSendArgs(args)
	if err != nil {
		return err
	}
	name, data, err := loadAttach(o.file)
	if err != nil {
		return err
	}
	text, err = readText(stdin, text)
	if err != nil {
		return err
	}
	from, err := selfAddr()
	if err != nil {
		return err
	}
	kind := o.kind
	if kind == "" {
		kind = kindNotice
	}
	body := sendBody{
		From:     from,
		To:       to,
		Text:     text,
		FileName: name,
		File:     data,
		Repo:     o.repo,
		Branch:   o.branch,
		Kind:     kind,
		AskUser:  o.askUser,
	}
	id, err := doSend(context.Background(), body)
	if err != nil {
		return sendFail(err)
	}
	fmt.Fprintf(out, "已排队 %s\n", id)
	return nil
}

// parseReplyArgs 解析 reply 的选项与位置参数；text 为空串表示从标准输入读。
func parseReplyArgs(args []string) (id, text, file string, err error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			if a != "--file" {
				return "", "", "", fmt.Errorf("未知选项 %s", a)
			}
			if i+1 >= len(args) {
				return "", "", "", errors.New("--file 后面要跟值")
			}
			file = args[i+1]
			i++
			continue
		}
		pos = append(pos, a)
	}
	switch len(pos) {
	case 0:
		return "", "", "", errors.New("缺少消息 id（形如 xxyi-msg reply <消息id> \"已改好\"）")
	case 1:
		id = pos[0]
	case 2:
		id, text = pos[0], pos[1]
	default:
		return "", "", "", fmt.Errorf("参数过多：%v", pos)
	}
	return id, text, file, nil
}

// cmdReply 回复某条请求：收件人由服务端按 reply_to 回填，这里不带 to。
func cmdReply(args []string, stdin io.Reader, out io.Writer) error {
	id, text, file, err := parseReplyArgs(args)
	if err != nil {
		return err
	}
	name, data, err := loadAttach(file)
	if err != nil {
		return err
	}
	text, err = readText(stdin, text)
	if err != nil {
		return err
	}
	from, err := selfAddr()
	if err != nil {
		return err
	}
	body := sendBody{
		From:     from,
		Text:     text,
		FileName: name,
		File:     data,
		Kind:     kindReply,
		ReplyTo:  id,
	}
	newID, err := doSend(context.Background(), body)
	if err != nil {
		return sendFail(err)
	}
	fmt.Fprintf(out, "已排队 %s\n", newID)
	return nil
}

// cmdInbox 打印收件箱：先欠回复的请求，再最近消息。
func cmdInbox(args []string, out io.Writer) error {
	n := inboxDefaultN
	switch len(args) {
	case 0:
	case 1:
		v, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("条数必须是整数：%s", args[0])
		}
		n = v
	default:
		return fmt.Errorf("参数过多：%v", args)
	}
	if err := checkInboxN(n); err != nil {
		return err
	}
	addr, err := selfAddr()
	if err != nil {
		return err
	}
	text, err := inboxText(context.Background(), addr, n)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, text)
	return nil
}

func cmdWho(out io.Writer) error {
	peers, err := doPeers(context.Background())
	if err != nil {
		return err
	}
	text := formatPeers(peers)
	if text == "" {
		text = "还没有对端上报状态"
	}
	fmt.Fprintln(out, text)
	return nil
}

// 忙闲状态字面量，与服务端 support.PeerState 一致。
const (
	stateBusy = "busy"
	stateIdle = "idle"
	stateGone = "gone"
)

// stopHook 是从钩子标准输入里读到的东西。
type stopHook struct {
	active bool
	ok     bool // 有没有读到并解析成功
}

// stopHookActive 读钩子塞进来的 JSON，取 stop_hook_active。
// 读放在 goroutine 里，跟 stateDeadline（再收窄到 stopHookWait）一起 select：
// 钩子不关标准输入也拖不住这条命令，超时就当没读到（ok=false），照常把状态报出去。
func stopHookActive(ctx context.Context, stdin io.Reader) (active bool, ok bool) {
	readCtx, cancel := context.WithTimeout(ctx, stopHookWait)
	defer cancel()
	ch := make(chan stopHook, 1)
	go func() {
		var h struct {
			StopHookActive bool `json:"stop_hook_active"`
		}
		err := json.NewDecoder(stdin).Decode(&h)
		ch <- stopHook{active: h.StopHookActive, ok: err == nil}
	}()
	select {
	case got := <-ch:
		return got.active, got.ok
	case <-readCtx.Done():
		return false, false
	}
}

// blockJSON 是 stop hook 的拒绝响应（一行 JSON）：reason 里逐条列出欠回复的请求。
func blockJSON(un []inboxMessage) string {
	items := make([]string, 0, len(un))
	for _, m := range un {
		// 服务端已按第一行截过，这里再截一次兜底：reason 是一行 JSON，塞整段正文没意义。
		items = append(items, fmt.Sprintf("#%s（来自 %s：%s）", m.ID, m.From, firstLine(m.Text)))
	}
	reason := "你还有没回复的 XXYI 请求：" + strings.Join(items, "；") +
		"。请先用 xxyi_reply（或 xxyi-msg reply <id> \"…\"）逐条回复，做不了也要回复说明原因。"
	raw, err := json.Marshal(map[string]string{"decision": "block", "reason": reason})
	if err != nil {
		return ""
	}
	return string(raw)
}

// cmdState 给 Claude Code 钩子用：任何错误都静默退出 0，总耗时不超过 stateDeadline。
// 只有 "idle --check" 才读标准输入（拿 stop_hook_active 决定要不要让服务端查欠回复）；
// 其余情况绝不碰标准输入，免得等 EOF 卡住钩子。
func cmdState(args []string, stdin io.Reader, stdout io.Writer) int {
	state := ""
	check := false
	for _, a := range args {
		switch a {
		case "--check":
			check = true
		case stateBusy, stateIdle, stateGone:
			if state != "" {
				return exitOK // 给了两个状态值：当参数不合法处理
			}
			state = a
		default:
			return exitOK
		}
	}
	if state == "" {
		return exitOK
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateDeadline)
	defer cancel()
	from, err := selfAddr()
	if err != nil {
		return exitOK
	}
	q := url.Values{"addr": {from}, "state": {state}}
	if state == stateIdle && check {
		if active, ok := stopHookActive(ctx, stdin); ok && !active {
			q.Set("check", "1")
		}
	}
	c, err := connect(ctx, stateDeadline)
	if err != nil {
		return exitOK
	}
	b, err := c.do(ctx, http.MethodPost, c.path("/msg/state", q), nil)
	if err != nil {
		return exitOK
	}
	if q.Get("check") != "1" {
		return exitOK // 没要求自查：服务端回什么都不拦 Claude 停下
	}
	var resp stateResp
	if json.Unmarshal(b, &resp) != nil || len(resp.Unanswered) == 0 {
		return exitOK
	}
	if line := blockJSON(resp.Unanswered); line != "" {
		fmt.Fprintln(stdout, line)
	}
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
	case "reply":
		err = cmdReply(args[1:], stdin, stdout)
	case "inbox":
		err = cmdInbox(args[1:], stdout)
	case "who":
		err = cmdWho(stdout)
	case "state":
		return cmdState(args[1:], stdin, stdout)
	case "mcp":
		err = serveMCP(stdin, stdout)
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
