# A1f 修 go/macexec/paths.go 的评审问题
评审意见原文：go/tasks-macexec/state 里没有入库，摘要如下（两条都要修）：
1. P1 符号链接可绕过 `.git` 禁止：仓库里有 `alias -> .git` 时传 `alias/config` 会返回 `.git/config`。修法：符号链接解析与边界检查之后，用 `filepath.Rel(realRoot, resolved)` 得到解析后的相对路径，再逐段检查 `.git`。
2. P2 macOS 大小写不敏感卷上 `.GIT/config` 可绕过：输入路径的段与解析后相对路径的段，都用 `strings.EqualFold(seg, ".git")` 判断。
只改 paths.go，不写测试（A2 会覆盖这两种情况）。
验收：cd go && go build ./... && go vet ./macexec/。不要 git commit。
