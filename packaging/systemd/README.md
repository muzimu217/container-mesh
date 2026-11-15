# container-mesh systemd deployment example

This directory contains an example unit for a Linux headless discovery node. It
assumes the managed installation root is `/opt/container-mesh`, so the stable
binary path is `/opt/container-mesh/current/cmesh`.

The unit uses an external, root-readable environment file. Keep passwords,
private keys, and tokens in a root-only secret manager or file; do not put real
values in this repository or in the unit. `cmesh install` only manages binaries
and never reads this state directory.

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

Before starting, replace the example values in the environment file through the
operator's approved secret/configuration process. A real Linux systemd boot,
privilege drop, and restart cycle were not available in this macOS workspace;
those remain required acceptance checks.
