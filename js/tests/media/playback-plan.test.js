'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const {
  findActiveCueIndex, findPrecedingCueIndex, findNextCueIndex, nextAction,
} = require('../../media/playback-plan');

const cues = [
  { start: 1000, end: 2000, text: 'a' },
  { start: 2000, end: 3000, text: 'b' },
  { start: 5000, end: 6000, text: 'c' }, // 3000~5000 是一段 2000ms 的空白
];

test('findActiveCueIndex：命中/不命中/边界', () => {
  assert.equal(findActiveCueIndex(cues, 1500), 0);
  assert.equal(findActiveCueIndex(cues, 2000), 1); // 左闭右开，2000 属于下一句
  assert.equal(findActiveCueIndex(cues, 3000), -1); // 空白区
  assert.equal(findActiveCueIndex(cues, 999), -1);
});

test('findActiveCueIndex：重叠字幕取 start 最小的一条', () => {
  const overlapping = [
    { start: 0, end: 2000, text: 'outer' },
    { start: 500, end: 1000, text: 'inner' },
  ];
  assert.equal(findActiveCueIndex(overlapping, 700), 0);
});

test('findPrecedingCueIndex / findNextCueIndex', () => {
  assert.equal(findPrecedingCueIndex(cues, 4000), 1);
  assert.equal(findPrecedingCueIndex(cues, 500), -1);
  assert.equal(findNextCueIndex(cues, 4000), 2);
  assert.equal(findNextCueIndex(cues, 6000), -1);
});

test('sentence：越过句末才暂停，句中不动作', () => {
  assert.deepEqual(
    nextAction(cues, { mode: 'sentence', prevMs: 1900, nowMs: 1950 }),
    { action: 'none' },
  );
  assert.deepEqual(
    nextAction(cues, { mode: 'sentence', prevMs: 1950, nowMs: 2010 }),
    { action: 'pause', atMs: 2000, cueIndex: 0 },
  );
});

test('sentence：不在任何字幕里，不动作', () => {
  assert.deepEqual(
    nextAction(cues, { mode: 'sentence', prevMs: 3500, nowMs: 3600 }),
    { action: 'none' },
  );
});

test('repeat-one：到目标句末跳回句首，跳到别的句子不影响', () => {
  assert.deepEqual(
    nextAction(cues, { mode: 'repeat-one', prevMs: 1990, nowMs: 2000, options: { cueIndex: 0 } }),
    { action: 'seek', toMs: 1000, reason: 'repeat-one' },
  );
  assert.deepEqual(
    nextAction(cues, { mode: 'repeat-one', prevMs: 1500, nowMs: 1600, options: { cueIndex: 0 } }),
    { action: 'none' },
  );
});

test('repeat-one：没给 cueIndex 不动作', () => {
  assert.deepEqual(
    nextAction(cues, { mode: 'repeat-one', prevMs: 1990, nowMs: 2000, options: {} }),
    { action: 'none' },
  );
});

test('ab-repeat：任意区间循环', () => {
  const range = { startMs: 1200, endMs: 2600 };
  assert.deepEqual(
    nextAction(cues, { mode: 'ab-repeat', prevMs: 2590, nowMs: 2600, options: { range } }),
    { action: 'seek', toMs: 1200, reason: 'ab-repeat' },
  );
  assert.deepEqual(
    nextAction(cues, { mode: 'ab-repeat', prevMs: 1300, nowMs: 1400, options: { range } }),
    { action: 'none' },
  );
});

test('skip-blank：空档超过 minGapMs 才跳，跳到下一句开头', () => {
  assert.deepEqual(
    nextAction(cues, { mode: 'skip-blank', prevMs: 3100, nowMs: 3200, options: { minGapMs: 1500 } }),
    { action: 'seek', toMs: 5000, reason: 'skip-blank' },
  );
});

test('skip-blank：空档不够长不跳', () => {
  assert.deepEqual(
    nextAction(cues, { mode: 'skip-blank', prevMs: 3100, nowMs: 3200, options: { minGapMs: 3000 } }),
    { action: 'none' },
  );
});

test('skip-blank：在字幕里不动作；片头空白（没有 preceding cue）也按整段空白算', () => {
  assert.deepEqual(
    nextAction(cues, { mode: 'skip-blank', prevMs: 1500, nowMs: 1600, options: { minGapMs: 100 } }),
    { action: 'none' },
  );
  const leading = [{ start: 3000, end: 4000, text: 'x' }];
  assert.deepEqual(
    nextAction(leading, { mode: 'skip-blank', prevMs: 100, nowMs: 200, options: { minGapMs: 1000 } }),
    { action: 'seek', toMs: 3000, reason: 'skip-blank' },
  );
});

test('未知模式抛错，不是静默不动作', () => {
  assert.throws(() => nextAction(cues, { mode: 'nope', prevMs: 0, nowMs: 0 }), TypeError);
});

test('repeat-one / ab-repeat：用户拖到区间后面去不会被拽回来', () => {
  const cues = [{ start: 0, end: 2000, text: 'a' }, { start: 5000, end: 7000, text: 'b' }];
  assert.deepEqual(
    nextAction(cues, { mode: 'repeat-one', prevMs: 6000, nowMs: 6016, options: { cueIndex: 0 } }),
    { action: 'none' },
  );
  assert.deepEqual(
    nextAction(cues, { mode: 'ab-repeat', prevMs: 6000, nowMs: 6016, options: { range: { startMs: 0, endMs: 3000 } } }),
    { action: 'none' },
  );
});
