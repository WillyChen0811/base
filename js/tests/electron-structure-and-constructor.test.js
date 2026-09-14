'use strict';

// 覆盖第 1 层验收清单里第 0 步补的三个洞（构造函数、被引用的 Structure、
// CommandLine 类），以及包入口能否被真实 require 使用。

const test = require('node:test');
const assert = require('node:assert/strict');

const { extract } = require('../electron/extract');

const VERSION = '43.3.0';

test('BrowserWindow 的构造函数被收进快照，key 为 BrowserWindow#constructor', () => {
  const snapshot = extract(VERSION);
  const entry = snapshot.entries['BrowserWindow#constructor'];
  assert.ok(entry, '应该有 BrowserWindow#constructor');
  assert.equal(entry.kind, 'constructor');
  assert.equal(entry.parameters[0].type, 'BrowserWindowConstructorOptions');
});

test('被引用的 Structure 展开成逐属性的 entry', () => {
  const snapshot = extract(VERSION);
  const webPrefs = snapshot.entries['BrowserWindowConstructorOptions.webPreferences'];
  assert.ok(webPrefs, '应该有 BrowserWindowConstructorOptions.webPreferences');
  assert.equal(webPrefs.itemType, 'Structure');
  assert.equal(webPrefs.propertyType, 'WebPreferences');

  const contextIsolation = snapshot.entries['WebPreferences.contextIsolation'];
  assert.ok(contextIsolation, '嵌套 Structure（WebPreferences）的属性也要有 key');
  assert.equal(contextIsolation.propertyType, 'boolean');
});

test('Structure 的 extends 继承链被拍平进子的 entries', () => {
  const snapshot = extract(VERSION);

  // BrowserWindowConstructorOptions 自己只声明了 2 个属性，width 是从
  // BaseWindowConstructorOptions 继承来的，拍平后应该能直接在子的 key 下查到，
  // 且带 inheritedFrom 标记是谁声明的。
  const width = snapshot.entries['BrowserWindowConstructorOptions.width'];
  assert.ok(width, 'BrowserWindowConstructorOptions.width 应该被拍平进来');
  assert.equal(width.inheritedFrom, 'BaseWindowConstructorOptions');

  // 自己声明的属性不带 inheritedFrom。
  const webPreferences = snapshot.entries['BrowserWindowConstructorOptions.webPreferences'];
  assert.equal(webPreferences.inheritedFrom, undefined);

  // 两级继承：MouseWheelInputEvent extends MouseInputEvent extends InputEvent，
  // modifiers 只在 InputEvent 声明，中间的 MouseInputEvent 没有重新声明，
  // 拍平后应该直接指向最终声明它的 InputEvent。
  const modifiers = snapshot.entries['MouseWheelInputEvent.modifiers'];
  assert.ok(modifiers, 'MouseWheelInputEvent.modifiers 应该被拍平进来（两级继承）');
  assert.equal(modifiers.inheritedFrom, 'InputEvent');

  // type 在 MouseWheelInputEvent 自己又重新声明了（possibleValues 收窄成
  // 'mouseWheel'），子覆盖父同名属性时应该以子为准，不带 inheritedFrom。
  const type = snapshot.entries['MouseWheelInputEvent.type'];
  assert.equal(type.inheritedFrom, undefined);
  assert.deepEqual(type.possibleValues, ['mouseWheel']);

  // 父 Structure 自己也要作为独立容器出现在 modules 里，带 extends 字段。
  const baseWindowOptions = snapshot.modules.find((m) => m.name === 'BaseWindowConstructorOptions');
  assert.ok(baseWindowOptions, 'BaseWindowConstructorOptions 应该出现在 modules 里');
  const bwOptions = snapshot.modules.find((m) => m.name === 'BrowserWindowConstructorOptions');
  assert.equal(bwOptions.extends, 'BaseWindowConstructorOptions');
});

test('快照顶层 modules 能看出哪些是 Structure', () => {
  const snapshot = extract(VERSION);
  const structureNames = snapshot.modules.filter((m) => m.type === 'Structure').map((m) => m.name);
  assert.ok(structureNames.includes('BrowserWindowConstructorOptions'));
  assert.ok(structureNames.includes('WebPreferences'));
  const containerNames = snapshot.modules.filter((m) => m.type !== 'Structure').map((m) => m.name);
  assert.ok(containerNames.includes('BrowserWindow'));
});

test('CommandLine 类被收录，appendSwitch 在快照里', () => {
  const snapshot = extract(VERSION);
  const entry = snapshot.entries['CommandLine#appendSwitch'];
  assert.ok(entry, '应该有 CommandLine#appendSwitch');
  assert.equal(entry.kind, 'method');
  assert.equal(entry.module, 'CommandLine');
});

test('包入口 require("jsbase") 能正常使用（loadSnapshot/listSnapshots/historyOf）', () => {
  const jsbase = require('../index');
  const snapshot = jsbase.loadSnapshot(VERSION);
  assert.equal(snapshot.electronVersion, VERSION);
  assert.ok(jsbase.listSnapshots().includes(VERSION));
  const history = jsbase.historyOf('WebContents@console-message');
  assert.equal(history.key, 'WebContents@console-message');
});

test('提取两次逐字节相同（含新加的构造函数/Structure entries）', () => {
  const a = require('../electron/stable-json').stableStringify(extract(VERSION));
  const b = require('../electron/stable-json').stableStringify(extract(VERSION));
  assert.equal(a, b);
});
