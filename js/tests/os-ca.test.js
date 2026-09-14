'use strict';

// os/ca.js 的测试：只验证"能 require、能在临时目录里生成并复用 CA、产出的
// PEM 里没有 \r"。不碰任何系统层面的东西（这个模块本身也不碰，纯本地文件 +
// node-forge），所以不需要 mock。

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { createCA, normalizePem } = require('../os/ca');
const NAMES = { commonName: 'Test Root CA', organizationName: 'Test Org' };

function tmpDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'jsbase-ca-test-'));
}

test('normalizePem 把 \\r\\n 都转成 \\n', () => {
  assert.equal(normalizePem('a\r\nb\r\nc'), 'a\nb\nc');
});

test('createCA(dir, names) 落在传入的临时目录里，产出的根证书 PEM 不含 \\r', () => {
  const dir = tmpDir();
  const ca = createCA(dir, NAMES);
  assert.equal(ca.CA_DIR, dir);

  const { certPem, keyPem } = ca.getCA();
  assert.ok(!/\r/.test(certPem), '根证书 PEM 不应该含 \\r');
  assert.ok(!/\r/.test(keyPem), '私钥 PEM 不应该含 \\r');
  assert.ok(fs.existsSync(path.join(dir, 'ca-cert.pem')));
  assert.ok(fs.existsSync(path.join(dir, 'ca-key.pem')));
});

test('同一个目录第二次 createCA 复用已有的 CA（不重新生成）', () => {
  const dir = tmpDir();
  const first = createCA(dir, NAMES).getCA();
  const second = createCA(dir, NAMES).getCA();
  assert.equal(first.certPem, second.certPem, '复用同一份 CA，证书内容应该一致');
});

test('certFor(hostname) 签发的叶子证书 PEM 不含 \\r，且按域名缓存', () => {
  const dir = tmpDir();
  const ca = createCA(dir, NAMES);
  const a = ca.certFor('example.com');
  const b = ca.certFor('example.com');
  assert.ok(!/\r/.test(a.cert));
  assert.ok(!/\r/.test(a.key));
  assert.equal(a.cert, b.cert, '同一域名应该命中缓存，拿到同一张证书');
});

test('IP 直连域名也能签出证书（SAN 走 IP 项）', () => {
  const dir = tmpDir();
  const ca = createCA(dir, NAMES);
  const pair = ca.certFor('192.168.1.10');
  assert.ok(!/\r/.test(pair.cert));
});
