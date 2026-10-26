package xxyica

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClients 是测试里的两侧客户端，权限用惯用的 DefaultPerm。
func testClients() map[string]Perm {
	return map[string]Perm{"mac": DefaultPerm("mac"), "ubuntu": DefaultPerm("ubuntu")}
}

// logCollector 收集 LogFunc 的输出；并发安全。
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCollector) log(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *logCollector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

// captureStderr 捕获 fn 期间写到 os.Stderr 的内容（LogFunc 为 nil 时的去向）。
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

// TestPrepareCA 首次调用把 CA 和各侧客户端包落对位置、权限收紧、权限声明用的是调用方传的；
// 再次调用幂等（密钥和证书内容不变）；revoked.json 写入后吊销表按 mtime 缓存生效。
func TestPrepareCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
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

	ca, revoked, err := PrepareCA(dir, testClients(), DefaultClientValidFor, nil)
	if err != nil {
		t.Fatalf("PrepareCA: %v", err)
	}
	if ca == nil || revoked == nil {
		t.Fatal("PrepareCA 应返回 CA 和吊销查询函数")
	}
	if got := permOf(filepath.Join(dir, "ca.key")); got != 0o600 {
		t.Errorf("ca.key 权限应为 0600，得 %o", got)
	}
	pubB := read(filepath.Join(dir, "ca.pub"))
	if len(pubB) == 0 {
		t.Error("ca.pub 为空")
	}
	macPath, ubiPath := ClientBundlePath(dir, "mac"), ClientBundlePath(dir, "ubuntu")
	if macPath != filepath.Join(dir, "clients", "mac.json") {
		t.Errorf("ClientBundlePath = %s", macPath)
	}
	macB, ubiB := read(macPath), read(ubiPath)

	macCert, macPriv, macCAPub, err := LoadClientBundle(macPath)
	if err != nil {
		t.Fatalf("读 mac 客户端包: %v", err)
	}
	ubiCert, _, _, err := LoadClientBundle(ubiPath)
	if err != nil {
		t.Fatalf("读 ubuntu 客户端包: %v", err)
	}
	if macCert.Name != "mac" || ubiCert.Name != "ubuntu" {
		t.Errorf("证书名不对: %q %q", macCert.Name, ubiCert.Name)
	}
	if macCert.Perm.AddrPrefix != "mac:" || ubiCert.Perm.AddrPrefix != "ubuntu:" {
		t.Errorf("权限声明不是调用方传的: %+v %+v", macCert.Perm, ubiCert.Perm)
	}
	if !macCAPub.Equal(ca.Public()) {
		t.Error("客户端包里的 CA 公钥应是当前 CA 的")
	}
	if err := VerifyCert(ca.Public(), macCert, time.Now(), nil); err != nil {
		t.Errorf("签发的证书用 CA 应验得过: %v", err)
	}
	if got := macCert.NotAfter.Sub(macCert.NotBefore); got != DefaultClientValidFor {
		t.Errorf("有效期 %v，want %v", got, DefaultClientValidFor)
	}
	// 客户端用私钥签请求，用 CA 公钥组的校验器能验过（整条链路通）。
	hdr, err := SignRequest(macPriv, macCert, "POST", "/x", []byte("b"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewVerifier(ca.Public(), revoked).Verify(hdr, "POST", "/x", []byte("b"), time.Now()); err != nil {
		t.Errorf("PrepareCA 返回的吊销函数配 NewVerifier 应放行未吊销证书: %v", err)
	}

	// 再次调用：CA 公钥与所有已签发文件原样保留。
	ca2, _, err := PrepareCA(dir, testClients(), DefaultClientValidFor, nil)
	if err != nil {
		t.Fatalf("第二次 PrepareCA: %v", err)
	}
	if !ca2.Public().Equal(ca.Public()) {
		t.Error("第二次调用 CA 公钥变了（密钥只在首次生成）")
	}
	if got := read(filepath.Join(dir, "ca.pub")); !bytes.Equal(got, pubB) {
		t.Error("第二次调用 ca.pub 内容变了")
	}
	if got := read(macPath); !bytes.Equal(got, macB) {
		t.Error("第二次调用 mac 客户端包内容变了")
	}
	if got := read(ubiPath); !bytes.Equal(got, ubiB) {
		t.Error("第二次调用 ubuntu 客户端包内容变了")
	}

	// 吊销表：先确认可用（文件不存在 = 空表），写入 ubuntu 序列号后对该序列号为真。
	if revoked(ubiCert.Serial) || revoked(macCert.Serial) {
		t.Fatal("刚建好不该有吊销")
	}
	revPath := RevokedPath(dir)
	if revPath != filepath.Join(dir, "revoked.json") {
		t.Errorf("RevokedPath = %s", revPath)
	}
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

// 权限由调用方传：传自定义权限，证书里就是那份；客户端名不合规在动磁盘之前就报错。
func TestPrepareCACustomPermAndBadName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	custom := Perm{AddrPrefix: "ubuntu:build", Kinds: []string{"notice"}}
	ca, _, err := PrepareCA(dir, map[string]Perm{"builder": custom}, time.Hour, nil)
	if err != nil {
		t.Fatalf("PrepareCA: %v", err)
	}
	cert, _, _, err := LoadClientBundle(ClientBundlePath(dir, "builder"))
	if err != nil {
		t.Fatal(err)
	}
	if cert.Perm.AddrPrefix != "ubuntu:build" || len(cert.Perm.Kinds) != 1 || cert.Perm.Kinds[0] != "notice" {
		t.Errorf("证书权限应是调用方传的: %+v", cert.Perm)
	}
	if err := VerifyCert(ca.Public(), cert, time.Now(), nil); err != nil {
		t.Errorf("自定义权限的证书应验得过: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "never")
	for _, name := range []string{"../evil", "Mac", "a/b", "", strings.Repeat("x", 33)} {
		if _, _, err := PrepareCA(bad, map[string]Perm{name: {}}, time.Hour, nil); err == nil {
			t.Errorf("客户端名 %q 应报错", name)
		}
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("名字不合规时不该动磁盘: %v", err)
	}
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

// 已存在的客户端包证书用当前 CA 校验不过时保留原客户端密钥对重新签发（只换证书）并通过 logf 说明；
// 重签后再跑一次不再重签。
func TestEnsureClientBundleResignsKeepsKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	macPath := ClientBundlePath(dir, "mac")

	ca, _, err := PrepareCA(dir, testClients(), DefaultClientValidFor, nil)
	if err != nil {
		t.Fatalf("PrepareCA: %v", err)
	}
	cert0, priv0, _, err := LoadClientBundle(macPath)
	if err != nil {
		t.Fatalf("读 mac 客户端包: %v", err)
	}

	tamperCertSig(t, macPath)
	// 前提确认：篡改后包还能读出同一对密钥，但证书（现读，签名已是假的）用当前 CA 校验必挂。
	certBad, privBad, _, err := LoadClientBundle(macPath)
	if err != nil {
		t.Fatalf("篡改签名后包应仍可读出密钥对: %v", err)
	}
	if !bytes.Equal(privBad, priv0) {
		t.Fatal("篡改不该动密钥对")
	}
	if err := VerifyCert(ca.Public(), certBad, time.Now(), nil); err == nil {
		t.Fatal("前提不成立：坏签名证书竟校验通过")
	}

	var lc logCollector
	if _, _, err = PrepareCA(dir, testClients(), DefaultClientValidFor, lc.log); err != nil {
		t.Fatalf("第二次 PrepareCA: %v", err)
	}
	if got := lc.text(); !strings.Contains(got, "校验不通过") || !strings.Contains(got, "重新签发") {
		t.Errorf("该说明校验不通过并重签：%q", got)
	}

	cert1, priv1, _, err := LoadClientBundle(macPath)
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
	if err := VerifyCert(ca.Public(), cert1, time.Now(), nil); err != nil {
		t.Errorf("重签出的证书用当前 CA 应验得过: %v", err)
	}

	// 再跑一次：新证书验得过，不再重签（幂等，没有重签说明）。
	var lc2 logCollector
	if _, _, err = PrepareCA(dir, testClients(), DefaultClientValidFor, lc2.log); err != nil {
		t.Fatalf("第三次 PrepareCA: %v", err)
	}
	if strings.Contains(lc2.text(), "重新签发") {
		t.Errorf("再跑一次不该重签：%q", lc2.text())
	}
	cert2, _, _, err := LoadClientBundle(macPath)
	if err != nil {
		t.Fatal(err)
	}
	if cert2.Serial != cert1.Serial {
		t.Error("证书验得过时不该动（指纹不能因重启而变）")
	}
}

// 客户端包读不了（内容损坏）：换新密钥重新签发，并说明；logf 为 nil 时说明写到 stderr。
func TestEnsureClientBundleUnreadableGetsNewKey(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, []byte("烂了"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr := captureStderr(t, func() {
		err = EnsureClientBundle(path, ca, "mac", DefaultPerm("mac"), time.Hour, nil)
	})
	if err != nil {
		t.Fatalf("EnsureClientBundle: %v", err)
	}
	if !strings.Contains(stderr, "读不了") || !strings.HasPrefix(stderr, "xxyica: ") || !strings.HasSuffix(stderr, "\n") {
		t.Errorf("logf 为 nil 时该写 stderr 一行并带前缀：%q", stderr)
	}
	cert, _, _, err := LoadClientBundle(path)
	if err != nil {
		t.Fatalf("换新后应能读: %v", err)
	}
	if err := VerifyCert(ca.Public(), cert, time.Now(), nil); err != nil {
		t.Errorf("新证书应验得过: %v", err)
	}
}

// 吊销表：不存在 = 空表；首次就读不到好内容一律当已吊销（fail-closed）并记录；
// 之前成功读到过则沿用上次成功的表；同一原因只记一次。
func TestRevokedTableFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")

	// 不存在 = 空表。
	if NewRevokedTable(path, nil).Revoked("aa") {
		t.Error("revoked.json 不存在应视为空表")
	}

	// 首次动作就撞上坏 JSON：此前从未成功读到 → 任何序列号都当已吊销，记录一次。
	if err := os.WriteFile(path, []byte(`{"not":"a list"`), 0o600); err != nil {
		t.Fatal(err)
	}
	var lc1 logCollector
	rt1 := NewRevokedTable(path, lc1.log)
	if !rt1.Revoked("aa") {
		t.Error("从未成功读到过吊销表时应一律当已吊销")
	}
	if !rt1.Revoked("bb") {
		t.Error("fail-closed 时任何序列号都该拦")
	}
	if got := lc1.text(); !strings.Contains(got, "按全部吊销处理") {
		t.Errorf("该说明按全部吊销处理：%q", got)
	}
	if n := len(lc1.lines); n != 1 {
		t.Errorf("同一原因只该记一次，记了 %d 次：%q", n, lc1.text())
	}

	// 修好后按表走。
	if err := os.WriteFile(path, []byte(`["aa"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt2 := NewRevokedTable(path, nil)
	if !rt2.Revoked("aa") {
		t.Error("表内的序列号应判已吊销")
	}
	if rt2.Revoked("bb") {
		t.Error("表外的序列号不该被吊销")
	}

	// 之前成功读到过、这次坏了：沿用上次成功的表，同样记录；logf 为 nil 时写 stderr。
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
	stderr := captureStderr(t, func() {
		if !rt2.Revoked("aa") {
			t.Error("应沿用上次成功的表：表内序列号仍拦")
		}
		if rt2.Revoked("cc") {
			t.Error("应沿用上次成功的表：表外序列号仍放")
		}
	})
	if !strings.Contains(stderr, "沿用上次成功读到的表") {
		t.Errorf("该说明沿用旧表：%q", stderr)
	}
}

// DenyAllVerifier：真 CA 签的合法请求也必须拒绝。
func TestDenyAllVerifier(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Issue("mac", pub, DefaultPerm("mac"), time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"from":"mac:x","to":"ubuntu:y","text":"hi"}`)
	hdr, err := SignRequest(priv, cert, "POST", "/x", body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DenyAllVerifier().Verify(hdr, "POST", "/x", body, time.Now()); err == nil {
		t.Fatal("DenyAllVerifier 对任何合法签名都应拒绝")
	}
}
