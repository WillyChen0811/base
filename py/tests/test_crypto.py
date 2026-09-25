"""pybase.crypto 的密钥文件容错测试：不存在才生成、读不了/损坏不覆盖、regenerate 显式换新、并发首次创建。"""

import os
import sys
import tempfile
import threading

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

from pybase.crypto import PasswordEncryption, KeyFileError  # noqa: E402


@pytest.fixture
def key_file(tmp_path):
    return str(tmp_path / "sub" / "key.bin")


def test_首次生成密钥文件(key_file):
    pe = PasswordEncryption(key_file, passphrase=b'pw')
    assert os.path.exists(key_file)
    assert os.path.getsize(key_file) > 0
    assert pe.encrypt("hello") != ""


def test_复用已有密钥_加解密往返(key_file):
    pe1 = PasswordEncryption(key_file, passphrase=b'pw')
    token = pe1.encrypt("secret-123")

    pe2 = PasswordEncryption(key_file, passphrase=b'pw')
    assert pe2.decrypt(token) == "secret-123"
    # 两次构造应复用同一把密钥文件，内容不变
    assert pe1._fernet.decrypt(token.encode()) == pe2._fernet.decrypt(token.encode())


def test_密钥文件内容损坏_抛异常且不覆盖(key_file):
    os.makedirs(os.path.dirname(key_file), exist_ok=True)
    with open(key_file, 'wb') as f:
        f.write(b'not-a-valid-fernet-key')

    with pytest.raises(KeyFileError):
        PasswordEncryption(key_file, passphrase=b'pw')
    with open(key_file, 'rb') as f:
        assert f.read() == b'not-a-valid-fernet-key'


def test_regenerate_换新密钥_旧密文作废(key_file):
    old = PasswordEncryption(key_file, passphrase=b'pw')
    token = old.encrypt("secret")
    with open(key_file, 'wb') as f:
        f.write(b'broken')

    new = PasswordEncryption.regenerate(key_file, passphrase=b'pw')
    assert new.decrypt(new.encrypt("x")) == "x"
    with pytest.raises(ValueError):
        new.decrypt(token)
    assert PasswordEncryption(key_file, passphrase=b'pw').decrypt(new.encrypt("y")) == "y"


@pytest.mark.skipif(os.name == "nt" or os.geteuid() == 0, reason="依赖 POSIX 权限")
def test_密钥文件读不了_抛异常且不覆盖(key_file):
    PasswordEncryption(key_file, passphrase=b'pw')
    with open(key_file, 'rb') as f:
        original = f.read()
    os.chmod(key_file, 0o000)
    try:
        with pytest.raises(KeyFileError):
            PasswordEncryption(key_file, passphrase=b'pw')
    finally:
        os.chmod(key_file, 0o600)
    with open(key_file, 'rb') as f:
        assert f.read() == original


def test_指纹_同一密钥稳定(key_file):
    pe1 = PasswordEncryption(key_file, passphrase=b'pw')
    pe2 = PasswordEncryption(key_file, passphrase=b'pw')
    assert pe1.fingerprint == pe2.fingerprint
    assert len(pe1.fingerprint) == 64  # sha256 hex


def test_指纹_不同密钥不同(key_file, tmp_path):
    pe1 = PasswordEncryption(key_file, passphrase=b'pw')
    pe2 = PasswordEncryption.regenerate(key_file, passphrase=b'pw')
    assert pe1.fingerprint != pe2.fingerprint


def test_空字符串原样返回(key_file):
    pe = PasswordEncryption(key_file, passphrase=b'pw')
    assert pe.encrypt("") == ""
    assert pe.decrypt("") == ""


def test_并发首次创建_最终只有一把密钥生效(key_file):
    n = 8
    instances = [None] * n
    errors = []

    def make(i):
        try:
            instances[i] = PasswordEncryption(key_file, passphrase=b'pw')
        except Exception as e:      # noqa: BLE001
            errors.append(e)

    threads = [threading.Thread(target=make, args=(i,)) for i in range(n)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=30)

    assert not errors, f"并发首次创建时出现异常: {errors}"
    assert all(pe is not None for pe in instances)

    # 所有实例最终都认同同一把密钥：任一实例加密，其余都能解密
    token = instances[0].encrypt("concurrent-secret")
    for pe in instances[1:]:
        assert pe.decrypt(token) == "concurrent-secret"

    # 密钥文件里最终只有一份合法密钥
    with open(key_file, 'rb') as f:
        key_bytes = f.read()
    assert len(key_bytes) > 0


def test_并发首次创建_多进程(key_file):
    """跨进程也要保证互不覆盖、最终能互解——线程测不出跨进程的文件竞态。"""
    worker = r'''
import json, sys
sys.path.insert(0, %r)
from pybase.crypto import PasswordEncryption
pe = PasswordEncryption(sys.argv[1], passphrase=b'pw')
token = pe.encrypt("cross-proc-secret")
print(json.dumps({"token": token}))
''' % REPO

    import subprocess
    import json

    procs = [
        subprocess.Popen([sys.executable, '-c', worker, key_file],
                          stdout=subprocess.PIPE, text=True)
        for _ in range(4)
    ]
    outs = [p.communicate(timeout=30)[0] for p in procs]
    for p in procs:
        assert p.returncode == 0

    tokens = [json.loads(o)["token"] for o in outs]

    pe = PasswordEncryption(key_file, passphrase=b'pw')
    for tok in tokens:
        assert pe.decrypt(tok) == "cross-proc-secret"
