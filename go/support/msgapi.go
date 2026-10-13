package support

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxMsgBodyBytes 是 /msg/send 正文上限：附件 1MB 经 base64 约 1.34MB，留出文字和 JSON 余量。
	maxMsgBodyBytes = 2 << 20
)

var (
	// outboxWait 是 GET /msg/outbox 长轮询的最长等待时间；测试可调小。
	outboxWait = 30 * time.Second
)

// msgSendReq 是 POST /msg/send 的正文；File 在 JSON 里是 base64（encoding/json 对 []byte 自动编解码）。
type msgSendReq struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Text     string `json:"text"`
	Repo     string `json:"repo,omitempty"`
	Branch   string `json:"branch,omitempty"`
	FileName string `json:"file_name,omitempty"`
	File     []byte `json:"file,omitempty"`
}

// msgEnvelope 是取件响应：Message 原字段 + base64 附件（Message 的 File 是 json:"-"）。
type msgEnvelope struct {
	Message
	FileB64 string `json:"file,omitempty"`
}

// autoUntilUnix 把自动模式截止时间转成 Unix 秒；关着为 0。
func autoUntilUnix(h *Hub) int64 {
	if t := h.AutoUntil(); !t.IsZero() {
		return t.Unix()
	}
	return 0
}

// checkSide 校验地址属于给定一侧（"mac:" 或 "ubuntu:"）。
// 返回响应码 0 表示通过；格式不对 400，冒充另一侧 403。
func checkSide(addr, side string) (int, string) {
	if err := ValidAddr(addr); err != nil {
		return http.StatusBadRequest, err.Error()
	}
	if !strings.HasPrefix(addr, side) {
		return http.StatusForbidden, fmt.Sprintf("地址 %q 必须属于 %s 一侧", addr, side)
	}
	return 0, ""
}

// readMsgBody 读取 /msg/send 正文并限长；返回 nil 表示已经写好错误响应。
func readMsgBody(w http.ResponseWriter, r *http.Request) []byte {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMsgBodyBytes+1))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "读取消息失败")
		return nil
	}
	if len(body) > maxMsgBodyBytes {
		errJSON(w, http.StatusRequestEntityTooLarge, "消息正文超过 2MB")
		return nil
	}
	return body
}

// msgErr 把 Hub 的 Decide/MarkDelivered 错误映射成响应码：查无此条 404，状态不满足 409。
func msgErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrMsgNotFound) {
		errJSON(w, http.StatusNotFound, err.Error())
		return
	}
	errJSON(w, http.StatusConflict, err.Error())
}

// msgList 是 GET /msg：全部消息 + 自动模式截止时间 + 对端忙闲。
func (s *Service) msgList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":   s.hub.List(),
		"auto_until": autoUntilUnix(s.hub),
		"peers":      s.hub.Peers(),
	})
}

// msgSend 是 POST /msg/send；side 是本侧允许的 From 前缀。入库成功返回 {"id":...}。
func (s *Service) msgSend(side string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := readMsgBody(w, r)
		if body == nil {
			return
		}
		var req msgSendReq
		if err := json.Unmarshal(body, &req); err != nil {
			errJSON(w, http.StatusBadRequest, "正文不是合法 JSON")
			return
		}
		// From 必须站在本侧：远端不能冒充 mac，本机也不能冒充 ubuntu。
		if code, msg := checkSide(req.From, side); code != 0 {
			errJSON(w, code, msg)
			return
		}
		id, err := s.hub.Send(Message{
			From:     req.From,
			To:       req.To,
			Text:     req.Text,
			Repo:     req.Repo,
			Branch:   req.Branch,
			FileName: req.FileName,
			File:     req.File,
		})
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		s.kickMsgPump()
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
}

// msgState 是 POST /msg/state?addr=&state=busy|idle；addr 必须是本侧地址。
func (s *Service) msgState(side string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		addr := q.Get("addr")
		if code, msg := checkSide(addr, side); code != 0 {
			errJSON(w, code, msg)
			return
		}
		if err := s.hub.SetState(addr, PeerState(q.Get("state"))); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		s.kickMsgPump()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// msgDecide 是 POST /msg/decide?id=&action=...（approve|now|reject|retry_sync|skip_sync）。
func (s *Service) msgDecide(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := s.hub.Decide(q.Get("id"), q.Get("action")); err != nil {
		msgErr(w, err)
		return
	}
	s.kickMsgPump()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// msgAuto 是 POST /msg/auto?minutes=：0 关闭，1..600 开启；返回新的 auto_until。
func (s *Service) msgAuto(w http.ResponseWriter, r *http.Request) {
	minutes, err := strconv.Atoi(r.URL.Query().Get("minutes"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "minutes 必须是整数")
		return
	}
	if err := s.hub.SetAuto(minutes); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auto_until": autoUntilUnix(s.hub)})
}

// msgOutbox 是 GET /msg/outbox?to=mac:<名>：长轮询取一条发给自己的出站消息（含附件），
// 等待超时返回 204 空正文。取走即从 outbox 移除，收到终端后还要 POST /msg/delivered 确认。
func (s *Service) msgOutbox(w http.ResponseWriter, r *http.Request) {
	to := r.URL.Query().Get("to")
	if code, msg := checkSide(to, "mac:"); code != 0 {
		errJSON(w, code, msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), outboxWait)
	defer cancel()
	m, ok := s.hub.TakeOutbox(ctx, to)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, msgEnvelope{Message: m, FileB64: base64.StdEncoding.EncodeToString(m.File)})
}

// msgDelivered 是 POST /msg/delivered?id=：mac 侧确认消息已注入终端。
func (s *Service) msgDelivered(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.MarkDelivered(r.URL.Query().Get("id")); err != nil {
		msgErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// msgWho 是 GET /msg/who：对端忙闲表（远端用它确认自己上报的状态被看到）。
func (s *Service) msgWho(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"peers": s.hub.Peers()})
}

// mountMsgLocal 把本机的消息接口挂进 LocalHandler 的 mux。
func (s *Service) mountMsgLocal(mux *http.ServeMux) {
	mux.HandleFunc("/msg", only(http.MethodGet, s.msgList))
	mux.HandleFunc("/msg/send", only(http.MethodPost, s.msgSend("mac:")))
	mux.HandleFunc("/msg/state", only(http.MethodPost, s.msgState("mac:")))
	mux.HandleFunc("/msg/decide", only(http.MethodPost, s.msgDecide))
	mux.HandleFunc("/msg/auto", only(http.MethodPost, s.msgAuto))
	mux.HandleFunc("/msg/outbox", only(http.MethodGet, s.msgOutbox))
	mux.HandleFunc("/msg/delivered", only(http.MethodPost, s.msgDelivered))
}

// mountMsgRemote 把远端的消息接口挂进 remoteMux 的 mux（外层 guard 鉴权）。
// 远端只能代发（From 必须 ubuntu:）、上报自己的忙闲、看对端表；批准/取件/自动模式一律不在远端。
func (s *Service) mountMsgRemote(mux *http.ServeMux) {
	mux.HandleFunc("/msg/send", only(http.MethodPost, s.msgSend("ubuntu:")))
	mux.HandleFunc("/msg/state", only(http.MethodPost, s.msgState("ubuntu:")))
	mux.HandleFunc("/msg/who", only(http.MethodGet, s.msgWho))
}
