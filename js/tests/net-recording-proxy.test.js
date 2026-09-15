'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const tls = require('node:tls');

const { RecordingProxy } = require('../net/recording-proxy');
const { createCA } = require('../os/ca');

// 这些用例只走明文 localhost；certFor 仍显式注入，以验证 base 不会偷偷创建 CA。
const unusedCertFor = () => { throw new Error('HTTP test must not request a certificate'); };

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => resolve(server.address().port));
  });
}

function close(server) {
  return new Promise((resolve) => server.close(resolve));
}

function throughProxy(proxyPort, url, options = {}) {
  return new Promise((resolve, reject) => {
    const req = http.request({
      host: '127.0.0.1',
      port: proxyPort,
      method: options.method || 'GET',
      path: url,
      headers: options.headers,
    }, (res) => {
      const chunks = [];
      res.on('data', (chunk) => chunks.push(chunk));
      res.on('end', () => resolve({
        status: res.statusCode,
        body: Buffer.concat(chunks).toString('utf8'),
      }));
    });
    req.once('error', reject);
    req.end(options.body);
  });
}

function httpsThroughProxy(proxyPort, targetPort) {
  return new Promise((resolve, reject) => {
    const tunnel = net.connect(proxyPort, '127.0.0.1');
    let handshake = Buffer.alloc(0);
    const fail = (error) => reject(error);
    tunnel.once('error', fail);
    tunnel.once('connect', () => {
      tunnel.write(`CONNECT localhost:${targetPort} HTTP/1.1\r\nHost: localhost:${targetPort}\r\n\r\n`);
    });
    const onHandshake = (chunk) => {
      handshake = Buffer.concat([handshake, chunk]);
      const end = handshake.indexOf('\r\n\r\n');
      if (end < 0) return;
      tunnel.removeListener('data', onHandshake);
      assert.match(handshake.subarray(0, end).toString('latin1'), /^HTTP\/1\.1 200/);
      const rest = handshake.subarray(end + 4);
      if (rest.length) tunnel.unshift(rest);

      const secure = tls.connect({ socket: tunnel, servername: 'localhost', rejectUnauthorized: false });
      const chunks = [];
      secure.once('error', fail);
      secure.once('secureConnect', () => {
        secure.write(`GET /secure HTTP/1.1\r\nHost: localhost:${targetPort}\r\nConnection: close\r\n\r\n`);
      });
      secure.on('data', (chunk) => {
        chunks.push(chunk);
        const response = Buffer.concat(chunks).toString('utf8');
        if (response.includes('secure-response')) {
          secure.destroy();
          resolve(response);
        }
      });
      secure.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    };
    tunnel.on('data', onHandshake);
  });
}

function eventually(check, timeoutMs = 1000) {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    const poll = () => {
      try {
        if (check()) return resolve();
      } catch (error) {
        return reject(error);
      }
      if (Date.now() >= deadline) return reject(new Error('condition did not converge'));
      setTimeout(poll, 10);
    };
    poll();
  });
}

test('本地 HTTP 经代理实际转发，并捕获请求与响应', async (t) => {
  const upstream = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(chunk));
    req.on('end', () => {
      res.writeHead(201, { 'content-type': 'application/json; charset=utf-8' });
      res.end(JSON.stringify({ path: req.url, body: Buffer.concat(chunks).toString('utf8') }));
    });
  });
  const upstreamPort = await listen(upstream);
  t.after(() => close(upstream));

  const records = [];
  const proxy = new RecordingProxy((record) => records.push(record), assert.fail, {
    certFor: unusedCertFor,
  });
  const proxyPort = await proxy.start();
  t.after(() => proxy.stop());

  const url = `http://127.0.0.1:${upstreamPort}/submit?q=1`;
  const response = await throughProxy(proxyPort, url, {
    method: 'POST',
    headers: { 'content-type': 'text/plain', 'sec-fetch-dest': 'empty' },
    body: 'hello',
  });

  assert.equal(response.status, 201);
  assert.deepEqual(JSON.parse(response.body), { path: '/submit?q=1', body: 'hello' });
  assert.equal(records.length, 1);
  assert.equal(records[0].url, url);
  assert.equal(records[0].method, 'POST');
  assert.equal(records[0].post_data, 'hello');
  assert.equal(records[0].status, 201);
  assert.equal(records[0].resource_type, 'Fetch');
  assert.equal(records[0].body_kind, 'inline');
  assert.equal(records[0].body_text, response.body);
  assert.equal(proxy.inflight(), 0);
});

test('HTTPS CONNECT 通过注入的 certFor 完成 SNI 解密、转发与捕获', async (t) => {
  const caDir = fs.mkdtempSync(path.join(os.tmpdir(), 'jsbase-proxy-ca-test-'));
  const ca = createCA(caDir, { commonName: 'Proxy Test CA', organizationName: 'Test' });
  const upstreamPair = ca.certFor('localhost');
  const upstream = https.createServer(upstreamPair, (_req, res) => {
    res.writeHead(200, { 'content-type': 'text/plain; charset=utf-8' });
    res.end('secure-response');
  });
  const upstreamPort = await listen(upstream);
  t.after(() => close(upstream));

  const signedHosts = [];
  const records = [];
  const proxy = new RecordingProxy((record) => records.push(record), assert.fail, {
    certFor(hostname) {
      signedHosts.push(hostname);
      return ca.certFor(hostname);
    },
  });
  const proxyPort = await proxy.start();
  t.after(() => proxy.stop());

  const response = await httpsThroughProxy(proxyPort, upstreamPort);
  assert.match(response, /^HTTP\/1\.1 200/);
  assert.match(response, /secure-response/);
  await eventually(() => records.length === 1);
  assert.deepEqual(signedHosts, ['localhost']);
  assert.equal(records.length, 1);
  assert.equal(records[0].url, `https://localhost:${upstreamPort}/secure`);
  assert.equal(records[0].protocol, 'https');
  assert.equal(records[0].body_text, 'secure-response');
  assert.equal(proxy.inflight(), 0);
});

test('上游失败仍产出记录、返回 502，inflight 最终排空', async (t) => {
  const reservation = net.createServer();
  const deadPort = await listen(reservation);
  await close(reservation);

  const records = [];
  const proxy = new RecordingProxy((record) => records.push(record), assert.fail, {
    certFor: unusedCertFor,
  });
  const proxyPort = await proxy.start();
  t.after(() => proxy.stop());

  const response = await throughProxy(proxyPort, `http://127.0.0.1:${deadPort}/missing`);
  assert.equal(response.status, 502);
  assert.match(response.body, /ECONNREFUSED/);
  await eventually(() => proxy.inflight() === 0);
  assert.equal(records.length, 1);
  assert.equal(records[0].status, null);
  assert.match(records[0].error_text, /ECONNREFUSED/);
  assert.equal(proxy.stats.failed, 1);
});

test('stop 会收掉仍连接的客户端并在有限时间内完成', async () => {
  const proxy = new RecordingProxy(() => {}, assert.fail, { certFor: unusedCertFor });
  const proxyPort = await proxy.start();
  const socket = net.connect(proxyPort, '127.0.0.1');
  await new Promise((resolve, reject) => {
    socket.once('connect', resolve);
    socket.once('error', reject);
  });

  await Promise.race([
    proxy.stop(),
    new Promise((_, reject) => setTimeout(() => reject(new Error('stop did not converge')), 500)),
  ]);
  assert.equal(proxy.server, null);
  assert.equal(proxy.tlsServer, null);
  assert.equal(proxy.sockets.size, 0);
  await eventually(() => socket.destroyed);
});

test('certFor 必须由调用方注入', () => {
  assert.throws(
    () => new RecordingProxy(() => {}),
    /requires options\.certFor/,
  );
});
