# Target registry contract v1 (foundation only)

This module parses declarations; it does not install VMs, probe runtime architecture,
dispatch tasks, enforce host budgets or implement a queue. The client CLI is unchanged
until the coordinator wires this contract into an execution adapter.

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
