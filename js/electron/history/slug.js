'use strict';

// GitHub 风格的 header anchor slug（github-slugger 同款规则），用来把
// breaking-changes.md 里的 `### 标题` 换算成锚点，好跟 API History YAML 里的
// `breaking-changes-header` 字段对上。规则是拿文档里的真实用例反推出来的
// （见 fetch.js 顶部注释里记的两个对照样本），不是凭空猜的：
//   1. 去掉 Markdown 的反引号/加粗等格式字符，只留纯文本
//   2. 转小写
//   3. 去掉不是字母数字空格连字符下划线的字符（标点、冒号等）
//   4. 空格换成连字符
// GitHub 对同一份文档里重复出现的 slug 会追加 `-1`、`-2`……本模块的
// `SlugTracker` 就是干这个的，按标题在文档里出现的先后顺序调用。
function slugify(text) {
  const plain = text
    // 反引号包代码、**加粗**、行内 HTML 都先剥掉，只留文字
    .replace(/`/g, '')
    .replace(/\*\*/g, '')
    .replace(/<[^>]+>/g, '');
  return plain
    .toLowerCase()
    .replace(/[^\w\- ]+/g, '')
    .trim()
    .replace(/ +/g, '-');
}

class SlugTracker {
  constructor() {
    this.seen = new Map();
  }

  // 传入原始标题文本，返回这一次出现该用的 slug（重复时自动带 -1/-2 后缀）。
  next(text) {
    const base = slugify(text);
    const count = this.seen.get(base) || 0;
    this.seen.set(base, count + 1);
    return count === 0 ? base : `${base}-${count}`;
  }
}

module.exports = { slugify, SlugTracker };
