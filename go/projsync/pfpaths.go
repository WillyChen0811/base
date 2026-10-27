package projsync

// §14.1/§14.4：XXYI 从业务库导出的「配置路径清单」~/.xxyi/projfiles-paths.json 的读取。
//
// 格式（version 1）：
//
//	{"version":1,"generated_at":"…","units":[{"key","code","serials":[…],"rel"}],"blocked":[{"code","reason"}]}
//
//   - code：项目编号（归一化）；key：单元键（code，或 code+R+该目录下最小流水号数字，如 260403J1001R2）；
//   - rel：配置目录相对工作目录的路径（/ 分隔）；同编号在主目录里面的另配目录导出时已并掉，不会在这里出现；
//   - blocked（可选，评审 22-1）：编号有配置、但配置不能同步（比如主目录不在工作目录下），导出时不能就这样把整个编号丢掉
//     （丢了开始同步的闸会把「没配置」当成可以放行）。编号在 blocked 里，开始同步一律拒绝，原因用 reason。
//
// 本文件只管读和校验形状，不管同步编排；路径本身由调用方传入（wgtund 传 ~/.xxyi/projfiles-paths.json）。
// 读不了（不存在/权限/JSON 坏了/形状不合格）一律返回 error——「有配置与否判断不了」时由调用方决定停下
// （开始同步的闸见 PFUnitMover.StartSync），绝不当成「没有配置」放行。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// PFConfigPathsVersion 是当前认得的清单版本。
const PFConfigPathsVersion = 1

// PFConfiguredUnit 是清单里的一条配置目录。
type PFConfiguredUnit struct {
	Key     string `json:"key"`
	Code    string `json:"code"`
	Serials []int  `json:"serials,omitempty"`
	Rel     string `json:"rel"`
}

// PFBlockedCode 是清单里一条「不能同步的编号」：编号有配置，但配置不能用（如主目录不在工作目录下）。
type PFBlockedCode struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"` // 中文，给人看；开始同步被拒绝时原样带上
}

// PFConfiguredPaths 是一份读出来的配置路径清单，按编号分组。
type PFConfiguredPaths struct {
	GeneratedAt string `json:"generated_at,omitempty"`
	// ByCode：编号 → 该编号配置的目录（按 rel 排序；主目录那条在前——rel 最短/键等于编号）。
	ByCode map[string][]PFConfiguredUnit `json:"-"`
	// Blocked：编号 → 不能同步的原因（清单里 blocked 字段；没有这个字段就是空 map）。
	Blocked map[string]string `json:"-"`
}

// BlockedReason 报告编号 code 是不是在清单的 blocked 里；在的话返回 reason（可能为空串）。
func (p *PFConfiguredPaths) BlockedReason(code string) (string, bool) {
	if p == nil {
		return "", false
	}
	r, ok := p.Blocked[code]
	return r, ok
}

// Units 返回编号 code 的配置目录（没有返回 nil）。
func (p *PFConfiguredPaths) Units(code string) []PFConfiguredUnit {
	if p == nil {
		return nil
	}
	return p.ByCode[code]
}

// Find 按 rel 精确找编号 code 名下的配置条目（rel 逐字节比对）。
func (p *PFConfiguredPaths) Find(code, rel string) (PFConfiguredUnit, bool) {
	for _, u := range p.Units(code) {
		if u.Rel == rel {
			return u, true
		}
	}
	return PFConfiguredUnit{}, false
}

// pfcfgDoc 是清单 JSON 的磁盘形状（DisallowUnknownFields 用）。
type pfcfgDoc struct {
	Version     int                `json:"version"`
	GeneratedAt string             `json:"generated_at,omitempty"`
	Units       []PFConfiguredUnit `json:"units"`
	Blocked     []PFBlockedCode    `json:"blocked,omitempty"`
}

// PFLoadConfiguredPaths 读并校验清单文件。文件不存在、读不了、JSON 不合法、条目不合格都返回 error
// （绝不返回空清单当「都没配置」）。
func PFLoadConfiguredPaths(path string) (*PFConfiguredPaths, error) {
	if path == "" {
		return nil, errors.New("projsync: 配置路径清单路径为空")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("projsync: 读配置路径清单 %s 失败: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc pfcfgDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("projsync: 配置路径清单 %s 不是合法 JSON: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("projsync: 配置路径清单 %s 后面还有多余内容", path)
	}
	if doc.Version != PFConfigPathsVersion {
		return nil, fmt.Errorf("projsync: 配置路径清单 %s 版本 %d 不认识（只认 %d）", path, doc.Version, PFConfigPathsVersion)
	}
	out := &PFConfiguredPaths{GeneratedAt: doc.GeneratedAt, ByCode: map[string][]PFConfiguredUnit{}, Blocked: map[string]string{}}
	for i, b := range doc.Blocked {
		if err := pfcfgCheckCode(b.Code); err != nil {
			return nil, fmt.Errorf("projsync: 配置路径清单 %s blocked 第 %d 条: %w", path, i+1, err)
		}
		if _, dup := out.Blocked[b.Code]; dup {
			return nil, fmt.Errorf("projsync: 配置路径清单 %s blocked 里编号 %s 出现了两次", path, b.Code)
		}
		out.Blocked[b.Code] = b.Reason
	}
	seenRel := map[string]int{} // rel → 首见条目号（同 rel 两条直接坏）
	for i, u := range doc.Units {
		if err := pfcfgCheckUnit(u); err != nil {
			return nil, fmt.Errorf("projsync: 配置路径清单 %s 第 %d 条: %w", path, i+1, err)
		}
		if j, ok := seenRel[u.Rel]; ok {
			return nil, fmt.Errorf("projsync: 配置路径清单 %s 里 rel %q 出现了两次（第 %d、%d 条）", path, u.Rel, j+1, i+1)
		}
		seenRel[u.Rel] = i
		out.ByCode[u.Code] = append(out.ByCode[u.Code], u)
	}
	for code := range out.ByCode {
		g := out.ByCode[code]
		sort.Slice(g, func(i, j int) bool {
			if g[i].Key != g[j].Key {
				return g[i].Key < g[j].Key
			}
			return g[i].Rel < g[j].Rel
		})
		out.ByCode[code] = g
	}
	return out, nil
}

// pfcfgCheckCode 校验清单里的项目编号：归一化形式、纯 ASCII、认得出是项目编号。
func pfcfgCheckCode(code string) error {
	if code == "" || !pfIsASCII(code) || PFNormCode(code) != code {
		return fmt.Errorf("编号 %q 不合法（应是归一化形式：无空白、大写、纯 ASCII）", code)
	}
	if c, ok := PFCodeFromName(code); !ok || c != code {
		return fmt.Errorf("编号 %q 认不出是项目编号", code)
	}
	return nil
}

// pfcfgCheckUnit 校验一条配置：编号、键、rel 的格式与相互关系（键 = 编号或 编号+R+流水号数字）。
func pfcfgCheckUnit(u PFConfiguredUnit) error {
	if err := pfcfgCheckCode(u.Code); err != nil {
		return err
	}
	pc, ok := PFUnitKeyProjectCode(u.Key)
	if !ok {
		return fmt.Errorf("键 %q 不合法（应是编号或 编号+R+流水号数字）", u.Key)
	}
	if pc != u.Code {
		return fmt.Errorf("键 %q 的编号 %q 与 code %q 不一致", u.Key, pc, u.Code)
	}
	if err := validRel(u.Rel); err != nil {
		return fmt.Errorf("rel %q 不合法: %w", u.Rel, err)
	}
	segs := strings.Split(u.Rel, "/")
	if len(segs) > PFUnitMaxDepth {
		return fmt.Errorf("rel %q 超过 %d 层", u.Rel, PFUnitMaxDepth)
	}
	return nil
}
