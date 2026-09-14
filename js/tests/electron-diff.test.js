'use strict';

// 现在只有一个版本，用同一份快照自比对验证 diff.js 能跑，输出应为空。

const test = require('node:test');
const assert = require('node:assert/strict');
const { diffSnapshots, loadSnapshot } = require('../electron/diff');

const VERSION = '43.3.0';

test('同一份快照自比对，新增/删除/变更都为空', () => {
  const snapshot = loadSnapshot(VERSION);
  const result = diffSnapshots(snapshot, snapshot);
  assert.deepEqual(result, { added: [], removed: [], changed: [] });
});
