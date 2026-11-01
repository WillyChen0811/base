package projsync

// 项目文件同步 T9a：一个单元一轮同步的「结果」和「状态」（技术设计 §5.2 第 5 步、§6.2、§8.4）。
//
// 本文件只放数据结构和状态文件的拼装、Mac 本机状态的读写：
//   - PFRoundResult：PFRoundRunUnit 的返回值——做了什么、跳过了什么及原因、暂缓清单、冲突、删除数、
//     出错与不确定、耗时。给以后的界面（T13）、取最新（T9b/T12）和归档接口（§8.4 的 aligned）用。
//   - 状态文件（Ubuntu 侧 status/<键>.json，PFRemoteStatus）由 pfRoundBuildStatus 从结果拼出来，
//     一轮结束时用 write_status 写到 Ubuntu；同一份另存一份在 Mac 的 <meta>/status/<键>.json（PFRoundSaved），
//     下一轮用它把「还没解决的冲突」带下去（冲突的两个路径都还在 base 里才算没解决）。
//
// 不管：怎么执行（pfround.go、pfround_exec.go）、怎么判定（pfdecide.go）。
//
// 【Mac 本机状态文件和 Ubuntu 的区别】Ubuntu 的状态文件是给 Ubuntu 上的 Python 读的（格式见 pfremote_files.go），
// Mac 本机这份多记了结果摘要，Mac 的界面和 wgtund 读它；格式随 PFRoundSaved，读的一方对未知键报错。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PFRoundOutcome 是一个单元这一轮的总体结果。
type PFRoundOutcome string

const (
	// PFRoundDone：跑完了（可能有跳过、暂缓、单个动作放弃，但没有「不确定」）。
	PFRoundDone PFRoundOutcome = "done"
	// PFRoundStopped：单元级关卡（G1、G4）命中，整个单元停下，没有任何写入。
	PFRoundStopped PFRoundOutcome = "stopped"
	// PFRoundUncertain：中途出现「放弃且结果不确定」或 Ubuntu 通信不确定，后续写入已停止；已做完的写进了 base。下一轮重新采集。
	PFRoundUncertain PFRoundOutcome = "uncertain"
	// PFRoundSkipped：这一轮没采集成（断网、Ubuntu 采集失败或单元在变、单元过大……），没有任何写入。
	PFRoundSkipped PFRoundOutcome = "skipped"
	// PFRoundLeaseHeld：Ubuntu 的协调者租约在别的 Mac 手里，这台不该动 Ubuntu，没有任何写入。
	PFRoundLeaseHeld PFRoundOutcome = "lease_held"
	// PFRoundFailed：出错没能进行（base 读不了、配置错误、Ubuntu 明确拒绝……），原因见 Reason。
	PFRoundFailed PFRoundOutcome = "failed"
)

// PFRoundDoneItem 是做成的一个动作。
type PFRoundDoneItem struct {
	Kind PFActionKind `json:"kind"`
	Path string       `json:"path"`
	// Side 是动手的那一边（落地的一边）；base_set / base_del 没有。
	Side  PFSide `json:"side,omitempty"`
	From  string `json:"from,omitempty"`  // 改名、本机复制的来源
	Bytes int64  `json:"bytes,omitempty"` // 为这个动作传了多少字节（改名、本机复制、复用已有 part 为 0）
	Note  string `json:"note,omitempty"`
}

// 跳过的来源。
const (
	PFRoundSrcDecide  = "decide"  // 判定时就跳过了（不明、撞名、超出范围、Office 锁……）
	PFRoundSrcApply   = "apply"   // 执行时前提不符或写原语放弃、推迟、超出范围
	PFRoundSrcStopped = "stopped" // 因为这个单元本轮已停止后续写入而没做
	PFRoundSrcTodo    = "todo"    // 本任务不做（台账）
)

// PFRoundSkip 是跳过的一项：两边都不动（或只做了一部分，见 Reason），下一轮重新判定。
type PFRoundSkip struct {
	Path   string       `json:"path"`
	Kind   PFActionKind `json:"kind,omitempty"` // 执行时跳过的动作种类
	Side   PFSide       `json:"side,omitempty"`
	Code   string       `json:"code"`
	Reason string       `json:"reason"`
	Source string       `json:"source"`
}

// 暂缓清单的方向。
const (
	PFRoundToUbuntu = "to_ubuntu"
	PFRoundToMac    = "to_mac"
)

// PFRoundDeferred 是被暂缓的一次传输（§5.4）：base 不动，网好或预计时间够短了下一轮自动补。
type PFRoundDeferred struct {
	Path       string  `json:"path"`
	Size       int64   `json:"size"`
	Dir        string  `json:"dir"`         // PFRoundToUbuntu | PFRoundToMac
	EstSeconds float64 `json:"est_seconds"` // 预计传完要多少秒（已确认的部分不算）
	Limit      float64 `json:"limit_seconds,omitempty"`
	Wanted     bool    `json:"wanted,omitempty"` // 用户指令或 Ubuntu 请求要的（用 max_seconds_user）
	Reason     string  `json:"reason"`           // 中文原因，状态文件里的 reason 固定写「暂缓」
	Conflict   bool    `json:"conflict,omitempty"`
}

// PFRoundError 是出错或不确定的一项。
type PFRoundError struct {
	Path      string `json:"path,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Uncertain bool   `json:"uncertain,omitempty"`
}

// PFRoundCounts 是两边各一个数。
type PFRoundCounts struct {
	Mac    int `json:"mac"`
	Ubuntu int `json:"ubuntu"`
}

// PFRoundResult 是 PFRoundRunUnit 的返回值。
type PFRoundResult struct {
	Unit    string         `json:"unit"`
	Round   string         `json:"round"`
	Outcome PFRoundOutcome `json:"outcome"`
	// Reason 是总体结果的主要原因（停下、不确定、跳过、失败时有）。
	Reason string `json:"reason,omitempty"`

	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	Duration  time.Duration `json:"duration"`

	// 判定结论。
	FirstTime bool     `json:"first_time,omitempty"`
	ResetBase bool     `json:"reset_base,omitempty"`
	Gates     []PFGate `json:"gates,omitempty"`
	// PlannedActions 是判定出的动作个数（含只改 base 的）；两边已一致时一轮是空的：Done 结果 + PlannedActions==0。
	PlannedActions int `json:"planned_actions"`

	Done     []PFRoundDoneItem `json:"done,omitempty"`
	Skipped  []PFRoundSkip     `json:"skipped,omitempty"`
	Deferred []PFRoundDeferred `json:"deferred,omitempty"`
	// Conflicts 是没有解决的冲突（本轮新出的加上上几轮留下、两个路径都还在 base 里的）。
	Conflicts []PFRemoteConflict `json:"conflicts,omitempty"`
	// NewConflicts 是本轮新出的冲突路径。
	NewConflicts []string `json:"new_conflicts,omitempty"`
	// Deletes 是本轮实际删了几个（都进了同步回收，只报告不拦，G3）。
	Deletes PFRoundCounts `json:"deletes"`
	// Notices 是要告诉用户的事：改的赢、Office 锁、残留的暂存文件、哈希缓存损坏……
	Notices []string `json:"notices,omitempty"`

	Errors          []PFRoundError `json:"errors,omitempty"`
	Uncertain       bool           `json:"uncertain,omitempty"`
	UncertainReason string         `json:"uncertain_reason,omitempty"`

	// 传输。
	BytesUp   int64 `json:"bytes_up,omitempty"`   // 本轮实际传到 Ubuntu 的字节数
	BytesDown int64 `json:"bytes_down,omitempty"` // 本轮实际从 Ubuntu 取回的字节数

	// Leftovers 是暂存区里崩溃留下的私有文件（Mac 本机扫描得到的，加上调用方传入的 Ubuntu 侧）。
	// swap 类可能是用户内容，只报告，绝不自动删。
	Leftovers []PFRemoteLeftover `json:"leftovers,omitempty"`

	// 写进 Ubuntu 的状态文件内容；StatusWritten 为假时没写成（原因在 Errors）。
	Status        PFRemoteStatus `json:"status"`
	StatusWritten bool           `json:"status_written"`
}

// Aligned 报告这个单元现在是不是完全对齐（§8.4 给归档用）：正常跑完、没有任何动作要做、没有暂缓、冲突、跳过、出错。
// 注意：只在 PlannedActions==0 的那一轮才会为真，也就是「两边已经一致」的下一轮。
func (r *PFRoundResult) Aligned() bool {
	return r != nil && r.Outcome == PFRoundDone && r.PlannedActions == 0 && len(r.Deferred) == 0 &&
		len(r.Conflicts) == 0 && len(r.Errors) == 0 && !r.Uncertain && !r.hasOpenSkips()
}

func (r *PFRoundResult) hasOpenSkips() bool {
	for _, s := range r.Skipped {
		if s.Source != PFRoundSrcTodo {
			return true
		}
	}
	return false
}

// ── Mac 本机状态文件 ────────────────────────────────────

// PFRoundSavedVersion 是 PFRoundSaved 的格式版本。
const PFRoundSavedVersion = 1

// PFRoundSaved 是 <meta>/status/<键>.json 的内容：写给 Ubuntu 的状态加一份结果摘要。
type PFRoundSaved struct {
	Version  int            `json:"version"`
	Unit     string         `json:"unit"`
	Round    string         `json:"round"`
	Outcome  PFRoundOutcome `json:"outcome"`
	Reason   string         `json:"reason,omitempty"`
	At       string         `json:"at"`
	Aligned  bool           `json:"aligned"`
	Status   PFRemoteStatus `json:"status"`
	Deferred int            `json:"deferred"`
	Skipped  int            `json:"skipped"`
	Errors   int            `json:"errors"`
	Done     int            `json:"done"`
	Deletes  PFRoundCounts  `json:"deletes"`
	BytesUp  int64          `json:"bytes_up,omitempty"`
	BytesDn  int64          `json:"bytes_down,omitempty"`
}

// PFRoundStatePath 返回 Mac 本机状态文件的路径。
func PFRoundStatePath(metaDir, unit string) string {
	return filepath.Join(metaDir, "status", unit+".json")
}

// PFRoundLoadState 读 Mac 本机状态文件。文件不存在返回 (nil, nil)；读不懂返回 error（调用方当作没有，不影响同步）。
func PFRoundLoadState(metaDir, unit string) (*PFRoundSaved, error) {
	if err := pfrCheckUnitKey(unit); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(PFRoundStatePath(metaDir, unit))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s PFRoundSaved
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("本机状态文件读不懂: %w", err)
	}
	if s.Version != PFRoundSavedVersion || s.Unit != unit {
		return nil, fmt.Errorf("本机状态文件版本或单元对不上（version=%d unit=%q）", s.Version, s.Unit)
	}
	return &s, nil
}

func pfRoundSaveState(metaDir string, r *PFRoundResult) error {
	s := PFRoundSaved{
		Version: PFRoundSavedVersion, Unit: r.Unit, Round: r.Round, Outcome: r.Outcome, Reason: r.Reason,
		At: r.EndedAt.Format(time.RFC3339), Aligned: r.Aligned(), Status: r.Status,
		Deferred: len(r.Deferred), Skipped: len(r.Skipped), Errors: len(r.Errors), Done: len(r.Done),
		Deletes: r.Deletes, BytesUp: r.BytesUp, BytesDn: r.BytesDown,
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(PFRoundStatePath(metaDir, r.Unit), append(b, '\n'), 0o600)
}

// ── 拼 Ubuntu 的状态文件 ────────────────────────────────

// pfRoundBuildStatus 把结果拼成 §6.2 的状态文件内容（pending 由调用方先算好放进 toUbuntu / toMac）。
func pfRoundBuildStatus(r *PFRoundResult, ubuntuRel string, at time.Time, toUbuntu, toMac []PFRemotePending) PFRemoteStatus {
	st := PFRemoteStatus{
		Version: PFRemoteStatusVersion, Unit: r.Unit,
		RoundAt: at.Format(time.RFC3339), MacSeenAt: at.Format(time.RFC3339), UbuntuRel: ubuntuRel,
		PendingToUbuntu: toUbuntu, PendingToMac: toMac,
		Conflicts: append([]PFRemoteConflict{}, r.Conflicts...),
	}
	switch r.Outcome {
	case PFRoundStopped:
		st.Note = "单元停下，没有同步：" + r.Reason
	case PFRoundUncertain:
		st.Note = "这一轮中途停止（结果不确定），下一轮重新核对：" + r.Reason
	case PFRoundFailed, PFRoundSkipped, PFRoundLeaseHeld:
		st.Note = r.Reason
	}
	if r.Deletes.Mac+r.Deletes.Ubuntu > 0 {
		st.Notes = append(st.Notes, fmt.Sprintf("这一轮删了 %d 个，都在同步回收里（Mac %d 个、Ubuntu %d 个）",
			r.Deletes.Mac+r.Deletes.Ubuntu, r.Deletes.Mac, r.Deletes.Ubuntu))
	}
	if n := len(r.Deferred); n > 0 {
		st.Notes = append(st.Notes, fmt.Sprintf("有 %d 个文件因为网络暂缓传输", n))
	}
	// 等待静止的文件名（§13：状态里要能看到）
	var waiting []string
	for _, s := range r.Skipped {
		if s.Source == PFRoundSrcDecide && s.Code == PFCodeNotQuiet {
			waiting = append(waiting, s.Path)
		}
	}
	if len(waiting) > 0 {
		sort.Strings(waiting)
		if len(waiting) > 5 {
			waiting = append(waiting[:5], fmt.Sprintf("等共 %d 个", len(waiting)))
		}
		st.Notes = append(st.Notes, "等待静止（还在被写）："+joinNames(waiting))
	}
	for _, n := range r.Notices {
		if len(st.Notes) >= 20 {
			break
		}
		st.Notes = append(st.Notes, n)
	}
	return st
}

func joinNames(ns []string) string {
	out := ""
	for i, n := range ns {
		if i > 0 {
			out += "、"
		}
		out += n
	}
	return out
}
