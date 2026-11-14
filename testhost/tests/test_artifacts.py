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
    with pytest.raises(client.THError):
        client.fetch_artifacts(FakeTransport(), 'a1', dst, True)
    assert not list(dst.glob('*.txt'))
    assert not list(dst.glob('.testhost-transfer-*'))


def test_fallback_does_not_mask_connection_errors(tmp_path):
    class Broken:
        def call(self, cmd):
            raise client.THError('SSH connection failed')
    with pytest.raises(client.THError, match='SSH'):
        client.fetch_extras(Broken(), 'a1', tmp_path, tree=True)


@pytest.mark.parametrize('kind', ['empty', 'truncated', 'invalid-json'])
def test_bad_transfer_cli_preserves_files_and_reports_channel_error(tmp_path, capsys, kind):
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode='w') as tf:
        data = b'{' if kind == 'invalid-json' else json.dumps({
            'version': 1, 'total_bytes': 5, 'files': [{
                'path': 'out/a.txt', 'size': 5,
                'sha256': hashlib.sha256(b'hello').hexdigest()}]}).encode()
        member = tarfile.TarInfo('.testhost-artifacts.json')
        member.size = len(data)
        tf.addfile(member, io.BytesIO(data))
        member = tarfile.TarInfo('out/a.txt')
        member.size = 5
        tf.addfile(member, io.BytesIO(b'hello'))
    raw = stream.getvalue()
    raw = b'' if kind == 'empty' else raw[:1540] if kind == 'truncated' else raw
    src = tmp_path / 'wire'
    src.write_bytes(raw)
    class Failed:
        def call(self, cmd):
            return {'task': 'build'} if cmd.startswith('status') else {'capabilities': ['artifacts-v1']}
        def _argv(self, cmd):
            return [sys.executable, '-c',
                    'import sys;sys.stderr.write("connection lost\\n");sys.stderr.flush();'
                    'sys.stdout.buffer.write(open(sys.argv[1],"rb").read());sys.exit(255)', str(src)], None
    out = tmp_path / 'received'
    out.mkdir()
    existing = out / 'a.txt'
    existing.write_bytes(b'previous package')
    assert client.main(['fetch', 'a1', '--out-dir', str(out)],
                       transport_factory=lambda _: Failed()) == 3
    error = capsys.readouterr().err
    assert 'connection lost' in error
    assert 'Traceback' not in error
    assert existing.read_bytes() == b'previous package'
    assert not list(out.glob('.testhost-transfer-*'))
