"""wireguard-go 进程管理：启动/停止用户态 WireGuard 引擎，拿到内核分配的接口名，
留一份尾部日志方便排查。

依赖复用 `pybase.media._proc` 里子进程封装的成熟模式（独立进程组、超时杀、跨平台
kill）——wireguard-go 是长驻前台进程而不是一次性命令，所以不能直接用 `run()`/
`run_streaming()`（它们都要等进程退出才返回），但进程组常量和杀进程的写法照抄。
"""
from __future__ import annotations

import os
import signal
import subprocess
import tempfile
import threading
import time
from collections import deque
from pathlib import Path

from pybase.media import _proc
from pybase.net.wireguard import default_uapi_path


class EngineError(RuntimeError):
    """wireguard-go 进程管理相关错误的基类。"""


class BinaryNotFoundError(EngineError):
    """找不到 wireguard-go 可执行文件。"""


class EngineStartError(EngineError):
    """wireguard-go 启动失败，或没能在超时内就绪（拿不到接口名/UAPI socket）。"""


class WireGuardGo:
    """管理一个 wireguard-go 前台进程（`-f`）。一个实例对应一个隧道接口；
    `start`/`stop` 不是线程安全的，调用方自己串行化，别并发调。
    """

    def __init__(
        self,
        *,
        binary: str | os.PathLike | None = None,
        uapi_dir: str | os.PathLike | None = None,
        start_timeout: float = 10.0,
        stop_timeout: float = 5.0,
        log_lines: int = 200,
    ) -> None:
        self._binary = binary
        self._uapi_dir = str(uapi_dir) if uapi_dir else None
        self._start_timeout = start_timeout
        self._stop_timeout = stop_timeout
        self._log: deque[str] = deque(maxlen=log_lines)
        self._process: subprocess.Popen | None = None
        self._ifname: str | None = None
        self._tmpdir: str | None = None
        self._reader: threading.Thread | None = None

    @property
    def ifname(self) -> str | None:
        return self._ifname

    @property
    def sock_path(self) -> str | None:
        if self._ifname is None:
            return None
        return default_uapi_path(self._ifname, uapi_dir=self._uapi_dir)

    @property
    def alive(self) -> bool:
        return self._process is not None and self._process.poll() is None

    def logs(self, n: int = 50) -> list[str]:
        """最近的 stderr 尾部；`n <= 0` 返回全部（仍受 `log_lines` 上限约束）。"""
        lines = list(self._log)
        return lines if n <= 0 else lines[-n:]

    def start(self, *, ifname_hint: str = "utun") -> str:
        """启动 wireguard-go，返回内核实际分配的接口名（macOS 上 `utun` 只是提示，
        真实编号由内核定，通过 `WG_TUN_NAME_FILE` 环境变量拿回来）。
        """
        if self.alive:
            raise EngineStartError("已经在运行，不能重复 start")
        try:
            binary_path = _proc.resolve_tool(self._binary, "wireguard-go")
        except _proc.ToolNotFoundError as exc:
            raise BinaryNotFoundError(
                f"{exc}（macOS 可执行 `brew install wireguard-go` 安装）"
            ) from exc

        self._tmpdir = tempfile.mkdtemp(prefix="wg-go-")
        name_file = Path(self._tmpdir) / "tun-name"
        env = os.environ.copy()
        env["WG_TUN_NAME_FILE"] = str(name_file)
        if self._uapi_dir:
            # 真实 wireguard-go 不认这个变量（固定用 /var/run/wireguard），只在
            # uapi_dir 被显式覆盖时传下去——主要是给测试用的假二进制看的
            env["WG_UAPI_DIR"] = self._uapi_dir

        process = subprocess.Popen(
            [binary_path, "-f", ifname_hint],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            errors="replace",
            env=env,
            # 独立进程组：停止时要整组杀，避免留下孤儿（同 media._proc 的理由）
            **_proc._GROUP_KWARGS,
        )
        self._process = process
        self._reader = threading.Thread(target=self._read_stderr, daemon=True)
        self._reader.start()

        try:
            self._ifname = self._wait_for_ifname(name_file)
            self._wait_for_socket()
        except EngineStartError:
            self._force_stop()
            raise
        return self._ifname

    def _read_stderr(self) -> None:
        process = self._process
        assert process is not None and process.stderr is not None
        try:
            for line in process.stderr:
                self._log.append(line.rstrip("\n"))
        except Exception:  # noqa: BLE001 - 读日志失败不该影响主流程
            pass

    def _wait_for_ifname(self, name_file: Path) -> str:
        started = time.monotonic()
        while True:
            if not self.alive:
                raise EngineStartError(
                    "wireguard-go 启动后立即退出：\n" + "\n".join(self.logs())
                )
            if name_file.exists():
                name = name_file.read_text().strip()
                if name:
                    return name
            if time.monotonic() - started > self._start_timeout:
                raise EngineStartError(f"等待 wireguard-go 分配接口名超时（{self._start_timeout}s）")
            time.sleep(0.05)

    def _wait_for_socket(self) -> None:
        path = self.sock_path
        assert path is not None
        started = time.monotonic()
        while True:
            if not self.alive:
                raise EngineStartError(
                    "wireguard-go 在建立 UAPI socket 前退出：\n" + "\n".join(self.logs())
                )
            if os.path.exists(path):
                return
            if time.monotonic() - started > self._start_timeout:
                raise EngineStartError(f"等待 UAPI socket 超时（{self._start_timeout}s）：{path}")
            time.sleep(0.05)

    def stop(self) -> None:
        """优雅停止（SIGTERM）→ 超时强杀（SIGKILL，整组）。幂等：没在跑直接返回。"""
        process = self._process
        if process is None:
            return
        try:
            if process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except OSError:
                    pass
                try:
                    process.wait(timeout=self._stop_timeout)
                except subprocess.TimeoutExpired:
                    _proc._kill(process, drain=False)
        finally:
            self._force_stop()

    def _force_stop(self) -> None:
        process = self._process
        if process is not None:
            if process.poll() is None:
                _proc._kill(process, drain=False)
            if process.stderr is not None:
                try:
                    process.stderr.close()
                except OSError:
                    pass
        if self._reader is not None:
            self._reader.join(timeout=2)
        sock_path = self.sock_path
        if sock_path:
            try:
                os.unlink(sock_path)
            except OSError:
                pass
        if self._tmpdir:
            try:
                for child in Path(self._tmpdir).iterdir():
                    child.unlink()
                os.rmdir(self._tmpdir)
            except OSError:
                pass
        self._process = None
        self._ifname = None
        self._tmpdir = None
        self._reader = None
