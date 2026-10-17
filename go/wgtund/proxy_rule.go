// proxy_rule.go：代理口的匹配规则——forwards 的 target 每条是 IP、CIDR 或域名，可选带 :端口。
// 命中多条取最具体的：精确地址 > 较长前缀 > 较短前缀，同一地址带端口的优先；域名只与域名目的匹配（精确，不区分大小写）。
package wgtund

import (
	"context"

	"fmt"
	"github.com/WillyChen0811/base/go/sshterm"
	"net"
	"strconv"
	"strings"
)

type proxyRule struct {
	fwd    *forwardSvc
	ipnet  *net.IPNet // IP/CIDR 规则（IP 视为 /32）；域名规则为 nil
	domain string
	port   int // 0 表示任意端口
	key    string
}

// parseRule 解析一条规则；只支持 IPv4（IPv6 目的不会命中规则，走 proxy_default）。
func parseRule(s string) (proxyRule, error) {
	r := proxyRule{}
	host := s
	switch strings.Count(s, ":") {
	case 0:
	case 1:
		i := strings.Index(s, ":")
		host = s[:i]
		n, err := strconv.Atoi(s[i+1:])
		if err != nil || n < 1 || n > 65535 {
			return r, fmt.Errorf("规则 %q 的端口无效", s)
		}
		r.port = n
	default:
		return r, fmt.Errorf("规则 %q 不支持 IPv6", s)
	}
	switch {
	case host == "":
		return r, fmt.Errorf("规则 %q 缺少地址", s)
	case strings.Contains(host, "/"):
		_, n, err := net.ParseCIDR(host)
		if err != nil || n.IP.To4() == nil {
			return r, fmt.Errorf("规则 %q 不是有效的 IPv4 CIDR", s)
		}
		r.ipnet = n
	case net.ParseIP(host) != nil:
		ip := net.ParseIP(host).To4()
		if ip == nil {
			return r, fmt.Errorf("规则 %q 不支持 IPv6", s)
		}
		r.ipnet = &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}
	default:
		if strings.ContainsAny(host, " /\t") {
			return r, fmt.Errorf("规则 %q 不是有效的域名", s)
		}
		r.domain = strings.ToLower(host)
	}
	if r.ipnet != nil {
		r.key = r.ipnet.String()
	} else {
		r.key = r.domain
	}
	r.key += "|" + strconv.Itoa(r.port)
	return r, nil
}

// score 返回规则对目的的具体程度；不匹配返回 -1。
func (r proxyRule) score(host string, port int) int {
	if r.port != 0 && r.port != port {
		return -1
	}
	bonus := 0
	if r.port != 0 {
		bonus = 1
	}
	if r.ipnet == nil {
		if r.domain == strings.ToLower(host) {
			return 64 + bonus
		}
		return -1
	}
	ip := net.ParseIP(host)
	if ip == nil || !r.ipnet.Contains(ip) {
		return -1
	}
	ones, _ := r.ipnet.Mask.Size()
	return ones*2 + bonus
}

// pick 选出目的 主机:端口 该走的转发：规则里最具体的一条，没命中则 proxy_default。
func (p *proxySvc) pick(dest string) *forwardSvc {
	host, ps, _ := net.SplitHostPort(dest)
	port, _ := strconv.Atoi(ps)
	best, bestScore := p.dflt, -1
	for _, r := range p.rules {
		if sc := r.score(host, port); sc > bestScore {
			best, bestScore = r.fwd, sc
		}
	}
	return best
}

// validateProxy 校验 proxy 口（只许回环）、各转发的 target 规则互不重复、proxy_default 有效。
func validateProxy(conf *fileConf) error {
	seen := map[string]string{}
	names := map[string]bool{}
	for _, fw := range conf.Forwards {
		names[fw.Name] = true
		for _, t := range fw.Target {
			r, err := parseRule(t)
			if err != nil {
				return fmt.Errorf("hosts.json: 转发 %q 的 target: %w", fw.Name, err)
			}
			if other, dup := seen[r.key]; dup {
				return fmt.Errorf("hosts.json: 转发 %q 与 %q 的 target %q 重复", fw.Name, other, t)
			}
			seen[r.key] = fw.Name
		}
	}
	if conf.ProxyDefault != "" && conf.ProxyDefault != "direct" && !names[conf.ProxyDefault] {
		return fmt.Errorf("hosts.json: proxy_default %q 既不是 direct 也不是已有的转发名", conf.ProxyDefault)
	}
	if conf.Proxy == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(conf.Proxy)
	if err != nil {
		return fmt.Errorf("hosts.json: proxy 地址 %q 要写成 主机:端口", conf.Proxy)
	}
	if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") {
		return fmt.Errorf("hosts.json: proxy 地址 %q 必须是回环地址（127.0.0.1 或 localhost）", conf.Proxy)
	}
	return nil
}

// dnsGuard 包住隧道线路的拨号器：目的是域名而隧道没配 DNS 时直接失败，
// 不把域名交下去（也就绝不会有本机解析）；有 DNS 时由隧道内 DNS 解析。
type dnsGuard struct {
	name   string
	d      sshterm.Dialer
	hasDNS func() bool
}

func (g *dnsGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if host, _, err := net.SplitHostPort(addr); err == nil && net.ParseIP(host) == nil && !g.hasDNS() {
		return nil, fmt.Errorf("隧道 %s 没配 DNS，无法解析域名 %s", g.name, host)
	}
	return g.d.DialContext(ctx, network, addr)
}
