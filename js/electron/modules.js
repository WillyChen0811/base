'use strict';

// macClient 实际用到的 Electron 面。改这份清单就能增减快照收录的模块/类，
// 不要把名字散落进 extract.js 的提取逻辑里。
//
// electron-api.json 里 Module 与 Class 是两个独立条目（例如 `session` 模块
// 与 `Session` 类），所以同一个能力面常常要在这里写两行（如 session/Session、
// webContents/WebContents）。名字必须和 electron-api.json 里的 `name` 字段
// 完全一致（大小写敏感），提取时找不到就报错，不要静默跳过。
module.exports = [
  'app',
  'BrowserWindow',
  'CommandLine',
  'contextBridge',
  'desktopCapturer',
  'dialog',
  'globalShortcut',
  'ipcMain',
  'ipcRenderer',
  'screen',
  'session',
  'Session',
  'shell',
  'systemPreferences',
  'webContents',
  'WebContents',
  'WebRequest',
  'webUtils',
];
