// Package projsync 在两台机器之间交换一个项目的已提交代码和 Claude Code 对话记录。
// 详见 go/PLAN-projsync.md。
package projsync

import (
	"context"
	"io"
	"time"
)

// Entry 是 FS 里的一个普通文件。
type Entry struct {
	Path    string    // 相对根目录的路径，用 / 分隔，不以 / 开头，不含 . 或 .. 段
	Size    int64     // 字节数
	ModTime time.Time // 修改时间
}

// FS 是一棵目录树（本机目录，或经 SSH 访问的远端目录）。根目录不存在时 List 返回空列表、不报错。
type FS interface {
	// List 递归列出所有普通文件（跳过符号链接、目录本身和其他特殊文件）。
	List(ctx context.Context) ([]Entry, error)
	// Hash 返回文件前 n 字节的 SHA-256（小写十六进制）。文件短于 n 字节时报错。
	Hash(ctx context.Context, path string, n int64) (string, error)
	// Read 打开文件，只读前 n 字节。文件短于 n 字节时，读到末尾报错（不能悄悄少给）。
	Read(ctx context.Context, path string, n int64) (io.ReadCloser, error)
	// Write 原子写入：按需建父目录，先写同目录临时文件再改名覆盖，最后把修改时间设成 mtime。
	Write(ctx context.Context, path string, r io.Reader, mtime time.Time) error
}

// Conflict 是两边都改过、无法自动决定的文件。
type Conflict struct {
	Path       string `json:"path"`
	LocalSize  int64  `json:"local_size"`
	RemoteSize int64  `json:"remote_size"`
	Reason     string `json:"reason"` // 中文说明
}

// FileResult 是一次目录双向补齐的结果。路径都按字典序排列。
type FileResult struct {
	Uploaded   []string   `json:"uploaded"`   // 本机 → 远端（新文件或追加）
	Downloaded []string   `json:"downloaded"` // 远端 → 本机
	Same       int        `json:"same"`       // 两边相同、跳过的文件数
	Conflicts  []Conflict `json:"conflicts"`
	Errors     []string   `json:"errors"` // 单个文件失败不中断整体，记在这里（"路径: 原因"）
}
