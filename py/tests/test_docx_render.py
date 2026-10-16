"""pybase.docx.render：render_template 合并后的文件/字节两种输出形态 + 旧签名/旧函数名的向后兼容。"""

import os
import sys
import zipfile

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

from pybase.docx.render import render_template, render_template_to_bytes  # noqa: E402

DOCUMENT_XML = (
    '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
    '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">'
    '<w:body><w:p><w:r><w:t>{{姓名}}</w:t></w:r></w:p></w:body></w:document>'
)

CONTENT_TYPES_XML = (
    '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
    '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
    '<Default Extension="xml" ContentType="application/xml"/>'
    '<Override PartName="/word/document.xml" '
    'ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>'
    '</Types>'
)


def _make_template(path):
    """手写一个最小 docx（不依赖 python-docx）：仅含 document.xml，带一个待替换标记。"""
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as zf:
        zf.writestr("[Content_Types].xml", CONTENT_TYPES_XML)
        zf.writestr("word/document.xml", DOCUMENT_XML)


def _read_document_xml(docx_path):
    with zipfile.ZipFile(docx_path, "r") as zf:
        return zf.read("word/document.xml").decode("utf-8")


@pytest.fixture
def template(tmp_path):
    path = tmp_path / "template.docx"
    _make_template(path)
    return path


def test_新签名_给output_path就写文件并返回None(template, tmp_path):
    output = tmp_path / "out.docx"
    result = render_template(str(template), {"{{姓名}}": "张三"}, output_path=str(output))
    assert result is None
    assert "张三" in _read_document_xml(output)


def test_新签名_不给output_path返回bytes(template):
    result = render_template(str(template), {"{{姓名}}": "李四"})
    assert isinstance(result, bytes)
    with zipfile.ZipFile(__import__("io").BytesIO(result), "r") as zf:
        assert "李四" in zf.read("word/document.xml").decode("utf-8")


def test_旧签名_位置参数output_path在前data在后仍可用(template, tmp_path):
    output = tmp_path / "legacy_out.docx"
    result = render_template(str(template), str(output), {"{{姓名}}": "王五"})
    assert result is None
    assert "王五" in _read_document_xml(output)


def test_旧签名_原地替换(template):
    # websubmit 的调用方式：render_template(output_path, output_path, data)，输出路径与模板路径相同
    render_template(str(template), str(template), {"{{姓名}}": "赵六"})
    assert "赵六" in _read_document_xml(template)


def test_render_template_to_bytes别名已废弃但仍可用(template):
    result = render_template_to_bytes(str(template), {"{{姓名}}": "孙七"})
    assert isinstance(result, bytes)
    with zipfile.ZipFile(__import__("io").BytesIO(result), "r") as zf:
        assert "孙七" in zf.read("word/document.xml").decode("utf-8")


def test_混用位置参数和关键字报错(template):
    with pytest.raises(TypeError):
        render_template(str(template), "a", {"x": "y"}, data={"x": "y"})
