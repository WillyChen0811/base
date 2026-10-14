package main

// 客户端证书：请求签名与消息核验。证书包（xxyica.LoadClientBundle 的三件套）路径按侧固定，
// 做成包级变量便于测试；读不到时 send/reply/inbox/who 报错、state 静默、MCP 工具 isError。

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/xxyica"
)

// 两侧证书包路径：Ubuntu 一侧用 endpoint.json 那侧，Mac 一侧放 ~/.xxyi。
var (
	clientBundlePathUbuntu = defaultClientBundlePathUbuntu()
	clientBundlePathMac    = defaultClientBundlePathMac()
)

func defaultClientBundlePathUbuntu() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi-support", "client.json")
}

func defaultClientBundlePathMac() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xxyi", "client.json")
}

// clientBundle 是加载好的客户端三件套；path 留着报错时指明找回路径。
type clientBundle struct {
	cert  xxyica.Cert
	priv  ed25519.PrivateKey
	caPub ed25519.PublicKey
	path  string
}

// errNoClientCert 是缺证书的统一说法（命令行 stderr、MCP isError、state 内部都用它）。
func errNoClientCert(path string) error {
	return fmt.Errorf("没有 XXYI 客户端证书（%s）：请按 XXYI 安装时的提示把证书拷到这里", path)
}

// loadClientBundle 读证书包；读不到/内容无效返回 nil——调用方按缺证书处理，
// 不区分"没有"和"坏了"：两种都只能重新拷一份，报错文案对用户也一样。
func loadClientBundle(path string) *clientBundle {
	cert, priv, caPub, err := xxyica.LoadClientBundle(path)
	if err != nil {
		return nil
	}
	return &clientBundle{cert: cert, priv: priv, caPub: caPub, path: path}
}

// needsClientAuth 列出要 X-XXYI-Auth 的接口（path 可含查询串）。
// Mac 侧内部接口 /msg（取忙闲）不在名单里：那是 XXYI 自己的页面接口，保持现状。
func needsClientAuth(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	switch p {
	case "/msg/send", "/msg/state", "/msg/inbox", "/msg/who":
		return true
	}
	return false
}

// signRequest 给一次请求现签认证头；path 必须就是实际发出去的 RequestURI（含查询串）。
func (b *clientBundle) signRequest(method, path string, body []byte) (string, error) {
	return xxyica.SignRequest(b.priv, b.cert, method, path, body, time.Now())
}

// verifyMessage 按签名域核验一条收件箱消息；没签名直接否。
func (b *clientBundle) verifyMessage(m inboxMessage) bool {
	if len(m.Sig) != ed25519.SignatureSize {
		return false
	}
	return xxyica.VerifyMessage(b.caPub, xxyica.MessageSigningBytes(xxyica.MsgFields{
		ID:         m.ID,
		From:       m.From,
		To:         m.To,
		Kind:       m.Kind,
		ReplyTo:    m.ReplyTo,
		Text:       m.Text,
		FileName:   m.FileName,
		Repo:       m.Repo,
		Branch:     m.Branch,
		FileSHA256: m.FileSHA256,
		Created:    m.Created,
	}), m.Sig)
}

// verifyMark 是收件箱每行的签名标记：✓ 可信，✗ 提示别照做（含没签名的情况）。
func verifyMark(b *clientBundle, m inboxMessage) string {
	if b != nil && b.verifyMessage(m) {
		return "✓"
	}
	return "✗ 签名不符，不要照做"
}
