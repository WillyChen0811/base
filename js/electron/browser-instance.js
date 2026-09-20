'use strict';

const { EventEmitter } = require('node:events');
const { pathToFileURL } = require('node:url');
const { normalizeConsoleMessage } = require('./console-message');

/**
 * Electron 浏览器窗口的通用生命周期。
 *
 * Electron、代理实现与应用资源路径全部由调用方传入，因此这个模块在普通 Node
 * 进程里也能加载和测试。业务层只需负责选择 partition、标题与 preload/html。
 */
class BrowserInstance extends EventEmitter {
  constructor(id, options = {}) {
    super();
    const electron = options.electron || {};
    if (typeof electron.BrowserWindow !== 'function') {
      throw new TypeError('BrowserInstance 需要 options.electron.BrowserWindow');
    }
    if (!electron.session || typeof electron.session.fromPartition !== 'function') {
      throw new TypeError('BrowserInstance 需要 options.electron.session');
    }
    // screen 也要在这里验：它只在 contentSize() 里用，而那里拿不到就按 1 倍走，
    // 失败方式是录像悄悄糊掉（见 contentSize 的注释），没有任何报错。
    if (!electron.screen || typeof electron.screen.getDisplayNearestPoint !== 'function') {
      throw new TypeError('BrowserInstance 需要 options.electron.screen');
    }
    if (typeof options.createProxy !== 'function') {
      throw new TypeError('BrowserInstance 需要 options.createProxy');
    }
    if (!options.partition) throw new TypeError('BrowserInstance 需要 options.partition');
    if (!options.shellFile || !options.shellPreload || !options.guestPreload) {
      throw new TypeError('BrowserInstance 需要 shellFile、shellPreload 和 guestPreload');
    }

    this.id = id;
    this.electron = electron;
    this.createProxy = options.createProxy;
    this.getUpstreamProxy = options.getUpstreamProxy || (async () => '');
    this.partition = options.partition;
    this.shellFile = options.shellFile;
    this.shellPreload = options.shellPreload;
    this.guestPreload = options.guestPreload;
    this.title = options.title || '';
    this.width = options.width || 1280;
    this.height = options.height || 900;
    this.window = null;
    this.guest = null;          // <webview> 的 webContents（被测页面本身）
    this.proxy = null;
    this._proxyStartPromise = null;
    this._proxyCandidate = null;
    this._proxyStopPromises = new WeakMap();
    this._disposed = false;
    this._disposePromise = null;
    this.currentUrl = '';
  }

  // ---------------------------------------------------------------- 窗口

  /**
   * 打开浏览器。**只开窗口、起代理，不产生任何录制**——这正是抽出这一层的意义。
   *
   * 已经开着就是"回到浏览器"：聚焦过去、按需把网址点过去，**不重开**。
   * 重开会把登录态与已经点到的那一页全丢掉，而用户按的只是一个按钮。
   */
  async open({ url = '' } = {}) {
    this._assertNotDisposed();
    await this._startProxy();
    // dispose 可能发生在代理初始化的任意一个 await 里。代理那边会负责清理自己，
    // 窗口这边也必须再验一次，不能让已经销毁的页签在旧 open 恢复后重新开窗。
    this._assertNotDisposed();

    if (this.window && !this.window.isDestroyed()) {
      this.window.focus();
      // **guest 还没挂上时不导航**：那会抛"浏览器窗口未打开"，而窗口明明好好
      // 开着，报到界面上是一句谁也看不懂的失败。窗口刚建出来那几百毫秒里正是
      // 这个状态，而首页的网址在 loadFile 的 query 里已经带进去了。
      if (url && this.guest && !this.guest.isDestroyed()) await this.navigate(url);
      this._changed();
      return this.status();
    }

    const { BrowserWindow } = this.electron;
    this.window = new BrowserWindow({
      width: this.width,
      height: this.height,
      title: this.title,
      webPreferences: {
        webviewTag: true,          // 地址栏那层外壳要用 <webview> 装被测页面
        contextIsolation: true,
        nodeIntegration: false,
        sandbox: false,
        preload: this.shellPreload,
      },
    });

    // guestPreload：被测页面里的操作采集（`browser/guest-preload.js`）。
    // **路径由主进程算好传过去**，不在 shell.html 里拼——打包后 asar 里的路径
    // 与开发时不是一回事，而 <webview> 的 preload 指不到文件时是**一声不响地
    // 不加载**，症状是"操作步骤一条都没有"，看不出是路径的事。
    this.window.loadFile(this.shellFile, {
      query: {
        partition: this.partition,
        tabId: String(this.id),
        guestPreload: pathToFileURL(this.guestPreload).toString(),
        ...(url ? { url } : {}),
      },
    });

    // **preload 与子 frame 支持在这里设**，不靠 <webview> 的属性：
    // `nodeIntegrationInSubFrames` 决定 preload 跑不跑进 iframe，而老系统几乎
    // 全是 iframe 框架页——不开的话内页里的操作**一条都采不到**（实测：框架页
    // 只有一条"打开页面"，里面点保存、填标题全丢）。渲染进程也因此选不了
    // preload，安全上更紧一点。
    this.window.webContents.on('will-attach-webview', (_event, webPreferences) => {
      webPreferences.preload = this.guestPreload;
      webPreferences.nodeIntegrationInSubFrames = true;
    });
    // **外壳自己装好了也算一次状态变化**。`_changed()` 在窗口刚建出来时就发过
    // 一次，可那时 shell.html 还没加载，`webContents.send` 是直接丢掉的——控制条
    // 于是停在"什么都没在录"的默认样子上，「开始录」压根不出现。原先它靠被测
    // 页面 did-finish-load 那一路兜住，而那是另一件事的副作用，空白页或页面加载
    // 失败时就不成立了。
    this.window.webContents.on('did-finish-load', () => this._changed());
    this.window.webContents.on('did-attach-webview', (_event, guest) => this._bindGuest(guest));
    // 录制中改了窗口大小，画面从那一刻起就带黑边了（帧尺寸是开录时定死的，
    // 中途换分辨率会让 WebM 多数播放器直接崩，所以不换）。这一层只管把事实发
    // 出去，**要不要提醒是订阅者的事**——没在录的时候拉窗口什么都不该发生。
    this.window.on('resize', () => {
      const size = this.contentSize();
      if (size) this.emit('resized', size);
    });
    this.window.on('closed', () => {
      this.window = null;
      this.guest = null;
      // **窗口关掉不停代理**：关窗再点「打开浏览器」是常规动作（页面卡了、想
      // 换个干净窗口重来一段），代理留着下次直接用；真要停在 dispose 里做。
      this._changed();
    });

    this._changed();
    return this.status();
  }

  async close() {
    if (this.window && !this.window.isDestroyed()) this.window.close();
    return this.status();
  }

  /** 窗口在不在（界面按钮该显示「打开浏览器」还是「回到浏览器」，看这一个） */
  get isOpen() {
    return !!(this.window && !this.window.isDestroyed());
  }

  status() {
    return {
      id: this.id,
      open: this.isOpen,
      url: this.currentUrl,
      partition: this.partition,
      proxyPort: this.proxy ? this.proxy.port : 0,
      stats: this.proxy ? this.proxy.stats : { total: 0, withBody: 0, failed: 0 },
    };
  }

  // ---------------------------------------------------------------- 导航
  //
  // 地址栏在 shell.html 里自成一套（直接驱动 <webview>），这几个方法是给主窗口
  // 那边的控制台用的——两边操作的是同一个 guest，不会打架。

  async navigate(url) {
    if (!this.guest || this.guest.isDestroyed()) throw new Error('浏览器窗口未打开');
    let target = String(url || '').trim();
    if (target && !/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(target)) target = `http://${target}`;
    await this.guest.loadURL(target);
    return this.status();
  }

  // Electron 30 起 webContents 的前进后退挪进了 navigationHistory，旧的还在但已弃用。
  // 两条都走一遍，升级到它们真被删掉的那天也不用回来改。
  _history() {
    return this.guest?.navigationHistory || this.guest;
  }

  goBack() {
    const h = this._history();
    if (h?.canGoBack()) h.goBack();
    return this.status();
  }

  goForward() {
    const h = this._history();
    if (h?.canGoForward()) h.goForward();
    return this.status();
  }

  reload() {
    this.guest?.reload();
    return this.status();
  }

  /** 截图打点用的目标：**优先被测页面**，地址栏与录制条是我们自己的界面 */
  captureTarget() {
    if (this.guest && !this.guest.isDestroyed()) return this.guest;
    if (this.isOpen) return this.window.webContents;
    return null;
  }

  /**
   * 给**被测页面**开开发者工具（不是这层外壳的）。
   *
   * 没有它，"页面在我们这儿不对劲、在 Chrome 里好好的"这类问题只能靠猜——
   * 而这正是自制浏览器最容易碰上的一类：代理、自签证书、注进去的采集脚本，
   * 三样都可能是嫌疑人，可页面自己的报错谁也看不见。
   *
   * **一律 detach 开**：docked 会把 webview 的内容区挤窄，而录像的帧尺寸是开录
   * 那一刻定死的——录着的时候开一下工具，剩下的画面就全带黑边了。
   */
  openGuestDevTools() {
    const wc = (this.guest && !this.guest.isDestroyed()) ? this.guest : null;
    if (!wc) throw new Error('被测页面还没打开');
    if (wc.isDevToolsOpened()) wc.closeDevTools();
    else wc.openDevTools({ mode: 'detach' });
    return this.status();
  }

  /** 录像要录的窗口 */
  mediaSourceId() {
    return this.isOpen ? this.window.getMediaSourceId() : null;
  }

  /**
   * 窗口内容区现在多大。**录像按它锁帧尺寸**：不给尺寸的话帧多大完全由采集那边
   * 协商，比例与窗口对不上就补黑边——黑边会被编码进文件，既难看又白占码率。
   *
   * **给的是物理像素，不是 `getContentSize()` 那个逻辑像素**。这一条原先是反着
   * 写的（"按物理像素录出来同样码率反而更糊"），而那是推出来的、没验过——
   * **糊的真正原因就在这儿**：Retina 上 scaleFactor=2，窗口 1280×835 实际是
   * 2560×1670 个真实像素，拿 1280 当 maxWidth 交给 getUserMedia，采集那边就先
   * 把画面**缩掉一半**再编码，回放时再放大回去，字自然是糊的。
   * 本机实测（Electron 43.3.0，窗口 1280×835、scaleFactor=2、8fps）：
   * 按逻辑像素录出来的帧是 1228×830，按物理像素是 2464×1670；同一段滚动画面
   * 截同一块区域放到同样大小看，前者的「需求编号 GN-45.1」笔画糊成一团、
   * 后者一眼可读。**分辨率是主因，不是码率**（同为 800kbps 时差别就已经这么大）。
   *
   * 体积代价实测(8 秒、一直滚动的表格，最坏情形)：逻辑像素 341KB、物理像素
   * 482KB；静止页面则是 118KB 对 258KB。换算成一小时仍在几百 MB 这个量级，
   * 没有变成"几个 GB"。
   *
   * `cssWidth/cssHeight` 是窗口在界面上的大小，只用来对人说话（"把窗口从
   * 1280×835 改成了…"）——跟用户说 2560×1670 他对不上自己看到的窗口。
   *
   * 宽高**取偶数**：编码器对奇数尺寸各有各的脾气，差一个像素不值得去赌。
   */
  contentSize() {
    if (!this.isOpen) return null;
    const [w, h] = this.window.getContentSize();
    const even = (n) => Math.max(2, Math.floor(n / 2) * 2);
    let scale = 1;
    try {
      // 窗口可能被拖到外接屏上，缩放比按**它现在所在的那块屏**算。
      // 取不到就当 1：少乘一次只是回到原先那份糊，比拦下不录强。
      scale = this.electron.screen.getDisplayNearestPoint(this.window.getBounds()).scaleFactor || 1;
    } catch (e) { /* 拿不到就按 1 倍 */ }
    return {
      width: even(w * scale),
      height: even(h * scale),
      cssWidth: even(w),
      cssHeight: even(h),
    };
  }

  /** 外壳工具条上的状态由订阅者算好推过来（两边显示的必须是同一份） */
  sendToShell(channel, payload) {
    if (this.isOpen) this.window.webContents.send(channel, payload);
  }

  // ---------------------------------------------------------------- 内部

  /**
   * 起代理，并把这个分区的流量指过去。
   *
   * `setCertificateVerifyProc` 只作用在**这一个分区**上：这个浏览器信任我们自签
   * 的证书，机器上别的软件、macClient 自己跟后端的通信都不受影响，也不需要往系统
   * 里装根证书。这是自带浏览器相对 Burp 的便宜——服务的对象就是自己的窗口。
   */
  async _startProxy() {
    this._assertNotDisposed();
    if (this.proxy) return;
    // 多个 open 会一起走到这里（例如界面连点两次）。共享完整的初始化过程，既避免
    // 起两个代理，也保证所有调用方看到同一个成功或失败结果。
    if (this._proxyStartPromise) return this._proxyStartPromise;

    const startPromise = this._initializeProxy();
    this._proxyStartPromise = startPromise;
    try {
      await startPromise;
    } finally {
      if (this._proxyStartPromise === startPromise) this._proxyStartPromise = null;
    }
  }

  async _initializeProxy() {
    let upstreamProxy = '';
    try { upstreamProxy = await this.getUpstreamProxy(); } catch (e) { /* 问不到就直连 */ }
    this._assertNotDisposed();

    // **一律往外发，不在这里判断该不该记**：暂停、会话都是订阅者的事。
    // 这一层判断了，流量录制一暂停，画面与步骤也跟着停。
    const proxy = this.createProxy({
      onRequest: (record) => { if (!this._disposed) this.emit('request', record); },
      onError: (message) => { if (!this._disposed) this.emit('error', message); },
      upstreamProxy,
    });
    this._proxyCandidate = proxy;
    let ses = null;
    let proxyWasConfigured = false;
    try {
      // createProxy 是调用方注入的；即使它同步触发了 dispose，也不能再启动候选资源。
      this._assertNotDisposed();
      // RecordingProxy.start 自己包含多段异步 listen；它没结束时不能抢先 stop，
      // 否则 start 后半段会继续使用已经被 stop 清空的 server。
      const port = await proxy.start();
      this._assertNotDisposed();

      ses = this.electron.session.fromPartition(this.partition);
      proxyWasConfigured = true;
      await ses.setProxy({
        proxyRules: `http=127.0.0.1:${port};https=127.0.0.1:${port}`,
        // 本机地址不要绕过代理：被测系统跑在本机是常事，绕过去就录不到了
        proxyBypassRules: '<-loopback>',
      });
      this._assertNotDisposed();
      // 0 = 采纳。这个分区的流量全部经我们的代理，证书本来就是我们签的
      ses.setCertificateVerifyProc((_request, callback) => callback(0));
      this.proxy = proxy;
      this._proxyCandidate = null;
    } catch (error) {
      // 候选代理绝不能留到下一次 open：否则失败会被误认成已经初始化成功。
      await this._stopProxyOnce(proxy);
      if (proxyWasConfigured && ses) await this._setDirectProxy(ses);
      if (this._proxyCandidate === proxy) {
        this._proxyCandidate = null;
      }
      throw error;
    }
  }

  _assertNotDisposed() {
    if (this._disposed) throw new Error('BrowserInstance 已销毁');
  }

  _stopProxyOnce(proxy) {
    if (!proxy) return Promise.resolve();
    let stopping = this._proxyStopPromises.get(proxy);
    if (!stopping) {
      stopping = Promise.resolve().then(() => proxy.stop()).catch(() => {});
      this._proxyStopPromises.set(proxy, stopping);
    }
    return stopping;
  }

  async _setDirectProxy(ses = null) {
    try {
      const target = ses || this.electron.session.fromPartition(this.partition);
      await target.setProxy({ mode: 'direct' });
    } catch (e) { /* 撤不下来也要接着往下走 */ }
  }

  _bindGuest(guest) {
    this.guest = guest;

    // 弹窗改成在同一个 webview 里打开：新窗口是另一个 webContents，虽然也走同一个
    // 分区因而照样被代理录到，但用户会在两个窗口之间迷路，地址栏也对不上。
    guest.setWindowOpenHandler(({ url }) => {
      if (url && url !== 'about:blank') guest.loadURL(url);
      return { action: 'deny' };
    });

    const push = () => {
      this.currentUrl = guest.isDestroyed() ? '' : guest.getURL();
      this._changed();
    };

    /**
     * 跳转也是一步操作。**由这一层发，不在页面里 hook `history.pushState`**：
     * preload 跑在隔离世界，往那儿打的补丁拦不到页面自己的调用；而单页应用换
     * 路由压根不触发 DOMContentLoaded，实测整场只有开头一条"打开页面"，看流水
     * 完全看不出人从哪一页走到了哪一页。Electron 的 did-navigate-in-page 对
     * pushState 与 hash 都报，这是现成的、也是唯一靠得住的信号。
     *
     * **时间戳取事件发生的那一刻**，文字晚 250ms 再取：单页应用是先换路由再改
     * document.title 的，当场读到的还是上一页的标题。时间对得上就行，文字晚一
     * 点没关系——步骤在时间轴上的位置按 ts 算。
     */
    const emitNav = (url) => {
      const ts = Date.now();
      setTimeout(() => {
        if (guest.isDestroyed()) return;
        const title = guest.getTitle();
        this.emit('step', {
          ts,
          kind: 'nav',
          page_url: url,
          page_title: title,
          target: `打开页面「${title || url}」`,
        });
      }, 250);
    };
    let lastNavUrl = '';
    const navOnce = (url) => {
      if (url === lastNavUrl) return;   // 同一个地址重复报不算又跳了一次
      lastNavUrl = url;
      emitNav(url);
    };

    // **两个事件的签名不一样，分开写**：did-navigate 的第三个参数是 HTTP 状态码
    // （实测拿到 200），did-navigate-in-page 的才是 isMainFrame。合成一个处理器
    // 时把状态码当成了 isMainFrame，现在碰巧能过（200 不等于 false），改天换个
    // 状态码或者调换判断方向就悄悄失灵。
    guest.on('did-navigate', (_event, url) => {
      push();
      navOnce(url);
    });
    guest.on('did-navigate-in-page', (_event, url, isMainFrame) => {
      push();
      // iframe 自己的跳转不当作一步：框架页里那些内页地址对看流水的人没有意义，
      // 里面的点击与输入照样采得到（nodeIntegrationInSubFrames）
      if (isMainFrame === false) return;
      navOnce(url);
    });
    guest.on('did-finish-load', push);
    guest.on('destroyed', () => { this.guest = null; });

    // 控制台日志用 Electron 自己的事件就够，不必为它开一路 CDP。
    // 事件签名换过（位置参数 35.0 起弃用），归一化见 ./console-message.js
    guest.on('console-message', (...args) => {
      const m = normalizeConsoleMessage(args);
      this.emit('console', {
        kind: 'console',
        level: m.level,
        text: m.message.slice(0, 4000),
        url: m.source,
        line_no: m.line,
        wall_time: Date.now() / 1000,
      });
    });
    this._changed();
  }

  _changed() {
    this.emit('changed', this.status());
  }

  /**
   * 收工：停代理、关窗口。
   *
   * **停代理必须先把连着的 socket 全掐掉**（在 proxy.stop 里做）：`server.close()`
   * 只是"不再接新连接"，而窗口开着时永远有连着的（keep-alive、CONNECT 隧道、
   * WebSocket 都不会自己断），回调不来就永远挂着，一路挂到界面上就是关页签时整个
   * 卡死——没人报错，只是没返回。
   */
  dispose() {
    if (this._disposePromise) return this._disposePromise;
    // 先立终态，再做任何异步清理。这样旧 open 与之后的新 open 都无法复活窗口。
    this._disposed = true;
    this.removeAllListeners();
    this._disposePromise = this._dispose();
    return this._disposePromise;
  }

  async _dispose() {
    const proxy = this.proxy;
    const candidate = this._proxyCandidate;

    if (proxy) {
      await this._stopProxyOnce(proxy);
      this.proxy = null;
    }
    if (candidate) {
      // start 与 stop 不能并发；setProxy 也必须等旧调用返回并回滚，才能保证 dispose
      // 返回后同 partition 的新实例不会再被旧初始化覆盖。失败分支负责统一收口。
      try { await this._proxyStartPromise; } catch (e) { /* 清理由初始化的失败分支完成 */ }
    }
    // 分区的代理设置要撤回去，否则窗口再开就走一个已经没了的端口
    await this._setDirectProxy();
    if (this.window && !this.window.isDestroyed()) this.window.destroy();
    this.window = null;
    this.guest = null;
  }
}

module.exports = { BrowserInstance };
