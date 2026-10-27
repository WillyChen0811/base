package projsync

// 项目文件同步（技术设计 §12.1–§12.5）：用户对某个项目点「开始同步」时，把它整个挪进同步目录、
// 在桌面原位置留一个符号链接；「取消同步」挪回来；「新建同步项目」直接建在同步目录里；
// 以及崩溃恢复和链接检查。桌面工作目录（local.work_dir，在 iCloud 下）本身照旧不同步、不扫描。
//
// 【函数】（都是 PFUnitMover 的方法，一个进程里一个实例，内部一把互斥锁：同一时间只做一件事）
//   StartSync(rel)      开始同步（§12.3）：只读前置检查 → 写移动日志 → 建上级目录 → 不覆盖改名 P→S → 在 P 建链接 →
//                       核对 → 登记 syncing → 日志记 done。
//   StopSync(code)      取消同步（§12.4）：登记置 stopping → 检查 S → 写日志 → 把桌面链接不覆盖地改名到 meta 下的临时位置、
//                       确认是指向 S 的链接才 unlink 它 → 不覆盖改名 S→P →
//                       核对 → 删空了的年份层 → 登记 stopped（base 不动）。
//   CreateSynced(rel)   新建同步项目（§12.5）：在 S 建空目录、在 P 建链接、登记。
//   RecoverMoves()      崩溃恢复（§12.3 第 5 条）：wgtund 启动时调，按磁盘现场补完或停下。
//   CheckLinks()        链接检查（§12.2）：每轮调。
//   PFUnitMoveValidateConfig  同步目录校验（§12.1）。
// P = 工作目录/rel（桌面原位置，开始同步后是链接），S = 同步目录/rel（项目实际所在）。
//
// 【调用方要保证的前提】
//   - HTTP 接口不在这里（wgtund 的 T11 接）；这里只是库函数。
//   - 「这个项目有没有进行中的放入、传档、导出任务」由 Python 侧在调 StartSync 之前先查，这里不管。
//   - StopSync 调用前，调用方要先拿到这个单元的单元锁、并等进行中的传输结束（§12.4 第 1 步后半）：
//     这里只负责把登记表置 stopping（让以后的轮次跳过），不会去等、也不会中断传输。
//   - 同一个 meta 目录同一时间只能有一个进程用（wgtund 单实例），units.json 的读改写靠本实例的互斥锁串行，
//     不跨进程加锁。
//
// 【边界与总原则】识别边界、超出就停：认不出、不确定的一律停下报告现场，不猜、不自动处理。
//   - 项目文件一个不丢；任何时刻不会出现「桌面上和同步目录里各有一份、都被当成项目」的情况。
//   - 对项目目录只做三件事：不覆盖的改名（renamex_np RENAME_EXCL）、建符号链接、unlink 一个符号链接
//     （且只 unlink 已经不覆盖地改名到临时位置、并确认是指向 S 的符号链接的那个名字，不 unlink 桌面原路径）。
//     绝不递归删除；删「年份层」只用 rmdir（目录不空就失败，不会删到东西）。
//   - 核对不符时停下报告，不删任何东西。
//   - 只在 macOS 上做（iCloud 是 Mac 的事）；其他平台 NewPFUnitMover 返回「不支持」。
//
// 【和技术设计不一样的地方】
//   - 前置检查的「编号未登记」：登记表里已有同编号但状态是 stopped（以前同步过、已取消）时放行——
//     §12.3 第 3 条说「以前同步过 → 用保留的 base 三方比较」，所以取消后可以再开始。
//   - 前置检查里 Office 锁用名字模式（`~$*`、`.~lock.*#`）判，不按文档逐个配对（PFOfficeLocks 对每个文档扫一遍目录，
//     大目录是平方级）；mtime 超过 12 小时的锁当残留（§4.2 第 9 条）：不挡，列在 Notes 里。
//   - 前置检查额外拒绝：读不了的目录（里面有没有 iCloud 占位无从确认）、项目里嵌着别的卷、桌面或同步目录一侧的上级目录是链接、
//     有没做完的移动日志。
//   - 同步目录校验额外要求：与 meta 互不包含、同步目录本身不是符号链接。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// ============================================================================
// 一、常量、结果、配置
// ============================================================================

// 操作种类（移动日志的 op、结果的 Op）。
const (
	PFUnitMoveOpStart   = "start"
	PFUnitMoveOpStop    = "stop"
	PFUnitMoveOpCreate  = "create"
	PFUnitMoveOpRecover = "recover"
)

// 移动日志的 state。
const (
	PFUnitMoveLogMoving     = "moving"      // 开始同步：正在挪过去
	PFUnitMoveLogMovingBack = "moving_back" // 取消同步：正在挪回来
	PFUnitMoveLogCreating   = "creating"    // 新建同步项目：正在建
	PFUnitMoveLogDone       = "done"        // 已完成（留作记录；下一次操作会覆盖）
)

const (
	pfumLogVersion = 1
	pfumListMax    = 50 // 拒绝原因里列出的文件最多这么多个
)

// PFUnitMoveStatus 是一次操作的结果种类。
type PFUnitMoveStatus string

const (
	PFUnitMoveDone       PFUnitMoveStatus = "done"        // 完成
	PFUnitMoveRefused    PFUnitMoveStatus = "refused"     // 前置检查没过，什么都没动
	PFUnitMoveRolledBack PFUnitMoveStatus = "rolled_back" // 动过手，已经回到原状
	PFUnitMoveHalted     PFUnitMoveStatus = "halted"      // 停下报告：现场见 Residue，没有自动处理、没有删任何东西
)

// 原因代码（结果的 Code、拒绝原因的 Code）：给上层按类处理；给人看的是 Reason。
const (
	PFUnitMoveCodeOK             = "ok"
	PFUnitMoveCodeBadRel         = "bad_rel"             // rel 不合法或不符合登记要求
	PFUnitMoveCodeNoCode         = "no_code"             // 目录名认不出项目编号
	PFUnitMoveCodeRegistered     = "registered"          // 编号已登记（同步中/正在开始/正在取消）
	PFUnitMoveCodeNotRegistered  = "not_registered"      // 取消同步：编号没登记
	PFUnitMoveCodeBadState       = "bad_state"           // 取消同步：登记状态不是 syncing/stopping
	PFUnitMoveCodeMovePending    = "move_pending"        // 有没做完的移动日志，先 RecoverMoves 或人工处理
	PFUnitMoveCodeNotRealDir     = "p_not_real_dir"      // 桌面位置不存在、是链接、不是目录
	PFUnitMoveCodePathLink       = "path_link"           // 上级目录是链接或文件
	PFUnitMoveCodeTargetExists   = "target_exists"       // 同步目录里这个位置已经有东西
	PFUnitMoveCodeCrossVolume    = "cross_volume"        // 与同步目录不同卷
	PFUnitMoveCodePlaceholder    = "icloud_placeholder"  // iCloud 占位文件（SF_DATALESS 或 .X.icloud）
	PFUnitMoveCodeOfficeLock     = "office_lock"         // Office 锁
	PFUnitMoveCodeNotQuiet       = "not_quiet"           // 最近有写入
	PFUnitMoveCodeUnstable       = "unstable"            // 间隔两次扫描指纹不同
	PFUnitMoveCodeUnreadable     = "unreadable"          // 读不了的目录
	PFUnitMoveCodeRegistryBad    = "registry_unreadable"    // units.json 读不了或坏了
	PFUnitMoveCodePathsBad       = "paths_list_unreadable"  // 配置路径清单读不到/坏了，有没有配置判断不了（§14.4）
	PFUnitMoveCodeNotConfigured  = "not_configured"          // 编号有配置但这个目录不是配置的那条（§14.2，可以合并）
	PFUnitMoveCodeChanged        = "changed_during_check"
	PFUnitMoveCodeNotOurLink     = "not_our_link" // 取消同步：桌面位置不是指向 S 的我们建的链接
	PFUnitMoveCodeSyncMissing    = "sync_missing" // S 不见了或不是真实目录
	PFUnitMoveCodeMkdirFailed    = "mkdir_failed"
	PFUnitMoveCodeRenameFailed   = "rename_failed"
	PFUnitMoveCodeLinkFailed     = "link_failed"
	PFUnitMoveCodeUnlinkFailed   = "unlink_failed"
	PFUnitMoveCodeVerifyFailed   = "verify_failed"
	PFUnitMoveCodeRegisterFailed = "register_failed"
	PFUnitMoveCodeLogFailed      = "log_failed"
	PFUnitMoveCodeUnknownState   = "unknown_state" // 恢复：磁盘现场不认识
	PFUnitMoveCodeHookAbort      = "hook_abort"    // 测试钩子模拟的中断
	PFUnitMoveCodeCanceled       = "canceled"
	PFUnitMoveCodeCleared        = "cleared"   // 恢复：没挪过，清了日志
	PFUnitMoveCodeCompleted      = "completed" // 恢复：补完了
	PFUnitMoveCodeReverted       = "reverted"  // 恢复：取消同步没做成，退回同步中
)

// PFUnitMoveRefusal 是前置检查的一条拒绝原因。
type PFUnitMoveRefusal struct {
	Code   string   `json:"code"`
	Reason string   `json:"reason"`
	Files  []string `json:"files,omitempty"` // 相对项目目录，字节序，最多 50 个
	More   int      `json:"more,omitempty"`  // 没列出来的个数
}

// PFUnitMoveResult 是一次操作的结果。可以直接 JSON 编码。
type PFUnitMoveResult struct {
	Op       string           `json:"op"`
	Status   PFUnitMoveStatus `json:"status"`
	Unit     string           `json:"unit,omitempty"` // 项目编号
	Rel      string           `json:"rel,omitempty"`
	Code     string           `json:"code,omitempty"`   // 主要原因代码；完成时是 ok
	Reason   string           `json:"reason,omitempty"` // 中文
	LinkPath string           `json:"link_path,omitempty"`
	SyncPath string           `json:"sync_path,omitempty"`
	// Refusals 只在 refused 时有：前置检查没过的全部原因。
	Refusals []PFUnitMoveRefusal `json:"refusals,omitempty"`
	// Files、Dirs、Bytes 是项目里会被同步的文件数、目录数、总字节数（排除项不算）：
	// 开始同步用来提示「首次同步 X GB」，取消同步用来提示「iCloud 会上传 X GB」。
	Files int   `json:"files,omitempty"`
	Dirs  int   `json:"dirs,omitempty"`
	Bytes int64 `json:"bytes,omitempty"`
	// Notes 是不挡路的提示（残留的锁、项目里的符号链接等）。
	Notes []string `json:"notes,omitempty"`
	// Residue 在 halted（和没能完全回退的 rolled_back）时说明现场：每行一句，P、S 现在各是什么。
	Residue []string `json:"residue,omitempty"`
	Err     error    `json:"-"`
	ErrText string   `json:"err,omitempty"`
}

// PFUnitMoveHook 是测试钩子：每个窗口调用一次，参数是窗口名（"start:logged-begin"、"start:after-rename" 这样）。
// 返回 error 表示「进程恰好在这里死掉」：操作立刻带着这个 error 返回，不做任何清理，磁盘停在真实崩溃会留下的样子。
// 钩子里也可以改文件系统（模拟「这时有人动了」）后返回 nil。生产代码传 nil。
//
// 窗口按发生顺序（"logged-X" 是写完「即将做 X」的日志之后，"after-X" 是 X 做完之后）：
//
//	start:  logged-begin, after-mkdir, logged-rename, after-rename, logged-link, after-link, after-verify,
//	        logged-register, after-register, after-done；另有 before-rollback（建链接失败、准备改回去之前）
//	stop:   after-stopping, logged-begin, logged-unlink（临时位置已写进日志、桌面链接还没动）, after-detach（桌面链接已改名到
//	        临时位置、还没核对）, after-unlink, logged-rename, after-rename, after-verify,
//	        logged-register, after-register, after-done；另有 before-relink（改名失败、准备把链接建回去之前）；
//	        P 本来就不在了（链接已没）时没有 logged-unlink、after-detach
//	create: logged-begin, after-mkdir, logged-link, after-link, after-verify, logged-register, after-register, after-done
//	recover: 前缀 recover:，窗口名同上（logged-link/after-link/…、logged-rename/after-rename/…），只经过实际做的那几步；
//	        另有 recover:after-unlink（补删了临时位置上的链接之后）
type PFUnitMoveHook func(window string) error

// PFUnitMoveConfigError 是配置错误（同步目录不合格等）：什么都没动，上层整个停下报告。
type PFUnitMoveConfigError struct{ Msg string }

func (e *PFUnitMoveConfigError) Error() string { return "projsync: 配置错误：" + e.Msg }

// PFUnitMoveConfig 是 PFUnitMover 的配置。
type PFUnitMoveConfig struct {
	WorkDir string // local.work_dir，绝对路径（桌面 Projects）
	SyncDir string // projfiles.sync_dir，绝对路径
	Meta    string // meta 目录（~/.xxyi/projfiles），放 units.json 和 moves/
	Home    string // 用户家目录，校验同步目录用；缺省 os.UserHomeDir()

	// PathsList：XXYI 导出的配置路径清单 ~/.xxyi/projfiles-paths.json（§14.4）。
	// 非空时 StartSync 走「开始同步的闸」：编号在清单里有配置 → rel 必须是其中一条、键取清单里的；
	// 清单读不到 → 拒绝。空 = 不启用闸（照旧：键 = 编号；测试与不带 XXYI 的调用方用）。
	// CreateSynced 不受闸限制（新建同步项目键 = 编号）。
	PathsList string

	QuietSeconds int           // 「最近没写入」的秒数，缺省 10
	StableGap    time.Duration // 开始同步前两次扫描的间隔，缺省 2 秒
	LockStale    time.Duration // Office 锁当残留的时长，缺省 12 小时

	// 下面是测试注入点，缺省用真实实现。
	Now     func() time.Time
	Lstat   PFStatFunc                                       // 缺省 PFLstat；测试用它模拟 iCloud 占位、不同卷
	Sleep   func(ctx context.Context, d time.Duration) error // 两次扫描之间等待；缺省按 ctx 可取消的定时器
	Symlink func(target, link string) error                  // 缺省 os.Symlink
	Rename  func(oldPath, newPath string) error              // 不覆盖的改名；缺省 pfRenameNoReplace
	Hook    PFUnitMoveHook
}

func (c PFUnitMoveConfig) withDefaults() PFUnitMoveConfig {
	if c.QuietSeconds <= 0 {
		c.QuietSeconds = PFDefaultQuietSeconds
	}
	if c.StableGap == 0 {
		c.StableGap = 2 * time.Second
	}
	if c.LockStale <= 0 {
		c.LockStale = PFApplyDefaultLockStale
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Lstat == nil {
		c.Lstat = PFLstat
	}
	if c.Sleep == nil {
		c.Sleep = pfumSleep
	}
	if c.Symlink == nil {
		c.Symlink = os.Symlink
	}
	if c.Rename == nil {
		c.Rename = pfRenameNoReplace
	}
	if c.Home == "" {
		c.Home, _ = os.UserHomeDir()
	}
	return c
}

func pfumSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ============================================================================
// 二、同步目录校验（§12.1）
// ============================================================================

// PFUnitMoveValidateConfig 校验目录配置：三个目录都是绝对路径；同步目录不在 ~/Desktop、~/Documents、
// ~/Library/Mobile Documents、~/Library/CloudStorage 下（也不等于它们；大小写不敏感，符号链接解析后也查）；
// 同步目录与工作目录互不包含；与 meta 同卷；另外同步目录与 meta 互不包含、同步目录本身不是符号链接。
// 不合格返回 *PFUnitMoveConfigError，什么都没动。
func PFUnitMoveValidateConfig(cfg PFUnitMoveConfig) error {
	cfg = cfg.withDefaults()
	bad := func(format string, a ...any) error {
		return &PFUnitMoveConfigError{Msg: fmt.Sprintf(format, a...)}
	}
	if err := pfumPlatformErr(); err != nil {
		return bad("%v", err)
	}
	for _, d := range []struct{ name, p string }{{"工作目录", cfg.WorkDir}, {"同步目录", cfg.SyncDir}, {"meta 目录", cfg.Meta}, {"家目录", cfg.Home}} {
		if d.p == "" {
			return bad("%s没有设置", d.name)
		}
		if !filepath.IsAbs(d.p) {
			return bad("%s %q 不是绝对路径", d.name, d.p)
		}
		if strings.ContainsRune(d.p, 0) || !utf8.ValidString(d.p) {
			return bad("%s %q 不是合法路径", d.name, d.p)
		}
	}
	work, syn, meta, home := filepath.Clean(cfg.WorkDir), filepath.Clean(cfg.SyncDir), filepath.Clean(cfg.Meta), filepath.Clean(cfg.Home)
	if syn == string(filepath.Separator) {
		return bad("同步目录不能是根目录")
	}

	// 不在 iCloud 管辖的位置下
	for _, h := range pfumUniq(home, pfumReal(home)) {
		for _, f := range []string{"Desktop", "Documents", "Library/Mobile Documents", "Library/CloudStorage"} {
			forb := filepath.Join(h, filepath.FromSlash(f))
			for _, s := range pfumUniq(syn, pfumReal(syn)) {
				if pfumUnder(s, forb) {
					return bad("同步目录 %s 在 %s 下，那里归 iCloud 管，同步目录必须放在它管不到的位置", syn, forb)
				}
			}
		}
	}
	// 互不包含
	pairs := []struct {
		an, bn string
		a, b   string
	}{{"同步目录", "工作目录", syn, work}, {"同步目录", "meta 目录", syn, meta}, {"工作目录", "meta 目录", work, meta}}
	for _, pr := range pairs {
		for _, a := range pfumUniq(pr.a, pfumReal(pr.a)) {
			for _, b := range pfumUniq(pr.b, pfumReal(pr.b)) {
				if pfumUnder(a, b) || pfumUnder(b, a) {
					return bad("%s %s 与%s %s 互相包含或相同", pr.an, pr.a, pr.bn, pr.b)
				}
			}
		}
	}
	// 同步目录本身不是符号链接
	if st, err := cfg.Lstat(syn); err == nil && st.Kind == PFKindSymlink {
		return bad("同步目录 %s 是符号链接，请填真实路径", syn)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return bad("读取同步目录 %s 失败：%v", syn, err)
	}
	// 与 meta 同卷
	sd, err := pfumDevOfNearest(cfg.Lstat, syn)
	if err != nil {
		return bad("读取同步目录 %s 所在的卷失败：%v", syn, err)
	}
	md, err := pfumDevOfNearest(cfg.Lstat, meta)
	if err != nil {
		return bad("读取 meta 目录 %s 所在的卷失败：%v", meta, err)
	}
	if sd != md {
		return bad("同步目录 %s 与 meta 目录 %s 不在同一个卷（st_dev 不同）：暂存区和同步回收放在 meta 里，放进正位是改名、不能跨盘", syn, meta)
	}
	return nil
}

// pfumReal 解析符号链接：路径存在就整条解析；不存在就解析到最近的存在的上级，再接上剩下的部分。
func pfumReal(p string) string {
	p = filepath.Clean(p)
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func pfumUniq(ss ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func pfumSegs(p string) []string {
	p = strings.Trim(filepath.ToSlash(filepath.Clean(p)), "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// pfumUnder 报告 p 是不是等于 dir 或在 dir 之下；按段比较，用折叠键（APFS 不区分大小写、不区分 NFC/NFD）。
func pfumUnder(p, dir string) bool {
	ps, ds := pfumSegs(p), pfumSegs(dir)
	if len(ps) < len(ds) {
		return false
	}
	for i, d := range ds {
		if PFFoldKey(ps[i]) != PFFoldKey(d) {
			return false
		}
	}
	return true
}

// pfumNearest 返回 p 往上第一个存在的路径（先解析符号链接）。
func pfumNearest(lstat PFStatFunc, p string) (string, error) {
	cur := pfumReal(p)
	for {
		if _, err := lstat(cur); err == nil {
			return cur, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("找不到存在的上级目录")
		}
		cur = parent
	}
}

func pfumDevOfNearest(lstat PFStatFunc, p string) (uint64, error) {
	n, err := pfumNearest(lstat, p)
	if err != nil {
		return 0, err
	}
	st, err := lstat(n)
	if err != nil {
		return 0, err
	}
	return st.Dev, nil
}

// ============================================================================
// 三、登记表与移动日志（文件格式）
// ============================================================================

// PFUnitMoveUnitsPath 是登记表 units.json 的路径。
func PFUnitMoveUnitsPath(meta string) string { return filepath.Join(meta, "units.json") }

// PFUnitMoveLogPath 是编号 code 的移动日志路径：<meta>/moves/<编号>.json。
func PFUnitMoveLogPath(meta, code string) string {
	return filepath.Join(meta, "moves", code+".json")
}

// PFUnitMoveLoadUnits 读登记表。文件不存在 = 还没有登记过 = 空表；读不了、内容坏了（解析或校验不过）一律报错，
// 绝不当成空表（否则会把已同步的项目当成没登记过）。
func PFUnitMoveLoadUnits(meta string) ([]PFUnitRecord, error) {
	data, err := os.ReadFile(PFUnitMoveUnitsPath(meta))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []PFUnitRecord{}, nil
		}
		return nil, fmt.Errorf("读 units.json 失败: %w", err)
	}
	return PFParseUnits(data)
}

// PFUnitMoveLog 是移动日志 <meta>/moves/<编号>.json 的内容（每个编号一份，每次操作覆盖）。
//
// 每步之前原子写（临时文件 + fsync + 改名 + 目录 fsync）。崩溃恢复不靠 Step，靠磁盘现场（P、S 现在各是什么）
// 与这里记的 Ino/Dev 对照；Step 只是给人看「进行到哪了」。
//
//	Op     start（开始同步）/ stop（取消同步）/ create（新建同步项目）
//	State  moving / moving_back / creating / done
//	Step   begin / rename / link / register / done 等
//	P、S   桌面位置（链接位置）、同步目录里的位置，绝对路径
//	Ino    被挪的目录的 inode：start 记挪之前的 P，stop 记挪之前的 S，create 记建出来的 S（建出来之前为 0）
//	Dev    同上，st_dev
//	StartedAt  登记表里要写的 started_at（stop 时是原登记的值）
//	CreatedDirs 本次要建（建过）的上级目录，从上到下；回退时空着的话按相反顺序 rmdir
//	UnlinkTmp  取消同步：桌面链接要暂放的临时位置 <meta>/moves/<编号>.unlink-<随机>（改名之前先写进来；删掉链接后清空）
type PFUnitMoveLog struct {
	Version     int       `json:"version"`
	Code        string    `json:"code"`
	Op          string    `json:"op"`
	State       string    `json:"state"`
	Step        string    `json:"step"`
	Rel         string    `json:"rel"`
	P           string    `json:"p"`
	S           string    `json:"s"`
	Ino         uint64    `json:"ino"`
	Dev         uint64    `json:"dev"`
	StartedAt   time.Time `json:"started_at"`
	CreatedDirs []string  `json:"created_dirs,omitempty"`
	UnlinkTmp   string    `json:"unlink_tmp,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func pfumParseLog(data []byte) (*PFUnitMoveLog, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var l PFUnitMoveLog
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("移动日志不合法: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("移动日志后面还有多余内容")
	}
	if l.Version != pfumLogVersion {
		return nil, fmt.Errorf("移动日志版本 %d 不认识", l.Version)
	}
	if l.Code == "" || PFNormCode(l.Code) != l.Code || !pfIsASCII(l.Code) {
		return nil, fmt.Errorf("移动日志的编号 %q 不合法", l.Code)
	}
	switch {
	case l.State == PFUnitMoveLogDone && (l.Op == PFUnitMoveOpStart || l.Op == PFUnitMoveOpStop || l.Op == PFUnitMoveOpCreate):
	case l.Op == PFUnitMoveOpStart && l.State == PFUnitMoveLogMoving:
	case l.Op == PFUnitMoveOpStop && l.State == PFUnitMoveLogMovingBack:
	case l.Op == PFUnitMoveOpCreate && l.State == PFUnitMoveLogCreating:
	default:
		return nil, fmt.Errorf("移动日志的 op=%q state=%q 对不上", l.Op, l.State)
	}
	for _, p := range []string{l.P, l.S} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, fmt.Errorf("移动日志里的路径 %q 不是干净的绝对路径", p)
		}
	}
	if l.Ino == 0 && !(l.Op == PFUnitMoveOpCreate) {
		return nil, errors.New("移动日志缺少 ino")
	}
	if l.UnlinkTmp != "" && (l.Op != PFUnitMoveOpStop || !filepath.IsAbs(l.UnlinkTmp) || filepath.Clean(l.UnlinkTmp) != l.UnlinkTmp) {
		return nil, fmt.Errorf("移动日志里的临时位置 %q 不合法", l.UnlinkTmp)
	}
	return &l, nil
}

// PFUnitMoveReadLog 读编号 code 的移动日志；没有返回 (nil, nil)；坏了返回 error。
func PFUnitMoveReadLog(meta, code string) (*PFUnitMoveLog, error) {
	data, err := os.ReadFile(PFUnitMoveLogPath(meta, code))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	l, err := pfumParseLog(data)
	if err != nil {
		return nil, err
	}
	if l.Code != code {
		return nil, fmt.Errorf("移动日志里的编号 %q 与文件名 %q 对不上", l.Code, code)
	}
	return l, nil
}

// PFUnitMoveListLogs 列出 <meta>/moves/ 下全部移动日志（含 done 的），按编号排序。
// 读不了或坏了的日志不放进结果，错误合并后一起返回（调用方要当成「有东西没读懂，停下」）。
func PFUnitMoveListLogs(meta string) ([]PFUnitMoveLog, error) {
	ents, err := os.ReadDir(filepath.Join(meta, "moves"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []PFUnitMoveLog
	var errs []error
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.Contains(name, ".tmp-") {
			continue
		}
		l, err := PFUnitMoveReadLog(meta, strings.TrimSuffix(name, ".json"))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, errors.Join(errs...)
}

// ============================================================================
// 四、执行器
// ============================================================================

// PFUnitMover 做开始/取消同步、新建、恢复、链接检查。不要复制；一个进程里一个实例。
type PFUnitMover struct {
	cfg  PFUnitMoveConfig
	work string // 清理过的工作目录
	sync string // 清理过的同步目录
	meta string
	mu   sync.Mutex
}

// NewPFUnitMover 校验配置（PFUnitMoveValidateConfig）并建执行器。其他平台返回「不支持」。
func NewPFUnitMover(cfg PFUnitMoveConfig) (*PFUnitMover, error) {
	cfg = cfg.withDefaults()
	if err := PFUnitMoveValidateConfig(cfg); err != nil {
		return nil, err
	}
	return &PFUnitMover{cfg: cfg, work: filepath.Clean(cfg.WorkDir), sync: filepath.Clean(cfg.SyncDir), meta: filepath.Clean(cfg.Meta)}, nil
}

func (m *PFUnitMover) now() time.Time { return m.cfg.Now().Round(0) }

// lstat 包一层：不存在不算错误。
func (m *PFUnitMover) lstat(p string) (PFStat, bool, error) {
	st, err := m.cfg.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PFStat{}, false, nil
		}
		return PFStat{}, false, err
	}
	return st, true, nil
}

// pfumStatFollow 是跟随符号链接的 stat。
func pfumStatFollow(path string) (PFStat, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return PFStat{}, err
	}
	md := fi.Mode()
	st := PFStat{Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano(), Mode: md}
	switch {
	case md.IsDir():
		st.Kind = PFKindDir
	case md.IsRegular():
		st.Kind = PFKindFile
	default:
		st.Kind = PFKindOther
	}
	if err := pfFillSys(fi, &st); err != nil {
		return PFStat{}, err
	}
	return st, nil
}

// hit 调测试钩子；钩子返回 error 表示「进程恰好死在这里」：res 标成 halted，返回 true，调用方立刻原样返回。
func (m *PFUnitMover) hit(res *PFUnitMoveResult, window string) bool {
	if m.cfg.Hook == nil {
		return false
	}
	err := m.cfg.Hook(window)
	if err == nil {
		return false
	}
	res.Status, res.Code, res.Err, res.ErrText = PFUnitMoveHalted, PFUnitMoveCodeHookAbort, err, err.Error()
	res.Reason = fmt.Sprintf("在「%s」处中断：%v", window, err)
	return true
}

func (res *PFUnitMoveResult) set(st PFUnitMoveStatus, code, format string, a ...any) {
	res.Status, res.Code, res.Reason = st, code, fmt.Sprintf(format, a...)
}

func (res *PFUnitMoveResult) fail(err error) {
	res.Err, res.ErrText = err, err.Error()
}

// ret 给公开方法当返回值：error 只在「登记表读不了」「ctx 取消」「测试钩子模拟的崩溃」时返回（res.Err 里还是底层原因）；
// 前置检查没过、已回退、停下报告，都靠 Status 区分，error 为 nil。
func (res *PFUnitMoveResult) ret() (PFUnitMoveResult, error) {
	switch res.Code {
	case PFUnitMoveCodeHookAbort, PFUnitMoveCodeCanceled, PFUnitMoveCodeRegistryBad:
		return *res, res.Err
	}
	return *res, nil
}

// ---- 登记表读写 ----

func (m *PFUnitMover) saveUnit(rec PFUnitRecord) error {
	us, err := PFUnitMoveLoadUnits(m.meta)
	if err != nil {
		return err
	}
	out, err := PFUpsertUnit(us, rec)
	if err != nil {
		return err
	}
	data, err := PFMarshalUnits(out)
	if err != nil {
		return err
	}
	return writeFileAtomic(PFUnitMoveUnitsPath(m.meta), data, 0o600)
}

// setUnitState 把已登记的单元置成 state；没登记返回 error。
func (m *PFUnitMover) setUnitState(code string, state PFUnitState) error {
	us, err := PFUnitMoveLoadUnits(m.meta)
	if err != nil {
		return err
	}
	rec, ok := PFFindUnit(us, code)
	if !ok {
		return fmt.Errorf("登记表里没有编号 %s", code)
	}
	if rec.State == state {
		return nil
	}
	rec.State = state
	return m.saveUnit(rec)
}

// ---- 日志读写 ----

func (m *PFUnitMover) writeLog(lg *PFUnitMoveLog, state, step string) error {
	lg.State, lg.Step = state, step
	lg.UpdatedAt = m.now()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(lg); err != nil {
		return err
	}
	return writeFileAtomic(PFUnitMoveLogPath(m.meta, lg.Code), buf.Bytes(), 0o600)
}

func (m *PFUnitMover) clearLog(code string) error {
	p := PFUnitMoveLogPath(m.meta, code)
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return pfSyncDir(filepath.Dir(p))
}

// step 在一步之前写日志（记下 step），再调钩子窗口 "<pfx>:logged-<step>"。返回 true 表示 res 已是最终结果。
func (m *PFUnitMover) step(lg *PFUnitMoveLog, res *PFUnitMoveResult, pfx, step string) bool {
	if err := m.writeLog(lg, lg.State, step); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeLogFailed, "写移动日志失败：%v", err)
		res.fail(err)
		res.Residue = m.residue(lg.P, lg.S)
		return true
	}
	return m.hit(res, pfx+":logged-"+step)
}

// finishLog 把日志记成 done。
func (m *PFUnitMover) finishLog(lg *PFUnitMoveLog, res *PFUnitMoveResult, pfx string) bool {
	if err := m.writeLog(lg, PFUnitMoveLogDone, "done"); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeLogFailed, "已经做完，但把移动日志记成 done 失败：%v（重启时 RecoverMoves 会补记）", err)
		res.fail(err)
		return true
	}
	return m.hit(res, pfx+":after-done")
}

// ---- 磁盘现场 ----

type pfumShape int

const (
	pfumShapeOther   pfumShape = iota // 其他：不认识
	pfumShapeUnmoved                  // P 是原来那个目录（ino/dev 对），S 不存在
	pfumShapeMoved                    // S 是那个目录，P 不存在
	pfumShapeLinked                   // P 是指向 S 的符号链接，S 是那个目录
)

func pfumDescribe(st PFStat, ex bool, readlink func() string) string {
	switch {
	case !ex:
		return "不存在"
	case st.Kind == PFKindSymlink:
		return "符号链接 → " + readlink()
	case st.Kind == PFKindDir:
		return fmt.Sprintf("目录（ino %d）", st.Ino)
	case st.Kind == PFKindFile:
		return "普通文件"
	}
	return "特殊文件"
}

// describe 用一句话说明 path 现在是什么。
func (m *PFUnitMover) describe(path string) string {
	st, ex, err := m.lstat(path)
	if err != nil {
		return "读不了（" + err.Error() + "）"
	}
	return pfumDescribe(st, ex, func() string {
		t, err := os.Readlink(path)
		if err != nil {
			return "（读不了：" + err.Error() + "）"
		}
		return t
	})
}

// residue 说明 P、S 现在各是什么，给 halted 的结果用。
func (m *PFUnitMover) residue(p, s string) []string {
	return []string{"桌面位置 " + p + "：" + m.describe(p), "同步目录位置 " + s + "：" + m.describe(s)}
}

// shape 看 P、S 现在是哪种形状；ino 为 0 时不核对 ino/dev（只在检查链接时用）。第二个返回值是现场描述。
func (m *PFUnitMover) shape(p, s string, ino, dev uint64) (pfumShape, string) {
	pst, pEx, err := m.lstat(p)
	if err != nil {
		return pfumShapeOther, fmt.Sprintf("读 %s 失败：%v", p, err)
	}
	sst, sEx, err := m.lstat(s)
	if err != nil {
		return pfumShapeOther, fmt.Sprintf("读 %s 失败：%v", s, err)
	}
	isIt := func(st PFStat) bool {
		return st.Kind == PFKindDir && (ino == 0 || (st.Ino == ino && st.Dev == dev))
	}
	switch {
	case pEx && !sEx && isIt(pst):
		return pfumShapeUnmoved, ""
	case !pEx && sEx && isIt(sst):
		return pfumShapeMoved, ""
	case pEx && sEx && pst.Kind == PFKindSymlink && isIt(sst):
		target, err := os.Readlink(p)
		if err != nil {
			return pfumShapeOther, fmt.Sprintf("读 %s 的链接目标失败：%v", p, err)
		}
		if target != s {
			return pfumShapeOther, fmt.Sprintf("%s 是链接，但指向 %q，不是 %q", p, target, s)
		}
		return pfumShapeLinked, ""
	}
	return pfumShapeOther, fmt.Sprintf("%s：%s；%s：%s", p, m.describe(p), s, m.describe(s))
}

// verifyLinked 核对（§12.3 第 5 步）：readlink(P)==S、ino(S) 等于原 ino、stat(P) 解析后就是 S。返回空串表示都对。
func (m *PFUnitMover) verifyLinked(p, s string, ino, dev uint64) string {
	sh, detail := m.shape(p, s, ino, dev)
	if sh != pfumShapeLinked {
		return "P、S 现在的样子不对：" + detail
	}
	fp, err := pfumStatFollow(p)
	if err != nil {
		return fmt.Sprintf("stat(%s) 解析失败：%v", p, err)
	}
	sst, ex, err := m.lstat(s)
	if err != nil || !ex {
		return fmt.Sprintf("读 %s 失败：%v", s, err)
	}
	if fp.Kind != PFKindDir || fp.Ino != sst.Ino || fp.Dev != sst.Dev {
		return fmt.Sprintf("stat(%s) 解析出来不是 %s（ino %d/%d）", p, s, fp.Ino, sst.Ino)
	}
	return ""
}

func (m *PFUnitMover) syncParents(paths ...string) {
	for _, p := range paths {
		_ = pfSyncDir(filepath.Dir(p))
	}
}

// ---- 建目录 ----

// walkChain 从 base 往下到 leafDir 逐级看：存在的必须是真实目录（不是链接、不是文件），base 本身可以是指向目录的链接；
// 返回还不存在、需要建的目录（从上到下，base 不存在时含 base 及它缺的上级）。有问题时 problem 非空。
func (m *PFUnitMover) walkChain(base, leafDir string) (missing []string, problem string, err error) {
	rel, rerr := filepath.Rel(base, leafDir)
	if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Sprintf("%s 不在 %s 下", leafDir, base), nil
	}
	var comps []string
	if rel != "." {
		comps = strings.Split(rel, string(filepath.Separator))
	}
	// base 及其上级
	cur := base
	var up []string
	for {
		st, ex, e := m.lstat(cur)
		if e != nil {
			return nil, "", e
		}
		if ex {
			if st.Kind == PFKindSymlink {
				fst, e := pfumStatFollow(cur)
				if e != nil {
					return nil, "", e
				}
				st = fst
			}
			if st.Kind != PFKindDir {
				return nil, fmt.Sprintf("%s 存在但不是目录", cur), nil
			}
			break
		}
		up = append(up, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(up) - 1; i >= 0; i-- {
		missing = append(missing, up[i])
	}
	cur = base
	for _, c := range comps {
		cur = filepath.Join(cur, c)
		if len(missing) > 0 { // base 都不存在，下面一定不存在
			missing = append(missing, cur)
			continue
		}
		st, ex, e := m.lstat(cur)
		if e != nil {
			return nil, "", e
		}
		if !ex {
			missing = append(missing, cur)
			continue
		}
		if st.Kind != PFKindDir {
			kind := "文件"
			if st.Kind == PFKindSymlink {
				kind = "符号链接"
			}
			return nil, fmt.Sprintf("%s 是%s，不是真实目录", cur, kind), nil
		}
	}
	return missing, "", nil
}

// mkdirs 依次建 dirs（从上到下）。目录已存在（别人刚建的）且是真实目录就算数。
func (m *PFUnitMover) mkdirs(dirs []string) error {
	for _, d := range dirs {
		if err := os.Mkdir(d, 0o755); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			st, ex, e := m.lstat(d)
			if e != nil || !ex || st.Kind != PFKindDir {
				return fmt.Errorf("%s 已经存在，但不是真实目录", d)
			}
		}
		_ = pfSyncDir(filepath.Dir(d))
	}
	return nil
}

// undoDirs 按相反顺序 rmdir dirs（用 rmdir：不空就失败，不会删到东西，也不会删掉被人换成链接的路径）。
func (m *PFUnitMover) undoDirs(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		if syscall.Rmdir(dirs[i]) == nil {
			_ = pfSyncDir(filepath.Dir(dirs[i]))
		}
	}
}

// ---- 扫描分析 ----

// pfumExcluded 判断相对路径是否被排除（§4.1：本身或某级上级目录被 PFExclude 排除）。
func pfumExcluded(rel string, isDir bool) bool {
	segs := strings.Split(rel, "/")
	parent := ""
	for i, seg := range segs {
		d := i < len(segs)-1 || isDir
		if _, ex := PFExclude(seg, d, parent); ex {
			return true
		}
		if parent == "" {
			parent = seg
		} else {
			parent += "/" + seg
		}
	}
	return false
}

// pfumIsOfficeLock 认 Office 锁文件名：`~$*`（Word/Excel/PowerPoint）、`.~lock.*#`（LibreOffice）。
func pfumIsOfficeLock(name string) bool {
	return strings.HasPrefix(name, "~$") || (strings.HasPrefix(name, ".~lock.") && strings.HasSuffix(name, "#"))
}

type pfumFacts struct {
	files, dirs int
	bytes       int64
	placeholder []string
	unreadable  []string
	locks       []string
	staleLocks  []string
	notQuiet    []string
	vanished    []string
	crossVol    []string
	symlinks    int
	specials    int
}

// analyze 把一次不带过滤器的扫描整理成事实。占位、读不了（含层数过深没进去看的目录）、Office 锁在全部条目里找
// （被排除的也算：内容只在云端的文件挪出 iCloud 就拿不回来了，进不去的目录里有什么无从确认，排除同步不能免除这个检查）；
// 「最近写入」和计数只看没被排除的。
func (m *PFUnitMover) analyze(r *PFScanResult, dev uint64) pfumFacts {
	var f pfumFacts
	now := m.now()
	for _, it := range r.Items {
		isDir := it.Kind == PFKindDir
		excl := pfumExcluded(it.Path, isDir)
		if it.Code == PFCodeICloudPlaceholder || (it.HasStat && it.Stat.Dataless()) {
			f.placeholder = append(f.placeholder, it.Path)
		}
		if it.State == PFStateOutOfScope && (it.Code == PFCodeUnreadable || it.Code == PFCodeTooDeep) {
			f.unreadable = append(f.unreadable, it.Path)
		}
		if it.Code == PFCodeVanished {
			f.vanished = append(f.vanished, it.Path)
		}
		if it.HasStat && it.Stat.Dev != dev {
			f.crossVol = append(f.crossVol, it.Path)
		}
		if !isDir && it.Kind != PFKindSymlink && pfumIsOfficeLock(pfBase(it.Path)) {
			if it.HasStat && now.UnixNano()-it.Stat.MtimeNs >= int64(m.cfg.LockStale) {
				f.staleLocks = append(f.staleLocks, it.Path)
			} else {
				f.locks = append(f.locks, it.Path)
			}
		}
		if excl {
			continue
		}
		if it.Code == PFCodeNotQuiet {
			f.notQuiet = append(f.notQuiet, it.Path)
		}
		switch it.Kind {
		case PFKindDir:
			if it.State == PFStateOK {
				f.dirs++
			}
		case PFKindFile:
			if it.HasStat {
				f.files++
				f.bytes += it.Stat.Size
			}
		case PFKindSymlink:
			f.symlinks++
		case PFKindOther:
			f.specials++
		}
	}
	return f
}

func (m *PFUnitMover) scanOpts() PFScanOptions {
	return PFScanOptions{Stat: m.cfg.Lstat, Now: m.cfg.Now, QuietSeconds: m.cfg.QuietSeconds}
}

// unstable 比较两次扫描，返回没被排除的条目里的差别（"路径（变动）"）。
func pfumUnstable(a, b *PFScanResult) []string {
	_, diffs := PFScansStable(a, b)
	var out []string
	for _, d := range diffs {
		isDir := false
		if it, ok := b.Find(d.Path); ok {
			isDir = it.Kind == PFKindDir
		} else if it, ok := a.Find(d.Path); ok {
			isDir = it.Kind == PFKindDir
		}
		if pfumExcluded(d.Path, isDir) {
			continue
		}
		out = append(out, d.Path+"（"+d.Why+"）")
	}
	return out
}

// pfumCap 排序并截到 50 个。
func pfumCap(paths []string) ([]string, int) {
	out := append([]string(nil), paths...)
	sort.Strings(out)
	if len(out) > pfumListMax {
		return out[:pfumListMax], len(out) - pfumListMax
	}
	return out, 0
}

type pfumRefusals struct{ list []PFUnitMoveRefusal }

func (r *pfumRefusals) add(code, reason string, files []string) {
	shown, more := pfumCap(files)
	r.list = append(r.list, PFUnitMoveRefusal{Code: code, Reason: reason, Files: shown, More: more})
}

func (r *pfumRefusals) addf(code, format string, a ...any) {
	r.list = append(r.list, PFUnitMoveRefusal{Code: code, Reason: fmt.Sprintf(format, a...)})
}

func (r *pfumRefusals) summary(what string) string {
	var parts []string
	for _, x := range r.list {
		parts = append(parts, x.Reason)
	}
	return fmt.Sprintf("有 %d 项原因不能%s：%s", len(r.list), what, strings.Join(parts, "；"))
}

func pfumErrCode(err error, def string) string {
	switch {
	case errors.Is(err, syscall.EEXIST):
		return PFUnitMoveCodeTargetExists
	case errors.Is(err, syscall.EXDEV):
		return PFUnitMoveCodeCrossVolume
	}
	return def
}

// ---- 公共的前置步骤 ----

// prepare 校验 rel，求出编号、P、S 和要登记的记录；填 res.Unit、LinkPath、SyncPath。
// 不合格时 res 已是 refused 结果，返回 false。
func (m *PFUnitMover) prepare(res *PFUnitMoveResult, rel string) (PFUnitRecord, bool) {
	refuse := func(code, format string, a ...any) (PFUnitRecord, bool) {
		res.set(PFUnitMoveRefused, code, format, a...)
		res.Refusals = []PFUnitMoveRefusal{{Code: code, Reason: res.Reason}}
		return PFUnitRecord{}, false
	}
	if err := validRel(rel); err != nil {
		return refuse(PFUnitMoveCodeBadRel, "项目路径不合法：%v", err)
	}
	last := rel[strings.LastIndex(rel, "/")+1:]
	code, ok := PFCodeFromName(last)
	if !ok {
		return refuse(PFUnitMoveCodeNoCode, "目录名「%s」里认不出项目编号，不能同步", last)
	}
	res.Unit = code
	p := filepath.Join(m.work, filepath.FromSlash(rel))
	s := filepath.Join(m.sync, filepath.FromSlash(rel))
	res.LinkPath, res.SyncPath = p, s
	rec := PFUnitRecord{Code: code, Rel: rel, State: PFUnitSyncing, LinkPath: p, SyncPath: s, StartedAt: m.now()}
	if err := PFValidateUnit(rec); err != nil {
		return refuse(PFUnitMoveCodeBadRel, "这个项目路径不符合登记要求：%v", err)
	}
	return rec, true
}

// pendingLog 看这个编号有没有没做完的移动日志（state 不是 done）。读不了也算有。
func (m *PFUnitMover) pendingLog(code string) (string, bool) {
	lg, err := PFUnitMoveReadLog(m.meta, code)
	if err != nil {
		return fmt.Sprintf("这个项目的移动日志读不了或坏了（%v），先处理它", err), true
	}
	if lg != nil && lg.State != PFUnitMoveLogDone {
		return fmt.Sprintf("上一次%s同步还没做完（移动日志停在「%s」），先恢复或处理现场", map[string]string{PFUnitMoveOpStart: "开始", PFUnitMoveOpStop: "取消", PFUnitMoveOpCreate: "新建"}[lg.Op], lg.Step), true
	}
	return "", false
}

func (m *PFUnitMover) registryBad(res *PFUnitMoveResult, err error) (PFUnitMoveResult, error) {
	res.set(PFUnitMoveRefused, PFUnitMoveCodeRegistryBad, "登记表 units.json 读不了或坏了，停下：%v", err)
	res.fail(err)
	return *res, err
}

// ============================================================================
// 五、开始同步（§12.3）
// ============================================================================

// StartSync 把 工作目录/rel 整个挪进同步目录，在原位置留一个指向同步目录里那份的符号链接。
// 返回的 error 只用于登记表坏了、测试钩子模拟的崩溃、ctx 取消（取消只在前置检查阶段响应）；
// 前置检查没过是 PFUnitMoveRefused（带全部原因）。
func (m *PFUnitMover) StartSync(ctx context.Context, rel string) (PFUnitMoveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := PFUnitMoveResult{Op: PFUnitMoveOpStart, Rel: rel}
	rec, ok := m.prepare(&res, rel)
	if !ok {
		return res, nil
	}
	p, s, code := rec.LinkPath, rec.SyncPath, rec.Code
	var rf pfumRefusals

	// 开始同步的闸（§14.4）：这个编号在清单里有配置 → rel 必须是其中一条，键取清单里的；
	// 清单里没有这个编号 → 照旧（键 = 编号）；清单读不到 → 有没有配置判断不了，拒绝。
	if m.cfg.PathsList != "" {
		cp, cpErr := PFLoadConfiguredPaths(m.cfg.PathsList)
		switch {
		case cpErr != nil:
			rf.addf(PFUnitMoveCodePathsBad, "配置路径清单读不到或坏了（%v），判断不了这个编号配置的目录，不能开始同步", cpErr)
		case len(cp.Units(code)) > 0:
			cu, conf := cp.Find(code, rel)
			if !conf {
				var rels []string
				for _, u := range cp.Units(code) {
					rels = append(rels, u.Rel)
				}
				rf.addf(PFUnitMoveCodeNotConfigured, "这个编号配置的目录是 %s，这个目录没配置，不能同步（可以合并进去）", strings.Join(rels, "、"))
			} else if cu.Key != rec.Code { // 另配目录单元（§14.1）：键 = 清单里的 key，编号记进 ProjectCode
				rec.Code, rec.ProjectCode = cu.Key, code
				if err := PFValidateUnit(rec); err != nil {
					rf.addf(PFUnitMoveCodeBadRel, "清单给的键 %q 配这个登记不合格：%v", cu.Key, err)
					rec.Code, rec.ProjectCode = code, ""
				} else {
					code = cu.Key
					res.Unit = cu.Key
				}
			}
		}
	}

	// 登记表、移动日志
	us, err := PFUnitMoveLoadUnits(m.meta)
	if err != nil {
		return m.registryBad(&res, err)
	}
	if cur, ok := PFFindUnit(us, code); ok && cur.State != PFUnitStopped {
		rf.addf(PFUnitMoveCodeRegistered, "编号 %s 已经登记了（状态 %s）", code, cur.State)
	}
	if msg, pending := m.pendingLog(code); pending {
		rf.addf(PFUnitMoveCodeMovePending, "%s", msg)
	}

	// P：真实目录，不是链接；上级不是链接
	pst, pEx, perr := m.lstat(p)
	pOK := false
	switch {
	case perr != nil:
		rf.addf(PFUnitMoveCodeNotRealDir, "读不了 %s：%v", p, perr)
	case !pEx:
		rf.addf(PFUnitMoveCodeNotRealDir, "桌面上没有这个项目目录 %s", p)
	case pst.Kind == PFKindSymlink:
		rf.addf(PFUnitMoveCodeNotRealDir, "%s 已经是符号链接（可能已经在同步），不是真实目录", p)
	case pst.Kind != PFKindDir:
		rf.addf(PFUnitMoveCodeNotRealDir, "%s 不是目录", p)
	case pst.Dataless():
		rf.add(PFUnitMoveCodePlaceholder, "项目目录本身是 iCloud 占位（内容还只在云端），挪出桌面会丢；先让它下载到本机", []string{"."})
	default:
		pOK = true
	}
	if _, prob, e := m.walkChain(m.work, filepath.Dir(p)); e != nil {
		rf.addf(PFUnitMoveCodePathLink, "读 %s 的上级目录失败：%v", p, e)
	} else if prob != "" {
		rf.addf(PFUnitMoveCodePathLink, "桌面一侧：%s", prob)
	}

	// S：不存在；上级不是链接
	if _, sEx, e := m.lstat(s); e != nil {
		rf.addf(PFUnitMoveCodeTargetExists, "读不了 %s：%v", s, e)
	} else if sEx {
		rf.addf(PFUnitMoveCodeTargetExists, "同步目录里这个位置已经有东西：%s", s)
	}
	missing, prob, e := m.walkChain(m.sync, filepath.Dir(s))
	if e != nil {
		rf.addf(PFUnitMoveCodePathLink, "读 %s 的上级目录失败：%v", s, e)
	} else if prob != "" {
		rf.addf(PFUnitMoveCodePathLink, "同步目录一侧：%s", prob)
	}

	// 同卷
	if pOK {
		if sdev, e := pfumDevOfNearest(m.cfg.Lstat, s); e != nil {
			rf.addf(PFUnitMoveCodeCrossVolume, "读不出同步目录所在的卷：%v", e)
		} else if sdev != pst.Dev {
			rf.addf(PFUnitMoveCodeCrossVolume, "桌面项目 %s 与同步目录 %s 不在同一个卷，不能直接挪（改名不能跨盘）", p, m.sync)
		}
	}

	// 递归扫描两次（间隔 2 秒）：占位、锁、最近写入、指纹
	if pOK {
		scan1, e := PFScan(ctx, p, m.scanOpts())
		if e != nil {
			if ctx.Err() != nil {
				res.set(PFUnitMoveRefused, PFUnitMoveCodeCanceled, "已取消")
				res.fail(ctx.Err())
				return res, ctx.Err()
			}
			rf.addf(PFUnitMoveCodeUnreadable, "扫描 %s 失败：%v", p, e)
		} else {
			if e := m.cfg.Sleep(ctx, m.cfg.StableGap); e != nil {
				res.set(PFUnitMoveRefused, PFUnitMoveCodeCanceled, "已取消")
				res.fail(e)
				return res, e
			}
			scan2, e2 := PFScan(ctx, p, m.scanOpts())
			f := m.analyze(scan1, pst.Dev)
			res.Files, res.Dirs, res.Bytes = f.files, f.dirs, f.bytes
			if len(f.placeholder) > 0 {
				rf.add(PFUnitMoveCodePlaceholder, fmt.Sprintf("有 %d 个 iCloud 占位文件（内容还只在云端），挪出桌面会丢；先让它们下载到本机", len(f.placeholder)), f.placeholder)
			}
			if len(f.locks) > 0 {
				rf.add(PFUnitMoveCodeOfficeLock, fmt.Sprintf("有 %d 个 Office 锁文件（文档正开着），先关掉", len(f.locks)), f.locks)
			}
			if len(f.notQuiet) > 0 {
				rf.add(PFUnitMoveCodeNotQuiet, fmt.Sprintf("有 %d 个文件 %d 秒内刚被写过，等写完再来", len(f.notQuiet), m.cfg.QuietSeconds), f.notQuiet)
			}
			if len(f.unreadable) > 0 {
				rf.add(PFUnitMoveCodeUnreadable, fmt.Sprintf("有 %d 个目录读不了，里面有没有 iCloud 占位无法确认", len(f.unreadable)), f.unreadable)
			}
			if len(f.crossVol) > 0 {
				rf.add(PFUnitMoveCodeCrossVolume, fmt.Sprintf("项目里嵌着别的卷的内容（%d 处），不能整个挪", len(f.crossVol)), f.crossVol)
			}
			if len(f.staleLocks) > 0 {
				shown, more := pfumCap(f.staleLocks)
				res.Notes = append(res.Notes, fmt.Sprintf("有 %d 个超过 %v 的 Office 锁文件，当作残留没有挡路：%s%s", len(f.staleLocks), m.cfg.LockStale, strings.Join(shown, "、"), pfumMore(more)))
			}
			if f.symlinks > 0 || f.specials > 0 {
				res.Notes = append(res.Notes, fmt.Sprintf("项目里有 %d 个符号链接、%d 个特殊文件，同步不处理它们", f.symlinks, f.specials))
			}
			unstable := f.vanished
			if e2 != nil {
				if ctx.Err() != nil {
					res.set(PFUnitMoveRefused, PFUnitMoveCodeCanceled, "已取消")
					res.fail(ctx.Err())
					return res, ctx.Err()
				}
				rf.addf(PFUnitMoveCodeUnstable, "第二次扫描失败：%v", e2)
			} else {
				unstable = append(unstable, pfumUnstable(scan1, scan2)...)
			}
			if len(unstable) > 0 {
				rf.add(PFUnitMoveCodeUnstable, fmt.Sprintf("间隔 %v 的两次扫描结果不同（%d 处），项目还在变动", m.cfg.StableGap, len(unstable)), unstable)
			}
		}
	}
	if len(rf.list) > 0 {
		res.Status, res.Code, res.Refusals = PFUnitMoveRefused, rf.list[0].Code, rf.list
		res.Reason = rf.summary("开始同步")
		return res, nil
	}

	// 执行前再看一眼现场：检查过程中有没有变
	if sh, detail := m.shape(p, s, pst.Ino, pst.Dev); sh != pfumShapeUnmoved {
		res.set(PFUnitMoveRefused, PFUnitMoveCodeChanged, "检查期间桌面项目或同步目录里的位置变了，本次不挪：%s", detail)
		res.Refusals = []PFUnitMoveRefusal{{Code: res.Code, Reason: res.Reason}}
		return res, nil
	}

	// ── 执行：从这里开始不响应取消 ──
	lg := &PFUnitMoveLog{Version: pfumLogVersion, Code: code, Op: PFUnitMoveOpStart, State: PFUnitMoveLogMoving, Rel: rel,
		P: p, S: s, Ino: pst.Ino, Dev: pst.Dev, StartedAt: rec.StartedAt, CreatedDirs: missing}
	if err := m.writeLog(lg, PFUnitMoveLogMoving, "begin"); err != nil {
		res.set(PFUnitMoveRefused, PFUnitMoveCodeLogFailed, "写移动日志失败，什么都没动：%v", err)
		res.fail(err)
		return res, nil
	}
	if m.hit(&res, "start:logged-begin") {
		return res.ret()
	}
	// ② 建 S 的上级目录
	if err := m.mkdirs(missing); err != nil {
		m.undoDirs(missing)
		_ = m.clearLog(code)
		res.set(PFUnitMoveRolledBack, PFUnitMoveCodeMkdirFailed, "建同步目录里的上级目录失败，什么都没挪：%v", err)
		res.fail(err)
		return res, nil
	}
	if m.hit(&res, "start:after-mkdir") {
		return res.ret()
	}
	// ③ 不覆盖改名 P → S
	if m.step(lg, &res, "start", "rename") {
		return res.ret()
	}
	if err := m.cfg.Rename(p, s); err != nil {
		switch sh, detail := m.shape(p, s, pst.Ino, pst.Dev); sh {
		case pfumShapeUnmoved: // 没动
			m.undoDirs(missing)
			_ = m.clearLog(code)
			res.set(PFUnitMoveRolledBack, pfumErrCode(err, PFUnitMoveCodeRenameFailed), "挪进同步目录失败，项目还在原处：%v", err)
			res.fail(err)
			return res, nil
		case pfumShapeMoved: // 报错了但其实挪成了，接着做
		default:
			res.set(PFUnitMoveHalted, PFUnitMoveCodeRenameFailed, "挪进同步目录时出错，现场不明，停下：%v（%s）", err, detail)
			res.fail(err)
			res.Residue = m.residue(p, s)
			return res, nil
		}
	}
	m.syncParents(p, s)
	if m.hit(&res, "start:after-rename") {
		return res.ret()
	}
	m.startFinish(lg, rec, &res, "start", true)
	return res.ret()
}

func pfumMore(n int) string {
	if n > 0 {
		return fmt.Sprintf(" 等 %d 个", n)
	}
	return ""
}

// startFinish 从「S 已就位、P 不存在」往后做：建链接 → 核对 → 登记 → done。
// canRollback：建链接失败时是否把目录改名回原处（恢复时不回退，直接停下报告）。结果写进 res。
func (m *PFUnitMover) startFinish(lg *PFUnitMoveLog, rec PFUnitRecord, res *PFUnitMoveResult, pfx string, canRollback bool) {
	p, s := lg.P, lg.S
	// ④ 在 P 建符号链接，目标是 S 的绝对路径
	if m.step(lg, res, pfx, "link") {
		return
	}
	if err := m.cfg.Symlink(s, p); err != nil {
		if canRollback {
			m.startRollback(lg, res, err)
		} else {
			res.set(PFUnitMoveHalted, PFUnitMoveCodeLinkFailed, "在桌面位置建链接失败：%v（项目数据在 %s）", err, s)
			res.fail(err)
			res.Residue = m.residue(p, s)
		}
		return
	}
	m.syncParents(p)
	if m.hit(res, pfx+":after-link") {
		return
	}
	m.startRegister(lg, rec, res, pfx)
}

// startRegister 从「P 是指向 S 的链接」往后做：核对 → 登记 → done。
func (m *PFUnitMover) startRegister(lg *PFUnitMoveLog, rec PFUnitRecord, res *PFUnitMoveResult, pfx string) {
	p, s := lg.P, lg.S
	// ⑤ 核对
	if prob := m.verifyLinked(p, s, lg.Ino, lg.Dev); prob != "" {
		_ = m.writeLog(lg, lg.State, "verify-failed")
		res.set(PFUnitMoveHalted, PFUnitMoveCodeVerifyFailed, "挪完后核对不符，停下，没有自动处理、没有删任何东西：%s", prob)
		res.Residue = m.residue(p, s)
		return
	}
	if m.hit(res, pfx+":after-verify") {
		return
	}
	// ⑥ 登记 syncing
	if m.step(lg, res, pfx, "register") {
		return
	}
	rec.StartedAt = lg.StartedAt
	if err := m.saveUnit(rec); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeRegisterFailed, "项目已挪好、链接已建好，但登记 units.json 失败：%v（重启时 RecoverMoves 会补登记）", err)
		res.fail(err)
		res.Residue = m.residue(p, s)
		return
	}
	if m.hit(res, pfx+":after-register") {
		return
	}
	if m.finishLog(lg, res, pfx) {
		return
	}
	res.set(PFUnitMoveDone, PFUnitMoveCodeOK, "已开始同步：项目挪到 %s，桌面位置留了链接", s)
}

// startRollback：建链接失败 → 把 S 不覆盖地改名回 P，核对，清日志。改名回不去就停下报告（项目数据都在 S）。
func (m *PFUnitMover) startRollback(lg *PFUnitMoveLog, res *PFUnitMoveResult, linkErr error) {
	p, s := lg.P, lg.S
	if m.hit(res, "start:before-rollback") {
		return
	}
	if err := m.cfg.Rename(s, p); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeLinkFailed, "在桌面位置建链接失败（%v），想把项目改名回桌面也失败（%v）；项目数据完整地在 %s", linkErr, err, s)
		res.fail(linkErr)
		res.Residue = m.residue(p, s)
		return
	}
	m.syncParents(p, s)
	if sh, detail := m.shape(p, s, lg.Ino, lg.Dev); sh != pfumShapeUnmoved {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeVerifyFailed, "建链接失败后把项目改名回桌面，核对不符，停下：%s", detail)
		res.fail(linkErr)
		res.Residue = m.residue(p, s)
		return
	}
	m.undoDirs(lg.CreatedDirs)
	if err := m.clearLog(lg.Code); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeLogFailed, "项目已改回桌面原处，但清移动日志失败：%v", err)
		res.fail(err)
		return
	}
	res.set(PFUnitMoveRolledBack, PFUnitMoveCodeLinkFailed, "在桌面位置建链接失败，项目已改回原处：%v", linkErr)
	res.fail(linkErr)
}

// ============================================================================
// 六、取消同步（§12.4）
// ============================================================================

// StopSync 把编号 code 的项目从同步目录挪回桌面原位置。base 不动，Ubuntu 那份不动。
// 桌面链接不是核对后直接 unlink：先不覆盖地改名到 <meta>/moves/<编号>.unlink-<随机>，对那里核对确是指向 S 的链接才 unlink
// （见 stopDetach）；桌面位置与 meta 不同卷则拒绝取消同步。
//
// 调用方的前提：已经拿到这个单元的单元锁，并且等进行中的传输结束（§12.4 第 1 步）。这里只把登记表置 stopping
// （以后的轮次跳过），不去等传输。
func (m *PFUnitMover) StopSync(ctx context.Context, code string) (PFUnitMoveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := PFUnitMoveResult{Op: PFUnitMoveOpStop}
	code = PFNormCode(code)
	res.Unit = code
	if _, ok := PFUnitKeyProjectCode(code); !ok { // §14.4：单元键 = 编号 或 编号+R+流水号数字
		res.set(PFUnitMoveRefused, PFUnitMoveCodeNoCode, "%q 不是项目编号（也不是编号+R+流水号的单元键）", code)
		res.Refusals = []PFUnitMoveRefusal{{Code: res.Code, Reason: res.Reason}}
		return res, nil
	}
	refuse := func(rf pfumRefusals) (PFUnitMoveResult, error) {
		res.Status, res.Code, res.Refusals = PFUnitMoveRefused, rf.list[0].Code, rf.list
		res.Reason = rf.summary("取消同步")
		return res, nil
	}

	us, err := PFUnitMoveLoadUnits(m.meta)
	if err != nil {
		return m.registryBad(&res, err)
	}
	rec, ok := PFFindUnit(us, code)
	if !ok {
		var rf pfumRefusals
		rf.addf(PFUnitMoveCodeNotRegistered, "编号 %s 没有登记，不在同步中", code)
		return refuse(rf)
	}
	res.Rel, res.LinkPath, res.SyncPath = rec.Rel, rec.LinkPath, rec.SyncPath
	if rec.State != PFUnitSyncing && rec.State != PFUnitStopping {
		var rf pfumRefusals
		rf.addf(PFUnitMoveCodeBadState, "编号 %s 的状态是 %s，只有同步中的项目能取消同步", code, rec.State)
		return refuse(rf)
	}
	if msg, pending := m.pendingLog(code); pending {
		var rf pfumRefusals
		rf.addf(PFUnitMoveCodeMovePending, "%s", msg)
		return refuse(rf)
	}
	p, s := rec.LinkPath, rec.SyncPath

	// 1. 登记置 stopping
	if rec.State != PFUnitStopping {
		if err := m.setUnitState(code, PFUnitStopping); err != nil {
			res.set(PFUnitMoveRefused, PFUnitMoveCodeRegistryBad, "登记表置 stopping 失败，什么都没动：%v", err)
			res.fail(err)
			return res, err
		}
	}
	if m.hit(&res, "stop:after-stopping") {
		return res.ret()
	}
	// 没动任何东西就退出：登记退回 syncing
	back := func(st PFUnitMoveStatus, code, format string, a ...any) (PFUnitMoveResult, error) {
		if err := m.setUnitState(rec.Code, PFUnitSyncing); err != nil {
			format += "（另：登记表退回 syncing 也失败了：%v，重启时 RecoverMoves 会处理）"
			a = append(a, err)
		}
		res.set(st, code, format, a...)
		return res, nil
	}

	// 2. 检查 S 和 P（只读）
	sst, sEx, serr := m.lstat(s)
	switch {
	case serr != nil:
		res, _ = back(PFUnitMoveHalted, PFUnitMoveCodeSyncMissing, "读不了同步目录里的项目 %s：%v", s, serr)
		res.Residue = m.residue(p, s)
		return res, nil
	case !sEx || sst.Kind != PFKindDir:
		res, _ = back(PFUnitMoveHalted, PFUnitMoveCodeSyncMissing, "同步目录里找不到项目目录 %s（现在是：%s），停下，没有动任何东西", s, m.describe(s))
		res.Residue = m.residue(p, s)
		return res, nil
	}
	// P 应当是指向 S 的我们建的链接；链接已经不在、位置是空的（Moved）也行——没东西可删，直接挪回去。
	pShape, pDetail := m.shape(p, s, sst.Ino, sst.Dev)
	if pShape != pfumShapeLinked && pShape != pfumShapeMoved {
		res, _ = back(PFUnitMoveHalted, PFUnitMoveCodeNotOurLink, "桌面位置 %s 不是指向 %s 的链接，停下，没有动任何东西：%s", p, s, pDetail)
		res.Residue = m.residue(p, s)
		return res, nil
	}
	scan, err := PFScan(ctx, s, m.scanOpts())
	if err != nil {
		if ctx.Err() != nil {
			res, _ = back(PFUnitMoveRefused, PFUnitMoveCodeCanceled, "已取消")
			res.fail(ctx.Err())
			return res, ctx.Err()
		}
		res, _ = back(PFUnitMoveRefused, PFUnitMoveCodeUnreadable, "扫描同步目录里的项目失败：%v", err)
		res.fail(err)
		return res, nil
	}
	f := m.analyze(scan, sst.Dev)
	res.Files, res.Dirs, res.Bytes = f.files, f.dirs, f.bytes
	var rf pfumRefusals
	if len(f.locks) > 0 {
		rf.add(PFUnitMoveCodeOfficeLock, fmt.Sprintf("有 %d 个 Office 锁文件（文档正开着），先关掉", len(f.locks)), f.locks)
	}
	if len(f.notQuiet) > 0 {
		rf.add(PFUnitMoveCodeNotQuiet, fmt.Sprintf("有 %d 个文件 %d 秒内刚被写过，等写完再来", len(f.notQuiet), m.cfg.QuietSeconds), f.notQuiet)
	}
	if len(f.staleLocks) > 0 {
		shown, more := pfumCap(f.staleLocks)
		res.Notes = append(res.Notes, fmt.Sprintf("有 %d 个超过 %v 的 Office 锁文件，当作残留没有挡路：%s%s", len(f.staleLocks), m.cfg.LockStale, strings.Join(shown, "、"), pfumMore(more)))
	}
	if pShape == pfumShapeLinked {
		// 桌面链接要先改名到 meta 下的临时位置，两边不同卷就做不了：开始之前拒绝，什么都不动
		if why := m.detachVolumeProblem(p); why != "" {
			rf.addf(PFUnitMoveCodeCrossVolume, "%s", why)
		}
	}
	if len(rf.list) > 0 {
		res, _ = back(PFUnitMoveRefused, rf.list[0].Code, "%s", rf.summary("取消同步"))
		res.Refusals = rf.list
		return res, nil
	}

	// 3. 写移动日志
	lg := &PFUnitMoveLog{Version: pfumLogVersion, Code: code, Op: PFUnitMoveOpStop, State: PFUnitMoveLogMovingBack, Rel: rec.Rel,
		P: p, S: s, Ino: sst.Ino, Dev: sst.Dev, StartedAt: rec.StartedAt}
	if err := m.writeLog(lg, PFUnitMoveLogMovingBack, "begin"); err != nil {
		res, _ = back(PFUnitMoveRefused, PFUnitMoveCodeLogFailed, "写移动日志失败，什么都没动：%v", err)
		res.fail(err)
		return res, nil
	}
	if m.hit(&res, "stop:logged-begin") {
		return res.ret()
	}
	// ── 从这里开始不响应取消 ──

	// 4. P 是指向 S 的我们建的链接就把它拿走。不是「核对是链接 → 再 unlink 原路径」（核对和 unlink 之间 P 可能被换成
	// 普通文件，unlink 会把它删掉），见 stopDetach：先不覆盖地改名到临时位置，对临时位置核对，才 unlink。
	switch sh, detail := m.shape(p, s, lg.Ino, lg.Dev); sh {
	case pfumShapeMoved: // 链接已经不在，没东西可删
	case pfumShapeLinked:
		if m.stopDetach(lg, &res) {
			return res.ret()
		}
	default:
		_ = m.clearLog(code)
		res, _ = back(PFUnitMoveHalted, PFUnitMoveCodeNotOurLink, "桌面位置 %s 在检查之后变了，不是指向 %s 的链接，停下，没有动任何东西：%s", p, s, detail)
		res.Residue = m.residue(p, s)
		return res, nil
	}
	m.syncParents(p)
	if m.hit(&res, "stop:after-unlink") {
		return res.ret()
	}
	m.stopRename(lg, rec, &res, "stop")
	return res.ret()
}

// pfumUnlinkTmpInfix 是取消同步时暂放桌面链接的临时位置的名字中缀：<meta>/moves/<编号>.unlink-<随机>。
// moves/ 下只认 .json 结尾的日志和 .json.tmp-* 的临时文件，这个名字两种都不是，恢复和列日志都不会把它当日志。
const pfumUnlinkTmpInfix = ".unlink-"

// detachVolumeProblem 桌面链接要先不覆盖地改名到 <meta>/moves/ 下，所以桌面位置所在目录与 meta 必须同卷；
// 不同卷（或读不了）返回原因，空串表示可以。
func (m *PFUnitMover) detachVolumeProblem(p string) string {
	pd, err := pfumDevOfNearest(m.cfg.Lstat, filepath.Dir(p))
	if err != nil {
		return fmt.Sprintf("读桌面位置 %s 所在的卷失败：%v", filepath.Dir(p), err)
	}
	moves := filepath.Join(m.meta, "moves")
	md, err := pfumDevOfNearest(m.cfg.Lstat, moves)
	if err != nil {
		return fmt.Sprintf("读 meta 目录 %s 所在的卷失败：%v", moves, err)
	}
	if pd != md {
		return fmt.Sprintf("桌面位置 %s 与 meta 目录 %s 不在同一个卷（st_dev 不同）：取消同步要先把桌面链接改名到 meta 里的临时位置，不能跨盘", filepath.Dir(p), moves)
	}
	return ""
}

// backToSyncing 什么都没删（或已把换下来的东西原样放回）就退出：登记退回 syncing，结果写进 res。
func (m *PFUnitMover) backToSyncing(res *PFUnitMoveResult, code string, st PFUnitMoveStatus, rcode, format string, a ...any) {
	if err := m.setUnitState(code, PFUnitSyncing); err != nil {
		format += "（另：登记表退回 syncing 也失败了：%v，重启时 RecoverMoves 会处理）"
		a = append(a, err)
	}
	res.set(st, rcode, format, a...)
}

// stopDetach 取消同步的第 4 步，此刻 P 看着是指向 S 的链接。不能「核对是链接 → 再 unlink 原路径」：
// 核对和 unlink 之间 P 若被换成了普通文件，unlink 会把它删掉。改成：
//  1. 把临时位置（<meta>/moves/<编号>.unlink-<随机>）写进移动日志（fsync）；
//  2. 把 P 不覆盖地改名到临时位置（与桌面同卷，开始前已核对）；
//  3. 对临时位置 Lstat：确实是指向 S 的符号链接才 unlink 它；
//     不是（普通文件、目录、别的链接）→ 不覆盖地改名回 P；P 已被占就留在临时位置。两种都停下报告（halted），不删任何东西。
//
// 返回 true 表示 res 已是最终结果，调用方立刻返回；false 表示链接已拿掉（或本来就不在了），接着做 S→P。
func (m *PFUnitMover) stopDetach(lg *PFUnitMoveLog, res *PFUnitMoveResult) bool {
	p, s := lg.P, lg.S
	tmp := filepath.Join(m.meta, "moves", lg.Code+pfumUnlinkTmpInfix+pfRand4()+pfRand4())
	lg.UnlinkTmp = tmp
	if m.step(lg, res, "stop", "unlink") {
		return true
	}
	if err := m.cfg.Rename(p, tmp); err != nil {
		switch sh, detail := m.shape(p, s, lg.Ino, lg.Dev); sh {
		case pfumShapeLinked: // 没挪动，一切照旧
			_ = m.clearLog(lg.Code)
			m.backToSyncing(res, lg.Code, PFUnitMoveRolledBack, PFUnitMoveCodeUnlinkFailed, "把桌面链接移到临时位置失败，一切照旧：%v", err)
			res.fail(err)
			return true
		case pfumShapeMoved: // 链接在这个间隙里自己没了，没东西可删
			lg.UnlinkTmp = ""
			return false
		default:
			_ = m.clearLog(lg.Code)
			m.backToSyncing(res, lg.Code, PFUnitMoveHalted, PFUnitMoveCodeNotOurLink, "桌面位置 %s 在检查之后变了，不是指向 %s 的链接，停下，没有动任何东西：%s", p, s, detail)
			res.Residue = m.residue(p, s)
			return true
		}
	}
	m.syncParents(p, tmp)
	if m.hit(res, "stop:after-detach") {
		return true
	}
	if m.isLinkTo(tmp, s) {
		if err := pfumUnlink(tmp); err != nil {
			// 还是链接（没删掉）就放回原处，一切照旧；放不回去就停下
			if m.isLinkTo(tmp, s) {
				if rerr := m.cfg.Rename(tmp, p); rerr == nil {
					m.syncParents(p, tmp)
					_ = m.clearLog(lg.Code)
					m.backToSyncing(res, lg.Code, PFUnitMoveRolledBack, PFUnitMoveCodeUnlinkFailed, "删桌面链接失败，已把链接放回原处，一切照旧：%v", err)
					res.fail(err)
					return true
				}
			}
			res.set(PFUnitMoveHalted, PFUnitMoveCodeUnlinkFailed, "删桌面链接时出错，现场不明，停下：%v", err)
			res.fail(err)
			res.Residue = append(m.residue(p, s), "桌面链接暂放在 "+tmp+"："+m.describe(tmp))
			return true
		}
		m.syncParents(tmp)
		lg.UnlinkTmp = ""
		return false
	}
	return m.putBackNotLink(lg, res, tmp)
}

// isLinkTo 报告 path 现在是不是指向 target 的符号链接（不跟随）。
func (m *PFUnitMover) isLinkTo(path, target string) bool {
	st, ex, err := m.lstat(path)
	if err != nil || !ex || st.Kind != PFKindSymlink {
		return false
	}
	t, err := os.Readlink(path)
	return err == nil && t == target
}

// putBackNotLink 临时位置 tmp 上的不是指向 S 的链接（桌面位置在检查之后被换成了别的东西，已被我们不覆盖地改名到了 tmp）：
// 不覆盖地改名回 P。改回去了：清日志、登记退回 syncing，停下报告；P 又被占了或读不了：留在 tmp，日志和 stopping 都保留，停下报告。
// 不论哪种都没有删任何东西。返回 true（res 已是最终结果）。
func (m *PFUnitMover) putBackNotLink(lg *PFUnitMoveLog, res *PFUnitMoveResult, tmp string) bool {
	p, s := lg.P, lg.S
	desc := m.describe(tmp)
	if _, ex, err := m.lstat(tmp); err != nil || !ex {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeNotOurLink, "桌面链接 %s 已改名到临时位置 %s，但那里现在读不到东西（%s），停下，没有删任何东西", p, tmp, desc)
		res.Residue = append(m.residue(p, s), "临时位置 "+tmp+"："+desc)
		return true
	}
	if err := m.cfg.Rename(tmp, p); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeNotOurLink, "桌面位置 %s 在检查之后被换成了别的东西（%s），不是指向 %s 的链接；想原样放回去时失败（%v），它现在留在 %s，没有删任何东西，停下", p, desc, s, err, tmp)
		res.fail(err)
		res.Residue = append(m.residue(p, s), "被换下来的东西留在 "+tmp+"："+desc)
		return true
	}
	m.syncParents(tmp, p)
	_ = m.clearLog(lg.Code)
	m.backToSyncing(res, lg.Code, PFUnitMoveHalted, PFUnitMoveCodeNotOurLink, "桌面位置 %s 在检查之后被换成了别的东西（%s），不是指向 %s 的链接；已原样放回 %s，停下，没有删任何东西", p, desc, s, p)
	res.Residue = m.residue(p, s)
	return true
}

// stopRename 从「P 不存在、S 是那个目录」往后做：不覆盖改名 S→P → 核对 → 删空年份层 → 登记 stopped → done。
// 改名失败就把链接建回去，登记退回 syncing，清日志。结果写进 res。
func (m *PFUnitMover) stopRename(lg *PFUnitMoveLog, rec PFUnitRecord, res *PFUnitMoveResult, pfx string) {
	p, s := lg.P, lg.S
	if m.step(lg, res, pfx, "rename") {
		return
	}
	if err := m.cfg.Rename(s, p); err != nil {
		// 报错但其实挪成了（P 已是原目录、S 不见了）就当成功往下走；否则把链接建回去
		if sh, _ := m.shape(p, s, lg.Ino, lg.Dev); sh != pfumShapeUnmoved {
			m.stopRelink(lg, rec, res, err)
			return
		}
	}
	m.syncParents(p, s)
	if m.hit(res, pfx+":after-rename") {
		return
	}
	m.stopFinish(lg, rec, res, pfx)
}

// stopRelink：S→P 失败，P 还空着，把链接建回去（项目留在同步目录）。
func (m *PFUnitMover) stopRelink(lg *PFUnitMoveLog, rec PFUnitRecord, res *PFUnitMoveResult, renameErr error) {
	p, s := lg.P, lg.S
	if m.hit(res, "stop:before-relink") {
		return
	}
	if err := m.cfg.Symlink(s, p); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeRenameFailed, "挪回桌面失败（%v），把链接建回去也失败（%v）；项目数据完整地在 %s", renameErr, err, s)
		res.fail(renameErr)
		res.Residue = m.residue(p, s)
		return
	}
	m.syncParents(p)
	if prob := m.verifyLinked(p, s, lg.Ino, lg.Dev); prob != "" {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeVerifyFailed, "挪回桌面失败后把链接建回去，核对不符，停下：%s", prob)
		res.fail(renameErr)
		res.Residue = m.residue(p, s)
		return
	}
	if err := m.setUnitState(lg.Code, PFUnitSyncing); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeRegisterFailed, "挪回桌面失败，链接已建回去，但登记退回 syncing 失败：%v", err)
		res.fail(err)
		return
	}
	if err := m.clearLog(lg.Code); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeLogFailed, "挪回桌面失败，链接已建回去，但清移动日志失败：%v", err)
		res.fail(err)
		return
	}
	res.set(PFUnitMoveRolledBack, pfumErrCode(renameErr, PFUnitMoveCodeRenameFailed), "挪回桌面失败，链接已建回去，项目仍在同步目录里同步：%v", renameErr)
	res.fail(renameErr)
}

// stopFinish 从「P 是那个目录、S 不存在」往后做：核对 → 删空年份层 → 登记 stopped → done。
func (m *PFUnitMover) stopFinish(lg *PFUnitMoveLog, rec PFUnitRecord, res *PFUnitMoveResult, pfx string) {
	p, s := lg.P, lg.S
	if sh, detail := m.shape(p, s, lg.Ino, lg.Dev); sh != pfumShapeUnmoved {
		_ = m.writeLog(lg, lg.State, "verify-failed")
		res.set(PFUnitMoveHalted, PFUnitMoveCodeVerifyFailed, "挪回桌面后核对不符，停下，没有自动处理、没有删任何东西：%s", detail)
		res.Residue = m.residue(p, s)
		return
	}
	if m.hit(res, pfx+":after-verify") {
		return
	}
	m.removeEmptyUpward(filepath.Dir(s))
	rec.State = PFUnitStopped
	rec.LinkPath, rec.SyncPath = "", ""
	rec.StartedAt = lg.StartedAt
	if m.step(lg, res, pfx, "register") {
		return
	}
	if err := m.saveUnit(rec); err != nil {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeRegisterFailed, "项目已挪回桌面，但登记 stopped 失败：%v（重启时 RecoverMoves 会补登记）", err)
		res.fail(err)
		res.Residue = m.residue(p, s)
		return
	}
	if m.hit(res, pfx+":after-register") {
		return
	}
	if m.finishLog(lg, res, pfx) {
		return
	}
	res.set(PFUnitMoveDone, PFUnitMoveCodeOK, "已取消同步，项目挪回了 %s", p)
}

// removeEmptyUpward 从 dir 往上，空目录用 rmdir 删掉（年份层空了就删），到同步目录为止（同步目录本身不删）。
// 只用 rmdir：目录不空就失败、停止，绝不会删到东西。
func (m *PFUnitMover) removeEmptyUpward(dir string) {
	for dir != m.sync && pfumUnder(dir, m.sync) {
		if err := syscall.Rmdir(dir); err != nil {
			return
		}
		_ = pfSyncDir(filepath.Dir(dir))
		dir = filepath.Dir(dir)
	}
}

// ============================================================================
// 七、新建要同步的项目（§12.5）
// ============================================================================

// CreateSynced 在同步目录建一个空的项目目录 S，在桌面位置 P 建指向它的链接，登记为 syncing。
// P 或 S 已经存在（任何类型）就拒绝。之后 Python 经 P（链接路径）照常建标准子目录、放文件。
func (m *PFUnitMover) CreateSynced(ctx context.Context, rel string) (PFUnitMoveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := PFUnitMoveResult{Op: PFUnitMoveOpCreate, Rel: rel}
	rec, ok := m.prepare(&res, rel)
	if !ok {
		return res, nil
	}
	p, s, code := rec.LinkPath, rec.SyncPath, rec.Code
	var rf pfumRefusals

	us, err := PFUnitMoveLoadUnits(m.meta)
	if err != nil {
		return m.registryBad(&res, err)
	}
	if cur, ok := PFFindUnit(us, code); ok && cur.State != PFUnitStopped {
		rf.addf(PFUnitMoveCodeRegistered, "编号 %s 已经登记了（状态 %s）", code, cur.State)
	}
	if msg, pending := m.pendingLog(code); pending {
		rf.addf(PFUnitMoveCodeMovePending, "%s", msg)
	}
	for _, x := range []struct{ name, path string }{{"桌面", p}, {"同步目录", s}} {
		if _, ex, e := m.lstat(x.path); e != nil {
			rf.addf(PFUnitMoveCodeTargetExists, "读不了 %s：%v", x.path, e)
		} else if ex {
			rf.addf(PFUnitMoveCodeTargetExists, "%s里这个位置已经有东西：%s", x.name, x.path)
		}
	}
	missS, prob, e := m.walkChain(m.sync, filepath.Dir(s))
	if e != nil {
		rf.addf(PFUnitMoveCodePathLink, "读 %s 的上级目录失败：%v", s, e)
	} else if prob != "" {
		rf.addf(PFUnitMoveCodePathLink, "同步目录一侧：%s", prob)
	}
	missP, prob, e := m.walkChain(m.work, filepath.Dir(p))
	if e != nil {
		rf.addf(PFUnitMoveCodePathLink, "读 %s 的上级目录失败：%v", p, e)
	} else if prob != "" {
		rf.addf(PFUnitMoveCodePathLink, "桌面一侧：%s", prob)
	}
	if len(rf.list) > 0 {
		res.Status, res.Code, res.Refusals = PFUnitMoveRefused, rf.list[0].Code, rf.list
		res.Reason = rf.summary("新建同步项目")
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		res.set(PFUnitMoveRefused, PFUnitMoveCodeCanceled, "已取消")
		res.fail(err)
		return res, err
	}

	dirs := append(append([]string(nil), missS...), missP...)
	lg := &PFUnitMoveLog{Version: pfumLogVersion, Code: code, Op: PFUnitMoveOpCreate, State: PFUnitMoveLogCreating, Rel: rel,
		P: p, S: s, StartedAt: rec.StartedAt, CreatedDirs: dirs}
	if err := m.writeLog(lg, PFUnitMoveLogCreating, "begin"); err != nil {
		res.set(PFUnitMoveRefused, PFUnitMoveCodeLogFailed, "写移动日志失败，什么都没建：%v", err)
		res.fail(err)
		return res, nil
	}
	if m.hit(&res, "create:logged-begin") {
		return res.ret()
	}
	rollback := func(cause error, code string) (PFUnitMoveResult, error) {
		_ = syscall.Rmdir(s) // 空目录才删得掉
		m.undoDirs(dirs)
		_ = m.clearLog(rec.Code)
		res.set(PFUnitMoveRolledBack, code, "新建同步项目失败，已清理：%v", cause)
		res.fail(cause)
		return res, nil
	}
	if err := m.mkdirs(missS); err != nil {
		return rollback(err, PFUnitMoveCodeMkdirFailed)
	}
	if err := os.Mkdir(s, 0o755); err != nil { // 不用 MkdirAll：别人抢先建了就失败
		if errors.Is(err, fs.ErrExist) { // 不是我们建的，别去 rmdir
			m.undoDirs(dirs)
			_ = m.clearLog(rec.Code)
			res.set(PFUnitMoveRolledBack, PFUnitMoveCodeTargetExists, "同步目录里 %s 刚被别人建了，本次不新建", s)
			res.fail(err)
			return res, nil
		}
		return rollback(err, PFUnitMoveCodeMkdirFailed)
	}
	_ = pfSyncDir(filepath.Dir(s))
	if m.hit(&res, "create:after-mkdir") {
		return res.ret()
	}
	sst, sEx, err := m.lstat(s)
	if err != nil || !sEx || sst.Kind != PFKindDir {
		res.set(PFUnitMoveHalted, PFUnitMoveCodeVerifyFailed, "刚建的 %s 读不到或不是目录：%v", s, err)
		res.Residue = m.residue(p, s)
		return res, nil
	}
	lg.Ino, lg.Dev = sst.Ino, sst.Dev
	if err := m.mkdirs(missP); err != nil {
		return rollback(err, PFUnitMoveCodeMkdirFailed)
	}
	if m.step(lg, &res, "create", "link") {
		return res.ret()
	}
	if err := m.cfg.Symlink(s, p); err != nil {
		return rollback(err, PFUnitMoveCodeLinkFailed)
	}
	m.syncParents(p)
	if m.hit(&res, "create:after-link") {
		return res.ret()
	}
	m.startRegister(lg, rec, &res, "create")
	if res.Status == PFUnitMoveDone {
		res.Reason = fmt.Sprintf("已新建同步项目：%s，桌面位置 %s 是指向它的链接", s, p)
	}
	return res.ret()
}

// ============================================================================
// 八、崩溃恢复（§12.3 第 5 条）
// ============================================================================

// RecoverMoves 对 <meta>/moves/ 下每份没做完的移动日志，按磁盘现场处理，wgtund 启动时调一次。
// 返回每份日志的处理结果（Op 是 recover，Code 说明做了什么：cleared/completed/reverted，或 unknown_state 停下）。
// 只有四种认得出的现场才动手，其他一律停下报告、什么都不动（Status=halted，Residue 说明现场）：
//
//	开始同步（moving）：
//	  P 仍是原 ino 的目录、S 不存在         → 没挪过：清日志（空着的上级目录一并 rmdir）
//	  S 是原 ino 的目录、P 不存在           → 补建链接、核对、登记 syncing、日志记 done
//	  P 已是指向 S 的链接、S 是原 ino 的目录 → 补登记（已登记就核对一致）、日志记 done
//	取消同步（moving_back）：
//	  P 是指向 S 的链接                     → 还没动：登记退回 syncing，清日志（要取消请再点一次）
//	  P 不存在、S 是原 ino 的目录           → 接着做：不覆盖改名 S→P、核对、登记 stopped
//	  P 是原 ino 的目录、S 不存在           → 已经挪回：核对、删空年份层、登记 stopped
//	  （日志里记了临时位置 UnlinkTmp 且那里有东西时先看它，见 recoverDetach：P 不存在、S 是那个目录时，是指向 S 的链接就补删，
//	   是别的东西就不覆盖地放回 P 并停下报告；P 又被占了、或现场不是这样，就停下不动）
//	新建（creating）：
//	  P、S 都不存在                          → 清日志
//	  S 是空目录（或 ino 对得上）、P 不存在  → 接着建链接、登记
//	  P 已是指向 S 的链接                    → 补登记
//
// 另外，登记表里状态是 stopping 却没有进行中的取消日志的（崩在「置 stopping」和「写日志」之间），
// 链接和 S 都完好就退回 syncing；其他现场停下报告。
// 临时文件（moves/ 下 .tmp-* ）是上次写日志崩溃留下的，这里删掉。
// 返回的 error 只用于 moves/ 目录读不了（此时什么都没做）或测试钩子模拟的崩溃（此时已处理的结果仍在返回值里）。
// 恢复不响应 ctx 取消（半途停下只会多留一份要再恢复的现场），ctx 只为与其他方法一致。
func (m *PFUnitMover) RecoverMoves(ctx context.Context) ([]PFUnitMoveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir := filepath.Join(m.meta, "moves")
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("读 moves 目录失败: %w", err)
	}
	var out []PFUnitMoveResult
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.Contains(name, ".json.tmp-") {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		code := strings.TrimSuffix(name, ".json")
		res := PFUnitMoveResult{Op: PFUnitMoveOpRecover, Unit: code}
		lg, err := PFUnitMoveReadLog(m.meta, code)
		if err != nil {
			res.set(PFUnitMoveHalted, PFUnitMoveCodeUnknownState, "移动日志读不了或坏了，停下，没有动任何东西：%v", err)
			res.fail(err)
			out = append(out, res)
			continue
		}
		if lg.State == PFUnitMoveLogDone {
			continue
		}
		res.Rel, res.LinkPath, res.SyncPath = lg.Rel, lg.P, lg.S
		switch lg.Op {
		case PFUnitMoveOpStart:
			m.recoverStart(lg, &res)
		case PFUnitMoveOpStop:
			m.recoverStop(lg, &res)
		case PFUnitMoveOpCreate:
			m.recoverCreate(lg, &res)
		}
		out = append(out, res)
		if res.Code == PFUnitMoveCodeHookAbort {
			return out, res.Err
		}
	}
	// 登记表里 stopping 却没有取消日志的
	us, err := PFUnitMoveLoadUnits(m.meta)
	if err != nil {
		res := PFUnitMoveResult{Op: PFUnitMoveOpRecover}
		res.set(PFUnitMoveHalted, PFUnitMoveCodeRegistryBad, "登记表 units.json 读不了或坏了，停下：%v", err)
		res.fail(err)
		return append(out, res), nil
	}
	for _, u := range us {
		if u.State != PFUnitStopping {
			continue
		}
		if lg, err := PFUnitMoveReadLog(m.meta, u.Code); err == nil && lg != nil && lg.State == PFUnitMoveLogMovingBack {
			continue // 上面按日志处理过了
		}
		res := PFUnitMoveResult{Op: PFUnitMoveOpRecover, Unit: u.Code, Rel: u.Rel, LinkPath: u.LinkPath, SyncPath: u.SyncPath}
		if sh, detail := m.shape(u.LinkPath, u.SyncPath, 0, 0); sh == pfumShapeLinked {
			if err := m.setUnitState(u.Code, PFUnitSyncing); err != nil {
				res.set(PFUnitMoveHalted, PFUnitMoveCodeRegisterFailed, "取消同步没做完（链接和项目都完好），退回 syncing 失败：%v", err)
				res.fail(err)
			} else {
				res.set(PFUnitMoveDone, PFUnitMoveCodeReverted, "取消同步没做完，链接和项目都完好，已退回同步中；要取消请再点一次")
			}
		} else {
			res.set(PFUnitMoveHalted, PFUnitMoveCodeUnknownState, "登记状态是 stopping，但没有取消同步的日志，现场也不是「链接完好」，停下，没有动任何东西：%s", detail)
			res.Residue = m.residue(u.LinkPath, u.SyncPath)
		}
		out = append(out, res)
	}
	return out, nil
}

func (m *PFUnitMover) recordOf(lg *PFUnitMoveLog, state PFUnitState) PFUnitRecord {
	rec := PFUnitRecord{Code: lg.Code, Rel: lg.Rel, State: state, LinkPath: lg.P, SyncPath: lg.S, StartedAt: lg.StartedAt}
	// §14.4：崩溃恢复出来的记录也要补 ProjectCode（键带 R 后缀时），别让空值被当成等于键。
	if pc, ok := PFUnitKeyProjectCode(lg.Code); ok && pc != lg.Code {
		rec.ProjectCode = pc
	}
	return rec
}

func (m *PFUnitMover) haltUnknown(res *PFUnitMoveResult, lg *PFUnitMoveLog, detail string) {
	res.set(PFUnitMoveHalted, PFUnitMoveCodeUnknownState, "磁盘现场和移动日志（%s，停在「%s」）对不上，停下，没有动任何东西：%s", lg.Op, lg.Step, detail)
	res.Residue = m.residue(lg.P, lg.S)
}

func (m *PFUnitMover) recoverStart(lg *PFUnitMoveLog, res *PFUnitMoveResult) {
	sh, detail := m.shape(lg.P, lg.S, lg.Ino, lg.Dev)
	switch sh {
	case pfumShapeUnmoved:
		m.undoDirs(lg.CreatedDirs)
		if err := m.clearLog(lg.Code); err != nil {
			res.set(PFUnitMoveHalted, PFUnitMoveCodeLogFailed, "项目没挪过，但清移动日志失败：%v", err)
			res.fail(err)
			return
		}
		res.set(PFUnitMoveDone, PFUnitMoveCodeCleared, "开始同步没挪成，项目还在桌面原处，已清掉移动日志")
	case pfumShapeMoved:
		m.startFinish(lg, m.recordOf(lg, PFUnitSyncing), res, "recover", false)
		if res.Status == PFUnitMoveDone {
			res.Code, res.Reason = PFUnitMoveCodeCompleted, "补完了开始同步：补建了链接并登记"
		}
	case pfumShapeLinked:
		m.startRegister(lg, m.recordOf(lg, PFUnitSyncing), res, "recover")
		if res.Status == PFUnitMoveDone {
			res.Code, res.Reason = PFUnitMoveCodeCompleted, "补完了开始同步：补登记"
		}
	default:
		m.haltUnknown(res, lg, detail)
	}
}

// recoverDetach 取消同步日志里记了临时位置（UnlinkTmp）时先看临时位置：
//   - 那里什么都没有：按 P、S 的现场往下（返回 false）；
//   - P 不存在、S 是那个目录，临时位置是指向 S 的链接：补删这个链接，往下接着做 S→P（返回 false）；
//   - P 不存在、S 是那个目录，临时位置是别的东西（桌面位置在检查之后被换过）：不覆盖地改名回 P，停下报告；
//     P 又被占了就留在临时位置，停下报告（返回 true）；
//   - 其他（临时位置有东西、P 和 S 却不是上面的样子，或日志里的临时位置不认识）：停下，什么都不动（返回 true）。
func (m *PFUnitMover) recoverDetach(lg *PFUnitMoveLog, res *PFUnitMoveResult) bool {
	tmp := lg.UnlinkTmp
	if filepath.Dir(tmp) != filepath.Join(m.meta, "moves") || !strings.HasPrefix(filepath.Base(tmp), lg.Code+pfumUnlinkTmpInfix) {
		m.haltUnknown(res, lg, fmt.Sprintf("移动日志里记的临时位置 %q 不认识", tmp))
		return true
	}
	_, ex, err := m.lstat(tmp)
	if err != nil {
		m.haltUnknown(res, lg, fmt.Sprintf("读临时位置 %s 失败：%v", tmp, err))
		return true
	}
	if !ex {
		lg.UnlinkTmp = ""
		return false
	}
	if sh, _ := m.shape(lg.P, lg.S, lg.Ino, lg.Dev); sh != pfumShapeMoved {
		m.haltUnknown(res, lg, fmt.Sprintf("临时位置 %s 上有东西（%s），可桌面位置和同步目录不是「桌面位置空着、项目在同步目录」的样子", tmp, m.describe(tmp)))
		res.Residue = append(res.Residue, "临时位置 "+tmp+"："+m.describe(tmp))
		return true
	}
	if m.isLinkTo(tmp, lg.S) {
		if err := pfumUnlink(tmp); err != nil {
			res.set(PFUnitMoveHalted, PFUnitMoveCodeUnlinkFailed, "补删临时位置 %s 上的桌面链接失败，停下：%v", tmp, err)
			res.fail(err)
			res.Residue = append(m.residue(lg.P, lg.S), "桌面链接暂放在 "+tmp+"："+m.describe(tmp))
			return true
		}
		m.syncParents(tmp)
		lg.UnlinkTmp = ""
		return m.hit(res, "recover:after-unlink")
	}
	return m.putBackNotLink(lg, res, tmp)
}

func (m *PFUnitMover) recoverStop(lg *PFUnitMoveLog, res *PFUnitMoveResult) {
	if lg.UnlinkTmp != "" && m.recoverDetach(lg, res) {
		return
	}
	sh, detail := m.shape(lg.P, lg.S, lg.Ino, lg.Dev)
	rec := m.recordOf(lg, PFUnitSyncing)
	switch sh {
	case pfumShapeLinked: // 还没动
		if err := m.setUnitState(lg.Code, PFUnitSyncing); err != nil {
			res.set(PFUnitMoveHalted, PFUnitMoveCodeRegisterFailed, "取消同步还没动手，退回 syncing 失败：%v", err)
			res.fail(err)
			return
		}
		if err := m.clearLog(lg.Code); err != nil {
			res.set(PFUnitMoveHalted, PFUnitMoveCodeLogFailed, "取消同步还没动手，已退回 syncing，但清移动日志失败：%v", err)
			res.fail(err)
			return
		}
		res.set(PFUnitMoveDone, PFUnitMoveCodeReverted, "取消同步没做成（链接和项目都完好），已退回同步中；要取消请再点一次")
	case pfumShapeMoved: // 链接已删，S 还没挪
		m.stopRename(lg, rec, res, "recover")
		if res.Status == PFUnitMoveDone {
			res.Code, res.Reason = PFUnitMoveCodeCompleted, "补完了取消同步：项目已挪回桌面并登记 stopped"
		}
	case pfumShapeUnmoved: // 已挪回
		m.stopFinish(lg, rec, res, "recover")
		if res.Status == PFUnitMoveDone {
			res.Code, res.Reason = PFUnitMoveCodeCompleted, "补完了取消同步：项目已在桌面，已登记 stopped"
		}
	default:
		m.haltUnknown(res, lg, detail)
	}
}

func (m *PFUnitMover) recoverCreate(lg *PFUnitMoveLog, res *PFUnitMoveResult) {
	p, s := lg.P, lg.S
	pst, pEx, perr := m.lstat(p)
	sst, sEx, serr := m.lstat(s)
	if perr != nil || serr != nil {
		m.haltUnknown(res, lg, fmt.Sprintf("读现场失败：%v %v", perr, serr))
		return
	}
	rec := m.recordOf(lg, PFUnitSyncing)
	switch {
	case !pEx && !sEx: // 什么都没建出来
		m.undoDirs(lg.CreatedDirs)
		if err := m.clearLog(lg.Code); err != nil {
			res.set(PFUnitMoveHalted, PFUnitMoveCodeLogFailed, "什么都没建出来，但清移动日志失败：%v", err)
			res.fail(err)
			return
		}
		res.set(PFUnitMoveDone, PFUnitMoveCodeCleared, "新建同步项目没建成，什么都没留下，已清掉移动日志")
	case !pEx && sEx && sst.Kind == PFKindDir && m.createdByUs(lg, s, sst):
		lg.Ino, lg.Dev = sst.Ino, sst.Dev
		if err := m.mkdirs(lg.CreatedDirs); err != nil { // 补建桌面一侧缺的上级目录（已有的会跳过）
			res.set(PFUnitMoveHalted, PFUnitMoveCodeMkdirFailed, "补建桌面一侧的上级目录失败：%v", err)
			res.fail(err)
			return
		}
		m.startFinish(lg, rec, res, "recover", false)
		if res.Status == PFUnitMoveDone {
			res.Code, res.Reason = PFUnitMoveCodeCompleted, "补完了新建同步项目：补建了链接并登记"
		}
	case pEx && pst.Kind == PFKindSymlink && sEx && sst.Kind == PFKindDir && m.createdByUs(lg, s, sst):
		lg.Ino, lg.Dev = sst.Ino, sst.Dev
		m.startRegister(lg, rec, res, "recover")
		if res.Status == PFUnitMoveDone {
			res.Code, res.Reason = PFUnitMoveCodeCompleted, "补完了新建同步项目：补登记"
		}
	default:
		m.haltUnknown(res, lg, fmt.Sprintf("%s：%s；%s：%s", p, m.describe(p), s, m.describe(s)))
	}
}

// createdByUs 判断 S 是不是这次新建出来的那个目录：日志里记了 ino 就对 ino/dev；没记（崩在建目录和写日志之间）就要求它是空的。
func (m *PFUnitMover) createdByUs(lg *PFUnitMoveLog, s string, st PFStat) bool {
	if lg.Ino != 0 {
		return st.Ino == lg.Ino && st.Dev == lg.Dev
	}
	ents, err := os.ReadDir(s)
	return err == nil && len(ents) == 0
}

// ============================================================================
// 九、链接检查（§12.2）
// ============================================================================

// PFUnitMoveLinkStatus 是链接检查的结果种类。
type PFUnitMoveLinkStatus string

const (
	PFUnitMoveLinkOK          PFUnitMoveLinkStatus = "ok"           // 链接在、指向 sync_path
	PFUnitMoveLinkRebuilt     PFUnitMoveLinkStatus = "rebuilt"      // 链接没了、原位置空：已重建
	PFUnitMoveLinkOccupied    PFUnitMoveLinkStatus = "occupied"     // 原位置被别的占了（真实目录、文件）：只报告
	PFUnitMoveLinkWrongTarget PFUnitMoveLinkStatus = "wrong_target" // 是链接但指向别处：只报告
	PFUnitMoveLinkSyncMissing PFUnitMoveLinkStatus = "sync_missing" // 同步目录里的项目不见了（G1：停下，不当删除）：只报告
	PFUnitMoveLinkSkipped     PFUnitMoveLinkStatus = "skipped"      // 有没做完的移动日志，这一轮不管
	PFUnitMoveLinkError       PFUnitMoveLinkStatus = "error"        // 读不了或重建失败
)

// PFUnitMoveLinkCheck 是一个单元的检查结果。
type PFUnitMoveLinkCheck struct {
	Unit     string               `json:"unit"`
	Rel      string               `json:"rel"`
	LinkPath string               `json:"link_path"`
	SyncPath string               `json:"sync_path"`
	Status   PFUnitMoveLinkStatus `json:"status"`
	Detail   string               `json:"detail,omitempty"`
}

// CheckLinks 对登记表里状态是 syncing 的每个单元检查桌面链接：lstat(link_path) 应是指向 sync_path 的符号链接；
// 链接没了且原位置空 → 重建；原位置被别的占了（真实目录、文件、指向别处的链接）→ 只报告，不动。
// 同步目录里的项目不见了 → 只报告（不重建链接，也不当作删除）。登记表读不了返回 error。
func (m *PFUnitMover) CheckLinks(ctx context.Context) ([]PFUnitMoveLinkCheck, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	us, err := PFUnitMoveLoadUnits(m.meta)
	if err != nil {
		return nil, err
	}
	var out []PFUnitMoveLinkCheck
	for _, u := range us {
		if u.State != PFUnitSyncing {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		c := PFUnitMoveLinkCheck{Unit: u.Code, Rel: u.Rel, LinkPath: u.LinkPath, SyncPath: u.SyncPath}
		c.Status, c.Detail = m.checkOneLink(u)
		out = append(out, c)
	}
	return out, nil
}

func (m *PFUnitMover) checkOneLink(u PFUnitRecord) (PFUnitMoveLinkStatus, string) {
	p, s := u.LinkPath, u.SyncPath
	if _, pending := m.pendingLog(u.Code); pending {
		return PFUnitMoveLinkSkipped, "有没做完的移动日志，这一轮不检查"
	}
	sst, sEx, err := m.lstat(s)
	if err != nil {
		return PFUnitMoveLinkError, fmt.Sprintf("读 %s 失败：%v", s, err)
	}
	if !sEx || sst.Kind != PFKindDir {
		return PFUnitMoveLinkSyncMissing, fmt.Sprintf("同步目录里的项目 %s 现在是：%s", s, m.describe(s))
	}
	pst, pEx, err := m.lstat(p)
	if err != nil {
		return PFUnitMoveLinkError, fmt.Sprintf("读 %s 失败：%v", p, err)
	}
	if pEx {
		if pst.Kind != PFKindSymlink {
			return PFUnitMoveLinkOccupied, fmt.Sprintf("桌面位置 %s 被别的东西占了：%s", p, m.describe(p))
		}
		target, err := os.Readlink(p)
		if err != nil {
			return PFUnitMoveLinkError, fmt.Sprintf("读 %s 的链接目标失败：%v", p, err)
		}
		if target != s {
			return PFUnitMoveLinkWrongTarget, fmt.Sprintf("桌面位置 %s 是链接，但指向 %q，不是 %q", p, target, s)
		}
		return PFUnitMoveLinkOK, ""
	}
	// 链接没了，原位置空：重建（桌面一侧缺的上级目录也补）
	missing, prob, err := m.walkChain(m.work, filepath.Dir(p))
	if err != nil {
		return PFUnitMoveLinkError, fmt.Sprintf("读 %s 的上级目录失败：%v", p, err)
	}
	if prob != "" {
		return PFUnitMoveLinkOccupied, "桌面一侧：" + prob
	}
	if len(missing) > 0 && missing[0] == m.work {
		return PFUnitMoveLinkError, fmt.Sprintf("工作目录 %s 不存在，不重建链接", m.work)
	}
	if err := m.mkdirs(missing); err != nil {
		return PFUnitMoveLinkError, fmt.Sprintf("补建 %s 的上级目录失败：%v", p, err)
	}
	if err := m.cfg.Symlink(s, p); err != nil {
		m.undoDirs(missing)
		return PFUnitMoveLinkError, fmt.Sprintf("重建链接 %s 失败：%v", p, err)
	}
	m.syncParents(p)
	if prob := m.verifyLinked(p, s, 0, 0); prob != "" {
		return PFUnitMoveLinkError, "重建链接后核对不符：" + prob
	}
	return PFUnitMoveLinkRebuilt, fmt.Sprintf("链接 %s 不见了，已重建，指向 %s", p, s)
}
