#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 muzimu217
#
# install.sh — fetch a verified cmesh binary for this machine (M4 (d)).
#
#   sh install.sh
#
# What it does:
#   1. detects the OS and CPU architecture;
#   2. downloads the matching static binary from the project's releases;
#   3. verifies its SHA-256 against the published SHA256SUMS;
#   4. writes it to the install directory.
#
# The binary is NOT installed before its checksum matches: a corrupted or
# tampered download fails with nothing written.
#
# The default install directory is the current directory — cmesh's home is
# containers and home directories, and writing into a system directory is a
# decision the operator makes, not the script. Point it elsewhere with
# CMESH_INSTALL, or copy the file yourself afterwards.
#
# Environment:
#   CMESH_INSTALL   where to write the binary (default: current directory)
#   CMESH_VERSION   release tag to fetch (default: latest)
#   CMESH_BASE_URL  release base URL (default the project's GitHub releases)

set -eu

BASE_URL=${CMESH_BASE_URL:-https://github.com/muzimu217/container-mesh/releases}
VERSION=${CMESH_VERSION:-latest}
INSTALL_DIR=${CMESH_INSTALL:-.}

# The base URL decides where a binary is fetched from, so it is restricted to
# https:// and to characters that cannot be confused with shell syntax.
case "$BASE_URL" in
    https://*) ;;
    *) echo "cmesh installer: CMESH_BASE_URL must be https://" >&2; exit 2 ;;
esac
case "$BASE_URL" in
    *[!A-Za-z0-9._~:/-]*) echo "cmesh installer: unsafe characters in CMESH_BASE_URL" >&2; exit 2 ;;
esac
case "$VERSION" in
    *[!A-Za-z0-9._-]*) echo "cmesh installer: unsafe characters in CMESH_VERSION" >&2; exit 2 ;;
esac
case "$INSTALL_DIR" in
    *[!A-Za-z0-9._/-]*) echo "cmesh installer: unsafe characters in CMESH_INSTALL" >&2; exit 2 ;;
esac

say() { printf '%s\n' "$*"; }
die() { printf 'cmesh installer: %s\n' "$*" >&2; exit 1; }

# --- platform detection ---------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
    linux) os=linux ;;
    darwin) os=darwin ;;
    freebsd | openbsd | netbsd) os=freebsd ;;
    mingw* | msys* | cygwin*) os=windows ;;
    *) die "unsupported operating system: $os" ;;
esac

arch=$(uname -m)
case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported CPU architecture: $arch" ;;
esac

suffix=""
[ "$os" = windows ] && suffix=".exe"

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

# --- download -------------------------------------------------------------
tmpdir=$(mktemp -d)
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
fetch "$sums_url" "$tmpdir/SHA256SUMS" || die "cannot fetch the release checksums"

say "  downloading binary..."
fetch "$binary_url" "$tmpdir/$asset_base" || die "cannot fetch the release binary"

# --- verify ---------------------------------------------------------------
say "  verifying checksum..."
if command -v sha256sum >/dev/null 2>&1; then
    checker="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
    checker="shasum -a 256"
else
    die "no sha256sum or shasum available; refusing to install an unverified binary"
fi

expected=$(grep " $asset_base\$" "$tmpdir/SHA256SUMS" | cut -d' ' -f1 | head -n 1)
[ -n "$expected" ] || die "the release does not list $asset_base"

actual=$($checker "$tmpdir/$asset_base" | cut -d' ' -f1)
[ "$expected" = "$actual" ] || die "checksum mismatch for $asset_base"

# --- write ----------------------------------------------------------------
if [ ! -d "$INSTALL_DIR" ]; then
    die "install directory does not exist: $INSTALL_DIR"
fi
if [ ! -w "$INSTALL_DIR" ]; then
    die "install directory is not writable: $INSTALL_DIR (set CMESH_INSTALL to a writable path)"
fi

target="$INSTALL_DIR/cmesh${suffix}"
cp "$tmpdir/$asset_base" "$target"
chmod 0755 "$target"

say "  installed: $target"
"$target" version || true
say ""
say "next steps:"
say "  cmesh init                       # create this device's identity"
say "  cmesh account create             # create the team account (anchor machine)"
say "  cmesh account device-add         # sign this device in"
say "  cmesh serve-discovery --panel    # run the anchor, then open the panel"
