/**
 * 录制代理 —— Burp 那条路：浏览器的流量全从这里过一遍。
 *
 * 为什么不向 Chromium 内核要数据（CDP）：实测内核在跨进程导航之后会**不报**
 * 页面开头那一截的 `requestWillBeSent`，漏掉的正好是首页静态资源和第一个接口
 * （通常就是登录），响应事件却一条不少——界面上根本发现不了漏了什么。代理是
 * 结构上不会漏：请求不经过它就出不去，请求头/体、响应头/体也都在一个地方拿到，
 * 不用跟两套事件流对账。
 *
 * ## HTTPS 怎么解
 *
 * `CONNECT` 之后用调用方 `certFor(hostname)` 签出的证书接管 TLS，解密、记录、
 * 再用真身份转发上游。证书存放位置和信任范围都由调用方决定，代理不创建默认 CA。
 *
 * ## 三个刻意的选择
 *
 * - **ALPN 只报 http/1.1**：把浏览器压到 HTTP/1.1，h2 的多路复用/HPACK 对录制
 *   毫无增益。
 * - **上游不校验证书**（`rejectUnauthorized: false`）：被测系统用自签/过期证书
 *   是常态，这是测试工具不是浏览器。
 * - **响应体存解压后的，原样转发压缩内容给浏览器**：存 gzip 的二进制块界面上
 *   看不了、关联也扫不了。
 * - **采集侧一个字段都不过滤**：要不要剔静态资源是查询与导出时的事——采集时
 *   丢了用户再也找不回来。
 */

const http = require('http');
const https = require('https');
const net = require('net');
const tls = require('tls');
const zlib = require('zlib');
const { URL } = require('url');

// 单个响应体最多存这么多；超过的照记元数据，只是不留正文
const MAX_BODY_BYTES = 2 * 1024 * 1024;

// 这些类型不存正文：存下来是一堆乱码，界面看不了，关联也扫不出东西
const BINARY_MIME_PREFIXES = [
  'image/', 'video/', 'audio/', 'font/',
  'application/octet-stream', 'application/pdf', 'application/zip',
  'application/x-rar', 'application/x-7z', 'application/gzip', 'application/wasm',
];

// 只在浏览器与代理之间有意义的头，不能转发给上游
const HOP_BY_HOP = new Set([
  'proxy-connection', 'connection', 'keep-alive', 'transfer-encoding',
  'te', 'trailer', 'upgrade', 'proxy-authorization', 'proxy-authenticate',
]);

// Chromium 会带 Sec-Fetch-Dest，正好就是资源类型，不用再猜
const DEST_TO_TYPE = {
  document: 'Document', iframe: 'Document', frame: 'Document',
  script: 'Script', style: 'Stylesheet', image: 'Image', font: 'Font',
  audio: 'Media', video: 'Media', track: 'Media',
  manifest: 'Manifest', worker: 'Script', sharedworker: 'Script',
  empty: 'Fetch', object: 'Other', embed: 'Other', report: 'Other',
};

/**
 * 把一个连接错误说成人话。
 *
 * **不能只取 e.message**：Node 在 Happy Eyeballs（同时试 IPv4/IPv6）全部失败时
 * 抛的是 AggregateError，而它的 message 是**空字符串**，真正的原因分散在
 * .errors 数组里。只取 message 的结果是——界面上一排"失败"，却一条原因都
 * 没有，等于告诉用户"出错了，但我不告诉你为什么"。
 *
 * code 也要带上：ECONNREFUSED / ETIMEDOUT / ENOTFOUND 这几个词本身就把问题
 * 说清楚了，比后面那句英文描述更有用。
 */
function describeError(e) {
  if (!e) return '未知错误';
  const parts = [];
  if (e.code) parts.push(e.code);
  if (e.message) parts.push(e.message);
  if (Array.isArray(e.errors) && e.errors.length) {
    const sub = e.errors
      .map((x) => [x?.code, x?.message].filter(Boolean).join(' '))
      .filter(Boolean);
    if (sub.length) parts.push(`(${[...new Set(sub)].join('；')})`);
  }
  return parts.join(' ').trim() || String(e);
}

/**
 * 把一段字节放进 JSON 记录里。
 *
 * **抓包收到的是任意字节**：GBK 的页面、二进制上传、压根不是文本的东西都有。
 * 而 JSON 塞不下裸字节，所以只有两条路——能当 UTF-8 文本就存文本，不能就
 * base64。以前一律 `buf.toString('utf8')`，非 UTF-8 的字节会被静默替换成
 * U+FFFD：一个 GBK 的表单提交抓下来是一串问号，**数据当场就毁了**，导出的
 * 脚本自然也是废的。
 *
 * 判定办法是解一遍再编回去比字节：能原样还原才是合法 UTF-8。
 */
function encodeBody(buf) {
  if (!buf || !buf.length) return { text: null, isBase64: 0 };
  const text = buf.toString('utf8');
  if (Buffer.from(text, 'utf8').equals(buf)) return { text, isBase64: 0 };
  return { text: buf.toString('base64'), isBase64: 1 };
}

function isBinaryMime(mime) {
  const m = (mime || '').toLowerCase();
  return BINARY_MIME_PREFIXES.some((p) => m.startsWith(p));
}

function resourceType(headers, contentType) {
  const dest = String(headers['sec-fetch-dest'] || '').toLowerCase();
  if (DEST_TO_TYPE[dest]) return DEST_TO_TYPE[dest];
  const ct = (contentType || '').toLowerCase();
  if (ct.startsWith('text/html')) return 'Document';
  if (ct.startsWith('text/css')) return 'Stylesheet';
  if (ct.includes('javascript')) return 'Script';
  if (ct.startsWith('image/')) return 'Image';
  if (ct.startsWith('font/') || ct.includes('font')) return 'Font';
  if (ct.includes('json') || ct.includes('xml')) return 'Fetch';
  return 'Other';
}

/** 按 Content-Encoding 解压一份用于存储；解不开就当没有正文，并说明原因。 */
function decodeBody(buf, encoding) {
  const enc = String(encoding || '').toLowerCase().trim();
  try {
    if (!enc || enc === 'identity') return { buf, note: null };
    if (enc === 'gzip' || enc === 'x-gzip') return { buf: zlib.gunzipSync(buf), note: null };
    if (enc === 'deflate') return { buf: zlib.inflateSync(buf), note: null };
    if (enc === 'br') return { buf: zlib.brotliDecompressSync(buf), note: null };
    if (enc === 'zstd' && zlib.zstdDecompressSync) {
      return { buf: zlib.zstdDecompressSync(buf), note: null };
    }
    return { buf: null, note: `无法解压的编码（${enc}）` };
  } catch (e) {
    return { buf: null, note: `解压失败（${enc}）：${e.message}` };
  }
}

/**
 * 上游代理 —— **我们自己出站时要不要再经一层代理**。
 *
 * 为什么需要：内网被测系统常常只能经某个代理访问（实测过直连 TLS 握手被掐、
 * 经本地代理如 Clash 才通的情形——浏览器能开是因为流量被那个客户端接管了，
 * 换 TLS 参数救不了直连）。
 *
 * 配置来源（按顺序，先有的赢）：1. 调用方给的 2. 环境变量 `https_proxy` /
 * `http_proxy` / `all_proxy`。**环境变量只在从终端起的时候有**：从 Dock/Finder
 * 双击起来的 app 不继承 shell 环境，所以界面上的填法是必需的，env 只是开发时
 * 顺手。
 */
function parseUpstreamProxy(value) {
  const raw = String(value || process.env.https_proxy || process.env.HTTPS_PROXY
    || process.env.http_proxy || process.env.HTTP_PROXY || '').trim();
  if (!raw) return null;
  try {
    const u = new URL(/^\w+:\/\//.test(raw) ? raw : `http://${raw}`);
    // **socks 不支持**：那要另一套握手，而 CONNECT 这条路一份代码管住 http/https。
    // 认不出来就当没配，并让调用方说出来——静默直连正是这次查了半天的那个坑。
    if (!/^https?:$/.test(u.protocol)) return { unsupported: raw };
    return {
      host: u.hostname,
      port: Number(u.port) || (u.protocol === 'https:' ? 443 : 80),
      auth: u.username ? `${decodeURIComponent(u.username)}:${decodeURIComponent(u.password || '')}` : '',
      shown: `${u.hostname}:${u.port || 80}`,
    };
  } catch (e) {
    return { unsupported: raw };
  }
}

/**
 * 这个主机要不要绕过上游代理。
 *
 * **本机地址一律绕过**：被测系统跑在本机是常态，把 127.0.0.1 塞给 Clash 那类
 * 代理轻则绕一圈重则直接不通。另外认 `no_proxy`（逗号分隔、后缀匹配、`*` 全绕过，
 * 与各家工具同规矩）。**内网网段不在默认绕过之列**：内网站点往往偏偏只有经
 * 代理才连得上，按"内网就直连"猜是猜错的。
 */
function bypassProxy(host) {
  const h = String(host || '').toLowerCase();
  // 整个 127/8 都是本机，不只是 127.0.0.1
  if (h === 'localhost' || h === '::1' || h.endsWith('.localhost')) return true;
  if (/^127\.\d+\.\d+\.\d+$/.test(h)) return true;
  const raw = String(process.env.no_proxy || process.env.NO_PROXY || '').trim();
  if (!raw) return false;
  return raw.split(',').map((x) => x.trim().toLowerCase()).filter(Boolean).some(
    (rule) => rule === '*' || h === rule || h.endsWith(rule.startsWith('.') ? rule : `.${rule}`),
  );
}

/**
 * 经上游代理开一条到目标的 TCP 隧道（CONNECT）。
 *
 * 拿到 200 之后**剩下的字节要 unshift 回去**：代理可能把响应头和后面的数据粘在
 * 一个包里，丢掉那一截就是 TLS 握手少了开头几个字节，报出来仍旧是一句看不懂的
 * 握手失败。
 */
function connectThroughProxy(up, host, port, cb) {
  let settled = false;
  const done = (err, sock) => { if (!settled) { settled = true; cb(err, sock); } };
  const sock = net.connect(up.port, up.host, () => {
    const lines = [
      `CONNECT ${host}:${port} HTTP/1.1`,
      `Host: ${host}:${port}`,
    ];
    if (up.auth) lines.push(`Proxy-Authorization: Basic ${Buffer.from(up.auth).toString('base64')}`);
    sock.write(lines.join('\r\n') + '\r\n\r\n');
  });
  let buf = Buffer.alloc(0);
  const onData = (c) => {
    buf = Buffer.concat([buf, c]);
    const idx = buf.indexOf('\r\n\r\n');
    if (idx === -1) {
      if (buf.length > 16 * 1024) { sock.destroy(); done(new Error('上游代理的应答不成形')); }
      return;
    }
    sock.removeListener('data', onData);
    const head = buf.subarray(0, idx).toString('latin1');
    const first = head.split('\r\n')[0] || '';
    if (!/^HTTP\/1\.[01] 200/.test(first)) {
      sock.destroy();
      return done(new Error(`上游代理不给隧道：${first}`));
    }
    const rest = buf.subarray(idx + 4);
    if (rest.length) sock.unshift(rest);
    done(null, sock);
  };
  sock.on('data', onData);
  sock.on('error', (e) => done(e));
}

function cleanHeaders(headers) {
  const out = {};
  for (const [k, v] of Object.entries(headers || {})) {
    if (HOP_BY_HOP.has(k.toLowerCase())) continue;
    out[k] = Array.isArray(v) ? v.join('\n') : String(v);
  }
  return out;
}

class RecordingProxy {
  /**
   * @param {(record: object) => void} onRecord 一条请求走完时回调
   * @param {(message: string) => void} onError
   * @param {{certFor: (hostname: string) => {cert: string, key: string}, upstreamProxy?: string}} options
   */
  constructor(onRecord, onError, options = {}) {
    this.onRecord = onRecord;
    this.onError = onError || (() => {});
    if (typeof options.certFor !== 'function') {
      throw new TypeError('RecordingProxy requires options.certFor(hostname)');
    }
    this.certFor = options.certFor;
    // 出站再经一层代理（内网被测系统常常只有这一条路走得通，见 parseUpstreamProxy）
    this.upstreamProxy = parseUpstreamProxy(options.upstreamProxy);
    if (this.upstreamProxy && this.upstreamProxy.unsupported) {
      this.onError(`上游代理这个写法认不出来，这一趟按直连走：${this.upstreamProxy.unsupported}`);
      this.upstreamProxy = null;
    }
    this.server = null;
    this.tlsServer = null;
    this.port = 0;
    this.tlsPort = 0;
    this.seq = 0;
    this.stats = { total: 0, withBody: 0, failed: 0 };
    // **还没走完的请求数**。记录是在一条请求走完时才 `_emit` 出去的，所以
    // "队列空了"不等于"都录下来了"——点导出那一刻正在飞的那几条还没进队列。
    // 导出前的排空（TrafficRecorder.drain）靠它等一等，见 inflight()
    this._inflight = 0;
    // 所有还连着的 socket。**必须自己记一份**：见 stop() 里那条教训
    this.sockets = new Set();
  }

  /** 记下这条连接，断开时自己摘掉——stop() 要靠这份清单把它们全掐了 */
  _track(socket) {
    this.sockets.add(socket);
    socket.once('close', () => this.sockets.delete(socket));
  }

  async start() {
    // 一台 TLS 服务器用 SNI 应付所有域名，比每个域名起一个省事得多
    this.tlsServer = https.createServer({
      SNICallback: (servername, cb) => {
        try {
          cb(null, tls.createSecureContext(this.certFor(servername)));
        } catch (e) {
          cb(e);
        }
      },
      ALPNProtocols: ['http/1.1'],
    });
    this.tlsServer.on('request', (req, res) => this._onRequest(req, res, true));
    this.tlsServer.on('upgrade', (req, socket, head) => this._onUpgrade(req, socket, head, true));
    this.tlsServer.on('clientError', () => { /* 浏览器主动断开是常态 */ });

    this.server = http.createServer();
    this.server.on('request', (req, res) => this._onRequest(req, res, false));
    this.server.on('connect', (req, socket, head) => this._onConnect(req, socket, head));
    this.server.on('upgrade', (req, socket, head) => this._onUpgrade(req, socket, head, false));
    this.server.on('clientError', () => {});

    this.server.on('connection', (s) => this._track(s));
    this.tlsServer.on('connection', (s) => this._track(s));

    await new Promise((r) => this.tlsServer.listen(0, '127.0.0.1', r));
    await new Promise((r) => this.server.listen(0, '127.0.0.1', r));
    this.tlsPort = this.tlsServer.address().port;
    this.port = this.server.address().port;
    return this.port;
  }

  /**
   * 收摊。
   *
   * **先掐连接再 close，而且 close 要有超时兜底。**
   * `server.close()` 只是"不再接新连接"，已经连着的它会一直等——而录制窗口还开着
   * 时永远有连着的：keep-alive、CONNECT 隧道、WebSocket 都不会自己断。回调不来，
   * `await` 就永远挂着，一路挂到界面上就是**关页签时整个卡死**（页签关不掉、
   * 也没有任何提示，因为没人报错，只是没返回）。
   *
   * 所以：自己记着每条 socket，停的时候一律 destroy；close 再 race 一个超时，
   * 万一还有漏网的连接也不会把关闭流程拖住。端口本来就是随机的，留一条半死的
   * 连接也不影响下次再起一个。
   */
  async stop() {
    for (const s of this.sockets) {
      try { s.destroy(); } catch (e) { /* 已经断了 */ }
    }
    this.sockets.clear();

    for (const s of [this.server, this.tlsServer]) {
      if (!s) continue;
      // Node 18.2+ 有这个，直接掐掉服务器自己还记着的连接（含 keep-alive 空闲的）
      try { s.closeAllConnections?.(); } catch (e) { /* 老版本没有就算了 */ }
      await Promise.race([
        new Promise((r) => s.close(r)),
        new Promise((r) => setTimeout(r, 2000)),
      ]);
    }
    this.server = null;
    this.tlsServer = null;
  }

  // -------------------------------------------------------------- HTTPS

  /**
   * CONNECT：告诉浏览器隧道建好了，然后把它接到我们自己的 TLS 服务器上。
   * 浏览器以为对面是目标站点，实际是我们；解密后的请求会走 _onRequest。
   */
  _onConnect(req, clientSocket, head) {
    const upstream = net.connect(this.tlsPort, '127.0.0.1', () => {
      clientSocket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
      if (head && head.length) upstream.write(head);
      clientSocket.pipe(upstream);
      upstream.pipe(clientSocket);
    });
    const bye = () => { clientSocket.destroy(); upstream.destroy(); };
    upstream.on('error', bye);
    clientSocket.on('error', bye);
  }

  /**
   * WebSocket 等协议升级：**原样打通,不解析帧**。
   * 压测脚本用不到帧内容,而挡在这里会让被测系统的实时功能直接坏掉——
   * 录制工具让被测系统不能用,是最不能接受的事。列表里记一条,标明是升级连接。
   */
  _onUpgrade(req, clientSocket, head, isHttps) {
    const target = this._target(req, isHttps);
    const connector = isHttps ? tls : net;
    const opts = isHttps
      ? { host: target.hostname, port: target.port, rejectUnauthorized: false, servername: target.hostname }
      : { host: target.hostname, port: target.port };

    const upstream = connector.connect(opts, () => {
      const lines = [`${req.method} ${req.url} HTTP/1.1`];
      for (const [k, v] of Object.entries(req.headers)) lines.push(`${k}: ${v}`);
      upstream.write(lines.join('\r\n') + '\r\n\r\n');
      if (head && head.length) upstream.write(head);
      clientSocket.pipe(upstream);
      upstream.pipe(clientSocket);
    });
    const bye = () => { clientSocket.destroy(); upstream.destroy(); };
    upstream.on('error', bye);
    clientSocket.on('error', bye);

    this._emit(this._record({
      req, target, isHttps,
      status: 101, statusText: 'Switching Protocols',
      resourceType: 'WebSocket',
      responseHeaders: {},
      bodyKind: 'skipped',
      bodyNote: '协议升级连接（WebSocket），只打通不记录帧',
      startedAt: Date.now(),
    }));
  }

  // -------------------------------------------------------------- HTTP

  _target(req, isHttps) {
    // 普通 HTTP 代理收到的是绝对 URI；TLS 解密后收到的是路径 + Host 头
    if (!isHttps && /^https?:\/\//i.test(req.url)) {
      const u = new URL(req.url);
      return {
        hostname: u.hostname,
        port: Number(u.port) || 80,
        path: u.pathname + u.search,
        url: u.toString(),
      };
    }
    const sni = req.socket.servername || '';
    const hostHeader = String(req.headers.host || sni || '');
    const [h, p] = hostHeader.split(':');
    const hostname = sni || h || 'unknown';
    const port = Number(p) || (isHttps ? 443 : 80);
    const scheme = isHttps ? 'https' : 'http';
    const shown = (isHttps && port === 443) || (!isHttps && port === 80)
      ? hostname : `${hostname}:${port}`;
    return { hostname, port, path: req.url, url: `${scheme}://${shown}${req.url}` };
  }

  /** 还有几条请求没走完（没 `_emit` 出记录）。导出前的排空拿它判断该不该再等 */
  inflight() {
    return this._inflight;
  }

  _onRequest(req, res, isHttps) {
    const startedAt = Date.now();
    const target = this._target(req, isHttps);

    // 这一条从现在起算"在飞"，到响应彻底结束为止。**认 res 的 close 而不是
    // 认 `_emit`**：客户端中途断开、上游报错都不一定走到 `_emit`，只认 close
    // 才保证一定减得回来，不会把计数永久卡在非零上、让排空白等满超时。
    this._inflight += 1;
    let settled = false;
    const settle = () => {
      if (settled) return;
      settled = true;
      this._inflight -= 1;
    };
    res.on('close', settle);
    res.on('finish', settle);

    // 转发与录制是两件事：请求原样流给上游，录制副本才受大小上限约束。
    const reqChunks = [];
    let reqStored = 0;
    let reqBytes = 0;
    req.on('data', (c) => {
      reqBytes += c.length;
      const remaining = MAX_BODY_BYTES - reqStored;
      if (remaining > 0) {
        const kept = c.length <= remaining ? c : c.subarray(0, remaining);
        reqChunks.push(kept);
        reqStored += kept.length;
      }
    });

    const recordedBody = () => Buffer.concat(reqChunks);
    // 请求体截断了就得说出来：界面上"这条为什么只有半截 body"要当场答得上来，
    // 与响应体那边的 overflow 是同一条口径
    const reqNote = () => (reqBytes > reqStored
      ? `请求体超 ${Math.round(MAX_BODY_BYTES / 1024)}KB 上限，只存了前 ${reqStored} 字节（共 ${reqBytes}）`
      : null);
    const options = {
      host: target.hostname,
      port: target.port,
      method: req.method,
      path: target.path,
      headers: cleanHeaders(req.headers),
      rejectUnauthorized: false,   // 被测系统用自签/过期证书是常态
      servername: isHttps ? target.hostname : undefined,
    };

    const mod = isHttps ? https : http;

    // 重试一次用得上的两样：请求体存完整了没有、请求收完了没有
    let reqEnded = false;
    req.on('end', () => { reqEnded = true; });
    let retried = false;
    let retryNote = null;
    let current = null;      // 当前那次上游请求（浏览器取消时要掐的就是它）

    /**
     * 发一次给上游。`extra` 是重试时才加的那套更宽松的连接参数。
     *
     * **头一次失败不能就回 502**：浏览器碰上"连接刚建就被掐"会自己换条连接重来
     * （同一个页面 Chrome 打得开、我们这儿报 502），而 Node 会把失败原样抛上来，
     * 我们就把它当成了服务器的回答，尽管请求压根没到服务器。
     *
     * 重试那一次只换一样：**`agent: false`，不走连接池**（池里那条可能是服务器
     * 早关掉的 keep-alive 连接，或带着对端不认的 TLS 会话票）。
     *
     * **不顺手放宽 TLS 版本与加密套件**：`@SECLEVEL=` 是 OpenSSL 的写法，而
     * **Electron 用的是 BoringSSL**，加了它 `https.request` 会同步抛
     * `INVALID_COMMAND`，主进程直接崩——比 502 糟得多。真碰上只说 TLS 1.0 的
     * 系统，要照实测报错再加，且必须在 Electron 里验（系统 Node 用 OpenSSL，
     * 不是一套）。
     *
     * 只在**还没开始回响应**、且**请求体完整存着**时重试：响应开了头就没法
     * 重来，请求体截断过就补不出原样的第二次。
     */
    /** 这一条走不下去了：记一条、给浏览器一个 502。两条失败路径共用 */
    const giveUp = (e) => {
      // 上游连不上也要记一条：调试时"这个请求发出去了但失败了"和"根本没发"
      // 是完全不同的两件事，不能只在控制台里一闪而过
      this.stats.failed += 1;
      this._emit(this._record({
        req, target, isHttps, postData: recordedBody(),
        status: null, statusText: '',
        responseHeaders: {},
        bodyKind: 'none',
        bodyNote: [reqNote(), retryNote].filter(Boolean).join('；') || null,
        errorText: describeError(e),
        startedAt,
      }));
      if (!res.headersSent) res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end(`录制代理连不上上游：${describeError(e)}`);
    };

    const fire = (extra) => {
      // 有上游代理时：http 直接把绝对地址发给它；https 先要一条 CONNECT 隧道，
      // 再在隧道里自己做 TLS（**录制照常**——我们解密的仍是自己这一段）
      const up0 = bypassProxy(target.hostname) ? null : this.upstreamProxy;
      const viaProxy = up0 ? (isHttps ? {
        agent: false,
        createConnection: (_o, cb) => {
          connectThroughProxy(up0, target.hostname, target.port, (err, sock) => {
            if (err) return cb(err);
            const secure = tls.connect({
              socket: sock, servername: target.hostname, rejectUnauthorized: false,
            }, () => cb(null, secure));
            secure.on('error', (e) => cb(e));
          });
        },
      } : { agent: false, host: up0.host, port: up0.port, path: target.url }) : {};

      let upstream;
      try {
        upstream = mod.request({ ...options, ...viaProxy, ...extra }, (up) => {
          const ct = up.headers['content-type'] || '';
          const chunks = [];
          let bytes = 0;
          let overflow = false;

          // 压缩内容原样转发给浏览器，另存一份解压后的给我们自己看
          res.writeHead(up.statusCode, up.statusMessage, up.headers);
          up.on('data', (c) => {
            bytes += c.length;
            if (bytes <= MAX_BODY_BYTES) chunks.push(c); else overflow = true;
            res.write(c);
          });
          up.on('end', () => {
            res.end();
            this._finish({
              req, target, isHttps, body: recordedBody(), startedAt,
              status: up.statusCode, statusText: up.statusMessage || '',
              responseHeaders: up.headers, contentType: ct,
              raw: Buffer.concat(chunks), bytes, overflow,
              reqNote: [reqNote(), retryNote].filter(Boolean).join('；') || null,
              remoteIp: up.socket?.remoteAddress || '',
            });
          });
          up.on('error', () => { res.end(); });
        });

      } catch (e) {
        // **请求连建都建不出来**（参数这一版内核不认之类）。它是同步抛的，没人接
        // 就是主进程崩、所有页面一起打不开——**重试这条路无论如何不能比原来更糟**，
        // 它存在的全部理由是把一次失败救回来。带着额外参数崩的，退回原样再来一次。
        if (Object.keys(extra).length) { retryNote = null; return fire({}); }
        return giveUp(e);
      }

      upstream.on('error', (e) => {
        // 还没开始回响应、请求体也完整存着 —— 换一套更宽松的连接参数再试一次
        if (!retried && !res.headersSent && reqBytes <= reqStored) {
          retried = true;
          retryNote = `头一次连上游失败（${describeError(e)}），换一条不走连接池的连接重试了一次`;
          req.unpipe(upstream);
          // **unpipe 会把这条流按暂停处理**，不 resume 的话 'end' 永远不来，
          // 重试就卡在那儿等一个不会到的事件（浏览器那边只看到一直转圈）
          req.resume();
          // **重试只加 agent:false**，上面那份 viaProxy 照旧生效（顺序上它在前面，
        // 而 agent 本来就是 false）——别把走不走上游代理这件事在重试时改掉
        const again = () => fire({ agent: false });
          if (reqEnded) again(); else req.once('end', again);
          return;
        }
        giveUp(e);
      });

      // 头一次用 pipe（背压归它管，也不会因为录制缓存到了 2 MiB 就丢内容）；
      // 重试那一次流已经被消费掉了，只能把存下来的请求体原样写出去
      if (retried) upstream.end(recordedBody());
      else req.pipe(upstream);

      // **只挂一次**：重试时再挂一个的话，先前那个还指着已经销毁的 upstream
      current = upstream;
    };

    fire({});
    req.on('error', () => { if (current) current.destroy(); /* 浏览器取消请求是常态 */ });
  }

  _finish(o) {
    const type = resourceType(o.req.headers, o.contentType);
    let bodyKind = 'none';
    let bodyText = null;
    let bodyNote = null;
    let bodyBase64 = 0;

    if (o.overflow) {
      bodyKind = 'skipped';
      bodyNote = `超过 ${Math.round(MAX_BODY_BYTES / 1024)}KB 上限（${o.bytes} 字节）`;
    } else if (isBinaryMime(o.contentType)) {
      bodyKind = 'skipped';
      bodyNote = `二进制类型不存正文（${o.contentType}）`;
    } else if (o.bytes === 0) {
      bodyKind = 'none';
      bodyNote = o.status === 204 || o.status === 304 ? `${o.status} 无正文` : '响应体为空';
    } else {
      const { buf, note } = decodeBody(o.raw, o.responseHeaders['content-encoding']);
      if (buf) {
        bodyKind = 'inline';   // 存库时再按大小决定进库还是落文件
        const enc = encodeBody(buf);
        bodyText = enc.text;
        bodyBase64 = enc.isBase64;
        if (enc.isBase64) {
          bodyNote = bodyNote ? `${bodyNote}；正文不是 UTF-8，按 base64 存`
            : '正文不是 UTF-8，按 base64 存';
        }
        this.stats.withBody += 1;
      } else {
        bodyKind = 'error';
        bodyNote = note;
      }
    }

    // 请求体的截断说明与响应体的并列，两条都要留住
    if (o.reqNote) bodyNote = bodyNote ? `${bodyNote}；${o.reqNote}` : o.reqNote;

    this._emit(this._record({
      ...o,
      postData: o.body,
      resourceType: type,
      bodyKind, bodyText, bodyNote, bodyBase64,
      encodedDataLength: o.bytes,
    }));
  }

  /** 组装一条通用记录；调用方可直接持久化或在边界处转换。 */
  _record(o) {
    const finishedAt = Date.now();
    let host = '';
    let path = '';
    let query = '';
    try {
      const u = new URL(o.target.url);
      host = u.host;
      path = u.pathname;
      query = u.search.replace(/^\?/, '');
    } catch (e) { /* 解析不了就留空 */ }

    const postBuf = o.postData;
    const encodedPost = encodeBody(postBuf);
    return {
      request_id: `p${++this.seq}`,
      frame_id: null,
      document_url: o.req.headers.referer || null,
      resource_type: o.resourceType || resourceType(o.req.headers, ''),
      is_redirect: 0,
      method: o.req.method,
      url: o.target.url,
      url_host: host,
      url_path: path,
      url_query: query,
      request_headers: cleanHeaders(o.req.headers),
      // 请求体同理：GBK 的表单、二进制上传都得原样留住。标志位存进
      // post_data_base64，取用方（导 jmx / 详情页）据此判断要不要先解码
      post_data: encodedPost.text,
      post_data_base64: encodedPost.isBase64,
      ts_request: o.startedAt / 1000,
      // 事务按时间段归属，所以这个时间戳是**唯一**的归属依据，必须是真实墙上时间
      wall_time: o.startedAt / 1000,
      status: o.status,
      status_text: o.statusText || '',
      mime_type: (o.contentType || '').split(';')[0] || '',
      response_headers: cleanHeaders(o.responseHeaders),
      remote_ip: o.remoteIp || null,
      remote_port: o.target.port,
      protocol: o.isHttps ? 'https' : 'http',
      from_cache: 0,
      from_service_worker: 0,
      ts_response: finishedAt / 1000,
      ts_finished: finishedAt / 1000,
      duration_ms: finishedAt - o.startedAt,
      encoded_data_length: o.encodedDataLength || 0,
      error_text: o.errorText || null,
      canceled: 0,
      body_kind: o.bodyKind || 'none',
      body_text: o.bodyText || null,
      body_path: null,
      body_sha256: null,
      body_size: null,
      body_base64: o.bodyBase64 || 0,
      body_note: o.bodyNote || null,
    };
  }

  _emit(record) {
    this.stats.total += 1;
    try {
      this.onRecord(record);
    } catch (e) {
      this.onError(`记录请求失败：${e.message}`);
    }
  }
}

module.exports = { parseUpstreamProxy, bypassProxy, RecordingProxy, MAX_BODY_BYTES };
