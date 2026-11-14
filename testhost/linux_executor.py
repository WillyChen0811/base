"""Lima guest transport for trusted testhost dispatchers, not an arbitrary shell API."""
import hashlib
import json
import os
import re
import shlex
import subprocess
import tarfile
from pathlib import Path

from client import THError, Transport


BOOTSTRAP = r'''
import hashlib, io, json, os, pathlib, shutil, subprocess, sys, tarfile
root = pathlib.Path.home() / '.local/share/testhost'
expected = json.loads(sys.argv[1])
if subprocess.check_output(['uname', '-sm'], text=True).strip() != 'Linux aarch64':
    raise SystemExit('requires native Linux aarch64 guest')
root.mkdir(parents=True, exist_ok=True)
for name in ('bin', 'bootstrap'):
    (root / name).mkdir(exist_ok=True)
with tarfile.open(fileobj=sys.stdin.buffer, mode='r|') as tar:
    seen = set()
    for m in tar:
        if m.name not in expected or m.name in seen or not m.isfile() or m.size > 200 * 1024**2:
            raise SystemExit('unexpected bootstrap member')
        seen.add(m.name)
        dst = root / 'bootstrap' / m.name
        h = hashlib.sha256()
        with tar.extractfile(m) as source, dst.open('wb') as out:
            for block in iter(lambda: source.read(1024**2), b''):
                h.update(block); out.write(block)
        if h.hexdigest() != expected[m.name]:
            dst.unlink(); raise SystemExit('bootstrap checksum mismatch')
if seen != set(expected): raise SystemExit('bootstrap input incomplete')
subprocess.run(['bash', '-n', str(root / 'bootstrap/agent.sh')], check=True)
receipt = root / 'miniforge-source.json'
if (root / 'miniforge').exists():
    if not receipt.is_file() or json.loads(receipt.read_text()) != {'sha256': expected['miniforge.sh']}:
        raise SystemExit('existing guest miniforge provenance differs')
else:
    subprocess.run(['bash', str(root / 'bootstrap/miniforge.sh'), '-b', '-p', str(root / 'miniforge')], check=True)
    receipt.write_text(json.dumps({'sha256': expected['miniforge.sh']}))
for cmd in ('git', 'bash', 'tar', 'ps', 'pgrep', 'nproc', 'awk'):
    if shutil.which(cmd) is None: raise SystemExit('guest prerequisite missing: ' + cmd)
agent = root / 'bin/testhost'
os.replace(root / 'bootstrap/agent.sh', agent)
agent.chmod(0o700)
(root / 'bootstrap/miniforge.sh').unlink()
print(json.dumps({'success': True, 'root': str(root), 'arch': 'arm64'}))
'''

# This trampoline uses fixed home-relative state, never a caller-supplied guest path.
DISPATCH = r'''
import os, pathlib, sys
root = pathlib.Path.home() / '.local/share/testhost'
os.environ['TESTHOST_HOME'] = str(root)
os.environ['SSH_ORIGINAL_COMMAND'] = sys.argv[1]
os.environ.pop('TESTHOST_LOCAL', None)
os.environ.pop('TESTHOST_ENV_STUB', None)
os.execv('/bin/bash', ['bash', str(root / 'bin/testhost')])
'''


class LimaTransport(Transport):
    """Reuse testhost protocol, source sync, dependencies and guest process lifecycle.

    Configuration is machine-local and trusted. Project task arguments never select
    the executable, VM, home directory or dispatcher program.
    """
    ACTIONS = frozenset('ping capabilities inventory sync snapshot prepare run status fetch fetchx artifacts plugin cancel clean'.split())

    def __init__(self, limactl, lima_home, instance='studio-linux-arm64', account_home=None):
        self.limactl = Path(limactl).expanduser().resolve()
        self.lima_home = Path(lima_home).expanduser().resolve()
        if not self.limactl.is_file() or not os.access(self.limactl, os.X_OK):
            raise THError('configured limactl is not executable')
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,79}', instance):
            raise THError('invalid Lima instance')
        self.instance = instance
        if account_home is None:
            import pwd
            account_home = pwd.getpwuid(os.getuid()).pw_dir
        self.account_home = str(Path(account_home).expanduser().resolve())

    def _environment(self):
        return {'HOME': self.account_home, 'LIMA_HOME': str(self.lima_home),
                'PATH': str(self.limactl.parent) + ':/usr/bin:/bin:/usr/sbin:/sbin'}

    def _shell(self, script, *args):
        return [str(self.limactl), 'shell', self.instance, '--',
                'python3', '-c', script, *args], self._environment()

    def _argv(self, cmd):
        if not isinstance(cmd, str) or any(c in cmd for c in ('\n', '\r', '\x00')):
            raise THError('invalid guest action')
        try:
            tokens = shlex.split(cmd)
        except ValueError as exc:
            raise THError('invalid guest action') from exc
        if not tokens or tokens[0] not in self.ACTIONS:
            raise THError('unsupported guest action')
        return self._shell(DISPATCH, shlex.join(tokens))

    def bootstrap(self, agent, miniforge_installer, installer_sha256):
        """Explicit installation from a locally supplied, SHA256-pinned Linux installer.

        Does not download, apt install, start/stop VM or alter host SSH restrictions.
        Calling code must pin and document the official installer source separately.
        """
        if not re.fullmatch(r'[a-fA-F0-9]{64}', installer_sha256):
            raise THError('pin installer SHA256')
        paths = {'agent.sh': Path(agent), 'miniforge.sh': Path(miniforge_installer)}
        expected = {}
        for name, path in paths.items():
            if not path.is_file() or path.stat().st_size > 200 * 1024**2:
                raise THError('invalid bootstrap file')
            digest = hashlib.sha256()
            with path.open('rb') as f:
                for block in iter(lambda: f.read(1024**2), b''):
                    digest.update(block)
            expected[name] = digest.hexdigest()
        if expected['miniforge.sh'] != installer_sha256.lower():
            raise THError('installer SHA256 mismatch')
        argv, env = self._shell(BOOTSTRAP, json.dumps(expected))
        import tempfile
        with tempfile.TemporaryFile() as archive:
            with tarfile.open(fileobj=archive, mode='w') as tar:
                for name, path in paths.items():
                    tar.add(path, arcname=name, recursive=False)
            archive.seek(0)
            result = subprocess.run(argv, env=env, stdin=archive, capture_output=True)
        if result.returncode:
            raise THError('guest bootstrap failed: ' + result.stderr.decode(errors='replace')[-1500:])
        return json.loads(result.stdout.decode().splitlines()[-1])
