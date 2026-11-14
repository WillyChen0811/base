import concurrent.futures
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from resources import LeasePool, ResourceError

OWNER = {'pid': 123, 'start': 'verified-process-start-1', 'uid': 1000}


def test_budget_queue_fifo_and_owner_release(tmp_path):
    pool = LeasePool(tmp_path, 'studio', 8, 32)
    assert pool.request('a', 6, 16, OWNER)['state'] == 'running'
    assert pool.request('b', 4, 8, OWNER)['state'] == 'queued'
    assert pool.request('c', 1, 1, OWNER)['state'] == 'queued'
    assert pool.snapshot()['used'] == {'cpu': 6, 'memory_gb': 16}
    with pytest.raises(ResourceError, match='owner'):
        pool.release('a', dict(OWNER, start='reused-pid'))
    assert pool.release('a', OWNER)
    snapshot = pool.snapshot()
    assert snapshot['leases']['b']['state'] == snapshot['leases']['c']['state'] == 'running'
    assert not pool.release('a', OWNER)


def test_vm_reservation_not_counted_twice(tmp_path):
    pool = LeasePool(tmp_path, 'studio', 12, 64)
    pool.request('vm', 8, 32, OWNER, kind='vm')
    pool.request('guest', 8, 32, OWNER, parent='vm')
    pool.request('mac', 4, 16, OWNER)
    assert pool.snapshot()['used'] == {'cpu': 12, 'memory_gb': 48}
    assert pool.request('guest2', 1, 1, OWNER, parent='vm')['state'] == 'queued'
    with pytest.raises(ResourceError, match='guest'):
        pool.release('vm', OWNER)
    with pytest.raises(ResourceError):
        pool.request('nested', 1, 1, OWNER, parent='vm', kind='vm')
    with pytest.raises(ResourceError):
        pool.request('orphan', 1, 1, OWNER, parent='absent')


def test_recovery_unknown_and_identity_reuse(tmp_path):
    pool = LeasePool(tmp_path, 'studio', 4, 8)
    pool.request('a', 4, 8, OWNER)
    pool.request('b', 4, 8, OWNER)
    assert pool.recover(lambda *args: 'unknown') == []
    def broken(*args):
        raise OSError('probe unavailable')
    assert pool.recover(broken) == []
    seen = []
    def probe(run_id, owner, kind):
        seen.append((run_id, owner))
        return 'dead' if run_id == 'a' else 'alive'
    assert pool.recover(probe) == ['a']
    assert seen[0][1] == OWNER
    assert pool.snapshot()['leases']['b']['state'] == 'running'


def test_confirmed_stopped_vm_releases_guests(tmp_path):
    pool = LeasePool(tmp_path, 'studio', 4, 8)
    pool.request('vm', 4, 8, OWNER, kind='vm')
    pool.request('guest', 4, 8, OWNER, parent='vm')
    assert pool.recover(lambda run, *_: 'dead' if run == 'vm' else 'unknown') == ['guest', 'vm']
    assert pool.snapshot()['used']['cpu'] == 0


@pytest.mark.parametrize('cpu,memory', [(True, 1), (0, 1), (-1, 1), (1, False), (1, -1), (1, 2.5)])
def test_invalid_resources(tmp_path, cpu, memory):
    pool = LeasePool(tmp_path, 'studio', 4, 8)
    with pytest.raises(ResourceError):
        pool.request('a', cpu, memory, OWNER)


def test_invalid_owner_and_duplicate_request(tmp_path):
    pool = LeasePool(tmp_path, 'studio', 4, 8)
    for owner in ({}, dict(OWNER, pid=True), dict(OWNER, start=''), dict(OWNER, uid=-1)):
        with pytest.raises(ResourceError):
            pool.request('a', 1, 1, owner)
    assert pool.request('a', 1, 1, OWNER) == pool.request('a', 1, 1, OWNER)
    with pytest.raises(ResourceError):
        pool.request('a', 2, 1, OWNER)
    with pytest.raises(ResourceError):
        pool.request('oversize', 5, 1, OWNER)


def test_atomic_concurrent_admission(tmp_path):
    def request(index):
        return LeasePool(tmp_path, 'studio', 4, 8).request('r' + str(index), 1, 2, OWNER)
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as executor:
        results = list(executor.map(request, range(20)))
    assert sum(x['state'] == 'running' for x in results) == 4
    pool = LeasePool(tmp_path, 'studio', 4, 8)
    assert len(pool.snapshot()['leases']) == 20
    assert pool.snapshot()['used'] == {'cpu': 4, 'memory_gb': 8}


def test_host_isolation_budget_changes_and_corrupt_state(tmp_path):
    first = LeasePool(tmp_path, 'studio', 4, 8)
    first.request('a', 4, 8, OWNER)
    second = LeasePool(tmp_path, 'ubuntu', 4, 8)
    assert not second.release('a', OWNER)
    assert first.snapshot()['used']['cpu'] == 4
    with pytest.raises(ResourceError, match='mismatch'):
        LeasePool(tmp_path, 'studio', 8, 8).snapshot()
    first.path.write_text('corrupt')
    with pytest.raises(ResourceError, match='invalid'):
        first.snapshot()
