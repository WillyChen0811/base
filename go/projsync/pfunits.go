package projsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 项目文件同步（技术设计 §12.2）里「同步单元」这一层：哪些目录算项目单元、Mac 登记了哪些单元。
//
// 本文件管：
//  1. 从目录名认出项目编号、编号归一化——xxyi 的 PROJECT_SEARCH_RE 的 Go 移植，用 Python 生成的金样锁住
//     （testdata/pfunits_codes.json）；
//  2. Ubuntu 侧单元目录发现（根下最多 5 层，认出编号即停，不跟随符号链接，同编号两个目录两个都标重复）；
//  3. Mac 侧登记表 units.json 的内存结构、校验、JSON 序列化/反序列化（只做内存与字节，不写文件，
//     原子写由编排层负责）。单元键 Code 可以是 编号 或 编号+R+流水号（§14.4），ProjectCode 记它属于哪个编号；
//  4. 单元对账分类（§14.4：按 rel 配对——同步中/正在转变/已停止/没配置的同编号目录/新项目/重复/Ubuntu 上不见了）
//     与「Mac 工作目录里是否已有同编号目录」。
//
// 本文件不管：写任何文件、判定、传输、开始/取消同步时的挪目录和建链接（pfunitmove）、base。
//
// 总原则「识别边界、超出就停」：认不出、读不了、对不上的一律报告并停手，不猜、不静默修。

// ============================================================================
// 一、项目编号识别（xxyi/common/codes.py:141 PROJECT_SEARCH_SOURCE 的移植）
// ============================================================================
//
// Python 原式（不带任何 flag，所以没有 IGNORECASE 的 Unicode 大小写折叠）：
//
//	(  (?<![0-9A-Za-z])\d{2}0403[PQJYXCNZTWpqjyxcnztw]\d{4}                     新格式
//	 | (?<![A-Za-z])[Cc][Cc][Ii][Cc]\s?[A-Za-z]{0,5}\s?\d{6}\s?\d{2,}           旧格式
//	 | (?<![A-Za-z])TMP\d{8,}                                                    临时编号
//	)(?![0-9])
//
// Go 的 regexp（RE2）不支持 (?<!…) 后行断言和 (?!…) 先行断言，没法照抄；这里不用 regexp，
// 按 Python 的搜索语义手写匹配（从左到右逐个起点试，三个分支按顺序试，量词按贪婪优先、
// 失败再回溯），差别与处理如下，金样里每一条都有覆盖：
//
//   - (?<![0-9A-Za-z]) / (?<![A-Za-z])：起点前一个字符的检查。只看 ASCII 字母数字，
//     全角数字、中文、下划线、连字符前面都算「没挨着」。
//   - (?![0-9])：终点后一个字符的检查，只拦 ASCII 数字，后面跟全角数字不拦。
//   - \d：Python 的 str 模式下认全部 Unicode 十进制数字（Nd 类，全角 ０-９、阿拉伯-印度数字等），
//     RE2 的 \d 只认 ASCII，所以用 unicode.IsDigit（同为 Nd 类）。
//     字面量 0403 是 ASCII 字面量，不受影响。
//   - \s：Python 的 str 模式下认 str.isspace() 为真的字符，包括全角空格 U+3000、不换行空格
//     U+00A0、U+1C-U+1F 控制符；RE2 的 \s 只认 ASCII 的 [\t\n\f\r ]；Go 的 unicode.IsSpace
//     又少了 U+1C-U+1F。所以用下面 pfIsSpace 这张与 Python 一致的表（零宽空格 U+200B、U+180E、
//     BOM 不算空白，Python 也不算）。
//   - `\s?[A-Za-z]{0,5}\s?\d{6}\s?\d{2,}` 的回溯：按优先顺序逐路试（\s? 先吃后不吃，字母从多到少，
//     \d{2,} 从多到少），取第一条成功的；与 Python 结果一致。
//   - 起点逐个字符（rune）前进；名字里不合法的 UTF-8 字节按 U+FFFD 处理，既不是数字也不是字母，
//     与 Python 里的代理转义字符同样「不挨着」，对结果没有影响。
//
// 认出的编号只含 ASCII 字母、(Unicode) 数字和空白；归一化 = 去掉所有空白、ASCII 字母转大写。
// Python 的 _norm 是 str.upper()（Unicode 大写），对本正则认出来的串没有区别（字母都是 ASCII）。

// pfIsSpace 与 Python 的 str.isspace()（re 的 \s）一致。
func pfIsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20:
		return true
	case r == 0x85, r == 0xa0, r == 0x1680:
		return true
	case r >= 0x2000 && r <= 0x200a:
		return true
	case r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// pfIsDigit 对应 Python 的 \d（Nd 类）。
func pfIsDigit(r rune) bool { return unicode.IsDigit(r) }

func pfIsASCIIDigit(r rune) bool  { return r >= '0' && r <= '9' }
func pfIsASCIILetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

// pfNoDigitAt 是 (?![0-9])：位置 i 上没有 ASCII 数字（含已到末尾）。
func pfNoDigitAt(rs []rune, i int) bool { return i >= len(rs) || !pfIsASCIIDigit(rs[i]) }

// pfNoAlnumBefore 是 (?<![0-9A-Za-z])。
func pfNoAlnumBefore(rs []rune, i int) bool {
	return i == 0 || !(pfIsASCIIDigit(rs[i-1]) || pfIsASCIILetter(rs[i-1]))
}

// pfNoLetterBefore 是 (?<![A-Za-z])。
func pfNoLetterBefore(rs []rune, i int) bool { return i == 0 || !pfIsASCIILetter(rs[i-1]) }

// pfDigitRun 返回从 i 起连续的 \d 个数。
func pfDigitRun(rs []rune, i int) int {
	n := 0
	for i+n < len(rs) && pfIsDigit(rs[i+n]) {
		n++
	}
	return n
}

// pfMatchNew 匹配新格式：\d{2}0403[类型字母]\d{4}，全定长。返回终点（不含）。
func pfMatchNew(rs []rune, i int) (int, bool) {
	if !pfNoAlnumBefore(rs, i) || i+11 > len(rs) {
		return 0, false
	}
	if !pfIsDigit(rs[i]) || !pfIsDigit(rs[i+1]) {
		return 0, false
	}
	for k, c := range "0403" {
		if rs[i+2+k] != c {
			return 0, false
		}
	}
	if !strings.ContainsRune("PQJYXCNZTWpqjyxcnztw", rs[i+6]) {
		return 0, false
	}
	for k := 7; k < 11; k++ {
		if !pfIsDigit(rs[i+k]) {
			return 0, false
		}
	}
	if !pfNoDigitAt(rs, i+11) {
		return 0, false
	}
	return i + 11, true
}

// pfMatchOld 匹配旧格式：CCIC\s?[A-Za-z]{0,5}\s?\d{6}\s?\d{2,}（CCIC 四个字母各自大小写都认）。
func pfMatchOld(rs []rune, i int) (int, bool) {
	if !pfNoLetterBefore(rs, i) || i+4 > len(rs) {
		return 0, false
	}
	for k, c := range "CCIC" {
		r := rs[i+k]
		if r != c && r != c+('a'-'A') {
			return 0, false
		}
	}
	p := i + 4
	// 第一个 \s?：先吃后不吃
	for _, take1 := range []bool{true, false} {
		q := p
		if take1 {
			if q < len(rs) && pfIsSpace(rs[q]) {
				q++
			} else {
				continue
			}
		}
		maxLetters := 0
		for maxLetters < 5 && q+maxLetters < len(rs) && pfIsASCIILetter(rs[q+maxLetters]) {
			maxLetters++
		}
		for k := maxLetters; k >= 0; k-- { // 字母从多到少
			for _, take2 := range []bool{true, false} {
				r := q + k
				if take2 {
					if r < len(rs) && pfIsSpace(rs[r]) {
						r++
					} else {
						continue
					}
				}
				if end, ok := pfMatchOldDigits(rs, r); ok {
					return end, true
				}
			}
		}
	}
	return 0, false
}

// pfMatchOldDigits 匹配 \d{6}\s?\d{2,} 并带上整条正则末尾的 (?![0-9])。
func pfMatchOldDigits(rs []rune, r int) (int, bool) {
	if pfDigitRun(rs, r) < 6 {
		return 0, false
	}
	s0 := r + 6
	for _, take3 := range []bool{true, false} {
		s := s0
		if take3 {
			if s < len(rs) && pfIsSpace(rs[s]) {
				s++
			} else {
				continue
			}
		}
		run := pfDigitRun(rs, s)
		for e := run; e >= 2; e-- { // \d{2,} 贪婪，失败退一位
			if pfNoDigitAt(rs, s+e) {
				return s + e, true
			}
		}
	}
	return 0, false
}

// pfMatchTemp 匹配临时编号：TMP\d{8,}（只认大写）。
func pfMatchTemp(rs []rune, i int) (int, bool) {
	if !pfNoLetterBefore(rs, i) || i+3 > len(rs) || rs[i] != 'T' || rs[i+1] != 'M' || rs[i+2] != 'P' {
		return 0, false
	}
	run := pfDigitRun(rs, i+3)
	for e := run; e >= 8; e-- {
		if pfNoDigitAt(rs, i+3+e) {
			return i + 3 + e, true
		}
	}
	return 0, false
}

func pfMatchCodeAt(rs []rune, i int) (int, bool) {
	if end, ok := pfMatchNew(rs, i); ok {
		return end, true
	}
	if end, ok := pfMatchOld(rs, i); ok {
		return end, true
	}
	return pfMatchTemp(rs, i)
}

// pfFindCodes 按 finditer 的语义找出全部匹配（不重叠，从上一个匹配的终点继续）。first 为真时只要第一个。
func pfFindCodes(name string, first bool) []string {
	rs := []rune(name)
	var out []string
	for i := 0; i < len(rs); {
		if end, ok := pfMatchCodeAt(rs, i); ok {
			out = append(out, string(rs[i:end]))
			if first {
				return out
			}
			i = end
			continue
		}
		i++
	}
	return out
}

// PFNormCode 归一化项目编号：去掉全部空白（含全角空格等，与 Python 的 \s 一致），ASCII 字母转大写。
// 非 ASCII 字符原样保留、不做大小写映射（识别出来的编号里没有非 ASCII 字母）。
func PFNormCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if pfIsSpace(r) {
			continue
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// PFCodeFromName 从目录名认出项目编号：取第一个匹配并归一化，认不出返回 ("", false)。
// 与 /list -project（local_fns.list_project_folders 的 PROJECT_CODE_PATTERN.search）一致。
//
// 注意：Python 的 \d 认全角数字等，所以返回值可能含非 ASCII 数字；要当单元键用的地方
// 另行检查 ASCII（见 pfDiscover、PFValidateUnit）。
func PFCodeFromName(name string) (string, bool) {
	m := pfFindCodes(name, true)
	if len(m) == 0 {
		return "", false
	}
	return PFNormCode(m[0]), true
}

// PFCodesInName 返回名字里按出现顺序认出的全部编号（归一化，含重复），与 scan_project_dirs 的 finditer 一致。
func PFCodesInName(name string) []string {
	m := pfFindCodes(name, false)
	out := make([]string, len(m))
	for i, s := range m {
		out[i] = PFNormCode(s)
	}
	return out
}

func pfIsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// ============================================================================
// 二、单元目录发现（Ubuntu 侧 poll 用，§12.2）
// ============================================================================

// PFUnitMaxDepth 是单元目录最多往下找的层数（根的直接子目录算第 1 层），与 /list -project 的 max_depth=5 一致。
const PFUnitMaxDepth = 5

// 单元状态文件/界面里用的说法（T8、T9 写状态时照用）。
const (
	PFNoteStopped         = "已停止同步"
	PFNoteNewOnUbuntu     = "Ubuntu 新项目"
	PFNoteDuplicate       = "重复，停下"
	PFNoteUnconfigured    = "同编号的另一个目录，没配置，不同步" // §14.2：登记单元按 rel 对，同编号不同 rel 只报告
	PFNoteStartOnMacFirst = "请先对 Mac 上这个项目点开始同步"
	PFNoteSlotOccupied    = "Mac 上这个位置已被占用（不是项目目录），不自动建"
)

// PFDirOps 是目录读取的可替换实现，便于测试注入；字段为 nil 时用 os.Lstat / os.ReadDir。
type PFDirOps struct {
	Lstat   func(path string) (fs.FileInfo, error)
	ReadDir func(path string) ([]fs.DirEntry, error)
}

func (o PFDirOps) lstat(p string) (fs.FileInfo, error) {
	if o.Lstat != nil {
		return o.Lstat(p)
	}
	return os.Lstat(p)
}

func (o PFDirOps) readDir(p string) ([]fs.DirEntry, error) {
	if o.ReadDir != nil {
		return o.ReadDir(p)
	}
	return os.ReadDir(p)
}

// PFFoundUnit 是发现到的一个单元目录。
type PFFoundUnit struct {
	Code    string   // 归一化编号（单元键）
	Rel     string   // 相对根的路径，/ 分隔
	Dup     bool     // G4：同一编号在两个以上目录里认出，这些目录都标重复，单元停下
	DupRels []string // Dup 时同编号的其他目录（不含自己，按路径排序）
	Others  []string // 目录名里认出的其他编号（不同于 Code，保持出现顺序、去重）；只供上层报告，单元键仍是第一个
}

// PFDiscoverSkip 是发现时跳过的目录或链接，以及原因。
type PFDiscoverSkip struct {
	Rel        string
	Reason     string
	Unreadable bool // 目录读不了（权限、IO 错误）
}

// PFDiscoverResult 是一次发现的结果。
type PFDiscoverResult struct {
	Units    []PFFoundUnit    // 按 Rel 排序；Dup 的也在里面
	Skipped  []PFDiscoverSkip // 符号链接、读不了的目录、到第 5 层仍认不出编号的目录、名字不合法的候选
	Excluded int              // 「.」开头的目录与链接的个数（不同步，只计数）
	// Incomplete 为真表示有目录没看到或没往里看（读不了、符号链接、层数用完），Units 可能不全：
	// 不能据此判定「某个已登记单元在 Ubuntu 上不见了」。
	Incomplete bool
}

// PFDiscoverUnits 在 root 下找单元目录：从 root 往下最多 PFUnitMaxDepth 层，目录名认得出编号的就是一个单元，
// 认出后不再往里走。不跟随符号链接（报进 Skipped）；读不了的子目录报进 Skipped（Unreadable）；
// root 本身读不了返回 error（不能当成「一个单元都没有」）。「.」开头的目录不进、只计数（§4.1）。
func PFDiscoverUnits(root string) (PFDiscoverResult, error) {
	return pfDiscover(root, PFUnitMaxDepth, os.ReadDir)
}

const (
	pfReasonSymlink = "符号链接，不跟随"
	pfReasonDepth   = "已到第 %d 层，目录名认不出编号，不再往里找"
)

func pfDiscover(root string, maxDepth int, readDir func(string) ([]fs.DirEntry, error)) (PFDiscoverResult, error) {
	var res PFDiscoverResult
	if root == "" {
		return res, errors.New("projsync: 发现单元的根目录不能为空")
	}
	var walk func(dir, relPrefix string, depth int) error
	walk = func(dir, relPrefix string, depth int) error {
		entries, err := readDir(dir)
		if err != nil {
			if depth == 1 {
				return fmt.Errorf("projsync: 读不了根目录 %s: %w", root, err)
			}
			res.Skipped = append(res.Skipped, PFDiscoverSkip{Rel: relPrefix, Reason: fmt.Sprintf("读不了: %v", err), Unreadable: true})
			res.Incomplete = true
			return nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			name := e.Name()
			rel := name
			if relPrefix != "" {
				rel = relPrefix + "/" + name
			}
			typ := e.Type()
			isLink := typ&fs.ModeSymlink != 0
			if !isLink && !e.IsDir() {
				continue // 文件和其他特殊文件：不是单元，也不往里走
			}
			if strings.HasPrefix(name, ".") {
				res.Excluded++
				continue
			}
			if isLink {
				res.Skipped = append(res.Skipped, PFDiscoverSkip{Rel: rel, Reason: pfReasonSymlink})
				res.Incomplete = true
				continue
			}
			if code, ok := PFCodeFromName(name); ok {
				// 认出编号：是单元，不再往里走。不合格的候选报告并跳过（仍不往里走，与 /list -project 一致）。
				if !utf8.ValidString(rel) {
					res.Skipped = append(res.Skipped, PFDiscoverSkip{Rel: rel, Reason: "名字不是合法 UTF-8，不作为同步单元"})
					res.Incomplete = true
					continue
				}
				if err := validRel(rel); err != nil {
					res.Skipped = append(res.Skipped, PFDiscoverSkip{Rel: rel, Reason: "路径不合法，不作为同步单元: " + err.Error()})
					res.Incomplete = true
					continue
				}
				if !pfIsASCII(code) {
					res.Skipped = append(res.Skipped, PFDiscoverSkip{Rel: rel, Reason: "编号含非 ASCII 字符（如全角数字），不作为同步单元"})
					res.Incomplete = true
					continue
				}
				res.Units = append(res.Units, PFFoundUnit{Code: code, Rel: rel, Others: pfOtherCodes(name, code)})
				continue
			}
			if depth >= maxDepth {
				res.Skipped = append(res.Skipped, PFDiscoverSkip{Rel: rel, Reason: fmt.Sprintf(pfReasonDepth, maxDepth)})
				res.Incomplete = true
				continue
			}
			if err := walk(filepath.Join(dir, name), rel, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, "", 1); err != nil {
		return PFDiscoverResult{}, err
	}
	sort.Slice(res.Units, func(i, j int) bool { return res.Units[i].Rel < res.Units[j].Rel })
	pfMarkDup(res.Units)
	return res, nil
}

// pfOtherCodes 取目录名里除 code 之外的其他编号，去重、保持顺序；没有返回 nil。
func pfOtherCodes(name, code string) []string {
	var out []string
	seen := map[string]bool{code: true}
	for _, c := range PFCodesInName(name) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// pfMarkDup 按编号分组，两个以上目录同编号的都标 Dup 并记下对方。units 已按 Rel 排序。
func pfMarkDup(units []PFFoundUnit) {
	byCode := map[string][]int{}
	for i, u := range units {
		byCode[u.Code] = append(byCode[u.Code], i)
	}
	for _, idx := range byCode {
		if len(idx) < 2 {
			continue
		}
		for _, i := range idx {
			units[i].Dup = true
			units[i].DupRels = nil
			for _, j := range idx {
				if j != i {
					units[i].DupRels = append(units[i].DupRels, units[j].Rel)
				}
			}
		}
	}
}

// ============================================================================
// 三、登记表 units.json（Mac 侧，§12.2；只做内存结构、校验和 JSON 进出，不写文件）
// ============================================================================

// PFUnitState 是登记表里单元的状态。
type PFUnitState string

const (
	PFUnitSyncing  PFUnitState = "syncing"  // 同步中
	PFUnitStarting PFUnitState = "starting" // 正在开始同步（挪目录、建链接的过程中）
	PFUnitStopping PFUnitState = "stopping" // 正在取消同步（轮次跳过）
	PFUnitStopped  PFUnitState = "stopped"  // 已停止同步（base 保留，Ubuntu 那份不删）
)

func (s PFUnitState) valid() bool {
	switch s {
	case PFUnitSyncing, PFUnitStarting, PFUnitStopping, PFUnitStopped:
		return true
	}
	return false
}

// PFUnitRecord 是登记表里的一项。
//
//   - Code：单元键（归一化），等于项目编号，或 编号+`R`+流水号数字（§14.1 的另配目录单元，如 `260403J1001R2`）。
//     200 多处按它分组、命名状态文件，不改名。
//   - ProjectCode：这个单元属于哪个项目编号；空 = 等于 Code（主目录单元）。键带 `R` 后缀时必须显式写。
//   - Rel：同步目录/工作目录下的相对路径（`<年>/<编号-名称>`，/ 分隔）。
//   - LinkPath：桌面原位置（工作目录/Rel）的绝对路径，开始同步后是指向 SyncPath 的符号链接。
//   - SyncPath：同步目录/Rel 的绝对路径，项目实际所在。
//   - StartedAt：开始同步的时间。
//
// state 为 stopped 时单元已挪回桌面，LinkPath、SyncPath 可以为空；不为空则照常校验。
type PFUnitRecord struct {
	Code        string      `json:"code"`
	ProjectCode string      `json:"project_code,omitempty"`
	Rel         string      `json:"rel"`
	State       PFUnitState `json:"state"`
	LinkPath    string      `json:"link_path"`
	SyncPath    string      `json:"sync_path"`
	StartedAt   time.Time   `json:"started_at"`
}

// PFUnitProjectCode 返回这条登记的编号（ProjectCode 为空时等于 Code）。
func (u PFUnitRecord) PFUnitProjectCode() string {
	if u.ProjectCode != "" {
		return u.ProjectCode
	}
	return u.Code
}

// pfAllASCIIDigits 报告 s 非空且全是 ASCII 数字。
func pfAllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// PFUnitKeyProjectCode 从单元键认出项目编号（§14.4）：键就是编号，或 编号+`R`+流水号数字。
// 带后缀时从右往左找第一个「后面全是数字的 R」，前段必须本身是认得出的归一化编号。认不出返回 ("", false)。
func PFUnitKeyProjectCode(key string) (string, bool) {
	if key == "" || !pfIsASCII(key) || PFNormCode(key) != key {
		return "", false
	}
	if c, ok := PFCodeFromName(key); ok && c == key {
		return key, true
	}
	for i := len(key) - 2; i >= 1; i-- {
		if key[i] != 'R' || !pfAllASCIIDigits(key[i+1:]) {
			continue
		}
		base := key[:i]
		if c, ok := PFCodeFromName(base); ok && c == base && pfIsASCII(base) {
			return base, true
		}
	}
	return "", false
}

// pfUnitKeyMatchesRel 判断单元键跟 Rel 最后一段认出的编号配不配：相等（主目录单元），
// 或 编号+`R`+流水号数字（另配目录单元，§14.4）。
func pfUnitKeyMatchesRel(key, relCode string) bool {
	if key == relCode {
		return true
	}
	return strings.HasPrefix(key, relCode+"R") && pfAllASCIIDigits(key[len(relCode)+1:])
}

// PFValidateUnit 校验一条登记：
//   - Code 非空、纯 ASCII、已是归一化形式（无空白、大写）、是认得出的项目编号，或 编号+`R`+流水号数字
//     （§14.4 的另配目录单元键）；
//   - ProjectCode 为空（= 等于 Code）时不查；非空时必须是 ASCII、归一化，并且等于 Rel 最后一段认出的编号；
//   - Rel 最后一段认出的编号必须等于 Code，或 Code = 该编号+`R`+流水号数字（§14.4）；
//   - State 是四个值之一；
//   - Rel 过 validRel，不超过 PFUnitMaxDepth 段，没有「.」开头的段，非最后一段的目录名里认不出编号
//     （否则 Ubuntu 侧发现会停在上级目录，对不上）；
//   - LinkPath、SyncPath 是干净的绝对路径（filepath.Clean 后不变）且都以 /Rel 结尾，彼此不同；
//   - StartedAt 非零。
//
// 旧的 units.json 没有 project_code 字段，照常读（omitempty；Code 就是编号的老记录全部走「等于」这条路）。
func PFValidateUnit(u PFUnitRecord) error {
	if u.Code == "" {
		return errors.New("projsync: 登记缺少编号 code")
	}
	if !pfIsASCII(u.Code) {
		return fmt.Errorf("projsync: 登记编号 %q 含非 ASCII 字符", u.Code)
	}
	if PFNormCode(u.Code) != u.Code {
		return fmt.Errorf("projsync: 登记编号 %q 不是归一化形式（应无空白、大写）", u.Code)
	}
	if _, ok := PFUnitKeyProjectCode(u.Code); !ok {
		return fmt.Errorf("projsync: 登记编号 %q 认不出是项目编号（也不是 编号+R+流水号 的单元键）", u.Code)
	}
	if !u.State.valid() {
		return fmt.Errorf("projsync: 编号 %s 的状态 %q 不是 syncing/starting/stopping/stopped 之一", u.Code, u.State)
	}
	if err := validRel(u.Rel); err != nil {
		return fmt.Errorf("projsync: 编号 %s 的 rel 不合法: %w", u.Code, err)
	}
	if !utf8.ValidString(u.Rel) {
		return fmt.Errorf("projsync: 编号 %s 的 rel %q 不是合法 UTF-8", u.Code, u.Rel)
	}
	segs := strings.Split(u.Rel, "/")
	if len(segs) > PFUnitMaxDepth {
		return fmt.Errorf("projsync: 编号 %s 的 rel %q 超过 %d 层", u.Code, u.Rel, PFUnitMaxDepth)
	}
	for i, seg := range segs {
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("projsync: 编号 %s 的 rel %q 含「.」开头的段 %q（不同步）", u.Code, u.Rel, seg)
		}
		if i < len(segs)-1 {
			if c, ok := PFCodeFromName(seg); ok {
				return fmt.Errorf("projsync: 编号 %s 的 rel %q 的上级目录 %q 已能认出编号 %s，与单元发现规则不一致", u.Code, u.Rel, seg, c)
			}
		}
	}
	relCode, ok := PFCodeFromName(segs[len(segs)-1])
	if !ok {
		return fmt.Errorf("projsync: rel %q 最后一段认不出项目编号", u.Rel)
	}
	if !pfUnitKeyMatchesRel(u.Code, relCode) {
		return fmt.Errorf("projsync: 编号 %s 与 rel %q 最后一段认出的编号 %q 不一致（也不是 %sR+流水号）", u.Code, u.Rel, relCode, relCode)
	}
	if u.ProjectCode != "" {
		if !pfIsASCII(u.ProjectCode) {
			return fmt.Errorf("projsync: 登记 %s 的 project_code %q 含非 ASCII 字符", u.Code, u.ProjectCode)
		}
		if PFNormCode(u.ProjectCode) != u.ProjectCode {
			return fmt.Errorf("projsync: 登记 %s 的 project_code %q 不是归一化形式（应无空白、大写）", u.Code, u.ProjectCode)
		}
		if u.ProjectCode != relCode {
			return fmt.Errorf("projsync: 登记 %s 的 project_code %q 与 rel %q 最后一段认出的编号 %q 不一致", u.Code, u.ProjectCode, u.Rel, relCode)
		}
	}
	if u.State != PFUnitStopped || u.LinkPath != "" {
		if err := pfCheckAbsPath("link_path", u.Code, u.LinkPath, u.Rel); err != nil {
			return err
		}
	}
	if u.State != PFUnitStopped || u.SyncPath != "" {
		if err := pfCheckAbsPath("sync_path", u.Code, u.SyncPath, u.Rel); err != nil {
			return err
		}
	}
	if u.LinkPath != "" && u.LinkPath == u.SyncPath {
		return fmt.Errorf("projsync: 编号 %s 的 link_path 与 sync_path 相同 %q", u.Code, u.LinkPath)
	}
	if u.StartedAt.IsZero() {
		return fmt.Errorf("projsync: 编号 %s 缺少 started_at", u.Code)
	}
	return nil
}

func pfCheckAbsPath(field, code, p, rel string) error {
	switch {
	case p == "":
		return fmt.Errorf("projsync: 编号 %s 的 %s 不能为空", code, field)
	case !utf8.ValidString(p) || strings.ContainsRune(p, 0):
		return fmt.Errorf("projsync: 编号 %s 的 %s %q 不是合法路径", code, field, p)
	case !filepath.IsAbs(p):
		return fmt.Errorf("projsync: 编号 %s 的 %s %q 必须是绝对路径", code, field, p)
	case filepath.Clean(p) != p:
		return fmt.Errorf("projsync: 编号 %s 的 %s %q 不是干净路径（含 ..、多余的 / 或 .）", code, field, p)
	case !strings.HasSuffix(filepath.ToSlash(p), "/"+rel):
		return fmt.Errorf("projsync: 编号 %s 的 %s %q 没有以 rel %q 结尾", code, field, p, rel)
	}
	return nil
}

// PFValidateUnits 校验整张登记表：每条合格；编号（单元键）不重复；rel 不重复。
//
// 不另查「单元互相包含」：单条校验已要求 rel 的上级目录名认不出编号，一个单元在另一个单元里面必然单条不合格。
// rel 要单独查重：同编号可以有多个单元（键不同、rel 不同），一个 rel 只能归一个单元，
// 两条登记挂同一个目录会造成一个目录被两套状态同步两次。
func PFValidateUnits(us []PFUnitRecord) error {
	codes := map[string]int{}
	rels := map[string]int{}
	for i, u := range us {
		if err := PFValidateUnit(u); err != nil {
			return fmt.Errorf("第 %d 条: %w", i+1, err)
		}
		if j, ok := codes[u.Code]; ok {
			return fmt.Errorf("projsync: 编号 %s 在登记表里出现了两次（第 %d、%d 条）", u.Code, j+1, i+1)
		}
		codes[u.Code] = i
		if j, ok := rels[u.Rel]; ok {
			return fmt.Errorf("projsync: rel %q 在登记表里出现了两次（第 %d、%d 条）", u.Rel, j+1, i+1)
		}
		rels[u.Rel] = i
	}
	return nil
}

// PFParseUnits 解析 units.json 的内容：顶层必须是数组，不认识的字段、结尾多余内容都报错，
// 解析后整张表过 PFValidateUnits。坏数据返回 error，不静默修、不丢条目。
func PFParseUnits(data []byte) ([]PFUnitRecord, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("projsync: units.json 是空的")
	}
	if trimmed[0] != '[' {
		return nil, errors.New("projsync: units.json 顶层必须是数组")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var us []PFUnitRecord
	if err := dec.Decode(&us); err != nil {
		return nil, fmt.Errorf("projsync: units.json 不合法: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("projsync: units.json 后面还有多余内容")
	}
	if us == nil {
		us = []PFUnitRecord{}
	}
	if err := PFValidateUnits(us); err != nil {
		return nil, fmt.Errorf("projsync: units.json 校验不通过: %w", err)
	}
	return us, nil
}

// PFMarshalUnits 把登记表序列化成 units.json 的内容（两格缩进、末尾换行，保持传入顺序）。
// 先过 PFValidateUnits：不合格的表不许序列化，免得把坏数据写进盘。
func PFMarshalUnits(us []PFUnitRecord) ([]byte, error) {
	if err := PFValidateUnits(us); err != nil {
		return nil, fmt.Errorf("projsync: 登记表校验不通过，不序列化: %w", err)
	}
	if us == nil {
		us = []PFUnitRecord{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(us); err != nil { // Encode 末尾自带换行
		return nil, fmt.Errorf("projsync: 序列化登记表失败: %w", err)
	}
	return buf.Bytes(), nil
}

// PFFindUnit 按编号找登记项（编号按归一化比）。
func PFFindUnit(us []PFUnitRecord, code string) (PFUnitRecord, bool) {
	code = PFNormCode(code)
	for _, u := range us {
		if u.Code == code {
			return u, true
		}
	}
	return PFUnitRecord{}, false
}

// PFUpsertUnit 返回一张新表：同编号的替换，没有的追加；整张表过 PFValidateUnits，不合格返回 error，
// 原表不改。
func PFUpsertUnit(us []PFUnitRecord, rec PFUnitRecord) ([]PFUnitRecord, error) {
	out := make([]PFUnitRecord, 0, len(us)+1)
	replaced := false
	for _, u := range us {
		if u.Code == rec.Code {
			out = append(out, rec)
			replaced = true
		} else {
			out = append(out, u)
		}
	}
	if !replaced {
		out = append(out, rec)
	}
	if err := PFValidateUnits(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ============================================================================
// 四、新单元判定（§12.2）与 Mac 工作目录检查
// ============================================================================

// PFUnitMatch 是登记项与 Ubuntu 上按 rel 配上的目录组成的一对（§14.4：对应关系按 rel，不再按编号）。
type PFUnitMatch struct {
	Reg   PFUnitRecord
	Found PFFoundUnit
}

// PFUnitClassification 是按登记表对 Ubuntu 发现结果分出的几类，每类按编号（或路径）排序。
type PFUnitClassification struct {
	Syncing      []PFUnitMatch  // 登记 syncing、Ubuntu 上有 rel 相同的目录：正常参加一轮
	Transitional []PFUnitMatch  // 登记 starting/stopping、Ubuntu 上有 rel 相同的目录：正在开始/取消同步，本轮不处理
	Stopped      []PFUnitMatch  // 登记 stopped、Ubuntu 上有 rel 相同的目录：不处理，状态标 PFNoteStopped
	New          []PFFoundUnit  // 编号完全没登记过：Ubuntu 新项目（PFNoteNewOnUbuntu），先用 PFCheckWorkDirForUnit 看 Mac 工作目录
	Duplicates   []PFFoundUnit  // 编号没登记过、Ubuntu 上同编号却有多个目录：不知道哪个是对的，都标 PFNoteDuplicate，停下
	Unconfigured []PFFoundUnit  // 同编号有登记单元、但 rel 不是任何登记单元的目录（§14.2）：只报告 PFNoteUnconfigured，不同步、不停
	Missing      []PFUnitRecord // 登记 syncing 但 Ubuntu 上没有 rel 相同的目录（可能还没传过去，也可能是不见了，由 base 判断，别直接当删除）
	Incomplete   bool           // 发现不完整（见 PFDiscoverResult.Incomplete）：Missing 不可信
}

// PFClassifyUnits 把 Ubuntu 侧发现的结果和 Mac 登记表对上（§14.4）：
//   - 按 rel 配对：登记单元配 Ubuntu 上 Rel 相同的目录（同编号两个登记单元时各配各的）；
//   - rel 配不上：编号（发现结果的 Code）是某个登记单元的编号 → Unconfigured（没配置的目录，只报告，
//     不再让整个项目停下——取代原来的 G4 重复即停）；编号完全没登记过 → 按编号看多个目录：
//     多个 → Duplicates（不知道哪个对，停下），一个 → New。
//
// 登记表不合格返回 error。重复与否按 found.Units 自己重算，不依赖 Dup 字段。
func PFClassifyUnits(found PFDiscoverResult, reg []PFUnitRecord) (PFUnitClassification, error) {
	var out PFUnitClassification
	if err := PFValidateUnits(reg); err != nil {
		return out, err
	}
	out.Incomplete = found.Incomplete
	regByRel := make(map[string]PFUnitRecord, len(reg))
	regProj := make(map[string]bool, len(reg))
	for _, u := range reg {
		regByRel[u.Rel] = u
		regProj[u.PFUnitProjectCode()] = true
	}
	foundRels := make(map[string]bool, len(found.Units))
	unregByCode := map[string][]PFFoundUnit{} // 没按 rel 配上、编号也没登记过的目录（按编号查重复）
	for _, f := range found.Units {
		foundRels[f.Rel] = true
		if r, ok := regByRel[f.Rel]; ok {
			switch r.State {
			case PFUnitSyncing:
				out.Syncing = append(out.Syncing, PFUnitMatch{Reg: r, Found: f})
			case PFUnitStopped:
				out.Stopped = append(out.Stopped, PFUnitMatch{Reg: r, Found: f})
			default: // starting / stopping
				out.Transitional = append(out.Transitional, PFUnitMatch{Reg: r, Found: f})
			}
			continue
		}
		if regProj[f.Code] {
			out.Unconfigured = append(out.Unconfigured, f)
			continue
		}
		unregByCode[f.Code] = append(unregByCode[f.Code], f)
	}
	for _, group := range unregByCode {
		if len(group) < 2 {
			out.New = append(out.New, group[0])
			continue
		}
		for i, f := range group {
			f.Dup = true
			f.DupRels = nil
			for j, g := range group {
				if j != i {
					f.DupRels = append(f.DupRels, g.Rel)
				}
			}
			sort.Strings(f.DupRels)
			out.Duplicates = append(out.Duplicates, f)
		}
	}
	for _, r := range reg {
		if r.State == PFUnitSyncing && !foundRels[r.Rel] {
			out.Missing = append(out.Missing, r)
		}
	}
	byCode := func(a, b PFUnitMatch) bool { return a.Reg.Code < b.Reg.Code }
	sort.Slice(out.Syncing, func(i, j int) bool { return byCode(out.Syncing[i], out.Syncing[j]) })
	sort.Slice(out.Transitional, func(i, j int) bool { return byCode(out.Transitional[i], out.Transitional[j]) })
	sort.Slice(out.Stopped, func(i, j int) bool { return byCode(out.Stopped[i], out.Stopped[j]) })
	sort.Slice(out.New, func(i, j int) bool { return out.New[i].Rel < out.New[j].Rel })
	sort.Slice(out.Duplicates, func(i, j int) bool {
		a, b := out.Duplicates[i], out.Duplicates[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Rel < b.Rel
	})
	sort.Slice(out.Unconfigured, func(i, j int) bool {
		a, b := out.Unconfigured[i], out.Unconfigured[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Rel < b.Rel
	})
	sort.Slice(out.Missing, func(i, j int) bool { return out.Missing[i].Code < out.Missing[j].Code })
	return out, nil
}

// PFWorkSlot 是 Mac 工作目录（桌面 Projects）里某个新单元对应位置的现状。
type PFWorkSlot string

const (
	PFSlotFree     PFWorkSlot = "free"     // 没有同编号的目录，位置空着：可以自动在同步目录建好、桌面放链接
	PFSlotRealDir  PFWorkSlot = "real_dir" // 已有同编号的真实目录（未同步）：不自动建，提示 PFNoteStartOnMacFirst
	PFSlotOccupied PFWorkSlot = "occupied" // 位置被占（符号链接或文件）：不自动建，提示 PFNoteSlotOccupied
)

// PFWorkDirFinding 是 PFCheckWorkDirForUnit 的结果；Rel 是工作目录里那个目录（或占位物）的相对路径，Free 时为空。
type PFWorkDirFinding struct {
	Slot PFWorkSlot
	Rel  string
}

// PFCheckWorkDirForUnit 看 Ubuntu 上的新单元 found 在 Mac 工作目录 workDir 里是否已有同编号的真实目录：
//  1. 按发现规则扫 workDir（最多 5 层、不跟随符号链接）找同编号的真实目录，任何名字都算；
//     名字认出同编号的符号链接记为位置被占；
//  2. 再 Lstat workDir/found.Rel：是真实目录 → real_dir；是链接、文件等其他东西 → occupied。
//
// 读不了的子目录、Lstat 出现「不存在」以外的错误，都返回 error——无法确认就不能自动建，由调用方停下报告。
// ops 里的函数为 nil 时用 os 的实现。
func PFCheckWorkDirForUnit(workDir string, found PFFoundUnit, ops PFDirOps) (PFWorkDirFinding, error) {
	if !filepath.IsAbs(workDir) {
		return PFWorkDirFinding{}, fmt.Errorf("projsync: 工作目录 %q 必须是绝对路径", workDir)
	}
	res, err := pfDiscover(workDir, PFUnitMaxDepth, ops.readDir)
	if err != nil {
		return PFWorkDirFinding{}, err
	}
	for _, s := range res.Skipped {
		if s.Unreadable {
			return PFWorkDirFinding{}, fmt.Errorf("projsync: 工作目录里 %s 读不了（%s），无法确认有没有同编号 %s 的目录", s.Rel, s.Reason, found.Code)
		}
	}
	var realRel, occRel string
	for _, u := range res.Units {
		if u.Code == found.Code && (realRel == "" || u.Rel == found.Rel) {
			realRel = u.Rel
		}
	}
	for _, s := range res.Skipped {
		if s.Reason != pfReasonSymlink {
			continue
		}
		name := s.Rel[strings.LastIndex(s.Rel, "/")+1:]
		if c, ok := PFCodeFromName(name); ok && c == found.Code && occRel == "" {
			occRel = s.Rel
		}
	}
	fi, err := ops.lstat(filepath.Join(workDir, filepath.FromSlash(found.Rel)))
	switch {
	case err == nil:
		if fi.Mode()&fs.ModeSymlink == 0 && fi.IsDir() {
			return PFWorkDirFinding{Slot: PFSlotRealDir, Rel: found.Rel}, nil
		}
		return PFWorkDirFinding{Slot: PFSlotOccupied, Rel: found.Rel}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return PFWorkDirFinding{}, fmt.Errorf("projsync: 查看工作目录里的 %s 失败: %w", found.Rel, err)
	}
	if realRel != "" {
		return PFWorkDirFinding{Slot: PFSlotRealDir, Rel: realRel}, nil
	}
	if occRel != "" {
		return PFWorkDirFinding{Slot: PFSlotOccupied, Rel: occRel}, nil
	}
	return PFWorkDirFinding{Slot: PFSlotFree}, nil
}
