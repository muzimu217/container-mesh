#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 muzimu217
#
# build-demo.sh — assemble the demo tarball.
#
# Produces dist/cmesh-demo.tar.gz containing statically linked Linux binaries
# for amd64 and arm64, the start/stop scripts, and the guide. The point is that
# the recipient extracts it on a server and runs ./start.sh: no Go toolchain, no
# package manager, no config editing.
#
#   ./packaging/build-demo.sh            # uses the current git describe
#   VERSION=v1.0.0 ./packaging/build-demo.sh

set -eu

cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}
OUT=dist/cmesh-demo

echo "building container-mesh demo package, version $VERSION"

rm -rf "$OUT"
mkdir -p "$OUT"

# -trimpath keeps the build reproducible and avoids leaking the build host's
# directory layout into the binary.
# -s -w drops the symbol table and DWARF: the package is meant to be scp'd to a
# server, and 9 MB instead of 14 MB matters on a slow link.
# Every target is CGO-free, so these are static binaries that run on any
# distribution without matching glibc versions. The client side is built for all
# three desktop platforms because a demo usually has a laptop on one end and a
# server on the other: shipping only Linux means the recipient has to install Go
# to talk to their own server.
build() {
    os=$1
    arch=$2
    suffix=""
    [ "$os" = windows ] && suffix=".exe"
    echo "  compiling $os/$arch"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
        go build -trimpath -ldflags "-s -w -X github.com/muzimu217/container-mesh/internal/cli.Version=$VERSION" \
        -o "$OUT/cmesh-$os-$arch$suffix" ./cmd/cm
}

for arch in amd64 arm64; do
    build linux "$arch"
done
# Desktop clients. arm64 macOS is Apple Silicon; amd64 Windows covers the vast
# majority of Windows machines.
build darwin arm64
build darwin amd64
build windows amd64

cp packaging/start.sh packaging/stop.sh packaging/README.md "$OUT/"
chmod +x "$OUT/start.sh" "$OUT/stop.sh"
chmod +x "$OUT"/cmesh-* 2>/dev/null || true

# COPYFILE_DISABLE stops macOS tar from writing extended attributes, which Linux
# tar then warns about on extraction ("Ignoring unknown extended header keyword
# LIBARCHIVE.xattr..."). Harmless, but it looks broken to a first-time user.
( cd dist && COPYFILE_DISABLE=1 tar --no-xattrs -czf cmesh-demo.tar.gz cmesh-demo 2>/dev/null \
    || COPYFILE_DISABLE=1 tar -czf cmesh-demo.tar.gz cmesh-demo )

echo
echo "wrote dist/cmesh-demo.tar.gz ($(du -h dist/cmesh-demo.tar.gz | cut -f1))"
echo
echo "contents:"
tar tzf dist/cmesh-demo.tar.gz | sed 's/^/  /'
echo
echo "to test on a server:"
echo "  scp dist/cmesh-demo.tar.gz USER@HOST:/tmp/"
echo "  ssh USER@HOST 'cd /tmp && tar xzf cmesh-demo.tar.gz && cd cmesh-demo && ./start.sh -d'"
