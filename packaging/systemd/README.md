# container-mesh systemd deployment example

This directory contains an example unit for a Linux headless discovery node. It
assumes the managed installation root is `/opt/container-mesh`, so the stable
binary path is `/opt/container-mesh/current/cmesh`.

The unit uses an external, root-readable environment file. Keep passwords,
private keys, and tokens in a root-only secret manager or file; do not put real
values in this repository or in the unit. `cmesh install` only manages binaries
and never reads this state directory.

The environment file is **required**, not optional. The unit reads
`CMESH_STATE`, `CMESH_ADDR`, `CMESH_ANNOUNCE`, `CMESH_ACCOUNT_PUB` and
`CMESH_PASSPHRASE` from it. Two of those are load-bearing in ways that are
easy to miss:

- `CMESH_ACCOUNT_PUB` — without `--account-pub` the discovery node does not
  mount the enrollment endpoint at all, so a member cannot join with a team
  credential. The public key is not a secret; `cmesh account show` prints it.
- `CMESH_PASSPHRASE` — a service has no tty. An empty passphrase makes the
  CLI read EOF and exit, so the node never comes up.

```sh
sudo install -d -m 0755 /opt/container-mesh
sudo install -d -m 0750 -o cmesh -g cmesh /var/lib/cmesh
sudo install -d -m 0750 -o root -g cmesh /etc/cmesh
sudo cp packaging/systemd/cmesh-discovery.service /etc/systemd/system/
sudo cp packaging/systemd/cmesh-discovery.env.example /etc/cmesh/discovery.env
sudo chmod 0640 /etc/cmesh/discovery.env
sudo systemctl daemon-reload
sudo systemctl enable --now cmesh-discovery.service
```

`StateDirectory=cmesh` creates `/var/lib/cmesh` with the right owner and
mode, so a fresh machine needs no manual directory step.

Before starting, replace the example values in the environment file through the
operator's approved secret/configuration process. A real Linux systemd boot,
privilege drop, and restart cycle were not available in this macOS workspace;
those remain required acceptance checks. The stop path is aligned with the
relay's drain behaviour: `KillSignal=SIGTERM` with `TimeoutStopSec=15s` lets
the listener close and the pairing rooms unwind before systemd escalates.
