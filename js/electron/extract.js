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
// 构造函数单独一种 kind：electron-api.json 把它放在 constructorMethod 字段，
// 不在 instanceMethods 里，但语义上和实例方法一样挂在 `#` 下——用
// `new BrowserWindow(...)` 时传的 options 也要能被第 2 层按 key 挂断代信息。
// Structure 的属性（0b）复用 `.` 分隔符，和模块成员/类静态成员同一形式：
// Structure 名大写开头、模块名小写开头，不会互相撞 key（extract() 里的撞车
// 断言覆盖了全部 entries，包括这里新增的两类）。
const DOT_SCOPES = new Set(['static', 'module', 'structure']);

function buildKey({ containerName, scope, kind, memberName }) {
  if (kind === 'event') return `${containerName}@${memberName}`;
  if (kind === 'constructor') return `${containerName}#constructor`;
  const sep = DOT_SCOPES.has(scope) ? '.' : '#';
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
    // 构造函数：不是所有收录的类都能被用户 new（如 CommandLine 只能从
    // app.commandLine 拿现成实例），constructorMethod 是 null 就跳过。
    if (item.constructorMethod) {
      const ctor = item.constructorMethod;
      entries.push({
        key: buildKey({ containerName: item.name, kind: 'constructor' }),
        module: item.name,
        itemType: item.type,
        kind: 'constructor',
        scope: 'instance',
        name: 'constructor',
        description: firstLine(ctor.description),
        additionalTags: ctor.additionalTags || [],
        signature: ctor.signature || '',
        parameters: (ctor.parameters || []).map(simplifyParam),
      });
    }
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

// ---- 0b：被引用的 Structure，传递闭包收集 ----
//
// electron-api.json 里参数/返回值/属性的 type 字段只记 Structure 的名字
// （如 "BrowserWindowConstructorOptions"），具体有哪些字段要去 Structure
// 条目自己的 properties 里找。第 2 层要挂"某个选项哪个版本才有"，没有 key
// 就无处可挂，所以这里把收录模块/类用到的 Structure 也展开成 entries——
// 但只展开"被引用到"的，不是 electron-api.json 里全部 174 项都收。
//
// electron-api.json 的 Class 条目（instanceMethods 等 5 个数组）已经是拍平的
// （子类自己不重复声明继承来的成员，但数组里已经包含父类成员，如
// BrowserWindow 的 instanceMethods 比 BaseWindow 多，`BrowserWindow#setBounds`
// 已经在里面），但 Structure 不是——BrowserWindowConstructorOptions 自己的
// properties 只有 2 条，width/height/show/x/y 等 50 条都在
// BaseWindowConstructorOptions 里，要靠 `extends` 字段才能找到。所以这里既
// 顺着 type 引用走，也顺着 `extends` 链把父 Structure 收进来；extractStructure
// 再把继承链拍平进子 Structure 自己的 entries（见下面 flattenStructureProperties），
// 这样查"BrowserWindow 能传哪些 options"不用消费方自己知道去查父类。
// extends 目标不是本次收录范围内的 Structure（比如 IpcMainEvent extends
// Event——Event 是 Node/DOM 内置类型，不在 electron-api.json 里）时链就到此为止，
// 没法拍平不存在的文档条目。

// 递归找出 type 结构（字符串/数组/对象）里出现的 Structure 名字，塞进 found。
function walkTypeRefs(type, structNames, found) {
  if (type == null) return;
  if (typeof type === 'string') {
    if (structNames.has(type)) found.add(type);
    return;
  }
  if (Array.isArray(type)) {
    for (const t of type) walkTypeRefs(t, structNames, found);
    return;
  }
  if (typeof type === 'object') {
    if (type.type !== undefined) walkTypeRefs(type.type, structNames, found);
    for (const p of type.parameters || []) walkNodeRefs(p, structNames, found);
    for (const p of type.properties || []) walkNodeRefs(p, structNames, found);
    for (const t of type.innerTypes || []) walkTypeRefs(t, structNames, found);
    if (type.returns) walkNodeRefs(type.returns, structNames, found);
  }
}

// 参数/属性/事件字段/返回值（raw 形态，未经 simplify）统一递归，收集其中
// 出现的 Structure 名字。
function walkNodeRefs(node, structNames, found) {
  if (node == null) return;
  walkTypeRefs(node.type, structNames, found);
  for (const p of node.properties || []) walkNodeRefs(p, structNames, found);
  for (const p of node.parameters || []) walkNodeRefs(p, structNames, found);
  for (const p of node.eventProperties || []) walkNodeRefs(p, structNames, found);
  for (const t of node.innerTypes || []) walkTypeRefs(t, structNames, found);
  if (node.returns) walkNodeRefs(node.returns, structNames, found);
}

// 收录模块/类自身（含构造函数）的所有方法/属性/事件节点，交给 walkNodeRefs 找引用。
function collectOwnNodes(item) {
  const nodes = [];
  if (item.type === 'Class') {
    if (item.constructorMethod) nodes.push(item.constructorMethod);
    nodes.push(
      ...(item.instanceMethods || []),
      ...(item.instanceProperties || []),
      ...(item.instanceEvents || []),
      ...(item.staticMethods || []),
      ...(item.staticProperties || [])
    );
  } else {
    nodes.push(...(item.methods || []), ...(item.properties || []), ...(item.events || []));
  }
  return nodes;
}

// 从 WANTED 模块/类出发，传递闭包收集被引用到的 Structure 条目（原始形态）。
// 返回 Map<name, rawItem>，按发现顺序无所谓，调用方会自己排序。
function collectReferencedStructures(byName, wantedNames) {
  const structNames = new Set(
    Array.from(byName.values())
      .filter((item) => item.type === 'Structure')
      .map((item) => item.name)
  );

  const found = new Set();
  for (const name of wantedNames) {
    const item = byName.get(name);
    if (!item) continue;
    for (const node of collectOwnNodes(item)) walkNodeRefs(node, structNames, found);
  }

  // 再从已发现的 Structure 出发继续找：(a) 它自己 properties 里引用到的
  // Structure（如 BrowserWindowConstructorOptions.webPreferences 指向
  // WebPreferences），(b) 它 extends 的父 Structure（如
  // BrowserWindowConstructorOptions -> BaseWindowConstructorOptions），
  // 直到不再有新发现为止——两条线一起做传递闭包，因为父 Structure 自己的
  // properties 也可能引用别的 Structure。
  const visited = new Set(found);
  let frontier = Array.from(found);
  while (frontier.length) {
    const next = [];
    for (const sname of frontier) {
      const s = byName.get(sname);
      if (!s) continue;
      const nested = new Set();
      for (const p of s.properties || []) walkNodeRefs(p, structNames, nested);
      if (s.extends && structNames.has(s.extends)) nested.add(s.extends);
      for (const n of nested) {
        if (!visited.has(n)) {
          visited.add(n);
          next.push(n);
        }
      }
    }
    frontier = next;
  }

  const result = new Map();
  for (const name of visited) {
    const item = byName.get(name);
    if (item) result.set(name, item);
  }
  return result;
}

// 沿着 `extends` 链把一个 Structure 的属性拍平：返回 Map<propName,
// {raw, declaredIn}>，declaredIn 是实际声明这个属性的 Structure 名字
// （自己声明的就是它自己）。顺序从最远的祖先到自己，子的同名属性覆盖父的
// （Map.set 覆盖同 key 时值更新、原插入位置不变，不影响正确性，最终 entries
// 落进 byKey 对象，对外不依赖属性顺序）。
// cycle 防御：electron-api.json 里没见过 extends 成环，但 `seen` 兜底，
// 防止文档以后出现环时死循环。
function flattenStructureProperties(byName, item) {
  const chain = [];
  const seen = new Set();
  let cur = item;
  while (cur && !seen.has(cur.name)) {
    seen.add(cur.name);
    chain.unshift(cur);
    const parent = cur.extends && byName.get(cur.extends);
    cur = parent && parent.type === 'Structure' ? parent : null;
  }

  const byPropName = new Map();
  for (const node of chain) {
    for (const raw of node.properties || []) {
      byPropName.set(raw.name, { raw, declaredIn: node.name });
    }
  }
  return byPropName;
}

// 把一个 Structure 条目的每个属性（含沿 extends 链继承来的）展开成一条快照
// entry，key 形如 `BrowserWindowConstructorOptions.webPreferences`。继承来的
// 属性带 `inheritedFrom: 声明它的祖先 Structure 名字`，自己声明的没有这个
// 字段——第 3 层要挂"某个选项哪个版本才有"就挂在子的 key 上，不用消费方自己
// 回溯继承链。
function extractStructure(item, byName) {
  const entries = [];
  const flattened = flattenStructureProperties(byName, item);
  for (const { raw, declaredIn } of flattened.values()) {
    const entry = {
      key: buildKey({ containerName: item.name, scope: 'structure', kind: 'property', memberName: raw.name }),
      module: item.name,
      itemType: 'Structure',
      kind: 'property',
      scope: 'structure',
      name: raw.name,
      required: !!raw.required,
      description: firstLine(raw.description),
      additionalTags: raw.additionalTags || [],
      propertyType: raw.type !== undefined ? simplifyType(raw.type) : null,
    };
    if (declaredIn !== item.name) entry.inheritedFrom = declaredIn;
    const possibleValues = simplifyPossibleValues(raw.possibleValues);
    if (possibleValues !== undefined) entry.possibleValues = possibleValues;
    entries.push(entry);
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

  const referencedStructures = collectReferencedStructures(byName, WANTED);
  const structureNames = Array.from(referencedStructures.keys()).sort();
  for (const name of structureNames) {
    entries.push(...extractStructure(referencedStructures.get(name), byName));
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

  // modules 顶层字段要能看出每一项是 Module/Class 还是（被引用而收录的）
  // Structure——WANTED 里的按原顺序在前，Structure 按名字排序追加在后。
  // Structure 带 extends 字段时原样带出来（父 Structure 自己也在这份列表里，
  // 是独立的容器，有自己的 entries）。
  const modules = [
    ...WANTED.map((name) => ({ name, type: byName.get(name).type })),
    ...structureNames.map((name) => {
      const item = referencedStructures.get(name);
      const m = { name, type: 'Structure' };
      if (item.extends) m.extends = item.extends;
      return m;
    }),
  ];

  return {
    electronVersion: version,
    modules,
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
  const structCount = snapshot.modules.filter((m) => m.type === 'Structure').length;
  const containerCount = snapshot.modules.length - structCount;
  console.error(
    `[extract] v${version}：${containerCount} 个模块/类，${structCount} 个被引用的 Structure，` +
      `${snapshot.entryCount} 条 API，已写入 ${path.relative(process.cwd(), snapshotPath(version))}`
  );
}

if (require.main === module) {
  main();
}

module.exports = { extract, writeSnapshot, rawPath, snapshotPath, loadRaw };
