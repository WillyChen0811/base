//go:build darwin || linux

package projsync

import "syscall"

// pfrDiskSpace 返回 path 所在文件系统的剩余空间（普通用户可用）和总空间。darwin、linux 的 Statfs_t 字段类型不同，统一转成 uint64。
func pfrDiskSpace(path string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1, -1, err
	}
	bs := uint64(st.Bsize)
	return int64(uint64(st.Bavail) * bs), int64(uint64(st.Blocks) * bs), nil
}
