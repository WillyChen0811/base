package wgtund

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/WillyChen0811/base/go/xxyica"
)

// certValidFor 客户端证书的有效期：十年。
// 这是本机自用协助通道，吊销靠删/换 revoked.json，不搞短期证书轮换。
const certValidFor = 3650 * 24 * time.Hour

// clientSides 是需要发证书的对讲两侧（客户端包名即 xxyica.DefaultPerm 的 side）。
var clientSides = []string{"mac", "ubuntu"}

// xxyiDir 返回 ~/.xxyi；HOME 取不到时报错，不落相对路径。
func xxyiDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("取不到主目录: %w", err)
	}
	return filepath.Join(home, ".xxyi"), nil
}

// prepareMsgCA 备好消息接力的 CA：
//  1. 加载/创建 ~/.xxyi/ca（密钥只在首次生成）；
//  2. clients/ 下 mac/ubuntu 客户端包不存在才签发（十年期 + DefaultPerm）；
//  3. 本机（mac）包复制到 ~/.xxyi/client.json（不存在或内容不同才写，0600），
//     本机的 xxyi-msg 只认这一个固定位置；
//  4. 返回按文件修改时间缓存的吊销表查询函数。
//
// 全程幂等：重复调用不重新生成密钥、不覆盖已签发的证书。
func prepareMsgCA() (*xxyica.CA, func(serial string) bool, error) {
	root, err := xxyiDir()
	if err != nil {
		return nil, nil, err
	}
	caDir := filepath.Join(root, "ca")
	ca, _, err := xxyica.LoadOrCreateCA(caDir)
	if err != nil {
		return nil, nil, err
	}
	clientsDir := filepath.Join(caDir, "clients")
	if err := os.MkdirAll(clientsDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("创建 %s: %w", clientsDir, err)
	}
	for _, side := range clientSides {
		if err := ensureClientBundle(filepath.Join(clientsDir, side+".json"), ca, side); err != nil {
			return nil, nil, err
		}
	}
	if err := copyIfChanged(filepath.Join(clientsDir, "mac.json"), filepath.Join(root, "client.json")); err != nil {
		return nil, nil, err
	}
	rc := &revokedTable{path: filepath.Join(caDir, "revoked.json")}
	return ca, rc.revoked, nil
}

// ensureClientBundle 保证客户端包可用：
//   - 包已存在且证书用当前 CA 校验通过 → 原样保留（证书指纹不能因重启而变）；
//   - 校验不过（签名域换过前缀、已过期等）→ 保留原客户端密钥对重新签发，只换证书，stderr 说明；
//   - 包读不了/读不到 → 生成新密钥对签发（旧包烂在那儿只会让该侧请求永远验不过）。
//
// 这里只看"证书还是不是当前 CA 签的、还在不在有效期"；吊销由请求时校验器的 revoked 回调把关，
// 不在此处消费（重签发会换新序列号，吊销旧序列号本来就该让旧证书请求 401，不该在这里触发重签复活）。
// 本机侧（mac）重签后由 prepareMsgCA 的 copyIfChanged 把新包同步到 ~/.xxyi/client.json。
func ensureClientBundle(path string, ca *xxyica.CA, side string) error {
	_, statErr := os.Stat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("读取 %s: %w", path, statErr)
	}
	if statErr == nil {
		cert, priv, _, lerr := xxyica.LoadClientBundle(path)
		if lerr == nil {
			if verr := xxyica.VerifyCert(ca.Public(), cert, time.Now(), nil); verr == nil {
				return nil
			} else {
				fmt.Fprintf(os.Stderr, "wgtund: 客户端 %s 的证书用当前 CA 校验不通过（%v），保留原密钥重新签发 %s\n", side, verr, path)
				cert2, err := ca.Issue(side, cert.PublicKey, xxyica.DefaultPerm(side), certValidFor, time.Now())
				if err != nil {
					return fmt.Errorf("重签 %s 证书失败: %w", side, err)
				}
				if err := xxyica.WriteClientBundle(path, cert2, priv, ca.Public()); err != nil {
					return fmt.Errorf("写 %s: %w", path, err)
				}
				return nil
			}
		}
		// 旧包读不了：救不出原密钥对，只能换新（报错文案让用户能对上现象）。
		fmt.Fprintf(os.Stderr, "wgtund: 客户端包 %s 读不了（%v），换新密钥重新签发\n", path, lerr)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("生成 %s 客户端密钥失败: %w", side, err)
	}
	cert, err := ca.Issue(side, pub, xxyica.DefaultPerm(side), certValidFor, time.Now())
	if err != nil {
		return fmt.Errorf("签发 %s 证书失败: %w", side, err)
	}
	if err := xxyica.WriteClientBundle(path, cert, priv, ca.Public()); err != nil {
		return fmt.Errorf("写 %s: %w", path, err)
	}
	return nil
}

// copyIfChanged 内容不同（或目标不存在）才原子复制；相同则不动目标的时间戳。
func copyIfChanged(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取 %s: %w", src, err)
	}
	if old, err := os.ReadFile(dst); err == nil && string(old) == string(data) {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读取 %s: %w", dst, err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写 %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // WriteFile 受 umask 影响，显式收紧
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("改名 %s: %w", tmp, err)
	}
	return nil
}

// revokedTable 读 ~/.xxyi/ca/revoked.json（JSON 序列号数组）：
// 按文件修改时间缓存，mtime 没变不重读；文件不存在 = 空表（记为最近一次成功状态）；
// Stat/读/解析失败（不存在除外）：沿用上次成功读到的表；从没成功读到过时 fail-closed
// ——所有序列号都当已吊销，并 stderr 记录。吊销表读不成却"全放进来"的后果
// 远大于暂时按住没吊销的证书：宁可误拦，不可误放。
type revokedTable struct {
	path    string
	mu      sync.Mutex
	mtime   time.Time
	haveAll bool            // 是否成功确定过表内容（读到好 JSON，或确认不存在=空表）
	serials map[string]bool // 上次成功读到的表
}

func (rt *revokedTable) revoked(serial string) bool {
	st, err := os.Stat(rt.path)
	if err != nil {
		if os.IsNotExist(err) {
			// 不存在 = 空表：这也是一次成功的确定，之后的读失败沿用空表。
			rt.mu.Lock()
			rt.serials = nil
			rt.mtime = time.Time{}
			rt.haveAll = true
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
	rt.mu.Unlock()
	return serials[serial]
}

// fallbackLocked 读失败兜底（调用方须持锁）：有上次成功读到的表就沿用；
// 从没成功读到过就 fail-closed（一律当已吊销）。两种都要 stderr 记录。
func (rt *revokedTable) fallbackLocked(what string, err error, serial string) bool {
	if rt.haveAll {
		fmt.Fprintf(os.Stderr, "wgtund: 吊销表 %s %s失败（%v），沿用上次成功读到的表\n", rt.path, what, err)
		return rt.serials[serial]
	}
	fmt.Fprintf(os.Stderr, "wgtund: 吊销表 %s %s失败（%v），此前从未成功读到，按全部吊销处理\n", rt.path, what, err)
	return true
}

// denyAllVerifier 在 CA 准备失败时顶上的永远拒绝的校验器：
// 拿一个跟磁盘 CA 无关的随机新公钥，任何真实证书都验不过，
// 消息接口保持"能连上但全 401"，比整个 wgtund 起不来好排查。
func denyAllVerifier() *xxyica.Verifier {
	if ca, err := xxyica.NewCA(); err == nil {
		return xxyica.NewVerifier(ca.Public(), nil)
	}
	// 连随机密钥都生成不出来（不可能走到）：用全零公钥顶上，同样什么都验不过。
	return xxyica.NewVerifier(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)), nil)
}
