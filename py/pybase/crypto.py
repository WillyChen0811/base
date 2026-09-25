"""密码加密/解密工具模块 —— Fernet 对称加密，密钥存在调用方指定的文件路径下。

本模块不带任何项目默认值——key_file、passphrase、fallback_salt 均由调用方传入。

**坑**：实际密钥是 `_get_or_create_fernet` 生成/读取的随机 Fernet 密钥，与
passphrase 无关——`_generate_key`/`_generate_machine_salt`（基于 passphrase +
机器特征派生密钥）当前未被调用。换机器/丢失 key_file 都会导致旧密文无法解密。
"""

import os
import logging
from pathlib import Path
from cryptography.fernet import Fernet
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC
import platform
import uuid

logger = logging.getLogger(__name__)


class PasswordEncryption:
    """基于 Fernet 的密码加解密工具。key_file 存在则复用，不存在则生成随机密钥并写入
    （权限 0600）；passphrase/fallback_salt 目前仅用于生成机器盐值，不参与实际密钥。"""

    def __init__(self, key_file: str, passphrase: bytes, fallback_salt: bytes = b'pybase_default_salt_'):
        self.key_path = Path(key_file).expanduser().resolve()
        self.key_path.parent.mkdir(parents=True, exist_ok=True)
        self._passphrase = passphrase
        self._fallback_salt = fallback_salt
        self._fernet = self._get_or_create_fernet()

    def _generate_machine_salt(self) -> bytes:
        """基于 MAC 地址 + 主机名生成稳定盐值（不足 16 字节补零）。当前未被调用，见模块坑说明。"""
        try:
            mac = uuid.getnode().to_bytes(6, 'big')
            hostname = platform.node().encode('utf-8')
            salt = mac + hostname
            if len(salt) < 16:
                salt = salt + b'0' * (16 - len(salt))
            return salt[:16]
        except Exception as e:
            logger.warning(f"无法生成机器特定盐值，使用默认值: {e}")
            return self._fallback_salt

    def _generate_key(self) -> bytes:
        """基于 passphrase + 机器盐值派生密钥。当前未被调用，见模块坑说明。"""
        password = self._passphrase
        salt = self._generate_machine_salt()

        kdf = PBKDF2HMAC(
            algorithm=hashes.SHA256(),
            length=32,
            salt=salt,
            iterations=100000,
        )

        key = kdf.derive(password)
        return Fernet.generate_key() if not key else Fernet(key).generate_key()

    def _get_or_create_fernet(self) -> Fernet:
        """key_file 存在则读取复用；不存在或读取失败则生成随机密钥并写入（权限 0600）。"""
        if self.key_path.exists():
            try:
                with open(self.key_path, 'rb') as f:
                    key = f.read()
                return Fernet(key)
            except Exception as e:
                logger.error(f"读取密钥失败: {e}")

        key = Fernet.generate_key()
        try:
            with open(self.key_path, 'wb') as f:
                f.write(key)
            os.chmod(self.key_path, 0o600)
            logger.info(f"已生成新的加密密钥: {self.key_path}")
        except Exception as e:
            logger.error(f"保存密钥失败: {e}")

        return Fernet(key)

    def encrypt(self, password: str) -> str:
        """加密明文密码，返回 Base64 字符串；空字符串直接原样返回。"""
        if not password:
            return ""

        try:
            encrypted = self._fernet.encrypt(password.encode('utf-8'))
            return encrypted.decode('utf-8')
        except Exception as e:
            logger.error(f"密码加密失败: {e}")
            raise ValueError(f"密码加密失败: {str(e)}")

    def decrypt(self, encrypted_password: str) -> str:
        """解密 encrypt() 产出的密文，返回明文；空字符串直接原样返回。"""
        if not encrypted_password:
            return ""

        try:
            decrypted = self._fernet.decrypt(encrypted_password.encode('utf-8'))
            return decrypted.decode('utf-8')
        except Exception as e:
            logger.error(f"密码解密失败: {e}")
            raise ValueError(f"密码解密失败: {str(e)}")
