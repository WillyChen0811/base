#!/usr/bin/env python3
"""SSH forced-command relay for trusted testhost developers (standard library only)."""
import argparse
import json
import os
from pathlib import Path
import re
import shlex
import sys

from client import Transport, THError


class BrokerError(ValueError):
    pass


NAME = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]{0,79}\Z')
SINGLE = {'ping', 'inventory', 'capabilities', 'update', 'media-spec'}
NAMED = {'sync', 'snapshot', 'status', 'fetch', 'fetchx', 'artifacts', 'cancel'}
ACTIONS = SINGLE | NAMED | {'plugin', 'prepare', 'run', 'clean', 'media-put',
                             'media-audio', 'media-frames', 'media-clean'}


def command_line(command):
    """Parse a protocol command, never pass unparsed shell source to the next hop.

    Studio's forced-command entry owns full action-specific validation, including
    task arguments and media ranges. Here restrict the protocol envelope and IDs.
    """
    if not isinstance(command, str) or any(ord(c) < 32 or ord(c) == 127 for c in command):
        raise BrokerError('invalid command characters')
    try:
        args = shlex.split(command)
    except ValueError as e:
        raise BrokerError('invalid command quoting') from e
    if not args or args[0] not in ACTIONS:
        raise BrokerError('unknown testhost action')
    action, rest = args[0], args[1:]
    if action in SINGLE:
        valid = not rest
    elif action in NAMED:
        valid = len(rest) == 1 and bool(NAME.fullmatch(rest[0]))
    elif action == 'plugin':
        valid = (len(rest) == 2 and bool(NAME.fullmatch(rest[0]))
                 and bool(re.fullmatch(r'[A-Za-z0-9_]{1,60}\.py', rest[1])))
    elif action in {'prepare', 'run'}:
        valid = len(rest) >= 2 and bool(NAME.fullmatch(rest[0]))
        if action == 'prepare':
            valid = valid and any(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,79}=[0-9a-f]{7,40}', x)
                                  for x in rest[1:])
        else:
            valid = valid and not rest[1].startswith(('/', '-')) and '..' not in rest[1]
    elif action == 'clean':
        valid = (rest in [['runs'], ['envs'], ['snapshots'], ['repos']]
                 or (len(rest) == 2 and rest[0] == 'run' and bool(NAME.fullmatch(rest[1])))
                 or (len(rest) == 3 and rest[:2] == ['runs', '--keep']
                     and bool(re.fullmatch(r'[0-9]{1,3}', rest[2]))))
    elif action == 'media-clean':
        valid = ((len(rest) == 1 and bool(re.fullmatch(r'[0-9a-f]{64}', rest[0])))
                 or (len(rest) == 2 and rest[0] == '--older-than'
                     and bool(re.fullmatch(r'[0-9]{1,4}(\.[0-9]{1,3})?', rest[1]))))
    else:
        valid = bool(rest) and bool(re.fullmatch(r'[0-9a-f]{64}', rest[0]))
        if action == 'media-put':
            valid = valid and (len(rest) == 2 or (len(rest) == 4 and rest[2] == '--size' and rest[3].isdigit()))
        elif action == 'media-audio':
            valid = valid and (len(rest) == 1 or (len(rest) == 3 and rest[1] == '--segment-min'))
        else:
            valid = valid and (len(rest) in (3, 5) and rest[1] == '--at'
                               and (len(rest) == 3 or rest[3] == '--width'))
    if not valid:
        raise BrokerError('invalid testhost action arguments')
    return shlex.join(args)


def load_config(path):
    """Machine-local upstream, never chosen through SSH_ORIGINAL_COMMAND."""
    data = json.loads(Path(path).expanduser().read_text())
    allowed = {'host', 'key', 'port', 'host_key_alias', 'known_hosts'}
    if not isinstance(data, dict) or set(data) - allowed:
        raise BrokerError('invalid broker configuration fields')
    for key in ('host', 'key', 'known_hosts'):
        if not isinstance(data.get(key), str) or not data[key]:
            raise BrokerError('broker configuration requires ' + key)
    if data['host'].startswith('-') or any(c.isspace() or ord(c) < 32 or ord(c) == 127 for c in data['host']):
        raise BrokerError('invalid upstream host')
    if 'port' in data and (type(data['port']) is not int or not 1 <= data['port'] <= 65535):
        raise BrokerError('invalid upstream port')
    if 'host_key_alias' in data and (not isinstance(data['host_key_alias'], str)
                                    or not data['host_key_alias'] or any(c.isspace() or ord(c) < 32 or ord(c) == 127
                                                                         for c in data['host_key_alias'])):
        raise BrokerError('invalid upstream host_key_alias')
    for key in ('key', 'known_hosts'):
        if any(ord(c) < 32 or ord(c) == 127 for c in data[key]):
            raise BrokerError('invalid upstream ' + key)
        data[key] = str(Path(data[key]).expanduser())
    return data


def relay_argv(config, command):
    canonical = command_line(command)
    tr = Transport(config['host'], config['key'], port=config.get('port'),
                   host_key_alias=config.get('host_key_alias'), known_hosts=config['known_hosts'])
    tr.local = None  # TESTHOST_LOCAL is only a test-client facility, never a broker route.
    argv, _ = tr._argv(canonical)
    return argv


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True, help='Ubuntu-local upstream JSON file')
    a = parser.parse_args(argv)
    try:
        config = load_config(a.config)
        argv = relay_argv(config, os.environ.get('SSH_ORIGINAL_COMMAND', ''))
        # exec preserves stdin/stdout/stderr (including large binary streams) and
        # the upstream SSH exit status; no Python buffers, temporary tar or shell.
        os.execvp(argv[0], argv)
    except (OSError, ValueError, THError) as e:
        print(json.dumps({'success': False, 'code': 'broker_error', 'message': str(e)}), file=sys.stderr)
        return 3


if __name__ == '__main__':
    sys.exit(main())
