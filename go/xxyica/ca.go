// Package xxyica 实现 XXYI 体系的证书签发与请求签名校验：CA 给客户端签发带权限声明的
// Ed25519 证书，客户端用它签请求头，服务端只拿 CA 公钥就能验真、验权限。纯标准库、无状态副作用（除文件读写）。
package xxyica

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const (
	// tsWindowSeconds 是请求时间戳允许的偏差：超过即视为过期或时钟漂移。
	tsWindowSeconds = 300
	// nonceWindow 是 nonce 防重放的记忆窗口，必须大于 tsWindowSeconds，
	// 否则窗口外的重放会先被时间戳检查拦下。
	nonceWindow = 10 * time.Minute
)

// certNamePattern 限制客户端名只用小写字母、数字和连字符，方便进路径和展示。
var certNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Cert 是 CA 签发给客户端的证书。
type Cert struct {
	Name      string    `json:"name"`       // 客户端名，[a-z0-9-]{1,32}
	PublicKey []byte    `json:"public_key"` // 客户端 Ed25519 公钥（32 字节）
	Perm      Perm      `json:"perm"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Serial    string    `json:"serial"` // 随机 16 位十六进制
	Sig       []byte    `json:"sig"`    // CA 对除 Sig 外字段的规范 JSON 的签名
}

// certSigningBytes 返回 Cert 去掉 Sig 后的规范 JSON——签发和校验两边都用它，
// 字段顺序由 certBody 的结构体定义固定，time/json.Marshal 都是确定性输出。
// certBody 各字段序列化都不可能出错，故忽略 Marshal 的错误返回。
func certSigningBytes(c Cert) []byte {
	b, _ := json.Marshal(struct {
		Name      string    `json:"name"`
		PublicKey []byte    `json:"public_key"`
		Perm      Perm      `json:"perm"`
		NotBefore time.Time `json:"not_before"`
		NotAfter  time.Time `json:"not_after"`
		Serial    string    `json:"serial"`
	}{
		Name:      c.Name,
		PublicKey: c.PublicKey,
		Perm:      c.Perm,
		NotBefore: c.NotBefore,
		NotAfter:  c.NotAfter,
		Serial:    c.Serial,
	})
	return b
}

// CA 是证书签发方，持有 Ed25519 密钥对。
type CA struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

// NewCA 生成一把新的 CA 密钥。
func NewCA() (*CA, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 CA 密钥失败: %w", err)
	}
	return &CA{pub: pub, priv: priv}, nil
}

// LoadOrCreateCA 从 dir 加载 CA（ca.key 存私钥 seed，ca.pub 存公钥）；
// 两个文件都不存在时生成新的并写入（目录 0700、私钥 0600，先临时文件再 rename 原子落盘），
// 首次创建返回 created=true。只存在一个文件时报错——不敢猜哪半是真的。
func LoadOrCreateCA(dir string) (*CA, bool, error) {
	keyPath := filepath.Join(dir, "ca.key")
	pubPath := filepath.Join(dir, "ca.pub")
	keyData, keyErr := os.ReadFile(keyPath)
	pubData, pubErr := os.ReadFile(pubPath)
	switch {
	case keyErr == nil && pubErr == nil:
		seed, err := decodePEM(keyData, "XXYI CA PRIVATE KEY")
		if err != nil {
			return nil, false, fmt.Errorf("读取 %s: %w", keyPath, err)
		}
		if len(seed) != ed25519.SeedSize {
			return nil, false, fmt.Errorf("%s 私钥 seed 长度 %d 不对", keyPath, len(seed))
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pub, ok := priv.Public().(ed25519.PublicKey)
		if !ok {
			return nil, false, fmt.Errorf("%s 私钥无效", keyPath)
		}
		pubKey, err := decodePEM(pubData, "XXYI CA PUBLIC KEY")
		if err != nil {
			return nil, false, fmt.Errorf("读取 %s: %w", pubPath, err)
		}
		if len(pubKey) != ed25519.PublicKeySize || !pub.Equal(ed25519.PublicKey(pubKey)) {
			return nil, false, fmt.Errorf("%s 与 %s 不是同一把钥匙", keyPath, pubPath)
		}
		return &CA{pub: pub, priv: priv}, false, nil
	case keyErr == nil || pubErr == nil:
		if keyErr != nil && !os.IsNotExist(keyErr) {
			return nil, false, fmt.Errorf("读取 %s: %w", keyPath, keyErr)
		}
		if pubErr != nil && !os.IsNotExist(pubErr) {
			return nil, false, fmt.Errorf("读取 %s: %w", pubPath, pubErr)
		}
		return nil, false, fmt.Errorf("CA 文件不完整：%s 和 %s 只存在一个", keyPath, pubPath)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("生成 CA 密钥失败: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("创建目录 %s: %w", dir, err)
	}
	if err := writePEM(keyPath, "XXYI CA PRIVATE KEY", priv.Seed()); err != nil {
		return nil, false, err
	}
	if err := writePEM(pubPath, "XXYI CA PUBLIC KEY", pub); err != nil {
		return nil, false, err
	}
	return &CA{pub: pub, priv: priv}, true, nil
}

// Public 返回 CA 公钥，交给服务端做校验用。
func (c *CA) Public() ed25519.PublicKey { return c.pub }

// Issue 给客户端签发证书：校验名字和公钥格式，有效期 [now, now+validFor)，
// 序列号随机 16 位十六进制，CA 对除 Sig 外的字段做规范 JSON 签名。
func (c *CA) Issue(name string, clientPub ed25519.PublicKey, perm Perm, validFor time.Duration, now time.Time) (Cert, error) {
	if !certNamePattern.MatchString(name) {
		return Cert{}, fmt.Errorf("客户端名 %q 不合法（只允许小写字母/数字/连字符，1-32 位）", name)
	}
	if len(clientPub) != ed25519.PublicKeySize {
		return Cert{}, fmt.Errorf("客户端公钥长度 %d 不对", len(clientPub))
	}
	serial := make([]byte, 8)
	if _, err := rand.Read(serial); err != nil {
		return Cert{}, fmt.Errorf("生成序列号失败: %w", err)
	}
	cert := Cert{
		Name:      name,
		PublicKey: clientPub,
		Perm:      perm,
		NotBefore: now,
		NotAfter:  now.Add(validFor),
		Serial:    hex.EncodeToString(serial),
	}
	cert.Sig = c.Sign(certSigningBytes(cert))
	return cert, nil
}

// Sign 用 CA 私钥签任意数据（给消息签名用）。
func (c *CA) Sign(data []byte) []byte {
	return ed25519.Sign(c.priv, data)
}

// VerifyCert 校验证书：CA 签名、时间窗 [NotBefore, NotAfter]、吊销名单（revoked 可为 nil）。
// 客户端和服务端都用它，只需要 CA 公钥。
func VerifyCert(caPub ed25519.PublicKey, cert Cert, now time.Time, revoked func(serial string) bool) error {
	if len(caPub) != ed25519.PublicKeySize {
		return fmt.Errorf("CA 公钥长度 %d 不对", len(caPub))
	}
	if len(cert.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("证书公钥长度 %d 不对", len(cert.PublicKey))
	}
	if !ed25519.Verify(caPub, certSigningBytes(cert), cert.Sig) {
		return fmt.Errorf("证书 %s 的 CA 签名无效", cert.Name)
	}
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("证书 %s 尚未生效", cert.Name)
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("证书 %s 已过期", cert.Name)
	}
	if revoked != nil && revoked(cert.Serial) {
		return fmt.Errorf("证书 %s（序列号 %s）已被吊销", cert.Name, cert.Serial)
	}
	return nil
}

// VerifyMessage 用 CA 公钥验证消息签名。
func VerifyMessage(caPub ed25519.PublicKey, data, sig []byte) bool {
	if len(caPub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(caPub, data, sig)
}

// authPayload 是 X-XXYI-Auth 请求头解码后的内容。
type authPayload struct {
	Cert  Cert   `json:"cert"`
	TS    int64  `json:"ts"`
	Nonce string `json:"nonce"`
	Sig   []byte `json:"sig"`
}

// requestSigningString 拼出请求签名的原文。ts 是 Unix 秒的十进制，nonce 是 16 字节随机 hex。
func requestSigningString(method, path string, ts int64, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%s\n%s\n%d\n%s\n%s", method, path, ts, nonce, hex.EncodeToString(sum[:]))
}

// SignRequest 用客户端私钥给一次请求签名，返回 X-XXYI-Auth 头的值
// （base64url 编码的 JSON{cert, ts, nonce, sig}）。path 含查询串。
// 私钥必须与证书公钥配对，否则直接报错，别让请求带着一对不配的钥匙出门。
func SignRequest(priv ed25519.PrivateKey, cert Cert, method, path string, body []byte, now time.Time) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("私钥长度 %d 不对", len(priv))
	}
	if len(cert.PublicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("证书公钥长度 %d 不对", len(cert.PublicKey))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok || !pub.Equal(ed25519.PublicKey(cert.PublicKey)) {
		return "", fmt.Errorf("私钥与证书 %s 的公钥不匹配", cert.Name)
	}
	ts := now.Unix()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	sig := ed25519.Sign(priv, []byte(requestSigningString(method, path, ts, nonce, body)))
	blob, err := json.Marshal(authPayload{Cert: cert, TS: ts, Nonce: nonce, Sig: sig})
	if err != nil {
		return "", fmt.Errorf("编码认证头失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(blob), nil
}

// Verifier 是服务端校验器：拿 CA 公钥验证书，再验请求签名的时间戳、
// nonce 防重放（10 分钟窗口，每次校验顺手清旧条目）。
type Verifier struct {
	caPub   ed25519.PublicKey
	revoked func(serial string) bool

	mu   sync.Mutex
	seen map[string]time.Time // nonce -> 见过的时间
}

// NewVerifier 创建服务端校验器，revoked 可为 nil。
func NewVerifier(caPub ed25519.PublicKey, revoked func(serial string) bool) *Verifier {
	return &Verifier{caPub: caPub, revoked: revoked, seen: make(map[string]time.Time)}
}

// Verify 校验一个请求头，通过后返回其中的证书（调用方再拿 cert.Perm 做授权判断）。
func (v *Verifier) Verify(header, method, path string, body []byte, now time.Time) (Cert, error) {
	blob, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return Cert{}, fmt.Errorf("认证头不是合法的 base64url: %w", err)
	}
	var payload authPayload
	if err := json.Unmarshal(blob, &payload); err != nil {
		return Cert{}, fmt.Errorf("认证头 JSON 解析失败: %w", err)
	}
	if err := VerifyCert(v.caPub, payload.Cert, now, v.revoked); err != nil {
		return Cert{}, err
	}
	diff := now.Unix() - payload.TS
	if diff > tsWindowSeconds || diff < -tsWindowSeconds {
		return Cert{}, fmt.Errorf("请求时间戳超出允许范围（%d 秒）", tsWindowSeconds)
	}
	if payload.Nonce == "" {
		return Cert{}, fmt.Errorf("认证头缺少 nonce")
	}
	// 先验签名再登记 nonce：签名不对的请求不该占用 nonce
	signing := requestSigningString(method, path, payload.TS, payload.Nonce, body)
	if !ed25519.Verify(payload.Cert.PublicKey, []byte(signing), payload.Sig) {
		return Cert{}, fmt.Errorf("请求签名无效")
	}
	if !v.useNonce(payload.Nonce, now) {
		return Cert{}, fmt.Errorf("nonce 重放被拒绝")
	}
	return payload.Cert, nil
}

// useNonce 记录并使用一个 nonce；10 分钟内见过即判重放。顺手清掉窗口外的旧条目。
func (v *Verifier) useNonce(nonce string, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, t := range v.seen {
		if now.Sub(t) > nonceWindow {
			delete(v.seen, k)
		}
	}
	if _, ok := v.seen[nonce]; ok {
		return false
	}
	v.seen[nonce] = now
	return true
}

// WriteClientBundle 把客户端三件套（证书、私钥、CA 公钥）写成一个 0600 的 JSON 文件，
// 先写同目录临时文件再 rename，避免半截文件。
func WriteClientBundle(path string, cert Cert, priv ed25519.PrivateKey, caPub ed25519.PublicKey) error {
	blob, err := json.Marshal(struct {
		Cert        Cert   `json:"cert"`
		PrivateKey  []byte `json:"private_key"`
		CAPublicKey []byte `json:"ca_public_key"`
	}{Cert: cert, PrivateKey: priv, CAPublicKey: caPub})
	if err != nil {
		return fmt.Errorf("编码客户端文件失败: %w", err)
	}
	return writeFileAtomic(path, blob, 0o600)
}

// LoadClientBundle 读回客户端三件套，并校验私钥与证书公钥确实配对。
func LoadClientBundle(path string) (Cert, ed25519.PrivateKey, ed25519.PublicKey, error) {
	var bundle struct {
		Cert        Cert   `json:"cert"`
		PrivateKey  []byte `json:"private_key"`
		CAPublicKey []byte `json:"ca_public_key"`
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return Cert{}, nil, nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	if err := json.Unmarshal(blob, &bundle); err != nil {
		return Cert{}, nil, nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if len(bundle.PrivateKey) != ed25519.PrivateKeySize {
		return Cert{}, nil, nil, fmt.Errorf("%s 私钥长度 %d 不对", path, len(bundle.PrivateKey))
	}
	if len(bundle.CAPublicKey) != ed25519.PublicKeySize {
		return Cert{}, nil, nil, fmt.Errorf("%s CA 公钥长度 %d 不对", path, len(bundle.CAPublicKey))
	}
	priv := ed25519.PrivateKey(bundle.PrivateKey)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok || !pub.Equal(ed25519.PublicKey(bundle.Cert.PublicKey)) {
		return Cert{}, nil, nil, fmt.Errorf("%s 私钥与证书公钥不匹配", path)
	}
	return bundle.Cert, priv, ed25519.PublicKey(bundle.CAPublicKey), nil
}

// decodePEM 取指定类型 PEM 块的原文。
func decodePEM(data []byte, typ string) ([]byte, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("找不到 %s 的 PEM 块", typ)
	}
	if block.Type != typ {
		return nil, fmt.Errorf("PEM 类型是 %s，期望 %s", block.Type, typ)
	}
	return block.Bytes, nil
}

// writePEM 原子写一个 PEM 文件（0600）。
func writePEM(path, typ string, data []byte) error {
	return writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: data}), 0o600)
}

// writeFileAtomic 先写同目录临时文件再 rename；rename 会把权限一并落定。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("创建临时文件 %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("写入临时文件 %s: %w", tmp, err)
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("设置 %s 权限: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("关闭临时文件 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("改名 %s -> %s: %w", tmp, path, err)
	}
	return nil
}
