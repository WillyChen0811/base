package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 轮询参数，测试里可调小。
var (
	eventsWait      = 5 // GET /events 的 wait 秒数；短一些，待批请求能及时提示
	httpTimeout     = 60 * time.Second
	retryDelay      = 2 * time.Second  // ask 轮询失败后的重试间隔
	projsyncTimeout = 40 * time.Minute // projsync 请求超时，单独设
)

// 退出码。
const (
	exitOK   = 0
	exitFail = 1 // 任务以失败结束
	exitErr  = 2 // 端点缺失或请求失败
)

type endpoint struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	Until int64  `json:"until"` // 协助截止时间（Unix 秒）；0 或缺省表示未知
}

type client struct {
	base  string
	token string
	hc    *http.Client
}

// do 发请求，非 200 时返回带中文原因的错误。
func (c *client) do(method, path string, body io.Reader, ctype string) ([]byte, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上 Mac：%w", err)
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

func (c *client) getJSON(path string, v any) error {
	b, err := c.do(http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type pendingReq struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
}

type statusResp struct {
	Active bool  `json:"active"`
	Until  int64 `json:"until"`
	Claude struct {
		Running   bool   `json:"running"`
		Busy      bool   `json:"busy"`
		SessionID string `json:"session_id"`
		Error     string `json:"error"`
		Turns     int    `json:"turns"`
	} `json:"claude"`
	Pending []pendingReq `json:"pending"`
	Repos   []string     `json:"repos"`
}

type eventsResp struct {
	Events []struct {
		Seq  int64           `json:"seq"`
		Data json.RawMessage `json:"data"`
	} `json:"events"`
	Last int64 `json:"last"`
}

func defaultEndpoint() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi-support", "endpoint.json")
}

const usage = "用法：macsup [-endpoint 路径] [-timeout 时长] status | sync <仓库> <分支> | ask <任务文字|-> | events [-after N] | stop-claude | projsync <项目名>"

// run 是程序主体，返回退出码。
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("macsup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	epPath := flags.String("endpoint", defaultEndpoint(), "端点文件路径")
	timeout := flags.Duration("timeout", 10*time.Hour, "ask 的总时长上限（默认 10 小时）")
	if err := flags.Parse(args); err != nil {
		return exitErr
	}
	rest := flags.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitErr
	}

	switch rest[0] {
	case "status", "sync", "ask", "events", "stop-claude", "projsync":
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n%s\n", rest[0], usage)
		return exitErr
	}

	raw, err := os.ReadFile(*epPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "Mac 没有开协助（找不到 %s）\n", *epPath)
		} else {
			fmt.Fprintf(stderr, "端点文件读不了：%s：%v\n", *epPath, err)
		}
		return exitErr
	}
	var ep endpoint
	if json.Unmarshal(raw, &ep) != nil || ep.Addr == "" {
		fmt.Fprintf(stderr, "端点文件格式不对：%s\n", *epPath)
		return exitErr
	}
	if ep.Until > 0 && ep.Until < time.Now().Unix() {
		fmt.Fprintln(stderr, "Mac 上的协助已到期（端点文件过期）")
		return exitErr
	}
	c := &client{base: "http://" + ep.Addr, token: ep.Token, hc: &http.Client{Timeout: httpTimeout}}

	var code int
	cmd, cargs := rest[0], rest[1:]
	switch cmd {
	case "status":
		code, err = cmdStatus(c, stdout)
	case "sync":
		code, err = cmdSync(c, cargs, stdout)
	case "ask":
		code, err = cmdAsk(c, cargs, stdin, stdout, *timeout, *epPath)
	case "events":
		code, err = cmdEvents(c, cargs, stdout)
	case "stop-claude":
		if _, err = c.do(http.MethodPost, "/stop-claude", nil, ""); err == nil {
			fmt.Fprintln(stdout, "已请求停止 Mac 上的 Claude")
		}
	case "projsync":
		code, err = cmdProjsync(c, cargs, stdout, projsyncTimeout)
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n%s\n", cmd, usage)
		return exitErr
	}
	if err != nil {
		fmt.Fprintln(stderr, "错误："+err.Error())
		return exitErr
	}
	return code
}

func cmdStatus(c *client, out io.Writer) (int, error) {
	var s statusResp
	if err := c.getJSON("/status", &s); err != nil {
		return 0, err
	}
	if !s.Active {
		fmt.Fprintln(out, "协助：未开启")
	} else {
		mins := int(math.Ceil(time.Until(time.Unix(s.Until, 0)).Minutes()))
		if mins < 0 {
			mins = 0
		}
		fmt.Fprintf(out, "协助：进行中，剩余 %d 分钟\n", mins)
	}
	cl := s.Claude
	state := "未运行"
	if cl.Running {
		state = "空闲"
		if cl.Busy {
			state = "忙碌"
		}
	}
	fmt.Fprintf(out, "Claude：%s，会话 %s，已 %d 轮\n", state, cl.SessionID, cl.Turns)
	if cl.Error != "" {
		fmt.Fprintf(out, "Claude 错误：%s\n", cl.Error)
	}
	fmt.Fprintf(out, "待批请求：%d 条\n", len(s.Pending))
	for _, p := range s.Pending {
		fmt.Fprintf(out, "  %s %s\n", p.Tool, p.Summary)
	}
	fmt.Fprintf(out, "可同步的仓库：%s\n", strings.Join(s.Repos, "、"))
	return exitOK, nil
}

func cmdSync(c *client, args []string, out io.Writer) (int, error) {
	if len(args) != 2 {
		return 0, errors.New("用法：macsup sync <仓库> <分支>")
	}
	q := url.Values{"repo": {args[0]}, "branch": {args[1]}}
	b, err := c.do(http.MethodPost, "/sync?"+q.Encode(), nil, "")
	if err != nil {
		return 0, err
	}
	var r struct {
		Head string `json:"head"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	fmt.Fprintln(out, r.Head)
	return exitOK, nil
}

func cmdEvents(c *client, args []string, out io.Writer) (int, error) {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	after := flags.Int64("after", 0, "")
	if err := flags.Parse(args); err != nil {
		return 0, errors.New("用法：macsup events [-after N]")
	}
	r, err := fetchEvents(c, *after, 0)
	if err != nil {
		return 0, err
	}
	for _, ev := range r.Events {
		printEvent(out, ev.Data)
	}
	fmt.Fprintf(out, "last=%d\n", r.Last)
	return exitOK, nil
}

func fetchEvents(c *client, after int64, wait int) (eventsResp, error) {
	var r eventsResp
	err := c.getJSON(fmt.Sprintf("/events?after=%d&wait=%d", after, wait), &r)
	return r, err
}

func printEvent(out io.Writer, data json.RawMessage) {
	for _, l := range formatEvent(data) {
		fmt.Fprintln(out, l)
	}
}

func cmdProjsync(c *client, args []string, out io.Writer, timeout time.Duration) (int, error) {
	if len(args) != 1 {
		return 0, errors.New("用法：macsup projsync <项目名>")
	}
	name := args[0]
	// 发出前先打印提示信息
	fmt.Fprintf(out, "已请求同步 %s，请在 Mac 的 xxyi 协助页点允许…\n", name)

	// 用单独的超时
	hc := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodPost, c.base+"/projsync?name="+url.QueryEscape(name), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("连不上 Mac：%w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return 0, fmt.Errorf("%s（HTTP %d）", e.Error, resp.StatusCode)
		}
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 成功：打印 result 的 JSON
	var r struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	enc, err := json.MarshalIndent(r.Result, "", "  ")
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(out, string(enc))
	return exitOK, nil
}

func cmdAsk(c *client, args []string, stdin io.Reader, out io.Writer, timeout time.Duration, epPath string) (int, error) {
	if len(args) != 1 {
		return 0, errors.New("用法：macsup ask <任务文字|->")
	}
	task := args[0]
	if task == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return 0, err
		}
		task = string(b)
	}
	if strings.TrimSpace(task) == "" {
		return 0, errors.New("任务文字为空")
	}

	// 先取当前最新序号，只看之后的事件。
	r, err := fetchEvents(c, math.MaxInt64, 0)
	if err != nil {
		return 0, err
	}
	last := r.Last
	var st statusResp
	if c.getJSON("/status", &st) == nil && st.Claude.Busy {
		fmt.Fprintln(out, "提示：Mac 上的 Claude 还在处理上一轮，本轮输出可能混在一起")
	}
	if _, err := c.do(http.MethodPost, "/task", bytes.NewReader([]byte(task)), "text/plain; charset=utf-8"); err != nil {
		return 0, err
	}

	deadline := time.Now().Add(timeout)
	seen := map[string]bool{}
	fails := 0
	// fail 记一次连续失败；达到 3 次返回错误，否则等一会儿再试。
	// 此时端点文件若已被删（Mac 停止协助时会撤销），说明是对方主动停了，不是网络问题。
	fail := func(err error) error {
		fails++
		if fails >= 3 {
			if _, serr := os.Stat(epPath); errors.Is(serr, fs.ErrNotExist) {
				return errors.New("Mac 上已停止协助")
			}
			return err
		}
		time.Sleep(retryDelay)
		return nil
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, fmt.Errorf("等待超过 %s，已放弃（Mac 上的任务可能仍在运行）", timeout)
		}
		wait := min(eventsWait, int(math.Ceil(remaining.Seconds())))
		r, err := fetchEvents(c, last, wait)
		if err != nil {
			if e := fail(err); e != nil {
				return 0, e
			}
			continue
		}
		done, code := false, exitOK
		for _, ev := range r.Events {
			printEvent(out, ev.Data)
			if ev.Seq > last {
				last = ev.Seq
			}
			switch eventType(ev.Data) {
			case "result":
				var res struct {
					IsError bool `json:"is_error"`
				}
				_ = json.Unmarshal(ev.Data, &res)
				if res.IsError {
					code = exitFail
				}
				done = true
			case "exited":
				code, done = exitFail, true
			}
		}
		if done {
			return code, nil
		}
		var s statusResp
		if err := c.getJSON("/status", &s); err != nil {
			if e := fail(err); e != nil {
				return 0, e
			}
			continue
		}
		fails = 0
		for _, p := range s.Pending {
			if !seen[p.ID] {
				seen[p.ID] = true
				fmt.Fprintf(out, "⏸ 等 Mac 上的用户授权：%s %s\n", p.Tool, p.Summary)
			}
		}
	}
}
