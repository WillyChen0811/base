"""JSON-RPC 底座

提供 RPC 服务端（FastAPI 路由 / 独立 HTTP / stdio）与客户端。
业务路由注册、跨端同步逻辑（原 shared/rpc/handlers、shared/rpc/sync.py）不在此列，
留在各项目自己的 rpc 包里，从这里 import 服务端/客户端基础设施。
"""

from .server import RPCDispatcher, create_fastapi_rpc_router, create_http_handler, run_stdio
from .client import RPCClient, RPCError, configure, get_client, call

__all__ = [
    # 服务端
    'RPCDispatcher',
    'create_fastapi_rpc_router',
    'create_http_handler',
    'run_stdio',
    # 客户端
    'RPCClient',
    'RPCError',
    'configure',
    'get_client',
    'call',
]
