"""Run as a declared testhost task; installs only in the service account's testhost tree.

Required: --version (without v), --archive-sha256, --image-url, --image-sha256.
Root reviewer must pin these against official release metadata before execution.
No sudo, Homebrew, GUI, VM deletion, autostart, or SSH policy changes.
"""
import argparse
import hashlib
import json
import os
import platform
import pwd
import re
import subprocess
import tarfile
import tempfile
import urllib.request
from pathlib import Path


def download(url, path, expected):
    digest = hashlib.sha256()
    with urllib.request.urlopen(url, timeout=60) as src, path.open('wb') as dst:
        while block := src.read(1024 * 1024):
            dst.write(block)
            digest.update(block)
    if digest.hexdigest() != expected.lower():
        raise RuntimeError('Checksum mismatch: ' + path.name)


def installed_files(directory):
    result = {}
    for path in sorted(directory.rglob('*')):
        if path.name == '.testhost-source.json':
            continue
        name = str(path.relative_to(directory))
        if path.is_symlink():
            result[name] = {'link': os.readlink(path)}
        elif path.is_file():
            digest = hashlib.sha256()
            with path.open('rb') as stream:
                while block := stream.read(1024 * 1024):
                    digest.update(block)
            result[name] = {'sha256': digest.hexdigest()}
    return result


def main():
    p = argparse.ArgumentParser(__doc__)
    for key in ('version', 'archive-sha256', 'image-url', 'image-sha256'):
        p.add_argument('--' + key, required=True)
    p.add_argument('--start', action='store_true')
    args = p.parse_args()
    if not re.fullmatch(r'\d+\.\d+\.\d+', args.version):
        raise SystemExit('Pin a stable numeric release version')
    if not all(re.fullmatch(r'[a-fA-F0-9]{64}', x) for x in
               (args.archive_sha256, args.image_sha256)):
        raise SystemExit('Both SHA256 values must be 64 hex characters')
    if not args.image_url.startswith('https://'):
        raise SystemExit('Use an HTTPS image URL')
    if platform.system() != 'Darwin' or platform.machine() != 'arm64':
        raise SystemExit('Requires Apple Silicon macOS')
    home = Path(pwd.getpwuid(os.getuid()).pw_dir)
    root = home / 'xxyi-testhost'
    install = root / 'tools' / ('lima-' + args.version)
    state = root / 'lima'
    url = f'https://github.com/lima-vm/lima/releases/download/v{args.version}/lima-{args.version}-Darwin-arm64.tar.gz'
    expected_source = {'url': url, 'sha256': args.archive_sha256.lower()}
    env = os.environ.copy()
    env.update(HOME=str(home), LIMA_HOME=str(state),
               PATH=str(install / 'bin') + ':/usr/bin:/bin:/usr/sbin:/sbin')
    if install.exists():
        receipt = install / '.testhost-source.json'
        if not receipt.is_file():
            raise SystemExit('Existing installation has no provenance receipt; refusing reuse')
        recorded = json.loads(receipt.read_text())
        if recorded.get('source') != expected_source or recorded.get('files') != installed_files(install):
            raise SystemExit('Existing installation provenance or file checksums differ')
    else:
        install.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=install.parent) as tmp:
            stage = Path(tmp)
            archive = stage / 'lima.tar.gz'
            download(url, archive, args.archive_sha256)
            dest = stage / 'unpack'
            dest.mkdir()
            with tarfile.open(archive) as tar:
                for member in tar.getmembers():
                    if member.name.startswith('/') or '..' in Path(member.name).parts:
                        raise RuntimeError('Unsafe archive path')
                    if member.issym() or member.islnk():
                        if member.linkname.startswith('/') or '..' in Path(member.linkname).parts:
                            raise RuntimeError('Unsafe archive link')
                tar.extractall(dest, filter='data')
            (dest / '.testhost-source.json').write_text(json.dumps({
                'source': expected_source, 'files': installed_files(dest)}, indent=2) + '\n')
            dest.rename(install)
    lima = str(install / 'bin/limactl')
    subprocess.run([lima, '--version'], env=env, check=True)
    config = root / 'config' / 'studio-linux-arm64.yaml'
    config.parent.mkdir(parents=True, exist_ok=True)
    # JSON is valid YAML; explicit mounts prevent default access to the real account home.
    spec = {'vmType': 'vz', 'arch': 'aarch64', 'cpus': 8, 'memory': '16GiB',
            'disk': '100GiB', 'mounts': [],
            'images': [{'location': args.image_url, 'arch': 'aarch64',
                        'digest': 'sha256:' + args.image_sha256}],
            'containerd': {'system': False, 'user': False}}
    if config.exists() and json.loads(config.read_text()) != spec:
        raise SystemExit('Existing VM config differs; refusing silent reconfiguration')
    config.write_text(json.dumps(spec, indent=2) + '\n')
    subprocess.run([lima, 'validate', str(config)], env=env, check=True)
    if args.start:
        subprocess.run([lima, 'start', '--tty=false', '--name=studio-linux-arm64',
                        str(config)], env=env, check=True, timeout=1200)
        subprocess.run([lima, 'shell', 'studio-linux-arm64', 'uname', '-sm'],
                       env=env, check=True, timeout=60)
    print(json.dumps({'installed': str(install), 'config': str(config),
                      'started': args.start}))


if __name__ == '__main__':
    main()
