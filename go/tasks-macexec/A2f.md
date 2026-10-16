# A2f 修 paths_test.go 的假阳性（A2 评审 P2）
paths_test.go:146 附近：`.GIT/config`、`.Git/config`、`ALIAS/config` 在大小写敏感的文件系统上根本不存在，EvalSymlinks 报错也会让测试通过，覆盖不到「大小写变体」这条安全规则。
修法：为这些输入创建实际存在的目录/符号链接（如真的建一个名为 `.GIT` 的目录或指向 .git 的链接），先断言路径可解析（os.Stat 成功），再断言 ResolveInRepo 拒绝。只改测试文件。
验收：cd go && go test ./macexec/ -run Path。不要 git commit。
