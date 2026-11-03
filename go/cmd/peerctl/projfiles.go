package main

// projfiles-poll / projfiles-facts / projfiles-apply：项目文件同步的 Ubuntu 侧协议子命令（Mac 的 wgtund 经 SSH 调用）。
// 写法同 projsync.go：stdin 读一个 JSON 请求（限长、严格解码、不许未知字段、带协议版本号），stdout 写一个 JSON 响应；
// 退出码 0 = 协议层成功（请求读得懂但这次不做的，原因在响应的 error 字段里，且保证没写过），非 0 = 协议层失败
// （输入坏了、版本不符、内部错误），此时 stdout 无输出、原因写 stderr。
// 全部逻辑在 projsync 包的 PFRemoteServe（白名单、租约、扫描哈希、写原语）；这里只管 stdin/stdout。
// 不读环境里的其它配置，不访问网络；meta、根的位置由请求带来，并对照 <meta>/roots.json 白名单。

import (
	"context"
	"fmt"
	"io"

	"github.com/WillyChen0811/base/go/projsync"
)

func cmdProjfiles(sub string, stdin io.Reader, stdout io.Writer) error {
	in, err := io.ReadAll(io.LimitReader(stdin, projsync.PFRemoteMaxInput+1))
	if err != nil {
		return fmt.Errorf("读请求失败: %w", err)
	}
	if len(in) > projsync.PFRemoteMaxInput {
		return fmt.Errorf("请求超过 %d 字节上限", projsync.PFRemoteMaxInput)
	}
	out, err := projsync.PFRemoteServe(context.Background(), sub, in)
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(out, '\n'))
	return err
}
