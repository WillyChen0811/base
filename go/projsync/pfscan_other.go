//go:build !darwin && !linux

package projsync

import (
	"fmt"
	"io/fs"
	"runtime"
)

// pfFillSys 在 darwin、linux 以外的平台不支持：拿不到 ctime、ino 就无法做哈希缓存和指纹，
// 宁可报错（识别边界、超出就停），不猜。
func pfFillSys(fi fs.FileInfo, st *PFStat) error {
	return fmt.Errorf("项目文件同步不支持 %s", runtime.GOOS)
}
