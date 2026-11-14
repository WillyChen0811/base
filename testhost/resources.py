"""Atomic per-host resource leases. Executors supply verified run lifecycle identity.

This module neither starts nor kills processes. Its queue is admission control only.
"""
import contextlib
import fcntl
import json
import os
import re
import tempfile
from pathlib import Path


class ResourceError(ValueError):
    pass


def identifier(value):
    if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,79}', value):
        raise ResourceError('invalid host/run identifier')
    return value


def positive(value):
    if type(value) is not int or value <= 0:
        raise ResourceError('CPU and memory budgets must be positive integers')
    return value


def owner_identity(owner):
    if not isinstance(owner, dict) or set(owner) != {'pid', 'start', 'uid'}:
        raise ResourceError('owner requires verified pid/start/uid identity')
    positive(owner['pid'])
    if type(owner['uid']) is not int or owner['uid'] < 0:
        raise ResourceError('invalid owner uid')
    if not isinstance(owner['start'], str) or not owner['start']:
        raise ResourceError('invalid owner process start identity')
    return dict(owner)


class LeasePool:
    def __init__(self, state_dir, host_id, cpu, memory_gb):
        self.host_id = identifier(host_id)
        self.budget = {'cpu': positive(cpu), 'memory_gb': positive(memory_gb)}
        self.root = Path(state_dir)
        self.root.mkdir(parents=True, exist_ok=True)
        self.path = self.root / (host_id + '.json')
        self.lock = self.root / (host_id + '.lock')

    @contextlib.contextmanager
    def _state(self):
        with self.lock.open('a+') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            if self.path.exists():
                try:
                    state = json.loads(self.path.read_text())
                except (OSError, ValueError) as exc:
                    raise ResourceError('invalid lease state; refusing to discard reservations') from exc
                if (state.get('version') != 1 or state.get('host_id') != self.host_id
                        or state.get('budget') != self.budget):
                    raise ResourceError('lease state identity/budget mismatch')
                if not isinstance(state.get('leases'), dict) or type(state.get('sequence')) is not int:
                    raise ResourceError('invalid lease state')
            else:
                state = {'version': 1, 'host_id': self.host_id, 'budget': self.budget,
                         'sequence': 0, 'leases': {}}
            yield state
            fd, name = tempfile.mkstemp(prefix=self.path.name + '.', dir=self.root)
            try:
                with os.fdopen(fd, 'w') as output:
                    json.dump(state, output, sort_keys=True, indent=2)
                    output.write('\n')
                    output.flush()
                    os.fsync(output.fileno())
                os.replace(name, self.path)
            finally:
                if os.path.exists(name):
                    os.unlink(name)

    @staticmethod
    def _usage(state, parent):
        active = [x for x in state['leases'].values()
                  if x['state'] == 'running' and x['parent'] == parent]
        return {key: sum(x[key] for x in active) for key in ('cpu', 'memory_gb')}

    def _promote(self, state):
        blocked = set()
        for run_id, lease in sorted(state['leases'].items(), key=lambda item: item[1]['sequence']):
            if lease['state'] != 'queued':
                continue
            parent = lease['parent']
            if parent is not None and state['leases'][parent]['state'] != 'running':
                lease['reason'] = 'waiting for VM reservation'
                continue
            if parent in blocked:
                lease['reason'] = 'waiting behind an earlier request in the same pool'
                continue
            capacity = state['leases'][parent] if parent else self.budget
            used = self._usage(state, parent)
            if any(used[key] + lease[key] > capacity[key] for key in self.budget):
                blocked.add(parent)
                lease['reason'] = 'CPU or memory budget occupied'
                continue
            lease.update(state='running', reason=None)

    def request(self, run_id, cpu, memory_gb, owner, *, parent=None, kind='task'):
        """Idempotent admission; owner is a waiting executor/supervisor, not a guest PID."""
        identifier(run_id)
        cpu, memory_gb, owner = positive(cpu), positive(memory_gb), owner_identity(owner)
        if kind not in ('task', 'vm') or (kind == 'vm' and parent is not None):
            raise ResourceError('only host-level VM reservations are supported')
        if parent is not None:
            identifier(parent)
        with self._state() as state:
            if parent is not None:
                vm = state['leases'].get(parent)
                if vm is None or vm['kind'] != 'vm':
                    raise ResourceError('parent VM reservation does not exist')
                capacity = vm
            else:
                capacity = self.budget
            if cpu > capacity['cpu'] or memory_gb > capacity['memory_gb']:
                raise ResourceError('request exceeds total pool budget')
            fields = {'cpu': cpu, 'memory_gb': memory_gb, 'owner': owner,
                      'parent': parent, 'kind': kind}
            if run_id in state['leases']:
                if any(state['leases'][run_id][k] != v for k, v in fields.items()):
                    raise ResourceError('run already has a different lease identity/request')
            else:
                state['sequence'] += 1
                state['leases'][run_id] = dict(fields, state='queued', reason='awaiting admission',
                                              sequence=state['sequence'])
            self._promote(state)
            return dict(state['leases'][run_id])

    def release(self, run_id, owner):
        """Called only after the trusted executor confirms run and its children have stopped."""
        identifier(run_id)
        owner = owner_identity(owner)
        with self._state() as state:
            if run_id in state['leases'] and state['leases'][run_id]['owner'] != owner:
                raise ResourceError('release owner identity mismatch')
            if any(x['parent'] == run_id for x in state['leases'].values()):
                raise ResourceError('release guest leases before VM reservation')
            removed = state['leases'].pop(run_id, None) is not None
            self._promote(state)
            return removed

    def recover(self, probe):
        """probe(run_id, owner, kind) -> alive/dead/unknown, from trusted executor.

        A missing/reused supervisor PID alone is NOT proof that orphaned work has stopped.
        'dead' for a VM must prove its guest environment is stopped too. Probe must be
        quick/nonblocking because admission remains locked during reconciliation.
        """
        with self._state() as state:
            dead = set()
            for run_id, lease in state['leases'].items():
                try:
                    status = probe(run_id, dict(lease['owner']), lease['kind'])
                except Exception:
                    status = 'unknown'
                if status not in ('alive', 'dead', 'unknown'):
                    raise ResourceError('invalid recovery probe result')
                if status == 'dead':
                    dead.add(run_id)
            # A confirmed stopped VM implies no live guest work. No processes are killed here.
            dead.update(run_id for run_id, lease in state['leases'].items() if lease['parent'] in dead)
            for run_id in dead:
                state['leases'].pop(run_id, None)
            self._promote(state)
            return sorted(dead)

    def snapshot(self):
        with self._state() as state:
            self._promote(state)
            return json.loads(json.dumps(dict(state, used=self._usage(state, None))))
