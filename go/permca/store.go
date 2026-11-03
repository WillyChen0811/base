package permca

// CA 的磁盘管理：加载或新建 CA 目录、给各侧客户端备好证书包、读吊销表。
// 从 wgtund/ca.go 的 prepareMsgCA 里挪出来的通用部分（2026-10-26 拆协助/消息/审批时，
// wgtund 的接线删了，这部分留在 permca）：目录路径、各侧的名字与权限都由调用方传，这里不写死任何路径。
//
// 目录布局（dir 由调用方定）：
//
//	dir/ca.key、dir/ca.pub         CA 密钥（LoadOrCreateCA）
//	dir/clients/<名字>.json        各侧客户端包（WriteClientBundle 的格式）
//	dir/revoked.json               吊销表：JSON 序列号数组，可以不存在
//
// 不在这里的：启动时自动建、把某一侧的包复制到固定位置（如 ~/.xxyi/client.json）、HTTP 路由——那些是接线，不是库。

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DefaultClientValidFor 是客户端证书的惯用有效期：十年。
// 自用场景，吊销靠删/换 revoked.json，不搞短期证书轮换。
const DefaultClientValidFor = 3650 * 24 * time.Hour

// LogFunc 接收告警和说明文字（format 不带换行）。传 nil 的地方一律写 stderr，前缀 "permca: "——
// 吊销表读不了这类事不能悄悄吞掉。
type LogFunc func(format string, args ...any)

func (l LogFunc) printf(format string, args ...any) {
	if l != nil {
		l(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "permca: "+format+"\n", args...)
}

// ClientBundlePath 是 PrepareCA 给名字为 name 的客户端放包的位置：dir/clients/<name>.json。
func ClientBundlePath(dir, name string) string {
	return filepath.Join(dir, "clients", name+".json")
}

// RevokedPath 是 PrepareCA 读吊销表的位置：dir/revoked.json。
func RevokedPath(dir string) string {
	return filepath.Join(dir, "revoked.json")
}

// PrepareCA 备好一个 CA 目录：
//  1. 加载/创建 dir 里的 CA（密钥只在首次生成）；
//  2. clients 里每个名字在 dir/clients/<名字>.json 备好客户端包（见 EnsureClientBundle），权限用 clients 里给的；
//  3. 返回 CA 和按文件修改时间缓存的吊销查询函数（读 dir/revoked.json，见 NewRevokedTable），
//     可直接交给 NewVerifier。
//
// 全程幂等：重复调用不重新生成密钥、不覆盖仍然有效的证书。客户端名要符合证书名规则（小写字母/数字/连字符，1-32 位），
// 它同时是文件名，所以不合规的名字在动磁盘之前就报错。
func PrepareCA(dir string, clients map[string]Perm, validFor time.Duration, logf LogFunc) (*CA, func(serial string) bool, error) {
	names := make([]string, 0, len(clients))
	for name := range clients {
		if !certNamePattern.MatchString(name) {
			return nil, nil, fmt.Errorf("客户端名 %q 不合法（只允许小写字母/数字/连字符，1-32 位）", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	ca, _, err := LoadOrCreateCA(dir)
	if err != nil {
		return nil, nil, err
	}
	clientsDir := filepath.Join(dir, "clients")
	if err := os.MkdirAll(clientsDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("创建 %s: %w", clientsDir, err)
	}
	for _, name := range names {
		if err := EnsureClientBundle(ClientBundlePath(dir, name), ca, name, clients[name], validFor, logf); err != nil {
			return nil, nil, err
		}
	}
	return ca, NewRevokedTable(RevokedPath(dir), logf).Revoked, nil
}

// EnsureClientBundle 保证 path 处有一个可用的客户端包（名字 name、权限 perm、有效期 validFor 用于签发）：
//   - 包已存在且证书用 ca 校验通过 → 原样保留（证书指纹不能因重启而变）；
//   - 校验不过（签名域换过前缀、已过期等）→ 保留原客户端密钥对重新签发，只换证书，并通过 logf 说明；
//   - 包读不了/读不到 → 生成新密钥对签发（旧包烂在那儿只会让该侧请求永远验不过）。
//
// 这里只看"证书还是不是当前 CA 签的、还在不在有效期"；吊销由请求时校验器的 revoked 回调把关，
// 不在此处消费（重签发会换新序列号，吊销旧序列号本来就该让旧证书请求被拒，不该在这里触发重签复活）。
func EnsureClientBundle(path string, ca *CA, name string, perm Perm, validFor time.Duration, logf LogFunc) error {
	_, statErr := os.Stat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("读取 %s: %w", path, statErr)
	}
	if statErr == nil {
		cert, priv, _, lerr := LoadClientBundle(path)
		if lerr == nil {
			verr := VerifyCert(ca.Public(), cert, time.Now(), nil)
			if verr == nil {
				return nil
			}
			logf.printf("客户端 %s 的证书用当前 CA 校验不通过（%v），保留原密钥重新签发 %s", name, verr, path)
			cert2, err := ca.Issue(name, cert.PublicKey, perm, validFor, time.Now())
			if err != nil {
				return fmt.Errorf("重签 %s 证书失败: %w", name, err)
			}
			if err := WriteClientBundle(path, cert2, priv, ca.Public()); err != nil {
				return fmt.Errorf("写 %s: %w", path, err)
			}
			return nil
		}
		// 旧包读不了：救不出原密钥对，只能换新（文案让用户能对上现象）。
		logf.printf("客户端包 %s 读不了（%v），换新密钥重新签发", path, lerr)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("生成 %s 客户端密钥失败: %w", name, err)
	}
	cert, err := ca.Issue(name, pub, perm, validFor, time.Now())
	if err != nil {
		return fmt.Errorf("签发 %s 证书失败: %w", name, err)
	}
	if err := WriteClientBundle(path, cert, priv, ca.Public()); err != nil {
		return fmt.Errorf("写 %s: %w", path, err)
	}
	return nil
}

// RevokedTable 读一个吊销表文件（JSON 序列号数组）：
// 按文件修改时间缓存，mtime 没变不重读；文件不存在 = 空表（记为最近一次成功状态）；
// Stat/读/解析失败（不存在除外）：沿用上次成功读到的表；从没成功读到过时 fail-closed
// ——所有序列号都当已吊销，并通过 logf 记录。吊销表读不成却"全放进来"的后果
// 远大于暂时按住没吊销的证书：宁可误拦，不可误放。并发安全。
type RevokedTable struct {
	path     string
	logf     LogFunc
	mu       sync.Mutex
	mtime    time.Time
	haveAll  bool            // 是否成功确定过表内容（读到好 JSON，或确认不存在=空表）
	serials  map[string]bool // 上次成功读到的表
	lastWarn string          // 上次打过的失败文本；同一原因只打一次，读成功后清零
}

// NewRevokedTable 建一个读 path 的吊销表；logf 为 nil 时告警写 stderr。
func NewRevokedTable(path string, logf LogFunc) *RevokedTable {
	return &RevokedTable{path: path, logf: logf}
}

// Revoked 报告序列号是否已吊销；签名正好是 NewVerifier 要的 revoked 回调。
func (rt *RevokedTable) Revoked(serial string) bool {
	st, err := os.Stat(rt.path)
	if err != nil {
		if os.IsNotExist(err) {
			// 不存在 = 空表：这也是一次成功的确定，之后的读失败沿用空表。
			rt.mu.Lock()
			rt.serials = nil
			rt.mtime = time.Time{}
			rt.haveAll = true
			rt.lastWarn = "" // 恢复正常：之后的失败重新提示
			rt.mu.Unlock()
			return false
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.fallbackLocked("Stat", err, serial)
	}
	rt.mu.Lock()
	if rt.haveAll && rt.mtime.Equal(st.ModTime()) {
		v := rt.serials[serial]
		rt.mu.Unlock()
		return v
	}
	rt.mu.Unlock()
	data, err := os.ReadFile(rt.path)
	if err != nil {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.fallbackLocked("读取", err, serial)
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.fallbackLocked("解析", err, serial)
	}
	serials := make(map[string]bool, len(list))
	for _, s := range list {
		serials[s] = true
	}
	rt.mu.Lock()
	rt.serials = serials
	rt.mtime = st.ModTime()
	rt.haveAll = true
	rt.lastWarn = "" // 恢复正常：之后的失败重新提示
	rt.mu.Unlock()
	return serials[serial]
}

// fallbackLocked 读失败兜底（调用方须持锁）：有上次成功读到的表就沿用；
// 从没成功读到过就 fail-closed（一律当已吊销）。两种都要记录，
// 但同一原因（同一句话）只记一次——Revoked 是每次校验都会走的热路径，
// 表一直读不了会把日志刷满；换个失败原因（文本不同）会重新记。
// 成功读到表后 lastWarn 清零，之后再次失败哪怕同一原因也会再记一次。
func (rt *RevokedTable) fallbackLocked(what string, err error, serial string) bool {
	if rt.haveAll {
		msg := fmt.Sprintf("吊销表 %s %s失败（%v），沿用上次成功读到的表", rt.path, what, err)
		if msg != rt.lastWarn {
			rt.logf.printf("%s", msg)
			rt.lastWarn = msg
		}
		return rt.serials[serial]
	}
	msg := fmt.Sprintf("吊销表 %s %s失败（%v），此前从未成功读到，按全部吊销处理", rt.path, what, err)
	if msg != rt.lastWarn {
		rt.logf.printf("%s", msg)
		rt.lastWarn = msg
	}
	return true
}

// DenyAllVerifier 返回一个永远拒绝的校验器，给"CA 备不出来"时顶上用：
// 拿一个跟磁盘 CA 无关的随机新公钥，任何真实证书都验不过，
// 接口保持"能连上但全拒绝"，比整个服务起不来好排查。
func DenyAllVerifier() *Verifier {
	if ca, err := NewCA(); err == nil {
		return NewVerifier(ca.Public(), nil)
	}
	// 连随机密钥都生成不出来（不可能走到）：用全零公钥顶上，同样什么都验不过。
	return NewVerifier(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)), nil)
}
