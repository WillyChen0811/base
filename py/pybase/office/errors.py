"""pybase.office 客户端异常分类

对应服务端 `/convert` 出错时回的 JSON `{"error": 码, "message": ...}`，外加连接层面
本身就够不着服务的情况（那种连错误 JSON 都拿不到）。`client.py` 把响应状态码/协议
错误码映射成这里的类型，调用方按类型分诊，不用再自己解析 message 猜错误种类。

重试策略（哪些异常会被 `WordPdfClient` 自动重试）见该类的文档字符串。
"""


class OfficeClientError(Exception):
    """pybase.office 客户端异常的基类"""


class ConnectionUnavailableError(OfficeClientError):
    """连不上服务：网络不通、拒连、DNS 解析失败等，重试若干次仍不通才抛出"""


class ServiceBusyError(OfficeClientError):
    """服务端返回 503：转换队列已满。重试若干次仍忙才抛出"""


class ConversionTimeoutError(OfficeClientError):
    """转换超时：服务端返回 504（已强杀并重建 Word 实例），或客户端本地等待超时"""


class ConversionFailedError(OfficeClientError):
    """服务端转换过程本身出错（如 Word COM 异常）。message 是服务端原样带回的错误信息"""


class AuthenticationError(OfficeClientError):
    """服务端返回 401：共享 token 校验未通过"""


class PayloadTooLargeError(OfficeClientError):
    """服务端返回 413：请求体超过服务端配置的上限"""


class OfficeProtocolError(OfficeClientError):
    """服务端返回了协议之外的东西：非 JSON 错误体、未识别的状态码等"""
