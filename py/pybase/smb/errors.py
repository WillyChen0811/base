"""三种取文件通道的错误翻译 —— 各自独立，不混用。

SMB / WebDAV / File Station 三套错误体系完全不同：
- SMB：smbclient 把协议错误包成 SMBOSError(.ntstatus) / SMBResponseException(.status)，
  连接层则是 SMBAuthenticationError / SMBConnectionClosed / socket 异常（无状态码）。
  NTSTATUS 常量见 smbprotocol.header.NtStatus。
- WebDAV：标准 HTTP 状态码 + requests 异常（含 WebDAV 专属 423 Locked，RFC 4918）。
- File Station：群晖自家 JSON error.code，且分两段——SYNO.API.Auth 登录错误码 与
  SYNO.FileStation 操作错误码，同一数值在两段里含义不同，必须按段翻译。
  （来源：群晖官方 File Station API 文档，建议按 DSM 版本回归确认。）

翻译目标：常规错误给用户看得懂的中文；认不出的统一“未知错误 + 底层代码”，
底层代码始终保留在 FetchError.code 里方便排查。
"""

import socket


class FetchError(Exception):
    """取文件失败：user_message 给用户看，code 是底层原始错误码（排查用）。"""

    def __init__(self, user_message: str, code: str = ""):
        super().__init__(user_message)
        self.user_message = user_message
        self.code = code

    def as_dict(self) -> dict:
        return {"success": False, "message": self.user_message, "code": self.code}


# ---------------- SMB（NTSTATUS） ----------------

# 按 NTSTATUS 原始值匹配（不依赖 smbprotocol 是否给该码命名）
_NT_STATUS = {
    0xC0000043: "文件正被其他程序打开（如 Word），请关闭后重试",  # SHARING_VIOLATION
    0xC0000054: "文件正被其他程序打开（如 Word），请关闭后重试",  # FILE_LOCK_CONFLICT
    0xC000006D: "账号或密码错误",                                  # LOGON_FAILURE
    0xC000006A: "账号或密码错误",                                  # WRONG_PASSWORD
    0xC0000022: "无权限访问该文件",                                # ACCESS_DENIED
    0xC0000034: "文件不存在或已被移动",                            # OBJECT_NAME_NOT_FOUND
    0xC000003A: "文件不存在或已被移动",                            # OBJECT_PATH_NOT_FOUND
    0xC000000F: "文件不存在或已被移动",                            # NO_SUCH_FILE
    0xC00000CC: "共享目录不存在（请检查共享名）",                  # BAD_NETWORK_NAME
    0xC00000C9: "与服务器连接中断，请重试",                        # NETWORK_NAME_DELETED
    0xC00000B5: "连接服务器超时",                                  # IO_TIMEOUT
}


def smb_error(exc: Exception) -> FetchError:
    """把 SMB 读取/连接抛出的异常翻译成 FetchError。"""
    # 1) 协议层错误：smbclient 包成带 NTSTATUS 的异常
    nt = getattr(exc, "ntstatus", None)          # SMBOSError
    if nt is None:
        nt = getattr(exc, "status", None)        # SMBResponseException
    if isinstance(nt, int):
        code = f"NTSTATUS 0x{nt & 0xFFFFFFFF:08X}"
        msg = _NT_STATUS.get(nt & 0xFFFFFFFF)
        if msg:
            return FetchError(msg, code)
        return FetchError(f"取文件失败：未知错误（SMB {code}）", code)

    # 2) 连接层错误（无状态码）：按类型/类名归类
    name = type(exc).__name__
    if name == "SMBAuthenticationError":
        return FetchError("账号或密码错误", name)
    if name == "SMBConnectionClosed":
        return FetchError("与服务器连接中断，请重试", name)
    if isinstance(exc, socket.gaierror):
        return FetchError("无法连接服务器（地址无法解析）", "gaierror")
    if isinstance(exc, (ConnectionRefusedError, ConnectionResetError)):
        return FetchError("无法连接服务器（检查地址、端口、SMB 是否开启）", name)
    if isinstance(exc, (TimeoutError, socket.timeout)):
        return FetchError("连接服务器超时", name)
    if isinstance(exc, OSError):
        return FetchError("无法连接服务器（检查地址、端口、网络）", f"OSError:{exc.errno}")

    return FetchError(f"取文件失败：未知错误（{name}）", f"{name}: {exc}")


# ---------------- WebDAV（HTTP 状态码 + requests 异常） ----------------

_HTTP_STATUS = {
    401: "账号或密码错误",
    403: "无权限访问该文件",
    404: "文件不存在或已被移动",
    423: "文件被锁定，请稍后重试",          # WebDAV Locked (RFC 4918)
}


def webdav_http_error(status: int) -> FetchError:
    """WebDAV 返回了非 200 的 HTTP 状态码。"""
    msg = _HTTP_STATUS.get(status)
    if msg:
        return FetchError(msg, f"HTTP {status}")
    return FetchError(f"取文件失败：服务器返回 HTTP {status}", f"HTTP {status}")


def webdav_exception(exc: Exception) -> FetchError:
    """WebDAV 请求过程抛出的 requests/网络异常。"""
    name = type(exc).__name__
    if name in ("ConnectTimeout", "ReadTimeout", "Timeout"):
        return FetchError("连接服务器超时", name)
    if name in ("ConnectionError", "SSLError"):
        return FetchError("无法连接（端口未开，或 WebDAV 套件未启用）", name)
    return FetchError(f"取文件失败：未知错误（WebDAV {name}）", f"{name}: {exc}")


# ---------------- File Station（群晖 JSON error.code，分段） ----------------

# 登录段：SYNO.API.Auth
_FS_AUTH = {
    400: "账号或密码错误",
    401: "账号已被禁用",
    402: "权限不足",
    403: "该账号需两步验证，暂不支持",
    404: "该账号需两步验证，暂不支持",
    406: "该账号需两步验证，暂不支持",
    407: "IP 被服务器封锁",
    408: "密码已过期，请在 NAS 上修改",
    409: "密码已过期，请在 NAS 上修改",
    410: "密码需修改，请在 NAS 上修改",
}

# 通用 API 段：SYNO.API.*
_FS_API = {
    102: "File Station 服务未启用或版本不符",
    103: "File Station 服务未启用或版本不符",
    104: "File Station 服务未启用或版本不符",
    105: "无权限访问",
    106: "登录会话超时，请重试",
    119: "登录会话失效，请重试",
}

# 操作段：SYNO.FileStation.*（下载/文件操作）
_FS_OP = {
    400: "路径或文件名非法",
    403: "无权限访问该文件",
    407: "无权限访问该文件",
    408: "文件不存在或已被移动",
    417: "服务器读盘错误",
    418: "路径或文件名非法",
    419: "路径或文件名非法",
}

_FS_SECTIONS = {"auth": _FS_AUTH, "api": _FS_API, "fs": _FS_OP}


def filestation_error(code: int, section: str) -> FetchError:
    """File Station 返回了错误码。section: 'auth'(登录) / 'api'(通用) / 'fs'(文件操作)。"""
    table = _FS_SECTIONS.get(section, {})
    msg = table.get(code)
    code_str = f"FileStation[{section}] {code}"
    if msg:
        return FetchError(msg, code_str)
    return FetchError(f"取文件失败：未知错误（{code_str}）", code_str)
