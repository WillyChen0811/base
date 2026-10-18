"""列共享：`smbprotocol`/`smbclient` 没有高层的共享枚举 API（要手写 SRVSVC RPC），
所以这一件事仍留在 pysmb 上——它是配置服务器时的一次性辅助，不走数据路径，
不值得为它把 SRVSVC 那套抄一遍。

**整个包里只有这个文件 import pysmb**，其余数据路径已全部迁到 smbprotocol。
"""

import logging
import socket
from typing import Dict, List

from .._vendor.smb.SMBConnection import SMBConnection

logger = logging.getLogger(__name__)


def list_shares(address: str, username: str = "", password: str = "", port: int = 445) -> List[Dict]:
    """列出服务器上的所有共享 (仍使用 pysmb)，过滤掉系统共享（$结尾）和 IPC$。

    返回 [{"name": "共享名", "path": "//addr/共享名"}, ...]
    """
    logger.info(f"开始列出共享: address={address}, username={username or '(空)'}, port={port}")

    try:
        my_name = socket.gethostname()
        conn = SMBConnection(
            username or "",
            password or "",
            my_name,
            "",
            use_ntlm_v2=True,
            is_direct_tcp=(port == 445),
        )

        connected = conn.connect(address, port, timeout=10)
        if not connected:
            raise Exception("无法连接到服务器")

        shares = conn.listShares()

        results = []
        for share in shares:
            # 过滤掉系统共享（以$结尾的）和 IPC$
            if not share.name.endswith("$"):
                results.append({
                    "name": share.name,
                    "path": f"//{address}/{share.name}",
                })

        conn.close()
        return results

    except Exception as e:
        logger.error(f"列出共享失败: {e}")
        import traceback
        logger.error(traceback.format_exc())
        raise
