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

// ensureClientBundle 客户端包已存在就原样保留（证书指纹不能因重启而变）；
// 不存在才生成密钥对并向 CA 申领证书。
func ensureClientBundle(path string, ca *xxyica.CA, side string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 %s: %w", path, err)
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
// 按文件修改时间缓存，mtime 没变不重读；文件不存在等于空表；
// 读失败/坏 JSON 沿用上一次的结果（吊销表写坏了不能让所有证书突然全放进来）。
type revokedTable struct {
	path    string
	mu      sync.Mutex
	mtime   time.Time
	loaded  bool
	serials map[string]bool
}

func (rt *revokedTable) revoked(serial string) bool {
	st, err := os.Stat(rt.path)
	if err != nil {
		rt.mu.Lock()
		rt.loaded = true
		rt.serials = nil
		rt.mtime = time.Time{}
		rt.mu.Unlock()
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.loaded && rt.mtime.Equal(st.ModTime()) {
		return rt.serials[serial]
	}
	data, err := os.ReadFile(rt.path)
	if err != nil {
		return rt.serials[serial] // 读不到沿用缓存
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return rt.serials[serial] // 坏 JSON 沿用缓存，不放大成"全部未吊销"
	}
	serials := make(map[string]bool, len(list))
	for _, s := range list {
		serials[s] = true
	}
	rt.serials = serials
	rt.mtime = st.ModTime()
	rt.loaded = true
	return serials[serial]
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
