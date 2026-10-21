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
for arch in amd64 arm64; do
    echo "  compiling linux/$arch"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -trimpath -ldflags "-s -w -X github.com/muzimu217/container-mesh/internal/cli.Version=$VERSION" \
        -o "$OUT/cmesh-linux-$arch" ./cmd/cm
done

cp packaging/start.sh packaging/stop.sh packaging/README.md "$OUT/"
chmod +x "$OUT/start.sh" "$OUT/stop.sh" "$OUT/cmesh-linux-amd64" "$OUT/cmesh-linux-arm64"

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
