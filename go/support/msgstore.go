package support

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// hubStoreVersion 是消息快照文件的格式版本；加载时不等于它的文件按损坏处理。
const hubStoreVersion = 1

// hubSnapshot 是落盘的最小状态集：消息（含附件）、对端状态、自动模式、欠回复计数。
// takenAt/outbox/outPlaced/inflight/nextTry/fails/peerSeq 不存：
// 重启后 queued 消息由下一次 Pump 正常重新处理（发往 mac: 的重新进 outbox）。
type hubSnapshot struct {
	Version   int                  `json:"version"`
	Messages  []storedMsg          `json:"messages"`
	Peers     map[string]PeerState `json:"peers"`
	PeerAt    map[string]time.Time `json:"peer_at"`
	AutoUntil time.Time            `json:"auto_until"`
	Nagged    map[string]int       `json:"nagged"`
}

// storedMsg 包一层 Message 单独存附件：Message.File 是 json:"-"（对外 JSON 不带正文），
// 这里的 File 在外层、深度更浅，编码为 base64，重启后附件还在。
type storedMsg struct {
	Message
	File []byte `json:"file,omitempty"`
}

// logStoreError 记落盘错误——落盘失败不影响内存状态，只留日志；必须在锁外调用。
func (h *Hub) logStoreError(event string, err error) {
	if h.cfg.Log == nil {
		return
	}
	h.cfg.Log("msg_store_error", map[string]string{
		"event": event,
		"path":  h.storePath,
		"error": err.Error(),
	})
}

// saveLocked 把当前状态快照成 JSON 字节并递增序号，等调用方解锁后由 flush 落盘。
// 内容字节没变就不递增（Pump 每 2 秒推进一次往往什么都没变），避免重复写盘；
// json.Marshal 对 map 按 key 排序输出、time.Time 只序列化墙上钟，字节比较可靠。
// 调用方须持 h.mu；一般经 persist 调用。
func (h *Hub) saveLocked() {
	if h.storePath == "" {
		return
	}
	msgs := make([]storedMsg, len(h.msgs))
	for i, m := range h.msgs {
		msgs[i] = storedMsg{Message: m, File: m.File}
	}
	peers := make(map[string]PeerState, len(h.peers))
	for k, v := range h.peers {
		peers[k] = v
	}
	peerAt := make(map[string]time.Time, len(h.peerAt))
	for k, v := range h.peerAt {
		peerAt[k] = v
	}
	nagged := make(map[string]int, len(h.nagged))
	for k, v := range h.nagged {
		nagged[k] = v
	}
	b, err := json.Marshal(hubSnapshot{
		Version:   hubStoreVersion,
		Messages:  msgs,
		Peers:     peers,
		PeerAt:    peerAt,
		AutoUntil: h.autoUntil,
		Nagged:    nagged,
	})
	if err != nil { // 本结构理论上 marshal 不会失败；真失败了不递增，保住上一次快照
		return
	}
	if bytes.Equal(b, h.snap) {
		return
	}
	h.snapSeq++
	h.snap = b
}

// persist 持锁取快照、解锁后落盘。凡是改了消息/对端状态/自动模式/nagged 的
// 公开方法（Send、SetState、Decide、SetAuto、TakeOutbox 取走、MarkDelivered、
// pumpOne 各结果分支、trim 淘汰经由 Send）都在解锁后调它。
func (h *Hub) persist() {
	if h.storePath == "" {
		return
	}
	h.mu.Lock()
	h.saveLocked()
	h.mu.Unlock()
	h.flush()
}

// flush 把尚未落盘的最近快照原子写出去。saveMu 串行化所有磁盘写，writtenSeq 只增：
// 即使多个 persist 的锁外窗口交错，后落盘的文件内容也一定不比先落盘的旧。
// writtenSeq 只在持 saveMu 时读写。写失败只记日志，内存状态不动。
func (h *Hub) flush() {
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	seq, data := h.snapSeq, h.snap
	h.mu.Unlock()
	if seq == 0 || seq <= h.writtenSeq {
		return
	}
	if err := atomicWriteFile(h.storePath, data); err != nil {
		h.logStoreError("write", err)
		return
	}
	h.writtenSeq = seq
}

// loadStore 读回上次落盘的快照，由 NewHub 在把 Hub 交出去之前调用（尚未共享，不用加锁）。
// 文件不存在 = 正常首次运行；读不了（权限等）= 记日志后从空开始、不碰原文件，下次启动再试；
// JSON 坏了或版本不对 = 隔离成 <path>.corrupt-<Unix 秒> 并记日志，从空开始，绝不 panic。
func (h *Hub) loadStore() {
	b, err := os.ReadFile(h.storePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			h.logStoreError("read", err)
		}
		return
	}
	var snap hubSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		h.quarantineStore("corrupt_json", err)
		return
	}
	if snap.Version != hubStoreVersion {
		h.quarantineStore("corrupt_version",
			fmt.Errorf("快照版本 %d 不支持（当前 %d）", snap.Version, hubStoreVersion))
		return
	}
	h.msgs = make([]Message, 0, len(snap.Messages))
	for _, sm := range snap.Messages {
		m := sm.Message
		m.File = sm.File
		h.msgs = append(h.msgs, m)
	}
	h.peers = make(map[string]PeerState, len(snap.Peers))
	for k, v := range snap.Peers {
		h.peers[k] = v
	}
	h.peerAt = make(map[string]time.Time, len(snap.PeerAt))
	for k, v := range snap.PeerAt {
		h.peerAt[k] = v
	}
	h.nagged = make(map[string]int, len(snap.Nagged))
	for k, v := range snap.Nagged {
		h.nagged[k] = v
	}
	h.autoUntil = snap.AutoUntil
	// takenAt/outbox/outPlaced/inflight/nextTry/fails/peerSeq 保持 NewHub 给的空表：
	// 加载出的 queued 消息会被下一次 Pump 正常处理，不保留"已放进 outbox"的痕迹。
}

// quarantineStore 把坏快照挪成 <path>.corrupt-<Unix 秒>（保证下次启动不再撞上它）并记日志。
func (h *Hub) quarantineStore(event string, err error) {
	backup := h.storePath + ".corrupt-" + strconv.FormatInt(time.Now().Unix(), 10)
	if rerr := os.Rename(h.storePath, backup); rerr != nil {
		h.logStoreError("backup_"+event, fmt.Errorf("挪备份失败: %v（原始错误: %v）", rerr, err))
	}
	h.logStoreError(event, err)
}

// atomicWriteFile 原子写：同目录临时文件（os.CreateTemp 固定 0600）→ 写 → fsync → rename。
// 临时名 ".<base>.tmp*" 是隐藏文件，不会跟 ".corrupt-*" 备份混在一起。
func atomicWriteFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(e error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return e
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o600); err != nil { // 再确认一次 0600，防 umask/平台差异
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
