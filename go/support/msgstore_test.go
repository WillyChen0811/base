package support

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		{"version_mismatch", `{"version":99,"messages":[]}`, "corrupt_version"},
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

// TestStoreIdlePumpNoRewrite：大附件消息已入库落盘后，对方 busy 导致 Pump 空转多轮
// ——每一轮都没改任何落盘状态，靠 dirty 标记挡住，快照文件一次都不该重写
// （写次数计数器 writtenSeq 不涨、文件 mtime 不变；不再靠序列化比字节）。
func TestStoreIdlePumpNoRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	h := NewHub(HubConfig{
		StorePath: path,
		Deliver:   func(context.Context, Message) error { return nil },
	})
	if err := h.SetState("ubuntu:bob", PeerBusy); err != nil { // busy：非 Now 消息按住
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("X"), 256*1024)
	if _, err := h.Send(Message{From: "ubuntu:a", To: "ubuntu:bob", Kind: KindNotice,
		Text: "大附件消息", FileName: "big.bin", File: big}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("入库后快照应已落盘: %v", err)
	}
	writes := h.writtenSeq
	if writes == 0 {
		t.Fatal("还没有过成功写盘")
	}
	mtime := st.ModTime()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		h.Pump(ctx)
	}
	if h.writtenSeq != writes {
		t.Fatalf("Pump 空转把快照又写了 %d 次（writtenSeq %d→%d）", h.writtenSeq-writes, writes, h.writtenSeq)
	}
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(mtime) {
		t.Fatalf("空转期间快照文件被重写: mtime %v → %v", mtime, st2.ModTime())
	}
}

// TestStoreAttachmentFileLifecycle：附件单独存 <StorePath>.files/<id>（0700/0600），
// 首次入库写一次；送达（附件释放）后删文件；快照只记 has_file，不内嵌正文。
func TestStoreAttachmentFileLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	attach := []byte("attachment lifecycle 正文")
	var delivered [][]byte
	var dmu sync.Mutex
	h := NewHub(HubConfig{
		StorePath: path,
		Deliver: func(_ context.Context, m Message) error {
			dmu.Lock()
			defer dmu.Unlock()
			delivered = append(delivered, append([]byte(nil), m.File...))
			return nil
		},
	})
	if err := h.SetState("ubuntu:bob", PeerIdle); err != nil {
		t.Fatal(err)
	}
	id, err := h.Send(Message{From: "ubuntu:a", To: "ubuntu:bob", Kind: KindNotice,
		Text: "带附件", FileName: "note.txt", File: attach})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Decide(id, "approve"); err != nil {
		t.Fatal(err)
	}

	attachPath := path + ".files/" + id
	body, err := os.ReadFile(attachPath)
	if err != nil {
		t.Fatalf("入库后附件文件应在: %v", err)
	}
	if !bytes.Equal(body, attach) {
		t.Fatalf("附件文件内容不一致: %q", body)
	}
	if st, err := os.Stat(filepath.Dir(attachPath)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("附件目录应 0700: %v %v", st, err)
	}
	if st, err := os.Stat(attachPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("附件文件应 0600: %v %v", st, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.Pump(ctx) // Deliver 成功 → injected（还没钩子确认，附件必须还在）
	dmu.Lock()
	nDeliver := len(delivered)
	dmu.Unlock()
	if nDeliver != 1 || !bytes.Equal(delivered[0], attach) {
		t.Fatalf("Deliver 收到的附件不对: %d 段", nDeliver)
	}
	snapB, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapB), `"has_file":true`) {
		t.Fatalf("快照应记 has_file: %s", snapB)
	}
	if strings.Contains(string(snapB), base64.StdEncoding.EncodeToString(attach)) {
		t.Fatal("V2 快照不该再内嵌附件正文")
	}
	if _, err := os.Stat(attachPath); err != nil {
		t.Fatalf("injected 阶段附件文件应还在: %v", err)
	}

	if err := h.SetState("ubuntu:bob", PeerBusy); err != nil { // 钩子报忙 → 确认送达
		t.Fatal(err)
	}
	if lst := h.List(); len(lst) != 1 || lst[0].State != MsgDelivered {
		t.Fatalf("应已送达: %+v", lst)
	}
	if _, err := os.Stat(attachPath); !os.IsNotExist(err) {
		t.Fatalf("送达释放后附件文件应被删: %v", err)
	}
	if h.hasFile[id] {
		t.Fatal("附件文件删了还留着 hasFile 账")
	}
	snapB, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap hubSnapshot
	if err := json.Unmarshal(snapB, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 1 || snap.Messages[0].HasFile {
		t.Fatalf("送达后快照不该再记 has_file: %s", snapB)
	}
}

// TestStoreV1LegacyMigratesAttachments：手写的 V1 旧快照（附件内嵌 file 字段）
// 要能加载：附件读回内存并迁移成 <StorePath>.files/<id> 文件；
// 之后的落盘升到 V2（has_file 标记、不再内嵌），新 Hub 从文件读回附件。
func TestStoreV1LegacyMigratesAttachments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	attach := []byte("旧格式里的附件 old attachment")
	v1 := `{"version":1,"messages":[{"id":"aa11bb22cc33dd44","from":"ubuntu:a","to":"ubuntu:bob",` +
		`"text":"旧格式带附件","kind":"notice","state":"queued","created":"2026-01-02T03:04:05Z",` +
		`"file_name":"old.txt","file":"` + base64.StdEncoding.EncodeToString(attach) + `"}],` +
		`"peers":{"ubuntu:bob":"idle"}}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewHub(HubConfig{StorePath: path})
	h.mu.Lock()
	var loaded Message
	if len(h.msgs) == 1 {
		loaded = h.msgs[0]
	}
	h.mu.Unlock()
	if loaded.ID != "aa11bb22cc33dd44" || loaded.State != MsgQueued {
		t.Fatalf("V1 快照没加载对: %+v", loaded)
	}
	if !bytes.Equal(loaded.File, attach) {
		t.Fatalf("V1 内嵌附件没读回: %q", loaded.File)
	}
	body, err := os.ReadFile(path + ".files/aa11bb22cc33dd44")
	if err != nil {
		t.Fatalf("附件没迁移成文件: %v", err)
	}
	if !bytes.Equal(body, attach) {
		t.Fatalf("迁移出的附件内容不一致: %q", body)
	}

	if err := h.SetAuto(10); err != nil { // 触发一次落盘：应升成 V2
		t.Fatal(err)
	}
	snapB, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap hubSnapshot
	if err := json.Unmarshal(snapB, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != hubStoreVersion {
		t.Fatalf("迁移后应落 V2: %d", snap.Version)
	}
	if len(snap.Messages) != 1 || !snap.Messages[0].HasFile || len(snap.Messages[0].File) != 0 {
		t.Fatalf("迁移后快照应只记 has_file、不内嵌附件: %s", snapB)
	}
	if strings.Contains(string(snapB), base64.StdEncoding.EncodeToString(attach)) {
		t.Fatal("V2 快照不该再内嵌附件正文")
	}

	// 再重启一次：V2 + 文件里的附件完好，能继续投递
	h2 := NewHub(HubConfig{StorePath: path})
	h2.mu.Lock()
	var back []byte
	if len(h2.msgs) == 1 {
		back = h2.msgs[0].File
	}
	h2.mu.Unlock()
	if !bytes.Equal(back, attach) {
		t.Fatalf("迁移后重启附件没读回: %q", back)
	}
}

// TestStoreUnreadablePreserved：快照文件读不了（chmod 000，非 root）时
// 必须改名 <path>.unreadable-<秒> 保住原内容再从空开始，绝不原地覆盖；
// 之后新快照照常写出去。
func TestStoreUnreadablePreserved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下 chmod 000 挡不住读，测不了这个分支")
	}
	path := filepath.Join(t.TempDir(), "hub.json")
	h1 := NewHub(HubConfig{StorePath: path})
	if _, err := h1.Send(Message{From: "ubuntu:a", To: "mac:bob", Text: "旧数据要保住"}); err != nil {
		t.Fatal(err)
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	var log storeLog
	h2 := NewHub(HubConfig{StorePath: path, Log: log.hook})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("读不了的文件应已改名挪走, stat: %v", err)
	}
	matches, err := filepath.Glob(path + ".unreadable-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("应恰好留一份 .unreadable-* 备份: %v (%v)", matches, err)
	}
	if got, err := os.ReadFile(matches[0]); err != nil || !bytes.Equal(got, orig) {
		t.Fatalf("备份内容没保住原文件: %q err=%v", got, err)
	}
	if !log.has("unreadable") {
		t.Fatalf("日志缺 unreadable: %v", log.events)
	}
	if len(h2.List()) != 0 {
		t.Fatalf("应从空开始: %+v", h2.List())
	}
	// 保住证据之后照常工作、照常落盘
	if _, err := h2.Send(Message{From: "ubuntu:a", To: "mac:bob", Text: "新数据"}); err != nil {
		t.Fatalf("隔离后 Send 失败: %v", err)
	}
	h3 := NewHub(HubConfig{StorePath: path})
	if n := len(h3.List()); n != 1 {
		t.Fatalf("新快照没能加载: %d 条", n)
	}
}

// TestStoreInjectingFlagFailsAtRestart：手写带 injecting 标记的快照
// （模拟崩溃正好撞上注入中）：加载时 queued 转 failed、Reason 说明要人工确认、
// 报告 Stage=deliver，Pump 不许再自动注入；标记消费掉之后落的新快照不再带它，
// 再重启一次仍是 failed，不会翻烧饼。
func TestStoreInjectingFlagFailsAtRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	snapJSON := `{"version":2,"messages":[{"id":"bb22cc33dd44ee55","from":"ubuntu:a","to":"ubuntu:bob",` +
		`"text":"注入时崩了","kind":"notice","state":"queued","created":"2026-01-02T03:04:05Z"}],` +
		`"peers":{"ubuntu:bob":"idle"},"injecting":{"bb22cc33dd44ee55":true}}`
	if err := os.WriteFile(path, []byte(snapJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	var delivers int
	var dmu sync.Mutex
	h := NewHub(HubConfig{StorePath: path, Deliver: func(context.Context, Message) error {
		dmu.Lock()
		defer dmu.Unlock()
		delivers++
		return nil
	}})
	lst := h.List()
	if len(lst) != 1 {
		t.Fatalf("应加载 1 条: %+v", lst)
	}
	m := lst[0]
	if m.State != MsgFailed || m.Reason != attachingAtRestartReason {
		t.Fatalf("injecting 标记的 queued 应转 failed 带原因: %+v", m)
	}
	if m.Report == nil || m.Report.Stage != "deliver" {
		t.Fatalf("报告应 Stage=deliver: %+v", m.Report)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.Pump(ctx) // failed 不再自动注入
	dmu.Lock()
	n := delivers
	dmu.Unlock()
	if n != 0 {
		t.Fatalf("转人工后不许再自动注入，注入了 %d 次", n)
	}

	if err := h.SetState("ubuntu:bob", PeerIdle); err != nil { // 触发落盘
		t.Fatal(err)
	}
	h2 := NewHub(HubConfig{StorePath: path, Deliver: func(context.Context, Message) error { return nil }})
	lst2 := h2.List()
	if len(lst2) != 1 || lst2[0].State != MsgFailed || lst2[0].Reason != attachingAtRestartReason {
		t.Fatalf("重启后应保持人工确认状态: %+v", lst2)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap hubSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Injecting["bb22cc33dd44ee55"] {
		t.Fatal("注入已结束，新快照不该还带 injecting 标记")
	}
}
