'use strict';

/**
 * 把一句字幕切成可点的词片段，供界面把每个词包一层可点击元素、点一下查词典。
 *
 * 用内置 `Intl.Segmenter`（`granularity: 'word'`），不用维护分词表——中文/日文
 * 没有空格分词，全靠它；英文缩写、连字符词也交给它判断，不在这里猜规则。
 * `locale` 必须由调用方传（字幕是什么语言，调用方最清楚），这里不写死默认语言。
 *
 * 实测行为（写进测试用例）：
 * - 英文缩写 "don't" 整体是一个 word-like 片段，撇号不拆开；
 * - 连字符词 "take-off" 会拆成 "take" / "-" / "off" 三段，连字符自己是
 *   非 word-like 的一段；
 * - 标点、空格都是非 word-like 片段，原样保留在结果里（界面要照原文渲染，
 *   不能丢字符）。
 *
 * @param {string} text 一句字幕原文
 * @param {string} locale BCP 47 locale，如 'en'、'zh'、'ja'；调用方必须传
 * @returns {Array<{text:string, start:number, end:number, isWord:boolean}>}
 *   start/end 是 UTF-16 code unit 偏移（JS 字符串下标，可直接拼原文验证）
 */
function segmentWords(text, locale) {
  if (typeof text !== 'string') throw new TypeError('segmentWords 需要字符串 text');
  if (!locale) throw new TypeError('segmentWords 需要调用方传 locale，不写默认语言');

  const segmenter = new Intl.Segmenter(locale, { granularity: 'word' });
  const result = [];
  for (const seg of segmenter.segment(text)) {
    result.push({
      text: seg.segment,
      start: seg.index,
      end: seg.index + seg.segment.length,
      isWord: !!seg.isWordLike,
    });
  }
  return result;
}

module.exports = { segmentWords };
