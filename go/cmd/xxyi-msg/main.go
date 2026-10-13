// xxyi-msg：Mac 与 Ubuntu 之间互发消息的命令行；跑在哪一侧由配置文件决定。
package main

import "os"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
