package wgtunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/net/dns/dnsmessage"
)

func genKey(t *testing.T) (priv, pub string) {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	p, err := curve25519.X25519(k, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k), base64.StdEncoding.EncodeToString(p)
}

// pair 在本机回环上起两条互为对端的隧道：a 是"服务器"（固定监听端口由系统分配），b 连向 a
func pair(t *testing.T, aAddr, bAddr string, dnsOnB string) (a, b *Tunnel) {
	t.Helper()
	aPriv, aPub := genKey(t)
	bPriv, bPub := genKey(t)
	confA := fmt.Sprintf("[Interface]\nPrivateKey=%s\nAddress=%s/32\n[Peer]\nPublicKey=%s\nAllowedIPs=%s/32\n",
		aPriv, aAddr, bPub, bAddr)
	ca, err := ParseConfig([]byte(confA))
	if err != nil {
		t.Fatal(err)
	}
	a, err = Open(ca, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if a.ListenPort() == 0 {
		t.Fatal("ListenPort() 应返回实际绑定的端口")
	}
	dnsLine := ""
	if dnsOnB != "" {
		dnsLine = "DNS=" + dnsOnB + "\n"
	}
	confB := fmt.Sprintf("[Interface]\nPrivateKey=%s\nAddress=%s/32\n%s[Peer]\nPublicKey=%s\nAllowedIPs=%s/32\nEndpoint=127.0.0.1:%d\nPersistentKeepalive=1\n",
		bPriv, bAddr, dnsLine, aPub, aAddr, a.ListenPort())
	cb, err := ParseConfig([]byte(confB))
	if err != nil {
		t.Fatal(err)
	}
	b, err = Open(cb, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return a, b
}

// echoServer 在隧道内监听，收到一行回一行，前面加上标记
func echoServer(t *testing.T, tun *Tunnel, addr, tag string) {
	t.Helper()
	ln, err := tun.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					io.WriteString(c, tag+":"+line)
				}
			}(c)
		}
	}()
}

func roundTrip(t *testing.T, tun *Tunnel, addr, msg string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := tun.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("经隧道连 %s 失败: %v", addr, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, msg+"\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("读回应失败: %v", err)
	}
	return strings.TrimSpace(line)
}

func TestTunnelTCP(t *testing.T) {
	a, b := pair(t, "10.77.0.1", "10.77.0.2", "")
	echoServer(t, a, "10.77.0.1:8080", "A")
	if got := roundTrip(t, b, "10.77.0.1:8080", "hello"); got != "A:hello" {
		t.Errorf("got %q", got)
	}
	// 反方向也能通：a 连 b
	echoServer(t, b, "10.77.0.2:9090", "B")
	if got := roundTrip(t, a, "10.77.0.2:9090", "back"); got != "B:back" {
		t.Errorf("got %q", got)
	}
}

// 同一进程里两组隧道用完全相同的网段，互不干扰
func TestOverlappingTunnels(t *testing.T) {
	a1, b1 := pair(t, "10.88.0.1", "10.88.0.2", "")
	a2, b2 := pair(t, "10.88.0.1", "10.88.0.2", "")
	echoServer(t, a1, "10.88.0.1:7000", "one")
	echoServer(t, a2, "10.88.0.1:7000", "two")
	if got := roundTrip(t, b1, "10.88.0.1:7000", "x"); got != "one:x" {
		t.Errorf("隧道1 got %q", got)
	}
	if got := roundTrip(t, b2, "10.88.0.1:7000", "y"); got != "two:y" {
		t.Errorf("隧道2 got %q", got)
	}
}

// 域名通过配置里的 DNS 在隧道内解析
func TestTunnelDNS(t *testing.T) {
	a, b := pair(t, "10.66.0.1", "10.66.0.2", "10.66.0.1")
	echoServer(t, a, "10.66.0.1:8080", "A")
	pc, err := a.ListenPacket("udp", "10.66.0.1:53")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go serveDNS(pc, "svc.home.lan.", [4]byte{10, 66, 0, 1})
	if got := roundTrip(t, b, "svc.home.lan:8080", "dns"); got != "A:dns" {
		t.Errorf("got %q", got)
	}
}

func serveDNS(pc net.PacketConn, name string, ip [4]byte) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		bld := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
		bld.EnableCompression()
		bld.StartQuestions()
		bld.Question(q)
		bld.StartAnswers()
		if q.Name.String() == name && q.Type == dnsmessage.TypeA {
			bld.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: ip})
		}
		out, err := bld.Finish()
		if err != nil {
			continue
		}
		pc.WriteTo(out, from)
	}
}

func TestStatsAndClose(t *testing.T) {
	a, b := pair(t, "10.55.0.1", "10.55.0.2", "")
	echoServer(t, a, "10.55.0.1:8080", "A")
	roundTrip(t, b, "10.55.0.1:8080", "ping")
	st := b.Stats()
	if len(st.Peers) != 1 {
		t.Fatalf("Peers = %+v", st.Peers)
	}
	p := st.Peers[0]
	if p.LastHandshake.IsZero() || time.Since(p.LastHandshake) > time.Minute {
		t.Errorf("LastHandshake = %v", p.LastHandshake)
	}
	if p.RxBytes == 0 || p.TxBytes == 0 {
		t.Errorf("Rx=%d Tx=%d", p.RxBytes, p.TxBytes)
	}
	if p.PublicKey == "" {
		t.Error("PublicKey 应填上（base64）")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("重复 Close 不应报错: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := b.DialContext(ctx, "tcp", "10.55.0.1:8080"); err == nil {
		t.Error("关闭后 Dial 应失败")
	}
}

// 连不上的地址要在 ctx 超时内返回，不能卡住
func TestDialTimeout(t *testing.T) {
	_, b := pair(t, "10.44.0.1", "10.44.0.2", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := b.DialContext(ctx, "tcp", netip.MustParseAddrPort("10.44.0.1:1").String())
	if err == nil {
		t.Fatal("应失败")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("等了 %v 才返回", time.Since(start))
	}
}
