package projsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// 项目文件同步：带时间预算的哈希，和哈希缓存（技术设计 §2.4、§4.3、§5.2、§5.7）。
//
// 本文件管：给 PFScan 扫出的普通文件算 sha256；哈希缓存的内存结构与 JSON 序列化/反序列化。
// 不管：把缓存写到磁盘（原子写由编排层调用别处的 writeFileAtomic）、判定、传输。
//
// 边界：
//   - 哈希读文件内容，iCloud 占位文件绝不读（读会触发下载）：占位文件只能命中缓存拿哈希，否则就是「不明」。
//   - 超时没算完、读的过程中被改、扫描后又变了，都标「不明」，不报错、不写缓存。
//   - 缓存损坏就报 error，不静默清空（和 quiet.go 的 LoadQuietTracker 同一原则）；调用方决定是重算还是停下。

// PFHashStats 是一次 PFHashItems 的统计。
type PFHashStats struct {
	CacheHits   int           // 命中缓存、没读文件的
	Hashed      int           // 实际读了内容并算出哈希的
	HashedBytes int64         // 实际读了多少字节
	NewUnknown  int           // 这一步里转成不明或超出范围的（超时、读的过程中被改、读不了……）
	Elapsed     time.Duration // 这一步总耗时
}

// ---- 哈希缓存 ----

// PFHashCacheVersion 是缓存文件的格式版本。
const PFHashCacheVersion = 1

// PFHashEntry 是缓存里一个文件的记录。命中条件只看 size、mtime_ns、ino，
// 不看 ctime 和 flags：iCloud 驱逐文件只会改 ctime 和 flags，驱逐后的占位文件要能命中。
type PFHashEntry struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Ino     uint64 `json:"ino"`
	SHA256  string `json:"sha256"`
}

// PFHashCache 是内存里的哈希缓存：键是相对路径（/ 分隔）。并发安全。
// 只管内存和 JSON；写文件由编排层调用别处的原子写。
type PFHashCache struct {
	mu sync.Mutex
	m  map[string]PFHashEntry
}

// NewPFHashCache 返回空缓存。
func NewPFHashCache() *PFHashCache { return &PFHashCache{m: map[string]PFHashEntry{}} }

// Len 返回条目数。
func (c *PFHashCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// Lookup 在 (size, mtime_ns, ino) 都相同时返回缓存的哈希。c 为 nil 恒不命中。
func (c *PFHashCache) Lookup(path string, st PFStat) (string, bool) {
	if c == nil || st.Kind != PFKindFile {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[path]
	if !ok || e.Size != st.Size || e.MtimeNs != st.MtimeNs || e.Ino != st.Ino {
		return "", false
	}
	return e.SHA256, true
}

// Put 记一条缓存，返回是否真的记了。不记的情况：
// mtime 距 now 不足 quietSeconds（可能还在写，哈希不可信）、不是普通文件、哈希格式不对、
// 路径不是合法的相对路径（含非 UTF-8——JSON 存不住，也进不了 base）。
func (c *PFHashCache) Put(path string, st PFStat, sha string, now time.Time, quietSeconds int) bool {
	if c == nil || st.Kind != PFKindFile || !pfValidSHA(sha) || pfCachePathProblem(path) != "" {
		return false
	}
	if !PFIsQuiet(st.MtimeNs, now, quietSeconds) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[path] = PFHashEntry{Size: st.Size, MtimeNs: st.MtimeNs, Ino: st.Ino, SHA256: sha}
	return true
}

// Delete 删一条。
func (c *PFHashCache) Delete(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, path)
}

// Prune 删掉 keep 返回 false 的条目，返回删了几条。编排层用它清掉已不存在的文件。
// 注意：只有确认文件真的不在了才清；iCloud 占位、不明、没扫到的子树都要保留，驱逐后的占位文件靠它免读拿哈希。
func (c *PFHashCache) Prune(keep func(path string) bool) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for p := range c.m {
		if !keep(p) {
			delete(c.m, p)
			n++
		}
	}
	return n
}

type pfHashCacheFile struct {
	Version int                    `json:"version"`
	Entries map[string]PFHashEntry `json:"entries"`
}

// Marshal 序列化成 JSON（键按路径排序，输出确定）。
func (c *PFHashCache) Marshal() ([]byte, error) {
	c.mu.Lock()
	f := pfHashCacheFile{Version: PFHashCacheVersion, Entries: make(map[string]PFHashEntry, len(c.m))}
	for k, v := range c.m {
		f.Entries[k] = v
	}
	c.mu.Unlock()
	for k := range f.Entries {
		if p := pfCachePathProblem(k); p != "" {
			return nil, fmt.Errorf("哈希缓存里的路径 %q 不合法: %s", k, p)
		}
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// 反序列化时用指针，缺字段能认出来（截断、手改的文件）。
type pfHashEntryRaw struct {
	Size    *int64  `json:"size"`
	MtimeNs *int64  `json:"mtime_ns"`
	Ino     *uint64 `json:"ino"`
	SHA256  *string `json:"sha256"`
}

type pfHashCacheFileRaw struct {
	Version *int                       `json:"version"`
	Entries map[string]*pfHashEntryRaw `json:"entries"`
}

// PFParseHashCache 反序列化。任何不对劲（不是 JSON、有多余内容、版本不认识、缺字段、有未知字段、
// 哈希格式不对、路径不合法）都返回 error，不静默当成空缓存；调用方决定重算还是停下报告。
// 文件不存在不是这里的事（调用方按「没有缓存」处理，用 NewPFHashCache）。
func PFParseHashCache(b []byte) (*PFHashCache, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var raw pfHashCacheFileRaw
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("哈希缓存已损坏: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("哈希缓存已损坏: JSON 之后还有多余内容")
	}
	if raw.Version == nil || *raw.Version != PFHashCacheVersion {
		return nil, fmt.Errorf("哈希缓存版本不认识: %v（要求 %d）", pfIntPtrText(raw.Version), PFHashCacheVersion)
	}
	if raw.Entries == nil {
		return nil, errors.New("哈希缓存已损坏: 缺少 entries")
	}
	c := NewPFHashCache()
	for p, e := range raw.Entries {
		if prob := pfCachePathProblem(p); prob != "" {
			return nil, fmt.Errorf("哈希缓存已损坏: 路径 %q 不合法: %s", p, prob)
		}
		if e == nil || e.Size == nil || e.MtimeNs == nil || e.Ino == nil || e.SHA256 == nil {
			return nil, fmt.Errorf("哈希缓存已损坏: %q 的记录缺字段", p)
		}
		if *e.Size < 0 || !pfValidSHA(*e.SHA256) {
			return nil, fmt.Errorf("哈希缓存已损坏: %q 的记录取值不对", p)
		}
		c.m[p] = PFHashEntry{Size: *e.Size, MtimeNs: *e.MtimeNs, Ino: *e.Ino, SHA256: *e.SHA256}
	}
	return c, nil
}

func pfIntPtrText(p *int) string {
	if p == nil {
		return "缺失"
	}
	return fmt.Sprint(*p)
}

func pfValidSHA(s string) bool {
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

// pfCachePathProblem 只做缓存键的最低限度检查（名字规则不归这里管）：返回空串表示可用。
func pfCachePathProblem(p string) string {
	switch {
	case p == "":
		return "为空"
	case !utf8.ValidString(p):
		return "不是合法 UTF-8"
	case p[0] == '/':
		return "不能以 / 开头"
	}
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if seg := p[start:i]; seg == "" || seg == "." || seg == ".." {
				return "含空段、. 或 .."
			}
			start = i + 1
		}
	}
	return ""
}

// ---- 带预算的哈希 ----

// PFHashItems 给 res 里还没有哈希的普通文件算 sha256，就地更新条目：
//
//  1. 先查缓存：(size, mtime_ns, ino) 相同就直接用缓存的哈希，不读内容、不占预算。
//     iCloud 占位文件（扫描阶段标成不明/icloud_placeholder）也只走这一步：命中就转成「有哈希」，没命中仍是不明，
//     绝不读内容。
//  2. 没命中的，按文件大小从小到大（同大小按路径）在 opt.HashBudget 预算内逐个读：
//     小文件先算完、结果进缓存，下一轮预算才轮得到大文件。单个文件大到一个预算读不完就永远算不出来，
//     调用方要给它更大的预算（例如首次全量）。
//     预算到了：剩下的标不明（hash_timeout），不报错。
//  3. 每个文件读之前重新 stat：和扫描时的 (size, mtime_ns, ino) 不同 → 不明（changed_since_scan）；
//     这时若变成了占位文件 → 不读。读完再 stat：(size, mtime_ns, ctime_ns, ino) 有变化，或读到的字节数
//     不等于 size → 不明（changed_while_reading），哈希不入缓存。
//  4. 读不了（权限等）→ 超出范围（unreadable）；读之前文件已不在 → 不明（vanished）。
//
// 新算出的哈希按 Put 的规则写进 cache（mtime 太新的不进）。cache 可为 nil。
// 返回的 error 只会是 ctx 被调用方取消，此时没算完的标 canceled，结果仍一致。
func PFHashItems(ctx context.Context, root string, res *PFScanResult, cache *PFHashCache, opt PFScanOptions) error {
	start := time.Now()
	stat := opt.stat()
	var stats PFHashStats
	defer func() {
		stats.Elapsed = time.Since(start)
		res.HashStats = stats
	}()

	var pending []int
	for i := range res.Items {
		it := &res.Items[i]
		if it.Kind != PFKindFile || !it.HasStat || it.Stub || it.Hash != "" {
			continue
		}
		switch {
		case it.State == PFStateOK:
			if h, ok := cache.Lookup(it.Path, it.Stat); ok {
				it.Hash, it.FromCache = h, true
				stats.CacheHits++
			} else if it.Stat.Dataless() { // 扫描阶段本该已标成占位；防御：不读
				pfSetUnknown(it, PFCodeICloudPlaceholder, "iCloud 占位文件，内容只在云端，不读取，缓存里也没有哈希")
				stats.NewUnknown++
			} else {
				pending = append(pending, i)
			}
		case it.State == PFStateUnknown && it.Code == PFCodeICloudPlaceholder && it.Stat.Dataless():
			if h, ok := cache.Lookup(it.Path, it.Stat); ok {
				it.State, it.Code, it.Reason = PFStateOK, "", ""
				it.Hash, it.FromCache = h, true
				stats.CacheHits++
			}
		}
	}
	sort.SliceStable(pending, func(a, b int) bool {
		x, y := &res.Items[pending[a]], &res.Items[pending[b]]
		if x.Stat.Size != y.Stat.Size {
			return x.Stat.Size < y.Stat.Size
		}
		return x.Path < y.Path
	})

	bctx := ctx
	if opt.HashBudget > 0 {
		var cancel context.CancelFunc
		bctx, cancel = context.WithTimeout(ctx, opt.HashBudget)
		defer cancel()
	}
	buf := make([]byte, 1<<20)
	for k, i := range pending {
		it := &res.Items[i]
		if bctx.Err() != nil {
			for _, j := range pending[k:] {
				pfSetStopped(ctx, &res.Items[j])
				stats.NewUnknown++
			}
			break
		}
		n, ok := pfHashOne(ctx, bctx, root, it, stat, &opt, cache, buf)
		if ok {
			stats.Hashed++
			stats.HashedBytes += n
		} else {
			stats.NewUnknown++
		}
	}
	return ctx.Err()
}

func pfSetUnknown(it *PFScanItem, code, reason string) {
	it.State, it.Code, it.Reason, it.Hash, it.FromCache = PFStateUnknown, code, reason, "", false
}

func pfSetOutOfScope(it *PFScanItem, code, reason string) {
	it.State, it.Code, it.Reason, it.Hash, it.FromCache = PFStateOutOfScope, code, reason, "", false
}

// pfSetStopped 把没算的条目标成「预算用完」或「被取消」。
func pfSetStopped(ctx context.Context, it *PFScanItem) {
	if ctx.Err() != nil {
		pfSetUnknown(it, PFCodeCanceled, "哈希被取消，本轮不明")
		return
	}
	pfSetUnknown(it, PFCodeHashTimeout, "哈希超出时间预算没算完，本轮不明")
}

// pfHashOne 算一个文件的哈希，成功返回 (读了多少字节, true)；其余情况已就地标好状态。
func pfHashOne(ctx, bctx context.Context, root string, it *PFScanItem, stat PFStatFunc, opt *PFScanOptions, cache *PFHashCache, buf []byte) (int64, bool) {
	full := filepath.Join(root, filepath.FromSlash(it.Path))
	st0, err := stat(full)
	if err != nil {
		pfSetStatFailure(it, err)
		return 0, false
	}
	if st0.Kind != PFKindFile || st0.Size != it.Stat.Size || st0.MtimeNs != it.Stat.MtimeNs || st0.Ino != it.Stat.Ino {
		pfSetUnknown(it, PFCodeChangedSinceScan, "扫描之后文件变了，本轮不明")
		return 0, false
	}
	if st0.Dataless() { // 扫描后才被驱逐：绝不读
		pfSetUnknown(it, PFCodeICloudPlaceholder, "iCloud 占位文件，内容只在云端，不读取（读会触发下载）")
		return 0, false
	}
	f, err := opt.open(full)
	if err != nil {
		pfSetOpenFailure(it, err)
		return 0, false
	}
	defer f.Close()
	h := sha256.New()
	var n int64
	for {
		if err := bctx.Err(); err != nil {
			pfSetStopped(ctx, it)
			return 0, false
		}
		m, rerr := f.Read(buf)
		if m > 0 {
			h.Write(buf[:m])
			n += int64(m)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			pfSetOutOfScope(it, PFCodeUnreadable, "读不了文件: "+pfErrText(rerr))
			return 0, false
		}
	}
	st1, err := stat(full)
	if err != nil {
		pfSetStatFailure(it, err)
		return 0, false
	}
	if st1.Kind != PFKindFile || st1.Size != st0.Size || st1.MtimeNs != st0.MtimeNs || st1.CtimeNs != st0.CtimeNs || st1.Ino != st0.Ino || n != st0.Size {
		pfSetUnknown(it, PFCodeChangedWhileRead, "读的过程中文件被改，本轮不明")
		return 0, false
	}
	it.Hash = hex.EncodeToString(h.Sum(nil))
	it.FromCache = false
	cache.Put(it.Path, st0, it.Hash, opt.now(), opt.quiet())
	return n, true
}

func pfSetStatFailure(it *PFScanItem, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		pfSetUnknown(it, PFCodeVanished, "哈希时文件已不在（刚被改名或删除），本轮不明")
		return
	}
	pfSetOutOfScope(it, PFCodeUnreadable, "读不了属性: "+pfErrText(err))
}

func pfSetOpenFailure(it *PFScanItem, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		pfSetUnknown(it, PFCodeVanished, "哈希时文件已不在（刚被改名或删除），本轮不明")
		return
	}
	pfSetOutOfScope(it, PFCodeUnreadable, "读不了文件: "+pfErrText(err))
}
