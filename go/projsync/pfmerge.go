package projsync

// pfmerge.go：没配置的同编号目录「合并」进配置的目录（技术设计 §14.3，只在 Mac 上真做）。
//
// 场景：Mac 工作目录里有多个同项目编号的目录，只有配置路径那个在同步（已登记的单元）；
// 其它没配置的目录留在原位、不同步，用户可以把它们合并进配置目录。
//
// 两步：PFMergePlan 只读出计划 + 指纹；PFMergeRun 带指纹执行——先重做计划，指纹变了就不动、
// 把新计划原样退回让人重新确认。逐文件按 rel 排序一条：
//   - move：目标没有同相对路径 → 不覆盖地挪过去（pfRenameNoReplace，父目录不存在就建）；
//   - same：目标有、大小相同且 MD5 相同 → 不挪，把来源那份放进同步回收区（原因「合并时内容相同」）；
//   - rename：目标有但内容不同（或类型不同、目标不是普通文件）→ 改名「原名（合并-<来源目录名>）.扩展名」
//     挪过去，仍撞名就加「-2」「-3」…；改名后单段超过 255 字节放不下 → 不动、列给用户问（ask）；
//   - ask：拿不准的一律不动、列出来——符号链接/特殊文件、读不了、Office 锁（自身是锁或同目录有它的锁）、
//     mtime 距今不到 quiet_seconds（正在写）。每条都带 reason。
//
// 挪完后来源里**实际空了**的目录自底向上逐级放进同步回收区（回收保留天数到期由 Clean 自动删）。
// 执行期任何一条出错（如目标已存在的竞态）：那一条记进失败、其余照常，绝不覆盖、绝不删除来源。
//
// 边界：
//   - 来源不能是已登记的同步单元（units.json 里有同 rel）——是就拒绝「这个目录正在同步，不能当合并来源」。
//   - 目标若是已登记单元（工作目录下是指向同步目录的链接）：按计划解析到同步目录里的真实位置算，
//     执行期间拿住这个单元的锁（PFSched.LockUnit），跟定时轮次、取最新互斥。
//   - 整块（回收）必须和来源/目标同卷：进回收是改名、跨盘做不到，PFRecycle.Check 挡下（配置错误整个停）。
//   - 计划只读、不建目录不挪文件；只有 run 才动手，且只在给定的两个目录内。

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// PFMergeAction 是一个文件在合并里的处置。
type PFMergeAction string

const (
	PFMergeMove   PFMergeAction = "move"   // 目标没有同相对路径 → 不覆盖地挪过去
	PFMergeSame   PFMergeAction = "same"   // 目标有、内容相同 → 来源那份进回收区
	PFMergeRename PFMergeAction = "rename" // 目标有、内容不同 → 改名挪过去
	PFMergeAsk    PFMergeAction = "ask"    // 拿不准 → 留在原处，列给用户
)

// PFMergeReasonSameRecycle 是 same 进回收区写进 index 的原因。
const PFMergeReasonSameRecycle = "合并时内容相同"

// PFMergeConfig 是一次合并的固定配置（全部由调用方传入，零业务默认值）。
type PFMergeConfig struct {
	WorkDir string // 工作目录（from_rel/to_rel 的根，桌面 Projects）
	SyncDir string // 同步目录（目标是同步中的单元时，链接解析进这里）
	MetaDir string // meta 目录（units.json 在这里；回收区缺省 PFRoundMacRecycleDir(MetaDir)）

	RecycleDir   string        // 回收区根；空 = PFRoundMacRecycleDir(MetaDir)
	QuietSeconds int           // 静止判定秒数（≤0 取缺省 PFDefaultQuietSeconds）
	LockWait     time.Duration // run 拿目标单元锁的等待：>0 到点放弃；0 = 一直等到拿到或 ctx 取消（与调度器一致）

	Sched *PFSched // 目标已登记单元时的单元锁；nil 时合并进已同步单元会失败（拿不到锁）

	// 以下只给测试注入。
	Now  func() time.Time
	Stat PFStatFunc
	// moveHook 只给测试：move/rename 真正调 pfRenameNoReplace 之前触发一下，用来注入「目标凭空出现」等竞态。
	moveHook func(it PFMergeItem)
}

func (c PFMergeConfig) filled() PFMergeConfig {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Stat == nil {
		c.Stat = PFLstat
	}
	if c.RecycleDir == "" {
		c.RecycleDir = PFRoundMacRecycleDir(c.MetaDir)
	}
	if c.QuietSeconds <= 0 {
		c.QuietSeconds = PFDefaultQuietSeconds
	}
	return c
}

func (c PFMergeConfig) check() error {
	switch {
	case c.WorkDir == "" || !filepath.IsAbs(c.WorkDir):
		return &PFApplyConfigError{Msg: fmt.Sprintf("合并缺少工作目录（work_dir 要是绝对路径，给了 %q）", c.WorkDir)}
	case c.MetaDir == "" || !filepath.IsAbs(c.MetaDir):
		return &PFApplyConfigError{Msg: fmt.Sprintf("合并缺少 meta 目录（meta_dir 要是绝对路径，给了 %q）", c.MetaDir)}
	case c.RecycleDir == "" || !filepath.IsAbs(c.RecycleDir):
		return &PFApplyConfigError{Msg: fmt.Sprintf("合并回收区路径 %q 不是绝对路径", c.RecycleDir)}
	}
	return nil
}

// PFMergeItem 是计划/执行里的一个文件条目。
type PFMergeItem struct {
	Rel     string        `json:"rel"`     // 相对来源目录的路径（/ 分隔），按 rel 排序
	Action  PFMergeAction `json:"action"`  // move / same / rename / ask
	Reason  string        `json:"reason"`  // 给人看的原因（每条都带）
	DestRel string        `json:"dest,omitempty"`
	// DestRel：落到目标里的相对路径（相对目标根，/ 分隔）。move/same 同 Rel；rename 是改过名的。
	ask bool // 计划内部：ask 时不进回收、不进 rename 撞名集合
}

// PFMergePlanResult 是一次只读计划的结果。
type PFMergePlanResult struct {
	FromRel     string        `json:"from_rel"`
	ToRel       string        `json:"to_rel"`        // 解析后的目标 rel（同步单元时是同步目录里的位置）
	TargetUnit  string        `json:"target_unit"`     // 目标是已登记单元时的编号（否则空）
	TargetSync  bool          `json:"target_sync"`       // 目标是不是经链接同步中的单元
	Items       []PFMergeItem `json:"items"`             // 逐文件，按 Rel 排序
	EmptyDirs   []string      `json:"empty_dirs"`        // 合并后会变空、要进回收区的来源目录（相对来源根，自底向上）
	Fingerprint string        `json:"fingerprint"`       // 来源与目标涉及文件的 size+mtime+ino 哈希
	HasAsk      bool          `json:"has_ask"`           // 有 ask 条目（要问用户）
	Notes       []string      `json:"notes,omitempty"`    // 提示（如目标经链接解析）
	Problems    []string      `json:"problems,omitempty"` // 没集成因（如来源里有正在写的文件计数）
}

// PFMergeRunResult 是一次执行的结果。
type PFMergeRunResult struct {
	Applied      []PFMergeItem `json:"applied"`       // 完成的 move/rename
	Recycled     []PFMergeItem `json:"recycled"`      // same 进回收区的
	Skipped      []PFMergeItem `json:"skipped"`       // ask 未动的
	Failed       []PFMergeItem `json:"failed"`        // 出错没做成的（reason 带错误）
	Remaining    []PFMergeItem `json:"remaining"`     // 出错之后没轮到的（若从头顺跑出错则停在出错条）
	RecycleDirs  []string      `json:"recycle_dirs"`  // 实际进了回收区的空目录（相对回收区根）
	RemainingSrc []string      `json:"remaining_src"`  // 来源里没挪走、留在原处的目录/文件（ask + 失败）
	Residue      []string      `json:"residue,omitempty"`
	// Residue：放进回收后记账/fsync 失败等残留说明（东西已在回收区，只是账没记全）。
	Touched     bool                `json:"touched"`     // 有没有动过任何东西
	Rejected    bool                `json:"rejected"`    // 被拒绝执行（指纹不符 / 来源是同步单元 / 拿不到锁 / 配置问题）
	Reason      string              `json:"reason,omitempty"`
	Plan        PFMergePlanResult   `json:"plan,omitempty"`
	// Plan：指纹不符时退回的新计划（其余情况是执行所依据的那份计划）。
	Fingerprint string `json:"fingerprint,omitempty"` // 执行所依据（或退回的新）计划指纹
}

// pfMergeCtx 是一次计划/执行的内部上下文（解析好的两棵目录树）。
type pfMergeCtx struct {
	cfg       PFMergeConfig
	fromRoot  string   // 来源真实目录（绝对）
	toRoot    string   // 目标真实目录（绝对，同步单元时已解析进同步目录）
	fromRel   string   // 清洗后的来源 rel（/ 分隔）
	toRel     string   // 清洗后的目标 rel
	srcName   string   // 来源目录名（from_rel 最后一段），改名里用
	target    []PFUnitRecord
	unitCode  string // 目标已登记单元的编号
	targetSyn bool // 目标是经链接同步中的单元
	notes     []string
}

// PFMergePlan 只读出计划：不建目录、不挪文件、不进回收区。
func PFMergePlan(cfg PFMergeConfig, fromRel, toRel string) (PFMergePlanResult, error) {
	m, err := pfMergePrepare(cfg, fromRel, toRel)
	if err != nil {
		return PFMergePlanResult{}, err
	}
	return m.plan()
}

// PFMergeRun 执行合并：先重做计划、比指纹，指纹不同就不动、退回新计划；相同就逐条执行。
func PFMergeRun(ctx context.Context, cfg PFMergeConfig, fromRel, toRel, fingerprint string) (PFMergeRunResult, error) {
	m, err := pfMergePrepare(cfg, fromRel, toRel)
	if err != nil {
		return PFMergeRunResult{}, err
	}
	plan, err := m.plan()
	if err != nil {
		return PFMergeRunResult{}, err
	}
	res := PFMergeRunResult{Plan: plan, Fingerprint: plan.Fingerprint}
	if plan.Fingerprint != fingerprint {
		res.Rejected = true
		res.Reason = "计划之后文件有变动，请重新确认"
		return res, nil
	}
	// 目标是已登记单元：整个执行期间拿住单元锁，跟定时轮次、取最新互斥。
	if plan.TargetUnit != "" {
		if cfg.Sched == nil {
			res.Rejected = true
			res.Reason = "目标是正在同步的单元，但没有调度器拿不了单元锁，不能合并"
			return res, nil
		}
		unlock, ok := cfg.Sched.LockUnit(ctx, plan.TargetUnit, cfg.LockWait)
		if !ok {
			res.Rejected = true
			if ctx.Err() != nil {
				res.Reason = "请求已取消"
			} else {
				res.Reason = fmt.Sprintf("单元 %s 正在同步/取最新，拿不到锁，稍后再试", plan.TargetUnit)
			}
			return res, nil
		}
		defer unlock()
	}
	// 回收区：same 与空目录都放进去；必须先确认与来源/目标同卷（进回收是改名）。
	rc, err := NewPFRecycle(cfg.RecycleDir, PFRecycleOptions{Stat: cfg.Stat, Now: cfg.Now})
	if err != nil {
		return res, err
	}
	if err := rc.Check(m.fromRoot); err != nil {
		res.Rejected = true
		res.Reason = err.Error()
		return res, nil
	}
	m.run(ctx, plan, rc, &res)
	return res, nil
}

// ── 准备：解析两个目录、认登记、校验来源 ────────────────

// pfMergePrepare 校验参数、解析来源与目标、读登记表判来源是否同步单元/目标是否同步中。
func pfMergePrepare(cfg PFMergeConfig, fromRel, toRel string) (*pfMergeCtx, error) {
	cfg = cfg.filled()
	if err := cfg.check(); err != nil {
		return nil, err
	}
	fr, err := pfMergeRel(fromRel, "from_rel")
	if err != nil {
		return nil, err
	}
	tr, err := pfMergeRel(toRel, "to_rel")
	if err != nil {
		return nil, err
	}
	if fr == tr {
		return nil, errors.New("projsync: 合并的来源与目标是同一个目录")
	}
	units, err := PFUnitMoveLoadUnits(cfg.MetaDir)
	if err != nil {
		return nil, fmt.Errorf("projsync: 读登记表失败、判不出来源是不是正在同步的目录，先不动：%w", err)
	}
	m := &pfMergeCtx{cfg: cfg, fromRel: fr, toRel: tr, target: units, srcName: path.Base(fr)}

	// 来源：必须不是已登记的同步单元（units.json 里有同 rel）。
	for _, u := range units {
		if u.Rel == fr {
			return nil, fmt.Errorf("projsync: 来源 %s 是已登记的同步单元（正在同步），不能当合并来源", fr)
		}
	}
	fromRoot := filepath.Join(cfg.WorkDir, filepath.FromSlash(fr))
	st, err := cfg.Stat(fromRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("projsync: 合并来源 %s 不存在", fr)
		}
		return nil, fmt.Errorf("projsync: 读合并来源 %s 失败: %w", fr, err)
	}
	if st.Kind != PFKindDir {
		return nil, fmt.Errorf("projsync: 合并来源 %s 不是目录", fr)
	}
	m.fromRoot = fromRoot

	// 目标：工作目录下的 to，若是指向同步目录的链接（已同步单元）→ 按链接解析到真实位置算。
	toLink := filepath.Join(cfg.WorkDir, filepath.FromSlash(tr))
	tst, err := cfg.Stat(toLink)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("projsync: 合并目标 %s 不存在", tr)
		}
		return nil, fmt.Errorf("projsync: 读合并目标 %s 失败: %w", tr, err)
	}
	toReal := toLink
	if tst.Kind == PFKindSymlink {
		rp, err := filepath.EvalSymlinks(toLink)
		if err != nil {
			return nil, fmt.Errorf("projsync: 合并目标 %s 是指向别处的链接，解析失败（要能读到同步目录里的真实位置才能合并）: %w", tr, err)
		}
		toReal = rp
		m.targetSyn = true
		m.notes = append(m.notes, fmt.Sprintf("目标 %s 是指向同步目录的链接，按真实位置 %s 合并", tr, toReal))
		rst, err := cfg.Stat(toReal)
		if err != nil {
			return nil, fmt.Errorf("projsync: 读合并目标真实位置 %s 失败: %w", toReal, err)
		}
		if rst.Kind != PFKindDir {
			return nil, fmt.Errorf("projsync: 合并目标 %s 指向的位置不是目录", tr)
		}
	}
	m.toRoot = toReal

	// 目标已登记单元：认出编号（执行期间拿它的锁）；登记的 SyncPath 若与解析位置不同，以磁盘解析为准并提示。
	for _, u := range units {
		if u.Rel == tr {
			m.unitCode = u.Code
			if u.State != PFUnitSyncing && u.State != PFUnitStarting && u.State != PFUnitStopping {
				// stopped：目录已挪回桌面，不再是同步单元，不锁、当普通目录合并。
				m.unitCode = ""
			} else if u.SyncPath != "" && filepath.Clean(u.SyncPath) != filepath.Clean(toReal) && m.targetSyn {
				m.notes = append(m.notes, fmt.Sprintf("登记里的同步位置 %s 与磁盘解析 %s 不同，按磁盘上实际指向的位置合并", u.SyncPath, toReal))
			}
			break
		}
	}
	return m, nil
}

// pfMergeRel 校验并清洗一段相对路径：非空、不含控制字符、不是绝对路径、没有 .. / . 段、无空段，规范化成 / 分隔。
func pfMergeRel(s, field string) (string, error) {
	if s == "" || strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
		return "", fmt.Errorf("projsync: 合并 %s 不能为空且不能含控制字符", field)
	}
	if strings.HasPrefix(s, "/") || filepath.IsAbs(s) {
		return "", fmt.Errorf("projsync: 合并 %s 必须是工作目录下的相对路径（不能用绝对路径 %q）", field, s)
	}
	segs := strings.Split(s, "/")
	var out []string
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("projsync: 合并 %s %q 含空段、. 或 ..", field, s)
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("projsync: 合并 %s 不能为空", field)
	}
	return strings.Join(out, "/"), nil
}

// ── 计划 ────────────────────────────────────────────────

// plan 走来源目录树、逐文件定 action，列出会变空的目录、算指纹。只读。
func (m *pfMergeCtx) plan() (PFMergePlanResult, error) {
	res := PFMergePlanResult{FromRel: m.fromRel, ToRel: m.toRel, TargetUnit: m.unitCode, TargetSync: m.targetSyn, Notes: append([]string(nil), m.notes...)}

	// 目标根必须真实存在且是目录（plan 里已保证 toRoot 是目录）。
	// 走来源树：逐目录收集 raw 名字（认 Office 锁），逐文件定 action。
	var fps []string // 指纹片段（按 rel 排序累积）
	// 目标某目录里「已存在的名字 + 本轮已排进去的名字」，认 rename 撞名（折叠键，照顾大小写不敏感的卷）。
	toNamesOf := func(dirRel string) *PFNameSet {
		abs := filepath.Join(m.toRoot, filepath.FromSlash(dirRel))
		ents, err := os.ReadDir(abs)
		var s *PFNameSet
		if err != nil {
			return NewPFNameSet(nil) // 目标目录还没有：没有已存在名字（父目录 run 时再建）
		}
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		s = NewPFNameSet(names)
		return s
	}
	// 计划阶段维护每个目标目录「本轮排进去的名字」，避免两条排到同一个新名（rel 唯一时不会，防御性处理）。
	planned := map[string]*PFNameSet{}

	var walk func(dirRel string) (emptiable bool, err error)
	walk = func(dirRel string) (bool, error) {
		fromDir := filepath.Join(m.fromRoot, filepath.FromSlash(dirRel))
		ents, err := os.ReadDir(fromDir)
		if err != nil {
			return false, fmt.Errorf("projsync: 读来源目录 %s 失败: %w", dirRel, err)
		}
		// 认 Office 锁要用「排除前」的全量名字（~$、.~lock 都在其中）。
		rawNames := make([]string, 0, len(ents))
		for _, e := range ents {
			rawNames = append(rawNames, e.Name())
		}
		var fileItems []PFMergeItem
		emptiable := true // 先假设合并后能空；有 ask / 挪不走的子项就转 false；空目录本就地保持 true
		for _, e := range ents {
			name := e.Name()
			childRel := name
			if dirRel != "" {
				childRel = dirRel + "/" + name
			}
			if e.IsDir() {
				sub, err := walk(childRel)
				if err != nil {
					return false, err
				}
				if !sub {
					emptiable = false // 子目录里有留下来的东西 → 本目录也不会空
				}
				continue
			}
			it, err := m.classifyFile(dirRel, childRel, name, rawNames, toNamesOf, planned)
			if err != nil {
				return false, err
			}
			if it.ask {
				emptiable = false // ask 的文件留在原处 → 目录不空
			}
			fileItems = append(fileItems, it)
		}
		res.Items = append(res.Items, fileItems...)
		// 这个子目录合并后会空（本来就空、或里面的东西都会挪走）→ 记下来（自底向上，深的已在递归里记过）
		if emptiable && dirRel != "" {
			res.EmptyDirs = append(res.EmptyDirs, dirRel)
		}
		return emptiable, nil
	}

	emptiableRoot, err := walk("")
	if err != nil {
		return res, err
	}

	sort.Slice(res.Items, func(i, j int) bool { return res.Items[i].Rel < res.Items[j].Rel })
	for _, it := range res.Items {
		if it.Action == PFMergeAsk {
			res.HasAsk = true
		}
	}
	// 整个来源目录都清空了 → 根本身也进回收，写 "."（执行时放回收区里认成的子树根，run 里据实际空再放）。
	if emptiableRoot {
		res.EmptyDirs = append(res.EmptyDirs, ".")
	}

	// 指纹：来源全部涉及文件 + 目标里同 rel 已存在的文件，各取 size+mtime+ino，按 rel 排序后哈希。
	for _, it := range res.Items {
		fps = append(fps, pfMergeFingerprintSide("F", it.Rel, filepath.Join(m.fromRoot, filepath.FromSlash(it.Rel)), m.cfg.Stat))
		if it.Action != PFMergeMove { // move 时目标本来没有；其余目标已有那份也要进指纹
			fps = append(fps, pfMergeFingerprintSide("T", it.Rel, filepath.Join(m.toRoot, filepath.FromSlash(it.Rel)), m.cfg.Stat))
		}
	}
	fps = append(fps, pfMergeFingerprintSide("D", ".", m.fromRoot, m.cfg.Stat))
	fps = append(fps, pfMergeFingerprintSide("D", ".", m.toRoot, m.cfg.Stat))
	res.Fingerprint = pfMergeFingerprintHash(fps)
	return res, nil
}

// classifyFile 定一个来源文件的 action 与原因。dirRel/rel 相对来源根；base 是文件名；rawNames 是同目录全量名字；
// toNamesOf 给目标同目录已存在的名字集合；planned 累积本轮排进目标的名字。
func (m *pfMergeCtx) classifyFile(dirRel, rel, base string, rawNames []string, toNamesOf func(string) *PFNameSet, planned map[string]*PFNameSet) (PFMergeItem, error) {
	fromAbs := filepath.Join(m.fromRoot, filepath.FromSlash(rel))
	it := PFMergeItem{Rel: rel, DestRel: rel}

	st, err := m.cfg.Stat(fromAbs)
	if err != nil {
		it.Action, it.ask = PFMergeAsk, true
		it.Reason = fmt.Sprintf("读不到来源文件（%v），不动", err)
		return it, nil
	}
	// 符号链接 / 特殊文件：不动，列出来。
	if st.Kind == PFKindSymlink {
		it.Action, it.ask = PFMergeAsk, true
		it.Reason = "是符号链接，不动（只搬真实文件，链接指向的东西不在合并范围）"
		return it, nil
	}
	if st.Kind != PFKindFile {
		it.Action, it.ask = PFMergeAsk, true
		it.Reason = "是特殊文件（FIFO/socket/设备），不动"
		return it, nil
	}
	// Office 锁：自身是锁文件，或同目录里有保护它的锁 → 不动。
	if pfumIsOfficeLock(base) {
		it.Action, it.ask = PFMergeAsk, true
		it.Reason = "是 Office 锁文件，留在原处（文档关掉后自然消失）"
		return it, nil
	}
	if locks := PFOfficeLocks(rawNames, base); len(locks) > 0 {
		it.Action, it.ask = PFMergeAsk, true
		it.Reason = fmt.Sprintf("同目录有它的 Office 锁 %q，可能正在编辑，不动", locks[0])
		return it, nil
	}
	// 未静止（mtime 距今不到 quiet_seconds）：正在写，不动。
	if !PFIsQuiet(st.MtimeNs, m.cfg.Now(), m.cfg.QuietSeconds) {
		it.Action, it.ask = PFMergeAsk, true
		it.Reason = "修改时间距今不到 quiet_seconds，可能正在写，不动"
		return it, nil
	}

	// 看目标同相对路径。
	toAbs := filepath.Join(m.toRoot, filepath.FromSlash(rel))
	tst, err := m.cfg.Stat(toAbs)
	if errors.Is(err, fs.ErrNotExist) {
		it.Action = PFMergeMove
		it.Reason = "目标没有同相对路径，不覆盖地挪过去"
		return it, nil
	}
	if err != nil {
		it.Action, it.ask = PFMergeAsk, true
		it.Reason = fmt.Sprintf("读不到目标同路径（%v），判不出要不要挪，不动", err)
		return it, nil
	}

	// 目标有、是普通文件、大小一样 → 比 MD5；一致 = same，不一致 = rename。
	if tst.Kind == PFKindFile && tst.Size == st.Size {
		sm, _, serr := pfMergeMD5(fromAbs)
		tm, _, terr := pfMergeMD5(toAbs)
		if serr != nil || terr != nil {
			it.Action, it.ask = PFMergeAsk, true
			if serr != nil {
				it.Reason = fmt.Sprintf("读来源内容失败（%v），不动", serr)
			} else {
				it.Reason = fmt.Sprintf("读目标内容失败（%v），不动", terr)
			}
			return it, nil
		}
		if sm == tm {
			it.Action = PFMergeSame
			it.Reason = "目标已有同相对路径、大小与 MD5 都一样，来源这份进回收区"
			return it, nil
		}
	}

	// 到这里：目标有但内容不同 / 大小不同 / 类型不同 → 改名挪过去；改名放不下（超 255 或试满仍撞）→ ask。
	dirSet := toNamesOf(dirRel)
	ps := planned[dirRel]
	if ps != nil {
		for k, ok := range ps.keys {
			if ok {
				dirSet.keys[k] = true
			}
		}
	}
	newName, ok := pfMergeRenameName(base, m.srcName, func(n string) bool { return dirSet.Has(n) })
	if !ok {
		it.Action, it.ask = PFMergeAsk, true
		it.Reason = "目标已有同相对路径且内容不同，但改名后单段超过 255 字节放不下，留在原处问你"
		return it, nil
	}
	it.Action = PFMergeRename
	it.DestRel = path.Join(dirRel, newName)
	it.Reason = "目标已有同相对路径、内容不同，改名「" + newName + "」挪过去"
	if ps == nil {
		ps = NewPFNameSet(nil)
		planned[dirRel] = ps
	}
	ps.keys[PFFoldKey(newName)] = true
	return it, nil
}

// ── 执行 ────────────────────────────────────────────────

// run 逐条执行计划；出错的那条记进 Failed、其余照常，绝不覆盖、绝不删除来源。
func (m *pfMergeCtx) run(ctx context.Context, plan PFMergePlanResult, rc *PFRecycle, res *PFMergeRunResult) {
	now := m.cfg.Now()
	for i, it := range plan.Items {
		if ctx.Err() != nil {
			res.Remaining = append(res.Remaining, plan.Items[i:]...)
			res.Reason = "已取消，后面没做"
			break
		}
		switch it.Action {
		case PFMergeAsk:
			res.Skipped = append(res.Skipped, it)
			res.RemainingSrc = append(res.RemainingSrc, it.Rel)
		case PFMergeSame:
			if err := m.recycleSame(rc, now, it, res); err != nil {
				m.failed(res, it, "进回收区", err)
			}
		case PFMergeMove, PFMergeRename:
			if err := m.moveOne(it, res); err != nil {
				m.failed(res, it, "挪动", err)
			} else {
				res.Applied = append(res.Applied, it)
			}
		}
	}
	if ctx.Err() == nil && !res.Rejected {
		m.recycleEmptyDirs(rc, now, plan, res)
	}
	if len(res.Applied) > 0 || len(res.Recycled) > 0 || len(res.RecycleDirs) > 0 {
		res.Touched = true
	}
}

// moveOne 不覆盖地挪一个文件：父目录不存在就建；目标已存在（竞态）返回错误、来源原地不动。
func (m *pfMergeCtx) moveOne(it PFMergeItem, res *PFMergeRunResult) error {
	src := filepath.Join(m.fromRoot, filepath.FromSlash(it.Rel))
	dst := filepath.Join(m.toRoot, filepath.FromSlash(it.DestRel))
	if err := pfMkdirAllSync(filepath.Dir(dst), pfDirPerm); err != nil {
		return err
	}
	// 再确认 src 还在且仍是普通文件（不跟随链接）。
	st, err := m.cfg.Stat(src)
	if err != nil {
		return err
	}
	if st.Kind != PFKindFile {
		return fmt.Errorf("来源不再是普通文件（现在是 %s）", st.Kind)
	}
	if _, err := m.cfg.Stat(dst); err == nil {
		return fmt.Errorf("目标 %s 在计划之后凭空出现，为避免覆盖不挪", it.DestRel)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if m.cfg.moveHook != nil { // 只给测试：在真正改名前注入竞态（如凭空造出目标），验证不覆盖
		m.cfg.moveHook(it)
	}
	return pfRenameNoReplace(src, dst)
}

// recycleSame 把内容相同的那份来源文件放进回收区并记账（含 fsync 残留写进 Residue）。
func (m *pfMergeCtx) recycleSame(rc *PFRecycle, now time.Time, it PFMergeItem, res *PFMergeRunResult) error {
	src := filepath.Join(m.fromRoot, filepath.FromSlash(it.Rel))
	st, err := m.cfg.Stat(src)
	if err != nil {
		return err
	}
	if st.Kind != PFKindFile {
		return fmt.Errorf("来源不再是普通文件（现在是 %s）", st.Kind)
	}
	placed, err := rc.Place(src, now, m.fromRel, it.Rel)
	if err != nil {
		if pfIsErrno(err, syscall.EXDEV) {
			return &PFApplyConfigError{Msg: "回收区与来源不在同一个文件系统，无法改名放入：" + err.Error()}
		}
		return err
	}
	if placed.Warn != nil {
		res.Residue = append(res.Residue, fmt.Sprintf("%s 已放进回收区，但目录 fsync 失败：%v", it.Rel, placed.Warn))
	}
	sha, _, herr := pfMergeSHA256(placed.Stored)
	if herr != nil {
		res.Residue = append(res.Residue, fmt.Sprintf("%s 已在回收区 %s，但算内容哈希失败：%v（账没记全）", it.Rel, placed.Rel, herr))
	}
	entry := PFRecycleEntry{
		Unit: m.fromRel, Path: it.Rel, SHA256: sha, Size: st.Size, MtimeNs: st.MtimeNs,
		Reason: PFMergeReasonSameRecycle, Round: "merge", Time: now.Format(time.RFC3339), Stored: placed.Rel,
	}
	if err := rc.Record(placed.Day, entry); err != nil {
		res.Residue = append(res.Residue, fmt.Sprintf("%s 已在回收区 %s，但记账（index.jsonl）失败：%v", it.Rel, placed.Rel, err))
	}
	res.Recycled = append(res.Recycled, it)
	return nil
}

// recycleEmptyDirs 自底向上把来源里**实际空了**的目录放进回收区（改名；只动确实空的，绝不删非空的）。
func (m *pfMergeCtx) recycleEmptyDirs(rc *PFRecycle, now time.Time, plan PFMergePlanResult, res *PFMergeRunResult) {
	// 深→浅排序（深的先挪，父目录随之空）。"." 是根，最后处理。
	dirs := sortByDepthDesc(plan.EmptyDirs)
	for _, d := range dirs {
		dirAbs := m.fromRoot
		if d != "." {
			dirAbs = filepath.Join(m.fromRoot, filepath.FromSlash(d))
		}
		st, err := m.cfg.Stat(dirAbs)
		if err != nil || st.Kind != PFKindDir {
			continue // 没了或不是目录：不用回收
		}
		ents, err := os.ReadDir(dirAbs)
		if err != nil || len(ents) > 0 {
			continue // 还没空（有 ask 文件、失败没挪走的、或读不了）：留着
		}
		// 回收落点：子目录按自身 rel；来源根本身（"."）用它的目录名，避免 "." 撞进刚建的空回收目录。
		recycleRel := d
		if d == "." {
			recycleRel = m.srcName
		}
		placed, err := rc.Place(dirAbs, now, m.fromRel, recycleRel)
		if err != nil {
			res.Residue = append(res.Residue, fmt.Sprintf("空目录 %s 进回收区失败：%v（留在原处）", d, err))
			continue
		}
		res.RecycleDirs = append(res.RecycleDirs, placed.Rel)
	}
}

// failed 记一条失败：来源没动（不覆盖、不删），其余继续。
func (m *pfMergeCtx) failed(res *PFMergeRunResult, it PFMergeItem, doing string, err error) {
	f := it
	f.Reason = fmt.Sprintf("%s失败（来源原地不动）：%v", doing, err)
	res.Failed = append(res.Failed, f)
}

// ── 命名与哈希小工具 ────────────────────────────────────

// pfMergeRenameName 生成「原名（合并-<来源目录名>）.扩展名」：撞名就加「-2」「-3」…；
// 任一候选单段超过 pfMaxSegBytes（255）字节放不下 → ok=false（交 ask）。
// exists 判断名字在目标目录是否已存在（用折叠键，照顾大小写不敏感的卷）。
func pfMergeRenameName(orig, srcName string, exists func(string) bool) (string, bool) {
	if exists == nil {
		exists = func(string) bool { return false }
	}
	ext := pfExt(orig)
	main := orig[:len(orig)-len(ext)]
	who := strings.ReplaceAll(srcName, "/", "_")
	if who == "" {
		who = "来源"
	}
	for i := 1; i <= pfConflictMaxTries; i++ {
		mark := "（合并-" + who + "）"
		if i > 1 {
			mark = "（合并-" + who + "）-" + strconv.Itoa(i)
		}
		name := main + mark + ext
		if len(name) > pfMaxSegBytes {
			return "", false // 单段超 255：不截断，交用户问
		}
		if !exists(name) {
			return name, true
		}
	}
	return "", false
}

// pfMergeMD5 读整个文件算 MD5（大小写十六进制小写），顺带返回字节数。
func pfMergeMD5(p string) (string, int64, error) {
	h := md5.New()
	n, err := pfMergeReadFile(p, h)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// pfMergeSHA256 读整个文件算 SHA-256（回收记账用）。
func pfMergeSHA256(p string) (string, int64, error) {
	h := sha256.New()
	n, err := pfMergeReadFile(p, h)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// pfMergeReadFile 以 O_NOFOLLOW 打开 p 并把内容灌进 w，返回读到的字节数；只读普通文件。
func pfMergeReadFile(p string, w io.Writer) (int64, error) {
	f, err := pfOpenNoFollow(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, fmt.Errorf("%s 不是普通文件", p)
	}
	return io.Copy(w, f)
}

// pfMergeFingerprintSide 算一条指纹片段：side(F/T/D) + rel + size + mtime + ino。
func pfMergeFingerprintSide(side, rel, abs string, stat PFStatFunc) string {
	st, err := stat(abs)
	if err != nil {
		return side + "|" + rel + "|gone"
	}
	return fmt.Sprintf("%s|%s|%d|%d|%d", side, rel, st.Size, st.MtimeNs, st.Ino)
}

// pfMergeFingerprintHash 把排好序的指纹片段拼起来哈希。
func pfMergeFingerprintHash(parts []string) string {
	s := append([]string(nil), parts...)
	sort.Strings(s)
	h := sha256.New()
	for _, p := range s {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sortByDepthDesc 按目录 rel 的深度从深到浅排（深的在前）；空串/根排最后。
func sortByDepthDesc(dirs []string) []string {
	out := append([]string(nil), dirs...)
	depth := func(s string) int {
		if s == "" || s == "." {
			return 0
		}
		return strings.Count(s, "/") + 1
	}
	sort.SliceStable(out, func(i, j int) bool { return depth(out[i]) > depth(out[j]) })
	return out
}
