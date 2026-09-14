'use strict';

// 三层设计的第 3 层：能力台账。手写 JSON（capabilities.json），这里只管读出来、
// 按 id 查、以及拿一个快照版本核对 requires 是不是都在——"这算不算一个能力"是
// 人判断的事，不是从第 1/2 层机械推导出来的，所以数据本身不生成，只校验。

const DATA = require('./capabilities.json');

const capabilities = DATA.capabilities;

// 按 id 建索引，重复 id 是数据错误，交给 capabilities.test.js 校验，这里不重复判断。
const byId = new Map(capabilities.map((c) => [c.id, c]));

// 列出全部能力条目（原样，不做拷贝——调用方不该改这份数据）。
function listCapabilities() {
  return capabilities;
}

// 按 id 取一条，取不到返回 undefined（不抛错，调用方自己决定要不要当错误处理）。
function capability(id) {
  return byId.get(id);
}

// 核对每条能力的 requires 在给定的快照版本里是否全在，返回每条的校验结果，
// 缺了哪些 key 一并列出来。不直接抛错——台账和快照可能故意暂时不同步（比如
// 先写能力条目、快照版本还没升级），抛不抛由调用方定。
function checkCapabilities(snapshotVersion) {
  // 延迟 require：capabilities/index.js 不该在模块加载时就依赖 electron/snapshots
  // 目录的存在，只有真正调用 checkCapabilities 时才去读。
  const { snapshotPath } = require('../extract');
  const fs = require('node:fs');
  const file = snapshotPath(snapshotVersion);
  if (!fs.existsSync(file)) {
    throw new Error(`快照不存在，先跑 node electron/extract.js ${snapshotVersion}：${file}`);
  }
  const snapshot = JSON.parse(fs.readFileSync(file, 'utf8'));

  return capabilities.map((c) => {
    const missing = c.requires.filter((key) => !(key in snapshot.entries));
    return { id: c.id, ok: missing.length === 0, missing };
  });
}

module.exports = { listCapabilities, capability, checkCapabilities };
