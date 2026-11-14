"""Artifact protocol tests, including the old 50 MiB boundary."""
import hashlib
import io
import json
import os
import sys
import tarfile
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import client


def test_large_declared_artifact(tmp_path, monkeypatch):
    th = tmp_path / 'th'
    run = th / 'runs/a1'
    (run / 'out').mkdir(parents=True)
    artifact = run / 'out/large.bin'
    with artifact.open('wb') as f:
        f.truncate(51 * 1024**2)
    (run / 'out/outputs').write_text('out/large.bin\n')
    (run / 'out/task').write_text('build')
    monkeypatch.setenv('TESTHOST_LOCAL', str(th))
    tr = client.Transport('unused', 'unused')
    assert 'artifacts-v1' in tr.call('capabilities')['capabilities']
    output = tmp_path / 'received'
    assert client.fetch_extras(tr, 'a1', output, tree=True) == ['large.bin']
    assert (output / 'large.bin').stat().st_size == artifact.stat().st_size
    assert client.file_sha256(output / 'large.bin') == client.file_sha256(artifact)
    assert client.fetch_extras(tr, 'a1', output, tree=True) == ['large.bin']


@pytest.mark.parametrize('kind', ['hash', 'path', 'symlink', 'truncated', 'exit'])
def test_reject_bad_stream(tmp_path, kind):
    name = '../outside' if kind == 'path' else 'out/a.txt'
    digest = hashlib.sha256(b'hello').hexdigest() if kind != 'hash' else '0' * 64
    manifest = {'version': 1, 'total_bytes': 5,
                'files': [{'path': name, 'size': 5, 'sha256': digest}]}
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode='w') as tf:
        data = json.dumps(manifest).encode()
        m = tarfile.TarInfo('.testhost-artifacts.json')
        m.size = len(data)
        tf.addfile(m, io.BytesIO(data))
        m = tarfile.TarInfo(name)
        if kind == 'symlink':
            m.type = tarfile.SYMTYPE
            m.linkname = '/etc/passwd'
            tf.addfile(m)
        else:
            m.size = 5
            tf.addfile(m, io.BytesIO(b'hello'))
    raw = stream.getvalue()
    if kind == 'truncated':
        raw = raw[:1600]
    src = tmp_path / 'bad.tar'
    src.write_bytes(raw)
    class FakeTransport:
        def _argv(self, cmd):
            return [sys.executable, '-c',
                    'import sys;sys.stdout.buffer.write(open(sys.argv[1],"rb").read());sys.exit(int(sys.argv[2]))',
                    str(src), '1' if kind == 'exit' else '0'], None
    dst = tmp_path / 'received'
    with pytest.raises((client.THError, tarfile.TarError)):
        client.fetch_artifacts(FakeTransport(), 'a1', dst, True)
    assert not list(dst.glob('*.txt'))
    assert not list(dst.glob('.testhost-transfer-*'))


def test_fallback_does_not_mask_connection_errors(tmp_path):
    class Broken:
        def call(self, cmd):
            raise client.THError('SSH connection failed')
    with pytest.raises(client.THError, match='SSH'):
        client.fetch_extras(Broken(), 'a1', tmp_path, tree=True)
