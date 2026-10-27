package projsync

// 项目文件同步 T9a：删空目录前清系统垃圾文件（技术设计 §3.1「目录」、PFApplier.Rmdir 的 not_empty 约定）。
//
// 判定看不到被排除的文件（.DS_Store 等），所以「另一边删了这个目录、这边已空」时 rmdir 可能因为里面只剩这些垃圾文件而 not_empty。
// 处理：目录里的东西全是系统垃圾文件（.DS_Store、._*、Thumbs.db、desktop.ini、Icon\r，且都是普通文件）→ 逐个移进同步回收
// （原因「删除」，进 index），再 rmdir；只要还有一样别的（隐藏目录、用户放的点开头文件……）就一个也不动，保留目录并报告。
//
// PFApplier.Delete 会按排除规则拒绝这些名字（它只处理同步范围内的文件），所以这里用同包的回收区原语直接改名放进去。
// Mac 侧直接调 pfrdJunkClearDir；Ubuntu 侧通过扩展步骤 junk_clear（pfrRegisterOp）在 Ubuntu 本机调同一个函数。
// 回收区里的东西本文件不删。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// pfrdOpJunkClear 是 Ubuntu 侧的扩展步骤名：path 是要清的目录（单元内相对路径）。
// 结果：Apply.Status 完成 = 垃圾文件都已移进回收（或本来就没有），可以接着 rmdir；放弃 + not_empty = 目录里还有别的东西，什么都没动。
// 结果的 Ext 是 {"moved":[名字…],"others":[名字…]}。
const pfrdOpJunkClear = "junk_clear"

type pfrdJunkExt struct {
	Moved  []string `json:"moved,omitempty"`
	Others []string `json:"others,omitempty"`
}

func pfrdJunkMoved(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var e pfrdJunkExt
	if json.Unmarshal(raw, &e) != nil {
		return nil
	}
	return e.Moved
}

func init() {
	pfrRegisterOp(pfrdOpJunkClear, pfrExtOp{
		unit: true,
		validate: func(st *PFRemoteStep) error {
			f := st.fields()
			if !f["path"] || len(f) != 1 {
				return fmt.Errorf("%s 步骤只能带 path", pfrdOpJunkClear)
			}
			return checkPFRelPathUTF8(st.Path)
		},
		run: func(ctx context.Context, rc *pfrApplyRun, st *PFRemoteStep) PFRemoteStepResult {
			a, err := rc.applier()
			if err != nil {
				return pfrCfgErr(err)
			}
			res, moved, others := pfrdJunkClearDir(ctx, a, st.Path)
			ext, _ := json.Marshal(pfrdJunkExt{Moved: moved, Others: others})
			return PFRemoteStepResult{Apply: &res, Ext: ext}
		},
	})
}

func checkPFRelPathUTF8(p string) error {
	if err := validRel(p); err != nil {
		return err
	}
	if !utf8.ValidString(p) {
		return fmt.Errorf("路径 %q 不是合法 UTF-8", p)
	}
	return nil
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// pfrdIsJunkName 是 §4.1 里「系统垃圾文件」那几类：.DS_Store、._*、Thumbs.db、desktop.ini、Icon\r。
func pfrdIsJunkName(name string) bool {
	l := strings.ToLower(name)
	return name == ".DS_Store" || strings.HasPrefix(name, "._") || l == "thumbs.db" || l == "desktop.ini" || name == "Icon\r"
}

// pfrdJunkClearDir 见文件头。返回移走的名字和（有别的东西时）其余的名字；其余的非空时一个文件也没动。
func pfrdJunkClearDir(ctx context.Context, a *PFApplier, rel string) (res PFApplyResult, moved, others []string) {
	if r := a.checkRel(rel, true); r != nil {
		return *r, nil, nil
	}
	w, wr := a.walk(rel, false)
	if wr != nil {
		return *wr, nil, nil
	}
	if w.leaf == nil {
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeTargetMissing, "目录已经不在了"), nil, nil
	}
	if w.leaf.Type()&os.ModeSymlink != 0 || !w.leaf.IsDir() {
		return pfResult(rel, PFApplyOutOfScope, PFApplyCodeNotDir, "这个位置在磁盘上不是目录（符号链接或文件），不动"), nil, nil
	}
	ents, err := os.ReadDir(a.abs(rel))
	if err != nil {
		return pfClassifyErr(rel, "读目录", err), nil, nil
	}
	var junk []string
	for _, e := range ents {
		if pfrdIsJunkName(e.Name()) && e.Type().IsRegular() {
			junk = append(junk, e.Name())
		} else {
			others = append(others, e.Name())
		}
	}
	if len(others) > 0 {
		shown := others
		if len(shown) > 5 {
			shown = append(append([]string{}, shown[:5]...), fmt.Sprintf("等共 %d 个", len(others)))
		}
		return pfResult(rel, PFApplyAbandoned, PFApplyCodeNotEmpty, "目录里还有别的东西（%s）", strings.Join(shown, "、")), nil, others
	}
	for _, n := range junk {
		p := rel + "/" + n
		entry := PFRecycleEntry{Unit: a.cfg.UnitRel, Path: p, Reason: string(PFReasonDelete), Round: a.cfg.Round}
		placed, err := a.recycle(a.abs(p), p, &entry)
		if err != nil {
			r := pfClassifyErr(p, "移入同步回收", err)
			r.Residue = moved
			return r, moved, nil
		}
		moved = append(moved, n)
		// 垃圾文件，不必像用户文件那样移走后再逐字节核对；但回收记录里的哈希和大小要真实
		if f, herr := pfHashFile(context.WithoutCancel(ctx), placed.stored, false); herr == nil {
			entry.SHA256, entry.Size, entry.MtimeNs = f.SHA, f.Stat.Size, f.Stat.MtimeNs
		} else {
			entry.Note = "移走后读内容失败：" + herr.Error()
		}
		if err := a.cfg.Recycle.record(placed.day, entry); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("回收记账（index.jsonl）写失败：%v（文件已在 %s）", err, placed.stored))
		}
	}
	res.Rel, res.Status, res.Code = rel, PFApplyDone, PFApplyCodeOK
	return res, moved, nil
}
