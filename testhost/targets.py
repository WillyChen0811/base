"""Machine-local execution target declarations; no scheduling or remote execution."""
import hashlib
import json
import re
from pathlib import Path


class TargetError(ValueError):
    pass


def arch_name(value):
    if not isinstance(value, str):
        raise TargetError('runtime architecture must be a string')
    value = {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(value, value)
    if value not in ('arm64', 'amd64'):
        raise TargetError('unsupported runtime architecture')
    return value


def _positive(value, field):
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise TargetError(field + ' must be a positive integer')
    return value


def _id(value):
    if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,79}', value):
        raise TargetError('invalid target/host identifier')
    return value


def validate_ssh_options(*, port=None, host_key_alias=None, known_hosts=None):
    """Validate named SSH settings; never accept arbitrary ssh options."""
    if port is not None and (type(port) is not int or not 1 <= port <= 65535):
        raise TargetError('ssh_port must be an integer from 1 to 65535')
    if host_key_alias is not None and (not isinstance(host_key_alias, str)
            or not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.:\[\]-]*', host_key_alias)):
        raise TargetError('invalid ssh_host_key_alias')
    if known_hosts is not None and (not isinstance(known_hosts, str) or not known_hosts
            or any(ord(c) < 32 or ord(c) == 127 for c in known_hosts)):
        raise TargetError('invalid ssh_known_hosts')


def validate_registry(data):
    """Validate budgets as configured reservations, not measured machine capacities."""
    if not isinstance(data, dict) or type(data.get('version')) is not int or data['version'] != 1:
        raise TargetError('unsupported target registry version')
    hosts, targets = data.get('hosts'), data.get('targets')
    if not isinstance(hosts, dict) or not isinstance(targets, dict) or not targets:
        raise TargetError('hosts and nonempty targets maps required')
    for name, host in hosts.items():
        _id(name)
        if not isinstance(host, dict):
            raise TargetError('host budget must be an object')
        _positive(host.get('cpu'), 'host cpu budget')
        _positive(host.get('memory_gb'), 'host memory_gb budget')
    normalized = {}
    for name, target in targets.items():
        _id(name)
        if not isinstance(target, dict):
            raise TargetError('target must be an object')
        t = dict(target)
        if t.get('backend') not in ('ssh', 'local') or not isinstance(t.get('host_id'), str) or t['host_id'] not in hosts:
            raise TargetError('unknown backend or host_id')
        if t.get('system') not in ('darwin', 'linux'):
            raise TargetError('unsupported runtime system')
        t['arch'] = arch_name(t.get('arch'))
        if type(t.get('emulated', False)) is not bool:
            raise TargetError('emulated must be boolean')
        t.setdefault('emulated', False)
        if t['backend'] == 'ssh':
            host = t.get('ssh_host')
            if not isinstance(host, str) or not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.@:\[\]-]*', host):
                raise TargetError('invalid ssh_host')
            validate_ssh_options(port=t.get('ssh_port'), host_key_alias=t.get('ssh_host_key_alias'),
                                 known_hosts=t.get('ssh_known_hosts'))
            if 'ssh_key' in t and (not isinstance(t['ssh_key'], str) or not t['ssh_key'] or '\x00' in t['ssh_key']):
                raise TargetError('invalid ssh_key')
        if 'image' in t and (not isinstance(t['image'], str) or not t['image']):
            raise TargetError('image must be a nonempty version/digest string')
        normalized[name] = t
    default = data.get('default_target')
    if not isinstance(default, str) or default not in normalized:
        raise TargetError('default_target not declared')
    return dict(data, hosts={k: dict(v) for k, v in hosts.items()}, targets=normalized)


def load_registry(path):
    """Only an explicitly supplied machine-local file is read."""
    try:
        return validate_registry(json.loads(Path(path).expanduser().read_text()))
    except (OSError, json.JSONDecodeError) as exc:
        raise TargetError('cannot read target registry: ' + str(exc)) from exc


def select_target(registry=None, name=None, *, legacy_host=None, legacy_key=None):
    """No registry preserves caller-provided old connection values without embedding IPs."""
    if registry is None:
        if name not in (None, 'studio-macos-arm64'):
            raise TargetError('named target requires machine-local registry')
        return {'id': 'studio-macos-arm64', 'backend': 'ssh', 'host_id': 'studio',
                'system': 'darwin', 'arch': 'arm64', 'emulated': False,
                'ssh_host': legacy_host, 'ssh_key': legacy_key, 'legacy': True}
    registry = validate_registry(registry)
    name = name or registry['default_target']
    if not isinstance(name, str) or name not in registry['targets']:
        raise TargetError('unknown execution target: ' + str(name))
    return dict(registry['targets'][name], id=name, legacy=False)


def check_requirements(target, requirements, *, runtime=None):
    """Check declaration early, and require a trusted probe before approving native execution.

    runtime is supplied by the execution adapter after probing, never read from registry.
    Missing native proof is an error; callers can perform a separate declaration preflight.
    """
    if not isinstance(requirements, dict) or set(requirements) - {'system', 'arch', 'native'}:
        raise TargetError('unsupported task platform requirements')
    if 'native' in requirements and type(requirements['native']) is not bool:
        raise TargetError('native must be boolean')
    actual = runtime if runtime is not None else target
    if requirements.get('system') is not None and requirements['system'] not in ('darwin', 'linux'):
        raise TargetError('unsupported required system')
    if requirements.get('system') is not None and requirements['system'] != actual.get('system'):
        raise TargetError('runtime system mismatch')
    if 'arch' in requirements and arch_name(requirements['arch']) != arch_name(actual.get('arch')):
        raise TargetError('runtime architecture mismatch')
    if requirements.get('native'):
        if (not isinstance(runtime, dict) or runtime.get('system') not in ('darwin', 'linux')
                or runtime.get('arch') not in ('arm64', 'aarch64', 'amd64', 'x86_64')
                or runtime.get('emulated') is not False):
            raise TargetError('native execution requires verified runtime proof')
    return True


def cache_namespace(runtime):
    """Namespace prefix; empty preserves legacy native macOS arm64 environment hashes.

    This function takes adapter-probed runtime identity, not registry declarations.
    image must identify the actual VM/container base when one is used.
    """
    system = runtime.get('system')
    arch = arch_name(runtime.get('arch'))
    emulated = runtime.get('emulated')
    if system not in ('darwin', 'linux') or type(emulated) is not bool:
        raise TargetError('incomplete runtime identity')
    image = runtime.get('image')
    if image is not None and (not isinstance(image, str) or not image):
        raise TargetError('invalid runtime image identity')
    if (system, arch, emulated, image) == ('darwin', 'arm64', False, None):
        return ''
    identity = {'system': system, 'arch': arch, 'emulated': emulated, 'image': image}
    return 'rt-' + hashlib.sha256(json.dumps(identity, sort_keys=True).encode()).hexdigest()[:12]
