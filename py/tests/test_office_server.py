"""pybase.office 服务端测试。

本机是 macOS，没有 Word，真 COM 驱动跑不了。`OfficeService`（队列/看门狗/清理
这套编排逻辑）只依赖 `WordConverter` 接口，这里用一个假 converter 替换掉真
Word 驱动，独立验证：队列满 503、任务超时后看门狗强杀并重建、COM 错误后重建、
达到处理数阈值后重建、临时文件在成功/失败/超时后都被清掉。

真正驱动 Word COM 的 `WordCOMConverter` 只能在 Windows + Office 2019 的虚拟机
上实测，这里只验证它在非 Windows 平台上能被安全 import、构造时能给出明确报错
（缺 pywin32），不验证转换本身。
"""

import platform
import threading
import time

import pytest
import requests

from pybase.office.server import (
    OfficeHTTPServer,
    OfficeServerConfig,
    OfficeService,
    ServiceBusyError,
    WordConverter,
)


@pytest.fixture(autouse=True)
def _no_proxy_env(monkeypatch):
    """同 test_office_client.py：本机若配了全局代理，连 127.0.0.1 也会被拦截，
    这里全是打本机随机端口的假服务，跟代理无关，清掉环境变量避免干扰。"""
    for key in ("HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"):
        monkeypatch.delenv(key, raising=False)


def _wait_until(predicate, timeout=3.0, interval=0.02):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(interval)
    raise AssertionError("等待条件超时")


class FakeConverter(WordConverter):
    """假的 Word 驱动：不碰 COM，行为完全可控，用来测编排逻辑。"""

    def __init__(self, behavior="success", delay=0.0, pages=3, warnings=None):
        self.behavior = behavior  # 'success' | 'raise'
        self.delay = delay
        self.pages = pages
        self.warnings = warnings or []
        self.convert_calls = []
        self.kill_calls = 0
        self.restart_calls = 0
        self._alive = True
        self._proceed = threading.Event()
        self._proceed.set()  # 默认不阻塞；测试用 gate() 让它先卡住

    def gate(self) -> threading.Event:
        """清空内部信号量，convert() 会卡在里面直到测试手动 set() 放行。"""
        self._proceed.clear()
        return self._proceed

    def convert(self, docx_path, pdf_path, update_fields):
        self.convert_calls.append((docx_path, update_fields))
        self._proceed.wait(timeout=10)
        if self.delay:
            time.sleep(self.delay)
        if self.behavior == "raise":
            raise RuntimeError("模拟 COM 错误")
        with open(pdf_path, "wb") as f:
            f.write(b"%PDF-1.4 fake")
        return self.pages, list(self.warnings)

    def kill_current(self):
        self.kill_calls += 1
        self._alive = False
        self._proceed.set()  # 模拟进程被杀掉后，阻塞的 convert() 得以返回/抛错

    def restart(self):
        self.restart_calls += 1
        self._alive = True
        self._proceed.set()

    def is_alive(self):
        return self._alive

    def close(self):
        pass


# ============================================================
# OfficeService：队列 / 看门狗 / 重建 / 清理
# ============================================================

def test_submit_success_returns_pdf_and_page_count():
    converter = FakeConverter(pages=5)
    service = OfficeService(converter, work_dir=None, queue_maxsize=2, task_timeout_sec=5)
    try:
        task = service.submit(b"docx bytes", True)
        assert task.error is None
        assert task.result == b"%PDF-1.4 fake"
        assert task.page_count == 5
        assert task.warnings == []
    finally:
        service.close()


def test_submit_success_carries_warnings_through():
    converter = FakeConverter(pages=5, warnings=["更新目录失败: 模拟"])
    service = OfficeService(converter, work_dir=None, queue_maxsize=2, task_timeout_sec=5)
    try:
        task = service.submit(b"docx bytes", True)
        assert task.error is None  # warnings 不是硬失败，仍然是成功响应
        assert task.warnings == ["更新目录失败: 模拟"]
    finally:
        service.close()


def test_queue_full_returns_busy():
    converter = FakeConverter()
    gate = converter.gate()
    service = OfficeService(converter, work_dir=None, queue_maxsize=1, task_timeout_sec=5)
    try:
        t1 = threading.Thread(target=service.submit, args=(b"a", True))
        t1.start()
        _wait_until(lambda: converter.convert_calls)  # worker 已经把第一个任务取出、卡在 convert() 里

        t2 = threading.Thread(target=service.submit, args=(b"b", True))
        t2.start()
        _wait_until(lambda: service._queue.qsize() == 1)  # 第二个任务填满了队列容量

        with pytest.raises(ServiceBusyError):
            service.submit(b"c", True)  # 队列已满，第三个直接被拒，不占位等待

        gate.set()
        t1.join(timeout=5)
        t2.join(timeout=5)
    finally:
        service.close()


def test_task_timeout_triggers_kill_and_restart():
    converter = FakeConverter(delay=1.0)
    service = OfficeService(converter, work_dir=None, queue_maxsize=2, task_timeout_sec=0.2)
    try:
        start = time.time()
        task = service.submit(b"x", True)
        elapsed = time.time() - start

        assert elapsed < 0.8, "看门狗应该在任务真正跑完（1s 延迟）之前就让 submit 返回"
        assert task.error is not None
        assert task.error[0] == "timeout"
        assert converter.kill_calls == 1

        _wait_until(lambda: converter.restart_calls >= 1)  # worker 收尾后才会重建，稍晚于 submit 返回
    finally:
        service.close()


def test_com_error_triggers_restart_without_kill():
    converter = FakeConverter(behavior="raise")
    service = OfficeService(converter, work_dir=None, queue_maxsize=2, task_timeout_sec=5)
    try:
        task = service.submit(b"x", True)
        assert task.error is not None
        assert task.error[0] == "conversion_failed"
        assert converter.restart_calls == 1
        assert converter.kill_calls == 0  # 没超时，看门狗没触发，不该调用 kill
    finally:
        service.close()


def test_restart_after_n_tasks():
    converter = FakeConverter()
    service = OfficeService(converter, work_dir=None, queue_maxsize=2, task_timeout_sec=5, restart_after_n=3)
    try:
        for _ in range(3):
            service.submit(b"x", True)
        assert converter.restart_calls == 1
    finally:
        service.close()


def test_temp_files_cleaned_up_after_success(tmp_path):
    converter = FakeConverter()
    service = OfficeService(converter, work_dir=str(tmp_path), queue_maxsize=2, task_timeout_sec=5)
    try:
        service.submit(b"hello docx", True)
        assert list(tmp_path.iterdir()) == []
    finally:
        service.close()


def test_temp_files_cleaned_up_after_error(tmp_path):
    converter = FakeConverter(behavior="raise")
    service = OfficeService(converter, work_dir=str(tmp_path), queue_maxsize=2, task_timeout_sec=5)
    try:
        service.submit(b"hello docx", True)
        assert list(tmp_path.iterdir()) == []
    finally:
        service.close()


def test_temp_files_cleaned_up_after_timeout(tmp_path):
    converter = FakeConverter(delay=1.0)
    service = OfficeService(converter, work_dir=str(tmp_path), queue_maxsize=2, task_timeout_sec=0.2)
    try:
        service.submit(b"hello docx", True)
        # 清理发生在 worker 的 finally 里，恰好排在 restart 之后，等目录真正
        # 清空（而不是只等 restart_calls）才不会跟清理动作本身赛跑。
        _wait_until(lambda: list(tmp_path.iterdir()) == [])
    finally:
        service.close()


def test_health_reports_memory_backed_only_when_explicitly_declared(tmp_path):
    converter = FakeConverter()
    service = OfficeService(
        converter, work_dir=str(tmp_path), work_dir_memory_backed=True,
        queue_maxsize=2, task_timeout_sec=5,
    )
    try:
        service.submit(b"x", True)
        h = service.health()
        assert h["memory_backed"] is True
        assert h["work_dir"] == str(tmp_path)
        assert h["processed"] == 1
        assert h["word_alive"] is True
        assert h["last_error"] is None
    finally:
        service.close()


def test_health_non_memory_backed_when_work_dir_omitted():
    converter = FakeConverter()
    service = OfficeService(converter, work_dir=None, queue_maxsize=2, task_timeout_sec=5)
    try:
        h = service.health()
        assert h["memory_backed"] is False
    finally:
        service.close()


def test_cli_work_dir_is_ramdisk_flag_defaults_false():
    from pybase.office.server import _build_arg_parser

    args = _build_arg_parser().parse_args(["--work-dir", "R:\\"])
    assert args.work_dir_is_ramdisk is False


def test_cli_work_dir_is_ramdisk_flag_set():
    from pybase.office.server import _build_arg_parser

    args = _build_arg_parser().parse_args(["--work-dir", "R:\\", "--work-dir-is-ramdisk"])
    assert args.work_dir_is_ramdisk is True


def test_health_non_memory_backed_when_work_dir_given_but_not_declared(tmp_path):
    """传了 work_dir，但没显式声明 work_dir_memory_backed=True——不该被当成内存盘。
    早先版本用 "work_dir is not None" 推断，这里专门验证不再这样猜。"""
    converter = FakeConverter()
    service = OfficeService(converter, work_dir=str(tmp_path), queue_maxsize=2, task_timeout_sec=5)
    try:
        h = service.health()
        assert h["memory_backed"] is False
    finally:
        service.close()


# ============================================================
# HTTP 层：端到端，走真实的 OfficeHTTPServer + requests
# ============================================================

def _start_http_server(config: OfficeServerConfig, converter: FakeConverter):
    service = OfficeService(
        converter, work_dir=config.work_dir, queue_maxsize=config.queue_maxsize,
        task_timeout_sec=config.task_timeout_sec, restart_after_n=config.restart_after_n,
    )
    httpd = OfficeHTTPServer(("127.0.0.1", 0), service, config)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    port = httpd.server_address[1]
    return httpd, service, f"http://127.0.0.1:{port}"


def test_http_success_returns_pdf_with_page_count_header():
    converter = FakeConverter(pages=9)
    config = OfficeServerConfig(queue_maxsize=2, task_timeout_sec=5)
    httpd, service, base = _start_http_server(config, converter)
    try:
        resp = requests.post(f"{base}/convert", data=b"docx bytes", timeout=10)
        assert resp.status_code == 200
        assert resp.headers["Content-Type"] == "application/pdf"
        assert resp.headers["X-Page-Count"] == "9"
        assert "X-Warnings" not in resp.headers  # 没有问题就不带这个头
        assert resp.content == b"%PDF-1.4 fake"
    finally:
        httpd.shutdown()
        service.close()


def test_http_success_with_warnings_header():
    import json
    from urllib.parse import unquote

    converter = FakeConverter(pages=9, warnings=["更新目录失败: 模拟", "更新域失败 story_type=6: 模拟"])
    config = OfficeServerConfig(queue_maxsize=2, task_timeout_sec=5)
    httpd, service, base = _start_http_server(config, converter)
    try:
        resp = requests.post(f"{base}/convert", data=b"docx bytes", timeout=10)
        assert resp.status_code == 200  # warnings 不是硬失败，状态码仍是 200
        assert "X-Warnings" in resp.headers
        warnings = json.loads(unquote(resp.headers["X-Warnings"]))
        assert warnings == ["更新目录失败: 模拟", "更新域失败 story_type=6: 模拟"]
    finally:
        httpd.shutdown()
        service.close()


def test_http_unauthorized_without_token():
    converter = FakeConverter()
    config = OfficeServerConfig(queue_maxsize=2, task_timeout_sec=5, token="secret")
    httpd, service, base = _start_http_server(config, converter)
    try:
        resp = requests.post(f"{base}/convert", data=b"x", timeout=10)
        assert resp.status_code == 401
        assert resp.json()["error"] == "unauthorized"

        resp_ok = requests.post(
            f"{base}/convert", data=b"x", headers={"X-Office-Token": "secret"}, timeout=10,
        )
        assert resp_ok.status_code == 200
    finally:
        httpd.shutdown()
        service.close()


def test_http_payload_too_large():
    converter = FakeConverter()
    config = OfficeServerConfig(queue_maxsize=2, task_timeout_sec=5, max_body_bytes=10)
    httpd, service, base = _start_http_server(config, converter)
    try:
        resp = requests.post(f"{base}/convert", data=b"x" * 100, timeout=10)
        assert resp.status_code == 413
        assert resp.json()["error"] == "too_large"
    finally:
        httpd.shutdown()
        service.close()


def test_http_empty_body_is_bad_request():
    converter = FakeConverter()
    config = OfficeServerConfig(queue_maxsize=2, task_timeout_sec=5)
    httpd, service, base = _start_http_server(config, converter)
    try:
        resp = requests.post(f"{base}/convert", data=b"", timeout=10)
        assert resp.status_code == 400
        assert resp.json()["error"] == "bad_request"
    finally:
        httpd.shutdown()
        service.close()


def test_http_busy_when_queue_full():
    converter = FakeConverter()
    gate = converter.gate()
    config = OfficeServerConfig(queue_maxsize=1, task_timeout_sec=5)
    httpd, service, base = _start_http_server(config, converter)
    try:
        results = []

        def _post():
            results.append(requests.post(f"{base}/convert", data=b"x", timeout=10))

        t1 = threading.Thread(target=_post)
        t1.start()
        _wait_until(lambda: converter.convert_calls)

        t2 = threading.Thread(target=_post)
        t2.start()
        _wait_until(lambda: service._queue.qsize() == 1)

        resp3 = requests.post(f"{base}/convert", data=b"x", timeout=10)
        assert resp3.status_code == 503
        assert resp3.json()["error"] == "busy"

        gate.set()
        t1.join(timeout=5)
        t2.join(timeout=5)
        assert all(r.status_code == 200 for r in results)
    finally:
        httpd.shutdown()
        service.close()


def test_http_timeout_returns_504():
    converter = FakeConverter(delay=1.0)
    config = OfficeServerConfig(queue_maxsize=2, task_timeout_sec=0.2)
    httpd, service, base = _start_http_server(config, converter)
    try:
        resp = requests.post(f"{base}/convert", data=b"x", timeout=10)
        assert resp.status_code == 504
        assert resp.json()["error"] == "timeout"
    finally:
        httpd.shutdown()
        service.close()


def test_http_health_endpoint():
    converter = FakeConverter()
    config = OfficeServerConfig(queue_maxsize=2, task_timeout_sec=5)
    httpd, service, base = _start_http_server(config, converter)
    try:
        resp = requests.get(f"{base}/health", timeout=10)
        assert resp.status_code == 200
        body = resp.json()
        assert set(body.keys()) == {
            "word_alive", "queue_len", "processed", "last_error", "memory_backed", "work_dir",
        }
    finally:
        httpd.shutdown()
        service.close()


# ============================================================
# WordCOMConverter：只验证非 Windows 上的导入/构造安全性
# ============================================================

@pytest.mark.skipif(platform.system() == "Windows", reason="这条只验证非 Windows 平台的降级行为")
def test_word_com_converter_import_is_safe_on_non_windows():
    """模块顶层不该在非 Windows 平台 import 时就失败——真正依赖 pywin32 的地方
    延迟到构造/使用时才触发，这样测试文件本身可以放心 import WordCOMConverter。"""
    from pybase.office.server import WordCOMConverter  # noqa: F401 —— import 本身不该抛异常


@pytest.mark.skipif(platform.system() == "Windows", reason="真实 Word COM 驱动只能在 Windows 上跑")
def test_word_com_converter_construction_fails_without_pywin32():
    from pybase.office.server import WordCOMConverter

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
    from pybase.office.server import WordCOMConverter

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
