'use strict';

// 包入口：台账查询函数，以及 electron/net/os 运行时地基。
// 快照与断代数据的内部布局不属于调用契约；仅快照 JSON 和既有 os 子路径
// 额外通过 package.json exports 开放，其余能力从这里取。

const fs = require('node:fs');
const path = require('node:path');
const { snapshotPath, loadRaw } = require('./electron/extract');
const { diffSnapshots } = require('./electron/diff');
const { loadBreakingChanges, loadApiHistory, historyOf } = require('./electron/history/store');
const {
  listCapabilities,
  capability,
  checkCapabilities,
} = require('./electron/capabilities');
const { createSysproxy } = require('./os/sysproxy');
const { createRootcert } = require('./os/rootcert');
const { createCA } = require('./os/ca');
const { normalizeConsoleMessage } = require('./electron/console-message');
const { registerRoutes } = require('./electron/ipc');
const { BrowserInstance } = require('./electron/browser-instance');
const recordingProxy = require('./net/recording-proxy');
const media = require('./media');

const SNAPSHOT_DIR = path.join(__dirname, 'electron', 'snapshots');

// 第 1 层：加载某个 Electron 版本的快照。
function loadSnapshot(version) {
  const file = snapshotPath(version);
  if (!fs.existsSync(file)) {
    throw new Error(`快照不存在，先跑 node electron/extract.js ${version}：${file}`);
  }
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

// 列出已入库的快照版本号（从 electron/snapshots 目录机械读取，不含 raw）。
function listSnapshots() {
  if (!fs.existsSync(SNAPSHOT_DIR)) return [];
  return fs
    .readdirSync(SNAPSHOT_DIR)
    .filter((f) => f.endsWith('.json'))
    .map((f) => f.slice(0, -'.json'.length))
    .sort();
}

// 第 2 层：两份断代数据合起来返回，供调用方自己按 key 查（也可以直接用
// 下面的 historyOf 按单个 key 查）。
function loadHistory() {
  return {
    breakingChanges: loadBreakingChanges(),
    apiHistory: loadApiHistory(),
  };
}

module.exports = {
  loadSnapshot,
  listSnapshots,
  diffSnapshots,
  loadHistory,
  historyOf,
  loadRaw,
  // 第 3 层：能力台账
  listCapabilities,
  capability,
  checkCapabilities,
  // 运行时依赖由调用方注入，普通 Node 加载包入口无需安装/启动 Electron。
  electron: { normalizeConsoleMessage, registerRoutes, BrowserInstance },
  net: recordingProxy,
  // 网页播放器基础件：播放计划、video 控制器、分词、Electron 本地媒体协议
  media,
  // 不依赖 Electron 的地基（js/os/*，工厂函数，落地路径由调用方传）
  os: {
    sysproxy: createSysproxy,
    rootcert: createRootcert,
    ca: createCA,
  },
};
