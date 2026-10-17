// logfile.go：运行日志。写到 <配置目录>/wgtund.log（同时保留 stderr），每行带本地时间（到秒）。
// 按天轮转（跨日把 wgtund.log 改名为 wgtund-YYYY-MM-DD.log 再新开），保留 3 天，
// 单文件超 20MB 也轮转一次（同日多份加序号）。日志里不放令牌、密钥、密码和终端内容。
package wgtund

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	logName     = "wgtund.log"
	logKeepDays = 3
	logMaxSize  = 20 << 20
	dayLayout   = "2006-01-02"
)

// rotLog 是按天、按大小轮转的日志文件，实现 io.Writer。
type rotLog struct {
	dir     string
	now     func() time.Time // 可注入，测试用
	maxSize int64

	mu   sync.Mutex
	f    *os.File
	size int64
	day  string // 当前文件对应的日期
}

func newRotLog(dir string, now func() time.Time, maxSize int64) (*rotLog, error) {
	r := &rotLog{dir: dir, now: now, maxSize: maxSize}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.openLocked(); err != nil {
		return nil, err
	}
	r.cleanLocked()
	return r, nil
}

// openLocked 打开（追加）当前日志；若旧文件属于更早的日子，先按它自己的日期改名归档。
func (r *rotLog) openLocked() error {
	path := filepath.Join(r.dir, logName)
	today := r.now().Format(dayLayout)
	if st, err := os.Stat(path); err == nil {
		if d := st.ModTime().In(r.now().Location()).Format(dayLayout); d != today {
			r.archive(path, d)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size, r.day = f, st.Size(), today
	return nil
}

// archive 把 path 改名为 wgtund-<day>.log；已存在则加序号 -1、-2……
func (r *rotLog) archive(path, day string) {
	dst := filepath.Join(r.dir, "wgtund-"+day+".log")
	for i := 1; ; i++ {
		if _, err := os.Lstat(dst); err != nil {
			break
		}
		dst = filepath.Join(r.dir, fmt.Sprintf("wgtund-%s-%d.log", day, i))
	}
	os.Rename(path, dst)
}

// cleanLocked 删除超过保留天数的 wgtund-YYYY-MM-DD*.log。
func (r *rotLog) cleanLocked() {
	files, _ := filepath.Glob(filepath.Join(r.dir, "wgtund-*.log"))
	now := r.now()
	oldest := time.Date(now.Year(), now.Month(), now.Day()-(logKeepDays-1), 0, 0, 0, 0, now.Location())
	for _, p := range files {
		name := strings.TrimPrefix(filepath.Base(p), "wgtund-")
		if len(name) < len(dayLayout) {
			continue
		}
		d, err := time.ParseInLocation(dayLayout, name[:len(dayLayout)], now.Location())
		if err != nil {
			continue
		}
		if d.Before(oldest) {
			os.Remove(p)
		}
	}
}

func (r *rotLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	today := r.now().Format(dayLayout)
	if today != r.day || (r.size > 0 && r.size+int64(len(p)) > r.maxSize) {
		r.f.Close()
		r.archive(filepath.Join(r.dir, logName), r.day)
		if err := r.openLocked(); err != nil {
			r.f = nil
			return 0, err
		}
		r.cleanLocked()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// logSink 是包级日志出口：行首加时间，同时写 stderr 和（若已设置）日志文件。
type logSink struct {
	mu   sync.Mutex
	file io.Writer
	err  io.Writer
	now  func() time.Time
	thr  map[string]*throttleEnt
}

var sysLog = &logSink{now: time.Now, thr: map[string]*throttleEnt{}}

// setFile 设置日志文件，返回恢复函数（只在当前仍是 w 时才清掉）。
func (s *logSink) setFile(w io.Writer) func() {
	s.mu.Lock()
	s.file = w
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.file == w {
			s.file = nil
		}
		s.mu.Unlock()
	}
}

func (s *logSink) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeLocked(fmt.Sprintf(format, args...))
}

func (s *logSink) writeLocked(msg string) {
	line := s.now().Format("2006-01-02 15:04:05") + " " + strings.ReplaceAll(strings.TrimRight(msg, "\n"), "\n", " ") + "\n"
	if s.err != nil {
		io.WriteString(s.err, line)
	} else {
		io.WriteString(os.Stderr, line) // 每次现取 os.Stderr（测试会临时替换它）
	}
	if s.file != nil {
		io.WriteString(s.file, line)
	}
}

// throttleEnt 记一个节流键最近一次写出的原因与时间，以及其后被压下的次数。
type throttleEnt struct {
	reason string
	at     time.Time
	n      int
}

// throttleInterval 是同一原因连续出现时的最短记录间隔。
const throttleInterval = time.Minute

// logThrottled 记一条失败：同一 key 下原因相同、且距上次写出不足 1 分钟则只累计次数；
// 下次写出时带上被压下的次数。
func (s *logSink) logThrottled(key, reason, format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := s.thr[key]
	if e != nil && e.reason == reason && now.Sub(e.at) < throttleInterval {
		e.n++
		return
	}
	n := 0
	if e != nil && e.reason == reason {
		n = e.n
	}
	s.thr[key] = &throttleEnt{reason: reason, at: now}
	msg := fmt.Sprintf(format, args...)
	if n > 0 {
		msg += fmt.Sprintf("（上次记录以来同样原因另有 %d 次）", n)
	}
	s.writeLocked(msg)
}

// clearThrottle 清掉节流记录；之前有过失败记录时返回 true（调用方可记一条「恢复」）。
func (s *logSink) clearThrottle(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.thr[key]
	delete(s.thr, key)
	return ok
}

func logf(format string, args ...any) { sysLog.logf(format, args...) }

// Logf 给 cmd 用：写一行带时间的日志（同时到 stderr 和日志文件）。
func Logf(format string, args ...any) { sysLog.logf(format, args...) }

// setFlagLog 改一个布尔状态标记，值变了才记一行（preferred_down / preferred_back）。
func setFlagLog(p *bool, v bool, what, name string) {
	if *p != v {
		*p = v
		logf("%s %s %v", name, what, v)
	}
}
