'use strict';

/**
 * Electron 里播本地视频用的自定义协议，处理好 HTTP Range。
 *
 * 不处理 Range 会怎样：`<video>` 请求整个文件一次性下完才能播，
 * `seekable.end()` 算出来是 0，用户一拖进度条就跳回开头
 * （electron/electron#38749 报的正是这个）。206 + Content-Range 是
 * `<video>` 判断"能不能拖"的唯一依据。
 *
 * 两个函数分两步注册，都要求调用方把 Electron 的 `protocol` 对象传进来——
 * 这个模块本身顶层不 `require('electron')`，普通 Node 进程也能加载、测试
 * Range 解析这部分逻辑。
 *
 * 1. `registerMediaScheme(protocol, scheme)`：**必须在 app ready 之前**调用，
 *    对应 Electron 的 `protocol.registerSchemesAsPrivileged`；
 * 2. `handleMediaScheme(protocol, scheme, { resolvePath })`：app ready 之后
 *    调用，真正接管这个 scheme 的请求。
 *
 * `resolvePath(url)` 由调用方给：URL 怎么映射到本地文件、这个文件能不能被
 * 这次请求读到，base 不替业务做这个决定——不同项目的访问控制策略天差地别。
 */

const fs = require('node:fs');
const { Readable } = require('node:stream');

/**
 * 在 app ready 之前注册协议权限。
 *
 * `stream: true` 是关键：没有它 Electron 不会把响应当成流处理，Range 请求
 * 也就没有意义。其余几项是本地媒体播放的常规需要：`standard` 让相对路径
 * 能正常解析，`secure` 避免被当成混合内容拦掉，`supportFetchAPI` 让页面能用
 * fetch 直接探测（比如探测长度），`corsEnabled` 方便跨 origin 的播放器脚本
 * 引用媒体资源。不给 `bypassCSP`：播媒体用不着，给了等于让这个 scheme 绕过页面 CSP。
 */
function registerMediaScheme(protocol, scheme) {
  if (!protocol || typeof protocol.registerSchemesAsPrivileged !== 'function') {
    throw new TypeError('registerMediaScheme 需要 Electron 的 protocol 对象，且必须在 app ready 之前调用');
  }
  if (!scheme) throw new TypeError('registerMediaScheme 需要 scheme 名字');
  protocol.registerSchemesAsPrivileged([
    {
      scheme,
      privileges: {
        standard: true,
        secure: true,
        stream: true,
        corsEnabled: true,
        supportFetchAPI: true,
      },
    },
  ]);
}

// 只覆盖网页播放器常见的媒体格式；猜不出的一律 application/octet-stream，
// 不影响播放——Content-Type 只是提示，`<video>` 真正认的是能不能 seek。
const EXT_CONTENT_TYPE = {
  '.mp4': 'video/mp4',
  '.m4v': 'video/mp4',
  '.webm': 'video/webm',
  '.mkv': 'video/x-matroska',
  '.mov': 'video/quicktime',
  '.ogv': 'video/ogg',
  '.mp3': 'audio/mpeg',
  '.m4a': 'audio/mp4',
  '.wav': 'audio/wav',
  '.ogg': 'audio/ogg',
  '.flac': 'audio/flac',
  '.aac': 'audio/aac',
  '.vtt': 'text/vtt',
  '.srt': 'application/x-subrip',
};

function guessContentType(filePath) {
  const dot = filePath.lastIndexOf('.');
  if (dot === -1) return 'application/octet-stream';
  return EXT_CONTENT_TYPE[filePath.slice(dot).toLowerCase()] || 'application/octet-stream';
}

/**
 * 解析 `Range` 请求头。纯函数，不碰文件系统，单独测三种写法 + 越界。
 *
 * 支持的写法：`bytes=100-200`（单区间）、`bytes=100-`（开放区间，读到文件尾）、
 * `bytes=-500`（后缀，读最后 500 字节）。不支持多区间（`bytes=0-10,20-30`）——
 * 网页播放器的 `<video>` 只会发单区间请求，多区间是没人用的复杂度。
 *
 * @param {string|null} header 请求头原始值，没有就传 null/undefined
 * @param {number} totalSize 文件总字节数
 * @returns {{type:'full'}|{type:'range',start:number,end:number}|{type:'unsatisfiable'}}
 */
function parseRange(header, totalSize) {
  if (!header) return { type: 'full' };
  const m = /^\s*bytes=(\d*)-(\d*)\s*$/.exec(header);
  if (!m) return { type: 'unsatisfiable' };
  const [, startStr, endStr] = m;
  if (startStr === '' && endStr === '') return { type: 'unsatisfiable' };

  let start;
  let end;
  if (startStr === '') {
    // 后缀形式：文件比请求的后缀还短，就从头给起
    const suffixLength = parseInt(endStr, 10);
    if (!(suffixLength > 0)) return { type: 'unsatisfiable' };
    start = Math.max(0, totalSize - suffixLength);
    end = totalSize - 1;
  } else {
    start = parseInt(startStr, 10);
    end = endStr === '' ? totalSize - 1 : parseInt(endStr, 10);
  }

  if (!Number.isFinite(start) || !Number.isFinite(end) || start > end || start >= totalSize || totalSize === 0) {
    return { type: 'unsatisfiable' };
  }
  return { type: 'range', start, end: Math.min(end, totalSize - 1) };
}

/**
 * 把解析结果换算成响应的 status/headers/字节区间。纯函数，跟 parseRange
 * 分开测：一个测"请求头怎么读"，一个测"读出来的区间怎么变成响应"。
 *
 * @param {ReturnType<typeof parseRange>} range
 * @param {number} totalSize
 * @param {string} [contentType]
 */
function buildRangeResponse(range, totalSize, contentType) {
  const headers = { 'Accept-Ranges': 'bytes' };
  if (contentType) headers['Content-Type'] = contentType;

  if (range.type === 'unsatisfiable') {
    headers['Content-Range'] = `bytes */${totalSize}`;
    return { status: 416, headers, start: 0, end: -1 };
  }
  if (range.type === 'range') {
    headers['Content-Range'] = `bytes ${range.start}-${range.end}/${totalSize}`;
    headers['Content-Length'] = String(range.end - range.start + 1);
    return { status: 206, headers, start: range.start, end: range.end };
  }
  headers['Content-Length'] = String(totalSize);
  return { status: 200, headers, start: 0, end: totalSize - 1 };
}

/**
 * app ready 之后调用，接管 scheme 的请求。
 *
 * `resolvePath(url)` 可以是同步或返回 Promise；返回本地文件路径就读它，
 * 返回空/抛异常就分别当 404/403 处理——**能不能读这个文件由调用方判断**，
 * 这里只负责"判断结果出来之后怎么响应"。
 */
function handleMediaScheme(protocol, scheme, { resolvePath }) {
  if (!protocol || typeof protocol.handle !== 'function') {
    throw new TypeError('handleMediaScheme 需要 Electron 的 protocol 对象');
  }
  if (!scheme) throw new TypeError('handleMediaScheme 需要 scheme 名字');
  if (typeof resolvePath !== 'function') {
    throw new TypeError('handleMediaScheme 需要调用方传 resolvePath：URL 映射到本地文件、决定能不能读');
  }

  protocol.handle(scheme, async (request) => {
    let filePath;
    try {
      filePath = await resolvePath(request.url);
    } catch (e) {
      return new Response(e && e.message ? e.message : '拒绝访问', { status: 403 });
    }
    if (!filePath) return new Response('未找到', { status: 404 });

    let stat;
    try {
      stat = await fs.promises.stat(filePath);
    } catch (e) {
      return new Response('文件不存在', { status: 404 });
    }

    const range = parseRange(request.headers.get('range'), stat.size);
    const built = buildRangeResponse(range, stat.size, guessContentType(filePath));

    if (built.status === 416) {
      return new Response(null, { status: 416, headers: built.headers });
    }
    // 空文件：createReadStream({end: -1}) 会直接抛错
    if (request.method === 'HEAD' || stat.size === 0) {
      return new Response(null, { status: built.status, headers: built.headers });
    }

    const nodeStream = fs.createReadStream(filePath, { start: built.start, end: built.end });
    return new Response(Readable.toWeb(nodeStream), { status: built.status, headers: built.headers });
  });
}

module.exports = {
  registerMediaScheme,
  handleMediaScheme,
  parseRange,
  buildRangeResponse,
  guessContentType,
};
