"""Word COM 驱动：docx -> pdf（仅 Windows + Office，需 `pip install pybase[office-word]`）。

`WordConverter` 是"怎么把 docx 变成 pdf"的接口，调用方（如转换服务的队列编排）
只依赖它，测试可用假实现替换。`WordCOMConverter` 是驱动 Word 2019 的真实实现。

**导入安全**：顶层只 import 标准库，`win32com`/`pythoncom` 在方法里延迟 import，
非 Windows 平台也能 import 本模块跑测试。
"""

from __future__ import annotations

import logging
import os
import subprocess
import tempfile
import time
from abc import ABC, abstractmethod
from typing import Optional, Tuple

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
# Word 驱动接口 —— 测试用假实现替换这一层
# ============================================================

class WordConverter(ABC):
    """"怎么把一份 docx 变成 pdf" 的接口。真实实现驱动 Word COM；测试用假实现。

    调用方（如转换服务的队列编排）只通过这几个方法跟它打交道，不关心背后是不是真 Word——
    这样队列满、超时重建、COM 错误重建这些逻辑可以在没有 Word 的机器上测。
    """

    @abstractmethod
    def convert(self, docx_path: str, pdf_path: str, update_fields: bool) -> "Tuple[int, list]":
        """把 docx_path 转成 pdf_path，返回 (页数, warnings)。

        warnings 是转换过程中的非致命问题，不吞掉、原样带给客户端（细节见
        `WordCOMConverter._update_fields`）；没问题就返回空列表。硬失败（转换
        本身做不下去）仍然是抛异常。
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

    **单实例假设**：这个类的所有方法只应该被同一个工作线程
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
