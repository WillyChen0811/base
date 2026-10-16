# C1f 修 actions.go 路径逃逸（C1 评审 P1/P2）
1. P1：编译类动作的工作目录用 `ResolveInRepo(root, "mac")` 取并校验（防 mac 是指向仓库外的符号链接）；build_app 的脚本用 `ResolveInRepo(root, "mac/build.sh")` 解析，要求是普通文件，Argv 用解析后的绝对路径。
2. P2：node_test 解析出的路径要 os.Stat 且 Mode().IsRegular()，否则返回参数错误。
只改 go/macexec/actions.go。验收：cd go && go build ./... && go vet ./macexec/ && go test ./macexec/ -run Action。不要 git commit。
