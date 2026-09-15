'use strict';

/**
 * IPC 路由表的统一注册：把 `{ 频道: (目标, args) => ... }` 接到
 * `ipcMain.handle`，异常一律变成 `{ success: false, message }`。
 *
 * 异常要在这里收住：抛回渲染进程只会变成一句 “Error invoking remote
 * method”，看不出真实原因。
 *
 * 返回值走一次 JSON 往返：IPC 只收得下可结构化克隆的东西，路由里随手带上
 * 一个类实例（录制器、窗口）会在序列化时才炸，而且错误与真实原因相隔很远。
 *
 * `after(id)` 是注册方的收尾钩子。它在路由成功返回后、序列化返回值前执行；
 * 路由失败时不执行，钩子自身失败也按本次 IPC 调用失败处理。
 */
function registerRoutes(ipcMain, routes, targetOf, after) {
  for (const [channel, fn] of Object.entries(routes)) {
    ipcMain.handle(channel, async (event, args) => {
      try {
        const result = await fn(targetOf(args?.id), args);
        if (after) after(args?.id);
        return { success: true, ...JSON.parse(JSON.stringify(result || {})) };
      } catch (e) {
        return { success: false, message: e.message };
      }
    });
  }
}

module.exports = { registerRoutes };
