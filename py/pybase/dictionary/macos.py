"""macOS 系统词典适配器：查用户在『词典』App 里启用/下载的词典。

数据版权属于苹果或词典出版方，随 macOS 系统授权给本机登录用户使用，
仅限个人查阅，不得导出或分发（见每条 `Entry.rights`）。base 不内置任何
词典数据。

只用标准库 `ctypes` 加载 `CoreFoundation`/`CoreServices` 里的
`Dictionary Services` 符号，不引入 pyobjc。公开文档只记录了
`DCSCopyTextDefinition` 一个函数（Apple Developer: Dictionary Services
Reference）；列词典、按词典查询、取 HTML 正文用的
`DCSCopyAvailableDictionaries`/`DCSDictionaryGetName`/`DCSDictionaryGetURL`/
`DCSCopyRecordsForSearchString`/`DCSRecordCopyData` 都是私有符号，没有官方
签名保证，随时可能在系统更新后消失——本文件对私有符号一律做特性探测
（符号不存在或调用失败就抛 `DictionaryUnavailable`，绝不让进程崩），公开
API 路径（不指定 `dictionary` 的 `lookup`）不依赖任何私有符号。私有符号的
名字和大致用法参考了公开的 Objective-C 逆向笔记，未复制任何 GPL/AGPL
项目代码，具体签名以本机 ctypes 探测结果为准。

各函数的内存管理都遵循 CoreFoundation 的 Copy/Create 规则：函数名带
Copy/Create 的返回值调用方负责 `CFRelease`，带 Get 的不用。
"""
from __future__ import annotations

import ctypes
import ctypes.util
import re
import sys
import threading
from contextlib import contextmanager
from dataclasses import dataclass, replace

from .models import Entry

RIGHTS_NOTICE = "随系统授权，仅限本机个人使用，不得导出或分发"

_UTF8 = 0x08000100  # kCFStringEncodingUTF8，CoreFoundation 常量，稳定不变
# DCSRecordCopyData 的第二个参数：0 = XHTML（3 是纯文本）。取值见 DictionaryKit
# （mattt/DictionaryKit，MIT）的 TTTDictionaryVersionHTML
_RECORD_VERSION_HTML = 0

_lock = threading.Lock()
# 框架句柄只加载一次；_loaded 标记"试过了"（不管成没成功），避免重复尝试
# LoadLibrary（失败也有开销，且不该每次调用都重新报错）。
_state: dict = {"loaded": False, "ok": False, "cf": None, "cs": None}


class DictionaryUnavailable(Exception):
    """私有 API 用不了：符号不存在、调用失败，或指定的词典名找不到。"""


class CFRange(ctypes.Structure):
    _fields_ = [("location", ctypes.c_long), ("length", ctypes.c_long)]


@dataclass(frozen=True)
class DictionaryInfo:
    """一本系统词典的概要（来自 `list_dictionaries`）。"""

    name: str
    # 是否已下载、可查。判据是私有 API `DCSDictionaryGetURL` 返回非空——
    # 本机验证：9 本已下载词典该值都是文件 URL，其余未下载的都是 NULL，
    # 且与"能否查到结果"完全对应（未下载的词典查询恒返回 NULL，不报错）。
    installed: bool


def available() -> bool:
    """是否 macOS 且能加载 CoreFoundation/CoreServices（公开 API 的前提）。

    只检查公开 API 依赖的框架能不能加载，不检查私有符号——私有符号缺失
    只影响 `lookup(dictionary=...)`/`list_dictionaries`/`lookup_html`，会在
    调用那些函数时抛 `DictionaryUnavailable`。
    """
    if sys.platform != "darwin":
        return False
    try:
        _ensure_loaded()
    except DictionaryUnavailable:
        return False
    return True


def lookup(term: str, *, dictionary: str | None = None) -> Entry | None:
    """查一个词，返回 `Entry`；查不到返回 `None`。

    不给 `dictionary`：走公开 API `DCSCopyTextDefinition(NULL, ...)`，查的
    是用户在『词典』App 里启用的词典中第一个命中的——系统不告诉调用方
    具体命中了哪本，所以返回的 `Entry.dictionary` 是 `None`。

    给 `dictionary`（词典的显示名，如 "牛津英汉汉英词典"）：走私有 API
    按名字找到该词典再查。这本词典必须在 `list_dictionaries()` 的结果里
    （不区分是否已下载）；不在列表里会抛 `DictionaryUnavailable`。在列表
    里但没下载（不能查）的话，跟查不到一样返回 `None`，不算异常。

    注意：公开 API 对短语是模糊匹配，返回的词头可能和查询词不同（比如查
    "take off" 命中的是名词词条 "take-off"），`Entry.headword` 会反映实际
    命中的词头。
    """
    if not term:
        raise ValueError("term 不能为空")
    cf, cs = _ensure_loaded()
    if dictionary is None:
        text = _copy_text_definition(cf, cs, None, term)
        source = "macOS 词典（未指定具体词典，取已启用词典里第一个命中的）"
    else:
        with _dictionary(cf, cs, dictionary) as dict_ref:
            text = _copy_text_definition(cf, cs, dict_ref, term)
        source = f"macOS 词典：{dictionary}"
    if text is None:
        return None
    return Entry(
        query=term,
        text=text,
        source=source,
        rights=RIGHTS_NOTICE,
        headword=parse_headword(text),
        dictionary=dictionary,
    )


def lookup_html(term: str, dictionary: str) -> Entry | None:
    """按指定词典查词，在 `lookup` 的基础上补一份 HTML 正文。

    走私有 API `DCSCopyRecordsForSearchString` + `DCSRecordCopyData`，本机
    在已下载的英语词典（New Oxford American Dictionary、Oxford Dictionary
    of English）上验证稳定：命中的 HTML 里带 `<span class="x_xoh">ORIGIN`
    等结构化词源信息，比纯文本更适合展示；中文类词典（牛津英汉汉英、
    成语词典等）本机也验证可查，但未见到词源标记，是否所有词典都带取决
    于该词典本身排版，不保证。

    只取第一条命中记录（一个词可能有多个义项/词性，各是一条独立记录）。
    HTML 里 `d:title` 属性给出的词头是词典自己标注的，比 `parse_headword`
    的文本启发式更可靠，能解析到就优先用它覆盖 `Entry.headword`。

    私有 records API 没命中（但公开/按词典查询命中了）时不算失败，退化
    成不带 `html` 的普通 `Entry`；两边都没命中才返回 `None`。
    """
    if not dictionary:
        raise ValueError("dictionary 不能为空——HTML 查询必须指定具体词典")
    entry = lookup(term, dictionary=dictionary)
    if entry is None:
        return None
    cf, cs = _ensure_loaded()
    with _dictionary(cf, cs, dictionary) as dict_ref:
        html = _copy_html_record(cf, cs, dict_ref, term)
    if html is None:
        return entry
    headword = _parse_html_title(html) or entry.headword
    return replace(entry, html=html, headword=headword)


def list_dictionaries() -> list[DictionaryInfo]:
    """列出本机『词典』App 里注册的所有词典，含未下载、查不了的。

    调用私有 API，符号不存在或调用失败会抛 `DictionaryUnavailable`。
    """
    cf, cs = _ensure_loaded()
    _bind_private(cs)
    dict_set = cs.DCSCopyAvailableDictionaries()
    if not dict_set:
        raise DictionaryUnavailable("DCSCopyAvailableDictionaries 返回空")
    try:
        refs = _cfset_to_list(cf, dict_set)
        result = []
        for ref in refs:
            name = _cfstring_to_py(cf, cs.DCSDictionaryGetName(ref))
            installed = bool(cs.DCSDictionaryGetURL(ref))
            if name is not None:
                result.append(DictionaryInfo(name=name, installed=installed))
        return result
    finally:
        cf.CFRelease(dict_set)


def parse_headword(text: str) -> str | None:
    """从 macOS 词典纯文本正文里尽力而为解析词头，解析不出返回 None。

    本机观察到两种版式（均来自 `DCSCopyTextDefinition` 的原文，没有更稳定
    的结构化分隔符可用）：
    - 英语类词典："headword | BrE .../AmE ... | 词性 ..."，取第一个 `|` 前
      的部分，如 "serendipity | BrE ... " → "serendipity"。
    - 中文类词典没有 `|`，词头是第一个空白前的词，如
      "苹果 píngguǒ 名 落叶乔木..." → "苹果"。
    两种规律都套不上（空串、第一段异常长）就返回 None，不瞎猜。
    """
    if not text:
        return None
    if "|" in text:
        candidate = text.split("|", 1)[0].strip()
    else:
        parts = text.split(None, 1)
        candidate = parts[0].strip() if parts else ""
    if not candidate or "\n" in candidate or len(candidate) > 64:
        return None
    return candidate


# ---------------------------------------------------------------------------
# 框架加载与 ctypes 绑定


def _ensure_loaded():
    """懒加载 CoreFoundation/CoreServices 并绑定公开 API 用到的符号。

    加锁保证线程安全；只在第一次调用时真正加载，之后直接返回缓存的句柄
    （或者复现第一次的失败原因）。
    """
    with _lock:
        if _state["loaded"]:
            if not _state["ok"]:
                raise DictionaryUnavailable("CoreFoundation/CoreServices 不可用")
            return _state["cf"], _state["cs"]
        _state["loaded"] = True
        if sys.platform != "darwin":
            raise DictionaryUnavailable("非 macOS 平台")
        try:
            cf_path = ctypes.util.find_library("CoreFoundation")
            cs_path = ctypes.util.find_library("CoreServices")
            if not cf_path or not cs_path:
                raise OSError("找不到 CoreFoundation/CoreServices 动态库")
            cf = ctypes.cdll.LoadLibrary(cf_path)
            cs = ctypes.cdll.LoadLibrary(cs_path)
            _bind_cf(cf)
            _bind_public(cs)
        except OSError as exc:
            raise DictionaryUnavailable(f"加载系统框架失败：{exc}") from exc
        _state["cf"] = cf
        _state["cs"] = cs
        _state["ok"] = True
        return cf, cs


def _bind_cf(cf) -> None:
    """绑定 CoreFoundation 里用到的公开符号（稳定的公开 API，不需要探测）。"""
    cf.CFStringCreateWithCString.restype = ctypes.c_void_p
    cf.CFStringCreateWithCString.argtypes = [
        ctypes.c_void_p, ctypes.c_char_p, ctypes.c_uint32,
    ]
    cf.CFStringGetLength.restype = ctypes.c_long
    cf.CFStringGetLength.argtypes = [ctypes.c_void_p]
    cf.CFStringGetMaximumSizeForEncoding.restype = ctypes.c_long
    cf.CFStringGetMaximumSizeForEncoding.argtypes = [ctypes.c_long, ctypes.c_uint32]
    cf.CFStringGetCString.restype = ctypes.c_bool
    cf.CFStringGetCString.argtypes = [
        ctypes.c_void_p, ctypes.c_char_p, ctypes.c_long, ctypes.c_uint32,
    ]
    cf.CFRelease.restype = None
    cf.CFRelease.argtypes = [ctypes.c_void_p]
    cf.CFSetGetCount.restype = ctypes.c_long
    cf.CFSetGetCount.argtypes = [ctypes.c_void_p]
    cf.CFSetGetValues.restype = None
    cf.CFSetGetValues.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
    cf.CFArrayGetCount.restype = ctypes.c_long
    cf.CFArrayGetCount.argtypes = [ctypes.c_void_p]
    cf.CFArrayGetValueAtIndex.restype = ctypes.c_void_p
    cf.CFArrayGetValueAtIndex.argtypes = [ctypes.c_void_p, ctypes.c_long]


def _bind_public(cs) -> None:
    """绑定唯一的公开 API 符号：`DCSCopyTextDefinition`。"""
    cs.DCSCopyTextDefinition.restype = ctypes.c_void_p
    cs.DCSCopyTextDefinition.argtypes = [ctypes.c_void_p, ctypes.c_void_p, CFRange]


def _bind_private(cs) -> None:
    """绑定列词典/按词典查询用到的私有符号，缺失就抛 `DictionaryUnavailable`。"""
    try:
        cs.DCSCopyAvailableDictionaries.restype = ctypes.c_void_p
        cs.DCSCopyAvailableDictionaries.argtypes = []
        cs.DCSDictionaryGetName.restype = ctypes.c_void_p
        cs.DCSDictionaryGetName.argtypes = [ctypes.c_void_p]
        cs.DCSDictionaryGetURL.restype = ctypes.c_void_p
        cs.DCSDictionaryGetURL.argtypes = [ctypes.c_void_p]
    except AttributeError as exc:
        raise DictionaryUnavailable(f"私有符号不存在：{exc}") from exc


def _bind_private_records(cs) -> None:
    """绑定取 HTML 正文用到的私有符号，缺失就抛 `DictionaryUnavailable`。"""
    try:
        cs.DCSCopyRecordsForSearchString.restype = ctypes.c_void_p
        cs.DCSCopyRecordsForSearchString.argtypes = [
            ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p,
        ]
        cs.DCSRecordCopyData.restype = ctypes.c_void_p
        # 第二个参数 version 漏了不会报错：被调用方会读到寄存器里的随机值
        cs.DCSRecordCopyData.argtypes = [ctypes.c_void_p, ctypes.c_long]
    except AttributeError as exc:
        raise DictionaryUnavailable(f"私有符号不存在：{exc}") from exc


# ---------------------------------------------------------------------------
# CF 类型与 Python 类型互转、查询实现


def _cfstring(cf, text: str):
    """Python str -> CFStringRef（Create 规则，调用方负责 CFRelease）。"""
    ref = cf.CFStringCreateWithCString(None, text.encode("utf-8"), _UTF8)
    if not ref:
        raise DictionaryUnavailable("字符串编码失败（CFStringCreateWithCString）")
    return ref


def _cfstring_to_py(cf, ref) -> str | None:
    """CFStringRef -> Python str，不 release（调用方按自己拿到 ref 的规则处理）。

    缓冲区按 `CFStringGetMaximumSizeForEncoding` 定大小，不用长度乘常数
    估算——中日韩字符在 UTF-8 下每字符最多 4 字节，乘 4 凑合能用，但用
    官方给的上界更准确也更不容易踩坑。
    """
    if not ref:
        return None
    length = cf.CFStringGetLength(ref)
    max_size = cf.CFStringGetMaximumSizeForEncoding(length, _UTF8) + 1
    buf = ctypes.create_string_buffer(max_size)
    ok = cf.CFStringGetCString(ref, buf, max_size, _UTF8)
    if not ok:
        return None
    return buf.value.decode("utf-8")


def _cfset_to_list(cf, cf_set) -> list:
    """CFSetRef -> 元素列表（元素本身不 release，跟着 set 走 Get 语义）。"""
    count = cf.CFSetGetCount(cf_set)
    arr = (ctypes.c_void_p * count)()
    cf.CFSetGetValues(cf_set, arr)
    return list(arr)


def _copy_text_definition(cf, cs, dict_ref, term: str) -> str | None:
    """调用 `DCSCopyTextDefinition`，`dict_ref` 为 None 时查公开 API 默认词典。"""
    t = _cfstring(cf, term)
    try:
        length = cf.CFStringGetLength(t)
        result = cs.DCSCopyTextDefinition(dict_ref, t, CFRange(0, length))
        if not result:
            return None
        try:
            return _cfstring_to_py(cf, result)
        finally:
            cf.CFRelease(result)
    finally:
        cf.CFRelease(t)


@contextmanager
def _dictionary(cf, cs, name: str):
    """按显示名找词典，在 with 块内提供 DCSDictionaryRef。

    ref 是从 CFSet 里 Get 出来的，只保证在 set 存活期间有效，所以 set 留到
    with 块结束才释放——不要把 ref 带出 with 块。找不到抛 `DictionaryUnavailable`。
    """
    _bind_private(cs)
    dict_set = cs.DCSCopyAvailableDictionaries()
    if not dict_set:
        raise DictionaryUnavailable("DCSCopyAvailableDictionaries 返回空")
    try:
        for ref in _cfset_to_list(cf, dict_set):
            if _cfstring_to_py(cf, cs.DCSDictionaryGetName(ref)) == name:
                yield ref
                return
        raise DictionaryUnavailable(f"词典 {name!r} 不在系统词典列表里")
    finally:
        cf.CFRelease(dict_set)


_TITLE_RE = re.compile(r'd:title="([^"]*)"')


def _parse_html_title(html: str) -> str | None:
    """从词典 HTML 正文里取 `d:title` 属性作为词头（词典自己标注的，更可靠）。"""
    m = _TITLE_RE.search(html)
    if not m:
        return None
    title = m.group(1).strip()
    return title or None


def _copy_html_record(cf, cs, dict_ref, term: str) -> str | None:
    """调用 `DCSCopyRecordsForSearchString` + `DCSRecordCopyData` 取第一条记录的 HTML。

    没命中（记录数为 0）返回 None；命中多条只取第一条——一个词可能有
    多个词性/义项，各是独立记录，这里不做消歧，只取词典默认排序的第一个。
    """
    _bind_private_records(cs)
    t = _cfstring(cf, term)
    try:
        records = cs.DCSCopyRecordsForSearchString(dict_ref, t, None, None)
        if not records:
            return None
        try:
            if cf.CFArrayGetCount(records) == 0:
                return None
            rec = cf.CFArrayGetValueAtIndex(records, 0)
            html_ref = cs.DCSRecordCopyData(rec, _RECORD_VERSION_HTML)
            if not html_ref:
                return None
            try:
                return _cfstring_to_py(cf, html_ref)
            finally:
                cf.CFRelease(html_ref)
        finally:
            cf.CFRelease(records)
    finally:
        cf.CFRelease(t)
