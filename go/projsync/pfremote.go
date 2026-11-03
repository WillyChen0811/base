package projsync

// 项目文件同步 T8：Mac 与 Ubuntu 之间的远程协议（技术设计 §0、§1.4、§5.2、§5.6、§6.2、§8.3、§12.1、§12.2）。
//
// Mac 的 wgtund 经现有 SSH 连接（RemoteRunner）在 Ubuntu 上跑三个 peerctl 子命令，每次一个 JSON 请求进 stdin、
// 一个 JSON 响应出 stdout：
//
//	projfiles-poll   一轮开头：续租协调者租约；列出根下的单元和每个单元的指纹；请求清单；剩余空间；暂存区残留。
//	projfiles-facts  对一个单元：扫描 + 带预算哈希（用 Ubuntu 本机的哈希缓存），返回扫描结果、全部原始名字、目录 dev+ino。
//	projfiles-apply  一批步骤，按顺序在 Ubuntu 本机用 PFApplier 执行（建目录、安装、删除、改名……），另有 verify_part、
//	                 write_status、take_requests、recycle_clean、staging_clean。
//
// 文件分工：本文件放协议类型、常量、错误；服务端在 pfremote_server.go（poll、租约、白名单）、pfremote_facts.go（facts 与
// 分页）、pfremote_apply.go（apply 步骤）；Mac 侧客户端在 pfremote_client.go；meta 目录里的文件格式与读写在 pfremote_files.go。
//
// 【协议约定】
//   - 退出码 0 = 协议层成功；非 0 = 协议层失败（stdin 超长、JSON 不合法、有未知字段、版本不符、内部 panic），此时 stdout 无输出，
//     原因写 stderr。协议层失败时 Mac 不得假定「没写过」（panic 可能发生在写动作之后）。
//   - 请求读得懂、但这次不做（roots.json 白名单越界、meta 不对、租约在别人手里、单元找不到、步骤不合法）→ 退出码 0，
//     响应里 Error 非空、别的字段无意义；服务端保证这种情况下一步写都没做（poll 例外：租约被占时只回租约信息）。
//   - 所有路径参数都是相对路径：单元 rel（相对 Ubuntu 根）和单元内 rel。Ubuntu 侧对照 <meta>/roots.json 白名单把根拼成
//     绝对路径并校验，越界一律拒绝。Meta、Root 是绝对路径，由 Mac 配置（hosts.json 的 projfiles 段）带来；
//     只有 roots.json 里写着的根才会被碰。
//   - 命令串只由「固定可执行路径 + 固定子命令名」组成（remoteCommand），所有参数只走 stdin。
//   - 通信失败 = 结果不确定：Mac 侧客户端返回的错误满足 errors.Is(err, ErrPFRemoteUncertain)，调用方停下这个单元本轮后续写入、
//     下一轮重新采集；客户端自己不重试（见 pfremote_client.go）。
//   - 输出受 sshterm 的 8MB 上限约束：facts 按字节分页（PageBytes），单元条目超过上限报「单元过大」，绝不截断后假装完整。

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 协议版本与子命令名。
const (
	PFRemoteVersion  = 1
	PFRemoteSubPoll  = "projfiles-poll"
	PFRemoteSubFacts = "projfiles-facts"
	PFRemoteSubApply = "projfiles-apply"
)

// 上限。
const (
	// PFRemoteMaxInput 是服务端读 stdin 的上限（一批 500 步、每步带前提，不会超过几 MB）。
	PFRemoteMaxInput = 8 << 20
	// PFRemoteMaxOutput 是 sshterm 单次调用 stdout 的上限（client.go 的 maxRunInputOut）；服务端自己的硬上限留 1MB 余量。
	PFRemoteMaxOutput  = 8 << 20
	pfrOutputLimit     = 7 << 20
	PFRemoteMaxSteps   = 500  // 一批 apply 最多的步数
	PFRemoteMaxPaths   = 2000 // 请求文件里 paths 的上限（§6.1）
	pfrMaxRequestFiles = 1000 // poll 一次最多列出的请求文件数（超出的标 RequestsMore）

	// PFRemoteDefaultMaxItems 是单元条目数上限（§4.2 第 12 条）：超过整个单元「过大」，facts 不返回内容。
	PFRemoteDefaultMaxItems = 50000
	// PFRemoteDefaultPageBytes 是 facts 一页响应内容的目标大小；总输出上限 8MB，留足余量。
	PFRemoteDefaultPageBytes = 4 << 20
	pfrMinPageBytes          = 1 << 10
	pfrMaxPageBytes          = 6 << 20
	// PFRemoteDefaultHashBudget 是 facts 缺省的哈希预算（§1.1、§5.2）。
	PFRemoteDefaultHashBudget = 60 * time.Second
	pfrMaxHashBudget          = 100 * time.Second // 加上扫描耗时要在 wgtund 单次调用的 2 分钟之内
	// PFRemoteDefaultLease 是协调者租约缺省时长。
	PFRemoteDefaultLease = 10 * time.Minute
	pfrMinLease          = 30 * time.Second
	pfrMaxLease          = time.Hour

	pfrMaxPages    = 200 // 客户端翻页上限（防服务端游标不前进）
	pfrLeftoverMax = 200 // poll 报告的暂存区残留最多条数
)

// ── 通用 ────────────────────────────────────────────────

// PFRemoteLease 是协调者租约（<meta>/coordinator.json，§8.3）：同一时间只允许一台 Mac 写。
// 同一结构也用在 poll 响应里说明现任持有者。Expires 是 RFC3339（UTC）。
type PFRemoteLease struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Host    string `json:"host"`
	Expires string `json:"expires"`
}

// PFRemoteLeaseResult 是 poll 对租约的结论。Granted=false 时 Holder 是现任持有者（未过期）或空（租约文件读不懂），Reason 说明。
type PFRemoteLeaseResult struct {
	Granted bool          `json:"granted"`
	Holder  PFRemoteLease `json:"holder"`
	Reason  string        `json:"reason,omitempty"`
}

// PFRemoteSummary 是一个单元扫描结果的计数（来自 PFScanResult.Summary，加条目总数）。
type PFRemoteSummary struct {
	Items              int   `json:"items"` // 条目总数（不含被排除的）
	Dirs               int   `json:"dirs"`
	Files              int   `json:"files"`
	Bytes              int64 `json:"bytes"`
	Unknown            int   `json:"unknown"` // 不明（所有原因）：有不明条目时不能只靠指纹跳过这个单元
	UnknownNotQuiet    int   `json:"unknown_not_quiet"`
	UnknownPlaceholder int   `json:"unknown_placeholder"`
	OutOfScope         int   `json:"out_of_scope"`
	Excluded           int   `json:"excluded"`
}

// PFRemoteSkip 是发现单元时跳过的目录（符号链接、读不了、层数用完仍认不出编号、名字不合格）。
type PFRemoteSkip struct {
	Rel        string `json:"rel"`
	Reason     string `json:"reason"`
	Unreadable bool   `json:"unreadable,omitempty"`
}

// PFRemoteLeftover 是 Ubuntu 暂存区里崩溃留下的私有文件（PFStagingLeftovers）。Kind=swap 的内容是用户文件，不能自动删。
type PFRemoteLeftover struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
}

// ── poll ────────────────────────────────────────────────

// PFRemotePollRequest 是 projfiles-poll 的请求。
type PFRemotePollRequest struct {
	Version int    `json:"version"`
	Meta    string `json:"meta"` // Ubuntu 的 meta 目录（绝对路径）
	Root    string `json:"root"` // Ubuntu 的同步根（绝对路径，必须在 <meta>/roots.json 里）
	// 协调者：CoordID 是这台 Mac 的协调者编号（wgtund 生成并保存，不变），CoordHost 给人看。
	CoordID      string `json:"coord_id"`
	CoordHost    string `json:"coord_host,omitempty"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"` // 续租时长，缺省 600，范围 30–3600
	QuietSeconds int    `json:"quiet_seconds,omitempty"` // 缺省 10；只影响 summary 里的「未静止」计数，不影响指纹
}

// PFRemotePollUnit 是 poll 里的一个单元。Dup 是同一编号在 Ubuntu 上认出多个目录（§14.2：登记单元按 rel 配对，
// 同编号的别的目录只报告、不停），Dup 的单元照常扫描、给指纹——登记的单元要靠指纹跳过没变化的轮次。
type PFRemotePollUnit struct {
	Code        string          `json:"code"`
	Rel         string          `json:"rel"`
	Dup         bool            `json:"dup,omitempty"`
	DupRels     []string        `json:"dup_rels,omitempty"`
	Others      []string        `json:"others,omitempty"`
	DirID       PFDirID         `json:"dir_id"`
	Fingerprint string          `json:"fingerprint,omitempty"` // PFScanResult.Fingerprint
	Summary     PFRemoteSummary `json:"summary"`
	Error       string          `json:"error,omitempty"` // 扫描这个单元失败的原因（没有指纹）
}

// PFRemotePollResponse 是 projfiles-poll 的响应。
//
// Error 非空：这次 poll 没做成（Lease 可能仍有内容），其余字段无意义。
// Lease.Granted=false：租约在别的协调者手里（或租约文件读不懂），只回租约信息，其余字段为空——别的协调者在写，这台 Mac 不该动 Ubuntu。
type PFRemotePollResponse struct {
	Version int                 `json:"version"`
	Error   string              `json:"error,omitempty"`
	NowNs   int64               `json:"now_ns"` // Ubuntu 当前时间（Unix 纳秒），算两边时钟差（G6）用
	Lease   PFRemoteLeaseResult `json:"lease"`

	Units            []PFRemotePollUnit `json:"units"`
	Skipped          []PFRemoteSkip     `json:"skipped,omitempty"`
	DiscoverExcluded int                `json:"discover_excluded,omitempty"` // 根下「.」开头的目录与链接个数
	Incomplete       bool               `json:"incomplete,omitempty"`        // 发现不完整：Units 可能不全，不能据此判定某单元「不见了」

	FreeBytes  int64 `json:"free_bytes"` // 根所在磁盘的剩余空间（普通用户可用）；-1 = 取不到
	TotalBytes int64 `json:"total_bytes"`

	Requests         []PFRemoteRequestFile `json:"requests"`
	BadRequests      []PFRemoteBadRequest  `json:"bad_requests,omitempty"`
	RequestsMore     bool                  `json:"requests_more,omitempty"` // 请求文件多于一次能列的数量，处理完再 poll
	StagingLeftovers []PFRemoteLeftover    `json:"staging_leftovers,omitempty"`
}

// ── facts ───────────────────────────────────────────────

// PFRemoteFactsRequest 是 projfiles-facts 的请求。一个单元的结果可能分好几页（More=true），
// 客户端带着上一页的 NextItem、NextName 作为游标继续请求；每一页都是一次完整扫描（stat 很便宜，哈希有缓存），
// 响应里的 Fingerprint 是整个扫描的指纹，客户端用它确认几页来自同一状态。
type PFRemoteFactsRequest struct {
	Version      int    `json:"version"`
	Meta         string `json:"meta"`
	Root         string `json:"root"`
	Code         string `json:"code"` // 单元键（归一化编号）
	Rel          string `json:"rel"`  // 单元相对根的路径
	QuietSeconds int    `json:"quiet_seconds,omitempty"`
	// HashBudgetMs 是本次调用（本页）的哈希时间预算，缺省 60000，最大 100000；没算完的文件标不明（hash_timeout）。
	HashBudgetMs int `json:"hash_budget_ms,omitempty"`
	// MaxItems 是单元条目数上限，缺省 50000；只能调小（测试用）。超过就返回 TooLarge、不带内容。
	MaxItems int `json:"max_items,omitempty"`
	// PageBytes 是一页内容的目标字节数，缺省 4MB（1KB–6MB）；一页至少一条记录。
	PageBytes int `json:"page_bytes,omitempty"`
	// AfterItem、AfterName 是翻页游标：前面已经收到的条目数、名字数（取上一页的 NextItem、NextName）；第一页都是 0。
	AfterItem int `json:"after_item,omitempty"`
	AfterName int `json:"after_name,omitempty"`
}

// PFRemoteItem 是 facts 里一个扫描条目的紧凑编码（短字段名，零值省略），与 PFScanItem 一一对应、可无损还原。
// 种类：f 文件、d 目录、l 符号链接、o 其他。State 省略 = ok。Dev 省略 = 与单元目录同一个设备。
type PFRemoteItem struct {
	Path      string  `json:"p"`
	Kind      string  `json:"k"`
	HasStat   bool    `json:"st,omitempty"`
	StatKind  string  `json:"sk,omitempty"` // Stat.Kind 与 Kind 不同时才有
	Size      int64   `json:"sz,omitempty"`
	MtimeNs   int64   `json:"mt,omitempty"`
	CtimeNs   int64   `json:"ct,omitempty"`
	Ino       uint64  `json:"in,omitempty"`
	Dev       *uint64 `json:"dv,omitempty"` // nil = 与单元目录同一个设备
	Mode      uint32  `json:"md,omitempty"`
	Flags     uint32  `json:"fl,omitempty"`
	State     string  `json:"s,omitempty"`
	Code      string  `json:"c,omitempty"`
	Reason    string  `json:"r,omitempty"`
	Hash      string  `json:"h,omitempty"`
	FromCache bool    `json:"fc,omitempty"`
	Stub      bool    `json:"sb,omitempty"`
}

// PFRemoteHashStats 是 PFHashStats 的线上形式（本页的哈希统计）。
type PFRemoteHashStats struct {
	CacheHits   int   `json:"cache_hits"`
	Hashed      int   `json:"hashed"`
	HashedBytes int64 `json:"hashed_bytes"`
	NewUnknown  int   `json:"new_unknown"`
	ElapsedMs   int64 `json:"elapsed_ms"`
}

// PFRemoteFactsResponse 是 projfiles-facts 的响应（一页）。
//
// Error 非空：没采集成，别的字段无意义。Exists=false：Rel 处没有这个编号的单元（单元目录不存在，或那里认不出这个编号）；
// Found 是 Ubuntu 上同编号的其他目录（G4 要用）；Incomplete 为真说明发现不完整，「没有」不可信。
// TooLarge：条目数超过 MaxItems，不带内容（TotalItems、Fingerprint、Summary 仍有）。
type PFRemoteFactsResponse struct {
	Version int    `json:"version"`
	Error   string `json:"error,omitempty"`
	NowNs   int64  `json:"now_ns"`

	Exists     bool     `json:"exists"`
	Found      []string `json:"found,omitempty"`
	Incomplete bool     `json:"incomplete,omitempty"`

	UnitPath string   `json:"unit_path,omitempty"` // 单元目录在 Ubuntu 上的绝对路径
	DirID    PFDirID  `json:"dir_id"`
	Dup      bool     `json:"dup,omitempty"`
	DupRels  []string `json:"dup_rels,omitempty"`

	TooLarge    bool            `json:"too_large,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	TotalItems  int             `json:"total_items"`
	TotalNames  int             `json:"total_names"`
	Excluded    int             `json:"excluded"`
	Summary     PFRemoteSummary `json:"summary"` // 扫描时（哈希之前）的计数；客户端拼好全部页后会按哈希之后的结果重算
	ScannedAtNs int64           `json:"scanned_at_ns"`

	// 本页内容：Items 是整个扫描结果（按路径字节序）里从 AfterItem 起的一段，Names 同理从 AfterName 起；
	// 几页拼起来就是全部。名字不是合法 UTF-8 的路径换成 \xNN 转义形式（Escaped 是个数），这种条目一定是超出范围。
	Items    []PFRemoteItem `json:"items"`
	Names    []PFRawName    `json:"names"` // 被排除的名字（含目录），带 mtime，给 Office 锁和 journal 判断用
	Escaped  int            `json:"escaped,omitempty"`
	More     bool           `json:"more,omitempty"`
	NextItem int            `json:"next_item,omitempty"` // More=true 时是下一页的 AfterItem
	NextName int            `json:"next_name,omitempty"` // More=true 时是下一页的 AfterName

	HashStats PFRemoteHashStats `json:"hash_stats"`
	Notes     []string          `json:"notes,omitempty"` // 如「哈希缓存损坏，已重算」
}

// ── apply ───────────────────────────────────────────────

// 步骤种类（PFRemoteStep.Op）。
const (
	PFRemoteOpUnitMkdir    = "unit_mkdir"      // 建单元目录本身（新项目首次传到 Ubuntu）
	PFRemoteOpMkdir        = "mkdir"           // PFApplier.Mkdir
	PFRemoteOpRmdir        = "rmdir"           // PFApplier.Rmdir
	PFRemoteOpInstallNew   = "install_new"     // PFApplier.InstallNew：暂存区 <sha>.part → 新建
	PFRemoteOpInstallOver  = "install_over"    // PFApplier.InstallOver
	PFRemoteOpDelete       = "delete"          // PFApplier.Delete（移入同步回收）
	PFRemoteOpRename       = "rename"          // PFApplier.Rename
	PFRemoteOpConflictName = "conflict_rename" // PFApplier.ConflictRename
	PFRemoteOpCopyNew      = "copy_new"        // PFApplier.CopyInstallNew（本机复制复用后新建）
	PFRemoteOpCopyOver     = "copy_over"       // PFApplier.CopyInstallOver
	PFRemoteOpVerifyPart   = "verify_part"     // 在 Ubuntu 本机算暂存区 part 的 sha256 和大小（给 T7 的 VerifyUp）
	PFRemoteOpWriteStatus  = "write_status"    // 写状态文件（§6.2）
	PFRemoteOpTakeRequests = "take_requests"   // 取走并删除已处理的请求文件
	PFRemoteOpRecycleClean = "recycle_clean"   // 清同步回收里超期的日期目录
	PFRemoteOpStagingClean = "staging_clean"   // 清暂存区里久没被引用的 part
)

// 复制来源的位置（PFRemoteCopySrc.Where）。
const (
	PFRemoteSrcUnit    = "unit"    // 单元里别处：Path 是单元内 rel
	PFRemoteSrcRecycle = "recycle" // 同步回收：Path 是回收记录的 Stored（相对回收区根）
	PFRemoteSrcStaging = "staging" // 暂存区：Path 是 <sha256>.part
)

// PFRemoteCopySrc 是 copy_new / copy_over 的复制来源（§3.3b）。
type PFRemoteCopySrc struct {
	Where  string `json:"where"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// PFRemoteStep 是 apply 批里的一步。不同种类用不同字段，其它字段必须为空（校验会拒绝多余的字段）：
//
//	unit_mkdir      （无）
//	mkdir, rmdir    path
//	install_new     path, sha256, size, mtime_ns            part = 暂存区 <sha256>.part
//	install_over    path, sha256, size, mtime_ns, expect
//	delete          path, expect, reason                    reason：覆盖/删除/冲突（进回收的原因）
//	rename          path（来源）, to（目标，单元内 rel）, expect
//	conflict_rename path, to（冲突副本名，只是名字不含目录）, expect
//	copy_new        src, path, mtime_ns
//	copy_over       src, path, mtime_ns, expect
//	verify_part     sha256
//	write_status    status 或 mac_status（二选一）
//	take_requests   names
//	recycle_clean   days                                    不足 7 天按 7 天算
//	staging_clean   max_age_hours（缺省 168）, keep（不能删的 sha256 列表）
//
// mtime_ns 必填且不为 0（0 当作没填）。Ext 留给后续任务的步骤（台账 ledger_stage / ledger_install 等，见 pfrRegisterOp）。
type PFRemoteStep struct {
	Op          string             `json:"op"`
	Path        string             `json:"path,omitempty"`
	To          string             `json:"to,omitempty"`
	SHA256      string             `json:"sha256,omitempty"`
	Size        int64              `json:"size,omitempty"`
	MtimeNs     int64              `json:"mtime_ns,omitempty"`
	Expect      *PFExpect          `json:"expect,omitempty"`
	Reason      string             `json:"reason,omitempty"`
	Src         *PFRemoteCopySrc   `json:"src,omitempty"`
	Status      *PFRemoteStatus    `json:"status,omitempty"`
	MacStatus   *PFRemoteMacStatus `json:"mac_status,omitempty"`
	Names       []string           `json:"names,omitempty"`
	Days        int                `json:"days,omitempty"`
	MaxAgeHours int                `json:"max_age_hours,omitempty"`
	Keep        []string           `json:"keep,omitempty"`
	Ext         json.RawMessage    `json:"ext,omitempty"`
}

// PFRemoteApplyRequest 是 projfiles-apply 的请求。
//
// Code、Rel 指定单元（任何要动单元的步骤都需要；只有 verify_part、write_status、take_requests、recycle_clean、staging_clean
// 的批可以不带）。CoordID 是协调者编号：租约在别的协调者手里且未过期时整批拒绝。Round 是轮次号（写进回收记录）。
// StopOnFail=true：任何一步结果不是「完成」就停止后面的步骤（缺省只在「放弃且不确定」或配置错误时停）。
type PFRemoteApplyRequest struct {
	Version    int            `json:"version"`
	Meta       string         `json:"meta"`
	Root       string         `json:"root"`
	CoordID    string         `json:"coord_id"`
	Code       string         `json:"code,omitempty"`
	Rel        string         `json:"rel,omitempty"`
	Round      string         `json:"round,omitempty"`
	StopOnFail bool           `json:"stop_on_fail,omitempty"`
	Steps      []PFRemoteStep `json:"steps"`
}

// PFRemoteCleanInfo 是 recycle_clean / staging_clean 的结果。
type PFRemoteCleanInfo struct {
	Days    int      `json:"days,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Files   int      `json:"files,omitempty"`
	Bytes   int64    `json:"bytes,omitempty"`
	Kept    int      `json:"kept"`
}

// PFRemoteBadFile 是 take_requests 里读不懂（但仍被删除）的请求文件。
type PFRemoteBadFile struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// PFRemoteStepResult 是一步的结果。写原语类的步骤 Apply 是 T6 的 PFApplyResult 原样；
// Error 非空表示这一步没能执行（Config=true 是配置错误：参数、暂存区/回收区与根不同文件系统……），此时 Apply 为空。
type PFRemoteStepResult struct {
	Op      string                `json:"op"`
	Apply   *PFApplyResult        `json:"apply,omitempty"`
	Error   string                `json:"error,omitempty"`
	Config  bool                  `json:"config,omitempty"`
	Sum     string                `json:"sum,omitempty"`     // verify_part：算出来的 sha256（只报告，不比较）
	Size    int64                 `json:"size,omitempty"`    // verify_part：读到的字节数
	Taken   []PFRemoteRequestFile `json:"taken,omitempty"`   // take_requests：取走并已删除的请求
	Missing []string              `json:"missing,omitempty"` // take_requests：本来就不在了的文件名
	Bad     []PFRemoteBadFile     `json:"bad,omitempty"`     // take_requests：读不懂但已删除的文件
	Clean   *PFRemoteCleanInfo    `json:"clean,omitempty"`
	Ext     json.RawMessage       `json:"ext,omitempty"`
}

// PFRemoteApplyResponse 是 projfiles-apply 的响应。
//
// Error 非空：整批没执行（一步都没做）。否则 Results 是已执行的步骤的结果（按顺序，与请求的前 len(Results) 步一一对应）：
// Stopped=false 时每一步都执行了；Stopped=true 时 StopAt 是触发停止的那一步的下标（它的结果是 Results 的最后一项），
// 后面的步骤没有执行。停止的原因：某一步「放弃且 Uncertain」、出现配置错误、StopOnFail 且某步没完成、请求被取消。
type PFRemoteApplyResponse struct {
	Version    int                  `json:"version"`
	Error      string               `json:"error,omitempty"`
	Results    []PFRemoteStepResult `json:"results"`
	Stopped    bool                 `json:"stopped,omitempty"`
	StopAt     int                  `json:"stop_at,omitempty"`
	StopReason string               `json:"stop_reason,omitempty"`
}

// ── 错误 ────────────────────────────────────────────────

// ErrPFRemoteUncertain 表示通信或执行层失败、响应读不懂或不合法：远端可能已经做了一部分，结果不确定。
// 调用方必须停下这个单元本轮后续写入、下一轮重新采集，不得假定没写过，更不得盲目重试（remote.go 的原则）。
var ErrPFRemoteUncertain = errors.New("projsync: 远端结果不确定")

// ErrPFRemoteLeaseHeld 表示租约在别的协调者手里（未过期）。
var ErrPFRemoteLeaseHeld = errors.New("projsync: 协调者租约在别的 Mac 手里")

// ErrPFRemoteUnstable 表示 facts 的几页对不上（采集过程中单元在变）：没有写过任何东西，本轮跳过这个单元、下一轮再采。
var ErrPFRemoteUnstable = errors.New("projsync: 远端采集的几页对不上（单元在变）")

// PFRemoteErrKind 是 PFRemoteError 的类别。
type PFRemoteErrKind int

const (
	// PFRemoteErrComm 通信/执行层失败、退出码非 0、响应不是合法 JSON 或字段组合不合法：结果不确定。
	PFRemoteErrComm PFRemoteErrKind = iota + 1
	// PFRemoteErrRefused 对方明确说这次没做（响应里的 Error）：越界、没部署、单元找不到……什么都没写。
	PFRemoteErrRefused
	// PFRemoteErrLease 租约在别人手里。
	PFRemoteErrLease
	// PFRemoteErrUnstable facts 几页对不上。
	PFRemoteErrUnstable
	// PFRemoteErrLocal 请求在本机就不合格（步骤校验、请求过大），没有发出去。
	PFRemoteErrLocal
)

// PFRemoteError 是客户端返回的错误。
type PFRemoteError struct {
	Sub    string // 子命令名
	Kind   PFRemoteErrKind
	Msg    string
	Err    error          // 底层错误
	Holder *PFRemoteLease // PFRemoteErrLease 时是现任持有者
}

func (e *PFRemoteError) Error() string {
	s := fmt.Sprintf("%s：%s", e.Sub, e.Msg)
	if e.Err != nil {
		s += "：" + e.Err.Error()
	}
	return s
}

func (e *PFRemoteError) Unwrap() error { return e.Err }

// Is 让 errors.Is 认出 ErrPFRemoteUncertain（通信类）、ErrPFRemoteLeaseHeld、ErrPFRemoteUnstable。
func (e *PFRemoteError) Is(target error) bool {
	switch target {
	case ErrPFRemoteUncertain:
		return e.Kind == PFRemoteErrComm
	case ErrPFRemoteLeaseHeld:
		return e.Kind == PFRemoteErrLease
	case ErrPFRemoteUnstable:
		return e.Kind == PFRemoteErrUnstable
	}
	return false
}

// PFRemoteIsUncertain 报告 err 是不是「结果不确定」。
func PFRemoteIsUncertain(err error) bool { return errors.Is(err, ErrPFRemoteUncertain) }
