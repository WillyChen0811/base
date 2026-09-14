'use strict';

// 递归按 key 排序后再 JSON.stringify，保证同样的数据两次跑出来逐字节相同——
// electron-api.json 本身的字段顺序在不同版本间会变，不排序的话快照没法 diff。
function sortKeysDeep(value) {
  if (Array.isArray(value)) {
    return value.map(sortKeysDeep);
  }
  if (value !== null && typeof value === 'object') {
    const sorted = {};
    for (const key of Object.keys(value).sort()) {
      sorted[key] = sortKeysDeep(value[key]);
    }
    return sorted;
  }
  return value;
}

// 固定两格缩进 + 结尾换行，避免不同编辑器/工具产出的空白差异污染 diff。
function stableStringify(value) {
  return JSON.stringify(sortKeysDeep(value), null, 2) + '\n';
}

module.exports = { sortKeysDeep, stableStringify };
