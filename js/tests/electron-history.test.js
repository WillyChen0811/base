'use strict';

// 第 2 层（断代层）的事实核对：数据是拉准了，还是解析器哪里想当然了。
// 这些断言对应的是 electron/electron 仓库 docs/breaking-changes.md（main）
// 和 docs/api/*.md（v43.3.0）里能实际查到的内容，不是编出来的期望值。

const test = require('node:test');
const assert = require('node:assert/strict');

const { loadBreakingChanges, loadApiHistory, historyOf } = require('../electron/history/store');

test('breaking-changes：32.0 有 File.path 移除', () => {
  const { entries } = loadBreakingChanges();
  const hit = entries.find((e) => e.version === '32.0' && /File\.path/.test(e.title));
  assert.ok(hit, '应该能找到 32.0 版本 File.path 被移除的条目');
  assert.equal(hit.kind, 'Removed');
});

test('breaking-changes：39.0 有 desktopCapturer / NSAudioCaptureUsageDescription', () => {
  const { entries } = loadBreakingChanges();
  const v39 = entries.filter((e) => e.version === '39.0');
  assert.ok(
    v39.some((e) => /NSAudioCaptureUsageDescription/.test(e.title) && /desktopCapturer/.test(e.title)),
    '39.0 应该有一条同时提到 NSAudioCaptureUsageDescription 和 desktopCapturer 的条目'
  );
});

test('breaking-changes：44.0 有 clipboard 相关条目', () => {
  const { entries } = loadBreakingChanges();
  const v44Clipboard = entries.filter((e) => e.version === '44.0' && /clipboard/i.test(e.title));
  assert.ok(v44Clipboard.length >= 2, '44.0 应该至少有两条 clipboard 相关条目（移除 + 重构）');
});

test('breaking-changes：解析统计非空、版本外段落被计数而不是丢掉', () => {
  const { entries, stats } = loadBreakingChanges();
  assert.equal(stats.totalEntries, entries.length);
  assert.ok(stats.totalEntries > 200, '累积文档条目数应该远大于 200');
  assert.equal(stats.skippedOutsideVersion, 1, '文档开头的"Types of Breaking Changes"说明段应该被计数跳过');
});

test('api-history：块总数、resolved/unresolved 数对得上（已知 6 个模块 0 块、web-contents 10 块）', () => {
  const { byKey, unresolved, stats } = loadApiHistory();
  assert.equal(stats.resolvedCount, Object.keys(byKey).length);
  assert.equal(stats.unresolvedCount, unresolved.length);
  assert.equal(stats.totalBlocks, stats.resolvedCount + stats.unresolvedCount);

  const webContentsKeys = Object.keys(byKey).filter((k) => k.startsWith('WebContents#') || k.startsWith('WebContents@'));
  assert.equal(webContentsKeys.length, 10, 'web-contents.md 摸底确认过是 10 个 YAML 块，且全部能 resolve');
});

test('api-history：解析不到 key 的块要计数列出，不能静默丢（module-level 两块）', () => {
  const { unresolved } = loadApiHistory();
  assert.equal(unresolved.length, 2);
  assert.ok(unresolved.every((u) => u.reason === 'module-level'));
});

test('historyOf：WebContents@console-message 关联到 breaking-changes 里 35.0 的弃用条目', () => {
  // 如实反映摸到的实物：v43.3.0（以及截至今天的 main）docs/api/web-contents.md
  // 里 console-message 事件没有 YAML history 块——旧位置参数的弃用只用行内
  // `_Deprecated_` 标记（第 1 层快照的 additionalTags: stability_deprecated
  // 已经收了这个信号），API History（数据源 B）本身对这个 key 是空的。
  // 所以这里不断言 apiHistory 非空，只断言能通过正文文本兜底关联
  // （historyOf 里的 mention 关联）找到 35.0 那条弃用记录。
  const result = historyOf('WebContents@console-message');
  assert.equal(result.apiHistory, null);
  const hit = result.breakingChanges.find((x) => x.entry.version === '35.0');
  assert.ok(hit, '应该关联到 35.0 版本的弃用条目');
  assert.equal(hit.linkedVia, 'mention');
  assert.match(hit.entry.title, /console-message/);
});

test('两次生成 breaking-changes.json / api-history.json 逐字节相同', () => {
  const { stableStringify } = require('../electron/stable-json');
  const a1 = stableStringify(loadBreakingChanges());
  const a2 = stableStringify(loadBreakingChanges());
  assert.equal(a1, a2);
  const b1 = stableStringify(loadApiHistory());
  const b2 = stableStringify(loadApiHistory());
  assert.equal(b1, b2);
});
