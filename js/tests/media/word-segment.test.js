'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { segmentWords } = require('../../media/word-segment');

test('word-segment：中文按词切，不按字', () => {
  const segs = segmentWords('我喜欢猫', 'zh');
  assert.deepEqual(segs.map((s) => s.text), ['我', '喜欢', '猫']);
  assert.ok(segs.every((s) => s.isWord));
  // 偏移量拼回去要等于原文
  assert.equal(segs.map((s) => s.text).join(''), '我喜欢猫');
});

test('word-segment：日文假名/汉字分词', () => {
  const segs = segmentWords('私は猫が好き', 'ja');
  assert.deepEqual(segs.map((s) => s.text), ['私', 'は', '猫', 'が', '好き']);
  assert.ok(segs.every((s) => s.isWord));
});

test('word-segment：英文——缩写整体是一个词，连字符词被拆开，标点和空格非词', () => {
  const segs = segmentWords("I don't like take-off.", 'en');
  assert.deepEqual(segs, [
    { text: 'I', start: 0, end: 1, isWord: true },
    { text: ' ', start: 1, end: 2, isWord: false },
    { text: "don't", start: 2, end: 7, isWord: true },
    { text: ' ', start: 7, end: 8, isWord: false },
    { text: 'like', start: 8, end: 12, isWord: true },
    { text: ' ', start: 12, end: 13, isWord: false },
    { text: 'take', start: 13, end: 17, isWord: true },
    { text: '-', start: 17, end: 18, isWord: false },
    { text: 'off', start: 18, end: 21, isWord: true },
    { text: '.', start: 21, end: 22, isWord: false },
  ]);
});

test('word-segment：偏移量能原样拼回原文（可点元素定位靠它）', () => {
  const text = "I don't like take-off.";
  const segs = segmentWords(text, 'en');
  const rebuilt = segs.map((s) => text.slice(s.start, s.end)).join('');
  assert.equal(rebuilt, text);
});

test('word-segment：不传 locale 要报错，不写默认语言', () => {
  assert.throws(() => segmentWords('hello', undefined), TypeError);
});
