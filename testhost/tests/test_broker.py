"""Broker protocol and local routing contract; real SSH coverage is separate."""
import json
import shlex
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import broker

SHA = 'a' * 64


@pytest.mark.parametrize('command', [
    'ping', 'inventory', 'capabilities', 'update', 'media-spec',
    'sync base', 'snapshot base-a123', 'status run1', 'fetch run1',
    'fetchx run1', 'artifacts run1', 'cancel run1', 'plugin run1 helper.py',
    'prepare run1 base=abcdef0 --env testhost/testenv.json --snap fixture=one',
    'run run1 base -n 2 -k "foo and not bar" tests/test_x.py',
    'run run1 base --plugin-env "OUT={run}/out/a b" --task build -- "--arg=$HOME; echo hi"',
    'clean runs --keep 2', 'clean run run1', 'clean envs', 'clean snapshots', 'clean repos',
    f'media-put {SHA} "录音 1.mp4" --size 123',
    f'media-audio {SHA} --segment-min 0.01',
    f'media-frames {SHA} --at 0,1000 --width 960',
    f'media-clean {SHA}', 'media-clean --older-than 1.5',
])
def test_existing_protocol_commands(command):
    assert shlex.split(broker.command_line(command)) == shlex.split(command)


@pytest.mark.parametrize('command', [
    '', 'bash -c whoami', 'ping; id', 'ping && id', 'ping extra', 'ping\nrun',
    'status ../../foo', 'sync -option', 'status', 'plugin run1 ../../evil.py',
    'prepare run1 --env foo', 'run run1 /tmp', 'run run1 ../base',
    'clean /tmp', 'clean run ../../foo', 'clean runs --keep many',
    f'media-put {SHA}', 'media-clean ../../file', "run run1 base 'unterminated",
])
def test_reject_non_protocol_commands(command):
    with pytest.raises(broker.BrokerError):
        broker.command_line(command)


def test_shell_metacharacters_are_literal_arguments():
    args = ['run', 'r1', 'base', '--task', 'build', '--', '$(touch /tmp/no)', ';id', '`id`', "a'b"]
    canonical = broker.command_line(shlex.join(args))
    assert shlex.split(canonical) == args
    assert "'$(touch /tmp/no)'" in canonical


def test_local_config_and_upstream_validation(tmp_path):
    path = tmp_path / 'broker.json'
    config = {'host': 'svc@studio', 'key': '~/key', 'known_hosts': '~/known',
              'port': 12222, 'host_key_alias': 'studio'}
    path.write_text(json.dumps(config))
    parsed = broker.load_config(path)
    assert parsed['host'] == 'svc@studio'
    assert parsed['key'] == str(Path.home() / 'key')
    for bad in ({}, {**config, 'port': True}, {**config, 'host': '-oProxyCommand=id'},
                {**config, 'host': 'studio extra'}, {**config, 'extra': 'no'},
                {**config, 'known_hosts': ''}, {**config, 'host_key_alias': 'a b'},
                {**config, 'host_key_alias': 'a\x00b'}, {**config, 'key': '/key\nbad'},
                {**config, 'known_hosts': '/known\x00bad'}):
        path.write_text(json.dumps(bad))
        with pytest.raises(broker.BrokerError):
            broker.load_config(path)


def test_relay_uses_pinned_upstream_and_ignores_local_test_mode(monkeypatch):
    calls = []

    class Transport:
        def __init__(self, host, key, **kwargs):
            calls.append((host, key, kwargs))
            self.local = '/should/not/run'

        def _argv(self, command):
            assert self.local is None
            return ['ssh', 'configured-upstream', command], None

    monkeypatch.setattr(broker, 'Transport', Transport)
    config = {'host': 'configured', 'key': '/key', 'known_hosts': '/known',
              'port': 12222, 'host_key_alias': 'studio'}
    argv = broker.relay_argv(config, 'sync base')
    assert calls == [('configured', '/key', {'port': 12222, 'host_key_alias': 'studio', 'known_hosts': '/known'})]
    assert argv == ['ssh', 'configured-upstream', 'sync base']
    with pytest.raises(broker.BrokerError):
        broker.relay_argv(config, 'ssh attacker')


def test_main_exec_preserves_standard_streams_and_exit_status(tmp_path, monkeypatch):
    path = tmp_path / 'broker.json'
    path.write_text(json.dumps({'host': 'studio', 'key': '/key', 'known_hosts': '/known'}))
    monkeypatch.setenv('SSH_ORIGINAL_COMMAND', 'artifacts r1')
    monkeypatch.setattr(broker, 'relay_argv', lambda config, command: ['ssh', 'studio', command])
    calls = []

    class ExecReplaced(BaseException):
        pass

    def execvp(program, args):
        calls.append((program, args))
        raise ExecReplaced

    monkeypatch.setattr(broker.os, 'execvp', execvp)
    with pytest.raises(ExecReplaced):
        broker.main(['--config', str(path)])
    assert calls == [('ssh', ['ssh', 'studio', 'artifacts r1'])]


def test_invalid_request_fails_before_upstream_spawn(tmp_path, monkeypatch, capsys):
    path = tmp_path / 'broker.json'
    path.write_text(json.dumps({'host': 'studio', 'key': '/key', 'known_hosts': '/known'}))
    monkeypatch.setenv('SSH_ORIGINAL_COMMAND', 'bash')
    monkeypatch.setattr(broker.os, 'execvp', lambda *args: pytest.fail('must not connect'))
    assert broker.main(['--config', str(path)]) == 3
    assert json.loads(capsys.readouterr().err)['code'] == 'broker_error'
