import io
import os

import pytest

from pybase.storage import LocalStorage, as_memory_file, materialize, read_bytes, Entry
from pybase.sheets import load_sheets, sheet_names


def test_roundtrip(tmp_path):
    (tmp_path / "d").mkdir()
    (tmp_path / "d" / "a.bin").write_bytes(b"hello")
    st = LocalStorage(tmp_path)
    assert read_bytes(st, "d/a.bin") == b"hello"
    bio = as_memory_file(st, "d/a.bin")
    assert bio.tell() == 0 and bio.read() == b"hello" and bio.name == "a.bin"


def test_max_bytes(tmp_path):
    (tmp_path / "a.bin").write_bytes(b"x" * 100)
    st = LocalStorage(tmp_path)
    assert len(read_bytes(st, "a.bin", max_bytes=100)) == 100
    with pytest.raises(ValueError):
        read_bytes(st, "a.bin", max_bytes=99)
    with pytest.raises(ValueError):
        as_memory_file(st, "a.bin", max_bytes=10)


def test_max_bytes_stat_wrong(tmp_path):
    class Liar(LocalStorage):
        def stat(self, path):
            return Entry("a.bin", path, False, 1)
    (tmp_path / "a.bin").write_bytes(b"x" * 100)
    with pytest.raises(ValueError):
        read_bytes(Liar(tmp_path), "a.bin", max_bytes=50)


def test_docx_and_xlsx(tmp_path):
    docx = pytest.importorskip("docx")
    openpyxl = pytest.importorskip("openpyxl")
    d = docx.Document()
    d.add_paragraph("委托单")
    d.save(tmp_path / "w.docx")
    wb = openpyxl.Workbook()
    wb.active.append(["a", 1])
    wb.save(tmp_path / "t.xlsx")
    (tmp_path / "t.csv").write_text("x,y\n1,2\n", encoding="utf-8")
    st = LocalStorage(tmp_path)
    assert docx.Document(as_memory_file(st, "w.docx")).paragraphs[0].text == "委托单"
    assert load_sheets(as_memory_file(st, "t.xlsx"))[0][1] == [["a", "1"]]
    assert sheet_names(as_memory_file(st, "t.xlsx")) == ["Sheet"]
    assert load_sheets(as_memory_file(st, "t.csv"))[0][1] == [["x", "y"], ["1", "2"]]
    assert load_sheets(io.BytesIO(b"p,q\n"), ext="csv")[0][1] == [["p", "q"]]


def test_materialize_local_no_copy(tmp_path):
    (tmp_path / "a.txt").write_text("hi")
    st = LocalStorage(tmp_path)
    with materialize(st, "a.txt") as p:
        assert p == str(tmp_path / "a.txt")
    assert (tmp_path / "a.txt").exists()


class FakeRam:
    def __init__(self, root):
        self.root = root

    def get_workspace(self):
        import tempfile
        return tempfile.mkdtemp(dir=self.root)


class FakeRemote:
    def __init__(self, files):
        self.files = files

    def open_read(self, path):
        return io.BytesIO(self.files[path])


def test_materialize_remote(tmp_path):
    ram = FakeRam(tmp_path)
    with materialize(FakeRemote({"x/委托单.docx": b"DATA"}), "x/委托单.docx", ramdisk=ram) as p:
        assert os.path.basename(p) == "委托单.docx"
        assert p.startswith(str(tmp_path))
        assert open(p, "rb").read() == b"DATA"
    assert os.listdir(tmp_path) == []
