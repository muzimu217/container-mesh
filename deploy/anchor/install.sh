#!/usr/bin/env bash
# Install the container-mesh discovery anchor on a host.
#
# ★ WHY THIS EXISTS.
#   The anchor is the one node the whole network depends on: it holds the
#   account key, the roster and the panel token. When its host is
#   reinstalled, all of it is gone, and putting it back was a page of
#   notes plus a session of guessing.
#
#   This is the local half: it builds the binary for the target's
#   platform, ships the payload, runs deploy/anchor/host.sh on the
#   target, and then READS THE EVIDENCE BACK (the version the host runs
#   and the state of the unit). A rebuild that prints what it did and
#   leaves nothing to verify is a rebuild nobody trusts.
#
# ★ IT DOES NOT GENERATE THE IDENTITY.
#   The identity is the one thing that cannot be regenerated for free: a
#   new account key invalidates every certificate the network holds, so
#   every member has to join again. The payload only creates one when it
#   is missing, and it SAYS SO LOUDLY when it does.
#
# Usage:
#   deploy/anchor/install.sh root@host
#
# Optional environment variables:
#   CMESH_DISCOVERY_ENV_SOURCE   a local discovery.env to install on the
#                                target (the payload refuses to invent one)
#   CMESH_SKIP_IDENTITY=1        do not create the identity on the target
#   CMESH_VERSION                the version string to stamp into the
#                                binary (default: git describe)
#   CMESH_GOARCH                 default amd64
#   DRY_RUN=1                    print the commands instead of running them
#
# It does NOT:
#   * touch the hub, the member nodes, or anything else on the host
#   * print or transfer a panel token
#   * write anything outside the paths it names
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"

say() { printf '  %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }

TARGET="${1:-}"
if [ -z "$TARGET" ]; then
	echo "usage: deploy/anchor/install.sh root@host" >&2
	exit 2
fi

GOOS="${CMESH_GOOS:-linux}"
GOARCH="${CMESH_GOARCH:-amd64}"
VERSION="${CMESH_VERSION:-}"
if [ -z "$VERSION" ]; then
	VERSION="$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

UNIT_NAME="cmesh-serve-discovery.service"
STAGE="/tmp/cmesh-anchor-install"

need ssh
need scp
need go

# DRY_RUN prints every command instead of running it. It is how the
# operator checks what the script WOULD do before it touches a host.
DRY_RUN="${DRY_RUN:-0}"
run() {
	if [ "$DRY_RUN" = "1" ]; then
		printf '  DRY-RUN %s\n' "$*"
		return 0
	fi
	"$@"
}

echo "=== building the anchor binary for $GOOS/$GOARCH ==="
# Cross-compile: the anchor runs on the server, not on the laptop that
# built it. (The repo used to carry a darwin/arm64 binary, which could
# not run on the server at all.)
#
# The version MUST be stamped: a binary built without it reports "dev",
# and the @info verb would then tell an operator that their peer runs
# "dev" instead of the version they installed. Same form as CI.
BUILD_OUT="$(mktemp)"
trap 'rm -f "$BUILD_OUT"' EXIT
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build \
	-trimpath \
	-ldflags "-X github.com/muzimu217/container-mesh/internal/cli.Version=$VERSION" \
	-o "$BUILD_OUT" "$REPO/cmd/cm"
say "built for $GOOS/$GOARCH, version $VERSION"

echo "=== staging the payload ==="
run scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	"$BUILD_OUT" "$TARGET:$STAGE/cmesh.install"
run scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	"$HERE/host.sh" "$TARGET:$STAGE/host.sh"
run scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	"$HERE/systemd/$UNIT_NAME" "$TARGET:$STAGE/$UNIT_NAME"

ENV_SRC="${CMESH_DISCOVERY_ENV_SOURCE:-}"
if [ -n "$ENV_SRC" ]; then
	if [ ! -f "$ENV_SRC" ]; then
		echo "the environment file source $ENV_SRC does not exist" >&2
		exit 1
	fi
	run scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		"$ENV_SRC" "$TARGET:$STAGE/discovery.env"
	say "staged the environment file from $ENV_SRC"
else
	say "no environment file to stage: the one on the target will be kept"
fi

echo "=== running the payload on $TARGET ==="
REMOTE_ENV=(
	"CMESH_BIN_SRC=$STAGE/cmesh.install"
	"CMESH_UNIT_SRC=$STAGE/$UNIT_NAME"
)
if [ -n "$ENV_SRC" ]; then
	REMOTE_ENV+=("CMESH_DISCOVERY_ENV_SOURCE=$STAGE/discovery.env")
fi
if [ -n "${CMESH_SKIP_IDENTITY:-}" ]; then
	REMOTE_ENV+=("CMESH_SKIP_IDENTITY=$CMESH_SKIP_IDENTITY")
fi

REMOTE_CMD="$(printf '%q ' bash "$STAGE/host.sh" "${REMOTE_ENV[@]}")"
run ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$TARGET" \
	"mkdir -p $STAGE && $REMOTE_CMD"

# The staging directory is this script's own scratch space on the
# target; removing it is removing something this script created.
run ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$TARGET" \
	"rm -rf $STAGE"

echo
echo "=== reading the evidence back from the host ==="
run ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$TARGET" \
	"cmesh version; systemctl is-active $UNIT_NAME; systemctl status $UNIT_NAME --no-pager -n 20 || true"

cat <<EOF

=== next steps (they are NOT done by this script) ===

  1. if the payload says the identity was rebuilt, EVERY member has to
     join again: a new account key invalidates the certificates the old
     one signed.

  2. the environment file (/etc/cmesh/discovery.env) is the operator's:
     it carries CMESH_STATE, CMESH_ADDR, CMESH_ANNOUNCE and
     CMESH_PASSPHRASE, and the script refuses to invent it.

  3. the panel token is never printed. If the payload generated one, it
     is in /etc/cmesh/panel-token on the host (root:cmesh, 0640); read
     it there when the panel asks for it.

  4. the order: the coordination channel first (the hub, deploy/hub/),
     then the anchor, then the members rejoin. See README.md.
EOF
