# jsbase

零业务依赖的 JS/Node 基础模块集，与 `py/`（pybase）对称。

## 模块一览

- `jsbase/electron`：Electron 版本 ↔ 能力台账，第 1 层——从官方 `electron-api.json`
  提取精简、可 diff 的 API 快照。

### Electron API 快照（三层设计的第 1 层）

背景：Electron 每个版本的 API 面貌散落在 `electron-api.json`（机器可读，但字段冗余、
不便 diff）、breaking-changes 文档、API History 标注里，实测才知道的坑更是完全没地方
记。整套台账分三层搭：

1. **快照层（本目录，已实现）**：纯机械地把 `electron-api.json` 里关心的模块/类，
   提取成精简、key 排序、可逐字节复现的 JSON。只回答"这个版本有什么"。
2. **断代层（未实现）**：结合 breaking-changes 和 API History，回答"两个版本之间
   变了什么、什么时候变的"。
3. **实测层（未实现）**：手写记录的实测坑，回答"文档没说但踩过的问题"。

第 2、3 层都要挂在第 1 层产出的 key 上（`ClassName#method` 这种形式），所以快照的
key 规则一旦定下不要轻易改，改了要连带处理已有的挂靠数据。

#### 用法

```bash
# 缺 raw 会自动用 gh cli 下载（需要已登录 gh，从 electron/electron 的 release 附件取）
node electron/extract.js 43.3.0

# 比较两个版本的快照，输出新增/删除/变更的 key
node electron/diff.js 43.3.0 44.0.0
```

- `electron/modules.js`：收录哪些模块/类的清单，改这份，不要改提取逻辑。
- `electron/raw/<version>.json`：下载下来的原始 `electron-api.json`（`.gitignore`
  排除，不入库）。
- `electron/snapshots/<version>.json`：入库的快照产出，key 排序、固定缩进。

#### key 规则

| 场景 | 形式 | 例子 |
|---|---|---|
| 模块方法/属性 | `name.member` | `app.getPath` |
| 模块事件 | `name@event-name` | `app@will-finish-launching` |
| 类实例方法/属性 | `ClassName#member` | `Session#setCertificateVerifyProc` |
| 类实例事件 | `ClassName@event-name` | `WebContents@console-message` |
| 类静态方法/属性 | `ClassName.member` | `BrowserWindow.getAllWindows` |

方法和属性共用同一个分隔符是因为摸底确认过收录范围内没有同名的方法+属性组合；
事件单独用 `@`，避免和同名方法（如 `close()` 方法 / `close` 事件）撞 key。

## 测试

```bash
cd js && node --test "tests/**/*.test.js"
```

## 安装

本地开发：这个包目前不发布，直接在 `js/` 里跑脚本即可（无运行时依赖，全部用
Node 内置模块）。`electron` 只作为 `peerDependencies` 声明，不会被安装——写成
`dependencies` 会再拖一份几百 MB 的 Electron 下来，而且版本不一定和调用方一致。
