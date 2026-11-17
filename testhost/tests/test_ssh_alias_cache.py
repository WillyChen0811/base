"""An SSH alias must resolve before choosing the reusable connection socket."""
import os
import pwd
import shlex
import subprocess
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import client
from test_remote_ssh import wire, sshd, pin, raw, tool


def test_effective_alias_changes_never_reuse_previous_master(wire, monkeypatch):
    root, _, _, caller, port, _, known = wire
    config = root / 'client_config'
    original_run = subprocess.run

    def configured_run(argv, *args, **kwargs):
        if Path(argv[0]).name == 'ssh':
            argv = [argv[0], '-F', str(config), *argv[1:]]
        return original_run(argv, *args, **kwargs)

    monkeypatch.setattr(client.subprocess, 'run', configured_run)
    user = pwd.getpwuid(os.getuid()).pw_name

    def configure(hostname='127.0.0.1', username=user, target_port=port):
        config.write_text('\n'.join([
            'Host synthetic-studio', '  HostName ' + hostname,
            '  User ' + username, '  Port ' + str(target_port), '',
        ]))

    configure()
    tr = client.Transport('synthetic-studio', str(caller), host_key_alias='studio-test',
                          known_hosts=str(known))

    def control():
        return next(x for x in tr._argv('ping')[0] if x.startswith('ControlPath='))

    assert tr.call('ping', timeout=20)['runtime']['system'] == 'darwin'
    first_control = control()
    # localhost reaches the same test sshd through a different resolved HostName.
    # Both sockets must coexist: changing the alias establishes a new connection.
    configure(hostname='localhost')
    hostname_control = control()
    assert hostname_control != first_control
    assert tr.call('ping', timeout=20)['runtime']['system'] == 'darwin'
    assert Path(hostname_control.split('=', 1)[1]).is_socket()
    assert Path(first_control.split('=', 1)[1]).is_socket()
    # Changing User must reject instead of using either authenticated old master.
    configure(username='testhost_nonexistent_account')
    assert control() != first_control
    assert raw(tr, 'ping').returncode == 255

    marker = root / 'second.py'
    marker.write_text('import json; print(json.dumps({"success": True, "endpoint": "second"}))\n')
    with sshd(root / 'second', caller, shlex.join([sys.executable, str(marker)])) as (second_port, key):
        # Trust both temporary servers under the same identity, so only the
        # effective Port change distinguishes the second connection from first.
        known.write_text(known.read_text() + 'studio-test ' + Path(str(key) + '.pub').read_text())
        configure(target_port=second_port)
        assert control() != first_control
        assert tr.call('ping', timeout=20)['endpoint'] == 'second'
        second_path = control().split('=', 1)[1]
        original_run([tool('ssh'), '-S', second_path, '-O', 'exit', 'localhost'],
                     capture_output=True, timeout=10)


def test_ssh_configuration_resolution_failure_is_cli_error(monkeypatch, capsys):
    monkeypatch.delenv('TESTHOST_LOCAL', raising=False)
    def failed(argv, **kwargs):
        assert '-G' in argv
        return subprocess.CompletedProcess(argv, 255, b'', b'bad SSH configuration')
    monkeypatch.setattr(client.subprocess, 'run', failed)
    assert client.main(['--host', 'bad-studio', 'ping']) == 3
    assert 'bad SSH configuration' in capsys.readouterr().err
