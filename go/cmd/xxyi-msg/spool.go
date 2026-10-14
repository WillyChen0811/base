package main

// 本地暂存：send/reply 连不上（connError）时把请求正文按条落盘——每条一个 0600 的 JSON，
// 文件名以创建时间（Unix 纳秒）打头保证字典序即时间序；目录 0700。
// 之后任何子命令运行前先补发：按创建顺序逐条重新签名发送，成功删文件，
// 连不上就地停（留下的下次再发），服务端明确拒绝（4xx）移入 rejected/ 并记下原因。
// 满 spoolMaxCount 条或最旧一条超过 spoolMaxAge 就不再暂存，让连不上原样报出去。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 暂存目录按侧固定（同 client 证书选侧同一依据），做成包级变量便于测试指到临时目录。
var (
	spoolDirUbuntu = defaultSpoolDirUbuntu()
	spoolDirMac    = defaultSpoolDirMac()
)

// 暂存上限：测试里调小。
var (
	spoolMaxCount = 50
	spoolMaxAge   = 24 * time.Hour
)

// spoolRejectedSub 是被拒消息的归档子目录。
const spoolRejectedSub = "rejected"

// spoolRejections 记本轮补发被拒的原因，收件箱输出顶部用；只在主协程读写（MCP 循环单线程）。
var spoolRejections []string

func defaultSpoolDirUbuntu() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi-support", "spool")
}

func defaultSpoolDirMac() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi", "spool")
}

// spoolEntry 是一条暂存请求：补发时用 Method/Path/Body 原样重发（签名每次现签）。
type spoolEntry struct {
	Created time.Time `json:"created"`
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	// Body 是 /msg/send 的原始 JSON 正文；用指针区分"没带正文"（读坏的旧文件）与"正文是 null"。
	Body         json.RawMessage `json:"body,omitempty"`
	RejectReason string          `json:"reject_reason,omitempty"`
	RejectedAt   *time.Time      `json:"rejected_at,omitempty"`
}

// spoolDirNow 按当前侧选暂存目录：有 endpoint.json 的是 Ubuntu 一侧（与 connect 同一判据）。
func spoolDirNow() string {
	if _, err := os.Stat(endpointPath); err == nil {
		return spoolDirUbuntu
	}
	return spoolDirMac
}

// spoolList 按创建顺序（文件名字典序，文件名以定宽纳秒打头）返回暂存文件的完整路径。
func spoolList(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, filepath.Join(dir, e.Name()))
	}
	sort.Strings(names)
	return names
}

// spoolNameNano 从文件名前缀还原创建时间；解不出返回 false（按"不超期"放行）。
func spoolNameNano(name string) (time.Time, bool) {
	stem := strings.TrimSuffix(filepath.Base(name), ".json")
	head, _, _ := strings.Cut(stem, "-")
	ns, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// spoolAdd 暂存一条请求，返回暂存后的总条数；满额或最旧超期返回错误——调用方据此把连不上原样报出去。
func spoolAdd(dir, method, path string, body []byte) (int, error) {
	files := spoolList(dir)
	now := time.Now()
	if len(files) >= spoolMaxCount {
		return 0, fmt.Errorf("暂存已满 %d 条", len(files))
	}
	if len(files) > 0 {
		if oldest, ok := spoolNameNano(files[0]); ok && now.Sub(oldest) > spoolMaxAge {
			return 0, fmt.Errorf("最旧一条已暂存超过 %v", spoolMaxAge)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("建暂存目录失败：%w", err)
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return 0, fmt.Errorf("生成暂存文件名失败：%w", err)
	}
	raw, err := json.Marshal(spoolEntry{Created: now, Method: method, Path: path, Body: json.RawMessage(body)})
	if err != nil {
		return 0, fmt.Errorf("编码暂存条目失败：%w", err)
	}
	name := fmt.Sprintf("%019d-%s.json", now.UnixNano(), hex.EncodeToString(suffix))
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		return 0, fmt.Errorf("暂存写入失败：%w", err)
	}
	return len(files) + 1, nil
}

// flushSpool 按创建顺序补发暂存，返回本轮被 4xx 拒绝的原因（收件箱顶注用）。
// 总耗时受 ctx 的 deadline 约束（state 钩子只剩不到 2 秒就这么点预算，超了自然放下次）；
// 没有暂存、连不上、没证书都静默——补发是顺手的事，不该拖慢手头这条命令。
func flushSpool(ctx context.Context) []string {
	files := spoolList(spoolDirNow())
	if len(files) == 0 {
		return nil
	}
	timeout := httpTimeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	if timeout <= 0 {
		return nil
	}
	c, err := connect(ctx, timeout)
	if err != nil || c.bundle == nil {
		return nil
	}
	var rejected []string
	for _, f := range files {
		var entry spoolEntry
		raw, rerr := os.ReadFile(f)
		if rerr == nil {
			rerr = json.Unmarshal(raw, &entry)
		}
		if rerr != nil || entry.Body == nil {
			// 读不了的暂存文件补发无望，和被拒的一样归档，别让坏文件堵住队头。
			if reason := spoolReject(spoolDirNow(), f, "暂存文件读不了，无法补发"); reason != "" {
				rejected = append(rejected, reason)
			}
			continue
		}
		method := entry.Method
		if method == "" {
			method = http.MethodPost
		}
		_, derr := c.do(ctx, method, c.path(entry.Path, nil), entry.Body) // do 内部每次现签新头
		if derr == nil {
			os.Remove(f)
			continue
		}
		var se *statusError
		if errors.As(derr, &se) && se.status >= 400 && se.status < 500 {
			if reason := spoolReject(spoolDirNow(), f, se.serverReason()); reason != "" {
				rejected = append(rejected, reason)
			}
			continue
		}
		break // 连不上或说不清的错：剩下的留着下次
	}
	return rejected
}

// spoolReject 把一条暂存移入 rejected/ 并记下原因；返回记录的原因（空串表示移动失败，原地留着）。
func spoolReject(dir, file, reason string) string {
	rejDir := filepath.Join(dir, spoolRejectedSub)
	if err := os.MkdirAll(rejDir, 0o700); err != nil {
		return ""
	}
	now := time.Now()
	raw, err := os.ReadFile(file)
	if err != nil {
		raw = nil
	}
	var entry spoolEntry
	if json.Unmarshal(raw, &entry) != nil {
		entry = spoolEntry{Created: now}
	}
	if b, ok := spoolNameNano(file); ok {
		entry.Created = b
	}
	entry.RejectReason = reason
	entry.RejectedAt = &now
	out, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	if err := os.WriteFile(filepath.Join(rejDir, filepath.Base(file)), out, 0o600); err != nil {
		return ""
	}
	if err := os.Remove(file); err != nil {
		return ""
	}
	return reason
}

// rejectedNote 渲染收件箱顶注：本轮补发被拒的条数与去重后的原因。
func rejectedNote() string {
	if len(spoolRejections) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(spoolRejections))
	reasons := make([]string, 0, len(spoolRejections))
	for _, r := range spoolRejections {
		if !seen[r] {
			seen[r] = true
			reasons = append(reasons, r)
		}
	}
	return fmt.Sprintf("有 %d 条暂存消息被拒：%s", len(spoolRejections), strings.Join(reasons, "；"))
}
