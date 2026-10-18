# 对原库的改动（脚本自动生成）
- 包内绝对 import 改为 `pybase._vendor.*`：92 处
- 外连替换：

| 文件 | 类别 | 处数 | 说明 |
|---|---|---|---|
| smbprotocol/transport.py | TCP | 1 | Tcp.connect：SMB2 直连 445 的 TCP |
| smb/SMBConnection.py | TCP | 1 | SMBConnection.connect：pysmb 的 TCP（sock_family 分支并入，wgnet 按目的地址定族） |
| nmb/NetBIOS.py | UDP | 1 | NetBIOS.__init__：NetBIOS 名字查询的 UDP |
| nmb/NetBIOS.py | UDP | 1 | NetBIOS.__init__：去掉 SO_BROADCAST/bind（wgnet.udp 不支持） |
| nmb/NetBIOS.py | UDP | 2 | NetBIOS._pollFor*Packet：select(fileno) 改 settimeout+recvfrom（wgnet.udp 无 fileno） |

合计 TCP 2 处、UDP 4 处（含配套改动）。

未改：`nmb/NetBIOSProtocol.py`（Twisted 版，不被 pybase 使用，不建 socket）、`smb/SMBHandler.py`（只取本机主机名）。
