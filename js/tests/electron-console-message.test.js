'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { normalizeConsoleMessage } = require('../electron/console-message');

const details = {
  level: 'warning', message: 'hello', lineNumber: 1, sourceId: 'about:blank', frame: {},
};

test('console-message：Electron 43 的 details 加四个弃用位置参数', () => {
  assert.deepEqual(
    normalizeConsoleMessage([details, 2, 'hello', 1, 'about:blank']),
    { level: 'warning', message: 'hello', line: 1, source: 'about:blank' },
  );
});

test('console-message：位置参数删除后只剩 details', () => {
  assert.deepEqual(
    normalizeConsoleMessage([details]),
    { level: 'warning', message: 'hello', line: 1, source: 'about:blank' },
  );
});

test('console-message：旧签名的第一个 event 对象不被误认成 details', () => {
  assert.deepEqual(
    normalizeConsoleMessage([{ preventDefault() {} }, 3, 'boom', 7, 'a.js']),
    { level: 'error', message: 'boom', line: 7, source: 'a.js' },
  );
});
