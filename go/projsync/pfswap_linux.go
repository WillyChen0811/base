//go:build linux

package projsync

// 项目文件同步写原语在 Linux 上用到的系统调用（技术设计 §1.2）：
//   - 不覆盖的改名：renameat2(RENAME_NOREPLACE)，目标已存在就报 EEXIST；
//   - 原子交换：renameat2(RENAME_EXCHANGE)，两个路径同时换位；
//   - 没有 clonefile：本平台复制一律走普通复制（pfCanClone 为 false）；
//   - 以 O_NOFOLLOW 打开、检查可写（access）。
// 文件系统不支持这两个标志时内核返回 EINVAL（个别网络盘返回 ENOTSUP），上层当「不支持」处理，
// 不退回成先删后改名之类会丢数据的做法。
// 本文件只管「调系统」，不判断业务；业务规矩在 pfapply.go。

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// pfCanClone 报告本平台是否有 clonefile 可用。
const pfCanClone = false

// pfRenameNoReplace 把 oldPath 改名为 newPath；newPath 已存在时失败（EEXIST），绝不覆盖。
func pfRenameNoReplace(oldPath, newPath string) error {
	return unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
}

// pfRenameExchange 原子地交换 a、b 两个路径指向的文件，两个都必须存在。
func pfRenameExchange(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}

// pfCloneFile 在 Linux 上不可用。
func pfCloneFile(src, dst string) error { return errPFNoClone }

// pfOpenNoFollow 只读打开，路径最后一段是符号链接时失败（ELOOP）。
func pfOpenNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// pfAccessWritable 检查当前用户能不能写 path；目录同时要求可进入（X_OK）。
func pfAccessWritable(path string, isDir bool) error {
	mode := uint32(unix.W_OK)
	if isDir {
		mode |= unix.X_OK
	}
	return unix.Access(path, mode)
}
