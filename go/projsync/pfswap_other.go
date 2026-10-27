//go:build !darwin && !linux

package projsync

// 项目文件同步的写原语只支持 macOS 和 Linux。其他平台上这些函数一律返回错误：
// 没有原子交换和不覆盖改名就做不到「不丢内容」，宁可整个不支持，也不退回成可能覆盖的做法。

import (
	"errors"
	"os"
)

// errPFUnsupportedPlatform 是其他平台上所有系统调用的错误。
var errPFUnsupportedPlatform = errors.New("项目文件同步的写入只支持 macOS 和 Linux")

const pfCanClone = false

func pfRenameNoReplace(oldPath, newPath string) error { return errPFUnsupportedPlatform }
func pfRenameExchange(a, b string) error              { return errPFUnsupportedPlatform }
func pfCloneFile(src, dst string) error               { return errPFUnsupportedPlatform }
func pfOpenNoFollow(path string) (*os.File, error)    { return nil, errPFUnsupportedPlatform }
func pfAccessWritable(path string, isDir bool) error  { return errPFUnsupportedPlatform }
