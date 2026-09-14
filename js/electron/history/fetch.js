#!/usr/bin/env node
'use strict';

// 三层设计的第 2 层：断代层。从 electron/electron 仓库机械拉取两份官方数据，
// 解析、对到第 1 层快照的 key 上，落成可复现的 JSON。第 2 层不判断"这算不算
// 一个能力"，只管把"什么时候变的"拉准——归类是第 3 层的事。
//
// 数据源 A：docs/breaking-changes.md（取 main 分支，这份文档是累积的，从
// 最早版本一路记到最新）。
// 数据源 B：docs/api/*.md 里每个成员标题后面的 `YAML history` 块（取
// v43.3.0 tag，跟第 1 层快照用的版本对齐）。
//
// 用 gh cli 而不是裸 fetch，原因同 electron/extract.js：复用本机已登录的
// gh 认证，不撞匿名 API 限流。

const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { stableStringify } = require('../stable-json');
const WANTED = require('../modules');
const { loadRaw } = require('../extract');
const { parseBreakingChanges } = require('./parse-breaking-changes');
const { buildPrefixMap, parseApiHistoryDoc } = require('./parse-api-history');

const RAW_DIR = path.join(__dirname, '..', 'raw');
const API_RAW_DIR = path.join(RAW_DIR, 'api');
const HISTORY_DIR = __dirname;

const API_HISTORY_REF = 'v43.3.0'; // 跟第 1 层快照对齐的版本

function ghApiContent(repoPath, ref) {
  const out = execFileSync(
    'gh',
    ['api', `repos/electron/electron/contents/${repoPath}?ref=${ref}`, '-q', '.content'],
    { encoding: 'utf8' }
  );
  return Buffer.from(out.trim(), 'base64').toString('utf8');
}

function ghCommitSha(ref) {
  return execFileSync('gh', ['api', `repos/electron/electron/commits/${ref}`, '-q', '.sha'], {
    encoding: 'utf8',
  }).trim();
}

function ghListDir(repoPath, ref) {
  const out = execFileSync(
    'gh',
    ['api', `repos/electron/electron/contents/${repoPath}?ref=${ref}`, '-q', '.[].name'],
    { encoding: 'utf8' }
  );
  return out.trim().split('\n').filter(Boolean);
}

// WANTED 里的模块/类名 -> docs/api/ 下的文件名（kebab-case）。摸底确认过
// 这份清单里的名字都符合"大写字母前插 - 再全小写"的通用规则（见
// js/README.md 断代层一节），不用为哪个名字单独打补丁。
function kebab(name) {
  return name.replace(/([a-z0-9])([A-Z])/g, '$1-$2').toLowerCase();
}

function fetchBreakingChanges() {
  fs.mkdirSync(RAW_DIR, { recursive: true });
  console.error('[fetch-history] 下载 docs/breaking-changes.md（main）...');
  const sha = ghCommitSha('main');
  const content = ghApiContent('docs/breaking-changes.md', 'main');
  fs.writeFileSync(path.join(RAW_DIR, 'breaking-changes.md'), content);
  return { content, sha };
}

function fetchApiDocs() {
  fs.mkdirSync(API_RAW_DIR, { recursive: true });
  const sha = ghCommitSha(API_HISTORY_REF);

  // 先列目录核对文件名，而不是直接假设 kebab() 一定对——清单和实物对不上时
  // 要报错，不能悄悄漏掉某个模块的文档。
  const dirNames = new Set(ghListDir('docs/api', API_HISTORY_REF));
  const files = new Map(); // moduleOrClassName -> file basename（不含 .md）
  const missingDocs = [];
  for (const name of WANTED) {
    const base = kebab(name);
    if (!dirNames.has(`${base}.md`)) {
      missingDocs.push({ name, expected: `${base}.md` });
      continue;
    }
    files.set(base, name); // 去重：session/Session、webContents/WebContents 共用一个文件
  }
  if (missingDocs.length) {
    throw new Error(
      `docs/api（${API_HISTORY_REF}）里找不到以下模块/类对应的文档，kebab() 规则和实物对不上：` +
        JSON.stringify(missingDocs)
    );
  }

  const docs = {};
  for (const base of files.keys()) {
    console.error(`[fetch-history] 下载 docs/api/${base}.md（${API_HISTORY_REF}）...`);
    docs[base] = ghApiContent(`docs/api/${base}.md`, API_HISTORY_REF);
    fs.writeFileSync(path.join(API_RAW_DIR, `${base}.md`), docs[base]);
  }
  return { docs, sha };
}

function buildBreakingChangesJson(content, sha) {
  const { entries, stats } = parseBreakingChanges(content);
  return {
    source: {
      repo: 'electron/electron',
      path: 'docs/breaking-changes.md',
      ref: 'main',
      sha,
    },
    stats,
    entries,
  };
}

function buildApiHistoryJson(docs, sha, version = '43.3.0') {
  const raw = loadRaw(version);
  const wantedSet = new Set(WANTED);
  const prefixMap = buildPrefixMap(raw, wantedSet);

  const byKey = {};
  const unresolved = [];
  let totalBlocks = 0;

  for (const [file, markdown] of Object.entries(docs)) {
    const { resolved, unresolved: docUnresolved } = parseApiHistoryDoc(markdown, {
      prefixMap,
      wantedSet,
      file,
    });
    totalBlocks += resolved.length + docUnresolved.length;
    unresolved.push(...docUnresolved);
    for (const { key, history } of resolved) {
      if (byKey[key]) {
        throw new Error(`API History 里同一个 key 出现了不止一个 YAML 块，需要人工核对：${key}`);
      }
      byKey[key] = history;
    }
  }

  return {
    source: {
      repo: 'electron/electron',
      path: 'docs/api/*.md',
      ref: API_HISTORY_REF,
      sha,
    },
    stats: {
      totalBlocks,
      resolvedCount: Object.keys(byKey).length,
      unresolvedCount: unresolved.length,
    },
    byKey,
    unresolved,
  };
}

function main() {
  const { content: breakingMd, sha: breakingSha } = fetchBreakingChanges();
  const { docs, sha: apiSha } = fetchApiDocs();

  const breakingChanges = buildBreakingChangesJson(breakingMd, breakingSha);
  const apiHistory = buildApiHistoryJson(docs, apiSha);

  fs.writeFileSync(
    path.join(HISTORY_DIR, 'breaking-changes.json'),
    stableStringify(breakingChanges)
  );
  fs.writeFileSync(path.join(HISTORY_DIR, 'api-history.json'), stableStringify(apiHistory));

  console.error(
    `[fetch-history] breaking-changes：${breakingChanges.stats.totalEntries} 条` +
      `（版本外 ${breakingChanges.stats.skippedOutsideVersion} 段跳过）`
  );
  console.error(
    `[fetch-history] api-history：${apiHistory.stats.totalBlocks} 个 YAML 块，` +
      `resolved ${apiHistory.stats.resolvedCount}，unresolved ${apiHistory.stats.unresolvedCount}`
  );
}

if (require.main === module) {
  main();
}

module.exports = { buildBreakingChangesJson, buildApiHistoryJson, kebab };
