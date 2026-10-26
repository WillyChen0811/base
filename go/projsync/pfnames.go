package projsync

// 项目文件同步的「文件名规则与排除清单」（技术设计 §4.1、§4.2 第 1–4、6 条、§3.4 冲突副本命名、§7 历史版本内部文件）。
//
// 【本文件只放纯函数】不碰文件系统、不读时钟、不联网；输入是名字（字符串）和少量上下文，输出是判断结果。
// 因此 macOS（APFS 不区分大小写、对 NFC/NFD 不敏感）上也能完整测试。
// 扫描、判定、写入由后续任务（pfscan.go、pfdecide.go 等）调用这里的函数。
//
// 【不用 scope.go 里的 isSafeRune / nameNFCProblem】它们是给 Git 分支名用的「窄白名单」
// （只放 ASCII 和汉字），会把全角标点（（）【】、，）、①、Ⅱ、emoji、日文假名全判成超出范围。
// 这里改用 golang.org/x/text：真正的 NFC（unicode/norm）和 Unicode 大小写折叠（cases.Fold）。
//
// 【边界】
//   - 折叠键用的是「完整」Unicode 大小写折叠（ß 与 ss 同键），比 APFS 的折叠更宽：
//     宁可把可能撞名的判成撞名（跳过并报告），也不把真会撞名的放过。
//   - 「排除」先于「超出范围」：调用方先用 PFExclude，排除的名字不再做其它检查
//     （如 Icon\r 含控制字符，但它是排除项，只计数、不报错）。
//   - 本文件不判断符号链接、FIFO、socket、设备文件、权限、iCloud 占位的 dataless 标志（§4.2 第 5、7 条），
//     那些要 lstat，归扫描层。

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	// pfMaxSegBytes 单段名字上限（ext4 的 NAME_MAX；APFS 允许 255 个字符，CJK 约 765 字节，按较严的 ext4 算）。
	pfMaxSegBytes = 255
	// pfMaxPathBytes 整条相对路径（用 / 连接）上限。
	pfMaxPathBytes = 1000
	// pfHistoryDir 历史版本目录名（§7）。
	pfHistoryDir = "历史版本"
)

// pfPackageExts Mac 包目录的扩展名（含点，小写）。只对「目录」生效：
// .key/.pages/.numbers 新版是单个文件（zip），那是普通文件，照常同步；包目录是一棵目录树。
var pfPackageExts = []string{".app", ".pages", ".numbers", ".key", ".rtfd", ".photoslibrary", ".bundle", ".fcpbundle", ".imovielibrary"}

// ---------------------------------------------------------------------------------------------
// 超出范围的错误类型
// ---------------------------------------------------------------------------------------------

// PFOutOfScope 表示某个路径超出自动同步范围：该路径（目录则含子树）本轮跳过并报告，其它照常。
// 对应 scope.go 的 ErrOutOfScope，但粒度是「文件」而不是「仓库」。
type PFOutOfScope struct {
	Path   string // 相对路径（用 / 连接）；名字可能含非法 UTF-8 或控制字符，显示时用 %q
	Reason string // 中文原因
}

func (e *PFOutOfScope) Error() string {
	return fmt.Sprintf("超出同步范围：%q：%s", e.Path, e.Reason)
}

// ---------------------------------------------------------------------------------------------
// §4.1 排除：不同步、不报错，只计数
// ---------------------------------------------------------------------------------------------

// PFExclude 判断路径里的一个名字是否「排除」（§4.1）：排除的不同步、不报错，扫描层只按原因计数。
//   - name：这一段的名字（不含 /）。
//   - isDir：它是不是目录。以 . 开头的目录和文件都排除；其余名字规则（~$、*.tmp 等）只对非目录生效——
//     一个叫「备份.tmp」的目录里可能有用户资料，静默不同步会丢数据，所以按普通目录处理。
//   - parentRel：它所在目录的相对路径（用 /；根目录传 ""）。路径里有一段等于「历史版本」时，
//     才启用历史版本内部文件的规则（§7）；名字本身等于「历史版本」的目录不算在内（它的内容才算）。
//
// 返回排除原因（中文，同一类原因文字固定，方便计数）和是否排除。
func PFExclude(name string, isDir bool, parentRel string) (reason string, excluded bool) {
	// 历史版本内部文件先判，原因更具体（.lock、.index-* 本来也以 . 开头）。
	if !isDir && pfInHistoryDir(parentRel) {
		switch {
		case name == ".lock":
			return "历史版本内部文件（.lock）", true
		case strings.HasPrefix(name, ".index-"):
			return "历史版本内部文件（.index-*）", true
		case strings.HasSuffix(name, ".sqlite3-journal"):
			return "历史版本内部文件（*.sqlite3-journal）", true
		case strings.HasSuffix(name, "-wal"):
			return "历史版本内部文件（*-wal）", true
		case strings.HasSuffix(name, "-shm"):
			return "历史版本内部文件（*-shm）", true
		}
	}
	// . 开头：文件和目录都排除（.DS_Store、._*、.~lock.*#、.Spotlight-V100、.Trashes、.fseventsd、
	// .TemporaryItems、.X.icloud、.xxyi-*、.projsync-tmp-*，与 websubmit 人工传档、入件口径一致）。
	if strings.HasPrefix(name, ".") {
		if _, ok := PFICloudPlaceholder(name); ok && !isDir {
			return "iCloud 占位文件（.X.icloud）", true
		}
		return "以 . 开头的隐藏文件或目录", true
	}
	if isDir {
		return "", false
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(name, "~$"):
		return "Office 锁文件（~$）", true
	case strings.HasPrefix(lower, "~wrl") && strings.HasSuffix(lower, ".tmp"):
		return "Word 临时文件（~WRL*.tmp）", true
	case lower == "thumbs.db":
		return "Windows 缩略图缓存（Thumbs.db）", true
	case lower == "desktop.ini":
		return "Windows 文件夹设置（desktop.ini）", true
	case name == "Icon\r":
		return "Mac 文件夹图标（Icon\\r）", true
	case strings.HasSuffix(lower, ".part"):
		return "未下载完的临时文件（*.part）", true
	case strings.HasSuffix(lower, ".tmp"):
		return "临时文件（*.tmp）", true
	case strings.HasSuffix(lower, ".crdownload"):
		return "未下载完的临时文件（*.crdownload）", true
	case strings.HasSuffix(lower, ".download"):
		return "未下载完的临时文件（*.download）", true
	}
	return "", false
}

// pfInHistoryDir 报告 parentRel 的某一段是否正好是「历史版本」。
func pfInHistoryDir(parentRel string) bool {
	if parentRel == "" {
		return false
	}
	for _, seg := range strings.Split(parentRel, "/") {
		if seg == pfHistoryDir {
			return true
		}
	}
	return false
}

// PFICloudPlaceholder 识别老系统的 iCloud 占位文件 `.X.icloud`，取出它代表的文件名 X。
// 占位文件本身是排除项（以 . 开头），但扫描层要知道「X 在 iCloud 里、没下载」（§4.3：X 当「不明」）。
// 不是占位文件（含 X 为空，如 `..icloud`）返回 ("", false)。
func PFICloudPlaceholder(name string) (orig string, ok bool) {
	const suffix = ".icloud"
	if len(name) <= len(".")+len(suffix) || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, suffix) {
		return "", false
	}
	return name[1 : len(name)-len(suffix)], true
}

// ---------------------------------------------------------------------------------------------
// §4.2 第 1、2、6 条：超出范围的名字检查
// ---------------------------------------------------------------------------------------------

// pfNameProblem 检查单个名字（路径的一段），返回中文原因，"" 表示没问题。
// isDir 只用于 Mac 包目录扩展名（§4.2 第 6 条）。
func pfNameProblem(seg string, isDir bool) string {
	if !utf8.ValidString(seg) {
		return "名字不是合法 UTF-8"
	}
	for _, r := range seg {
		if r < 0x20 || r == 0x7f { // U+0000–001F、U+007F；其它（全角标点、①、Ⅱ、emoji、假名、C1 区）不算
			return fmt.Sprintf("名字含控制字符 U+%04X", r)
		}
	}
	if len(seg) > pfMaxSegBytes {
		return fmt.Sprintf("名字超过 %d 字节（实际 %d 字节）", pfMaxSegBytes, len(seg))
	}
	if isDir {
		ext := strings.ToLower(pfExt(seg))
		for _, p := range pfPackageExts {
			if ext == p {
				return fmt.Sprintf("Mac 包目录（%s），整个跳过", p)
			}
		}
	}
	return ""
}

// PFCheckName 检查单个名字（不含 /）：合法 UTF-8、无控制字符、≤255 字节、不是 Mac 包目录。
// 返回 nil 表示在范围内；否则返回的 Path 就是 name 本身。调用方应先用 PFExclude 排除，排除的名字不必检查。
func PFCheckName(name string, isDir bool) *PFOutOfScope {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return &PFOutOfScope{Path: name, Reason: "名字为空、是 . 或 ..，或含 /"}
	}
	if r := pfNameProblem(name, isDir); r != "" {
		return &PFOutOfScope{Path: name, Reason: r}
	}
	return nil
}

// PFCheckPath 检查整条相对路径（用 / 连接，isDir 指最后一段是不是目录）：
// 路径结构合法（对应 validRel：非空、不以 / 开头、无反斜杠、无空段和 . / .. 段），
// 每一段通过 PFCheckName（中间各段按目录算，所以路径穿过 Foo.app 也算超出），整条路径 ≤1000 字节。
// 返回 nil 表示在范围内；否则返回第一个问题，Path 是传入的整条路径。
//
// 反斜杠在 Mac 和 Linux 上是合法文件名字符，但 validRel 不接受含它的路径，
// 后面所有按 validRel 校验的代码会报错；这里先判超出范围，「跳过并报告」比「整轮报错」好。
func PFCheckPath(rel string, isDir bool) *PFOutOfScope {
	oos := func(format string, a ...any) *PFOutOfScope {
		return &PFOutOfScope{Path: rel, Reason: fmt.Sprintf(format, a...)}
	}
	if rel == "" {
		return oos("路径为空")
	}
	if strings.HasPrefix(rel, "/") {
		return oos("路径不能以 / 开头")
	}
	if strings.ContainsRune(rel, '\\') {
		return oos("路径含反斜杠")
	}
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return oos("路径含非法段 %q", seg)
		}
		segIsDir := i < len(segs)-1 || isDir
		if r := pfNameProblem(seg, segIsDir); r != "" {
			if len(segs) > 1 {
				return oos("%s：%s", pfSegLabel(seg), r)
			}
			return oos("%s", r)
		}
	}
	if len(rel) > pfMaxPathBytes {
		return oos("整条路径超过 %d 字节（实际 %d 字节）", pfMaxPathBytes, len(rel))
	}
	return nil
}

// pfSegLabel 给原因里指明是哪一段；段本身可能含非法字节，用 %q 转义。
func pfSegLabel(seg string) string { return "段 " + strconv.Quote(seg) }

// pfExt 返回名字的最后一个扩展名（含点）。名字以点开头且只有这一个点（如 .bashrc）算没有扩展名；
// 以点结尾（「以点结尾.」）时扩展名是单独的 "."。
func pfExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 {
		return ""
	}
	return name[i:]
}

// ---------------------------------------------------------------------------------------------
// 折叠键、撞名（§4.2 第 3、4 条）
// ---------------------------------------------------------------------------------------------

// PFFoldKey 返回名字的折叠键：NFC(名字) 再做 Unicode 大小写折叠（cases.Fold），最后再规范成 NFC
// （折叠后个别字符不再是 NFC）。折叠键相同、字节不同 = 在不区分大小写或不区分 NFC/NFD 的文件系统上会是同一个名字。
// 名字不是合法 UTF-8 时返回带前缀的原字节：这种名字本来就判超出范围，也不会与别的名字撞键。
// 键只用来比较，不要拿它当文件名用。
func PFFoldKey(name string) string {
	if !utf8.ValidString(name) {
		return "\x00非法UTF-8\x00" + name
	}
	f := cases.Fold() // 带状态，不能跨 goroutine 复用，每次新建
	return norm.NFC.String(f.String(norm.NFC.String(name)))
}

// PFSameSideClashes 在同一目录的一组名字里找「折叠键相同、字节不同」的组（§4.2 第 3 条）：
// 如 Ubuntu 上的 Report.docx 与 report.docx，或 café 的 NFC 与 NFD。组内每个名字都判超出范围（两个都跳过）。
// 目录和文件的名字放在一起比（同一目录里 Report 目录与 report 文件也撞）。
// 重复的完全相同的名字只算一个；返回的每组按字节序排序，组与组按第一个名字排序；没有撞名返回 nil。
// 调用方传入的应是已排除之外的名字（排除项不同步，不会撞）。
func PFSameSideClashes(names []string) [][]string {
	groups := map[string]map[string]bool{}
	for _, n := range names {
		k := PFFoldKey(n)
		if groups[k] == nil {
			groups[k] = map[string]bool{}
		}
		groups[k][n] = true
	}
	var out [][]string
	for _, set := range groups {
		if len(set) < 2 {
			continue
		}
		g := make([]string, 0, len(set))
		for n := range set {
			g = append(g, n)
		}
		sort.Strings(g)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// PFCrossClash 是「两边对不上」的一对：Mac 的名字和 Ubuntu 的名字折叠键相同、字节不同。
type PFCrossClash struct {
	Mac    string
	Ubuntu string
}

// PFCrossSideClashes 给 Mac 和 Ubuntu 同一目录的名字集合，找折叠键相同但字节不同的对（§4.2 第 4 条）：
// 含只差大小写（Mac Report.docx / Ubuntu report.docx）、一边 NFC 一边 NFD；
// 名字是目录时同样算（目录段只差大小写 → 调用方跳过这个目录的整棵子树）。
// 两边字节完全相同的名字不算；一个键下一边有多个名字时，所有字节不同的 (Mac, Ubuntu) 组合都列出
// （其中可能有与另一边字节相同的名字，它们同时是「同一边撞名」，由 PFSameSideClashes 报告，调用方取并集即可）。
// 结果按 (Mac, Ubuntu) 字节序排序，没有返回 nil。调用方把每对里的两个名字都判超出范围。
func PFCrossSideClashes(mac, ubuntu []string) []PFCrossClash {
	byKey := func(names []string) map[string]map[string]bool {
		m := map[string]map[string]bool{}
		for _, n := range names {
			k := PFFoldKey(n)
			if m[k] == nil {
				m[k] = map[string]bool{}
			}
			m[k][n] = true
		}
		return m
	}
	mk, uk := byKey(mac), byKey(ubuntu)
	var out []PFCrossClash
	for k, ms := range mk {
		us, ok := uk[k]
		if !ok {
			continue
		}
		for m := range ms {
			for u := range us {
				if m != u {
					out = append(out, PFCrossClash{Mac: m, Ubuntu: u})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mac != out[j].Mac {
			return out[i].Mac < out[j].Mac
		}
		return out[i].Ubuntu < out[j].Ubuntu
	})
	return out
}

// PFNameSet 是一组名字，按折叠键判断「有没有」：给 PFConflictName 的 exists 用，
// 目标边是不区分大小写或不区分 NFC/NFD 的文件系统时，近似名字也算已存在。
type PFNameSet struct{ keys map[string]bool }

// NewPFNameSet 用一组名字建集合。
func NewPFNameSet(names []string) *PFNameSet {
	s := &PFNameSet{keys: make(map[string]bool, len(names))}
	for _, n := range names {
		s.keys[PFFoldKey(n)] = true
	}
	return s
}

// Has 报告集合里有没有折叠键与 name 相同的名字。
func (s *PFNameSet) Has(name string) bool { return s.keys[PFFoldKey(name)] }

// ---------------------------------------------------------------------------------------------
// §4.2 第 9 条：Office 锁文件对应关系
// ---------------------------------------------------------------------------------------------

// PFOfficeLockCandidates 返回文档 x（文件名，不含路径）可能对应的锁文件名（~$ 的各种写法在前，LibreOffice 的在最后）。
//
// 【Word/Excel/PowerPoint 的 ~$ 锁（owner file）】名字是 "~$" + 文档名，开头的若干个字符被 "~$" 顶替：
//   - 微软 KB 313472 只给了例子：Document.doc 的 owner file 是 ~$cument.doc（8 个字符的主名去掉开头 2 个）；
//   - Nextcloud 论坛帖（help.nextcloud.com 「MS Office Lock-File (owner file) behavior differs between
//     Netdrive and Synced-Folders」）实测 Word：主名 ≤6 个字符 → 去掉 0 个（~$file12.docx）；
//     =7 个 → 去掉 1 个（~$ile123.docx）；≥8 个 → 去掉 2 个（~$le123456.docx）；
//     同帖说 Excel 无论多长都不去（~$file12345678.xlsx）。
//   - 任务说明写的是「≥7 个字符去掉 2 个」，与上面 7 个字符的实测不一致；PowerPoint 查不到可靠资料。
//     这套规则没有微软的正式文档，所以：主名（最后一个扩展名之前的部分）≤6 个字符只给 "~$"+x；
//     ≥7 个字符时 0、1、2 三种都当候选。宁可把「长得像锁」的当成锁（顶多多等一会儿，锁文件消失就放行），
//     也不把真锁漏掉（会覆盖 Office 正在编辑的文件）。
//   - 长度和去掉的都按【字符】（rune，按 NFC 形式）算，不按字节；BMP 之外的字符（emoji）在 Windows 上
//     占两个 UTF-16 码元，所以长度判断取 rune 数和 UTF-16 码元数中较大的一个。
//
// 【LibreOffice 的锁】".~lock.<x>#"，名字原样（来源：LibreOffice 锁文件格式，manifest 里的
// ".~lock.需求表.xlsx#" 即是）。
//
// 这些名字都在排除清单里（~$ 与 . 开头）：它们不同步，只作「正在编辑」的信号，所以扫描层要保留一份
// 「排除前」的名字列表交给 PFOfficeLocks。
func PFOfficeLockCandidates(x string) []string {
	nfc := norm.NFC.String(x)
	ext := pfExt(nfc)
	stem := nfc[:len(nfc)-len(ext)]
	runes := []rune(stem)
	n := len(runes)
	if u := pfUTF16Len(stem); u > n {
		n = u
	}
	ks := []int{0}
	if n >= 7 {
		ks = []int{0, 1, 2}
	}
	var out []string
	for _, k := range ks {
		if k >= len(runes) {
			continue
		}
		out = append(out, "~$"+string(runes[k:])+ext)
	}
	out = append(out, ".~lock."+x+"#")
	return out
}

// pfUTF16Len 返回字符串按 UTF-16 编码的码元数。
func pfUTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// PFOfficeLocks 在一个目录的名字列表 names（含排除项）里找文档 x 的锁文件，返回找到的锁文件名（按 names 顺序）。
// 比较用折叠键（Mac 和 Ubuntu 上大小写、NFC/NFD 可能不一致）。没有锁返回 nil。
// 锁文件超过 12 小时当残留（§4.2 第 9 条）要看 mtime，由调用方对返回的名字去判断。
func PFOfficeLocks(names []string, x string) []string {
	want := map[string]bool{}
	for _, c := range PFOfficeLockCandidates(x) {
		want[PFFoldKey(c)] = true
	}
	var out []string
	for _, n := range names {
		if want[PFFoldKey(n)] {
			out = append(out, n)
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// §3.4 冲突副本命名
// ---------------------------------------------------------------------------------------------

// pfConflictMaxTries 同名冲突副本最多试到 -N；超过说明 exists 有问题（恒真），不能无限循环。
const pfConflictMaxTries = 1000

// PFConflictName 生成冲突副本的文件名：<主名>（冲突-<机器>-<MMDD-HHMM>）<扩展名>（括号是全角）。
//   - orig：原文件名（不含路径）；machine：被改名那份所在的机器名（如 "Mac"、"Ubuntu"）；
//     t：被改名那份的时间，按 t 自身的时区格式化（调用方传本地时间）。
//   - exists：判断名字在目标目录里是否已存在；已存在就在括号里的时间后加 -2、-3……
//     （如「报告（冲突-Mac-1026-1530-2）.docx」）。调用方用 NewPFNameSet(目录名字).Has 就会按折叠键判断。
//   - 扩展名只取最后一个：「a.tar.gz」的扩展名是「.gz」，主名是「a.tar」，结果是「a.tar（冲突-…）.gz」。
//   - 整个名字超过 255 字节时，按字符（rune）从主名末尾截短，保留扩展名和冲突标记；
//     扩展名本身长到放不下标记时放弃保留扩展名（按整个名字截）。
//   - machine 里的 /、\、控制字符换成 _，最多取前 20 个字符；为空写「未知」。
//
// 返回的名字不会是 orig 本身；试了 1000 次都已存在返回错误。
func PFConflictName(orig, machine string, t time.Time, exists func(string) bool) (string, error) {
	if orig == "" || strings.ContainsRune(orig, '/') || !utf8.ValidString(orig) {
		return "", fmt.Errorf("projsync: 无法为 %q 生成冲突副本名（名字为空、含 / 或不是合法 UTF-8）", orig)
	}
	if exists == nil {
		exists = func(string) bool { return false }
	}
	ext := pfExt(orig)
	main := orig[:len(orig)-len(ext)]
	tag := "冲突-" + pfCleanMachine(machine) + "-" + t.Format("0102-1504")
	for i := 1; i <= pfConflictMaxTries; i++ {
		mark := tag
		if i > 1 {
			mark += "-" + strconv.Itoa(i)
		}
		name, err := pfBuildConflictName(main, mark, ext)
		if err != nil {
			return "", fmt.Errorf("projsync: 无法为 %q 生成冲突副本名：%w", orig, err)
		}
		if name != orig && !exists(name) {
			return name, nil
		}
	}
	return "", fmt.Errorf("projsync: 为 %q 试了 %d 个冲突副本名都已存在", orig, pfConflictMaxTries)
}

// pfBuildConflictName 拼 main + （mark） + ext，超 255 字节时截短 main（按字符，至少留 1 个字符）；
// 扩展名放不下时丢掉扩展名，把它并入主名一起截。
func pfBuildConflictName(main, mark, ext string) (string, error) {
	deco := "（" + mark + "）"
	if len(main)+len(deco)+len(ext) <= pfMaxSegBytes {
		return main + deco + ext, nil
	}
	if len(deco)+len(ext)+utf8.RuneLen('x') > pfMaxSegBytes {
		main, ext = main+ext, "" // 扩展名长得离谱，放弃保留
	}
	budget := pfMaxSegBytes - len(deco) - len(ext)
	if budget < 1 {
		return "", errors.New("冲突标记本身就超过 255 字节")
	}
	rs := []rune(main)
	cut := 0
	for i, r := range rs {
		n := utf8.RuneLen(r)
		if cut+n > budget {
			rs = rs[:i]
			break
		}
		cut += n
	}
	if len(rs) == 0 {
		return "", errors.New("截短后主名为空")
	}
	return string(rs) + deco + ext, nil
}

// pfCleanMachine 清理机器名：去掉路径分隔符和控制字符，最多 20 个字符，空则写「未知」。
func pfCleanMachine(m string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.ToValidUTF8(m, "_") {
		if n >= 20 {
			break
		}
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			r = '_'
		}
		b.WriteRune(r)
		n++
	}
	if b.Len() == 0 {
		return "未知"
	}
	return b.String()
}
