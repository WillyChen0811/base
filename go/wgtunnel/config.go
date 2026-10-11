package wgtunnel

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Config 表示一个 WireGuard（wg-quick 格式）配置。
type Config struct {
	PrivateKey string         // base64 原文
	Addresses  []netip.Prefix // 无前缀长度的地址：IPv4 /32，IPv6 /128
	DNS        []netip.Addr   // DNS 一栏里能解析成 IP 的
	DNSSearch  []string       // DNS 一栏里不是 IP 的（搜索域）
	MTU        int            // 默认 1420
	ListenPort int
	Peers      []Peer
}

// Peer 表示一个对等体。
type Peer struct {
	PublicKey, PresharedKey string // base64 原文
	Endpoint                string // 原样保存 host:port
	AllowedIPs              []netip.Prefix
	PersistentKeepalive     int
}

// ConfigError 配置解析错误，Line=0 表示不针对某一行。
type ConfigError struct {
	Line int
	Msg  string
}

func (e *ConfigError) Error() string {
	if e.Line == 0 {
		return e.Msg
	}
	return fmt.Sprintf("第 %d 行: %s", e.Line, e.Msg)
}

// 允许静默忽略的 wg-quick 字段。
var ignoreFields = map[string]struct{}{
	"preup": {}, "postup": {}, "predown": {}, "postdown": {},
	"table": {}, "saveconfig": {}, "fwmark": {},
}

// peerRaw 解析过程中暂存的 peer 数据。
type peerRaw struct {
	line                    int // [Peer] 所在行号（用于"缺 PublicKey"报错）
	endpointLine            int // Endpoint 字段行号
	publicKey, presharedKey string
	endpoint                string
	allowedIPs              []netip.Prefix
	persistentKeepalive     int
}

// unknownKey 生成"未知字段"报错；字段名可能是误粘进来的密钥，
// 只有纯字母且不超过 30 个字符时才回显。
func unknownKey(line int, key string) *ConfigError {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 30 {
		return &ConfigError{Line: line, Msg: "无法识别的行"}
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return &ConfigError{Line: line, Msg: "无法识别的行"}
		}
	}
	return &ConfigError{Line: line, Msg: "未知字段 " + key}
}

// ParseConfig 解析 wg-quick 格式的配置文本。
func ParseConfig(data []byte) (*Config, error) {
	// 去掉开头的 UTF-8 BOM（Windows 记事本可能添加）
	data = removeBOM(data)

	cfg := &Config{MTU: 1420}
	var peerBufs []peerRaw

	section := ""
	ifaceCount := 0

	lines := strings.Split(string(data), "\n")
	for i, raw := range lines {
		lineNum := i + 1
		line := raw

		// 去除行内注释
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 小节头
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && len(line) >= 3 {
			name := strings.ToLower(line[1 : len(line)-1])
			switch name {
			case "interface":
				if section != "" {
					if ifaceCount > 0 {
						return nil, &ConfigError{Line: lineNum, Msg: "重复的 [Interface] 小节"}
					}
					if len(peerBufs) > 0 {
						return nil, &ConfigError{Line: lineNum, Msg: "[Interface] 必须写在最前面"}
					}
				}
				section = "interface"
				ifaceCount++
			case "peer":
				section = "peer"
				peerBufs = append(peerBufs, peerRaw{line: lineNum})
			default:
				return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("未知小节 [%s]", line[1:len(line)-1])}
			}
			continue
		}

		// 小节外的字段
		if section == "" {
			if eqIdx := strings.Index(line, "="); eqIdx >= 0 {
				return nil, unknownKey(lineNum, line[:eqIdx])
			}
			return nil, &ConfigError{Line: lineNum, Msg: "无法识别的行"}
		}

		// 分割 key=value；缺 "=" 的行报错
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			e := unknownKey(lineNum, line)
			if e.Msg != "无法识别的行" {
				e.Msg = "缺少等号: " + strings.TrimSpace(line)
			}
			return nil, e
		}
		key := strings.TrimSpace(strings.ToLower(line[:eqIdx]))
		val := strings.TrimSpace(line[eqIdx+1:])

		if section == "interface" {
			if _, ok := ignoreFields[key]; ok {
				continue
			}
			switch key {
			case "privatekey":
				if err := validateKey(val); err != nil {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("PrivateKey: %s", err.Error())}
				}
				cfg.PrivateKey = val
			case "address":
				if err := parsePrefixLine(val, &cfg.Addresses); err != nil {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("Address: %s", err.Error())}
				}
			case "dns":
				parseDNSItems(val, &cfg.DNS, &cfg.DNSSearch)
			case "mtu":
				v, err := strconv.Atoi(val)
				if err != nil {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("MTU 不是数字: %s", val)}
				}
				if v < 576 || v > 65535 {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("MTU 范围越界: %d", v)}
				}
				cfg.MTU = v
			case "listenport":
				v, err := strconv.Atoi(val)
				if err != nil {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("ListenPort 不是数字: %s", val)}
				}
				if v < 0 || v > 65535 {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("ListenPort 范围越界: %d", v)}
				}
				cfg.ListenPort = v
			default:
				return nil, unknownKey(lineNum, line[:eqIdx])
			}
		} else {
			pr := &peerBufs[len(peerBufs)-1]
			switch key {
			case "publickey":
				if val == "" {
					return nil, &ConfigError{Line: lineNum, Msg: "PublicKey 为空"}
				}
				if err := validateKey(val); err != nil {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("PublicKey: %s", err.Error())}
				}
				pr.publicKey = val
			case "presharedkey":
				if err := validateKey(val); err != nil {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("PresharedKey: %s", err.Error())}
				}
				pr.presharedKey = val
			case "endpoint":
				pr.endpoint = val
				pr.endpointLine = lineNum
			case "allowedips":
				if err := parsePrefixLine(val, &pr.allowedIPs); err != nil {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("AllowedIPs: %s", err.Error())}
				}
			case "persistentkeepalive":
				v, err := strconv.Atoi(val)
				if err != nil {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("PersistentKeepalive 不是数字: %s", val)}
				}
				if v < 0 || v > 65535 {
					return nil, &ConfigError{Line: lineNum, Msg: fmt.Sprintf("PersistentKeepalive 范围越界: %d", v)}
				}
				pr.persistentKeepalive = v
			default:
				return nil, unknownKey(lineNum, line[:eqIdx])
			}
		}
	}

	// 为缺少显式前缀的地址自动补 /32 或 /128
	for i := range cfg.Addresses {
		p := cfg.Addresses[i]
		if p.Bits() == 0 && !p.Addr().IsUnspecified() {
			bits := 128
			if p.Addr().Is4() {
				bits = 32
			}
			cfg.Addresses[i] = netip.PrefixFrom(p.Addr(), bits)
		}
	}

	// 构建 Peers
	for _, pr := range peerBufs {
		if pr.publicKey == "" {
			return nil, &ConfigError{Line: pr.line, Msg: "Peer 缺少 PublicKey"}
		}
		if pr.endpoint != "" {
			if err := validateEndpoint(pr.endpoint); err != nil {
				return nil, &ConfigError{Line: pr.endpointLine, Msg: fmt.Sprintf("Endpoint: %s", err.Error())}
			}
		}
		cfg.Peers = append(cfg.Peers, Peer{
			PublicKey:           pr.publicKey,
			PresharedKey:        pr.presharedKey,
			Endpoint:            pr.endpoint,
			AllowedIPs:          pr.allowedIPs,
			PersistentKeepalive: pr.persistentKeepalive,
		})
	}

	// 必填项校验
	if cfg.PrivateKey == "" {
		return nil, &ConfigError{Line: 0, Msg: "缺少 PrivateKey"}
	}
	if len(cfg.Addresses) == 0 {
		return nil, &ConfigError{Line: 0, Msg: "缺少 Address"}
	}

	return cfg, nil
}

// removeBOM 去掉开头的 UTF-8 BOM 字节。
func removeBOM(data []byte) []byte {
	const bom = "\xEF\xBB\xBF"
	if strings.HasPrefix(string(data), bom) {
		return data[len(bom):]
	}
	return data
}

// validateEndpoint 校验 Endpoint：必须有 host:port，端口必须是 1..65535。
func validateEndpoint(ep string) error {
	colon := strings.LastIndex(ep, ":")
	if colon < 0 || colon == len(ep)-1 {
		return fmt.Errorf("格式错误: 缺少端口 %s", ep)
	}
	// 支持 [IPv6]:port
	if ep[0] == '[' {
		closeBracket := strings.Index(ep, "]")
		if closeBracket < 0 || closeBracket+1 != colon {
			return fmt.Errorf("格式错误: %s", ep)
		}
	}
	// 非 IPv6 格式：只有一个冒号（host:port），无需额外检查

	portStr := ep[colon+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("端口不是数字: %s", portStr)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("端口超出范围: %d", port)
	}
	host := ep[:colon]
	if ep[0] == '[' {
		host = host[1:]
	}
	if host == "" {
		return fmt.Errorf("主机名为空: %s", ep)
	}
	return nil
}

// parsePrefixLine 解析逗号分隔的前缀列表（Address 或 AllowedIPs 共用）。
// 没有 '/' 的裸 IP 按 IPv4 /32、IPv6 /128 处理。
func parsePrefixLine(val string, dst *[]netip.Prefix) error {
	for _, item := range strings.Split(val, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		pfx, err := parsePrefix(item)
		if err != nil {
			return err
		}
		*dst = append(*dst, pfx)
	}
	return nil
}

// parsePrefix 解析单个前缀字符串。显式写 '/' 的走 netip.ParsePrefix；
// 裸 IP 按 IPv4 /32、IPv6 /128 一步到位。
func parsePrefix(item string) (netip.Prefix, error) {
	if slashIdx := strings.Index(item, "/"); slashIdx >= 0 {
		return netip.ParsePrefix(item)
	}
	addr, err := netip.ParseAddr(item)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("地址格式错误: %s", item)
	}
	bits := 128
	if addr.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(addr, bits), nil
}

// parseDNSItems 解析 DNS 项：IP 入 dns，非 IP 入 search，空项跳过。
func parseDNSItems(val string, dns *[]netip.Addr, search *[]string) {
	for _, item := range strings.Split(val, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			*search = append(*search, item)
		} else {
			*dns = append(*dns, addr)
		}
	}
}

// validateKey 校验 base64 密钥解码后正好 32 字节，不暴露密钥原文。
func validateKey(raw string) error {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return fmt.Errorf("base64 解码失败")
	}
	if len(decoded) != 32 {
		return fmt.Errorf("密钥长度不是 32 字节（base64 解码后 %d 字节）", len(decoded))
	}
	return nil
}

// toHex 将 base64 密钥转为小写十六进制。出错返回空串+error。
func toHex(base64Key string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败")
	}
	if len(decoded) != 32 {
		return "", fmt.Errorf("密钥长度不是 32 字节")
	}
	return hex.EncodeToString(decoded), nil
}

// UAPI 生成 wireguard-go IpcSet 格式的文本。
func (c *Config) UAPI(lookup func(host string) (netip.Addr, error)) (string, error) {
	// lookup 为 nil 时使用系统 DNS
	if lookup == nil {
		lookup = func(host string) (netip.Addr, error) {
			addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", host)
			if err != nil {
				return netip.Addr{}, err
			}
			if len(addrs) == 0 {
				return netip.Addr{}, fmt.Errorf("no such host: %s", host)
			}
			a := addrs[0].Unmap()
			if !a.IsValid() {
				return netip.Addr{}, fmt.Errorf("不支持的地址类型: %s", host)
			}
			return a, nil
		}
	}

	var sb strings.Builder

	pkHex, err := toHex(c.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("PrivateKey 转换失败: %s", err.Error())
	}
	sb.WriteString("private_key=" + pkHex + "\n")

	if c.ListenPort != 0 {
		sb.WriteString(fmt.Sprintf("listen_port=%d\n", c.ListenPort))
	}
	sb.WriteString("replace_peers=true\n")

	for _, p := range c.Peers {
		pkHex, err := toHex(p.PublicKey)
		if err != nil {
			return "", fmt.Errorf("PublicKey 转换失败: %s", err.Error())
		}
		sb.WriteString("public_key=" + pkHex + "\n")

		if p.PresharedKey != "" {
			pskHex, err := toHex(p.PresharedKey)
			if err != nil {
				return "", fmt.Errorf("PresharedKey 转换失败: %s", err.Error())
			}
			sb.WriteString("preshared_key=" + pskHex + "\n")
		}

		// 服务器端常见没有 Endpoint，这时不输出
		if p.Endpoint != "" {
			if err := validateEndpoint(p.Endpoint); err != nil {
				return "", fmt.Errorf("Endpoint %s", err.Error())
			}
			host, portStr, _ := net.SplitHostPort(p.Endpoint)
			endpoint := p.Endpoint
			if _, err := netip.ParseAddr(host); err != nil {
				ip, err := lookup(host)
				if err != nil {
					return "", fmt.Errorf("解析端点主机名 %s 失败: %s", host, err.Error())
				}
				port, _ := strconv.Atoi(portStr)
				endpoint = netip.AddrPortFrom(ip, uint16(port)).String()
			}
			sb.WriteString("endpoint=" + endpoint + "\n")
		}

		if p.PersistentKeepalive != 0 {
			sb.WriteString(fmt.Sprintf("persistent_keepalive_interval=%d\n", p.PersistentKeepalive))
		}

		sb.WriteString("replace_allowed_ips=true\n")
		for _, aip := range p.AllowedIPs {
			sb.WriteString("allowed_ip=" + aip.String() + "\n")
		}
	}

	return sb.String(), nil
}

// String 返回配置的日志摘要，不含私钥/预共享密钥。
func (c *Config) String() string {
	var sb strings.Builder
	sb.WriteString("Config{PrivateKey=<hidden>")
	if len(c.Addresses) > 0 {
		fmt.Fprintf(&sb, " Addresses=%v", c.Addresses)
	}
	if len(c.DNS) > 0 {
		fmt.Fprintf(&sb, " DNS=%v", c.DNS)
	}
	if len(c.DNSSearch) > 0 {
		fmt.Fprintf(&sb, " DNSSearch=%v", c.DNSSearch)
	}
	fmt.Fprintf(&sb, " MTU=%d", c.MTU)
	if c.ListenPort != 0 {
		fmt.Fprintf(&sb, " ListenPort=%d", c.ListenPort)
	}
	fmt.Fprintf(&sb, " Peers=%d", len(c.Peers))
	sb.WriteString("}")
	return sb.String()
}
