package projsync

// 本文件管暂存区里的文件格式和清理。
//
// 暂存区（上传在 Ubuntu、下载在本机）里每份内容两个文件，按期望 sha256 寻址：
//   <sha256>.part  内容（边传边写，传完核验通过后才算数）
//   <sha256>.meta  文本：第一行是头（JSON：版本、源路径、大小、期望 sha256、块大小），
//                  之后每行一个已确认块的 sha256（64 位小写十六进制），
//                  核验通过后再加最后一行 "verified <大小> <part 的 mtime 纳秒>"。
// 同一内容只会有一份，不管从哪个源路径来。
// meta 只追加不改（续传回退时整份重写）；最后一行没写完（没有换行）就当它不存在。
// 边界：meta 坏了、对不上，一律当作没有，重新传，不会因此产出错误内容。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const pfTrMetaVersion = 1

// meta 超过这个大小就当坏的（64 位十六进制 × 块数，几十 GB 的文件也只有几百 KB）。
const pfTrMetaMax = 64 << 20

type pfTrMetaHeader struct {
	Version int    `json:"v"`
	Src     string `json:"src"` // 第一次传时的源路径，只作参考
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Block   int64  `json:"block"`
}

type pfTrMeta struct {
	hdr    pfTrMetaHeader
	blocks []string
	// 核验通过时记下的 part 大小和 mtime；hasVer 为 false 表示还没核验过
	hasVer   bool
	verSize  int64
	verMTime int64
}

var pfTrHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// parsePFTrMeta 解析 meta 内容；格式不对返回 nil（调用方当作没有 meta）。
func parsePFTrMeta(data []byte) *pfTrMeta {
	text := string(data)
	// 最后一段（最后一个换行之后）是没写完的，丢掉
	i := strings.LastIndexByte(text, '\n')
	if i < 0 {
		return nil
	}
	lines := strings.Split(text[:i], "\n")
	var m pfTrMeta
	if err := json.Unmarshal([]byte(lines[0]), &m.hdr); err != nil || m.hdr.Version != pfTrMetaVersion {
		return nil
	}
	if m.hdr.Size < 0 || m.hdr.Block < 1 || !pfTrHex64.MatchString(m.hdr.SHA256) {
		return nil
	}
	for _, ln := range lines[1:] {
		switch {
		case m.hasVer:
			return nil // 核验行之后不该再有内容
		case pfTrHex64.MatchString(ln):
			m.blocks = append(m.blocks, ln)
		case strings.HasPrefix(ln, "verified "):
			f := strings.Fields(ln)
			if len(f) != 3 {
				return nil
			}
			sz, err1 := strconv.ParseInt(f[1], 10, 64)
			mt, err2 := strconv.ParseInt(f[2], 10, 64)
			if err1 != nil || err2 != nil {
				return nil
			}
			m.hasVer, m.verSize, m.verMTime = true, sz, mt
		default:
			return nil
		}
	}
	return &m
}

// encode 返回 meta 头一行（含换行）。
func (h pfTrMetaHeader) encode() []byte {
	b, _ := json.Marshal(h)
	return append(b, '\n')
}

func pfTrNumBlocks(size, block int64) int64 {
	if size <= 0 {
		return 0
	}
	return (size + block - 1) / block
}

// pfTrBlockEnd 是第 i 块（从 0 数）结束的偏移（最后一块可能不满）。
func pfTrBlockEnd(i, size, block int64) int64 {
	return min((i+1)*block, size)
}

func pfTrSumHex(sum []byte) string { return hex.EncodeToString(sum) }

// ── 清理 ────────────────────────────────────────────────

var pfTrStageName = regexp.MustCompile(`^([0-9a-f]{64})\.(part|meta)$`)

// PFCleanResult 是一次清理的结果。
type PFCleanResult struct {
	Removed []string // 删掉的文件名
	Bytes   int64    // 释放的字节数
	Kept    int      // 还没到期、或正在用而保留的内容份数
}

// PFCleanStaging 删掉暂存区 dir 里 maxAge 没被引用的 part/meta（按内容成对删）。
//
// 「被引用」= 这份内容的 part 或 meta 最近被写过或被提交过（Submit 复用 part 时会刷新 meta 的时间）。
// 只认 <64位小写十六进制>.part/.meta 这两种名字，别的文件一律不碰；inUse 返回 true 的内容跳过
// （传输层自己的清理会传进行中的任务）；maxAge <= 0 拒绝执行，免得一个错参数清空暂存区。
// 调用方每天调一次；单个文件删不掉不会停，错误合并后一起返回。目录不存在不算错。
func PFCleanStaging(ctx context.Context, fs PFTransferFS, dir string, maxAge time.Duration, now time.Time, inUse func(sha string) bool) (PFCleanResult, error) {
	var res PFCleanResult
	if maxAge <= 0 {
		return res, errors.New("清理暂存区：保留时长必须大于 0")
	}
	ents, err := fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("清理暂存区：列目录 %s 失败: %w", dir, err)
	}
	type group struct {
		files  map[string]os.FileInfo // 后缀 → 信息
		latest time.Time
	}
	groups := map[string]*group{}
	for _, e := range ents {
		m := pfTrStageName.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		g := groups[m[1]]
		if g == nil {
			g = &group{files: map[string]os.FileInfo{}}
			groups[m[1]] = g
		}
		g.files[m[2]] = e
		if e.ModTime().After(g.latest) {
			g.latest = e.ModTime()
		}
	}
	shas := make([]string, 0, len(groups))
	for s := range groups {
		shas = append(shas, s)
	}
	sort.Strings(shas)
	var errs []error
	for _, sha := range shas {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		g := groups[sha]
		if now.Sub(g.latest) < maxAge || (inUse != nil && inUse(sha)) {
			res.Kept++
			continue
		}
		// 先删 part 再删 meta：中途失败留下的是没有 part 的 meta，无害
		for _, ext := range []string{"part", "meta"} {
			fi, ok := g.files[ext]
			if !ok {
				continue
			}
			if err := fs.Remove(fs.Join(dir, fi.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("清理暂存区：删除 %s 失败: %w", fi.Name(), err))
				break
			}
			res.Removed = append(res.Removed, fi.Name())
			res.Bytes += fi.Size()
		}
	}
	return res, errors.Join(errs...)
}

// pfTrReadAll 把已知大小的文件一次读完（按偏移读，SFTP 上比顺序读快得多）。
func pfTrReadAll(f PFTransferFile, size int64) ([]byte, error) {
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, 0)
	if int64(n) == size {
		return buf, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}
