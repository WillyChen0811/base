'use strict';

// 解析 electron/electron docs/api/*.md 里的 `<!-- ```YAML history ... ``` -->`
// 块，归到第 1 层快照的 key 上。
//
// 每个 YAML 块紧跟在它描述的那个标题（### 或 ####，取决于该文档是模块+类
// 混合还是纯模块）后面，块本身没有名字信息，名字要从标题文本反推：
//   `new BrowserWindow([options])`        -> 构造函数 -> BrowserWindow#constructor
//   `win.setBounds(bounds[, animate])`    -> 实例方法（win 是文档手写别名）-> BrowserWindow#setBounds
//   `BrowserWindow.getAllWindows()`       -> 静态方法 -> BrowserWindow.getAllWindows
//   `session.fromPartition(...)`          -> 模块方法 -> session.fromPartition
//   Event: 'console-message'              -> 事件，容器要看当前在哪个 `## Class: X` 段落里
//                                             （没有就退回文档顶部 `# 模块名`）
//
// 标题前缀（win./ses./contents.）是文档作者手写的简称别名，跟
// electron-api.json 里的 instanceName 字段对不上（如 BrowserWindow 的
// instanceName 是 browserWindow，文档里却用 win），没法从 electron-api.json
// 机械推出来，所以单独维护一张小别名表——是从抓下来的文档里实际 grep 出来的
// （见 fetch.js），不是猜的。新增收录模块/类时如果文档也用了简称前缀，要把
// 新别名加进来；漏了不会静默丢，对应块会进 unresolved。

const MANUAL_PREFIX_ALIASES = {
  win: { owner: 'BrowserWindow', scope: 'instance' },
  ses: { owner: 'Session', scope: 'instance' },
  contents: { owner: 'WebContents', scope: 'instance' },
};

// 从 electron-api.json 的原始条目机械推出 前缀 -> {owner, scope} 的映射：
//   - Module 条目：前缀就是模块名本身，scope: 'module'
//   - Class 条目：前缀是类名本身时 scope: 'static'；类的 instanceName
//     字段（如 WebRequest 的 webRequest、CommandLine 的 commandLine）
//     很多情况下文档就是直接拿它当实例别名，scope: 'instance'
// 模块名冲突时（如 Session 类的 instanceName 恰好是 'session'，和 session
// 模块自己撞了前缀），模块条目优先——摸实物确认过 session.md 里
// `session.fromPartition` 这类前缀真的是模块方法，Session 类的实例方法
// 文档里用的是另一个手写别名 `ses.`（在 MANUAL_PREFIX_ALIASES 里），不会
// 走到这条自动推导的路径上。
function buildPrefixMap(rawItems, wantedSet) {
  const map = {};
  const byName = new Map(rawItems.map((it) => [it.name, it]));

  for (const name of wantedSet) {
    const item = byName.get(name);
    if (!item) continue;
    if (item.type === 'Class') {
      map[item.name] = { owner: item.name, scope: 'static' };
      if (item.instanceName && !(item.instanceName in map)) {
        map[item.instanceName] = { owner: item.name, scope: 'instance' };
      }
    }
  }
  // Module 条目最后处理，覆盖掉可能撞车的 instanceName 别名（见上面注释）。
  for (const name of wantedSet) {
    const item = byName.get(name);
    if (item && item.type === 'Module') {
      map[item.name] = { owner: item.name, scope: 'module' };
    }
  }
  Object.assign(map, MANUAL_PREFIX_ALIASES);
  return map;
}

function extractCodeSpan(title) {
  const m = title.match(/`([^`]+)`/);
  return m ? m[1] : null;
}

function extractEventName(title) {
  const m = title.match(/^Event:\s*'([^']+)'/);
  return m ? m[1] : null;
}

// 把一条标题解析成快照 key；解析不出来返回 null，调用方计入 unresolved。
function resolveKey(title, { owner, prefixMap, wantedSet }) {
  if (/^Event:/.test(title)) {
    const eventName = extractEventName(title);
    if (!eventName || !owner) return null;
    return `${owner}@${eventName}`;
  }

  const code = extractCodeSpan(title);
  if (!code) return null;

  const ctorMatch = code.match(/^new\s+([A-Za-z0-9_$]+)/);
  if (ctorMatch) {
    return wantedSet.has(ctorMatch[1]) ? `${ctorMatch[1]}#constructor` : null;
  }

  const memberMatch = code.match(/^([A-Za-z_$][\w$]*)\.([A-Za-z_$][\w$]*)/);
  if (!memberMatch) return null;
  const [, prefix, member] = memberMatch;

  const mapped = prefixMap[prefix];
  if (!mapped) return null;
  const sep = mapped.scope === 'instance' ? '#' : '.';
  return `${mapped.owner}${sep}${member}`;
}

// ---- 极简 YAML 子集解析：只认这份文档实际用到的形状 ----
//   added:
//     - pr-url: https://...
//       description: "..."
//   changes:
//     - pr-url: https://...
//       description: "..."
//       breaking-changes-header: xxx
//   deprecated:
//     - pr-url: https://...
// 不引入 yaml 依赖——jsbase 全程零运行时依赖，这个子集比通用 YAML 简单得多。
function parseYamlHistoryBlock(bodyLines) {
  const result = {};
  let currentSection = null;
  let currentItem = null;

  function setField(item, rawKey, rawValue) {
    let value = rawValue.trim();
    if (value.startsWith('"') && value.endsWith('"')) {
      value = value.slice(1, -1).replace(/\\"/g, '"');
    }
    const camelKey = rawKey.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
    item[camelKey] = value;
  }

  for (const line of bodyLines) {
    if (!line.trim()) continue;
    const topMatch = line.match(/^(added|changes|deprecated):\s*$/);
    if (topMatch) {
      currentSection = topMatch[1];
      result[currentSection] = [];
      currentItem = null;
      continue;
    }
    const itemMatch = line.match(/^ {2}- ([\w-]+):\s?(.*)$/);
    if (itemMatch && currentSection) {
      currentItem = {};
      result[currentSection].push(currentItem);
      setField(currentItem, itemMatch[1], itemMatch[2]);
      continue;
    }
    const fieldMatch = line.match(/^ {4}([\w-]+):\s?(.*)$/);
    if (fieldMatch && currentItem) {
      setField(currentItem, fieldMatch[1], fieldMatch[2]);
      continue;
    }
    // 认不出的行不吞掉信息，但也不强行解析——YAML 块的形状是摸实物摸出来的，
    // 出现新形状时宁可这条记录不完整，也不要猜错字段。
  }
  return result;
}

// 解析一份 docs/api/*.md 全文，返回 { resolved: [{key, history}], unresolved: [...] }。
function parseApiHistoryDoc(markdown, { prefixMap, wantedSet, file }) {
  const lines = markdown.split('\n');
  let moduleName = null;
  let currentClass = null;
  let lastHeadingTitle = null;

  const resolved = [];
  const unresolved = [];

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];

    const h1 = line.match(/^# (\S+)/);
    if (h1) {
      moduleName = h1[1].replace(/`/g, '');
      lastHeadingTitle = moduleName;
      continue;
    }
    const classHeading = line.match(/^## Class: (\S+)/);
    if (classHeading) {
      currentClass = classHeading[1].replace(/`/g, '');
      lastHeadingTitle = line.replace(/^#+\s*/, '');
      continue;
    }
    const headingMatch = line.match(/^#{2,6}\s+(.+)$/);
    if (headingMatch) {
      lastHeadingTitle = headingMatch[1].trim();
      continue;
    }

    if (line.trim() === '```YAML history') {
      const bodyLines = [];
      let j = i + 1;
      while (j < lines.length && lines[j].trim() !== '```') {
        bodyLines.push(lines[j]);
        j += 1;
      }
      i = j; // 跳过整个块

      const history = parseYamlHistoryBlock(bodyLines);
      const owner = currentClass || moduleName;
      const key = lastHeadingTitle
        ? resolveKey(lastHeadingTitle, { owner, prefixMap, wantedSet })
        : null;

      if (key) {
        resolved.push({ key, history });
      } else {
        // 紧跟在文档 h1（模块名）后面、还没进入任何 ### 成员标题的块，是在
        // 说"这个模块整体"的变化（如 contextBridge.md 开头那条），不是某个
        // 成员——这类块本来就不在 `module.member` 的 key 命名空间里，标成
        // module-level 而不是笼统的"解析不出来"，方便看统计时区分。
        const reason = lastHeadingTitle === moduleName ? 'module-level' : 'unrecognized-heading';
        unresolved.push({ file, title: lastHeadingTitle, owner, reason });
      }
    }
  }

  return { resolved, unresolved };
}

module.exports = {
  buildPrefixMap,
  parseYamlHistoryBlock,
  parseApiHistoryDoc,
  resolveKey,
  extractCodeSpan,
  extractEventName,
};
