//go:build linux

package projsync

import (
	"fmt"
	"io/fs"
	"syscall"
)

// pfFillSys 补全 PFStat 里 os.FileInfo 给不出的字段：ctime、ino、dev。
// Linux 没有 st_flags，Flags 恒为 0（iCloud 占位只会出现在 Mac 上）。
func pfFillSys(fi fs.FileInfo, st *PFStat) error {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || sys == nil {
		return fmt.Errorf("取不到 syscall.Stat_t")
	}
	st.MtimeNs = int64(sys.Mtim.Sec)*1e9 + int64(sys.Mtim.Nsec)
	st.CtimeNs = int64(sys.Ctim.Sec)*1e9 + int64(sys.Ctim.Nsec)
	st.Ino = sys.Ino
	st.Dev = uint64(sys.Dev)
	return nil
}
