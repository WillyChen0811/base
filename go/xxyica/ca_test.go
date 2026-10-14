package xxyica

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestCerts(t *testing.T) (*CA, ed25519.PublicKey, ed25519.PrivateKey, Cert) {
	t.Helper()
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	cPub, cPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	cert, err := ca.Issue("mac-1", cPub, DefaultPerm("mac"), 24*time.Hour, now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return ca, cPub, cPriv, cert
}

func TestIssueAndVerify(t *testing.T) {
	ca, _, _, cert := newTestCerts(t)
	if len(cert.Serial) != 16 {
		t.Errorf("序列号长度 %d，期望 16: %q", len(cert.Serial), cert.Serial)
	}
	if len(cert.Sig) != ed25519.SignatureSize {
		t.Errorf("签名长度 %d 不对", len(cert.Sig))
	}
	if err := VerifyCert(ca.Public(), cert, time.Now(), nil); err != nil {
		t.Fatalf("VerifyCert: %v", err)
	}
	// JSON 往返后仍能验签（时间戳、字段顺序都要稳定）。
	blob, err := json.Marshal(cert)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Cert
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := VerifyCert(ca.Public(), back, time.Now(), nil); err != nil {
		t.Fatalf("往返后 VerifyCert: %v", err)
	}
}

func TestVerifyCertTamperedPerm(t *testing.T) {
	ca, _, _, cert := newTestCerts(t)
	for name, mutate := range map[string]func(*Cert){
		"改 SendTo":     func(c *Cert) { c.Perm.SendTo = []string{"*"} },
		"改 AddrPrefix": func(c *Cert) { c.Perm.AddrPrefix = "ubuntu:" },
		"改 Kinds":      func(c *Cert) { c.Perm.Kinds = []string{"request", "reply", "notice", "code", "evil"} },
		"改 Repos":      func(c *Cert) { c.Perm.Repos = []string{"secret"} },
		"改 Attach":     func(c *Cert) { c.Perm.Attach = false },
		"改 Name":       func(c *Cert) { c.Name = "ubuntu-1" },
		"改有效期":         func(c *Cert) { c.NotAfter = c.NotAfter.Add(time.Hour) },
	} {
		bad := cert
		bad.Perm = DefaultPerm("mac") // 复制 slice 引用前先重置
		bad.Perm.SendTo = append([]string{}, cert.Perm.SendTo...)
		bad.Perm.Kinds = append([]string{}, cert.Perm.Kinds...)
		bad.Perm.Repos = append([]string{}, cert.Perm.Repos...)
		mutate(&bad)
		if err := VerifyCert(ca.Public(), bad, time.Now(), nil); err == nil {
			t.Errorf("%s 后验签竟然通过", name)
		}
	}
}

func TestVerifyCertTimeWindowAndRevoked(t *testing.T) {
	ca, _, _, cert := newTestCerts(t)
	now := time.Now()
	if err := VerifyCert(ca.Public(), cert, now.Add(-time.Hour), nil); err == nil || !strings.Contains(err.Error(), "尚未生效") {
		t.Errorf("未生效应报错，实际: %v", err)
	}
	if err := VerifyCert(ca.Public(), cert, now.Add(48*time.Hour), nil); err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Errorf("过期应报错，实际: %v", err)
	}
	revoked := false
	if err := VerifyCert(ca.Public(), cert, now, func(string) bool { return revoked }); err != nil {
		t.Fatalf("未吊销时应通过: %v", err)
	}
	revoked = true
	err := VerifyCert(ca.Public(), cert, now, func(string) bool { return revoked })
	if err == nil || !strings.Contains(err.Error(), "吊销") {
		t.Errorf("吊销应报错，实际: %v", err)
	}
	// 吊销回调收到的必须是序列号。
	var got string
	_ = VerifyCert(ca.Public(), cert, now, func(s string) bool { got = s; return false })
	if got != cert.Serial {
		t.Errorf("revoked 收到 %q，期望 %q", got, cert.Serial)
	}
}

func TestVerifyCertWrongCA(t *testing.T) {
	_, _, _, cert := newTestCerts(t)
	other, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := VerifyCert(other.Public(), cert, time.Now(), nil); err == nil {
		t.Error("别的 CA 公钥验签不应通过")
	}
}

func TestIssueValidation(t *testing.T) {
	ca, cPub, _, _ := newTestCerts(t)
	if _, err := ca.Issue("Mac_1", cPub, DefaultPerm("mac"), time.Hour, time.Now()); err == nil {
		t.Error("非法名字应报错")
	}
	if _, err := ca.Issue(strings.Repeat("a", 33), cPub, DefaultPerm("mac"), time.Hour, time.Now()); err == nil {
		t.Error("超长名字应报错")
	}
	if _, err := ca.Issue("ok", cPub[:31], DefaultPerm("mac"), time.Hour, time.Now()); err == nil {
		t.Error("短公钥应报错")
	}
}

func TestAllows(t *testing.T) {
	perm := DefaultPerm("mac")
	if err := perm.Allows("mac:book-1", "ubuntu:studio", "request", "base", false); err != nil {
		t.Errorf("默认权限内应通过: %v", err)
	}
	if err := perm.Allows("mac:book-1", "ubuntu:studio", "request", "", true); err != nil {
		t.Errorf("空 repo 应不检查: %v", err)
	}
	// 否定用例用收窄的 Perm（默认 Perm Repos 是 "*" 且 Attach=true，拒不了）。
	narrow := Perm{
		AddrPrefix: "mac:",
		SendTo:     []string{"ubuntu:studio"},
		Kinds:      []string{"request"},
		Repos:      []string{"base"},
		Attach:     false,
	}
	cases := []struct {
		name           string
		from, to, kind string
		repo           string
		attach         bool
		wantSub        string
	}{
		{"前缀不符", "ubuntu:studio", "ubuntu:studio", "request", "base", false, "前缀"},
		{"SendTo 不符", "mac:book-1", "mac:other", "request", "base", false, "发往"},
		{"kind 不在", "mac:book-1", "ubuntu:studio", "delete", "base", false, "消息类型"},
		{"repo 不在", "mac:book-1", "ubuntu:studio", "request", "secret", false, "仓库"},
		{"附件不许", "mac:book-1", "ubuntu:studio", "request", "base", true, "附件"},
	}
	for _, c := range cases {
		err := narrow.Allows(c.from, c.to, c.kind, c.repo, c.attach)
		if err == nil || !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s: 期望含 %q 的错误，实际 %v", c.name, c.wantSub, err)
		}
	}

	wild := Perm{
		AddrPrefix: "mac:",
		SendTo:     []string{"ubuntu:studio", "*"},
		Kinds:      []string{"notice"},
		Repos:      nil,
		Attach:     false,
	}
	if err := wild.Allows("mac:a", "anything:at/all", "notice", "", false); err != nil {
		t.Errorf(`"*" 应匹配任意地址: %v`, err)
	}
	if err := wild.Allows("mac:a", "ubuntu:studio", "notice", "", false); err != nil {
		t.Errorf("全等地址应通过: %v", err)
	}
	// 通配 "ubuntu:*" 不能匹配掉冒号前缀的邻居。
	star := Perm{AddrPrefix: "mac:", SendTo: []string{"ubuntu:*"}, Kinds: []string{"request"}, Repos: []string{"*"}}
	if err := star.Allows("mac:a", "ubuntu:", "request", "any", false); err != nil {
		t.Errorf(`"ubuntu:*" 应匹配前缀本身: %v`, err)
	}
	if err := star.Allows("mac:a", "ubuntuX:y", "request", "any", false); err == nil {
		t.Error(`"ubuntu:*" 不应匹配 "ubuntuX:y"`)
	}
	if err := star.Allows("mac:a", "ubuntu:x", "request", "any", true); err == nil {
		t.Error("Attach=false 时带附件应拒绝")
	}

	if got := DefaultPerm("ubuntu"); got.AddrPrefix != "ubuntu:" || got.SendTo[0] != "mac:*" || !got.Attach {
		t.Errorf("ubuntu 默认权限不对: %+v", got)
	}
	if got := DefaultPerm("nonsense"); got.AddrPrefix != "" {
		t.Errorf("未知 side 应返回空 Perm: %+v", got)
	}
}

func TestSignRequestAndVerify(t *testing.T) {
	ca, _, cPriv, cert := newTestCerts(t)
	v := NewVerifier(ca.Public(), nil)
	now := time.Now()
	header, err := SignRequest(cPriv, cert, "POST", "/msg/send?x=1", []byte(`{"text":"hi"}`), now)
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	got, err := v.Verify(header, "POST", "/msg/send?x=1", []byte(`{"text":"hi"}`), now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Name != cert.Name || got.Serial != cert.Serial {
		t.Errorf("返回证书不对: %+v", got)
	}

	// 每个用例都重新签一个新 header（新 nonce），失败原因才落在被篡改的那一项上。
	fresh := func(at time.Time) string {
		h, err := SignRequest(cPriv, cert, "POST", "/msg/send?x=1", []byte(`{"text":"hi"}`), at)
		if err != nil {
			t.Fatalf("SignRequest: %v", err)
		}
		return h
	}
	tamper := []struct {
		name     string
		wantSub  string // 空表示只看成败
		run      func() error
		wantPass bool
	}{
		{"改 body", "签名", func() error {
			_, err := v.Verify(fresh(now), "POST", "/msg/send?x=1", []byte("evil"), now)
			return err
		}, false},
		{"改 path", "签名", func() error {
			_, err := v.Verify(fresh(now), "POST", "/msg/other", []byte(`{"text":"hi"}`), now)
			return err
		}, false},
		{"改 method", "签名", func() error {
			_, err := v.Verify(fresh(now), "GET", "/msg/send?x=1", []byte(`{"text":"hi"}`), now)
			return err
		}, false},
		{"ts 超 5 分钟", "时间戳", func() error {
			_, err := v.Verify(fresh(now), "POST", "/msg/send?x=1", []byte(`{"text":"hi"}`), now.Add(6*time.Minute))
			return err
		}, false},
		{"ts 拨到 4 分钟前仍可以", "", func() error {
			_, err := v.Verify(fresh(now.Add(-4*time.Minute)), "POST", "/msg/send?x=1", []byte(`{"text":"hi"}`), now)
			return err
		}, true},
		{"认证头乱码", "base64", func() error {
			_, err := v.Verify("!!!not-base64!!!", "POST", "/p", nil, now)
			return err
		}, false},
	}
	for _, c := range tamper {
		err := c.run()
		if c.wantPass {
			if err != nil {
				t.Errorf("%s 应通过: %v", c.name, err)
			}
			continue
		}
		if err == nil || (c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub)) {
			t.Errorf("%s 应失败（原因含 %q），实际: %v", c.name, c.wantSub, err)
		}
	}

	// nonce 重放：同一个 header 第二次必须被拒。
	if _, err := v.Verify(header, "POST", "/msg/send?x=1", []byte(`{"text":"hi"}`), now); err == nil || !strings.Contains(err.Error(), "重放") {
		t.Errorf("nonce 重放应被拒，实际: %v", err)
	}

	// 别的 CA 签的请求：签名阶段就挂。
	otherCA, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	v2 := NewVerifier(otherCA.Public(), nil)
	if _, err := v2.Verify(header, "POST", "/msg/send?x=1", []byte(`{"text":"hi"}`), now); err == nil {
		t.Error("别的 CA 的请求头不应通过")
	}

	// 吊销生效。
	v3 := NewVerifier(ca.Public(), func(serial string) bool { return serial == cert.Serial })
	h3, err := SignRequest(cPriv, cert, "GET", "/x", nil, now)
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	if _, err := v3.Verify(h3, "GET", "/x", nil, now); err == nil || !strings.Contains(err.Error(), "吊销") {
		t.Errorf("吊销应被拒，实际: %v", err)
	}
}

func TestSignRequestKeyMismatch(t *testing.T) {
	ca, _, _, cert := newTestCerts(t)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if _, err := SignRequest(otherPriv, cert, "GET", "/", nil, time.Now()); err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Errorf("私钥与证书不配应报错，实际: %v", err)
	}
	_ = ca
}

func TestLoadOrCreateCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	ca1, created, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("首次 LoadOrCreateCA: %v", err)
	}
	if !created {
		t.Error("首次应 created=true")
	}
	st, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("Stat ca.key: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("ca.key 权限 %o，期望 600", st.Mode().Perm())
	}
	data, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "XXYI CA PRIVATE KEY") {
		t.Errorf("ca.key 不是期望的 PEM 类型:\n%s", data)
	}
	// 没有残留临时文件。
	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(tmps) != 0 {
		t.Errorf("残留临时文件: %v", tmps)
	}

	ca2, created, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("再次 LoadOrCreateCA: %v", err)
	}
	if created {
		t.Error("再次应 created=false")
	}
	if !ca2.Public().Equal(ca1.Public()) {
		t.Error("两次加载的 CA 公钥不同")
	}
	msg := []byte("hello")
	if !VerifyMessage(ca2.Public(), msg, ca1.Sign(msg)) {
		t.Error("重载后的 CA 验不过旧私钥的签名")
	}

	// 只剩一个文件必须报错，不许静默重新生成。
	for _, victim := range []string{"ca.key", "ca.pub"} {
		d2 := filepath.Join(t.TempDir(), "half")
		if _, _, err := LoadOrCreateCA(d2); err != nil {
			t.Fatalf("准备半套环境: %v", err)
		}
		if err := os.Remove(filepath.Join(d2, victim)); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, _, err := LoadOrCreateCA(d2); err == nil || !strings.Contains(err.Error(), "只存在一个") {
			t.Errorf("只剩 %s 应报错，实际: %v", victim, err)
		}
	}
}

func TestClientBundleRoundTrip(t *testing.T) {
	ca, _, cPriv, cert := newTestCerts(t)
	path := filepath.Join(t.TempDir(), "client.json")
	if err := WriteClientBundle(path, cert, cPriv, ca.Public()); err != nil {
		t.Fatalf("WriteClientBundle: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("bundle 权限 %o，期望 600", st.Mode().Perm())
	}
	cert2, priv2, caPub2, err := LoadClientBundle(path)
	if err != nil {
		t.Fatalf("LoadClientBundle: %v", err)
	}
	if cert2.Name != cert.Name || cert2.Serial != cert.Serial {
		t.Errorf("证书往返不对: %+v", cert2)
	}
	if !priv2.Equal(cPriv) || !caPub2.Equal(ca.Public()) {
		t.Error("私钥/CA 公钥往返不对")
	}

	// 私钥不配：手工改坏 bundle 里的 private_key。
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	_, stray, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	raw["private_key"], _ = json.Marshal([]byte(stray))
	bad, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	badPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badPath, bad, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, _, err := LoadClientBundle(badPath); err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Errorf("私钥不配应报错，实际: %v", err)
	}
}

func TestVerifyMessage(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	data := []byte("notice body")
	if !VerifyMessage(ca.Public(), data, ca.Sign(data)) {
		t.Error("真签名应通过")
	}
	if VerifyMessage(ca.Public(), []byte("other"), ca.Sign(data)) {
		t.Error("改过的内容不应通过")
	}
	other, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if VerifyMessage(other.Public(), data, ca.Sign(data)) {
		t.Error("别的 CA 的公钥不应通过")
	}
	if VerifyMessage(ca.Public()[:31], data, ca.Sign(data)) {
		t.Error("短公钥不应通过")
	}
}
