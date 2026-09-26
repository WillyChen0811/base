'use strict';

const { EventEmitter } = require('node:events');
const { findActiveCueIndex, nextAction } = require('./playback-plan');

/**
 * 包一个注入进来的 video 元素，按 playback-plan 的判断去驱动它。
 *
 * `video` 是鸭子类型：真实用法传 `<video>`，测试里传一个手搭的假对象，只要有
 * `currentTime`（get/set，单位秒）、`paused`、`playbackRate`、`play()`、
 * `pause()`、`addEventListener`/`removeEventListener` 就够；`requestVideoFrameCallback`
 * /`cancelVideoFrameCallback` 有就用，没有就退化。
 *
 * ## 计时驱动的选择（构造时定一次，不动态切换）
 *
 * 1. `requestVideoFrameCallback`（Chromium 83+，暂停时不触发，解码到哪一帧就报
 *    哪一帧的 `mediaTime`，精度最高）；
 * 2. 没有就退到 `requestAnimationFrame`（通过 options 注入，不碰全局，浏览器里
 *    调用方传 `window.requestAnimationFrame`）轮询 `currentTime`；
 * 3. 都没有就退到 `timeupdate` 事件——每 150～250ms 才触发一次，逐句暂停会晚
 *    停一截，是没有前两者时的降级选择，不是首选。
 *
 * ## 暂停/跳转后重挂回调
 *
 * rVFC 和 rAF 在视频暂停时都不会再触发，循环会在检测到 `video.paused` 后自然
 * 断掉；恢复播放靠监听 video 自己的 `play` 事件重新排。`timeupdate` 不用重挂——
 * 监听器一直挂着，播放暂停/恢复它自己会跟着停/发。
 *
 * 外部（不经过这个类的 `seek()`）直接改了 `currentTime`，靠监听 video 的
 * `seeked` 事件把内部基准点拉回来；不然下一次判断"是否越过句末"会拿旧位置
 * 当作上一刻，逻辑就乱了。
 */
class VideoController extends EventEmitter {
  constructor(video, options = {}) {
    super();
    if (!video || typeof video.play !== 'function' || typeof video.pause !== 'function'
      || typeof video.addEventListener !== 'function' || typeof video.removeEventListener !== 'function') {
      throw new TypeError('VideoController 需要一个鸭子类型的 video 对象');
    }
    this.video = video;
    this.cues = options.cues || [];
    this.mode = options.mode || null;
    this.modeOptions = options.modeOptions || {};
    this._raf = options.requestAnimationFrame || null;
    this._caf = options.cancelAnimationFrame || null;
    this._rangeOnce = null;
    this._pendingHandle = null;
    this._disposed = false;
    this._currentCueIndex = -1;

    this._driver = typeof video.requestVideoFrameCallback === 'function'
      ? 'frame-callback'
      : (typeof this._raf === 'function' ? 'raf' : 'timeupdate');

    this._onPlay = () => this._scheduleNext();
    this._onSeeked = () => {
      const ms = this._readMs();
      this._lastMs = ms;
      this._updateCurrentCue(ms);
    };
    this._onTimeUpdate = () => {
      if (this._driver === 'timeupdate') this._tick(this._readMs());
    };
    video.addEventListener('play', this._onPlay);
    video.addEventListener('seeked', this._onSeeked);
    video.addEventListener('timeupdate', this._onTimeUpdate);

    this._lastMs = this._readMs();
    this._updateCurrentCue(this._lastMs);
    if (!video.paused) this._scheduleNext();
  }

  _readMs() {
    return Math.round((this.video.currentTime || 0) * 1000);
  }

  /** 排下一次回调；只在 frame-callback/raf 驱动下需要显式排，timeupdate 靠事件自己来 */
  _scheduleNext() {
    if (this._disposed) return;
    if (this.video.paused) return; // 等 play 事件重挂
    if (this._driver === 'frame-callback') {
      this._pendingHandle = this.video.requestVideoFrameCallback((_now, metadata) => {
        this._pendingHandle = null;
        const ms = metadata && typeof metadata.mediaTime === 'number'
          ? Math.round(metadata.mediaTime * 1000)
          : this._readMs();
        this._tick(ms);
      });
    } else if (this._driver === 'raf') {
      this._pendingHandle = this._raf(() => {
        this._pendingHandle = null;
        this._tick(this._readMs());
      });
    }
    // 'timeupdate' 驱动什么都不用排，监听器在构造时已经挂好。
  }

  _tick(nowMs) {
    if (this._disposed) return;
    const prevMs = this._lastMs;
    this._lastMs = nowMs;
    this._updateCurrentCue(nowMs);

    if (this._rangeOnce && nowMs >= this._rangeOnce.endMs) {
      // playRange 是"播一句"，到了终点就停，不循环——跟 ab-repeat 的循环是两回事。
      const { endMs, restore } = this._rangeOnce;
      this._rangeOnce = null;
      this.video.currentTime = endMs / 1000;
      this.video.pause();
      this._lastMs = endMs;
      if (restore) {
        this.mode = restore.mode;
        this.modeOptions = restore.options;
      }
    } else if (this.mode) {
      const action = nextAction(this.cues, {
        mode: this.mode, prevMs, nowMs, options: this.modeOptions,
      });
      this._applyAction(action);
    }

    this._scheduleNext();
  }

  _applyAction(action) {
    if (action.action === 'pause') {
      // 落到句末的精确毫秒上再暂停：帧回调本身有粒度，不补这一下会有零点几秒
      // 的超出，逐句复读时一句比一句晚一点点。
      this.video.currentTime = action.atMs / 1000;
      this.video.pause();
      this._lastMs = action.atMs;
      this.emit('boundary', { type: 'pause', atMs: action.atMs, cueIndex: action.cueIndex });
    } else if (action.action === 'seek') {
      this.video.currentTime = action.toMs / 1000;
      this._lastMs = action.toMs;
      this.emit('boundary', { type: 'seek', toMs: action.toMs, reason: action.reason });
    }
  }

  _updateCurrentCue(nowMs) {
    const idx = findActiveCueIndex(this.cues, nowMs);
    if (idx === this._currentCueIndex) return;
    this._currentCueIndex = idx;
    this.emit('cuechange', idx === -1 ? null : { index: idx, cue: this.cues[idx] });
  }

  /** 当前生效的字幕，{index, cue} 或 null */
  currentCue() {
    return this._currentCueIndex === -1
      ? null
      : { index: this._currentCueIndex, cue: this.cues[this._currentCueIndex] };
  }

  setCues(cues) {
    this.cues = cues || [];
    this._updateCurrentCue(this._readMs());
  }

  /** 切换播放模式；modeOptions 按 playback-plan.nextAction 的 options 格式给 */
  setMode(mode, modeOptions = {}) {
    this.mode = mode;
    this.modeOptions = modeOptions;
  }

  /** 跳转（毫秒）。跳转基准点在这里同步更新，不用等 seeked 事件回来。 */
  seek(ms) {
    this.video.currentTime = ms / 1000;
    this._lastMs = ms;
    this._updateCurrentCue(ms);
  }

  /**
   * 播一句：跳到 startMs 播放，到 endMs 就停，不循环，播完恢复之前的模式。
   * 用户点一句字幕单独听一遍是典型场景。
   */
  playRange(startMs, endMs) {
    this._rangeOnce = { endMs, restore: { mode: this.mode, options: this.modeOptions } };
    this.seek(startMs);
    this.video.play();
  }

  setRate(rate) {
    this.video.playbackRate = rate;
  }

  dispose() {
    if (this._disposed) return;
    this._disposed = true;
    if (this._pendingHandle != null) {
      if (this._driver === 'frame-callback' && typeof this.video.cancelVideoFrameCallback === 'function') {
        this.video.cancelVideoFrameCallback(this._pendingHandle);
      } else if (this._driver === 'raf' && typeof this._caf === 'function') {
        this._caf(this._pendingHandle);
      }
      this._pendingHandle = null;
    }
    this.video.removeEventListener('play', this._onPlay);
    this.video.removeEventListener('seeked', this._onSeeked);
    this.video.removeEventListener('timeupdate', this._onTimeUpdate);
    this.removeAllListeners();
  }
}

module.exports = { VideoController };
