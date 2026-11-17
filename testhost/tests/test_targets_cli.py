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


@pytest.mark.parametrize('machine', ['aarch64', 'x86_64'])
def test_linux_uname_cannot_prove_native(machine, monkeypatch, capsys):
    import platform
    script = client.ENTRY.read_text().split("<<'PYRUNTIME'\n", 1)[1].split('\nPYRUNTIME', 1)[0]
    monkeypatch.setattr(platform, 'system', lambda: 'Linux')
    monkeypatch.setattr(platform, 'machine', lambda: machine)
    exec(compile(script, str(client.ENTRY), 'exec'), {})
    runtime = json.loads(capsys.readouterr().out)
    assert runtime['emulated'] is None
    assert client.validate_runtime({'runtime': runtime}, {'system': 'linux', 'arch': machine})
    with pytest.raises(TargetError, match='verified runtime proof'):
        client.validate_runtime({'runtime': runtime}, {'native': True})



def test_forwarded_ssh_options_and_control_identity(tmp_path, monkeypatch):
    monkeypatch.delenv('TESTHOST_LOCAL', raising=False)
    monkeypatch.setenv('HOME', str(tmp_path))
    hosts = str(tmp_path / "known hosts")
    tr = client.Transport('tester@127.0.0.1', 'remote-key', port=2222,
                          host_key_alias='studio', known_hosts=hosts)
    argv, env = tr._argv('ping')
    assert env is None
    assert argv[-2:] == ['tester@127.0.0.1', 'ping']
    assert argv[argv.index('-p') + 1] == '2222'
    assert 'StrictHostKeyChecking=yes' in argv
    assert 'HostKeyAlias=studio' in argv
    assert '-T' in argv
    # Let OpenSSH itself parse the options, including a path containing spaces.
    import subprocess
    expanded = subprocess.run(argv[:-2] + ['-G', tr.host], text=True, capture_output=True, check=True).stdout
    assert 'port 2222\n' in expanded
    assert 'hostkeyalias studio\n' in expanded
    assert 'stricthostkeychecking true\n' in expanded or 'stricthostkeychecking yes\n' in expanded
    assert 'userknownhostsfile "' + hosts + '"' in expanded or 'userknownhostsfile ' + hosts in expanded
    def control(transport):
        return next(x for x in transport._argv('ping')[0] if x.startswith('ControlPath='))
    assert control(tr) != control(client.Transport(tr.host, tr.key, port=2222, host_key_alias='other', known_hosts=hosts))
    assert control(tr) != control(client.Transport(tr.host, tr.key, port=2222, host_key_alias='studio', known_hosts=hosts + '.other'))
    assert control(tr) != control(client.Transport(tr.host, 'other-key', port=2222, host_key_alias='studio', known_hosts=hosts))


def test_forward_options_cli_and_registry_override(tmp_path, monkeypatch):
    registry = {'version': 1, 'default_target': 'mac',
                'hosts': {'studio': {'cpu': 8, 'memory_gb': 32}},
                'targets': {'mac': {'backend': 'ssh', 'host_id': 'studio',
                                    'system': 'darwin', 'arch': 'arm64',
                                    'ssh_host': 'tester@localhost', 'ssh_port': 2222,
                                    'ssh_host_key_alias': 'studio', 'ssh_known_hosts': str(tmp_path / 'hosts')}}}
    path = tmp_path / 'targets.json'
    path.write_text(json.dumps(registry))
    args = Namespace(host='legacy', key='legacy-key', targets_file=str(path), target=None)
    tr = client.selected_transport(args)
    assert (tr.port, tr.host_key_alias, tr.known_hosts) == (2222, 'studio', str(tmp_path / 'hosts'))
    args.port, args.host_key_alias, args.known_hosts = 2223, 'other', str(tmp_path / 'other')
    tr = client.selected_transport(args)
    assert (tr.port, tr.host_key_alias, tr.known_hosts) == (2223, 'other', str(tmp_path / 'other'))
    seen = []
    def factory(args):
        seen.append((args.port, args.host_key_alias, args.known_hosts))
        class Probe:
            def call(self, cmd):
                return {'success': True}
        return Probe()
    assert client.main(['--port', '2224', '--host-key-alias', 'studio', '--known-hosts', str(path),
                        'ping'], transport_factory=factory) == 0
    assert seen == [(2224, 'studio', str(path))]


@pytest.mark.parametrize('args', [['--port', '0'], ['--port', '65536'],
                                  ['--host-key-alias', 'bad alias'], ['--known-hosts', 'bad\nfile']])
def test_bad_forward_cli_never_connects(args, monkeypatch):
    def unexpected(*args, **kwargs):
        raise AssertionError('must not connect')
    monkeypatch.setattr(client.Transport, 'call', unexpected)
    assert client.main(args + ['ping']) == 3



def test_control_path_fits_ssh_unix_socket_limit(monkeypatch):
    # OpenSSH adds a dot and 16 random characters while creating its listener.
    # Keep the actual Ubuntu home path within the stricter macOS 104-byte bound.
    monkeypatch.delenv('TESTHOST_LOCAL', raising=False)
    monkeypatch.setattr(client.Path, 'home', classmethod(lambda cls: Path('/home/chenwy')))
    monkeypatch.setattr(client.Path, 'mkdir', lambda *args, **kwargs: None)
    argv, _ = client.Transport('tester@studio', 'key', port=2222, host_key_alias='studio')._argv('ping')
    path = next(x.split('=', 1)[1] for x in argv if x.startswith('ControlPath='))
    assert '%C' not in path
    assert len((path + '.' + 'x' * 16).encode()) < 104
