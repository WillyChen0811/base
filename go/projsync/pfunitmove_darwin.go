//go:build darwin

package projsync

// 开始/取消同步（pfunitmove.go）在 macOS 上用到的系统调用。iCloud 是 Mac 的事，这套只在 macOS 上做。
// 不覆盖的改名用 pfswap_darwin.go 的 pfRenameNoReplace（renamex_np RENAME_EXCL）。

import "golang.org/x/sys/unix"

// pfumPlatformErr 在支持的平台上返回 nil。
func pfumPlatformErr() error { return nil }

// pfumUnlink 只删 path 这一个名字（unlink(2)）：对符号链接删的是链接本身，不碰它指向的东西。
// 对目录它会失败（EPERM/EISDIR），绝不递归、也不会像 os.Remove 那样退而求其次去 rmdir 一个空目录。
func pfumUnlink(path string) error { return unix.Unlink(path) }
