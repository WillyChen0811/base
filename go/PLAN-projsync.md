# 计划：项目同步（Mac 与 Ubuntu 两边同时开工时，交换代码和 Claude 对话记录）

## 目标
用户在 Mac 的 xxyi 里点「同步」（或 Ubuntu 上的 Claude 经协助通道发起、用户在 xxyi 点允许），
由 Mac 上的 wgtund 自己执行（不经过受限的协助 Claude），经主机的当前线路和 Ubuntu 交换：
1. **代码**：只交换已提交的内容，不碰任何一边的工作区和本地分支。
   - Mac 的 `refs/heads/*` 推到 Ubuntu 的 `refs/remotes/mac/*`（`--prune`，强推，这个命名空间只归 Mac 管）；
   - Ubuntu 的 `refs/heads/*` 取回 Mac 的 `refs/remotes/ubuntu/*`（`--prune`）；
   - 报告每个本地分支与对方同名分支的领先/落后提交数。合并由人决定。
2. **对话记录**：Claude Code 的项目目录（会话 jsonl、会话子目录、memory/）双向补齐。
   - 只有一边有 → 拷到另一边；
   - 两边相同 → 跳过；
   - 一边是另一边的前缀（会话只追加）→ 用长的覆盖短的；
   - 其他（两边都改过）→ 冲突，两边都不动，报告出来；
   - 永不删除；只处理普通文件，跳过符号链接；写入先写临时文件再改名，保留源文件修改时间。
   - 目录名按 Claude Code 规则从项目路径推出：路径里每个非字母数字字符换成 `-`
     （`/Users/weiyichen/Documents/codes/ai-browser` → `-Users-weiyichen-Documents-codes-ai-browser`）。

## 配置（hosts.json 新增可选段）
```json
"projsync": {
  "host": "ubuntu",
  "projects": [
    {"name": "ai-browser",
     "local": "~/Documents/codes/ai-browser",
     "remote": "/home/chenwy/Documents/codes/ai-browser"}
  ]
}
```
可选 `local_claude` / `remote_claude` 覆盖推出来的对话目录；`remote_claude` 是相对远端登录目录的路径（默认 `.claude/projects/<推出的名字>`）。
git 远端地址由 wgtund 拼：`ssh://<host.user>@<当前线路地址>/<remote>`，经线路转发口走，严格校验主机密钥，私钥用主机的第一个 identity_files。

## 结构
- `go/projsync`（新包，无网络依赖，可单测）
  - `files.go`：`FS` 接口（List / Hash(前 n 字节) / Read(前 n 字节) / Write(原子写+设修改时间)）、`LocalFS`、`SyncDirs(ctx, local, remote FS) (FileResult, error)`。
  - `git.go`：`SyncGit(ctx, dir, remoteURL, env, localName, remoteName) (GitResult, error)`。
  - `names.go`：`ClaudeDirName(path)`。
- `go/projsync/sftpfs.go`：远端 `FS`，文件操作走 pkg/sftp（BSD-2），Hash 走 ssh 执行 `head -c N | sha256sum`。
- `go/sshterm`：`Client.SFTP()`。
- `go/wgtund`：读 `projsync` 段；本机接口 `GET /projsync`、`POST /projsync/run?name=`（要求允许的 Origin）；
  给 support 远端入口加 `POST /projsync?name=`：进待批队列（工具名 `ProjectSync`），用户允许后执行，返回结果。
- `go/cmd/macsup`：`macsup projsync <名字>`。
- xxyi：协助页加「项目同步」卡片（项目列表、同步按钮、上次结果、冲突列表）。

## 任务与执行者
| # | 内容 | 执行 | 评审 |
|---|---|---|---|
| P1 | projsync/files.go + names.go + 测试 | Flash Next | Sonnet |
| P2 | projsync/git.go + 测试（本地临时仓库） | 35B | Sonnet |
| P3 | sftpfs.go + sshterm.Client.SFTP + 测试（pkg/sftp 进程内服务端） | Flash Next | Sonnet |
| P4 | wgtund 接线 + support 远端接口 + macsup projsync + README | Flash Next | Sonnet |
| P5 | xxyi 协助页「项目同步」卡片 + 浏览器测试 | Flash Next | Sonnet |
| P6 | Ubuntu 端到端（testenv sshd） | Claude | — |
| P7 | Mac 实测：同步 ai-browser | 用户 + Claude | — |
本地模型写代码并自写测试；Sonnet 评审（每条意见带改法）；作者按意见改；本地模型两次做不好升级 Sonnet；Claude 复核并提交。
