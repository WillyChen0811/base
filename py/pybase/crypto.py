"""密码加密/解密工具模块

使用 Fernet 对称加密来安全存储 SMB 密码。
密钥基于机器特征生成，存储在用户目录下。
"""

import os
import logging
from pathlib import Path
from typing import Optional
from cryptography.fernet import Fernet
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC
import platform
import uuid

logger = logging.getLogger(__name__)


class PasswordEncryption:
    """密码加密/解密工具类"""

    def __init__(self, key_file: str = "~/.filecore/encryption.key"):
        """初始化加密工具
        
        Args:
            key_file: 加密密钥文件路径
        """
        self.key_path = Path(key_file).expanduser().resolve()
        self.key_path.parent.mkdir(parents=True, exist_ok=True)
        self._fernet = self._get_or_create_fernet()

    def _generate_machine_salt(self) -> bytes:
        """生成基于机器特征的盐值
        
        使用机器的 MAC 地址和主机名生成稳定的盐值
        """
        try:
            # 获取 MAC 地址
            mac = uuid.getnode().to_bytes(6, 'big')
            # 获取主机名
            hostname = platform.node().encode('utf-8')
            # 组合
            salt = mac + hostname
            # 确保盐值长度至少 16 字节
            if len(salt) < 16:
                salt = salt + b'0' * (16 - len(salt))
            return salt[:16]
        except Exception as e:
            logger.warning(f"无法生成机器特定盐值，使用默认值: {e}")
            return b'filecore_default'

    def _generate_key(self) -> bytes:
        """生成加密密钥
        
        基于机器特征生成密钥，确保在同一台机器上密钥稳定
        """
        password = b'filecore_smb_encryption'
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
        """获取或创建 Fernet 加密器"""
        if self.key_path.exists():
            # 读取现有密钥
            try:
                with open(self.key_path, 'rb') as f:
                    key = f.read()
                return Fernet(key)
            except Exception as e:
                logger.error(f"读取密钥失败: {e}")
                # 如果读取失败，生成新密钥
                
        # 生成新密钥
        key = Fernet.generate_key()
        try:
            with open(self.key_path, 'wb') as f:
                f.write(key)
            # 设置文件权限为仅用户可读写
            os.chmod(self.key_path, 0o600)
            logger.info(f"已生成新的加密密钥: {self.key_path}")
        except Exception as e:
            logger.error(f"保存密钥失败: {e}")
            
        return Fernet(key)

    def encrypt(self, password: str) -> str:
        """加密密码
        
        Args:
            password: 明文密码
            
        Returns:
            加密后的密码（Base64 编码字符串）
        """
        if not password:
            return ""
        
        try:
            encrypted = self._fernet.encrypt(password.encode('utf-8'))
            return encrypted.decode('utf-8')
        except Exception as e:
            logger.error(f"密码加密失败: {e}")
            raise ValueError(f"密码加密失败: {str(e)}")

    def decrypt(self, encrypted_password: str) -> str:
        """解密密码
        
        Args:
            encrypted_password: 加密的密码（Base64 编码字符串）
            
        Returns:
            明文密码
        """
        if not encrypted_password:
            return ""
        
        try:
            decrypted = self._fernet.decrypt(encrypted_password.encode('utf-8'))
            return decrypted.decode('utf-8')
        except Exception as e:
            logger.error(f"密码解密失败: {e}")
            raise ValueError(f"密码解密失败: {str(e)}")


# 全局实例
_encryption: Optional[PasswordEncryption] = None


def get_encryption() -> PasswordEncryption:
    """获取全局加密实例"""
    global _encryption
    if _encryption is None:
        _encryption = PasswordEncryption()
    return _encryption
