import copy
import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from targets import (TargetError, cache_namespace, check_requirements, load_registry,
                     select_target, validate_registry)


def registry():
    return {'version': 1, 'default_target': 'linux',
            'hosts': {'studio': {'cpu': 12, 'memory_gb': 64}},
            'targets': {'linux': {'backend': 'ssh', 'host_id': 'studio',
                                 'ssh_host': 'tester@vm', 'system': 'linux', 'arch': 'aarch64'}}}


def test_registry_selection_and_legacy(tmp_path):
    p = tmp_path / 'targets.json'
    p.write_text(json.dumps(registry()))
    loaded = load_registry(p)
    assert select_target(loaded)['arch'] == 'arm64'
    assert select_target(legacy_host='custom', legacy_key='key')['ssh_host'] == 'custom'
    assert select_target()['ssh_host'] is None
    with pytest.raises(TargetError):
        select_target(name='linux')
    with pytest.raises(TargetError):
        select_target(loaded, 'missing')


@pytest.mark.parametrize('field,value', [('cpu', 0), ('cpu', True), ('memory_gb', -1), ('memory_gb', 1.5)])
def test_invalid_budgets(field, value):
    d = registry()
    d['hosts']['studio'][field] = value
    with pytest.raises(TargetError):
        validate_registry(d)


@pytest.mark.parametrize('field,value', [('arch', []), ('system', 'windows'),
    ('host_id', []), ('backend', 'shell'), ('ssh_host', '-oProxyCommand=x'), ('emulated', 'false')])
def test_invalid_targets(field, value):
    d = registry()
    d['targets']['linux'][field] = value
    with pytest.raises(TargetError):
        validate_registry(d)


def test_requirements_and_runtime_native_proof():
    target = select_target(registry())
    assert check_requirements(target, {'system': 'linux', 'arch': 'arm64'})
    for req in ({'system': 'darwin'}, {'arch': 'amd64'}, {'native': True}, {'native': 'yes'}, {'unknown': 1}):
        with pytest.raises(TargetError):
            check_requirements(target, req)
    # Configuration claiming native is insufficient; actual adapter probe is required.
    target['emulated'] = False
    with pytest.raises(TargetError):
        check_requirements(target, {'native': True})
    with pytest.raises(TargetError):
        check_requirements(target, {'native': True}, runtime={'emulated': False})
    runtime = {'system': 'linux', 'arch': 'arm64', 'emulated': False}
    assert check_requirements(target, {'native': True}, runtime=runtime)
    with pytest.raises(TargetError):
        check_requirements(target, {'native': True}, runtime=dict(runtime, emulated=True))


def test_runtime_cache_separation():
    mac = {'system': 'darwin', 'arch': 'arm64', 'emulated': False}
    assert cache_namespace(mac) == ''
    linux = dict(mac, system='linux')
    identities = [linux, dict(linux, image='ubuntu@a'), dict(linux, image='ubuntu@b'),
                  dict(linux, arch='amd64', emulated=True), dict(linux, arch='amd64')]
    assert len({cache_namespace(x) for x in identities}) == len(identities)
    assert cache_namespace(dict(linux, arch='aarch64')) == cache_namespace(linux)
    assert cache_namespace(linux) == cache_namespace(copy.deepcopy(linux))
    with pytest.raises(TargetError):
        cache_namespace({'system': 'linux', 'arch': 'arm64'})
