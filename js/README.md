# jsbase

零业务依赖的 JS/Node 基础模块集，与 `py/`（pybase）对称。

## 模块一览

- `jsbase/electron`：Electron 版本 ↔ 能力台账，三层设计——第 1 层从官方
  `electron-api.json` 提取精简、可 diff 的 API 快照，第 2 层断代（什么时候变
  的），第 3 层手写能力台账（我想干什么、要用哪些 API、有什么已知的坑）。
- `jsbase/os`：不依赖 Electron 的地基——系统代理、根证书、自签 CA。
- 包入口的 `electron`：console 参数归一化、IPC 路由注册、浏览器实例；
  `net`：HTTP/HTTPS 录制代理。运行时依赖和项目配置由调用方传入。

### Electron API 快照（三层设计的第 1 层）

背景：Electron 每个版本的 API 面貌散落在 `electron-api.json`（机器可读，但字段冗余、
不便 diff）、breaking-changes 文档、API History 标注里，实测才知道的坑更是完全没地方
记。整套台账分三层搭：

1. **快照层（本目录，已实现）**：纯机械地把 `electron-api.json` 里关心的模块/类，
   提取成精简、key 排序、可逐字节复现的 JSON。只回答"这个版本有什么"。
2. **断代层（`electron/history/`，已实现）**：结合 breaking-changes 和 API History，
   回答"什么时候变的"。**不做归类判断**——"这算不算一个能力"是第 3 层的事。
3. **能力台账（`electron/capabilities/`，已实现）**：手写 JSON，机器只校验、
   不生成。回答"文档没说但踩过的问题"——**这一层才做归类判断**（"这算不算一个
   能力"），前两层都不做。

第 2、3 层都要挂在第 1 层产出的 key 上（`ClassName#method` 这种形式），所以快照的
key 规则一旦定下不要轻易改，改了要连带处理已有的挂靠数据。

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

构造函数、以及被参数/返回值/属性引用到的 Structure，也挂在同一套规则下：

| 场景 | 形式 | 例子 |
|---|---|---|
| 类构造函数 | `ClassName#constructor` | `BrowserWindow#constructor` |
| Structure 属性 | `StructureName.属性名` | `BrowserWindowConstructorOptions.webPreferences` |

Structure 只收"被收录模块/类引用到的"，不是 `electron-api.json` 里全部 174 项都收
（v43.3.0 收录 18 个模块/类，传递闭包收出 60 个 Structure，1140 条 API）；顺着参数/
返回值/属性的 **类型引用** 走，也顺着 Structure 的 `extends` 继承链走（如
`BrowserWindowConstructorOptions extends BaseWindowConstructorOptions`）。

electron-api.json 的 Class 条目本身已经是拍平的（子类的 `instanceMethods` 等
数组里已经包含继承来的成员），但 Structure 不是——`BrowserWindowConstructorOptions`
自己的 `properties` 只有 2 条，`width`/`height`/`show`/`x`/`y` 等 50 条都在
`BaseWindowConstructorOptions` 里。所以这里做两件事：

1. `extends` 目标（父 Structure）也收进来，自己作为独立容器出现在 `modules` 里，
   带 `extends` 字段，有自己完整的 entries（比如 `BaseWindowConstructorOptions.width`
   也存在）。
2. 子 Structure 的 entries 是**拍平过的**——继承来的属性也生成
   `BrowserWindowConstructorOptions.width` 这样的 key，entry 上带
   `inheritedFrom: 'BaseWindowConstructorOptions'`；自己声明的没有这个字段。
   子覆盖父同名属性时以子为准（如 `MouseWheelInputEvent` 重新声明了 `type` 的
   `possibleValues`，拍平后 `MouseWheelInputEvent.type` 不带 `inheritedFrom`）。
   两级继承（`MouseWheelInputEvent extends MouseInputEvent extends InputEvent`）
   也会拍平到底，`inheritedFrom` 指向实际声明这个属性的那一级
   （如 `modifiers` 只在 `InputEvent` 声明，`MouseWheelInputEvent.modifiers.
   inheritedFrom === 'InputEvent'`，不是中间的 `MouseInputEvent`）。

这样消费方问"BrowserWindow 能收哪些 options"一次查子的 key 就够，不用自己知道
还要回溯到父类；第 3 层要挂"某个选项哪个版本才有"也挂在子的 key 上。`extends`
目标不在收录范围内（比如 `IpcMainEvent extends Event`，`Event` 是 Node/DOM 内置
类型，不在 `electron-api.json` 里）时链就停在那，没法拍平不存在的文档条目。

快照顶层 `modules` 字段是 `{name, type, extends?}` 的数组，`type` 为
`'Module' | 'Class' | 'Structure'`，可以看出哪些名字是被引用收进来的 Structure，
`extends` 在有父 Structure 时才出现。

### 断代层（三层设计的第 2 层）

`electron/history/` 从 electron/electron 仓库机械拉取两份官方数据，解析、对到第
1 层快照的 key 上。**不做人为归类**——只回答"什么时候变的"，不判断"这算不算一个
能力"。

**数据源 A：`docs/breaking-changes.md`（取 `main` 分支）。** 这份文档是累积的，
从很早的版本一路记到最新（不用 tag 是因为 tag 对应的版本只能看到那之前的累积
内容）。结构是 `## Breaking API Changes (44.0)` 这种版本段，下面挂
`### Removed: xxx` / `### Deprecated: xxx` / `### Behavior Changed: xxx` /
`### Default Changed: xxx` / `### API Changed: xxx` 这些条目；约 v14 及更早的
版本段里，条目标题不带这个"Kind: 标题"前缀（自由文本，如 `` ### `webContents` ``），
这类归一成 `kind: 'Other'`，不强行套一个不存在的分类。每条产出 `version`、`kind`、
`title`、`anchor`（GitHub 风格的 header slug，用来跟 API History 的
`breaking-changes-header` 对齐，slug 算法见 `electron/history/slug.js`，是拿文档
里真实的内部锚点链接反推出来的，不是猜的）、`summary`（正文前 300 字左右）、
`mentionedApis`（正文里所有反引号包住的片段，去重保序）。版本段之外的标题（如
文档开头"Types of Breaking Changes"说明段）不当条目收，但计入
`stats.skippedOutsideVersion`，不静默丢。

**数据源 B：`docs/api/*.md` 里的 API History YAML 块（取 tag，与快照版本对齐，
目前是 `v43.3.0`）。** 每个成员标题后面可能跟一个
`` <!-- ```YAML history ... ``` --> `` 块，记 `added`/`changes`/`deprecated`，
每条有 `pr-url`，可选 `description`、`breaking-changes-header`。解析时把 YAML 块
归到它紧跟的那个标题，再把标题文本换算成第 1 层的 key：

- `` `new BrowserWindow([options])` `` → 构造函数 → `BrowserWindow#constructor`
- `` `win.setBounds(...)` `` → 实例方法（`win` 是文档手写的别名，跟
  `electron-api.json` 里的 `instanceName` 字段对不上，单独维护在
  `parse-api-history.js` 的 `MANUAL_PREFIX_ALIASES` 里）→ `BrowserWindow#setBounds`
- `` `BrowserWindow.getAllWindows()` `` → 静态方法 → `BrowserWindow.getAllWindows`
- `` `session.fromPartition(...)` `` → 模块方法 → `session.fromPartition`
- `Event: 'console-message'` → 事件，容器看当前在哪个 `## Class: X` 段落里（没有
  就退回文档 `# 模块名`）→ `WebContents@console-message`

**覆盖稀是已知的**：15 个原收录模块里 6 个（`app`/`screen`/`session`/
`system-preferences`/`web-request`/`web-utils`）完全没有 YAML 块，`web-contents`
有 10 块——Electron 官方也不是每次改动都补 API History。解析不到 key 的块不静默
丢，计入 `unresolved`（并标 `reason`：`module-level` 是块直接挂在文档 h1 模块名
下、不属于任何成员，比如 `contextBridge.md` 开头那条；`unrecognized-heading` 是
标题格式没认出来）。

**已知的局限（如实记录，不是要立刻补的坑）**：不少弃用/变更只在
breaking-changes.md 里用大白话提过，API History 里压根没打
`breaking-changes-header`——比如 `WebContents` 的 `console-message` 事件弃用
`level`/`message` 等位置参数，`docs/api/web-contents.md` 里那段只用行内
`` _Deprecated_ `` 标记（第 1 层快照的 `additionalTags: stability_deprecated`
已经收了这个信号），没有 YAML 块可关联。`historyOf(key)` 对这种情况有个文本兜底：
breaking-changes 条目正文里提到的反引号词（`mentionedApis`）如果同时包含成员名和
容器名，就当作大概率相关，标 `linkedVia: 'mention'`，跟通过
`breaking-changes-header` 精确关联的（`linkedVia: 'breaking-changes-header'`）
区分开——这是启发式，不保证准确，调用方可以自己决定要不要用。

**产出**：`electron/history/breaking-changes.json`（`{source: {repo, path, ref,
sha}, stats, entries}`，按 version 降序、同版本按文档出现顺序；`sha` 是拉取时
`docs/breaking-changes.md@main` 所在的 commit，不记时间戳——时间戳会破坏"两次生成
逐字节相同"）、`electron/history/api-history.json`（`{source, stats, byKey,
unresolved}`）。两份都走 `stable-json`，`node electron/history/fetch.js` 重新拉取
生成。

### 能力台账（三层设计的第 3 层）

`electron/capabilities/` 是手写的 JSON（`capabilities.json`），机器只负责校验、
不负责生成——"这算不算一个能力"是人判断的事，不是从第 1/2 层机械推导出来的。

每条"能力"= 一句人话说清"我想让 Electron 帮我干什么"，记下：

- `requires`：干这件事要用到的 API（第 1 层快照的 key），必须全在快照里，
  少一个都算数据错误；
- `knownIssues`：已知的坑，每条 `{symptom, cause, fix, platform, since,
  breakingChange}`。能对到第 2 层断代数据的填 `breakingChange`（
  breaking-changes.json 的 anchor）和 `since`（对应版本号），两者必须能在
  breaking-changes.json 里核对上；对不到的（纯实测踩出来的）`breakingChange:
  null`，`cause` 照实写清楚是什么原因，不硬凑一个不存在的版本号。`platform`
  取 `all` / `darwin` / `win32` / `linux`，需要具体系统版本时另加
  `osVersion`（如 `"≥14.2"`）。
- 不依赖 Electron API 的能力（比如改系统代理、装根证书）`requires` 写
  `[]`，改用顶层 `os` 字段声明支持哪些平台。

```js
const { listCapabilities, capability, checkCapabilities } = require('jsbase');

listCapabilities();               // 全部能力条目
capability('mitm.https');         // 按 id 取一条
checkCapabilities('43.3.0');      // 每条能力的 requires 在这个快照版本里是否全在
```

`electron/capabilities/index.js` 只管读数据、按 id 查、核对 requires——数据
本身的准确性靠 `tests/electron-capabilities.test.js` 校验：requires 的 key
必须在快照里存在、`breakingChange` 必须在 breaking-changes.json 里存在且
`since` 与该条目的实际 `version` 一致、id 不能重复、`platform` 取值不能瞎写。

### os/：不依赖 Electron 的地基

`os/sysproxy.js`、`os/rootcert.js`、`os/ca.js` 是从 websubmit macClient 的
`traffic/sysproxy.js`、`traffic/certmgr.js`、`traffic/ca.js` 原样搬过来的——
这三份只依赖 Node 内置模块 + `node-forge`，逻辑上跟 Electron、跟 macClient
的业务毫无关系，属于该待在 base 里的地基。

搬的时候**逻辑、文案、平台分支一行都不改**，唯一改动是原来写死的
`~/.filecore/traffic/...` 路径（CA 目录、状态文件）不再写死，改成工厂函数
让调用方显式传入——跟 pybase 的规矩一致，base 不带项目默认值：

```js
const { os } = require('jsbase');

const sysproxy = os.sysproxy('/path/to/sysproxy.json'); // 状态文件路径调用方给
// CA 目录与主体名都由调用方给（名字会进用户的钥匙串，是项目身份，base 不带默认值）
const names = { commonName: 'MyApp Recording Root CA', organizationName: 'MyApp' };
const rootcert = os.rootcert('/path/to/ca-dir', names);
const ca = os.ca('/path/to/ca-dir', names);               // 与 rootcert 同一目录、同一组名字

await sysproxy.apply(8080);
await rootcert.install();
const { cert, key } = ca.certFor('example.com');
```

也可以按路径直接 require（`exports` 里 `./os/*` 已开放）：
`require('jsbase/os/sysproxy')` 等。

`node-forge` 是这个包第一个运行时依赖（`dependencies` 而非
`peerDependencies`）：`os/ca.js` 自签证书要用到它做 RSA 密钥对与 X.509 证书
的生成/签名，Node 内置模块没有这个能力，跟 `electron` 那种"调用方本来就会装"
的情况不一样，只能老老实实声明依赖。

### 运行时地基

```js
const { electron, net } = require('jsbase');
const { normalizeConsoleMessage, registerRoutes, BrowserInstance } = electron;
const { RecordingProxy, parseUpstreamProxy, bypassProxy, MAX_BODY_BYTES } = net;

const browser = new BrowserInstance('tab-1', {
  electron: require('electron'),
  partition: 'persist:myapp-tab-1',
  shellFile: '/absolute/path/shell.html',
  shellPreload: '/absolute/path/shell-preload.js',
  guestPreload: '/absolute/path/guest-preload.js',
  title: 'My browser', width: 1280, height: 900,
  createProxy: ({ onRequest, onError, upstreamProxy }) => new RecordingProxy(
    onRequest, onError, { upstreamProxy, certFor: ca.certFor },
  ),
});
```

示例里的 `ca` 由前面的 `os.ca(caDir, names)` 创建。外壳需要读取查询参数
`partition`、`tabId`、`guestPreload` 和可选的 `url`，并用 `<webview>` 承载页面；
实例发出 `request`、`console`、`step`、`changed` 和 `resized` 事件。

`normalizeConsoleMessage(args)` 接收事件的全部参数，兼容旧位置参数、43.3.0
仍同时发送位置参数的形态，以及只剩首个 details 对象的形态。
`registerRoutes(ipcMain, routes, targetOf, after)` 保持原有成功/异常返回包装。

浏览器实例与代理从 macClient 上移，macClient 保留配置适配层。CA 签发函数、
Electron 对象、页面与 preload 路径、持久化分区和窗口配置由应用提供；加载
jsbase 本身不会加载 Electron，也不会创建窗口或安装系统证书。

代理记录保持原有字段与时间戳口径，方便已有消费者直接使用；本轮迁移保留
正文上限、上游 TLS 策略和重试行为。业务中的会话、暂停、攒批与落库仍由消费者处理。

### 命令行用法

```bash
# 缺 raw 会自动用 gh cli 下载（需要已登录 gh，从 electron/electron 的 release 附件取）
node electron/extract.js 43.3.0

# 比较两个版本的快照，输出新增/删除/变更的 key
node electron/diff.js 43.3.0 44.0.0

# 重新拉取断代数据（需要已登录 gh）
node electron/history/fetch.js
```

或者直接 `require('jsbase')`（见下面"安装"一节）：

```js
const { loadSnapshot, listSnapshots, diffSnapshots, loadHistory, historyOf } = require('jsbase');

loadSnapshot('43.3.0');           // 第 1 层：某个版本的快照
listSnapshots();                  // 已入库的版本号列表
diffSnapshots(a, b);              // 两份快照的新增/删除/变更
loadHistory();                    // 第 2 层：{ breakingChanges, apiHistory } 两份合起来
historyOf('WebContents@console-message'); // 某个 key 的 API History + 关联的 breaking-changes 条目
```

## 测试

```bash
cd js && node --test "tests/**/*.test.js"
```

## 安装

本地开发：这个包目前不发布，直接在 `js/` 里跑脚本即可，或者在调用方
`package.json` 写 `"jsbase": "file:../base/js"` 之类的本地路径依赖后
`require('jsbase')`（无运行时依赖，全部用 Node 内置模块）。`electron` 只作为
`peerDependencies` 声明，不会被安装——写成 `dependencies` 会再拖一份几百 MB 的
Electron 下来，而且版本不一定和调用方一致。

包入口是 `index.js`（`package.json` 的 `main`/`exports` 指过去），`exports`
只放开两条路径：`"."`（`index.js` 本身）和 `"./electron/snapshots/*.json"`
（需要直接拿原始快照 JSON 的场景，比如前端展示）。其它内部路径（`electron/
extract.js`、`electron/history/*.js` 等）不在 `exports` 里，调用方 require 不到，
要用的功能都应该从 `index.js` 导出的函数走。
