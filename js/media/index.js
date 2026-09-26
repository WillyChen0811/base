'use strict';

const { findActiveCueIndex, findPrecedingCueIndex, findNextCueIndex, nextAction } = require('./playback-plan');
const { VideoController } = require('./video-controller');
const { segmentWords } = require('./word-segment');
const {
  registerMediaScheme, handleMediaScheme, parseRange, buildRangeResponse, guessContentType,
} = require('./media-protocol');

module.exports = {
  // playback-plan：纯函数，给定 cues + 模式 + 当前时间，算下一步动作
  findActiveCueIndex,
  findPrecedingCueIndex,
  findNextCueIndex,
  nextAction,
  // video-controller：包 video 元素，驱动播放计划
  VideoController,
  // word-segment：一句字幕切成可点的词
  segmentWords,
  // media-protocol：Electron 本地媒体协议 + Range 解析
  registerMediaScheme,
  handleMediaScheme,
  parseRange,
  buildRangeResponse,
  guessContentType,
};
