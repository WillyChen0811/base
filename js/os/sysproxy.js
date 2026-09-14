/**
 * 系统代理的设置与还原（全局抓包用）。
 *
 * 录制窗口那条路不碰系统代理——它只给自己那个 session 设 `setProxy`。但全局
 * 抓包的对象是**别的程序**（C/S 客户端、APP、别的浏览器），只能走系统代理。
 *
 * ## 还原是这里最要紧的事
 *
 * 设了代理不还原，用户整台机器都上不了网，而且**绝对想不到是我们干的**——
 * 界面上早就关掉了，代理还指着一个已经不存在的端口。所以：
 *
 *   · 动手之前把原样子记进文件（`~/.filecore/traffic/sysproxy.json`），
 *     **不只记在内存里**：程序崩了、被强杀了，内存里那份就没了，而文件还在，
 *     下次启动照样还得回去；
 *   · 界面上「还原」按钮**任何时候都能点**（哪怕这次没开过抓包），它认的是
 *     那个文件；
 *   · 起抓包时如果发现文件已经在了，说明上次没还原干净，**不覆盖它**——
 *     覆盖了就把"原本的样子"换成了"我们上次设的代理"，再也回不去。
 *
 * ## 权限
 *
 * macOS 的 `networksetup` 改代理要 root，所以走 `osascript ... with
 * administrator privileges` 弹一次系统授权框。用户不给权限是正常选择，
 * 这时**如实报回去并给出手动设置的说明**，不要假装成功——他会以为在抓，
 * 结果一条都没有。
 *
 * 从 websubmit macClient（traffic/sysproxy.js）搬入 base：唯一改动是状态文件
 * 路径不再写死 `~/.filecore/traffic/sysproxy.json`，改成工厂函数按调用方传入
 * 的路径建实例。
 */
const { exec } = require('child_process');
const fs = require('fs');
const path = require('path');

function run(cmd, opts = {}) {
  return new Promise((resolve) => {
    exec(cmd, { timeout: 30000, ...opts }, (err, stdout, stderr) => {
      resolve({ ok: !err, out: (stdout || '').trim(), err: (stderr || err?.message || '').trim() });
    });
  });
}

/** 用管理员权限跑一段 shell（macOS 会弹系统授权框） */
function runAsAdmin(script) {
  const esc = script.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
  return run(`osascript -e "do shell script \\"${esc}\\" with administrator privileges"`);
}

/**
 * 建一个系统代理管理器实例。`stateFile` 由调用方传入（原实现写死
 * `~/.filecore/traffic/sysproxy.json`，base 不带项目默认值）。
 *
 * @param {string} stateFile 记录"原样子"的状态文件路径
 */
function createSysproxy(stateFile) {
  const STATE_FILE = stateFile;

  function readState() {
    try {
      return JSON.parse(fs.readFileSync(STATE_FILE, 'utf8'));
    } catch (e) {
      return null;
    }
  }

  function writeState(state) {
    fs.mkdirSync(path.dirname(STATE_FILE), { recursive: true });
    fs.writeFileSync(STATE_FILE, JSON.stringify(state, null, 2), 'utf8');
  }

  function clearState() {
    try { fs.unlinkSync(STATE_FILE); } catch (e) { /* 本来就没有 */ }
  }

  // ------------------------------------------------------------------ macOS

  /**
   * 真正在用的那个网络服务（Wi-Fi / 以太网）。
   *
   * **不能对所有服务一律设代理**：`-listallnetworkservices` 会列出
   * "LG Monitor Controls" 这种显示器附带的虚拟服务，对它们设代理没有意义，
   * 而且失败信息会盖住真正的错。所以顺着默认路由找接口（en0），再按
   * `-listnetworkserviceorder` 把接口映射回服务名。
   *
   * 找不到就退回"所有启用着的服务"——宁可多设几个，也不能一个都没设上却
   * 报成功（用户会以为在抓，结果一条都没有）。
   */
  async function macServices() {
    const all = await run('networksetup -listallnetworkservices');
    const enabled = all.ok
      ? all.out.split('\n').slice(1).map((s) => s.trim()).filter((s) => s && !s.startsWith('*'))
      : [];

    const dev = await run('route get default');
    const iface = (dev.out.match(/interface:\s*(\S+)/) || [])[1];
    if (iface) {
      const order = await run('networksetup -listnetworkserviceorder');
      // "(6) Wi-Fi\n(Hardware Port: Wi-Fi, Device: en0)" —— 两行一组
      const re = new RegExp(`\\(\\d+\\)\\s*(.+)\\n\\(Hardware Port:.*Device: ${iface}\\)`);
      const m = order.out.match(re);
      if (m && enabled.includes(m[1].trim())) return [m[1].trim()];
    }
    return enabled;
  }

  async function macRead(service) {
    const web = await run(`networksetup -getwebproxy "${service}"`);
    const secure = await run(`networksetup -getsecurewebproxy "${service}"`);
    const parse = (out) => {
      const get = (k) => (out.match(new RegExp(`^${k}: (.*)$`, 'm')) || [])[1] || '';
      return { enabled: get('Enabled') === 'Yes', server: get('Server'), port: get('Port') };
    };
    return { service, web: parse(web.out), secure: parse(secure.out) };
  }

  async function macApply(port) {
    const services = await macServices();
    if (!services.length) return { ok: false, message: '没找到可用的网络服务' };

    // 只在没有旧账的时候记——上次没还原干净时覆盖会把"原本的样子"弄丢
    if (!readState()) {
      const before = [];
      for (const s of services) before.push(await macRead(s));
      writeState({ platform: 'darwin', at: Date.now(), port, before });
    }

    const cmds = services.flatMap((s) => [
      `networksetup -setwebproxy '${s}' 127.0.0.1 ${port}`,
      `networksetup -setsecurewebproxy '${s}' 127.0.0.1 ${port}`,
    ]);
    const res = await runAsAdmin(cmds.join('; '));
    if (!res.ok) {
      return { ok: false, message: res.err.includes('User canceled')
        ? '你取消了授权。也可以到「系统设置 → 网络 → 详细信息 → 代理」里手动填 127.0.0.1'
        : `设置系统代理失败：${res.err}` };
    }
    return { ok: true, services };
  }

  async function macRestore(state) {
    const cmds = [];
    for (const b of state.before || []) {
      const s = b.service;
      if (b.web?.enabled && b.web.server) {
        cmds.push(`networksetup -setwebproxy '${s}' ${b.web.server} ${b.web.port}`);
      } else {
        cmds.push(`networksetup -setwebproxystate '${s}' off`);
      }
      if (b.secure?.enabled && b.secure.server) {
        cmds.push(`networksetup -setsecurewebproxy '${s}' ${b.secure.server} ${b.secure.port}`);
      } else {
        cmds.push(`networksetup -setsecurewebproxystate '${s}' off`);
      }
    }
    if (!cmds.length) return { ok: true };
    const res = await runAsAdmin(cmds.join('; '));
    return res.ok ? { ok: true } : { ok: false, message: `还原失败：${res.err}` };
  }

  // ---------------------------------------------------------------- Windows

  const WIN_KEY = 'HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings';

  /**
   * 改完注册表**必须发一次刷新通知**，否则已经开着的程序不认——症状是
   * "设了没用"，而注册表里明明写着。WinINet 的 InternetSetOption 干这件事，
   * 命令行没有现成的，所以借 PowerShell 调一下。
   */
  async function winRefresh() {
    const ps = [
      "$s='[DllImport(\"wininet.dll\",SetLastError=true)]public static extern bool InternetSetOption(IntPtr h,int o,IntPtr b,int l);';",
      "$t=Add-Type -MemberDefinition $s -Name WinINet -Namespace Win32 -PassThru;",
      '$t::InternetSetOption([IntPtr]::Zero,39,[IntPtr]::Zero,0)|Out-Null;',
      '$t::InternetSetOption([IntPtr]::Zero,37,[IntPtr]::Zero,0)|Out-Null;',
    ].join('');
    return run(`powershell -NoProfile -ExecutionPolicy Bypass -Command "${ps}"`,
      { windowsHide: true });
  }

  async function winRead() {
    const q = async (name) => {
      const res = await run(`reg query "${WIN_KEY}" /v ${name}`, { windowsHide: true });
      if (!res.ok) return '';
      const m = res.out.match(new RegExp(`${name}\\s+REG_\\w+\\s+(.*)`));
      return m ? m[1].trim() : '';
    };
    return { enable: await q('ProxyEnable'), server: await q('ProxyServer') };
  }

  async function winApply(port) {
    if (!readState()) {
      writeState({ platform: 'win32', at: Date.now(), port, before: await winRead() });
    }
    const a = await run(`reg add "${WIN_KEY}" /v ProxyServer /t REG_SZ /d "127.0.0.1:${port}" /f`,
      { windowsHide: true });
    const b = await run(`reg add "${WIN_KEY}" /v ProxyEnable /t REG_DWORD /d 1 /f`,
      { windowsHide: true });
    if (!a.ok || !b.ok) return { ok: false, message: `写注册表失败：${a.err || b.err}` };
    await winRefresh();
    return { ok: true };
  }

  async function winRestore(state) {
    const before = state.before || {};
    // 原来是 0x1 这种十六进制串，还原时按原样写回去；没有就关掉
    const on = /0x1/.test(before.enable || '');
    await run(`reg add "${WIN_KEY}" /v ProxyEnable /t REG_DWORD /d ${on ? 1 : 0} /f`,
      { windowsHide: true });
    if (before.server) {
      await run(`reg add "${WIN_KEY}" /v ProxyServer /t REG_SZ /d "${before.server}" /f`,
        { windowsHide: true });
    }
    await winRefresh();
    return { ok: true };
  }

  // -------------------------------------------------------------------- 对外

  async function apply(port) {
    if (process.platform === 'darwin') return macApply(port);
    if (process.platform === 'win32') return winApply(port);
    return { ok: false, message: '这个系统上还不会自动设代理，请手动配置' };
  }

  /**
   * 还原。**认的是文件不是内存**，所以程序崩过、重启过也照样还得回去。
   * 没有那份文件 = 我们没动过系统代理，直接算成功（按钮任何时候都能点）。
   */
  async function restore() {
    const state = readState();
    if (!state) return { ok: true, nothing: true };
    const res = state.platform === 'win32' ? await winRestore(state) : await macRestore(state);
    if (res.ok) clearState();
    return res;
  }

  /** 现在系统代理是什么样，以及有没有我们上次留下的、还没还原的账 */
  async function status() {
    const pending = readState();
    let current = null;
    try {
      if (process.platform === 'darwin') {
        const services = await macServices();
        current = services.length ? await macRead(services[0]) : null;
      } else if (process.platform === 'win32') {
        current = await winRead();
      }
    } catch (e) { /* 读不到就报 null，不影响开关 */ }
    return { pending: pending ? { at: pending.at, port: pending.port } : null, current };
  }

  /** 自动设不成时，界面上照着念给用户听 */
  function manualHint(port) {
    if (process.platform === 'win32') {
      return `Windows：设置 → 网络和 Internet → 代理 → 手动设置代理，`
        + `地址 127.0.0.1，端口 ${port}，保存。`;
    }
    return `macOS：系统设置 → 网络 → （当前网络）详细信息 → 代理 → `
      + `勾选「网页代理 HTTP」与「安全网页代理 HTTPS」，服务器 127.0.0.1，端口 ${port}。`;
  }

  return { apply, restore, status, manualHint, STATE_FILE };
}

module.exports = { createSysproxy };
