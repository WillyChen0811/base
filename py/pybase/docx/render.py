"""Word 模板渲染器 —— 对 document.xml/header*.xml/footer*.xml 做纯字符串替换（标记 -> 值），
不解析成对象树，所以能保留模板原有格式。"""
import os
import shutil
import zipfile
import tempfile
import logging
from io import BytesIO
from typing import Any, Dict, Optional

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
    *args: Any,
    output_path: Optional[str] = None,
    data: Optional[Dict[str, Any]] = None,
) -> Optional[bytes]:
    """渲染 Word 模板：对 data 里的每个"标记": "值"，在 document.xml/header*.xml/footer*.xml
    里做字符串替换。调用前需确保模板已做过标记清洗（见 clean_xml_content）。

    给了 output_path 就写文件并返回 None；不给就返回渲染后的 bytes（不落盘，取代原来
    单独的 render_template_to_bytes）。

    为兼容外部仓库里已有的调用方，支持两种参数形态（按位置参数个数区分，不要混用）：
    - 新：render_template(template_path, data, output_path=None)
    - 旧：render_template(template_path, output_path, data)  # output_path 可以是 None
    """
    if len(args) == 2:
        if output_path is not None or data is not None:
            raise TypeError("render_template: 不能同时用位置参数和 output_path/data 关键字传参")
        output_path, data = args
    elif len(args) == 1:
        if data is not None:
            raise TypeError("render_template: data 不能重复传入")
        data = args[0]
    elif len(args) != 0:
        raise TypeError(
            f"render_template: 除 template_path 外最多接受 2 个位置参数，收到 {len(args)} 个"
        )

    if data is None:
        raise TypeError("render_template: 缺少 data 参数")

    if not os.path.exists(template_path):
        raise FileNotFoundError(f"模板文件不存在: {template_path}")

    replacements = {}
    for k, v in data.items():
        key = str(k)
        value = preprocess_text(str(v) if v is not None else "")
        replacements[key] = value

    temp_dir = tempfile.mkdtemp()
    try:
        with zipfile.ZipFile(template_path, 'r') as zip_ref:
            file_list = zip_ref.namelist()
            target_files = [
                name for name in file_list
                if name == 'word/document.xml'
                or name.startswith('word/header')
                or name.startswith('word/footer')
            ]
            # zipfile 不支持原地改单个成员，只能整体解压、改文件、重新打包
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

        # 打包输出：给了 output_path 就落盘，否则打到内存返回 bytes
        zip_target = output_path if output_path is not None else BytesIO()
        with zipfile.ZipFile(zip_target, 'w', zipfile.ZIP_DEFLATED) as zip_out:
            for root, dirs, files in os.walk(temp_dir):
                for file in files:
                    file_path = os.path.join(root, file)
                    arcname = os.path.relpath(file_path, temp_dir)
                    zip_out.write(file_path, arcname)

        if output_path is not None:
            return None
        return zip_target.getvalue()

    except Exception as e:
        logger.error(f"模板渲染失败: {str(e)}")
        raise
    finally:
        shutil.rmtree(temp_dir)


def render_template_to_bytes(template_path: str, data: Dict[str, Any]) -> bytes:
    """已废弃，改用 render_template(template_path, data)（不传 output_path 即返回 bytes）。"""
    return render_template(template_path, data)
