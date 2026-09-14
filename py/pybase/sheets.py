"""表格文件读取 —— .xlsx / .xls / .csv 一律读成 [[str]]

**有意不用 pandas**：shared 里写一份两端都接，而客户端的安装包不该为了
"把表读成二维字符串"这件事背上 pandas + numpy。直接用 openpyxl(.xlsx) /
xlrd(.xls) / 标准库 csv，行为与原先 `header=None` 的 read_excel 一致：
所有物理行原样出来，空格子给空字符串，不做类型推断、不丢表头行。
"""

import csv
import os
from typing import List, Tuple


def _rows_from_xlsx(file_path: str) -> List[Tuple[str, List[List[str]]]]:
    import openpyxl

    wb = openpyxl.load_workbook(file_path, data_only=True)
    try:
        result = []
        for ws in wb.worksheets:
            rows = [
                ["" if v is None else str(v) for v in row]
                for row in ws.iter_rows(values_only=True)
            ]
            result.append((ws.title, rows))
        return result
    finally:
        wb.close()


def _rows_from_xls(file_path: str) -> List[Tuple[str, List[List[str]]]]:
    import xlrd

    book = xlrd.open_workbook(file_path)
    result = []
    for sheet in book.sheets():
        rows = [
            ["" if v is None else str(v) for v in sheet.row_values(r)]
            for r in range(sheet.nrows)
        ]
        result.append((sheet.name, rows))
    return result


def _rows_from_csv(file_path: str) -> List[Tuple[str, List[List[str]]]]:
    # utf-8-sig 吃掉 Excel 导出的 BOM，否则第一格标题会带一个看不见的
    with open(file_path, newline="", encoding="utf-8-sig") as f:
        rows = [[cell.strip() for cell in row] for row in csv.reader(f)]
    return [("Sheet1", rows)]


def load_sheets(file_path: str) -> List[Tuple[str, List[List[str]]]]:
    """读全部 sheet，返回 [(sheet 名, 行)]"""
    ext = os.path.splitext(file_path)[1].lower()
    if ext in (".xlsx", ".xlsm"):
        return _rows_from_xlsx(file_path)
    if ext == ".xls":
        return _rows_from_xls(file_path)
    if ext == ".csv":
        return _rows_from_csv(file_path)
    raise ValueError(f"不支持的文件类型：{ext or '(无扩展名)'}，请提供 .xlsx/.xls/.csv 文件")


def sheet_names(file_path: str) -> List[str]:
    """只要 sheet 名清单（不读单元格，几十兆的表也是秒回）"""
    ext = os.path.splitext(file_path)[1].lower()
    if ext in (".xlsx", ".xlsm"):
        import openpyxl

        wb = openpyxl.load_workbook(file_path, read_only=True)
        try:
            return list(wb.sheetnames)
        finally:
            wb.close()
    if ext == ".xls":
        import xlrd

        return xlrd.open_workbook(file_path, on_demand=True).sheet_names()
    if ext == ".csv":
        return ["Sheet1"]
    raise ValueError(f"不支持的文件类型：{ext or '(无扩展名)'}，请提供 .xlsx/.xls/.csv 文件")
