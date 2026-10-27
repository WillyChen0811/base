package projsync

// 本文件管「搬一个文件」：暂存区 part 文件、分块续传、整体核验。
// 算法移植自 pybase（py/pybase/fileops.py:195-235 copy_file_resumable），不调用 Python：
//   - 按块（缺省 8MB）搬，每块写完才在 meta 里记下这块的 sha256（已确认）。
//   - 续传：以 meta 里已确认的块为准，part 截到最后一个完整块的边界；
//     把最后一块从 part 读回来和记下的哈希比对，对不上退一块，直到对上或退到 0；从这个偏移继续。
//   - 传完由目标机整体算 part 的 sha256 和期望比（见 PFTargetVerify）；
//     不符 → 删掉 part 和 meta 重来一轮；同一任务连续两次不符 → 报错停止。
//     源在传输中被改只会让整体哈希对不上，不会产出半新半旧的结果。
//   - 通信错误不重试，原样（带中文原因）返回，part 保留。
// 边界：这里不决定谁先传、传几个（pftransfer.go），也不把 part 放到正位。

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// errPFTrSourceChanged 表示源在传输中变短、变样：按「核验不符」处理（丢掉 part 重来）。
var errPFTrSourceChanged = errors.New("源文件在传输中发生了变化")

type pfTrRun struct {
	m           *PFTransfer
	j           *pfTrJob
	src         PFTransferFS // 源所在的文件系统
	stage       PFTransferFS // 暂存区所在的文件系统（目标）
	srcRemote   bool
	stageRemote bool
	stageDir    string
	part, meta  string
	verify      PFTargetVerify
}

func (m *PFTransfer) newRun(j *pfTrJob) *pfTrRun {
	r := &pfTrRun{m: m, j: j}
	remote := m.getRemote()
	if j.spec.Dir == PFUpload {
		r.src, r.stage = m.cfg.Local, remote
		r.stageRemote = true
		r.stageDir = m.cfg.RemoteStaging
		r.verify = m.cfg.VerifyUp
	} else {
		r.src, r.stage = remote, m.cfg.Local
		r.srcRemote = true
		r.stageDir = m.cfg.LocalStaging
		r.verify = m.cfg.VerifyDown
	}
	if r.stage != nil {
		r.part = r.stage.Join(r.stageDir, j.spec.SHA256+".part")
		r.meta = r.stage.Join(r.stageDir, j.spec.SHA256+".meta")
	}
	return r
}

// ── 错误归类 ────────────────────────────────────────────

func (r *pfTrRun) newErr(kind PFErrKind, path, msg string, err error) *PFTransferError {
	return &PFTransferError{Kind: kind, Dir: r.j.spec.Dir, Path: path, Msg: msg, Err: err}
}

func (r *pfTrRun) canceled() error {
	if err := r.j.ctx.Err(); err != nil {
		return r.m.canceledErr(r.j)
	}
	return nil
}

// srcErr 把读源的错误归类：不存在 = 源问题；其余按源在哪一边分通信/本机。
func (r *pfTrRun) srcErr(op string, err error) error {
	if c := r.canceled(); c != nil {
		return c
	}
	if errors.Is(err, os.ErrNotExist) {
		return r.newErr(PFErrSource, r.j.spec.Src, "源文件不存在（"+op+"）", err)
	}
	if r.srcRemote {
		return r.newErr(PFErrComm, r.j.spec.Src, op+"失败（远端通信或读取出错，结果不确定）", err)
	}
	return r.newErr(PFErrLocal, r.j.spec.Src, op+"失败", err)
}

// stageErr 把暂存区读写的错误归类：暂存区在远端就是通信错误，否则是本机错误。
func (r *pfTrRun) stageErr(op string, err error) error {
	if c := r.canceled(); c != nil {
		return c
	}
	if r.stageRemote {
		return r.newErr(PFErrComm, r.part, op+"失败（远端通信或写入出错，结果不确定）", err)
	}
	return r.newErr(PFErrLocal, r.part, op+"失败", err)
}

// ── meta 读写 ───────────────────────────────────────────

func (r *pfTrRun) header() pfTrMetaHeader {
	return pfTrMetaHeader{Version: pfTrMetaVersion, Src: r.j.spec.Src, Size: r.j.spec.Size, SHA256: r.j.spec.SHA256, Block: r.m.cfg.BlockSize}
}

// loadMeta 读 meta；不存在或内容不能用返回 nil, nil；只有读写出错才返回 error。
func (r *pfTrRun) loadMeta() (*pfTrMeta, error) {
	if r.stage == nil {
		return nil, r.newErr(PFErrConfig, "", "没有可用的文件系统", nil)
	}
	fi, err := r.stage.Stat(r.meta)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, r.stageErr("读取 meta 信息", err)
	}
	if fi.IsDir() || fi.Size() > pfTrMetaMax {
		return nil, nil
	}
	f, err := r.stage.Open(r.meta)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, r.stageErr("打开 meta", err)
	}
	defer f.Close()
	data, err := pfTrReadAll(f, fi.Size())
	if err != nil {
		return nil, r.stageErr("读取 meta", err)
	}
	return parsePFTrMeta(data), nil
}

// metaMatches 判断 meta 是不是这个任务的（大小、sha256、块大小都对得上）。
func (r *pfTrRun) metaMatches(m *pfTrMeta) bool {
	h := r.header()
	return m.hdr.Size == h.Size && m.hdr.SHA256 == h.SHA256 && m.hdr.Block == h.Block &&
		int64(len(m.blocks)) <= pfTrNumBlocks(h.Size, h.Block)
}

// writeMeta 整份重写 meta：头 + 已确认的块；返回写入的字节数（后面追加从这里开始）。
func (r *pfTrRun) writeMeta(blocks []string) (int64, error) {
	buf := r.header().encode()
	for _, b := range blocks {
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	f, err := r.stage.OpenFile(r.meta, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return 0, r.stageErr("写 meta", err)
	}
	if _, err := f.WriteAt(buf, 0); err != nil {
		f.Close()
		return 0, r.stageErr("写 meta", err)
	}
	if err := f.Close(); err != nil {
		return 0, r.stageErr("写 meta", err)
	}
	return int64(len(buf)), nil
}

// discard 删掉 part 和 meta（核验不符、续传对不上时）。
func (r *pfTrRun) discard() error {
	for _, p := range []string{r.part, r.meta} {
		if err := r.stage.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return r.stageErr("删除暂存文件 "+p, err)
		}
	}
	return nil
}

// markVerified 在 meta 末尾记下「核验通过」和 part 当时的大小、mtime；失败只是以后不能直接复用，不算错。
func (r *pfTrRun) markVerified() {
	fi, err := r.stage.Stat(r.part)
	if err != nil {
		return
	}
	mfi, err := r.stage.Stat(r.meta)
	if err != nil {
		return
	}
	f, err := r.stage.OpenFile(r.meta, os.O_WRONLY)
	if err != nil {
		return
	}
	line := fmt.Sprintf("verified %d %d\n", fi.Size(), fi.ModTime().UnixNano())
	_, _ = f.WriteAt([]byte(line), mfi.Size())
	_ = f.Close()
}

// ── 一轮传输 ────────────────────────────────────────────

// transferPass 跑任务的一轮：可能传完（res.PartPath 有值）、让出（yield）、或出错。
func (m *PFTransfer) transferPass(j *pfTrJob) (res PFTransferResult, yield bool, err error) {
	r := m.newRun(j)
	defer func() {
		res.Job = j.spec
		res.SentBytes = j.sent
		res.ResumedFrom = j.firstOffset
		res.Attempts = j.attempts
		res.Yields = j.yields
	}()
	yield, err = r.run(&res)
	return res, yield, err
}

func (r *pfTrRun) run(res *PFTransferResult) (yield bool, err error) {
	j, spec := r.j, r.j.spec
	if r.src == nil || r.stage == nil {
		return false, r.newErr(PFErrConfig, spec.Src, "没有可用的远端连接", nil)
	}
	if err := r.stage.MkdirAll(r.stageDir); err != nil {
		return false, r.stageErr("创建暂存区目录 "+r.stageDir, err)
	}
	for {
		if c := r.canceled(); c != nil {
			return false, c
		}

		// 1. 暂存区里已有核验过的同内容 part：直接复用
		meta, err := r.loadMeta()
		if err != nil {
			return false, err
		}
		if meta != nil && r.metaMatches(meta) && meta.hasVer {
			if fi, err := r.stage.Stat(r.part); err == nil && fi.Size() == spec.Size &&
				meta.verSize == spec.Size && meta.verMTime == fi.ModTime().UnixNano() {
				now := time.Now()
				_ = r.stage.Chtimes(r.meta, now, now) // 记一次引用，清理按它算
				if !j.firstSet {
					j.firstOffset, j.firstSet = spec.Size, true
				}
				j.confirmed.Store(spec.Size)
				res.Reused, res.PartPath = true, r.part
				return false, nil
			}
		}

		// 2. 源还是不是期望的那个文件
		sfi, err := r.src.Stat(spec.Src)
		if err != nil {
			return false, r.srcErr("读取源文件信息", err)
		}
		if sfi.IsDir() {
			return false, r.newErr(PFErrSource, spec.Src, "源路径不是文件", nil)
		}
		if sfi.Size() != spec.Size {
			if err := r.contentMismatch(fmt.Sprintf("源文件现在是 %d 字节，期望 %d 字节", sfi.Size(), spec.Size)); err != nil {
				return false, err
			}
			continue
		}

		// 3. 续传点：截到最后一个对得上的完整块
		offset, blocks, metaLen, err := r.plan(meta)
		if err != nil {
			return false, err
		}
		if !j.firstSet {
			j.firstOffset, j.firstSet = offset, true
		}
		j.confirmed.Store(offset)

		// 4. 搬剩下的块（让出后回来继续同一轮，不算新的一轮）
		if !j.attemptOpen {
			j.attempts++
			j.attemptOpen = true
		}
		yield, err := r.copyBlocks(offset, blocks, metaLen)
		if yield {
			return true, nil
		}
		if errors.Is(err, errPFTrSourceChanged) {
			if err := r.contentMismatch(err.Error()); err != nil {
				return false, err
			}
			continue
		}
		if err != nil {
			return false, err
		}

		// 5. 目标机整体核验
		sum, size, verr := r.verify(j.ctx, r.part)
		if verr != nil {
			if c := r.canceled(); c != nil {
				return false, c
			}
			kind, msg := PFErrLocal, "本机核验暂存文件失败"
			if r.stageRemote {
				kind, msg = PFErrComm, "目标端核验失败（通信或计算出错，结果不确定，暂存文件保留）"
			}
			return false, r.newErr(kind, r.part, msg, verr)
		}
		if sum != spec.SHA256 || size != spec.Size {
			if err := r.contentMismatch(fmt.Sprintf("目标端算出 sha256=%s 大小=%d，期望 sha256=%s 大小=%d", sum, size, spec.SHA256, spec.Size)); err != nil {
				return false, err
			}
			continue
		}
		r.markVerified()
		j.verifyFails = 0
		res.PartPath = r.part
		return false, nil
	}
}

// contentMismatch 处理「内容和期望对不上」：丢掉 part 和 meta；这是同一任务第二次就返回错误，否则返回 nil 让调用方重来。
func (r *pfTrRun) contentMismatch(why string) error {
	if err := r.discard(); err != nil {
		return err
	}
	r.j.attemptOpen = false
	r.j.verifyFails++
	if r.j.verifyFails >= 2 {
		return r.newErr(PFErrVerify, r.j.spec.Src, "连续两次整体核验不符，已丢弃暂存文件，不产出结果（源文件可能在传输期间被改动）: "+why, nil)
	}
	return nil
}

// plan 决定从哪个偏移续传：返回偏移、已确认块的哈希、重写后 meta 的长度。
// meta 不存在或对不上 → 清掉旧 part 从头来；否则把最后一块读回来比对，不对就退一块。
func (r *pfTrRun) plan(meta *pfTrMeta) (offset int64, blocks []string, metaLen int64, err error) {
	spec, bsz := r.j.spec, r.m.cfg.BlockSize
	if meta == nil || !r.metaMatches(meta) {
		if err := r.discard(); err != nil {
			return 0, nil, 0, err
		}
		n, err := r.writeMeta(nil)
		return 0, nil, n, err
	}
	n := int64(len(meta.blocks))
	var have int64 = -1
	if fi, err := r.stage.Stat(r.part); err == nil && !fi.IsDir() {
		have = fi.Size()
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, nil, 0, r.stageErr("读取 part 信息", err)
	}
	for n > 0 && pfTrBlockEnd(n-1, spec.Size, bsz) > have {
		n-- // part 比 meta 记的短：只信 part 里确实有的块
	}
	if n > 0 {
		f, err := r.stage.Open(r.part)
		if err != nil {
			return 0, nil, 0, r.stageErr("打开 part", err)
		}
		defer f.Close()
		for n > 0 {
			start, end := (n-1)*bsz, pfTrBlockEnd(n-1, spec.Size, bsz)
			buf := make([]byte, end-start)
			rn, rerr := f.ReadAt(buf, start)
			if int64(rn) < int64(len(buf)) && rerr != nil && !errors.Is(rerr, io.EOF) {
				return 0, nil, 0, r.stageErr("读回 part 的最后一块", rerr)
			}
			if int64(rn) == int64(len(buf)) && pfTrSumHex(pfTrSum(buf)) == meta.blocks[n-1] {
				break
			}
			n-- // 对不上：退一块
		}
	}
	blocks = append([]string(nil), meta.blocks[:n]...)
	if n > 0 {
		offset = pfTrBlockEnd(n-1, spec.Size, bsz)
	}
	metaLen, err = r.writeMeta(blocks)
	return offset, blocks, metaLen, err
}

func pfTrSum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// copyBlocks 从 offset 起一块一块搬；每块搬完记 meta、测速、报进度，再看要不要让出。
func (r *pfTrRun) copyBlocks(offset int64, blocks []string, metaLen int64) (yield bool, err error) {
	j, spec, m := r.j, r.j.spec, r.m
	bsz := m.cfg.BlockSize

	sf, err := r.src.Open(spec.Src)
	if err != nil {
		return false, r.srcErr("打开源文件", err)
	}
	defer sf.Close()
	pf, err := r.stage.OpenFile(r.part, os.O_WRONLY|os.O_CREATE)
	if err != nil {
		return false, r.stageErr("打开 part", err)
	}
	closed := false
	defer func() {
		if !closed {
			pf.Close()
		}
	}()
	if fi, err := r.stage.Stat(r.part); err != nil {
		return false, r.stageErr("读取 part 信息", err)
	} else if fi.Size() != offset {
		if err := pf.Truncate(offset); err != nil { // 截到已确认块的边界
			return false, r.stageErr("截断 part", err)
		}
	}
	mf, err := r.stage.OpenFile(r.meta, os.O_WRONLY)
	if err != nil {
		return false, r.stageErr("打开 meta", err)
	}
	defer mf.Close()

	buf := make([]byte, m.cfg.IOChunk)
	off := offset
	for off < spec.Size {
		if c := r.canceled(); c != nil {
			return false, c
		}
		blen := min(bsz, spec.Size-off)
		h := sha256.New()
		t0 := time.Now()
		for done := int64(0); done < blen; {
			if c := r.canceled(); c != nil {
				return false, c
			}
			n := min(int64(m.chunkFor(spec.Dir)), blen-done, int64(len(buf)))
			rn, rerr := sf.ReadAt(buf[:n], off+done)
			if int64(rn) < n {
				if rerr == nil || errors.Is(rerr, io.EOF) {
					return false, fmt.Errorf("%w：偏移 %d 处源文件已结束", errPFTrSourceChanged, off+done+int64(rn))
				}
				return false, r.srcErr("读取源文件", rerr)
			}
			if err := m.waitLimit(j.ctx, spec.Dir, rn); err != nil {
				if c := r.canceled(); c != nil {
					return false, c
				}
				return false, r.newErr(PFErrLocal, spec.Src, "限速等待失败", err)
			}
			wn, werr := pf.WriteAt(buf[:rn], off+done)
			j.sent += int64(wn)
			if werr == nil && wn < rn {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				return false, r.stageErr("写入 part", werr)
			}
			h.Write(buf[:rn])
			done += int64(rn)
		}
		sum := pfTrSumHex(h.Sum(nil))
		if _, err := mf.WriteAt([]byte(sum+"\n"), metaLen); err != nil {
			return false, r.stageErr("记录已确认的块", err)
		}
		metaLen += int64(len(sum)) + 1
		blocks = append(blocks, sum)
		off += blen
		j.confirmed.Store(off)
		if blen >= m.cfg.MinSampleBytes {
			m.speed.Observe(spec.Dir, blen, time.Since(t0))
		}
		if m.cfg.Progress != nil {
			m.cfg.Progress(PFTransferProgress{Job: spec, Attempt: j.attempts, Block: len(blocks) - 1, Done: off, Total: spec.Size})
		}
		if off < spec.Size && m.higherWaiting(j) {
			return true, nil // 让出：part 和 meta 已落好，回头续传
		}
	}
	closed = true
	if err := pf.Close(); err != nil {
		return false, r.stageErr("关闭 part", err)
	}
	return false, nil
}
