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
	"strings"
	"time"
)

// hubStoreVersion 是消息快照文件的格式版本；加载时不等于它的文件按损坏处理。
const hubStoreVersion = 1

// hubSnapshot 是落盘的最小状态集：消息（含附件、Attempts/Report，Message 上的
// ClientID 随之进出快照）、对端状态、自动模式、欠回复计数，
// 重试预算与线路暂停进度（firstFail/pausedAtFirst 键为 id+"|"+stage，按阶段各算各的；
// linkDown/pausedTotal，重启后预算和暂停累计接着算，不能从头再来），
// 以及送达确认与欠回复超时的进度（injectedAt/ackSeq/deliveredAt）。
// takenAt/outbox/outPlaced/inflight/nextTry/fails/peerSeq/hookBusySeq/ackBase 不存：
// 重启后 queued 消息由下一次 Pump 正常重新处理（发往 mac: 的重新进 outbox），
// injected 消息的 ackSeq 归零、injectedAt 记成加载时刻——重启后重新等满 AckTimeout、
// 等一次钩子 busy 上报才算确认（见 loadStore），因为重启期间对端有没有上报已无从判断。
type hubSnapshot struct {
	Version       int                      `json:"version"`
	Messages      []storedMsg              `json:"messages"`
	Peers         map[string]PeerState     `json:"peers"`
	PeerAt        map[string]time.Time     `json:"peer_at"`
	AutoUntil     time.Time                `json:"auto_until"`
	Nagged        map[string]int           `json:"nagged"`
	LinkDown      bool                     `json:"link_down"`
	PausedSince   time.Time                `json:"paused_since"`
	PausedTotal   time.Duration            `json:"paused_total"`
	FirstFail     map[string]time.Time     `json:"first_fail"`
	PausedAtFirst map[string]time.Duration `json:"paused_at_first"`
	InjectedAt    map[string]time.Time     `json:"injected_at,omitempty"`
	AckSeq        map[string]uint64        `json:"ack_seq,omitempty"`
	DeliveredAt   map[string]time.Time     `json:"delivered_at,omitempty"`
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
	injectedAt := make(map[string]time.Time, len(h.injectedAt))
	for k, v := range h.injectedAt {
		injectedAt[k] = v
	}
	ackSeq := make(map[string]uint64, len(h.ackSeq))
	for k, v := range h.ackSeq {
		ackSeq[k] = v
	}
	deliveredAt := make(map[string]time.Time, len(h.deliveredAt))
	for k, v := range h.deliveredAt {
		deliveredAt[k] = v
	}
	b, err := json.Marshal(hubSnapshot{
		Version:       hubStoreVersion,
		Messages:      msgs,
		Peers:         peers,
		PeerAt:        peerAt,
		AutoUntil:     h.autoUntil,
		Nagged:        nagged,
		LinkDown:      h.linkDown,
		PausedSince:   h.pausedSince,
		PausedTotal:   h.pausedTotal,
		FirstFail:     h.firstFail,
		PausedAtFirst: h.pausedAtFirst,
		InjectedAt:    injectedAt,
		AckSeq:        ackSeq,
		DeliveredAt:   deliveredAt,
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

// persist 持锁取快照、解锁后落盘。凡是改了消息/对端状态/自动模式/nagged/送达确认进度的
// 公开方法（Send、SetState、Decide、SetAuto、TakeOutbox 取走、MarkDelivered、
// Pump 的确认推进与欠回复超时、pumpOne 各结果分支、trim 淘汰经由 Send）都在解锁后调它。
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
	// 预算键是 id+"|"+stage；老快照里裸 id 的键（按消息共享窗口的旧格式）没有
	// 阶段归属，读回也用不上，直接丢弃，免得在 map 里越积越多。
	h.firstFail = make(map[string]time.Time, len(snap.FirstFail))
	for k, v := range snap.FirstFail {
		if !strings.Contains(k, "|") {
			continue
		}
		h.firstFail[k] = v
	}
	h.pausedAtFirst = make(map[string]time.Duration, len(snap.PausedAtFirst))
	for k, v := range snap.PausedAtFirst {
		if !strings.Contains(k, "|") {
			continue
		}
		h.pausedAtFirst[k] = v
	}
	h.autoUntil = snap.AutoUntil
	h.linkDown = snap.LinkDown
	h.pausedSince = snap.PausedSince
	h.pausedTotal = snap.PausedTotal
	// 送达确认与欠回复超时的进度：injected 消息重新等满 AckTimeout
	// （重启期间对端到底上报过没有已无从判断，ackSeq 归零等于"下一次上报就算确认"，
	// injectedAt 记加载时刻让计时从头开始）；delivered 的起算点原样恢复，
	// 老快照里没记过的退回创建时间，至少不会一重启就误报超时之外的极端情况。
	h.injectedAt = make(map[string]time.Time)
	h.ackSeq = make(map[string]uint64)
	h.deliveredAt = make(map[string]time.Time, len(snap.DeliveredAt))
	now := h.now()
	for _, m := range h.msgs {
		switch m.State {
		case MsgInjected:
			h.injectedAt[m.ID] = now
			h.ackSeq[m.ID] = 0
		case MsgDelivered:
			if at, ok := snap.DeliveredAt[m.ID]; ok {
				h.deliveredAt[m.ID] = at
			} else {
				h.deliveredAt[m.ID] = m.Created
			}
		}
	}
	// takenAt/outbox/outPlaced/inflight/nextTry/fails/peerSeq 保持 NewHub 给的空表：
	// 加载出的 queued 消息会被下一次 Pump 正常处理，不保留"已放进 outbox"的痕迹；
	// lastProbe 归零——重启后第一次 Pump 正好立刻放一条探测。
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
