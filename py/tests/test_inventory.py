"""pybase.inventory：经 Storage 列清单、跳过规则、列不动的目录上报、路径/同名两级匹配。"""

import os
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

from pybase.inventory import Index, compare, walk_files  # noqa: E402
from pybase.storage import LocalStorage  # noqa: E402


def make(root, files):
    for rel, data in files.items():
        p = root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(data)


def test_列清单_跳过名字与目录(tmp_path):
    make(tmp_path, {"p/a.docx": b"1234", "p/.DS_Store": b"x", "p/node_modules/m.js": b"m", "p/sub/b.pdf": b"12"})
    listing = walk_files(LocalStorage(tmp_path), "p", skip_name=lambda n: n == ".DS_Store",
                         skip_dir=lambda rel: rel.split("/")[-1] == "node_modules")
    assert listing.files == {"a.docx": 4, "sub/b.pdf": 2}
    assert listing.failed == []


def test_列不动的目录要上报(tmp_path):
    class Broken(LocalStorage):
        def listdir(self, path=""):
            if path.endswith("bad"):
                raise PermissionError(path)
            return super().listdir(path)

    make(tmp_path, {"ok/a": b"1", "bad/b": b"2"})
    listing = walk_files(Broken(tmp_path))
    assert listing.files == {"ok/a": 1}
    assert listing.failed == ["bad"]


def test_两级匹配():
    idx = Index().add({"4-报告/r.docx": 10, "2-视频/v.mp4": 99}, label="存档/P1")
    got = {rel: (kind, ref) for rel, _, kind, ref in compare(
        {"4-报告/r.docx": 10, "其他/v.mp4": 99, "4-报告/r2.docx": 10, "x.zip": 5}, idx)}
    assert got["4-报告/r.docx"] == ("path", "存档/P1/4-报告/r.docx")
    assert got["其他/v.mp4"] == ("name", "存档/P1/2-视频/v.mp4")
    assert got["4-报告/r2.docx"] == (None, None)       # 同大小不同名不算
    assert got["x.zip"] == (None, None)
