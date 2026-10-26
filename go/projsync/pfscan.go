package projsync

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 项目文件同步：单元目录的扫描、静止判断、单元指纹（技术设计 §3.1、§4.2 第 5/7/8 条、§4.3、§5.1、§5.2 第 2 步）。
//
// 本文件管：从一个单元目录递归 Lstat（不跟随符号链接），给每个条目一个最终状态
// （有哈希 / 不明及原因 / 超出范围及原因），排除的只计数。哈希与缓存在 pfhash.go。
// 不管：名字规则（排除表、包目录、长度、NFC 等由调用方经 PFScanFilter 传入）、
// 写文件、判定、传输。
//
// 总原则「识别边界、超出就停」：
//   - 读不了、认不出的一律报出来（路径 + 原因），绝不当成「没有」——调用方若把「没有」当删除会丢数据。
//   - 凡不是「有哈希」的文件，都只表示「本轮不参与判定」，调用方不得据此推断删除或改动。
//   - iCloud 占位文件（darwin st_flags 的 SF_DATALESS，或老系统的 .X.icloud）内容只在云端，
//     读会触发下载，所以本包绝不读它们的内容。
//
// 状态分工（给判定层）：
//   - PFStateUnknown（不明）是暂时性的，下一轮可能自己好：未静止、iCloud 占位、哈希没算完、读的过程中被改、扫描时消失。
//   - PFStateOutOfScope（超出范围）是需要人看的：符号链接、特殊文件、调用方过滤器判的、读不了。

// PFKind 是条目类型。
type PFKind string

const (
	PFKindFile    PFKind = "file"    // 普通文件
	PFKindDir     PFKind = "dir"     // 目录
	PFKindSymlink PFKind = "symlink" // 符号链接（只报告，不跟随）
	PFKindOther   PFKind = "other"   // FIFO、socket、设备文件等
)

// PFSFDataless 是 darwin st_flags 里的 SF_DATALESS：iCloud 占位（内容只在云端）。
// 常量值固定，不引 golang.org/x/sys，其他平台上 Flags 恒为 0。
const PFSFDataless uint32 = 0x40000000

// PFDefaultQuietSeconds 是 quiet_seconds 的缺省值（§4.2 第 8 条）。
const PFDefaultQuietSeconds = 10

// pfMaxDepth 是目录层数上限，超过的目录按超出范围处理（防挂载环等病态结构）。
const pfMaxDepth = 128

// PFStat 是一次 Lstat 得到的全部字段。
type PFStat struct {
	Kind    PFKind
	Size    int64
	MtimeNs int64
	CtimeNs int64
	Ino     uint64
	Dev     uint64
	Mode    fs.FileMode
	Flags   uint32 // darwin 的 st_flags；其他平台恒为 0
}

// Dataless 报告这个条目是不是 iCloud 占位（SF_DATALESS）。
func (s PFStat) Dataless() bool { return s.Flags&PFSFDataless != 0 }

// PFStatFunc 是可注入的 Lstat：测试里用它模拟 SF_DATALESS、读失败、前后两次 stat 不同。
type PFStatFunc func(path string) (PFStat, error)

// ErrPFRootNotDir 表示单元目录本身不是真实目录（不存在的情况返回底层错误）。
var ErrPFRootNotDir = errors.New("projsync: 单元目录不是真实目录")

// PFLstat 是缺省的 Lstat：不跟随符号链接，取全部字段。
func PFLstat(path string) (PFStat, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return PFStat{}, err
	}
	m := fi.Mode()
	st := PFStat{Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano(), Mode: m}
	switch {
	case m&fs.ModeSymlink != 0:
		st.Kind = PFKindSymlink
	case m.IsDir():
		st.Kind = PFKindDir
	case m.IsRegular():
		st.Kind = PFKindFile
	default:
		st.Kind = PFKindOther
	}
	if err := pfFillSys(fi, &st); err != nil {
		return PFStat{}, fmt.Errorf("读取 %s 的底层属性失败: %w", path, err)
	}
	return st, nil
}

// 条目状态。
type PFState string

const (
	PFStateOK         PFState = "ok"           // 在范围内。普通文件在哈希完成后才算「有哈希」
	PFStateUnknown    PFState = "unknown"      // 不明：本轮不参与判定，原因见 Code
	PFStateOutOfScope PFState = "out_of_scope" // 超出范围：跳过并报告，原因见 Code
)

// 原因代码（PFScanItem.Code）。
const (
	PFCodeNotQuiet          = "not_quiet"             // 未静止：mtime 距今不足 quiet_seconds
	PFCodeICloudPlaceholder = "icloud_placeholder"    // iCloud 占位
	PFCodeHashTimeout       = "hash_timeout"          // 哈希超出时间预算没算完
	PFCodeChangedWhileRead  = "changed_while_reading" // 读的过程中文件被改
	PFCodeChangedSinceScan  = "changed_since_scan"    // 扫描之后、哈希之前文件变了
	PFCodeVanished          = "vanished"              // 扫描或哈希时文件消失
	PFCodeCanceled          = "canceled"              // 调用方取消
	PFCodeUnreadable        = "unreadable"            // 读不了
	PFCodeSymlink           = "symlink"               // 符号链接
	PFCodeSpecialFile       = "special_file"          // FIFO、socket、设备文件等
	PFCodeFilter            = "filter"                // 调用方过滤器判超出范围（原因文字由过滤器给）
	PFCodeTooDeep           = "too_deep"              // 目录层数过深
)

// PFScanItem 是扫描结果里的一个条目（不含被排除的）。
type PFScanItem struct {
	Path    string // 相对单元目录的路径，/ 分隔
	Kind    PFKind
	Stat    PFStat // HasStat 为 false 时无意义
	HasStat bool
	State   PFState
	Code    string // State 不是 ok 时的原因代码
	Reason  string // 给人看的原因
	Hash    string // 小写十六进制 sha256；只有普通文件、哈希完成后才有
	// FromCache 表示 Hash 来自哈希缓存（没读文件内容）。
	FromCache bool
	// Stub 表示这个条目是被同目录的 .X.icloud 标成不明的（X 本身可能根本不在本地）。
	Stub bool
}

// PFScanVerdictKind 是过滤器对一个名字的判断。
type PFScanVerdictKind int

const (
	PFScanKeep       PFScanVerdictKind = iota // 零值：保留
	PFScanExclude                             // 排除：不同步、不报错、只计数；目录不进入
	PFScanOutOfScope                          // 超出范围：报告（带原因）；目录不进入
)

// PFScanVerdict 是过滤器的返回值；零值 = 保留。
type PFScanVerdict struct {
	Kind   PFScanVerdictKind
	Reason string // 超出范围时的原因（中文，给人看）
}

// PFScanFilter 由调用方传入（T1 的名字规则在 T8/T9 接上），rel 是相对单元目录的路径（/ 分隔）。
// 对每个条目（目录和文件）调用一次；父目录已通过才会问到子条目。
// isDir 是目录项自带的类型提示；若 Lstat 后发现类型不同，会按真实类型再问一次。
type PFScanFilter func(rel string, isDir bool) PFScanVerdict

// PFScanOptions 是扫描和哈希共用的选项；零值可用（但没有过滤器，什么都不排除）。
type PFScanOptions struct {
	// Stat 缺省 PFLstat；测试注入用。
	Stat PFStatFunc
	// Filter 缺省不过滤。
	Filter PFScanFilter
	// OnExcluded 每排除一个条目（含目录）回调一次，调用方可以借此收集特定名字（如 Office 锁）。
	OnExcluded func(rel string, isDir bool)
	// Now 缺省 time.Now；静止判断用。
	Now func() time.Time
	// QuietSeconds <= 0 取缺省 10。
	QuietSeconds int
	// Open 缺省 os.Open；测试注入用（统计有没有读过占位文件、让读变慢、读的中途改文件）。
	Open func(path string) (io.ReadCloser, error)
	// HashBudget 是整批哈希的时间预算；<= 0 不限。到时没算完的标不明（hash_timeout）。
	HashBudget time.Duration
}

func (o *PFScanOptions) stat() PFStatFunc {
	if o.Stat != nil {
		return o.Stat
	}
	return PFLstat
}

func (o *PFScanOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *PFScanOptions) quiet() int {
	if o.QuietSeconds <= 0 {
		return PFDefaultQuietSeconds
	}
	return o.QuietSeconds
}

func (o *PFScanOptions) open(path string) (io.ReadCloser, error) {
	if o.Open != nil {
		return o.Open(path)
	}
	return os.Open(path)
}

// PFIsQuiet 判断 mtime 距 now 是否已满 quietSeconds 秒（quietSeconds <= 0 取缺省 10）。
// mtime 晚于 now（时钟不一致）按未静止处理，宁可判得更晚。
func PFIsQuiet(mtimeNs int64, now time.Time, quietSeconds int) bool {
	if quietSeconds <= 0 {
		quietSeconds = PFDefaultQuietSeconds
	}
	return now.UnixNano()-mtimeNs >= int64(quietSeconds)*int64(time.Second)
}

// PFScanResult 是一次扫描（可能还加了哈希）的结果。
type PFScanResult struct {
	Root      string
	Items     []PFScanItem // 按 Path 字节序排序；不含被排除的
	Excluded  int          // 被排除的条目数（含目录；目录里面的不再数）
	ScannedAt time.Time
	HashStats PFHashStats // PFHashItems 填写
}

// Find 按相对路径找条目。
func (r *PFScanResult) Find(path string) (PFScanItem, bool) {
	i := sort.Search(len(r.Items), func(i int) bool { return r.Items[i].Path >= path })
	if i < len(r.Items) && r.Items[i].Path == path {
		return r.Items[i], true
	}
	return PFScanItem{}, false
}

// SubtreeSkipped 判断 path 是不是落在一个「没进去看」的子树里：某级祖先是符号链接，
// 或是不明/超出范围的目录（读不了、iCloud 占位、被过滤器判超出范围、层数过深）。
// 是的话返回那个祖先的路径。判定层必须用它：这种子树下 base 里有、扫描里没有的路径，不是「删除」，是「没看」。
func (r *PFScanResult) SubtreeSkipped(path string) (string, bool) {
	for i := 0; i < len(path); i++ {
		if path[i] != '/' {
			continue
		}
		anc := path[:i]
		if it, ok := r.Find(anc); ok {
			if it.Kind == PFKindSymlink || (it.Kind == PFKindDir && it.State != PFStateOK) {
				return anc, true
			}
		}
	}
	return "", false
}

// PFScanSummary 是各类条目的计数。
type PFScanSummary struct {
	Dirs               int   // 在范围内的目录
	Files              int   // 在范围内的普通文件（State=ok）
	HashedFiles        int   // 其中已有哈希的
	Bytes              int64 // State=ok 的普通文件总字节数
	Unknown            int   // 不明（所有原因）
	UnknownNotQuiet    int
	UnknownPlaceholder int
	OutOfScope         int
	Excluded           int
}

// Summary 统计各类条目。
func (r *PFScanResult) Summary() PFScanSummary {
	s := PFScanSummary{Excluded: r.Excluded}
	for _, it := range r.Items {
		switch it.State {
		case PFStateOK:
			switch it.Kind {
			case PFKindDir:
				s.Dirs++
			case PFKindFile:
				s.Files++
				s.Bytes += it.Stat.Size
				if it.Hash != "" {
					s.HashedFiles++
				}
			}
		case PFStateUnknown:
			s.Unknown++
			switch it.Code {
			case PFCodeNotQuiet:
				s.UnknownNotQuiet++
			case PFCodeICloudPlaceholder:
				s.UnknownPlaceholder++
			}
		case PFStateOutOfScope:
			s.OutOfScope++
		}
	}
	return s
}

// ---- 扫描 ----

type pfScanner struct {
	root     string
	opt      PFScanOptions
	stat     PFStatFunc
	now      time.Time
	quiet    int
	items    []PFScanItem
	excluded int
}

// PFScan 从 root 递归 Lstat。只在这些情况返回 error：root 读不了或不是真实目录、调用方取消。
// 其余读不了的目录/文件都作为条目报出来，不吞掉。
// 返回的普通文件条目 State=ok 的还没有哈希，要接着调 PFHashItems（或一步到位的 PFScanAndHash）。
func PFScan(ctx context.Context, root string, opt PFScanOptions) (*PFScanResult, error) {
	s := &pfScanner{root: root, opt: opt, stat: opt.stat(), now: opt.now(), quiet: opt.quiet()}
	st, err := s.stat(root)
	if err != nil {
		return nil, fmt.Errorf("读取单元目录 %s 失败: %w", root, err)
	}
	if st.Kind != PFKindDir {
		return nil, fmt.Errorf("%w: %s 是%s", ErrPFRootNotDir, root, pfKindName(st.Kind))
	}
	if st.Dataless() {
		return nil, fmt.Errorf("%w: %s 是 iCloud 占位目录", ErrPFRootNotDir, root)
	}
	if err := s.walk(ctx, "", -1, 0); err != nil {
		return nil, err
	}
	sort.Slice(s.items, func(i, j int) bool { return s.items[i].Path < s.items[j].Path })
	return &PFScanResult{Root: root, Items: s.items, Excluded: s.excluded, ScannedAt: s.now}, nil
}

func pfKindName(k PFKind) string {
	switch k {
	case PFKindFile:
		return "普通文件"
	case PFKindDir:
		return "目录"
	case PFKindSymlink:
		return "符号链接"
	}
	return "特殊文件"
}

func pfJoinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func (s *pfScanner) fullPath(rel string) string {
	if rel == "" {
		return s.root
	}
	return filepath.Join(s.root, filepath.FromSlash(rel))
}

func (s *pfScanner) add(it PFScanItem) int {
	s.items = append(s.items, it)
	return len(s.items) - 1
}

func (s *pfScanner) exclude(rel string, isDir bool) {
	s.excluded++
	if s.opt.OnExcluded != nil {
		s.opt.OnExcluded(rel, isDir)
	}
}

func (s *pfScanner) verdict(rel string, isDir bool) PFScanVerdict {
	if s.opt.Filter == nil {
		return PFScanVerdict{}
	}
	return s.opt.Filter(rel, isDir)
}

// applyVerdict 处理排除和超出范围，返回 true 表示这个条目已处理完。
func (s *pfScanner) applyVerdict(v PFScanVerdict, rel string, isDir bool) bool {
	switch v.Kind {
	case PFScanExclude:
		s.exclude(rel, isDir)
		return true
	case PFScanOutOfScope:
		kind := PFKindFile
		if isDir {
			kind = PFKindDir
		}
		reason := v.Reason
		if reason == "" {
			reason = "名字规则判为超出范围"
		}
		s.add(PFScanItem{Path: rel, Kind: kind, State: PFStateOutOfScope, Code: PFCodeFilter, Reason: reason})
		return true
	}
	return false
}

// pfICloudStubTarget 认 `.X.icloud` 占位文件名，返回 X。
func pfICloudStubTarget(name string) (string, bool) {
	const suffix = ".icloud"
	if len(name) > len(suffix)+1 && name[0] == '.' && strings.HasSuffix(name, suffix) {
		return name[1 : len(name)-len(suffix)], true
	}
	return "", false
}

func (s *pfScanner) walk(ctx context.Context, dirRel string, dirIdx, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full := s.fullPath(dirRel)
	ents, err := os.ReadDir(full)
	if err != nil {
		if dirIdx < 0 {
			return fmt.Errorf("读取单元目录 %s 失败: %w", full, err)
		}
		// 读不了的目录：整个子树「没看」，报出来，不当成空。部分列出的条目一概不要（列表不完整）。
		it := &s.items[dirIdx]
		it.State, it.Code, it.Reason = PFStateOutOfScope, PFCodeUnreadable, "读不了目录: "+pfErrText(err)
		return nil
	}
	// 先找 .X.icloud：它的存在说明 X 的内容只在云端。
	stubs := map[string]bool{}
	present := map[string]bool{}
	for _, e := range ents {
		present[e.Name()] = true
		if x, ok := pfICloudStubTarget(e.Name()); ok && !e.IsDir() {
			stubs[x] = true
		}
	}
	for _, e := range ents {
		name := e.Name()
		if _, ok := pfICloudStubTarget(name); ok && !e.IsDir() {
			continue // 占位文件本身只作信号，不单独成条目
		}
		if err := s.entry(ctx, pfJoinRel(dirRel, name), e.IsDir(), stubs[name], depth); err != nil {
			return err
		}
	}
	// X 本身不在本地（老式 iCloud：只剩 .X.icloud）：不能当成「没有」，补一个不明条目。
	var lone []string
	for x := range stubs {
		if !present[x] {
			lone = append(lone, x)
		}
	}
	sort.Strings(lone)
	for _, x := range lone {
		rel := pfJoinRel(dirRel, x)
		if s.applyVerdict(s.verdict(rel, false), rel, false) {
			continue
		}
		s.add(PFScanItem{Path: rel, Kind: PFKindFile, State: PFStateUnknown, Code: PFCodeICloudPlaceholder,
			Reason: fmt.Sprintf("iCloud 占位（.%s.icloud），内容只在云端，本地没有这个文件", x), Stub: true})
	}
	return nil
}

func pfErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func (s *pfScanner) entry(ctx context.Context, rel string, hintDir, stub bool, depth int) error {
	if s.applyVerdict(s.verdict(rel, hintDir), rel, hintDir) {
		return nil
	}
	st, err := s.stat(s.fullPath(rel))
	if err != nil {
		kind := PFKindFile
		if hintDir {
			kind = PFKindDir
		}
		it := PFScanItem{Path: rel, Kind: kind}
		if errors.Is(err, fs.ErrNotExist) {
			it.State, it.Code, it.Reason = PFStateUnknown, PFCodeVanished, "扫描时消失（刚被改名或删除），本轮不明"
		} else {
			it.State, it.Code, it.Reason = PFStateOutOfScope, PFCodeUnreadable, "读不了属性: "+pfErrText(err)
		}
		s.add(it)
		return nil
	}
	if isDir := st.Kind == PFKindDir; isDir != hintDir {
		if s.applyVerdict(s.verdict(rel, isDir), rel, isDir) {
			return nil
		}
	}
	it := PFScanItem{Path: rel, Kind: st.Kind, Stat: st, HasStat: true, State: PFStateOK}
	markPlaceholder := func() {
		it.State, it.Code = PFStateUnknown, PFCodeICloudPlaceholder
		if stub {
			it.Stub = true
			it.Reason = fmt.Sprintf("iCloud 占位（.%s.icloud），内容只在云端", pfBase(rel))
		} else {
			it.Reason = "iCloud 占位文件，内容只在云端，不读取（读会触发下载）"
		}
	}
	switch st.Kind {
	case PFKindSymlink:
		it.State, it.Code, it.Reason = PFStateOutOfScope, PFCodeSymlink, "符号链接：不跟随、不进入"
		s.add(it)
	case PFKindOther:
		it.State, it.Code, it.Reason = PFStateOutOfScope, PFCodeSpecialFile, "特殊文件（"+pfSpecialName(st.Mode)+"）：不同步"
		s.add(it)
	case PFKindDir:
		switch {
		case stub:
			markPlaceholder()
		case st.Dataless():
			markPlaceholder()
			it.Reason = "iCloud 占位目录，内容只在云端，不进入（读会触发下载）"
		case depth >= pfMaxDepth:
			it.State, it.Code, it.Reason = PFStateOutOfScope, PFCodeTooDeep, fmt.Sprintf("目录层数超过 %d，不进入", pfMaxDepth)
		}
		idx := s.add(it)
		if it.State == PFStateOK {
			return s.walk(ctx, rel, idx, depth+1)
		}
	default: // 普通文件
		switch {
		case stub, st.Dataless():
			markPlaceholder()
		case !PFIsQuiet(st.MtimeNs, s.now, s.quiet):
			it.State, it.Code = PFStateUnknown, PFCodeNotQuiet
			if age := s.now.UnixNano() - st.MtimeNs; age < 0 {
				it.Reason = "未静止：修改时间晚于当前时间（时钟不一致），按未静止处理"
			} else {
				it.Reason = fmt.Sprintf("未静止：修改时间距今 %.1f 秒，不足 %d 秒（可能还在写）", float64(age)/1e9, s.quiet)
			}
		}
		s.add(it)
	}
	return nil
}

func pfBase(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

func pfSpecialName(m fs.FileMode) string {
	switch {
	case m&fs.ModeNamedPipe != 0:
		return "FIFO"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "设备文件"
	}
	return "其他"
}

// ---- 单元指纹 ----

func pfPutField(h io.Writer, b []byte) {
	var n [binary.MaxVarintLen64]byte
	h.Write(n[:binary.PutUvarint(n[:], uint64(len(b)))])
	h.Write(b)
}

func pfPutInt(h io.Writer, v uint64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	h.Write(n[:])
}

// Fingerprint 是单元指纹（§5.2 第 2 步）：排序后每个条目的 (path, size, mtime_ns, ctime_ns, ino) 的 sha256，
// 用来快速判断这个单元有没有变化。细节：
//   - 目录只取 (path, ino, mode)，不取 size/mtime/ctime：目录的这几项会因为排除的文件（.DS_Store 等）出现或消失而变，
//     会造成无谓的整轮重扫；目录里真正的变化已经体现在子条目上。
//   - 没有 stat 的条目（读不了、消失、iCloud 占位只剩 .X.icloud、过滤器判超出范围）取 (path, 原因代码)。
//   - 不含 State：「未静止变成已静止」这种只随时间发生的变化指纹看不出来，
//     所以单元里还有不明条目（Summary().Unknown > 0）时，调用方不能只靠指纹决定要不要重扫。
func (r *PFScanResult) Fingerprint() string {
	h := sha256.New()
	for _, it := range r.Items {
		pfPutField(h, []byte(it.Path))
		pfPutField(h, []byte(it.Kind))
		switch {
		case !it.HasStat:
			pfPutField(h, []byte("!"+it.Code))
		case it.Kind == PFKindDir:
			pfPutInt(h, it.Stat.Ino)
			pfPutInt(h, uint64(it.Stat.Mode))
		default:
			pfPutInt(h, uint64(it.Stat.Size))
			pfPutInt(h, uint64(it.Stat.MtimeNs))
			pfPutInt(h, uint64(it.Stat.CtimeNs))
			pfPutInt(h, it.Stat.Ino)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- 两次扫描比较（开始同步前置检查，§12.3 第 2 步） ----

// PFScanDiff 是两次扫描之间的一处差别。
type PFScanDiff struct {
	Path string
	Why  string // "新增"、"消失"、"变动"
}

// pfStableSig 是「间隔 N 秒两次扫描是否相同」用的签名：(size, mtime_ns, ino)；目录只看 ino；没有 stat 的看原因代码。
func pfStableSig(it PFScanItem) string {
	switch {
	case !it.HasStat:
		return "!" + it.Code
	case it.Kind == PFKindDir:
		return fmt.Sprintf("d|%d", it.Stat.Ino)
	}
	return fmt.Sprintf("%s|%d|%d|%d", it.Kind, it.Stat.Size, it.Stat.MtimeNs, it.Stat.Ino)
}

// PFScansStable 比较两次扫描：(size, mtime_ns, ino) 指纹完全相同才算稳定（条目的增减也算不稳定）。
// 返回所有差别，按路径排序（调用方要列「最多 50 个」自己截）。
// 它不判断静止；「所有文件 mtime 距今 >= quiet_seconds」另看 Summary().UnknownNotQuiet == 0。
func PFScansStable(a, b *PFScanResult) (bool, []PFScanDiff) {
	am := make(map[string]string, len(a.Items))
	for _, it := range a.Items {
		am[it.Path] = pfStableSig(it)
	}
	var diffs []PFScanDiff
	seen := make(map[string]bool, len(b.Items))
	for _, it := range b.Items {
		seen[it.Path] = true
		old, ok := am[it.Path]
		switch {
		case !ok:
			diffs = append(diffs, PFScanDiff{it.Path, "新增"})
		case old != pfStableSig(it):
			diffs = append(diffs, PFScanDiff{it.Path, "变动"})
		}
	}
	for _, it := range a.Items {
		if !seen[it.Path] {
			diffs = append(diffs, PFScanDiff{it.Path, "消失"})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return len(diffs) == 0, diffs
}

// QuietFingerprint 是 (size, mtime_ns, ino) 指纹的哈希形式，只在要存下来或传出去比较时用；
// 要知道差在哪用 PFScansStable。
func (r *PFScanResult) QuietFingerprint() string {
	h := sha256.New()
	for _, it := range r.Items {
		pfPutField(h, []byte(it.Path))
		pfPutField(h, []byte(pfStableSig(it)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PFScanAndHash 扫描后接着哈希：每个普通文件最终是「有哈希 / 不明 / 超出范围」之一。
// cache 可为 nil。返回的 error 只会是调用方取消（此时结果仍然完整，没算完的标 canceled）
// 或扫描本身失败（结果为 nil）。哈希预算到了不算错误。
func PFScanAndHash(ctx context.Context, root string, cache *PFHashCache, opt PFScanOptions) (*PFScanResult, error) {
	res, err := PFScan(ctx, root, opt)
	if err != nil {
		return nil, err
	}
	return res, PFHashItems(ctx, root, res, cache, opt)
}
