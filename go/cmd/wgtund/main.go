// 命令 wgtund 是 WireGuard 隧道 + SSH 终端桥的助手进程。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/WillyChen0811/base/go/wgtund"
)

func main() {
	dir := flag.String("dir", "", "配置目录（必填，由调用方指定）")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "wgtund: 必须用 -dir 指定配置目录")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := wgtund.Run(ctx, *dir, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "wgtund:", err)
		os.Exit(1)
	}
}
