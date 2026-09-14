'use strict';

// 读取已入库的两份断代数据（electron/history/*.json，由 fetch.js 生成），
// 提供按 key 查询的入口。数据本身的抓取/解析逻辑在 fetch.js + parse-*.js，
// 这个文件只管"读出来、拼起来"。

const path = require('node:path');

function loadBreakingChanges() {
  // 用 require 而不是 fs.readFileSync + JSON.parse：这两份是入库的静态
  // 文件，require 自带缓存，重复调用不用重复解析。
  return require(path.join(__dirname, 'breaking-changes.json'));
}

function loadApiHistory() {
  return require(path.join(__dirname, 'api-history.json'));
}

function parseKey(key) {
  let m = key.match(/^(.+)#constructor$/);
  if (m) return { container: m[1], member: 'constructor', kind: 'constructor' };
  m = key.match(/^(.+)@([^@]+)$/);
  if (m) return { container: m[1], member: m[2], kind: 'event' };
  m = key.match(/^(.+)[.#]([^.#]+)$/);
  if (m) return { container: m[1], member: m[2], kind: 'member' };
  return { container: key, member: null, kind: 'unknown' };
}

// 精确关联：API History 该 key 下 added/changes/deprecated 里任何一条带
// breaking-changes-header 的记录，直接对到 breaking-changes 里同 anchor 的
// 条目——这是文档作者自己标注的关联，最可靠。
function exactLinkedEntries(apiHistoryEntry, breakingChanges) {
  if (!apiHistoryEntry) return [];
  const anchors = new Set();
  for (const section of ['added', 'changes', 'deprecated']) {
    for (const item of apiHistoryEntry[section] || []) {
      if (item.breakingChangesHeader) anchors.add(item.breakingChangesHeader);
    }
  }
  if (!anchors.size) return [];
  return breakingChanges.entries.filter((e) => anchors.has(e.anchor));
}

// 兜底关联：不少 API 的弃用/变更只在 breaking-changes.md 里用大白话提过，
// API History YAML 里压根没打 breaking-changes-header（摸实物确认过，比如
// WebContents 的 console-message 事件弃用几个位置参数就是这样——
// docs/api/web-contents.md 里那段只用行内 `_Deprecated_` 标记，没有 YAML
// 块）。这种时候退而求其次：breaking-changes 条目正文里提到的反引号词
// （mentionedApis）如果同时包含成员名和容器名，就认为大概率是在说这个
// key——这是文本匹配的启发式，不保证准确，所以关联结果里单独标
// linkedVia: 'mention'，跟精确关联区分开，调用方可以自己决定要不要用。
function mentionLinkedEntries(key, breakingChanges) {
  const { container, member } = parseKey(key);
  if (!member) return [];
  return breakingChanges.entries.filter((e) => {
    const mentions = e.mentionedApis || [];
    return mentions.includes(member) && mentions.includes(container);
  });
}

// 某个 key 的完整断代信息：API History（可能为空）+ 关联到的 breaking-changes
// 条目（精确关联在前，兜底关联在后，且不重复）。
function historyOf(key) {
  const breakingChanges = loadBreakingChanges();
  const apiHistory = loadApiHistory();
  const apiHistoryEntry = apiHistory.byKey[key] || null;

  const exact = exactLinkedEntries(apiHistoryEntry, breakingChanges).map((e) => ({
    entry: e,
    linkedVia: 'breaking-changes-header',
  }));
  const exactAnchors = new Set(exact.map((x) => x.entry.anchor));
  const mentioned = mentionLinkedEntries(key, breakingChanges)
    .filter((e) => !exactAnchors.has(e.anchor))
    .map((e) => ({ entry: e, linkedVia: 'mention' }));

  return {
    key,
    apiHistory: apiHistoryEntry,
    breakingChanges: [...exact, ...mentioned],
  };
}

module.exports = { loadBreakingChanges, loadApiHistory, historyOf, parseKey };
