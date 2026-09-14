"""往 docx 里写格式属性的安全底座。

这套东西只做一件事:**改属性,不动树结构**。字体不拆 run、对齐不重排段落,
所以风险与"改资料"那种要 getparent()/删节点区间的文本替换不是一个量级。

但有一处非踩不可的坑,已经踩过一次:**w:rPr / w:pPr 的子元素顺序由 schema
规定死,新建子元素 append 到末尾会让 Word 提示"文档已损坏,需要修复"**。
实测:原 rPr 是 [b, sz],把 rFonts append 上去成了 [b, sz, rFonts, szCs],
zip 完好、XML 也能解析,四道检查全过——只有 Word 会报错。所以:

- 新建子元素一律走 `ensure_child`,按 _RPR_ORDER/_PPR_ORDER 插到正确位置;
- 改完必须跑 `check_element_order`,把顺序违规当硬错误挡在保存之前。

字体不需要拆 run:w:rFonts 上 eastAsia 管中文、ascii/hAnsi 管西文数字,
一个 run 同时设这三个属性就管好中英混排,不必按字符切分。
"""

from typing import Dict, List, Optional

from docx.oxml import OxmlElement
from docx.oxml.ns import qn

W = '{http://schemas.openxmlformats.org/wordprocessingml/2006/main}'

# ECMA-376 规定的 w:rPr 子元素顺序(常用部分,够覆盖本模块要写的)
_RPR_ORDER: List[str] = [
    'rStyle', 'rFonts', 'b', 'bCs', 'i', 'iCs', 'caps', 'smallCaps', 'strike',
    'dstrike', 'outline', 'shadow', 'emboss', 'imprint', 'noProof', 'snapToGrid',
    'vanish', 'webHidden', 'color', 'spacing', 'w', 'kern', 'position', 'sz',
    'szCs', 'highlight', 'u', 'effect', 'bdr', 'shd', 'fitText', 'vertAlign',
    'rtl', 'cs', 'em', 'lang', 'eastAsianLayout', 'specVanish', 'oMath',
]

# w:pPr 同理
_PPR_ORDER: List[str] = [
    'pStyle', 'keepNext', 'keepLines', 'pageBreakBefore', 'framePr', 'widowControl',
    'numPr', 'suppressLineNumbers', 'pBdr', 'shd', 'tabs', 'suppressAutoHyphens',
    'kinsoku', 'wordWrap', 'overflowPunct', 'topLinePunct', 'autoSpaceDE',
    'autoSpaceDN', 'bidi', 'adjustRightInd', 'snapToGrid', 'spacing', 'ind',
    'contextualSpacing', 'mirrorIndents', 'suppressOverlap', 'jc', 'textDirection',
    'textAlignment', 'textboxTightWrap', 'outlineLvl', 'divId', 'cnfStyle', 'rPr',
    'sectPr', 'pPrChange',
]

# w:tcPr 同理。合并单元格要新建 w:gridSpan,它必须落在 tcW 之后、vMerge 之前
_TCPR_ORDER: List[str] = [
    'cnfStyle', 'tcW', 'gridSpan', 'hMerge', 'vMerge', 'tcBorders', 'shd',
    'noWrap', 'tcMar', 'textDirection', 'tcFitText', 'vAlign', 'hideMark',
]

_ORDERS: Dict[str, List[str]] = {
    'rPr': _RPR_ORDER, 'pPr': _PPR_ORDER, 'tcPr': _TCPR_ORDER,
}


def ensure_child(parent, tag: str, order: List[str]):
    """取或建子元素;新建时按 schema 顺序插入,**不能 append**"""
    el = parent.find(qn(tag))
    if el is not None:
        return el
    el = OxmlElement(tag)
    name = tag.split(':', 1)[1]
    pos = order.index(name) if name in order else len(order)
    for i, child in enumerate(parent):
        cname = child.tag.split('}', 1)[-1]
        cpos = order.index(cname) if cname in order else len(order)
        if cpos > pos:
            parent.insert(i, el)
            return el
    parent.append(el)
    return el


def ensure_rpr(run_el):
    """w:rPr 必须是 w:r 的第一个子元素"""
    rpr = run_el.find(qn('w:rPr'))
    if rpr is None:
        rpr = OxmlElement('w:rPr')
        run_el.insert(0, rpr)
    return rpr


def ensure_ppr(p_el):
    """w:pPr 必须是 w:p 的第一个子元素"""
    ppr = p_el.find(qn('w:pPr'))
    if ppr is None:
        ppr = OxmlElement('w:pPr')
        p_el.insert(0, ppr)
    return ppr


def set_val(parent, tag: str, value: str, order: List[str]) -> bool:
    """设 w:val 型子元素,返回是否发生了改动"""
    el = ensure_child(parent, tag, order)
    if el.get(qn('w:val')) == value:
        return False
    el.set(qn('w:val'), value)
    return True


def check_element_order(root) -> List[str]:
    """自查:rPr/pPr 的子元素顺序与位置。返回违规描述,空列表=干净。

    这道检查抓住过真实的 3 处违规(append 造成),**不要因为"看起来没问题"
    就跳过**——zip 完好和 XML 可解析都发现不了它。
    """
    problems: List[str] = []
    for tag, order in _ORDERS.items():
        for el in root.iter(W + tag):
            names = [c.tag.replace(W, '') for c in el if c.tag.startswith(W)]
            idx = [order.index(n) for n in names if n in order]
            if idx != sorted(idx):
                problems.append(f"{tag} 子元素顺序违规: {names}")
    for parent_tag, child_tag in (('r', 'rPr'), ('p', 'pPr')):
        for el in root.iter(W + parent_tag):
            child = el.find(W + child_tag)
            if child is not None and len(el) and el[0] is not child:
                problems.append(f"{child_tag} 不是 {parent_tag} 的第一个子元素")
    return problems
