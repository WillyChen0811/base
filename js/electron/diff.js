#!/usr/bin/env node
'use strict';

// 比较两个版本的快照，输出新增/删除/变更的 key，是"这次升级动了哪些 API"的机械底稿。

const path = require('node:path');
const { snapshotPath } = require('./extract');
const fs = require('node:fs');

function loadSnapshot(version) {
  const file = snapshotPath(version);
  if (!fs.existsSync(file)) {
    throw new Error(`快照不存在，先跑 node electron/extract.js ${version}：${file}`);
  }
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

function diffSnapshots(a, b) {
  const aKeys = new Set(Object.keys(a.entries));
  const bKeys = new Set(Object.keys(b.entries));

  const added = [...bKeys].filter((k) => !aKeys.has(k)).sort();
  const removed = [...aKeys].filter((k) => !bKeys.has(k)).sort();
  const changed = [...aKeys]
    .filter((k) => bKeys.has(k))
    .filter((k) => JSON.stringify(a.entries[k]) !== JSON.stringify(b.entries[k]))
    .sort();

  return { added, removed, changed };
}

function main() {
  const [versionA, versionB] = process.argv.slice(2);
  if (!versionA || !versionB) {
    console.error('用法：node electron/diff.js <版本A> <版本B>');
    process.exit(1);
  }
  const a = loadSnapshot(versionA);
  const b = loadSnapshot(versionB);
  const result = diffSnapshots(a, b);

  console.log(`${versionA} -> ${versionB}`);
  console.log(`新增 ${result.added.length}，删除 ${result.removed.length}，变更 ${result.changed.length}`);
  for (const k of result.added) console.log(`  + ${k}`);
  for (const k of result.removed) console.log(`  - ${k}`);
  for (const k of result.changed) console.log(`  ~ ${k}`);
}

if (require.main === module) {
  main();
}

module.exports = { diffSnapshots, loadSnapshot };
