package wgtund

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
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
