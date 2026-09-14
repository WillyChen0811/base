#!/usr/bin/env node
'use strict';

// 三层设计的第 1 层：快照层，纯机械地把 electron-api.json 里我们关心的模块/类
// 提取成精简、可 diff 的 JSON。第 2 层（断代）、第 3 层（实测坑）以后挂在这份
// 快照的 key 上，这里先不做。

const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const WANTED = require('./modules');
const { stableStringify } = require('./stable-json');

const RAW_DIR = path.join(__dirname, 'raw');
const SNAPSHOT_DIR = path.join(__dirname, 'snapshots');

function rawPath(version) {
  return path.join(RAW_DIR, `${version}.json`);
}

function snapshotPath(version) {
  return path.join(SNAPSHOT_DIR, `${version}.json`);
}

// electron-api.json 不随源码走，只挂在 GitHub release 附件里，所以缺了就现下。
// 用 gh cli 而不是裸 fetch：release 附件是公开的，但匿名访问 GitHub API 有限流，
// gh 复用本机已有的认证就不会撞上。
function downloadRaw(version) {
  fs.mkdirSync(RAW_DIR, { recursive: true });
  const dest = rawPath(version);
  console.error(`[extract] 下载 electron-api.json（v${version}）...`);
  execFileSync(
    'gh',
    [
      'release',
      'download',
      `v${version}`,
      '-R',
      'electron/electron',
      '-p',
      'electron-api.json',
      '-D',
      RAW_DIR,
      '--clobber',
    ],
    { stdio: 'inherit' }
  );
  const downloaded = path.join(RAW_DIR, 'electron-api.json');
  fs.renameSync(downloaded, dest);
  return dest;
}

function loadRaw(version) {
  const file = rawPath(version);
  if (!fs.existsSync(file)) {
    downloadRaw(version);
  }
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

// 取 description 第一行/第一句，快照只要一行摘要，完整说明去官方文档看。
function firstLine(description) {
  if (!description) return '';
  return description.split('\n')[0].trim();
}

// type 字段在 electron-api.json 里三种形态都会出现：
// - 字符串（"String" / "boolean" / ...）
// - 字符串数组（联合类型，如 [Function, null]）
// - 对象（嵌套结构，如 Function 类型自带 parameters/returns，Object 类型自带 properties）
// 统一递归成 JSON 安全的结构，字段名尽量沿用原文件（type/collection/parameters/
// properties/returns/innerTypes），方便对照官方文档反查。
function simplifyType(type) {
  if (type == null) return type;
  if (typeof type === 'string') return type;
  if (Array.isArray(type)) return type.map(simplifyType);
  if (typeof type === 'object') {
    const out = {};
    if (type.type !== undefined) out.type = simplifyType(type.type);
    if (type.collection) out.collection = true;
    if (type.parameters && type.parameters.length) {
      out.parameters = type.parameters.map(simplifyParam);
    }
    if (type.properties && type.properties.length) {
      out.properties = type.properties.map(simplifyParam);
    }
    if (type.innerTypes && type.innerTypes.length) {
      out.innerTypes = type.innerTypes.map(simplifyType);
    }
    if (type.returns) out.returns = simplifyParam(type.returns);
    return out;
  }
  return type;
}

// possibleValues 里的每一项是 {value, description}；description 在实物里基本
// 都是空字符串，只在非空时才保留，避免快照里全是噪声字段。
function simplifyPossibleValues(possibleValues) {
  if (!possibleValues) return undefined;
  return possibleValues.map((pv) =>
    pv.description ? { value: pv.value, description: pv.description } : pv.value
  );
}

// 参数/属性/事件字段/返回值在 electron-api.json 里是同一套形状，递归复用一个函数。
// 特意不带上 description：嵌套层级的说明文字太长，会把快照变成半份文档，
// diff 的意义也弱——规格要的是 name/type/required/eventProperties/possibleValues。
function simplifyParam(node) {
  if (node == null) return null;
  const out = {};
  if (node.name !== undefined) out.name = node.name;
  if (node.required !== undefined) out.required = !!node.required;
  if (node.collection) out.collection = true;
  if (node.type !== undefined) out.type = simplifyType(node.type);
  const possibleValues = simplifyPossibleValues(node.possibleValues);
  if (possibleValues !== undefined) out.possibleValues = possibleValues;
  if (node.additionalTags && node.additionalTags.length) {
    out.additionalTags = node.additionalTags;
  }
  if (node.properties && node.properties.length) {
    out.properties = node.properties.map(simplifyParam);
  }
  if (node.parameters && node.parameters.length) {
    out.parameters = node.parameters.map(simplifyParam);
  }
  if (node.eventProperties && node.eventProperties.length) {
    out.eventProperties = node.eventProperties.map(simplifyParam);
  }
  if (node.innerTypes && node.innerTypes.length) {
    out.innerTypes = node.innerTypes.map(simplifyType);
  }
  if (node.returns) out.returns = simplifyParam(node.returns);
  return out;
}

// 稳定 key 规则（第 2/3 层要挂在这上面，规则一旦定下不要轻改）：
//   模块方法/属性   name.memberName        如 app.getPath
//   模块事件        name@event-name        如 app@will-finish-launching
//   类实例方法/属性  ClassName#memberName   如 Session#setCertificateVerifyProc
//   类实例事件      ClassName@event-name   如 WebContents@console-message
//   类静态方法/属性  ClassName.memberName   如 BrowserWindow.getAllWindows
// 方法与属性共用同一个分隔符（# 或 .）是因为摸底确认过两边同一模块/类内部
// 没有同名的方法+属性组合（见 js/tests 的 no-collision 测试）；事件单独用 @，
// 即使同名方法/事件（如 BrowserWindow 的 close() 方法与 close 事件）也不会撞 key。
function buildKey({ containerName, scope, kind, memberName }) {
  if (kind === 'event') return `${containerName}@${memberName}`;
  const sep = scope === 'static' ? '.' : scope === 'module' ? '.' : '#';
  return `${containerName}${sep}${memberName}`;
}

// 把一个 Module 或 Class 条目里的方法/属性/事件都展开成快照条目数组。
function extractContainer(item) {
  const isClass = item.type === 'Class';
  const entries = [];

  function pushAll(list, kind, scope) {
    for (const raw of list || []) {
      const key = buildKey({
        containerName: item.name,
        scope,
        kind,
        memberName: raw.name,
      });
      const entry = {
        key,
        module: item.name,
        itemType: item.type,
        kind,
        scope,
        name: raw.name,
        description: firstLine(raw.description),
        additionalTags: raw.additionalTags || [],
      };
      if (kind === 'method') {
        entry.signature = raw.signature || '';
        entry.parameters = (raw.parameters || []).map(simplifyParam);
        entry.returns = raw.returns ? simplifyParam(raw.returns) : null;
      } else if (kind === 'property') {
        entry.propertyType = raw.type !== undefined ? simplifyType(raw.type) : null;
        const possibleValues = simplifyPossibleValues(raw.possibleValues);
        if (possibleValues !== undefined) entry.possibleValues = possibleValues;
      } else if (kind === 'event') {
        entry.parameters = (raw.parameters || []).map(simplifyParam);
      }
      entries.push(entry);
    }
  }

  if (isClass) {
    pushAll(item.instanceMethods, 'method', 'instance');
    pushAll(item.instanceProperties, 'property', 'instance');
    pushAll(item.instanceEvents, 'event', 'instance');
    pushAll(item.staticMethods, 'method', 'static');
    pushAll(item.staticProperties, 'property', 'static');
  } else {
    pushAll(item.methods, 'method', 'module');
    pushAll(item.properties, 'property', 'module');
    pushAll(item.events, 'event', 'module');
  }

  return entries;
}

function extract(version) {
  const raw = loadRaw(version);
  const byName = new Map(raw.map((item) => [item.name, item]));

  const missing = WANTED.filter((name) => !byName.has(name));
  if (missing.length) {
    throw new Error(
      `electron-api.json（v${version}）里找不到以下模块/类，清单和实物对不上，` +
        `先确认是改名了还是版本里确实没有：${missing.join(', ')}`
    );
  }

  const entries = [];
  for (const name of WANTED) {
    entries.push(...extractContainer(byName.get(name)));
  }

  const keys = new Set();
  for (const entry of entries) {
    if (keys.has(entry.key)) {
      throw new Error(`快照 key 撞了，key 规则需要重新设计：${entry.key}`);
    }
    keys.add(entry.key);
  }

  const byKey = {};
  for (const entry of entries) byKey[entry.key] = entry;

  return {
    electronVersion: version,
    modules: WANTED.slice(),
    entryCount: entries.length,
    entries: byKey,
  };
}

function writeSnapshot(version) {
  const snapshot = extract(version);
  fs.mkdirSync(SNAPSHOT_DIR, { recursive: true });
  fs.writeFileSync(snapshotPath(version), stableStringify(snapshot));
  return snapshot;
}

function main() {
  const version = process.argv[2];
  if (!version) {
    console.error('用法：node electron/extract.js <electron 版本号，如 43.3.0>');
    process.exit(1);
  }
  const snapshot = writeSnapshot(version);
  const moduleCount = new Set(Object.values(snapshot.entries).map((e) => e.module)).size;
  console.error(
    `[extract] v${version}：${moduleCount} 个模块/类，${snapshot.entryCount} 条 API，` +
      `已写入 ${path.relative(process.cwd(), snapshotPath(version))}`
  );
}

if (require.main === module) {
  main();
}

module.exports = { extract, writeSnapshot, rawPath, snapshotPath, loadRaw };
