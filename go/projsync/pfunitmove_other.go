//go:build !darwin

package projsync

// 开始/取消同步只支持 macOS；其他平台上 NewPFUnitMover 和 PFUnitMoveValidateConfig 返回「不支持」，
// 不会走到下面的 pfumUnlink。

import (
	"errors"
	"runtime"
)

var errPFUnitMoveUnsupported = errors.New("开始/取消同步只支持 macOS（当前 " + runtime.GOOS + "）")

func pfumPlatformErr() error { return errPFUnitMoveUnsupported }

func pfumUnlink(path string) error { return errPFUnitMoveUnsupported }
