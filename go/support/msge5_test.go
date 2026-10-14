package support

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestE5PersistSnapshotOrder：两次 persist 的序列化窗口交错时，后拍的快照不能被先拍的覆盖。
// 注入 marshalSnap：第一次序列化阻塞到第二次序列化完成为止——先拍的快照（序号小）
// 晚写完，flush 必须丢弃它，盘上留下的必须是后拍的那份。
func TestE5PersistSnapshotOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	h := NewHub(HubConfig{StorePath: path})

	var mu sync.Mutex
	calls := 0
	started := make(chan struct{})
	release := make(chan struct{})
	orig := marshalSnap
	marshalSnap = func(v any) ([]byte, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(started)
			<-release // 先拍的快照卡在序列化里，让后拍的先落盘
		}
		return orig(v)
	}
	defer func() { marshalSnap = orig }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.SetAuto(60); err != nil { // 第一次 persist：快照含 auto_until=+60min
			t.Errorf("SetAuto(60): %v", err)
		}
	}()
	<-started // 第一次序列化已开始（快照已在锁内拍好、序号已定）

	if err := h.SetAuto(0); err != nil { // 第二次 persist：快照亮照拍（auto_until=零），先落盘
		t.Fatalf("SetAuto(0): %v", err)
	}
	close(release)
	wg.Wait() // 第一次序列化结束：它晚到，必须被序号丢弃

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap hubSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.AutoUntil.IsZero() {
		t.Errorf("先拍的快照（auto_until=%v）覆盖了后拍的：盘上 auto_until 应为零", snap.AutoUntil)
	}
}

// TestE5InjectingPersistedBeforeDeliver：pumpOne 调 Deliver 前，injecting 标记必须
// 已经真正写进盘——Deliver 回调里读文件应能看到这条消息带 injecting 标记；
// 注入结束落盘后标记清掉。
func TestE5InjectingPersistedBeforeDeliver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	var sawInjecting, sawQueued bool
	h := NewHub(HubConfig{StorePath: path, Deliver: func(_ context.Context, m Message) error {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("Deliver 期间读快照失败: %v", err)
			return nil
		}
		var snap hubSnapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			t.Errorf("Deliver 期间解析快照失败: %v", err)
			return nil
		}
		sawInjecting = snap.Injecting[m.ID]
		for _, sm := range snap.Messages {
			if sm.ID == m.ID {
				sawQueued = sm.State == MsgQueued
			}
		}
		return nil
	}})
	id, err := h.Send(Message{From: "ubuntu:a", To: "ubuntu:b", Kind: KindNotice, Text: "注入前查盘"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}
	if err := h.SetState("ubuntu:b", PeerIdle); err != nil {
		t.Fatal(err)
	}
	h.pumpOne(context.Background(), id)

	if !sawInjecting {
		t.Error("Deliver 被调时盘上没有 injecting 标记（注入前没落盘）")
	}
	if !sawQueued {
		t.Error("Deliver 被调时盘上消息状态不是 queued")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap hubSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Injecting[id] {
		t.Error("注入结束后盘上 injecting 标记没清掉")
	}
}

// TestE5ClientIDDedupBeforeReplyChecks：reply 带 client_id 重发两次——
// 第二次必须在「已经回复过了」等一切校验之前被幂等吸收，返回同一个 ID 且不报错。
func TestE5ClientIDDedupBeforeReplyChecks(t *testing.T) {
	h := NewHub(HubConfig{})
	reqID, err := h.Send(Message{From: "ubuntu:a", To: "ubuntu:b", Kind: KindRequest, Text: "做个事"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(reqID, "now"); err != nil {
		t.Fatal(err)
	}
	if err := h.MarkDelivered(reqID); err != nil { // Now 强送：直接 delivered
		t.Fatal(err)
	}

	id1, err := h.Send(Message{From: "ubuntu:b", Kind: KindReply, ReplyTo: reqID,
		Text: "第一次回复", ClientID: "c-1"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := h.Send(Message{From: "ubuntu:b", Kind: KindReply, ReplyTo: reqID,
		Text: "第一次回复", ClientID: "c-1"})
	if err != nil {
		t.Fatalf("同 client_id 重发被拒: %v", err)
	}
	if id2 != id1 {
		t.Errorf("重发返回了新 ID %s，原 ID %s", id2, id1)
	}
	// 换个 client_id 才是真重复回复，照常拦下
	if _, err := h.Send(Message{From: "ubuntu:b", Kind: KindReply, ReplyTo: reqID,
		Text: "换幂等 ID 的第二次回复", ClientID: "c-2"}); err == nil {
		t.Error("不同 client_id 的重复 reply 没有被拦下")
	}
}

// TestE5OrphanAttachmentCleanupOnLoad：加载快照时清理 .files/ 下不属于任何未结束消息
// 的文件（崩溃窗口留下的孤儿）；未结束消息的附件必须原样保留。
func TestE5OrphanAttachmentCleanupOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	filesDir := path + ".files"
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("queued 消息的附件")
	for name, data := range map[string][]byte{
		"q1":     body, // 未结束（queued）：保留
		"d1":     []byte("已结束的消息"),
		"orphan": []byte("没有任何消息认领"),
	} {
		if err := os.WriteFile(filepath.Join(filesDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap := hubSnapshot{
		Version: hubStoreVersion,
		Messages: []storedMsg{
			{Message: Message{ID: "q1", From: "ubuntu:a", To: "ubuntu:b", Kind: KindNotice, Text: "还没完", State: MsgQueued}, HasFile: true},
			{Message: Message{ID: "d1", From: "ubuntu:a", To: "ubuntu:b", Kind: KindNotice, Text: "已送达", State: MsgDelivered}},
		},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewHub(HubConfig{StorePath: path})
	if _, err := os.Stat(filepath.Join(filesDir, "q1")); err != nil {
		t.Errorf("未结束消息的附件被删了: %v", err)
	}
	for _, gone := range []string{"d1", "orphan"} {
		if _, err := os.Stat(filepath.Join(filesDir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s 该被当孤儿清掉，却还在（err=%v）", gone, err)
		}
	}
	// 未结束消息的附件内容原样读回
	for _, m := range h.msgs {
		if m.ID == "q1" && string(m.File) != string(body) {
			t.Errorf("q1 附件内容读回不对: %q", m.File)
		}
	}
}
