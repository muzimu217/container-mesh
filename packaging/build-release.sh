#!/usr/bin/env bash
# Build the release assets for a given version.
#
# The binaries are CGO-free and statically linked, so they run on any
# distribution without matching a glibc version. The version is stamped
# in, so `cmesh version` and the @info verb report the real build — the
# round-62 finding was that the published binaries reported "dev".
#
# Usage: build-release.sh <version> <outdir>
set -eu

cd "$(dirname "$0")/.."

VERSION=${1:?usage: build-release.sh <version> <outdir>}
OUT=${2:?usage: build-release.sh <version> <outdir>}
BIN="$OUT/bin"

# The version is interpolated into the ldflags, so it is validated before it
# is used: a version is a tag name, nothing else.
case "$VERSION" in
    v[0-9]*)
        # The rest must be tag-ish characters only.
        if printf '%s' "${VERSION#v}" | grep -qvE '^[0-9A-Za-z._+-]+$'; then
            echo "refusing to build: the version must look like a tag name (v1.2.3)" >&2
            exit 2
        fi
        ;;
    *)
        echo "refusing to build: the version must start with 'v' (v1.2.3)" >&2
        exit 2
        ;;
esac

rm -rf "$OUT"
mkdir -p "$BIN"

build() {
    os=$1
    arch=$2
    suffix=""
    [ "$os" = windows ] && suffix=".exe"
    echo "  compiling $os/$arch"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
        go build -trimpath -ldflags "-s -w -X github.com/muzimu217/container-mesh/internal/cli.Version=$VERSION" \
        -o "$BIN/cmesh-$VERSION-$os-$arch$suffix" ./cmd/cm
}

for arch in amd64 arm64; do
    build linux "$arch"
done
build darwin arm64
build darwin amd64
build windows amd64
build windows arm64

echo
echo "assembling the release"
go run ./tools/mkrelease \
    --in "$BIN" \
    --out "$OUT/release" \
    --version "$VERSION" \
    --installer packaging/install.sh \
    --license LICENSE

echo
echo "release assets in $OUT/release:"
ls -la "$OUT/release"
