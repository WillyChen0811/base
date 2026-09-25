r"""Word→PDF 转换服务端 —— 仅 Windows + Office 2019 能真正跑起来。

只依赖标准库 + pywin32（`pip install pybase[office-server]`，pywin32 只在
`sys_platform == 'win32'` 时才会被装）。HTTP 用标准库 `http.server`，不引
FastAPI——这台机器只做一件事，没必要带一整套 web 框架。

**为什么不做成 Windows 服务**：Word COM 自动化在 Session 0 / 服务账号下会挂死
（微软自己的文档都写了不建议在服务里跑 Office 自动化）。部署上用"自动登录桌面
会话 + 计划任务在登录时启动"代替，本模块只管服务本身，不管这层。

**导入安全**：本文件顶层只 import 标准库，`win32com`/`pythoncom` 只在
`WordCOMConverter` 真正要用 Word 的方法里延迟 import——这样即使在 macOS/Linux
上，`import pybase.office.server` 也不会炸，方便非 Windows 平台跑单元测试
（用假 converter 替换掉真 Word 驱动那部分）。

## 架构

```
HTTP 线程（ThreadingHTTPServer，每请求一线程）
    -> OfficeService.submit()：入队，阻塞等这个任务的 threading.Event
                                队列满直接抛 ServiceBusyError（503），不进队

单一工作线程 "office-word"（整个进程只有这一个，串行处理，因为 Word 实例本身
不能并发喂多个文档）
    -> 从队列取任务，起一个看门狗 Timer（task_timeout_sec 后触发）
    -> converter.convert(docx_path, pdf_path, update_fields)
    -> 正常完成 / 抛异常 / 被看门狗打断，三条路径都在 finally 里：
       清工作目录、按情况重建 Word 实例、task.event.set() 唤醒等待的 HTTP 线程

看门狗（Timer 线程，每个任务一个）
    -> 到点了任务还没完成：标记 timed_out，调 converter.kill_current()
       （真实现里是 taskkill /F /PID 杀掉 WINWORD.EXE），立即 task.event.set()
       让 HTTP 线程先拿到 504 返回，不用等 worker 线程那边真正收尾
```

`WordConverter` 是这层编排与"怎么驱动 Word"之间的接口，测试用假实现替换，
覆盖队列满 / 超时重建 / COM 错误重建 / 临时文件清理，这些跟真 Word 在不在跑
无关，值得脱离 Windows 独立验证（见 `tests/test_office_server.py`）。
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import logging
import os
import queue
import shutil
import subprocess
import tempfile
import threading
import time
from abc import ABC, abstractmethod
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Optional, Tuple
from urllib.parse import parse_qs, quote, urlparse

logger = logging.getLogger(__name__)

# Word 导出为 PDF：ExportFormat 常量（wdExportFormatPDF）
WD_EXPORT_FORMAT_PDF = 17
# OptimizeFor：0=打印质量(wdExportOptimizeForPrint)，1=屏幕最小化(wdExportOptimizeForOnScreen)
WD_EXPORT_OPTIMIZE_FOR_PRINT = 0
# DisplayAlerts：0=不弹任何提示(wdAlertsNone)
WD_ALERTS_NONE = 0
# AutomationSecurity：3=强制禁用宏(msoAutomationSecurityForceDisable)
MSO_AUTOMATION_SECURITY_FORCE_DISABLE = 3
# ComputeStatistics 的统计项：2=页数(wdStatisticPages)
WD_STATISTIC_PAGES = 2
# Close(SaveChanges=...)：0=不保存(wdDoNotSaveChanges)
WD_DO_NOT_SAVE_CHANGES = 0


# ============================================================
# 服务内部异常（不是协议的一部分，protocol 错误码在 __init__.py 文档里）
# ============================================================

class ServiceBusyError(Exception):
    """队列已满，调用方应映射成 HTTP 503"""


class InternalTimeoutError(Exception):
    """看门狗理论上必定先触发；等到这个说明看门狗本身卡住了，是异常状态，映射成 500"""


# ============================================================
# 任务
# ============================================================

@dataclasses.dataclass
class _Task:
    docx_bytes: bytes
    update_fields: bool
    timeout_sec: float
    event: threading.Event = dataclasses.field(default_factory=threading.Event)
    timed_out: threading.Event = dataclasses.field(default_factory=threading.Event)
    result: Optional[bytes] = None
    page_count: Optional[int] = None
    # 转换过程中的非致命问题（TOC 更新失败等），原样带给客户端，不悄悄吞掉
    warnings: list = dataclasses.field(default_factory=list)
    # (错误码, 消息)；错误码取值见 __init__.py 文档：timeout / conversion_failed
    error: Optional[Tuple[str, str]] = None


_SENTINEL = object()  # 放进队列让工作线程退出


# ============================================================
# Word 驱动接口 —— 测试用假实现替换这一层
# ============================================================

class WordConverter(ABC):
    """"怎么把一份 docx 变成 pdf" 的接口。真实实现驱动 Word COM；测试用假实现。

    编排层（OfficeService）只通过这几个方法跟它打交道，不关心背后是不是真 Word——
    这样队列满、超时重建、COM 错误重建这些逻辑可以在没有 Word 的机器上测。
    """

    @abstractmethod
    def convert(self, docx_path: str, pdf_path: str, update_fields: bool) -> "Tuple[int, list]":
        """把 docx_path 转成 pdf_path，返回 (页数, warnings)。

        warnings 是转换过程中遇到的非致命问题（比如某个 TOC 更新失败、某段
        StoryRange 更新域失败）——这些不该被吞掉再悄悄吐出一份"看起来正常、
        其实目录页码是旧值"的 PDF，调用方（OfficeService）原样把它们带给
        客户端。没有问题就返回空列表。硬失败（转换本身做不下去）仍然是抛异常。
        """

    @abstractmethod
    def kill_current(self) -> None:
        """看门狗专用：强杀当前正卡在 convert() 里的那个实例。不抛异常（尽力而为）。"""

    @abstractmethod
    def restart(self) -> None:
        """重建一个干净实例。convert() 抛异常之后、或看门狗杀过之后都会调用一次。"""

    @abstractmethod
    def is_alive(self) -> bool:
        """给 /health 用：当前是否有一个可用的 Word 实例。"""

    def close(self) -> None:
        """服务整体关闭时调用一次，释放资源。默认调用 restart 逻辑的反面——不重建，只收尾。"""


class WordCOMConverter(WordConverter):
    """真实实现：driva Word 2019 通过 win32com 完成 docx -> pdf。

    只在 Windows 上能用——构造时才会真正 import win32com/pythoncom，方便这个类
    的*定义*在其它平台也能被 import（比如被类型检查/测试文件引用到，即使不实例化）。

    **单实例假设**：这个类的所有方法只应该被 `OfficeService` 的那一个工作线程
    调用（`kill_current` 例外，它从看门狗的 Timer 线程调用，但只是发一条
    taskkill，不碰 COM 对象本身，是安全的）。不要跨线程共享一个实例去并发调用
    convert()——Word 应用本身就不支持并发处理多个文档。
    """

    def __init__(
        self,
        export_format: int = WD_EXPORT_FORMAT_PDF,
        optimize_for: int = WD_EXPORT_OPTIMIZE_FOR_PRINT,
        create_bookmarks: bool = True,
        doc_structure_tags: bool = True,
        pid_dir: Optional[str] = None,
    ):
        self._export_format = export_format
        self._optimize_for = optimize_for
        self._create_bookmarks = create_bookmarks
        self._doc_structure_tags = doc_structure_tags

        self._word = None
        self._pid: Optional[int] = None
        self._pid_file: Optional[str] = None
        self._com_initialized = False

        self._pids = _PidTracker(pid_dir or os.path.join(tempfile.gettempdir(), "pybase_office_pids"))
        # 服务刚启动：上次可能异常退出留下的孤儿 WINWORD，先清掉。
        # 只清自己记录过的 PID，不会碰用户手工打开的 Word。
        self._pids.cleanup_orphans()

        self._start()

    # ---------- 生命周期 ----------

    def _com_init(self):
        """CoInitialize 要在实际使用 COM 的那根线程上调用一次；重复调用是安全的
        （pythoncom 内部按线程计数），所以每个会碰 COM 的方法入口都可以放心调。"""
        import pythoncom
        pythoncom.CoInitialize()
        self._com_initialized = True

    def _snapshot_winword_pids(self) -> set:
        """当前系统里所有 WINWORD.EXE 的 PID 集合。

        用 tasklist 而不是 psutil：pybase 不想给这一个功能多引一个依赖，
        tasklist 是 Windows 自带命令，CSV 输出解析起来也简单。
        """
        try:
            out = subprocess.run(
                ["tasklist", "/FI", "IMAGENAME eq WINWORD.EXE", "/FO", "CSV", "/NH"],
                capture_output=True, text=True, timeout=10,
            )
        except Exception:
            logger.exception("tasklist 查询 WINWORD 进程失败")
            return set()
        pids = set()
        for line in out.stdout.splitlines():
            parts = [p.strip('"') for p in line.strip().split('","')]
            if len(parts) >= 2:
                try:
                    pids.add(int(parts[1].strip('"')))
                except ValueError:
                    continue
        return pids

    def _start(self):
        """DispatchEx 新开一个独立 Word 实例（不复用已有的），记下它的 PID。

        **PID 怎么定位**：DispatchEx 本身不直接给 PID（返回的是 COM 接口），
        用"启动前后 WINWORD.EXE 进程快照差集"这个笨办法但可靠——比解析窗口句柄
        再 GetWindowThreadProcessId 简单，代价是有极小概率跟"同一时刻恰好有别的
        WINWORD.EXE 启动"撞上；这台机器专职做转换、没有别人会开 Word，这个假设
        成立。撞上时退化为"差集里不止一个 PID，取不到就置 None"，后续看门狗/
        重建那一路会因为 kill_current 是 no-op 而退化成"杀不掉、只能等下次重启"，
        不是致命故障，只是少一层保险，记日志说明。
        """
        self._com_init()
        import win32com.client

        before = self._snapshot_winword_pids()
        self._word = win32com.client.DispatchEx("Word.Application")
        # 触发一次真正的进程创建（DispatchEx 有时是懒启动），读一个属性强制它起来
        _ = self._word.Version
        after = self._snapshot_winword_pids()
        new_pids = after - before

        if len(new_pids) == 1:
            self._pid = new_pids.pop()
        else:
            self._pid = None
            logger.warning(
                "无法唯一定位新 WINWORD 实例的 PID（新增 %d 个候选），"
                "看门狗超时杀进程这一路会失效，只能靠下次重建兜底", len(new_pids),
            )

        if self._pid is not None:
            self._pid_file = self._pids.record(self._pid)

        self._apply_hygiene_settings()
        logger.info("Word 实例已启动 PID=%s", self._pid)

    def _apply_hygiene_settings(self):
        """Word 卫生设置：不弹窗、禁宏、关自动保存类后台动作。

        每一项单独 try/except——不同 Word 版本/语言 build 个别属性可能不存在，
        一项失败不该拖累其它项，也不该拖累启动本身。
        """
        w = self._word
        settings = [
            ("Visible", False),
            ("DisplayAlerts", WD_ALERTS_NONE),
            ("AutomationSecurity", MSO_AUTOMATION_SECURITY_FORCE_DISABLE),
            ("ScreenUpdating", False),
        ]
        for name, value in settings:
            try:
                setattr(w, name, value)
            except Exception as e:
                logger.debug("设置 Application.%s 失败（忽略）: %s", name, e)

        option_settings = [
            ("CreateBackup", False),        # 关闭生成 .wbk 备份
            ("SaveInterval", 0),             # 0 = 关闭定时自动恢复保存
            ("BackgroundSave", False),       # 关闭后台保存
            ("Pagination", False),           # 关闭后台实时重新分页，导出前手动 Repaginate 一次即可
            ("AllowReadingMode", False),
            ("DoNotPromptForConvert", True),
            ("ConfirmConversions", False),
        ]
        for name, value in option_settings:
            try:
                setattr(w.Options, name, value)
            except Exception as e:
                logger.debug("设置 Options.%s 失败（忽略）: %s", name, e)

    # ---------- 转换 ----------

    def convert(self, docx_path: str, pdf_path: str, update_fields: bool) -> "Tuple[int, list]":
        self._com_init()
        doc = self._word.Documents.Open(
            docx_path,
            ConfirmConversions=False,
            ReadOnly=True,
            AddToRecentFiles=False,
            Visible=False,
        )
        warnings: list = []
        try:
            if update_fields:
                warnings = self._update_fields(doc)
            doc.ExportAsFixedFormat(
                OutputFileName=pdf_path,
                ExportFormat=self._export_format,
                OpenAfterExport=False,
                OptimizeFor=self._optimize_for,
                CreateBookmarks=1 if self._create_bookmarks else 0,
                DocStructureTags=self._doc_structure_tags,
            )
            try:
                page_count = int(doc.ComputeStatistics(WD_STATISTIC_PAGES))
            except Exception:
                logger.exception("读取页数失败，回退为 0")
                page_count = 0
        finally:
            # 只读打开、绝不保存回原文件——原文件本来就是我们自己写到 work_dir
            # 的临时拷贝，但这里仍然显式 SaveChanges=0，不给 Word 任何写回的机会。
            try:
                doc.Close(SaveChanges=WD_DO_NOT_SAVE_CHANGES)
            except Exception:
                logger.exception("关闭文档失败（继续）")
        return page_count, warnings

    # NextStoryRange 链的保险上限：防的是"链意外成环"这种反常情况，不是正常
    # 文档会碰到的节数——一份检测报告顶多几十节，1000 步绰绰有余。
    _MAX_STORY_CHAIN = 1000

    def _update_fields(self, doc) -> list:
        """更新所有域 + 重新分页 + 更新目录/图目录（跑两遍）。返回 warnings 列表。

        这是本服务的核心价值之一：python-docx 生成端算不出真实页码（依赖 Word
        的字体度量、分页算法），必须靠 Word 实际排一遍版才知道目录该写哪一页。

        **顺序**：Fields.Update（各 StoryRange）-> Repaginate -> TOC/TOF
        Update -> Repaginate -> TOC/TOF Update 再来一遍。
        目录条目的页码依赖的是"最终排版结果"——在 Repaginate 之前去 Update
        目录，拿到的还是旧页码；而目录本身被撑长/缩短又会推挤/收缩后面正文
        的页码，所以第一遍目录更新完还要再 Repaginate、再更新一遍目录，这是
        Word 自己"两遍收敛"的惯例做法。报告类文档量级很小，这里不追求收敛到
        不动点，固定跑两遍足够。

        **StoryRanges 要跟 NextStoryRange 链，且不能按 StoryType 去重**：
        `doc.StoryRanges` 只是"每种 story 类型各给一个起始 Range"，多节文档
        （比如奇偶页页眉不同）的后续节挂在 `NextStoryRange` 链上——**同一条链
        上后面几节的 `StoryType` 跟第一节完全相同**。早先版本按 StoryType 去重
        来防死循环，第一次碰到同类型就 break，实际上是把要覆盖的多节页眉页脚
        整段跳过了。改成按链步数设硬上限（`_MAX_STORY_CHAIN`），不再看类型。

        **失败不再悄悄吞掉**：单个 story 的 Fields.Update / TOC / TOF /
        Repaginate 失败都记 warning 并收进返回值，调用方原样带给客户端——
        否则吐出的是一份目录页码是旧值的 PDF，看起来正常，其实是错的。
        """
        warnings: list = []

        def _update_story_fields():
            for story in doc.StoryRanges:
                rng = story
                steps = 0
                while rng is not None:
                    steps += 1
                    if steps > self._MAX_STORY_CHAIN:
                        msg = f"StoryRange 链超过 {self._MAX_STORY_CHAIN} 步，判定为异常成环，已中止跟踪"
                        warnings.append(msg)
                        logger.warning(msg)
                        break
                    try:
                        rng.Fields.Update()
                    except Exception as e:
                        msg = f"更新域失败 story_type={rng.StoryType}: {e}"
                        warnings.append(msg)
                        logger.warning(msg)
                    try:
                        rng = rng.NextStoryRange
                    except Exception:
                        rng = None

        def _repaginate():
            try:
                doc.Repaginate()
            except Exception as e:
                msg = f"Repaginate 失败: {e}"
                warnings.append(msg)
                logger.warning(msg)

        def _update_toc_tof():
            try:
                for i in range(1, doc.TablesOfContents.Count + 1):
                    doc.TablesOfContents(i).Update()
            except Exception as e:
                msg = f"更新目录失败: {e}"
                warnings.append(msg)
                logger.warning(msg)
            try:
                for i in range(1, doc.TablesOfFigures.Count + 1):
                    doc.TablesOfFigures(i).Update()
            except Exception as e:
                msg = f"更新图目录失败: {e}"
                warnings.append(msg)
                logger.warning(msg)

        _update_story_fields()
        _repaginate()
        _update_toc_tof()
        _repaginate()
        _update_toc_tof()

        return warnings

    # ---------- 维护 ----------

    def kill_current(self) -> None:
        """看门狗调用。从 Timer 线程跑，不碰 COM 对象本身，只发 taskkill，线程安全。"""
        if self._pid is None:
            logger.warning("没有记录到 PID，无法强杀，只能等下次 restart() 兜底")
            return
        try:
            subprocess.run(
                ["taskkill", "/F", "/PID", str(self._pid)],
                capture_output=True, timeout=10,
            )
            logger.warning("已强杀超时的 WINWORD 实例 PID=%s", self._pid)
        except Exception:
            logger.exception("taskkill 强杀 PID=%s 失败", self._pid)

    def restart(self) -> None:
        """重建实例：先礼后兵——Quit 一次，再兜底 taskkill，最后起新实例。"""
        try:
            if self._word is not None:
                self._word.Quit(SaveChanges=WD_DO_NOT_SAVE_CHANGES)
        except Exception:
            logger.debug("重建前 Quit 失败（大概率进程已经不在了，继续）")
        # 不管 Quit 是否成功都兜底杀一次：Quit 在文档没关干净时可能弹提示卡住，
        # 或者进程本来就是被看门狗判定为超时/失联的，Quit 本身也可能没反应。
        self.kill_current()
        if self._pid_file:
            self._pids.forget(self._pid_file)
        self._word = None
        self._pid = None
        self._pid_file = None
        self._start()

    def is_alive(self) -> bool:
        if self._word is None:
            return False
        try:
            _ = self._word.Visible
            return True
        except Exception:
            return False

    def close(self) -> None:
        try:
            if self._word is not None:
                self._word.Quit(SaveChanges=WD_DO_NOT_SAVE_CHANGES)
        except Exception:
            logger.debug("关闭时 Quit 失败（忽略）")
        self.kill_current()
        if self._pid_file:
            self._pids.forget(self._pid_file)
        self._word = None
        self._pid = None


class _PidTracker:
    """记录本服务自己开过的 WINWORD 实例 PID，跨重启也找得回、清得掉孤儿。

    **只清自己留下的**：PID 文件由本类在实例启动后写入，正常关闭/重建时删除。
    干净退出时目录是空的；只有异常退出（崩溃、被外部强杀）才会留下陈旧文件，
    下次启动据此定点 `taskkill /F /PID`，不会去扫描系统里所有 WINWORD 进程，
    不会误杀用户手工打开的 Word——这是需求里明确要求的边界。
    """

    def __init__(self, pid_dir: str):
        self._dir = pid_dir
        os.makedirs(self._dir, exist_ok=True)

    def cleanup_orphans(self) -> None:
        try:
            names = os.listdir(self._dir)
        except OSError:
            return
        for name in names:
            path = os.path.join(self._dir, name)
            try:
                with open(path, "r", encoding="utf-8") as f:
                    pid = int(f.read().strip())
                subprocess.run(["taskkill", "/F", "/PID", str(pid)], capture_output=True, timeout=10)
                logger.warning("清理上次异常退出遗留的孤儿 WINWORD 进程 PID=%s", pid)
            except Exception:
                pass
            finally:
                try:
                    os.remove(path)
                except OSError:
                    pass

    def record(self, pid: int) -> str:
        path = os.path.join(self._dir, f"{pid}.pid")
        with open(path, "w", encoding="utf-8") as f:
            f.write(str(pid))
        return path

    def forget(self, path: Optional[str]) -> None:
        if not path:
            return
        try:
            os.remove(path)
        except OSError:
            pass


# ============================================================
# 编排层：队列 + 串行工作线程 + 看门狗
# ============================================================

class OfficeService:
    """转换服务的核心编排，不碰 HTTP。

    只依赖 `WordConverter` 接口，方便脱离真 Word 单独测试队列满 / 超时重建 /
    COM 错误重建 / 临时文件清理这些跟"怎么驱动 Word"无关的编排逻辑。
    """

    def __init__(
        self,
        converter: WordConverter,
        work_dir: Optional[str] = None,
        work_dir_memory_backed: bool = False,
        queue_maxsize: int = 4,
        task_timeout_sec: float = 120.0,
        restart_after_n: int = 50,
    ):
        self._converter = converter
        # **不推断，只如实转述部署方的声明**：早先版本用 "work_dir is not None"
        # 猜是不是内存盘——传了个普通磁盘目录一样会报 True，这本身就是在"自称
        # 内存盘"，违背 pybase.ramdisk 的 memory_backed 如实上报原则。Windows
        # 上判断一个盘符是不是 ImDisk/内存盘没有可靠的通用办法，索性不猜：
        # 部署方通过 work_dir_memory_backed（CLI: --work-dir-is-ramdisk）显式
        # 声明，没声明就是 False，即使传了 work_dir 也一样。
        self._memory_backed = work_dir_memory_backed
        self._effective_work_dir = work_dir or tempfile.gettempdir()
        os.makedirs(self._effective_work_dir, exist_ok=True)

        self._queue: "queue.Queue" = queue.Queue(maxsize=queue_maxsize)
        self._task_timeout_sec = task_timeout_sec
        self._restart_after_n = max(1, restart_after_n)
        self._processed_since_restart = 0
        self._processed_total = 0
        self._last_error: Optional[str] = None

        self._thread = threading.Thread(target=self._worker_loop, name="office-word", daemon=True)
        self._thread.start()

    # ---------- 对外 ----------

    def submit(self, docx_bytes: bytes, update_fields: bool) -> _Task:
        """入队并等待这个任务处理完。队列满直接抛 ServiceBusyError，不占位等待。"""
        task = _Task(docx_bytes=docx_bytes, update_fields=update_fields, timeout_sec=self._task_timeout_sec)
        try:
            self._queue.put_nowait(task)
        except queue.Full:
            raise ServiceBusyError(f"转换队列已满（上限 {self._queue.maxsize}），请稍后重试")

        # 等待窗口比任务超时多留一截余量：看门狗到点必定会 set 这个 event，
        # 多出来的时间只是给"看门狗触发 -> worker 收尾"这段路留缓冲，正常
        # 不会被用满。真被用满说明看门狗本身出了问题，属于要报出来的异常状态。
        waited = task.event.wait(self._task_timeout_sec + 15.0)
        if not waited:
            raise InternalTimeoutError("转换任务未在预期时间内收到结果（看门狗未按预期触发，属于异常状态）")
        return task

    def health(self) -> dict:
        return {
            "word_alive": self._converter.is_alive(),
            "queue_len": self._queue.qsize(),
            "processed": self._processed_total,
            "last_error": self._last_error,
            "memory_backed": self._memory_backed,
            "work_dir": self._effective_work_dir,
        }

    def close(self) -> None:
        self._queue.put(_SENTINEL)
        self._thread.join(timeout=30)
        try:
            self._converter.close()
        except Exception:
            logger.exception("关闭 converter 失败")

    # ---------- 工作线程 ----------

    def _worker_loop(self):
        while True:
            task = self._queue.get()
            if task is _SENTINEL:
                return

            watchdog = threading.Timer(task.timeout_sec, self._on_timeout, args=(task,))
            watchdog.daemon = True
            watchdog.start()

            work_subdir = None
            try:
                work_subdir = tempfile.mkdtemp(dir=self._effective_work_dir, prefix="cvt-")
                docx_path = os.path.join(work_subdir, "input.docx")
                pdf_path = os.path.join(work_subdir, "output.pdf")
                with open(docx_path, "wb") as f:
                    f.write(task.docx_bytes)

                page_count, warnings = self._converter.convert(docx_path, pdf_path, task.update_fields)

                if not task.timed_out.is_set():
                    with open(pdf_path, "rb") as f:
                        task.result = f.read()
                    task.page_count = page_count
                    task.warnings = warnings
                # else: 看门狗已经判了超时并把 task.error 填好、task.event set 过了，
                # 这次"其实转完了"的结果不采信，避免跟已经发出去的 504 打架。
            except Exception as e:
                logger.exception("转换任务失败")
                if not task.timed_out.is_set():
                    self._last_error = str(e)
                    task.error = ("conversion_failed", str(e))
            finally:
                watchdog.cancel()

                if task.timed_out.is_set():
                    # 无论是看门狗杀的、还是杀之后 convert() 自己也报了错，Word
                    # 实例这时候大概率已经不可用，一律重建，不留给下一个任务用。
                    self._safe_restart("看门狗超时后")
                elif task.error is not None:
                    # 没超时，但 convert() 自己抛了异常（典型是 COM 错误）：
                    # 同样不信任这个实例的状态，重建后再接下一个任务。
                    self._safe_restart("转换出错后")

                # 用独立子目录 + 任务结束后整目录删除的方式清理：docx 原件、
                # 导出的 pdf、以及 Word 可能留下的 ~$ 锁文件，不用逐个认名字，
                # rmtree 一把全清，缺一漏一的风险最低。
                self._cleanup_dir(work_subdir)

                self._processed_since_restart += 1
                self._processed_total += 1
                if self._processed_since_restart >= self._restart_after_n:
                    self._safe_restart("达到处理数阈值后")
                    self._processed_since_restart = 0

                task.event.set()  # 幂等：看门狗可能已经 set 过一次

    def _on_timeout(self, task: _Task):
        if task.event.is_set():
            return  # 任务已经正常完成，看门狗白跑一趟，什么都不做
        task.timed_out.set()
        message = f"转换超时（超过 {task.timeout_sec:.0f} 秒），已强制终止并重建 Word 实例"
        task.error = ("timeout", message)
        self._last_error = message
        try:
            self._converter.kill_current()
        except Exception:
            logger.exception("看门狗强杀实例失败")
        task.event.set()  # 立即放行等待的 HTTP 线程，不用等 worker 线程收尾

    def _safe_restart(self, reason: str) -> None:
        try:
            self._converter.restart()
        except Exception as e:
            logger.exception("%s重建 Word 实例失败", reason)
            self._last_error = f"{reason}重建 Word 实例失败: {e}"

    def _cleanup_dir(self, path: Optional[str]) -> None:
        if not path:
            return
        try:
            shutil.rmtree(path, ignore_errors=True)
        except Exception:
            logger.warning("清理临时目录失败: %s", path)


# ============================================================
# HTTP 层
# ============================================================

@dataclasses.dataclass
class OfficeServerConfig:
    host: str = "0.0.0.0"
    port: int = 8765
    # 建议指向内存盘盘符（部署清单见 README）；不传则退化为系统临时目录。
    work_dir: Optional[str] = None
    # 是不是内存盘由部署方显式声明（CLI: --work-dir-is-ramdisk），不推断——
    # 见 OfficeService.__init__ 的注释。没声明就是 False，health() 如实报。
    work_dir_memory_backed: bool = False
    token: Optional[str] = None
    max_body_bytes: int = 100 * 1024 * 1024
    queue_maxsize: int = 4
    task_timeout_sec: float = 120.0
    restart_after_n: int = 50
    export_format: int = WD_EXPORT_FORMAT_PDF
    optimize_for: int = WD_EXPORT_OPTIMIZE_FOR_PRINT
    create_bookmarks: bool = True
    doc_structure_tags: bool = True


class OfficeRequestHandler(BaseHTTPRequestHandler):
    """薄薄一层协议翻译：HTTP <-> OfficeService。业务判断都在 OfficeService 里。"""

    server_version = "PybaseOffice/1.0"

    # service/config 通过 self.server（OfficeHTTPServer 实例）取，标准 http.server 用法

    def log_message(self, fmt, *args):
        logger.info("%s - %s", self.address_string(), fmt % args)

    @property
    def _service(self) -> OfficeService:
        return self.server.service  # type: ignore[attr-defined]

    @property
    def _config(self) -> OfficeServerConfig:
        return self.server.config  # type: ignore[attr-defined]

    def _write_json(self, status: int, payload: dict, extra_headers: Optional[dict] = None):
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        for k, v in (extra_headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def _write_error(self, status: int, code: str, message: str):
        self._write_json(status, {"error": code, "message": message})

    def _check_token(self) -> bool:
        if not self._config.token:
            return True
        return self.headers.get("X-Office-Token") == self._config.token

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/health":
            self._write_json(200, self._service.health())
        else:
            self._write_error(404, "not_found", f"未知路径: {parsed.path}")

    def do_POST(self):
        parsed = urlparse(self.path)
        if parsed.path != "/convert":
            self._write_error(404, "not_found", f"未知路径: {parsed.path}")
            return

        if not self._check_token():
            self._write_error(401, "unauthorized", "token 校验失败")
            return

        length_header = self.headers.get("Content-Length")
        try:
            content_length = int(length_header) if length_header is not None else 0
        except ValueError:
            self._write_error(400, "bad_request", "Content-Length 非法")
            return

        if content_length <= 0:
            self._write_error(400, "bad_request", "请求体为空")
            return

        if content_length > self._config.max_body_bytes:
            self._write_error(
                413, "too_large",
                f"请求体 {content_length} 字节超过服务端上限 {self._config.max_body_bytes} 字节",
            )
            # 上限之外的字节不读，直接关连接即可，标准库会在响应写完后处理连接收尾
            return

        docx_bytes = self.rfile.read(content_length)
        params = parse_qs(parsed.query)
        update_fields = params.get("update_fields", ["1"])[0] != "0"

        try:
            task = self._service.submit(docx_bytes, update_fields)
        except ServiceBusyError as e:
            self._write_error(503, "busy", str(e))
            return
        except InternalTimeoutError as e:
            self._write_error(500, "internal", str(e))
            return

        if task.error is not None:
            code, message = task.error
            status = 504 if code == "timeout" else 500
            self._write_error(status, code, message)
            return

        self.send_response(200)
        self.send_header("Content-Type", "application/pdf")
        self.send_header("Content-Length", str(len(task.result or b"")))
        self.send_header("X-Page-Count", str(task.page_count if task.page_count is not None else 0))
        if task.warnings:
            # 转换过程中的非致命问题（TOC 更新失败等）原样带给客户端，不悄悄
            # 吞掉——JSON 数组 + URL 编码，因为 HTTP 头只认 ASCII 且不能换行。
            self.send_header("X-Warnings", quote(json.dumps(task.warnings, ensure_ascii=False)))
        self.end_headers()
        self.wfile.write(task.result or b"")


class OfficeHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, server_address, service: OfficeService, config: OfficeServerConfig):
        self.service = service
        self.config = config
        super().__init__(server_address, OfficeRequestHandler)


# ============================================================
# 组装 + CLI 入口
# ============================================================

def create_service(config: OfficeServerConfig, converter: Optional[WordConverter] = None) -> OfficeService:
    """converter=None 时用真的 WordCOMConverter（只有 Windows 能成功）；
    测试传一个假 converter 进来，跳过真 Word。"""
    if converter is None:
        converter = WordCOMConverter(
            export_format=config.export_format,
            optimize_for=config.optimize_for,
            create_bookmarks=config.create_bookmarks,
            doc_structure_tags=config.doc_structure_tags,
        )
    return OfficeService(
        converter,
        work_dir=config.work_dir,
        work_dir_memory_backed=config.work_dir_memory_backed,
        queue_maxsize=config.queue_maxsize,
        task_timeout_sec=config.task_timeout_sec,
        restart_after_n=config.restart_after_n,
    )


def serve(config: OfficeServerConfig, converter: Optional[WordConverter] = None) -> None:
    service = create_service(config, converter)
    httpd = OfficeHTTPServer((config.host, config.port), service, config)
    logger.info(
        "pybase.office 服务已启动 %s:%s work_dir=%s memory_backed=%s token=%s",
        config.host, config.port, service._effective_work_dir, service._memory_backed,
        "已启用" if config.token else "未启用",
    )
    if not service._memory_backed:
        # 没声明 --work-dir-is-ramdisk 就一律如实警告，不管 work_dir 是不是
        # 传了自定义路径——传了路径不代表那是内存盘，见 OfficeService 的注释。
        logger.warning("工作目录未声明为内存盘，文档会短暂落到真实磁盘")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()
        service.close()


def _build_arg_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="pybase.office Word→PDF 转换服务（仅 Windows + Office，需要 pip install pybase[office-server]）",
    )
    p.add_argument("--host", default="0.0.0.0")
    p.add_argument("--port", type=int, default=8765)
    p.add_argument(
        "--work-dir", default=None,
        help="工作目录，建议指向内存盘盘符（如 R:\\）。不传则退化为系统临时目录",
    )
    p.add_argument(
        "--work-dir-is-ramdisk", action="store_true",
        help="显式声明 --work-dir 指向的是内存盘（如 ImDisk）。不加这个开关，"
             "/health 的 memory_backed 一律如实报 False，即使传了 --work-dir——"
             "Windows 上没有可靠的通用办法自动判断一个盘符是不是内存盘，只能靠部署方声明",
    )
    p.add_argument("--token", default=None, help="共享 token；不传则 /convert 不做鉴权")
    p.add_argument("--max-body-mb", type=int, default=100, help="请求体大小上限（MB），默认 100")
    p.add_argument("--queue-size", type=int, default=4, help="转换队列容量，满了直接 503")
    p.add_argument("--task-timeout", type=float, default=120.0, help="单任务超时（秒），超时后看门狗强杀重建")
    p.add_argument("--restart-after", type=int, default=50, help="处理满 N 个任务后主动重启 Word 实例")
    return p


def main(argv=None) -> None:
    args = _build_arg_parser().parse_args(argv)
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    config = OfficeServerConfig(
        host=args.host,
        port=args.port,
        work_dir=args.work_dir,
        work_dir_memory_backed=args.work_dir_is_ramdisk,
        token=args.token,
        max_body_bytes=args.max_body_mb * 1024 * 1024,
        queue_maxsize=args.queue_size,
        task_timeout_sec=args.task_timeout,
        restart_after_n=args.restart_after,
    )
    serve(config)


if __name__ == "__main__":
    main()
