# Remote Studio tests

Both paths use the existing testhost CLI and Studio fixed-command entry. Nothing
needs an HTTP listener or a new CA. SSH authenticates the caller and verifies the
server host key on each SSH hop. Provision known host keys through an independently
verified source before connecting; unknown or changed keys fail closed.

## Direct SSH through a forwarded port

The remote computer holds its own Studio SSH private key. A wgtund TCP forward
can listen on `127.0.0.1:12222` and route to `192.168.147.196:22`, directly, through
a WireGuard tunnel, or via an Ubuntu host's SSH connection. wgtund only transports
the Studio SSH connection; Studio authenticates the remote computer itself.

For example, merge this forward into the remote computer's existing wgtund
`hosts.json`, whose `hosts` already contains an authenticated `ubuntu` connection:

```json
{
  "forwards": [{
    "name": "studio-ssh",
    "listen": "127.0.0.1:12222",
    "routes": [{"via": "ubuntu", "addr": "192.168.147.196:22"}]
  }]
}
```

`studio-ssh` must not duplicate any host name. Ubuntu must be able to reach Studio's
SSH port. The client must already be able to reach the forwarding listener; a
loopback listener does not expose a port on the public Internet.

```sh
python testhost/client.py --host omlxwake@127.0.0.1 --port 12222 \
  --key ~/.ssh/studio-testhost --host-key-alias studio-testhost \
  --known-hosts ~/.ssh/testhost_known_hosts ping
```

The known_hosts file must contain the verified Studio host key under
`studio-testhost` (HostKeyAlias), not Ubuntu's host key. This keeps Studio's identity
stable when the forwarding address or port changes. Local wgtund listeners remain
loopback-only; route remote access through the existing authenticated tunnel.

## Ubuntu SSH test service

Here the remote computer holds an Ubuntu service credential only. Ubuntu keeps
the Studio credential and checks Studio's host key. Remote callers check Ubuntu's
host key. The two SSH authentications are independent; no agent forwarding or
Studio private-key download is needed.

Install this repository on Ubuntu at a stable path. Create a machine-local JSON
file readable by the service account; keep it and the private key outside git:

```json
{
  "host": "omlxwake@192.168.147.196",
  "key": "/home/testservice/.ssh/studio-testhost",
  "known_hosts": "/home/testservice/.ssh/studio_known_hosts"
}
```

Optional `port` and `host_key_alias` configure a forwarded upstream. Every upstream
uses strict host-key checking. The Studio public key must authorize its existing
fixed-command testhost entry; the broker does not install or change that entry.

Add the remote computer's Ubuntu service public key to the Ubuntu service account's
`authorized_keys` with a forced command (replace paths and the public key):

```text
restrict,command="/usr/bin/python3 /opt/base/testhost/broker.py --config /home/testservice/.config/testhost/broker.json" ssh-ed25519 AAAA... remote-test-client
```

Run the normal client against Ubuntu with the remote computer's own credential:

```sh
python testhost/client.py --host testservice@ubuntu.example \
  --key ~/.ssh/ubuntu-test-service --known-hosts ~/.ssh/ubuntu_known_hosts ping
```

Use the same connection options for `run`, `status`, `result`, `fetch`, `cancel`,
`inventory`, `clean`, `push-tool` and `media`. No extra command prefix is needed.
The broker parses an exact testhost action, quotes each argument, and forwards
stdin/stdout/stderr without buffering uploads or tar downloads. SSH and testhost
failures preserve their exit status; broker setup/command errors exit 3.

The service is for trusted developers with testhost privileges: authorized callers
can upload code, run declared tasks, clean shared test caches, and update the Studio
testhost entry with `push-tool`. It is not a sandbox for untrusted users or a
multi-tenant job isolation service. Machine-local upstream routing cannot be
changed by a request. Nothing here modifies real keys or opens a network listener
automatically.
