import json
import sys
from argparse import Namespace
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import client
from targets import TargetError


def ping(system='darwin', arch='arm64', emulated=False):
    return {'runtime': {'system': system, 'arch': arch, 'emulated': emulated}}


def test_runtime_preflight():
    assert client.validate_runtime(ping(), {'native': True})['emulated'] is False
    for data, req in [(ping('linux'), {'system': 'darwin'}),
                      (ping(arch='amd64'), {'arch': 'arm64'}),
                      (ping(emulated=True), {'native': True}),
                      ({'macos': '27.0', 'arch': 'arm64'}, {'native': True})]:
        with pytest.raises(TargetError):
            client.validate_runtime(data, req)
    assert client.validate_runtime({'macos': '27.0', 'arch': 'arm64'}, {'arch': 'arm64'})


def test_task_requirement_overrides():
    desc = {'requirements': {'system': 'darwin'},
            'tasks': {'build': {'requirements': {'arch': 'arm64', 'native': True}}}}
    assert client.task_requirements(desc, 'build') == {
        'system': 'darwin', 'arch': 'arm64', 'native': True}
    for invalid in ({'requirements': []}, {'requirements': {'native': 'true'}},
                    {'requirements': {'unknown': 1}}):
        with pytest.raises((client.THError, TargetError)):
            client.task_requirements(invalid, None)


def test_selected_transport_and_unsupported_backends(tmp_path):
    registry = {'version': 1, 'default_target': 'mac',
                'hosts': {'studio': {'cpu': 8, 'memory_gb': 32}},
                'targets': {'mac': {'backend': 'ssh', 'host_id': 'studio',
                                    'system': 'darwin', 'arch': 'arm64',
                                    'ssh_host': 'custom@studio', 'ssh_key': 'custom-key'}}}
    path = tmp_path / 'targets.json'
    path.write_text(json.dumps(registry))
    args = Namespace(host='legacy', key='legacy-key', targets_file=None, target=None)
    assert client.selected_transport(args).host == 'legacy'
    assert args.execution_target is None
    args.targets_file = str(path)
    transport = client.selected_transport(args)
    assert (transport.host, transport.key) == ('custom@studio', 'custom-key')
    registry['targets']['mac']['system'] = 'linux'
    path.write_text(json.dumps(registry))
    with pytest.raises(client.THError, match='Linux'):
        client.selected_transport(args)
    registry['targets']['mac']['backend'] = 'local'
    path.write_text(json.dumps(registry))
    with pytest.raises(client.THError, match='local'):
        client.selected_transport(args)


def test_mismatch_rejected_before_sync(monkeypatch):
    monkeypatch.setattr(client, 'plan', lambda _: ({}, {'requirements': {'system': 'linux'}}, [], {}))
    class Probe:
        def __init__(self):
            self.calls = []
        def call(self, command):
            self.calls.append(command)
            assert command == 'ping'
            return ping()
    args = Namespace(task=None, tests=['test.py'], execution_target=None)
    transport = Probe()
    with pytest.raises(TargetError):
        client.cmd_run(transport, args)
    assert transport.calls == ['ping']


def test_invalid_target_cli_returns_error_without_connecting(tmp_path, monkeypatch):
    def unexpected(*args, **kwargs):
        raise AssertionError('must not connect')
    monkeypatch.setattr(client.Transport, 'call', unexpected)
    assert client.main(['--target', 'undeclared', 'ping']) == 3


def test_controlled_transport_factory_reuses_entrypoint(capsys):
    seen = []
    class Controlled:
        def call(self, command):
            assert command == 'ping'
            return {'success': True, 'host': 'controlled-guest'}
    def factory(args):
        seen.append(args.cmd)
        return Controlled()
    assert client.main(['ping'], transport_factory=factory) == 0
    assert seen == ['ping']
    assert 'controlled-guest' in capsys.readouterr().out


def test_ping_runtime_and_numeric_machine_fields(tmp_path, monkeypatch):
    monkeypatch.setenv('TESTHOST_LOCAL', str(tmp_path / 'host'))
    result = client.Transport('unused', 'unused').call('ping')
    assert type(result['ncpu']) is int and result['ncpu'] > 0
    assert type(result['mem_gb']) is int and result['mem_gb'] >= 0
    assert result['runtime']['system'] in ('darwin', 'linux')
    assert result['runtime']['arch']
    assert result['runtime']['emulated'] in (True, False, None)
