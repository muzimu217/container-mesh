#!/usr/bin/env bash
# Install the coordination hub on a host.
#
# ★ WHY THIS EXISTS. (Round 102.) The whole coordination environment —
#   the hub binary, its systemd unit, its nginx location, the ACME
#   certificate and its renewal timer — lived ONLY on one machine. When
#   that machine was reinstalled, the environment was gone, and it took
#   a full round to rebuild it, including a certificate, a token and a
#   public path.
#
#   The files are in the repo now (deploy/hub/systemd/, deploy/hub/nginx/).
#   This script puts them back. A reinstall should be a command, not an
#   investigation.
#
# ★ IT DOES NOT GENERATE A NEW TOKEN. The token is the ONE credential
#   every agent already holds in its own environment. Rotating it means
#   every agent has to be reconfigured, and the agents cannot be told
#   (their channel is this one). So: pass the existing token in, or set
#   it on the host afterwards. See README.md.
#
# Usage:
#   HUB_TOKEN=<64 hex> ./deploy/hub/install.sh [root@host]
#
# It does NOT:
#   * touch the anchor or anything of the product's
#   * create an nginx server block on a host that already serves
#     new.blackevil.cn (see deploy/hub/nginx/cmesh-hub.conf — T-4.6)
#   * write anything outside the paths it names
set -euo pipefail

ANCHOR="${1:-root@175.24.139.148}"
HERE="$(cd "$(dirname "$0")" && pwd)"
HUB_ADDR="${HUB_ADDR:-127.0.0.1:8787}"
HUB_DIR="${HUB_DIR:-/var/lib/cmesh-hub}"
HUB_BIN="${HUB_BIN:-/usr/local/bin/cmesh-hub}"
CERT_NAME="${CERT_NAME:-new.blackevil.cn}"

say() { printf '  %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need ssh
need scp
need go

if [ -z "${HUB_TOKEN:-}" ]; then
  echo "HUB_TOKEN is not set." >&2
  echo "★ Do NOT generate a new one here — the agents already hold the" >&2
  echo "  current one. Read it from the host you are replacing, or from" >&2
  echo "  the coordinator's local .team/hub-token." >&2
  exit 1
fi

echo "=== building the hub for linux/amd64 ==="
# Cross-compile: the hub runs on the server, not on the laptop that
# built it. (The repo used to carry a darwin/arm64 binary, which could
# not run on the server at all.)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/cmesh-hub.install "$HERE"
say "built /tmp/cmesh-hub.install"

echo "=== installing on $ANCHOR ==="
scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  /tmp/cmesh-hub.install "$ANCHOR:/tmp/cmesh-hub.install"
scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "$HERE/systemd/cmesh-hub.service" "$ANCHOR:/tmp/cmesh-hub.service"

ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$ANCHOR" bash -s <<REMOTE
set -e
# The dedicated low-privilege account.
if ! id cmesh-hub >/dev/null 2>&1; then
  useradd --system --home-dir $HUB_DIR --shell /usr/sbin/nologin cmesh-hub
  say "created the cmesh-hub user"
fi
install -d -o cmesh-hub -g cmesh-hub -m 0700 "$HUB_DIR"
install -m 0755 -o root -g root /tmp/cmesh-hub.install $HUB_BIN
rm -f /tmp/cmesh-hub.install

# The token. ★ 0600, root-only.
umask 077
if [ -f /etc/default/cmesh-hub ]; then
  cp -a /etc/default/cmesh-hub /etc/default/cmesh-hub.bak.\$(date +%s)
fi
printf 'HUB_TOKEN=%s\n' '$HUB_TOKEN' > /etc/default/cmesh-hub
chmod 600 /etc/default/cmesh-hub

install -m 0644 /tmp/cmesh-hub.service /etc/systemd/system/cmesh-hub.service
rm -f /tmp/cmesh-hub.service
systemctl daemon-reload
systemctl enable --now cmesh-hub
sleep 1
systemctl is-active cmesh-hub
REMOTE

rm -f /tmp/cmesh-hub.install

echo
echo "=== verify from the host itself ==="
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$ANCHOR" \
  "curl -sS -m 5 -o /dev/null -w '  /healthz → http=%{http_code}\n' http://$HUB_ADDR/healthz"

cat <<EOF

=== next steps (they are NOT done by this script) ===

  1. nginx: copy the location into the EXISTING server block for
     $CERT_NAME. Do NOT drop in deploy/hub/nginx/cmesh-hub.conf on a
     host that already serves that name — it is a whole server block,
     and a second one takes the whole vhost (T-4.6).

  2. certificate:
       certbot certonly --webroot -w /var/lib/cmesh-acme/webroot \\
         -d $CERT_NAME --non-interactive --agree-tos --email <you>
     then install the renewal unit:
       install -m 0644 deploy/hub/systemd/cmesh-acme-renew.service /etc/systemd/system/
       install -m 0644 deploy/hub/systemd/cmesh-acme-renew.timer  /etc/systemd/system/
       systemctl daemon-reload && systemctl enable --now cmesh-acme-renew.timer

  3. check the PUBLIC path from outside — the loop's probe must pass:
       curl -sSf -m 8 -6 -H "X-Hub-Token: \$TOKEN" \\
         "https://$CERT_NAME/hub/reports?topic=issue8"
     (IPv4 may be filtered by the security group while IPv6 is not.)

  4. tell the coordinator which token you installed, so the local
     .team/hub-token matches it.
EOF
