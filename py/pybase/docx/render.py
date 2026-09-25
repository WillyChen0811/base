"""Word 模板渲染器 —— 对 document.xml/header*.xml/footer*.xml 做纯字符串替换（标记 -> 值），
不解析成对象树，所以能保留模板原有格式。"""
import os
import shutil
import zipfile
import tempfile
import logging
from typing import Dict, Any

# 配置日志
logger = logging.getLogger(__name__)

import xml.etree.ElementTree as ET

# Word XML命名空间
NAMESPACES = {
    'w': 'http://schemas.openxmlformats.org/wordprocessingml/2006/main',
}

def clean_xml_content(xml_content: str) -> str:
    """合并同一个 run (<w:r>) 内被拆分的多个 <w:t> 节点，让跨节点的标记能被整体替换。

    只对合并后文本包含关键字前缀"这是"的 run 生效，避免误伤页码域代码等不相关结构。
    """
    try:
        for prefix, uri in NAMESPACES.items():
            ET.register_namespace(prefix, uri)

        root = ET.fromstring(xml_content)

        for run in root.findall('.//w:r', NAMESPACES):
            text_elements = run.findall('.//w:t', NAMESPACES)

            if len(text_elements) > 1:
                # 先计算合并后的文本
                merged_text = ''.join([t.text or '' for t in text_elements])

                # 只有包含关键字前缀"这是"时才执行合并
                if '这是' not in merged_text:
                    continue

                # 执行合并
                text_elements[0].text = merged_text
                for t in text_elements[1:]:
                    t.text = ''

        return ET.tostring(root, encoding='unicode')

    except Exception as e:
        logger.warning(f"XML 清洗失败，将使用原始内容: {str(e)}")
        return xml_content

def escape_xml(text: str) -> str:
    """转义 XML 特殊字符"""
    if not isinstance(text, str):
        return str(text)
    
    return text.replace("&", "&amp;") \
               .replace("<", "&lt;") \
               .replace(">", "&gt;") \
               .replace('"', "&quot;") \
               .replace("'", "&apos;")

def preprocess_text(text: str) -> str:
    """替换值预处理：按行转义 XML 特殊字符，再用 `</w:t><w:br/><w:t>` 拼回——

    换行不能直接转义进 <w:t> 文本，得先断开标签插入 w:br，否则 Word 里不会换行。
    """
    if not isinstance(text, str):
        return str(text)

    text = text.replace("\r\n", "\n").replace("\r", "\n")
    lines = text.split("\n")
    escaped_lines = [escape_xml(line) for line in lines]
    result = '</w:t><w:br/><w:t>'.join(escaped_lines)

    return result

def render_template(
    template_path: str,
    output_path: str,
    data: Dict[str, Any]
) -> None:
    """渲染 Word 模板：对 data 里的每个"标记": "值"，在 document.xml/header*.xml/footer*.xml
    里做字符串替换，输出到 output_path。调用前需确保模板已做过标记清洗（见 clean_xml_content）。
    """
    if not os.path.exists(template_path):
        raise FileNotFoundError(f"模板文件不存在: {template_path}")

    temp_dir = tempfile.mkdtemp()
    temp_docx = os.path.join(temp_dir, "temp.docx")

    try:
        shutil.copy2(template_path, temp_docx)

        replacements = {}
        for k, v in data.items():
            key = str(k)
            value = preprocess_text(str(v) if v is not None else "")
            replacements[key] = value

        target_files = []

        with zipfile.ZipFile(temp_docx, 'r') as zip_ref:
            file_list = zip_ref.namelist()
            for file_name in file_list:
                if file_name == 'word/document.xml' or \
                   file_name.startswith('word/header') or \
                   file_name.startswith('word/footer'):
                    target_files.append(file_name)

        # zipfile 不支持原地改单个成员，只能整体解压、改文件、重新打包
        with zipfile.ZipFile(temp_docx, 'r') as zip_ref:
            zip_ref.extractall(temp_dir)

        for rel_path in target_files:
            xml_path = os.path.join(temp_dir, rel_path)
            if not os.path.exists(xml_path):
                continue

            with open(xml_path, 'r', encoding='utf-8') as f:
                content = f.read()

            for marker, value in replacements.items():
                if marker in content:
                    content = content.replace(marker, value)
                    logger.debug(f"Replaced {marker} in {rel_path}")

            with open(xml_path, 'w', encoding='utf-8') as f:
                f.write(content)

        with zipfile.ZipFile(output_path, 'w', zipfile.ZIP_DEFLATED) as zip_out:
            for root, dirs, files in os.walk(temp_dir):
                for file in files:
                    if file == "temp.docx":  # 跳过临时 docx 本身，别把它打进输出
                        continue

                    file_path = os.path.join(root, file)
                    arcname = os.path.relpath(file_path, temp_dir)
                    zip_out.write(file_path, arcname)

    except Exception as e:
        logger.error(f"模板渲染失败: {str(e)}")
        raise e
    finally:
        shutil.rmtree(temp_dir)


def render_template_to_bytes(
    template_path: str,
    data: Dict[str, Any]
) -> bytes:
    """同 render_template，但直接返回渲染后的字节数据（不落盘输出文件）。"""
    from io import BytesIO

    if not os.path.exists(template_path):
        raise FileNotFoundError(f"模板文件不存在: {template_path}")

    temp_dir = tempfile.mkdtemp()

    try:
        # 预处理替换数据
        replacements = {}
        for k, v in data.items():
            key = str(k)
            value = preprocess_text(str(v) if v is not None else "")
            replacements[key] = value

        # 解压模板
        with zipfile.ZipFile(template_path, 'r') as zip_ref:
            zip_ref.extractall(temp_dir)

        # 需要处理的 XML 文件
        target_files = []
        for item in os.listdir(os.path.join(temp_dir, 'word')):
            if item == 'document.xml' or item.startswith('header') or item.startswith('footer'):
                target_files.append(os.path.join('word', item))

        # 执行替换
        for rel_path in target_files:
            xml_path = os.path.join(temp_dir, rel_path)
            if not os.path.exists(xml_path):
                continue

            with open(xml_path, 'r', encoding='utf-8') as f:
                content = f.read()

            for marker, value in replacements.items():
                if marker in content:
                    content = content.replace(marker, value)

            with open(xml_path, 'w', encoding='utf-8') as f:
                f.write(content)

        # 打包到内存
        output_buffer = BytesIO()
        with zipfile.ZipFile(output_buffer, 'w', zipfile.ZIP_DEFLATED) as zip_out:
            for root, dirs, files in os.walk(temp_dir):
                for file in files:
                    file_path = os.path.join(root, file)
                    arcname = os.path.relpath(file_path, temp_dir)
                    zip_out.write(file_path, arcname)

        return output_buffer.getvalue()

    finally:
        shutil.rmtree(temp_dir)
