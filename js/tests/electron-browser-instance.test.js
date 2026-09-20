'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const { pathToFileURL } = require('node:url');
const { BrowserInstance } = require('../electron/browser-instance');

class FakeWebContents extends EventEmitter {
  constructor() {
    super();
    this.sent = [];
  }

  send(channel, payload) { this.sent.push([channel, payload]); }
}

function harness(overrides = {}) {
  const windows = [];
  class FakeBrowserWindow extends EventEmitter {
    constructor(options) {
      super();
      this.options = options;
      this.webContents = new FakeWebContents();
      this.destroyed = false;
      this.focused = false;
      windows.push(this);
    }

    loadFile(file, options) { this.loaded = { file, options }; }
    isDestroyed() { return this.destroyed; }
    focus() { this.focused = true; }
    close() { this.emit('closed'); }
    destroy() { this.destroyed = true; }
    getContentSize() { return [1001, 701]; }
    getBounds() { return { x: 0, y: 0, width: 1001, height: 701 }; }
    getMediaSourceId() { return 'window:7'; }
  }

  const proxyCalls = [];
  const browserSession = {
    proxySettings: [],
    async setProxy(settings) { this.proxySettings.push(settings); },
    setCertificateVerifyProc(handler) { this.verify = handler; },
  };
  const partitionCalls = [];
  const proxy = {
    port: 18443,
    stats: { total: 2, withBody: 1, failed: 0 },
    starts: 0,
    stops: 0,
    async start() { this.starts += 1; return this.port; },
    async stop() { this.stops += 1; },
  };
  const electron = {
    BrowserWindow: FakeBrowserWindow,
    session: {
      fromPartition(partition) {
        partitionCalls.push(partition);
        return browserSession;
      },
    },
    screen: {
      getDisplayNearestPoint() { return { scaleFactor: 2 }; },
    },
  };
  const instance = new BrowserInstance('tab-7', {
    electron,
    partition: 'persist:test-tab-7',
    shellFile: '/app/browser/shell.html',
    shellPreload: '/app/browser/shell-preload.js',
    guestPreload: '/app/browser/guest-preload.js',
    title: 'Test Browser',
    width: 1280,
    height: 900,
    getUpstreamProxy: async () => 'proxy.internal:8080',
    createProxy(options) { proxyCalls.push(options); return proxy; },
    ...overrides,
  });
  return { instance, windows, proxy, proxyCalls, browserSession, partitionCalls };
}

function fakeGuest() {
  const guest = new EventEmitter();
  guest.destroyed = false;
  guest.url = 'https://example.test/start';
  guest.loaded = [];
  guest.isDestroyed = () => guest.destroyed;
  guest.getURL = () => guest.url;
  guest.getTitle = () => 'Example';
  guest.loadURL = async (url) => { guest.loaded.push(url); guest.url = url; };
  guest.reload = () => { guest.reloaded = true; };
  guest.setWindowOpenHandler = (handler) => { guest.windowOpenHandler = handler; };
  guest.isDevToolsOpened = () => false;
  guest.openDevTools = (options) => { guest.devToolsOptions = options; };
  guest.closeDevTools = () => { guest.devToolsClosed = true; };
  return guest;
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((onResolve, onReject) => {
    resolve = onResolve;
    reject = onReject;
  });
  return { promise, resolve, reject };
}

function nextTurn() {
  return new Promise((resolve) => setImmediate(resolve));
}

test('open 使用注入的窗口资源、partition 与代理，并保留子 frame preload', async () => {
  const { instance, windows, proxy, proxyCalls, browserSession, partitionCalls } = harness();
  const requests = [];
  instance.on('request', (record) => requests.push(record));
  instance.on('error', () => {});

  const status = await instance.open({ url: 'https://example.test' });

  assert.equal(windows.length, 1);
  assert.deepEqual(windows[0].options, {
    width: 1280,
    height: 900,
    title: 'Test Browser',
    webPreferences: {
      webviewTag: true,
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: false,
      preload: '/app/browser/shell-preload.js',
    },
  });
  assert.deepEqual(windows[0].loaded, {
    file: '/app/browser/shell.html',
    options: { query: {
      partition: 'persist:test-tab-7',
      tabId: 'tab-7',
      guestPreload: pathToFileURL('/app/browser/guest-preload.js').toString(),
      url: 'https://example.test',
    } },
  });
  assert.deepEqual(partitionCalls, ['persist:test-tab-7']);
  assert.deepEqual(browserSession.proxySettings, [{
    proxyRules: 'http=127.0.0.1:18443;https=127.0.0.1:18443',
    proxyBypassRules: '<-loopback>',
  }]);
  let verifyResult;
  browserSession.verify({}, (result) => { verifyResult = result; });
  assert.equal(verifyResult, 0);
  assert.equal(proxyCalls[0].upstreamProxy, 'proxy.internal:8080');
  proxyCalls[0].onRequest({ id: 1 });
  assert.deepEqual(requests, [{ id: 1 }]);
  assert.equal(proxy.starts, 1);
  assert.equal(status.proxyPort, 18443);

  // shell 已打开但 guest 尚未挂上时，第二次 open 只聚焦，不能误调用 navigate。
  await instance.open({ url: 'https://later.test' });
  assert.equal(windows[0].focused, true);
  assert.equal(windows.length, 1);

  const webPreferences = {};
  windows[0].webContents.emit('will-attach-webview', {}, webPreferences);
  assert.deepEqual(webPreferences, {
    preload: '/app/browser/guest-preload.js',
    nodeIntegrationInSubFrames: true,
  });
});

test('guest 优先使用 navigationHistory，控制台兼容 details 单参数', async () => {
  const { instance, windows } = harness();
  await instance.open();
  const guest = fakeGuest();
  let back = 0;
  let forward = 0;
  guest.navigationHistory = {
    canGoBack: () => true,
    goBack: () => { back += 1; },
    canGoForward: () => true,
    goForward: () => { forward += 1; },
  };
  const messages = [];
  instance.on('console', (message) => messages.push(message));
  windows[0].webContents.emit('did-attach-webview', {}, guest);

  instance.goBack();
  instance.goForward();
  guest.emit('console-message', {
    level: 'warning',
    message: 'hello',
    lineNumber: 9,
    sourceId: 'page.js',
  });

  assert.equal(back, 1);
  assert.equal(forward, 1);
  assert.deepEqual(messages.map(({ wall_time, ...message }) => message), [{
    kind: 'console', level: 'warning', text: 'hello', url: 'page.js', line_no: 9,
  }]);
  assert.equal(typeof messages[0].wall_time, 'number');
});

test('旧 Electron 没有 navigationHistory 时回退到 guest 的导航方法', async () => {
  const { instance, windows } = harness();
  await instance.open();
  const guest = fakeGuest();
  let back = 0;
  let forward = 0;
  guest.canGoBack = () => true;
  guest.goBack = () => { back += 1; };
  guest.canGoForward = () => true;
  guest.goForward = () => { forward += 1; };
  windows[0].webContents.emit('did-attach-webview', {}, guest);

  instance.goBack();
  instance.goForward();

  assert.equal(back, 1);
  assert.equal(forward, 1);
});

test('导航去重、忽略子 frame，并保留事件时间、在标题稳定后发 step', async (t) => {
  t.mock.timers.enable({ apis: ['Date', 'setTimeout'], now: 1_000 });
  const { instance, windows } = harness();
  await instance.open();
  const guest = fakeGuest();
  let title = '旧标题';
  guest.getTitle = () => title;
  const steps = [];
  instance.on('step', (step) => steps.push(step));
  windows[0].webContents.emit('did-attach-webview', {}, guest);

  guest.url = 'https://example.test/a';
  guest.emit('did-navigate', {}, guest.url, 200);
  guest.emit('did-navigate-in-page', {}, guest.url, true);
  guest.emit('did-navigate-in-page', {}, 'https://frame.test/', false);
  setTimeout(() => { title = '新页面'; }, 100);
  t.mock.timers.tick(250);

  assert.equal(instance.currentUrl, 'https://example.test/a');
  assert.equal(steps.length, 1);
  assert.equal(steps[0].ts, 1_000);
  assert.equal(steps[0].page_url, 'https://example.test/a');
  assert.equal(steps[0].page_title, '新页面');
  assert.equal(steps[0].target, '打开页面「新页面」');
});

test('并发 open 共用一次完整的代理初始化', { timeout: 2_000 }, async () => {
  const upstream = deferred();
  const setProxyGate = deferred();
  const setProxyEntered = deferred();
  let upstreamCalls = 0;
  const created = [];
  const { instance, windows, browserSession } = harness({
    async getUpstreamProxy() {
      upstreamCalls += 1;
      return upstream.promise;
    },
    createProxy() {
      const proxy = {
        port: 18_443,
        stats: { total: 0, withBody: 0, failed: 0 },
        starts: 0,
        stops: 0,
        async start() { this.starts += 1; return this.port; },
        async stop() { this.stops += 1; },
      };
      created.push(proxy);
      return proxy;
    },
  });
  browserSession.setProxy = async function setProxy(settings) {
    this.proxySettings.push(settings);
    if (settings.mode !== 'direct') {
      setProxyEntered.resolve();
      await setProxyGate.promise;
    }
  };

  let firstSettled = false;
  let secondSettled = false;
  const first = instance.open().finally(() => { firstSettled = true; });
  const second = instance.open().finally(() => { secondSettled = true; });
  await nextTurn();
  upstream.resolve('proxy.internal:8080');
  await setProxyEntered.promise;

  const beforeRelease = {
    upstreamCalls,
    created: created.length,
    starts: created.reduce((total, proxy) => total + proxy.starts, 0),
    firstSettled,
    secondSettled,
    windows: windows.length,
  };

  setProxyGate.resolve();
  const statuses = await Promise.all([first, second]);
  assert.deepEqual(beforeRelease, {
    upstreamCalls: 1,
    created: 1,
    starts: 1,
    firstSettled: false,
    secondSettled: false,
    windows: 0,
  });
  assert.deepEqual(statuses.map((status) => status.open), [true, true]);
  assert.equal(windows.length, 1);
});

test('proxy.start 失败会清理，下次 open 重新初始化', { timeout: 2_000 }, async () => {
  const created = [];
  const { instance, windows } = harness({
    createProxy() {
      const attempt = created.length;
      const proxy = {
        port: 18_443 + attempt,
        stats: { total: 0, withBody: 0, failed: 0 },
        starts: 0,
        stops: 0,
        async start() {
          this.starts += 1;
          if (attempt === 0) throw new Error('start failed');
          return this.port;
        },
        async stop() { this.stops += 1; },
      };
      created.push(proxy);
      return proxy;
    },
  });

  await assert.rejects(instance.open(), /start failed/);
  assert.equal(created.length, 1);
  assert.equal(created[0].stops, 1);
  assert.equal(windows.length, 0);

  const status = await instance.open();
  assert.equal(status.open, true);
  assert.equal(created.length, 2);
  assert.equal(created[1].starts, 1);
});

test('session.setProxy 失败会停代理，下次 open 可重试', { timeout: 2_000 }, async () => {
  const created = [];
  const { instance, windows, browserSession } = harness({
    createProxy() {
      const proxy = {
        port: 18_443 + created.length,
        stats: { total: 0, withBody: 0, failed: 0 },
        starts: 0,
        stops: 0,
        async start() { this.starts += 1; return this.port; },
        async stop() { this.stops += 1; },
      };
      created.push(proxy);
      return proxy;
    },
  });
  let forwardAttempts = 0;
  browserSession.setProxy = async function setProxy(settings) {
    this.proxySettings.push(settings);
    if (settings.mode === 'direct') return;
    forwardAttempts += 1;
    if (forwardAttempts === 1) throw new Error('setProxy failed');
  };

  await assert.rejects(instance.open(), /setProxy failed/);
  assert.equal(created.length, 1);
  assert.equal(created[0].stops, 1);
  assert.equal(windows.length, 0);
  assert.deepEqual(browserSession.proxySettings.at(-1), { mode: 'direct' });

  const status = await instance.open();
  assert.equal(status.open, true);
  assert.equal(created.length, 2);
  assert.equal(forwardAttempts, 2);
});

test('dispose 可越过未返回的上游代理配置，旧 open 不会复活资源', { timeout: 2_000 }, async () => {
  const upstream = deferred();
  const upstreamEntered = deferred();
  let created = 0;
  const { instance, windows, browserSession } = harness({
    async getUpstreamProxy() {
      upstreamEntered.resolve();
      return upstream.promise;
    },
    createProxy() {
      created += 1;
      return {
        port: 18_443,
        stats: { total: 0, withBody: 0, failed: 0 },
        async start() { return this.port; },
        async stop() {},
      };
    },
  });

  const opening = instance.open();
  await upstreamEntered.promise;
  await instance.dispose();
  await instance.dispose();

  upstream.resolve('proxy.internal:8080');
  await assert.rejects(opening, /销毁|disposed/i);
  await assert.rejects(instance.open(), /销毁|disposed/i);
  assert.equal(created, 0);
  assert.equal(windows.length, 0);
  assert.deepEqual(browserSession.proxySettings, [{ mode: 'direct' }]);
});

test('dispose 在 proxy.start 完成后停掉代理，旧 open 不再开窗', { timeout: 2_000 }, async () => {
  const startGate = deferred();
  const startEntered = deferred();
  const { instance, windows, proxy, browserSession } = harness();
  proxy.start = async function start() {
    this.starts += 1;
    startEntered.resolve();
    await startGate.promise;
    return this.port;
  };

  const opening = instance.open();
  await startEntered.promise;
  let disposeSettled = 0;
  const disposing = [
    instance.dispose().finally(() => { disposeSettled += 1; }),
    instance.dispose().finally(() => { disposeSettled += 1; }),
  ];
  await nextTurn();
  const settledBeforeStart = disposeSettled;
  startGate.resolve();

  await Promise.all(disposing);
  await assert.rejects(opening, /销毁|disposed/i);
  assert.equal(settledBeforeStart, 0);
  assert.equal(disposeSettled, 2);
  assert.equal(proxy.stops, 1);
  assert.equal(windows.length, 0);
  assert.deepEqual(browserSession.proxySettings.at(-1), { mode: 'direct' });
});

test('dispose 在 session.setProxy 期间会最终停代理并撤回分区设置', { timeout: 2_000 }, async () => {
  const setProxyGate = deferred();
  const setProxyEntered = deferred();
  const { instance, windows, proxy, browserSession } = harness();
  browserSession.setProxy = async function setProxy(settings) {
    this.proxySettings.push(settings);
    if (settings.mode !== 'direct') {
      setProxyEntered.resolve();
      await setProxyGate.promise;
    }
  };

  const opening = instance.open();
  await setProxyEntered.promise;
  let disposeSettled = 0;
  const disposing = [
    instance.dispose().finally(() => { disposeSettled += 1; }),
    instance.dispose().finally(() => { disposeSettled += 1; }),
  ];
  await nextTurn();
  const settledBeforeSetProxy = disposeSettled;
  setProxyGate.resolve();

  await Promise.all(disposing);
  await assert.rejects(opening, /销毁|disposed/i);
  assert.equal(settledBeforeSetProxy, 0);
  assert.equal(disposeSettled, 2);
  assert.equal(proxy.stops, 1);
  assert.equal(windows.length, 0);
  assert.deepEqual(browserSession.proxySettings.at(-1), { mode: 'direct' });
});

test('物理像素尺寸、关窗保留代理、dispose 撤销代理并销毁窗口', async () => {
  const { instance, windows, proxy, browserSession } = harness();
  await instance.open();
  assert.deepEqual(instance.contentSize(), {
    width: 2002, height: 1402, cssWidth: 1000, cssHeight: 700,
  });
  assert.equal(instance.mediaSourceId(), 'window:7');

  await instance.close();
  assert.equal(proxy.stops, 0);
  assert.equal(instance.isOpen, false);

  await instance.open();
  const secondWindow = windows[1];
  const firstDispose = instance.dispose();
  const secondDispose = instance.dispose();
  assert.equal(firstDispose, secondDispose);
  await Promise.all([firstDispose, secondDispose]);
  assert.equal(proxy.stops, 1);
  assert.deepEqual(browserSession.proxySettings.at(-1), { mode: 'direct' });
  assert.equal(secondWindow.destroyed, true);
  assert.equal(instance.window, null);
  assert.equal(instance.guest, null);
  await assert.rejects(instance.open(), /销毁|disposed/i);
});

test('普通 Node 可加载模块，缺少 Electron 注入时给出明确错误', () => {
  assert.throws(() => new BrowserInstance('x'), /options\.electron\.BrowserWindow/);
});
