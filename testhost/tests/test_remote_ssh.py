"""Real SSH wire tests: temporary keys/sshd only, no machine credentials/config.

Run on Studio after merging the direct Transport and broker implementations.
Missing OpenSSH tools skip explicitly; a present sshd that cannot start fails
with its log, so a platform problem cannot look like validated connectivity.
"""
import contextlib
import json
import os
import pwd
import select
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import client


PAYLOAD = bytes(range(256)) * 8192


def tool(name):
    found = shutil.which(name)
    if not found and Path('/usr/sbin', name).is_file():
        found = str(Path('/usr/sbin', name))
    if not found:
        pytest.skip(f'real SSH integration requires {name}')
    return found


def keypair(path):
    subprocess.run([tool('ssh-keygen'), '-q', '-t', 'ed25519', '-N', '', '-f', str(path)],
                   check=True, capture_output=True)
    return path


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def pin(path, alias, key):
    path.write_text(alias + ' ' + Path(str(key) + '.pub').read_text())
    return path


@contextlib.contextmanager
def sshd(root, caller, command):
    root.mkdir()
    hostkey = keypair(root / 'hostkey')
    auth = root / 'authorized_keys'
    auth.write_text(Path(str(caller) + '.pub').read_text())
    port = free_port()
    config = root / 'sshd_config'
    config.write_text('\n'.join([
        f'Port {port}', 'ListenAddress 127.0.0.1',
        f'HostKey {hostkey}', f'PidFile {root / "pid"}',
        f'AuthorizedKeysFile {auth}', 'StrictModes no', 'UsePAM no',
        'PasswordAuthentication no', 'KbdInteractiveAuthentication no',
        'PubkeyAuthentication yes', 'AuthenticationMethods publickey',
        'PermitRootLogin yes', 'AllowAgentForwarding no',
        'AllowTcpForwarding no', 'PermitTTY no', 'LogLevel VERBOSE',
        'ForceCommand ' + command, '',
    ]))
    process = subprocess.Popen([tool('sshd'), '-D', '-e', '-f', str(config)],
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                               start_new_session=True)
    lines = []
    try:
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            ready, _, _ = select.select([process.stderr], [], [], max(0, deadline-time.monotonic()))
            if not ready:
                break
            line = process.stderr.readline()
            if not line:
                break
            lines.append(line.decode(errors='replace'))
            if b'Server listening on' in line:
                break
        else:
            pytest.fail('temporary sshd did not become ready: ' + ''.join(lines))
        if not any('Server listening on' in line for line in lines):
            pytest.fail('temporary sshd failed: ' + ''.join(lines))
        # Drain logs continuously so SSH traffic cannot fill the pipe.
        threading.Thread(target=lambda: process.stderr.read(), daemon=True).start()
        yield port, hostkey
    finally:
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=10)
        process.stderr.close()


class Forward:
    """A raw TCP hop; it owns no SSH identity or plaintext protocol state."""
    def __init__(self, upstream):
        self.upstream = upstream
        self.listener = socket.socket()
        self.listener.bind(('127.0.0.1', 0))
        self.listener.listen()
        self.port = self.listener.getsockname()[1]
        self.connections = []
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self.accept, daemon=True)
        self.thread.start()

    def accept(self):
        while not self.stopped.is_set():
            try:
                down, _ = self.listener.accept()
                up = socket.create_connection(('127.0.0.1', self.upstream), timeout=5)
                up.settimeout(None)
            except OSError:
                return
            self.connections.extend((down, up))
            for source, target in ((down, up), (up, down)):
                threading.Thread(target=self.copy, args=(source, target), daemon=True).start()

    def copy(self, source, target):
        try:
            while data := source.recv(65536):
                target.sendall(data)
            target.shutdown(socket.SHUT_WR)
        except OSError:
            pass

    def close(self):
        self.stopped.set()
        self.listener.close()
        for sock in self.connections:
            with contextlib.suppress(OSError):
                sock.shutdown(socket.SHUT_RDWR)
            sock.close()


@pytest.fixture
def wire(monkeypatch):
    tool('ssh')
    tool('sshd')
    short = tempfile.TemporaryDirectory(prefix='thssh-', dir='/tmp')
    tmp_path = Path(short.name)
    monkeypatch.delenv('TESTHOST_LOCAL', raising=False)
    # ssh control sockets have a short Unix pathname limit, especially on macOS.
    # Transport resolves Path.home(), while sshd's account HOME remains untouched.
    monkeypatch.setenv('HOME', str(tmp_path))
    caller = keypair(tmp_path / 'caller')
    other = keypair(tmp_path / 'other')
    studio_caller = keypair(tmp_path / 'studio_caller')
    script = tmp_path / 'synthetic.py'
    script.write_text('''import json, os, shlex, sys
args = shlex.split(os.environ['SSH_ORIGINAL_COMMAND'])
if args == ['ping']:
    print(json.dumps({'success': True, 'runtime': {'system': 'darwin', 'arch': 'arm64', 'emulated': False}}))
elif args == ['sync', 'toy']:
    while data := sys.stdin.buffer.read(65536):
        sys.stdout.buffer.write(data)
    sys.stdout.buffer.flush()
elif args == ['fetch', 'toy']:
    sys.stdout.buffer.write(bytes(range(256)) * 8192)
elif args == ['status', 'failed']:
    sys.stderr.write('synthetic remote failure\\n')
    sys.exit(7)
else:
    sys.stderr.write('unexpected synthetic command\\n')
    sys.exit(8)
''')
    command = shlex.join([sys.executable, str(script)])
    with sshd(tmp_path / 'studio', studio_caller, command) as (port, hostkey):
        known = pin(tmp_path / 'studio_known_hosts', 'studio-test', hostkey)
        try:
            yield tmp_path, caller, other, studio_caller, port, hostkey, known
        finally:
            for control in (tmp_path / '.cache').rglob('cm-*'):
                if control.is_socket():
                    subprocess.run([tool('ssh'), '-S', str(control), '-O', 'exit',
                                    'localhost'], capture_output=True, timeout=10)
    short.cleanup()


def transport(key, port, alias, known):
    return client.Transport(pwd.getpwuid(os.getuid()).pw_name + '@127.0.0.1', str(key),
                            port=port, host_key_alias=alias, known_hosts=str(known))


def raw(tr, command, data=b''):
    argv, env = tr._argv(command)
    return subprocess.run(argv, env=env, input=data, capture_output=True, timeout=20)


def assert_protocol(tr):
    assert tr.call('ping', timeout=20)['runtime']['system'] == 'darwin'
    upload = raw(tr, 'sync toy', PAYLOAD)
    assert upload.returncode == 0, upload.stderr.decode(errors='replace')
    assert upload.stdout == PAYLOAD
    download = raw(tr, 'fetch toy')
    assert download.returncode == 0, download.stderr.decode(errors='replace')
    assert download.stdout == PAYLOAD
    failed = raw(tr, 'status failed')
    assert failed.returncode == 7
    assert b'synthetic remote failure' in failed.stderr


def test_direct_forwarded_ssh_identity_and_streams(wire):
    root, _, other, studio_caller, port, _, known = wire
    forward = Forward(port)
    try:
        good = transport(studio_caller, forward.port, 'studio-test', known)
        assert_protocol(good)
        # The TCP endpoint is unchanged. These must not reuse good's SSH master.
        denied = raw(transport(other, forward.port, 'studio-test', known), 'ping')
        assert denied.returncode == 255
        wrong_known = pin(root / 'wrong_known_hosts', 'studio-test', other)
        denied = raw(transport(studio_caller, forward.port, 'studio-test', wrong_known), 'ping')
        assert denied.returncode == 255
        assert b'HOST IDENTIFICATION HAS CHANGED' in denied.stderr or b'Host key verification failed' in denied.stderr
        unknown = root / 'empty_known_hosts'
        unknown.write_text('')
        assert raw(transport(studio_caller, forward.port, 'studio-test', unknown), 'ping').returncode == 255
        forward.close()
        assert raw(good, 'ping').returncode != 0
    finally:
        forward.close()


def test_ubuntu_broker_separate_credentials_and_both_host_checks(wire):
    root, caller, other, studio_caller, port, _, known = wire
    broker = Path(client.__file__).with_name('broker.py')
    assert broker.exists(), 'merge broker implementation before running integration tests'
    config = root / 'broker.json'
    config.write_text(json.dumps({
        'host': pwd.getpwuid(os.getuid()).pw_name + '@127.0.0.1',
        'key': str(studio_caller), 'port': port,
        'host_key_alias': 'studio-test', 'known_hosts': str(known),
    }))
    config.chmod(0o600)
    command = shlex.join([sys.executable, str(broker), '--config', str(config)])
    with sshd(root / 'ubuntu', caller, command) as (ubuntu_port, ubuntu_key):
        ubuntu_known = pin(root / 'ubuntu_known_hosts', 'ubuntu-test', ubuntu_key)
        tr = transport(caller, ubuntu_port, 'ubuntu-test', ubuntu_known)
        assert_protocol(tr)
        assert raw(transport(other, ubuntu_port, 'ubuntu-test', ubuntu_known), 'ping').returncode == 255
        wrong = pin(root / 'wrong_ubuntu_hosts', 'ubuntu-test', other)
        assert raw(transport(caller, ubuntu_port, 'ubuntu-test', wrong), 'ping').returncode == 255
        # Ubuntu authentication still succeeds, but it must reject Studio's key.
        # New known_hosts path gives the broker a different SSH master namespace.
        wrong_studio = pin(root / 'wrong_studio_hosts', 'studio-test', other)
        settings = json.loads(config.read_text())
        settings['known_hosts'] = str(wrong_studio)
        config.write_text(json.dumps(settings))
        denied = raw(tr, 'ping')
        assert denied.returncode != 0
        assert not denied.stdout
        assert b'Host key verification failed' in denied.stderr or b'HOST IDENTIFICATION HAS CHANGED' in denied.stderr
        settings['known_hosts'] = str(known)
        settings['key'] = str(other)
        config.write_text(json.dumps(settings))
        assert raw(tr, 'ping').returncode != 0
