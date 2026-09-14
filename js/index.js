'use strict';

// 包入口。三层设计对外只暴露这几个函数，调用方不用知道快照/断代数据
// 分别存在哪个文件、用什么格式——具体文件布局（electron/snapshots/*.json、
// electron/history/*.json）算内部实现，只有 electron/snapshots/*.json 额外
// 走 exports 显式开放（见 package.json），是给需要直接 fetch 快照文件本身
// 的场景（比如前端展示原始 JSON）用的，其它路径一律封死。

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
  // 不依赖 Electron 的地基（js/os/*，工厂函数，落地路径由调用方传）
  os: {
    sysproxy: createSysproxy,
    rootcert: createRootcert,
    ca: createCA,
  },
};
