package wgtund

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/xxyica"
)

// TestPrepareMsgCA 首次调用把 CA、mac/ubuntu 客户端包、本机 client.json 落对位置、权限收紧；
// 再次调用幂等（密钥和证书内容不变）；revoked.json 写入后吊销表按 mtime 缓存生效。
func TestPrepareMsgCA(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".xxyi")
	caDir := filepath.Join(root, "ca")
	clientsDir := filepath.Join(caDir, "clients")

	ca, revoked, err := prepareMsgCA()
	if err != nil {
		t.Fatalf("prepareMsgCA: %v", err)
	}
	if ca == nil || revoked == nil {
		t.Fatal("prepareMsgCA 应返回 CA 和吊销查询函数")
	}

	read := func(p string) []byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读 %s: %v", p, err)
		}
		return b
	}
	permOf := func(p string) os.FileMode {
		t.Helper()
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		return st.Mode().Perm()
	}

	if got := permOf(filepath.Join(caDir, "ca.key")); got != 0o600 {
		t.Errorf("ca.key 权限应为 0600，得 %o", got)
	}
	pubB := read(filepath.Join(caDir, "ca.pub"))
	if len(pubB) == 0 {
		t.Error("ca.pub 为空")
	}
	macB := read(filepath.Join(clientsDir, "mac.json"))
	ubiB := read(filepath.Join(clientsDir, "ubuntu.json"))
	clientB := read(filepath.Join(root, "client.json"))
	if got := permOf(filepath.Join(root, "client.json")); got != 0o600 {
		t.Errorf("client.json 权限应为 0600，得 %o", got)
	}
	if !bytes.Equal(clientB, macB) {
		t.Error("client.json 应与 clients/mac.json 内容相同（本机只认这一个固定位置）")
	}

	// 再次调用：CA 公钥与所有已签发文件原样保留。
	ca2, _, err := prepareMsgCA()
	if err != nil {
		t.Fatalf("第二次 prepareMsgCA: %v", err)
	}
	if !ca2.Public().Equal(ca.Public()) {
		t.Error("第二次调用 CA 公钥变了（密钥只在首次生成）")
	}
	if got := read(filepath.Join(caDir, "ca.pub")); !bytes.Equal(got, pubB) {
		t.Error("第二次调用 ca.pub 内容变了")
	}
	if got := read(filepath.Join(clientsDir, "mac.json")); !bytes.Equal(got, macB) {
		t.Error("第二次调用 clients/mac.json 内容变了")
	}
	if got := read(filepath.Join(clientsDir, "ubuntu.json")); !bytes.Equal(got, ubiB) {
		t.Error("第二次调用 clients/ubuntu.json 内容变了")
	}
	if got := read(filepath.Join(root, "client.json")); !bytes.Equal(got, clientB) {
		t.Error("第二次调用 client.json 内容变了")
	}

	// 吊销表：先确认可用（文件不存在 = 空表），写入 ubuntu 序列号后对该序列号为真。
	ubiCert, _, _, err := xxyica.LoadClientBundle(filepath.Join(clientsDir, "ubuntu.json"))
	if err != nil {
		t.Fatalf("读 ubuntu 客户端包: %v", err)
	}
	macCert, _, _, err := xxyica.LoadClientBundle(filepath.Join(clientsDir, "mac.json"))
	if err != nil {
		t.Fatalf("读 mac 客户端包: %v", err)
	}
	if revoked(ubiCert.Serial) {
		t.Fatal("刚建好不该有吊销")
	}
	if revoked(macCert.Serial) {
		t.Fatal("刚建好不该有吊销")
	}
	revPath := filepath.Join(caDir, "revoked.json")
	if err := os.WriteFile(revPath, []byte(`["`+ubiCert.Serial+`"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 吊销表按 mtime 缓存：把 mtime 推后一秒，确保缓存失效重读。
	st, err := os.Stat(revPath)
	if err != nil {
		t.Fatal(err)
	}
	plus := st.ModTime().Add(time.Second)
	if err := os.Chtimes(revPath, plus, plus); err != nil {
		t.Fatal(err)
	}
	if !revoked(ubiCert.Serial) {
		t.Error("写入 revoked.json 后 ubuntu 序列号应判已吊销")
	}
	if revoked(macCert.Serial) {
		t.Error("mac 序列号不该被吊销")
	}
}

// captureStderr 捕获 fn 期间写到 os.Stderr 的内容（wgtund 的告警都走 stderr）。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	fn()
	os.Stderr = old
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tamperCertSig 把客户端包里的证书签名换成 64 个零字节（模拟旧签名域/坏签名），
// 密钥对与其余字段原样保留——LoadClientBundle 照样能读，VerifyCert 必挂。
func tamperCertSig(t *testing.T, path string) {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var bundle map[string]json.RawMessage
	if err := json.Unmarshal(blob, &bundle); err != nil {
		t.Fatal(err)
	}
	var certMap map[string]json.RawMessage
	if err := json.Unmarshal(bundle["cert"], &certMap); err != nil {
		t.Fatal(err)
	}
	certMap["sig"] = json.RawMessage(`"` + base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)) + `"`)
	bundle["cert"], err = json.Marshal(certMap)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureClientBundleResignsKeepsKey 已存在的客户端包证书用当前 CA 校验不过时
// 保留原客户端密钥对重新签发（只换证书）并 stderr 说明；本机 client.json 随之更新。
func TestEnsureClientBundleResignsKeepsKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".xxyi")
	macPath := filepath.Join(root, "ca", "clients", "mac.json")

	ca, _, err := prepareMsgCA()
	if err != nil {
		t.Fatalf("prepareMsgCA: %v", err)
	}
	cert0, priv0, _, err := xxyica.LoadClientBundle(macPath)
	if err != nil {
		t.Fatalf("读 mac 客户端包: %v", err)
	}

	tamperCertSig(t, macPath)
	// 前提确认：篡改后包还能读出同一对密钥，但证书（现读，签名已是假的）用当前 CA 校验必挂。
	certBad, privBad, _, err := xxyica.LoadClientBundle(macPath)
	if err != nil {
		t.Fatalf("篡改签名后包应仍可读出密钥对: %v", err)
	}
	if !bytes.Equal(privBad, priv0) {
		t.Fatal("篡改不该动密钥对")
	}
	if err := xxyica.VerifyCert(ca.Public(), certBad, time.Now(), nil); err == nil {
		t.Fatal("前提不成立：坏签名证书竟校验通过")
	}

	stderr := captureStderr(t, func() {
		_, _, err = prepareMsgCA()
	})
	if err != nil {
		t.Fatalf("第二次 prepareMsgCA: %v", err)
	}
	if !strings.Contains(stderr, "校验不通过") || !strings.Contains(stderr, "重新签发") {
		t.Errorf("stderr 该说明校验不通过并重签：%q", stderr)
	}

	cert1, priv1, _, err := xxyica.LoadClientBundle(macPath)
	if err != nil {
		t.Fatalf("重签后读 mac 客户端包: %v", err)
	}
	if !bytes.Equal(priv1, priv0) {
		t.Error("重签应保留原客户端密钥对，私钥变了")
	}
	if !bytes.Equal(cert1.PublicKey, cert0.PublicKey) {
		t.Error("重签后公钥应与原密钥对一致")
	}
	if cert1.Serial == cert0.Serial {
		t.Error("重签应换新的证书序列号")
	}
	if err := xxyica.VerifyCert(ca.Public(), cert1, time.Now(), nil); err != nil {
		t.Errorf("重签出的证书用当前 CA 应验得过: %v", err)
	}

	// 本机 ~/.xxyi/client.json 随之更新为新包。
	clientB, err := os.ReadFile(filepath.Join(root, "client.json"))
	if err != nil {
		t.Fatal(err)
	}
	macB, err := os.ReadFile(macPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clientB, macB) {
		t.Error("重签后 client.json 应与新 clients/mac.json 一致")
	}

	// 再跑一次：新证书验得过，不再重签（幂等，stderr 无重签说明）。
	stderr2 := captureStderr(t, func() {
		_, _, err = prepareMsgCA()
	})
	if err != nil {
		t.Fatalf("第三次 prepareMsgCA: %v", err)
	}
	if strings.Contains(stderr2, "重新签发") {
		t.Errorf("第二次重跑不该再重签：%q", stderr2)
	}
	cert2, _, _, err := xxyica.LoadClientBundle(macPath)
	if err != nil {
		t.Fatal(err)
	}
	if cert2.Serial != cert1.Serial {
		t.Error("证书验得过时不该动（指纹不能因重启而变）")
	}
}

// TestRevokedTableFailClosed 吊销表：不存在 = 空表；首次就读不到好内容一律当已吊销
// （fail-closed）并 stderr 记录；之前成功读到过则沿用上次成功的表。
func TestRevokedTableFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")

	// 不存在 = 空表。
	rt0 := &revokedTable{path: path}
	if rt0.revoked("aa") {
		t.Error("revoked.json 不存在应视为空表")
	}

	// 首次动作就撞上坏 JSON：此前从未成功读到 → 任何序列号都当已吊销，stderr 记录。
	if err := os.WriteFile(path, []byte(`{"not":"a list"`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt1 := &revokedTable{path: path}
	stderr := captureStderr(t, func() {
		if !rt1.revoked("aa") {
			t.Error("从未成功读到过吊销表时应一律当已吊销")
		}
		if !rt1.revoked("bb") {
			t.Error("fail-closed 时任何序列号都该拦")
		}
	})
	if !strings.Contains(stderr, "按全部吊销处理") {
		t.Errorf("stderr 该说明按全部吊销处理：%q", stderr)
	}

	// 修好后按表走。
	if err := os.WriteFile(path, []byte(`["aa"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt2 := &revokedTable{path: path}
	if !rt2.revoked("aa") {
		t.Error("表内的序列号应判已吊销")
	}
	if rt2.revoked("bb") {
		t.Error("表外的序列号不该被吊销")
	}

	// 之前成功读到过、这次坏了：沿用上次成功的表，同样 stderr 记录。
	if err := os.WriteFile(path, []byte(`[broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	plus := st.ModTime().Add(time.Second)
	if err := os.Chtimes(path, plus, plus); err != nil { // 确保缓存失效、真去重读
		t.Fatal(err)
	}
	stderr2 := captureStderr(t, func() {
		if !rt2.revoked("aa") {
			t.Error("应沿用上次成功的表：表内序列号仍拦")
		}
		if rt2.revoked("cc") {
			t.Error("应沿用上次成功的表：表外序列号仍放")
		}
	})
	if !strings.Contains(stderr2, "沿用上次成功读到的表") {
		t.Errorf("stderr 该说明沿用旧表：%q", stderr2)
	}
}

// TestDenyAllVerifier CA 准备失败时顶上的校验器：真 CA 签的合法请求也必须拒绝。
func TestDenyAllVerifier(t *testing.T) {
	ca, err := xxyica.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Issue("mac", pub, xxyica.DefaultPerm("mac"), time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"from":"mac:x","to":"ubuntu:y","text":"hi"}`)
	hdr, err := xxyica.SignRequest(priv, cert, "POST", "/msg/send", body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	v := denyAllVerifier()
	if _, err := v.Verify(hdr, "POST", "/msg/send", body, time.Now()); err == nil {
		t.Fatal("denyAllVerifier 对任何合法签名都应拒绝")
	}
}
