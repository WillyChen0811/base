package xxyica

import (
	"fmt"
	"strings"
)

// Perm 是证书里的权限声明。
type Perm struct {
	AddrPrefix string   `json:"addr_prefix"` // 本客户端能用的发件地址前缀，"mac:" 或 "ubuntu:"
	SendTo     []string `json:"send_to"`     // 可发往的地址模式，"ubuntu:*" 或完整地址；"*" 表示任意
	Kinds      []string `json:"kinds"`       // 可用消息类型：request/reply/notice/code
	Repos      []string `json:"repos"`       // 可同步的仓库名；"*" 表示全部
	Attach     bool     `json:"attach"`      // 可带附件
}

// DefaultPerm 给出某一侧的默认权限：mac 只能发往 ubuntu 侧，反之亦然；
// 消息类型、仓库全开，允许附件。side 不是 "mac"/"ubuntu" 时返回空 Perm（什么都拒绝）。
func DefaultPerm(side string) Perm {
	switch side {
	case "mac":
		return Perm{
			AddrPrefix: "mac:",
			SendTo:     []string{"ubuntu:*"},
			Kinds:      []string{"request", "reply", "notice", "code"},
			Repos:      []string{"*"},
			Attach:     true,
		}
	case "ubuntu":
		return Perm{
			AddrPrefix: "ubuntu:",
			SendTo:     []string{"mac:*"},
			Kinds:      []string{"request", "reply", "notice", "code"},
			Repos:      []string{"*"},
			Attach:     true,
		}
	default:
		return Perm{}
	}
}

// Allows 检查一次发送是否落在证书权限内，不满足返回中文原因的错误。
// repo 为空时不检查仓库；to 的模式匹配规则：
// "*" 匹配任意，"x:*" 匹配以 "x:" 开头的地址，其余按全等。
func (p Perm) Allows(from, to, kind, repo string, attach bool) error {
	if !strings.HasPrefix(from, p.AddrPrefix) {
		return fmt.Errorf("发件地址 %s 不符合证书前缀 %s", from, p.AddrPrefix)
	}
	if !matchSendTo(p.SendTo, to) {
		return fmt.Errorf("证书不允许发往 %s", to)
	}
	if !contains(p.Kinds, kind) {
		return fmt.Errorf("证书不允许消息类型 %s", kind)
	}
	if repo != "" && !contains(p.Repos, repo) && !contains(p.Repos, "*") {
		return fmt.Errorf("证书不允许同步仓库 %s", repo)
	}
	if attach && !p.Attach {
		return fmt.Errorf("证书不允许带附件")
	}
	return nil
}

// matchSendTo 按模式列表匹配收件地址。
func matchSendTo(patterns []string, to string) bool {
	for _, pat := range patterns {
		if pat == "*" {
			return true
		}
		if prefix, ok := strings.CutSuffix(pat, "*"); ok {
			if strings.HasPrefix(to, prefix) {
				return true
			}
			continue
		}
		if pat == to {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
