"""apple_xhtml 解析测试。样本照 macOS 牛津系词典的真实结构手写，内容是编的——
不能把词典原文放进仓库（版权属于出版方）。"""
from pybase.dictionary.apple_xhtml import parse_entry

_HEAD = ('<?xml version="1.0" encoding="UTF-8"?><html xmlns:d="http://www.apple.com/DTDs/'
         'DictionaryService-1.0.rng"><body>')

# 英汉版式：gramb/ps → semb（sn、trg: ind + trans + ty_pinyin）→ exg
BILINGUAL = _HEAD + '''<d:entry d:title="blorp" class="entry"><span class="hwg x_xh0">
<span class="hw">blorp </span>
<span dialect="BrE" class="prx"><span class="gp tg_prx"> | </span><span class="gp tg_prx">BrE </span><span class="ph">blɔːp</span></span>
<span dialect="AmE" class="prx"><span class="gp tg_prx">AmE </span><span class="ph">blɔrp</span></span></span>
<span class="gramb x_xd0"><span class="x_xdh"><span class="gp sn ty_label tg_gramb">A.</span><span class="ps">noun </span></span>
<span class="semb x_xd1"><span class="gp x_xdh sn ty_label tg_semb">① </span>
<span class="trgg"><span class="trg"><span class="ind"><span class="gp tg_ind">(</span>sound<span class="gp tg_ind">) </span></span>
<span class="trans">咕噜声 </span><span class="trans ty_pinyin">gūlū shēng</span><span class="gp tg_trg">; </span></span>
<span class="trg"><span class="trans">闷响 </span><span class="trans ty_pinyin">mèn xiǎng</span></span></span>
<span class="exg"><span class="x_xdh"><span class="sn">▸ </span><span class="ex">a loud blorp </span></span>
<span class="trg"><span class="trans">一声响亮的咕噜 </span><span class="trans ty_pinyin">yī shēng</span></span></span></span></span>
</d:entry></body></html>'''

# 英英版式：hg → se1/posg/pos → se2（编号）→ msDict（df、eg/ex）；etym；派生词带自己的音标
MONOLINGUAL = _HEAD + '''<d:entry d:title="blorp" class="entry"><span class="hg x_xh0"><span class="hw">blorp</span>
<span class="prx"><span class="ph t_respell">blôrp</span></span></span>
<span class="sg"><span class="se1 x_xd0"><span class="posg x_xdh"><span class="pos"><span class="gp tg_pos">noun </span></span></span>
<span class="se2 x_xd1 hasSn"><span class="gp x_xdh sn ty_label tg_se2">1 </span>
<span class="msDict t_core"><span class="df">a low wet sound</span><span class="eg"><span class="ex">the pipe gave a blorp</span></span></span>
<span class="msDict t_subsense"><span class="df">a small failure</span></span></span>
<span class="se2 x_xd1 hasSn"><span class="gp x_xdh sn ty_label tg_se2">2 </span>
<span class="msDict t_core"><span class="df">a made-up word</span></span></span></span></span>
<span class="subEntryBlock"><span class="subEntry"><span class="prx"><span class="ph">blôrpiness</span></span></span></span>
<span class="etym x_xo0"><span class="gp x_xoLblBlk ty_label tg_etym">ORIGIN </span><span class="x_xo1">
<span class="dg"><span class="date">early 21st century </span></span>: imitative.</span></span>
</d:entry></body></html>'''


def test_bilingual_drops_pinyin_and_keeps_structure():
    e = parse_entry(BILINGUAL)
    assert e.headword == "blorp"
    assert e.pronunciations == [("BrE", "blɔːp"), ("AmE", "blɔrp")]
    [group] = e.groups
    assert group.pos == "noun"
    [sense] = group.senses
    assert sense.number == "①"
    assert sense.definitions == ["(sound) 咕噜声", "闷响"]
    assert [(x.text, x.translation) for x in sense.examples] == [("a loud blorp", "一声响亮的咕噜")]
    assert e.etymology is None


def test_monolingual_numbered_senses_etymology_and_headword_pronunciation_only():
    e = parse_entry(MONOLINGUAL)
    assert e.pronunciations == [(None, "blôrp")]  # 派生词的音标不算
    [group] = e.groups
    assert group.pos == "noun"
    assert [(s.number, s.definitions) for s in group.senses] == [
        ("1", ["a low wet sound", "a small failure"]),
        ("2", ["a made-up word"]),
    ]
    assert group.senses[0].examples[0].text == "the pipe gave a blorp"
    assert e.etymology == "early 21st century : imitative."


def test_unknown_layout_returns_none():
    assert parse_entry(_HEAD + '<d:entry class="entry"><p>plain</p></d:entry></body></html>') is None
    assert parse_entry("not xml <") is None
