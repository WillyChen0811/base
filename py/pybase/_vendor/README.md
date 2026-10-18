# _vendor

- smbprotocol 1.17.0：`smbprotocol/`、`smbclient/`（LICENSE.smbprotocol）
- pysmb 1.2.15：`smb/`、`nmb/`（LICENSE.pysmb）

整库拷贝，包内 import 已改指向本目录，外连（TCP/UDP）已换成 `pybase.wgnet`。
由 `py/scripts/vendor_smb.py` 生成，升级库后重跑脚本，不要手改。改动清单见 PATCHES.md。

它们依赖的 cryptography、pyspnego、pyasn1 等不拷，照旧用安装的。
