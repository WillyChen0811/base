"""
Word 模板渲染器 - 直接操作 XML 以保留格式
移植自原 PHP 服务逻辑
"""
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
    """
    清洗 XML 内容，合并 run (<w:r>) 内被拆分的 text (<w:t>) 节点

    安全策略：只对包含关键字前缀"这是"的 run 执行合并，
    避免破坏页码域代码等不相关的 XML 结构。
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
    """
    预处理替换文本 - 处理特殊字符和换行

    处理流程：
    1. 统一换行符格式
    2. 按换行符分割文本
    3. 对每一行分别进行 XML 转义
    4. 用 Word XML 换行标签连接各行

    这样可以确保：
    - 每行的特殊字符被正确转义
    - 换行符被转换为正确的 Word XML 结构
    - 生成的 XML 标签不会被转义
    """
    if not isinstance(text, str):
        return str(text)

    # 1. 统一换行符格式 (Windows \r\n, Mac \r, Unix \n)
    text = text.replace("\r\n", "\n").replace("\r", "\n")

    # 2. 按换行符分割
    lines = text.split("\n")

    # 3. 分别转义每一行的 XML 特殊字符
    escaped_lines = [escape_xml(line) for line in lines]

    # 4. 用 Word XML 换行标签连接
    # Word 中的换行结构: </w:t><w:br/><w:t>
    # 这样会利用原有的 <w:t> 标签，形成正确的 XML 结构
    result = '</w:t><w:br/><w:t>'.join(escaped_lines)

    return result

def render_template(
    template_path: str,
    output_path: str,
    data: Dict[str, Any]
) -> None:
    """
    渲染 Word 模板
    
    Args:
        template_path: 模板文件路径
        output_path: 输出文件路径
        data: 替换数据字典 {"标记": "值"}
    """
    if not os.path.exists(template_path):
        raise FileNotFoundError(f"模板文件不存在: {template_path}")

    # 创建临时目录
    temp_dir = tempfile.mkdtemp()
    temp_docx = os.path.join(temp_dir, "temp.docx")
    
    try:
        # 复制模板到临时文件
        shutil.copy2(template_path, temp_docx)
        
        # 预处理替换数据
        replacements = {}
        for k, v in data.items():
            # 确保键也是字符串
            key = str(k)
            # 预处理值
            value = preprocess_text(str(v) if v is not None else "")
            replacements[key] = value
            
        # 需要处理的 XML 文件列表
        # 通常主要内容在 word/document.xml
        # 页眉页脚在 word/header*.xml, word/footer*.xml
        target_files = []
        
        # 读取 zip 内容列表
        with zipfile.ZipFile(temp_docx, 'r') as zip_ref:
            file_list = zip_ref.namelist()
            for file_name in file_list:
                if file_name == 'word/document.xml' or \
                   file_name.startswith('word/header') or \
                   file_name.startswith('word/footer'):
                    target_files.append(file_name)
        
        # 执行替换
        # 注意：zipfile 不支持直接修改文件，我们需要创建一个新的 zip
        # 或者使用 update 模式（但在某些系统上可能不稳定），这里采用重写方式
        
        # 解压所有文件到临时目录
        with zipfile.ZipFile(temp_docx, 'r') as zip_ref:
            zip_ref.extractall(temp_dir)
            
        # 遍历目标 XML 文件进行替换
        for rel_path in target_files:
            xml_path = os.path.join(temp_dir, rel_path)
            if not os.path.exists(xml_path):
                continue
                
            with open(xml_path, 'r', encoding='utf-8') as f:
                content = f.read()

            # 执行字符串替换
            # 注意：模板上传时已通过 template_optimizer 处理，无需再次清洗
            for marker, value in replacements.items():
                if marker in content:
                    content = content.replace(marker, value)
                    logger.debug(f"Replaced {marker} in {rel_path}")
            
            # 写回文件
            with open(xml_path, 'w', encoding='utf-8') as f:
                f.write(content)
                
        # 重新打包为 docx
        with zipfile.ZipFile(output_path, 'w', zipfile.ZIP_DEFLATED) as zip_out:
            for root, dirs, files in os.walk(temp_dir):
                for file in files:
                    # 跳过临时 docx 本身
                    if file == "temp.docx":
                        continue
                        
                    file_path = os.path.join(root, file)
                    # 计算在 zip 中的相对路径
                    arcname = os.path.relpath(file_path, temp_dir)
                    zip_out.write(file_path, arcname)
                    
    except Exception as e:
        logger.error(f"模板渲染失败: {str(e)}")
        raise e
    finally:
        # 清理临时目录
        shutil.rmtree(temp_dir)


def render_template_to_bytes(
    template_path: str,
    data: Dict[str, Any]
) -> bytes:
    """
    渲染 Word 模板，返回字节数据

    Args:
        template_path: 模板文件路径
        data: 替换数据字典 {"标记": "值"}

    Returns:
        渲染后的文档字节数据
    """
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
