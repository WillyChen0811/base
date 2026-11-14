"""The guest bridge accepts protocol actions, never caller-selected shell commands."""
import hashlib
import json
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from linux_executor import LimaTransport, THError


@pytest.fixture
def transport(tmp_path):
    lima = tmp_path / 'limactl'
    lima.write_text('#!/bin/sh\nexit 0\n')
    lima.chmod(0o755)
    return LimaTransport(lima, tmp_path / 'state', account_home=tmp_path)


@pytest.mark.parametrize('action', ['ping', 'prepare r1 repo=abc --env testhost.json',
                                  'run r1 repo --task build', 'cancel r1', 'artifacts r1'])
def test_protocol_forwarding(transport, action):
    argv, env = transport._argv(action)
    assert argv[1:4] == ['shell', 'studio-linux-arm64', '--']
    assert argv[4:6] == ['python3', '-c']
    assert argv[-1] == action
    assert set(env) == {'HOME', 'LIMA_HOME', 'PATH'}
    assert 'SSH_ORIGINAL_COMMAND' in argv[-2]


@pytest.mark.parametrize('action', ['bash -c id', 'update', 'ping\nrun r1 repo',
                                  'ping\x00', 'sh', '', "run 'unterminated"])
def test_reject_actions(transport, action):
    with pytest.raises(THError):
        transport._argv(action)


def test_invalid_instance(tmp_path):
    lima = tmp_path / 'limactl'
    lima.touch(mode=0o700)
    with pytest.raises(THError, match='instance'):
        LimaTransport(lima, tmp_path, instance='bad;command')


def test_bootstrap_hash_before_guest(transport, tmp_path):
    agent = tmp_path / 'agent'
    agent.write_text('agent')
    installer = tmp_path / 'installer'
    installer.write_text('installer')
    with pytest.raises(THError, match='SHA256 mismatch'):
        transport.bootstrap(agent, installer, '0' * 64)


def test_bootstrap_pinned_input(transport, tmp_path, monkeypatch):
    agent = tmp_path / 'agent'
    agent.write_text('agent')
    installer = tmp_path / 'installer'
    installer.write_text('installer')
    def fake_run(argv, **kwargs):
        assert argv[1:4] == ['shell', 'studio-linux-arm64', '--']
        expected = json.loads(argv[-1])
        assert expected['miniforge.sh'] == hashlib.sha256(b'installer').hexdigest()
        assert kwargs['stdin'].read(3) != b''
        return SimpleNamespace(returncode=0, stdout=b'installer output\n{"success":true}\n', stderr=b'')
    monkeypatch.setattr('linux_executor.subprocess.run', fake_run)
    assert transport.bootstrap(agent, installer, hashlib.sha256(b'installer').hexdigest())['success']
