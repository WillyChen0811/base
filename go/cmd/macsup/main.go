// macsup：在 Ubuntu 上调用 Mac 协助服务的远端接口。
package main

import "os"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
