package projsync

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/tmux"
	"github.com/pkg/sftp"
)

// SFTPFS 是远端的一棵目录树：文件操作走 SFTP，Hash 在远端执行命令算（省得把大文件拉回来）。
type SFTPFS struct {
	Client *sftp.Client
	Root   string                                                // 远端根目录；相对路径相对远端登录目录
	Run    func(ctx context.Context, cmd string) ([]byte, error) // 在远端执行 shell 命令，返回标准输出
}

// ── 路径校验 ────────────────────────────────────────────

// validRelSFTP 校验相对路径：非空、不以 / 开头、不含 \、没有空段/./..。
func validRelSFTP(p string) error {
	if p == "" {
		return errors.New("路径不能为空")
	}
	if strings.HasPrefix(p, "/") {
		return errors.New("路径不能以 / 开头")
	}
	if strings.Contains(p, "\\") {
		return errors.New("路径不能含 \\")
	}
	segments := strings.Split(p, "/")
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("路径段不合法: %q", seg)
		}
	}
	return nil
}

// remotePath 将远端根目录和相对路径拼成完整路径。
func (fs *SFTPFS) remotePath(p string) string {
	return path.Join(fs.Root, p)
}

// ── List ────────────────────────────────────────────────

func (fs *SFTPFS) List(ctx context.Context) ([]Entry, error) {
	fi, err := fs.Client.Stat(fs.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("sftpfs: 列出 %s 失败: %w", fs.Root, err)
	}
	if !fi.IsDir() {
		return nil, nil
	}
	// Walk 对根用 Lstat；根本身是符号链接时先解析成真实路径再走。
	base, err := fs.Client.RealPath(fs.Root)
	if err != nil {
		return nil, fmt.Errorf("sftpfs: 解析 %s 失败: %w", fs.Root, err)
	}
	prefix := strings.TrimSuffix(base, "/") + "/"

	var entries []Entry
	w := fs.Client.Walk(base)
	for w.Step() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if w.Err() != nil {
			// 单个子目录读不了（比如刚被删）也算失败，不悄悄漏文件。
			return nil, fmt.Errorf("sftpfs: 遍历 %s 失败: %w", fs.Root, w.Err())
		}
		fi := w.Stat()
		if !fi.Mode().IsRegular() { // 目录、符号链接和其他特殊文件都不收
			continue
		}
		if strings.HasPrefix(fi.Name(), ".projsync-tmp-") {
			continue
		}
		rel := strings.TrimPrefix(w.Path(), prefix)
		if rel == w.Path() || rel == "" {
			continue
		}
		entries = append(entries, Entry{Path: rel, Size: fi.Size(), ModTime: fi.ModTime()})
	}
	if w.Err() != nil {
		return nil, fmt.Errorf("sftpfs: 遍历 %s 失败: %w", fs.Root, w.Err())
	}
	return entries, nil
}

// ── Hash ────────────────────────────────────────────────

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (fs *SFTPFS) Hash(ctx context.Context, p string, n int64) (string, error) {
	if err := validRelSFTP(p); err != nil {
		return "", err
	}
	if n < 0 {
		return "", errors.New("sftpfs: n 不能为负")
	}
	if n == 0 {
		return emptySHA256, nil
	}
	remote := fs.remotePath(p)

	fi, err := fs.Client.Stat(remote)
	if err != nil {
		return "", fmt.Errorf("sftpfs: 确认 %s 大小失败: %w", p, err)
	}
	if fi.Size() < n {
		return "", fmt.Errorf("sftpfs: 文件 %s 大小 %d 不足 %d", p, fi.Size(), n)
	}

	cmd := fmt.Sprintf("head -c %d -- %s | sha256sum", n, tmux.Quote(remote))
	out, err := fs.Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("sftpfs: 计算 %s 的 hash 失败: %w", p, err)
	}
	fields := bytes.Fields(out)
	if len(fields) < 1 {
		return "", fmt.Errorf("sftpfs: sha256sum 输出解析失败: %q", string(out))
	}
	h := string(fields[0])
	if b, err := hex.DecodeString(h); err != nil || len(b) != 32 || h != strings.ToLower(h) {
		return "", fmt.Errorf("sftpfs: sha256sum 输出格式异常: %q", h)
	}
	return h, nil
}

// ── Read ────────────────────────────────────────────────

// limitedFile 只吐前 n 字节；文件在 n 之前结束就报 io.ErrUnexpectedEOF。
type limitedFile struct {
	f    *sftp.File
	left int64
}

func (l *limitedFile) Read(b []byte) (int, error) {
	if l.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > l.left {
		b = b[:l.left]
	}
	n, err := l.f.Read(b)
	l.left -= int64(n)
	if err == io.EOF && l.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	if err == io.EOF && n == 0 {
		return 0, io.EOF
	}
	if err == io.EOF {
		err = nil
	}
	return n, err
}

func (l *limitedFile) Close() error { return l.f.Close() }

func (fs *SFTPFS) Read(ctx context.Context, p string, n int64) (io.ReadCloser, error) {
	if err := validRelSFTP(p); err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, errors.New("sftpfs: n 不能为负")
	}
	f, err := fs.Client.Open(fs.remotePath(p))
	if err != nil {
		return nil, fmt.Errorf("sftpfs: 打开 %s 失败: %w", p, err)
	}
	return &limitedFile{f: f, left: n}, nil
}

// ── Write ───────────────────────────────────────────────

// ctxReader 在 ctx 取消后让拷贝尽快停下。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

func (fs *SFTPFS) Write(ctx context.Context, p string, r io.Reader, mtime time.Time) (err error) {
	if err := validRelSFTP(p); err != nil {
		return err
	}
	remote := fs.remotePath(p)
	dir := path.Dir(remote)
	if err := fs.Client.MkdirAll(dir); err != nil {
		return fmt.Errorf("sftpfs: 创建目录 %s 失败: %w", dir, err)
	}

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("sftpfs: 生成临时文件名失败: %w", err)
	}
	tmp := path.Join(dir, ".projsync-tmp-"+hex.EncodeToString(rnd[:]))
	f, err := fs.Client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return fmt.Errorf("sftpfs: 创建临时文件失败: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			fs.Client.Remove(tmp)
		}
	}()

	if _, err := io.Copy(f, ctxReader{ctx, r}); err != nil {
		f.Close()
		return fmt.Errorf("sftpfs: 写入 %s 失败: %w", p, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("sftpfs: 关闭临时文件失败: %w", err)
	}
	// 先在临时文件上设好时间和权限，再原子改名，这样目标上不会出现半成品状态。
	if err := fs.Client.Chtimes(tmp, mtime, mtime); err != nil {
		return fmt.Errorf("sftpfs: 设置 %s 修改时间失败: %w", p, err)
	}
	if err := fs.Client.Chmod(tmp, 0600); err != nil {
		return fmt.Errorf("sftpfs: 设置 %s 权限失败: %w", p, err)
	}
	if err := fs.Client.PosixRename(tmp, remote); err != nil {
		return fmt.Errorf("sftpfs: 改名到 %s 失败: %w", p, err)
	}
	renamed = true
	return nil
}

var _ FS = (*SFTPFS)(nil)
