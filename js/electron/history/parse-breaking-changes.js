'use strict';

// 解析 electron/electron 的 docs/breaking-changes.md。这份文档是累积的
// （从很早的版本到最新 44.x/45.x/46.x 都在一份里），结构是：
//
//   ## Breaking API Changes (46.0)          <- 版本段（也可能是 "Planned
//   ### Behavior Changed: xxx                  Breaking API Changes"）
//   正文……
//   ### Removed: yyy
//   正文……
//   ## Breaking API Changes (45.0)
//   ……
//
// 只机械抽取，不做归类判断（哪些条目算"能力"变化是第 3 层的事）。

const { SlugTracker } = require('./slug');

const VERSION_HEADER_RE = /^## (?:Planned )?Breaking API Changes \(([\d.]+)\)\s*$/;
const ENTRY_HEADER_RE = /^### (.+)$/;

// 摸实物确认过的标准前缀（"Kind: 标题"形式）；早期版本（约 v14 及更早）的
// 条目标题不带这个前缀，直接是自由文本（如 "### \`webContents\`"），这类
// 归一成 kind: 'Other'，不强行套一个不存在的分类。
const KIND_PREFIX_RE =
  /^(Removed|Deprecated|Behavior Changed|Behavior Changes|Behavior changed|Default Changed|API Changed|Changed):\s*(.+)$/;

const KIND_ALIASES = {
  'Behavior Changes': 'Behavior Changed',
  'Behavior changed': 'Behavior Changed',
};

// 正文摘要：折叠空白后取前 300 字左右，代码块/链接原样保留在摘要里（不额外
// 剥离），因为摘要只是给人对照原文用，不追求"干净"。
function summarize(body, limit = 300) {
  const collapsed = body.replace(/\s+/g, ' ').trim();
  return collapsed.length > limit ? `${collapsed.slice(0, limit)}…` : collapsed;
}

// 正文里所有反引号包住的片段，去重、保序——用来当"提到的 API 名字"，
// 后面 historyOf() 的文本兜底关联就靠这个字段。
function extractMentionedApis(body) {
  const found = [];
  const seen = new Set();
  const re = /`([^`\n]+)`/g;
  let m;
  while ((m = re.exec(body))) {
    const name = m[1].trim();
    if (name && !seen.has(name)) {
      seen.add(name);
      found.push(name);
    }
  }
  return found;
}

function parseBreakingChanges(markdown) {
  const lines = markdown.split('\n');
  const slugTracker = new SlugTracker();

  const entries = [];
  let currentVersion = null;
  let current = null; // 正在收集正文的条目
  let bodyLines = [];
  let skippedOutsideVersion = 0; // 版本段之外的 ### 标题（如文首的说明段）

  function flush() {
    if (current) {
      const body = bodyLines.join('\n').trim();
      current.summary = summarize(body);
      current.mentionedApis = extractMentionedApis(body);
      entries.push(current);
    }
    current = null;
    bodyLines = [];
  }

  for (const line of lines) {
    const versionMatch = line.match(VERSION_HEADER_RE);
    if (versionMatch) {
      flush();
      currentVersion = versionMatch[1];
      continue;
    }
    if (/^## /.test(line)) {
      // 其它二级标题（非版本段，理论上不会出现在这份文档里，防御性处理）
      flush();
      currentVersion = null;
      continue;
    }

    const entryMatch = line.match(ENTRY_HEADER_RE);
    if (entryMatch) {
      flush();
      const title = entryMatch[1].trim();
      if (!currentVersion) {
        // 版本段之外的三级标题（如文档开头的"Types of Breaking Changes"），
        // 不是断代条目，计数报出来但不当作 entry。
        skippedOutsideVersion += 1;
        continue;
      }
      const anchor = slugTracker.next(title);
      const kindMatch = title.match(KIND_PREFIX_RE);
      let kind;
      let displayTitle;
      if (kindMatch) {
        const rawKind = kindMatch[1];
        kind = KIND_ALIASES[rawKind] || rawKind;
        displayTitle = kindMatch[2].trim();
      } else {
        kind = 'Other';
        displayTitle = title;
      }
      current = {
        version: currentVersion,
        kind,
        title: displayTitle,
        rawTitle: title,
        anchor,
      };
      continue;
    }

    if (current) bodyLines.push(line);
  }
  flush();

  // 按 version 降序（数字比较），同版本内按文档出现顺序（parseFloat 处理
  // "43.0" 这种 major.0 形式；理论上只会是这个形式，出现别的形式说明文档
  // 格式变了，需要人来看）。
  const withIndex = entries.map((e, i) => ({ e, i }));
  withIndex.sort((a, b) => {
    const va = parseFloat(a.e.version);
    const vb = parseFloat(b.e.version);
    if (va !== vb) return vb - va;
    return a.i - b.i;
  });
  const sorted = withIndex.map((x) => x.e);

  const kindCounts = {};
  const versionCounts = {};
  for (const e of sorted) {
    kindCounts[e.kind] = (kindCounts[e.kind] || 0) + 1;
    versionCounts[e.version] = (versionCounts[e.version] || 0) + 1;
  }

  return {
    entries: sorted,
    stats: {
      totalEntries: sorted.length,
      byKind: kindCounts,
      byVersion: versionCounts,
      skippedOutsideVersion,
    },
  };
}

module.exports = { parseBreakingChanges, summarize, extractMentionedApis };
