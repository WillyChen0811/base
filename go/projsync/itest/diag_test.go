//go:build itest

package itest

import (
	"bytes"
	"encoding/base64"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestItestDiagHostKeyProbe 是一次性诊断（不是同步场景）：连一次 Ubuntu，把「服务器实际出示的」
// 和「环境变量注入期望的」主机公钥按指纹打全，给 RESULT 留定性证据。
// 这里观察用的回调只记录不拒绝（不跑任何写操作）；七个场景测试仍然用 FixedHostKey，绝不放宽。
func TestItestDiagHostKeyProbe(t *testing.T) {
	addr := envReq(t, "PROJFILES_ITEST_ADDR")
	user := envReq(t, "PROJFILES_ITEST_USER")
	keyPEM, err := base64.StdEncoding.DecodeString(envReq(t, "PROJFILES_ITEST_KEY_B64"))
	if err != nil {
		t.Fatalf("PROJFILES_ITEST_KEY_B64 不是合法 base64：%v", err)
	}
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err != nil {
		t.Fatalf("解析 SSH 私钥失败：%v", err)
	}
	expRaw := []byte(envReq(t, "PROJFILES_ITEST_HOSTKEY"))
	expKey, _, _, _, err := ssh.ParseAuthorizedKey(expRaw)
	if err != nil {
		t.Fatalf("PROJFILES_ITEST_HOSTKEY 解析失败：%v", err)
	}
	t.Logf("期望主机公钥：type=%s 指纹=%s", expKey.Type(), ssh.FingerprintSHA256(expKey))
	t.Logf("注入原文（%d 字节）：%q", len(expRaw), string(expRaw))
	t.Logf("注入私钥：type=%s", signer.PublicKey().Type())

	var got ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			got = k
			t.Logf("服务器实际出示：type=%s 指纹=%s", k.Type(), ssh.FingerprintSHA256(k))
			return nil
		},
		// 钉住 ed25519（与场景配置一致）：让对方出示与注入同型的钥匙再比。
		HostKeyAlgorithms: []string{"ssh-ed25519"},
		Timeout:           15 * time.Second,
	}
	c, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Logf("诊断拨号失败：%v", err)
		return
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		t.Logf("诊断开会话失败：%v", err)
		return
	}
	defer s.Close()
	hostname, err := s.Output("hostname")
	t.Logf("诊断连接执行 hostname：out=%q err=%v", hostname, err)
	t.Logf("两边公钥一致=%v", got != nil && bytes.Equal(got.Marshal(), expKey.Marshal()))
}
