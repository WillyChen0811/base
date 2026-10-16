// 命令 wgtund 是 WireGuard 隧道 + SSH 终端桥的助手进程。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/WillyChen0811/base/go/wgtund"
)

func main() {
	// 子命令：wgtund support-hook [地址]，供 Claude Code 的权限钩子调用
	if len(os.Args) > 1 && os.Args[1] == "support-hook" {
		os.Exit(wgtund.RunHook(os.Args[2:], os.Stdin, os.Stdout, os.Getenv))
	}
	dir := flag.String("dir", "", "配置目录（必填，由调用方指定）")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "wgtund: 必须用 -dir 指定配置目录")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eofCh := make(chan struct{})
	exited := make(chan struct{})
	go watchShutdown(ctx.Done(), eofCh, exited, shutdownGrace, func(code int) {
		fmt.Fprintln(os.Stderr, "wgtund: 关闭超过 3 秒，强制退出")
		os.Exit(code)
	})
	in := newEOFReader(os.Stdin, func() { close(eofCh) })
	err := wgtund.Run(ctx, *dir, in, os.Stdout)
	close(exited)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wgtund:", err)
		os.Exit(1)
	}
}

// shutdownGrace 是进入关闭流程后允许的最长时间，超过就强制退出。
const shutdownGrace = 3 * time.Second

// watchShutdown 等待关闭事件（ctx 取消或 stdin EOF），之后开始计时；
// 在 grace 内 exited 被关闭则什么都不做，否则调用 exit(0)。
func watchShutdown(done, eof, exited <-chan struct{}, grace time.Duration, exit func(int)) {
	select {
	case <-done:
	case <-eof:
	case <-exited:
		return
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-exited:
	case <-t.C:
		exit(0)
	}
}

// eofReader 包装 stdin：读到 io.EOF 时调用一次 onEOF；Close 转发给底层。
type eofReader struct {
	r    io.Reader
	once sync.Once
	on   func()
}

func newEOFReader(r io.Reader, onEOF func()) *eofReader {
	return &eofReader{r: r, on: onEOF}
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.once.Do(e.on)
	}
	return n, err
}

func (e *eofReader) Close() error {
	if c, ok := e.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
