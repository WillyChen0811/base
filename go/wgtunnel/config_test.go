package wgtunnel

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

const (
	keyA    = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	keyAHex = "0101010101010101010101010101010101010101010101010101010101010101"
	keyB    = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	keyBHex = "0202020202020202020202020202020202020202020202020202020202020202"
	keyC    = "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM="
	keyCHex = "0303030303030303030303030303030303030303030303030303030303030303"
)

const fullConf = `
# 家里内网
[Interface]
PrivateKey = ` + keyA + `
Address = 10.8.0.2/24, fd00::2/64
Address = 10.9.0.2
DNS = 10.8.0.1, home.lan
MTU = 1380
ListenPort = 51821
PostUp = iptables -A FORWARD -i %i -j ACCEPT
Table = off

[Peer]
PublicKey = ` + keyB + `
PresharedKey = ` + keyC + `
Endpoint = vpn.example.com:51820
AllowedIPs = 10.8.0.0/24, 192.168.1.0/24
PersistentKeepalive = 25

[peer]
publickey=` + keyC + `
allowedips=0.0.0.0/0 # 注释也要能去掉
endpoint = [2001:db8::1]:51820
`

func TestParseFull(t *testing.T) {
	c, err := ParseConfig([]byte(fullConf))
	if err != nil {
		t.Fatal(err)
	}
	if c.PrivateKey != keyA {
		t.Errorf("PrivateKey = %q", c.PrivateKey)
	}
	wantAddr := []netip.Prefix{
		netip.MustParsePrefix("10.8.0.2/24"),
		netip.MustParsePrefix("fd00::2/64"),
		netip.MustParsePrefix("10.9.0.2/32"),
	}
	if len(c.Addresses) != len(wantAddr) {
		t.Fatalf("Addresses = %v", c.Addresses)
	}
	for i := range wantAddr {
		if c.Addresses[i] != wantAddr[i] {
			t.Errorf("Addresses[%d] = %v, want %v", i, c.Addresses[i], wantAddr[i])
		}
	}
	if len(c.DNS) != 1 || c.DNS[0] != netip.MustParseAddr("10.8.0.1") {
		t.Errorf("DNS = %v", c.DNS)
	}
	if len(c.DNSSearch) != 1 || c.DNSSearch[0] != "home.lan" {
		t.Errorf("DNSSearch = %v", c.DNSSearch)
	}
	if c.MTU != 1380 || c.ListenPort != 51821 {
		t.Errorf("MTU=%d ListenPort=%d", c.MTU, c.ListenPort)
	}
	if len(c.Peers) != 2 {
		t.Fatalf("Peers = %d", len(c.Peers))
	}
	p := c.Peers[0]
	if p.PublicKey != keyB || p.PresharedKey != keyC || p.Endpoint != "vpn.example.com:51820" || p.PersistentKeepalive != 25 {
		t.Errorf("peer0 = %+v", p)
	}
	if len(p.AllowedIPs) != 2 || p.AllowedIPs[1] != netip.MustParsePrefix("192.168.1.0/24") {
		t.Errorf("peer0 AllowedIPs = %v", p.AllowedIPs)
	}
	q := c.Peers[1]
	if q.PublicKey != keyC || q.Endpoint != "[2001:db8::1]:51820" || q.PersistentKeepalive != 0 {
		t.Errorf("peer1 = %+v", q)
	}
	if len(q.AllowedIPs) != 1 || q.AllowedIPs[0] != netip.MustParsePrefix("0.0.0.0/0") {
		t.Errorf("peer1 AllowedIPs = %v", q.AllowedIPs)
	}
}

func TestDefaults(t *testing.T) {
	c, err := ParseConfig([]byte("[Interface]\nPrivateKey=" + keyA + "\nAddress=10.0.0.2/32\n[Peer]\nPublicKey=" + keyB + "\nAllowedIPs=10.0.0.0/24\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.MTU != 1420 {
		t.Errorf("默认 MTU 应为 1420，得到 %d", c.MTU)
	}
	if c.ListenPort != 0 || c.Peers[0].Endpoint != "" {
		t.Errorf("ListenPort=%d Endpoint=%q", c.ListenPort, c.Peers[0].Endpoint)
	}
}

// 出错要带行号，方便用户找到配置里哪一行写错
func TestErrors(t *testing.T) {
	cases := []struct {
		name, conf string
		line       int
		contains   string
	}{
		{"缺私钥", "[Interface]\nAddress=10.0.0.2/32\n", 0, "PrivateKey"},
		{"缺地址", "[Interface]\nPrivateKey=" + keyA + "\n", 0, "Address"},
		{"私钥长度不对", "[Interface]\nPrivateKey=AAAA\nAddress=10.0.0.2/32\n", 2, "PrivateKey"},
		{"Peer 缺公钥", "[Interface]\nPrivateKey=" + keyA + "\nAddress=10.0.0.2/32\n[Peer]\nAllowedIPs=10.0.0.0/24\n", 4, "PublicKey"},
		{"地址写错", "[Interface]\nPrivateKey=" + keyA + "\nAddress=10.0.0.300/24\n", 3, "Address"},
		{"未知字段", "[Interface]\nPrivateKey=" + keyA + "\nAddress=10.0.0.2/32\nFoo=bar\n", 4, "Foo"},
		{"未知小节", "[Interface]\nPrivateKey=" + keyA + "\nAddress=10.0.0.2/32\n[Wat]\n", 4, "Wat"},
		{"小节外的字段", "PrivateKey=" + keyA + "\n", 1, "PrivateKey"},
		{"MTU 不是数字", "[Interface]\nPrivateKey=" + keyA + "\nAddress=10.0.0.2/32\nMTU=abc\n", 4, "MTU"},
		{"Endpoint 没端口", "[Interface]\nPrivateKey=" + keyA + "\nAddress=10.0.0.2/32\n[Peer]\nPublicKey=" + keyB + "\nEndpoint=1.2.3.4\n", 6, "Endpoint"},
		{"两个 Interface", "[Interface]\nPrivateKey=" + keyA + "\nAddress=10.0.0.2/32\n[Interface]\n", 4, "Interface"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.conf))
			if err == nil {
				t.Fatal("应该报错")
			}
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("错误类型应为 *ConfigError，得到 %T: %v", err, err)
			}
			if ce.Line != tc.line {
				t.Errorf("Line = %d, want %d (%v)", ce.Line, tc.line, err)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("错误信息 %q 应包含 %q", err.Error(), tc.contains)
			}
		})
	}
}

func TestUAPI(t *testing.T) {
	c, err := ParseConfig([]byte(fullConf))
	if err != nil {
		t.Fatal(err)
	}
	lookups := []string{}
	lookup := func(host string) (netip.Addr, error) {
		lookups = append(lookups, host)
		if host == "vpn.example.com" {
			return netip.MustParseAddr("203.0.113.7"), nil
		}
		return netip.Addr{}, errors.New("no such host")
	}
	got, err := c.UAPI(lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := "private_key=" + keyAHex + "\n" +
		"listen_port=51821\n" +
		"replace_peers=true\n" +
		"public_key=" + keyBHex + "\n" +
		"preshared_key=" + keyCHex + "\n" +
		"endpoint=203.0.113.7:51820\n" +
		"persistent_keepalive_interval=25\n" +
		"replace_allowed_ips=true\n" +
		"allowed_ip=10.8.0.0/24\n" +
		"allowed_ip=192.168.1.0/24\n" +
		"public_key=" + keyCHex + "\n" +
		"endpoint=[2001:db8::1]:51820\n" +
		"replace_allowed_ips=true\n" +
		"allowed_ip=0.0.0.0/0\n"
	if got != want {
		t.Errorf("UAPI 输出不对\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	// 已经是 IP 的端点不应去查
	if len(lookups) != 1 || lookups[0] != "vpn.example.com" {
		t.Errorf("lookups = %v", lookups)
	}
}

func TestUAPILookupFails(t *testing.T) {
	c, err := ParseConfig([]byte(fullConf))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UAPI(func(string) (netip.Addr, error) { return netip.Addr{}, errors.New("boom") })
	if err == nil || !strings.Contains(err.Error(), "vpn.example.com") {
		t.Errorf("解析端点失败时应报错并带上主机名，得到 %v", err)
	}
}

// 私钥不能出现在错误信息和 String() 里
func TestNoKeyLeak(t *testing.T) {
	c, err := ParseConfig([]byte(fullConf))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.String(); strings.Contains(s, keyA) || strings.Contains(s, keyC) {
		t.Errorf("String() 泄漏了私钥/预共享密钥: %s", s)
	}
	_, err = ParseConfig([]byte("[Interface]\nPrivateKey=" + keyA + "x\nAddress=10.0.0.2/32\n"))
	if err == nil || strings.Contains(err.Error(), keyA) {
		t.Errorf("错误信息不能包含密钥原文: %v", err)
	}
}
