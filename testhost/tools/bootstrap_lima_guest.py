"""Bootstrap the existing Lima arm64 guest through a declared testhost task."""
import argparse
import json
import os
import pwd
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from linux_executor import LimaTransport
from tools.install_lima import download

VERSION = '26.9.4-2'
SHA256 = '1adf18b530e4ffb00886ce6d5bd2eb1fa6311d2f14709b13bd154d8cd98d94c8'
URL = f'https://github.com/conda-forge/miniforge/releases/download/{VERSION}/Miniforge3-{VERSION}-Linux-aarch64.sh'


def main():
    parser = argparse.ArgumentParser(__doc__)
    parser.add_argument('--installer-file', help='Optional pinned installer provided via snapshot')
    args = parser.parse_args()
    home = Path(pwd.getpwuid(os.getuid()).pw_dir)
    root = home / 'xxyi-testhost'
    installer = root / 'downloads' / (SHA256 + '.sh')
    download(URL, installer, SHA256, args.installer_file)
    transport = LimaTransport(root / 'tools/lima-2.3.0/bin/limactl',
                              root / 'lima', account_home=home)
    agent = Path(__file__).resolve().parents[1] / 'mac_testhost.sh'
    result = transport.bootstrap(agent, installer, SHA256)
    print(json.dumps({'bootstrap': result, 'runtime': transport.call('ping'),
                      'miniforge': {'version': VERSION, 'url': URL, 'sha256': SHA256}}, indent=2))


if __name__ == '__main__':
    main()
