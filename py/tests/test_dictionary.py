"""覆盖 pybase.dictionary：统一模型 + macOS 系统词典适配器。

非 macOS 平台整体跳过依赖真实系统词典的用例（`available()` 恒为 False）；
纯 Python 的 `parse_headword` 解析、私有 API 缺失时的异常路径用固定
字符串/monkeypatch 测，不依赖平台。
"""
from __future__ import annotations

import sys

import pytest

from pybase.dictionary import Entry, macos

requires_macos_dictionary = pytest.mark.skipif(
    not macos.available(), reason="非 macOS，或本机加载不了系统词典框架"
)


# ---------------------------------------------------------------------------
# parse_headword：纯函数，固定字符串，不依赖平台
# ---------------------------------------------------------------------------


def test_parse_headword_pipe_format():
    text = "serendipity | BrE .../ AmE ... | noun formal ① uncountable ..."
    assert macos.parse_headword(text) == "serendipity"


def test_parse_headword_chinese_no_pipe():
    text = "苹果 píngguǒ 名 落叶乔木，叶子椭圆形。"
    assert macos.parse_headword(text) == "苹果"


def test_parse_headword_empty_text_is_none():
    assert macos.parse_headword("") is None


def test_parse_headword_no_recognizable_pattern_is_none():
    # 没有空白、没有 "|"，整段当第一个 token，长度超阈值就当解析不出。
    assert macos.parse_headword("a" * 100) is None


# ---------------------------------------------------------------------------
# _parse_html_title：纯函数
# ---------------------------------------------------------------------------


def test_parse_html_title_found():
    html = '<d:entry id="e1" d:title="serendipity" class="entry">...'
    assert macos._parse_html_title(html) == "serendipity"


def test_parse_html_title_missing():
    assert macos._parse_html_title("<html><body>no title attr</body></html>") is None


# ---------------------------------------------------------------------------
# available()
# ---------------------------------------------------------------------------


def test_available_false_on_non_darwin(monkeypatch):
    monkeypatch.setattr(sys, "platform", "linux")
    monkeypatch.setitem(macos._state, "loaded", False)
    assert macos.available() is False


# ---------------------------------------------------------------------------
# 私有 API 缺失：特性探测必须抛 DictionaryUnavailable，不能让进程崩
# ---------------------------------------------------------------------------


def test_bind_private_missing_symbol_raises_clear_error():
    class _FakeLib:
        """没有任何 DCS* 属性的假库，模拟符号不存在。"""

    with pytest.raises(macos.DictionaryUnavailable):
        macos._bind_private(_FakeLib())


def test_bind_private_records_missing_symbol_raises_clear_error():
    class _FakeLib:
        pass

    with pytest.raises(macos.DictionaryUnavailable):
        macos._bind_private_records(_FakeLib())


def test_list_dictionaries_propagates_private_api_failure(monkeypatch):
    def _boom(cs):
        raise macos.DictionaryUnavailable("模拟私有符号缺失")

    monkeypatch.setattr(macos, "_ensure_loaded", lambda: (object(), object()))
    monkeypatch.setattr(macos, "_bind_private", _boom)
    with pytest.raises(macos.DictionaryUnavailable):
        macos.list_dictionaries()


def test_lookup_with_dictionary_propagates_private_api_failure(monkeypatch):
    def _boom(cf, cs, name):
        raise macos.DictionaryUnavailable("模拟私有符号缺失")

    monkeypatch.setattr(macos, "_ensure_loaded", lambda: (object(), object()))
    monkeypatch.setattr(macos, "_dictionary", _boom)
    with pytest.raises(macos.DictionaryUnavailable):
        macos.lookup("apple", dictionary="不存在的词典")


def test_lookup_empty_term_raises_value_error():
    with pytest.raises(ValueError):
        macos.lookup("")


def test_lookup_html_requires_dictionary_name():
    with pytest.raises(ValueError):
        macos.lookup_html("apple", "")


# ---------------------------------------------------------------------------
# 真实系统词典：需要 macOS 且框架能加载
# ---------------------------------------------------------------------------


@requires_macos_dictionary
def test_list_dictionaries_non_empty():
    dicts = macos.list_dictionaries()
    assert len(dicts) > 0
    assert all(isinstance(d.name, str) and d.name for d in dicts)


@requires_macos_dictionary
def test_lookup_common_word_or_skip_if_no_enabled_dictionary():
    dicts = macos.list_dictionaries()
    if not any(d.installed for d in dicts):
        pytest.skip("本机没有任何已下载、可查的系统词典")
    entry = macos.lookup("hello")
    if entry is None:
        pytest.skip("已启用词典都查不到 hello（词库内容因人而异）")
    assert isinstance(entry, Entry)
    assert entry.query == "hello"
    assert entry.text
    assert entry.rights == macos.RIGHTS_NOTICE
    assert entry.dictionary is None


@requires_macos_dictionary
def test_lookup_gibberish_returns_none():
    assert macos.lookup("asdfghjkl123zzzqqq不存在的词xyz") is None


@requires_macos_dictionary
def test_lookup_unknown_dictionary_name_raises():
    with pytest.raises(macos.DictionaryUnavailable):
        macos.lookup("apple", dictionary="__一个肯定不存在的词典名__")


@requires_macos_dictionary
def test_lookup_html_unknown_dictionary_name_raises():
    with pytest.raises(macos.DictionaryUnavailable):
        macos.lookup_html("apple", "__一个肯定不存在的词典名__")
