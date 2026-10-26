//go:build darwin

package projsync

import (
	"fmt"
	"io/fs"
	"syscall"
)

// pfFillSys 补全 PFStat 里 os.FileInfo 给不出的字段：ctime、ino、dev，
// 以及 darwin 才有的 st_flags（SF_DATALESS 判 iCloud 占位要用）。
func pfFillSys(fi fs.FileInfo, st *PFStat) error {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || sys == nil {
		return fmt.Errorf("取不到 syscall.Stat_t")
	}
	st.MtimeNs = int64(sys.Mtimespec.Sec)*1e9 + int64(sys.Mtimespec.Nsec)
	st.CtimeNs = int64(sys.Ctimespec.Sec)*1e9 + int64(sys.Ctimespec.Nsec)
	st.Ino = sys.Ino
	st.Dev = uint64(uint32(sys.Dev))
	st.Flags = sys.Flags
	return nil
}
