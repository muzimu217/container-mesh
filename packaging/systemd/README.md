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
operator's approved secret/configuration process. The systemd behaviours the
units rely on were measured on the real Linux host (OpenCloudOS 9.4, systemd
255, 2026-11-20): `${VAR}` unset expands to exactly one empty argument, `$VAR`
unset expands to zero arguments, `$VAR` set splits on whitespace, and a
missing `LoadCredential=` file fails the unit with status 243/CREDENTIALS.
What is still NOT measured on a real boot: this member unit end to end (the
anchor unit has been accepted on the real host; the member unit has not).
The stop path is aligned with the relay's drain behaviour: `KillSignal=SIGTERM`
with `TimeoutStopSec=15s` lets the listener close and the pairing rooms unwind
before systemd escalates.

## Secrets on the command line

Two secrets reach the CLI through the unit, and neither should be readable
from `/proc/<pid>/cmdline` by another local user:

| Secret | What it grants | Source |
|---|---|---|
| panel token | the whole roster, read-only | `LoadCredential=panel-token:…` + `--panel-token-file` |
| team credential | **authority to join** — it can mint a device certificate under the account | `LoadCredential=cred:…` + `--cred-file` |

`LoadCredential=` hands the service a private, root-owned copy (mode 0600)
readable by the service's own process and by nobody else — so the secret is
in neither the process list nor the environment. `EnvironmentFile=` is *not*
equivalent: those variables do land in the service's `environ`, where every
process running as the same user can read them.

The credential's sources are ordered, and the order is a security
decision, not a convenience one:

| Order | Source | Where the secret is visible |
|---|---|---|
| 1 (strongest) | `--cred-file <file>` | only the processes that can read that file — `LoadCredential=` gives the service a private copy |
| 2 | `--cred <string>` | every local user, through `/proc/<pid>/cmdline` |
| 3 (weakest) | `CMESH_CRED=<string>` | every process running as the same user, through `/proc/<pid>/environ` |

The file is strongest because it keeps the credential out of both the
process list and the environment. The flag beats the ambient variable
because a flag on the command line is a deliberate choice by the operator
who typed it, while an environment variable may have been exported for
some other purpose entirely.

Two rules that come with the order:

- A credential file that cannot be read — or that is empty — is an **error**,
  never a fall-through. Falling back would hide the mistake and could join
  the network with a *different* credential than the operator asked for.
- `CMESH_CRED` is consulted only on the credential path: no invite
  argument, and neither `--cred` nor `--cred-file` given. An ambient
  `CMESH_CRED` therefore never breaks `cmesh join <invite>`.

The `cmesh-member.service` example in this directory shows the member side:

```sh
sudo install -d -m 0750 -o root -g cmesh /etc/cmesh   # both units need this directory
sudo install -m 0600 -o root -g cmesh /path/to/credential /etc/cmesh/cred
sudo cp packaging/systemd/cmesh-member.service /etc/systemd/system/
sudo cp packaging/systemd/cmesh-member.env.example /etc/cmesh/member.env
sudo chmod 0640 /etc/cmesh/member.env
sudo systemctl daemon-reload
sudo systemctl enable --now cmesh-member.service
```

The `cmesh` user and group are a prerequisite of BOTH units and nothing in
this repository creates them (`cmesh install` manages binaries only). Create
them once per machine:

```sh
sudo groupadd --system cmesh
sudo useradd --system --gid cmesh --home-dir /var/lib/cmesh --no-create-home cmesh
```

The member unit refuses to start when `CMESH_RELAY_TRUNK` or
`CMESH_RELAY_TRUNK_ID` is unset (an `ExecStartPre` guard): a member without
a relay trunk is pure-outbound and can never RECEIVE a relayed session,
which is a silent failure the CLI cannot detect for you.

Measured on the real machine: with the credential on the command line, `ps`
printed the whole credential string.

## The relay switch: SIGHUP

An anchor that runs with `--relay-trunk` carries relayed sessions for the
whole network. **An operator may need to stop that without taking the
node down** — the members' control plane, the panel and the pairing relay
must keep working.

**`SIGHUP` is the switch.** It toggles the trunk between carrying relays
and not carrying them, live:

```sh
sudo systemctl reload cmesh-discovery.service   # sends SIGHUP (ExecReload=)
# or, equivalently:
sudo kill -HUP "$(systemctl show -p MainPID --value cmesh-discovery.service)"
```

The node does not restart. The trunk simply stops accepting new relays.
**Turning it OFF also tears down the relays it is currently carrying** —
a trunk that is "off" but still forwarding other members' traffic is
worse than one that is off.

**The unit declares `ExecReload=/bin/kill -HUP $MAINPID`** — without it
`systemctl reload` would do nothing, which is the same shape as a
capability nobody calls: the operator sends the signal, sees no change,
and concludes the switch is broken.

The result is printed on stderr (`relay trunk: ON` / `relay trunk: OFF`),
because an operator who sends a signal and sees nothing must be able to
tell "it did not arrive" from "it arrived and did nothing".

## The rollback floor

The shipped units use flags that older builds do not have: the anchor
uses `--panel-token-file`, the member uses `--cred-file`. Rolling
`current` back past those builds does not fail loudly — the symlink
rotates happily and then BOTH units die in a restart loop with
`flag provided but not defined` (status=2/INVALIDARGUMENT).

So the rollback floor is the commit that introduced the file-based
secrets (`f428483`, "the credential stops being a command-line
secret"). Below that, roll FORWARD instead. The installer does not
check this yet — it is a known gap, and the honest statement is that the
floor is a rule an operator has to know, not a rule the tool enforces.

The other direction is safe: rolling FORWARD keeps the state directory
byte-for-byte (measured — the device identity, the account key and the
certificates all survive an upgrade unchanged), so an upgrade is not a
migration.

## The start-limit window

Both units set `RestartSec=5s` and, explicitly, `StartLimitIntervalSec=60`
with `StartLimitBurst=5`.

**★ The two `StartLimit*` directives live in the `[Unit]` section, not in
`[Service]`.** They are `[Unit]` directives, and systemd has **no
`Service.StartLimitIntervalSec` alias at all** — so in `[Service]` they
are silently ignored:

```
$ systemd-analyze verify cmesh-discovery.service
cmesh-discovery.service:79: Unknown key name 'StartLimitIntervalSec'
in section 'Service', ignoring.

$ systemctl show cmesh-discovery.service -p StartLimitIntervalUSec
StartLimitIntervalUSec=10s     # the DEFAULT — not the 60s we wrote
```

This was the state of the units until the round-98 audit: the arithmetic
in the comment was right, the **section** was wrong, and the fix was
therefore **inert**. Measured on real systemd 255 (OpenCloudOS 9.4) with
`ExecStart=/bin/false`, `Restart=on-failure`, `RestartSec=5s`:

| placement | outcome over 95 s |
|---|---|
| `[Service]` (the old, broken one) | **NRestarts = 18, still looping** |
| `[Unit]` (the current one) | **NRestarts = 5, then `ActiveState=failed`** |

**Do not change `RestartSec` on its own.** The two settings are one
decision, and the arithmetic binds them:

- systemd's defaults are a burst of **5 starts in a 10-second window**;
- with `RestartSec=5s` the starts land at 0 s, 5 s, 10 s, 15 s … so **any
  10-second window holds at most 3 starts** — the burst of 5 is
  **unreachable**;
- a burst is reachable only if the window is longer than
  `RestartSec × (Burst − 1)`, which here is `5 × 4 = 20 s`.

So without the explicit `StartLimitIntervalSec=60`, a **configuration
error** — an empty passphrase, an unreadable credential, a read-only
state directory — **restarts forever**, filling the journal instead of
stopping where an operator can see it. (The member's `ExecCondition`
already refuses a missing trunk configuration *without* restarting; every
other failure path still exits non-zero, and this is what stops those.)

The window being 60 s costs nothing in production: the property that
matters there is "kill −9 and it comes back in 5 s", and that is
`RestartSec`, which is unchanged.

**The placement has been verified BY BEHAVIOUR on a real systemd 255**, and
so has the `ExecCondition` vs `ExecStartPre` distinction the member unit
depends on: an `ExecCondition` that exits 1 gives `NRestarts = 0` (the
unit does not loop), while an `ExecStartPre` that exits 1 gives
`NRestarts = 5` and then fails. **So the member's use of `ExecCondition`
for a pure precondition is right, and no `ExecStartPre` remains in either
unit.**
