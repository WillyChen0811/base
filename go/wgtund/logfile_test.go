package wgtund

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func logNames(t *testing.T, dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "wgtund*.log"))
	for i := range m {
		m[i] = filepath.Base(m[i])
	}
	sort.Strings(m)
	return m
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func day(d int) time.Time { return time.Date(2026, 3, d, 10, 0, 0, 0, time.Local) }

func TestRotLogDailyRotation(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{day(10)}
	r, err := newRotLog(dir, clk.now, logMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Write([]byte("a\n"))
	clk.t = day(11)
	r.Write([]byte("b\n"))
	if got := logNames(t, dir); strings.Join(got, ",") != "wgtund-2026-03-10.log,wgtund.log" {
		t.Fatalf("文件: %v", got)
	}
	old, _ := os.ReadFile(filepath.Join(dir, "wgtund-2026-03-10.log"))
	cur, _ := os.ReadFile(filepath.Join(dir, "wgtund.log"))
	if string(old) != "a\n" || string(cur) != "b\n" {
		t.Fatalf("内容 old=%q cur=%q", old, cur)
	}
}

func TestRotLogStartupArchivesStaleFileAndKeepsThreeDays(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"wgtund-2026-03-07.log", "wgtund-2026-03-08.log", "wgtund-2026-03-09.log", "wgtund-2026-03-10.log", "wgtund-notes.log"} {
		write(n)
	}
	write("wgtund.log")
	os.Chtimes(filepath.Join(dir, "wgtund.log"), day(11), day(11)) // 昨天写的残留
	clk := &fakeClock{day(12)}
	r, err := newRotLog(dir, clk.now, logMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// 保留 12、11、10 三天；07/08/09 删除；无法解析日期的不动
	want := "wgtund-2026-03-10.log,wgtund-2026-03-11.log,wgtund-notes.log,wgtund.log"
	if got := strings.Join(logNames(t, dir), ","); got != want {
		t.Fatalf("文件: %s\n想要: %s", got, want)
	}
}

func TestRotLogCleanOnRotate(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{day(10)}
	r, _ := newRotLog(dir, clk.now, logMaxSize)
	defer r.Close()
	for d := 10; d <= 14; d++ {
		clk.t = day(d)
		r.Write([]byte("l\n"))
	}
	want := "wgtund-2026-03-12.log,wgtund-2026-03-13.log,wgtund.log"
	if got := strings.Join(logNames(t, dir), ","); got != want {
		t.Fatalf("文件: %s\n想要: %s", got, want)
	}
}

func TestRotLogSizeRotation(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{day(10)}
	r, _ := newRotLog(dir, clk.now, 10)
	defer r.Close()
	for i := 0; i < 3; i++ {
		r.Write([]byte("123456\n")) // 7 字节：每次写入都会超 10
	}
	want := "wgtund-2026-03-10-1.log,wgtund-2026-03-10.log,wgtund.log"
	if got := strings.Join(logNames(t, dir), ","); got != want {
		t.Fatalf("文件: %s\n想要: %s", got, want)
	}
	cur, _ := os.ReadFile(filepath.Join(dir, "wgtund.log"))
	if string(cur) != "123456\n" {
		t.Fatalf("当前文件应只有最后一行: %q", cur)
	}
}

func TestLogSinkPrefixAndFile(t *testing.T) {
	var file, errw strings.Builder
	clk := &fakeClock{time.Date(2026, 3, 10, 9, 8, 7, 0, time.Local)}
	s := &logSink{err: &errw, file: &file, now: clk.now, thr: map[string]*throttleEnt{}}
	s.logf("你好 %d\n换行", 1)
	want := "2026-03-10 09:08:07 你好 1 换行\n"
	if file.String() != want || errw.String() != want {
		t.Fatalf("file=%q err=%q", file.String(), errw.String())
	}
}

func TestLogThrottled(t *testing.T) {
	var file strings.Builder
	clk := &fakeClock{day(10)}
	s := &logSink{err: io.Discard, file: &file, now: clk.now, thr: map[string]*throttleEnt{}}
	fail := func(reason string) { s.logThrottled("k", reason, "拨号失败：%s", reason) }
	fail("超时") // 记
	for i := 0; i < 5; i++ {
		clk.t = clk.t.Add(5 * time.Second)
		fail("超时") // 压下
	}
	if n := strings.Count(file.String(), "\n"); n != 1 {
		t.Fatalf("1 分钟内应只记 1 行，实际 %d: %q", n, file.String())
	}
	clk.t = clk.t.Add(time.Minute)
	fail("超时") // 满 1 分钟：记并带累计次数
	lines := strings.Split(strings.TrimSpace(file.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "另有 5 次") {
		t.Fatalf("应带累计次数: %q", lines)
	}
	fail("拒绝") // 原因变了：立即记，不带次数
	lines = strings.Split(strings.TrimSpace(file.String()), "\n")
	if len(lines) != 3 || strings.Contains(lines[2], "另有") {
		t.Fatalf("换原因应立即记: %q", lines)
	}
	if !s.clearThrottle("k") || s.clearThrottle("k") {
		t.Fatal("clearThrottle 应首次 true、再次 false")
	}
}
