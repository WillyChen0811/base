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
import shutil
import tarfile
import tempfile
import time
import urllib.request
from pathlib import Path


def sha256(path):
    digest = hashlib.sha256()
    with path.open('rb') as src:
        while block := src.read(1024 * 1024):
            digest.update(block)
    return digest.hexdigest()


def download(url, path, expected, local=None):
    """Reuse only verified cache entries; keep partial downloads out of the cache."""
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.is_file() and sha256(path) == expected.lower():
        print('Verified cached resource: ' + path.name, flush=True)
        return
    if local:
        source = Path(local).resolve(strict=True)
        if not source.is_file() or sha256(source) != expected.lower():
            raise RuntimeError('Local resource checksum mismatch: ' + source.name)
        with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as dst:
            temp = Path(dst.name)
            try:
                with source.open('rb') as src:
                    shutil.copyfileobj(src, dst, 1024 * 1024)
            except BaseException:
                temp.unlink(missing_ok=True)
                raise
        if sha256(temp) != expected.lower():
            temp.unlink(missing_ok=True)
            raise RuntimeError('Copied resource checksum mismatch')
        temp.replace(path)
        print('Verified local resource: ' + path.name, flush=True)
        return
    for attempt in range(1, 4):
        print(f'Download {path.name}: attempt {attempt}/3 (30s socket timeout)', flush=True)
        temp = None
        try:
            started = time.monotonic()
            progress = started
            size = 0
            digest = hashlib.sha256()
            with urllib.request.urlopen(url, timeout=30) as src, tempfile.NamedTemporaryFile(
                    dir=path.parent, delete=False) as dst:
                temp = Path(dst.name)
                while block := src.read(1024 * 1024):
                    size += len(block)
                    if size > 4 * 1024**3 or time.monotonic() - started > 900:
                        raise RuntimeError('Resource exceeds 4GiB/900s download bound')
                    dst.write(block)
                    digest.update(block)
                    if time.monotonic() - progress > 10:
                        print(f'Download {path.name}: {size // 1024**2} MiB', flush=True)
                        progress = time.monotonic()
            if digest.hexdigest() != expected.lower():
                raise RuntimeError('Downloaded resource checksum mismatch')
            temp.replace(path)
            print(f'Verified download: {path.name} ({size} bytes)', flush=True)
            return
        except (OSError, RuntimeError) as exc:
            if temp:
                temp.unlink(missing_ok=True)
            print(f'Download attempt {attempt} failed: {type(exc).__name__}', flush=True)
            if attempt == 3:
                raise RuntimeError('Download failed after 3 attempts; supply verified local resource') from exc


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
    p.add_argument('--archive-file', help='Verified Lima archive supplied through a testhost snapshot')
    p.add_argument('--image-file', help='Verified Ubuntu image supplied through a testhost snapshot')
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
    cache = root / 'downloads'
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
            archive = cache / (args.archive_sha256.lower() + '.tar.gz')
            download(url, archive, args.archive_sha256, args.archive_file)
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
    image = cache / (args.image_sha256.lower() + '.img')
    download(args.image_url, image, args.image_sha256, args.image_file)
    # JSON is valid YAML; explicit mounts prevent default access to the real account home.
    spec = {'vmType': 'vz', 'arch': 'aarch64', 'cpus': 8, 'memory': '16GiB',
            'disk': '100GiB', 'mounts': [],
            'images': [{'location': str(image), 'arch': 'aarch64',
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
