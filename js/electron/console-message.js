'use strict';

/**
 * `webContents` 的 `console-message` 事件换过签名，这里抹平。
 *
 *   旧：(event, level: number, message, line, sourceId)
 *   新：(details: Event & { level: string, message, lineNumber, sourceId, frame })
 *
 * **新签名里 details 就是第一个参数**（它自己兼任 event），不是在 event 后面
 * 又多一个——照着“事件回调第一个参数总是 event”去跳过第一个，拿到的是那几个
 * 弃用的位置参数。43.3.0 上实测发出来的是 5 个：
 * `[details, level: Integer, message, line, sourceId]`。
 *
 * 位置参数是 35.0 起标弃用的（`historyOf('WebContents@console-message')` 可查），
 * 43 上还在发，所以两条路现在都走得通；等它们真被删掉、只剩 details 一个
 * 参数时，认错位置就会安静地收到一片空白。两版都留着，将来最低支持版本抬上去
 * 时再删旧分支。
 */

// 旧签名里 level 是数字：0=verbose 1=info 2=warning 3=error。
const LEGACY_LEVELS = ['debug', 'log', 'warning', 'error'];

/**
 * 把两种签名归一成 { level, message, line, source }。
 * @param {any[]} args 监听器收到的全部参数（含首个 event）
 */
function normalizeConsoleMessage(args) {
  const [first, second, third, fourth, fifth] = args;

  // 不能只判断“是个对象”：旧签名的第一个 event 也是对象，必须认字段。
  if (first && typeof first.message === 'string' && typeof first.level === 'string') {
    return {
      level: first.level || 'log',
      message: first.message,
      line: first.lineNumber ?? null,
      source: first.sourceId || '',
    };
  }

  return {
    level: LEGACY_LEVELS[second] || 'log',
    message: String(third ?? ''),
    line: fourth ?? null,
    source: fifth || '',
  };
}

module.exports = { normalizeConsoleMessage };
