"""JSON-RPC 服务端

统一的 RPC 路由，支持 FastAPI 和独立 HTTP 服务
"""

import inspect
import json
import logging
import threading
from typing import Dict, Any, Callable

logger = logging.getLogger(__name__)

# 全局 stdout 写锁：RPC 主循环的响应与后台线程的通知都向 stdout 打印 JSON 行，
# 主进程按行解析，并发打印会撕裂同一行导致解析失败，必须串行化所有写。
_stdout_lock = threading.Lock()


def emit_json(obj: Any):
    """加锁后向 stdout 打印一行 JSON（供响应与通知统一使用）"""
    line = json.dumps(obj, ensure_ascii=False, default=str)
    with _stdout_lock:
        print(line, flush=True)


class RPCDispatcher:
    """RPC 方法分发器"""

    def __init__(self):
        self._methods: Dict[str, Callable] = {}

    def method(self, name: str):
        """注册方法装饰器"""
        def decorator(func: Callable):
            self._methods[name] = func
            return func
        return decorator

    def register(self, name: str, func: Callable):
        """注册方法"""
        self._methods[name] = func

    def dispatch(self, method: str, params: Dict = None) -> Any:
        """分发调用"""
        if method not in self._methods:
            raise ValueError(f"Method not found: {method}")

        func = self._methods[method]
        if params:
            return func(**params)
        return func()

    def handle_request(self, request: Dict) -> Dict:
        """处理 JSON-RPC 请求。

        **错误分类不能靠异常类型判**：原来是 `except ValueError -> -32601`
        （因为 dispatch 用 ValueError 表示"方法不存在"）、
        `except TypeError -> -32602`，于是**方法体内部抛的 ValueError/TypeError
        全被误判**——业务里 `raise ValueError("默认规则集不能删除")` 会返回
        "Method not found"，照着这个错去查方法名当然什么也查不出来。
        而 ValueError/TypeError 恰恰是业务代码最常抛的两种。

        改成：方法在不在、参数对不对得上签名，都在调用之前判掉；
        真正调用时抛的一律算内部错误(-32000)，原样把消息带回去。
        """
        req_id = request.get("id")
        method = request.get("method")
        params = request.get("params", {})

        if not method:
            return self._error_response(req_id, -32600, "Invalid Request")
        if method not in self._methods:
            return self._error_response(
                req_id, -32601, f"Method not found: {method}")

        func = self._methods[method]
        kwargs = params or {}
        try:
            inspect.signature(func).bind(**kwargs)
        except TypeError as e:
            # 签名对不上（缺参/多参/params 传成了数组）
            return self._error_response(req_id, -32602, f"Invalid params: {e}")

        try:
            result = func(**kwargs) if kwargs else func()
            return self._success_response(req_id, result)
        except Exception as e:
            logger.exception(f"RPC error in {method}")
            return self._error_response(req_id, -32000, str(e))

    def _success_response(self, req_id: Any, result: Any) -> Dict:
        return {"jsonrpc": "2.0", "id": req_id, "result": result}

    def _error_response(self, req_id: Any, code: int, message: str) -> Dict:
        return {"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}}

    def list_methods(self) -> list:
        return list(self._methods.keys())


def create_fastapi_rpc_router(dispatcher: RPCDispatcher):
    """创建 FastAPI RPC 路由（挂载于 /rpc，另提供 /rpc/methods 列出已注册方法）。"""
    from fastapi import APIRouter, Request
    from fastapi.responses import JSONResponse

    router = APIRouter(prefix="/rpc", tags=["rpc"])

    @router.post("")
    async def rpc_endpoint(request: Request):
        try:
            body = await request.json()
        except json.JSONDecodeError:
            return JSONResponse(
                {"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "Parse error"}},
                status_code=400
            )

        # 批量请求
        if isinstance(body, list):
            responses = [dispatcher.handle_request(req) for req in body]
            return JSONResponse(responses)

        return JSONResponse(dispatcher.handle_request(body))

    @router.get("/methods")
    async def list_methods():
        return {"methods": dispatcher.list_methods()}

    return router


def create_http_handler(dispatcher: RPCDispatcher):
    """创建标准 HTTP 处理器（用于简单 HTTP 服务器），返回一个 str->str 的请求处理函数。"""
    def handle(request_body: str) -> str:
        try:
            body = json.loads(request_body)
        except json.JSONDecodeError:
            return json.dumps(
                {"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "Parse error"}}
            )

        if isinstance(body, list):
            responses = [dispatcher.handle_request(req) for req in body]
            return json.dumps(responses, ensure_ascii=False, default=str)

        return json.dumps(dispatcher.handle_request(body), ensure_ascii=False, default=str)

    return handle


def run_stdio(dispatcher: RPCDispatcher, max_workers: int = 8):
    """运行 stdio 模式（用于 Electron 等进程间通信）

    主循环只负责读行，请求本身交给线程池处理。

    以前是读一行、就地处理完再读下一行，于是任何慢方法（SMB 递归扫描、取文件、
    视频预扫描）都会独占这根管道：后面排队的调用一直等到 30 秒默认超时报错，
    表现为"客户端点了没反应/ 连不上"。而慢方法本身多是 IO 等待，占着主循环纯属浪费。

    并发安全的前提（都已成立，改动前逐条验证过）：
    - 响应按 id 配对，客户端不依赖返回顺序
    - emit_json 持 _stdout_lock，多线程写 stdout 不会撕裂 JSON 行
    - Store 每次操作新开 sqlite 连接、Session 每次从工厂新建，无跨线程共享对象
    - scan/report/seal 等既有后台线程本来就与主循环并发跑同一个 Store

    Args:
        dispatcher: RPC 分发器
        max_workers: 并发处理的请求数上限
    """
    import sys
    from concurrent.futures import ThreadPoolExecutor

    # **这根管子必须是 UTF-8，两端都得钉死。**
    #
    # JSON 按规范就是 UTF-8，客户端（Electron）也一直按 UTF-8 收发；可 Python
    # 的 stdin/stdout 在管道模式下跟随系统本地编码——中文 Windows 上是 cp936，
    # 于是进来的中文字节被当 GBK 解，解不掉的字节变成 \udcXX 这类孤立代理字符，
    # 等到写库或回话时再编码成 UTF-8 就炸：
    #
    #     'utf-8' codec can't encode character '\udcba': surrogates not allowed
    #
    # 症状很能骗人：纯 ASCII 的请求一路正常，网址/请求头/正文里一旦有中文，
    # 那一整批就写不进去。**这跟被抓的内容是什么编码无关，是管子自己错了。**
    for _stream in (sys.stdin, sys.stdout):
        try:
            _stream.reconfigure(encoding="utf-8", errors="strict")
        except Exception:      # noqa: BLE001  老 Python，或流被替换过
            pass
    # 日志走 stderr，只给人看：这里宁可显示成问号，也不要因为编码把进程弄挂
    try:
        sys.stderr.reconfigure(encoding="utf-8", errors="replace")
    except Exception:          # noqa: BLE001
        pass

    logger.info("RPC Server started (stdio mode)")

    def _handle_and_reply(request):
        """在工作线程里处理单个请求并回写响应"""
        try:
            if isinstance(request, list):  # 批量请求
                emit_json([dispatcher.handle_request(req) for req in request])
            else:
                emit_json(dispatcher.handle_request(request))
        except Exception as e:
            # handle_request 内部已兜底，走到这里说明是回写阶段出的问题。
            # 仍要给出带 id 的响应，否则客户端要一直挂到超时。
            logger.exception("RPC 请求处理失败")
            req_id = request.get("id") if isinstance(request, dict) else None
            try:
                emit_json({
                    "jsonrpc": "2.0", "id": req_id,
                    "error": {"code": -32000, "message": str(e)},
                })
            except Exception:
                logger.exception("RPC 错误响应回写失败")

    # 发送就绪通知
    emit_json({"jsonrpc": "2.0", "method": "ready"})

    with ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix="rpc") as pool:
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue

            try:
                request = json.loads(line)
            except json.JSONDecodeError:
                emit_json({"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "Parse error"}})
                continue

            pool.submit(_handle_and_reply, request)
