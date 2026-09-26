'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { VideoController } = require('../../media/video-controller');

/**
 * 鸭子类型假 video：只实现 VideoController 用得到的那一小撮接口。
 * `withRVFC` 控制要不要模拟 requestVideoFrameCallback，用来分别测三条驱动路径。
 */
class FakeVideo {
  constructor({ withRVFC = true } = {}) {
    this.currentTime = 0; // 秒
    this.paused = true;
    this.playbackRate = 1;
    this._listeners = {};
    this._rvfcCb = null;
    this._handle = 0;
    if (withRVFC) {
      this.requestVideoFrameCallback = (cb) => {
        this._rvfcCb = cb;
        this._handle += 1;
        return this._handle;
      };
      this.cancelVideoFrameCallback = () => { this._rvfcCb = null; };
    }
  }

  addEventListener(type, fn) {
    (this._listeners[type] = this._listeners[type] || []).push(fn);
  }

  removeEventListener(type, fn) {
    this._listeners[type] = (this._listeners[type] || []).filter((f) => f !== fn);
  }

  _emit(type, ...args) {
    (this._listeners[type] || []).slice().forEach((f) => f(...args));
  }

  play() { this.paused = false; this._emit('play'); }

  pause() { this.paused = true; this._emit('pause'); }

  // 测试用：喂一帧，模拟浏览器解码到这个时间点才回调
  fireFrame(mediaTimeSec) {
    this.currentTime = mediaTimeSec;
    const cb = this._rvfcCb;
    this._rvfcCb = null;
    if (cb) cb(0, { mediaTime: mediaTimeSec });
  }

  fireSeeked(sec) {
    this.currentTime = sec;
    this._emit('seeked');
  }
}

const cues = [{ start: 1000, end: 2000, text: 'a' }, { start: 3000, end: 4000, text: 'b' }];

test('sentence 模式：越过句末暂停，落在精确毫秒，暂停后不再排下一帧', () => {
  const video = new FakeVideo();
  const c = new VideoController(video, { cues, mode: 'sentence' });
  const boundaries = [];
  c.on('boundary', (b) => boundaries.push(b));

  video.play();
  assert.ok(video._rvfcCb, 'play 后应该重新排 rVFC');

  video.fireFrame(1.5); // 句中，不动作
  assert.ok(video._rvfcCb, '还在播，继续排下一帧');

  video.fireFrame(2.1); // 越过句末
  assert.equal(video.paused, true);
  assert.equal(video.currentTime, 2); // 落在精确的句末，不是 2.1
  assert.deepEqual(boundaries, [{ type: 'pause', atMs: 2000, cueIndex: 0 }]);
  assert.equal(video._rvfcCb, null, '暂停状态不应该再排下一帧');

  c.dispose();
});

test('cuechange：只在当前生效字幕真正变化时发', () => {
  const video = new FakeVideo();
  const c = new VideoController(video, { cues, mode: null });
  const changes = [];
  c.on('cuechange', (info) => changes.push(info ? info.index : -1));

  video.play();
  video.fireFrame(1.5); // 进入第 0 句
  video.fireFrame(1.7); // 还在第 0 句，不应该重复发
  video.fireFrame(2.5); // 离开所有字幕
  video.fireFrame(3.5); // 进入第 1 句

  assert.deepEqual(changes, [0, -1, 1]);
  c.dispose();
});

test('repeat-one：到目标句末跳回句首，仍在播放', () => {
  const video = new FakeVideo();
  const c = new VideoController(video, { cues, mode: 'repeat-one', modeOptions: { cueIndex: 0 } });
  video.play();
  video.fireFrame(1.9);
  video.fireFrame(2.0);
  assert.equal(video.currentTime, 1); // 跳回句首 1000ms
  assert.equal(video.paused, false); // 循环不暂停
  assert.ok(video._rvfcCb, '还在播就该继续排下一帧');
  c.dispose();
});

test('playRange：播完停在终点，不循环，并恢复之前的模式', () => {
  const video = new FakeVideo();
  const c = new VideoController(video, { cues, mode: 'sentence' });
  c.playRange(1000, 1500);
  assert.equal(video.currentTime, 1); // seek 到起点
  assert.equal(video.paused, false); // 调了 play()

  video.fireFrame(1.6); // 越过 endMs
  assert.equal(video.paused, true);
  assert.equal(video.currentTime, 1.5);
  assert.equal(c.mode, 'sentence'); // 恢复原模式

  c.dispose();
});

test('seek()：立即更新基准点与当前字幕，不用等 seeked 事件', () => {
  const video = new FakeVideo();
  const c = new VideoController(video, { cues, mode: null });
  const changes = [];
  c.on('cuechange', (info) => changes.push(info && info.index));
  c.seek(3500);
  assert.equal(video.currentTime, 3.5);
  assert.deepEqual(changes, [1]);
  assert.deepEqual(c.currentCue(), { index: 1, cue: cues[1] });
  c.dispose();
});

test('外部直接改 currentTime 触发 seeked：基准点被拉回来，不会用旧位置误判', () => {
  const video = new FakeVideo();
  const c = new VideoController(video, { cues, mode: 'sentence' });
  video.play();
  video.fireFrame(1.5); // 进入第 0 句，_lastMs=1500
  // 外部代码直接把进度拖到第 1 句中间，绕开了 c.seek()
  video.fireSeeked(3.5);
  // 再来一帧，若基准点没被纠正，prevMs 仍是 1500，会被当成"还在第 0 句"
  video.fireFrame(3.6);
  // 3.6s 明明在第 1 句(3000-4000)里，不应该触发第 0 句的暂停逻辑
  assert.equal(video.paused, false);
  c.dispose();
});

test('setRate / setCues / setMode', () => {
  const video = new FakeVideo();
  const c = new VideoController(video, { cues, mode: 'sentence' });
  c.setRate(1.5);
  assert.equal(video.playbackRate, 1.5);
  c.setCues([{ start: 0, end: 100, text: 'x' }]);
  assert.equal(c.cues.length, 1);
  c.setMode('ab-repeat', { range: { startMs: 0, endMs: 50 } });
  assert.equal(c.mode, 'ab-repeat');
  c.dispose();
});

test('raf 兜底：没有 requestVideoFrameCallback 时用注入的 requestAnimationFrame', () => {
  const video = new FakeVideo({ withRVFC: false });
  let rafCb = null;
  const c = new VideoController(video, {
    cues, mode: 'sentence', requestAnimationFrame: (cb) => { rafCb = cb; return 1; },
  });
  video.play();
  assert.ok(rafCb, 'play 后应该用注入的 rAF 排下一帧');

  // 先落一帧到句中，建立基准点，再跳到越过句末——跟 rVFC 那条路一样，
  // 判断"是否越过"靠前后两次的时间区间，不能只看瞬时值。
  video.currentTime = 1.5;
  let fn = rafCb; rafCb = null; fn();
  assert.ok(rafCb, '还在播，继续排下一帧');

  video.currentTime = 2.1;
  fn = rafCb; rafCb = null; fn();
  assert.equal(video.paused, true);
  assert.equal(video.currentTime, 2); // 句末暂停逻辑同样生效
  c.dispose();
});

test('timeupdate 兜底：既没有 rVFC 也没注入 rAF，靠 timeupdate 事件驱动', () => {
  const video = new FakeVideo({ withRVFC: false });
  const c = new VideoController(video, { cues, mode: 'sentence' });
  video.play();
  video.currentTime = 1.5;
  video._emit('timeupdate');
  video.currentTime = 2.1;
  video._emit('timeupdate');
  assert.equal(video.paused, true);
  assert.equal(video.currentTime, 2);
  c.dispose();
});

test('dispose：清掉监听器，之后的事件不再驱动任何逻辑', () => {
  const video = new FakeVideo();
  const c = new VideoController(video, { cues, mode: 'sentence' });
  video.play();
  c.dispose();
  assert.equal(video._rvfcCb, null, 'dispose 要取消挂起的 rVFC');
  // dispose 之后再喂帧不应该抛错，也不应该再产生动作
  assert.doesNotThrow(() => video._emit('timeupdate'));
});
