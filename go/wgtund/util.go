package wgtund

// 项目同步（projsync.go、projsync_v2.go）和 wgtund.go 共用的几个小工具。
// 原先放在 support.go 里；2026-10-26 拆协助时 support.go 整个删了，这几个项目同步还在用，挪到这里。

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/WillyChen0811/base/go/tmux"
)

// xxyiDir 返回 ~/.xxyi；HOME 取不到时报错，不落相对路径。
func xxyiDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("取不到主目录: %w", err)
	}
	return filepath.Join(home, ".xxyi"), nil
}

// canonPath 把路径规范化为绝对路径并解开符号链接（路径必须存在）。
func canonPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// gitToplevel 返回 dir 所在 git 仓库顶层目录（已解开符号链接）。
func gitToplevel(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSpace(string(out)))
}

// gitSSHCommandVia 生成 GIT_SSH_COMMAND：git 经本机线路转发口（forwardAddr，形如 127.0.0.1:端口）走 SSH，
// 转发口会接到主机的当前线路；主机密钥仍按仓库 remote 里写的主机名严格校验，只用指定私钥。
func gitSSHCommandVia(forwardAddr, knownHosts, identity string) string {
	host, port, err := net.SplitHostPort(forwardAddr)
	if err != nil {
		host, port = forwardAddr, "0"
	}
	return fmt.Sprintf(
		"GIT_SSH_COMMAND=ssh -o ProxyCommand='nc %s %s' -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -i %s",
		host, port, tmux.Quote(knownHosts), tmux.Quote(identity))
}

// routeForwarder 在 127.0.0.1 上开一个转发口：每个进来的连接都拨到主机的当前线路，再双向拷贝。
func routeForwarder(pool *sshPool, host string) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // 转发口已关闭
			}
			go func() {
				ctx, cancel := context.WithCancel(pool.ctx)
				defer cancel()
				up, err := pool.dialActive(ctx, host)
				if err != nil {
					c.Close()
					return
				}
				done := make(chan struct{}, 2)
				cp := func(dst, src net.Conn) {
					io.Copy(dst, src)
					done <- struct{}{}
				}
				go cp(up, c)
				go cp(c, up)
				<-done // 任一方向结束就两边都关
				c.Close()
				up.Close()
				<-done
			}()
		}
	}()
	return ln, nil
}
