#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 muzimu217
#
# Verified cmesh installer for POSIX shells. Installation is versioned and
# separate from runtime state:
#   <install>/versions/<version>/cmesh
#   <install>/current -> versions/<version>
#   <install>/cmesh -> current/cmesh
#
# Runtime identities, certificates, and member lists stay in CMESH_STATE (or
# the normal ~/.cmesh default). This script never reads or writes credentials.

set -eu

BASE_URL=${CMESH_BASE_URL:-https://github.com/muzimu217/container-mesh/releases}
VERSION=${CMESH_VERSION:-latest}
INSTALL_DIR=${CMESH_INSTALL:-.}
STATE_DIR=${CMESH_STATE:-${HOME:-}/.cmesh}

say() { printf '%s\n' "$*"; }
die() { printf 'cmesh installer: %s\n' "$*" >&2; exit 1; }

usage() {
    cat >&2 <<'EOF'
usage: install.sh
       install.sh --uninstall [--purge]

CMESH_INSTALL selects the managed install root. CMESH_STATE selects the
runtime state directory used by --purge; it is never touched by a normal
install or uninstall.
EOF
    exit 2
}

PURGE=0
if [ "${1:-}" = "--uninstall" ]; then
    shift
    [ "${1:-}" = "--purge" ] && { PURGE=1; shift; }
    [ "$#" -eq 0 ] || usage
    marker="$INSTALL_DIR/.cmesh-install-root"
    [ -f "$marker" ] || die "$INSTALL_DIR is not a cmesh install root"
    case "$INSTALL_DIR" in
        ""|/) die "refusing to uninstall from an empty path or filesystem root" ;;
    esac
    if [ "$PURGE" -eq 1 ]; then
        rm -rf -- "$INSTALL_DIR"
        if [ -n "$STATE_DIR" ] && [ "$STATE_DIR" != "$INSTALL_DIR" ]; then
            rm -rf -- "$STATE_DIR"
        fi
        say "purged install root and runtime state"
    else
        rm -rf -- "$INSTALL_DIR/cmesh" "$INSTALL_DIR/current" "$INSTALL_DIR/versions" "$marker"
        say "uninstalled binaries; runtime state was retained at $STATE_DIR"
    fi
    exit 0
fi
[ "$#" -eq 0 ] || usage

case "$BASE_URL" in
    https://*) ;;
    *) die "CMESH_BASE_URL must be https://" ;;
esac
case "$BASE_URL" in
    *[!A-Za-z0-9._~:/-]*) die "unsafe characters in CMESH_BASE_URL" ;;
esac
case "$VERSION" in
    ''|*[!A-Za-z0-9._+-]*) die "unsafe characters in CMESH_VERSION" ;;
esac
case "$INSTALL_DIR" in
    ''|*[!A-Za-z0-9._/-]*) die "unsafe characters in CMESH_INSTALL" ;;
esac

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
    linux) os=linux ;;
    darwin) os=darwin ;;
    freebsd|openbsd|netbsd) os=freebsd ;;
    mingw*|msys*|cygwin*) os=windows ;;
    *) die "unsupported operating system: $os" ;;
esac
arch=$(uname -m)
case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported CPU architecture: $arch" ;;
esac

suffix=""
[ "$os" = windows ] && suffix=.exe
if [ "$VERSION" = latest ]; then
    asset_base="cmesh-latest-${os}-${arch}${suffix}"
    sums_url="$BASE_URL/latest/download/SHA256SUMS"
    binary_url="$BASE_URL/latest/download/$asset_base"
else
    asset_base="cmesh-${VERSION}-${os}-${arch}${suffix}"
    sums_url="$BASE_URL/download/$VERSION/SHA256SUMS"
    binary_url="$BASE_URL/download/$VERSION/$asset_base"
fi
say "cmesh installer"
say "  platform : ${os}/${arch}"
say "  asset    : $asset_base"
say "  target   : $INSTALL_DIR"

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/cmesh-install.XXXXXX")
trap 'rm -rf "$tmpdir"' EXIT INT TERM
fetch() {
    url=$1
    out=$2
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$out"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$url" -O "$out"
    else
        die "neither curl nor wget is available; install one and retry"
    fi
}

say "  downloading checksums..."
if ! fetch "$sums_url" "$tmpdir/SHA256SUMS"; then
    # GitHub's /releases/latest points at the newest NON-prerelease. While
    # the only release is a prerelease — which is where this project is,
    # v1.0.0-rc1 — that path is a 404, and the default install fails
    # outright: a new user following the README gets nothing at all.
    #
    # So the "latest" case falls back to the release API, which lists
    # prereleases too, and installs the newest release it names.
    if [ "$VERSION" != latest ]; then
        die "cannot fetch the release checksums"
    fi
    say "  no published release at the latest URL; asking the release API"
    api_url="${CMESH_RELEASE_API:-https://api.github.com/repos/muzimu217/container-mesh/releases}"
    if ! fetch "$api_url" "$tmpdir/releases.json"; then
        die "cannot fetch the release checksums, and the release API is unreachable"
    fi
    # The newest release that is neither a draft nor a prerelease, and if
    # there is none, the newest prerelease. Both are "a release"; the
    # order is the order GitHub's own UI uses.
    resolved=$(awk '
        /"draft":/ { d = ($0 ~ /"draft": *true/) ? 1 : 0 }
        /"prerelease":/ { p = ($0 ~ /"prerelease": *true/) ? 1 : 0 }
        /"tag_name":/ {
            if (match($0, /"tag_name": *"[^\"]+"/)) {
                v = substr($0, RSTART, RLENGTH)
                sub(/^"tag_name": *"/, "", v); sub(/"$/, "", v)
                if (d == 0 && p == 0) { print v; exit }
                if (best == "") best = v
            }
        }
        END { if (best != "") print best }
    ' "$tmpdir/releases.json" | head -1)
    [ -n "$resolved" ] || die "the release API named no installable release"
    say "  using release $resolved"
    VERSION=$resolved
    asset_base="cmesh-${VERSION}-${os}-${arch}${suffix}"
    sums_url="$BASE_URL/download/$VERSION/SHA256SUMS"
    binary_url="$BASE_URL/download/$VERSION/$asset_base"
    fetch "$sums_url" "$tmpdir/SHA256SUMS" || die "cannot fetch the checksums for $VERSION"
fi
say "  downloading binary..."
fetch "$binary_url" "$tmpdir/$asset_base" || die "cannot fetch the release binary"

say "  verifying checksum..."
if command -v sha256sum >/dev/null 2>&1; then
    checker=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    checker="shasum -a 256"
else
    die "no sha256sum or shasum available; refusing to install an unverified binary"
fi
expected=$(awk -v name="$asset_base" '$2==name || $2=="*" name {print $1; exit}' "$tmpdir/SHA256SUMS")
case "$expected" in
    ''|*[!0-9A-Fa-f]*) die "the release does not list a valid checksum for $asset_base" ;;
esac
[ "${#expected}" -eq 64 ] || die "the release does not list a valid checksum for $asset_base"
actual=$($checker "$tmpdir/$asset_base" | awk '{print $1}')
[ "$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')" ] || die "checksum mismatch for $asset_base"

mkdir -p "$INSTALL_DIR/versions" || die "cannot create install directory $INSTALL_DIR"
stage=$(mktemp -d "$INSTALL_DIR/versions/.cmesh-install.XXXXXX")
old=""
cleanup_stage() { rm -rf "$stage"; [ -z "$old" ] || rm -rf "$old"; }
trap 'cleanup_stage' EXIT INT TERM
replace_link() {
    link=$1
    target=$2
    tmp=$(mktemp "$INSTALL_DIR/.cmesh-link.XXXXXX")
    rm -f "$tmp"
    ln -s "$target" "$tmp"
    if mv -f -T "$tmp" "$link" 2>/dev/null; then
        return 0
    fi
    # BSD mv has no -T and follows a directory symlink. Remove only the
    # managed link after the new link is ready, then rename it into place.
    rm -f "$link"
    mv -f "$tmp" "$link"
}
cp "$tmpdir/$asset_base" "$stage/cmesh"
chmod 0755 "$stage/cmesh"
# The staging directory came from mktemp -d, which creates it 0700 so
# nothing else can read the half-written install. That mode travels with
# the directory when it is renamed into place, so the committed version
# directory would be 0700 — and only its owner could enter it. The
# systemd unit runs the node as an unprivileged "cmesh" user and executes
# the binary out of the managed install root, so a 0700 version
# directory means an officially installed binary cannot be run by the
# service that is supposed to run it.
#
# The binary itself is already 0755; the directory around it is what has
# to stay traversable.
chmod 0755 "$stage"
dest="$INSTALL_DIR/versions/$VERSION"
if [ -e "$dest" ] || [ -L "$dest" ]; then
    old=$(mktemp -d "$INSTALL_DIR/versions/.cmesh-old.XXXXXX")
    rmdir "$old"
    mv "$dest" "$old" || die "cannot replace existing version $VERSION"
fi
mv "$stage" "$dest" || die "cannot commit version $VERSION"
stage=""
replace_link "$INSTALL_DIR/current" "versions/$VERSION"
replace_link "$INSTALL_DIR/cmesh" current/cmesh
printf '%s\n' 'container-mesh managed install' > "$INSTALL_DIR/.cmesh-install-root"
[ -z "$old" ] || rm -rf "$old"
old=""
say "  installed: $INSTALL_DIR/current/cmesh"
say "  runtime state retained separately: $STATE_DIR"
