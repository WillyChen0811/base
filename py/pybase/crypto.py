"""密码加密/解密工具模块 —— Fernet 对称加密，密钥存在调用方指定的文件路径下。

本模块不带任何项目默认值——key_file、passphrase、fallback_salt 均由调用方传入。

**坑**：实际密钥是 `_get_or_create_key` 生成/读取的随机 Fernet 密钥，与
passphrase 无关。key_file 不存在时自动生成；存在但读不了/损坏时抛 KeyFileError，由用户决定是否调
`regenerate` 换新密钥（旧密文作废、用户重新输入密码）。`fingerprint` 是当前密钥的
SHA-256，库不存，调用方存起来跟旧值比对就能查出"密钥被换过"（包括 key_file 丢失后
被自动重建成新密钥这种情况）。
"""

import os
import hashlib
import logging
import platform
import tempfile
from pathlib import Path
from cryptography.fernet import Fernet

logger = logging.getLogger(__name__)


class KeyFileError(Exception):
    """key_file 读不了、内容无效或写入失败。库不自动重新生成——读不了可能是暂时的，
    换不换由用户决定（先重启重试，仍不行再调 `PasswordEncryption.regenerate`）。"""


class PasswordEncryption:
    """基于 Fernet 的密码加解密工具。

    key_file 不存在时生成随机密钥并原子写入（多进程首次并发启动也只会有一份密钥生效）；
    存在则复用；读不了或内容无效抛 KeyFileError，绝不自动覆盖。
    """

    def __init__(self, key_file: str, passphrase: bytes, fallback_salt: bytes = b'pybase_default_salt_'):
        """passphrase/fallback_salt 已不使用，保留仅为兼容调用方签名。"""
        self.key_path = Path(key_file).expanduser().resolve()
        self.key_path.parent.mkdir(parents=True, exist_ok=True)
        self._passphrase = passphrase
        self._fallback_salt = fallback_salt
        self._key = self._get_or_create_key()
        self._fernet = Fernet(self._key)

    @property
    def fingerprint(self) -> str:
        """当前密钥的 SHA-256 指纹（hex）。库不存这个值，调用方存起来跟旧指纹比对，
        判断密钥是不是被换过。"""
        return hashlib.sha256(self._key).hexdigest()

    @classmethod
    def regenerate(cls, key_file: str, passphrase: bytes, fallback_salt: bytes = b'pybase_default_salt_'):
        """删除现有 key_file 并生成新密钥，旧密文全部作废。只在用户确认更换后调用。"""
        path = Path(key_file).expanduser().resolve()
        try:
            path.unlink()
        except FileNotFoundError:
            pass
        except OSError as e:
            raise KeyFileError(f"删除密钥文件 {path} 失败: {e}") from e
        return cls(key_file, passphrase, fallback_salt)

    def _secure_permissions(self, path: Path) -> None:
        """限制密钥文件只有当前用户可读。Windows 上 chmod 管不住其他账户，改用 icacls；
        失败只记警告，不阻断（密钥仍然能用，只是权限没收紧）。"""
        if platform.system() == 'Windows':
            try:
                import subprocess
                username = os.environ.get('USERNAME') or os.getlogin()
                subprocess.run(
                    ['icacls', str(path), '/inheritance:r', '/grant:r', f'{username}:F'],
                    check=True, capture_output=True,
                )
            except Exception as e:
                logger.warning(f"设置密钥文件 Windows 权限失败: {e}")
        else:
            os.chmod(path, 0o600)

    def _get_or_create_key(self) -> bytes:
        """key_file 存在则读取复用（读不了/内容无效抛 KeyFileError）。
        不存在则原子创建（临时文件 + os.link，目标已存在会失败，POSIX/NTFS 均支持），
        多进程并发首次启动时，抢先失败的一方改为读取已创建的密钥，不覆盖。"""
        if self.key_path.exists():
            try:
                key = self.key_path.read_bytes()
            except OSError as e:
                raise KeyFileError(f"密钥文件 {self.key_path} 读取失败: {e}") from e
            try:
                Fernet(key)  # 校验格式，不接住返回值——只是为了在无效时抛错
            except ValueError as e:
                raise KeyFileError(f"密钥文件 {self.key_path} 内容无效: {e}") from e
            return key

        key = Fernet.generate_key()
        fd, tmp_name = tempfile.mkstemp(
            dir=str(self.key_path.parent), prefix=f".{self.key_path.name}.", suffix=".tmp"
        )
        try:
            with os.fdopen(fd, 'wb') as f:
                f.write(key)
            self._secure_permissions(Path(tmp_name))

            try:
                os.link(tmp_name, self.key_path)
                logger.info(f"已生成新的加密密钥: {self.key_path}")
            except FileExistsError:
                # 其他进程已抢先完成首次创建，读取它写入的密钥，不覆盖
                try:
                    key = self.key_path.read_bytes()
                except Exception as e:
                    raise KeyFileError(
                        f"检测到并发创建的密钥文件 {self.key_path}，但读取失败: {e}"
                    ) from e
        except OSError as e:
            raise KeyFileError(f"写入密钥文件 {self.key_path} 失败: {e}") from e
        finally:
            try:
                os.unlink(tmp_name)
            except FileNotFoundError:
                pass

        return key

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
