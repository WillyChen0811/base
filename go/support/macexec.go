package support

// Mac 端执行接口 /exec/*（挂在 remoteMux，鉴权由外层 guard 做）：
// 同步跑固定动作名单里的命令、读日志尾部、截屏、异步起装 app 脚本。
// 动作/命令/超时都在 go/macexec 里写死，这里只做 HTTP 映射；
// 调用记录单独写 ~/.xxyi/macexec.log（不进协助日志，不调 logEvent），见 tasks-macexec/CONTRACT.md。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WillyChen0811/base/go/macexec"
)

const (
	// maxExecRunBodyBytes 是 POST /exec/run 正文上限：动作+参数几个小字段，留足余量。
	maxExecRunBodyBytes = 16 << 10
	// execLogsDefaultLines / execLogsMaxLines 是 /exec/logs 的默认与上限（约定：N ≤ 500）。
	execLogsDefaultLines = 200
	execLogsMaxLines     = 500
	// execCaptureTimeout 是截图两个外部命令共享的超时；调用方（MCP）给 60s，这里留收尾余量。
	execCaptureTimeout = 30 * time.Second
	// execScreenshotMaxBytes 是截图响应的读取上限（sips 已缩到宽 ≤1600，超了说明异常，拒绝而不是撑爆内存）。
	execScreenshotMaxBytes = 32 << 20
	// macexecLogMaxBytes 是 macexec.log 的轮转阈值（约定：超过 1MB 改名 macexec.log.1 再新建）。
	macexecLogMaxBytes = 1 << 20
	// execWaitDelay 防止外部命令的子孙继承管道把 Wait 卡死。
	execWaitDelay = 5 * time.Second
)

// execLogFile 把 /exec/logs 的 name 映射到 ~/.xxyi 下的日志文件名；只认名单里的名字。
// service=协助服务日志、wgtund=隧道守护日志（界面日志 ui 已按 v2 约定去掉）。
func execLogFile(name string) (string, bool) {
	switch name {
	case "service":
		return "service.log", true
	case "wgtund":
		return "wgtund.log", true
	default:
		return "", false
	}
}

// mountMacExecRemote 把 Mac 执行接口挂进 remoteMux 的 mux（外层 guard 鉴权）。
func (s *Service) mountMacExecRemote(mux *http.ServeMux) {
	mux.HandleFunc("/exec/run", only(http.MethodPost, s.execRun))
	mux.HandleFunc("/exec/logs", only(http.MethodGet, s.execLogs))
	mux.HandleFunc("/exec/screenshot", only(http.MethodGet, s.execScreenshot))
	mux.HandleFunc("/exec/install", only(http.MethodPost, s.execInstall))
}

// execRun 是 POST /exec/run {action, repo, name, branch}：同步执行到结束，
// 返回 {exit_code, timed_out, seconds, tail}。忙 409，参数/路径错 400，没跑成 502。
// branch 可选，只对编译类动作有意义：给了就在专用 worktree 里 fetch 检出该分支再跑，不切换主工作区
// （见 macexec_branch.go），tail 最前面加一行"分支 … @ 提交，目录 …"；git_status 带 branch 报 400。
// 会话结束（guard 绑定的 ctx 取消）时 macexec.Run 会杀掉整个进程组。
func (s *Service) execRun(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxExecRunBodyBytes+1))
	if err != nil {
		macexecRecord(macexecCall{API: "run", Result: "read_body_failed"})
		errJSON(w, http.StatusBadRequest, "读请求体失败")
		return
	}
	if len(body) > maxExecRunBodyBytes {
		macexecRecord(macexecCall{API: "run", Result: "too_large"})
		errJSON(w, http.StatusRequestEntityTooLarge, "正文超过 16KB")
		return
	}
	var req struct {
		Action string `json:"action"`
		Repo   string `json:"repo"`
		Name   string `json:"name"`
		Branch string `json:"branch"`
	}
	call := macexecCall{API: "run"}
	if err := json.Unmarshal(body, &req); err != nil {
		call.Result = "bad_request"
		call.Error = "正文不是合法 JSON"
		macexecRecord(call)
		errJSON(w, http.StatusBadRequest, "正文不是合法 JSON")
		return
	}
	call.Action, call.Repo, call.Name, call.Branch = req.Action, req.Repo, req.Name, req.Branch

	home, err := os.UserHomeDir()
	if err != nil {
		call.Result = "error"
		call.Error = "找不到用户目录"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "找不到用户目录")
		return
	}
	params := macexec.Params{Repo: req.Repo, Name: req.Name}
	header := ""
	if req.Branch != "" {
		if req.Action == macexec.ActionGitStatus {
			// 不能悄悄忽略调用方的选择
			call.Result = "bad_request"
			call.Error = "git_status 不支持 branch"
			macexecRecord(call)
			errJSON(w, http.StatusBadRequest, "git_status 不支持 branch")
			return
		}
		if req.Repo != "" && req.Repo != xxyiRepoName {
			call.Result = "bad_request"
			call.Error = "branch 只用于 xxyi 仓库"
			macexecRecord(call)
			errJSON(w, http.StatusBadRequest, "branch 只用于 xxyi 仓库")
			return
		}
		// 专用 worktree 只有一个槽位：准备（checkout/clean）与动作执行整段持有，别人拿不到就按忙处理。
		if !branchBuildMu.TryLock() {
			call.Result = "busy"
			macexecRecord(call)
			errJSON(w, http.StatusConflict, "专用分支编译目录正被使用")
			return
		}
		defer branchBuildMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), syncTimeout)
		dir, commit, err := s.prepareBranchWorktree(ctx, req.Branch)
		cancel()
		if err != nil {
			status := http.StatusBadGateway
			var be *branchError
			if errors.As(err, &be) {
				status = be.status
			}
			call.Result = "branch_failed"
			call.Error = err.Error()
			macexecRecord(call)
			errJSON(w, status, err.Error())
			return
		}
		params.RootOverride = dir
		header = "分支 " + req.Branch + " @ " + commit[:12] + "，目录 " + dir
	}
	spec, err := macexec.BuildSpec(req.Action, params, home)
	if err != nil {
		call.Result = "bad_request"
		call.Error = err.Error()
		macexecRecord(call)
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := macexec.Run(r.Context(), spec)
	switch {
	case errors.Is(err, macexec.ErrBusy):
		call.Result = "busy"
		macexecRecord(call)
		errJSON(w, http.StatusConflict, "Mac 上正有任务在跑")
	case err != nil:
		call.Result = "error"
		call.Error = err.Error()
		macexecRecord(call)
		errJSON(w, http.StatusBadGateway, "执行失败："+err.Error())
	default:
		exit := res.ExitCode
		call.Result = "done"
		call.ExitCode = &exit
		call.TimedOut = res.TimedOut
		macexecRecord(call)
		tail := res.Tail
		if header != "" {
			tail = header + "\n" + tail
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"exit_code": res.ExitCode,
			"timed_out": res.TimedOut,
			"seconds":   res.Seconds,
			"tail":      tail,
		})
	}
}

// execLogs 是 GET /exec/logs?name=service|wgtund&lines=N：返回 ~/.xxyi 下对应日志的末尾 N 行。
// N 缺省 200，给了必须 1..500；名字不对 400，文件不存在 404。
func (s *Service) execLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	file, ok := execLogFile(name)
	if !ok {
		macexecRecord(macexecCall{API: "logs", Name: name, Result: "bad_request", Error: "日志名不对"})
		errJSON(w, http.StatusBadRequest, "日志名只能是 service/wgtund")
		return
	}
	lines := execLogsDefaultLines
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			macexecRecord(macexecCall{API: "logs", Name: name, Result: "bad_request", Error: "lines 不是整数"})
			errJSON(w, http.StatusBadRequest, "lines 必须是整数")
			return
		}
		if n < 1 || n > execLogsMaxLines {
			macexecRecord(macexecCall{API: "logs", Name: name, Result: "bad_request", Error: "lines 越界"})
			errJSON(w, http.StatusBadRequest, "lines 必须 1..500")
			return
		}
		lines = n
	}
	home, err := os.UserHomeDir()
	if err != nil {
		macexecRecord(macexecCall{API: "logs", Name: name, Result: "error", Error: "找不到用户目录"})
		errJSON(w, http.StatusInternalServerError, "找不到用户目录")
		return
	}
	text, err := tailFile(filepath.Join(home, ".xxyi", file), lines)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			macexecRecord(macexecCall{API: "logs", Name: name, Result: "not_found"})
			errJSON(w, http.StatusNotFound, "日志文件不存在")
			return
		}
		macexecRecord(macexecCall{API: "logs", Name: name, Result: "error", Error: err.Error()})
		errJSON(w, http.StatusInternalServerError, "读日志失败")
		return
	}
	macexecRecord(macexecCall{API: "logs", Name: name, Result: "ok"})
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

// execScreenshot 是 GET /exec/screenshot：screencapture -x 整屏，sips -Z 1600 缩小后原样吐 PNG；
// 临时文件用完即删。非 macOS 501；工具失败 500（错误带命令输出的尾部便于排查）。
func (s *Service) execScreenshot(w http.ResponseWriter, r *http.Request) {
	call := macexecCall{API: "screenshot"}
	if runtime.GOOS != "darwin" {
		call.Result = "unsupported"
		macexecRecord(call)
		errJSON(w, http.StatusNotImplemented, "截图只在 macOS 上可用")
		return
	}
	dir, err := os.MkdirTemp("", "macexec-shot-")
	if err != nil {
		call.Result = "error"
		call.Error = "建临时目录失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "建临时目录失败")
		return
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(r.Context(), execCaptureTimeout)
	defer cancel()
	raw := filepath.Join(dir, "shot.png")
	if err := execCapture(ctx, "screencapture", "-x", raw); err != nil {
		call.Result = "error"
		call.Error = "screencapture 失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "截屏失败："+err.Error())
		return
	}
	out := filepath.Join(dir, "small.png")
	if err := execCapture(ctx, "sips", "-Z", "1600", raw, "--out", out); err != nil {
		call.Result = "error"
		call.Error = "sips 缩小失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "缩小截图失败："+err.Error())
		return
	}
	st, err := os.Stat(out)
	if err != nil {
		call.Result = "error"
		call.Error = "读截图失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "读截图失败")
		return
	}
	if st.Size() > execScreenshotMaxBytes {
		call.Result = "too_large"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "截图超过 32MB")
		return
	}
	data, err := os.ReadFile(out)
	if err != nil {
		call.Result = "error"
		call.Error = "读截图失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "读截图失败")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	call.Result = "ok"
	macexecRecord(call)
}

// macexecInstallScript 是装 app 脚本的全文（写死，不接受任何外部参数，见 CONTRACT.md）：
// 校验 build/XXYI.app 签名 → 退出 XXYI → pkill 残留的 XXYI 与 wgtund → 直接打开打包目录里的新版。
// **不覆盖用户桌面上的 XXYI.app**（用户 10/16：新版只留在打包目录，替换由用户自己决定）。
// 第一行 rm -f "$0" 删掉自己的脚本文件（sh 已打开该文件，删除后照常读完）。
// 不另存日志：输出全部丢弃，调用方用截图和 /exec/logs 看结果。
const macexecInstallScript = `#!/bin/sh
set -e
SRC="$HOME/xxyi-work/xxyi/mac/build/XXYI.app"
rm -f "$0"
codesign --verify --deep --strict "$SRC"
osascript -e 'quit app "XXYI"' || true
sleep 2
pkill -x XXYI || true
pkill -f wgtund || true
open "$SRC"
`

// execInstall 是 POST /exec/install：把脚本写到临时文件，独立进程组起 /bin/sh 后立即返回
// {"started": true}。脚本不在请求 ctx 里跑——它随后会杀掉 wgtund（也就是本进程），
// 必须脱离本进程活着装完；stdin/stdout/stderr 都接 /dev/null（exec.Cmd 的 nil 即空设备）。
func (s *Service) execInstall(w http.ResponseWriter, r *http.Request) {
	call := macexecCall{API: "install"}
	if runtime.GOOS != "darwin" {
		call.Result = "unsupported"
		macexecRecord(call)
		errJSON(w, http.StatusNotImplemented, "安装只在 macOS 上可用")
		return
	}
	f, err := os.CreateTemp("", "macexec-install-*.sh")
	if err != nil {
		call.Result = "error"
		call.Error = "写临时脚本失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "写临时脚本失败")
		return
	}
	path := f.Name()
	if _, err := f.WriteString(macexecInstallScript); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		call.Result = "error"
		call.Error = "写临时脚本失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "写临时脚本失败")
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		call.Result = "error"
		call.Error = "写临时脚本失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "写临时脚本失败")
		return
	}
	cmd := exec.Command("/bin/sh", path)
	setupProc(cmd)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(path)
		call.Result = "error"
		call.Error = "起安装进程失败"
		macexecRecord(call)
		errJSON(w, http.StatusInternalServerError, "起安装进程失败")
		return
	}
	// 不 Wait：装到一半脚本会 pkill wgtund，本进程随之结束，子进程过继给 launchd 回收；
	// wgtund 活着时由这个 goroutine 收尸，防止僵尸。
	go func() { _ = cmd.Wait() }()
	call.Result = "started"
	macexecRecord(call)
	writeJSON(w, http.StatusOK, map[string]bool{"started": true})
}

// execCapture 跑一个截图外部命令（只在 darwin 上被调用）：自成进程组，超时整组杀，
// 合并输出截 500 字进错误信息（这条错误会原样回给调用方，用于排查权限问题）。
func execCapture(ctx context.Context, argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	setupProc(cmd)
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	cmd.WaitDelay = execWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(truncRunes(strings.TrimSpace(string(out)), 500) + "：" + err.Error())
	}
	return nil
}

// tailFile 从文件末尾按 64KB 块往前读，只读到凑够 lines 行为止；返回末尾 lines 行。
func tailFile(path string, lines int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("日志路径不是普通文件")
	}
	const chunk = 64 << 10
	var buf []byte
	size := st.Size()
	for size > 0 {
		n := int64(chunk)
		if size < n {
			n = size
		}
		b := make([]byte, n)
		if _, err := f.ReadAt(b, size-n); err != nil {
			return "", err
		}
		buf = append(b, buf...)
		size -= n
		if strings.Count(string(buf), "\n") >= lines || size == 0 {
			break
		}
	}
	text := strings.TrimSuffix(string(buf), "\n")
	if text == "" {
		return "", nil
	}
	all := strings.Split(text, "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n"), nil
}

// ---------- 调用记录 ~/.xxyi/macexec.log ----------

// macexecCall 是一条调用记录：时间、接口/动作、仓库、结果（退出码/超时/忙/参数错）。
// 只记"发生了什么"，绝不记输出内容；不进协助日志（不调 logEvent），见 CONTRACT.md。
type macexecCall struct {
	Time     string `json:"time"`
	API      string `json:"api"`
	Action   string `json:"action,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Name     string `json:"name,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Result   string `json:"result"`
	Error    string `json:"error,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// macexecLogMu 串行化对 macexec.log 的追加与轮转（进程级：多个会话/并发请求共享同一文件）。
var macexecLogMu sync.Mutex

// macexecRecord 追加一行到 ~/.xxyi/macexec.log（0600）；超过 1MB 先改名为 macexec.log.1 再新建
// （rename 直接覆盖，只留这一份旧的）。任何失败都静默——记账失败不能影响接口本身。
func macexecRecord(c macexecCall) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	c.Time = time.Now().Format(time.RFC3339)
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	macexecLogMu.Lock()
	defer macexecLogMu.Unlock()
	path := filepath.Join(home, ".xxyi", "macexec.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size() > macexecLogMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	if err := f.Chmod(0o600); err != nil { // 已存在的宽权限文件也收紧；收不紧就不写
		_ = f.Close()
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}
