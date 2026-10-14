package support

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/xxyica"
)

// msgAuthFixture 打包带证书校验的协助服务：一个 CA、mac/ubuntu 两侧客户端证书、
// 可变吊销表，以及带 X-XXYI-Auth 签名的请求入口（path+query 必须与服务端 RequestURI 一致）。
type msgAuthFixture struct {
	s     *Service
	base  string // http://<远端监听地址>
	token string
	lbase string // 本机 LocalHandler 的根
	ca    *xxyica.CA

	macC xxyica.Cert
	macP ed25519.PrivateKey
	ubiC xxyica.Cert
	ubiP ed25519.PrivateKey

	revMu  sync.Mutex
	revSet map[string]bool
}

// newMsgAuthFix 起服务；withAuth=false 时不配 MsgAuth/MsgSign（验证行为不变）。
func newMsgAuthFix(t *testing.T, withAuth bool) *msgAuthFixture {
	t.Helper()
	oldP, oldW := msgPumpInterval, outboxWait
	msgPumpInterval = 20 * time.Millisecond
	outboxWait = 300 * time.Millisecond
	t.Cleanup(func() { msgPumpInterval, outboxWait = oldP, oldW })

	ca, err := xxyica.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	now := time.Now()
	macPub, macPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("mac 客户端密钥: %v", err)
	}
	ubiPub, ubiPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ubuntu 客户端密钥: %v", err)
	}
	macCert, err := ca.Issue("mac", macPub, xxyica.DefaultPerm("mac"), time.Hour, now)
	if err != nil {
		t.Fatalf("签发 mac 证书: %v", err)
	}
	ubiCert, err := ca.Issue("ubuntu", ubiPub, xxyica.DefaultPerm("ubuntu"), time.Hour, now)
	if err != nil {
		t.Fatalf("签发 ubuntu 证书: %v", err)
	}

	f := &msgAuthFixture{
		ca: ca, macC: macCert, macP: macPriv, ubiC: ubiCert, ubiP: ubiPriv,
		revSet: map[string]bool{},
	}
	cfg := Config{
		MsgDeliver: func(context.Context, Message) error { return nil },
		MsgSync:    func(context.Context, Message) error { return nil },
	}
	if withAuth {
		cfg.MsgAuth = xxyica.NewVerifier(ca.Public(), f.isRevoked)
		cfg.MsgSign = ca.Sign
	}
	s, fr := newTestService(t, cfg)
	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.s = s
	f.base = "http://" + s.Status().RemoteAddr
	f.token = fr.publishRec(0).token
	f.lbase = newServer(t, s.LocalHandler()).URL
	return f
}

func (f *msgAuthFixture) isRevoked(serial string) bool {
	f.revMu.Lock()
	defer f.revMu.Unlock()
	return f.revSet[serial]
}

func (f *msgAuthFixture) revoke(serial string) {
	f.revMu.Lock()
	defer f.revMu.Unlock()
	f.revSet[serial] = true
}

// authDo 带任意头发请求；签名 path 是 URL 的 path+query（服务端看的就是 RequestURI）。
func authDo(t *testing.T, method, rawURL, token, certHdr string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if certHdr != "" {
		req.Header.Set("X-XXYI-Auth", certHdr)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(t, resp)
}

// remoteSign 对远端一次请求签名（pathWithQuery 如 "/msg/state?addr=ubuntu:x"）。
func (f *msgAuthFixture) remoteSign(t *testing.T, method, pathWithQuery string, priv ed25519.PrivateKey, cert xxyica.Cert, body []byte) string {
	t.Helper()
	hdr, err := xxyica.SignRequest(priv, cert, method, pathWithQuery, body, time.Now())
	if err != nil {
		t.Fatalf("签名请求 %s %s: %v", method, pathWithQuery, err)
	}
	return hdr
}

// remoteAuth 远端：令牌 + 客户端证书双过头。
func (f *msgAuthFixture) remoteAuth(t *testing.T, method, pathWithQuery string, priv ed25519.PrivateKey, cert xxyica.Cert, body []byte) (int, []byte) {
	t.Helper()
	hdr := f.remoteSign(t, method, pathWithQuery, priv, cert, body)
	return authDo(t, method, f.base+pathWithQuery, f.token, hdr, body)
}

// localAuth 本机：只带客户端证书。
func (f *msgAuthFixture) localAuth(t *testing.T, method, pathWithQuery string, priv ed25519.PrivateKey, cert xxyica.Cert, body []byte) (int, []byte) {
	t.Helper()
	hdr := f.remoteSign(t, method, pathWithQuery, priv, cert, body)
	return authDo(t, method, f.lbase+pathWithQuery, "", hdr, body)
}

// tamperAuthHeader 把认证头里的签名改一个字节（JSON 结构保持合法，只坏签名）。
func tamperAuthHeader(t *testing.T, hdr string) string {
	t.Helper()
	blob, err := base64.RawURLEncoding.DecodeString(hdr)
	if err != nil {
		t.Fatalf("解认证头: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(blob, &payload); err != nil {
		t.Fatalf("认证头 JSON: %v", err)
	}
	sig, ok := payload["sig"].(string)
	if !ok {
		t.Fatalf("认证头没有 sig 字段: %s", blob)
	}
	b, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("解 sig: %v", err)
	}
	b[0] ^= 0x01
	payload["sig"] = base64.StdEncoding.EncodeToString(b)
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(out)
}

// TestMsgAuthRemoteSendRejectsBadAuth 远端 /msg/send：不带证书、坏签名、被吊销都 401；
// ubuntu 证书签名发往 mac 一侧 200、发往另一侧地址 403（DefaultPerm 不许）。
func TestMsgAuthRemoteSendRejectsBadAuth(t *testing.T) {
	f := newMsgAuthFix(t, true)
	body := msgBody("ubuntu:x", "mac:y", "帮看下", nil, "")

	// 不带 X-XXYI-Auth（令牌有）：401，且是证书门拦的而不是 guard。
	if code, b := authDo(t, http.MethodPost, f.base+"/msg/send", f.token, "", body); code != 401 || !strings.Contains(string(b), "证书校验失败") {
		t.Fatalf("无证书应 401 证书校验失败，得 %d %s", code, b)
	}
	// 坏签名（改一个字节）。
	hdr := f.remoteSign(t, http.MethodPost, "/msg/send", f.ubiP, f.ubiC, body)
	if code, b := authDo(t, http.MethodPost, f.base+"/msg/send", f.token, tamperAuthHeader(t, hdr), body); code != 401 {
		t.Fatalf("坏签名应 401，得 %d %s", code, b)
	}
	// 合法签名 → 200 入库。
	if code, b := f.remoteAuth(t, http.MethodPost, "/msg/send", f.ubiP, f.ubiC, body); code != 200 {
		t.Fatalf("ubuntu 证书发往 mac:y 应 200，得 %d %s", code, b)
	}
	// 权限：ubuntu 证书只能发往 mac:*，发往 ubuntu:z 应 403（签名本身是合法的）。
	other := msgBody("ubuntu:x", "ubuntu:z", "x", nil, "")
	if code, b := f.remoteAuth(t, http.MethodPost, "/msg/send", f.ubiP, f.ubiC, other); code != 403 {
		t.Fatalf("发往本侧地址应 403，得 %d %s", code, b)
	}
}

// TestMsgAuthRevokedCert 吊销 ubuntu 证书后再请求 401；mac 证书不受影响。
func TestMsgAuthRevokedCert(t *testing.T) {
	f := newMsgAuthFix(t, true)
	path := "/msg/who"
	if code, _ := f.remoteAuth(t, http.MethodGet, path, f.ubiP, f.ubiC, nil); code != 200 {
		t.Fatalf("吊销前应 200，得 %d", code)
	}
	f.revoke(f.ubiC.Serial)
	if code, b := f.remoteAuth(t, http.MethodGet, path, f.ubiP, f.ubiC, nil); code != 401 || !strings.Contains(string(b), "吊销") {
		t.Fatalf("吊销后应 401 且说明吊销，得 %d %s", code, b)
	}
	if code, _ := f.remoteAuth(t, http.MethodGet, path, f.macP, f.macC, nil); code != 200 {
		t.Fatalf("mac 证书不该受牵连，得 %d", code)
	}
}

// TestMsgAuthStateAddrScope /msg/state 上报的地址必须落在证书前缀内：
// ubuntu 证书报 mac:x 应 403，报 ubuntu:x 应 200。
func TestMsgAuthStateAddrScope(t *testing.T) {
	f := newMsgAuthFix(t, true)
	if code, b := f.remoteAuth(t, http.MethodPost, "/msg/state?addr=mac:x&state=idle", f.ubiP, f.ubiC, nil); code != 403 {
		t.Fatalf("报别人侧地址应 403，得 %d %s", code, b)
	}
	if code, b := f.remoteAuth(t, http.MethodPost, "/msg/state?addr=ubuntu:x&state=idle", f.ubiP, f.ubiC, nil); code != 200 {
		t.Fatalf("报自己侧地址应 200，得 %d %s", code, b)
	}
}

// TestMsgAuthInboxWhoNoCert 远端 /msg/inbox、/msg/who 不带证书一律 401；带合法证书 200。
func TestMsgAuthInboxWhoNoCert(t *testing.T) {
	f := newMsgAuthFix(t, true)
	for _, p := range []string{"/msg/inbox?addr=ubuntu:x", "/msg/who"} {
		if code, b := authDo(t, http.MethodGet, f.base+p, f.token, "", nil); code != 401 || !strings.Contains(string(b), "证书校验失败") {
			t.Fatalf("无证书 GET %s 应 401 证书校验失败，得 %d %s", p, code, b)
		}
	}
	if code, b := f.remoteAuth(t, http.MethodGet, "/msg/inbox?addr=ubuntu:x", f.ubiP, f.ubiC, nil); code != 200 {
		t.Fatalf("带证书 GET inbox 应 200，得 %d %s", code, b)
	}
}

// TestMsgAuthLocalSendAndPublic 本机：/msg/send 要 mac 证书（From=mac:a → 200）；
// /msg 列表、/msg/outbox、/msg/delivered 不需要证书。
func TestMsgAuthLocalSendAndPublic(t *testing.T) {
	f := newMsgAuthFix(t, true)
	body := msgBody("mac:a", "ubuntu:b", "本机发", nil, "")

	if code, b := f.localAuth(t, http.MethodPost, "/msg/send", f.macP, f.macC, body); code != 200 {
		t.Fatalf("mac 证书本机发送应 200，得 %d %s", code, b)
	}
	// 本机同样要有证书；拿 ubuntu 证书冒充 mac 侧发件应 403（证书过了、权限不过）。
	if code, b := authDo(t, http.MethodPost, f.lbase+"/msg/send", "", "", body); code != 401 {
		t.Fatalf("本机无证书发送应 401，得 %d %s", code, b)
	}
	if code, b := f.localAuth(t, http.MethodPost, "/msg/send", f.ubiP, f.ubiC, body); code != 403 {
		t.Fatalf("ubuntu 证书冒充 mac 侧发件应 403，得 %d %s", code, b)
	}

	// 列表/取件/确认：不带头也不 401。
	if code, b := authDo(t, http.MethodGet, f.lbase+"/msg", "", "", nil); code != 200 {
		t.Fatalf("GET /msg 不该要证书，得 %d %s", code, b)
	}
	if code, _ := authDo(t, http.MethodGet, f.lbase+"/msg/outbox?to=mac:a", "", "", nil); code == 401 {
		t.Fatalf("GET /msg/outbox 不该 401，得 %d", code)
	}
	if code, _ := authDo(t, http.MethodPost, f.lbase+"/msg/delivered?id=nope", "", "", nil); code == 401 {
		t.Fatalf("POST /msg/delivered 不该 401，得 %d", code)
	}
}

// signedMsgJSON 是 /msg 列表里一条消息（含签名域字段）。
type signedMsgJSON struct {
	ID         string    `json:"id"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Kind       string    `json:"kind"`
	ReplyTo    string    `json:"reply_to"`
	Text       string    `json:"text"`
	FileName   string    `json:"file_name"`
	Repo       string    `json:"repo"`
	Branch     string    `json:"branch"`
	FileSHA256 string    `json:"file_sha256"`
	Created    time.Time `json:"created"`
	Sig        []byte    `json:"sig"`
}

func (m signedMsgJSON) fields() xxyica.MsgFields {
	return xxyica.MsgFields{
		ID: m.ID, From: m.From, To: m.To, Kind: m.Kind, ReplyTo: m.ReplyTo,
		Text: m.Text, FileName: m.FileName, Repo: m.Repo, Branch: m.Branch,
		FileSHA256: m.FileSHA256, Created: m.Created,
	}
}

// TestMsgAuthMessageSigVerifiable 入库消息的 Sig 能用 CA 公钥按签名域核验；改正文后核验失败。
func TestMsgAuthMessageSigVerifiable(t *testing.T) {
	f := newMsgAuthFix(t, true)
	body := msgBody("mac:a", "ubuntu:b", "核验消息", nil, "")
	code, b := f.localAuth(t, http.MethodPost, "/msg/send", f.macP, f.macC, body)
	if code != 200 {
		t.Fatalf("本机发送: %d %s", code, b)
	}
	var resp map[string]string
	if err := json.Unmarshal(b, &resp); err != nil || resp["id"] == "" {
		t.Fatalf("响应应带 id: %s", b)
	}
	id := resp["id"]

	code, b = authDo(t, http.MethodGet, f.lbase+"/msg", "", "", nil)
	if code != 200 {
		t.Fatalf("GET /msg: %d %s", code, b)
	}
	var list struct {
		Messages []signedMsgJSON `json:"messages"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("解列表: %v %s", err, b)
	}
	var got *signedMsgJSON
	for i := range list.Messages {
		if list.Messages[i].ID == id {
			got = &list.Messages[i]
		}
	}
	if got == nil {
		t.Fatalf("列表里找不到 %s: %s", id, b)
	}
	if len(got.Sig) == 0 {
		t.Fatalf("配了 MsgSign，入库消息应带 Sig: %s", b)
	}
	if !xxyica.VerifyMessage(f.ca.Public(), xxyica.MessageSigningBytes(got.fields()), got.Sig) {
		t.Fatalf("签名核验应通过（消息 %+v）", *got)
	}
	tampered := got.fields()
	tampered.Text = "篡改过的正文"
	if xxyica.VerifyMessage(f.ca.Public(), xxyica.MessageSigningBytes(tampered), got.Sig) {
		t.Fatal("改 Text 后核验竟通过")
	}
}

// TestMsgAuthNilAllowsPlain 不配 MsgAuth 时一切照旧：不带证书头照常 200。
func TestMsgAuthNilAllowsPlain(t *testing.T) {
	f := newMsgAuthFix(t, false)
	if code, b := authDo(t, http.MethodPost, f.base+"/msg/send", f.token, "",
		msgBody("ubuntu:x", "mac:y", "无证书模式", nil, "")); code != 200 {
		t.Fatalf("远端无证书发送应 200，得 %d %s", code, b)
	}
	if code, b := authDo(t, http.MethodPost, f.lbase+"/msg/send", "", "",
		msgBody("mac:a", "ubuntu:b", "无证书模式", nil, "")); code != 200 {
		t.Fatalf("本机无证书发送应 200，得 %d %s", code, b)
	}
	if code, _ := authDo(t, http.MethodGet, f.base+"/msg/who", f.token, "", nil); code != 200 {
		t.Fatalf("无证书 GET who 应 200，得 %d", code)
	}
}
