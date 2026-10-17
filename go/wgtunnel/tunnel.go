package wgtunnel

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Options 打开隧道时的可选项。
type Options struct {
	Lookup func(host string) (netip.Addr, error) // 解析 Endpoint 主机名；nil 时用 net.DefaultResolver
	Logf   func(format string, args ...any)      // nil 时不输出日志
}

// Tunnel 是一条进程内 WireGuard 隧道（netstack，不碰系统网络栈）。
type Tunnel struct {
	dev        *device.Device
	net        *netstack.Net
	listenPort atomic.Uint64
	logf       func(format string, args ...any)
	hasDNS     bool // 配置里写了 DNS= 的 IP
	closed     atomic.Bool
	closeOnce  sync.Once
}

// Stats 隧道统计快照。
type Stats struct {
	Peers []PeerStats
}

// PeerStats 单个对等体的统计。
type PeerStats struct {
	PublicKey        string // base64
	Endpoint         string
	LastHandshake    time.Time // 从未握手时为零值
	RxBytes, TxBytes uint64
}

var errTunnelClosed = errors.New("wgtunnel: 隧道已关闭")

// Open 建立隧道：netstack tun + wireguard-go 设备，配置来自 cfg.UAPI。
func Open(cfg *Config, opts Options) (*Tunnel, error) {
	addrs := make([]netip.Addr, 0, len(cfg.Addresses))
	for _, p := range cfg.Addresses {
		addrs = append(addrs, p.Addr())
	}
	// 先解析配置（可能要查端点域名），失败时还没创建任何资源
	uapi, err := cfg.UAPI(opts.Lookup)
	if err != nil {
		return nil, err
	}
	tun, tnet, err := netstack.CreateNetTUN(addrs, cfg.DNS, cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("创建 netstack tun: %w", err)
	}

	// 只转发错误日志；verbose 日志（握手、端点等）噪声大，丢弃
	logger := device.NewLogger(device.LogLevelSilent, "wgtunnel")
	if opts.Logf != nil {
		logger.Errorf = opts.Logf
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), logger)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("下发配置: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("启动设备: %w", err)
	}

	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t := &Tunnel{dev: dev, net: tnet, logf: logf, hasDNS: len(cfg.DNS) > 0}
	// listen_port 以设备实际绑定为准（配置写 0 时由系统分配）
	if s, err := dev.IpcGet(); err == nil {
		for _, line := range strings.Split(s, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == "listen_port" {
				if p, err := strconv.Atoi(v); err == nil {
					t.listenPort.Store(uint64(p))
				}
			}
		}
	}
	return t, nil
}

// HasDNS 报告隧道配置是否写了 DNS 服务器；没写时隧道内无法解析域名（不会回落到本机解析）。
func (t *Tunnel) HasDNS() bool { return t.hasDNS }

// ListenPort 返回实际绑定的 UDP 端口。
func (t *Tunnel) ListenPort() int {
	return int(t.listenPort.Load())
}

// DialContext 经隧道拨号；域名由隧道内 DNS 解析。
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if t.closed.Load() {
		return nil, errTunnelClosed
	}
	return t.net.DialContext(ctx, network, address)
}

// Listen 在隧道内监听（仅支持 "tcp"）。
func (t *Tunnel) Listen(network, address string) (net.Listener, error) {
	if network != "tcp" {
		return nil, net.UnknownNetworkError(network)
	}
	if t.closed.Load() {
		return nil, errTunnelClosed
	}
	ap, err := t.resolveAddrPort(context.Background(), address)
	if err != nil {
		return nil, err
	}
	return t.net.ListenTCPAddrPort(ap)
}

// ListenPacket 在隧道内监听 UDP（仅支持 "udp"）。
func (t *Tunnel) ListenPacket(network, address string) (net.PacketConn, error) {
	if network != "udp" {
		return nil, net.UnknownNetworkError(network)
	}
	if t.closed.Load() {
		return nil, errTunnelClosed
	}
	ap, err := t.resolveAddrPort(context.Background(), address)
	if err != nil {
		return nil, err
	}
	return t.net.ListenUDPAddrPort(ap)
}

// resolveAddrPort 拆 host:port；host 不是 IP 时经隧道内 DNS 解析。
func (t *Tunnel) resolveAddrPort(ctx context.Context, address string) (netip.AddrPort, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return netip.AddrPort{}, fmt.Errorf("端口无效: %s", portStr)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(addr, uint16(port)), nil
	}
	addrs, err := t.net.LookupContextHost(ctx, host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("无法解析主机: %s", host)
	}
	addr, err := netip.ParseAddr(addrs[0])
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

// Stats 从设备 IpcGet 文本解析对等体统计。
func (t *Tunnel) Stats() Stats {
	var st Stats
	if t.closed.Load() {
		return st
	}
	s, err := t.dev.IpcGet()
	if err != nil {
		t.logf("wgtunnel: IpcGet 失败: %v", err)
		return st
	}
	var p *PeerStats
	var hsSec, hsNsec int64
	flush := func() {
		if p == nil {
			return
		}
		if hsSec != 0 || hsNsec != 0 { // 全 0 表示从未握手
			p.LastHandshake = time.Unix(hsSec, hsNsec)
		}
		st.Peers = append(st.Peers, *p)
	}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "public_key":
			flush()
			p = &PeerStats{PublicKey: hexToBase64(v)}
			hsSec, hsNsec = 0, 0
		case "endpoint":
			if p != nil {
				p.Endpoint = v
			}
		case "last_handshake_time_sec":
			hsSec, _ = strconv.ParseInt(v, 10, 64)
		case "last_handshake_time_nsec":
			hsNsec, _ = strconv.ParseInt(v, 10, 64)
		case "rx_bytes":
			if p != nil {
				p.RxBytes, _ = strconv.ParseUint(v, 10, 64)
			}
		case "tx_bytes":
			if p != nil {
				p.TxBytes, _ = strconv.ParseUint(v, 10, 64)
			}
		}
	}
	flush()
	return st
}

// Close 关闭设备；可重复调用，之后的 Dial 一律报 errTunnelClosed。
func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.dev.Close() // 该版本 dev.Close() 无返回值
	})
	return nil
}

func hexToBase64(h string) string {
	b, err := hex.DecodeString(strings.TrimSpace(h))
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}
