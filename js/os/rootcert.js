/**
 * 根证书的装、卸、查（全局抓 HTTPS 用）。
 *
 * ## 为什么全局抓包要装，录制窗口不用
 *
 * 录制窗口只需要**它自己那个 session** 信任我们的 CA——`setCertificateVerifyProc`
 * 一句话的事，用户机器上别的软件完全不受影响。这是自带浏览器相对 Burp 最大的
 * 便宜。但全局抓包的对象是**别的程序**（C/S 客户端、手机 APP、别的浏览器），
 * 它们只认系统信任库，所以只能真装进去。
 *
 * **装完这张 CA 就能签任何网站**，所以：
 *
 *   · 必须是用户明确点一下的动作，不在开始抓包时偷偷装；
 *   · 「卸载」按钮跟「安装」摆在一起，不是藏在某个角落——用完就该卸掉，
 *     只在文档里写一句"记得删"等于没写。
 *
 * ## 两个平台差得很远，文案也不能共用
 *
 * macOS 叫「钥匙串」、Windows 叫「受信任的根证书颁发机构」。原先界面上只写了
 * macOS 那套说法，Windows 用户照着去找根本找不到——自动装不上时的手动指引
 * 尤其要按平台说，那正是他最需要照着做的时候。
 *
 * 装到**当前用户**而不是整机：Windows 的 `-user Root` 不需要管理员权限，
 * macOS 用登录钥匙串同理。抓包是一个人在自己账号下干的活，没必要动整台机器。
 *
 * 从 websubmit macClient（traffic/certmgr.js）搬入 base：唯一改动是 CA 目录不再
 * 写死 `~/.filecore/traffic/ca`，改成工厂函数按调用方传入的 `./ca.js` CA 实例。
 */
const { exec } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { createCA } = require('./ca');

function run(cmd, opts = {}) {
  return new Promise((resolve) => {
    exec(cmd, { timeout: 60000, windowsHide: true, ...opts }, (err, stdout, stderr) => {
      resolve({ ok: !err, out: (stdout || '').trim(), err: (stderr || err?.message || '').trim() });
    });
  });
}

/**
 * 建一个根证书管理器实例。`caDir` 由调用方传入，转手给 `./ca.js` 的
 * `createCA` 拿到同一份 CA（原实现写死 `~/.filecore/traffic/ca`，base 不带
 * 项目默认值）。
 *
 * @param {string} caDir CA 私钥/证书的落地目录，与 `os/ca.js` 用的是同一个目录
 * @param {{commonName: string, organizationName: string}} names CA 主体名，见 `createCA`
 */
function createRootcert(caDir, names) {
  const { getCA, CA_DIR } = createCA(caDir, names);
  // 钥匙串里按这个名字找、按这个名字删，必须与签出来的 CA 主体一致
  const CA_CN = names.commonName;
  const CA_CERT = path.join(CA_DIR, 'ca-cert.pem');

  /**
   * 确保 CA 文件真的在磁盘上。
   *
   * **必须能主动调**：`certFor` 只在 TLS 握手的 SNICallback 里被调到，也就是
   * **第一个 HTTPS 请求到来时才生成 CA**。而用户的正确操作顺序恰恰是"开始抓包
   * → 先把证书装上 → 再去点被测系统"，那时目录还是空的，他会以为程序坏了。
   */
  function ensureCA() {
    getCA();                    // 没有就在这一步建出来
    return CA_CERT;
  }

  // ------------------------------------------------------------------ macOS

  function macKeychain() {
    return path.join(os.homedir(), 'Library', 'Keychains', 'login.keychain-db');
  }

  async function macInstall() {
    // -r trustRoot 是"当作根证书信任"；不加 -d 就是装进用户域，不用 sudo。
    // 系统会弹一次授权框让用户确认——装根证书本来就该由人点头
    const res = await run(
      `security add-trusted-cert -r trustRoot -k "${macKeychain()}" "${CA_CERT}"`);
    if (res.ok) return { ok: true };
    const canceled = /User canceled|errAuthorizationCanceled|-128/.test(res.err);
    return { ok: false, canceled, message: canceled ? '你取消了授权' : res.err };
  }

  async function macUninstall() {
    const res = await run(`security delete-certificate -c "${CA_CN}" "${macKeychain()}"`);
    if (res.ok) return { ok: true };
    return { ok: false, message: res.err };
  }

  async function macInstalled() {
    const res = await run(`security find-certificate -c "${CA_CN}" "${macKeychain()}"`);
    return res.ok;
  }

  // ---------------------------------------------------------------- Windows

  async function winInstall() {
    // -user Root = 当前用户的「受信任的根证书颁发机构」，不需要管理员。
    // Windows 会弹一个安全警告框让用户确认，这是它自己的行为，也正该如此
    const res = await run(`certutil -addstore -user -f Root "${CA_CERT}"`);
    if (res.ok) return { ok: true };
    const canceled = /拒绝|denied|0x800704c7|canceled/i.test(res.err + res.out);
    return { ok: false, canceled, message: canceled ? '你取消了安装' : (res.err || res.out) };
  }

  async function winUninstall() {
    const res = await run(`certutil -delstore -user Root "${CA_CN}"`);
    if (res.ok) return { ok: true };
    return { ok: false, message: res.err || res.out };
  }

  async function winInstalled() {
    const res = await run(`certutil -store -user Root "${CA_CN}"`);
    return res.ok && new RegExp(CA_CN).test(res.out);
  }

  // -------------------------------------------------------------------- 对外

  async function install() {
    ensureCA();
    if (!fs.existsSync(CA_CERT)) return { ok: false, message: '证书还没生成出来' };
    if (process.platform === 'darwin') return macInstall();
    if (process.platform === 'win32') return winInstall();
    return { ok: false, message: '这个系统上要手动装证书' };
  }

  /**
   * 卸载。**先查再删，不靠错误文案判断"本来就没装"**。
   *
   * 踩过：`security delete-certificate` 找不到时报的是
   * "Unable to **delete** certificate matching..."，而按 "Unable to find" 去认
   * 就漏了——用户明明没装，界面却告诉他"卸不掉"。各系统各版本的文案都可能变，
   * 查一下是不是装着才是稳的判据。
   */
  async function uninstall() {
    const st = await status();
    if (!st.installed) return { ok: true, nothing: true };
    if (process.platform === 'darwin') return macUninstall();
    if (process.platform === 'win32') return winUninstall();
    return { ok: false, message: '这个系统上要手动删证书' };
  }

  async function status() {
    const exists = fs.existsSync(CA_CERT);
    let installed = false;
    try {
      if (exists && process.platform === 'darwin') installed = await macInstalled();
      else if (exists && process.platform === 'win32') installed = await winInstalled();
    } catch (e) { /* 查不出来就当没装：宁可让他多点一次，也不能说装了其实没装 */ }
    return { path: CA_CERT, exists, installed, hint: manualHint() };
  }

  /** 自动装不上时照着念给用户听。**按平台说**——两边的菜单名字完全不一样 */
  function manualHint() {
    if (process.platform === 'win32') {
      return `双击 ca-cert.pem →「安装证书」→ 存储位置选「当前用户」→ `
        + `选「将所有证书都放入下列存储」→ 浏览 →「受信任的根证书颁发机构」→ 完成。`;
    }
    return `双击 ca-cert.pem 装进「登录」钥匙串 → 在钥匙串访问里双击它 → `
      + `展开「信任」→「使用此证书时」选「始终信任」→ 关窗口输密码。`;
  }

  return { install, uninstall, status, ensureCA, CA_CERT, CA_CN };
}

module.exports = { createRootcert };
