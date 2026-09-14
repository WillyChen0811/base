"""内存盘的跨进程行为测试：引用计数、重挂预告、宽限期。

**必须用真子进程**：flock 的引用计数按进程算，用线程测不出真相（同进程内
重复加锁会被放行）。所以这里用 subprocess 起独立解释器扮演"另一个客户端"。

这套逻辑光看代码看不出对错——"谁退出该不该卸载""重挂时别人会不会写失败"
都是时序问题，只有真跑一遍才知道。改 ram_disk.py 之后请务必跑这个文件。

本机可能有真实的 backend/filecore 在跑并共用这块盘，所以断言都写成
"相对于本测试之外的使用者"，不假设自己是唯一使用者。

    pytest filecore/tests/test_ram_disk_multiproc.py -v -s
"""

import json
import os
import subprocess
import sys
import tempfile
import time

import pytest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # base/py
sys.path.insert(0, REPO)

from pybase.ramdisk import get_ram_disk       # noqa: E402

pytestmark = pytest.mark.skipif(
    sys.platform != 'darwin',
    reason='内存盘的挂载/重挂行为是 macOS 专有；Linux 用 tmpfs，无此语义',
)

_USERS_DIR = f"{tempfile.gettempdir()}/.PyBaseTemp.users.d"

# 子进程：一个"客户端"，按 stdin 指令做事、stdout 回报
_WORKER = r'''
import json, os, sys
sys.path.insert(0, %r)
from pybase.ramdisk import get_ram_disk

def say(**kw):
    print(json.dumps(kw), flush=True)

rd = get_ram_disk()
for line in sys.stdin:
    c = line.strip()
    if not c:
        continue
    try:
        if c == 'ensure':
            rd.ensure()
            say(ok=True, exists=os.path.exists(rd.mount_point))
        elif c == 'write':
            d = os.path.join(rd.get_temp_dir(), 'proc_' + sys.argv[1])
            os.makedirs(d, exist_ok=True)
            with open(os.path.join(d, 'data.bin'), 'wb') as f:
                f.write(b'x' * 1048576)
            say(ok=True)
        elif c == 'read':
            p = os.path.join(rd.get_temp_dir(), 'proc_' + sys.argv[1], 'data.bin')
            say(ok=True, exists=os.path.exists(p))
        elif c == 'usage':
            u = rd.usage()
            say(ok=True, total=u['total_mb'])
        elif c == 'quit':
            rd.cleanup()
            say(ok=True, still_mounted=os.path.exists(rd.mount_point))
            break
    except Exception as e:
        say(ok=False, error=f'{type(e).__name__}: {e}'[:160])
''' % REPO

# 子进程：持续做短读写，用来验证重挂宽限期不会打断正在跑的操作
_BUSY = r'''
import json, os, sys, time
sys.path.insert(0, %r)
from pybase.ramdisk import get_ram_disk
rd = get_ram_disk(); rd.ensure()
ok = fails = 0; waits = []
end = time.time() + float(sys.argv[1])
while time.time() < end:
    t0 = time.time()
    try:
        d = os.path.join(rd.get_temp_dir(), 'busy')
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, 'f.bin'), 'wb') as f:
            f.write(b'y' * 1048576)
        ok += 1
    except Exception:
        fails += 1
    dt = time.time() - t0
    if dt > 1:
        waits.append(round(dt, 1))
    time.sleep(0.25)
print(json.dumps({'ok': ok, 'fails': fails, 'waits': waits}), flush=True)
''' % REPO


def _outsiders() -> int:
    """本测试之外、本机还在用这块盘的进程数（比如用户自己跑的 backend）"""
    try:
        return len([f for f in os.listdir(_USERS_DIR) if f.endswith('.lock')])
    except OSError:
        return 0


class Client:
    """一个扮演客户端的子进程"""

    def __init__(self, name):
        self.name = name
        self.p = subprocess.Popen(
            [sys.executable, '-c', _WORKER, name],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1,
            env={**os.environ, 'PYTHONWARNINGS': 'ignore'})

    def do(self, cmd):
        self.p.stdin.write(cmd + '\n')
        self.p.stdin.flush()
        line = self.p.stdout.readline()
        assert line.strip().startswith('{'), f'{self.name} 无响应: {line!r}'
        return json.loads(line)

    def quit(self):
        r = self.do('quit')
        self.p.wait(timeout=30)
        return r


@pytest.fixture
def clients():
    made = []

    def make(*names):
        for n in names:
            made.append(Client(n))
        return made

    yield make
    for c in made:
        if c.p.poll() is None:
            try:
                c.quit()
            except Exception:            # noqa: BLE001
                c.p.kill()


def test_多客户端共用一块盘_互不干扰(clients):
    base = _outsiders()
    c1, c2, c3 = clients('c1', 'c2', 'c3')
    for c in (c1, c2, c3):
        assert c.do('ensure')['ok']
        assert c.do('write')['ok']
    # 三个子进程都注册成了使用者
    assert _outsiders() >= base + 3
    # 各写各的，谁都还在
    for c in (c1, c2, c3):
        assert c.do('read')['exists'], f'{c.name} 的文件被别人清掉了'


def test_退出时按引用计数卸载_有人在用就不拔盘(clients):
    c1, c2 = clients('a1', 'a2')
    c1.do('ensure')
    c2.do('ensure')
    # c1 退出时 c2 还在 => 盘必须留着
    assert c1.quit()['still_mounted'], 'c2 仍在用，盘却被卸载了'
    assert c2.do('read')['ok'], 'c1 退出后 c2 不可用'
    r = c2.quit()
    if _outsiders() == 0:
        assert not r['still_mounted'], '已无使用者，盘却仍挂着'


def test_盘被卸载后_新进程能自己挂起来(clients):
    (c,) = clients('newcomer')
    rd = get_ram_disk()
    if not os.path.exists(rd.mount_point) and _outsiders() == 0:
        pass                              # 盘确实不在，正好测自挂
    assert c.do('ensure')['exists'], '新进程无法挂起内存盘'
    assert c.do('write')['ok']


def test_重挂有宽限期_正在跑的操作不会失败():
    """核心用例：重挂前先预告，别的进程取目录时会等到新盘挂好。

    没有这道预告就是直接拔盘——能自愈，但正在写的那一次必然失败。
    """
    rd = get_ram_disk()
    rd.ensure()
    before = rd.usage()['total_mb']
    target = 768 if before != 768 else 640

    busy = subprocess.Popen(
        [sys.executable, '-c', _BUSY, '12'],
        stdout=subprocess.PIPE, text=True,
        env={**os.environ, 'PYTHONWARNINGS': 'ignore'})
    try:
        time.sleep(3)                     # 让它先跑起来
        t0 = time.time()
        r = rd.remount(target, grace_sec=5)
        elapsed = time.time() - t0
        assert r['success'] and r['size_mb'] == target
        assert elapsed >= 5, f'宽限期没生效，只用了 {elapsed:.1f}s'

        out, _ = busy.communicate(timeout=60)
    finally:
        if busy.poll() is None:
            busy.kill()

    stats = json.loads([l for l in out.strip().split('\n')
                        if l.strip().startswith('{')][-1])
    assert stats['ok'] > 0, '陪跑进程一次都没成功，测试本身没跑起来'
    assert stats['fails'] == 0, (
        f"重挂期间有 {stats['fails']} 次操作失败——宽限期没起作用")
    # 至少有一次调用被挡住并等待（正好撞上窗口）
    assert stats['waits'], '没有任何调用被宽限期挡住，可能预告没被看到'


def test_陈旧的重挂预告会自动失效():
    """发起方崩在半路留下预告，不能把所有人永远卡住"""
    rd = get_ram_disk()
    rd.ensure()
    with open(rd._remount_flag, 'w') as f:
        f.write(str(time.time() - rd.REMOUNT_STALE_SEC - 10))
    try:
        t0 = time.time()
        rd.get_temp_dir()                 # 不该被卡住
        assert time.time() - t0 < 2, '陈旧预告把取目录卡住了'
        assert not os.path.exists(rd._remount_flag), '陈旧预告没有被清掉'
    finally:
        if os.path.exists(rd._remount_flag):
            os.remove(rd._remount_flag)
