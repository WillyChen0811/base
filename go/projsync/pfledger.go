package projsync

// 项目文件同步 T10：历史版本台账第一版（技术设计 §7、§13 第 3 条）。
//
// 台账 = 历史版本/档案台账.sqlite3 + 档案索引.json，一对当一个整体：读一对、拷一对、装一对。
//
//   - 读一对（pflStage）：对 <历史版本目录>/.lock 持 flock（与 Python VersionStore._locked 同一把锁，
//     拿不到就推迟）；台账旁边有 -journal/-wal 说明应用正在写，推迟；算两个文件的哈希、合并
//     （PFLedgerPairHash），把两个文件各自拷进暂存区（内容寻址的 <sha256>.part，与传输层同一个暂存区），放锁。
//     锁内读保证拷走的一对是同一时刻的自洽快照。
//   - 装一对（pflInstallPair）：先核对新 JSON 引用的每个快照文件在这台机器上都在且哈希对（快照先于台账——
//     快照文件的拷贝是 Phase 1 的动作，早于 Phase 3 的台账；不齐就说明有东西没落地，推迟、不装）；
//     再持同一把锁：核对目标两个文件还是判定时期望的样子（变了就放弃，两边原样），然后把旧的一对换进
//     同步回收（原因「覆盖」）、新的放上正位——顺序是先 sqlite 后 json，理由见下面「装序」。
//     任何一步失败，旧的一对要么还在正位、要么完整地在回收区，不会丢。
//   - Ubuntu 侧的读/装注册成扩展步骤 ledger_stage、ledger_install（同 junk_clear 的做法），编排仍由 Mac 发起。
//   - 一轮里的编排（doLedgerCopy/doLedgerBase）在本文件末尾，由 pfround_exec.go 的台账动作调用：
//     源边读一对 → 两个 part 搬到目标边暂存区（Mac 目标就是本机暂存区，不再传）→ 目标边装 →
//     成功后把生效的索引导出存进 <meta>/base/<键>.ledger/<对哈希>.json 并把 base 的台账哈希设成新值。
//
// 【装序：先 sqlite 后 json】（versions.py 的规矩：漏写（索引）比多写危险得多）
// sqlite 是台账本体（archive_id 和全部版本行都在里面），json 只是导出的索引。万一装完 sqlite 没装上 json，
// 应用看到的是「新台账 + 旧导出」——导出比实际少的部分应用下次 _flush 会自动补齐，不会凭空多出东西；
// 反过来（新 json + 旧 sqlite）则可能让应用在 _initialize 的 import 路径上按 json 重建出台账里从来没有过的
// 版本行。所以宁可暂时少一份导出，不能暂时多一份索引。
// 两个文件各用一次原子交换（InstallOver），正位任何时刻都是一对完整可读的台账；中间态只存在于
// 「sqlite 已换、json 未换」这一个小窗口，且上面已论证是安全方向。
//
// 【不参与暂缓】台账一对小、且判定已保证两边静止，ledger_copy 的传输不走暂缓判定（pfRoundAdmit），直接传。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"
)

const (
	pfLedgerLockName = ".lock" // 与 versions.py 的 self.history / ".lock" 同一个文件
	pflJournalSuffix = "-journal"
	pflWalSuffix     = "-wal"
)

// 台账第一版的尺寸上限：超过就不自动同步（宁可不做，不能撑爆内存）。
const (
	pflMaxJSONBytes   = 32 << 20
	pflMaxSQLiteBytes = 256 << 20
)

// 步骤结果代码（本模块特有的；通用的沿用 PFApplyCode*）。
const (
	pflCodeLocked     = "ledger_locked"            // 台账锁拿不到，推迟
	pflCodeJournal    = "ledger_journal"           // 旁边有 journal/wal，推迟
	pflCodeSnapshots  = "ledger_snapshots_missing" // JSON 引用的快照不齐，推迟
	pflCodeExpect     = "ledger_expect_changed"    // 目标不是判定时的样子，放弃
	pflCodeChanged    = "ledger_changed"           // 判定之后台账又变了，放弃
	pflCodeSrcChanged = "ledger_src_changed"       // 源在判定之后又变了，放弃
	pflCodeTooBig     = "ledger_too_big"           // 台账超过第一版上限，不动
)

var (
	// pflLockWait 是等台账锁的上限；超时还没拿到就推迟（应用可能正在写台账）。
	pflLockWait = 30 * time.Second
	// pflLockPoll 是重试间隔。
	pflLockPoll = 50 * time.Millisecond
)

// ── 台账锁 ──────────────────────────────────────────────

// pfldLockFile 对 dir/.lock 持 flock(LOCK_EX)（非阻塞轮询，带截止时间和 ctx）。
// 返回 (文件, 是否拿到锁, 真错误)：没拿到锁不是错误（ok=false、锁未持有），调用方推迟。
// 锁文件不存在会创建（与 Python 的 open("a+b") 相同）；进程崩溃时内核自动释放。
func pfldLockFile(ctx context.Context, dir string, wait time.Duration) (*os.File, bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, pfLedgerLockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, err
	}
	held := false
	defer func() {
		if !held {
			_ = f.Close()
		}
	}()
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			held = true
			return f, true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, err
		}
		if !time.Now().Add(pflLockPoll).Before(deadline) {
			return nil, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(pflLockPoll):
		}
	}
}

func pfldUnlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// pfldJournal 看目录里有没有台账的 journal/wal（应用正在写）；有就返回那个名字。
func pfldJournal(dir string) string {
	for _, n := range []string{pfLedgerSQLite + pflJournalSuffix, pfLedgerSQLite + pflWalSuffix} {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			return n
		}
	}
	return ""
}

// ── 小工具 ──────────────────────────────────────────────

func pflSHA256Bytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// pflReadCapped 读一个普通文件的全部字节；不是普通文件或超过 cap 返回错误（不读符号链接指向的外面东西）。
func pflReadCapped(p string, cap int64) ([]byte, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件", filepath.Base(p))
	}
	if st.Size() > cap {
		return nil, fmt.Errorf("%s 有 %d 字节，超过上限 %d", filepath.Base(p), st.Size(), cap)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != st.Size() {
		return nil, fmt.Errorf("%s 读到的字节数与大小不符（读到一半被改了）", filepath.Base(p))
	}
	return b, nil
}

// pfldFileSpec 是台账里一个文件在某一边的身份：哈希 + 大小 + mtime（装的时候按它准备 part 的时间戳）。
type pfldFileSpec struct {
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
}

// pfldStaged 是读一对的结果：合并哈希 + 两个文件（part 就在暂存区 <sha>.part）。
type pfldStaged struct {
	Hash   string       `json:"hash"`
	SQLite pfldFileSpec `json:"sqlite"`
	JSON   pfldFileSpec `json:"json"`
}

// pfldSpecOf 把刚哈希过的文件记成 spec。
func pfldSpecOf(f pfFacts) pfldFileSpec {
	return pfldFileSpec{SHA256: f.SHA, Size: f.Stat.Size, MtimeNs: f.Stat.MtimeNs}
}

// pfldMatchWant 核对刚哈希过的目标文件还是期望的样子；不符合返回原因（空 = 符合）。
func pfldMatchWant(f pfFacts, want *PFExpect) string {
	if want == nil || want.Absent {
		return ""
	}
	return pfMatchWant(f, want.Ino, *want)
}

// ── 读一对（持锁：journal 检查、算哈希、拷进暂存区）────────

// pflStage 在 applier 的根里读台账一对（dirRel 是历史版本目录，单元内相对路径）。
// 不核对判定期望值——拷走的内容以锁内哈希为准，编排在装之前会把合并哈希与计划的比对。
func pflStage(ctx context.Context, a *PFApplier, dirRel string) (pfldStaged, PFApplyResult) {
	res := PFApplyResult{Rel: dirRel}
	dir := a.abs(dirRel)
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return pfldStaged{}, pfResult(dirRel, PFApplyAbandoned, PFApplyCodeTargetMissing, "历史版本目录已经不在了")
	}
	lk, ok, err := pfldLockFile(ctx, dir, pflLockWait)
	if err != nil {
		return pfldStaged{}, pfClassifyErr(dirRel, "打开台账锁", err)
	}
	if !ok {
		return pfldStaged{}, pfResult(dirRel, PFApplyDeferred, pflCodeLocked, "台账正被占用（等锁超时），推迟")
	}
	defer pfldUnlock(lk)
	if n := pfldJournal(dir); n != "" {
		return pfldStaged{}, pfResult(dirRel, PFApplyDeferred, pflCodeJournal, "台账旁边有 %q（应用正在写），推迟", n)
	}
	sqRel := dirRel + "/" + pfLedgerSQLite
	jsRel := dirRel + "/" + pfLedgerJSON
	sq, err := pfHashFile(ctx, filepath.Join(dir, pfLedgerSQLite), false)
	if err != nil {
		return pfldStaged{}, pfClassifyErr(sqRel, "读台账 sqlite", err)
	}
	js, err := pfHashFile(ctx, filepath.Join(dir, pfLedgerJSON), false)
	if err != nil {
		return pfldStaged{}, pfClassifyErr(jsRel, "读档案索引", err)
	}
	_, r, err := a.StageCopy(ctx, PFCopySrc{Path: filepath.Join(dir, pfLedgerSQLite), SHA256: sq.SHA, Size: sq.Stat.Size})
	if err != nil {
		return pfldStaged{}, pfClassifyErr(sqRel, "拷台账进暂存区", err)
	}
	if r.Status != PFApplyDone {
		r.Rel = sqRel
		return pfldStaged{}, r
	}
	_, r2, err := a.StageCopy(ctx, PFCopySrc{Path: filepath.Join(dir, pfLedgerJSON), SHA256: js.SHA, Size: js.Stat.Size})
	if err != nil {
		return pfldStaged{}, pfClassifyErr(jsRel, "拷档案索引进暂存区", err)
	}
	if r2.Status != PFApplyDone {
		r2.Rel = jsRel
		return pfldStaged{}, r2
	}
	staged := pfldStaged{Hash: PFLedgerPairHash(sq.SHA, js.SHA), SQLite: pfldSpecOf(sq), JSON: pfldSpecOf(js)}
	res.Status, res.Code = PFApplyDone, PFApplyCodeOK
	res.Notes = append(res.Notes, r.Notes...)
	res.Notes = append(res.Notes, r2.Notes...)
	return staged, res
}

// ── 装一对的核对 ────────────────────────────────────────

// pfldIndex 只解析安装核对要用的字段（结构见 versions.py 的 _export）。
type pfldIndex struct {
	SchemaVersion int    `json:"schema_version"`
	ArchiveID     string `json:"archive_id"`
	Versions      []struct {
		ID       string `json:"id"`
		SHA256   string `json:"sha256"`
		Snapshot string `json:"snapshot"`
		State    string `json:"state"`
	} `json:"versions"`
}

// pflCheckSnapshots 读暂存区里的新档案索引，核对它引用的每个快照文件都在且哈希对。
// 快照路径是相对 VersionStore 根的（versions.py _insert_version_row 的 relative_to(self.root)，
// 形如「历史版本/文档/<file_id>/V###_<8>_<名>」）；按 台账目录的上级 → 单元根 → 台账目录 依次拼，
// 第一个存在的为准。返回原因（空 = 全对）。
func pflCheckSnapshots(ctx context.Context, a *PFApplier, dirRel, jsonSHA string) string {
	data, err := pflReadCapped(filepath.Join(a.stg, jsonSHA+".part"), pflMaxJSONBytes)
	if err != nil {
		return "读新档案索引失败：" + err.Error()
	}
	if pflSHA256Bytes(data) != jsonSHA {
		return "暂存区里的档案索引哈希对不上"
	}
	var idx pfldIndex
	if json.Unmarshal(data, &idx) != nil {
		return "新档案索引不是合法 JSON"
	}
	if idx.SchemaVersion != 1 {
		return fmt.Sprintf("不认识档案索引的格式版本 %d", idx.SchemaVersion)
	}
	dir := a.abs(dirRel)
	roots := []string{filepath.Dir(dir), a.root, dir}
	for _, v := range idx.Versions {
		if v.Snapshot == "" {
			continue
		}
		p := ""
		for _, root := range roots {
			cand := filepath.Join(root, filepath.FromSlash(v.Snapshot))
			if st, err := os.Lstat(cand); err == nil && st.Mode().IsRegular() {
				p = cand
				break
			}
		}
		if p == "" {
			return "引用的快照「" + v.Snapshot + "」还没到位"
		}
		if v.SHA256 == "" {
			return "版本行缺 sha256，无法核对快照「" + v.Snapshot + "」"
		}
		f, err := pfHashFile(ctx, p, false)
		if err != nil {
			return "读快照「" + v.Snapshot + "」失败：" + err.Error()
		}
		if f.SHA != v.SHA256 {
			return "快照「" + v.Snapshot + "」的内容与台账记的哈希不同"
		}
	}
	return ""
}

// pflTargetState 看目标边某个台账文件现在的位置和哈希：不存在返回 facts=nil。
func pflTargetState(ctx context.Context, a *PFApplier, rel string) (*pfFacts, error) {
	p := a.abs(rel)
	if _, err := os.Lstat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	f, err := pfHashFile(ctx, p, false)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ── 装一对（持锁：期望核对、旧的进回收、新的上正位）────────

// pfldInstallReq 是装台账一对的输入；Mac 侧直接构造，Ubuntu 侧经 ledger_install 的 ext 传过来。
// ExpectSQLite/ExpectJSON 是判定时的目标现状（Absent = 目标还没有台账）。
type pfldInstallReq struct {
	SQLite       pfldFileSpec `json:"sqlite"`
	JSON         pfldFileSpec `json:"json"`
	ExpectSQLite *PFExpect    `json:"expect_sqlite,omitempty"`
	ExpectJSON   *PFExpect    `json:"expect_json,omitempty"`
}

func (req pfldInstallReq) check() error {
	switch {
	case !pfValidSHA(req.SQLite.SHA256) || !pfValidSHA(req.JSON.SHA256):
		return errors.New("台账的哈希不合法")
	case req.SQLite.Size < 0 || req.JSON.Size < 0:
		return errors.New("台账的大小为负")
	case req.ExpectSQLite == nil || req.ExpectJSON == nil:
		return errors.New("缺少目标边的期望值")
	}
	return nil
}

// pflInstallPair 装台账一对到 applier 的根里。成功 = 两个都上了正位；
// 推迟/放弃时两个文件都还在原来的位置（或旧的一对已完整进回收，见 InstallOver 的保证）。
func pflInstallPair(ctx context.Context, a *PFApplier, dirRel string, req pfldInstallReq) PFApplyResult {
	res := PFApplyResult{Rel: dirRel}
	if err := req.check(); err != nil {
		res.Status, res.Code, res.Err = PFApplyAbandoned, "config", err
		res.Reason = "装台账的请求不合法：" + err.Error()
		return res
	}
	// 1) 快照先齐（只读，不持锁）：新 JSON 引用的快照必须都已经在这台机器上落地且哈希对。
	if why := pflCheckSnapshots(ctx, a, dirRel, req.JSON.SHA256); why != "" {
		return pfResult(dirRel, PFApplyDeferred, pflCodeSnapshots, "快照不齐，台账不装：%s", why)
	}
	dir := a.abs(dirRel)
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return pfResult(dirRel, PFApplyAbandoned, PFApplyCodeTargetMissing, "历史版本目录已经不在了")
	}
	// 2) 持台账锁——从这一刻起到装完放锁，应用（VersionStore）不会同时改台账。
	lk, ok, err := pfldLockFile(ctx, dir, pflLockWait)
	if err != nil {
		return pfClassifyErr(dirRel, "打开台账锁", err)
	}
	if !ok {
		return pfResult(dirRel, PFApplyDeferred, pflCodeLocked, "台账正被占用（等锁超时），推迟")
	}
	defer pfldUnlock(lk)
	if n := pfldJournal(dir); n != "" {
		return pfResult(dirRel, PFApplyDeferred, pflCodeJournal, "台账旁边有 %q（应用正在写），推迟", n)
	}
	// 3) 锁内核对：目标两个文件还是判定时的期望哈希（变了就放弃，本轮不动，下一轮重新判定）。
	sqRel, jsRel := dirRel+"/"+pfLedgerSQLite, dirRel+"/"+pfLedgerJSON
	if why := pflCheckTarget(sqRel, req.ExpectSQLite, func() (*pfFacts, error) { return pflTargetState(ctx, a, sqRel) }); why != "" {
		return pfResult(dirRel, PFApplyAbandoned, pflCodeExpect, "台账在判定之后又被改了：%s，放弃", why)
	}
	if why := pflCheckTarget(jsRel, req.ExpectJSON, func() (*pfFacts, error) { return pflTargetState(ctx, a, jsRel) }); why != "" {
		return pfResult(dirRel, PFApplyAbandoned, pflCodeExpect, "台账在判定之后又被改了：%s，放弃", why)
	}
	// 4) 装：先 sqlite 后 json（理由见文件头「装序」）。旧的由 InstallOver 换下并进同步回收（原因「覆盖」）。
	r1 := pflInstallFile(ctx, a, sqRel, req.SQLite, req.ExpectSQLite)
	if r1.Status != PFApplyDone {
		r1.Notes = append(r1.Notes, "档案索引还没动，两边下一轮重新判定")
		return r1
	}
	r2 := pflInstallFile(ctx, a, jsRel, req.JSON, req.ExpectJSON)
	if r2.Status != PFApplyDone {
		r2.Notes = append(r2.Notes, "台账 sqlite 已经换成新的、档案索引还是旧的：下一轮会判成「两边都改了台账待合并」停下来等人，不会再自动动")
		return r2
	}
	res.Status, res.Code = PFApplyDone, PFApplyCodeOK
	res.Notes = append(res.Notes, r1.Notes...)
	res.Notes = append(res.Notes, r2.Notes...)
	if r2.Recycled != nil {
		res.Recycled = r2.Recycled
	} else if r1.Recycled != nil {
		res.Recycled = r1.Recycled
	}
	res.Installed = r2.Installed
	return res
}

// pflCheckTarget 核对目标现状与期望；返回原因（空 = 符合）。get 返回 nil facts 表示目标不存在。
func pflCheckTarget(rel string, want *PFExpect, get func() (*pfFacts, error)) string {
	f, err := get()
	if err != nil {
		return rel + " 读不了：" + err.Error()
	}
	switch {
	case want.Absent && f != nil:
		return rel + " 已经出现了（判定之后有人建了）"
	case !want.Absent && f == nil:
		return rel + " 不在了"
	case f != nil:
		return pfldMatchWant(*f, want)
	}
	return ""
}

// pflInstallFile 装台账的一个文件：期望不存在就 InstallNew，存在就 InstallOver（旧的进回收「覆盖」）。
func pflInstallFile(ctx context.Context, a *PFApplier, rel string, spec pfldFileSpec, want *PFExpect) PFApplyResult {
	part := PFPart{Path: filepath.Join(a.stg, spec.SHA256+".part"), SHA256: spec.SHA256, Size: spec.Size}
	mtime := max(spec.MtimeNs, 1)
	if want.Absent {
		res, err := a.InstallNew(ctx, rel, part, mtime)
		if err != nil {
			res.Err = err
			res.Reason = "装台账（" + rel + "）配置错误：" + err.Error()
		}
		return res
	}
	res, err := a.InstallOver(ctx, rel, part, mtime, *want)
	if err != nil {
		res.Err = err
		res.Reason = "装台账（" + rel + "）配置错误：" + err.Error()
	}
	return res
}

// ── Ubuntu 扩展步骤：ledger_stage / ledger_install ─────

const (
	pfrdOpLedgerStage   = "ledger_stage"   // path=历史版本目录；结果 ext=pfldStaged（合并哈希 + 两个文件的 spec）
	pfrdOpLedgerInstall = "ledger_install" // path=历史版本目录；ext=pfldInstallReq；结果 Apply=装一对的结果
)

func init() {
	pfrRegisterOp(pfrdOpLedgerStage, pfrExtOp{
		unit: true,
		validate: func(st *PFRemoteStep) error {
			f := st.fields()
			if !f["path"] || len(f) != 1 {
				return fmt.Errorf("%s 步骤只能带 path", pfrdOpLedgerStage)
			}
			return checkPFRelPathUTF8(st.Path)
		},
		run: func(ctx context.Context, rc *pfrApplyRun, st *PFRemoteStep) PFRemoteStepResult {
			a, err := rc.applier()
			if err != nil {
				return pfrCfgErr(err)
			}
			staged, res := pflStage(ctx, a, st.Path)
			if res.Status != PFApplyDone {
				return PFRemoteStepResult{Apply: &res}
			}
			ext, err := json.Marshal(staged)
			if err != nil {
				res.Status, res.Code, res.Reason = PFApplyAbandoned, "config", "台账结果编不出来："+err.Error()
				return PFRemoteStepResult{Apply: &res}
			}
			return PFRemoteStepResult{Apply: &res, Ext: ext}
		},
	})
	pfrRegisterOp(pfrdOpLedgerInstall, pfrExtOp{
		unit: true,
		validate: func(st *PFRemoteStep) error {
			f := st.fields()
			if !f["path"] || !f["ext"] || len(f) != 2 {
				return fmt.Errorf("%s 步骤只能带 path 和 ext", pfrdOpLedgerInstall)
			}
			if err := checkPFRelPathUTF8(st.Path); err != nil {
				return err
			}
			var req pfldInstallReq
			if json.Unmarshal(st.Ext, &req) != nil {
				return fmt.Errorf("%s 的 ext 读不懂", pfrdOpLedgerInstall)
			}
			return req.check()
		},
		run: func(ctx context.Context, rc *pfrApplyRun, st *PFRemoteStep) PFRemoteStepResult {
			var req pfldInstallReq
			if json.Unmarshal(st.Ext, &req) != nil {
				return pfrErr("%s 的 ext 读不懂", pfrdOpLedgerInstall)
			}
			if err := req.check(); err != nil {
				return pfrCfgErr(err)
			}
			a, err := rc.applier()
			if err != nil {
				return pfrCfgErr(err)
			}
			res := pflInstallPair(ctx, a, st.Path, req)
			return PFRemoteStepResult{Apply: &res}
		},
	})
}

// ── 一轮里的编排（pfround_exec.go 的台账动作调这里）────

// doLedgerCopy 执行一个 ledger_copy 动作（技术设计 §7）：
// 源边读一对 → 两个 part 搬到目标边暂存区 → 目标边装一对 → base 记导出 + 台账合并哈希。
// 台账一对不参与暂缓：判定已保证两边静止，一对台账很小，直接传。
func (r *pfrdRun) doLedgerCopy(i int, a PFAction) {
	ls := a.Ledger
	if ls == nil || ls.Hash == "" || ls.Dir == "" {
		r.setSkip(i, a, "", "ledger_bad_step", "台账动作缺了信息，本轮不动")
		return
	}
	// 1) 源边读一对（持锁、journal 检查、进暂存区）。
	staged, ok := r.pfldStageSide(i, a, ls.Dir)
	if !ok {
		return
	}
	if staged.Hash != ls.Hash {
		r.setSkip(i, a, a.Src, pflCodeSrcChanged,
			fmt.Sprintf("%s 上台账在判定之后又变了（合并哈希 %s… 不是 %s…），本轮不拷，下一轮重新判定",
				a.Src.Machine(), staged.Hash[:16], ls.Hash[:16]))
		return
	}
	// 2) 两个文件搬到目标边暂存区（Mac 目标：本机暂存区已有，不搬；Ubuntu 目标：上传）。
	sent := int64(0)
	for _, spec := range []pfldFileSpec{staged.SQLite, staged.JSON} {
		if a.Dst == PFSideMac {
			break // 源就是 Mac：part 本来就在 Mac 暂存区
		}
		tr, ok := r.fetch(i, a, r.pflJob(a.Dst, ls.Dir, spec))
		if !ok {
			return
		}
		sent += tr.SentBytes
	}
	// 3) 读导出的字节（装之前从暂存区拿——装会把 part 用掉）。
	export, err := pflReadCapped(filepath.Join(r.in.Staging, staged.JSON.SHA256+".part"), pflMaxJSONBytes)
	if err != nil || pflSHA256Bytes(export) != staged.JSON.SHA256 {
		msg := "读导出的档案索引失败"
		if err == nil {
			msg = "导出的档案索引哈希对不上"
		} else {
			msg += "：" + err.Error()
		}
		r.setSkip(i, a, "", "ledger_export", msg+"，本轮不装，下一轮重新判定")
		return
	}
	// 4) 目标边装一对。
	if !r.pfldInstallSide(i, a, ls.Dir, pfldInstallReq{SQLite: staged.SQLite, JSON: staged.JSON, ExpectSQLite: ls.DstSQLite, ExpectJSON: ls.DstJSON}) {
		return
	}
	// 5) base：导出 + 台账合并哈希。
	if !r.pflBaseSet(i, a, ls.Hash, export) {
		return
	}
	r.recordDone(i, a, sent)
	r.notice("%s 上的历史版本台账已同步（合并哈希 %s…）", a.Dst.Machine(), ls.Hash[:16])
}

// doLedgerBase 执行一个 base_ledger 动作：两边台账一样、只有 base 落后——不动文件，
// 锁下再核对一遍台账还是那个哈希，把索引导出存好并把 base 的台账记录设上。
func (r *pfrdRun) doLedgerBase(i int, a PFAction) {
	if a.BaseLedgerHash == "" {
		r.setSkip(i, a, "", "ledger_bad_step", "base_ledger 动作没带合并哈希")
		return
	}
	dir := path.Dir(a.Path)
	app := r.applier()
	if app == nil {
		r.markNotRun(i, a)
		return
	}
	data, ok := r.pfldReadPair(i, a, app, dir, a.BaseLedgerHash)
	if !ok {
		return
	}
	if !r.pflBaseSet(i, a, a.BaseLedgerHash, data) {
		return
	}
	r.recordDone(i, a, 0)
}

// ── 编排的分支细节 ──────────────────────────────────────

// pfldStageSide 在源边读一对：Mac 本机调 pflStage；Ubuntu 发 ledger_stage。
func (r *pfrdRun) pfldStageSide(i int, a PFAction, dir string) (pfldStaged, bool) {
	if a.Src == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return pfldStaged{}, false
		}
		staged, res := pflStage(r.ctx, app, dir)
		if !r.macResult(i, a, res, nil) {
			return pfldStaged{}, false
		}
		return staged, true
	}
	rs, ok := r.sendNow([]PFRemoteStep{{Op: pfrdOpLedgerStage, Path: dir}}, true)
	if !ok || len(rs) == 0 {
		r.markNotRun(i, a)
		return pfldStaged{}, false
	}
	if _, ok := r.stepApply(i, a, rs[0]); !ok {
		return pfldStaged{}, false
	}
	var staged pfldStaged
	if err := json.Unmarshal(rs[0].Ext, &staged); err != nil || staged.Hash == "" ||
		!pfValidSHA(staged.SQLite.SHA256) || !pfValidSHA(staged.JSON.SHA256) {
		r.addError(a.Path, "ledger_stage", false, "Ubuntu 回的台账结果读不懂")
		r.setSkip(i, a, PFSideUbuntu, "ledger_stage_bad", "Ubuntu 回的台账结果读不懂，本轮不拷")
		return pfldStaged{}, false
	}
	return staged, true
}

// pfldInstallSide 在目标边装一对。
func (r *pfrdRun) pfldInstallSide(i int, a PFAction, dir string, req pfldInstallReq) bool {
	if a.Dst == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return false
		}
		return r.macResult(i, a, pflInstallPair(r.ctx, app, dir, req), nil)
	}
	ext, err := json.Marshal(req)
	if err != nil {
		r.setSkip(i, a, PFSideUbuntu, "ledger_install_bad", "装台账的请求编不出来："+err.Error())
		return false
	}
	rs, ok := r.sendNow([]PFRemoteStep{{Op: pfrdOpLedgerInstall, Path: dir, Ext: ext}}, true)
	if !ok || len(rs) == 0 {
		r.markNotRun(i, a)
		return false
	}
	_, ok = r.stepApply(i, a, rs[0])
	return ok
}

// pfldReadPair 在 Mac 锁下读台账一对并核对合并哈希（base_ledger 用：不动文件，只为拿导出的字节）。
func (r *pfrdRun) pfldReadPair(i int, a PFAction, app *PFApplier, dirRel, hash string) ([]byte, bool) {
	dir := app.abs(dirRel)
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		r.setSkip(i, a, PFSideMac, PFApplyCodeTargetMissing, "Mac 上历史版本目录已经不在了，base 不变")
		return nil, false
	}
	lk, ok, err := pfldLockFile(r.ctx, dir, pflLockWait)
	if err != nil {
		r.setSkip(i, a, PFSideMac, "ledger_lock", "打开台账锁失败："+err.Error())
		return nil, false
	}
	if !ok {
		r.setSkip(i, a, PFSideMac, pflCodeLocked, "台账正被占用（等锁超时），本轮不更新 base")
		return nil, false
	}
	defer pfldUnlock(lk)
	if n := pfldJournal(dir); n != "" {
		r.setSkip(i, a, PFSideMac, pflCodeJournal, fmt.Sprintf("台账旁边有 %q（应用正在写），本轮不更新 base", n))
		return nil, false
	}
	sq, err := pflReadCapped(filepath.Join(dir, pfLedgerSQLite), pflMaxSQLiteBytes)
	if err != nil {
		r.setSkip(i, a, PFSideMac, pflCodeTooBig, "读台账 sqlite 失败："+err.Error())
		return nil, false
	}
	js, err := pflReadCapped(filepath.Join(dir, pfLedgerJSON), pflMaxJSONBytes)
	if err != nil {
		r.setSkip(i, a, PFSideMac, pflCodeTooBig, "读档案索引失败："+err.Error())
		return nil, false
	}
	if PFLedgerPairHash(pflSHA256Bytes(sq), pflSHA256Bytes(js)) != hash {
		r.setSkip(i, a, PFSideMac, pflCodeChanged, "台账在判定之后又被改了，base 不变，下一轮重新判定")
		return nil, false
	}
	return js, true
}

// pflBaseSet 把导出存进 base 并把台账合并哈希记上；失败按写 base 失败处理（单元本轮停止写入）。
func (r *pfrdRun) pflBaseSet(i int, a PFAction, hash string, export []byte) bool {
	ref, err := r.base.SaveLedgerExport(hash, export)
	if err != nil {
		r.stopHard("base_write", "存台账导出失败：%v", err)
		r.setSkip(i, a, "", "base_write", fmt.Sprintf("台账已同步，但存导出副本失败：%v", err))
		return false
	}
	if err := r.base.SetLedger(PFBaseLedger{Hash: hash, Export: ref}); err != nil {
		r.stopHard("base_write", "写 base 的台账记录失败：%v", err)
		r.setSkip(i, a, "", "base_write", fmt.Sprintf("台账已同步，但写 base 失败：%v", err))
		return false
	}
	return true
}

// pflJob 造台账一个 part 的传输任务：目标边是 Ubuntu 就从 Mac 暂存区上传；
// 是 Mac 就从 Ubuntu 暂存区下载（源就是远端暂存区里那个内容寻址的 part）。
func (r *pfrdRun) pflJob(dst PFSide, dirRel string, spec pfldFileSpec) PFTransferJob {
	j := PFTransferJob{SHA256: spec.SHA256, Size: spec.Size, Priority: r.prio(dirRel)}
	if dst == PFSideUbuntu {
		j.Dir, j.Src = PFUpload, filepath.Join(r.in.Staging, spec.SHA256+".part")
	} else {
		j.Dir, j.Src = PFDownload, path.Join(PFRemoteStagingDir(r.in.Remote.Meta), spec.SHA256+".part")
	}
	return j
}
