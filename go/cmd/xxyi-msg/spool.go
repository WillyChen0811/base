package main

// 本地暂存：send/reply 连不上（或暂存里还压着旧消息）时把请求正文按条落盘——每条一个 0600 的 JSON，
// 文件名以创建时间（Unix 纳秒）打头保证字典序即时间序；目录 0700。
// 写入走「同目录临时文件 → rename」，暂存目录里不会出现半截 .json；正文里带着 client_id，
// 暂存原样保存、补发不变——服务端按发件人+client_id 去重，响应丢失后重发不会出两条。
// send/reply/inbox/who 与 MCP 工具调用前补发（state 钩子不补发）：处理每条前先 rename 成
// .sending 认领（rename 失败说明别的进程正在补它，跳过）；发送成功删掉，连不上/可重试状态码
// （401、408、429、5xx、网络错误）rename 回原名并就地停（后面的不越过它），
// 永久拒绝（只有 400/403/404/409）移入 rejected/ 并写同名 .reason；创建超过 24 小时的条目不再发，同样拒掉。
// 被拒消息由 inbox 在顶部列出（正文第一行 + 原因），列出后把 .reason 改名为 .reason.seen 不再重复提示。
// 补发开头把超过 sendingStaleAfter 的 .sending 改回原名（认领它的进程多半已经死了）。
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

// 暂存上限与陈旧认领时限：测试里调小。
var (
	spoolMaxCount     = 50
	spoolMaxAge       = 24 * time.Hour
	sendingStaleAfter = 2 * time.Minute
)

// 暂存目录里的后缀与被拒归档子目录名。
const (
	spoolRejectedSub = "rejected"
	sendingSuffix    = ".sending"
	spoolTmpSuffix   = ".tmp"
	reasonSuffix     = ".reason"
	seenSuffix       = ".seen"
)

func defaultSpoolDirUbuntu() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi-support", "spool")
}

func defaultSpoolDirMac() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi", "spool")
}

// spoolEntry 是一条暂存请求：补发时用 Method/Path/Body 原样重发（Body 里已带 client_id，
// 服务端按它去重；签名头每次现签）。RejectReason/RejectStatus 是被拒归档时补记的。
type spoolEntry struct {
	Created time.Time `json:"created"`
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	// Body 是 /msg/send 的原始 JSON 正文；用指针区分"没带正文"（读坏的旧文件）与"正文是 null"。
	Body         json.RawMessage `json:"body,omitempty"`
	RejectReason string          `json:"reject_reason,omitempty"`
	RejectStatus int             `json:"reject_status,omitempty"`
	RejectedAt   *time.Time      `json:"rejected_at,omitempty"`
}

// rejection 是被拒消息的旁车 .reason：跨运行持久，inbox 列一次后改名为 .reason.seen。
type rejection struct {
	Text       string    `json:"text,omitempty"`
	Reason     string    `json:"reason"`
	Status     int       `json:"status,omitempty"`
	RejectedAt time.Time `json:"rejected_at"`
}

// spoolDirNow 按当前侧选暂存目录：有 endpoint.json 的是 Ubuntu 一侧（与 connect 同一判据）。
func spoolDirNow() string {
	if _, err := os.Stat(endpointPath); err == nil {
		return spoolDirUbuntu
	}
	return spoolDirMac
}

// spoolList 按创建顺序（文件名字典序，文件名以定宽纳秒打头）返回暂存文件的完整路径。
// 只认 .json：认领中的 .sending、写一半的 .tmp、归档的 .reason 都不在其中。
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

// newClientID 生成消息正文的随机去重 id（16 字节 hex，服务端限 [A-Za-z0-9._-] ≤64 字节）：
// 服务端按发件人+client_id 去重，响应丢失后补发同一个 id 不会入库两条。
func newClientID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成消息去重 id 失败：%w", err)
	}
	return hex.EncodeToString(b), nil
}

// permanentReject 只有这几个状态码算服务端明确永久拒绝（移入 rejected/）；
// 401、408、429、5xx 连同网络错误都可重试——留在暂存，等下次补发。
func permanentReject(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return true
	}
	return false
}

// spoolAdd 暂存一条请求，按「同目录临时文件 → rename」原子写入，
// 返回最终暂存文件名和暂存后的总条数；满额或最旧超期返回错误——调用方据此把连不上原样报出去。
func spoolAdd(dir, method, path string, body []byte) (string, int, error) {
	files := spoolList(dir)
	now := time.Now()
	if len(files) >= spoolMaxCount {
		return "", 0, fmt.Errorf("暂存已满 %d 条", len(files))
	}
	if len(files) > 0 {
		if oldest, ok := spoolNameNano(files[0]); ok && now.Sub(oldest) > spoolMaxAge {
			return "", 0, fmt.Errorf("最旧一条已暂存超过 %v", spoolMaxAge)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("建暂存目录失败：%w", err)
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", 0, fmt.Errorf("生成暂存文件名失败：%w", err)
	}
	raw, err := json.Marshal(spoolEntry{Created: now, Method: method, Path: path, Body: json.RawMessage(body)})
	if err != nil {
		return "", 0, fmt.Errorf("编码暂存条目失败：%w", err)
	}
	name := fmt.Sprintf("%019d-%s.json", now.UnixNano(), hex.EncodeToString(suffix))
	tmp := filepath.Join(dir, name+spoolTmpSuffix)
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", 0, fmt.Errorf("暂存写入失败：%w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("暂存写入失败：%w", err)
	}
	return name, len(files) + 1, nil
}

// flushSpool 按创建顺序补发暂存，返回补发成功的「暂存文件名 → 服务端消息 id」（调用方追自己的那条结果用）；
// 第二个返回值在非可重试错误处停下时非 nil——可重试错误就地停，后面的不越过它，留下的下次再发。
// 没有暂存、没证书都静默——补发是顺手的事，不该拖慢手头这条命令。
func flushSpool(ctx context.Context) (map[string]string, error) {
	dir := spoolDirNow()
	reclaimStaleSending(dir)
	files := spoolList(dir)
	if len(files) == 0 {
		return nil, nil
	}
	timeout := httpTimeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	if timeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	c, err := connect(ctx, timeout)
	if err != nil {
		return nil, err
	}
	if c.bundle == nil {
		return nil, errNoClientCert(c.bundlePath)
	}
	sent := make(map[string]string)
	now := time.Now()
	for _, f := range files {
		base := filepath.Base(f)
		claimed := f + sendingSuffix
		if err := os.Rename(f, claimed); err != nil {
			continue // 认领不上：别的进程正在补它，跳过
		}
		// rename 不改 mtime：认领时刷新，让「认领超过 2 分钟算死」从认领时刻起算
		claimNow := time.Now()
		_ = os.Chtimes(claimed, claimNow, claimNow)
		var entry spoolEntry
		raw, rerr := os.ReadFile(claimed)
		if rerr == nil {
			rerr = json.Unmarshal(raw, &entry)
		}
		if rerr != nil || entry.Body == nil {
			// 读不了的暂存文件补发无望，和被拒的一样归档，别让坏文件堵住队头。
			spoolReject(dir, claimed, base, "暂存文件读不了，无法补发", 0)
			continue
		}
		created, ok := spoolNameNano(base)
		if !ok {
			created = entry.Created
		}
		if !created.IsZero() && now.Sub(created) > spoolMaxAge {
			spoolReject(dir, claimed, base, "暂存超过 24 小时，已放弃", 0)
			continue
		}
		method := entry.Method
		if method == "" {
			method = http.MethodPost
		}
		b, derr := c.do(ctx, method, c.path(entry.Path, nil), entry.Body) // do 内部每次现签新头
		if derr == nil {
			var r struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(b, &r)
			os.Remove(claimed)
			sent[base] = r.ID
			continue
		}
		var se *statusError
		if errors.As(derr, &se) && permanentReject(se.status) {
			spoolReject(dir, claimed, base, se.serverReason(), se.status)
			continue
		}
		// 连不上或可重试状态码（401/408/429/5xx）：rename 回原名，就地停，等下次
		os.Rename(claimed, f)
		return sent, derr
	}
	return sent, nil
}

// reclaimStaleSending 把超过 sendingStaleAfter 的 .sending 改回原名：
// rename 不改文件 mtime（mtime 还是创建时间），超时的说明认领它的进程多半已经死了，别永远挂着。
func reclaimStaleSending(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, sendingSuffix) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || now.Sub(info.ModTime()) <= sendingStaleAfter {
			continue
		}
		os.Rename(filepath.Join(dir, name), filepath.Join(dir, strings.TrimSuffix(name, sendingSuffix)))
	}
}

// spoolReject 把认领中（.sending 后缀）的暂存移入 rejected/（恢复原名）并在条目里记下原因，
// 同时写同名 .reason 供收件箱顶注列出。返回记录的原因（空串表示归档失败，原地留着等超时回收重试）。
func spoolReject(dir, claimed, base, reason string, status int) string {
	rejDir := filepath.Join(dir, spoolRejectedSub)
	if err := os.MkdirAll(rejDir, 0o700); err != nil {
		return ""
	}
	now := time.Now()
	raw, err := os.ReadFile(claimed)
	if err != nil {
		raw = nil
	}
	var entry spoolEntry
	if json.Unmarshal(raw, &entry) != nil {
		entry = spoolEntry{Created: now}
	}
	if t, ok := spoolNameNano(base); ok {
		entry.Created = t
	}
	entry.RejectReason = reason
	entry.RejectStatus = status
	entry.RejectedAt = &now
	out, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	rejFile := filepath.Join(rejDir, base)
	if err := os.WriteFile(rejFile, out, 0o600); err != nil {
		return ""
	}
	note := rejection{Reason: reason, Status: status, RejectedAt: now}
	var body sendBody
	if json.Unmarshal(entry.Body, &body) == nil {
		note.Text = body.Text
	}
	if rawNote, nerr := json.Marshal(note); nerr == nil {
		if err := os.WriteFile(rejFile+reasonSuffix, rawNote, 0o600); err != nil {
			return ""
		}
	}
	if err := os.Remove(claimed); err != nil {
		return ""
	}
	return reason
}

// readRejection 读 rejected/xxx.json 的 .reason 旁车（不标已读）；没有或读不了返回 false。
func readRejection(dir, base string) (rejection, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, spoolRejectedSub, base+reasonSuffix))
	if err != nil {
		return rejection{}, false
	}
	var rj rejection
	if json.Unmarshal(raw, &rj) != nil || rj.Reason == "" {
		return rejection{}, false
	}
	return rj, true
}

// rejectedNoteText 渲染收件箱顶注：列出所有还没看过的被拒暂存（正文第一行 + 原因），
// 列出后把 .reason 改名为 .reason.seen，下次不再提示。
func rejectedNoteText(dir string) string {
	rejDir := filepath.Join(dir, spoolRejectedSub)
	entries, err := os.ReadDir(rejDir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), reasonSuffix) {
			names = append(names, filepath.Join(rejDir, e.Name()))
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	items := make([]string, 0, len(names))
	for _, n := range names {
		var rj rejection
		if raw, rerr := os.ReadFile(n); rerr == nil {
			_ = json.Unmarshal(raw, &rj)
		}
		reason := rj.Reason
		if reason == "" {
			reason = "服务端拒绝"
		}
		line := reason
		if text := firstLine(rj.Text); text != "" {
			line = "「" + text + "」：" + reason
		}
		items = append(items, line)
		os.Rename(n, n+seenSuffix)
	}
	return fmt.Sprintf("有 %d 条暂存消息被拒：%s", len(items), strings.Join(items, "；"))
}
