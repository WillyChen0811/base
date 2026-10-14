package support

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// storeLog 收集 Hub 的 msg_store_error 日志。
type storeLog struct {
	mu     sync.Mutex
	events []string
}

func (l *storeLog) hook(kind string, detail map[string]string) {
	if kind != "msg_store_error" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, detail["event"])
}

func (l *storeLog) has(event string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e == event {
			return true
		}
	}
	return false
}

func (l *storeLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// msgSummary 把消息列表压成可 DeepEqual 的字符串切片（List 已剥 File，这里不含附件）。
func msgSummary(ms []Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%t|%t", m.ID, m.From, m.To, m.State, m.Text, m.Now, m.Answered))
	}
	return out
}

// TestStoreReloadRestoresMessagesPeersAuto：各种状态的消息 + 对端状态 + 自动模式，
// 换 Hub（同路径）后必须原样还在，附件内容经 Pump/Deliver 校验。
func TestStoreReloadRestoresMessagesPeersAuto(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")

	var fmu sync.Mutex
	var gotFiles [][]byte
	deliver := func(_ context.Context, m Message) error {
		fmu.Lock()
		defer fmu.Unlock()
		b := make([]byte, len(m.File))
		copy(b, m.File)
		gotFiles = append(gotFiles, b)
		return nil
	}

	attach := []byte("attachment body 附件")
	h := NewHub(HubConfig{Deliver: deliver, StorePath: path})
	id1, err := h.Send(Message{From: "ubuntu:a", To: "mac:bob", Kind: KindNotice, Text: "待批通知"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := h.Send(Message{From: "ubuntu:a", To: "ubuntu:b", Kind: KindNotice, Text: "带附件等批准", FileName: "note.txt", File: attach})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(id2, "approve"); err != nil { // queued
		t.Fatal(err)
	}
	id3, err := h.Send(Message{From: "mac:bob", To: "ubuntu:a", Kind: KindRequest, Text: "已送达的请求"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(id3, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := h.MarkDelivered(id3); err != nil { // delivered
		t.Fatal(err)
	}
	id4, err := h.Send(Message{From: "ubuntu:a", To: "mac:bob", Kind: KindNotice, Text: "会被拒绝"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(id4, "reject"); err != nil { // rejected
		t.Fatal(err)
	}
	if id1 == "" || id2 == "" || id3 == "" || id4 == "" {
		t.Fatal("Send 没返回 ID")
	}
	if err := h.SetState("mac:bob", PeerBusy); err != nil {
		t.Fatal(err)
	}
	if err := h.SetState("ubuntu:b", PeerIdle); err != nil {
		t.Fatal(err)
	}
	if err := h.SetAuto(60); err != nil {
		t.Fatal(err)
	}
	until := h.AutoUntil()
	if until.IsZero() {
		t.Fatal("自动模式没开")
	}
	wantList := msgSummary(h.List())

	// 模拟 wgtund 重启：同路径新建 Hub
	h2 := NewHub(HubConfig{Deliver: deliver, StorePath: path})
	if got := msgSummary(h2.List()); !reflect.DeepEqual(got, wantList) {
		t.Fatalf("重启后消息不一致:\n got %v\nwant %v", got, wantList)
	}
	if !reflect.DeepEqual(h2.Peers(), h.Peers()) {
		t.Fatalf("重启后对端状态不一致: %v vs %v", h2.Peers(), h.Peers())
	}
	if got := h2.AutoUntil(); !got.Equal(until) {
		t.Fatalf("重启后自动模式不一致: %v vs %v", got, until)
	}

	// 附件：queued 的 id2 在新 Hub 上 Pump 后 Deliver 收到的正文必须原样
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h2.Pump(ctx)
	fmu.Lock()
	defer fmu.Unlock()
	if len(gotFiles) != 1 || !bytes.Equal(gotFiles[0], attach) {
		t.Fatalf("重启后附件不一致: %d 段, %q", len(gotFiles), gotFiles)
	}
}

// TestStoreReloadQueuedMacPumpTake：mac: 方向的 queued 消息不落 outbox/outPlaced，
// 重启后下一次 Pump 要能重新放进 outbox 被取走，附件完好。
func TestStoreReloadQueuedMacPumpTake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	attach := []byte("restart me 正文")

	a := NewHub(HubConfig{StorePath: path})
	if err := a.SetState("mac:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	id, err := a.Send(Message{From: "ubuntu:a", To: "mac:bob", Kind: KindNotice, Text: "mac 收件带附件", FileName: "note.txt", File: attach})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.Pump(ctx) // 进 a 的 outbox（outPlaced 记下）；不取走

	// 重启：outbox/outPlaced 从空开始
	b := NewHub(HubConfig{StorePath: path})
	if lst := b.List(); len(lst) != 1 || lst[0].ID != id || lst[0].State != MsgQueued {
		t.Fatalf("重启后消息状态不对: %+v", lst)
	}
	b.Pump(ctx) // 对端（已恢复为 idle）可送，重新进 outbox
	got, ok := b.TakeOutbox(ctx, "mac:bob")
	if !ok {
		t.Fatal("重启后 queued 的 mac 消息没被重新放进 outbox")
	}
	if got.ID != id {
		t.Fatalf("取到错的消息: %s vs %s", got.ID, id)
	}
	if !bytes.Equal(got.File, attach) {
		t.Fatalf("重启后附件不一致: %q", got.File)
	}
}

// TestStoreCorruptQuarantine：坏 JSON / 不认的版本号 → 文件挪成 .corrupt-*、记日志、从空开始。
func TestStoreCorruptQuarantine(t *testing.T) {
	cases := []struct {
		name    string
		content string
		event   string
	}{
		{"broken_json", "{ not json {{{", "corrupt_json"},
		{"version_mismatch", `{"version":2,"messages":[]}`, "corrupt_version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.json")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			var log storeLog
			h := NewHub(HubConfig{StorePath: path, Log: log.hook})
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("坏文件应已挪走, stat: %v", err)
			}
			matches, err := filepath.Glob(path + ".corrupt-*")
			if err != nil || len(matches) != 1 {
				t.Fatalf("应恰好留一份 .corrupt-* 备份: %v (%v)", matches, err)
			}
			if !log.has(c.event) {
				t.Fatalf("日志缺 %s: %v", c.event, log.events)
			}
			if len(h.List()) != 0 {
				t.Fatalf("应从空开始: %+v", h.List())
			}
			// 从空开始后收发一切正常
			if _, err := h.Send(Message{From: "ubuntu:a", To: "mac:bob", Text: "照常工作"}); err != nil {
				t.Fatalf("隔离坏文件后 Send 失败: %v", err)
			}
			h2 := NewHub(HubConfig{StorePath: path})
			if n := len(h2.List()); n != 1 {
				t.Fatalf("新写的数据没能加载: %d 条", n)
			}
		})
	}
}

// TestStoreFilePerm：落盘文件必须 0600（里面有消息正文和附件）。
func TestStoreFilePerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	h := NewHub(HubConfig{StorePath: path})
	if _, err := h.Send(Message{From: "ubuntu:a", To: "mac:bob", Text: "权限检查"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("落盘文件权限应 0600, 实际 %v", st.Mode())
	}
}

// TestStoreWriteFailureStillSends：StorePath 指向创建不出来的位置（普通文件下面），
// Send 照常成功，错误只进 msg_store_error 日志。
func TestStoreWriteFailureStillSends(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("我是普通文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "sub", "hub.json")

	var log storeLog
	h := NewHub(HubConfig{StorePath: path, Log: log.hook})
	id, err := h.Send(Message{From: "ubuntu:a", To: "mac:bob", Text: "写不进盘也要收"})
	if err != nil {
		t.Fatalf("写盘失败不应影响 Send: %v", err)
	}
	if id == "" {
		t.Fatal("Send 没返回 ID")
	}
	if !log.has("write") {
		t.Fatalf("日志缺 msg_store_error/write: %v", log.events)
	}
	// 内存状态不受影响
	if lst := h.List(); len(lst) != 1 || lst[0].Text != "写不进盘也要收" {
		t.Fatalf("内存消息不对: %+v", lst)
	}
}

// TestStoreConcurrentPersistRace：并发 Send + SetState（-race）不竞态，
// 全部完成后文件能加载且条数正确、没有落盘错误。
func TestStoreConcurrentPersistRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	var log storeLog
	h := NewHub(HubConfig{StorePath: path, Log: log.hook})

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := h.Send(Message{From: "ubuntu:a", To: "mac:bob", Kind: KindNotice, Text: fmt.Sprintf("并发 %d", i)}); err != nil {
				t.Errorf("并发 Send %d 失败: %v", i, err)
			}
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := h.SetState("mac:bob", PeerBusy); err != nil {
				t.Errorf("SetState busy 失败: %v", err)
			}
			if err := h.SetState("mac:bob", PeerIdle); err != nil {
				t.Errorf("SetState idle 失败: %v", err)
			}
			if err := h.SetAuto(i % 5); err != nil {
				t.Errorf("SetAuto 失败: %v", err)
			}
		}(i)
	}
	wg.Wait()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("并发后文件读不出: %v", err)
	}
	var snap hubSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("并发后文件 JSON 坏了: %v", err)
	}
	if snap.Version != hubStoreVersion {
		t.Fatalf("版本不对: %d", snap.Version)
	}
	if len(snap.Messages) != n {
		t.Fatalf("落盘消息 %d 条, 期望 %d", len(snap.Messages), n)
	}
	if len(h.List()) != n {
		t.Fatalf("内存消息 %d 条, 期望 %d", len(h.List()), n)
	}
	if c := log.count(); c != 0 {
		t.Errorf("不该有落盘错误: %v", log.events)
	}
}
