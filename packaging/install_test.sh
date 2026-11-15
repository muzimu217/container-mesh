#!/usr/bin/env sh
# Offline shell regression tests for packaging/install.sh.
set -eu
ROOT=$(cd -- "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

assert_link() {
    test -L "$1" || { printf 'missing symlink: %s\n' "$1" >&2; exit 1; }
    test "$(readlink "$1")" = "$2" || { printf 'wrong symlink: %s\n' "$1" >&2; exit 1; }
}

install_root="$TMP/install"
mkdir -p "$TMP/releases/latest/download" "$TMP/bin"
printf 'fake cmesh linux binary\n' > "$TMP/releases/latest/download/cmesh-latest-darwin-arm64"
checksum=$(shasum -a 256 "$TMP/releases/latest/download/cmesh-latest-darwin-arm64" | awk '{print $1}')
printf '%s  cmesh-latest-darwin-arm64\n' "$checksum" > "$TMP/releases/latest/download/SHA256SUMS"

# Keep the production installer HTTPS-only. This test-only curl maps a fixed
# HTTPS fixture host to the temporary local release tree without network I/O.
cat > "$TMP/bin/curl" <<'EOF'
#!/bin/sh
set -eu
url=""
out=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) out=$2; shift 2 ;;
        -*) shift ;;
        *) url=$1; shift ;;
    esac
done
case "$url" in
    https://fixture.invalid/releases/latest/download/*)
        src="$FIXTURE_ROOT/releases/latest/download/${url##*/}"
        ;;
    *) exit 1 ;;
esac
cp "$src" "$out"
EOF
chmod 0755 "$TMP/bin/curl"
export FIXTURE_ROOT="$TMP"

if CMESH_BASE_URL=http://fixture.invalid/releases CMESH_INSTALL="$TMP/http-install" \
    sh "$ROOT/packaging/install.sh" >"$TMP/http.out" 2>&1; then
    printf '%s\n' 'HTTP base URL was accepted' >&2
    exit 1
fi
grep -q 'CMESH_BASE_URL must be https://' "$TMP/http.out"

CMESH_BASE_URL=https://fixture.invalid/releases CMESH_INSTALL="$install_root" \
    PATH="$TMP/bin:$PATH" sh "$ROOT/packaging/install.sh"
assert_link "$install_root/current" versions/latest
assert_link "$install_root/cmesh" current/cmesh
cmp "$TMP/releases/latest/download/cmesh-latest-darwin-arm64" "$install_root/current/cmesh"
test -x "$install_root/current/cmesh"

mkdir -p "$TMP/state"
printf 'state must survive\n' > "$TMP/state/identity.enc"
CMESH_INSTALL="$install_root" CMESH_STATE="$TMP/state" sh "$ROOT/packaging/install.sh" --uninstall
test -f "$TMP/state/identity.enc"
test ! -e "$install_root/current"
test ! -e "$install_root/cmesh"

# Reinstall and verify --purge removes the explicitly selected state directory.
CMESH_BASE_URL=https://fixture.invalid/releases CMESH_INSTALL="$install_root" \
    PATH="$TMP/bin:$PATH" sh "$ROOT/packaging/install.sh"
CMESH_INSTALL="$install_root" CMESH_STATE="$TMP/state" \
    sh "$ROOT/packaging/install.sh" --uninstall --purge
test ! -e "$TMP/state"
test ! -e "$install_root"

printf '%s\n' 'packaging install tests: ok'
