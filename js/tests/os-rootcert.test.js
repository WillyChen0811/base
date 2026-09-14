'use strict';

// os/rootcert.js 的测试。**不允许真的调用 security / certutil**——所以只测
// 不触碰系统信任库的路径：能 require、CA 目录参数生效、ensureCA() 能在临时
// 目录里把证书文件落地、没装过时 status() 不触发任何系统命令就能如实报
// exists/installed=false（因为 CA_CERT 还没生成，existsSync 提前短路，压根
// 走不到 macInstalled/winInstalled 里的 exec）、manualHint() 按平台给文案。

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { createRootcert } = require('../os/rootcert');
const NAMES = { commonName: 'Test Root CA', organizationName: 'Test Org' };

function tmpCaDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'jsbase-rootcert-test-'));
}

test('createRootcert(caDir, names) 的 CA_CERT 落在传入目录下（不写死项目路径）', () => {
  const dir = tmpCaDir();
  const rc = createRootcert(dir, NAMES);
  assert.equal(rc.CA_CERT, path.join(dir, 'ca-cert.pem'));
});

test('ensureCA() 在临时目录里把证书文件真正生成出来', () => {
  const dir = tmpCaDir();
  const rc = createRootcert(dir, NAMES);
  assert.ok(!fs.existsSync(rc.CA_CERT), '还没调用前不该有文件');
  rc.ensureCA();
  assert.ok(fs.existsSync(rc.CA_CERT));
});

test('CA 还没生成时 status() 如实报 exists/installed 为 false，不触发系统命令', async () => {
  const dir = tmpCaDir();
  const rc = createRootcert(dir, NAMES);
  const st = await rc.status();
  assert.equal(st.exists, false);
  assert.equal(st.installed, false);
  assert.equal(st.path, rc.CA_CERT);
});

test('status() 里的 hint 文案非空（manualHint 走的是同一份内部实现，按平台给文案）', async () => {
  const rc = createRootcert(tmpCaDir(), NAMES);
  const st = await rc.status();
  assert.ok(st.hint && st.hint.length > 0);
  if (process.platform === 'win32') {
    assert.match(st.hint, /受信任的根证书颁发机构/);
  } else if (process.platform === 'darwin') {
    assert.match(st.hint, /钥匙串/);
  }
});

test('两个实例各自的 CA 目录独立，互不干扰', () => {
  const a = createRootcert(tmpCaDir(), NAMES);
  const b = createRootcert(tmpCaDir(), NAMES);
  assert.notEqual(a.CA_CERT, b.CA_CERT);
});
