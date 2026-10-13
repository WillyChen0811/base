package projsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// tmpPrefix 是本包写入时用的临时文件名前缀，List 会跳过它。
const tmpPrefix = ".projsync-tmp-"

// validRel 校验相对路径：非空、不以 / 开头、不含 \、按 / 切开后
// 每段非空且不是 . 或 ..。不合规报错，绝不访问根目录之外。
func validRel(p string) error {
	if p == "" {
		return errors.New("projsync: 路径不能为空")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("projsync: 路径 %q 不能以 / 开头", p)
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("projsync: 路径 %q 不能含反斜杠", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("projsync: 路径 %q 含非法段 %q", p, seg)
		}
	}
	return nil
}

// LocalFS 是本地目录树。Root 不存在时 List 返回空列表、不报错。
type LocalFS struct {
	Root string
}

// full 把已校验的相对路径拼到根目录下（只在 validRel 通过后调用）。
func (l *LocalFS) full(path string) string {
	return filepath.Join(l.Root, filepath.FromSlash(path))
}

// List 递归列出所有普通文件（跳过符号链接、目录和其他特殊文件），
// 也跳过 .projsync-tmp- 开头的临时文件。路径用 / 分隔的相对路径。
// 根目录不存在时返回 nil, nil。
func (l *LocalFS) List(ctx context.Context) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fi, err := os.Stat(l.Root)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("projsync: 访问根目录 %s 失败: %w", l.Root, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("projsync: 根目录 %s 不是目录", l.Root)
	}
	var out []Entry
	err = filepath.WalkDir(l.Root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil // 跳过目录和符号链接（WalkDir 也不跟进符号链接目录）
		}
		if strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil // 跳过本包写入用的临时文件
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(l.Root, path)
		if err != nil {
			return err
		}
		out = append(out, Entry{
			Path:    filepath.ToSlash(rel),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("projsync: 遍历 %s 失败: %w", l.Root, err)
	}
	return out, nil
}

// Hash 返回文件前 n 字节的 SHA-256（小写十六进制）。
// n==0 时返回空串的 SHA-256；文件短于 n 字节时报错。
func (l *LocalFS) Hash(ctx context.Context, path string, n int64) (string, error) {
	if err := validRel(path); err != nil {
		return "", err
	}
	if n < 0 {
		return "", fmt.Errorf("projsync: Hash 长度不能为负: %d", n)
	}
	if n == 0 {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:]), nil
	}
	f, err := os.Open(l.full(path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	var got int64
	for got < n {
		want := n - got
		if int64(len(buf)) > want {
			buf = buf[:want]
		}
		nr, err := f.Read(buf)
		got += int64(nr)
		if nr > 0 {
			h.Write(buf[:nr])
		}
		if err != nil {
			if err == io.EOF {
				if got < n {
					return "", fmt.Errorf("projsync: 文件 %q 不足 %d 字节（实际 %d）", path, n, got)
				}
				break
			}
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Read 打开文件，只读前 n 字节；文件短于 n 字节时读到末尾返回 io.ErrUnexpectedEOF
// （不能悄悄少给）。
func (l *LocalFS) Read(ctx context.Context, path string, n int64) (io.ReadCloser, error) {
	if err := validRel(path); err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("projsync: Read 长度不能为负: %d", n)
	}
	f, err := os.Open(l.full(path))
	if err != nil {
		return nil, err
	}
	return &prefixReader{base: f, remaining: n}, nil
}

// prefixReader 限制最多读 n 字节；文件短于 n 时，读到末尾返回 io.ErrUnexpectedEOF。
type prefixReader struct {
	base      io.ReadCloser
	remaining int64
}

func (r *prefixReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.base.Read(p)
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *prefixReader) Close() error {
	return r.base.Close()
}

// Write 原子写入：按需建父目录（0700），先写同目录临时文件（0600）再改名覆盖，
// 最后把修改时间设成 mtime；任何一步失败删掉临时文件并返回错误。
func (l *LocalFS) Write(ctx context.Context, path string, r io.Reader, mtime time.Time) error {
	if err := validRel(path); err != nil {
		return err
	}
	full := l.full(path)
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("projsync: 建父目录 %s 失败: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return fmt.Errorf("projsync: 建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("projsync: 写临时文件失败: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("projsync: 设临时文件权限失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("projsync: 同步临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("projsync: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("projsync: 改名覆盖 %s 失败: %w", full, err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		return fmt.Errorf("projsync: 设置修改时间失败: %w", err)
	}
	return nil
}

// SyncDirs 在 local 和 remote 之间双向补齐文件：
// 只一边有 → 拷到另一边；两边大小相同且哈希相同 → 跳过（Same++）；
// 一边是另一边的前缀 → 用长的覆盖短的；其余 → 冲突（两边都不动）。
// 单个文件的 Hash/Read/Write 出错记入 Errors，不中断整体；永不删除任何文件。
// 路径按字典序处理，结果切片也是字典序；ctx 取消时停止并返回 ctx.Err()。
func SyncDirs(ctx context.Context, local, remote FS) (FileResult, error) {
	res := FileResult{
		Uploaded:   []string{},
		Downloaded: []string{},
		Conflicts:  []Conflict{},
		Errors:     []string{},
	}
	localEntries, err := local.List(ctx)
	if err != nil {
		return res, err
	}
	remoteEntries, err := remote.List(ctx)
	if err != nil {
		return res, err
	}

	// 按路径合并两边列表
	type pair struct{ local, remote *Entry }
	byPath := make(map[string]*pair, len(localEntries)+len(remoteEntries))
	for i := range localEntries {
		e := localEntries[i]
		p := byPath[e.Path]
		if p == nil {
			p = &pair{}
			byPath[e.Path] = p
		}
		p.local = &e
	}
	for i := range remoteEntries {
		e := remoteEntries[i]
		p := byPath[e.Path]
		if p == nil {
			p = &pair{}
			byPath[e.Path] = p
		}
		p.remote = &e
	}

	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		both := byPath[p]
		switch {
		case both.local != nil && both.remote == nil:
			// 只本机有 → 上传（用本机的修改时间）
			if err := copyFile(ctx, local, remote, p, both.local); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
				continue
			}
			res.Uploaded = append(res.Uploaded, p)
		case both.remote != nil && both.local == nil:
			// 只远端有 → 下载
			if err := copyFile(ctx, remote, local, p, both.remote); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
				continue
			}
			res.Downloaded = append(res.Downloaded, p)
		default:
			// 两边都有
			if both.local.Size == both.remote.Size {
				hl, err := local.Hash(ctx, p, both.local.Size)
				if err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
					continue
				}
				hr, err := remote.Hash(ctx, p, both.remote.Size)
				if err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
					continue
				}
				if hl == hr {
					res.Same++
				} else {
					res.Conflicts = append(res.Conflicts, Conflict{
						Path:       p,
						LocalSize:  both.local.Size,
						RemoteSize: both.remote.Size,
						Reason:     "两边大小相同但内容不同",
					})
				}
			} else {
				// 设短的一边为 shortFS：长边前 s 字节与短边相同 → 长边覆盖短边
				var shortFS, longFS FS
				var shortE, longE *Entry
				toDownload := both.local.Size < both.remote.Size
				if toDownload {
					shortFS, shortE = local, both.local
					longFS, longE = remote, both.remote
				} else {
					shortFS, shortE = remote, both.remote
					longFS, longE = local, both.local
				}
				hs, err := shortFS.Hash(ctx, p, shortE.Size)
				if err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
					continue
				}
				hl, err := longFS.Hash(ctx, p, shortE.Size)
				if err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
					continue
				}
				if hs == hl {
					if err := copyFile(ctx, longFS, shortFS, p, longE); err != nil {
						res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
						continue
					}
					if toDownload {
						res.Downloaded = append(res.Downloaded, p)
					} else {
						res.Uploaded = append(res.Uploaded, p)
					}
				} else {
					res.Conflicts = append(res.Conflicts, Conflict{
						Path:       p,
						LocalSize:  both.local.Size,
						RemoteSize: both.remote.Size,
						Reason:     "两边都有对方没有的改动",
					})
				}
			}
		}
	}
	return res, nil
}

// copyFile 从 src 读 path 的全长内容写到 dst，修改时间保留 src 的。
func copyFile(ctx context.Context, src, dst FS, path string, e *Entry) error {
	r, err := src.Read(ctx, path, e.Size)
	if err != nil {
		return err
	}
	defer r.Close()
	return dst.Write(ctx, path, r, e.ModTime)
}
