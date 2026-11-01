package projsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// 项目文件同步（pf*）共用的落盘工具：原子写、追加一行并 fsync、目录 fsync。
//
// 写法参照 quiet.go 的 Save（临时文件 + fsync + 改名），补上 quiet.go 没做的两件事：
// 改名之后对目录做 fsync（否则断电后改名可能丢），权限可参数化。quiet.go 本身不动，以后再统一。
//
// 本文件只管「把字节安全地放到磁盘上」，不认识 base、哈希缓存、状态文件的内容。
// 边界：
//   - 不跨进程加锁。同一个目标路径同一时间只能有一个写者（调用方持单元锁）。
//   - 只保证「写完整或没写」。断电时文件系统自己撕裂数据块的情况，靠调用方的校验和发现。
//   - 目标目录与临时文件同一个目录，所以改名不会跨文件系统。

// pfDefaultPerm 是调用方没指定权限时文件用的权限。
const pfDefaultPerm fs.FileMode = 0o600

// pfDirPerm 是本文件替调用方新建目录时用的权限。
const pfDirPerm fs.FileMode = 0o700

// pfHook 是崩溃注入钩子，只给测试用：写入过程每过一步就以步骤名调用一次。
// 返回非 nil 表示「进程恰好在这一步之后死掉」：函数立刻带着这个 error 返回，
// 不做任何清理（临时文件留在原地），磁盘停在真实崩溃时会留下的样子。
// 生产代码传 nil。
type pfHook func(step string) error

// pfStep 在钩子不为空时调用它。
func pfStep(h pfHook, step string) error {
	if h == nil {
		return nil
	}
	return h(step)
}

// pfPrefixHook 给步骤名加前缀，让同一个钩子能区分是哪次写入里的哪一步。
func pfPrefixHook(h pfHook, prefix string) pfHook {
	if h == nil {
		return nil
	}
	return func(step string) error { return h(prefix + step) }
}

// writeFileAtomic 把 data 原子地写到 path：同目录临时文件 → 写 → fsync 文件 → 改名 → fsync 目录。
// perm 为 0 时用 0600。父目录不存在会建（0700，并对新建的每一级 fsync 其父目录）。
// 任何一步失败都返回 error 并删掉临时文件，path 原有内容不变。
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	_, err := pfWriteAtomic(path, data, perm, nil)
	return err
}

// PFWriteFileAtomic 是 writeFileAtomic 的导出版，给别的包（wgtund 的状态文件等）用。
func PFWriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return writeFileAtomic(path, data, perm)
}

// pfWriteAtomic 是 writeFileAtomic 的完整形式，带钩子，并告诉调用方「改名是否已经发生」。
// renamed=true 且 err!=nil 说明新内容可能已经是 path 的内容、但目录 fsync 没确认：
// 调用方要把这种情况当作「磁盘状态不确定」。
//
// 钩子步骤名依次是：tmp-created、tmp-written、tmp-synced、renamed、dir-synced。
func pfWriteAtomic(path string, data []byte, perm fs.FileMode, hook pfHook) (renamed bool, err error) {
	if perm == 0 {
		perm = pfDefaultPerm
	}
	dir := filepath.Dir(path)
	if err := pfMkdirAllSync(dir, pfDirPerm); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	crashed := false // 钩子模拟崩溃：不清理
	done := false
	defer func() {
		if !done {
			f.Close() // 已关过再关只返回 error，忽略
			if !crashed {
				os.Remove(tmp)
			}
		}
	}()
	fail := func(step string) error {
		if e := pfStep(hook, step); e != nil {
			crashed = true
			return e
		}
		return nil
	}
	if err := f.Chmod(perm); err != nil {
		return false, err
	}
	if err := fail("tmp-created"); err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		return false, err
	}
	if err := fail("tmp-written"); err != nil {
		return false, err
	}
	if err := f.Sync(); err != nil {
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := fail("tmp-synced"); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	done = true // 改名之后临时文件已经不存在，不再清理
	if err := pfStep(hook, "renamed"); err != nil {
		return true, err
	}
	if err := pfSyncDir(dir); err != nil {
		return true, fmt.Errorf("改名后同步目录 %s 失败: %w", dir, err)
	}
	if err := pfStep(hook, "dir-synced"); err != nil {
		return true, err
	}
	return true, nil
}

// appendLineSync 往日志文件末尾追加一行（自动补结尾换行）并 fsync。文件不存在会建（权限 perm，0 则 0600）；
// 新建时还会 fsync 目录，保证文件名本身也落盘。line 里不能含换行。
func appendLineSync(path string, line []byte, perm fs.FileMode) error {
	return appendLinesSync(path, [][]byte{line}, perm, nil)
}

// appendLinesSync 一次写入多行、只 fsync 一次：每行末尾都补换行，行内不能含换行。
// 一次 write 调用写出全部内容，所以崩溃只会让文件停在「某个字节处」，
// 回放时由调用方丢掉写了一半的最后一行。
//
// 写或 fsync 出错后，文件尾可能留着半行：调用方必须把这个日志当作状态不确定，
// 重新加载（加载会丢掉半行）之前不要再往里追加，否则新行会接在半行后面粘成坏行。
//
// 钩子步骤名依次是：append:opened、append:written、append:synced、（新建文件时）append:dir-synced。
func appendLinesSync(path string, lines [][]byte, perm fs.FileMode, hook pfHook) error {
	if perm == 0 {
		perm = pfDefaultPerm
	}
	var buf []byte
	for _, l := range lines {
		for _, c := range l {
			if c == '\n' {
				return errors.New("日志行内不能含换行")
			}
		}
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	if len(buf) == 0 {
		return nil
	}
	dir := filepath.Dir(path)
	if err := pfMkdirAllSync(dir, pfDirPerm); err != nil {
		return err
	}
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, fs.ErrNotExist)
	if statErr != nil && !created {
		return statErr
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, perm)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	if err := pfStep(hook, "append:opened"); err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil {
		return err
	}
	if err := pfStep(hook, "append:written"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	closed = true
	if err := f.Close(); err != nil {
		return err
	}
	if err := pfStep(hook, "append:synced"); err != nil {
		return err
	}
	if created {
		if err := pfSyncDir(dir); err != nil {
			return fmt.Errorf("新建日志后同步目录 %s 失败: %w", dir, err)
		}
		if err := pfStep(hook, "append:dir-synced"); err != nil {
			return err
		}
	}
	return nil
}

// pfSyncDir 对目录做 fsync，让里面刚发生的新建、改名落盘。
// 个别文件系统不支持对目录 fsync（返回 EINVAL/ENOTSUP/ENOTTY），这种情况没有别的办法，忽略；
// 其他错误（如 EIO）照实返回。
func pfSyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOTTY) {
			return nil
		}
		return err
	}
	return nil
}

// pfMkdirAllSync 相当于 os.MkdirAll，但对新建出来的每一级目录，fsync 它的父目录，
// 避免「文件落盘了、它所在的新目录在目录项里却丢了」。
func pfMkdirAllSync(dir string, perm fs.FileMode) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s 存在但不是目录", dir)
		}
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := pfMkdirAllSync(parent, perm); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if parent != dir {
		return pfSyncDir(parent)
	}
	return nil
}
