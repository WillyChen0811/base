// 命令 omlxgw 是 oMLX 的统一入口网关（并发与优先级调度）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/WillyChen0811/base/go/omlxgw"
)

func main() {
	cfg := flag.String("config", "", "配置 JSON 文件（必填）")
	flag.Parse()
	if *cfg == "" {
		fmt.Fprintln(os.Stderr, "omlxgw: 必须用 -config 指定配置文件")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := omlxgw.Run(ctx, *cfg, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "omlxgw:", err)
		os.Exit(1)
	}
}
