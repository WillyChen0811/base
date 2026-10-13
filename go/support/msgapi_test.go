package support

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// msgAPISRV 打包一个已启动的协助服务 + 本机/远端的访问入口。
type msgAPISRV struct {
	s     *Service
	base  string // http://<远端监听地址>
	token string
	lURL  string // 本机 /msg
}

// newMsgAPI 建服务：Deliver/Sync 记录调用，推进和长轮询间隔调小。
func newMsgAPI(t *testing.T, minutes int) *msgAPISRV {
	t.Helper()
	oldP, oldW := msgPumpInterval, outboxWait
	msgPumpInterval = 20 * time.Millisecond
	outboxWait = 300 * time.Millisecond
	t.Cleanup(func() { msgPumpInterval, outboxWait = oldP, oldW })

	var mu sync.Mutex
	var delivered []string
	s, fr := newTestService(t, Config{
		MsgDeliver: func(_ context.Context, m Message) error {
			mu.Lock()
			defer mu.Unlock()
			delivered = append(delivered, fmt.Sprintf("%s|%s|%s", m.ID, m.From, m.Text))
			return nil
		},
		MsgSync: func(_ context.Context, m Message) error { return nil },
	})
	if err := s.Start(minutes); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return &msgAPISRV{
		s:     s,
		base:  "http://" + s.Status().RemoteAddr,
		token: fr.publishRec(0).token,
		lURL:  newServer(t, s.LocalHandler()).URL + "/msg",
	}
}

// msgBody 拼 /msg/send 的 JSON 正文。
func msgBody(from, to, text string, file []byte, fileName string) []byte {
	b, _ := json.Marshal(msgSendReq{From: from, To: to, Text: text, FileName: fileName, File: file})
	return b
}

func post(t *testing.T, url string, body []byte) (int, []byte) {
	t.Helper()
	return remoteDo(t, http.MethodPost, url, "", body)
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	return remoteDo(t, http.MethodGet, url, "", nil)
}

type msgListResp struct {
	Messages  []Message         `json:"messages"`
	AutoUntil int64             `json:"auto_until"`
	Peers     map[string]string `json:"peers"`
}

func listMsg(t *testing.T, url string) msgListResp {
	t.Helper()
	code, body := get(t, url)
	if code != 200 {
		t.Fatalf("GET /msg: %d %s", code, body)
	}
	var r msgListResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("解 /msg: %v %s", err, body)
	}
	return r
}

func msgStateOf(t *testing.T, url, id string) MsgState {
	t.Helper()
	for _, m := range listMsg(t, url).Messages {
		if m.ID == id {
			return m.State
		}
	}
	t.Fatalf("消息 %s 不在列表里", id)
	return ""
}

// TestMsgLocalSendDecideDeliver 本机发消息 → 批准 → 推进循环经 Deliver 送达。
func TestMsgLocalSendDecideDeliver(t *testing.T) {
	a := newMsgAPI(t, 30)

	code, body := post(t, a.lURL+"/send", msgBody("mac:frank", "ubuntu:bob", "帮看下", nil, ""))
	if code != 200 {
		t.Fatalf("本机发消息: %d %s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil || resp["id"] == "" {
		t.Fatalf("响应应带 id: %s", body)
	}
	id := resp["id"]
	if st := msgStateOf(t, a.lURL, id); st != MsgPending {
		t.Fatalf("未批准前状态应为 pending，得 %s", st)
	}

	code, body = post(t, a.lURL+"/decide?id="+id+"&action=approve", nil)
	if code != 200 {
		t.Fatalf("approve: %d %s", code, body)
	}
	waitFor(t, 3*time.Second, "消息送达", func() bool {
		return msgStateOf(t, a.lURL, id) == MsgDelivered
	})

	// 再批准应 409（已不是 pending）；查无此条应 404。
	if code, _ := post(t, a.lURL+"/decide?id="+id+"&action=approve", nil); code != 409 {
		t.Fatalf("重复 approve 应 409，得 %d", code)
	}
	if code, _ := post(t, a.lURL+"/decide?id=nope&action=approve", nil); code != 404 {
		t.Fatalf("不存在的 id 应 404，得 %d", code)
	}
}

// TestMsgSendSideEnforced 本机不能冒充 ubuntu，远端不能冒充 mac。
func TestMsgSendSideEnforced(t *testing.T) {
	a := newMsgAPI(t, 30)

	if code, body := post(t, a.lURL+"/send", msgBody("ubuntu:bob", "mac:frank", "x", nil, "")); code != 403 {
		t.Fatalf("本机冒充 ubuntu 发消息应 403，得 %d %s", code, body)
	}
	if code, body := remoteDo(t, http.MethodPost, a.base+"/msg/send", a.token, msgBody("mac:frank", "ubuntu:bob", "x", nil, "")); code != 403 {
		t.Fatalf("远端冒充 mac 发消息应 403，得 %d %s", code, body)
	}
	// 远端以自己身份发没问题；地址格式不对应 400。
	if code, body := remoteDo(t, http.MethodPost, a.base+"/msg/send", a.token, msgBody("ubuntu:bob", "mac:frank", "ok", nil, "")); code != 200 {
		t.Fatalf("远端发消息应 200，得 %d %s", code, body)
	}
	if code, _ := post(t, a.lURL+"/send", msgBody("bob", "mac:frank", "x", nil, "")); code != 400 {
		t.Fatalf("坏地址应 400，得 %d", code)
	}
	// 远端不带令牌应 401（guard 拦在最前）。
	if code, _ := remoteDo(t, http.MethodPost, a.base+"/msg/send", "", msgBody("ubuntu:bob", "mac:frank", "x", nil, "")); code != 401 {
		t.Fatalf("远端无令牌应 401，得 %d", code)
	}
}

// TestMsgOutboxFlow ubuntu 发给 mac 的消息进 outbox，本机长轮询取件（带附件）并确认。
func TestMsgOutboxFlow(t *testing.T) {
	a := newMsgAPI(t, 30)

	file := []byte("附件内容\x00\xff")
	code, body := remoteDo(t, http.MethodPost, a.base+"/msg/send", a.token,
		msgBody("ubuntu:bob", "mac:frank", "带附件", file, "/tmp/report.txt"))
	if code != 200 {
		t.Fatalf("远端发带附件消息: %d %s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	id := resp["id"]
	// 列表里的 file_name 应已被剥成基名。
	if got := listMsg(t, a.lURL); len(got.Messages) != 1 || got.Messages[0].FileName != "report.txt" {
		t.Fatalf("列表 file_name 不对: %+v", got.Messages)
	}

	if code, body = post(t, a.lURL+"/decide?id="+id+"&action=approve", nil); code != 200 {
		t.Fatalf("approve: %d %s", code, body)
	}

	gcode, gbody := get(t, a.lURL+"/outbox?to=mac:frank")
	if gcode != 200 {
		t.Fatalf("取件应 200，得 %d %s", gcode, gbody)
	}
	var out struct {
		ID       string `json:"id"`
		To       string `json:"to"`
		Text     string `json:"text"`
		FileName string `json:"file_name"`
		File     string `json:"file"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(gbody, &out); err != nil {
		t.Fatalf("解取件响应: %v %s", err, gbody)
	}
	if out.ID != id || out.To != "mac:frank" || out.FileName != "report.txt" {
		t.Fatalf("取件响应不对: %+v", out)
	}
	back, err := base64.StdEncoding.DecodeString(out.File)
	if err != nil || !bytes.Equal(back, file) {
		t.Fatalf("附件 base64 还原不对: %v %q", err, back)
	}

	// 取走即从 outbox 移除，再取会超时 204。
	if code, _ = get(t, a.lURL+"/outbox?to=mac:frank"); code != http.StatusNoContent {
		t.Fatalf("取空了应 204，得 %d", code)
	}

	if code, body = post(t, a.lURL+"/delivered?id="+id, nil); code != 200 {
		t.Fatalf("确认送达: %d %s", code, body)
	}
	if st := msgStateOf(t, a.lURL, id); st != MsgDelivered {
		t.Fatalf("确认后状态应 delivered，得 %s", st)
	}
	// 重复确认 409；不存在的 id 404。
	if code, _ := post(t, a.lURL+"/delivered?id="+id, nil); code != 409 {
		t.Fatalf("重复确认应 409，得 %d", code)
	}
	if code, _ := post(t, a.lURL+"/delivered?id=nope", nil); code != 404 {
		t.Fatalf("不存在的 id 应 404，得 %d", code)
	}
	// 坏地址 / 另一侧地址不允许取件。
	if code, _ := get(t, a.lURL+"/outbox?to=ubuntu:bob"); code != 403 {
		t.Fatalf("取别人的 outbox 应 403，得 %d", code)
	}
}

// TestMsgWhoAndState 远端上报忙闲，本机 /msg 和远端 /msg/who 都能看到。
func TestMsgWhoAndState(t *testing.T) {
	a := newMsgAPI(t, 30)

	if code, body := remoteDo(t, http.MethodPost, a.base+"/msg/state?addr=ubuntu:bob&state=busy", a.token, nil); code != 200 {
		t.Fatalf("上报忙: %d %s", code, body)
	}
	if got := listMsg(t, a.lURL).Peers["ubuntu:bob"]; got != "busy" {
		t.Fatalf("peers 里应看到 busy，得 %q", got)
	}
	code, body := remoteDo(t, http.MethodGet, a.base+"/msg/who", a.token, nil)
	if code != 200 {
		t.Fatalf("/msg/who: %d %s", code, body)
	}
	var who struct {
		Peers map[string]string `json:"peers"`
	}
	if err := json.Unmarshal(body, &who); err != nil || who.Peers["ubuntu:bob"] != "busy" {
		t.Fatalf("/msg/who 不对: %s", body)
	}
	// 本机上报也要过本机前缀（mac:），上报 ubuntu 的忙闲应 403。
	if code, body = post(t, a.lURL+"/state?addr=ubuntu:bob&state=idle", nil); code != 403 {
		t.Fatalf("本机上报 ubuntu 状态应 403，得 %d %s", code, body)
	}
	if code, body = post(t, a.lURL+"/state?addr=mac:frank&state=idle", nil); code != 200 {
		t.Fatalf("本机上报自己: %d %s", code, body)
	}
	if code, _ := post(t, a.lURL+"/state?addr=mac:frank&state=xx", nil); code != 400 {
		t.Fatalf("坏状态应 400，得 %d", code)
	}
}

// TestMsgAuto 自动模式开关和越界。
func TestMsgAuto(t *testing.T) {
	a := newMsgAPI(t, 30)

	if code, _ := post(t, a.lURL+"/auto?minutes=abc", nil); code != 400 {
		t.Fatalf("非整数应 400，得 %d", code)
	}
	if code, _ := post(t, a.lURL+"/auto?minutes=-1", nil); code != 400 {
		t.Fatalf("负数应 400，得 %d", code)
	}
	if code, _ := post(t, a.lURL+"/auto?minutes=99999999999999999999", nil); code != 400 {
		t.Fatalf("溢出应 400，得 %d", code)
	}

	code, body := post(t, a.lURL+"/auto?minutes=60", nil)
	if code != 200 {
		t.Fatalf("开自动: %d %s", code, body)
	}
	var auto struct {
		AutoUntil int64 `json:"auto_until"`
	}
	if err := json.Unmarshal(body, &auto); err != nil || auto.AutoUntil <= 0 {
		t.Fatalf("开自动应返回未来的 auto_until: %s", body)
	}
	if got := listMsg(t, a.lURL).AutoUntil; got != auto.AutoUntil {
		t.Fatalf("/msg 的 auto_until 应一致：%d vs %d", got, auto.AutoUntil)
	}

	// 自动模式开着，新消息直接 queued（不等批准）。
	code, body = post(t, a.lURL+"/send", msgBody("mac:frank", "ubuntu:bob", "自动单", nil, ""))
	if code != 200 {
		t.Fatalf("自动模式发消息: %d %s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "自动模式送达", func() bool {
		return msgStateOf(t, a.lURL, resp["id"]) == MsgDelivered
	})

	// 关掉。
	if code, body = post(t, a.lURL+"/auto?minutes=0", nil); code != 200 {
		t.Fatalf("关自动: %d %s", code, body)
	}
	if got := listMsg(t, a.lURL).AutoUntil; got != 0 {
		t.Fatalf("关掉后 auto_until 应为 0，得 %d", got)
	}
}

// TestMsgBodyTooLarge 正文超过 2MB 应 413。
func TestMsgBodyTooLarge(t *testing.T) {
	a := newMsgAPI(t, 30)
	big := msgBody("mac:frank", "ubuntu:bob", strings.Repeat("啊", 1<<20), nil, "")
	if code, _ := post(t, a.lURL+"/send", big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大正文应 413，得 %d", code)
	}
}

// TestMsgPumpLifecycle 协助没开时消息入库不推进；开启后由推进循环送达；Stop 后循环退出。
func TestMsgPumpLifecycle(t *testing.T) {
	oldP, oldW := msgPumpInterval, outboxWait
	msgPumpInterval = 20 * time.Millisecond
	outboxWait = 200 * time.Millisecond
	t.Cleanup(func() { msgPumpInterval, outboxWait = oldP, oldW })

	var mu sync.Mutex
	var n int
	s, _ := newTestService(t, Config{
		MsgDeliver: func(_ context.Context, m Message) error {
			mu.Lock()
			defer mu.Unlock()
			n++
			return nil
		},
	})
	lURL := newServer(t, s.LocalHandler()).URL + "/msg"

	// 没开协助：能入库、能批准，但不推进。
	if _, err := s.Hub().Send(Message{From: "mac:frank", To: "ubuntu:bob", Text: "攒着"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := post(t, lURL+"/auto?minutes=0", nil); code != 200 {
		t.Fatalf("查关自动不应失败")
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	got := n
	mu.Unlock()
	if got != 0 {
		t.Fatalf("协助没开不该推进，Deliver 被调了 %d 次", got)
	}

	// 没开协助时 approve 应失败（hub 里还是 pending），确认没绕过批准。
	if code, _ := post(t, lURL+"/decide?id=nope&action=approve", nil); code != 404 {
		t.Fatalf("不存在的 id 应 404，得 %d", code)
	}

	if err := s.Start(30); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 让 pending 那条批准：靠推进循环送达。
	id := s.Hub().List()[0].ID
	if code, body := post(t, lURL+"/decide?id="+id+"&action=approve", nil); code != 200 {
		t.Fatalf("approve: %d %s", code, body)
	}
	waitFor(t, 3*time.Second, "开启后送达", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return n == 1
	})

	s.Stop("测完")

	// Stop 后循环退出：再发消息批准也不该再送达。
	if _, err := s.Hub().Send(Message{From: "mac:frank", To: "ubuntu:bob", Text: "停后再发"}); err != nil {
		t.Fatal(err)
	}
	id2 := s.Hub().List()[0].ID
	if code, body := post(t, lURL+"/decide?id="+id2+"&action=approve", nil); code != 200 {
		t.Fatalf("approve: %d %s", code, body)
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	got = n
	mu.Unlock()
	if got != 1 {
		t.Fatalf("Stop 后 Pump 应退出，却送达了第 %d 条", got)
	}
	// 本机接口在协助结束后仍可读。
	if got := listMsg(t, lURL); len(got.Messages) != 2 {
		t.Fatalf("列表应有 2 条，得 %d", len(got.Messages))
	}
}

// TestMsgHandlersRejectWrongMethod /msg/send 用 GET 等方法应 405。
func TestMsgHandlersRejectWrongMethod(t *testing.T) {
	a := newMsgAPI(t, 30)
	if code, _ := get(t, a.lURL+"/send"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /msg/send 应 405，得 %d", code)
	}
	if code, _ := post(t, a.lURL, nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /msg 应 405，得 %d", code)
	}
}
