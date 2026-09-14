/**
 * 录制代理的证书颁发机构。
 *
 * 要解密 HTTPS 就得中间人，中间人就得给每个域名现签一张证书。这里自建一个 CA，
 * 第一次用的时候生成，之后一直复用（`~/.filecore/traffic/ca/`）。
 *
 * **这张 CA 证书不装进系统钥匙串。** 只有录制窗口那个 session 被告知"信任它"
 * （`session.setCertificateVerifyProc`，见 proxy.js 的调用方）。所以：
 *
 *   - 不需要用户点任何"安装证书/信任根证书"的对话框；
 *   - 你机器上的 Chrome、Safari、其它软件完全不受影响，它们不认识这张证书；
 *   - 卸载 macClient 或删掉 `~/.filecore/traffic/ca/` 就干净了。
 *
 * 这是比 Burp 省事的地方——Burp 要装根证书，是因为它服务的是外部浏览器；
 * 我们的浏览器就是自己的窗口，能精确地只对它开这个口子。
 *
 * 从 websubmit macClient（traffic/ca.js）搬入 base：唯一改动是 CA 目录不再写死
 * `~/.filecore/traffic/ca`，改成工厂函数按调用方传入的目录建实例。
 */

const fs = require('fs');
const path = require('path');
const forge = require('node-forge');

/**
 * node-forge 产出的 PEM 用 `\r\n` 换行，而 Electron 里的 BoringSSL 只认 `\n`——
 * 直接喂过去报 `error:09000064:PEM routines:OPENSSL_internal:BAD_BASE64_DECODE`，
 * 而且是在 TLS 握手那一层抛出来的，光看报错完全想不到是换行符的事。
 * 所有出去的 PEM 都从这里过一道。
 */
function normalizePem(pem) {
  return String(pem).replace(/\r\n/g, '\n');
}

/**
 * 证书主体里的名字**必须全是 ASCII**。
 *
 * node-forge 默认按 PrintableString 编码属性值，写中文进去会把 DER 编坏，
 * 之后无论 Node 还是 BoringSSL 载入都报
 * `error:04800064:PEM routines::bad base64 decode`——报错指向 base64，
 * 真凶却是证书内容，查起来非常绕。这里一律用英文。
 */
function _attrs(commonName, organizationName) {
  return [
    { name: 'commonName', value: commonName },
    { name: 'organizationName', value: organizationName },
    { shortName: 'OU', value: 'Local recording only' },
  ];
}

/**
 * 建一个 CA 实例。`caDir` 由调用方传入（原实现写死
 * `~/.filecore/traffic/ca`，base 不带项目默认值）。
 *
 * @param {string} caDir CA 私钥/证书的落地目录
 */
function createCA(caDir, names) {
  // CA 的名字是项目身份（它会出现在用户的钥匙串里，卸载也按它找），base 不带
  // 项目默认值，所以两个名字都由调用方给，缺了直接报，不悄悄用一个通用名。
  const { commonName, organizationName } = names || {};
  if (!commonName || !organizationName) {
    throw new Error('createCA 需要 names.commonName 与 names.organizationName');
  }
  const CA_DIR = caDir;
  const CA_KEY = path.join(CA_DIR, 'ca-key.pem');
  const CA_CERT = path.join(CA_DIR, 'ca-cert.pem');

  // 叶子证书签一次要几十毫秒，一个页面几十个域名就得缓存
  const leafCache = new Map();

  /** 读出或新建 CA。返回 forge 的 {key, cert} 以及 PEM。 */
  function loadOrCreateCA() {
    if (fs.existsSync(CA_KEY) && fs.existsSync(CA_CERT)) {
      const keyPem = fs.readFileSync(CA_KEY, 'utf8');
      const certPem = fs.readFileSync(CA_CERT, 'utf8');
      try {
        return {
          key: forge.pki.privateKeyFromPem(keyPem),
          cert: forge.pki.certificateFromPem(certPem),
          keyPem: normalizePem(keyPem),
          certPem: normalizePem(certPem),
        };
      } catch (e) {
        // 文件坏了就重新生成，没必要让用户去手工删
      }
    }

    const keys = forge.pki.rsa.generateKeyPair(2048);
    const cert = forge.pki.createCertificate();
    cert.publicKey = keys.publicKey;
    cert.serialNumber = '01' + forge.util.bytesToHex(forge.random.getBytesSync(8));
    cert.validity.notBefore = new Date(Date.now() - 24 * 3600 * 1000);
    cert.validity.notAfter = new Date(Date.now() + 10 * 365 * 24 * 3600 * 1000);
    cert.setSubject(_attrs(commonName, organizationName));
    cert.setIssuer(_attrs(commonName, organizationName));
    cert.setExtensions([
      { name: 'basicConstraints', cA: true, critical: true },
      { name: 'keyUsage', keyCertSign: true, cRLSign: true, critical: true },
      { name: 'subjectKeyIdentifier' },
    ]);
    cert.sign(keys.privateKey, forge.md.sha256.create());

    const keyPem = normalizePem(forge.pki.privateKeyToPem(keys.privateKey));
    const certPem = normalizePem(forge.pki.certificateToPem(cert));
    fs.mkdirSync(CA_DIR, { recursive: true, mode: 0o700 });
    fs.writeFileSync(CA_KEY, keyPem, { mode: 0o600 });
    fs.writeFileSync(CA_CERT, certPem, { mode: 0o600 });

    return { key: keys.privateKey, cert, keyPem, certPem };
  }

  const ca = { value: null };
  function getCA() {
    if (!ca.value) ca.value = loadOrCreateCA();
    return ca.value;
  }

  /**
   * 给一个域名签一张证书，返回 { key, cert } 两段 PEM，可直接喂给 tls.createServer。
   *
   * IP 直连（内网被测系统很常见：https://192.168.1.10）要写进 SAN 的 IP 项而不是
   * DNS 项，写错了 Chromium 一律报 ERR_CERT_COMMON_NAME_INVALID。
   */
  function certFor(hostname) {
    const cached = leafCache.get(hostname);
    if (cached) return cached;

    const root = getCA();
    const keys = forge.pki.rsa.generateKeyPair(2048);
    const cert = forge.pki.createCertificate();
    cert.publicKey = keys.publicKey;
    cert.serialNumber = '01' + forge.util.bytesToHex(forge.random.getBytesSync(8));
    cert.validity.notBefore = new Date(Date.now() - 24 * 3600 * 1000);
    cert.validity.notAfter = new Date(Date.now() + 2 * 365 * 24 * 3600 * 1000);
    cert.setSubject(_attrs(hostname, organizationName));
    cert.setIssuer(root.cert.subject.attributes);

    const isIP = /^\d{1,3}(\.\d{1,3}){3}$/.test(hostname);
    cert.setExtensions([
      { name: 'basicConstraints', cA: false },
      { name: 'keyUsage', digitalSignature: true, keyEncipherment: true },
      { name: 'extKeyUsage', serverAuth: true },
      {
        name: 'subjectAltName',
        altNames: [isIP ? { type: 7, ip: hostname } : { type: 2, value: hostname }],
      },
    ]);
    cert.sign(root.key, forge.md.sha256.create());

    const pair = {
      key: normalizePem(forge.pki.privateKeyToPem(keys.privateKey)),
      cert: normalizePem(forge.pki.certificateToPem(cert)),
    };
    // 一次录制里域名不会太多，但也别无限涨
    if (leafCache.size > 200) leafCache.clear();
    leafCache.set(hostname, pair);
    return pair;
  }

  return { getCA, certFor, CA_DIR };
}

module.exports = { createCA, normalizePem };
