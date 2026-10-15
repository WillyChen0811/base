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

---
## 新需求（用户 2026-10-15）：两边同时编码不分叉的全面同步
用户原话要点：项目同步的目的是「两边同时编码防止出现分支，利用 XXYI 跟 Ubuntu 接近 root 的权限，对代码、git 数据、项目记忆、日志进行全面同步」。现有实现只交换已提交的分支到对方命名空间并报告领先/落后、对话目录双向补齐、手动触发，不能防止分叉，也不同步日志。

### 用户已定
1. **日志全部同步**：XXYI/wgtund/钩子日志、Claude Code 会话记录、Codex 会话记录、本地模型（omlx-agent / omlx 会话）日志，都纳入。
2. **范围**：xxyi、base、ai-browser、websubmit、bioflow、biogateway。Mac 一律用 ~/Documents/codes/ 下的仓库（注意 Mac 上 base 现有两份：~/xxyi-work/base 与 ~/Documents/codes/base，xxyi 在 ~/xxyi-work/xxyi，需与用户确认最终以哪份为准）。
3. **分支合并交给 agent 做**，不追求一套自动合并算法（用户：「应该很难设计完美的方案」）。XXYI 负责发现分叉、把两边的提交都备齐、把合并任务派给 agent，并把结果交回。
4. **不丢数据是硬要求**：任何同步、合并、失败、重试都不能让任何一边已有的内容（已提交、未提交、未跟踪、对话记录、日志）丢失或被覆盖。

### 用户补充（10/15，覆盖下面草案里的对应问题）
5. **能快进就直接同步，不阻止**：没有分叉，或者合并结果能直接判断不影响（例如一边是另一边的祖先、或改动互不重叠能干净合并），XXYI 直接同步，不需要人或 agent 介入。
6. **没提交的改动由那台设备自己提交**：用该设备上的权限提交，具体怎么提交（提交信息、拆不拆、哪些未跟踪文件该提交、哪些该忽略）让 agent 设计。不再另做工作区快照这类保全机制。
7. **只要有提交、有轨迹，就允许直接合并**：数据安全靠「所有改动都进了提交历史」保证；合并不需要再额外冗余备份（用户：打包好的程序另外保存，发布的版本包也保留一段时间，「所以不要过分冗余」）。真正有冲突、合并不了的才交给 agent 处理。

### 设计草案要回答的问题（待用户定，已按上面补充收窄）
- 「由那台设备的 agent 提交」：触发时机（同步前自动派给该设备的 agent？）、该设备上没有空闲 agent 时怎么办、提交前要不要先让用户看一眼。
- 真冲突时谁来合并、在哪台机器、哪个工作区（建议另开 worktree，不动正在干活的工作区）；agent 也解决不了时交给用户的方式。
- 同步时机：手动、每次 kind=code 消息、定时、还是两边任何提交后自动触发。
- 日志与会话记录的合并规则：沿用「只追加则取长、两边都改过则冲突不动」，还是日志按时间合并成一份。
- 和消息代码同步（support.repos，单向 ff-only）的关系：合并成一套，还是保留两套。

### 用户补充（10/15）：需要 agent 介入时由用户指定 agent
omlx 将来是用户的常驻 agent，但现在还没设计好。所以现阶段凡是需要 agent 介入的（未提交改动的提交、真冲突的合并等），**由用户指定用哪个 agent**；XXYI 要有「指定 agent」的能力——类似现在指挥派 omlx 编码、派 Codex 审查那样，按任务类型（提交、合并、审查……）选定执行的会话。待设计：在哪里指定（设置里按任务类型配置默认 agent，还是每次触发时选）、指定的 agent 不在线/忙时怎么办、能否指定「新开一个会话」。
