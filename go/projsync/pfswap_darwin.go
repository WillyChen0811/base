//go:build darwin

package projsync

// 项目文件同步写原语在 macOS 上用到的系统调用（技术设计 §1.2）：
//   - 不覆盖的改名：renamex_np(RENAME_EXCL)，目标已存在就报 EEXIST；
//   - 原子交换：renamex_np(RENAME_SWAP)，两个路径同时换位，APFS 支持；
//   - 写时复制：clonefile，同一卷上复制大文件几乎不占空间、不花时间；
//   - 以 O_NOFOLLOW 打开、检查可写（access）。
// 本文件只管「调系统」，不判断业务；业务规矩在 pfapply.go。

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// pfCanClone 报告本平台是否有 clonefile 可用。
const pfCanClone = true

// pfRenameNoReplace 把 oldPath 改名为 newPath；newPath 已存在时失败（EEXIST），绝不覆盖。
func pfRenameNoReplace(oldPath, newPath string) error {
	return unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
}

// pfRenameExchange 原子地交换 a、b 两个路径指向的文件，两个都必须存在。
func pfRenameExchange(a, b string) error {
	return unix.RenamexNp(a, b, unix.RENAME_SWAP)
}

// pfCloneFile 用 clonefile 把 src 复制成 dst（dst 必须不存在；不跟随符号链接）。
func pfCloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}

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
