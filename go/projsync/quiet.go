package projsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
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

// 未提交改动的静止时长记录（PLAN-sync-v2 第六节第 3 条：至少 20 分钟没有变动才允许自动提交）。
//
// 总原则「识别边界、超出就停」：任何读取失败都返回 error，由调用方停手，不猜。

const (
	cleanFingerprint = "clean"
	// DefaultDetectInterval 是同步服务两次检测之间的预期间隔。
	DefaultDetectInterval = 10 * time.Minute
	// smallFileHashLimit 以内的文件额外哈希内容，见 WorktreeFingerprint 的取舍说明。
	smallFileHashLimit = 1 << 20
)

// WorktreeFingerprint 返回工作区指纹与是否 dirty。干净返回 ("clean", false, nil)。
//
// 指纹 = sha256( status 原始输出 + 按路径排序后每个列出路径的 lstat(size, mtime 纳秒, mode, 符号链接目标) )。
// 目录（如子模块）跳过。已修改文件继续被编辑时 status 输出不变，但 mtime/size 会变，所以指纹也变。
//
// 取舍（重要，方向与直觉相反，如实写明）：「漏检」= 实际变了而指纹没变 = 静止被判得【更长】，
// 这是不安全的方向（可能过早提交）。stat 指纹的漏检只发生在「同一纳秒/同一内核时间片内同尺寸改写」
// 或「工具把 mtime 还原」（touch -r、rsync -t）两种情形。前者误差上界是文件系统时间戳粒度
// （毫秒级，对 20 分钟门槛可忽略；之后任何新写入都会改变 mtime 而被检出）；后者 stat 无法防住。
// 因此对 ≤1MiB 的文件额外哈希内容（开销很小），同时覆盖这两种情形；
// >1MiB 的大文件不读内容，仅靠 stat，残余风险仅为「mtime 被人为还原的大文件」。
// 读取失败一律 error；文件在 status 与 lstat/读取之间被删除记为 "gone"（视为变动，指纹因而变化）。
func WorktreeFingerprint(ctx context.Context, wtDir string, env []string) (fp string, dirty bool, err error) {
	e := append(append([]string{}, env...), "GIT_OPTIONAL_LOCKS=0")
	out, err := runGit(ctx, wtDir, e, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return "", false, fmt.Errorf("读取工作区 %s 状态失败: %w", wtDir, err)
	}
	if len(out) == 0 {
		return cleanFingerprint, false, nil
	}
	paths, err := parseStatusV2Paths(out)
	if err != nil {
		return "", false, fmt.Errorf("解析工作区 %s 状态失败: %w", wtDir, err)
	}
	sort.Strings(paths)
	h := sha256.New()
	writeFP(h, out)
	last := "\x00"
	for _, p := range paths {
		if p == last {
			continue
		}
		last = p
		writeFP(h, []byte(p))
		desc, err := statDesc(filepath.Join(wtDir, filepath.FromSlash(p)))
		if err != nil {
			return "", false, fmt.Errorf("读取 %s 失败: %w", filepath.Join(wtDir, p), err)
		}
		writeFP(h, []byte(desc))
	}
	return fmt.Sprintf("%x", h.Sum(nil)), true, nil
}

// writeFP 写入长度前缀 + 内容，避免拼接歧义。
func writeFP(h io.Writer, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	h.Write(n[:])
	h.Write(b)
}

// statDesc 描述一个路径的当前状态；不存在返回 "gone"；目录返回 "dir"（跳过）。
func statDesc(p string) (string, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "gone", nil
		}
		return "", err
	}
	switch {
	case fi.IsDir():
		return "dir", nil
	case fi.Mode()&fs.ModeSymlink != 0:
		t, err := os.Readlink(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "gone", nil
			}
			return "", err
		}
		return fmt.Sprintf("link %d %d %o -> %s", fi.Size(), fi.ModTime().UnixNano(), fi.Mode(), t), nil
	}
	desc := fmt.Sprintf("file %d %d %o", fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
	if fi.Mode().IsRegular() && fi.Size() <= smallFileHashLimit {
		f, err := os.Open(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "gone", nil
			}
			return "", err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		desc += fmt.Sprintf(" %x", h.Sum(nil))
	}
	return desc, nil
}

// parseStatusV2Paths 解析 `status --porcelain=v2 -z` 输出，返回所有记录里的路径（含 rename 的新旧两个路径）。
// 路径保留原始字节，不 TrimSpace。无法识别的记录类型是 error（超出范围就停）。
func parseStatusV2Paths(out []byte) ([]string, error) {
	recs := bytes.Split(out, []byte{0})
	if len(recs) > 0 && len(recs[len(recs)-1]) == 0 {
		recs = recs[:len(recs)-1]
	}
	var paths []string
	for i := 0; i < len(recs); i++ {
		r := string(recs[i])
		if len(r) < 3 || r[1] != ' ' {
			return nil, fmt.Errorf("无法识别的记录 %q", r)
		}
		switch r[0] {
		case '1': // 1 XY sub mH mI mW hH hI path
			f := strings.SplitN(r, " ", 9)
			if len(f) != 9 {
				return nil, fmt.Errorf("普通记录字段不足 %q", r)
			}
			paths = append(paths, f[8])
		case '2': // 2 XY sub mH mI mW hH hI Xscore path \0 origPath
			f := strings.SplitN(r, " ", 10)
			if len(f) != 10 || i+1 >= len(recs) {
				return nil, fmt.Errorf("重命名记录不完整 %q", r)
			}
			i++
			paths = append(paths, f[9], string(recs[i]))
		case 'u': // u XY sub m1 m2 m3 mW h1 h2 h3 path
			f := strings.SplitN(r, " ", 11)
			if len(f) != 11 {
				return nil, fmt.Errorf("冲突记录字段不足 %q", r)
			}
			paths = append(paths, f[10])
		case '?', '!':
			paths = append(paths, r[2:])
		default:
			return nil, fmt.Errorf("无法识别的记录类型 %q", r)
		}
	}
	return paths, nil
}

// QuietEntry 是某个工作区的静止记录。
type QuietEntry struct {
	Fingerprint string    `json:"fingerprint"`
	Since       time.Time `json:"since"`    // 该指纹首次出现的时间
	LastSeen    time.Time `json:"lastSeen"` // 最近一次观察
}

// QuietTracker 把各工作区的指纹持久化到一个 JSON 文件。非并发安全。
type QuietTracker struct {
	path string
	// DetectInterval 是预期检测间隔；两次观察相隔超过 2 倍即视为中间有未观察到的变化。零值按默认 10 分钟。
	DetectInterval time.Duration
	Entries        map[string]QuietEntry
}

// LoadQuietTracker 读取记录文件。不存在→空记录；损坏→error（不静默清空，由调用方按「超出范围就停」处理）。
func LoadQuietTracker(path string) (*QuietTracker, error) {
	t := &QuietTracker{path: path, DetectInterval: DefaultDetectInterval, Entries: map[string]QuietEntry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取静止记录 %s 失败: %w", path, err)
	}
	var m map[string]QuietEntry
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, fmt.Errorf("静止记录 %s 已损坏: %v", path, err)
	}
	t.Entries = m
	return t, nil
}

// canonWorktree 规范化工作区路径：EvalSymlinks + Clean；解析失败（路径不存在等）退回 Clean。
func canonWorktree(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	return filepath.Clean(p)
}

// Observe 记录一次观察并返回已静止的整分钟数。
//   - 首次、指纹变化、fp=="clean"：since=now，返回 0。
//   - 时钟回拨（now 早于 since 或 lastSeen）：视为指纹刚变，返回 0。宁可判得更短，不能更长。
//   - 距上次观察超过 2×DetectInterval：同步服务停过一段时间，空白期内可能有未观察到的变化，
//     不能把空白当成「静止」，since 重置为 now，返回 0。
//   - 否则指纹相同：返回 now-since 的整分钟数。
func (t *QuietTracker) Observe(wt string, fp string, now time.Time) int {
	if t.Entries == nil {
		t.Entries = map[string]QuietEntry{}
	}
	iv := t.DetectInterval
	if iv <= 0 {
		iv = DefaultDetectInterval
	}
	key := canonWorktree(wt)
	e, ok := t.Entries[key]
	reset := !ok || fp == cleanFingerprint || e.Fingerprint != fp ||
		now.Before(e.Since) || now.Before(e.LastSeen) || now.Sub(e.LastSeen) > 2*iv
	if reset {
		t.Entries[key] = QuietEntry{Fingerprint: fp, Since: now, LastSeen: now}
		return 0
	}
	e.LastSeen = now
	t.Entries[key] = e
	return int(now.Sub(e.Since) / time.Minute)
}

// Save 原子写入：先写同目录临时文件再 rename，权限 0600。
func (t *QuietTracker) Save() error {
	b, err := json.MarshalIndent(t.Entries, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(t.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return err
	}
	ok = true
	return nil
}

// QuietFunc 返回给 CollectLocalWorktrees 用的回调：fps 里有该路径就 Observe 并返回 (分钟, true)，否则 (0, false)。
func (t *QuietTracker) QuietFunc(now time.Time, fps map[string]string) func(path string) (int, bool) {
	return func(path string) (int, bool) {
		fp, ok := fps[path]
		if !ok {
			return 0, false
		}
		return t.Observe(path, fp, now), true
	}
}
