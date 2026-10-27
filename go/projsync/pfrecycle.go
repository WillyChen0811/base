package projsync

// 项目文件同步的「同步回收」（技术设计 §3.5）：被覆盖、被删除、落选的冲突文件不直接消失，改名挪进回收区，
// 留一份 index.jsonl 记账，过了保留天数才由清理删掉。
//
// 布局：<回收区>/<YYYY-MM-DD>/<单元 rel>/<文件 rel>；每个日期目录里一份 index.jsonl。
//   - 重名（同一天同一路径回收了第二份）在扩展名前加「~HHMMSS-<4位hex>」，用不覆盖的改名，不会盖掉先来的；
//   - 回收区里以前放过一个叫 a 的文件、现在要放 a/b（或反过来）时，路径结构冲突，改放到当天的「_散放」目录，
//     原路径仍写在 index 里；
//   - index 每条写完 fsync；先改名、后记账——崩在中间只会多出一份没记账的文件，不会丢内容。
//
// 【边界】
//   - 回收区必须和同步根在同一个文件系统（比 st_dev）：放进回收是改名，不是复制。不同就是配置错误，
//     调用方整个单元（整个同步）停下，不退回成复制再删。
//   - 清理之外，本包任何原语都不删回收区里的东西；清理只删名字严格是 YYYY-MM-DD 的日期目录，
//     且至少留 PFRecycleMinDays 天（调用方给得更小也按 7 天算）。
//   - 本文件不认识「覆盖、删除」的前后核对（在 pfapply.go）；这里只负责放进去、记账、清理、统计。
//   - 同一个 PFRecycle 里的记账有互斥锁；多个进程同时往同一个回收区写不在设计范围内（单元锁、协调者租约保证）。

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// PFRecycleMinDays 是回收保留天数的下限（设置项最少 7）。
	PFRecycleMinDays = 7
	// PFRecycleDefaultDays 是缺省保留天数。
	PFRecycleDefaultDays = 30

	pfRecycleIndexName = "index.jsonl"
	pfRecycleStrayDir  = "_散放"
	pfRecycleDayLayout = "2006-01-02"
	pfRecycleMaxTries  = 50
)

var pfRecycleDayRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// PFRecycleReason 是进回收的原因，写进 index。
type PFRecycleReason string

const (
	PFReasonOverwrite PFRecycleReason = "覆盖" // 被同步来的新版本换下来
	PFReasonDelete    PFRecycleReason = "删除" // 另一边删了，这边跟着删
	PFReasonConflict  PFRecycleReason = "冲突" // 冲突里没被选中的那份
)

func (r PFRecycleReason) valid() bool {
	return r == PFReasonOverwrite || r == PFReasonDelete || r == PFReasonConflict
}

// PFRecycleEntry 是 index.jsonl 里的一条记录。
type PFRecycleEntry struct {
	Unit    string `json:"unit"`           // 单元 rel（相对同步根）
	Path    string `json:"path"`           // 文件在单元里的原路径
	SHA256  string `json:"sha256"`         // 回收时核对过的内容哈希
	Size    int64  `json:"size"`           // 字节数
	MtimeNs int64  `json:"mtime_ns"`       // 回收时文件的 mtime
	Reason  string `json:"reason"`         // 覆盖 / 删除 / 冲突
	Round   string `json:"round"`          // 轮次号
	Time    string `json:"time"`           // RFC3339，本地时间
	Stored  string `json:"stored"`         // 在回收区里的位置，相对回收区根目录，/ 分隔
	Note    string `json:"note,omitempty"` // 特殊情况说明（如核对不符、原位被占而留在回收）
}

// PFRecycleOptions 是可注入的部分（测试用）。
type PFRecycleOptions struct {
	Stat PFStatFunc       // 缺省 PFLstat；测试用它模拟「不同卷」
	Now  func() time.Time // 缺省 time.Now
}

// PFRecycle 是一个同步回收区。
type PFRecycle struct {
	dir  string
	stat PFStatFunc
	now  func() time.Time
	mu   sync.Mutex // 保护 index 追加
}

// NewPFRecycle 打开（必要时建出）回收区目录 dir。dir 必须是绝对路径。
// 与同步根是否同卷由 Check 判断（调用方启动时、每轮开始时调）。
func NewPFRecycle(dir string, opt PFRecycleOptions) (*PFRecycle, error) {
	if !filepath.IsAbs(dir) {
		return nil, &PFApplyConfigError{Msg: fmt.Sprintf("回收区路径 %q 不是绝对路径", dir)}
	}
	r := &PFRecycle{dir: filepath.Clean(dir), stat: opt.Stat, now: opt.Now}
	if r.stat == nil {
		r.stat = PFLstat
	}
	if r.now == nil {
		r.now = time.Now
	}
	if err := pfMkdirAllSync(r.dir, pfDirPerm); err != nil {
		return nil, fmt.Errorf("projsync: 建回收区目录 %s 失败: %w", r.dir, err)
	}
	st, err := r.stat(r.dir)
	if err != nil {
		return nil, fmt.Errorf("projsync: 读取回收区目录 %s 失败: %w", r.dir, err)
	}
	if st.Kind != PFKindDir {
		return nil, &PFApplyConfigError{Msg: fmt.Sprintf("回收区 %s 不是真实目录", r.dir)}
	}
	return r, nil
}

// Dir 返回回收区根目录。
func (r *PFRecycle) Dir() string { return r.dir }

// Path 返回一条记录对应的文件在磁盘上的完整路径。
func (r *PFRecycle) Path(e PFRecycleEntry) string {
	return filepath.Join(r.dir, filepath.FromSlash(e.Stored))
}

// Check 检查回收区和 root（同步根或单元目录）是否在同一个文件系统（st_dev 相同）。
// 不同卷是配置错误：返回 *PFApplyConfigError，调用方整个停下，不要退回成复制。
func (r *PFRecycle) Check(root string) error {
	rs, err := r.stat(root)
	if err != nil {
		return &PFApplyConfigError{Msg: fmt.Sprintf("读取同步根 %s 失败：%v", root, err)}
	}
	ds, err := r.stat(r.dir)
	if err != nil {
		return &PFApplyConfigError{Msg: fmt.Sprintf("读取回收区 %s 失败：%v", r.dir, err)}
	}
	if rs.Dev != ds.Dev {
		return &PFApplyConfigError{Msg: fmt.Sprintf("回收区 %s 与同步根 %s 不在同一个文件系统（st_dev 不同），放进回收是改名、不能跨盘，请把回收区挪到同一个盘上", r.dir, root)}
	}
	return nil
}

// ── 放进回收 ────────────────────────────────────────────

type pfPlaced struct {
	Stored string // 放进去后的完整路径
	Rel    string // 相对回收区根目录的路径（/ 分隔）
	Day    string // 日期目录名
	Warn   error  // 改名已成功，但目录 fsync 失败
}

// place 把 src 用不覆盖的改名放进回收区 <日期>/<unitRel>/<fileRel>，重名自动换名，不写 index。
// 失败时 src 原地不动。src 必须与回收区同一个文件系统，否则返回的错误里带 EXDEV。
func (r *PFRecycle) place(src string, t time.Time, unitRel, fileRel string) (pfPlaced, error) {
	day := t.Format(pfRecycleDayLayout)
	dirRel := path.Join(day, unitRel, path.Dir(fileRel))
	destDir := filepath.Join(r.dir, filepath.FromSlash(dirRel))
	if err := pfMkdirAllSync(destDir, pfDirPerm); err != nil {
		// 回收区里这个位置已被一个同名文件占着：放进当天的散放目录，原路径记在 index 里
		dirRel = path.Join(day, pfRecycleStrayDir)
		destDir = filepath.Join(r.dir, filepath.FromSlash(dirRel))
		if err2 := pfMkdirAllSync(destDir, pfDirPerm); err2 != nil {
			return pfPlaced{}, fmt.Errorf("建回收目录 %s 失败: %w", destDir, err2)
		}
	}
	base := path.Base(fileRel)
	var lastErr error
	for i := 0; i < pfRecycleMaxTries; i++ {
		name := base
		if i > 0 {
			name = pfRecycleSuffixed(base, t, pfRand4())
		}
		dst := filepath.Join(destDir, name)
		err := pfRenameNoReplace(src, dst)
		if err == nil {
			p := pfPlaced{Stored: dst, Rel: path.Join(dirRel, name), Day: day}
			for _, d := range []string{destDir, filepath.Dir(src)} {
				if e := pfSyncDir(d); e != nil {
					p.Warn = errors.Join(p.Warn, e)
				}
			}
			return p, nil
		}
		if !pfIsErrno(err, syscall.EEXIST) {
			return pfPlaced{}, err
		}
		lastErr = err
	}
	return pfPlaced{}, fmt.Errorf("回收区里 %s 的重名试了 %d 次都已存在: %w", base, pfRecycleMaxTries, lastErr)
}

// record 往当天的 index.jsonl 追加一条并 fsync。
func (r *PFRecycle) record(day string, e PFRecycleEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return appendLineSync(filepath.Join(r.dir, day, pfRecycleIndexName), b, pfDefaultPerm)
}

func pfRand4() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机数不可用时退回到时间纳秒，只影响重名后缀的随机性
		n := time.Now().UnixNano()
		b[0], b[1] = byte(n), byte(n>>8)
	}
	return hex.EncodeToString(b[:])
}

// pfRecycleSuffixed 在扩展名前加 ~HHMMSS-xxxx；整个名字超 255 字节时按字符截短主名。
func pfRecycleSuffixed(base string, t time.Time, hex4 string) string {
	ext := pfExt(base)
	main := base[:len(base)-len(ext)]
	suffix := "~" + t.Format("150405") + "-" + hex4
	budget := pfMaxSegBytes - len(suffix) - len(ext)
	if budget < 1 { // 扩展名长得离谱：连同扩展名一起当主名截
		main, ext = base, ""
		budget = pfMaxSegBytes - len(suffix)
	}
	if len(main) > budget {
		cut := 0
		for cut < len(main) {
			_, n := utf8.DecodeRuneInString(main[cut:])
			if cut+n > budget {
				break
			}
			cut += n
		}
		main = main[:cut]
	}
	return main + suffix + ext
}

// ── 读 index ────────────────────────────────────────────

// Days 返回回收区里所有日期目录名（升序）。
func (r *PFRecycle) Days() ([]string, error) {
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && pfRecycleDayRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil // ReadDir 已按名字排序
}

// ReadIndex 读某一天的 index.jsonl；没有这一天返回空。写了一半的最后一行和读不懂的行跳过。
func (r *PFRecycle) ReadIndex(day string) ([]PFRecycleEntry, error) {
	if !pfRecycleDayRe.MatchString(day) {
		return nil, fmt.Errorf("projsync: 日期 %q 格式不对", day)
	}
	f, err := os.Open(filepath.Join(r.dir, day, pfRecycleIndexName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []PFRecycleEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e PFRecycleEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Stored == "" {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// ── 清理与统计 ──────────────────────────────────────────

// PFRecycleCleanResult 是一次清理的结果。
type PFRecycleCleanResult struct {
	Days    int      // 实际用的保留天数（不足 7 天按 7 天算）
	Removed []string // 删掉的日期目录
	Files   int      // 随之删掉的文件数（不含 index）
	Bytes   int64    // 释放的字节数
	Kept    int      // 保留的日期目录数
}

// Clean 删掉超过 retainDays 天的日期目录：日期早于「今天 0 点往前推 retainDays 天」的整个目录删掉，
// 所以每份东西至少留满 retainDays 天。retainDays 小于 PFRecycleMinDays 时按 PFRecycleMinDays 算。
// 只认名字严格是 YYYY-MM-DD 的真实目录（符号链接、别的名字的文件一概不碰）；
// 单个目录删不掉不会停，错误合并后一起返回。
func (r *PFRecycle) Clean(retainDays int) (PFRecycleCleanResult, error) {
	res := PFRecycleCleanResult{Days: retainDays}
	if res.Days < PFRecycleMinDays {
		res.Days = PFRecycleMinDays
	}
	now := r.now()
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	cutoff := today.AddDate(0, 0, -res.Days)
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, err
	}
	var errs []error
	for _, e := range ents {
		name := e.Name()
		if !pfRecycleDayRe.MatchString(name) || e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		d, err := time.ParseInLocation(pfRecycleDayLayout, name, loc)
		if err != nil || !d.Before(cutoff) {
			res.Kept++
			continue
		}
		full := filepath.Join(r.dir, name)
		files, bytes := pfTreeSize(full)
		if err := os.RemoveAll(full); err != nil {
			errs = append(errs, fmt.Errorf("清理回收区：删除 %s 失败: %w", name, err))
			res.Kept++
			continue
		}
		res.Removed = append(res.Removed, name)
		res.Files += files
		res.Bytes += bytes
	}
	return res, errors.Join(errs...)
}

// PFRecycleDayUsage 是一个日期目录的占用。
type PFRecycleDayUsage struct {
	Day   string
	Files int   // 不含 index
	Bytes int64 // 含 index
}

// PFRecycleUsage 是回收区的占用统计。
type PFRecycleUsage struct {
	Files int
	Bytes int64
	Days  []PFRecycleDayUsage
}

// Usage 统计回收区占用（只数真实文件，不跟随符号链接）。
func (r *PFRecycle) Usage() (PFRecycleUsage, error) {
	var u PFRecycleUsage
	days, err := r.Days()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return u, nil
		}
		return u, err
	}
	for _, d := range days {
		files, bytes := pfTreeSize(filepath.Join(r.dir, d))
		u.Days = append(u.Days, PFRecycleDayUsage{Day: d, Files: files, Bytes: bytes})
		u.Files += files
		u.Bytes += bytes
	}
	sort.Slice(u.Days, func(i, j int) bool { return u.Days[i].Day < u.Days[j].Day })
	return u, nil
}

// pfTreeSize 统计目录树里普通文件的个数（不含名为 index.jsonl 的）和总字节数（含 index）。读不了的跳过。
func pfTreeSize(root string) (files int, bytes int64) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			bytes += fi.Size()
		}
		if !(filepath.Dir(p) == root && d.Name() == pfRecycleIndexName) {
			files++
		}
		return nil
	})
	return files, bytes
}

// pfIsErrno 报告 err 的链上有没有这些 errno 之一（Go 的 ErrExist 把 ENOTEMPTY 也算进去了，这里要分开判断）。
func pfIsErrno(err error, errnos ...syscall.Errno) bool {
	for _, e := range errnos {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
