package support

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hubStoreVersion 是消息快照文件的格式版本。
// V2：附件不再内嵌（storedMsg.HasFile 指向 <StorePath>.files/<id>，文件单独原子写）；
// V1（hubStoreVersionV1）附件内嵌在消息里，仍能加载，附件读回时迁移成文件；
// 其余版本按损坏处理。
const hubStoreVersion = 2

// hubStoreVersionV1 是旧格式：附件 base64 内嵌进快照（storedMsg.File）。
const hubStoreVersionV1 = 1

// attachLostReason 是 V2 快照记着 has_file、文件却读不出来时写进 Reason 的提示语。
const attachLostReason = "附件丢失"

// attachingAtRestartReason 是加载时发现 injecting 标记（崩溃/重启发生在注入中途）
// 写进 Reason 的提示语：对方终端里可能已经有这条消息，不能自动重投，只能人工确认。
const attachingAtRestartReason = "重启时正在注入，可能已经送达，请人工确认"

// attachIDSafe 限定能当附件文件名用的消息 ID：纯 [A-Za-z0-9._-]。
// 消息 ID 正常是随机十六进制；这里防的是手改/伪造的快照里带 "../" 之类把文件写到目录外。
var attachIDSafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// hubSnapshot 是落盘的最小状态集：消息（HasFile 指向外部附件文件；Attempts/Report，
// Message 上的 ClientID 随之进出快照）、对端状态、自动模式、欠回复计数，
// 重试预算与线路暂停进度（firstFail/pausedAtFirst 键为 id+"|"+stage，按阶段各算各的；
// linkDown/pausedTotal，重启后预算和暂停累计接着算，不能从头再来），
// 送达确认与欠回复超时的进度（injectedAt/ackSeq/deliveredAt），
// 以及注入进行中的标记（injecting：调 Deliver 前置上、返回后清掉，重启撞上它说明
// 可能注入到一半，转人工不自动重投，见 loadStore）。
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
	Injecting     map[string]bool          `json:"injecting,omitempty"`
}

// storedMsg 包一层 Message：附件不再内嵌，HasFile 表示 <StorePath>.files/<id> 已写入
// （消息首次入库时写一次，释放/淘汰时删文件）；File 只为读 V1 旧快照保留（迁移用），
// V2 不再写出（空切片被 omitempty 省略）。Message.File 仍是 json:"-"，对外 JSON 不带正文。
type storedMsg struct {
	Message
	HasFile bool   `json:"has_file,omitempty"`
	File    []byte `json:"file,omitempty"` // 仅 V1 兼容读；V2 写出恒为空
}

// attachPath 算附件文件路径 <StorePath>.files/<id>；不落盘或 ID 不适合当文件名时返回 false。
// 纯函数不加锁（storePath 只在 loadStore 失败分支改，那时 Hub 尚未共享）。
func (h *Hub) attachPath(id string) (string, bool) {
	if h.storePath == "" || !attachIDSafe.MatchString(id) {
		return "", false
	}
	return filepath.Join(h.storePath+".files", id), true
}

// writeAttachFile 原子写附件文件（目录 0700、文件 0600）。只在消息首次入库和
// V1 快照迁移时调用；锁外调用。
func (h *Hub) writeAttachFile(id string, data []byte) error {
	p, ok := h.attachPath(id)
	if !ok {
		return fmt.Errorf("附件路径不可用（不落盘或消息 ID %q 非法）", id)
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil { // 目录已存在时 MkdirAll 不改权限，这里补一次
		return err
	}
	return atomicWriteFile(p, data)
}

// removeAttachFile 删附件文件（附件释放/消息淘汰时）；文件本来就不在不算错。锁外调用。
func (h *Hub) removeAttachFile(id string) {
	p, ok := h.attachPath(id)
	if !ok {
		return
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.logStoreError("attach_remove", err)
	}
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

// snapshotLocked 在锁内拷出待落盘的快照数据：消息浅拷贝、全部 map 独立拷贝、标量直读，
// 不做任何 JSON 序列化——Marshal 挪到锁外的 persist 里做，缩短持锁时间，
// 也保证解锁后序列化的数据与 h 的后续改动再无共享（值类型/全新 map/不可变 slice）。
// 调用方须持 h.mu；一般经 persist 调用。
func (h *Hub) snapshotLocked() hubSnapshot {
	msgs := make([]storedMsg, len(h.msgs))
	for i, m := range h.msgs {
		msgs[i] = storedMsg{Message: m, HasFile: h.hasFile[m.ID]}
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
	firstFail := make(map[string]time.Time, len(h.firstFail))
	for k, v := range h.firstFail {
		firstFail[k] = v
	}
	pausedAtFirst := make(map[string]time.Duration, len(h.pausedAtFirst))
	for k, v := range h.pausedAtFirst {
		pausedAtFirst[k] = v
	}
	injecting := make(map[string]bool, len(h.injecting))
	for k := range h.injecting {
		injecting[k] = true
	}
	return hubSnapshot{
		Version:       hubStoreVersion,
		Messages:      msgs,
		Peers:         peers,
		PeerAt:        peerAt,
		AutoUntil:     h.autoUntil,
		Nagged:        nagged,
		LinkDown:      h.linkDown,
		PausedSince:   h.pausedSince,
		PausedTotal:   h.pausedTotal,
		FirstFail:     firstFail,
		PausedAtFirst: pausedAtFirst,
		InjectedAt:    injectedAt,
		AckSeq:        ackSeq,
		DeliveredAt:   deliveredAt,
		Injecting:     injecting,
	}
}

// marshalSnap 是快照序列化入口；测试里替换成会阻塞/失败的实现，
// 制造多个 persist 的锁外序列化窗口交错的场景。
var marshalSnap = json.Marshal

// persist 把当前改动落盘：dirty 没置起一次磁盘 I/O 都不碰（是否碰磁盘看状态变化，
// 不靠序列化出来的字节比较）。见 persistNow。
func (h *Hub) persist() {
	// 写失败已由 persistNow 记日志；dirty 保留，下一轮再试。
	_ = h.persistNow()
}

// persistNow 同步落盘当前状态并把写盘错误原样返回（调用方需要"盘上确有这份状态"
// 才继续往下走时用，见 pumpOne 注入前打标记）；不落盘或没有改动返回 nil。
// 序号在 h.mu 内与快照一起分配（nextSnapSeq 只增）：内容归属由拍快照的先后决定，
// 序列化再慢也不会让先拍的内容顶着后拍的序号——flush 按序号比对，
// 序号不比已写盘的新就丢弃，后拍的快照不会被先拍的覆盖。
// 待删附件清单与快照同锁摘下：本轮写出的快照里这些消息已不带 has_file，
// 写盘成功后再删文件（先写后删）；写失败把清单放回，下次写成功再删。
func (h *Hub) persistNow() error {
	if h.storePath == "" {
		return nil
	}
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return nil
	}
	seq := h.nextSnapSeq
	h.nextSnapSeq++
	snap := h.snapshotLocked()
	del := h.pendingFileDelete
	h.pendingFileDelete = nil
	h.dirty = false
	h.mu.Unlock()

	data, err := marshalSnap(snap)
	if err != nil { // 本结构理论上 marshal 不会失败；真失败了保留 dirty 下次重试，保住上一次快照
		h.mu.Lock()
		h.dirty = true
		h.pendingFileDelete = append(del, h.pendingFileDelete...)
		h.mu.Unlock()
		return err
	}
	if err := h.flush(seq, data); err != nil {
		h.mu.Lock()
		h.pendingFileDelete = append(del, h.pendingFileDelete...)
		h.mu.Unlock()
		return err
	}
	for _, id := range del {
		h.removeAttachFile(id)
	}
	return nil
}

// flush 按序号原子写快照。saveMu 串行化所有磁盘写，writtenSeq 只增：
// 序号是拍快照时定的，即使多个 persist 的锁外窗口交错、老的晚到，
// 序号不比已写盘的新就丢弃，落盘的内容一定不比上一次旧。
// writtenSeq 只在持 saveMu 时读写。写失败只记日志并返回给调用方，内存状态不动。
func (h *Hub) flush(seq uint64, data []byte) error {
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	if seq <= h.writtenSeq {
		return nil
	}
	if err := atomicWriteFile(h.storePath, data); err != nil {
		h.logStoreError("write", err)
		return err
	}
	h.writtenSeq = seq
	return nil
}

// loadStore 读回上次落盘的快照，由 NewHub 在把 Hub 交出去之前调用（尚未共享，不用加锁）。
// 文件不存在（含父路径不是目录的 ENOTDIR）= 正常首次运行，之后照常写盘；
// 读不了（权限等）= 把原文件改名 <path>.unreadable-<Unix 秒>
// 保留证据再从空开始——原文件还在，绝不从空开始又落新快照把读不了的旧数据覆盖掉；
// 连改名都失败就本次运行不落盘（storePath 置空）并记日志。
// JSON 坏了或版本不认识 = 隔离成 <path>.corrupt-<Unix 秒> 并记日志，从空开始，绝不 panic。
// V1 快照（附件内嵌）照常加载，附件迁移成单独文件；V2 快照 has_file 而文件读不出
// 的消息按无附件处理、Reason 记「附件丢失」。带 injecting 标记的 queued 消息转人工，
// 不自动重投（见 injectingAtRestartReason）。
// 最后清理 <StorePath>.files/ 下的孤儿附件（见 cleanupOrphanAttachments）。
func (h *Hub) loadStore() {
	b, err := os.ReadFile(h.storePath)
	if err != nil {
		// 不存在、以及父路径根本不是目录（ENOTDIR，比如 StorePath 落在一个普通文件下面）
		// 都等同首次运行：从空开始，之后照常尝试写盘（写不进自会有 write 错误日志）。
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return
		}
		backup := h.storePath + ".unreadable-" + strconv.FormatInt(time.Now().Unix(), 10)
		if rerr := os.Rename(h.storePath, backup); rerr != nil {
			h.logStoreError("unreadable_backup", fmt.Errorf("读不了又挪不走: %v（原始错误: %v）", rerr, err))
			h.storePath = "" // 保住原文件要紧：本次运行不再落盘
			return
		}
		// 备份是给人事后查的：能 chmod 动它的只有属主，补成 0600 让内容可读；
		// chmod 失败无所谓，文件本身已经保住，别让取证性修饰把主流程带崩。
		if cerr := os.Chmod(backup, 0o600); cerr != nil {
			h.logStoreError("unreadable_backup_chmod", cerr)
		}
		h.logStoreError("unreadable", err)
		return
	}
	var snap hubSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		h.quarantineStore("corrupt_json", err)
		return
	}
	if snap.Version != hubStoreVersion && snap.Version != hubStoreVersionV1 {
		h.quarantineStore("corrupt_version",
			fmt.Errorf("快照版本 %d 不支持（当前 %d）", snap.Version, hubStoreVersion))
		return
	}
	legacy := snap.Version == hubStoreVersionV1
	h.msgs = make([]Message, 0, len(snap.Messages))
	for _, sm := range snap.Messages {
		m := sm.Message
		if legacy && len(sm.File) > 0 {
			// V1 旧快照：内嵌附件迁移成单独文件，迁移成功的才记 has_file
			if werr := h.writeAttachFile(m.ID, sm.File); werr != nil {
				h.logStoreError("attach_migrate", werr)
			} else {
				h.hasFile[m.ID] = true
			}
			m.File = sm.File // 附件留在内存里接着用，迁移只是让它换个地方过冬
		} else if sm.HasFile {
			p, ok := h.attachPath(m.ID)
			if !ok {
				h.logStoreError("attach_path", fmt.Errorf("快照里消息 ID %q 不能当附件文件名", m.ID))
				m.File = nil
				m.Reason = attachLostReason
			} else if data, rerr := os.ReadFile(p); rerr != nil {
				h.logStoreError("attach_read", rerr)
				m.File = nil
				m.Reason = attachLostReason
			} else {
				m.File = data
			}
		}
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
	// injecting 标记：崩溃/重启正好撞上注入——消息可能已经进了对方终端也可能没有，
	// 自动重投会在对方窗口里出现两条，一律转人工（failed + 报告 Stage=deliver）等确认。
	for i := range h.msgs {
		m := &h.msgs[i]
		if m.State != MsgQueued || !snap.Injecting[m.ID] {
			continue
		}
		m.State = MsgFailed
		m.Reason = attachingAtRestartReason
		ps := "unknown"
		if st, ok := h.peers[m.To]; ok {
			ps = string(st)
		}
		m.Report = &FailReport{Stage: "deliver", At: now, PeerState: ps}
		h.dirty = true
	}
	if legacy || len(snap.Injecting) > 0 {
		// V1 迁移过的快照、以及消费过 injecting 标记的，都要在下次 persist
		// 落成 V2（injecting 消费完不能再带着旧标记落盘，否则下次重启又会误判）；
		// 若进程在此之前又崩了，文件还是原样，标记照旧保守转人工，不会丢数据。
		h.dirty = true
	}
	// takenAt/outbox/outPlaced/inflight/nextTry/fails/peerSeq/hookBusySeq 保持 NewHub 给的空表：
	// 加载出的 queued 消息会被下一次 Pump 正常处理，不保留"已放进 outbox"的痕迹；
	// lastProbe 归零——重启后第一次 Pump 正好立刻放一条探测。

	h.cleanupOrphanAttachments()
}

// cleanupOrphanAttachments 删掉 <StorePath>.files/ 下不属于任何未结束消息的文件。
// 正常收尾时附件文件跟着消息结束被删；能剩下来的是崩溃窗口：
// 消息先写附件后入库（Send）、快照先写新盘后删附件（persistNow），两步之间崩溃
// 就会留下没有消息认领的文件。未结束消息（pending/queued/injected/sync_failed/failed）
// 的文件必须保留——还要靠它重投/读回；已结束消息的 has_file 早已不在快照里，
// 它们的文件留在盘上只会白占空间。loadStore 末尾调用（尚未共享，不加锁）。
func (h *Hub) cleanupOrphanAttachments() {
	dir := h.storePath + ".files"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			h.logStoreError("attach_dir", err)
		}
		return
	}
	keep := make(map[string]bool)
	for _, m := range h.msgs {
		if m.State != MsgDelivered && m.State != MsgRejected && m.State != MsgRecalled {
			keep[m.ID] = true
		}
	}
	for _, e := range entries {
		if e.IsDir() || keep[e.Name()] {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			h.logStoreError("attach_orphan_remove", rerr)
		}
	}
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
