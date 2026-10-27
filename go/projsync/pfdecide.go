package projsync

// 项目文件同步的「三方判定」（技术设计 §3 全部、§4.2 第 3、4、8、9、13 条、§7 第一版、§12.2）。
//
// 本文件只有一个确定性的纯函数 PFDecide：输入「单元的 base + Mac 扫描 + Ubuntu 扫描 + 两边单元目录 ID
// + 时钟差 + 原始名字列表 + 当前时间」，输出这一轮的计划（动作清单、跳过清单、单元级结论）。
// 不读文件、不联网、不读时钟（时间由调用方传入）；同一输入（条目顺序可以任意打乱）输出逐字节相同。
// 执行归 T6（写原语）、T9（一轮编排）、T10（台账）；本文件的每个动作都带着「判定时看到的前提」，
// 执行前要逐项核对，不符就放弃这一步（下一轮重新采集重新判定）。
//
// 【三条硬标准】不写错一边、不覆盖未同步的改动、不丢数据。落到判定上就是：
//   - 只看 State=OK 且有哈希的条目；不明（Unknown）、超出范围（OutOfScope）一律「本轮跳过，两边不动、base 不变」。
//   - 删除只在「base 里有、这一边没改过、另一边确实没有」时才出（表第 4、5 行）；没看的子树下 base 有而扫描没有，
//     不是删除（用 SubtreeSkipped 认），进跳过清单。
//   - 覆盖只在「被覆盖的一边等于 base」时才出（表第 2、3 行）；两边都改成不同内容走冲突（两份都留）。
//   - 首次规则（没有 base、G2、G5）只用表里第 11–14 行，绝不推断删除。
//   - 判不准的（撞名、两边对不上、一边文件一边目录、Office 锁在目标边、历史版本里同名不同内容……）一律跳过并报告原因。
//
// 【动作清单的顺序就是执行顺序】按 (Phase, 路径) 排序：
//
//	Phase 0 建目录（路径升序，上级先建；Path=="" 是单元根目录本身）
//	Phase 1 文件动作与只改 base 的动作（路径升序，互相没有先后依赖）
//	Phase 2 空了才删的目录（路径降序，深的先删）
//	Phase 3 台账（排在同单元所有快照文件动作之后）
//
// 【各种动作怎么执行】（Src=内容来自哪一边，Dst=动手的那一边；单边动作只有 Dst）
//
//	copy_new    把 Src 边 Path 的内容拷到 Dst 边新建。前提：Src 边 Path 仍是 SrcExpect；Dst 边 Path 不存在。
//	copy_over   同上但 Dst 边 Path 必须仍是 DstExpect（就是 base 里记的那份），旧的先进同步回收再换上新的。
//	delete      Dst 边 Path 必须仍是 DstExpect，整个移入同步回收（不是真删）。
//	rename_local Dst 边本机把 From 改名为 Path（§3.3(a)）：From 仍是 FromExpect、Path 不存在才做；不进回收，不传输。
//	            Src/SrcExpect 是另一边 Path 现状，只供核对。
//	clone_local Dst 边本机把 From 复制成 Path（§3.3(b)，Mac 用 clonefile）：From 仍是 FromExpect、Path 不存在；
//	            复制出的文件仍要核对哈希等于 SrcExpect.SHA256。不传输。
//	conflict    见 PFConflictStep。
//	mkdir       Dst 边建目录 Path（须不存在）。
//	rmdir       Dst 边目录 Path 仍是 DstExpect（ino）且 IfEmpty：执行时再判一次，空才删，非空就保留（不算失败，base 不动）。
//	base_set / base_del  只改 base，没有文件操作。
//	ledger_copy 台账一对（执行归 T10），见 PFLedgerStep。
//	base_ledger 台账两边已一致，只更新 base 的台账记录。
//
// 动作成功后，执行者把该动作的 Base 变更写进 base（同一个动作的多条变更要放在同一次 Apply 里）；
// Base 里 FillMac / FillUb 为 true 的那一边的 PFSideStat 是预估值（mtime 按源设置，ino 为 0），
// 执行者装好文件后要重新 stat 目标文件把 mtime、ino 填进去。台账动作成功后写 BaseLedgerHash。
//
// 【本文件对 T10 的约定】台账合并哈希 = sha(sqlite) 的 64 位小写十六进制 + sha(json) 的 64 位小写十六进制（直接连接，
// 共 128 个字符），见 PFLedgerPairHash。base.Ledger().Hash 必须用同一种写法，判定才比得上。
//
// 【判不了的边界】
//   - 只认扫描里的条目；回收区、暂存区里已有的内容（§3.3(b) 的「回收区或暂存区已有内容 H」）本函数看不到，
//     不做复用，那种情况照常传输（T7 自己会复用已落地的 part）。
//   - 冲突新旧只按原始 mtime 比较，不按时钟差修正（时钟差超过 5 分钟就不比，Mac 占原名）。
//   - 单元里有多处「历史版本」台账（多于一对）时，台账一律跳过，不自动同步。
//   - 「已释放」的单元（§8.4）不在本函数里认，由调用方在 G1 之前处理。

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------------------------
// 公开类型
// ---------------------------------------------------------------------------------------------

// PFSide 是两台机器之一。
type PFSide string

const (
	PFSideMac    PFSide = "mac"
	PFSideUbuntu PFSide = "ubuntu"
)

// Other 返回另一边。
func (s PFSide) Other() PFSide {
	if s == PFSideMac {
		return PFSideUbuntu
	}
	return PFSideMac
}

// Machine 返回冲突副本名和报告里用的机器名。
func (s PFSide) Machine() string {
	if s == PFSideMac {
		return "Mac"
	}
	return "Ubuntu"
}

// PFRawName 是单元里看到的一个名字（含被排除的，如 ~$ 锁、.~lock.*#、*-journal），用来认 Office 锁和台账 journal。
// Path 是相对单元目录的路径（/ 分隔）；扫描层用 PFScanOptions.OnExcluded 就能收到被排除的那部分，
// 没被排除的已经在扫描结果里，不必重复给（给了也无妨）。
// MtimeNs 只有锁文件要用（超过 12 小时当残留，§4.2 第 9 条）：0 表示不知道，按「仍是锁」处理（保守）。
type PFRawName struct {
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir,omitempty"`
	MtimeNs int64  `json:"mtime_ns,omitempty"`
}

// PFDecideInput 是 PFDecide 的输入。
type PFDecideInput struct {
	// Unit 是单元键，只用来标注输出。
	Unit string
	// Base 是单元的 base 视图。nil = 没有可用的 base（缺失或损坏），按首次规则。
	Base PFBaseView
	// BaseProblem 只在 Base==nil 时看：非空表示 base 存在但损坏、不可信（G5），内容是 T4 给的原因；空表示本来就没有 base。
	BaseProblem string
	// Mac、Ubuntu 是两边的扫描结果（已哈希）。nil = 这一边的单元目录不存在（不是「扫描出错」——出错时不要调本函数，
	// 本轮跳过这个单元）。条目顺序可以是任意的，函数内部会重新排序。
	Mac, Ubuntu *PFScanResult
	// MacDirID、UbuntuDirID 是两边单元目录当前的 dev+ino；G2 只比 ino。目录不存在时传零值。
	MacDirID, UbuntuDirID PFDirID
	// MacRel、UbuntuRel 是两边项目文件夹当前相对根的路径，与 base 记的不同就是 G4（改名/挪位置）。空串 = 不检查。
	MacRel, UbuntuRel string
	// MacDup、UbuntuDup 是同一编号在那一边认出的其他目录（PFFoundUnit.DupRels）。
	// MacDup 非空仍是 G4 停；UbuntuDup 自 §14.2 起只报告（没配置的目录，提醒合并，不停）。
	MacDup, UbuntuDup []string
	// ClockSkew 是两边时钟差（Mac 的钟减 Ubuntu 的钟，正负都行），只看绝对值：超过 5 分钟就是 G6。
	// ClockUnknown 为 true（测不出来）同样按 G6 处理。
	ClockSkew    time.Duration
	ClockUnknown bool
	// MacNames、UbuntuNames 是两边的原始名字（含被排除的），见 PFRawName。
	MacNames, UbuntuNames []PFRawName
	// Now 是当前时间（锁文件 12 小时规则用）；零值 = 不判 12 小时规则，所有锁都当真锁。
	Now time.Time
	// Loc 是冲突副本名里时间的时区，nil 取 time.Local。
	Loc *time.Location
}

// PFExpect 的定义在 pfapply.go（T5 判定产出、T6 写原语核对的是同一个类型）。

// PFActionKind 是动作种类。
type PFActionKind string

const (
	PFActCopyNew    PFActionKind = "copy_new"
	PFActCopyOver   PFActionKind = "copy_over"
	PFActDelete     PFActionKind = "delete"
	PFActRename     PFActionKind = "rename_local"
	PFActClone      PFActionKind = "clone_local"
	PFActConflict   PFActionKind = "conflict"
	PFActMkdir      PFActionKind = "mkdir"
	PFActRmdir      PFActionKind = "rmdir"
	PFActBaseSet    PFActionKind = "base_set"
	PFActBaseDel    PFActionKind = "base_del"
	PFActLedgerCopy PFActionKind = "ledger_copy"
	PFActBaseLedger PFActionKind = "base_ledger"
)

// 执行阶段（见文件头）。
const (
	PFPhaseMkdir  = 0
	PFPhaseFiles  = 1
	PFPhaseRmdir  = 2
	PFPhaseLedger = 3
)

// PFBaseChange 是动作成功后要写进 base 的一条变更。
// FillMac / FillUb 为 true 表示那一边的 PFSideStat 是预估的（mtime 照源，ino 为 0），执行者要在装好文件后重新 stat 填写。
type PFBaseChange struct {
	Path    string      `json:"path"`
	Del     bool        `json:"del,omitempty"`
	Entry   PFBaseEntry `json:"entry"`
	FillMac bool        `json:"fill_mac,omitempty"`
	FillUb  bool        `json:"fill_ub,omitempty"`
}

// PFConflictStep 是冲突动作（§3.4）的补充信息。动作本身：Src=胜出一边（占原名），Dst=落败一边，
// SrcExpect/DstExpect=两边原名文件的现状。执行顺序：
//
//  1. 落败边：把 Path 原子改名为 ConflictPath（Path 仍是 DstExpect、ConflictPath 不存在）；
//  2. 落败边：在 Path 处新建胜出版本（从胜出边 Path 拷来，胜出边 Path 仍是 SrcExpect）；
//  3. 胜出边：在 ConflictPath 处新建落败版本（从落败边的 ConflictPath 拷来，哈希是 DstExpect.SHA256，胜出边 ConflictPath 不存在）。
//
// base：Path 记胜出哈希，ConflictPath 记落败哈希（两条在同一次 Apply 里写）。
// 任一步前提不符就停在那一步（每一步都不丢数据），下一轮重新判定。
type PFConflictStep struct {
	Winner       PFSide `json:"winner"`
	Loser        PFSide `json:"loser"`
	ConflictPath string `json:"conflict_path"` // 冲突副本的路径（与原名同目录，两边同名）
	Why          string `json:"why"`           // 为什么这一边占原名
}

// PFLedgerStep 是台账（§7）那一对文件的补充信息。动作：Src 边的一对拷到 Dst 边。
// 前提：Src 边两个文件仍是 SrcSQLite/SrcJSON；Dst 边两个文件仍是 DstSQLite/DstJSON（Absent 表示不存在）。
// 执行细节（持历史版本/.lock 的 flock、journal 存在就推迟、快照先于台账、旧的一对进回收）归 T10。
type PFLedgerStep struct {
	Dir       string    `json:"dir"`
	SQLite    string    `json:"sqlite"`
	JSON      string    `json:"json"`
	Hash      string    `json:"hash"` // Src 边的合并哈希，装好后 Dst 边应当等于它
	SrcSQLite *PFExpect `json:"src_sqlite"`
	SrcJSON   *PFExpect `json:"src_json"`
	DstSQLite *PFExpect `json:"dst_sqlite"`
	DstJSON   *PFExpect `json:"dst_json"`
}

// PFAction 是计划里的一个动作。
type PFAction struct {
	Kind  PFActionKind `json:"kind"`
	Phase int          `json:"phase"`
	// Row 是判定表的行号（1–17），0 表示不是表里的行（建上级目录、台账、历史版本规则等）。
	Row  int    `json:"row,omitempty"`
	Path string `json:"path"`
	Src  PFSide `json:"src,omitempty"`
	Dst  PFSide `json:"dst,omitempty"`
	// SrcExpect / DstExpect 是 Src / Dst 边 Path 判定时的现状；nil 表示不适用。
	SrcExpect *PFExpect `json:"src_expect,omitempty"`
	DstExpect *PFExpect `json:"dst_expect,omitempty"`
	// From / FromExpect 只有 rename_local、clone_local 用：Dst 边本机的来源路径和现状。
	From       string    `json:"from,omitempty"`
	FromExpect *PFExpect `json:"from_expect,omitempty"`
	// IfEmpty 只有 rmdir：执行时再判一次，空才删。
	IfEmpty  bool            `json:"if_empty,omitempty"`
	Conflict *PFConflictStep `json:"conflict,omitempty"`
	Ledger   *PFLedgerStep   `json:"ledger,omitempty"`
	// Base 是动作成功后要写进 base 的变更；BaseLedgerHash 非空时还要把 base 的台账记录 Hash 设成它。
	Base           []PFBaseChange `json:"base,omitempty"`
	BaseLedgerHash string         `json:"base_ledger_hash,omitempty"`
	Reason         string         `json:"reason"`
	// Notice 非空表示动作成功后要报告给用户的事（改的赢、源边有 Office 锁等）。
	Notice string `json:"notice,omitempty"`
}

// 跳过原因代码（PFSkip.Code）。
const (
	PFSkipUnknown         = "unknown"                  // 不明（表第 15、16 行）
	PFSkipOutOfScope      = "out_of_scope"             // 超出范围（扫描判的、路径不合规）
	PFSkipSubtree         = "subtree_not_seen"         // 落在没看的子树里（不是删除）
	PFSkipAncestor        = "ancestor_skipped"         // 上级目录被跳过，整棵子树跟着跳过
	PFSkipSameSideClash   = "same_side_clash"          // 同一边撞名（§4.2 第 3 条）
	PFSkipCrossSideClash  = "cross_side_clash"         // 两边对不上（§4.2 第 4 条）
	PFSkipKindMismatch    = "kind_mismatch"            // 一边文件一边目录（表第 17 行）
	PFSkipOfficeLock      = "office_lock_target"       // Office 锁在目标边，等关闭（§4.2 第 9 条）
	PFSkipHistoryDiffers  = "history_snapshot_differs" // 历史版本里快照同名不同内容（§7）
	PFSkipLedgerBoth      = "ledger_both_changed"      // 台账两边都变了，待合并（§7）
	PFSkipLedgerUnknown   = "ledger_unknown"           // 台账一对不完整或有一边不明
	PFSkipLedgerJournal   = "ledger_journal"           // 台账旁有 journal / wal，正在写，推迟
	PFSkipLedgerMulti     = "ledger_multiple"          // 单元里有多处台账，不自动同步
	PFSkipDirKept         = "dir_kept"                 // 一边删了目录但另一边里还有没同步的东西，保留
	PFSkipAncestorMissing = "ancestor_missing"         // 目标边上级不是目录（防御，正常不会出现）
	PFSkipConflictName    = "conflict_name"            // 生成不出冲突副本名
)

// PFSkip 是本轮跳过的一个路径：两边都不动，base 不变。
type PFSkip struct {
	Path string `json:"path"`
	// Side 是问题出在哪一边；两边都有问题或不分边时为空。
	Side   PFSide `json:"side,omitempty"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// PFPlanNote 是计划里要报告但不属于某个动作的事（如 12 小时以上的锁文件当残留）。
type PFPlanNote struct {
	Path string `json:"path,omitempty"`
	Code string `json:"code"`
	Text string `json:"text"`
}

// 单元级关卡的结论。
const (
	PFGateStop      = "stop"       // 整个单元停下，不出动作
	PFGateFirstTime = "first_time" // 本轮按首次规则
	PFGateReport    = "report"     // 只报告，不拦
)

// PFGate 是一个关卡命中的结果。Code：G1、G2、G3、G4、G5、G6，或 "no_base"（本来就没有 base）。
type PFGate struct {
	Code   string `json:"code"`
	Effect string `json:"effect"`
	Reason string `json:"reason"`
}

// PFVerdict 是单元级结论。
type PFVerdict struct {
	Gates []PFGate `json:"gates,omitempty"`
	// Stopped：G1 或 G4 命中，整个单元不出动作。
	Stopped bool `json:"stopped"`
	// FirstTime：本轮按首次规则（没有 base、G2、G5），只用表第 11–14 行，绝不推断删除。
	FirstTime bool `json:"first_time"`
	// ResetBase：首次规则但磁盘上还有一份可读的旧 base（G2）：执行者先 base.Reset(Plan.Meta)，再写本轮的 base 变更，
	// 免得旧条目留下来，之后某一边碰巧新建同内容文件被误判成「另一边删了」。
	ResetBase bool `json:"reset_base"`
	// ClockUntrusted：G6，冲突时 Mac 占原名。
	ClockUntrusted bool `json:"clock_untrusted"`
	// MacDeletes、UbuntuDeletes 是这一轮 Mac、Ubuntu 要删（移入回收）的文件个数（G3：只报告不拦）。
	MacDeletes    int `json:"mac_deletes"`
	UbuntuDeletes int `json:"ubuntu_deletes"`
}

// PFPlan 是一个单元这一轮的计划。
type PFPlan struct {
	Unit    string       `json:"unit"`
	Verdict PFVerdict    `json:"verdict"`
	Meta    PFBaseMeta   `json:"meta"` // 本轮结束后写进 base 的单元级元数据（两边 rel 和单元目录 ID）
	Actions []PFAction   `json:"actions,omitempty"`
	Skips   []PFSkip     `json:"skips,omitempty"`
	Notes   []PFPlanNote `json:"notes,omitempty"`
}

// PFLedgerPairHash 是台账一对文件的合并哈希：sha(sqlite) 与 sha(json) 的小写十六进制直接连接。
func PFLedgerPairHash(sqliteSHA, jsonSHA string) string { return sqliteSHA + jsonSHA }

const (
	pfLedgerSQLite = "档案台账.sqlite3"
	pfLedgerJSON   = "档案索引.json"
	// pfStaleLock 是锁文件超过多久当残留（§4.2 第 9 条）。
	pfStaleLock = 12 * time.Hour
	// pfClockLimit 是时钟差的上限（G6）。
	pfClockLimit = 5 * time.Minute
	// pfConflictTie 是冲突时 mtime 差在多少以内算平手（Mac 占原名）。
	pfConflictTie = 2 * time.Second
)

// ---------------------------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------------------------

// pfdParent 返回相对路径的上级目录（根下的返回 ""）。
func pfdParent(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

// pfdAncestors 返回 rel 的各级上级目录，浅的在前（不含根、不含自己）。
func pfdAncestors(rel string) []string {
	var out []string
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			out = append(out, rel[:i])
		}
	}
	return out
}

// pfdInHistory 报告路径（文件或目录）是否落在「历史版本」里：上级的某一段等于「历史版本」。
func pfdInHistory(rel string) bool { return pfInHistoryDir(pfdParent(rel)) }

func pfdSideStatOf(it PFScanItem) PFSideStat {
	return PFSideStat{MtimeNs: it.Stat.MtimeNs, Ino: it.Stat.Ino}
}

// pfdExpectOf 把扫描条目转成前提：文件核对 size、sha256、mtime、ino；目录只核对 ino。
func pfdExpectOf(it PFScanItem) *PFExpect {
	if it.Kind == PFKindDir {
		return &PFExpect{T: PFBaseTypeDir, Ino: it.Stat.Ino}
	}
	return &PFExpect{T: PFBaseTypeFile, Size: it.Stat.Size, SHA256: it.Hash, MtimeNs: it.Stat.MtimeNs, Ino: it.Stat.Ino}
}

func pfdNone() *PFExpect { return &PFExpect{Absent: true} }

// pfdSetSide 设置条目里某一边的 stat。
func pfdSetSide(e *PFBaseEntry, side PFSide, st PFSideStat) {
	if side == PFSideMac {
		e.Mac = st
	} else {
		e.Ub = st
	}
}

// pfdEntry 用 known 边的条目建 base 条目：known 边是真实 stat，另一边是预估（mtime 照源、ino 为 0）。
func pfdEntry(known PFSide, it PFScanItem) PFBaseEntry {
	e := PFBaseEntry{}
	if it.Kind == PFKindDir {
		e.T = PFBaseTypeDir
	} else {
		e.T, e.Size, e.SHA256 = PFBaseTypeFile, it.Stat.Size, it.Hash
	}
	st := pfdSideStatOf(it)
	pfdSetSide(&e, known, st)
	pfdSetSide(&e, known.Other(), PFSideStat{MtimeNs: st.MtimeNs})
	return e
}

// pfdEntryBoth 两边都有真实条目（内容相同）时建 base 条目。
func pfdEntryBoth(mac, ub PFScanItem) PFBaseEntry {
	e := pfdEntry(PFSideMac, mac)
	e.Ub = pfdSideStatOf(ub)
	return e
}

// pfdFill 构造「src 边已知、另一边要填」的变更。
func pfdFill(path string, known PFSide, e PFBaseEntry) PFBaseChange {
	return PFBaseChange{Path: path, Entry: e, FillMac: known != PFSideMac, FillUb: known != PFSideUbuntu}
}

// ---------------------------------------------------------------------------------------------
// 一边的视图
// ---------------------------------------------------------------------------------------------

// pfdSide 是一边（Mac 或 Ubuntu）的扫描结果和按路径、按目录建好的索引。
type pfdSide struct {
	side     PFSide
	res      *PFScanResult // 条目排序后的拷贝；nil = 这一边的单元目录不存在
	items    map[string]PFScanItem
	dup      map[string]bool            // 扫描结果里出现了不止一次的路径
	names    map[string][]string        // 目录 → 扫描条目的名字（不含被排除的）
	raw      map[string]map[string]bool // 目录 → 调用方给的原始名字（含被排除的）
	rawMtime map[string]int64           // 路径 → 调用方给的 mtime
	allCache map[string][]string
}

func newPFDSide(side PFSide, res *PFScanResult, raw []PFRawName) *pfdSide {
	s := &pfdSide{
		side: side, items: map[string]PFScanItem{}, dup: map[string]bool{},
		names: map[string][]string{}, raw: map[string]map[string]bool{},
		rawMtime: map[string]int64{}, allCache: map[string][]string{},
	}
	if res != nil {
		cp := *res
		cp.Items = append([]PFScanItem(nil), res.Items...)
		sort.SliceStable(cp.Items, func(i, j int) bool { return cp.Items[i].Path < cp.Items[j].Path })
		s.res = &cp
		for _, it := range cp.Items {
			if _, ok := s.items[it.Path]; ok {
				s.dup[it.Path] = true
				continue
			}
			s.items[it.Path] = it
			dir := pfdParent(it.Path)
			s.names[dir] = append(s.names[dir], pfBase(it.Path))
		}
	}
	for _, r := range raw {
		if r.Path == "" {
			continue
		}
		dir := pfdParent(r.Path)
		if s.raw[dir] == nil {
			s.raw[dir] = map[string]bool{}
		}
		s.raw[dir][pfBase(r.Path)] = true
		if r.MtimeNs != 0 {
			s.rawMtime[r.Path] = r.MtimeNs
		}
	}
	return s
}

// allNames 返回目录 dir 里这一边所有的名字（扫描条目加原始名字，含被排除的），排序去重。
func (s *pfdSide) allNames(dir string) []string {
	if v, ok := s.allCache[dir]; ok {
		return v
	}
	set := map[string]bool{}
	for n := range s.raw[dir] {
		set[n] = true
	}
	for _, n := range s.names[dir] {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	s.allCache[dir] = out
	return out
}

// under 返回路径在 dir 之下（不含 dir 自己）的所有扫描条目，按路径排序。
func (s *pfdSide) under(dir string) []PFScanItem {
	if s.res == nil {
		return nil
	}
	items := s.res.Items
	prefix := dir + "/"
	i := sort.Search(len(items), func(i int) bool { return items[i].Path >= prefix })
	j := i
	for j < len(items) && strings.HasPrefix(items[j].Path, prefix) {
		j++
	}
	return items[i:j]
}

// 一个路径在某一边的样子。
const (
	pfdvAbsent  = iota // 这一边没有（也不在没看的子树里）
	pfdvFile           // 有哈希的普通文件
	pfdvDir            // 在范围内的目录
	pfdvUnknown        // 不明或超出范围（本轮不参与判定）
)

type pfdView struct {
	k      int
	it     PFScanItem
	code   string // 不明时的原因代码（PFSkipUnknown / PFSkipOutOfScope / PFSkipSubtree）
	reason string
}

// view 返回路径 path 在这一边的样子。只有 State=OK 且（文件的话）有哈希的条目才参与判定。
func (s *pfdSide) view(path string) pfdView {
	if s.res == nil {
		return pfdView{k: pfdvAbsent}
	}
	if s.dup[path] {
		return pfdView{k: pfdvUnknown, code: PFSkipUnknown, reason: "扫描结果里这个路径出现了不止一次，不明"}
	}
	it, ok := s.items[path]
	if !ok {
		if anc, sk := s.res.SubtreeSkipped(path); sk {
			return pfdView{k: pfdvUnknown, code: PFSkipSubtree, reason: fmt.Sprintf("没看：上级 %q 本轮没进去看", anc)}
		}
		return pfdView{k: pfdvAbsent}
	}
	if it.State != PFStateOK {
		code := PFSkipUnknown
		if it.State == PFStateOutOfScope {
			code = PFSkipOutOfScope
		}
		reason := it.Reason
		if reason == "" {
			reason = string(it.State)
		}
		return pfdView{k: pfdvUnknown, it: it, code: code, reason: reason}
	}
	switch it.Kind {
	case PFKindDir:
		return pfdView{k: pfdvDir, it: it}
	case PFKindFile:
		if !pfIsHex64(it.Hash) {
			return pfdView{k: pfdvUnknown, it: it, code: PFSkipUnknown, reason: "没有内容哈希（还没算出来）"}
		}
		return pfdView{k: pfdvFile, it: it}
	}
	return pfdView{k: pfdvUnknown, it: it, code: PFSkipOutOfScope, reason: "不是普通文件或目录"}
}

// ---------------------------------------------------------------------------------------------
// 判定器
// ---------------------------------------------------------------------------------------------

type pfdClash struct {
	side   PFSide
	code   string
	reason string
}

type pfdRmCand struct {
	path string
	rm   PFSide // 这一边还有这个目录，另一边已经删了
}

type pfdPlanner struct {
	in       *PFDecideInput
	base     PFBaseView // 有效的 base；nil = 按首次规则（不看任何旧条目）
	mac, ub  *pfdSide
	loc      *time.Location
	clockBad bool

	ledgerDirs  []string
	ledgerPaths map[string]bool
	clash       map[string]pfdClash
	blocked     map[string]bool // 被跳过的路径（目录的话整棵子树跟着跳过）
	skipped     map[string]bool
	staleSeen   map[string]bool
	conflictUse map[string][]string // 目录 → 本轮已用掉的冲突副本名

	actions []PFAction
	skips   []PFSkip
	notes   []PFPlanNote
	rmCands []pfdRmCand
}

func (p *pfdPlanner) sd(s PFSide) *pfdSide {
	if s == PFSideMac {
		return p.mac
	}
	return p.ub
}

func (p *pfdPlanner) bentry(path string) (PFBaseEntry, bool) {
	if p.base == nil || p.ledgerPaths[path] {
		return PFBaseEntry{}, false
	}
	return p.base.Get(path)
}

// skip 记一条跳过。跳过的路径从此「被挡住」：它下面的路径也一并跳过。同一路径只记一次。
func (p *pfdPlanner) skip(path string, side PFSide, code, reason string) {
	if p.skipped[path] {
		return
	}
	p.skipped[path] = true
	p.blocked[path] = true
	p.skips = append(p.skips, PFSkip{Path: path, Side: side, Code: code, Reason: reason})
}

func (p *pfdPlanner) blockedAncestor(path string) (string, bool) {
	for _, a := range pfdAncestors(path) {
		if p.blocked[a] {
			return a, true
		}
	}
	return "", false
}

func (p *pfdPlanner) add(a PFAction) {
	if a.Phase == 0 && a.Kind != PFActMkdir {
		a.Phase = PFPhaseFiles
	}
	p.actions = append(p.actions, a)
}

// ---------------------------------------------------------------------------------------------
// PFDecide
// ---------------------------------------------------------------------------------------------

// PFDecide 是三方判定（见文件头）。永远返回非 nil 的计划；Verdict.Stopped 时没有动作。
func PFDecide(in PFDecideInput) *PFPlan {
	pl := &PFPlan{
		Unit: in.Unit,
		Meta: PFBaseMeta{MacRel: in.MacRel, UbuntuRel: in.UbuntuRel, MacDirID: in.MacDirID, UbuntuDirID: in.UbuntuDirID},
	}
	v := &pl.Verdict
	gate := func(code, effect, format string, a ...any) {
		v.Gates = append(v.Gates, PFGate{Code: code, Effect: effect, Reason: fmt.Sprintf(format, a...)})
	}

	macMissing, ubMissing := in.Mac == nil, in.Ubuntu == nil
	baseLen := 0
	baseHasContent := false
	if in.Base != nil {
		baseLen = in.Base.Len()
		baseHasContent = baseLen > 0 || in.Base.Ledger().Hash != ""
	}

	// G1：单元目录在一边不见了（base 里有内容）→ 整个单元停下，不当成删除。
	switch {
	case macMissing && ubMissing:
		gate("G1", PFGateStop, "两边的单元目录都不在，整个单元停下，不当成删除")
	case (macMissing || ubMissing) && baseHasContent:
		side := PFSideMac
		if ubMissing {
			side = PFSideUbuntu
		}
		gate("G1", PFGateStop, "%s 的单元目录不见了（base 里有 %d 个条目），整个单元停下，不当成删除", side.Machine(), baseLen)
	}
	// G4：同一编号认出两个目录，或项目文件夹在一边改名/挪位置（rel 和 base 不同）→ 单元停下，报两边路径。
	if len(in.MacDup) > 0 {
		gate("G4", PFGateStop, "Mac 上同一编号认出多个目录（%s 和另外 %s），整个单元停下", in.MacRel, strings.Join(in.MacDup, "、"))
	}
	// §14.2：Ubuntu 上同编号的其他目录 = 没配置的目录，只报告提醒合并，不再让整个单元停下（取代 G4 重复即停）。
	if len(in.UbuntuDup) > 0 {
		gate("G4", PFGateReport, "Ubuntu 上同一编号还有别的目录（%s），那是没配置的目录，不同步；可以在 XXYI 里合并过来", strings.Join(in.UbuntuDup, "、"))
	}
	if in.Base != nil {
		m := in.Base.Meta()
		if m.MacRel != "" && in.MacRel != "" && in.MacRel != m.MacRel {
			gate("G4", PFGateStop, "Mac 上项目文件夹的位置变了（上次 %s，现在 %s），整个单元停下", m.MacRel, in.MacRel)
		}
		if m.UbuntuRel != "" && in.UbuntuRel != "" && in.UbuntuRel != m.UbuntuRel {
			gate("G4", PFGateStop, "Ubuntu 上项目文件夹的位置变了（上次 %s，现在 %s），整个单元停下", m.UbuntuRel, in.UbuntuRel)
		}
	}
	for _, g := range v.Gates {
		if g.Effect == PFGateStop {
			v.Stopped = true
		}
	}
	if v.Stopped {
		return pl
	}

	// G5、G2：没有可用的 base，或单元目录被换过 → 本轮按首次规则。
	var base PFBaseView
	switch {
	case in.Base == nil && in.BaseProblem != "":
		v.FirstTime = true
		gate("G5", PFGateFirstTime, "base 不可信（%s），本轮按首次规则，不推断任何删除", in.BaseProblem)
	case in.Base == nil:
		v.FirstTime = true
		gate("no_base", PFGateFirstTime, "没有 base，本轮按首次规则，不推断任何删除")
	default:
		// G2 只比 ino，不比 dev（macOS 重启或重新挂载后 dev 可能变，比 dev 会让首次规则误触发）。
		m := in.Base.Meta()
		var changed []string
		if !macMissing && (m.MacDirID.Ino == 0 || m.MacDirID.Ino != in.MacDirID.Ino) {
			changed = append(changed, "Mac")
		}
		if !ubMissing && (m.UbuntuDirID.Ino == 0 || m.UbuntuDirID.Ino != in.UbuntuDirID.Ino) {
			changed = append(changed, "Ubuntu")
		}
		if len(changed) > 0 {
			v.FirstTime, v.ResetBase = true, true
			gate("G2", PFGateFirstTime, "%s 的单元目录 ino 和 base 记的不同（整个目录被换过，或 base 没记过），本轮按首次规则，不推断任何删除", strings.Join(changed, "、"))
		} else {
			base = in.Base
		}
	}
	// G6：两边时钟差 > 5 分钟（或不知道）→ 冲突时不按 mtime 定新旧，Mac 占原名。
	skew := in.ClockSkew
	if skew < 0 {
		skew = -skew
	}
	if in.ClockUnknown || skew > pfClockLimit {
		v.ClockUntrusted = true
		if in.ClockUnknown {
			gate("G6", PFGateReport, "测不出两边的时钟差，冲突时不按修改时间定新旧，Mac 占原名")
		} else {
			gate("G6", PFGateReport, "两边时钟差 %v，超过 5 分钟，冲突时不按修改时间定新旧，Mac 占原名", skew.Round(time.Second))
		}
	}

	loc := in.Loc
	if loc == nil {
		loc = time.Local
	}
	p := &pfdPlanner{
		in: &in, base: base, loc: loc, clockBad: v.ClockUntrusted,
		mac: newPFDSide(PFSideMac, in.Mac, in.MacNames), ub: newPFDSide(PFSideUbuntu, in.Ubuntu, in.UbuntuNames),
		ledgerPaths: map[string]bool{}, clash: map[string]pfdClash{}, blocked: map[string]bool{}, skipped: map[string]bool{},
		staleSeen: map[string]bool{}, conflictUse: map[string][]string{},
	}
	p.run()

	pl.Actions, pl.Skips, pl.Notes = p.actions, p.skips, p.notes
	for _, a := range pl.Actions {
		if a.Kind == PFActDelete {
			if a.Dst == PFSideMac {
				v.MacDeletes++
			} else {
				v.UbuntuDeletes++
			}
		}
	}
	// G3：只报告，不拦。删除都先进同步回收，不丢。
	if v.MacDeletes+v.UbuntuDeletes > 0 {
		gate("G3", PFGateReport, "这一轮 Mac 要删 %d 个、Ubuntu 要删 %d 个，都先进同步回收", v.MacDeletes, v.UbuntuDeletes)
	}
	return pl
}

// run 跑完整个判定，结果在 p.actions / p.skips / p.notes。
func (p *pfdPlanner) run() {
	p.findLedger()
	p.computeClashes()
	// 一边的单元目录还不存在（首次，对方有）：先建单元根目录（Path == ""）。
	for _, sd := range []*pfdSide{p.mac, p.ub} {
		if sd.res == nil {
			p.add(PFAction{Kind: PFActMkdir, Phase: PFPhaseMkdir, Path: "", Dst: sd.side, DstExpect: pfdNone(),
				Reason: fmt.Sprintf("%s 上还没有这个单元的目录，先建", sd.side.Machine())})
		}
	}
	for _, path := range p.universe() {
		p.decidePath(path)
	}
	p.decideLedger()
	p.reuse()
	p.ensureAncestors()
	p.resolveRmdirs()
	p.finalize()
}

// universe 是要判定的全部路径：两边扫描里的条目加 base 里的条目（台账那一对另判），排序去重。
func (p *pfdPlanner) universe() []string {
	set := map[string]struct{}{}
	for _, sd := range []*pfdSide{p.mac, p.ub} {
		for path := range sd.items {
			if !p.ledgerPaths[path] {
				set[path] = struct{}{}
			}
		}
		for path := range sd.dup {
			if !p.ledgerPaths[path] {
				set[path] = struct{}{}
			}
		}
	}
	if p.base != nil {
		p.base.Range(func(path string, _ PFBaseEntry) bool {
			if !p.ledgerPaths[path] {
				set[path] = struct{}{}
			}
			return true
		})
	}
	out := make([]string, 0, len(set))
	for path := range set {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// computeClashes 找撞名（§4.2 第 3、4 条）：同一边同一目录里折叠键相同的名字、两边同一目录里折叠键相同但字节不同的名字，
// 都判超出范围（两个都跳过；目录的话整棵子树）。
func (p *pfdPlanner) computeClashes() {
	for _, sd := range []*pfdSide{p.mac, p.ub} {
		dirs := make([]string, 0, len(sd.names))
		for d := range sd.names {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			for _, g := range PFSameSideClashes(sd.names[dir]) {
				for _, n := range g {
					path := pfJoinRel(dir, n)
					if _, ok := p.clash[path]; ok {
						continue
					}
					p.clash[path] = pfdClash{side: sd.side, code: PFSkipSameSideClash,
						reason: fmt.Sprintf("%s 上同一目录里有几个名字只差大小写或 NFC/NFD（%s），在不区分的文件系统上会是同一个名字，都不同步", sd.side.Machine(), pfdQuoteAll(g))}
				}
			}
		}
	}
	dirs := make([]string, 0, len(p.mac.names))
	for d := range p.mac.names {
		if _, ok := p.ub.names[d]; ok {
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		for _, c := range PFCrossSideClashes(p.mac.names[dir], p.ub.names[dir]) {
			reason := fmt.Sprintf("两边对不上：Mac 上叫 %q，Ubuntu 上叫 %q（只差大小写或 NFC/NFD），第一版不自动处理，两边都不动", c.Mac, c.Ubuntu)
			mp, up := pfJoinRel(dir, c.Mac), pfJoinRel(dir, c.Ubuntu)
			if _, ok := p.clash[mp]; !ok {
				p.clash[mp] = pfdClash{side: PFSideMac, code: PFSkipCrossSideClash, reason: reason}
			}
			if _, ok := p.clash[up]; !ok {
				p.clash[up] = pfdClash{side: PFSideUbuntu, code: PFSkipCrossSideClash, reason: reason}
			}
		}
	}
}

func pfdQuoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, "、")
}

// ---------------------------------------------------------------------------------------------
// 逐路径判定
// ---------------------------------------------------------------------------------------------

func pfdKindName(t string) string {
	if t == PFBaseTypeDir {
		return "目录"
	}
	return "文件"
}

func (p *pfdPlanner) decidePath(path string) {
	// 没看的子树：任一边的某级上级是符号链接，或是不明/超出范围的目录。base 里有、这一边扫描里没有，不是删除。
	for _, sd := range []*pfdSide{p.mac, p.ub} {
		if sd.res == nil {
			continue
		}
		if anc, ok := sd.res.SubtreeSkipped(path); ok {
			p.skip(path, sd.side, PFSkipSubtree, fmt.Sprintf("%s 上没进去看这棵子树（上级 %q 本轮不明或超出范围），不是删除，两边都不动", sd.side.Machine(), anc))
			return
		}
	}
	if anc, ok := p.blockedAncestor(path); ok {
		p.skip(path, "", PFSkipAncestor, fmt.Sprintf("上级 %q 被跳过，这棵子树本轮都不动", anc))
		return
	}
	if c, ok := p.clash[path]; ok {
		p.skip(path, c.side, c.code, c.reason)
		return
	}
	mv, uv := p.mac.view(path), p.ub.view(path)
	be, hasB := p.bentry(path)

	// 表第 15、16 行：任何一边不明/超出范围 → 本轮跳过，两边不动、base 不变。
	if mv.k == pfdvUnknown || uv.k == pfdvUnknown {
		var parts []string
		var side PFSide
		code := ""
		rank := map[string]int{PFSkipUnknown: 1, PFSkipSubtree: 2, PFSkipOutOfScope: 3}
		for _, x := range []struct {
			s PFSide
			v pfdView
		}{{PFSideMac, mv}, {PFSideUbuntu, uv}} {
			if x.v.k != pfdvUnknown {
				continue
			}
			parts = append(parts, x.s.Machine()+"："+x.v.reason)
			side = x.s
			if rank[x.v.code] > rank[code] {
				code = x.v.code
			}
		}
		if len(parts) == 2 {
			side = ""
		}
		p.skip(path, side, code, "本轮跳过，两边不动、base 不变（"+strings.Join(parts, "；")+"）")
		return
	}

	// 表第 17 行：一边文件一边目录（base 记的算一边）→ 超出范围，路径及子树跳过。
	kinds := map[string]bool{}
	var desc []string
	if hasB {
		kinds[be.T] = true
		desc = append(desc, "base 记的是"+pfdKindName(be.T))
	}
	for _, x := range []struct {
		s PFSide
		v pfdView
	}{{PFSideMac, mv}, {PFSideUbuntu, uv}} {
		switch x.v.k {
		case pfdvFile:
			kinds[PFBaseTypeFile] = true
			desc = append(desc, x.s.Machine()+" 上是文件")
		case pfdvDir:
			kinds[PFBaseTypeDir] = true
			desc = append(desc, x.s.Machine()+" 上是目录")
		}
	}
	if len(kinds) > 1 {
		p.skip(path, "", PFSkipKindMismatch, "同一路径一边是文件、一边是目录（"+strings.Join(desc, "，")+"），超出同步范围，这个路径和它下面的都不动")
		return
	}
	isDir := kinds[PFBaseTypeDir]
	if !isDir && len(kinds) == 0 {
		return // 不会发生：路径在全集里，必有一处有它
	}
	if mv.k != pfdvAbsent || uv.k != pfdvAbsent {
		if o := PFCheckPath(path, isDir); o != nil {
			p.skip(path, "", PFSkipOutOfScope, "路径不合规："+o.Reason)
			return
		}
	}
	if isDir {
		p.decideDir(path, mv, uv, hasB)
	} else {
		p.decideFile(path, mv, uv, be, hasB)
	}
}

// 一边相对 base 的样子。
const (
	pfdrNone    = iota // 没有 base，这边也没有
	pfdrNew            // 没有 base，这边有
	pfdrSame           // 有 base，这边同
	pfdrChanged        // 有 base，这边改了
	pfdrDeleted        // 有 base，这边没有了
)

func pfdRelate(v pfdView, be PFBaseEntry, hasB bool) int {
	if !hasB {
		if v.k == pfdvFile {
			return pfdrNew
		}
		return pfdrNone
	}
	if v.k != pfdvFile {
		return pfdrDeleted
	}
	if v.it.Hash == be.SHA256 && v.it.Stat.Size == be.Size {
		return pfdrSame
	}
	return pfdrChanged
}

// pfdRowOf 查判定表（§3.1）。返回 0 表示没有这种组合。
func pfdRowOf(hasB bool, ms, us int, equal bool) int {
	if hasB {
		switch {
		case ms == pfdrSame && us == pfdrSame:
			return 1
		case ms == pfdrSame && us == pfdrChanged:
			return 2
		case ms == pfdrChanged && us == pfdrSame:
			return 3
		case ms == pfdrSame && us == pfdrDeleted:
			return 4
		case ms == pfdrDeleted && us == pfdrSame:
			return 5
		case ms == pfdrDeleted && us == pfdrDeleted:
			return 6
		case ms == pfdrChanged && us == pfdrChanged:
			if equal {
				return 7
			}
			return 8
		case ms == pfdrChanged && us == pfdrDeleted:
			return 9
		case ms == pfdrDeleted && us == pfdrChanged:
			return 10
		}
		return 0
	}
	switch {
	case ms == pfdrNew && us == pfdrNone:
		return 11
	case ms == pfdrNone && us == pfdrNew:
		return 12
	case ms == pfdrNew && us == pfdrNew:
		if equal {
			return 13
		}
		return 14
	}
	return 0
}

func (p *pfdPlanner) decideFile(path string, mv, uv pfdView, be PFBaseEntry, hasB bool) {
	ms, us := pfdRelate(mv, be, hasB), pfdRelate(uv, be, hasB)
	equal := mv.k == pfdvFile && uv.k == pfdvFile && mv.it.Hash == uv.it.Hash && mv.it.Stat.Size == uv.it.Stat.Size
	row := pfdRowOf(hasB, ms, us, equal)
	if row == 0 || row == 1 {
		return // 表第 1 行：无事
	}
	// 历史版本（§7）：快照文件只增。同名不同内容（一边改了、两边都改、改的对删的）一律跳过，绝不覆盖、不做冲突副本。
	if pfdInHistory(path) {
		switch row {
		case 2, 3, 8, 9, 10, 14:
			p.skip(path, "", PFSkipHistoryDiffers, "历史版本里的快照文件同名不同内容（快照只增不改），绝不覆盖、也不做冲突副本，两边都不动")
			return
		}
	}
	m, u := mv.it, uv.it
	switch row {
	case 2: // M 同、U 改：U→Mac 覆盖（Mac 旧的先回收）
		if p.lockedSkip(PFSideMac, path) {
			return
		}
		p.emitCopy(PFActCopyOver, row, path, PFSideUbuntu, u, &m, "Mac 没改、Ubuntu 改了：用 Ubuntu 的覆盖 Mac（Mac 旧的先进回收）", "")
	case 3: // M 改、U 同
		if p.lockedSkip(PFSideUbuntu, path) {
			return
		}
		p.emitCopy(PFActCopyOver, row, path, PFSideMac, m, &u, "Mac 改了、Ubuntu 没改：用 Mac 的覆盖 Ubuntu（Ubuntu 旧的先进回收）", "")
	case 4: // M 同、U 删：删 Mac（移入回收）
		if p.lockedSkip(PFSideMac, path) {
			return
		}
		p.emitDelete(row, path, PFSideMac, m, "Ubuntu 删了、Mac 没改：把 Mac 的移入回收")
	case 5:
		if p.lockedSkip(PFSideUbuntu, path) {
			return
		}
		p.emitDelete(row, path, PFSideUbuntu, u, "Mac 删了、Ubuntu 没改：把 Ubuntu 的移入回收")
	case 6: // 两边都删了：只删 base 条目
		p.add(PFAction{Kind: PFActBaseDel, Row: row, Path: path, Base: []PFBaseChange{{Path: path, Del: true}},
			Reason: "两边都删了，只把 base 里的条目删掉"})
	case 7: // 两边都改成一样：只更新 base
		p.add(PFAction{Kind: PFActBaseSet, Row: row, Path: path, Base: []PFBaseChange{{Path: path, Entry: pfdEntryBoth(m, u)}},
			Reason: "两边都改了，但改成了一样的内容，只更新 base"})
	case 8, 14: // 两边都改（或首次都新）且内容不同：冲突
		p.emitConflict(row, path, m, u)
	case 9: // M 改、U 删：改的赢，Ubuntu 恢复
		p.emitCopy(PFActCopyNew, row, path, PFSideMac, m, nil, "Mac 改了、Ubuntu 删了：改的赢，把 Mac 的恢复到 Ubuntu",
			"Ubuntu 上删了、Mac 上改了：保留了改的，已恢复到 Ubuntu")
	case 10:
		p.emitCopy(PFActCopyNew, row, path, PFSideUbuntu, u, nil, "Ubuntu 改了、Mac 删了：改的赢，把 Ubuntu 的恢复到 Mac",
			"Mac 上删了、Ubuntu 上改了：保留了改的，已恢复到 Mac")
	case 11:
		p.emitCopy(PFActCopyNew, row, path, PFSideMac, m, nil, "只有 Mac 有：新建到 Ubuntu", "")
	case 12:
		p.emitCopy(PFActCopyNew, row, path, PFSideUbuntu, u, nil, "只有 Ubuntu 有：新建到 Mac", "")
	case 13: // 两边都新建且一样：只写 base
		p.add(PFAction{Kind: PFActBaseSet, Row: row, Path: path, Base: []PFBaseChange{{Path: path, Entry: pfdEntryBoth(m, u)}},
			Reason: "两边各有一份且内容一样，只写进 base"})
	}
}

// lockedSkip：目标边有 Office 锁就跳过（不覆盖、不删、不改名，等关闭），返回是否跳过。
func (p *pfdPlanner) lockedSkip(target PFSide, path string) bool {
	if !p.lockedOn(target, path) {
		return false
	}
	p.skip(path, target, PFSkipOfficeLock, fmt.Sprintf("%s 上这个文件有 Office 锁（正在编辑），不覆盖、不删、不改名，等关闭后下一轮再处理", target.Machine()))
	return true
}

// lockedOn 看 side 边 path 所在目录里有没有这个文件的 Office 锁（~$ 的各种写法、.~lock.<名>#）。
// 锁文件超过 12 小时当残留（报告后不再当锁）；不知道 mtime 的按真锁处理。
func (p *pfdPlanner) lockedOn(side PFSide, path string) bool {
	sd := p.sd(side)
	dir, name := pfdParent(path), pfBase(path)
	locked := false
	for _, l := range PFOfficeLocks(sd.allNames(dir), name) {
		lp := pfJoinRel(dir, l)
		if mt := sd.rawMtime[lp]; mt != 0 && !p.in.Now.IsZero() && p.in.Now.UnixNano()-mt > int64(pfStaleLock) {
			key := string(side) + "|" + lp
			if !p.staleSeen[key] {
				p.staleSeen[key] = true
				p.notes = append(p.notes, PFPlanNote{Path: lp, Code: "stale_lock",
					Text: fmt.Sprintf("%s 上的锁文件 %q 已超过 12 小时没动，当作残留，不再当锁", side.Machine(), lp)})
			}
			continue
		}
		locked = true
	}
	return locked
}

// emitCopy 出一个拷贝动作：src 边的 srcIt 拷到另一边；dstIt 为 nil 表示目标边没有（新建），否则是被覆盖的现状。
func (p *pfdPlanner) emitCopy(kind PFActionKind, row int, path string, src PFSide, srcIt PFScanItem, dstIt *PFScanItem, reason, notice string) {
	a := PFAction{Kind: kind, Phase: PFPhaseFiles, Row: row, Path: path, Src: src, Dst: src.Other(),
		SrcExpect: pfdExpectOf(srcIt), Reason: reason, Notice: notice}
	if dstIt == nil {
		a.DstExpect = pfdNone()
	} else {
		a.DstExpect = pfdExpectOf(*dstIt)
	}
	a.Base = []PFBaseChange{pfdFill(path, src, pfdEntry(src, srcIt))}
	if p.lockedOn(src, path) {
		extra := fmt.Sprintf("%s 上这个文件有 Office 锁（可能正在编辑），发送的是已保存的内容", src.Machine())
		if a.Notice == "" {
			a.Notice = extra
		} else {
			a.Notice += "；" + extra
		}
	}
	p.add(a)
}

func (p *pfdPlanner) emitDelete(row int, path string, dst PFSide, it PFScanItem, reason string) {
	p.add(PFAction{Kind: PFActDelete, Phase: PFPhaseFiles, Row: row, Path: path, Dst: dst, DstExpect: pfdExpectOf(it),
		Base: []PFBaseChange{{Path: path, Del: true}}, Reason: reason})
}

// pickWinner 定冲突时谁占原名（§3.4）：mtime 较新的占；相差 2 秒以内或时钟不可信（G6）时 Mac 占。
func (p *pfdPlanner) pickWinner(m, u PFScanItem) (PFSide, string) {
	if p.clockBad {
		return PFSideMac, "两边时钟不可信，不按修改时间定新旧，Mac 占原名"
	}
	d := m.Stat.MtimeNs - u.Stat.MtimeNs
	ad := d
	if ad < 0 {
		ad = -ad
	}
	switch {
	case ad <= int64(pfConflictTie):
		return PFSideMac, "两边修改时间相差不到 2 秒，Mac 占原名"
	case d > 0:
		return PFSideMac, "Mac 的修改时间较新，Mac 占原名"
	}
	return PFSideUbuntu, "Ubuntu 的修改时间较新，Ubuntu 占原名"
}

// emitConflict 出冲突动作（表第 8、14 行，§3.4）。
func (p *pfdPlanner) emitConflict(row int, path string, m, u PFScanItem) {
	for _, s := range []PFSide{PFSideMac, PFSideUbuntu} {
		if p.lockedSkip(s, path) {
			return
		}
	}
	winner, why := p.pickWinner(m, u)
	loser := winner.Other()
	wi, li := m, u
	if winner == PFSideUbuntu {
		wi, li = u, m
	}
	dir, name := pfdParent(path), pfBase(path)
	names := append([]string(nil), p.mac.allNames(dir)...)
	names = append(names, p.ub.allNames(dir)...)
	names = append(names, p.conflictUse[dir]...)
	cname, err := PFConflictName(name, loser.Machine(), time.Unix(0, li.Stat.MtimeNs).In(p.loc), NewPFNameSet(names).Has)
	if err != nil {
		p.skip(path, "", PFSkipConflictName, "两边都改了且内容不同，但生成不出冲突副本名："+err.Error())
		return
	}
	cpath := pfJoinRel(dir, cname)
	if o := PFCheckPath(cpath, false); o != nil {
		p.skip(path, "", PFSkipConflictName, "两边都改了且内容不同，但冲突副本的路径不合规："+o.Reason)
		return
	}
	p.conflictUse[dir] = append(p.conflictUse[dir], cname)
	win := pfdEntry(winner, wi) // 落败边装好胜出版本后要填
	los := pfdEntry(loser, li)  // 落败边的原文件只是改名，stat 不变；胜出边装好副本后要填
	reason := "两边都改了且内容不同：冲突，两份都留。" + why
	if row == 14 {
		reason = "两边各有一份同名文件且内容不同：冲突，两份都留。" + why
	}
	p.add(PFAction{Kind: PFActConflict, Phase: PFPhaseFiles, Row: row, Path: path, Src: winner, Dst: loser,
		SrcExpect: pfdExpectOf(wi), DstExpect: pfdExpectOf(li),
		Conflict: &PFConflictStep{Winner: winner, Loser: loser, ConflictPath: cpath, Why: why},
		Base:     []PFBaseChange{pfdFill(path, winner, win), pfdFill(cpath, loser, los)},
		Reason:   reason,
		Notice:   fmt.Sprintf("%q 两边都改了：%s 的版本留在原名，%s 的版本改名为 %q", path, winner.Machine(), loser.Machine(), cname),
	})
}

// ---------------------------------------------------------------------------------------------
// 目录
// ---------------------------------------------------------------------------------------------

func (p *pfdPlanner) decideDir(path string, mv, uv pfdView, hasB bool) {
	mHas, uHas := mv.k == pfdvDir, uv.k == pfdvDir
	switch {
	case mHas && uHas:
		if !hasB { // 表第 13 行
			p.add(PFAction{Kind: PFActBaseSet, Row: 13, Path: path, Base: []PFBaseChange{{Path: path, Entry: pfdEntryBoth(mv.it, uv.it)}},
				Reason: "两边各有这个目录，只写进 base"})
		}
	case mHas:
		if hasB { // 表第 4 行的目录版：Ubuntu 删了，等本轮文件动作后看空不空
			p.rmCands = append(p.rmCands, pfdRmCand{path: path, rm: PFSideMac})
		} else {
			p.emitMkdir(11, path, PFSideMac, mv.it, "只有 Mac 有这个目录：在 Ubuntu 建")
		}
	case uHas:
		if hasB {
			p.rmCands = append(p.rmCands, pfdRmCand{path: path, rm: PFSideUbuntu})
		} else {
			p.emitMkdir(12, path, PFSideUbuntu, uv.it, "只有 Ubuntu 有这个目录：在 Mac 建")
		}
	default:
		if hasB { // 表第 6 行的目录版
			p.add(PFAction{Kind: PFActBaseDel, Row: 6, Path: path, Base: []PFBaseChange{{Path: path, Del: true}},
				Reason: "两边都没有这个目录了，只把 base 里的条目删掉"})
		}
	}
}

// emitMkdir：src 边有目录 it，另一边建。
func (p *pfdPlanner) emitMkdir(row int, path string, src PFSide, it PFScanItem, reason string) {
	p.add(PFAction{Kind: PFActMkdir, Phase: PFPhaseMkdir, Row: row, Path: path, Src: src, Dst: src.Other(),
		SrcExpect: pfdExpectOf(it), DstExpect: pfdNone(),
		Base: []PFBaseChange{pfdFill(path, src, pfdEntry(src, it))}, Reason: reason})
}

// ---------------------------------------------------------------------------------------------
// 改名/复制复用（§3.3）
// ---------------------------------------------------------------------------------------------

type pfdKey struct {
	sha  string
	size int64
}

// reuse 把「目标边要新建内容为 H 的 p2」的拷贝动作，尽量换成目标边本机的动作，不传输：
//
//	(a) 本轮目标边本来要删、内容也是 H 的 p1 → 目标边把 p1 改名为 p2（不进回收）；
//	(b) 目标边别处有一份没人动的、内容是 H 的文件 → 本机复制。
//
// 目录改名 = 逐个文件命中 (a)。只处理「新建」（copy_new）；覆盖和冲突不复用。
func (p *pfdPlanner) reuse() {
	touched := map[PFSide]map[string]bool{PFSideMac: {}, PFSideUbuntu: {}}
	for _, a := range p.actions {
		switch a.Kind {
		case PFActCopyNew, PFActCopyOver, PFActDelete, PFActMkdir, PFActRename, PFActClone:
			touched[a.Dst][a.Path] = true
			if a.From != "" {
				touched[a.Dst][a.From] = true
			}
		case PFActConflict:
			for _, s := range []PFSide{PFSideMac, PFSideUbuntu} {
				touched[s][a.Path] = true
				touched[s][a.Conflict.ConflictPath] = true
			}
		}
	}
	dels := map[PFSide]map[pfdKey][]int{PFSideMac: {}, PFSideUbuntu: {}}
	var creates []int
	for i, a := range p.actions {
		switch {
		case a.Kind == PFActDelete && a.DstExpect != nil && a.DstExpect.T == PFBaseTypeFile:
			k := pfdKey{a.DstExpect.SHA256, a.DstExpect.Size}
			dels[a.Dst][k] = append(dels[a.Dst][k], i)
		case a.Kind == PFActCopyNew && a.SrcExpect != nil && a.SrcExpect.T == PFBaseTypeFile:
			creates = append(creates, i)
		}
	}
	if len(creates) == 0 {
		return
	}
	sort.Slice(creates, func(i, j int) bool { return p.actions[creates[i]].Path < p.actions[creates[j]].Path })

	consumed := map[int]bool{}
	stable := map[PFSide]map[pfdKey][]string{}
	stableOf := func(x PFSide) map[pfdKey][]string {
		if m, ok := stable[x]; ok {
			return m
		}
		m := map[pfdKey][]string{}
		sd := p.sd(x)
		if sd.res != nil {
			for _, it := range sd.res.Items { // 已按路径排序
				if it.State != PFStateOK || it.Kind != PFKindFile || !pfIsHex64(it.Hash) {
					continue
				}
				if p.ledgerPaths[it.Path] || touched[x][it.Path] || p.skipped[it.Path] || sd.dup[it.Path] {
					continue
				}
				k := pfdKey{it.Hash, it.Stat.Size}
				m[k] = append(m[k], it.Path)
			}
		}
		stable[x] = m
		return m
	}

	for _, ci := range creates {
		a := p.actions[ci]
		x := a.Dst
		k := pfdKey{a.SrcExpect.SHA256, a.SrcExpect.Size}
		// (a)
		pick := -1
		for _, di := range dels[x][k] {
			if consumed[di] {
				continue
			}
			if pick < 0 || pfdBetterCand(a.Path, p.actions[di].Path, p.actions[pick].Path) {
				pick = di
			}
		}
		if pick >= 0 {
			d := p.actions[pick]
			consumed[pick] = true
			entry := a.Base[0].Entry
			pfdSetSide(&entry, x, PFSideStat{MtimeNs: d.DstExpect.MtimeNs, Ino: d.DstExpect.Ino}) // 改名不改 mtime 和 ino
			base := append([]PFBaseChange(nil), d.Base...)
			base = append(base, PFBaseChange{Path: a.Path, Entry: entry})
			p.actions[ci] = PFAction{Kind: PFActRename, Phase: PFPhaseFiles, Row: a.Row, Path: a.Path, Src: a.Src, Dst: x,
				SrcExpect: a.SrcExpect, DstExpect: pfdNone(), From: d.Path, FromExpect: d.DstExpect, Base: base,
				Reason: fmt.Sprintf("%s 上本来要删 %q（内容相同），改名成这个路径，不重传（%s）", x.Machine(), d.Path, a.Reason), Notice: a.Notice}
			continue
		}
		// (b)
		var q string
		for _, cand := range stableOf(x)[k] {
			if q == "" || pfdBetterCand(a.Path, cand, q) {
				q = cand
			}
		}
		if q != "" {
			qi := p.sd(x).items[q]
			p.actions[ci] = PFAction{Kind: PFActClone, Phase: PFPhaseFiles, Row: a.Row, Path: a.Path, Src: a.Src, Dst: x,
				SrcExpect: a.SrcExpect, DstExpect: pfdNone(), From: q, FromExpect: pfdExpectOf(qi), Base: a.Base,
				Reason: fmt.Sprintf("%s 上已有内容相同的 %q，本机复制，不重传（%s）", x.Machine(), q, a.Reason), Notice: a.Notice}
		}
	}
	if len(consumed) > 0 {
		kept := p.actions[:0]
		for i, a := range p.actions {
			if !consumed[i] {
				kept = append(kept, a)
			}
		}
		p.actions = kept
	}
}

// pfdBetterCand：给要新建的 target 选来源时，cand 是否比 cur 更合适：文件名相同的优先，其次路径字节序靠前。
func pfdBetterCand(target, cand, cur string) bool {
	cs, us := pfBase(cand) == pfBase(target), pfBase(cur) == pfBase(target)
	if cs != us {
		return cs
	}
	return cand < cur
}

// ---------------------------------------------------------------------------------------------
// 上级目录
// ---------------------------------------------------------------------------------------------

// ensureAncestors 保证所有「在目标边新建东西」的动作，目标边的上级目录都在：不在就补一个 mkdir
// （一边删了目录、另一边在里面有新东西时，目录要先回来）。
func (p *pfdPlanner) ensureAncestors() {
	type key struct {
		s PFSide
		p string
	}
	planned := map[key]bool{}
	for _, a := range p.actions {
		if a.Kind == PFActMkdir {
			planned[key{a.Dst, a.Path}] = true
		}
	}
	var add []PFAction
	for i := range p.actions {
		a := p.actions[i]
		switch a.Kind {
		case PFActCopyNew, PFActRename, PFActClone, PFActMkdir, PFActLedgerCopy:
		default:
			continue
		}
		x, y := a.Dst, a.Dst.Other()
		ok := true
		for _, anc := range pfdAncestors(a.Path) {
			if xi, has := p.sd(x).items[anc]; has {
				if xi.State == PFStateOK && xi.Kind == PFKindDir {
					continue
				}
				ok = false
				break
			}
			if planned[key{x, anc}] {
				continue
			}
			yi, has := p.sd(y).items[anc]
			if !has || yi.State != PFStateOK || yi.Kind != PFKindDir {
				ok = false
				break
			}
			planned[key{x, anc}] = true
			add = append(add, PFAction{Kind: PFActMkdir, Phase: PFPhaseMkdir, Path: anc, Src: y, Dst: x,
				SrcExpect: pfdExpectOf(yi), DstExpect: pfdNone(),
				Base:   []PFBaseChange{pfdFill(anc, y, pfdEntry(y, yi))},
				Reason: fmt.Sprintf("%s 上 %q 的上级目录不在，先建", x.Machine(), a.Path)})
		}
		if !ok {
			p.skip(a.Path, x, PFSkipAncestorMissing, fmt.Sprintf("%s 上它的上级不是目录，不动", x.Machine()))
			p.actions[i].Kind = "" // 丢弃，finalize 清掉
		}
	}
	p.actions = append(p.actions, add...)
}

// resolveRmdirs 处理「一边删了目录、另一边还有」的目录（base 里有它）：
//   - 本轮有东西要在删了的那一边的这个目录下新建 → 目录要回来（ensureAncestors 已补 mkdir），不删；
//   - 这一边目录下除了本轮要删、要改名走的，还有别的（不明、没同步的新文件……）→ 保留，进跳过清单；
//   - 否则出 rmdir（IfEmpty，执行时再判一次是否为空）。
//
// 深的目录先处理：子目录决定删了，上级才看得出空不空。
func (p *pfdPlanner) resolveRmdirs() {
	if len(p.rmCands) == 0 {
		return
	}
	needed := map[PFSide]map[string]bool{PFSideMac: {}, PFSideUbuntu: {}}
	gone := map[PFSide]map[string]bool{PFSideMac: {}, PFSideUbuntu: {}}
	for _, a := range p.actions {
		switch a.Kind {
		case PFActMkdir:
			needed[a.Dst][a.Path] = true
		case PFActDelete:
			gone[a.Dst][a.Path] = true
		case PFActRename:
			gone[a.Dst][a.From] = true
		}
	}
	cands := append([]pfdRmCand(nil), p.rmCands...)
	sort.Slice(cands, func(i, j int) bool { return cands[i].path > cands[j].path })
	for _, c := range cands {
		if needed[c.rm.Other()][c.path] {
			continue
		}
		var left []string
		for _, it := range p.sd(c.rm).under(c.path) {
			if !gone[c.rm][it.Path] {
				left = append(left, it.Path)
			}
		}
		if len(left) > 0 {
			p.skip(c.path, c.rm, PFSkipDirKept, fmt.Sprintf("%s 已删了这个目录，但 %s 上它里面还有没同步的东西（如 %q），保留", c.rm.Other().Machine(), c.rm.Machine(), left[0]))
			continue
		}
		it := p.sd(c.rm).items[c.path]
		gone[c.rm][c.path] = true
		row := 4
		if c.rm == PFSideUbuntu {
			row = 5
		}
		p.add(PFAction{Kind: PFActRmdir, Phase: PFPhaseRmdir, Row: row, Path: c.path, Dst: c.rm, DstExpect: pfdExpectOf(it), IfEmpty: true,
			Base:   []PFBaseChange{{Path: c.path, Del: true}},
			Reason: fmt.Sprintf("%s 已删了这个目录，%s 上本轮动作后已空：空了才删（执行时再判一次）", c.rm.Other().Machine(), c.rm.Machine())})
	}
}

// ---------------------------------------------------------------------------------------------
// 历史版本台账（§7 第一版）
// ---------------------------------------------------------------------------------------------

// findLedger 找台账所在目录：路径里有「历史版本」一段、名字是 档案台账.sqlite3 / 档案索引.json 的文件所在目录。
func (p *pfdPlanner) findLedger() {
	set := map[string]bool{}
	for _, sd := range []*pfdSide{p.mac, p.ub} {
		if sd.res == nil {
			continue
		}
		for _, it := range sd.res.Items {
			if n := pfBase(it.Path); (n == pfLedgerSQLite || n == pfLedgerJSON) && pfdInHistory(it.Path) {
				set[pfdParent(it.Path)] = true
			}
		}
	}
	for d := range set {
		p.ledgerDirs = append(p.ledgerDirs, d)
	}
	sort.Strings(p.ledgerDirs)
	for _, d := range p.ledgerDirs {
		p.ledgerPaths[pfJoinRel(d, pfLedgerSQLite)] = true
		p.ledgerPaths[pfJoinRel(d, pfLedgerJSON)] = true
	}
}

// 台账一对在某一边的样子。
type pfdLedgerView struct {
	k      int // pfdvAbsent、pfdvFile（两个都有哈希）、pfdvUnknown
	sq, js pfdView
	hash   string
	reason string
}

func (p *pfdPlanner) ledgerView(side PFSide, sq, js string) pfdLedgerView {
	sd := p.sd(side)
	a, b := sd.view(sq), sd.view(js)
	switch {
	case a.k == pfdvUnknown:
		return pfdLedgerView{k: pfdvUnknown, reason: a.reason}
	case b.k == pfdvUnknown:
		return pfdLedgerView{k: pfdvUnknown, reason: b.reason}
	case a.k == pfdvAbsent && b.k == pfdvAbsent:
		return pfdLedgerView{k: pfdvAbsent}
	case a.k == pfdvFile && b.k == pfdvFile:
		return pfdLedgerView{k: pfdvFile, sq: a, js: b, hash: PFLedgerPairHash(a.it.Hash, b.it.Hash)}
	}
	return pfdLedgerView{k: pfdvUnknown, reason: "台账那一对只有一半，或不是普通文件"}
}

func (p *pfdPlanner) pathPresent(path string) bool {
	_, a := p.mac.items[path]
	_, b := p.ub.items[path]
	return a || b
}

// decideLedger 把台账那一对（档案台账.sqlite3 + 档案索引.json）当一个整体判：
//   - 只一边变（相对 base 的台账哈希）→ 输出一个 ledger_copy，把变了的一对拷过去；
//   - 一边有、一边没有 → 拷过去（只增不删，不会丢）；
//   - 两边都变且不同（首次两边不同也算）→ 跳过，报「两边都新存了历史版本，台账待合并」；
//   - 一对不完整、有不明、旁边有 journal/wal（正在写）→ 跳过，推迟。
//
// 台账动作排在同单元所有快照文件动作之后（Phase 3）。
func (p *pfdPlanner) decideLedger() {
	if len(p.ledgerDirs) == 0 {
		return
	}
	if len(p.ledgerDirs) > 1 {
		for _, d := range p.ledgerDirs {
			for _, f := range []string{pfJoinRel(d, pfLedgerSQLite), pfJoinRel(d, pfLedgerJSON)} {
				if p.pathPresent(f) {
					p.skip(f, "", PFSkipLedgerMulti, "单元里有多处历史版本台账，第一版不自动同步")
				}
			}
		}
		return
	}
	dir := p.ledgerDirs[0]
	sq, js := pfJoinRel(dir, pfLedgerSQLite), pfJoinRel(dir, pfLedgerJSON)
	skipBoth := func(side PFSide, code, reason string) {
		for _, f := range []string{sq, js} {
			if p.pathPresent(f) {
				p.skip(f, side, code, reason)
			}
		}
	}
	if anc, ok := p.blockedAncestor(sq); ok {
		skipBoth("", PFSkipAncestor, fmt.Sprintf("上级 %q 被跳过，台账本轮不动", anc))
		return
	}
	for _, f := range []string{sq, js} {
		if c, ok := p.clash[f]; ok {
			skipBoth(c.side, c.code, c.reason)
			return
		}
	}
	for _, sd := range []*pfdSide{p.mac, p.ub} {
		for _, n := range sd.allNames(dir) {
			if n == pfLedgerSQLite+"-journal" || n == pfLedgerSQLite+"-wal" {
				skipBoth(sd.side, PFSkipLedgerJournal, fmt.Sprintf("%s 上台账旁边有 %q（正在写），推迟", sd.side.Machine(), n))
				return
			}
		}
	}
	mv, uv := p.ledgerView(PFSideMac, sq, js), p.ledgerView(PFSideUbuntu, sq, js)
	if mv.k == pfdvUnknown || uv.k == pfdvUnknown {
		var parts []string
		if mv.k == pfdvUnknown {
			parts = append(parts, "Mac："+mv.reason)
		}
		if uv.k == pfdvUnknown {
			parts = append(parts, "Ubuntu："+uv.reason)
		}
		skipBoth("", PFSkipLedgerUnknown, "台账本轮跳过，两边不动、base 不变（"+strings.Join(parts, "；")+"）")
		return
	}
	hashB := ""
	if p.base != nil {
		hashB = p.base.Ledger().Hash
	}
	switch {
	case mv.k == pfdvAbsent && uv.k == pfdvAbsent:
		return
	case uv.k == pfdvAbsent:
		p.emitLedgerCopy(dir, sq, js, PFSideMac, mv, uv, "只有 Mac 有台账：拷到 Ubuntu")
	case mv.k == pfdvAbsent:
		p.emitLedgerCopy(dir, sq, js, PFSideUbuntu, uv, mv, "只有 Ubuntu 有台账：拷到 Mac")
	case mv.hash == uv.hash:
		if mv.hash != hashB {
			p.add(PFAction{Kind: PFActBaseLedger, Phase: PFPhaseLedger, Path: sq, BaseLedgerHash: mv.hash,
				Reason: "两边台账一样，只更新 base 的台账记录"})
		}
	case hashB != "" && mv.hash == hashB:
		p.emitLedgerCopy(dir, sq, js, PFSideUbuntu, uv, mv, "Mac 的台账没变、Ubuntu 的变了：把 Ubuntu 的一对拷到 Mac")
	case hashB != "" && uv.hash == hashB:
		p.emitLedgerCopy(dir, sq, js, PFSideMac, mv, uv, "Ubuntu 的台账没变、Mac 的变了：把 Mac 的一对拷到 Ubuntu")
	default:
		skipBoth("", PFSkipLedgerBoth, "两边都新存了历史版本，台账待合并：两边都不动，各边继续用自己的台账")
	}
}

func (p *pfdPlanner) emitLedgerCopy(dir, sq, js string, src PFSide, sv, dv pfdLedgerView, reason string) {
	step := &PFLedgerStep{Dir: dir, SQLite: sq, JSON: js, Hash: sv.hash,
		SrcSQLite: pfdExpectOf(sv.sq.it), SrcJSON: pfdExpectOf(sv.js.it)}
	if dv.k == pfdvFile {
		step.DstSQLite, step.DstJSON = pfdExpectOf(dv.sq.it), pfdExpectOf(dv.js.it)
	} else {
		step.DstSQLite, step.DstJSON = pfdNone(), pfdNone()
	}
	p.add(PFAction{Kind: PFActLedgerCopy, Phase: PFPhaseLedger, Path: sq, Src: src, Dst: src.Other(),
		SrcExpect: step.SrcSQLite, DstExpect: step.DstSQLite, Ledger: step, BaseLedgerHash: sv.hash, Reason: reason})
}

// ---------------------------------------------------------------------------------------------
// 收尾：去掉丢弃的动作、排序
// ---------------------------------------------------------------------------------------------

// finalize 去掉被丢弃的动作，并排序：动作按 (Phase, 路径) 排（Phase 2 路径降序），结果确定。
func (p *pfdPlanner) finalize() {
	kept := p.actions[:0]
	for _, a := range p.actions {
		if a.Kind != "" {
			kept = append(kept, a)
		}
	}
	p.actions = kept
	sort.SliceStable(p.actions, func(i, j int) bool {
		a, b := p.actions[i], p.actions[j]
		if a.Phase != b.Phase {
			return a.Phase < b.Phase
		}
		if a.Path != b.Path {
			if a.Phase == PFPhaseRmdir {
				return a.Path > b.Path
			}
			return a.Path < b.Path
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Dst < b.Dst
	})
	sort.SliceStable(p.skips, func(i, j int) bool {
		a, b := p.skips[i], p.skips[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Side != b.Side {
			return a.Side < b.Side
		}
		return a.Code < b.Code
	})
	sort.SliceStable(p.notes, func(i, j int) bool {
		a, b := p.notes[i], p.notes[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Code < b.Code
	})
}
