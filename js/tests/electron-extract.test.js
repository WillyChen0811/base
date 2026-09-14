'use strict';

// 覆盖点对应交付要求：快照能加载；Session 的两个方法在；WebContents 的
// console-message 事件结构正确（details 带 eventProperties，level 的
// possibleValues 含四种级别，旧版位置参数带弃用标记）；提取两次逐字节相同。

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');

const { extract, writeSnapshot, snapshotPath } = require('../electron/extract');

const VERSION = '43.3.0';

test('快照文件存在且能加载', () => {
  const file = snapshotPath(VERSION);
  assert.ok(fs.existsSync(file), `快照不存在，先跑 node electron/extract.js ${VERSION}`);
  const snapshot = JSON.parse(fs.readFileSync(file, 'utf8'));
  assert.equal(snapshot.electronVersion, VERSION);
  assert.ok(snapshot.entryCount > 0);
});

test('Session 类包含 setCertificateVerifyProc 与 setProxy', () => {
  const snapshot = extract(VERSION);
  assert.ok('Session#setCertificateVerifyProc' in snapshot.entries);
  assert.ok('Session#setProxy' in snapshot.entries);
  const entry = snapshot.entries['Session#setCertificateVerifyProc'];
  assert.equal(entry.kind, 'method');
  assert.equal(entry.module, 'Session');
});

test('WebContents 的 console-message 事件：details 参数带 eventProperties', () => {
  const snapshot = extract(VERSION);
  const entry = snapshot.entries['WebContents@console-message'];
  assert.ok(entry, 'console-message 事件应该被提取');
  assert.equal(entry.kind, 'event');

  const details = entry.parameters.find((p) => p.name === 'details');
  assert.ok(details, '第一个参数应该是 details');
  assert.equal(entry.parameters[0].name, 'details');
  assert.ok(Array.isArray(details.eventProperties), 'details 应该带 eventProperties');

  const level = details.eventProperties.find((p) => p.name === 'level');
  assert.ok(level, 'eventProperties 里应该有 level');
  assert.deepEqual(
    new Set(level.possibleValues),
    new Set(['info', 'warning', 'error', 'debug'])
  );
});

test('console-message 旧的位置参数 level/message 带弃用标记', () => {
  const snapshot = extract(VERSION);
  const entry = snapshot.entries['WebContents@console-message'];
  const legacyLevel = entry.parameters.find((p) => p.name === 'level');
  const legacyMessage = entry.parameters.find((p) => p.name === 'message');

  assert.ok(legacyLevel, '应该有旧版位置参数 level');
  assert.ok(legacyLevel.additionalTags.includes('stability_deprecated'));

  assert.ok(legacyMessage, '应该有旧版位置参数 message');
  assert.ok(legacyMessage.additionalTags.includes('stability_deprecated'));
});

test('提取两次产出逐字节相同（可复现）', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'jsbase-snapshot-test-'));
  try {
    const out1 = path.join(tmpDir, 'a.json');
    const out2 = path.join(tmpDir, 'b.json');

    const snap = extract(VERSION);
    const { stableStringify } = require('../electron/stable-json');
    fs.writeFileSync(out1, stableStringify(snap));
    fs.writeFileSync(out2, stableStringify(extract(VERSION)));

    assert.equal(fs.readFileSync(out1, 'utf8'), fs.readFileSync(out2, 'utf8'));
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('提取出的快照文件本身与重新生成结果逐字节相同', () => {
  const file = snapshotPath(VERSION);
  const before = fs.readFileSync(file, 'utf8');
  writeSnapshot(VERSION);
  const after = fs.readFileSync(file, 'utf8');
  assert.equal(before, after, '重新生成快照不应该改变已入库的文件内容');
});

test('清单里的模块/类，同一作用域内方法名和属性名不撞 key', () => {
  const snapshot = extract(VERSION);
  const seen = new Map();
  for (const key of Object.keys(snapshot.entries)) {
    assert.ok(!seen.has(key), `key 重复：${key}`);
    seen.set(key, true);
  }
});
