//go:build itest

// T14e 规模联调：4 个项目、约 4145 个文件、约 3.6GB 的一次完整首次同步 + 无变化轮 + 内存峰值。
//
// 只有 run_itest.py 带 --scale（环境变量 PROJFILES_ITEST_SCALE=1）时才跑；平时全量联调 t.Skip。
//
// 数据在 Go 里确定性生成，不用 Python、不读现成假数据：固定种子 splitmix64，同一种子每次生成
// 完全相同的文件名和字节。类别与数量见 scaleFullPlan：小文件 3000、文档 800（约一半 <1MB）、
// 图片 320、视频 4（3×300MB + 1×1GB）、项目说明 21 → 合计 4145 个文件。
// 内容一律伪随机字节（不可压缩）；大文件按 1MB 分块写、边写边算 sha256，
// 不把任何文件整个读进内存（架子里的 macTree/fileBody/bigFile 都是整读，规模测试一律不用）。
//
// 现场与别的场景一样经 newCluster 建：Mac 侧全在 t.TempDir() 下，Ubuntu 走真 SSH+SFTP。
// 生成前查本机临时目录所在盘的剩余空间，不足 12GB 直接 t.Skip（写不满会假失败）。

package itest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// scaleShrink 只在开发调通时临时置 true：量缩到几十 MB、并且不看 PROJFILES_ITEST_SCALE。
// 最终提交必须是 false（全量 3.6GB），不要把这个开关留在别的取值上。
const scaleShrink = false

const (
	scaleEnv       = "PROJFILES_ITEST_SCALE"
	scaleFreeMin   = int64(12 << 30)    // 生成前要求的本机剩余空间
	scaleMaxRounds = 30                  // 首次同步最多允许几轮做完
	scaleRoundTO   = 50 * time.Minute    // 单轮上限（整个 TestMain 由 -timeout 120m 兜）
	scaleHeapMax   = int64(200 << 20)    // HeapInuse 峰值上限
	scaleIdleLimit = 5 * time.Second     // 无变化一轮的硬上限（>3 秒在 RESULT 里提一句）
	scaleSeed      = uint64(0x9E3779B97F4A7C15) // 固定种子：文件名与字节都可复现
	scaleIOChunk   = 1 << 20
)

// scaleUnitPlan 是一个项目要生成的内容（数量 + 视频尺寸）。
type scaleUnitPlan struct {
	rel, code string
	small     int     // 小文件（1KB–64KB）
	docs      int     // 文档（64KB–10MB，一半 <1MB）
	images    int     // 图片（64KB–5MB）
	videos    []int64 // 视频（精确尺寸）
	notes     int     // 项目说明类（几百字节–2KB）
}

// scaleFullPlan：小 3000、文档 800、图片 320、视频 4（1GB + 3×300MB）、说明 21 → 4145 个文件。
var scaleFullPlan = []scaleUnitPlan{
	{rel: "2026/260403J1001-测试项目甲", code: "260403J1001", small: 1200, docs: 300, images: 120, videos: []int64{1 << 30}, notes: 6},
	{rel: "2026/260403J1002-测试项目乙", code: "260403J1002", small: 900, docs: 250, images: 90, videos: []int64{300 << 20}, notes: 5},
	{rel: "2026/260403J1101-测试项目丙", code: "260403J1101", small: 500, docs: 150, images: 60, videos: []int64{300 << 20}, notes: 5},
	{rel: "2026/260403J1102-测试项目丁", code: "260403J1102", small: 400, docs: 100, images: 50, videos: []int64{300 << 20}, notes: 5},
}

// scaleShrinkPlan：同结构、同目录、同命名，只是数量和尺寸缩小（约 60MB），开发调通用。
var scaleShrinkPlan = []scaleUnitPlan{
	{rel: "2026/260403J1001-测试项目甲", code: "260403J1001", small: 60, docs: 14, images: 6, videos: []int64{8 << 20}, notes: 6},
	{rel: "2026/260403J1002-测试项目乙", code: "260403J1002", small: 45, docs: 12, images: 5, videos: []int64{3 << 20}, notes: 5},
	{rel: "2026/260403J1101-测试项目丙", code: "260403J1101", small: 25, docs: 7, images: 3, videos: []int64{3 << 20}, notes: 5},
	{rel: "2026/260403J1102-测试项目丁", code: "260403J1102", small: 20, docs: 5, images: 3, videos: []int64{3 << 20}, notes: 5},
}

// 目录与名字池：都是中文名，落在生产的排除规则之外（不生成 ~$、.DS_Store 这类）。
var (
	scaleImgDirs   = []string{"1-资料/图片", "1-资料/图纸"}
	scaleDocDirs   = []string{"1-资料/文稿", "3-报告/草稿", "3-报告/终稿", "6-其他/历史版本"}
	scaleVidDirs   = []string{"2-素材/视频"}
	scaleSmallDirs = []string{"4-便签", "5-表格", "6-其他/历史版本"}
	scaleNoteNames = []string{"单元说明.txt", "目录结构.md", "交接备忘.txt", "修改记录.md", "联系人清单.csv", "工作日志.txt"}
	scaleImgWords  = []string{"现场照片", "会议合影", "产品图", "图纸扫描", "截图存档"}
	scaleDocWords  = []string{"采访整理", "需求说明", "会议纪要", "正文初稿", "校样", "材料汇编"}
	scaleVidWords  = []string{"拍摄素材", "发布会录像", "访谈录像", "花絮"}
	scaleSmWords   = []string{"便签", "速记", "联系人", "通话记录", "随手记"}
	scaleImgExt    = []string{".jpg", ".png"}
	scaleDocExt    = []string{".docx", ".md", ".txt"}
	scaleSmExt     = []string{".txt", ".md", ".csv"}
	// 类别标签只用于统计和日志，不进文件名；各类名字池互不相交，同目录里也不会撞名。
	catSmall = "小文件"
	catDoc   = "文档"
	catImage = "图片"
	catVideo = "视频"
	catNotes = "说明"
)

// ── 确定性伪随机 ────────────────────────────────────────

// scaleRand 是 splitmix64：算法写死在这里，不依赖标准库随机数的实现细节，跨版本可复现。
type scaleRand struct{ s uint64 }

func newScaleRand(seed uint64) *scaleRand { return &scaleRand{s: seed} }

func (r *scaleRand) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *scaleRand) n(max int64) int64 {
	if max <= 0 {
		return 0
	}
	return int64(r.next() % uint64(max))
}

func (r *scaleRand) pick(ss []string) string { return ss[r.n(int64(len(ss)))] }

// f 返回 [0,1) 上的均匀浮点数（用 53 位尾数）。
func (r *scaleRand) f() float64 { return float64(r.next()>>11) / float64(int64(1)<<53) }

// fill 用伪随机字节填满 buf（不可压缩）。
func (r *scaleRand) fill(buf []byte) {
	i := 0
	for ; i+8 <= len(buf); i += 8 {
		binary.LittleEndian.PutUint64(buf[i:], r.next())
	}
	for ; i < len(buf); i++ {
		buf[i] = byte(r.next())
	}
}

// ── 计划与落盘 ──────────────────────────────────────────

// scaleFile 是一个待生成的文件：项目内相对路径、大小、内容种子、类别（统计用）。
type scaleFile struct {
	rel  string
	size int64
	seed uint64
	cat  string
}

// scaleUnitFiles 展开一个项目的清单；名字和大小都由种子决定，重复运行结果完全一致。
func scaleUnitFiles(p scaleUnitPlan, idx int) []scaleFile {
	r := newScaleRand(scaleSeed ^ uint64(idx)*0x100000001b3)
	fs := make([]scaleFile, 0, p.small+p.docs+p.images+len(p.videos)+p.notes)
	fs = append(fs, scaleGen(p.small, r, catSmall, scaleSmallDirs, scaleSmWords, scaleSmExt)...)
	fs = append(fs, scaleGen(p.docs, r, catDoc, scaleDocDirs, scaleDocWords, scaleDocExt)...)
	fs = append(fs, scaleGen(p.images, r, catImage, scaleImgDirs, scaleImgWords, scaleImgExt)...)
	for j, sz := range p.videos {
		fs = append(fs, scaleFile{
			rel:  scaleVidDirs[j%len(scaleVidDirs)] + "/" + fmt.Sprintf("%s%03d.mp4", r.pick(scaleVidWords), j+1),
			size: sz, seed: r.next(), cat: catVideo})
	}
	for j := 0; j < p.notes && j < len(scaleNoteNames); j++ {
		fs = append(fs, scaleFile{rel: scaleNoteNames[j], size: 200 + r.n(1800), seed: r.next(), cat: catNotes})
	}
	return fs
}

// scaleGen 生成 n 个某类别的文件名与大小（都不可压缩）：
// 小文件 1KB–64KB；文档一半 64KB–1MB、一半 1MB–10MB（用 f^5 压向小端，让总量落在约 3.6GB）；
// 图片 64KB–5MB（用 f^2）。文件名 = 中文词 + 4 位序号 + 扩展名，同类别内序号唯一 → 不撞名。
func scaleGen(n int, r *scaleRand, cat string, dirs, words, exts []string) []scaleFile {
	out := make([]scaleFile, 0, n)
	for i := 0; i < n; i++ {
		var size int64
		switch cat {
		case catSmall:
			size = 1<<10 + r.n(64<<10-1<<10)
		case catDoc:
			if i%2 == 0 {
				size = 64<<10 + r.n(1<<20-64<<10)
			} else {
				size = 1<<20 + int64(math.Pow(r.f(), 5)*float64(10<<20-1<<20))
			}
		default:
			size = 64<<10 + int64(math.Pow(r.f(), 2)*float64(5<<20-64<<10))
		}
		out = append(out, scaleFile{
			rel:  fmt.Sprintf("%s/%s%04d%s", dirs[i%len(dirs)], r.pick(words), i+1, r.pick(exts)),
			size: size, seed: r.next(), cat: cat})
	}
	return out
}

// scaleWrite 把一个单元的文件写进 root（分块写、边写边算 sha256），返回 相对路径→sha256 和总字节。
func scaleWrite(t *testing.T, root string, files []scaleFile) (map[string]string, int64) {
	t.Helper()
	buf := make([]byte, scaleIOChunk)
	out := make(map[string]string, len(files))
	var total int64
	g0 := time.Now()
	for i, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建目录 %s 失败：%v", filepath.Dir(p), err)
		}
		w, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatalf("创建 %s 失败：%v", p, err)
		}
		h, rs := sha256.New(), newScaleRand(f.seed)
		var left int64 = f.size
		for left > 0 {
			n := int64(scaleIOChunk)
			if left < n {
				n = left
			}
			rs.fill(buf[:n])
			if _, err := w.Write(buf[:n]); err != nil {
				w.Close()
				t.Fatalf("写 %s 失败：%v", p, err)
			}
			h.Write(buf[:n])
			left -= n
		}
		if err := w.Close(); err != nil {
			t.Fatalf("关闭 %s 失败：%v", p, err)
		}
		out[f.rel] = hex.EncodeToString(h.Sum(nil))
		total += f.size
		if (i+1)%400 == 0 || i+1 == len(files) {
			t.Logf("生成进度 %d/%d 个文件、%.0f MB（用时 %v）", i+1, len(files), float64(total)/(1<<20), time.Since(g0).Round(time.Second))
		}
	}
	return out, total
}

// ── 采样与工具 ──────────────────────────────────────────

// scaleFreeBytes 返回 path 所在文件系统上普通用户可用的字节数（照 projsync 的 statfs 写法转类型）。
func scaleFreeBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		t.Fatalf("查 %s 所在盘剩余空间失败：%v", path, err)
	}
	bs := uint64(st.Bsize)
	return int64(uint64(st.Bavail) * bs)
}

// scaleMemWatch 每秒采样一次 runtime 的 HeapInuse / Sys，记最大值。
type scaleMemWatch struct {
	quit     chan struct{}
	done     chan struct{}
	peakHeap atomic.Int64
	peakSys  atomic.Int64
	samples  atomic.Int64
}

func newScaleMemWatch() *scaleMemWatch {
	w := &scaleMemWatch{quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		var ms runtime.MemStats
		sample := func() {
			runtime.ReadMemStats(&ms)
			w.samples.Add(1)
			if int64(ms.HeapInuse) > w.peakHeap.Load() {
				w.peakHeap.Store(int64(ms.HeapInuse))
			}
			if int64(ms.Sys) > w.peakSys.Load() {
				w.peakSys.Store(int64(ms.Sys))
			}
		}
		sample()
		for {
			select {
			case <-w.quit:
				sample() // 收尾再取一次，别漏掉最后一段
				return
			case <-time.After(time.Second):
				sample()
			}
		}
	}()
	return w
}

// halt 停掉采样，返回峰值与采样次数。
func (w *scaleMemWatch) halt() (heap, sys, samples int64) {
	close(w.quit)
	<-w.done
	return w.peakHeap.Load(), w.peakSys.Load(), w.samples.Load()
}

// scaleRound 跑一轮全量核对：超时给足规模用量；只打紧凑摘要（4145 个文件的 Done 不整个 JSON 倒进日志）。
func scaleRound(t *testing.T, sch *projsync.PFSched, tag string) (*projsync.PFSchedReport, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleRoundTO)
	defer cancel()
	t0 := time.Now()
	rep := sch.RunRound(ctx, projsync.PFSchedRoundOptions{Full: true})
	el := time.Since(t0)
	t.Logf("%s：outcome=%s 用时=%v ran=%d 动作=%d 暂缓=%d 冲突=%d 错误=%d 不确定=%d 上行=%d 下行=%d Ubuntu剩余=%d",
		tag, rep.Outcome, el.Round(time.Millisecond), rep.Totals.Ran, rep.Totals.Done, rep.Totals.Deferred,
		rep.Totals.Conflicts, rep.Totals.Errors, rep.Totals.Uncertain, rep.Totals.BytesUp, rep.Totals.BytesDown, rep.FreeBytes)
	if rep.Outcome != projsync.PFSchedDone {
		t.Fatalf("%s outcome=%s reason=%s 单元情况=%s", tag, rep.Outcome, rep.Reason, scaleUnitsBrief(rep))
	}
	for _, u := range rep.Units {
		if u.Result != nil && len(u.Result.Errors) > 0 {
			n := len(u.Result.Errors)
			if n > 10 {
				n = 10
			}
			t.Fatalf("%s 单元 %s 有 %d 条错误（前 %d 条）：%s", tag, u.Code, len(u.Result.Errors), n, jsonOf(u.Result.Errors[:n]))
		}
	}
	return rep, el
}

// scaleUnitsBrief 打每个单元的紧凑一行（失败现场用，不含逐文件清单）。
func scaleUnitsBrief(rep *projsync.PFSchedReport) string {
	var b []byte
	for _, u := range rep.Units {
		if u.Result == nil {
			b = append(b, fmt.Sprintf("[%s action=%s why=%s note=%s]", u.Code, u.Action, u.Why, u.Note)...)
			continue
		}
		r := u.Result
		b = append(b, fmt.Sprintf("[%s %s planned=%d done=%d defer=%d skip=%d err=%d unc=%s reason=%s]",
			u.Code, r.Outcome, r.PlannedActions, len(r.Done), len(r.Deferred), len(r.Skipped), len(r.Errors),
			fmt.Sprint(r.Uncertain), r.Reason)...)
	}
	return string(b)
}

// scaleIdle 判断这一轮没有任何要做的事。
func scaleIdle(rep *projsync.PFSchedReport) bool {
	return rep.Totals.Done == 0 && rep.Totals.BytesUp == 0 && rep.Totals.BytesDown == 0 &&
		rep.Totals.Deferred == 0 && rep.Totals.Errors == 0 && rep.Totals.Uncertain == 0 && !rep.Incomplete
}

// scaleScanCached 用生产扫描 + 盘上的 Mac 侧哈希缓存重算一遍清单：
// 命中缓存的文件不读内容（scaleStats 里 CacheHits/Hashed 就是命中统计），返回 相对路径→sha256。
func scaleScanCached(t *testing.T, meta, macRoot, code string) (map[string]string, projsync.PFHashStats, int) {
	t.Helper()
	cp := filepath.Join(meta, projsync.PFRemoteCacheName, code+".json")
	cache := projsync.NewPFHashCache()
	cacheSize := int64(-1)
	if b, err := os.ReadFile(cp); err == nil {
		cacheSize = int64(len(b))
		c2, perr := projsync.PFParseHashCache(b)
		if perr != nil {
			t.Errorf("读 %s 的哈希缓存失败（当作没有，会重算）：%v", cp, perr)
		} else {
			cache = c2
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("读哈希缓存文件 %s 出错：%v", cp, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	opt := projsync.PFScanOptions{Filter: projsync.PFRemoteScanFilter(), QuietSeconds: 1, Now: time.Now}
	res, err := projsync.PFScanAndHash(ctx, macRoot, cache, opt)
	if err != nil {
		t.Fatalf("重扫 %s 失败：%v", macRoot, err)
	}
	out := make(map[string]string, len(res.Items))
	for _, it := range res.Items {
		if it.Kind == projsync.PFKindFile && it.State == projsync.PFStateOK {
			if it.Hash == "" {
				t.Errorf("%s 里 %s 没有哈希（state=%s code=%s reason=%s）", code, it.Path, it.State, it.Code, it.Reason)
				continue
			}
			out[it.Path] = it.Hash
		}
	}
	if res.Excluded > 0 {
		t.Logf("%s 扫描排除了 %d 个条目（规模数据不该有被排除的名字）", code, res.Excluded)
	}
	t.Logf("%s 哈希缓存：文件 %s %d 条；本轮命中 %d、新算 %d（读了 %d 字节）、不明 %d，用时 %v",
		code, cp, cacheSize, res.HashStats.CacheHits, res.HashStats.Hashed, res.HashStats.HashedBytes,
		res.HashStats.NewUnknown, res.HashStats.Elapsed.Round(time.Millisecond))
	return out, res.HashStats, cache.Len()
}

// ── 场景 ────────────────────────────────────────────────

// TestItestScale 规模联调：4 个项目 4145 个文件约 3.6GB。
// 1) 生成 → 开始同步 → 反复跑轮直到没有要做的事（≤30 轮）；2) Ubuntu 逐文件 sha256 与 Mac 一致；
// 3) 无变化一轮 <5 秒（目标 3 秒）且没有传输；4) 第二轮哈希缓存命中率；5) HeapInuse 峰值 <200MB；
// 6) 结束清 Ubuntu 与本机临时目录。
func TestItestScale(t *testing.T) {
	if !scaleShrink && os.Getenv(scaleEnv) != "1" {
		t.Skipf("环境变量 %s 不是 1（run_itest.py 要带 --scale）：规模测试跳过", scaleEnv)
	}
	tmp := os.TempDir()
	free := scaleFreeBytes(t, tmp)
	if free < scaleFreeMin {
		t.Skipf("本机临时目录 %s 所在盘只剩 %d MB，少于 %d MB：造不出 3.6GB，跳过规模测试",
			tmp, free>>20, scaleFreeMin>>20)
	}
	plan := scaleFullPlan
	if scaleShrink {
		plan = scaleShrinkPlan
		t.Logf("scaleShrink=true：用缩小规模的数据（只为调通流程，不是 3.6GB 的验收数据）")
	}
	type unitWork struct {
		p     scaleUnitPlan
		files []scaleFile
	}
	work := make([]unitWork, 0, len(plan))
	var wantFiles int
	var wantBytes int64
	cats := map[string]int{}
	for i, p := range plan {
		u := unitWork{p: p, files: scaleUnitFiles(p, i)}
		for _, f := range u.files {
			wantBytes += f.size
			cats[f.cat]++
		}
		wantFiles += len(u.files)
		work = append(work, u)
	}
	if !scaleShrink {
		for cat, want := range map[string]int{catSmall: 3000, catDoc: 800, catImage: 320, catVideo: 4, catNotes: 21} {
			if cats[cat] != want {
				t.Errorf("%s 计划 %d 个，实际排了 %d 个", cat, want, cats[cat])
			}
		}
		if wantFiles != 4145 {
			t.Errorf("文件总数 %d，要求 4145", wantFiles)
		}
	}
	t.Logf("规模数据计划：%d 个项目、%d 个文件、%.2f GB（%d 字节）；小文件 %d、文档 %d、图片 %d、视频 %d、说明 %d；临时目录 %s 剩余 %.1f GB",
		len(work), wantFiles, float64(wantBytes)/(1<<30), wantBytes, cats[catSmall], cats[catDoc],
		cats[catImage], cats[catVideo], cats[catNotes], tmp, float64(free)/(1<<30))

	watch := newScaleMemWatch()

	c := newCluster(t)
	ctx := context.Background()
	t.Cleanup(func() {
		c.cleanUbuntu(ctx)
		for _, d := range []string{c.sync, c.work} {
			if err := os.RemoveAll(d); err != nil {
				t.Errorf("清理本机 %s 失败：%v", d, err)
			}
		}
		t.Logf("收尾：Ubuntu 已清场（roots.json 原样保留），本机 work/sync 已删除，本机剩余 %.1f GB",
			float64(scaleFreeBytes(t, tmp))/(1<<30))
	})

	// 1. 生成数据 → 立项（照现有场景：写在 work 下、mtime 拨回两小时前、StartSync 整挪进 sync_dir）。
	gen := make(map[string]map[string]string, len(work)) // 编号 → 相对路径 → sha256（生成时边写边算）
	var genBytes int64
	g0 := time.Now()
	for _, u := range work {
		root := filepath.Join(c.work, filepath.FromSlash(u.p.rel))
		m, nb := scaleWrite(t, root, u.files)
		gen[u.p.code] = m
		genBytes += nb
		quietPast(t, root)
		res, err := c.mob.StartSync(ctx, u.p.rel)
		if err != nil {
			t.Fatalf("StartSync(%s) 出错：%v", u.p.rel, err)
		}
		if res.Status != projsync.PFUnitMoveDone {
			t.Fatalf("StartSync(%s) 没做成：status=%s reason=%s refusals=%s", u.p.rel, res.Status, res.Reason, jsonOf(res.Refusals))
		}
		t.Logf("已立项 %s：%d 个文件、%.0f MB", u.p.rel, len(u.files), float64(nb)/(1<<20))
	}
	t.Logf("数据生成完毕：%d 个文件、%.2f GB、用时 %v", wantFiles, float64(genBytes)/(1<<30), time.Since(g0).Round(time.Second))
	if genBytes != wantBytes {
		t.Errorf("生成的字节数 %d 与计划 %d 不符", genBytes, wantBytes)
	}

	sync0 := time.Now()
	var rounds int
	var upTotal, downTotal int64
	idle := false
	for rounds < scaleMaxRounds {
		rep, _ := scaleRound(t, c.sch, fmt.Sprintf("首次同步第 %d 轮", rounds+1))
		rounds++
		upTotal += rep.Totals.BytesUp
		downTotal += rep.Totals.BytesDown
		if scaleIdle(rep) {
			idle = true
			break
		}
	}
	firstDur := time.Since(sync0)
	if !idle {
		t.Errorf("跑了 %d 轮还没做完（上限 %d 轮）；单元情况见上面各轮日志", rounds, scaleMaxRounds)
	}
	t.Logf("首次同步：用时=%v 轮数=%d 上传字节=%d（%.2f GB）下载字节=%d",
		firstDur.Round(time.Millisecond), rounds, upTotal, float64(upTotal)/(1<<30), downTotal)
	if upTotal < wantBytes {
		t.Errorf("几轮合计上传 %d 字节，少于要传的 %d：数据没传够", upTotal, wantBytes)
	}

	// 2. Ubuntu 逐文件 sha256 与 Mac 一致：Ubuntu 侧经 SFTP 流式读来算，
	//    Mac 侧的基准是生成时边写边算的 sha256（同一种子同一字节，见第 4 步再用生产扫描核对一遍）。
	var ubFiles int
	for _, u := range work {
		ub := ubuntuTree(t, c.env.sf, c.env.root+"/"+u.p.rel)
		onlyA, onlyB, differ := cmpTree(gen[u.p.code], ub)
		if len(onlyA)+len(onlyB)+len(differ) > 0 {
			t.Errorf("%s 两边不一致：只在 Mac=%s 只在 Ubuntu=%s 内容不同=%s",
				u.p.code, scaleCut(onlyA), scaleCut(onlyB), scaleCut(differ))
		}
		var ubBytes int64
		for _, f := range u.files {
			ubBytes += f.size
		}
		ubFiles += len(ub)
		t.Logf("%s Ubuntu 核过 %d 个文件、%.0f MB", u.p.code, len(ub), float64(ubBytes)/(1<<20))
	}
	if ubFiles != wantFiles {
		t.Errorf("Ubuntu 上共 %d 个文件，Mac 生成了 %d 个", ubFiles, wantFiles)
	}
	t.Logf("逐文件 sha256 比对：%d 个文件全部核过，共 %.2f GB", ubFiles, float64(wantBytes)/(1<<30))

	// 3. 无变化一轮：不该有任何传输，用时给足余量（目标 3 秒、硬上限 5 秒）。
	rep2, el2 := scaleRound(t, c.sch, "无变化轮")
	if !scaleIdle(rep2) {
		t.Errorf("对齐后又有的做了：动作=%d 上行=%d 下行=%d 暂缓=%d 单元情况=%s",
			rep2.Totals.Done, rep2.Totals.BytesUp, rep2.Totals.BytesDown, rep2.Totals.Deferred, scaleUnitsBrief(rep2))
	}
	if el2 >= scaleIdleLimit {
		t.Errorf("无变化一轮用了 %v，硬上限 %v", el2.Round(time.Millisecond), scaleIdleLimit)
	}
	if el2 > 3*time.Second {
		t.Logf("注意：无变化一轮用了 %v，超过 3 秒的目标（在 5 秒硬上限内，按放宽口径算过）", el2.Round(time.Millisecond))
	}

	// 4. 第二轮的哈希缓存：用生产扫描 + 盘上的 <meta>/hashcache/<编号>.json 重扫一遍，
	//    全部命中缓存才算过（命中就不读内容）；顺带核对生产扫出的哈希与生成时算的一致。
	//    轮次报告（PFSchedReport/PFRoundResult）里没有哈希缓存命中统计，只有 PFScanResult.HashStats，
	//    所以命中数在这里取。
	var hits, hashed, unknown, cacheEntries int
	for _, u := range work {
		mac, hs, n := scaleScanCached(t, c.meta, filepath.Join(c.sync, filepath.FromSlash(u.p.rel)), u.p.code)
		hits += hs.CacheHits
		hashed += hs.Hashed
		unknown += hs.NewUnknown
		cacheEntries += n
		if len(mac) != len(gen[u.p.code]) {
			t.Errorf("%s Mac 扫到 %d 个文件，生成的是 %d 个", u.p.code, len(mac), len(gen[u.p.code]))
		}
		for p, sha := range mac {
			if gen[u.p.code][p] != sha {
				t.Errorf("%s %s 生产扫出的 sha=%s 与生成时算的 %s 不符", u.p.code, p, sha, gen[u.p.code][p])
			}
		}
	}
	if hashed != 0 {
		t.Errorf("重扫新算了 %d 个文件的哈希，想要 0（全部命中缓存）", hashed)
	}
	if hits != wantFiles {
		t.Errorf("重扫命中缓存 %d 次，想要 %d（每个文件一次）", hits, wantFiles)
	}
	if unknown != 0 {
		t.Errorf("重扫有 %d 个条目标成不明", unknown)
	}
	t.Logf("哈希缓存：命中 %d/%d（100%%），新算 %d，不明 %d，缓存条目合计 %d", hits, wantFiles, hashed, unknown, cacheEntries)

	peakHeap, peakSys, samples := watch.halt()
	t.Logf("内存：每秒采样 %d 次，HeapInuse 峰值 %d MB，Sys 峰值 %d MB", samples, peakHeap>>20, peakSys>>20)
	if peakHeap > scaleHeapMax {
		t.Errorf("HeapInuse 峰值 %d MB，超过上限 %d MB", peakHeap>>20, scaleHeapMax>>20)
	}

	t.Logf("规模联调指标：首次同步用时=%v 轮数=%d 上传=%d 字节（%.2f GB）无变化一轮=%v 哈希缓存命中=%d/%d HeapInuse峰值=%d MB Sys峰值=%d MB",
		firstDur.Round(time.Millisecond), rounds, upTotal, float64(upTotal)/(1<<30), el2.Round(time.Millisecond),
		hits, wantFiles, peakHeap>>20, peakSys>>20)
}

// scaleCut 失败现场用：一条日志里最多列 10 个路径。
func scaleCut(v []string) string {
	if len(v) > 10 {
		return fmt.Sprintf("%d 个（前 10）%v", len(v), v[:10])
	}
	return fmt.Sprintf("%d 个 %v", len(v), v)
}
