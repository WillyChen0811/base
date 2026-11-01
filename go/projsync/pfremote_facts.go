package projsync

// 项目文件同步 T8：projfiles-facts 服务端与线上编码（技术设计 §1.1 的 sshterm 输出上限、§2.4、§5.2 第 4 步、§4.2 第 12 条）。
//
// 对一个单元：整个单元扫一遍（stat，便宜）→ 用 Ubuntu 本机的哈希缓存 <meta>/hashcache/<键>.json 给本页的文件算哈希（带预算）
// → 返回扫描条目、被排除的名字（含 mtime）、单元目录 dev+ino、整个扫描的指纹。
//
// 【输出上限】sshterm 单次调用的 stdout 上限是 8MB，超了会报错（不会截断）。所以：
//   - 条目数超过 MaxItems（缺省 50000，§4.2 第 12 条）→ TooLarge，不返回内容，整个单元超出范围；
//   - 其余按字节分页：一页的目标大小 PageBytes（缺省 4MB），客户端带着 NextItem/NextName 游标继续要，直到 More=false。
//     每一页都是一次完整扫描（哈希有缓存、只算本页的文件），响应里的 Fingerprint 是整个扫描的指纹，
//     客户端核对几页指纹一致，并用收到的条目重算一遍指纹，才认这次采集是完整、一致的。
//   - 一条记录都放不下一页时也至少放一条，保证游标前进；整个响应超过 7MB 是协议层失败，绝不截断后假装完整。
//
// 【游标】用「已经返回的条目数/名字数」而不是路径：条目按路径字节序排序，名字不是合法 UTF-8 的条目在线上会换成
// 转义形式（\xNN）显示，按路径比较会乱序；单元在翻页过程中变了会被指纹对不上发现，所以用序号是安全的。
//
// 【哈希缓存】损坏就当没有缓存重算（§2.4），并在 Notes 里说明；新算的哈希和已经不在的文件的清理在第一页之后写回（writeFileAtomic）。
// 缓存只是加速：写失败不影响结果，只记进 Notes。
//
// 边界：本函数不判断任何同步规则，只报事实；读不了、没算完的文件按扫描层的规矩标「不明/超出范围」，不会被当作「没有」。

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ── 线上编码 ────────────────────────────────────────────

var pfrKindCode = map[PFKind]string{PFKindFile: "f", PFKindDir: "d", PFKindSymlink: "l", PFKindOther: "o"}

func pfrKindFromCode(c string) (PFKind, bool) {
	for k, v := range pfrKindCode {
		if v == c {
			return k, true
		}
	}
	return "", false
}

// pfrEscapePath 把非 UTF-8 的路径换成可显示的形式：不合法的字节写成 \xNN。线上形式只用于显示和翻页核对，
// 这种路径对应的条目一定是超出范围（名字不是合法 UTF-8），Mac 不会用它去操作文件。
func pfrEscapePath(p string) (string, bool) {
	if utf8.ValidString(p) {
		return p, false
	}
	var b strings.Builder
	for i := 0; i < len(p); {
		r, n := utf8.DecodeRuneInString(p[i:])
		if r == utf8.RuneError && n == 1 {
			fmt.Fprintf(&b, `\x%02x`, p[i])
			i++
			continue
		}
		b.WriteString(p[i : i+n])
		i += n
	}
	return b.String(), true
}

// pfrToWire 把扫描条目编码成线上形式。dirDev 是单元目录的 dev，条目 dev 与它相同时省略。esc 累计转义过的路径数。
func pfrToWire(it PFScanItem, dirDev uint64, esc *int) PFRemoteItem {
	w := PFRemoteItem{Kind: pfrKindCode[it.Kind], HasStat: it.HasStat, Code: it.Code, Reason: it.Reason, Hash: it.Hash, FromCache: it.FromCache, Stub: it.Stub}
	var escaped bool
	w.Path, escaped = pfrEscapePath(it.Path)
	if escaped {
		*esc++
	}
	switch it.State {
	case PFStateOK:
	case PFStateUnknown:
		w.State = "u"
	case PFStateOutOfScope:
		w.State = "o"
	default:
		w.State = string(it.State) // 不认识的原样带过去，解码时报错
	}
	if it.HasStat {
		st := it.Stat
		if st.Kind != it.Kind {
			w.StatKind = pfrKindCode[st.Kind]
		}
		w.Size, w.MtimeNs, w.CtimeNs, w.Ino, w.Mode, w.Flags = st.Size, st.MtimeNs, st.CtimeNs, st.Ino, uint32(st.Mode), st.Flags
		if st.Dev != dirDev {
			d := st.Dev
			w.Dev = &d
		}
	}
	return w
}

// pfrFromWire 是 pfrToWire 的逆；字段不认识时报错（不猜）。
func pfrFromWire(w PFRemoteItem, dirDev uint64) (PFScanItem, error) {
	kind, ok := pfrKindFromCode(w.Kind)
	if !ok {
		return PFScanItem{}, fmt.Errorf("条目 %q 的种类 %q 不认识", w.Path, w.Kind)
	}
	it := PFScanItem{Path: w.Path, Kind: kind, HasStat: w.HasStat, Code: w.Code, Reason: w.Reason, Hash: w.Hash, FromCache: w.FromCache, Stub: w.Stub}
	switch w.State {
	case "":
		it.State = PFStateOK
	case "u":
		it.State = PFStateUnknown
	case "o":
		it.State = PFStateOutOfScope
	default:
		return PFScanItem{}, fmt.Errorf("条目 %q 的状态 %q 不认识", w.Path, w.State)
	}
	if w.HasStat {
		sk := kind
		if w.StatKind != "" {
			if sk, ok = pfrKindFromCode(w.StatKind); !ok {
				return PFScanItem{}, fmt.Errorf("条目 %q 的 stat 种类 %q 不认识", w.Path, w.StatKind)
			}
		}
		it.Stat = PFStat{Kind: sk, Size: w.Size, MtimeNs: w.MtimeNs, CtimeNs: w.CtimeNs, Ino: w.Ino, Dev: dirDev, Mode: fs.FileMode(w.Mode), Flags: w.Flags}
		if w.Dev != nil {
			it.Stat.Dev = *w.Dev
		}
	} else if w.Size != 0 || w.MtimeNs != 0 || w.CtimeNs != 0 || w.Ino != 0 || w.Dev != nil || w.Mode != 0 || w.Flags != 0 || w.StatKind != "" {
		return PFScanItem{}, fmt.Errorf("条目 %q 没有 stat 却带着 stat 字段", w.Path)
	}
	if it.Hash != "" && !pfValidSHA(it.Hash) {
		return PFScanItem{}, fmt.Errorf("条目 %q 的哈希不合法", w.Path)
	}
	return it, nil
}

// ── 请求校验 ────────────────────────────────────────────

func pfrCheckFactsRequest(req *PFRemoteFactsRequest) error {
	if err := pfrCheckUnitKey(req.Code); err != nil {
		return err
	}
	if err := validRel(req.Rel); err != nil {
		return err
	}
	if req.QuietSeconds < 0 || req.QuietSeconds > 3600 {
		return fmt.Errorf("quiet_seconds %d 超出 0–3600", req.QuietSeconds)
	}
	if req.HashBudgetMs < 0 || time.Duration(req.HashBudgetMs)*time.Millisecond > pfrMaxHashBudget {
		return fmt.Errorf("hash_budget_ms %d 超出 0–%d", req.HashBudgetMs, pfrMaxHashBudget.Milliseconds())
	}
	if req.MaxItems < 0 || req.MaxItems > PFRemoteDefaultMaxItems {
		return fmt.Errorf("max_items %d 超出 0–%d（只能调小）", req.MaxItems, PFRemoteDefaultMaxItems)
	}
	if req.PageBytes != 0 && (req.PageBytes < pfrMinPageBytes || req.PageBytes > pfrMaxPageBytes) {
		return fmt.Errorf("page_bytes %d 超出 %d–%d", req.PageBytes, pfrMinPageBytes, pfrMaxPageBytes)
	}
	if req.AfterItem < 0 || req.AfterName < 0 {
		return fmt.Errorf("游标不能为负")
	}
	return nil
}

// ── facts ───────────────────────────────────────────────

func (s *PFRemoteServer) facts(ctx context.Context, req *PFRemoteFactsRequest) PFRemoteFactsResponse {
	resp := PFRemoteFactsResponse{Version: PFRemoteVersion, NowNs: s.now().UnixNano(), Items: []PFRemoteItem{}, Names: []PFRawName{}}
	fail := func(f string, a ...any) PFRemoteFactsResponse {
		resp.Error = fmt.Sprintf(f, a...)
		return resp
	}
	env, err := s.openEnv(req.Meta, req.Root)
	if err != nil {
		return fail("%v", err)
	}
	disc, err := PFDiscoverUnits(env.root)
	if err != nil {
		return fail("发现单元失败: %v", err)
	}
	resp.Incomplete = disc.Incomplete
	// §14.4：req.Code 是单元键（主目录=编号，另配目录=编号+R+流水号）；
	// Ubuntu 上按 rel 配对，比对编号时用键的前段（同编号多个目录已合法）。
	pcf, _ := PFUnitKeyProjectCode(req.Code)
	var unit *PFFoundUnit
	for i := range disc.Units {
		u := &disc.Units[i]
		switch {
		case u.Code != pcf:
		case u.Rel == req.Rel:
			unit = u
		default:
			resp.Found = append(resp.Found, u.Rel)
		}
	}
	if unit == nil {
		return resp // Exists=false
	}
	resp.Exists, resp.Dup, resp.DupRels = true, unit.Dup, unit.DupRels
	abs := env.unitAbs(unit.Rel)
	resp.UnitPath = abs
	id, err := s.dirID(abs)
	if err != nil {
		return fail("读单元目录失败: %v", err)
	}
	resp.DirID = id

	// 扫描（整个单元），顺便收集被排除的名字
	var names []PFRawName
	opts := s.scanOpts(req.QuietSeconds)
	statf := s.stat()
	opts.OnExcluded = func(rel string, isDir bool) {
		n := PFRawName{Path: rel, IsDir: isDir}
		if !isDir {
			if st, e := statf(env.unitAbs(unit.Rel + "/" + rel)); e == nil {
				n.MtimeNs = st.MtimeNs
			}
		}
		names = append(names, n)
	}
	budget := PFRemoteDefaultHashBudget
	if req.HashBudgetMs > 0 {
		budget = time.Duration(req.HashBudgetMs) * time.Millisecond
	}
	opts.HashBudget = budget
	res, err := PFScan(ctx, abs, opts)
	if err != nil {
		if ctx.Err() != nil {
			return fail("已取消")
		}
		return fail("扫描单元失败: %v", err)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Path < names[j].Path })
	resp.Fingerprint, resp.Summary = res.Fingerprint(), pfrSummary(res)
	resp.TotalItems, resp.TotalNames, resp.Excluded = len(res.Items), len(names), res.Excluded
	resp.ScannedAtNs = res.ScannedAt.UnixNano()

	maxItems := req.MaxItems
	if maxItems == 0 {
		maxItems = PFRemoteDefaultMaxItems
	}
	if len(res.Items) > maxItems || len(names) > maxItems {
		resp.TooLarge = true
		return resp
	}
	if req.AfterItem > len(res.Items) || req.AfterName > len(names) {
		return fail("翻页游标超出范围（单元在翻页期间变了）")
	}

	// 选本页：按路径把条目和名字归并起来，累计（估算的）字节数，到 PageBytes 为止，至少一条
	pageBytes := req.PageBytes
	if pageBytes == 0 {
		pageBytes = PFRemoteDefaultPageBytes
	}
	var esc int
	i, j, used := req.AfterItem, req.AfterName, 0
	for i < len(res.Items) || j < len(names) {
		var size int
		takeItem := j >= len(names) || (i < len(res.Items) && res.Items[i].Path < names[j].Path)
		if takeItem {
			w := pfrToWire(res.Items[i], id.Dev, new(int))
			size = pfrWireSize(w)
			if w.Kind == "f" && w.State == "" && w.Hash == "" {
				size += 70 // 哈希还没算：留出 "h":"<64 位>", 的位置
			}
		} else {
			size = pfrNameSize(names[j])
		}
		if used > 0 && used+size > pageBytes {
			break
		}
		used += size
		if takeItem {
			i++
		} else {
			j++
		}
	}
	pageItems := append([]PFScanItem(nil), res.Items[req.AfterItem:i]...)
	resp.Names = append(resp.Names, names[req.AfterName:j]...)
	if i < len(res.Items) || j < len(names) {
		resp.More, resp.NextItem, resp.NextName = true, i, j
	}

	// 哈希本页的文件
	cachePath := pfrJoin(env.meta, PFRemoteCacheName, req.Code+".json")
	cache, cnote := pfrLoadCache(cachePath)
	if cnote != "" {
		resp.Notes = append(resp.Notes, cnote)
	}
	sub := &PFScanResult{Root: abs, Items: pageItems}
	herr := PFHashItems(ctx, abs, sub, cache, opts)
	if herr != nil && ctx.Err() != nil {
		return fail("已取消")
	}
	pageItems = sub.Items
	hs := sub.HashStats
	resp.HashStats = PFRemoteHashStats{CacheHits: hs.CacheHits, Hashed: hs.Hashed, HashedBytes: hs.HashedBytes, NewUnknown: hs.NewUnknown, ElapsedMs: hs.Elapsed.Milliseconds()}
	pruned := 0
	if req.AfterItem == 0 && req.AfterName == 0 {
		pruned = cache.Prune(func(p string) bool {
			if _, ok := res.Find(p); ok {
				return true
			}
			_, skipped := res.SubtreeSkipped(p)
			return skipped
		})
	}
	if hs.Hashed > 0 || pruned > 0 || cnote != "" {
		if b, merr := cache.Marshal(); merr != nil {
			resp.Notes = append(resp.Notes, "哈希缓存编码失败，没有写回: "+merr.Error())
		} else if werr := writeFileAtomic(cachePath, b, 0o600); werr != nil {
			resp.Notes = append(resp.Notes, "哈希缓存写回失败（不影响本次结果）: "+werr.Error())
		}
	}

	for _, it := range pageItems {
		resp.Items = append(resp.Items, pfrToWire(it, id.Dev, &esc))
	}
	for k := range resp.Names {
		if p, e := pfrEscapePath(resp.Names[k].Path); e {
			resp.Names[k].Path = p
			esc++
		}
	}
	resp.Escaped = esc
	return resp
}

// pfrLoadCache 读哈希缓存；不存在 = 空缓存；损坏 = 空缓存加一条说明（调用方会写回新的）。
func pfrLoadCache(p string) (*PFHashCache, string) {
	b, err := pfrReadSmall(p, 256<<20)
	if err != nil {
		if pfrIsNotExist(err) {
			return NewPFHashCache(), ""
		}
		return NewPFHashCache(), "哈希缓存读不了，已当作没有、全部重算: " + err.Error()
	}
	c, err := PFParseHashCache(b)
	if err != nil {
		return NewPFHashCache(), "哈希缓存损坏，已当作没有、全部重算: " + err.Error()
	}
	return c, ""
}

func pfrWireSize(w PFRemoteItem) int {
	b, _ := json.Marshal(w)
	return len(b) + 1
}

func pfrNameSize(n PFRawName) int {
	b, _ := json.Marshal(n)
	return len(b) + 1
}
