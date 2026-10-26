package projsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// 项目文件同步的「上次对齐清单」（base）：每个项目单元一份，记下上次两边一致时每个路径的状态，
// 判定层拿它做三方比较（技术设计 §2）。
//
// 磁盘上（<meta> 是 Mac 的 ~/.xxyi/projfiles/，只在 Mac 存一份权威 base）：
//
//	<meta>/base/<键>.json          快照：整份 base + 校验和，原子写
//	<meta>/base/<键>.journal       日志：快照之后的逐条变更（JSONL），每次追加都 fsync
//	<meta>/base/<键>.ledger/<对哈希>.json   台账 JSON 的导出副本（给以后三方合并当 base）
//	<meta>/base/坏/<键>-<时间>.json|.journal   损坏后被移走的原文件
//
// 键 = 归一化项目编号（ASCII，如 261001J0101）。
//
// 并发前提（重要）：同一单元同一时间只有一个写者，由调用方持单元锁保证；
// 本文件不做跨进程加锁。对象内部有一把互斥锁，只保护同一进程内「一个协程写、别的协程读视图」。
// 同一个单元不能同时被两个进程（或两个 PFBase 对象）加载并写入。
//
// 总原则「识别边界、超出就停」：
//   - 内容损坏（JSON 坏、校验和不对、版本不认识、日志中间行坏）→ 原文件移进 坏/，返回「按首次处理」和原因，绝不静默清空。
//   - 读不了（权限、IO 错误）→ 返回 error，什么都不动。
//   - 快照不存在 → 返回「无 base」，调用方按首次规则（§3.2：所有路径当 B=无，只用判定表第 11–14 行，不推断任何删除）。
//   - 写日志或压实出错后，磁盘状态可能和内存不一致：对象进入「出错」状态，后续写入一律拒绝，必须重新加载。
//
// 调用方（T9）的典型用法：
//
//	r, err := PFBaseLoad(meta, key)         // err: 读不了，本单元本轮停下
//	if r.Status != PFBaseOK {               // 无 base 或已损坏：按首次规则，r.Reason 报给用户
//	    b, err = PFBaseCreate(meta, key, m) // 建一份空 base（gen=1）
//	}
//	…每个动作提交成功后 b.Set / b.Del（或同批的多个用 b.Apply）…
//	b.SetMeta(m); b.Compact()               // 单元一轮结束
//
// 首次规则里 dir_id 变了（G2）、或 base 不可信（G5）而 base 其实还能读时，先 b.Reset(meta) 清掉旧条目：
// 旧条目里已经不存在的路径若留下来，之后某一边碰巧新建同内容文件，会被误判成「另一边删了」。

const (
	pfBaseVersion = 1
	pfBadDirName  = "坏"
	pfStampLayout = "20060102-150405.000"
	// 日志里除了设计里的 set、del，还有 ledger：台账那一对文件的哈希变更，同样要「提交即落盘」。
	pfOpSet    = "set"
	pfOpDel    = "del"
	pfOpLedger = "ledger"
)

// 条目类型。
const (
	PFBaseTypeFile = "f"
	PFBaseTypeDir  = "d"
)

// ErrPFBaseBroken 表示写入出错后 base 的磁盘状态不确定：必须重新 PFBaseLoad，在此之前不能再写。
var ErrPFBaseBroken = errors.New("projsync: base 写入出错后状态不确定，必须重新加载")

// ErrPFBaseExists 表示 PFBaseCreate 时已有快照：先 PFBaseLoad，不要覆盖。
var ErrPFBaseExists = errors.New("projsync: 该单元已有 base 快照")

// PFDirID 是单元目录的 dev+ino，用来发现「整个目录被换掉」（G2）。
// 注意 macOS 上 dev 在重启或重新挂载后可能变，变了只会让本轮走保守的首次规则，不会写错。
type PFDirID struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// IsZero 表示没有记录（dev、ino 都是 0）。
func (d PFDirID) IsZero() bool { return d.Dev == 0 && d.Ino == 0 }

// PFSideStat 是某一边（Mac 或 Ubuntu）对齐时看到的文件 stat 指纹，给哈希缓存和「没变」判断用。
type PFSideStat struct {
	MtimeNs int64  `json:"mtime_ns"`
	Ino     uint64 `json:"ino"`
}

// PFBaseEntry 是 base 里一个路径的状态。
// 文件（T=f）：Size 和 SHA256（小写 64 位十六进制）必填；目录（T=d）：Size 为 0、SHA256 为空。
// Mac、Ub 分别是两边对齐时的 mtime_ns 和 ino。
type PFBaseEntry struct {
	T      string     `json:"t"`
	Size   int64      `json:"size"`
	SHA256 string     `json:"sha256,omitempty"`
	Mac    PFSideStat `json:"mac"`
	Ub     PFSideStat `json:"ub"`
}

// PFBaseLedger 是历史版本台账那一对文件（档案台账.sqlite3 + 档案索引.json）的对齐记录（§7）。
// 结构先留字段，T10 填内容：Hash = 合并哈希（sha(sqlite)‖sha(json) 的十六进制连接）；
// Export = 台账 JSON 导出副本的引用，是相对 base 目录的路径（<键>.ledger/<对哈希>.json），见 SaveLedgerExport。
type PFBaseLedger struct {
	Hash   string `json:"hash,omitempty"`
	Export string `json:"export,omitempty"`
}

// PFBaseMeta 是单元级的元数据：两边的项目文件夹相对路径，和两边单元目录的 dev+ino。
type PFBaseMeta struct {
	MacRel      string
	UbuntuRel   string
	MacDirID    PFDirID
	UbuntuDirID PFDirID
}

// PFBaseOp 是 Apply 的一个变更：Del=true 删掉 Path；否则把 Path 设成 Entry。
type PFBaseOp struct {
	Path  string
	Del   bool
	Entry PFBaseEntry
}

// PFBaseStatus 是加载结果。
type PFBaseStatus int

const (
	// PFBaseOK 载入成功，Base 可用。
	PFBaseOK PFBaseStatus = iota
	// PFBaseMissing 没有 base（快照不存在）：按首次规则。
	PFBaseMissing
	// PFBaseCorrupt base 损坏：原文件已移进 坏/，按首次规则，原因见 Reason。
	PFBaseCorrupt
)

func (s PFBaseStatus) String() string {
	switch s {
	case PFBaseOK:
		return "ok"
	case PFBaseMissing:
		return "missing"
	case PFBaseCorrupt:
		return "corrupt"
	}
	return "unknown"
}

// PFBaseLoadResult 是 PFBaseLoad 的结果。
type PFBaseLoadResult struct {
	Status PFBaseStatus
	Base   *PFBase  // 仅 Status==PFBaseOK 时非 nil
	Reason string   // 无 base 或损坏的原因，给用户看
	Moved  []string // 被移进 坏/ 的文件的新路径
	Notes  []string // 载入成功时的提示：丢掉了半行日志、忽略了旧一代日志等
}

// FirstTime 表示调用方这一单元要按首次规则处理（没有可用的 base）。
func (r PFBaseLoadResult) FirstTime() bool { return r.Status != PFBaseOK }

// PFDirIDCheck 是单元目录 dev+ino 和 base 记录的对比结果。
type PFDirIDCheck struct {
	MacSame    bool
	UbuntuSame bool
}

// PFBaseView 是给判定层（T5）的只读视图。*PFBase 实现它；测试判定函数时也可以自己实现一个内存版。
type PFBaseView interface {
	Unit() string
	Gen() uint64
	Meta() PFBaseMeta
	// Get 按路径查条目。
	Get(path string) (PFBaseEntry, bool)
	// Len 是条目数。
	Len() int
	// Range 按路径字节序遍历全部条目（遍历的是调用时刻的拷贝，回调里可以随便调用 base 的任何方法）；fn 返回 false 提前结束。
	Range(fn func(path string, e PFBaseEntry) bool)
	// Entries 返回全部条目的拷贝。
	Entries() map[string]PFBaseEntry
	// Ledger 是台账那一对文件的对齐记录。
	Ledger() PFBaseLedger
	// CompareDirIDs 把两边当前的单元目录 dev+ino 和 base 记录的对比。
	// base 里没记录（零值）或不相等都算「不同」，调用方按 G2 走首次规则。
	CompareDirIDs(mac, ubuntu PFDirID) PFDirIDCheck
}

// PFBase 是一个单元的 base。创建用 PFBaseLoad / PFBaseCreate，不要直接构造。
type PFBase struct {
	mu      sync.RWMutex
	baseDir string // <meta>/base
	unit    string
	env     pfBaseEnv

	gen     uint64
	meta    PFBaseMeta
	updated string
	entries map[string]PFBaseEntry
	ledger  PFBaseLedger

	journalLines int   // 当前日志里属于本代的行数
	broken       error // 非 nil：写入出错，状态不确定
}

var _ PFBaseView = (*PFBase)(nil)

// pfBaseEnv 是可注入的时钟和钩子，只给测试用，生产代码是零值。
type pfBaseEnv struct {
	hook pfHook
	now  func() time.Time
}

func (e pfBaseEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e pfBaseEnv) stamp() string { return e.clock().UTC().Format(time.RFC3339Nano) }

// ---------- 磁盘格式 ----------

type pfSnapBody struct {
	Version     int                    `json:"version"`
	Unit        string                 `json:"unit"`
	Gen         uint64                 `json:"gen"`
	MacRel      string                 `json:"mac_rel"`
	UbuntuRel   string                 `json:"ubuntu_rel"`
	MacDirID    PFDirID                `json:"mac_dir_id"`
	UbuntuDirID PFDirID                `json:"ubuntu_dir_id"`
	Updated     string                 `json:"updated"`
	Entries     map[string]PFBaseEntry `json:"entries"`
	Ledger      PFBaseLedger           `json:"ledger"`
}

// pfSnapFile 是快照文件：正文各字段（嵌入，JSON 里平铺）+ checksum。
// checksum = 正文（不含 checksum）规范化 JSON 的 sha256。
type pfSnapFile struct {
	pfSnapBody
	Checksum string `json:"checksum"`
}

type pfJournalRec struct {
	Gen    uint64        `json:"gen"`
	Path   string        `json:"path,omitempty"`
	Op     string        `json:"op"`
	Entry  *PFBaseEntry  `json:"entry,omitempty"`
	Ledger *PFBaseLedger `json:"ledger,omitempty"`
	T      string        `json:"t"`
}

// pfCanonJSON 是规范化 JSON：字段按结构体顺序、map 按键排序、不转义 HTML 字符、无多余空白、无结尾换行。
// 校验和和落盘都用它，同一份内容两次序列化结果字节相同。
func pfCanonJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func pfBodyChecksum(body *pfSnapBody) (string, error) {
	b, err := pfCanonJSON(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// pfCorrupt 表示 base 的内容损坏（区别于读不了的 IO 错误）。
type pfCorrupt struct{ reason string }

func (c *pfCorrupt) Error() string { return c.reason }

func pfCorruptf(format string, a ...any) *pfCorrupt {
	return &pfCorrupt{reason: fmt.Sprintf(format, a...)}
}

// ---------- 校验 ----------

// pfCheckUnitKey 校验单元键：1–64 个 ASCII 字母数字、下划线、连字符（归一化项目编号），不含任何路径字符。
func pfCheckUnitKey(key string) error {
	if key == "" || len(key) > 64 {
		return fmt.Errorf("projsync: 单元键 %q 长度不合法", key)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		ok := c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("projsync: 单元键 %q 含不允许的字符", key)
		}
	}
	return nil
}

// pfCheckBasePath 校验 base 里的路径：合法 UTF-8（Go 的 JSON 会把非法字节改写成替换字符，
// 写进去再读出来就成了另一个名字）+ validRel 的相对路径规则。
func pfCheckBasePath(p string) error {
	if !utf8.ValidString(p) {
		return fmt.Errorf("projsync: 路径 %q 不是合法 UTF-8", p)
	}
	return validRel(p)
}

func pfIsHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func pfCheckBaseEntry(path string, e PFBaseEntry) error {
	if err := pfCheckBasePath(path); err != nil {
		return err
	}
	switch e.T {
	case PFBaseTypeFile:
		if e.Size < 0 {
			return fmt.Errorf("projsync: %q 的大小 %d 为负", path, e.Size)
		}
		if !pfIsHex64(e.SHA256) {
			return fmt.Errorf("projsync: %q 的 sha256 不是 64 位小写十六进制", path)
		}
	case PFBaseTypeDir:
		if e.Size != 0 || e.SHA256 != "" {
			return fmt.Errorf("projsync: 目录 %q 不应有大小或哈希", path)
		}
	default:
		return fmt.Errorf("projsync: %q 的类型 %q 不认识（只有 f、d）", path, e.T)
	}
	return nil
}

func pfCheckBaseMeta(m PFBaseMeta) error {
	if !utf8.ValidString(m.MacRel) || !utf8.ValidString(m.UbuntuRel) {
		return errors.New("projsync: mac_rel / ubuntu_rel 不是合法 UTF-8")
	}
	return nil
}

func pfCheckLedger(l PFBaseLedger) error {
	if !utf8.ValidString(l.Hash) || !utf8.ValidString(l.Export) {
		return errors.New("projsync: 台账记录不是合法 UTF-8")
	}
	return nil
}

// ---------- 快照解析 ----------

// pfParseSnapshot 解析并校验快照；任何问题都是 *pfCorrupt。
func pfParseSnapshot(raw []byte, unit string) (*pfSnapBody, *pfCorrupt) {
	var ver struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(raw, &ver); err != nil {
		return nil, pfCorruptf("快照不是合法 JSON: %v", err)
	}
	if ver.Version == nil {
		return nil, pfCorruptf("快照没有 version 字段")
	}
	if *ver.Version != pfBaseVersion {
		return nil, pfCorruptf("快照版本 %d 不认识（本程序只认 %d）", *ver.Version, pfBaseVersion)
	}
	var f pfSnapFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, pfCorruptf("快照内容不合规: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, pfCorruptf("快照末尾有多余内容")
	}
	if f.Entries == nil {
		f.Entries = map[string]PFBaseEntry{}
	}
	sum, err := pfBodyChecksum(&f.pfSnapBody)
	if err != nil {
		return nil, pfCorruptf("快照无法规范化: %v", err)
	}
	if f.Checksum != sum {
		return nil, pfCorruptf("快照校验和不对（文件记 %q，算得 %q）", f.Checksum, sum)
	}
	if f.Unit != unit {
		return nil, pfCorruptf("快照属于单元 %q，不是 %q", f.Unit, unit)
	}
	if f.Gen < 1 {
		return nil, pfCorruptf("快照 gen 为 0")
	}
	for p, e := range f.Entries {
		if err := pfCheckBaseEntry(p, e); err != nil {
			return nil, pfCorruptf("快照条目不合规: %v", err)
		}
	}
	return &f.pfSnapBody, nil
}

// ---------- 日志回放 ----------

func pfParseJournalLine(line []byte) (*pfJournalRec, error) {
	var r pfJournalRec
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("行末有多余内容")
	}
	if r.Gen < 1 {
		return nil, errors.New("gen 缺失")
	}
	switch r.Op {
	case pfOpSet:
		if r.Entry == nil || r.Ledger != nil {
			return nil, errors.New("set 必须带 entry")
		}
		if err := pfCheckBaseEntry(r.Path, *r.Entry); err != nil {
			return nil, err
		}
	case pfOpDel:
		if r.Entry != nil || r.Ledger != nil {
			return nil, errors.New("del 不能带 entry")
		}
		if err := pfCheckBasePath(r.Path); err != nil {
			return nil, err
		}
	case pfOpLedger:
		if r.Ledger == nil || r.Entry != nil || r.Path != "" {
			return nil, errors.New("ledger 必须只带 ledger")
		}
	default:
		return nil, fmt.Errorf("op %q 不认识", r.Op)
	}
	return &r, nil
}

type pfReplayResult struct {
	applied  int    // 回放了几行
	stale    int    // 旧一代、忽略了几行
	validLen int    // 日志里完整可用部分的字节数
	tail     []byte // 末尾丢掉的半行（没有则为空）
}

// pfReplay 把日志里 gen == snapGen 的行依次作用到 entries、ledger 上。
//   - 最后一行写了一半（没有换行结尾，或有换行但不是合规的一行）→ 丢掉，放进 tail，继续。
//   - 中间某行坏 → *pfCorrupt。
//   - gen 比快照小：压实写完新快照、还没来得及截断日志时留下的旧行，内容已在新快照里，忽略。
//   - gen 比快照大：日志来自更新的一代，说明快照被回退过，base 不可信 → *pfCorrupt。
func pfReplay(data []byte, snapGen uint64, entries map[string]PFBaseEntry, ledger *PFBaseLedger) (pfReplayResult, *pfCorrupt) {
	var res pfReplayResult
	pos, lineNo := 0, 0
	for pos < len(data) {
		lineNo++
		nl := bytes.IndexByte(data[pos:], '\n')
		if nl < 0 {
			res.tail = data[pos:]
			break
		}
		line := data[pos : pos+nl]
		next := pos + nl + 1
		rec, err := pfParseJournalLine(line)
		if err != nil {
			if next == len(data) {
				res.tail = data[pos:]
				break
			}
			return res, pfCorruptf("日志第 %d 行损坏: %v", lineNo, err)
		}
		switch {
		case rec.Gen < snapGen:
			res.stale++
		case rec.Gen > snapGen:
			return res, pfCorruptf("日志第 %d 行属于第 %d 代，比快照（第 %d 代）新，快照可能被回退过", lineNo, rec.Gen, snapGen)
		default:
			switch rec.Op {
			case pfOpSet:
				entries[rec.Path] = *rec.Entry
			case pfOpDel:
				delete(entries, rec.Path)
			case pfOpLedger:
				*ledger = *rec.Ledger
			}
			res.applied++
		}
		pos = next
	}
	res.validLen = pos
	return res, nil
}

// ---------- 加载、创建 ----------

func pfBasePaths(metaDir, unit string) (baseDir, snap, journal string) {
	baseDir = filepath.Join(metaDir, "base")
	return baseDir, filepath.Join(baseDir, unit+".json"), filepath.Join(baseDir, unit+".journal")
}

// PFBaseLoad 加载 metaDir 下某单元的 base。
//
// 返回 error 只表示「读不了」（权限、IO 错误、参数不合法）：这时什么都没动，本单元本轮应停下。
// base 不存在或内容损坏不是 error，而是 Status 为 PFBaseMissing / PFBaseCorrupt、FirstTime() 为 true，
// 调用方按首次规则处理；损坏时原文件（快照和日志）已移进 <meta>/base/坏/。
func PFBaseLoad(metaDir, unit string) (PFBaseLoadResult, error) {
	return pfBaseLoad(metaDir, unit, pfBaseEnv{})
}

func pfBaseLoad(metaDir, unit string, env pfBaseEnv) (PFBaseLoadResult, error) {
	if err := pfCheckUnitKey(unit); err != nil {
		return PFBaseLoadResult{}, err
	}
	baseDir, snapPath, journalPath := pfBasePaths(metaDir, unit)
	pfRemoveStaleTmp(baseDir, unit)

	raw, err := os.ReadFile(snapPath)
	if errors.Is(err, fs.ErrNotExist) {
		return env.loadMissing(baseDir, unit, journalPath)
	}
	if err != nil {
		return PFBaseLoadResult{}, fmt.Errorf("读取 base 快照 %s 失败: %w", snapPath, err)
	}
	body, cerr := pfParseSnapshot(raw, unit)
	if cerr != nil {
		return env.loadCorrupt(baseDir, unit, cerr.reason)
	}

	jraw, err := os.ReadFile(journalPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return PFBaseLoadResult{}, fmt.Errorf("读取 base 日志 %s 失败: %w", journalPath, err)
	}
	b := &PFBase{
		baseDir: baseDir, unit: unit, env: env,
		gen: body.Gen, updated: body.Updated,
		meta:    PFBaseMeta{MacRel: body.MacRel, UbuntuRel: body.UbuntuRel, MacDirID: body.MacDirID, UbuntuDirID: body.UbuntuDirID},
		entries: body.Entries, ledger: body.Ledger,
	}
	rr, cerr := pfReplay(jraw, body.Gen, b.entries, &b.ledger)
	if cerr != nil {
		return env.loadCorrupt(baseDir, unit, cerr.reason)
	}
	var notes []string
	b.journalLines = rr.applied
	if rr.stale > 0 {
		notes = append(notes, fmt.Sprintf("忽略了 %d 行旧一代日志（压实后没来得及截断，内容已在快照里）", rr.stale))
	}
	if len(rr.tail) > 0 {
		saved, err := env.dropJournalTail(baseDir, unit, journalPath, rr)
		if err != nil {
			return PFBaseLoadResult{}, err
		}
		notes = append(notes, fmt.Sprintf("丢掉了日志末尾 %d 字节没写完的一行（原样存在 %s）", len(rr.tail), saved))
	}
	return PFBaseLoadResult{Status: PFBaseOK, Base: b, Notes: notes}, nil
}

// PFBaseCreate 为单元新建一份空 base（gen=1，立刻写出快照），用于首次规则：
// PFBaseLoad 返回无 base 或损坏之后调用。已有快照返回 ErrPFBaseExists。
// 目录里若留着没有快照可配的日志（孤立日志），先移进 坏/，免得它的旧行被新一代误认。
func PFBaseCreate(metaDir, unit string, meta PFBaseMeta) (*PFBase, error) {
	return pfBaseCreate(metaDir, unit, meta, pfBaseEnv{})
}

func pfBaseCreate(metaDir, unit string, meta PFBaseMeta, env pfBaseEnv) (*PFBase, error) {
	if err := pfCheckUnitKey(unit); err != nil {
		return nil, err
	}
	if err := pfCheckBaseMeta(meta); err != nil {
		return nil, err
	}
	baseDir, snapPath, journalPath := pfBasePaths(metaDir, unit)
	if _, err := os.Lstat(snapPath); err == nil {
		return nil, ErrPFBaseExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if _, _, err := env.quarantineOrphanJournal(baseDir, unit, journalPath); err != nil {
		return nil, err
	}
	b := &PFBase{baseDir: baseDir, unit: unit, env: env, entries: map[string]PFBaseEntry{}}
	if err := b.persistLocked(b.entries, PFBaseLedger{}, meta); err != nil {
		return nil, err
	}
	return b, nil
}

// pfRemoveStaleTmp 删掉上次崩溃留下的快照临时文件（单写者前提下没有别人在用它）。
func pfRemoveStaleTmp(baseDir, unit string) {
	m, _ := filepath.Glob(filepath.Join(baseDir, unit+".json.tmp-*"))
	for _, p := range m {
		os.Remove(p)
	}
}

// loadMissing 处理快照不存在：日志若有内容就是孤立日志，移进 坏/；返回「无 base」。
func (e pfBaseEnv) loadMissing(baseDir, unit, journalPath string) (PFBaseLoadResult, error) {
	res := PFBaseLoadResult{Status: PFBaseMissing, Reason: "没有上次对齐清单（快照不存在）"}
	moved, size, err := e.quarantineOrphanJournal(baseDir, unit, journalPath)
	if err != nil {
		return PFBaseLoadResult{}, err
	}
	if moved != "" {
		res.Moved = []string{moved}
		res.Reason += fmt.Sprintf("；日志有 %d 字节但没有快照可配，已移入 坏/", size)
	}
	return res, nil
}

// loadCorrupt 把快照和日志一起移进 坏/，返回「按首次处理」。
func (e pfBaseEnv) loadCorrupt(baseDir, unit, reason string) (PFBaseLoadResult, error) {
	_, snapPath, journalPath := pfBasePaths(filepath.Dir(baseDir), unit)
	moved, err := e.quarantine(baseDir, unit, []pfMoveItem{{snapPath, ".json"}, {journalPath, ".journal"}})
	if err != nil {
		return PFBaseLoadResult{}, fmt.Errorf("base 已损坏（%s），移入 坏/ 时失败: %w", reason, err)
	}
	return PFBaseLoadResult{
		Status: PFBaseCorrupt,
		Reason: "上次对齐清单已损坏：" + reason + "；原文件已移入 坏/",
		Moved:  moved,
	}, nil
}

// quarantineOrphanJournal 日志存在且非空时把它移进 坏/，返回新路径和字节数；空文件或不存在返回 "".
func (e pfBaseEnv) quarantineOrphanJournal(baseDir, unit, journalPath string) (string, int64, error) {
	fi, err := os.Lstat(journalPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if fi.Size() == 0 {
		return "", 0, nil
	}
	moved, err := e.quarantine(baseDir, unit, []pfMoveItem{{journalPath, ".journal"}})
	if err != nil {
		return "", 0, err
	}
	return moved[0], fi.Size(), nil
}

type pfMoveItem struct {
	src string
	ext string // 在 坏/ 里的扩展名
}

// quarantine 把存在的文件移进 <baseDir>/坏/<键>-<时间><扩展名>（同一批用同一个时间戳；重名加 -2、-3），
// 返回新路径。只移动，绝不删除。每一步移动后 fsync 两个目录。
func (e pfBaseEnv) quarantine(baseDir, unit string, items []pfMoveItem) ([]string, error) {
	var exist []pfMoveItem
	for _, it := range items {
		if _, err := os.Lstat(it.src); err == nil {
			exist = append(exist, it)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if len(exist) == 0 {
		return nil, nil
	}
	badDir := filepath.Join(baseDir, pfBadDirName)
	if err := pfMkdirAllSync(badDir, pfDirPerm); err != nil {
		return nil, err
	}
	stamp := e.clock().UTC().Format(pfStampLayout)
	var dsts []string
	for n := 1; ; n++ {
		suffix := stamp
		if n > 1 {
			suffix = fmt.Sprintf("%s-%d", stamp, n)
		}
		dsts = dsts[:0]
		clash := false
		for _, it := range exist {
			d := filepath.Join(badDir, unit+"-"+suffix+it.ext)
			if _, err := os.Lstat(d); err == nil {
				clash = true
				break
			}
			dsts = append(dsts, d)
		}
		if !clash {
			break
		}
	}
	for i, it := range exist {
		if err := os.Rename(it.src, dsts[i]); err != nil {
			return nil, err
		}
		if err := pfSyncDir(badDir); err != nil {
			return nil, err
		}
		if err := pfSyncDir(baseDir); err != nil {
			return nil, err
		}
		if err := pfStep(e.hook, "quarantine:moved"); err != nil {
			return nil, err
		}
	}
	return append([]string(nil), dsts...), nil
}

// dropJournalTail 把日志末尾没写完的一行原样存进 坏/（不丢任何字节），再把日志截到最后一个完整行，
// 这样后面追加的新行不会粘在半行后面。返回存放路径。
func (e pfBaseEnv) dropJournalTail(baseDir, unit, journalPath string, rr pfReplayResult) (string, error) {
	badDir := filepath.Join(baseDir, pfBadDirName)
	stamp := e.clock().UTC().Format(pfStampLayout)
	dst := filepath.Join(badDir, unit+"-"+stamp+".journal-tail")
	for n := 2; ; n++ {
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dst = filepath.Join(badDir, fmt.Sprintf("%s-%s-%d.journal-tail", unit, stamp, n))
	}
	if err := writeFileAtomic(dst, rr.tail, 0o600); err != nil {
		return "", fmt.Errorf("保存日志尾行失败: %w", err)
	}
	if err := pfStep(e.hook, "load:tail-saved"); err != nil {
		return "", err
	}
	f, err := os.OpenFile(journalPath, os.O_WRONLY, 0)
	if err != nil {
		return "", fmt.Errorf("截断日志 %s 失败: %w", journalPath, err)
	}
	defer f.Close()
	if err := f.Truncate(int64(rr.validLen)); err != nil {
		return "", fmt.Errorf("截断日志 %s 失败: %w", journalPath, err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("截断日志 %s 失败: %w", journalPath, err)
	}
	if err := pfStep(e.hook, "load:tail-trimmed"); err != nil {
		return "", err
	}
	return dst, nil
}

// ---------- 只读视图 ----------

// Unit 是单元键。
func (b *PFBase) Unit() string { return b.unit }

// Gen 是当前这一代（每次压实 +1）。
func (b *PFBase) Gen() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.gen
}

// Meta 是单元级元数据（两边的相对路径、dev+ino）。
func (b *PFBase) Meta() PFBaseMeta {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.meta
}

// Updated 是最近一次写出快照的时间（RFC3339，UTC）。
func (b *PFBase) Updated() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.updated
}

// Get 按路径查条目。
func (b *PFBase) Get(path string) (PFBaseEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e, ok := b.entries[path]
	return e, ok
}

// Len 是条目数。
func (b *PFBase) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries)
}

// Range 按路径字节序遍历全部条目；遍历的是调用时刻的拷贝，fn 里调用本对象的任何方法都不会死锁。
func (b *PFBase) Range(fn func(path string, e PFBaseEntry) bool) {
	b.mu.RLock()
	type kv struct {
		p string
		e PFBaseEntry
	}
	all := make([]kv, 0, len(b.entries))
	for p, e := range b.entries {
		all = append(all, kv{p, e})
	}
	b.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].p < all[j].p })
	for _, x := range all {
		if !fn(x.p, x.e) {
			return
		}
	}
}

// Entries 返回全部条目的拷贝。
func (b *PFBase) Entries() map[string]PFBaseEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	m := make(map[string]PFBaseEntry, len(b.entries))
	for p, e := range b.entries {
		m[p] = e
	}
	return m
}

// Ledger 是台账那一对文件的对齐记录。
func (b *PFBase) Ledger() PFBaseLedger {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ledger
}

// CompareDirIDs 见 PFBaseView。
func (b *PFBase) CompareDirIDs(mac, ubuntu PFDirID) PFDirIDCheck {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return PFDirIDCheck{
		MacSame:    !b.meta.MacDirID.IsZero() && b.meta.MacDirID == mac,
		UbuntuSame: !b.meta.UbuntuDirID.IsZero() && b.meta.UbuntuDirID == ubuntu,
	}
}

// JournalLines 是日志里属于当前这一代的行数，调用方可据此决定要不要提前压实。
func (b *PFBase) JournalLines() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.journalLines
}

// Err 非 nil 表示写入出错、状态不确定，必须重新加载（见 ErrPFBaseBroken）。
func (b *PFBase) Err() error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.broken
}

// ---------- 写 ----------

// Set 记下「这个路径现在两边一致，状态是 e」，追加日志并 fsync 后才返回。
// 路径不是合法 UTF-8 或条目不合规 → 返回 error，不写。和现有条目完全相同时什么也不写。
func (b *PFBase) Set(path string, e PFBaseEntry) error {
	return b.Apply([]PFBaseOp{{Path: path, Entry: e}})
}

// Del 记下「这个路径已不在 base 里」。路径本来就不在 base 里时什么也不写。
func (b *PFBase) Del(path string) error {
	return b.Apply([]PFBaseOp{{Path: path, Del: true}})
}

// Apply 批量变更：全部校验通过才写，一次写入、一次 fsync（便宜动作很多时比逐条 fsync 快得多）。
// 同一批里对同一路径的多个操作按顺序生效。出错时 base 的内存状态不变，但日志可能已部分落盘，
// 对象进入出错状态（Err()），必须重新加载。
func (b *PFBase) Apply(ops []PFBaseOp) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.broken != nil {
		return b.broken
	}
	type st struct {
		e   PFBaseEntry
		del bool
	}
	overlay := map[string]st{}
	cur := func(p string) (PFBaseEntry, bool) {
		if o, ok := overlay[p]; ok {
			return o.e, !o.del
		}
		e, ok := b.entries[p]
		return e, ok
	}
	now := b.env.stamp()
	var lines [][]byte
	for _, op := range ops {
		if op.Del {
			if err := pfCheckBasePath(op.Path); err != nil {
				return err
			}
			if _, ok := cur(op.Path); !ok {
				continue
			}
			overlay[op.Path] = st{del: true}
			line, err := pfCanonJSON(pfJournalRec{Gen: b.gen, Path: op.Path, Op: pfOpDel, T: now})
			if err != nil {
				return err
			}
			lines = append(lines, line)
			continue
		}
		if err := pfCheckBaseEntry(op.Path, op.Entry); err != nil {
			return err
		}
		if old, ok := cur(op.Path); ok && old == op.Entry {
			continue
		}
		e := op.Entry
		overlay[op.Path] = st{e: e}
		line, err := pfCanonJSON(pfJournalRec{Gen: b.gen, Path: op.Path, Op: pfOpSet, Entry: &e, T: now})
		if err != nil {
			return err
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil
	}
	if err := b.appendLocked(lines); err != nil {
		return err
	}
	for p, o := range overlay {
		if o.del {
			delete(b.entries, p)
		} else {
			b.entries[p] = o.e
		}
	}
	return nil
}

// SetLedger 记下台账那一对文件的对齐记录（日志里的 ledger 行），fsync 后返回。和现有记录相同时什么也不写。
// 先用 SaveLedgerExport 把导出副本落盘，再调用本函数，这样记录里引用的副本一定存在。
func (b *PFBase) SetLedger(l PFBaseLedger) error {
	if err := pfCheckLedger(l); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.broken != nil {
		return b.broken
	}
	if b.ledger == l {
		return nil
	}
	line, err := pfCanonJSON(pfJournalRec{Gen: b.gen, Op: pfOpLedger, Ledger: &l, T: b.env.stamp()})
	if err != nil {
		return err
	}
	if err := b.appendLocked([][]byte{line}); err != nil {
		return err
	}
	b.ledger = l
	return nil
}

// appendLocked 追加日志行；失败即进入出错状态。调用方持写锁。
func (b *PFBase) appendLocked(lines [][]byte) error {
	jp := filepath.Join(b.baseDir, b.unit+".journal")
	if err := appendLinesSync(jp, lines, 0o600, b.env.hook); err != nil {
		b.broken = fmt.Errorf("%w: 追加日志失败: %v", ErrPFBaseBroken, err)
		return err
	}
	b.journalLines += len(lines)
	return nil
}

// SetMeta 更新单元级元数据（两边的相对路径、dev+ino），只改内存，下一次 Compact 才落盘。
// 不进日志的理由：崩溃时丢了它，下一轮看到的只是「dev+ino 和 base 记的不同」，走保守的首次规则。
func (b *PFBase) SetMeta(m PFBaseMeta) error {
	if err := pfCheckBaseMeta(m); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.meta = m
	return nil
}

// Compact 压实：把当前全部内容写成新快照（gen+1，原子写 + 目录 fsync），再截断日志。
// 任何一步之间崩溃，重新加载都不会丢已经 fsync 过的条目：
// 新快照写好之前，旧快照 + 旧日志是完整的；新快照落盘之后，旧日志里的行 gen 偏小、会被忽略，而它们的内容已在新快照里。
func (b *PFBase) Compact() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.broken != nil {
		return b.broken
	}
	return b.persistLocked(b.entries, b.ledger, b.meta)
}

// Reset 用 meta 清空全部条目和台账记录，写成新一代快照（gen+1）。给首次规则用（G2、G5）：
// 这一轮不信旧 base，轮次里再逐个 Set 新条目。这是调用方显式要求的清空，不是静默清空。
func (b *PFBase) Reset(meta PFBaseMeta) error {
	if err := pfCheckBaseMeta(meta); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.broken != nil {
		return b.broken
	}
	return b.persistLocked(map[string]PFBaseEntry{}, PFBaseLedger{}, meta)
}

// persistLocked 写新一代快照并截断日志；成功后才把 entries、ledger、meta、gen 换进内存。调用方持写锁。
func (b *PFBase) persistLocked(entries map[string]PFBaseEntry, ledger PFBaseLedger, meta PFBaseMeta) error {
	if err := pfStep(b.env.hook, "compact:begin"); err != nil {
		return err
	}
	body := pfSnapBody{
		Version: pfBaseVersion, Unit: b.unit, Gen: b.gen + 1,
		MacRel: meta.MacRel, UbuntuRel: meta.UbuntuRel, MacDirID: meta.MacDirID, UbuntuDirID: meta.UbuntuDirID,
		Updated: b.env.stamp(), Entries: entries, Ledger: ledger,
	}
	if body.Entries == nil {
		body.Entries = map[string]PFBaseEntry{}
	}
	sum, err := pfBodyChecksum(&body)
	if err != nil {
		return err
	}
	data, err := pfCanonJSON(pfSnapFile{pfSnapBody: body, Checksum: sum})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	snapPath := filepath.Join(b.baseDir, b.unit+".json")
	renamed, err := pfWriteAtomic(snapPath, data, 0o600, pfPrefixHook(b.env.hook, "snap:"))
	if err != nil {
		if renamed {
			// 新快照可能已经是磁盘上的那份，而内存还是旧一代：再往日志追加会写成旧代行被忽略。
			b.broken = fmt.Errorf("%w: 写快照时改名之后出错: %v", ErrPFBaseBroken, err)
		}
		return err
	}
	b.gen = body.Gen
	b.updated = body.Updated
	b.entries = body.Entries
	b.ledger = ledger
	b.meta = meta
	if err := pfStep(b.env.hook, "compact:snapshot-done"); err != nil {
		return err
	}
	if err := b.truncateJournalLocked(); err != nil {
		return fmt.Errorf("新快照（第 %d 代）已写好，但截断日志失败（旧日志行会在加载时被忽略）: %w", b.gen, err)
	}
	b.journalLines = 0
	return pfStep(b.env.hook, "compact:journal-truncated")
}

// truncateJournalLocked 把日志截成空。日志不存在就什么也不做。
func (b *PFBase) truncateJournalLocked() error {
	jp := filepath.Join(b.baseDir, b.unit+".journal")
	f, err := os.OpenFile(jp, os.O_WRONLY|os.O_TRUNC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// ---------- 台账导出副本 ----------

func pfCheckLedgerHash(h string) error {
	if h == "" || len(h) > 256 {
		return fmt.Errorf("projsync: 台账哈希长度不合法")
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-') {
			return fmt.Errorf("projsync: 台账哈希 %q 含不允许的字符", h)
		}
	}
	return nil
}

// SaveLedgerExport 把台账 JSON 的导出副本原子地存到 <meta>/base/<键>.ledger/<对哈希>.json，
// 返回写进 PFBaseLedger.Export 的引用（相对 base 目录的路径）。以对哈希命名，内容不变，重复保存无害。
func (b *PFBase) SaveLedgerExport(hash string, data []byte) (string, error) {
	if err := pfCheckLedgerHash(hash); err != nil {
		return "", err
	}
	ref := b.unit + ".ledger/" + hash + ".json"
	if err := writeFileAtomic(filepath.Join(b.baseDir, filepath.FromSlash(ref)), data, 0o600); err != nil {
		return "", err
	}
	return ref, nil
}

// ReadLedgerExport 读回某个对哈希的导出副本。
func (b *PFBase) ReadLedgerExport(hash string) ([]byte, error) {
	if err := pfCheckLedgerHash(hash); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(b.baseDir, b.unit+".ledger", hash+".json"))
}
