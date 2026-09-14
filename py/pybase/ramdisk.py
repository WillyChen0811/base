r"""内存级临时盘（RAM Disk）管理 —— backend 与 filecore 共用。

为什么需要：解析、打包等流程要把数据落盘成临时文件，放内存盘可避免频繁读写
真实磁盘、也避免敏感报告内容残留在磁盘上。

平台：
- macOS：用 hdiutil 挂一块 512MB RAM disk 到 /Volumes/PyBaseTemp。
- Linux：直接用 tmpfs 目录 /dev/shm/pybase（内存文件系统，无需挂载）。
- Windows：**退化为真实磁盘**上的临时目录 %TEMP%\PyBaseTemp。

Windows 为什么是例外：这个系统压根没有内置的内存文件系统——没有 tmpfs，也没有
hdiutil 那样自带的命令。要真挂内存盘只能装第三方内核驱动（ImDisk / OSFMount 等），
而那要管理员权限装驱动，与现场版"拷过去就能跑"的定位冲突。

**这是一处明确的安全让步**：上面"不留残留"的好处在 Windows 上拿不到，只能靠
cleanup() 退出时尽力删干净（删不掉只记日志，不影响主流程）。所以 Windows 版按
定位只跑压测、不碰报告数据；哪天要在 Windows 上做文档类流程，这个前提得重新评估。
`usage()` 里的 memory_backed 字段会如实报出当前是不是真内存盘，别拿它当内存盘用。

同一台设备多进程并发（最典型：本机同时跑 backend 和 filecore）：
两端用同名同路径，**共享同一块内存盘**。用 flock 做跨进程引用计数解决"谁退出谁
卸载会把还在用的另一方的盘拔掉"的冲突——每进程一个 pid 锁文件：
- 每个进程在 users 目录下建 "{pid}.lock" 并独占它（LOCK_EX，fd 活到进程结束）。
  进程崩溃时内核自动释放该锁，留下的陈旧文件可被识别清理。
- 创建/注册/卸载都在 create 锁（LOCK_EX）下串行，避免并发创建出 "PyBaseTemp 1"，
  也保证"判断是否最后一人"与"新进程注册"互斥。
- 卸载时移除自己的 pid 文件，再逐个 LOCK_EX|NB 试别的 pid 文件：能拿到=该进程已死
  （清掉陈旧文件）；拿不到=有进程仍在用。全部可拿 => 自己是最后一个 => 才 eject。
  注意：不能用"释放自己的锁再回头测同一文件"那套——macOS/BSD 下同一进程释放后对
  同一文件再请求 EX 会被错误放行（无视他进程的锁），故必须测"别人的"文件。
"""

import os
import platform
import shutil
import time
import tempfile
import logging
import subprocess

logger = logging.getLogger(__name__)

_DEFAULT_NAME = "PyBaseTemp"
_DEFAULT_LINUX_DIRNAME = "pybase"
_DEFAULT_SIZE_MB = 512


class RamDiskManager:
    """内存盘生命周期管理（严格模式，挂不上即抛异常，不降级）"""

    # 重挂前留给其它进程收尾的时间。这套系统里除了下载解析，操作基本都在
    # 1 秒内完成，几秒足够；预告超过 STALE 秒还在就当发起方已崩，自动失效。
    REMOUNT_GRACE_SEC = 5.0
    REMOUNT_STALE_SEC = 60.0

    def __init__(
        self,
        name: str = _DEFAULT_NAME,
        linux_dirname: str = _DEFAULT_LINUX_DIRNAME,
        size_mb: int = _DEFAULT_SIZE_MB,
    ):
        self.system = platform.system()
        if self.system not in ("Darwin", "Linux", "Windows"):
            raise RuntimeError(
                f"不支持的操作系统: {self.system}。"
                f"仅支持 macOS / Linux（内存盘）与 Windows（磁盘临时目录）。"
            )

        self.name = name
        self.size_mb = size_mb
        _tmp = tempfile.gettempdir()
        if self.system == "Darwin":
            self.mount_point = f"/Volumes/{name}"
        elif self.system == "Linux":
            self.mount_point = f"/dev/shm/{linux_dirname}"
        else:
            self.mount_point = os.path.join(_tmp, name)
        # 真内存盘，还是退化到了真实磁盘。使用方（以及 usage()）据此判断能不能
        # 指望"不落盘"这件事——见模块注释里 Windows 那段。
        self.memory_backed = self.system in ("Darwin", "Linux")
        self.use_ramdisk = False
        # macOS 引用计数：create 串行锁 + 每进程一个 pid 锁文件（活到进程结束的 fd）
        self._create_lock = os.path.join(_tmp, f".{name}.create.lock")
        self._users_dir = os.path.join(_tmp, f".{name}.users.d")
        self._pidfile = os.path.join(self._users_dir, f"{os.getpid()}.lock")
        self._users_fd = None
        # 重挂预告：谁要换盘就在这里留个条，别的进程取目录时会看见并等待
        self._remount_flag = os.path.join(_tmp, f".{name}.remount.pending")

    # ---------- 锁 ----------

    def _flock(self, fd, op):
        import fcntl
        fcntl.flock(fd, op)

    def _open_create_lock(self):
        """打开并独占 create 锁，返回 fd（调用方负责关闭以释放）"""
        import fcntl
        fd = os.open(self._create_lock, os.O_CREAT | os.O_RDWR, 0o666)
        fcntl.flock(fd, fcntl.LOCK_EX)
        return fd

    # ---------- 生命周期 ----------

    def ensure(self) -> bool:
        """确保内存盘可用；失败抛异常。"""
        if self.system == "Windows":
            # 真实磁盘上的普通目录，建出来就算就绪：没有挂载、也没有跨进程引用计数
            self._sweep_stale_windows()
            try:
                os.makedirs(self.mount_point, exist_ok=True)
            except Exception as e:
                raise RuntimeError(f"无法创建临时目录 {self.mount_point}: {e}")
            self.use_ramdisk = True
            return True

        if self.system == "Linux":
            try:
                os.makedirs(self.mount_point, exist_ok=True)
            except Exception as e:
                raise RuntimeError(f"无法创建 Linux tmpfs 目录 {self.mount_point}: {e}")
            self.use_ramdisk = True
            return True

        # macOS：create 锁下串行地检查/创建 + 注册本进程为活跃使用者
        import fcntl
        create_fd = self._open_create_lock()
        try:
            if not os.path.exists(self.mount_point):
                self._create_macos_ramdisk()
            self.use_ramdisk = True
            # 注册：独占本进程的 pid 锁文件，持有到进程结束（标记"我在用")
            if self._users_fd is None:
                os.makedirs(self._users_dir, exist_ok=True)
                self._users_fd = os.open(self._pidfile, os.O_CREAT | os.O_RDWR, 0o666)
                fcntl.flock(self._users_fd, fcntl.LOCK_EX)
        finally:
            fcntl.flock(create_fd, fcntl.LOCK_UN)
            os.close(create_fd)
        return True

    def _sweep_stale_windows(self) -> None:
        """清掉上一次留下的临时目录。**只在 Windows 这条分支上有意义。**

        cleanup() 挂在退出的 finally 里，进程被强杀（任务管理器、崩溃、门禁
        的硬退出）时根本跑不到，目录就留在磁盘上——而这条分支存的是真文件，
        留着既占地方，也违背"不留残留"那点仅存的承诺。

        **怎么确定没有别的实例正在用**：先把它改个名。Windows 上只要目录里
        还有进程打开着文件，改名就会失败——失败即说明有人在用，那就原样放着
        什么都不做。改名成功才删，删的也已经是改名后的那份，不会跟别人抢。
        比起加锁文件，这个判据借的是操作系统本来就有的保证，没有额外状态。
        """
        if not os.path.isdir(self.mount_point):
            return
        stale = f"{self.mount_point}.stale-{os.getpid()}"
        try:
            os.rename(self.mount_point, stale)
        except OSError:
            return          # 改不动 = 有别的实例在用，别碰
        try:
            shutil.rmtree(stale, ignore_errors=True)
            logger.info("清掉了上次残留的临时目录: %s", self.mount_point)
        except OSError as e:
            logger.warning("清理残留临时目录失败 %s: %s", stale, e)

    def _create_macos_ramdisk(self):
        """macOS 上用 hdiutil + diskutil 创建并挂载 RAM disk"""
        logger.info(f"创建 {self.size_mb}MB RAM disk: {self.mount_point}")
        blocks = self.size_mb * 2048  # 512 字节/块
        result = subprocess.run(
            ["hdiutil", "attach", "-nomount", f"ram://{blocks}"],
            capture_output=True, text=True, check=True,
        )
        device = result.stdout.strip().replace(" ", "").replace("\n", "")
        if not device:
            raise RuntimeError("hdiutil 未返回设备节点")
        subprocess.run(
            ["diskutil", "erasevolume", "HFS+", self.name, device],
            capture_output=True, check=True,
        )
        logger.info(f"✓ RAM disk 创建成功: {self.mount_point}")

    def cleanup(self):
        """释放本进程的使用引用；若已是最后一个使用者则卸载（仅 macOS）。"""
        if self.system == "Windows":
            # 落在真实磁盘上，退出时**必须尽力删掉**——这是 Windows 分支唯一能
            # 提供的"不留残留"，见模块注释。删不掉（文件被占用等）只记日志：
            # 清临时文件失败不该把正常退出变成报错。
            try:
                shutil.rmtree(self.mount_point, ignore_errors=False)
            except FileNotFoundError:
                pass
            except OSError as e:
                logger.warning("清理临时目录失败 %s: %s", self.mount_point, e)
            self.use_ramdisk = False
            return

        if self.system != "Darwin":
            # Linux 的 /dev/shm 目录共享，不删除（其它进程可能仍在用）
            self.use_ramdisk = False
            return

        import fcntl
        create_fd = self._open_create_lock()
        try:
            # 移除本进程自己的 pid 锁文件（释放锁 + 删文件）
            if self._users_fd is not None:
                try:
                    fcntl.flock(self._users_fd, fcntl.LOCK_UN)
                    os.close(self._users_fd)
                except OSError:
                    pass
                self._users_fd = None
            try:
                os.remove(self._pidfile)
            except OSError:
                pass

            if not os.path.exists(self.mount_point):
                self.use_ramdisk = False
                return

            # 逐个测别的 pid 文件：能 EX|NB 拿到=该进程已死(清掉)；拿不到=仍在用
            others_alive = self._others_alive()
            if others_alive:
                logger.info("内存盘仍被其它进程使用，跳过卸载")
            else:
                self._eject_macos()
        finally:
            fcntl.flock(create_fd, fcntl.LOCK_UN)
            os.close(create_fd)
        self.use_ramdisk = False

    def _others_alive(self) -> bool:
        """扫描 users 目录里别的 pid 锁文件；清理陈旧(已死)文件，返回是否还有活进程。"""
        import fcntl
        alive = False
        try:
            entries = os.listdir(self._users_dir)
        except OSError:
            return False
        for entry in entries:
            path = os.path.join(self._users_dir, entry)
            fd = None
            try:
                fd = os.open(path, os.O_RDWR)
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except (BlockingIOError, OSError):
                alive = True  # 锁不到 => 持锁进程仍活着
                if fd is not None:
                    os.close(fd)
                continue
            # 拿到了独占锁 => 持锁进程已死，文件陈旧，清掉
            try:
                os.remove(path)
            except OSError:
                pass
            fcntl.flock(fd, fcntl.LOCK_UN)
            os.close(fd)
        return alive

    def _eject_macos(self, force: bool = False):
        """macOS 上卸载 RAM disk。

        force=True 用于用户主动重挂：此时同机其它进程可能仍持有引用，
        普通 eject 会被拒（Resource busy），需要 -force。
        """
        try:
            result = subprocess.run(
                ["diskutil", "info", self.mount_point],
                capture_output=True, text=True,
            )
            device = None
            for line in result.stdout.split("\n"):
                if "Device Node" in line:
                    device = line.split(":")[1].strip()
                    break
            if device:
                cmd = ["diskutil", "eject", device]
                r = subprocess.run(cmd, capture_output=True, text=True)
                if r.returncode != 0 and force:
                    subprocess.run(["diskutil", "unmountDisk", "force", device],
                                   capture_output=True)
                    subprocess.run(["hdiutil", "detach", device, "-force"],
                                   capture_output=True)
                logger.info("✓ RAM disk 已卸载")
        except Exception as e:
            logger.error(f"卸载 RAM disk 失败: {e}")

    # ---------- 容量 ----------

    def usage(self) -> dict:
        """容量与占用。空间的事由这块盘自己交代，用的人不必各自去算。"""
        info = {
            'mount_point': self.mount_point,
            'system': self.system,
            'size_mb': self.size_mb,
            # 是不是真内存盘。Windows 上是磁盘目录，界面别再说"内存占用"
            'memory_backed': self.memory_backed,
            'mounted': os.path.exists(self.mount_point),
            'total_mb': 0.0, 'used_mb': 0.0, 'free_mb': 0.0,
            'entries': [],
        }
        if not info['mounted']:
            return info
        try:
            if self.system == "Windows":
                # 没有 os.statvfs。这里报的是**临时目录所在卷**的容量，不是
                # size_mb——Windows 分支没有容量上限这回事，size_mb 只是个摆设。
                du = shutil.disk_usage(self.mount_point)
                info['total_mb'] = round(du.total / 1048576, 1)
                info['free_mb'] = round(du.free / 1048576, 1)
                info['used_mb'] = round((du.total - du.free) / 1048576, 1)
            else:
                st = os.statvfs(self.mount_point)
                total = st.f_blocks * st.f_frsize
                free = st.f_bavail * st.f_frsize
                info['total_mb'] = round(total / 1048576, 1)
                info['free_mb'] = round(free / 1048576, 1)
                info['used_mb'] = round((total - free) / 1048576, 1)
        except OSError:
            pass
        # 按一级子目录报占用，好知道空间被谁吃了
        try:
            for name in sorted(os.listdir(self.mount_point)):
                p = os.path.join(self.mount_point, name)
                if name.startswith('.'):
                    continue
                size = 0
                if os.path.isdir(p):
                    for root, _dirs, files in os.walk(p):
                        for f in files:
                            try:
                                size += os.path.getsize(os.path.join(root, f))
                            except OSError:
                                pass
                else:
                    try:
                        size = os.path.getsize(p)
                    except OSError:
                        pass
                info['entries'].append({
                    'name': name, 'is_dir': os.path.isdir(p),
                    'size_mb': round(size / 1048576, 2),
                })
        except OSError:
            pass
        info['entries'].sort(key=lambda e: -e['size_mb'])
        return info

    # ---------- 重挂预告（像服务优雅停机那样打个招呼）----------

    def _read_pending(self):
        """读重挂预告：返回生效时间戳，没有或已失效返回 None"""
        try:
            with open(self._remount_flag) as f:
                deadline = float(f.read().strip())
        except (OSError, ValueError):
            return None
        # 发起方崩了会留下陈旧预告，超过容错窗口就当它不存在
        if time.time() > deadline + self.REMOUNT_STALE_SEC:
            try:
                os.remove(self._remount_flag)
            except OSError:
                pass
            return None
        return deadline

    def wait_if_remounting(self):
        """有人预告要重挂就等它挂好——**新操作不该在这个窗口里开始**。

        使用方不必自己判断：`get_temp_dir()` / `get_workspace()` 会先过这一道，
        所以"要重挂了"这件事对用它的代码是透明的，只表现为那一次取目录慢几秒。
        """
        waited = 0.0
        while True:
            deadline = self._read_pending()
            if deadline is None:
                return waited
            remain = deadline - time.time()
            if remain <= 0:
                # 已过生效时刻：重挂正在进行，再多等一会儿让新盘挂上
                remain = 0.3
            time.sleep(min(remain, 0.3))
            waited += min(remain, 0.3)
            if waited > self.REMOUNT_STALE_SEC:
                return waited          # 兜底，别无限等

    def remount(self, size_mb: int = None, grace_sec: float = None) -> dict:
        """按新大小重新挂载（**盘上现有内容全部丢失**）。

        macOS 的 RAM disk 大小在创建时固定，改大小只能卸载重挂——所以这不是
        "扩容"，是换一块。盘上放的都是临时文件（解析中转、试算产物），丢了
        重做即可，这也是它们当初被放在内存盘的原因。

        **动手前先打招呼**：写一份"重挂预告"，同机其它进程（backend、别的
        客户端）在取目录时会看到并等待，已经在跑的操作有 grace_sec 秒收尾。
        除了下载解析，这套系统里的操作基本都在 1 秒内完成，几秒宽限足够。
        没有这一步就是直接拔盘让别人自愈——能自愈，但正在写的那次会失败。
        """
        if self.system == 'Windows':
            # 普通磁盘目录，没有大小、也没有挂载，"重挂"无从谈起。
            # 想腾地方用 storage.clean。
            return {'success': True, 'skipped': True,
                    'message': 'Windows 使用磁盘临时目录，没有容量上限，无需重挂'}
        if self.system != 'Darwin':
            # Linux 的 /dev/shm 由内核按需分配，没有"重挂换大小"这件事
            return {'success': True, 'skipped': True,
                    'message': 'Linux 使用 tmpfs，容量由系统管理，无需重挂'}
        if size_mb:
            if size_mb < 64:
                raise ValueError('内存盘不能小于 64MB')
            self.size_mb = int(size_mb)
        grace = self.REMOUNT_GRACE_SEC if grace_sec is None else float(grace_sec)

        import fcntl
        # 预告放在 create 锁之外：要让别人**在我拿锁之前**就看见
        deadline = time.time() + grace
        try:
            with open(self._remount_flag, 'w') as f:
                f.write(str(deadline))
        except OSError as e:
            logger.warning('写重挂预告失败（继续重挂）: %s', e)
        logger.info('内存盘将在 %.1fs 后重挂为 %dMB，已通知其它使用者',
                    grace, self.size_mb)

        try:
            time.sleep(max(0.0, deadline - time.time()))
            create_fd = self._open_create_lock()
            try:
                if os.path.exists(self.mount_point):
                    self._eject_macos(force=True)
                self._create_macos_ramdisk()
                self.use_ramdisk = True
            finally:
                fcntl.flock(create_fd, fcntl.LOCK_UN)
                os.close(create_fd)
        finally:
            try:
                os.remove(self._remount_flag)
            except OSError:
                pass
        return {'success': True, 'size_mb': self.size_mb,
                'grace_sec': grace, 'mount_point': self.mount_point}

    # ---------- 取目录/工作区 ----------

    def get_temp_dir(self) -> str:
        """返回内存盘根目录（确保已就绪）。

        先看有没有人预告要重挂：有就等它挂完再返回——**新操作不该踩进重挂窗口**。
        对调用方透明，只表现为那一次取目录慢几秒。
        """
        self.wait_if_remounting()
        if not self.use_ramdisk or not os.path.exists(self.mount_point):
            self.ensure()
        return self.mount_point

    def get_workspace(self) -> str:
        """在内存盘下创建一个独立临时工作区目录并返回。失败抛异常。"""
        base = self.get_temp_dir()
        try:
            return tempfile.mkdtemp(dir=base)
        except Exception as e:
            raise RuntimeError(f"创建工作区失败 ({base}): {e}")


# 进程级单例：backend 与 filecore 各自一个，参数一致 => 同路径 => 共享同一块盘
_singleton = None


def get_ram_disk(
    size_mb: int = None,
    name: str = _DEFAULT_NAME,
    linux_dirname: str = _DEFAULT_LINUX_DIRNAME,
) -> RamDiskManager:
    """获取进程级 RAM Disk 管理单例（同名同路径的调用方共享同一块盘）。

    size_mb 只在**首次创建单例**时决定初始大小，之后传入只更新"下次重挂用多大"
    ——盘已经挂上了，改这个数不会让它当场变大（macOS 的 RAM disk 大小创建时固定）。

    name/linux_dirname 同理只在首次创建单例时生效；多个调用方要共享同一块盘，
    必须传相同的 name/linux_dirname（或都不传，用默认值）。
    """
    global _singleton
    if _singleton is None:
        _singleton = RamDiskManager(
            name=name,
            linux_dirname=linux_dirname,
            size_mb=size_mb or _DEFAULT_SIZE_MB,
        )
    elif size_mb and int(size_mb) != _singleton.size_mb:
        _singleton.size_mb = int(size_mb)
    return _singleton
