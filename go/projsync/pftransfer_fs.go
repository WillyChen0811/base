package projsync

// 本文件管「传输层看到的文件系统」：把本机和远端（SFTP）抽象成同一个接口，
// 传输、续传、核验、清理都只依赖这个接口，生产用 *sftp.Client（PFSFTP 包一层），
// 测试用进程内 SFTP 服务或 PFLocalFS。
//
// 边界：这里只有读写字节和目录操作，不懂暂存区、续传、哈希的规矩（在 pftransfer*.go 里）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
)

// PFTransferFile 是传输层要用的已打开文件：按偏移读写、截断、关闭。
type PFTransferFile interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
	Truncate(size int64) error
}

// PFTransferFS 是传输层用的文件系统。路径都是这台机器上的完整路径；
// 本机用 filepath 规则，远端用 / 分隔，所以拼路径走 Join，不要在外面自己拼。
type PFTransferFS interface {
	// Open 只读打开；文件不存在要返回能用 errors.Is(err, os.ErrNotExist) 认出的错误。
	Open(p string) (PFTransferFile, error)
	// OpenFile 按 os.O_* 标志打开（用到 O_WRONLY、O_CREATE、O_TRUNC）。
	OpenFile(p string, flag int) (PFTransferFile, error)
	Stat(p string) (os.FileInfo, error)
	Remove(p string) error
	MkdirAll(p string) error
	ReadDir(p string) ([]os.FileInfo, error)
	Chtimes(p string, atime, mtime time.Time) error
	Join(elem ...string) string
}

// ── 本机 ────────────────────────────────────────────────

// PFLocalFS 用 os 包实现 PFTransferFS。
type PFLocalFS struct{}

func (PFLocalFS) Open(p string) (PFTransferFile, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (PFLocalFS) OpenFile(p string, flag int) (PFTransferFile, error) {
	f, err := os.OpenFile(p, flag, 0o600)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (PFLocalFS) Stat(p string) (os.FileInfo, error) { return os.Stat(p) }
func (PFLocalFS) Remove(p string) error              { return os.Remove(p) }
func (PFLocalFS) MkdirAll(p string) error            { return os.MkdirAll(p, 0o700) }

func (PFLocalFS) ReadDir(p string) ([]os.FileInfo, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(ents))
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // 列目录和取信息之间被删了
			}
			return nil, err
		}
		out = append(out, fi)
	}
	return out, nil
}

func (PFLocalFS) Chtimes(p string, atime, mtime time.Time) error {
	return os.Chtimes(p, atime, mtime)
}

func (PFLocalFS) Join(elem ...string) string { return filepath.Join(elem...) }

// ── 远端（SFTP）────────────────────────────────────────

type pfSFTPFS struct{ c *sftp.Client }

// PFSFTP 把 *sftp.Client 包成 PFTransferFS。
//
// 想让大块写得快（高延迟线路），创建 sftp.Client 时加 sftp.UseConcurrentWrites(true)；
// 传输层每次写 IOChunk（缺省 1MB）大的缓冲区，客户端会把它拆成多个包并发。
// 并发写出错时可能在后面留下空洞，传输层只信「已确认块」之内的内容，续传时会截掉后面的，不受影响。
func PFSFTP(c *sftp.Client) PFTransferFS { return pfSFTPFS{c: c} }

func (s pfSFTPFS) Open(p string) (PFTransferFile, error) {
	f, err := s.c.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s pfSFTPFS) OpenFile(p string, flag int) (PFTransferFile, error) {
	f, err := s.c.OpenFile(p, flag)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s pfSFTPFS) Stat(p string) (os.FileInfo, error)      { return s.c.Stat(p) }
func (s pfSFTPFS) Remove(p string) error                   { return s.c.Remove(p) }
func (s pfSFTPFS) MkdirAll(p string) error                 { return s.c.MkdirAll(p) }
func (s pfSFTPFS) ReadDir(p string) ([]os.FileInfo, error) { return s.c.ReadDir(p) }
func (s pfSFTPFS) Chtimes(p string, a, m time.Time) error  { return s.c.Chtimes(p, a, m) }
func (s pfSFTPFS) Join(elem ...string) string              { return path.Join(elem...) }

// ── 目标端核验 ──────────────────────────────────────────

// PFTargetVerify 是「目标端核验」：在**目标机器本机**对 partPath 整个文件算 sha256。
//
// 约定（传输层和实现方都要遵守）：
//  1. partPath 是目标机上的完整路径；上传方向目标是 Ubuntu（实现方在 Ubuntu 本机算，
//     不许把文件拉回 Mac 再算），下载方向目标是本机（用 PFHashLocalFile 即可）。
//  2. 只报告「算出来是什么」，不和期望值比较：返回小写十六进制 sum 和读到的字节数 size，
//     比较由传输层做。返回了 sum 就表示整个文件已读完。
//  3. 算不出来（通信失败、超时、文件读不了、文件不存在）一律返回 error：
//     传输层保留 part，把错误原样带给调用方，不当作「不符」，也不盲目重试。
//  4. 要能响应 ctx 取消。
type PFTargetVerify func(ctx context.Context, partPath string) (sum string, size int64, err error)

// PFHashLocalFile 在本机算文件的 sha256，满足 PFTargetVerify，下载方向直接用它。
func PFHashLocalFile(ctx context.Context, p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, ctxReader{ctx, f})
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
