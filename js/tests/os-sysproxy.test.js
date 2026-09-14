'use strict';

// os/sysproxy.js 的测试。**不允许真的动系统代理**（不调 networksetup /
// security / netsh / certutil），所以只测不触发这些命令的路径：能 require、
// 状态文件路径参数生效、manualHint(port) 文案含端口、没有状态文件时
// restore() 直接短路返回成功（这条路径本身就不会调用任何 exec，readState()
// 先判定文件不存在就返回了）。

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { createSysproxy } = require('../os/sysproxy');

function tmpStateFile() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'jsbase-sysproxy-test-'));
  return path.join(dir, 'sysproxy.json');
}

test('createSysproxy(path) 的 STATE_FILE 就是传入的路径（不写死项目路径）', () => {
  const file = tmpStateFile();
  const sp = createSysproxy(file);
  assert.equal(sp.STATE_FILE, file);
});

test('没有状态文件时 restore() 直接短路成功，不触发任何系统命令', async () => {
  const file = tmpStateFile(); // 目录存在但文件本身不存在
  const sp = createSysproxy(file);
  const res = await sp.restore();
  assert.deepEqual(res, { ok: true, nothing: true });
});

test('manualHint(port) 文案里带着端口号', () => {
  const sp = createSysproxy(tmpStateFile());
  const hint = sp.manualHint(18080);
  assert.match(hint, /18080/);
});

test('两个实例互不干扰：各自的状态文件独立', () => {
  const a = createSysproxy(tmpStateFile());
  const b = createSysproxy(tmpStateFile());
  assert.notEqual(a.STATE_FILE, b.STATE_FILE);
});
