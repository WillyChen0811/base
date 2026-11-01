//go:build !darwin && !linux

package projsync

import "errors"

// pfrDiskSpace 在 darwin、linux 以外的平台取不到（项目文件同步本来就只支持这两个平台，见 pfscan_other.go）。
func pfrDiskSpace(path string) (free, total int64, err error) {
	return -1, -1, errors.New("本平台取不到磁盘剩余空间")
}
