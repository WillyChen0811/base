# jsbase

零业务依赖的 JS/Node 基础模块集，与 `py/`（pybase）对称。

## 模块一览

- `jsbase/electron`：Electron 版本↔能力台账，三层——快照（`extract.js`，"有什么"）/
  断代（`history/`，"什么时候变"）/ 能力台账（`capabilities/`，手写，"踩过的坑"）
- `jsbase/os`：不依赖 Electron 的地基——系统代理（`sysproxy`）、根证书（`rootcert`）、自签 CA（`ca`）
- 包入口 `electron`：console 参数归一化、IPC 路由注册、`BrowserInstance`
- 包入口 `net`：HTTP/HTTPS 录制代理 `RecordingProxy`

## Electron API 快照三层

1. **快照层**：机械提取 `electron-api.json`，key 排序、逐字节可复现，只回答"这个版本有什么"
2. **断代层**（`electron/history/`）：结合 breaking-changes 和 API History，回答"什么时候变的"，不做归类判断
3. **能力台账**（`electron/capabilities/`）：手写 JSON，机器只校验不生成，回答"踩过的坑"，这一层才做归类判断

第 2、3 层都挂在第 1 层产出的 key 上，key 规则见下表，一旦定下不要改，改了要连带迁移挂靠数据。

### key 规则

| 场景 | 形式 | 例子 |
|---|---|---|
| 模块方法/属性 | `name.member` | `app.getPath` |
| 模块事件 | `name@event-name` | `app@will-finish-launching` |
| 类实例方法/属性 | `ClassName#member` | `Session#setCertificateVerifyProc` |
| 类实例事件 | `ClassName@event-name` | `WebContents@console-message` |
| 类静态方法/属性 | `ClassName.member` | `BrowserWindow.getAllWindows` |
| 类构造函数 | `ClassName#constructor` | `BrowserWindow#constructor` |
| Structure 属性 | `StructureName.属性名` | `BrowserWindowConstructorOptions.webPreferences` |

Structure 只收被引用到的（类型引用 + `extends` 闭包），继承属性拍平后带 `inheritedFrom`。

```bash
node electron/extract.js <version>   # 产出快照，缺 raw 会自动用 gh cli 下载
node electron/diff.js <a> <b>        # 比较两个版本快照的新增/删除/变更
node electron/history/fetch.js       # 重新拉取断代数据
```

```js
const {
  loadSnapshot, listSnapshots, diffSnapshots, loadHistory, historyOf,
  listCapabilities, capability, checkCapabilities,
} = require('jsbase');
```

## os/：不依赖 Electron 的地基

```js
const { os } = require('jsbase');

const sysproxy = os.sysproxy('/path/to/sysproxy.json');   // 状态文件路径由调用方给
const rootcert = os.rootcert('/path/to/ca-dir', names);   // CA 目录与主体名由调用方给
const ca = os.ca('/path/to/ca-dir', names);                // 与 rootcert 同目录同名字
```

`node-forge` 是本包唯一运行时依赖（`ca.js` 签证书用）。

## Electron 运行时地基

```js
const { electron, net } = require('jsbase');

const browser = new electron.BrowserInstance('tab-1', {
  electron: require('electron'),
  partition: 'persist:myapp-tab-1',
  shellFile, shellPreload, guestPreload,
  createProxy: ({ onRequest, onError, upstreamProxy }) =>
    new net.RecordingProxy(onRequest, onError, { upstreamProxy, certFor: ca.certFor }),
});
```

事件：`request` / `console` / `step` / `changed` / `resized`。外壳读取查询参数
`partition`/`tabId`/`guestPreload`/`url`，用 `<webview>` 承载。加载 jsbase 本身不加载
Electron、不开窗、不装证书。

## 安装

这个包目前不发布，直接在 `js/` 里跑脚本，或在调用方 `package.json` 写
`"jsbase": "file:../base/js"` 本地路径依赖后 `require('jsbase')`。`electron` 只作为
`peerDependencies` 声明，不会被安装。

## 测试

```bash
cd js && node --test "tests/**/*.test.js"
```
