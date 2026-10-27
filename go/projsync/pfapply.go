package projsync

// 项目文件同步的「写原语」（技术设计 §0「写入」、§3.3、§3.4「执行」、§3.5、§4.2 第 7/9/14 条、§5.6）。
// 这是同步里唯一真正改用户文件的地方：任何一步都不能覆盖或丢掉一份内容。
//
// 【都在本机执行】Mac 上 wgtund 直接调；Ubuntu 上以后由 xxyi-msg 调同一套函数。路径都是本机绝对路径，
// 文件名都是「单元目录下的相对路径（/ 分隔）」。同一个单元同一时间只能有一个调用者（单元锁在上层）。
//
// 【原语】
//   InstallNew     安装（新建）：暂存区 part → 目标路径，目标必须不存在，不覆盖的改名
//   InstallOver    安装（覆盖）：目标必须仍是期望的那份；原子交换 → 核对换下来的 → 移入同步回收
//   Delete         删除：目标不覆盖地改名移进同步回收 → 核对移过去的 → 不符就在原位仍空时移回去
//   Rename         本机改名复用（§3.3a）：p1 → p2，不覆盖；前后核对
//   ConflictRename 冲突改名：原名文件不覆盖地改成冲突副本名（和 Rename 同一套核对）
//   StageCopy      本机复制复用（§3.3b）：同一文件系统里的来源 → 暂存区 part（darwin 用 clonefile），核对哈希
//   CopyInstallNew / CopyInstallOver  StageCopy 之后接着安装
//   Mkdir / Rmdir  建目录 / 空了才删目录（rmdir 在执行时由系统确认为空）
//   PFStagingLeftovers  列出暂存区里崩溃留下的 .pfswap-*、.pfcopy-* 文件
//
// 【前提】每个会动「已有文件」的原语都带一个 PFExpect：判定时看到的目标现状（不存在，或存在且大小、mtime、ino、sha256）。
// 执行前核对（大小/mtime/ino，不读内容），执行后再核对（读内容算 sha256）；不符就放弃并恢复原状。
//
// 【结果】返回 (PFApplyResult, error)。
//   Status 有四种：完成 / 放弃 / 推迟 / 超出范围，都带中文原因；
//     放弃  这一步没做（前提不符、被人改了、竞态）；现场已恢复原状；下一轮重新采集再判定。
//           Uncertain 为 true 表示「没能完全恢复原状」（内容都在，位置见 Residue）：上层停下这个单元本轮后续写入并报告。
//     推迟  Office 锁在目标边、iCloud 占位文件：不是错误，等条件消失再来。
//     超出范围  只读/uchg、名字按字节对不上（大小写或 NFC/NFD）、符号链接、一边文件一边目录等：需要人看，不自动处理。
//   error 只用于配置错误（参数不合法、暂存区或回收区与根不在同一文件系统）：此时什么都没动。
//
// 【钩子】PFApplyConfig.Hook 在每个窗口（核对之后、改名/交换之前等）调用一次，参数是窗口名。
// 测试用它模拟「这时文件被别人改了」；钩子返回 error 表示「进程恰好在这里死掉」：原语立刻带着
// Uncertain 返回，不做任何清理，磁盘停在真实崩溃会留下的样子。窗口名见各原语的注释。
//
// 【和技术设计不一样的地方】
//   - 覆盖时不是直接拿 <sha>.part 去交换，而是先把 part 不覆盖地改名为暂存区里的私有名字 .pfswap-<sha前8位>-<随机>，
//     再与目标交换：被换下来的用户文件落在私有名字上，不会落在 <sha>.part 上被暂存区清理（PFCleanStaging）当成
//     过期 part 删掉，崩溃时也不会被传输层当作已核验的 part 复用。放弃时再改回 <sha>.part。
//   - 覆盖时新文件保留目标原来的权限位（不同步权限位，但不能把可执行脚本覆盖成不可执行）；新建时 0644。
//
// 【边界】
//   - 名字按字节核对：覆盖、删除、改名之前读父目录，必须存在字节完全相同的名字；新建之前父目录里不能有折叠键
//     （PFFoldKey）相同的名字。路径上每一级目录都这样核对，也不许是符号链接。
//   - 回收区里的东西，除 PFRecycle.Clean 外，本文件任何地方都不删。
//   - 暂存区里只动 <sha>.part（核对不符时删它和同名 .meta）、自己建的 .pfswap-*/.pfcopy-*。
//   - 不管：判定、传输、base 更新、冲突副本取名（PFConflictName）、台账（flock）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ── 结果 ────────────────────────────────────────────────

// PFApplyStatus 是一步写原语的结果种类。
type PFApplyStatus int

const (
	PFApplyDone       PFApplyStatus = iota + 1 // 完成
	PFApplyAbandoned                           // 放弃：这一步没做，现场已恢复（Uncertain 时没能完全恢复）
	PFApplyDeferred                            // 推迟：Office 锁、iCloud 占位，本轮不动
	PFApplyOutOfScope                          // 超出范围：只读、名字对不上、符号链接等，需要人看
)

func (s PFApplyStatus) String() string {
	switch s {
	case PFApplyDone:
		return "完成"
	case PFApplyAbandoned:
		return "放弃"
	case PFApplyDeferred:
		return "推迟"
	case PFApplyOutOfScope:
		return "超出范围"
	}
	return fmt.Sprintf("未知(%d)", int(s))
}

// 原因代码（PFApplyResult.Code）：给上层按类处理；给人看的是 Reason。
const (
	PFApplyCodeOK              = "ok"
	PFApplyCodeTargetExists    = "target_exists"     // 要新建的名字已经有了
	PFApplyCodeTargetMissing   = "target_missing"    // 要动的文件不见了
	PFApplyCodeTargetChanged   = "target_changed"    // 执行前核对：大小/mtime/ino 与判定时不同
	PFApplyCodeChangedInWindow = "changed_in_window" // 执行后核对：核对之后、改名/交换之前文件被改，已恢复
	PFApplyCodePartMissing     = "part_missing"      // 暂存区里没有 part
	PFApplyCodePartMismatch    = "part_mismatch"     // part 内容与期望的 sha256 不符
	PFApplyCodePartChanged     = "part_changed"      // part 在安装过程中被动过
	PFApplyCodeSourceChanged   = "source_changed"    // 复制来源内容与期望不符
	PFApplyCodeNameMismatch    = "name_mismatch"     // 父目录里没有字节完全相同的名字（大小写/NFC/NFD 不同）
	PFApplyCodeNameClash       = "name_clash"        // 父目录里有折叠键相同、字节不同的名字
	PFApplyCodeOfficeLock      = "office_lock"       // Office 锁在目标边
	PFApplyCodeICloud          = "icloud"            // iCloud 占位文件
	PFApplyCodeReadOnly        = "read_only"         // 目标或所在目录不可写
	PFApplyCodeImmutable       = "immutable"         // 带 uchg 等不可改标志
	PFApplyCodeNotRegular      = "not_regular"       // 目标不是普通文件（目录、符号链接、特殊文件）
	PFApplyCodeNotDir          = "not_dir"           // 路径上有不是目录的一级，或要建/删的目录位置是文件
	PFApplyCodeSymlinkInPath   = "symlink_in_path"   // 路径上某一级是符号链接
	PFApplyCodeBadName         = "bad_name"          // 名字按规则不同步或超出范围
	PFApplyCodeCaseOnlyRename  = "case_only_rename"  // 只差大小写/NFC 的改名，第一版不做
	PFApplyCodeUnsupportedFS   = "unsupported_fs"    // 文件系统不支持不覆盖改名或原子交换
	PFApplyCodeDiskFull        = "disk_full"
	PFApplyCodeIO              = "io_error"
	PFApplyCodeNotEmpty        = "not_empty" // 要删的目录不空
	PFApplyCodeCanceled        = "canceled"
	PFApplyCodeHookAbort       = "hook_abort" // 测试钩子模拟的中断
)

// PFApplyResult 是一步写原语的结果。
type PFApplyResult struct {
	Status    PFApplyStatus
	Code      string // 原因代码；完成时是 PFApplyCodeOK
	Reason    string // 中文原因；完成时可空
	Rel       string // 主要路径（单元内相对路径）
	Uncertain bool   // 放弃时：没能完全恢复原状，上层要停下这个单元本轮后续写入并报告
	Err       error  // 底层错误（如果有）

	// Installed 是完成后目标位置上那份文件的现状（安装、改名后），给上层写 base 用。
	Installed *PFFileInfo
	// Recycled 是进了同步回收的那一份的记录（覆盖、删除）；放弃时也可能有（内容留在回收里，见 Residue）。
	Recycled *PFRecycleEntry
	// Existed 在 Mkdir 时表示目录本来就有。
	Existed bool
	// StaleLocks 是被当作残留、没有挡住这一步的 Office 锁文件（超过 12 小时）。
	StaleLocks []string
	// Notes 是不影响结果的提示（如回收记账写失败、目录 fsync 失败）。
	Notes []string
	// Residue 说明放弃但没能完全恢复时，内容现在在哪。
	Residue []string
}

func (r PFApplyResult) String() string {
	s := fmt.Sprintf("%s[%s] %s", r.Status, r.Code, r.Reason)
	if r.Rel != "" {
		s += "（" + r.Rel + "）"
	}
	if r.Uncertain {
		s += " 不确定"
	}
	if len(r.Residue) > 0 {
		s += " 残留：" + strings.Join(r.Residue, "；")
	}
	return s
}

// PFFileInfo 是一份文件落盘后的现状。
type PFFileInfo struct {
	Size    int64
	MtimeNs int64
	Ino     uint64
	SHA256  string
}

// PFExpect 是判定时看到的「目标现状」，原语执行前后都要核对。
type PFExpect struct {
	Absent  bool   // true：期望目标不存在（其余字段忽略）
	Size    int64  // 期望的大小
	MtimeNs int64  // 期望的 mtime（纳秒）
	Ino     uint64 // 期望的 ino；0 表示不核对 ino
	SHA256  string // 期望的内容哈希（小写十六进制）
}

// PFExpectAbsent 返回「目标不存在」的前提。
func PFExpectAbsent() PFExpect { return PFExpect{Absent: true} }

// PFExpectFile 返回「目标存在且是这份」的前提。
func PFExpectFile(size, mtimeNs int64, ino uint64, sha256hex string) PFExpect {
	return PFExpect{Size: size, MtimeNs: mtimeNs, Ino: ino, SHA256: sha256hex}
}

// PFPart 是暂存区里已核验的 part：路径必须是 <暂存区>/<sha256>.part。
type PFPart struct {
	Path   string
	SHA256 string
	Size   int64
}

// PFCopySrc 是本机复制复用的来源：项目里别处、回收区或暂存区里的一份文件，内容必须是 SHA256。
type PFCopySrc struct {
	Path   string
	SHA256 string
	Size   int64
}

// PFApplyConfigError 是配置错误（参数不合法、不同文件系统等）：此时什么都没动，上层整个停下报告。
type PFApplyConfigError struct{ Msg string }

func (e *PFApplyConfigError) Error() string { return "projsync: 配置错误：" + e.Msg }

// PFApplyHook 是测试钩子，见文件头。
type PFApplyHook func(window string) error

var errPFNoClone = errors.New("本平台没有 clonefile")

// pfFlagsImmutable 是 darwin st_flags 里「不许改」的几个标志：UF_IMMUTABLE（uchg）、UF_APPEND、SF_IMMUTABLE、SF_APPEND。
const pfFlagsImmutable uint32 = 0x2 | 0x4 | 0x20000 | 0x40000

// PFApplyDefaultLockStale 是 Office 锁文件当残留的时长（§4.2 第 9 条）。
const PFApplyDefaultLockStale = 12 * time.Hour

// ── 执行器 ──────────────────────────────────────────────

// PFApplyConfig 是一个单元的写原语执行器的配置。
type PFApplyConfig struct {
	Root    string     // 单元目录（绝对路径）；原语的 rel 都相对它
	UnitRel string     // 单元相对同步根的路径，进回收区的路径用（如 "2026/260403J1001-名称"）
	Staging string     // 暂存区目录（绝对路径），与 Root 必须同一个文件系统
	Recycle *PFRecycle // 同步回收区，与 Root 必须同一个文件系统
	Round   string     // 轮次号，写进回收 index

	Now       func() time.Time // 缺省 time.Now
	Stat      PFStatFunc       // 缺省 PFLstat；测试用它模拟 iCloud 标志、不同卷
	LockStale time.Duration    // Office 锁当残留的时长，缺省 12 小时
	Hook      PFApplyHook
}

// PFApplier 执行写原语。不并发安全：一个单元同一时间一个调用者。
type PFApplier struct {
	cfg  PFApplyConfig
	root string
	stg  string
}

// NewPFApplier 检查配置并建执行器。暂存区不存在会建（0700）。
// 暂存区、回收区与 Root 不在同一个文件系统是配置错误（§3.5、§5.6）。
func NewPFApplier(cfg PFApplyConfig) (*PFApplier, error) {
	bad := func(format string, a ...any) (*PFApplier, error) {
		return nil, &PFApplyConfigError{Msg: fmt.Sprintf(format, a...)}
	}
	if !filepath.IsAbs(cfg.Root) {
		return bad("单元目录 %q 不是绝对路径", cfg.Root)
	}
	if !filepath.IsAbs(cfg.Staging) {
		return bad("暂存区 %q 不是绝对路径", cfg.Staging)
	}
	if err := validRel(cfg.UnitRel); err != nil {
		return bad("单元 rel 不合法：%v", err)
	}
	if cfg.Recycle == nil {
		return bad("没有给同步回收区")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Stat == nil {
		cfg.Stat = PFLstat
	}
	if cfg.LockStale <= 0 {
		cfg.LockStale = PFApplyDefaultLockStale
	}
	a := &PFApplier{cfg: cfg, root: filepath.Clean(cfg.Root), stg: filepath.Clean(cfg.Staging)}
	rs, err := a.cfg.Stat(a.root)
	if err != nil {
		return bad("读取单元目录 %s 失败：%v", a.root, err)
	}
	if rs.Kind != PFKindDir {
		return bad("单元目录 %s 不是真实目录", a.root)
	}
	if err := pfMkdirAllSync(a.stg, pfDirPerm); err != nil {
		return bad("建暂存区 %s 失败：%v", a.stg, err)
	}
	ss, err := a.cfg.Stat(a.stg)
	if err != nil {
		return bad("读取暂存区 %s 失败：%v", a.stg, err)
	}
	if ss.Kind != PFKindDir {
		return bad("暂存区 %s 不是真实目录", a.stg)
	}
	if ss.Dev != rs.Dev {
		return bad("暂存区 %s 与单元目录 %s 不在同一个文件系统（st_dev 不同），放到正位是改名、不能跨盘", a.stg, a.root)
	}
	if err := cfg.Recycle.Check(a.root); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *PFApplier) abs(rel string) string { return filepath.Join(a.root, filepath.FromSlash(rel)) }

// hit 在窗口 window 调钩子；钩子返回 error 时给出「崩溃」结果，调用方要立刻返回它。
func (a *PFApplier) hit(res PFApplyResult, window string) (PFApplyResult, bool) {
	if a.cfg.Hook == nil {
		return res, false
	}
	if err := a.cfg.Hook(window); err != nil {
		res.Status, res.Code, res.Uncertain, res.Err = PFApplyAbandoned, PFApplyCodeHookAbort, true, err
		res.Reason = fmt.Sprintf("在「%s」处中断：%v", window, err)
		return res, true
	}
	return res, false
}

func pfResult(rel string, st PFApplyStatus, code, format string, args ...any) PFApplyResult {
	return PFApplyResult{Status: st, Code: code, Rel: rel, Reason: fmt.Sprintf(format, args...)}
}

// pfClassifyErr 把写操作（改名、建目录等）失败归成结果。调用方保证此时现场已恢复。
func pfClassifyErr(rel, what string, err error) PFApplyResult {
	var r PFApplyResult
	switch {
	case pfIsErrno(err, syscall.EACCES, syscall.EPERM, syscall.EROFS):
		r = pfResult(rel, PFApplyOutOfScope, PFApplyCodeReadOnly, "%s被拒绝（只读、没有权限或带不可改标志）", what)
	case pfIsErrno(err, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.EINVAL, syscall.ENOSYS):
		r = pfResult(rel, PFApplyOutOfScope, PFApplyCodeUnsupportedFS, "%s失败：这个文件系统不支持不覆盖改名或原子交换", what)
	case pfIsErrno(err, syscall.ENOSPC, syscall.EDQUOT):
		r = pfResult(rel, PFApplyAbandoned, PFApplyCodeDiskFull, "%s失败：磁盘空间不足", what)
	case pfIsErrno(err, syscall.ENOENT):
		r = pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "%s失败：路径上有东西刚被删除或改名", what)
	default:
		r = pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "%s失败", what)
	}
	r.Err = err
	return r
}

// ── 名字检查 ────────────────────────────────────────────

// checkRel 检查 rel 是不是同步范围内的合法名字（路径结构、长度、控制字符、包目录、排除清单）。
func (a *PFApplier) checkRel(rel string, isDir bool) *PFApplyResult {
	if err := validRel(rel); err != nil {
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeBadName, "路径不合法：%v", err)
		return &r
	}
	if oos := PFCheckPath(rel, isDir); oos != nil {
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeBadName, "超出同步范围：%s", oos.Reason)
		return &r
	}
	segs := strings.Split(rel, "/")
	for i := range segs {
		segDir := i < len(segs)-1 || isDir
		parent := strings.Join(segs[:i], "/")
		if why, ex := PFExclude(segs[i], segDir, parent); ex {
			r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeBadName, "按规则不同步：%s", why)
			return &r
		}
	}
	return nil
}

// pfWalk 是逐级按字节核对路径的结果。
type pfWalk struct {
	dir     string      // 叶子所在目录；上级还没建出来时是已存在的最深一级
	missing []string    // 叶子之前还不存在的目录段（要新建时才允许非空）
	base    string      // 叶子名字
	names   []string    // 叶子所在目录里的全部名字（missing 为空时有效）
	leaf    fs.DirEntry // 叶子所在目录里字节完全相同的项；没有为 nil
	fold    []string    // 叶子所在目录里折叠键相同但字节不同的名字
}

// walk 从单元目录往下，每一级都读目录、确认存在字节完全相同的名字（Mac 上按名字打开可能打到别的文件）：
//   - 中间各级必须是真实目录，不能是符号链接；
//   - 某一级没有字节相同的名字，但有折叠键相同的 → 超出范围（name_clash）；
//   - 上级不存在：allowMissing 时记进 missing（之后由 mkParents 建），否则放弃（target_missing）。
func (a *PFApplier) walk(rel string, allowMissing bool) (*pfWalk, *PFApplyResult) {
	segs := strings.Split(rel, "/")
	w := &pfWalk{base: segs[len(segs)-1], dir: a.root}
	cur := a.root
	for i, seg := range segs {
		last := i == len(segs)-1
		if len(w.missing) > 0 {
			if !last {
				w.missing = append(w.missing, seg)
			}
			continue
		}
		ents, err := os.ReadDir(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				r := pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "上级目录 %s 刚被删除或改名", cur)
				r.Err = err
				return nil, &r
			}
			r := pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "读取目录 %s 失败", cur)
			r.Err = err
			return nil, &r
		}
		var hit fs.DirEntry
		for _, e := range ents {
			if e.Name() == seg {
				hit = e
				break
			}
		}
		var fold []string
		if hit == nil {
			key := PFFoldKey(seg)
			for _, e := range ents {
				if PFFoldKey(e.Name()) == key {
					fold = append(fold, e.Name())
				}
			}
		}
		if last {
			w.dir, w.leaf, w.fold = cur, hit, fold
			w.names = make([]string, len(ents))
			for j, e := range ents {
				w.names[j] = e.Name()
			}
			return w, nil
		}
		// 中间一级
		switch {
		case hit != nil && hit.Type()&fs.ModeSymlink != 0:
			r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeSymlinkInPath, "路径上的 %q 是符号链接，不穿过它写入", seg)
			return nil, &r
		case hit != nil && !hit.IsDir():
			r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotDir, "路径上的 %q 在磁盘上是文件、不是目录", seg)
			return nil, &r
		case hit != nil:
			cur = filepath.Join(cur, seg)
			w.dir = cur
		case len(fold) > 0:
			r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeNameClash, "上级目录 %q 在磁盘上只有 %q（只差大小写或 NFC/NFD），不按名字打开，避免打到另一个目录", seg, fold[0])
			return nil, &r
		case allowMissing:
			w.missing = append(w.missing, seg)
		default:
			r := pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "上级目录 %q 不存在", seg)
			return nil, &r
		}
	}
	return w, nil // 走不到：最后一段在循环里返回
}

// leafDir 是叶子所在目录的完整路径（上级还没建时是将要建出来的路径）。
func (w *pfWalk) leafDir() string {
	d := w.dir
	for _, m := range w.missing {
		d = filepath.Join(d, m)
	}
	return d
}

// mkParents 建出 w.missing 里的目录（0755），每建一级 fsync 它的父目录；返回本次新建的目录（从浅到深）。
func mkParents(w *pfWalk) (created []string, err error) {
	cur := w.dir
	for _, seg := range w.missing {
		cur = filepath.Join(cur, seg)
		if err := os.Mkdir(cur, 0o755); err != nil {
			return created, err
		}
		created = append(created, cur)
		_ = os.Chmod(cur, 0o755) // 不受 umask 影响
		if err := pfSyncDir(filepath.Dir(cur)); err != nil {
			return created, err
		}
	}
	return created, nil
}

// undoParents 把 mkParents 新建的目录倒着删掉（只删空的；有人往里放了东西就留着）。
func undoParents(created []string) {
	for i := len(created) - 1; i >= 0; i-- {
		_ = syscall.Rmdir(created[i])
	}
}

// ── 目标检查 ────────────────────────────────────────────

// dirWritable 检查目录能不能写（带 uchg 或没有写权限 → 超出范围）。
func (a *PFApplier) dirWritable(rel, dir string) *PFApplyResult {
	st, err := a.cfg.Stat(dir)
	if err != nil {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "读取目录 %s 失败", dir)
		if errors.Is(err, fs.ErrNotExist) {
			r.Code = PFApplyCodeTargetMissing
		}
		r.Err = err
		return &r
	}
	if st.Flags&pfFlagsImmutable != 0 {
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeImmutable, "所在目录 %s 带 uchg 等不可改标志，不写入", dir)
		return &r
	}
	if err := pfAccessWritable(dir, true); err != nil {
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeReadOnly, "所在目录 %s 不可写", dir)
		r.Err = err
		return &r
	}
	return nil
}

// iCloudStub 在叶子所在目录里找 .X.icloud 占位（X 与叶子名折叠键相同）。
func (w *pfWalk) iCloudStub() string {
	key := PFFoldKey(w.base)
	for _, n := range w.names {
		if orig, ok := PFICloudPlaceholder(n); ok && PFFoldKey(orig) == key {
			return n
		}
	}
	return ""
}

// officeLocks 在叶子所在目录里找 base 的 Office 锁文件：返回仍有效的锁和被当作残留的锁（mtime 超过 LockStale）。
func (a *PFApplier) officeLocks(w *pfWalk) (live, stale []string) {
	for _, n := range PFOfficeLocks(w.names, w.base) {
		fi, err := os.Lstat(filepath.Join(w.dir, n))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // 刚好关掉了
			}
			live = append(live, n) // 看不了锁文件的状态：宁可当有锁
			continue
		}
		if a.cfg.Now().Sub(fi.ModTime()) > a.cfg.LockStale {
			stale = append(stale, n)
		} else {
			live = append(live, n)
		}
	}
	return live, stale
}

func pfDiffExpect(st PFStat, want PFExpect) string {
	var d []string
	if st.Size != want.Size {
		d = append(d, fmt.Sprintf("大小 %d→%d", want.Size, st.Size))
	}
	if st.MtimeNs != want.MtimeNs {
		d = append(d, "修改时间不同")
	}
	if want.Ino != 0 && st.Ino != want.Ino {
		d = append(d, "文件已被换成另一个（ino 不同）")
	}
	return strings.Join(d, "、")
}

// pfTarget 是 checkExisting 通过后的目标现状。
type pfTarget struct {
	w     *pfWalk
	st    PFStat
	stale []string
}

// checkExisting 是覆盖、删除、改名前对「已有文件」的全部检查（不读内容）：
// 字节名、iCloud、类型、与判定时的现状比对、不可改标志、可写、Office 锁。
func (a *PFApplier) checkExisting(rel string, want PFExpect, checkLock bool) (*pfTarget, *PFApplyResult) {
	w, r := a.walk(rel, false)
	if r != nil {
		return nil, r
	}
	abs := a.abs(rel)
	if w.leaf == nil {
		if len(w.fold) > 0 {
			r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeNameMismatch, "目录里没有字节完全相同的名字 %q，只有 %q（只差大小写或 NFC/NFD）；不按名字打开，避免动到另一个文件", w.base, w.fold[0])
			return nil, &r
		}
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "目标文件已不存在（判定之后被删除或改名）")
		return nil, &r
	}
	if stub := w.iCloudStub(); stub != "" {
		r := pfResult(rel, PFApplyDeferred, PFApplyCodeICloud, "目录里有 iCloud 占位文件 %q，这个文件的内容可能还在云端，不动", stub)
		return nil, &r
	}
	st, err := a.cfg.Stat(abs)
	if err != nil {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "读取目标信息失败")
		if errors.Is(err, fs.ErrNotExist) {
			r = pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "目标文件已不存在（判定之后被删除或改名）")
		}
		r.Err = err
		return nil, &r
	}
	switch st.Kind {
	case PFKindFile:
	case PFKindSymlink:
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotRegular, "目标现在是符号链接，不处理")
		return nil, &r
	case PFKindDir:
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotRegular, "目标现在是目录、不是文件（一边文件一边目录）")
		return nil, &r
	default:
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotRegular, "目标是特殊文件（FIFO、socket、设备），不处理")
		return nil, &r
	}
	if st.Dataless() {
		r := pfResult(rel, PFApplyDeferred, PFApplyCodeICloud, "目标是 iCloud 占位文件（内容在云端），不读取、不覆盖，等下载好再来")
		return nil, &r
	}
	if diff := pfDiffExpect(st, want); diff != "" {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetChanged, "目标与判定时看到的不同：%s", diff)
		return nil, &r
	}
	if st.Flags&pfFlagsImmutable != 0 {
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeImmutable, "目标带 uchg 等不可改标志，不动")
		return nil, &r
	}
	if err := pfAccessWritable(abs, false); err != nil {
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeReadOnly, "目标文件只读，不动")
		r.Err = err
		return nil, &r
	}
	if r := a.dirWritable(rel, w.dir); r != nil {
		return nil, r
	}
	t := &pfTarget{w: w, st: st}
	if checkLock {
		live, stale := a.officeLocks(w)
		if len(live) > 0 {
			r := pfResult(rel, PFApplyDeferred, PFApplyCodeOfficeLock, "文件旁有 Office 锁 %q，可能正被打开，推迟到关闭之后", live[0])
			r.StaleLocks = stale
			return nil, &r
		}
		t.stale = stale
	}
	return t, nil
}

// checkNewName 是新建一个名字前的检查：叶子不能已存在、不能有折叠键相同的、没有 iCloud 占位、所在目录可写。
func (a *PFApplier) checkNewName(rel string, w *pfWalk) *PFApplyResult {
	if len(w.missing) == 0 {
		if w.leaf != nil {
			var r PFApplyResult
			if w.leaf.IsDir() {
				r = pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotRegular, "目标位置已经是一个目录（一边文件一边目录）")
			} else {
				r = pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetExists, "目标已存在（判定之后有人建了同名文件）")
			}
			return &r
		}
		if len(w.fold) > 0 {
			r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeNameClash, "目录里已有 %q，与要新建的 %q 只差大小写或 NFC/NFD；在不区分大小写的磁盘上会打到它，不新建", w.fold[0], w.base)
			return &r
		}
		if stub := w.iCloudStub(); stub != "" {
			r := pfResult(rel, PFApplyDeferred, PFApplyCodeICloud, "目录里有 iCloud 占位文件 %q，这个名字在云端已有文件，不新建", stub)
			return &r
		}
	}
	return a.dirWritable(rel, w.dir)
}

// ── 哈希 ────────────────────────────────────────────────

var errPFChangedWhileHashing = errors.New("读取过程中文件被改动")

// pfFacts 是一份文件的状态加内容哈希。
type pfFacts struct {
	Stat PFStat
	SHA  string
}

func pfStatFromInfo(fi fs.FileInfo) (PFStat, error) {
	m := fi.Mode()
	st := PFStat{Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano(), Mode: m}
	switch {
	case m&fs.ModeSymlink != 0:
		st.Kind = PFKindSymlink
	case m.IsDir():
		st.Kind = PFKindDir
	case m.IsRegular():
		st.Kind = PFKindFile
	default:
		st.Kind = PFKindOther
	}
	if err := pfFillSys(fi, &st); err != nil {
		return PFStat{}, err
	}
	return st, nil
}

// pfHashFile 以 O_NOFOLLOW 打开 p 算 sha256。状态取自同一个打开的文件（fstat），读之前读之后各取一次，
// 大小或 mtime 变了就返回 errPFChangedWhileHashing（读的过程中被改，哈希不可信）。doSync 为 true 时读完 fsync 一次。
func pfHashFile(ctx context.Context, p string, doSync bool) (pfFacts, error) {
	f, err := pfOpenNoFollow(p)
	if err != nil {
		return pfFacts{}, err
	}
	defer f.Close()
	fi1, err := f.Stat()
	if err != nil {
		return pfFacts{}, err
	}
	st1, err := pfStatFromInfo(fi1)
	if err != nil {
		return pfFacts{}, err
	}
	if st1.Kind != PFKindFile {
		return pfFacts{}, fmt.Errorf("%s 不是普通文件", p)
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, ctxReader{ctx, f}, make([]byte, 1<<20))
	if err != nil {
		return pfFacts{}, err
	}
	if doSync {
		if err := f.Sync(); err != nil {
			return pfFacts{}, err
		}
	}
	fi2, err := f.Stat()
	if err != nil {
		return pfFacts{}, err
	}
	st2, err := pfStatFromInfo(fi2)
	if err != nil {
		return pfFacts{}, err
	}
	if n != st1.Size || st2.Size != st1.Size || st2.MtimeNs != st1.MtimeNs {
		return pfFacts{}, errPFChangedWhileHashing
	}
	return pfFacts{Stat: st2, SHA: hex.EncodeToString(h.Sum(nil))}, nil
}

// pfMatchWant 核对一份文件是不是期望的那份；返回空串表示是，否则是差异说明。
func pfMatchWant(f pfFacts, ino uint64, want PFExpect) string {
	var d []string
	if f.Stat.Kind != PFKindFile {
		d = append(d, "不是普通文件")
	}
	if f.Stat.Size != want.Size {
		d = append(d, fmt.Sprintf("大小 %d（期望 %d）", f.Stat.Size, want.Size))
	}
	if f.Stat.MtimeNs != want.MtimeNs {
		d = append(d, "修改时间不同")
	}
	if ino != 0 && f.Stat.Ino != ino {
		d = append(d, "不是原来那个文件（ino 不同）")
	}
	if f.SHA != want.SHA256 {
		d = append(d, "内容哈希不同")
	}
	return strings.Join(d, "、")
}

// ── part 的核对与准备 ───────────────────────────────────

func (a *PFApplier) checkPartArgs(rel string, part PFPart) error {
	if !pfValidSHA(part.SHA256) {
		return &PFApplyConfigError{Msg: fmt.Sprintf("part 的 sha256 %q 不合法", part.SHA256)}
	}
	if part.Size < 0 {
		return &PFApplyConfigError{Msg: "part 的大小不能为负"}
	}
	if filepath.Clean(part.Path) != filepath.Join(a.stg, part.SHA256+".part") {
		return &PFApplyConfigError{Msg: fmt.Sprintf("part %q 不在暂存区 %s，或名字不是 <sha256>.part", part.Path, a.stg)}
	}
	return nil
}

func (a *PFApplier) checkExpectArgs(want PFExpect) error {
	if want.Absent {
		return &PFApplyConfigError{Msg: "这个原语的前提必须是「目标存在」，不能是 Absent"}
	}
	if !pfValidSHA(want.SHA256) {
		return &PFApplyConfigError{Msg: fmt.Sprintf("前提里的 sha256 %q 不合法", want.SHA256)}
	}
	return nil
}

// verifyPart 在本机重新算 part 的 sha256 与期望比（放到正位前必须再核一次），顺手 fsync。
// part 与目标不在同一文件系统返回配置错误；内容不符就把这份 part 和同名 .meta 删掉（否则传输层会按已核验复用它），
// 返回放弃。读不了、读的过程中被改、被取消都只放弃，不删。
func (a *PFApplier) verifyPart(ctx context.Context, rel string, part PFPart) (pfFacts, *PFApplyResult, error) {
	st, err := a.cfg.Stat(part.Path)
	if err != nil {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "读取暂存文件 %s 失败", filepath.Base(part.Path))
		if errors.Is(err, fs.ErrNotExist) {
			r = pfResult(rel, PFApplyAbandoned, PFApplyCodePartMissing, "暂存区里没有这份文件（%s）", filepath.Base(part.Path))
		}
		r.Err = err
		return pfFacts{}, &r, nil
	}
	if st.Kind != PFKindFile {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodePartMismatch, "暂存区里的 %s 不是普通文件", filepath.Base(part.Path))
		return pfFacts{}, &r, nil
	}
	rs, err := a.cfg.Stat(a.root)
	if err != nil {
		return pfFacts{}, nil, &PFApplyConfigError{Msg: fmt.Sprintf("读取单元目录 %s 失败：%v", a.root, err)}
	}
	if st.Dev != rs.Dev {
		return pfFacts{}, nil, &PFApplyConfigError{Msg: fmt.Sprintf("暂存文件 %s 与单元目录 %s 不在同一个文件系统，放到正位是改名、不能跨盘", part.Path, a.root)}
	}
	f, err := pfHashFile(ctx, part.Path, true)
	if err != nil {
		var r PFApplyResult
		switch {
		case ctx.Err() != nil && errors.Is(err, ctx.Err()):
			r = pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消")
		case errors.Is(err, errPFChangedWhileHashing):
			r = pfResult(rel, PFApplyAbandoned, PFApplyCodePartChanged, "暂存文件在核对哈希时被改动（可能还在传输），本轮不装")
		default:
			r = pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "核对暂存文件的哈希失败")
		}
		r.Err = err
		return pfFacts{}, &r, nil
	}
	if f.SHA != part.SHA256 || f.Stat.Size != part.Size {
		_ = os.Remove(part.Path)
		_ = os.Remove(strings.TrimSuffix(part.Path, ".part") + ".meta")
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodePartMismatch,
			"暂存文件内容与期望不符（本机算出 sha256=%s 大小=%d，期望 sha256=%s 大小=%d），已删掉这份暂存文件，需要重新传", f.SHA, f.Stat.Size, part.SHA256, part.Size)
		return pfFacts{}, &r, nil
	}
	return f, nil, nil
}

// pfPartPrep 记着 part 被改过的权限和 mtime，放弃时改回去（传输层按 meta 里记的 mtime 判断能不能复用）。
type pfPartPrep struct {
	mode  fs.FileMode
	mtime time.Time
}

// preparePart 给 part 设上要安装的 mtime 和权限（在放到正位之前设，正位上的文件从来没有过错的 mtime）。
func preparePart(p string, orig PFStat, mtimeNs int64, mode fs.FileMode) (pfPartPrep, error) {
	prep := pfPartPrep{mode: orig.Mode.Perm(), mtime: time.Unix(0, orig.MtimeNs)}
	t := time.Unix(0, mtimeNs)
	if err := os.Chtimes(p, t, t); err != nil {
		return prep, err
	}
	if err := os.Chmod(p, mode); err != nil {
		prep.undo(p)
		return prep, err
	}
	return prep, nil
}

func (p pfPartPrep) undo(path string) {
	_ = os.Chmod(path, p.mode)
	_ = os.Chtimes(path, p.mtime, p.mtime)
}

// ── 安装（新建）──────────────────────────────────────────

// InstallNew 把暂存区里的 part 放到 rel：目标必须不存在，用不覆盖的改名（darwin renamex_np(RENAME_EXCL)、
// linux renameat2(RENAME_NOREPLACE)），mtime 设成 mtimeNs（源的 mtime），权限 0644，按需建父目录（0755），fsync 目录。
// 放到正位前在本机重新算 part 的 sha256，与 part.SHA256 不符就删掉 part 并放弃。
//
// 钩子窗口：new:before-rename（核对和建父目录都做完、改名之前）、new:after-rename（改名之后、核对之前）。
// 竞态：别人在窗口里建了同名文件 → EEXIST → 放弃，part 保持原样（权限和 mtime 改回），自己建的空目录删掉。
func (a *PFApplier) InstallNew(ctx context.Context, rel string, part PFPart, mtimeNs int64) (PFApplyResult, error) {
	res := PFApplyResult{Rel: rel}
	if err := a.checkPartArgs(rel, part); err != nil {
		return res, err
	}
	if r := a.checkRel(rel, false); r != nil {
		return *r, nil
	}
	if ctx.Err() != nil {
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消"), nil
	}
	// 先做不花力气的检查：目标位置被占着时不必白算一遍大文件的哈希
	if w, r := a.walk(rel, true); r != nil {
		return *r, nil
	} else if r := a.checkNewName(rel, w); r != nil {
		return *r, nil
	}
	pf, r, err := a.verifyPart(ctx, rel, part)
	if err != nil || r != nil {
		if r != nil {
			return *r, err
		}
		return res, err
	}
	// 哈希花了时间，重新核对一遍
	w, r := a.walk(rel, true)
	if r != nil {
		return *r, nil
	}
	if r := a.checkNewName(rel, w); r != nil {
		return *r, nil
	}
	prep, err := preparePart(part.Path, pf.Stat, mtimeNs, 0o644)
	if err != nil {
		return pfClassifyErr(rel, "设置暂存文件的 mtime 和权限", err), nil
	}
	pst, err := a.cfg.Stat(part.Path)
	if err != nil {
		prep.undo(part.Path)
		return pfClassifyErr(rel, "读取暂存文件信息", err), nil
	}
	created, err := mkParents(w)
	if err != nil {
		undoParents(created)
		prep.undo(part.Path)
		if errors.Is(err, fs.ErrExist) {
			r := pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetChanged, "建上级目录时发现目录刚被别人建了，本轮不装")
			r.Err = err
			return r, nil
		}
		return pfClassifyErr(rel, "建上级目录", err), nil
	}
	if out, stop := a.hit(res, "new:before-rename"); stop {
		return out, nil
	}
	// 改名之前再确认 part 还是我们核对过的那个（传输层不该动它，这里只是兜底）
	if cur, err := a.cfg.Stat(part.Path); err != nil || cur.Ino != pst.Ino || cur.Size != pst.Size || cur.MtimeNs != pst.MtimeNs {
		undoParents(created)
		prep.undo(part.Path)
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodePartChanged, "暂存文件在安装前被改动，本轮不装")
		r.Err = err
		return r, nil
	}
	target := a.abs(rel)
	if err := pfRenameNoReplace(part.Path, target); err != nil {
		fail := a.newRenameFailure(rel, w, err)
		undoParents(created)
		prep.undo(part.Path)
		return fail, nil
	}
	if out, stop := a.hit(res, "new:after-rename"); stop {
		return out, nil
	}
	res.Status, res.Code = PFApplyDone, PFApplyCodeOK
	for _, d := range []string{filepath.Dir(target), a.stg} {
		if e := pfSyncDir(d); e != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("目录 %s fsync 失败：%v", d, e))
		}
	}
	// 执行后再核对：正位上的文件就是我们放的那个
	now, err := a.cfg.Stat(target)
	if err != nil || now.Kind != PFKindFile || now.Ino != pst.Ino || now.Size != pst.Size || now.MtimeNs != pst.MtimeNs {
		res.Notes = append(res.Notes, "装好之后目标文件又被改动或换掉了（内容没有丢，下一轮会当作目标一边的新改动处理）")
	}
	if pst.MtimeNs != mtimeNs {
		res.Notes = append(res.Notes, fmt.Sprintf("文件系统的时间精度不够，mtime 实际记成了 %d 纳秒（要求 %d）", pst.MtimeNs, mtimeNs))
	}
	res.Installed = &PFFileInfo{Size: pst.Size, MtimeNs: pst.MtimeNs, Ino: pst.Ino, SHA256: part.SHA256}
	return res, nil
}

// newRenameFailure 把「不覆盖改名失败」归类：EEXIST 要分清是别人刚建了这个名字，还是磁盘认为同名而折叠表没认出（§4.2 第 14 条）。
func (a *PFApplier) newRenameFailure(rel string, w *pfWalk, err error) PFApplyResult {
	if pfIsErrno(err, syscall.EEXIST) {
		ents, rerr := os.ReadDir(w.leafDir())
		if rerr == nil {
			for _, e := range ents {
				if e.Name() == w.base {
					r := pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetExists, "目标已存在（核对之后、改名之前有人建了同名文件），没有覆盖")
					r.Err = err
					return r
				}
			}
		}
		r := pfResult(rel, PFApplyOutOfScope, PFApplyCodeNameClash, "磁盘认为 %q 已存在，但目录里没有字节相同的名字（大小写或 NFC/NFD 同名），不覆盖，这个路径停下", w.base)
		r.Err = err
		return r
	}
	if pfIsErrno(err, syscall.EXDEV) {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "暂存区与目标不在同一个文件系统，无法改名放入（配置错误）")
		r.Err = err
		return r
	}
	if pfIsErrno(err, syscall.ENOENT) {
		// 要么 part 没了，要么上级目录刚被别人删了/改名
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "改名失败：暂存文件或上级目录刚被删除或改名")
		r.Err = err
		return r
	}
	return pfClassifyErr(rel, "改名放入目标位置", err)
}

// ── 安装（覆盖）──────────────────────────────────────────

// InstallOver 用暂存区里的 part 覆盖 rel：目标必须仍是 want 描述的那份。
//
// 流程：检查（字节名、前提、Office 锁、可写）→ 核 part 哈希 → 再检查 → part 不覆盖地改名为暂存区私有名字（slot）
// → 与目标原子交换（darwin RENAME_SWAP、linux RENAME_EXCHANGE）→ 核对换下来的（在 slot 上）确实是 want 那份
//   - 是：移入同步回收（原因「覆盖」），写 index；
//   - 不是（核对后、交换前文件被改，或读不了）：再交换一次换回去，把 slot 改回 <sha>.part，放弃（changed_in_window）。
//
// 钩子窗口：over:before-swap（交换之前）、over:after-swap（交换之后、核对之前）、over:before-recycle（核对通过、移入回收之前）、
// over:after-recycle（移入回收之后、记账之前）。
func (a *PFApplier) InstallOver(ctx context.Context, rel string, part PFPart, mtimeNs int64, want PFExpect) (PFApplyResult, error) {
	res := PFApplyResult{Rel: rel}
	if err := a.checkPartArgs(rel, part); err != nil {
		return res, err
	}
	if err := a.checkExpectArgs(want); err != nil {
		return res, err
	}
	if r := a.checkRel(rel, false); r != nil {
		return *r, nil
	}
	if err := a.cfg.Recycle.Check(a.root); err != nil {
		return res, err
	}
	if ctx.Err() != nil {
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消"), nil
	}
	if _, r := a.checkExisting(rel, want, true); r != nil {
		return *r, nil
	}
	pf, r, err := a.verifyPart(ctx, rel, part)
	if err != nil || r != nil {
		if r != nil {
			return *r, err
		}
		return res, err
	}
	tgt, r := a.checkExisting(rel, want, true)
	if r != nil {
		return *r, nil
	}
	res.StaleLocks = tgt.stale
	if ctx.Err() != nil {
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消"), nil
	}

	// 新文件的权限：保留目标原来的权限位（至少属主可读写）
	mode := tgt.st.Mode.Perm() | 0o600
	prep, err := preparePart(part.Path, pf.Stat, mtimeNs, mode)
	if err != nil {
		return pfClassifyErr(rel, "设置暂存文件的 mtime 和权限", err), nil
	}
	pst, err := a.cfg.Stat(part.Path)
	if err != nil {
		prep.undo(part.Path)
		return pfClassifyErr(rel, "读取暂存文件信息", err), nil
	}
	slot := filepath.Join(a.stg, ".pfswap-"+part.SHA256[:8]+"-"+pfRand4())
	if err := pfRenameNoReplace(part.Path, slot); err != nil {
		prep.undo(part.Path)
		return pfClassifyErr(rel, "把暂存文件改名到交换位置", err), nil
	}
	// giveUp：交换没发生（或已换回），把 slot 改回 <sha>.part 并恢复它的权限和 mtime
	giveUp := func(r PFApplyResult) PFApplyResult {
		if err := pfRenameNoReplace(slot, part.Path); err != nil {
			r.Uncertain = true
			r.Residue = append(r.Residue, fmt.Sprintf("新内容的暂存文件留在 %s（改回 %s 失败：%v）", slot, filepath.Base(part.Path), err))
			return r
		}
		prep.undo(part.Path)
		return r
	}
	target := a.abs(rel)
	if out, stop := a.hit(res, "over:before-swap"); stop {
		return out, nil
	}
	if err := pfRenameExchange(slot, target); err != nil {
		return giveUp(pfClassifyErr(rel, "与目标原子交换", err)), nil
	}

	// ── 从这里起交换已经发生，不再响应取消，直到核对完、恢复或放进回收 ──
	ctx = context.WithoutCancel(ctx)
	if out, stop := a.hit(res, "over:after-swap"); stop {
		return out, nil
	}
	old, herr := pfHashFile(ctx, slot, false)
	diff := ""
	switch {
	case herr != nil:
		diff = "读取换下来的文件失败：" + herr.Error()
	default:
		diff = pfMatchWant(old, tgt.st.Ino, want)
	}
	if diff != "" {
		return a.swapBack(rel, slot, target, part, pst, prep, diff), nil
	}

	// 核对通过：换下来的就是期望的那份，移入同步回收
	if out, stop := a.hit(res, "over:before-recycle"); stop {
		return out, nil
	}
	entry := PFRecycleEntry{Unit: a.cfg.UnitRel, Path: rel, SHA256: old.SHA, Size: old.Stat.Size, MtimeNs: old.Stat.MtimeNs,
		Reason: string(PFReasonOverwrite), Round: a.cfg.Round}
	placed, perr := a.recycle(slot, rel, &entry)
	if perr != nil {
		// 新内容已经在正位，旧内容完好地留在暂存区私有名字上（不会被清理删），只是没进回收
		res.Status, res.Code = PFApplyDone, PFApplyCodeOK
		res.Notes = append(res.Notes, fmt.Sprintf("旧文件没能移入同步回收（%v），仍完好地留在暂存区 %s，请人工处理", perr, slot))
		res.Residue = append(res.Residue, "被换下来的旧文件："+slot)
	} else {
		res.Status, res.Code, res.Recycled = PFApplyDone, PFApplyCodeOK, &entry
		res.Notes = append(res.Notes, placed.notes...)
		if out, stop := a.hit(res, "over:after-recycle"); stop {
			return out, nil
		}
		if err := a.cfg.Recycle.record(placed.day, entry); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("回收记账（index.jsonl）写失败：%v（文件已在 %s）", err, placed.stored))
		}
	}
	if e := pfSyncDir(filepath.Dir(target)); e != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("目录 %s fsync 失败：%v", filepath.Dir(target), e))
	}
	now, err := a.cfg.Stat(target)
	if err != nil || now.Kind != PFKindFile || now.Ino != pst.Ino || now.Size != pst.Size || now.MtimeNs != pst.MtimeNs {
		res.Notes = append(res.Notes, "装好之后目标文件又被改动或换掉了（内容没有丢，下一轮会当作目标一边的新改动处理）")
	}
	if pst.MtimeNs != mtimeNs {
		res.Notes = append(res.Notes, fmt.Sprintf("文件系统的时间精度不够，mtime 实际记成了 %d 纳秒（要求 %d）", pst.MtimeNs, mtimeNs))
	}
	res.Installed = &PFFileInfo{Size: pst.Size, MtimeNs: pst.MtimeNs, Ino: pst.Ino, SHA256: part.SHA256}
	return res, nil
}

// swapBack 在交换后核对不符时把两个文件换回去：目标位置恢复成换之前的样子（现在的内容，可能是别人刚改过的），
// 新内容的 part 回到暂存区。换回去之后还要确认 slot 上的确是我们的 part（否则说明目标位置又被第三方换过）。
func (a *PFApplier) swapBack(rel, slot, target string, part PFPart, pst PFStat, prep pfPartPrep, diff string) PFApplyResult {
	reason := fmt.Sprintf("核对之后、交换之前目标被改动（%s），已换回原样，不覆盖", diff)
	if err := pfRenameExchange(slot, target); err != nil {
		// 换不回去：新内容已在正位，被换下来的（被改动过的）文件还在 slot 上。把它放进回收保住内容。
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeChangedInWindow, "%s；但换回去失败：%v", reason, err)
		r.Uncertain, r.Err = true, err
		entry := PFRecycleEntry{Unit: a.cfg.UnitRel, Path: rel, SHA256: "", Reason: string(PFReasonOverwrite), Round: a.cfg.Round,
			Note: "核对不符且换不回去：这是目标位置被改动过的内容，已被新内容覆盖"}
		if f, herr := pfHashFile(context.Background(), slot, false); herr == nil {
			entry.SHA256, entry.Size, entry.MtimeNs = f.SHA, f.Stat.Size, f.Stat.MtimeNs
		}
		if placed, perr := a.recycle(slot, rel, &entry); perr == nil {
			r.Recycled = &entry
			r.Residue = append(r.Residue, "目标位置被改动过的旧内容已放进同步回收："+placed.stored)
			if err := a.cfg.Recycle.record(placed.day, entry); err != nil {
				r.Notes = append(r.Notes, fmt.Sprintf("回收记账写失败：%v", err))
			}
		} else {
			r.Residue = append(r.Residue, "目标位置被改动过的旧内容留在暂存区："+slot)
		}
		return r
	}
	r := pfResult(rel, PFApplyAbandoned, PFApplyCodeChangedInWindow, "%s", reason)
	cur, err := a.cfg.Stat(slot)
	if err != nil || cur.Ino != pst.Ino || cur.Size != pst.Size {
		// 目标位置在两次交换之间又被第三方换过：slot 上现在是第三方的文件，我们的 part 已经不在了
		r.Uncertain = true
		r.Residue = append(r.Residue, fmt.Sprintf("目标位置在交换期间又被换成了别的文件，那份留在暂存区 %s；原来被改动的文件已换回目标位置；新内容的暂存文件已不在，需要重新传", slot))
		return r
	}
	if err := pfRenameNoReplace(slot, part.Path); err != nil {
		r.Uncertain = true
		r.Residue = append(r.Residue, fmt.Sprintf("新内容的暂存文件留在 %s（改回 %s 失败：%v）", slot, filepath.Base(part.Path), err))
		return r
	}
	prep.undo(part.Path)
	return r
}

// pfRecycled 是 recycle 的结果。
type pfRecycled struct {
	stored string
	day    string
	notes  []string
}

// recycle 把 src 放进回收区并填好 entry.Stored、Time；不写 index（核对通过后才由调用方记账）。
func (a *PFApplier) recycle(src, rel string, entry *PFRecycleEntry) (pfRecycled, error) {
	t := a.cfg.Now()
	p, err := a.cfg.Recycle.place(src, t, a.cfg.UnitRel, rel)
	if err != nil {
		return pfRecycled{}, err
	}
	entry.Stored = p.Rel
	entry.Time = t.Format(time.RFC3339)
	out := pfRecycled{stored: p.Stored, day: p.Day}
	if p.Warn != nil {
		out.notes = append(out.notes, fmt.Sprintf("回收目录 fsync 失败：%v", p.Warn))
	}
	return out, nil
}

// ── 删除 ────────────────────────────────────────────────

// Delete 把 rel 用不覆盖的改名移进同步回收（原因 reason：删除或冲突），再核对移过去的确实是 want 那份：
//   - 是：记账，完成；
//   - 不是（核对后、改名前文件被改）：在原位置仍空着时用不覆盖的改名移回去，放弃；
//     原位置已被占：内容留在回收里（记账并注明），放弃，Residue 说明。
//
// 钩子窗口：del:before-move（改名之前）、del:after-move（改名之后、核对之前）、del:after-verify（核对通过、记账之前）。
func (a *PFApplier) Delete(ctx context.Context, rel string, want PFExpect, reason PFRecycleReason) (PFApplyResult, error) {
	res := PFApplyResult{Rel: rel}
	if err := a.checkExpectArgs(want); err != nil {
		return res, err
	}
	if !reason.valid() || reason == PFReasonOverwrite {
		return res, &PFApplyConfigError{Msg: fmt.Sprintf("删除的回收原因 %q 不合法（只能是「删除」或「冲突」）", reason)}
	}
	if r := a.checkRel(rel, false); r != nil {
		return *r, nil
	}
	if err := a.cfg.Recycle.Check(a.root); err != nil {
		return res, err
	}
	if ctx.Err() != nil {
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消"), nil
	}
	tgt, r := a.checkExisting(rel, want, true)
	if r != nil {
		return *r, nil
	}
	res.StaleLocks = tgt.stale
	if out, stop := a.hit(res, "del:before-move"); stop {
		return out, nil
	}
	target := a.abs(rel)
	entry := PFRecycleEntry{Unit: a.cfg.UnitRel, Path: rel, Reason: string(reason), Round: a.cfg.Round}
	placed, err := a.recycle(target, rel, &entry)
	if err != nil {
		if pfIsErrno(err, syscall.EXDEV) {
			return res, &PFApplyConfigError{Msg: fmt.Sprintf("同步回收与单元目录不在同一个文件系统，无法改名放入：%v", err)}
		}
		return pfClassifyErr(rel, "移入同步回收", err), nil
	}

	// ── 改名已经发生，不再响应取消 ──
	ctx = context.WithoutCancel(ctx)
	res.Notes = append(res.Notes, placed.notes...)
	if out, stop := a.hit(res, "del:after-move"); stop {
		return out, nil
	}
	moved, herr := pfHashFile(ctx, placed.stored, false)
	diff := ""
	if herr != nil {
		diff = "读取移走的文件失败：" + herr.Error()
	} else {
		diff = pfMatchWant(moved, tgt.st.Ino, want)
	}
	if diff != "" {
		return a.deleteRestore(rel, target, placed, entry, moved, herr, diff, res), nil
	}
	if out, stop := a.hit(res, "del:after-verify"); stop {
		return out, nil
	}
	entry.SHA256, entry.Size, entry.MtimeNs = moved.SHA, moved.Stat.Size, moved.Stat.MtimeNs
	res.Status, res.Code, res.Recycled = PFApplyDone, PFApplyCodeOK, &entry
	if err := a.cfg.Recycle.record(placed.day, entry); err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("回收记账（index.jsonl）写失败：%v（文件已在 %s）", err, placed.stored))
	}
	return res, nil
}

// deleteRestore 处理「移走之后核对发现不是期望那份」：原位置仍空着就移回去，否则留在回收里。
func (a *PFApplier) deleteRestore(rel, target string, placed pfRecycled, entry PFRecycleEntry, moved pfFacts, herr error, diff string, res PFApplyResult) PFApplyResult {
	r := pfResult(rel, PFApplyAbandoned, PFApplyCodeChangedInWindow, "核对之后、改名之前文件被改动（%s），不删除", diff)
	r.StaleLocks, r.Notes = res.StaleLocks, res.Notes
	if out, stop := a.hit(r, "del:before-restore"); stop {
		return out
	}
	err := pfRenameNoReplace(placed.stored, target)
	if err == nil {
		for _, d := range []string{filepath.Dir(target), filepath.Dir(placed.stored)} {
			_ = pfSyncDir(d)
		}
		return r // 移回去了，现场与动手前一样（改动过的文件在原位）
	}
	// 原位置被占了（或移回去失败）：内容留在回收里，如实记账
	r.Uncertain, r.Err = true, err
	entry.Note = "核对不符（删除前文件被改动），原位置已被占用，改动过的这份留在回收里，没有删除"
	if herr == nil {
		entry.SHA256, entry.Size, entry.MtimeNs = moved.SHA, moved.Stat.Size, moved.Stat.MtimeNs
	}
	r.Recycled = &entry
	r.Residue = append(r.Residue, fmt.Sprintf("被改动过的文件留在同步回收 %s（移回原位置失败：%v）", placed.stored, err))
	if err := a.cfg.Recycle.record(placed.day, entry); err != nil {
		r.Notes = append(r.Notes, fmt.Sprintf("回收记账写失败：%v", err))
	}
	return r
}

// ── 改名（本机改名复用、冲突改名）──────────────────────────

// Rename 把 from 改名为 to（都是单元内相对路径，可以在不同目录），不覆盖：to 必须不存在。
// 前提 want 描述 from 当前的样子。改名前核对（字节名、前提、Office 锁、to 不存在且没有折叠键相同的名字），
// 改名后读内容核对移过去的确实是 want 那份；不符就在 from 仍空着时用不覆盖的改名移回去，放弃。
// 只差大小写（折叠键相同）的改名第一版判超出范围（§3.3、§4.2 第 3、4 条）。不进回收。
//
// 钩子窗口：ren:before-rename、ren:after-rename、ren:before-restore。
func (a *PFApplier) Rename(ctx context.Context, from, to string, want PFExpect) (PFApplyResult, error) {
	return a.rename(ctx, from, to, want, "ren")
}

// ConflictRename 把原名文件 rel 不覆盖地改成同目录下的冲突副本名 conflictName（只是名字，不含路径）。
// 前提核对、Office 锁、竞态处理同 Rename。冲突名由 PFConflictName 生成。钩子窗口名前缀 conf:。
func (a *PFApplier) ConflictRename(ctx context.Context, rel, conflictName string, want PFExpect) (PFApplyResult, error) {
	if conflictName == "" || strings.ContainsRune(conflictName, '/') {
		return PFApplyResult{Rel: rel}, &PFApplyConfigError{Msg: fmt.Sprintf("冲突副本名 %q 不合法", conflictName)}
	}
	to := conflictName
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		to = rel[:i+1] + conflictName
	}
	return a.rename(ctx, rel, to, want, "conf")
}

func (a *PFApplier) rename(ctx context.Context, from, to string, want PFExpect, pre string) (PFApplyResult, error) {
	res := PFApplyResult{Rel: from}
	if err := a.checkExpectArgs(want); err != nil {
		return res, err
	}
	for _, p := range []string{from, to} {
		if r := a.checkRel(p, false); r != nil {
			r.Rel = from
			return *r, nil
		}
	}
	if from == to {
		return res, &PFApplyConfigError{Msg: "改名的新旧路径相同"}
	}
	if PFFoldKey(from) == PFFoldKey(to) {
		return pfResult(from, PFApplyOutOfScope, PFApplyCodeCaseOnlyRename, "新旧名字只差大小写或 NFC/NFD（%q → %q），第一版不自动改名", from, to), nil
	}
	if ctx.Err() != nil {
		return pfResult(from, PFApplyAbandoned, PFApplyCodeCanceled, "已取消"), nil
	}
	tgt, r := a.checkExisting(from, want, true)
	if r != nil {
		return *r, nil
	}
	res.StaleLocks = tgt.stale
	w2, r := a.walk(to, true)
	if r != nil {
		r.Rel = from
		return *r, nil
	}
	if r := a.checkNewName(to, w2); r != nil {
		r.Rel = from
		return *r, nil
	}
	created, err := mkParents(w2)
	if err != nil {
		undoParents(created)
		if errors.Is(err, fs.ErrExist) {
			return pfResult(from, PFApplyAbandoned, PFApplyCodeTargetChanged, "建新位置的上级目录时发现目录刚被别人建了，本轮不改名"), nil
		}
		return pfClassifyErr(from, "建上级目录", err), nil
	}
	if out, stop := a.hit(res, pre+":before-rename"); stop {
		return out, nil
	}
	src, dst := a.abs(from), a.abs(to)
	if err := pfRenameNoReplace(src, dst); err != nil {
		fail := a.newRenameFailure(from, w2, err)
		undoParents(created)
		return fail, nil
	}

	// ── 改名已经发生，不再响应取消 ──
	ctx = context.WithoutCancel(ctx)
	if out, stop := a.hit(res, pre+":after-rename"); stop {
		return out, nil
	}
	moved, herr := pfHashFile(ctx, dst, false)
	diff := ""
	if herr != nil {
		diff = "读取改名后的文件失败：" + herr.Error()
	} else {
		diff = pfMatchWant(moved, tgt.st.Ino, want)
	}
	if diff != "" {
		r := pfResult(from, PFApplyAbandoned, PFApplyCodeChangedInWindow, "核对之后、改名之前文件被改动（%s），已改回原名", diff)
		r.StaleLocks = res.StaleLocks
		if out, stop := a.hit(r, pre+":before-restore"); stop {
			return out, nil
		}
		if err := pfRenameNoReplace(dst, src); err != nil {
			r.Uncertain, r.Err = true, err
			r.Reason = fmt.Sprintf("核对之后、改名之前文件被改动（%s）；想改回原名失败：%v", diff, err)
			r.Residue = append(r.Residue, fmt.Sprintf("被改动过的文件现在在新位置 %s，原位置 %s 已被占用", to, from))
			return r, nil
		}
		undoParents(created)
		for _, d := range []string{filepath.Dir(src), filepath.Dir(dst)} {
			_ = pfSyncDir(d)
		}
		return r, nil
	}
	res.Status, res.Code = PFApplyDone, PFApplyCodeOK
	res.Rel = from
	for _, d := range []string{filepath.Dir(src), filepath.Dir(dst)} {
		if e := pfSyncDir(d); e != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("目录 %s fsync 失败：%v", d, e))
		}
	}
	res.Installed = &PFFileInfo{Size: moved.Stat.Size, MtimeNs: moved.Stat.MtimeNs, Ino: moved.Stat.Ino, SHA256: moved.SHA}
	return res, nil
}

// ── 复制复用 ────────────────────────────────────────────

// pfWithin 报告 p 是不是在目录 dir 之内（按路径字面，不看符号链接）。
func pfWithin(dir, p string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(p))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// StageCopy 把本机一份文件（项目里别处、同步回收、暂存区）复制进暂存区成 <sha256>.part，供接着安装：
// darwin 用 clonefile（同一卷上几乎瞬间且不占空间），失败或 linux 用普通复制；复制完核对哈希与 src.SHA256，
// 来源在复制时被改（哈希不符）就删掉复制品并放弃。暂存区里已有同名 part 时：内容对就直接用，不对（可能正在传）就放弃，不删。
// 来源在 iCloud 占位（dataless）→ 推迟（读会触发下载）。来源必须在单元目录、回收区或暂存区之内。
//
// 钩子窗口：copy:after-copy（复制完、核对之前）。
func (a *PFApplier) StageCopy(ctx context.Context, src PFCopySrc) (PFPart, PFApplyResult, error) {
	rel := filepath.Base(src.Path)
	res := PFApplyResult{Rel: rel}
	if !pfValidSHA(src.SHA256) || src.Size < 0 {
		return PFPart{}, res, &PFApplyConfigError{Msg: "复制来源的 sha256 或大小不合法"}
	}
	if !filepath.IsAbs(src.Path) || !(pfWithin(a.root, src.Path) || pfWithin(a.stg, src.Path) || pfWithin(a.cfg.Recycle.Dir(), src.Path)) {
		return PFPart{}, res, &PFApplyConfigError{Msg: fmt.Sprintf("复制来源 %q 不在单元目录、暂存区或同步回收之内", src.Path)}
	}
	part := PFPart{Path: filepath.Join(a.stg, src.SHA256+".part"), SHA256: src.SHA256, Size: src.Size}
	if ctx.Err() != nil {
		return PFPart{}, pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消"), nil
	}
	st, err := a.cfg.Stat(src.Path)
	if err != nil {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "读取复制来源信息失败")
		if errors.Is(err, fs.ErrNotExist) {
			r = pfResult(rel, PFApplyAbandoned, PFApplyCodeSourceChanged, "复制来源已经不在了")
		}
		r.Err = err
		return PFPart{}, r, nil
	}
	if st.Dataless() {
		return PFPart{}, pfResult(rel, PFApplyDeferred, PFApplyCodeICloud, "复制来源是 iCloud 占位文件，读取会触发下载，不复制"), nil
	}
	if st.Kind != PFKindFile || st.Size != src.Size {
		return PFPart{}, pfResult(rel, PFApplyAbandoned, PFApplyCodeSourceChanged, "复制来源不是期望的文件（类型或大小不对）"), nil
	}
	// 暂存区里已有同名 part
	if pst, err := a.cfg.Stat(part.Path); err == nil {
		if pst.Kind == PFKindFile && pst.Size == src.Size {
			if f, err := pfHashFile(ctx, part.Path, false); err == nil && f.SHA == src.SHA256 {
				res.Status, res.Code = PFApplyDone, PFApplyCodeOK
				res.Notes = append(res.Notes, "暂存区里已有同内容的文件，直接用")
				return part, res, nil
			}
		}
		return PFPart{}, pfResult(rel, PFApplyAbandoned, PFApplyCodePartChanged, "暂存区里已有同名但内容不全或不符的文件（可能正在传输），本轮不复制"), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "读取暂存区失败")
		r.Err = err
		return PFPart{}, r, nil
	}
	tmp := filepath.Join(a.stg, ".pfcopy-"+src.SHA256[:8]+"-"+pfRand4())
	cleanup := func() { _ = os.Remove(tmp) }
	cloned := false
	if pfCanClone {
		if err := pfCloneFile(src.Path, tmp); err == nil {
			cloned = true
		} else {
			cleanup() // clonefile 失败不会留文件，保险起见
		}
	}
	var sum string
	if cloned {
		if out, stop := a.hit(res, "copy:after-copy"); stop {
			return PFPart{}, out, nil
		}
		f, err := pfHashFile(ctx, tmp, true)
		if err != nil {
			cleanup()
			return PFPart{}, a.copyHashFailure(rel, ctx, err), nil
		}
		sum = f.SHA
		if f.Stat.Size != src.Size {
			sum = ""
		}
	} else {
		sum, err = a.plainCopy(ctx, src.Path, tmp, st)
		if err != nil {
			cleanup()
			return PFPart{}, a.copyHashFailure(rel, ctx, err), nil
		}
		if out, stop := a.hit(res, "copy:after-copy"); stop {
			return PFPart{}, out, nil
		}
	}
	if sum != src.SHA256 {
		cleanup()
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeSourceChanged, "复制来源的内容与期望不同（复制时被改动，或来源本来就不对），已删掉复制品")
		return PFPart{}, r, nil
	}
	if err := pfRenameNoReplace(tmp, part.Path); err != nil {
		cleanup()
		if pfIsErrno(err, syscall.EEXIST) {
			return PFPart{}, pfResult(rel, PFApplyAbandoned, PFApplyCodePartChanged, "复制期间暂存区里有人建了同名文件，本轮不复制"), nil
		}
		return PFPart{}, pfClassifyErr(rel, "把复制品放到暂存区", err), nil
	}
	_ = pfSyncDir(a.stg)
	res.Status, res.Code = PFApplyDone, PFApplyCodeOK
	if cloned {
		res.Notes = append(res.Notes, "用 clonefile 复制")
	}
	return part, res, nil
}

func (a *PFApplier) copyHashFailure(rel string, ctx context.Context, err error) PFApplyResult {
	var r PFApplyResult
	switch {
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		r = pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消")
	case errors.Is(err, errPFChangedWhileHashing):
		r = pfResult(rel, PFApplyAbandoned, PFApplyCodeSourceChanged, "复制来源在复制时被改动，本轮不复制")
	case pfIsErrno(err, syscall.ENOSPC, syscall.EDQUOT):
		r = pfResult(rel, PFApplyAbandoned, PFApplyCodeDiskFull, "复制失败：磁盘空间不足")
	default:
		r = pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "复制失败")
	}
	r.Err = err
	return r
}

// plainCopy 普通复制：边读边算 sha256，写完 fsync；读之前读之后来源的大小或 mtime 变了就报「被改动」。返回算出的哈希。
func (a *PFApplier) plainCopy(ctx context.Context, src, dst string, before PFStat) (string, error) {
	in, err := pfOpenNoFollow(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(out, h), ctxReader{ctx, in}, make([]byte, 1<<20))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	fi, err := in.Stat()
	if err != nil {
		return "", err
	}
	after, err := pfStatFromInfo(fi)
	if err != nil {
		return "", err
	}
	if n != before.Size || after.Size != before.Size || after.MtimeNs != before.MtimeNs {
		return "", errPFChangedWhileHashing
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CopyInstallNew 本机复制复用后新建：StageCopy 再 InstallNew。
func (a *PFApplier) CopyInstallNew(ctx context.Context, src PFCopySrc, rel string, mtimeNs int64) (PFApplyResult, error) {
	part, r, err := a.StageCopy(ctx, src)
	if err != nil || r.Status != PFApplyDone {
		r.Rel = rel
		return r, err
	}
	return a.InstallNew(ctx, rel, part, mtimeNs)
}

// CopyInstallOver 本机复制复用后覆盖：StageCopy 再 InstallOver。
func (a *PFApplier) CopyInstallOver(ctx context.Context, src PFCopySrc, rel string, mtimeNs int64, want PFExpect) (PFApplyResult, error) {
	part, r, err := a.StageCopy(ctx, src)
	if err != nil || r.Status != PFApplyDone {
		r.Rel = rel
		return r, err
	}
	return a.InstallOver(ctx, rel, part, mtimeNs, want)
}

// ── 建目录、删目录 ──────────────────────────────────────

// Mkdir 建目录 rel（0755），上级按需一起建。已经有字节完全相同的目录就算完成（Existed=true）；
// 同名的是文件、符号链接 → 超出范围；只有折叠键相同的名字 → 超出范围。钩子窗口：mkdir:before。
func (a *PFApplier) Mkdir(ctx context.Context, rel string) (PFApplyResult, error) {
	res := PFApplyResult{Rel: rel}
	if r := a.checkRel(rel, true); r != nil {
		return *r, nil
	}
	if ctx.Err() != nil {
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消"), nil
	}
	w, r := a.walk(rel, true)
	if r != nil {
		return *r, nil
	}
	if len(w.missing) == 0 {
		switch {
		case w.leaf != nil && w.leaf.Type()&fs.ModeSymlink != 0:
			return pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotDir, "这个位置是符号链接，不当目录用"), nil
		case w.leaf != nil && w.leaf.IsDir():
			res.Status, res.Code, res.Existed = PFApplyDone, PFApplyCodeOK, true
			return res, nil
		case w.leaf != nil:
			return pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotDir, "这个位置在磁盘上是文件、不是目录（一边文件一边目录）"), nil
		case len(w.fold) > 0:
			return pfResult(rel, PFApplyOutOfScope, PFApplyCodeNameClash, "目录里已有 %q，与要建的 %q 只差大小写或 NFC/NFD，不新建", w.fold[0], w.base), nil
		}
	}
	if r := a.dirWritable(rel, w.dir); r != nil {
		return *r, nil
	}
	if out, stop := a.hit(res, "mkdir:before"); stop {
		return out, nil
	}
	// 叶子也是一级要新建的目录
	w2 := *w
	w2.missing = append(append([]string{}, w.missing...), w.base)
	created, err := mkParents(&w2)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// 别人刚建了：如果是字节相同的目录就算完成
			if fi, lerr := os.Lstat(a.abs(rel)); lerr == nil && fi.IsDir() {
				res.Status, res.Code, res.Existed = PFApplyDone, PFApplyCodeOK, true
				return res, nil
			}
			undoParents(created)
			r := pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetChanged, "建目录时发现这个位置刚被别人占了")
			r.Err = err
			return r, nil
		}
		undoParents(created)
		return pfClassifyErr(rel, "建目录", err), nil
	}
	res.Status, res.Code = PFApplyDone, PFApplyCodeOK
	return res, nil
}

// Rmdir 删除空目录 rel：字节名核对后直接 rmdir，是否为空由系统在执行时确认（不空 → 放弃，目录保留，not_empty）。
// 目录带 uchg、所在目录不可写 → 超出范围。注意：目录里只剩 .DS_Store 这类不同步的隐藏文件时也是「不空」。
// 钩子窗口：rmdir:before-rmdir。
func (a *PFApplier) Rmdir(ctx context.Context, rel string) (PFApplyResult, error) {
	res := PFApplyResult{Rel: rel}
	if r := a.checkRel(rel, true); r != nil {
		return *r, nil
	}
	if ctx.Err() != nil {
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeCanceled, "已取消"), nil
	}
	w, r := a.walk(rel, false)
	if r != nil {
		return *r, nil
	}
	if w.leaf == nil {
		if len(w.fold) > 0 {
			return pfResult(rel, PFApplyOutOfScope, PFApplyCodeNameMismatch, "目录里没有字节完全相同的名字 %q，只有 %q（只差大小写或 NFC/NFD）；不按名字打开，避免删到另一个目录", w.base, w.fold[0]), nil
		}
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "要删的目录已经不在了"), nil
	}
	if w.leaf.Type()&fs.ModeSymlink != 0 || !w.leaf.IsDir() {
		return pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotDir, "这个位置在磁盘上不是目录（符号链接或文件），不删"), nil
	}
	abs := a.abs(rel)
	st, err := a.cfg.Stat(abs)
	if err != nil {
		r := pfResult(rel, PFApplyAbandoned, PFApplyCodeIO, "读取目录信息失败")
		r.Err = err
		return r, nil
	}
	if st.Flags&pfFlagsImmutable != 0 {
		return pfResult(rel, PFApplyOutOfScope, PFApplyCodeImmutable, "目录带 uchg 等不可改标志，不删"), nil
	}
	if r := a.dirWritable(rel, w.dir); r != nil {
		return *r, nil
	}
	if out, stop := a.hit(res, "rmdir:before-rmdir"); stop {
		return out, nil
	}
	if err := syscall.Rmdir(abs); err != nil {
		if pfIsErrno(err, syscall.ENOTEMPTY, syscall.EEXIST) {
			r := pfResult(rel, PFApplyAbandoned, PFApplyCodeNotEmpty, "目录不是空的，保留")
			r.Err = err
			return r, nil
		}
		if pfIsErrno(err, syscall.ENOENT) {
			return pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "要删的目录刚被别人删了"), nil
		}
		return pfClassifyErr(rel, "删除目录", err), nil
	}
	if e := pfSyncDir(w.dir); e != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("目录 %s fsync 失败：%v", w.dir, e))
	}
	res.Status, res.Code = PFApplyDone, PFApplyCodeOK
	return res, nil
}

// ── 暂存区崩溃残留 ──────────────────────────────────────

// PFLeftover 是暂存区里崩溃留下的私有文件。
type PFLeftover struct {
	Path    string
	Kind    string // "swap"：覆盖时的交换位置，可能是被换下来的用户文件，内容必须保留；"copy"：复制到一半的复制品，可删
	Size    int64
	MtimeNs int64
}

// PFStagingLeftovers 列出暂存区 dir 里的 .pfswap-*、.pfcopy-* 文件。它们不会被 PFCleanStaging 删（名字不符），
// 上层启动时应报告 swap 类（内容是用户的，不能自动删）；copy 类可以删。
func PFStagingLeftovers(dir string) ([]PFLeftover, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []PFLeftover
	for _, e := range ents {
		n := e.Name()
		kind := ""
		switch {
		case strings.HasPrefix(n, ".pfswap-"):
			kind = "swap"
		case strings.HasPrefix(n, ".pfcopy-"):
			kind = "copy"
		default:
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, PFLeftover{Path: filepath.Join(dir, n), Kind: kind, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
