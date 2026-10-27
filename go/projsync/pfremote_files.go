package projsync

// 项目文件同步 T8：Ubuntu 侧 meta 目录里的文件格式与读写（技术设计 §6.2、§8.3、§12.1）。
//
// 这些文件有 Go 和 Python 两个读写方：Mac 的 wgtund 经 projfiles-apply 写状态文件、取走请求文件；
// Ubuntu 上的 Python（取最新钩子，xxyi/projfiles/fresh_ubuntu.py）读状态文件、写请求文件。
// 格式以本注释为准，Python 照着实现；所有 JSON 都是 UTF-8、键名固定、读的一方对未知键报错（版本号变了再放开）。
//
// 【meta 目录布局】（<meta> 缺省 /home/chenwy/.xxyi-support/projfiles/）
//
//	roots.json              白名单，部署时人工写；Go 只读
//	coordinator.json        协调者租约；poll 续租、apply 核对
//	staging/<sha256>.part   暂存区（传输层写、写原语放到正位）；<sha256>.meta 是传输层的续传记录
//	回收/<YYYY-MM-DD>/...   同步回收（pfrecycle.go）
//	hashcache/<键>.json     哈希缓存（pfhash.go 的格式；Ubuntu 与 Mac 各一份，坏了只是重算）
//	status/<键>.json        每个单元的状态（Mac 每轮写，Python 读）
//	status/_mac.json        Mac 最近接触时间和网络类别（Mac 每轮写，Python 读）
//	requests/<键>-<时间>-<随机>.json   请求文件（Python 写，Mac poll 读、处理完 take 掉）
//
// <键> = 单元键 = 归一化项目编号（大写 ASCII 字母数字，如 260403J1001）。
//
// 【roots.json】
//
//	{"version": 1, "roots": ["/home/chenwy/xxyi-projects"]}
//
// roots 是同步根的白名单（绝对路径，已 Clean，无重复，不含 meta、也不在 meta 里面）。请求里的 Root 不在这里就拒绝。
//
// 【coordinator.json】
//
//	{"version": 1, "id": "<协调者编号>", "host": "<Mac 主机名>", "expires": "2026-10-27T01:23:45Z"}
//
// expires 是 RFC3339（UTC）。读不懂的租约文件不会被覆盖，poll 会拒绝并要求人工处理。
//
// 【status/<键>.json】（PFRemoteStatus；时间都是 RFC3339，空串 = 不知道）
//
//	{
//	  "version": 1,
//	  "unit": "260403J1001",
//	  "round_at": "...",          本单元最近一轮结束的时间
//	  "mac_seen_at": "...",       Mac 最近一次接触 Ubuntu 的时间
//	  "ubuntu_rel": "2026/260403J1001-名称",
//	  "stopped": false,           true = 此项目已停止同步（Mac 上可能有更新）
//	  "note": "",                 单元级说明（如「已停止同步」「重复，停下」）
//	  "pending_to_ubuntu": [{"path": "报告/a.docx", "size": 123, "sha": "<Mac 上这份的 sha256>",
//	                         "mac_mtime_ns": 1790000000000000000,
//	                         "ub_size": 100, "ub_mtime_ns": 1780000000000000000,   Mac 上次看到的 Ubuntu 这份（Ubuntu 没有则省略）
//	                         "reason": "暂缓"}],                                    暂缓 | 排队 | 冲突
//	  "pending_to_mac":    [{"path": "...", "size": 1, "sha": "...", "ub_mtime_ns": 1, "reason": "排队"}],
//	  "conflicts":         [{"path": "a.docx", "conflict_path": "a（冲突-Mac-1026-1530）.docx", "winner": "ubuntu", "at": "..."}],
//	  "notes":             ["这一轮删了 3 个，都在同步回收里"]
//	}
//
// 读取方的用法（§6.2）：路径在 pending_to_ubuntu 里、且 Ubuntu 本地 stat 的 size 和 mtime_ns 仍等于 ub_size、ub_mtime_ns
// （没有 ub_ 字段则要求 Ubuntu 上本来就没有这个文件）→ 附注「Mac 上有新版本还没传过来」并写请求文件。
//
// 【status/_mac.json】（PFRemoteMacStatus）
//
//	{"version": 1, "mac_seen_at": "...", "network": "good|expensive|unknown|offline", "host": "<Mac 主机名>", "round_at": "..."}
//
// 【requests/<键>-<时间>-<随机>.json】（PFRemoteRequest）
// 文件名 = <键>-<UTC 时间 YYYYMMDDTHHMMSSZ>-<8 位小写十六进制随机>.json；写入必须原子（先写同目录的临时文件、再改名成这个名字；
// 不以 .json 结尾的名字 poll 忽略）。
//
//	{"version": 1, "kind": "fetch", "unit": "260403J1001", "paths": ["报告/a.docx"], "priority": "user", "created": "..."}
//
// kind：fetch（缺省）= Ubuntu 要这些文件的最新版本（paths 至少一条，最多 2000 条）；touched = Ubuntu 刚写了文件，
// Mac 下次 poll 优先处理这个单元（paths 可以为空 = 整个单元）。priority 目前只有 "user"。unit 必须等于文件名里的键。
// paths 是单元内相对路径（/ 分隔）。Mac 处理完用 apply 的 take_requests 取走并删除，Python 不要自己删别人的请求。
//
// 边界：
//   - 读不懂的文件（不是 JSON、有未知键、字段不合法）一律报告，不静默当作没有、也不自动删（take_requests 点名删才删）。
//   - 状态和请求文件只放进度信息，不放项目内容。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// meta 下的子目录和文件名。
const (
	PFRemoteRootsFile   = "roots.json"
	PFRemoteLeaseFile   = "coordinator.json"
	PFRemoteStagingName = "staging"
	PFRemoteRecycleName = "回收"
	PFRemoteCacheName   = "hashcache"
	PFRemoteStatusName  = "status"
	PFRemoteRequestName = "requests"
	pfrMacStatusFile    = "_mac.json"

	// 格式版本。
	PFRemoteStatusVersion  = 1
	PFRemoteRequestVersion = 1

	pfrMaxSmallFile = 1 << 20 // 请求、租约、白名单等小文件读取上限
	pfrMaxStatus    = 4 << 20 // 状态文件上限
)

// PFRemoteStagingDir 返回 meta 下的暂存区（Mac 的 SFTP 往这里传 <sha256>.part，路径用 / 拼）。
func PFRemoteStagingDir(meta string) string { return pfrJoin(meta, PFRemoteStagingName) }

// PFRemoteRecycleDir 返回 meta 下的同步回收区。
func PFRemoteRecycleDir(meta string) string { return pfrJoin(meta, PFRemoteRecycleName) }

func pfrJoin(meta string, elem ...string) string {
	return filepath.Join(append([]string{meta}, elem...)...)
}

// 单元键：归一化编号，ASCII 大写字母和数字。
var pfrUnitKeyRe = regexp.MustCompile(`^[A-Z0-9]{1,64}$`)

func pfrCheckUnitKey(k string) error {
	if !pfrUnitKeyRe.MatchString(k) {
		return fmt.Errorf("单元键 %q 不合法（应是归一化编号：大写字母和数字，1–64 个）", k)
	}
	return nil
}

// pfrDecodeFile 严格解码一个文件：不许未知键、不许多余内容。
func pfrDecodeFile(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("JSON 后面还有多余内容")
	}
	return nil
}

// pfrReadSmall 读一个小文件（不跟随符号链接，限长）。文件不存在返回的错误能用 errors.Is(err, fs.ErrNotExist) 认出。
func pfrReadSmall(p string, limit int64) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件", p)
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("%s 超过 %d 字节上限", p, limit)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s 超过 %d 字节上限", p, limit)
	}
	return b, nil
}

func pfrCheckTime(field, v string, required bool) error {
	if v == "" {
		if required {
			return fmt.Errorf("%s 不能为空", field)
		}
		return nil
	}
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return fmt.Errorf("%s %q 不是 RFC3339 时间", field, v)
	}
	return nil
}

func pfrCheckRelPath(field, p string) error {
	if err := validRel(p); err != nil {
		return fmt.Errorf("%s：%v", field, err)
	}
	if !utf8.ValidString(p) {
		return fmt.Errorf("%s %q 不是合法 UTF-8", field, p)
	}
	return nil
}

// ── roots.json ──────────────────────────────────────────

type pfrRootsFile struct {
	Version int      `json:"version"`
	Roots   []string `json:"roots"`
}

// PFRemoteWriteRoots 写 <meta>/roots.json（部署时用；0644）。
func PFRemoteWriteRoots(meta string, roots []string) error {
	f := pfrRootsFile{Version: 1, Roots: roots}
	if err := pfrCheckRoots(f.Roots); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(pfrJoin(meta, PFRemoteRootsFile), append(b, '\n'), 0o644)
}

// PFRemoteLoadRoots 读 <meta>/roots.json。不存在、读不懂、内容不合格都返回错误（没部署就不碰任何目录）。
func PFRemoteLoadRoots(meta string) ([]string, error) {
	b, err := pfrReadSmall(pfrJoin(meta, PFRemoteRootsFile), pfrMaxSmallFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("没有部署白名单 %s", pfrJoin(meta, PFRemoteRootsFile))
		}
		return nil, fmt.Errorf("读白名单失败: %w", err)
	}
	var f pfrRootsFile
	if err := pfrDecodeFile(b, &f); err != nil {
		return nil, fmt.Errorf("白名单 roots.json 读不懂: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("白名单 roots.json 版本 %d 不认识", f.Version)
	}
	if err := pfrCheckRoots(f.Roots); err != nil {
		return nil, fmt.Errorf("白名单 roots.json 不合格: %w", err)
	}
	return f.Roots, nil
}

func pfrCheckRoots(roots []string) error {
	if len(roots) == 0 {
		return errors.New("roots 为空")
	}
	seen := map[string]bool{}
	for _, r := range roots {
		if err := pfrCheckAbsDir("根", r); err != nil {
			return err
		}
		if seen[r] {
			return fmt.Errorf("根 %q 重复", r)
		}
		seen[r] = true
	}
	return nil
}

// pfrCheckAbsDir 要求是干净的绝对路径（已 Clean、合法 UTF-8、无 NUL、不是 /）。
func pfrCheckAbsDir(what, p string) error {
	switch {
	case p == "" || !filepath.IsAbs(p):
		return fmt.Errorf("%s %q 不是绝对路径", what, p)
	case strings.ContainsRune(p, 0) || !utf8.ValidString(p):
		return fmt.Errorf("%s %q 含 NUL 或不是合法 UTF-8", what, p)
	case filepath.Clean(p) != p:
		return fmt.Errorf("%s %q 不是干净的路径（有多余的 /、. 或 ..）", what, p)
	case p == "/":
		return fmt.Errorf("%s 不能是 /", what)
	}
	return nil
}

// ── coordinator.json ────────────────────────────────────

var pfrCoordIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (l *PFRemoteLease) check() error {
	if l.Version != 1 {
		return fmt.Errorf("租约版本 %d 不认识", l.Version)
	}
	if !pfrCoordIDRe.MatchString(l.ID) {
		return fmt.Errorf("协调者编号 %q 不合法", l.ID)
	}
	if len(l.Host) > 256 || !utf8.ValidString(l.Host) || strings.ContainsAny(l.Host, "\x00\n\r") {
		return errors.New("协调者主机名不合法")
	}
	if _, err := time.Parse(time.RFC3339, l.Expires); err != nil {
		return fmt.Errorf("租约到期时间 %q 不是 RFC3339", l.Expires)
	}
	return nil
}

func (l *PFRemoteLease) expiresAt() time.Time {
	t, _ := time.Parse(time.RFC3339, l.Expires)
	return t
}

// pfrReadLease 读租约。文件不存在返回 (nil, nil)；读不懂返回 error（调用方不得覆盖）。
func pfrReadLease(meta string) (*PFRemoteLease, error) {
	b, err := pfrReadSmall(pfrJoin(meta, PFRemoteLeaseFile), pfrMaxSmallFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var l PFRemoteLease
	if err := pfrDecodeFile(b, &l); err != nil {
		return nil, err
	}
	if err := l.check(); err != nil {
		return nil, err
	}
	return &l, nil
}

func pfrWriteLease(meta string, l PFRemoteLease) error {
	l.Version = 1
	if err := l.check(); err != nil {
		return err
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return writeFileAtomic(pfrJoin(meta, PFRemoteLeaseFile), append(b, '\n'), 0o644)
}

// ── 状态文件 ────────────────────────────────────────────

// 待传原因（status 里 pending 的 reason）。
const (
	PFRemoteReasonDeferred = "暂缓"
	PFRemoteReasonQueued   = "排队"
	PFRemoteReasonConflict = "冲突"
)

// PFRemotePending 是状态文件里一个还没对齐的文件。Size、Sha 是源边（pending_to_ubuntu 里是 Mac，pending_to_mac 里是 Ubuntu）这份的；
// UbSize/UbMtimeNs 是 Mac 上次看到的 Ubuntu 那份（pending_to_ubuntu 用来判断 Ubuntu 的文件自那以后有没有动）。
type PFRemotePending struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Sha        string `json:"sha,omitempty"`
	MacMtimeNs int64  `json:"mac_mtime_ns,omitempty"`
	UbSize     *int64 `json:"ub_size,omitempty"`
	UbMtimeNs  int64  `json:"ub_mtime_ns,omitempty"`
	Reason     string `json:"reason"`
}

// PFRemoteConflict 是状态文件里一条冲突记录。
type PFRemoteConflict struct {
	Path         string `json:"path"`
	ConflictPath string `json:"conflict_path"`
	Winner       string `json:"winner"` // mac | ubuntu
	At           string `json:"at,omitempty"`
}

// PFRemoteStatus 是 status/<键>.json 的内容。
type PFRemoteStatus struct {
	Version         int                `json:"version"`
	Unit            string             `json:"unit"`
	RoundAt         string             `json:"round_at,omitempty"`
	MacSeenAt       string             `json:"mac_seen_at,omitempty"`
	UbuntuRel       string             `json:"ubuntu_rel,omitempty"`
	Stopped         bool               `json:"stopped,omitempty"`
	Note            string             `json:"note,omitempty"`
	PendingToUbuntu []PFRemotePending  `json:"pending_to_ubuntu"`
	PendingToMac    []PFRemotePending  `json:"pending_to_mac"`
	Conflicts       []PFRemoteConflict `json:"conflicts"`
	Notes           []string           `json:"notes,omitempty"`
}

// 网络类别（status/_mac.json 的 network）。
const (
	PFRemoteNetGood      = "good"
	PFRemoteNetExpensive = "expensive"
	PFRemoteNetUnknown   = "unknown"
	PFRemoteNetOffline   = "offline"
)

// PFRemoteMacStatus 是 status/_mac.json 的内容。
type PFRemoteMacStatus struct {
	Version   int    `json:"version"`
	MacSeenAt string `json:"mac_seen_at"`
	Network   string `json:"network"`
	Host      string `json:"host,omitempty"`
	RoundAt   string `json:"round_at,omitempty"`
}

func (p *PFRemotePending) check(field string) error {
	if err := pfrCheckRelPath(field+".path", p.Path); err != nil {
		return err
	}
	if p.Size < 0 || (p.UbSize != nil && *p.UbSize < 0) {
		return fmt.Errorf("%s %q 的大小为负", field, p.Path)
	}
	if p.Sha != "" && !pfValidSHA(p.Sha) {
		return fmt.Errorf("%s %q 的 sha 不合法", field, p.Path)
	}
	switch p.Reason {
	case PFRemoteReasonDeferred, PFRemoteReasonQueued, PFRemoteReasonConflict:
	default:
		return fmt.Errorf("%s %q 的 reason %q 不认识", field, p.Path, p.Reason)
	}
	return nil
}

// Validate 检查状态文件内容是否合格（读写两端都用）。
func (s *PFRemoteStatus) Validate() error {
	if s.Version != PFRemoteStatusVersion {
		return fmt.Errorf("状态文件版本 %d 不认识", s.Version)
	}
	if err := pfrCheckUnitKey(s.Unit); err != nil {
		return err
	}
	for f, v := range map[string]string{"round_at": s.RoundAt, "mac_seen_at": s.MacSeenAt} {
		if err := pfrCheckTime(f, v, false); err != nil {
			return err
		}
	}
	if s.UbuntuRel != "" {
		if err := pfrCheckRelPath("ubuntu_rel", s.UbuntuRel); err != nil {
			return err
		}
	}
	for i := range s.PendingToUbuntu {
		if err := s.PendingToUbuntu[i].check("pending_to_ubuntu"); err != nil {
			return err
		}
	}
	for i := range s.PendingToMac {
		if err := s.PendingToMac[i].check("pending_to_mac"); err != nil {
			return err
		}
	}
	for _, c := range s.Conflicts {
		if err := pfrCheckRelPath("conflicts.path", c.Path); err != nil {
			return err
		}
		if err := pfrCheckRelPath("conflicts.conflict_path", c.ConflictPath); err != nil {
			return err
		}
		if c.Winner != "mac" && c.Winner != "ubuntu" {
			return fmt.Errorf("冲突 %q 的 winner %q 不认识", c.Path, c.Winner)
		}
		if err := pfrCheckTime("conflicts.at", c.At, false); err != nil {
			return err
		}
	}
	return nil
}

// Validate 检查 _mac.json 内容。
func (s *PFRemoteMacStatus) Validate() error {
	if s.Version != PFRemoteStatusVersion {
		return fmt.Errorf("状态文件版本 %d 不认识", s.Version)
	}
	if err := pfrCheckTime("mac_seen_at", s.MacSeenAt, true); err != nil {
		return err
	}
	if err := pfrCheckTime("round_at", s.RoundAt, false); err != nil {
		return err
	}
	switch s.Network {
	case PFRemoteNetGood, PFRemoteNetExpensive, PFRemoteNetUnknown, PFRemoteNetOffline:
	default:
		return fmt.Errorf("network %q 不认识", s.Network)
	}
	if len(s.Host) > 256 || !utf8.ValidString(s.Host) {
		return errors.New("host 不合法")
	}
	return nil
}

// norm 保证切片不是 nil（JSON 里写 [] 而不是 null，Python 读起来不用判空）。
func (s *PFRemoteStatus) norm() {
	if s.PendingToUbuntu == nil {
		s.PendingToUbuntu = []PFRemotePending{}
	}
	if s.PendingToMac == nil {
		s.PendingToMac = []PFRemotePending{}
	}
	if s.Conflicts == nil {
		s.Conflicts = []PFRemoteConflict{}
	}
}

// PFRemoteWriteStatus 原子写 <meta>/status/<键>.json（0644）。
func PFRemoteWriteStatus(meta string, s PFRemoteStatus) error {
	s.norm()
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(b) > pfrMaxStatus {
		return fmt.Errorf("状态文件超过 %d 字节上限", pfrMaxStatus)
	}
	return writeFileAtomic(pfrJoin(meta, PFRemoteStatusName, s.Unit+".json"), append(b, '\n'), 0o644)
}

// PFRemoteReadStatus 读 <meta>/status/<键>.json。文件不存在时 errors.Is(err, fs.ErrNotExist) 为真。
func PFRemoteReadStatus(meta, unit string) (PFRemoteStatus, error) {
	var s PFRemoteStatus
	if err := pfrCheckUnitKey(unit); err != nil {
		return s, err
	}
	b, err := pfrReadSmall(pfrJoin(meta, PFRemoteStatusName, unit+".json"), pfrMaxStatus)
	if err != nil {
		return s, err
	}
	if err := pfrDecodeFile(b, &s); err != nil {
		return PFRemoteStatus{}, fmt.Errorf("状态文件 %s 读不懂: %w", unit, err)
	}
	if err := s.Validate(); err != nil {
		return PFRemoteStatus{}, fmt.Errorf("状态文件 %s 不合格: %w", unit, err)
	}
	if s.Unit != unit {
		return PFRemoteStatus{}, fmt.Errorf("状态文件 %s 里的 unit 是 %q", unit, s.Unit)
	}
	s.norm()
	return s, nil
}

// PFRemoteWriteMacStatus 原子写 <meta>/status/_mac.json（0644）。
func PFRemoteWriteMacStatus(meta string, s PFRemoteMacStatus) error {
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(pfrJoin(meta, PFRemoteStatusName, pfrMacStatusFile), append(b, '\n'), 0o644)
}

// PFRemoteReadMacStatus 读 <meta>/status/_mac.json。
func PFRemoteReadMacStatus(meta string) (PFRemoteMacStatus, error) {
	var s PFRemoteMacStatus
	b, err := pfrReadSmall(pfrJoin(meta, PFRemoteStatusName, pfrMacStatusFile), pfrMaxSmallFile)
	if err != nil {
		return s, err
	}
	if err := pfrDecodeFile(b, &s); err != nil {
		return PFRemoteMacStatus{}, fmt.Errorf("_mac.json 读不懂: %w", err)
	}
	if err := s.Validate(); err != nil {
		return PFRemoteMacStatus{}, fmt.Errorf("_mac.json 不合格: %w", err)
	}
	return s, nil
}

// ── 请求文件 ────────────────────────────────────────────

// 请求种类。
const (
	PFRemoteReqFetch   = "fetch"
	PFRemoteReqTouched = "touched"
	// PFRemotePrioUser 是请求的优先级（目前只有这一种，对应传输层的 PFPrioUbuntu 之上的用户指令）。
	PFRemotePrioUser = "user"
)

// PFRemoteRequest 是 requests/ 下一个请求文件的内容。
type PFRemoteRequest struct {
	Version  int      `json:"version"`
	Kind     string   `json:"kind,omitempty"` // fetch（缺省）| touched
	Unit     string   `json:"unit"`
	Paths    []string `json:"paths,omitempty"`
	Priority string   `json:"priority"`
	Created  string   `json:"created"`
}

// PFRemoteRequestFile 是一个请求文件：文件名加内容。
type PFRemoteRequestFile struct {
	Name    string          `json:"name"`
	Request PFRemoteRequest `json:"request"`
}

// PFRemoteBadRequest 是读不懂的请求文件。
type PFRemoteBadRequest struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

var pfrReqNameRe = regexp.MustCompile(`^([A-Z0-9]{1,64})-(\d{8}T\d{6}Z)-([0-9a-f]{8})\.json$`)

const pfrReqTimeLayout = "20060102T150405Z"

// Validate 检查请求内容；Kind 为空按 fetch。
func (r *PFRemoteRequest) Validate() error {
	if r.Version != PFRemoteRequestVersion {
		return fmt.Errorf("请求版本 %d 不认识", r.Version)
	}
	if err := pfrCheckUnitKey(r.Unit); err != nil {
		return err
	}
	kind := r.Kind
	if kind == "" {
		kind = PFRemoteReqFetch
	}
	switch kind {
	case PFRemoteReqFetch:
		if len(r.Paths) == 0 {
			return errors.New("fetch 请求至少要有一个路径")
		}
	case PFRemoteReqTouched:
	default:
		return fmt.Errorf("请求种类 %q 不认识", r.Kind)
	}
	if len(r.Paths) > PFRemoteMaxPaths {
		return fmt.Errorf("请求里的路径超过 %d 条", PFRemoteMaxPaths)
	}
	for _, p := range r.Paths {
		if err := pfrCheckRelPath("paths", p); err != nil {
			return err
		}
	}
	if r.Priority != PFRemotePrioUser {
		return fmt.Errorf("优先级 %q 不认识", r.Priority)
	}
	return pfrCheckTime("created", r.Created, true)
}

// PFRemoteParseRequest 解析并校验一个请求文件的内容；name 非空时还核对 unit 等于文件名里的键。
func PFRemoteParseRequest(name string, b []byte) (PFRemoteRequest, error) {
	var r PFRemoteRequest
	if err := pfrDecodeFile(b, &r); err != nil {
		return PFRemoteRequest{}, err
	}
	if err := r.Validate(); err != nil {
		return PFRemoteRequest{}, err
	}
	if name != "" {
		m := pfrReqNameRe.FindStringSubmatch(name)
		if m == nil {
			return PFRemoteRequest{}, fmt.Errorf("文件名 %q 不符合 <键>-<时间>-<随机>.json", name)
		}
		if m[1] != r.Unit {
			return PFRemoteRequest{}, fmt.Errorf("文件名里的键 %s 与内容里的 unit %s 不同", m[1], r.Unit)
		}
	}
	return r, nil
}

// PFRemoteWriteRequest 原子写一个请求文件到 <meta>/requests/，返回文件名。Version、Created 缺省自动填；now 是文件名里的时间。
func PFRemoteWriteRequest(meta string, req PFRemoteRequest, now time.Time) (string, error) {
	if req.Version == 0 {
		req.Version = PFRemoteRequestVersion
	}
	if req.Created == "" {
		req.Created = now.UTC().Format(time.RFC3339)
	}
	if err := req.Validate(); err != nil {
		return "", err
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s.json", req.Unit, now.UTC().Format(pfrReqTimeLayout), hex.EncodeToString(rnd[:]))
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(pfrJoin(meta, PFRemoteRequestName, name), append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return name, nil
}

// PFRemoteListRequests 列出 <meta>/requests/ 下的请求文件，按文件名里的时间升序（同时间按名字）。
// 名字不符合格式的（含原子写留下的临时文件）忽略；符合格式但读不懂的进 bad，不删。
// 好的加坏的最多列 limit 个（<= 0 取 1000），还有没列的返回 more=true。目录不存在 = 没有请求。
func PFRemoteListRequests(meta string, limit int) (good []PFRemoteRequestFile, bad []PFRemoteBadRequest, more bool, err error) {
	if limit <= 0 {
		limit = pfrMaxRequestFiles
	}
	dir := pfrJoin(meta, PFRemoteRequestName)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []PFRemoteRequestFile{}, nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("读请求目录失败: %w", err)
	}
	type nm struct{ name, ts string }
	var names []nm
	for _, e := range ents {
		m := pfrReqNameRe.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		names = append(names, nm{e.Name(), m[2]})
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].ts != names[j].ts {
			return names[i].ts < names[j].ts
		}
		return names[i].name < names[j].name
	})
	good = []PFRemoteRequestFile{}
	for i, n := range names {
		if i >= limit {
			more = true
			break
		}
		b, rerr := pfrReadSmall(filepath.Join(dir, n.name), pfrMaxSmallFile)
		var r PFRemoteRequest
		if rerr == nil {
			r, rerr = PFRemoteParseRequest(n.name, b)
		}
		if rerr != nil {
			bad = append(bad, PFRemoteBadRequest{Name: n.name, Reason: rerr.Error()})
			continue
		}
		good = append(good, PFRemoteRequestFile{Name: n.name, Request: r})
	}
	return good, bad, more, nil
}

// PFRemoteTakeRequests 取走并删除点名的请求文件：读得懂的返回内容（taken），已经不在的进 missing，
// 读不懂的也删掉（Mac 没法处理，留着只会一直报）并进 bad。名字必须符合请求文件格式、直接在 requests/ 下。
func PFRemoteTakeRequests(meta string, names []string) (taken []PFRemoteRequestFile, missing []string, bad []PFRemoteBadFile, err error) {
	dir := pfrJoin(meta, PFRemoteRequestName)
	for _, n := range names {
		if !pfrReqNameRe.MatchString(n) {
			return taken, missing, bad, fmt.Errorf("%q 不是请求文件名", n)
		}
	}
	for _, n := range names {
		p := filepath.Join(dir, n)
		b, rerr := pfrReadSmall(p, pfrMaxSmallFile)
		if rerr != nil && errors.Is(rerr, fs.ErrNotExist) {
			missing = append(missing, n)
			continue
		}
		var r PFRemoteRequest
		if rerr == nil {
			r, rerr = PFRemoteParseRequest(n, b)
		}
		if derr := os.Remove(p); derr != nil && !errors.Is(derr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("删除请求文件 %s 失败: %w", n, derr))
			continue
		}
		if rerr != nil {
			bad = append(bad, PFRemoteBadFile{Name: n, Reason: rerr.Error()})
			continue
		}
		taken = append(taken, PFRemoteRequestFile{Name: n, Request: r})
	}
	if len(taken) > 0 || len(bad) > 0 {
		_ = pfSyncDir(dir)
	}
	return taken, missing, bad, err
}
