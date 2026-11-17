# Target registry contract v1 (foundation only)

This module parses declarations; it does not install VMs, probe runtime architecture,
dispatch tasks, enforce host budgets or implement a queue. The client supports an explicit machine-local registry for existing macOS SSH targets:

`python client.py --targets-file ~/.config/testhost/targets.json --target studio-macos-arm64 run ...`

The two options precede the subcommand. With neither option the old host/key path
is unchanged. Task `requirements` override corresponding top-level `requirements`.
The selected target and live `ping.runtime` are checked before repository synchronization.
Older endpoints without runtime native proof are rejected only for native-required tasks.
Registry `local` and Linux selections are explicitly rejected by this CLI until their
adapters are integrated. Trusted Python callers may inject `main(..., transport_factory=...)`
to reuse the same workflow with a controlled adapter; this adds no arbitrary-shell CLI.

Machine-local registry (do not commit connection details into project manifests):

```json
{
  "version": 1,
  "default_target": "studio-macos-arm64",
  "hosts": {"studio": {"cpu": 12, "memory_gb": 64}},
  "targets": {
    "studio-macos-arm64": {
      "backend": "ssh", "host_id": "studio", "ssh_host": "account@studio",
      "ssh_key": "~/.ssh/testhost", "system": "darwin", "arch": "arm64"
    },
    "studio-linux-arm64": {
      "backend": "ssh", "host_id": "studio", "ssh_host": "account@linux-vm",
      "system": "linux", "arch": "arm64", "image": "ubuntu-image-digest"
    }
  }
}
```

`hosts` values are administrator-configured budgets, not measured CPU/RAM capacity.
Targets on the same host share its future scheduler budget. VM reservation accounting
must avoid double counting guest task resources. No limit is enforced by this module.

Proposed project task requirements:
`{"system":"linux","arch":"arm64","native":true}`. Only these three keys are allowed.
Omit optional requirements to preserve existing task behavior. `aarch64` is normalized
to `arm64`, and `x86_64` to `amd64`. Unknown targets/platforms fail explicitly.

`select_target()` with no registry keeps caller-supplied legacy connection values;
it embeds no default IP/key. The configured target's `emulated` flag is an expectation,
not evidence of native execution. `check_requirements(native=true)` requires a separate
runtime object produced by a trusted adapter probe. The adapter must not copy registry
claims into that object without checking the actual running OS/process architecture.

`cache_namespace(runtime)` must use actual adapter-probed runtime identity. Native
macOS arm64 with no VM image returns the empty prefix to preserve existing environment
hashes; other environments return an opaque prefix. VM/container image must be immutable
or versioned. Future integration includes this prefix in environment cache paths.
Cache names alone do not establish compatibility or provenance.


## SSH through a forwarded port

The client supports the same test protocol over a direct Studio SSH connection or
an SSH port forwarded by wgtund. The caller retains its Studio private key; the
forwarder only carries bytes. For example, after forwarding local port 12222 to
Studio port 22:

```sh
python client.py --host tester@127.0.0.1 --port 12222 --key ~/.ssh/studio_testhost \
  --host-key-alias studio --known-hosts ~/.ssh/testhost_known_hosts ping
```

`--host-key-alias` identifies Studio independently of the local forward port.
Provision the verified Studio host public key under that alias in the selected
known_hosts file before connecting. Every SSH connection uses
`StrictHostKeyChecking=yes`, `BatchMode=yes`, and disables PTY allocation. Unknown
or changed host keys fail rather than being automatically accepted. There is no
interactive first-connection enrollment in this client.

Registry SSH targets accept optional `ssh_port` (integer 1..65535),
`ssh_host_key_alias`, and `ssh_known_hosts` (a file path). Corresponding explicit
CLI options override these target settings. Omitted settings retain the system
SSH port, host identity, and known_hosts configuration. SSH control connections
are separated by destination, key, port, host-key alias, and known_hosts path so a
connection authenticated under one configuration is not reused under another.
Each invocation resolves the effective SSH configuration with `ssh -G` before
choosing its control socket, so changing an alias's HostName, User, or Port also
selects a new connection while keeping the socket filename short.
A broker authenticated at Ubuntu may also use these options to verify Ubuntu;
its downstream Studio credentials and host-key verification remain on Ubuntu.
