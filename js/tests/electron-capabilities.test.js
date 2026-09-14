'use strict';

// 第 3 层（能力台账）的校验测试。台账是手写 JSON，机器只负责校验不负责生成：
// requires 里的 key 必须在快照里真的存在，knownIssues 里非 null 的
// breakingChange 必须在 breaking-changes.json 里真的能查到且 since 对得上，
// id 不能重、platform 取值不能瞎写。

const test = require('node:test');
const assert = require('node:assert/strict');

const { loadSnapshot } = require('../index');
const { loadBreakingChanges } = require('../electron/history/store');
const { listCapabilities, capability, checkCapabilities } = require('../electron/capabilities');

const SNAPSHOT_VERSION = '43.3.0';
const VALID_PLATFORMS = new Set(['all', 'darwin', 'win32', 'linux']);

test('capabilities.json 至少有 7 条', () => {
  assert.ok(listCapabilities().length >= 7);
});

test('id 唯一', () => {
  const ids = listCapabilities().map((c) => c.id);
  assert.equal(new Set(ids).size, ids.length, '存在重复 id');
});

test('每条 requires 的 key 在 43.3.0 快照里都存在', () => {
  const snapshot = loadSnapshot(SNAPSHOT_VERSION);
  for (const c of listCapabilities()) {
    for (const key of c.requires) {
      assert.ok(
        key in snapshot.entries,
        `能力 ${c.id} 的 requires 里 ${key} 在 v${SNAPSHOT_VERSION} 快照里找不到`
      );
    }
  }
});

test('platform 取值只能是 all/darwin/win32/linux', () => {
  for (const c of listCapabilities()) {
    for (const issue of c.knownIssues || []) {
      assert.ok(
        VALID_PLATFORMS.has(issue.platform),
        `能力 ${c.id} 的坑「${issue.symptom}」platform 取值非法：${issue.platform}`
      );
    }
  }
});

test('非 null 的 breakingChange 在 breaking-changes.json 里存在，且 since 与该条目 version 一致', () => {
  const { entries } = loadBreakingChanges();
  const byAnchor = new Map(entries.map((e) => [e.anchor, e]));
  for (const c of listCapabilities()) {
    for (const issue of c.knownIssues || []) {
      if (issue.breakingChange == null) continue;
      const hit = byAnchor.get(issue.breakingChange);
      assert.ok(
        hit,
        `能力 ${c.id} 的坑引用的 breakingChange anchor「${issue.breakingChange}」在 breaking-changes.json 里找不到`
      );
      assert.equal(
        issue.since,
        hit.version,
        `能力 ${c.id} 的坑 since（${issue.since}）与 breaking-changes 条目实际 version（${hit.version}）不一致`
      );
    }
  }
});

test('capability(id) 能按 id 取到单条，取不到返回 undefined', () => {
  const first = listCapabilities()[0];
  assert.equal(capability(first.id), first);
  assert.equal(capability('not-a-real-id'), undefined);
});

test('checkCapabilities(43.3.0)：全部能力的 requires 都在快照里（本来就是照着这份快照写的）', () => {
  const result = checkCapabilities(SNAPSHOT_VERSION);
  assert.equal(result.length, listCapabilities().length);
  for (const r of result) {
    assert.deepEqual(r.missing, [], `${r.id} 有缺失的 key`);
    assert.equal(r.ok, true);
  }
});

test('os.sysproxy / os.rootcert 不依赖 Electron API，requires 为空，另带 os 字段', () => {
  const sysproxy = capability('os.sysproxy');
  const rootcert = capability('os.rootcert');
  assert.ok(sysproxy && rootcert);
  assert.deepEqual(sysproxy.requires, []);
  assert.deepEqual(rootcert.requires, []);
  assert.deepEqual(sysproxy.os, ['darwin', 'win32']);
  assert.deepEqual(rootcert.os, ['darwin', 'win32']);
});
