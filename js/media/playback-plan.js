'use strict';

/**
 * 播放计划：纯函数，不碰 DOM、不持有定时器。
 *
 * 思路借鉴 asbplayer 的"播放计划 + 执行器"拆法（只借思路，未看过它的代码，
 * 它是 AGPL-3.0）：这一层只回答"给定当前时间和状态，下一步该干什么"，
 * 谁来定时调用、谁来真正执行 pause/seek，是 video-controller.js 的事。
 *
 * 时间单位统一整数毫秒，跟 pybase.subtitle 的 Cue{start,end,text} 对齐；
 * `<video>.currentTime` 是秒，换算在 video-controller.js 里做，这里不碰。
 *
 * cues 约定按 start 升序排列，允许重叠（多语字幕、注释轨都可能重叠）。
 */

/**
 * 找出 timeMs 时刻"生效"的字幕下标：start <= timeMs < end。
 * 重叠时取 start 最小的一条（cues 已按 start 排序，第一个命中的就是它）。
 * @returns {number} 找不到返回 -1
 */
function findActiveCueIndex(cues, timeMs) {
  for (let i = 0; i < cues.length; i += 1) {
    const c = cues[i];
    if (c.start <= timeMs && timeMs < c.end) return i;
  }
  return -1;
}

/** timeMs 及之前，最后结束的那条字幕的下标（end <= timeMs 里 end 最大的一条） */
function findPrecedingCueIndex(cues, timeMs) {
  let best = -1;
  for (let i = 0; i < cues.length; i += 1) {
    if (cues[i].end <= timeMs) {
      if (best === -1 || cues[i].end > cues[best].end) best = i;
    }
  }
  return best;
}

/** timeMs 之后，最早开始的那条字幕的下标（start > timeMs 里 start 最小的一条） */
function findNextCueIndex(cues, timeMs) {
  for (let i = 0; i < cues.length; i += 1) {
    if (cues[i].start > timeMs) return i;
  }
  return -1;
}

/**
 * 计算下一步动作。每次时间推进（一帧、一次 timeupdate）调用一次，
 * 只看这一次传入的区间 `[prevMs, nowMs]` 和当前模式，不在内部攒状态。
 *
 * @param {Array<{start:number,end:number,text:string}>} cues 按 start 升序，可能重叠
 * @param {Object} params
 * @param {'sentence'|'repeat-one'|'skip-blank'|'ab-repeat'} params.mode
 * @param {number} params.prevMs 上一次检查的时间（毫秒）；首次调用可以等于 nowMs
 * @param {number} params.nowMs 这一次检查的时间（毫秒），nowMs >= prevMs
 * @param {Object} [params.options]
 * @param {number} [params.options.cueIndex] repeat-one 用：要循环的句子在 cues 里的下标
 * @param {{startMs:number,endMs:number}} [params.options.range] ab-repeat 用：循环区间
 * @param {number} [params.options.minGapMs] skip-blank 用：空档超过它才跳，调用方必须传
 * @returns {
 *   {action:'none'} |
 *   {action:'pause', atMs:number, cueIndex:number} |
 *   {action:'seek', toMs:number, reason:string}
 * }
 */
function nextAction(cues, { mode, prevMs, nowMs, options = {} }) {
  switch (mode) {
    case 'sentence': {
      // 逐句自动暂停：上一刻还在某句字幕里，这一刻已经越过它的句末，就在句末暂停。
      // 用区间穿越判断而不是只看 nowMs：只看瞬时值分不清"刚到句末"和"已经过
      // 了句末又被恢复播放"，会在同一个点反复暂停。
      const prevCue = findActiveCueIndex(cues, prevMs);
      if (prevCue === -1) return { action: 'none' };
      const cue = cues[prevCue];
      if (prevMs < cue.end && nowMs >= cue.end) {
        return { action: 'pause', atMs: cue.end, cueIndex: prevCue };
      }
      return { action: 'none' };
    }

    case 'repeat-one': {
      // 单句循环：目标句子由调用方指定（用户点了哪一句就循环哪一句），
      // 不是"当前播放到哪句就循环哪句"——用户可能正要跳到别处。
      const idx = options.cueIndex;
      if (idx == null || !cues[idx]) return { action: 'none' };
      const cue = cues[idx];
      // 同样按区间穿越判断：只看 nowMs >= end 的话，用户拖到后面去会被立刻拽回句首
      if (prevMs < cue.end && nowMs >= cue.end) {
        return { action: 'seek', toMs: cue.start, reason: 'repeat-one' };
      }
      return { action: 'none' };
    }

    case 'ab-repeat': {
      const range = options.range;
      if (!range) return { action: 'none' };
      if (prevMs < range.endMs && nowMs >= range.endMs) {
        return { action: 'seek', toMs: range.startMs, reason: 'ab-repeat' };
      }
      return { action: 'none' };
    }

    case 'skip-blank': {
      const minGapMs = options.minGapMs;
      if (minGapMs == null) return { action: 'none' };
      if (findActiveCueIndex(cues, nowMs) !== -1) return { action: 'none' };
      const nextIdx = findNextCueIndex(cues, nowMs);
      if (nextIdx === -1) return { action: 'none' };
      // 空档大小按"上一句结束"到"下一句开始"算整段的，不是按"现在离下一句
      // 还有多远"算——否则用户手动暂停在空档中间再续播，剩余部分不够
      // minGapMs 就会漏跳，明明这是一段该跳过的空白。
      const precedingIdx = findPrecedingCueIndex(cues, nowMs);
      const gapStart = precedingIdx === -1 ? 0 : cues[precedingIdx].end;
      const gap = cues[nextIdx].start - gapStart;
      if (gap >= minGapMs) {
        return { action: 'seek', toMs: cues[nextIdx].start, reason: 'skip-blank' };
      }
      return { action: 'none' };
    }

    default:
      throw new TypeError(`未知的播放模式：${mode}`);
  }
}

module.exports = {
  findActiveCueIndex,
  findPrecedingCueIndex,
  findNextCueIndex,
  nextAction,
};
