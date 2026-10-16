package main

// mac_* 工具：从这边经协助远端（wgtund 的 remoteMux）操作 Mac 上的执行接口 /exec/*。
// 地址与令牌走现有 connect()，与 xxyi_send 等工具同一套通道；约定见 tasks-macexec/CONTRACT.md。
// 短调用（状态/读文件/日志/查任务/截图）一次往返；长任务（构建/自测/节点测试/打包/装 app）
// 先 POST /exec/start 拿 job，再按固定 5 秒节拍查 /exec/job，最多等 9 分钟——
// 没跑完不报错，返回 job id 让调用方用 mac_job 接着查。
// wgtund 重启后协助会按 autostart 自动新开，令牌与地址都会变（D1 调研），入口发布在
// ~/.xxyi-support/endpoint.json：所以每次调用都现读入口（connect 每次重读该文件），
// 连不上或 401 时再读一次、重试一次；轮询期间同样每次现读，单次查询失败（连不上/401/5xx）
// 容忍重试，连续 3 次或累计 60 秒才报错（install_app 会重启 wgtund，必须靠这个撑过去）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/macexec"
)

// 超时、轮询节奏与失败容忍，测试里可调小。
var (
	macCallTimeout             = 30 * time.Second       // 单次 /exec/* 请求的 HTTP 超时
	macQuickWait               = 30 * time.Second       // 快任务（git status）从启动到结束的等待上限
	macPollInterval            = 5 * time.Second        // 长任务轮询节拍（约定：5 秒，从上次查询开始算）
	macQuickPollInterval       = 500 * time.Millisecond // 快任务轮询节拍
	macMaxWait                 = 9 * time.Minute        // 长任务在工具内部最多等 9 分钟（从启动独立计）
	macFailMax                 = 3                      // 轮询连续失败次数上限，超过才报错
	macFailBudget              = 60 * time.Second       // 轮询失败累计上限（期间反复重读入口等 Mac 回来）
	screenshotMax        int64 = 8 << 20                // 截图响应上限 8MB（服务端本就限宽 1600，防异常响应撑爆内存）
)

// pngSignature 是 PNG 的完整 8 字节签名（RFC 2083）；只比 "\x89PNG" 4 字节太松，半截响应也可能撞上。
var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// macRepoXxyi 是长任务动作所在的仓库（swift_build/selftest/node_test/build_app 都在 xxyi 里）。
const macRepoXxyi = "xxyi"

// macNodeTestName 与服务端 actions 同一套规则：测试名只许小写字母数字下划线。
var macNodeTestName = regexp.MustCompile(`^[a-z0-9_]+$`)

// mcpMacTools 是加进 tools/list 的十个 mac_* 工具。
var mcpMacTools = []map[string]any{
	{
		"name":        "mac_git_status",
		"description": "看 Mac 上某个仓库的 git status（porcelain 格式，含分支行）。",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo": map[string]any{"type": "string", "enum": macexec.DefaultRepos, "description": "仓库名"},
			},
			"required": []string{"repo"},
		},
	},
	{
		"name":        "mac_read_file",
		"description": "读 Mac 上仓库里的一个文本文件（按行，offset 从 0 起；单次上限 256KB，非 UTF-8 会报错）。",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo":   map[string]any{"type": "string", "enum": macexec.DefaultRepos, "description": "仓库名"},
				"path":   map[string]any{"type": "string", "description": "仓库内相对路径（不许绝对路径、.. 或 .git 段）"},
				"offset": map[string]any{"type": "integer", "description": "起始行（0 起），默认 0"},
				"limit":  map[string]any{"type": "integer", "description": "读取行数，默认由服务端定（读满 256KB 为止）"},
			},
			"required": []string{"repo", "path"},
		},
	},
	{
		"name":        "mac_logs",
		"description": "看 Mac 上的日志末尾若干行：name 取 service（协助服务）/ wgtund（隧道守护）/ ui（界面）。",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "enum": []string{"service", "wgtund", "ui"}, "description": "日志名"},
				"lines": map[string]any{"type": "integer", "description": "末尾行数，1..500，默认 200"},
			},
			"required": []string{"name"},
		},
	},
	{
		"name":        "mac_job",
		"description": "按 id 查 Mac 上任务的状态与输出末尾（长任务超时没跑完时接着查用）。",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "任务 id（长任务工具返回的）"},
			},
			"required": []string{"id"},
		},
	},
	{
		"name":        "mac_build",
		"description": "在 Mac 上跑 swift build（xxyi/mac）。内部每 5 秒查一次、最多等 9 分钟；没跑完返回 job id，用 mac_job 接着查。",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
	},
	{
		"name":        "mac_selftest",
		"description": "在 Mac 上跑 swift run xxyi-selftest（xxyi/mac）。内部每 5 秒查一次、最多等 9 分钟；没跑完返回 job id，用 mac_job 接着查。",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
	},
	{
		"name":        "mac_node_test",
		"description": "在 Mac 上跑 node tests/js/<name>.test.js（xxyi 仓库）。name 只许小写字母数字下划线。内部每 5 秒查一次、最多等 9 分钟；没跑完返回 job id，用 mac_job 接着查。",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "测试文件名（不带 .test.js 后缀）"},
			},
			"required": []string{"name"},
		},
	},
	{
		"name":        "mac_build_app",
		"description": "在 Mac 上跑 ./build.sh 打包 app（xxyi/mac）。内部每 5 秒查一次、最多等 9 分钟；没跑完返回 job id，用 mac_job 接着查。",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
	},
	{
		"name":        "mac_install_app",
		"description": "在 Mac 上安装刚打包的 XXYI.app 并重启（装 ~/xxyi-work/xxyi/mac/build/XXYI.app）。安装会重启 wgtund，地址与令牌随之变化，工具会自动重读入口接着查。内部每 5 秒查一次、最多等 9 分钟；没跑完返回 job id，用 mac_job 接着查。",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
	},
	{
		"name":        "mac_screenshot",
		"description": "截 Mac 的屏：target=screen 整屏、target=xxyi XXYI 界面窗口，返回 PNG 图片。",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{"type": "string", "enum": []string{"screen", "xxyi"}, "description": "截图对象，默认 screen"},
			},
			"required": []string{},
		},
	},
}

// allMcpTools 是 tools/list 的完整名单：五个消息工具 + 十个 mac 工具。
func allMcpTools() []map[string]any {
	return append(append([]map[string]any{}, mcpTools...), mcpMacTools...)
}

// runMacTool 是 mac_* 文字类工具的实现（mac_screenshot 在 callTool 里走图片通道，不经过这里）。
// 每个工具自带总预算：长任务 = macMaxWait（截止时间在里面独立计）+ 一次调用的收尾余量，
// 其余 = macQuickWait + 余量。
func runMacTool(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "mac_git_status":
		repo, err := macRepoArg(args)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, macQuickWait+macCallTimeout)
		defer cancel()
		return macRunJob(ctx, "git_status", repo, "", true)
	case "mac_read_file":
		repo, err := macRepoArg(args)
		if err != nil {
			return "", err
		}
		path, err := argString(args, "path", true)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(path) == "" {
			return "", errors.New("path 不能为空")
		}
		offset, err := argInt(args, "offset", 0)
		if err != nil {
			return "", err
		}
		limit, err := argInt(args, "limit", 0)
		if err != nil {
			return "", err
		}
		if offset < 0 || limit < 0 {
			return "", fmt.Errorf("offset/limit 不能为负：offset=%d limit=%d", offset, limit)
		}
		ctx, cancel := context.WithTimeout(ctx, macQuickWait+macCallTimeout)
		defer cancel()
		return macReadFile(ctx, repo, path, offset, limit)
	case "mac_logs":
		name, err := argString(args, "name", true)
		if err != nil {
			return "", err
		}
		switch name {
		case "service", "wgtund", "ui":
		default:
			return "", fmt.Errorf("name 只能是 service/wgtund/ui，实际是 %q", name)
		}
		lines, err := argInt(args, "lines", 200)
		if err != nil {
			return "", err
		}
		if lines < 1 || lines > 500 {
			return "", fmt.Errorf("lines 必须 1..500，实际 %d", lines)
		}
		ctx, cancel := context.WithTimeout(ctx, macQuickWait+macCallTimeout)
		defer cancel()
		return macLogs(ctx, name, lines)
	case "mac_job":
		id, err := argString(args, "id", true)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, macQuickWait+macCallTimeout)
		defer cancel()
		// mac_job 本身也每次现读入口：装完 app 拿到的旧 id，重启后照样能查到。
		job, tail, err := macGetJob(ctx, id)
		if err != nil {
			return "", err
		}
		return macJobText(job, tail), nil
	case "mac_build":
		return macRunLong(ctx, "swift_build", macRepoXxyi, "")
	case "mac_selftest":
		return macRunLong(ctx, "selftest", macRepoXxyi, "")
	case "mac_build_app":
		return macRunLong(ctx, "build_app", macRepoXxyi, "")
	case "mac_install_app":
		// 装 app 会重启 wgtund（协助自动重开、地址令牌变）：靠 macRunJob 每次现读入口 + 失败容忍撑过去。
		return macRunLong(ctx, "install_app", macRepoXxyi, "")
	case "mac_node_test":
		testName, err := argString(args, "name", true)
		if err != nil {
			return "", err
		}
		if !macNodeTestName.MatchString(testName) {
			return "", fmt.Errorf("测试名 %q 不合法：只许小写字母、数字、下划线", testName)
		}
		return macRunLong(ctx, "node_test", macRepoXxyi, testName)
	default:
		return "", fmt.Errorf("没有这个工具 %q", name)
	}
}

// macRepoArg 取必填的 repo 并按服务端同一套名单校验。
func macRepoArg(args map[string]any) (string, error) {
	repo, err := argString(args, "repo", true)
	if err != nil {
		return "", err
	}
	for _, r := range macexec.DefaultRepos {
		if repo == r {
			return repo, nil
		}
	}
	return "", fmt.Errorf("仓库 %q 不在允许名单里（可用：%s）", repo, strings.Join(macexec.DefaultRepos, "/"))
}

// macRunLong 启动长任务并轮询到结束：done 返回文字；failed/timeout 报错（带退出码与输出末尾）；
// 从启动独立计的 macMaxWait 预算用完还没跑完则正常返回 job id，让调用方用 mac_job 接着查。
// 外层 ctx 只多留一次调用的余量兜底，真正的 9 分钟截止在 macRunJob 里按 deadline 算。
func macRunLong(ctx context.Context, action, repo, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, macMaxWait+macCallTimeout)
	defer cancel()
	return macRunJob(ctx, action, repo, name, false)
}

// macStaleEntry 判错误是不是"入口旧了"：连不上或 401——wgtund 重启后协助自动重开，
// 地址与令牌都换到了新的 endpoint.json（D1 调研），必须重读入口再试。
func macStaleEntry(err error) bool {
	var ce *connError
	if errors.As(err, &ce) {
		return true
	}
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusUnauthorized
}

// macPollRetryable 判轮询中单次 /exec/job 查询失败可否容忍：连不上/401/5xx——
// install_app 会重启 wgtund，Mac 回来前这段窗口里的查询注定是这个样子（404/400 重试没意义）。
func macPollRetryable(err error) bool {
	if macStaleEntry(err) {
		return true
	}
	var se *statusError
	return errors.As(err, &se) && se.status >= 500
}

// macCallOnce 现读入口（connect 每次都重读 endpoint.json）做一次请求。
func macCallOnce(ctx context.Context, fn func(c *client) ([]byte, error)) ([]byte, error) {
	c, err := connect(ctx, macCallTimeout)
	if err != nil {
		return nil, err
	}
	return fn(c)
}

// macCall 是 mac_* 所有请求的统一入口：每次现读入口做一次请求；
// 入口旧了（连不上/401）就再读一次、重试一次，第二次仍失败则拿第二次的错误。
func macCall(ctx context.Context, fn func(c *client) ([]byte, error)) ([]byte, error) {
	b, err := macCallOnce(ctx, fn)
	if err == nil || !macStaleEntry(err) {
		return b, err
	}
	return macCallOnce(ctx, fn)
}

// macRunJob 启动一个动作并轮询到状态不再是 running（或预算用完）。
// 长任务：截止时间从启动独立计算（macMaxWait 是预算不是叠加出来的墙），
// 按固定节拍轮询（节拍从上次查询开始算，请求耗时不往后叠），每次查询的 ctx 限在剩余预算内；
// 单次查询失败（连不上/401/5xx）不立即报错，下一个节拍重读入口再查，
// 连续 macFailMax 次或失败累计 macFailBudget 才报错并附 job id。
// quick=true 用快任务的节奏与预算（macQuickWait），到点还没完直接报错——
// 那是异常，不该让调用方拿着 id 干等；预算用完同样如此。
func macRunJob(ctx context.Context, action, repo, name string, quick bool) (string, error) {
	job, err := macStartJob(ctx, action, repo, name)
	if err != nil {
		return "", err
	}
	interval, budget := macPollInterval, macMaxWait
	if quick {
		interval, budget = macQuickPollInterval, macQuickWait
	}
	deadline := time.Now().Add(budget)
	next := time.Now().Add(interval) // 节拍锚点：上次查询开始 + interval
	fails := 0
	failSince := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("查任务 %s 超时：%w（可用 mac_job id=%s 接着查）", job.ID, ctx.Err(), job.ID)
		case <-time.After(time.Until(next)):
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			break // 预算用完：统一在循环外收尾（长任务正常交回 job id）
		}
		pollAt := time.Now()
		qctx, cancel := context.WithTimeout(ctx, min(macCallTimeout, remain))
		job2, tail, err := macGetJob(qctx, job.ID)
		cancel()
		next = pollAt.Add(interval)
		if err != nil {
			if !macPollRetryable(err) {
				return "", fmt.Errorf("查任务 %s 失败：%w（可用 mac_job id=%s 接着查）", job.ID, err, job.ID)
			}
			if fails == 0 {
				failSince = pollAt
			}
			fails++
			if fails < macFailMax && time.Since(failSince) < macFailBudget {
				continue // 容忍这一次（多半是 wgtund 正在重启），下个节拍重读入口再查
			}
			return "", fmt.Errorf("查任务 %s 连续 %d 次失败（自 %s 起）：%w（可用 mac_job id=%s 接着查）",
				job.ID, fails, failSince.Format("15:04:05"), err, job.ID)
		}
		fails = 0
		job = job2
		if job.State != macexec.StateRunning {
			text := macJobText(job, tail)
			if job.State != macexec.StateDone {
				return "", errors.New(text)
			}
			return text, nil
		}
		if !time.Now().Before(deadline) {
			break // 最后一次查询也没等到头
		}
	}
	if quick {
		return "", fmt.Errorf("任务 %s（%s）超过 %v 还没跑完，用 mac_job id=%s 接着查", job.ID, job.Action, budget, job.ID)
	}
	return fmt.Sprintf("任务 %s（%s）还在跑，已等 %v；用 mac_job id=%s 接着查", job.ID, job.Action, budget, job.ID), nil
}

// macStartJob 启动任务（经 macCall：每次现读入口，入口旧了重读再试一次）；
// 忙（409）等错误由 client.do 原样带出服务端原因，不重试。
func macStartJob(ctx context.Context, action, repo, name string) (macexec.Job, error) {
	body, err := json.Marshal(map[string]any{"action": action, "repo": repo, "name": name})
	if err != nil {
		return macexec.Job{}, err
	}
	b, err := macCall(ctx, func(c *client) ([]byte, error) {
		return c.do(ctx, http.MethodPost, c.path("/exec/start", nil), body)
	})
	if err != nil {
		return macexec.Job{}, err
	}
	var r struct {
		Job macexec.Job `json:"job"`
	}
	if json.Unmarshal(b, &r) != nil || r.Job.ID == "" {
		return macexec.Job{}, errors.New("Mac 端响应里没有 job id")
	}
	return r.Job, nil
}

// macGetJob 查任务状态与输出末尾（服务端已截到 200 行 / 64KB）；同样每次现读入口。
func macGetJob(ctx context.Context, id string) (macexec.Job, string, error) {
	b, err := macCall(ctx, func(c *client) ([]byte, error) {
		return c.do(ctx, http.MethodGet, c.path("/exec/job", url.Values{"id": {id}}), nil)
	})
	if err != nil {
		return macexec.Job{}, "", err
	}
	var r struct {
		Job  macexec.Job `json:"job"`
		Tail string      `json:"tail"`
	}
	if json.Unmarshal(b, &r) != nil || r.Job.ID == "" {
		return macexec.Job{}, "", errors.New("Mac 端 job 响应格式不对")
	}
	return r.Job, r.Tail, nil
}

// macReadFile 按行读远端仓库文件，返回正文（含行数说明，让调用方知道有没有读全）。
func macReadFile(ctx context.Context, repo, path string, offset, limit int) (string, error) {
	body, err := json.Marshal(map[string]any{"repo": repo, "path": path, "offset": offset, "limit": limit})
	if err != nil {
		return "", err
	}
	b, err := macCall(ctx, func(c *client) ([]byte, error) {
		return c.do(ctx, http.MethodPost, c.path("/exec/read", nil), body)
	})
	if err != nil {
		return "", err
	}
	var r struct {
		Text       string `json:"text"`
		LinesTotal int    `json:"lines_total"`
	}
	if json.Unmarshal(b, &r) != nil {
		return "", errors.New("Mac 端 read 响应格式不对")
	}
	// 本段行数按正文数：末尾换行只是行尾，不额外算一行（数 "\n"+1 会把整段多算一行）。
	got := 0
	if r.Text != "" {
		got = strings.Count(strings.TrimSuffix(r.Text, "\n"), "\n") + 1
	}
	head := fmt.Sprintf("%s:%s（全文 %d 行，本段自第 %d 行起 %d 行）", repo, path, r.LinesTotal, offset+1, got)
	if offset+got < r.LinesTotal {
		head += "，后面还有，加大 limit 或推进 offset"
	}
	return head + "\n" + r.Text, nil
}

// macLogs 取远端日志末尾。
func macLogs(ctx context.Context, name string, lines int) (string, error) {
	q := url.Values{"name": {name}, "lines": {fmt.Sprint(lines)}}
	b, err := macCall(ctx, func(c *client) ([]byte, error) {
		return c.do(ctx, http.MethodGet, c.path("/exec/logs", q), nil)
	})
	if err != nil {
		return "", err
	}
	var r struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(b, &r) != nil {
		return "", errors.New("Mac 端 logs 响应格式不对")
	}
	if strings.TrimSpace(r.Text) == "" {
		return fmt.Sprintf("%s 日志最近 %d 行是空的", name, lines), nil
	}
	return r.Text, nil
}

// macScreenshot 取 PNG 截图原始字节；超限、不是 PNG 或正文断在半路都当场报错：
// 签名查完整 8 字节，再整图解码一次验完整性（截断/坏块在解码时就会报，
// 只查前缀会把中断当成功交出去）。
func macScreenshot(ctx context.Context, args map[string]any) ([]byte, error) {
	target, err := argString(args, "target", false)
	if err != nil {
		return nil, err
	}
	if target == "" {
		target = "screen"
	}
	if target != "screen" && target != "xxyi" {
		return nil, fmt.Errorf("target 只能是 screen/xxyi，实际是 %q", target)
	}
	ctx, cancel := context.WithTimeout(ctx, macQuickWait+macCallTimeout)
	defer cancel()
	b, err := macCall(ctx, func(c *client) ([]byte, error) {
		return c.doLimited(ctx, http.MethodGet, c.path("/exec/screenshot", url.Values{"target": {target}}), nil, screenshotMax)
	})
	if err != nil {
		return nil, err
	}
	if err := macCheckPNG(b); err != nil {
		return nil, fmt.Errorf("Mac 端截图不可用：%w", err)
	}
	return b, nil
}

// macCheckPNG 校验完整 8 字节 PNG 签名，并用 image/png 整图解码一次确认内容完整。
func macCheckPNG(b []byte) error {
	if !bytes.HasPrefix(b, pngSignature) {
		return fmt.Errorf("响应不是 PNG（%d 字节）", len(b))
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		return fmt.Errorf("PNG 不完整（%d 字节）：%w", len(b), err)
	}
	return nil
}

// macJobText 渲染任务状态：一行摘要 + 输出末尾。
func macJobText(job macexec.Job, tail string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "任务 %s（%s）：%s", job.ID, job.Action, job.State)
	if job.State != macexec.StateRunning {
		fmt.Fprintf(&sb, "，退出码 %d", job.ExitCode)
	}
	if job.Note != "" {
		fmt.Fprintf(&sb, "，说明：%s", job.Note)
	}
	if tail != "" {
		sb.WriteString("\n输出（末尾）：\n")
		sb.WriteString(tail)
	}
	return sb.String()
}
