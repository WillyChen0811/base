package approval

// 通用审批通道用的小工具。原先借用 go/support 里的同名实现（侧别校验、读正文、证书校验、写 JSON），
// 挪出来时在这里自带一份，行为不变。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/permca"
)

const (
	// maxBodyBytes 是 POST /approval/ask 正文上限。
	maxBodyBytes = 2 << 20
	// maxDecideMessage 是批准/拒绝附言的上限（按 rune）。
	maxDecideMessage = 500
)

// certAuthHeader 是客户端带证书与请求签名的头（permca.SignRequest 的产物）。
const certAuthHeader = "X-XXYI-Auth"

// addrNameIllegal 匹配地址名字里不许出现的字符：名字只许 ASCII 字母数字和 . _ -。
var addrNameIllegal = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// validAddr 校验会话地址：mac:<名字> 或 ubuntu:<名字>，名字 1..64 字节，只许 ASCII 字母数字和 . _ -。
func validAddr(addr string) error {
	host, name, ok := strings.Cut(addr, ":")
	if !ok || (host != "mac" && host != "ubuntu") {
		return fmt.Errorf("approval: 地址 %q 必须以 mac: 或 ubuntu: 开头", addr)
	}
	if len(name) < 1 || len(name) > 64 {
		return fmt.Errorf("approval: 地址名字长度必须 1..64 字节，实际 %d", len(name))
	}
	if m := addrNameIllegal.FindString(name); m != "" {
		return fmt.Errorf("approval: 地址名字包含非法字符 %q，只允许 ASCII 字母数字和 . _ -", m)
	}
	return nil
}

// checkSide 校验地址属于给定一侧（"mac:" 或 "ubuntu:"）。
// 返回响应码 0 表示通过；格式不对 400，冒充另一侧 403。
func checkSide(addr, side string) (int, string) {
	if err := validAddr(addr); err != nil {
		return http.StatusBadRequest, err.Error()
	}
	if !strings.HasPrefix(addr, side) {
		return http.StatusForbidden, fmt.Sprintf("地址 %q 必须属于 %s 一侧", addr, side)
	}
	return 0, ""
}

// readBody 读取请求正文并限长；返回 nil 表示已经写好错误响应。
// 没有正文也算读成功（空字节串），签名原文里 body 就是空。
func readBody(w http.ResponseWriter, r *http.Request) []byte {
	if r.Body == nil {
		return []byte{}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "读取消息失败")
		return nil
	}
	if len(body) > maxBodyBytes {
		errJSON(w, http.StatusRequestEntityTooLarge, "消息正文超过 2MB")
		return nil
	}
	return body
}

// certCheck 校验客户端接口的请求证书：用请求头对 method+RequestURI+body 验签，
// 时间戳/nonce/吊销由 Verifier 管。Verifier 未配置时直接放行（返回空证书，权限检查一并跳过）。
// 失败已写好 401，返回 false。
func (c *Channel) certCheck(w http.ResponseWriter, r *http.Request, body []byte) (permca.Cert, bool) {
	if c.cfg.Verifier == nil {
		return permca.Cert{}, true
	}
	cert, err := c.cfg.Verifier.Verify(r.Header.Get(certAuthHeader), r.Method, r.URL.RequestURI(), body, time.Now())
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "证书校验失败："+err.Error())
		return permca.Cert{}, false
	}
	return cert, true
}

// certAddrPerm 检查上报的地址是否落在证书前缀内。
func certAddrPerm(perm permca.Perm, addr string) (int, string) {
	if !strings.HasPrefix(addr, perm.AddrPrefix) {
		return http.StatusForbidden, fmt.Sprintf("地址 %s 不符合证书前缀 %s", addr, perm.AddrPrefix)
	}
	return 0, ""
}

// only 校验请求方法，不对就 405。
func only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			errJSON(w, http.StatusMethodNotAllowed, "方法不对")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// truncRunes 按 rune 截断到 n 个字符。
func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
