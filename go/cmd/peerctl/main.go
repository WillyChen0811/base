// peerctl：Ubuntu 一侧的小命令，只剩两件事（2026-10-26 协助/消息/审批拆除后）：
//   - state busy|idle|gone：给 Claude/Codex 的钩子用，把忙闲状态写进本机 ~/.peerctl-state，永不联网；
//   - projsync-facts / projsync-apply：项目同步的远程协议，Mac 的 wgtund 经 SSH 调用。
//
// 名字沿用旧的：Mac 的 hosts.json 里 projsync 段的 remote_cmd 缺省路径还指向 ~/.local/bin/peerctl。
package main

import "os"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
