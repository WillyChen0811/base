"""NAS HTTP 取文件通道（WebDAV / File Station）—— 底层实现。

当 SMB 直读被独占占用（最典型是 docx 正被 Word 打开）时，群晖的 HTTP 服务在 NAS
本机直接读盘、不经过 SMB 的字节锁检查，仍能取到磁盘上当前已保存的内容。本模块提供
两条 HTTP 通道的底层取文件实现：

  - WebDAV       GET  https/http       —— 需装 WebDAV Server 套件
  - File Station login -> SYNO.FileStation.Download —— DSM 自带

两条通道彼此独立、不串联（是否尝试、按什么顺序由上层/前端决定）。端口与是否校验
HTTPS 证书都来自每台服务器各自的配置：
  - server["webdav_port"] / server["filestation_port"]
  - server["http_verify_ssl"]（默认关闭——群晖内网多为自签证书）
失败统一抛 errors.FetchError（带用户友好文案 + 底层原始码）。
"""

import base64
import json
import logging
import ssl
from urllib.parse import urlencode

from .. import wgnet

from .errors import FetchError, webdav_http_error, webdav_exception, filestation_error

logger = logging.getLogger(__name__)

_HTTP_TIMEOUT = 60

# 群晖默认端口（仅作为配置缺省回退）
_DEFAULT_WEBDAV_PORT = 5006
_DEFAULT_FILESTATION_PORT = 5001



class _Resp:
    """requests.Response 的最小替身：只含本模块用到的 status_code / headers / content / json()。"""

    def __init__(self, r):
        self.status_code, self.headers, self.content = r.status, r.headers, r.body

    def json(self):
        return json.loads(self.content.decode("utf-8"))


def _get(url, params=None, auth=None, verify=False, timeout=_HTTP_TIMEOUT) -> _Resp:
    """GET，底层经 wgnet.http（线路、DNS 由 wgtund 决定）；不校验证书时用不验证的 SSL 上下文。"""
    if params:
        url += ("&" if "?" in url else "?") + urlencode(params)
    headers = {}
    if auth:
        headers["Authorization"] = "Basic " + base64.b64encode(f"{auth[0]}:{auth[1]}".encode()).decode()
    ctx = None
    if not verify:
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    return _Resp(wgnet.http.get(url, headers, timeout=timeout, context=ctx))


def _rel(path: str) -> str:
    """归一相对共享根的路径：去掉首尾斜杠、统一正斜杠。"""
    return "/".join(p for p in path.replace("\\", "/").split("/") if p)


def _verify(server: dict) -> bool:
    return bool(server.get("http_verify_ssl"))


def fetch_webdav(server: dict, path: str) -> bytes:
    """走群晖 WebDAV 取文件。失败抛 FetchError。"""
    port = server.get("webdav_port") or _DEFAULT_WEBDAV_PORT
    rel = _rel(path)
    url = f"https://{server['address']}:{port}/{server['share']}/{rel}"
    try:
        r = _get(
            url,
            auth=(server.get("username") or "", server.get("password") or ""),
            verify=_verify(server), timeout=_HTTP_TIMEOUT,
        )
    except Exception as e:
        logger.info(f"WebDAV 请求异常 {server['address']}:{port}: {e}")
        raise webdav_exception(e)

    if r.status_code != 200:
        logger.info(f"WebDAV 取文件失败 HTTP {r.status_code}: {rel}")
        raise webdav_http_error(r.status_code)
    if not r.content:
        raise FetchError("取文件失败：服务器返回空内容", "WebDAV empty")
    logger.info(f"WebDAV 取文件成功，{len(r.content)} 字节: {rel}")
    return r.content


def fetch_filestation(server: dict, path: str) -> bytes:
    """走群晖 File Station（DSM 自带）取文件：登录 -> 下载 -> 注销。失败抛 FetchError。"""
    port = server.get("filestation_port") or _DEFAULT_FILESTATION_PORT
    verify = _verify(server)
    base = f"https://{server['address']}:{port}/webapi"
    rel = _rel(path)
    fs_path = "/" + server["share"] + "/" + rel  # File Station 路径以 / + 共享名开头

    # 登录
    try:
        login = _get(f"{base}/auth.cgi", params={
            "api": "SYNO.API.Auth", "version": "3", "method": "login",
            "account": server.get("username") or "", "passwd": server.get("password") or "",
            "session": "FileStation", "format": "sid",
        }, verify=verify, timeout=_HTTP_TIMEOUT).json()
    except Exception as e:
        logger.info(f"File Station 登录请求异常 {server['address']}:{port}: {e}")
        raise webdav_exception(e)
    if not login.get("success"):
        code = (login.get("error") or {}).get("code", 0)
        logger.info(f"File Station 登录失败 code={code}")
        raise filestation_error(code, "auth")
    sid = login["data"]["sid"]

    try:
        r = _get(f"{base}/entry.cgi", params={
            "api": "SYNO.FileStation.Download", "version": "2", "method": "download",
            "path": fs_path, "mode": "download", "_sid": sid,
        }, verify=verify, timeout=_HTTP_TIMEOUT)
        # 成功是文件流；失败群晖返回 JSON 错误体
        ctype = r.headers.get("Content-Type", "")
        if r.status_code == 200 and "application/json" not in ctype and r.content:
            logger.info(f"File Station 取文件成功，{len(r.content)} 字节: {rel}")
            return r.content
        # 解析错误码：>=400 属文件操作段，否则通用 API 段
        code = 0
        try:
            code = (r.json().get("error") or {}).get("code", 0)
        except Exception:
            pass
        section = "fs" if code >= 400 else ("api" if code else "fs")
        logger.info(f"File Station 下载失败 HTTP {r.status_code} code={code}: {rel}")
        raise filestation_error(code, section)
    finally:
        try:
            _get(f"{base}/auth.cgi", params={
                "api": "SYNO.API.Auth", "version": "1", "method": "logout",
                "session": "FileStation", "_sid": sid,
            }, verify=verify, timeout=10)
        except Exception:
            pass
