"""pybase.office.word 测试：WordCOMConverter 的导入安全性与纯逻辑方法（无需 Word）。"""

import platform

import pytest

# ============================================================
# WordCOMConverter：只验证非 Windows 上的导入/构造安全性
# ============================================================

@pytest.mark.skipif(platform.system() == "Windows", reason="这条只验证非 Windows 平台的降级行为")
def test_word_com_converter_import_is_safe_on_non_windows():
    """模块顶层不该在非 Windows 平台 import 时就失败——真正依赖 pywin32 的地方
    延迟到构造/使用时才触发，这样测试文件本身可以放心 import WordCOMConverter。"""
    from pybase.office.word import WordCOMConverter  # noqa: F401 —— import 本身不该抛异常


@pytest.mark.skipif(platform.system() == "Windows", reason="真实 Word COM 驱动只能在 Windows 上跑")
def test_word_com_converter_construction_fails_without_pywin32():
    from pybase.office.word import WordCOMConverter

    with pytest.raises(Exception):
        WordCOMConverter()


# ============================================================
# WordCOMConverter._update_fields / convert：纯逻辑单元测试
#
# _update_fields 只在传入的 doc 对象上调用 StoryRanges / Fields.Update /
# NextStoryRange / TablesOfContents / TablesOfFigures / Repaginate 这几个
# COM 属性/方法，不碰 win32com 本身——用一组假的 Python 对象模拟这层接口，
# 就能在没有 Word、没有 pywin32 的机器上验证链路遍历、调用顺序、warnings
# 收集这几件事本身的逻辑对不对。用 object.__new__ 绕开 __init__（会触发真
# COM 初始化），只测被抽出来的这几个纯逻辑方法。
# ============================================================

class _FakeFields:
    def __init__(self, fail=False):
        self.fail = fail
        self.update_calls = 0

    def Update(self):
        self.update_calls += 1
        if self.fail:
            raise RuntimeError("模拟域更新失败")


class _FakeRange:
    """模拟 Word 的 Range：StoryType 相同也能串成链——这正是需求要验证的场景：
    同一条 NextStoryRange 链上，第 2 节及之后的 Range 和第 1 节 StoryType 完全
    相同，不能按类型去重来"防止重复处理"。"""

    def __init__(self, story_type, next_range=None, fail=False):
        self.StoryType = story_type
        self.Fields = _FakeFields(fail=fail)
        self.NextStoryRange = next_range


class _FakeUpdatable:
    def __init__(self, fail=False):
        self.fail = fail
        self.update_calls = 0

    def Update(self):
        self.update_calls += 1
        if self.fail:
            raise RuntimeError("模拟目录/图目录更新失败")


class _FakeCollection:
    """模拟 doc.TablesOfContents / doc.TablesOfFigures：1-based，Count + 可调用取项"""

    def __init__(self, entries):
        self._entries = list(entries)

    @property
    def Count(self):
        return len(self._entries)

    def __call__(self, i):
        return self._entries[i - 1]


class _FakeDoc:
    def __init__(self, story_ranges, toc_entries=None, tof_entries=None):
        self.StoryRanges = story_ranges
        self.TablesOfContents = _FakeCollection(toc_entries or [])
        self.TablesOfFigures = _FakeCollection(tof_entries or [])
        self.repaginate_calls = 0

    def Repaginate(self):
        self.repaginate_calls += 1


def _new_converter():
    """绕开 __init__（会触发 COM 初始化），只测 _update_fields / convert 这几个纯逻辑方法。"""
    from pybase.office.word import WordCOMConverter

    return object.__new__(WordCOMConverter)


def test_update_fields_follows_next_story_range_chain_across_same_type_sections():
    """回归用例：早先版本按 StoryType 去重防死循环，第一次碰到同类型的 Range
    就 break，把同一条链上第 2 节及之后的页眉/页脚整段跳过了。"""
    converter = _new_converter()
    section2_header = _FakeRange(story_type=6, next_range=None)
    section1_header = _FakeRange(story_type=6, next_range=section2_header)
    main_text = _FakeRange(story_type=1, next_range=None)
    doc = _FakeDoc(story_ranges=[main_text, section1_header])

    warnings = converter._update_fields(doc)

    assert main_text.Fields.update_calls == 1
    assert section1_header.Fields.update_calls == 1
    assert section2_header.Fields.update_calls == 1  # 关键：不能被 StoryType 去重跳过
    assert warnings == []


def test_update_fields_stops_on_cyclic_chain_and_warns():
    converter = _new_converter()
    a = _FakeRange(story_type=1)
    b = _FakeRange(story_type=1, next_range=a)
    a.NextStoryRange = b  # a -> b -> a 人为构造一个环
    doc = _FakeDoc(story_ranges=[a])

    warnings = converter._update_fields(doc)  # 不该死循环/挂住，测试本身能跑完就是通过的一半

    assert any("成环" in w for w in warnings)


def test_update_fields_calls_repaginate_and_toc_update_twice_in_order():
    converter = _new_converter()
    toc = _FakeUpdatable()
    tof = _FakeUpdatable()
    doc = _FakeDoc(story_ranges=[_FakeRange(story_type=1)], toc_entries=[toc], tof_entries=[tof])

    warnings = converter._update_fields(doc)

    # Fields -> Repaginate -> TOC/TOF -> Repaginate -> TOC/TOF：目录更新两遍，
    # 每遍之前都先 Repaginate 过；不能是"目录只更新一遍、排在 Repaginate 之前"。
    assert doc.repaginate_calls == 2
    assert toc.update_calls == 2
    assert tof.update_calls == 2
    assert warnings == []


def test_update_fields_collects_warnings_without_aborting_other_work():
    converter = _new_converter()
    failing_range = _FakeRange(story_type=1, fail=True)
    ok_range = _FakeRange(story_type=6)
    toc = _FakeUpdatable(fail=True)
    doc = _FakeDoc(story_ranges=[failing_range, ok_range], toc_entries=[toc])

    warnings = converter._update_fields(doc)

    assert ok_range.Fields.update_calls == 1  # 一个 story 更新失败不影响其它 story
    assert any("域" in w for w in warnings)
    assert any("目录" in w for w in warnings)


def test_convert_plumbs_page_count_and_warnings(monkeypatch):
    converter = _new_converter()
    converter._export_format = 17
    converter._optimize_for = 0
    converter._create_bookmarks = True
    converter._doc_structure_tags = True
    monkeypatch.setattr(converter, "_com_init", lambda: None)  # 跳过真实 COM 初始化

    doc = _FakeDoc(story_ranges=[_FakeRange(story_type=1, fail=True)])
    doc.ExportAsFixedFormat = lambda **kw: None
    doc.Close = lambda **kw: None
    doc.ComputeStatistics = lambda stat: 4

    class _FakeDocuments:
        def Open(self, *args, **kwargs):
            return doc

    class _FakeWordApp:
        Documents = _FakeDocuments()

    converter._word = _FakeWordApp()

    page_count, warnings = converter.convert("in.docx", "out.pdf", update_fields=True)

    assert page_count == 4
    assert any("域" in w for w in warnings)
