'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { registerRoutes } = require('../electron/ipc');

function fakeIpcMain() {
  const handlers = new Map();
  return {
    handlers,
    handle(channel, handler) { handlers.set(channel, handler); },
  };
}

test('IPC 路由注册全部频道，并把 targetOf(id) 的目标与原 args 交给路由', async () => {
  const ipcMain = fakeIpcMain();
  const target = { name: 'browser' };
  const seen = [];
  registerRoutes(ipcMain, {
    open: async (actualTarget, args) => {
      seen.push({ actualTarget, args });
      return { value: 42 };
    },
    close: () => ({}),
  }, (id) => {
    seen.push({ id });
    return target;
  });

  assert.deepEqual([...ipcMain.handlers.keys()], ['open', 'close']);
  const args = { id: 'tab-1', extra: true };
  assert.deepEqual(await ipcMain.handlers.get('open')({}, args), { success: true, value: 42 });
  assert.deepEqual(seen, [{ id: 'tab-1' }, { actualTarget: target, args }]);
});

test('IPC 路由把同步和异步异常变成带原消息的失败结果', async () => {
  const ipcMain = fakeIpcMain();
  registerRoutes(ipcMain, {
    sync: () => { throw new Error('同步失败'); },
    async: async () => { throw new Error('异步失败'); },
  }, () => null);

  assert.deepEqual(await ipcMain.handlers.get('sync')({}, {}),
    { success: false, message: '同步失败' });
  assert.deepEqual(await ipcMain.handlers.get('async')({}, {}),
    { success: false, message: '异步失败' });
});

test('targetOf 抛出的异常也由路由边界包装', async () => {
  const ipcMain = fakeIpcMain();
  registerRoutes(ipcMain, { open: () => ({}) }, () => {
    throw new Error('目标不存在');
  });

  assert.deepEqual(await ipcMain.handlers.get('open')({}, { id: 'missing' }),
    { success: false, message: '目标不存在' });
});

test('IPC 成功值先经 JSON 往返，无法序列化的值也收束成失败结果', async () => {
  const ipcMain = fakeIpcMain();
  const circular = {};
  circular.self = circular;
  registerRoutes(ipcMain, {
    clean: () => ({ when: new Date('2026-09-15T00:00:00.000Z'), omitted: undefined }),
    circular: () => circular,
  }, () => null);

  assert.deepEqual(await ipcMain.handlers.get('clean')({}, {}), {
    success: true,
    when: '2026-09-15T00:00:00.000Z',
  });
  const failed = await ipcMain.handlers.get('circular')({}, {});
  assert.equal(failed.success, false);
  assert.match(failed.message, /circular/i);
});

test('after(id) 在路由成功后、序列化前执行；路由失败时不执行，钩子失败则调用失败', async () => {
  const ipcMain = fakeIpcMain();
  const calls = [];
  const circular = {};
  circular.self = circular;
  registerRoutes(ipcMain, {
    ok: () => { calls.push('route:ok'); return { ok: true }; },
    routeFail: () => { calls.push('route:fail'); throw new Error('route'); },
    afterFail: () => { calls.push('route:afterFail'); return {}; },
    serializeFail: () => { calls.push('route:serializeFail'); return circular; },
  }, () => null, (id) => {
    calls.push(`after:${id}`);
    if (id === 'bad-after') throw new Error('after');
  });

  assert.deepEqual(await ipcMain.handlers.get('ok')({}, { id: 'one' }),
    { success: true, ok: true });
  assert.deepEqual(await ipcMain.handlers.get('routeFail')({}, { id: 'two' }),
    { success: false, message: 'route' });
  assert.deepEqual(await ipcMain.handlers.get('afterFail')({}, { id: 'bad-after' }),
    { success: false, message: 'after' });
  const serializeFailed = await ipcMain.handlers.get('serializeFail')({}, { id: 'three' });
  assert.equal(serializeFailed.success, false);
  assert.match(serializeFailed.message, /circular/i);
  assert.deepEqual(calls, [
    'route:ok', 'after:one',
    'route:fail',
    'route:afterFail', 'after:bad-after',
    'route:serializeFail', 'after:three',
  ]);
});
