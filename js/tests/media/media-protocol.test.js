'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {
  registerMediaScheme, handleMediaScheme, parseRange, buildRangeResponse, guessContentType,
} = require('../../media/media-protocol');

// ---------------------------------------------------------------- parseRange

test('parseRange：没有 Range 头，返回整段', () => {
  assert.deepEqual(parseRange(null, 1000), { type: 'full' });
  assert.deepEqual(parseRange(undefined, 1000), { type: 'full' });
});

test('parseRange：单区间', () => {
  assert.deepEqual(parseRange('bytes=100-199', 1000), { type: 'range', start: 100, end: 199 });
});

test('parseRange：开放区间（读到文件尾）', () => {
  assert.deepEqual(parseRange('bytes=900-', 1000), { type: 'range', start: 900, end: 999 });
});

test('parseRange：后缀区间（最后 N 字节）', () => {
  assert.deepEqual(parseRange('bytes=-500', 1000), { type: 'range', start: 500, end: 999 });
});

test('parseRange：后缀比文件还大，从头给起', () => {
  assert.deepEqual(parseRange('bytes=-5000', 1000), { type: 'range', start: 0, end: 999 });
});

test('parseRange：越界/不合法都算 unsatisfiable', () => {
  assert.deepEqual(parseRange('bytes=2000-3000', 1000), { type: 'unsatisfiable' });
  assert.deepEqual(parseRange('bytes=500-100', 1000), { type: 'unsatisfiable' });
  assert.deepEqual(parseRange('not-a-range', 1000), { type: 'unsatisfiable' });
  assert.deepEqual(parseRange('bytes=-', 1000), { type: 'unsatisfiable' });
});

// ---------------------------------------------------------------- buildRangeResponse

test('buildRangeResponse：无 Range 头返回 200 + 全长', () => {
  const res = buildRangeResponse({ type: 'full' }, 1000, 'video/mp4');
  assert.equal(res.status, 200);
  assert.equal(res.headers['Content-Length'], '1000');
  assert.equal(res.headers['Content-Type'], 'video/mp4');
  assert.equal(res.headers['Accept-Ranges'], 'bytes');
  assert.equal(res.start, 0);
  assert.equal(res.end, 999);
});

test('buildRangeResponse：单区间返回 206 + Content-Range', () => {
  const res = buildRangeResponse({ type: 'range', start: 100, end: 199 }, 1000);
  assert.equal(res.status, 206);
  assert.equal(res.headers['Content-Range'], 'bytes 100-199/1000');
  assert.equal(res.headers['Content-Length'], '100');
});

test('buildRangeResponse：越界返回 416 + Content-Range: bytes */size', () => {
  const res = buildRangeResponse({ type: 'unsatisfiable' }, 1000);
  assert.equal(res.status, 416);
  assert.equal(res.headers['Content-Range'], 'bytes */1000');
});

test('guessContentType：常见扩展名，猜不出退到 octet-stream', () => {
  assert.equal(guessContentType('/a/b.mp4'), 'video/mp4');
  assert.equal(guessContentType('/a/b.MKV'), 'video/x-matroska');
  assert.equal(guessContentType('/a/b.weird'), 'application/octet-stream');
});

// ---------------------------------------------------------------- registerMediaScheme

test('registerMediaScheme：必须在 app ready 前调用（传 registerSchemesAsPrivileged），权限带 stream', () => {
  const calls = [];
  const fakeProtocol = { registerSchemesAsPrivileged: (configs) => calls.push(configs) };
  registerMediaScheme(fakeProtocol, 'testmedia');
  assert.equal(calls.length, 1);
  const [config] = calls[0];
  assert.equal(config.scheme, 'testmedia');
  assert.equal(config.privileges.stream, true);
});

test('registerMediaScheme：protocol 或 scheme 缺失要报错', () => {
  assert.throws(() => registerMediaScheme(null, 'x'), TypeError);
  assert.throws(() => registerMediaScheme({ registerSchemesAsPrivileged() {} }, ''), TypeError);
});

// ---------------------------------------------------------------- handleMediaScheme

function fakeProtocolWithHandler() {
  let handler = null;
  return {
    handle(scheme, fn) { this.scheme = scheme; handler = fn; },
    async dispatch(request) { return handler(request); },
  };
}

test('handleMediaScheme：无 Range 头，200 返回全文', async () => {
  const tmp = path.join(os.tmpdir(), `media-protocol-test-${Date.now()}.bin`);
  const content = Buffer.from('0123456789');
  fs.writeFileSync(tmp, content);
  try {
    const protocol = fakeProtocolWithHandler();
    handleMediaScheme(protocol, 'testmedia', { resolvePath: () => tmp });
    const res = await protocol.dispatch({
      url: 'testmedia://x', method: 'GET', headers: new Headers(),
    });
    assert.equal(res.status, 200);
    const buf = Buffer.from(await res.arrayBuffer());
    assert.equal(buf.toString(), '0123456789');
  } finally {
    fs.unlinkSync(tmp);
  }
});

test('handleMediaScheme：带 Range 头，206 只返回请求的字节区间', async () => {
  const tmp = path.join(os.tmpdir(), `media-protocol-test-${Date.now()}-range.bin`);
  fs.writeFileSync(tmp, Buffer.from('0123456789'));
  try {
    const protocol = fakeProtocolWithHandler();
    handleMediaScheme(protocol, 'testmedia', { resolvePath: () => tmp });
    const res = await protocol.dispatch({
      url: 'testmedia://x', method: 'GET', headers: new Headers({ Range: 'bytes=2-4' }),
    });
    assert.equal(res.status, 206);
    assert.equal(res.headers.get('Content-Range'), 'bytes 2-4/10');
    const buf = Buffer.from(await res.arrayBuffer());
    assert.equal(buf.toString(), '234');
  } finally {
    fs.unlinkSync(tmp);
  }
});

test('handleMediaScheme：越界 Range 返回 416', async () => {
  const tmp = path.join(os.tmpdir(), `media-protocol-test-${Date.now()}-416.bin`);
  fs.writeFileSync(tmp, Buffer.from('0123456789'));
  try {
    const protocol = fakeProtocolWithHandler();
    handleMediaScheme(protocol, 'testmedia', { resolvePath: () => tmp });
    const res = await protocol.dispatch({
      url: 'testmedia://x', method: 'GET', headers: new Headers({ Range: 'bytes=100-200' }),
    });
    assert.equal(res.status, 416);
  } finally {
    fs.unlinkSync(tmp);
  }
});

test('handleMediaScheme：resolvePath 返回空当 404，抛异常当 403（访问控制由调用方决定）', async () => {
  const protocol = fakeProtocolWithHandler();
  handleMediaScheme(protocol, 'testmedia', {
    resolvePath: (url) => { if (url.includes('deny')) throw new Error('no'); return null; },
  });
  const notFound = await protocol.dispatch({ url: 'testmedia://missing', method: 'GET', headers: new Headers() });
  assert.equal(notFound.status, 404);
  const denied = await protocol.dispatch({ url: 'testmedia://deny', method: 'GET', headers: new Headers() });
  assert.equal(denied.status, 403);
});

test('handleMediaScheme：缺 resolvePath 要报错', () => {
  const protocol = fakeProtocolWithHandler();
  assert.throws(() => handleMediaScheme(protocol, 'testmedia', {}), TypeError);
});

test('handleMediaScheme：空文件返回 200 空体，不抛错', async () => {
  const tmp = path.join(os.tmpdir(), `media-protocol-test-${Date.now()}-empty.bin`);
  fs.writeFileSync(tmp, Buffer.alloc(0));
  try {
    const protocol = fakeProtocolWithHandler();
    handleMediaScheme(protocol, 'testmedia', { resolvePath: () => tmp });
    const res = await protocol.dispatch({
      url: 'testmedia://x', method: 'GET', headers: new Headers(),
    });
    assert.equal(res.status, 200);
    assert.equal((await res.arrayBuffer()).byteLength, 0);
  } finally {
    fs.unlinkSync(tmp);
  }
});
