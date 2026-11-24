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

# The fixture must serve the asset name install.sh will ask for on THIS
# platform: the installer resolves os/arch from uname, so a hardcoded
# darwin-arm64 fixture fails on every other platform (F-1: the offline
# regression rc=1 on linux/amd64). Resolve the same way the installer does.
fixture_os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$fixture_os" in
    linux | darwin) ;;
    freebsd | openbsd | netbsd) fixture_os=freebsd ;;
    mingw* | msys* | cygwin*) fixture_os=windows ;;
esac
fixture_arch=$(uname -m)
case "$fixture_arch" in
    x86_64 | amd64) fixture_arch=amd64 ;;
    aarch64 | arm64) fixture_arch=arm64 ;;
esac
fixture_asset="cmesh-latest-${fixture_os}-${fixture_arch}"
printf 'fake cmesh binary for %s/%s\n' "$fixture_os" "$fixture_arch" \
    > "$TMP/releases/latest/download/$fixture_asset"
checksum=$(shasum -a 256 "$TMP/releases/latest/download/$fixture_asset" | awk '{print $1}')
printf '%s  %s\n' "$checksum" "$fixture_asset" > "$TMP/releases/latest/download/SHA256SUMS"

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
    https://fixture.invalid/releases/download/*)
        src="$FIXTURE_ROOT/releases/download/${url#https://fixture.invalid/releases/download/}"
        ;;
    https://api.fixture.invalid/repos/*/releases*)
        # The release API: the installer falls back to it when
        # /releases/latest/download is 404.
        src="$FIXTURE_ROOT/api-releases.json"
        ;;
    *) exit 1 ;;
esac
# A missing file is a 404, not a transport failure — that is what GitHub
# answers for a release path that does not exist, and the installer
# distinguishes the two.
if [ ! -f "$src" ]; then
    printf 'curl: (22) The requested URL returned error: 404\n' >&2
    exit 22
fi
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
cmp "$TMP/releases/latest/download/$fixture_asset" "$install_root/current/cmesh"
test -x "$install_root/current/cmesh"

# The default VERSION is "latest", and GitHub's /releases/latest points at
# the newest NON-prerelease. While the only release is a prerelease (which
# is where this project is — v1.0.0-rc1), that path is a 404 and the
# default install fails outright: a new user following the README gets
# nothing.
#
# The installer must therefore fall back to the release API, which lists
# prereleases too, and install the newest release it finds.
#
# The fixture below serves the API response and the versioned asset path
# but NOT /releases/latest/download — exactly the shape that broke. The
# stub answers 404 for a missing file rather than a transport error.
mkdir -p "$TMP/releases/download/v1.0.0-rc1"
prerelease_asset="cmesh-v1.0.0-rc1-${fixture_os}-${fixture_arch}"
printf 'fake cmesh binary for %s/%s (prerelease)\n' "$fixture_os" "$fixture_arch" \
    > "$TMP/releases/download/v1.0.0-rc1/$prerelease_asset"
prerelease_sum=$(shasum -a 256 "$TMP/releases/download/v1.0.0-rc1/$prerelease_asset" | awk '{print $1}')
printf '%s  %s\n' "$prerelease_sum" "$prerelease_asset" \
    > "$TMP/releases/download/v1.0.0-rc1/SHA256SUMS"
printf '[{"tag_name":"v1.0.0-rc1","prerelease":true,"draft":false}]' \
    > "$TMP/api-releases.json"
# The "latest" tree is what GitHub 404s for a prerelease-only project.
mv "$TMP/releases/latest" "$TMP/releases/latest-standby"

prerelease_root="$TMP/prerelease-install"
CMESH_BASE_URL=https://fixture.invalid/releases CMESH_INSTALL="$prerelease_root" \
    CMESH_RELEASE_API=https://api.fixture.invalid/repos/muzimu217/container-mesh/releases \
    PATH="$TMP/bin:$PATH" sh "$ROOT/packaging/install.sh"
assert_link "$prerelease_root/current" versions/v1.0.0-rc1
cmp "$TMP/releases/download/v1.0.0-rc1/$prerelease_asset" "$prerelease_root/current/cmesh"
# The standby tree comes back for the later tests.
mv "$TMP/releases/latest-standby" "$TMP/releases/latest"

# The committed version directory must be traversable by someone who does
# not own it. The staging directory comes from mktemp -d, which creates it
# 0700, and that mode used to travel with it into the committed
# directory — so the systemd unit's unprivileged "cmesh" user could not
# enter it and could not execute an officially installed binary.
version_dir="$install_root/versions/latest"
mode=$(ls -ld "$version_dir" | cut -c1-10)
# "drwx------" means owner-only: nobody but the owner can enter it.
case "$mode" in
    d???------) printf 'the version directory mode is %s: an unprivileged service user cannot traverse into it\n' "$mode" >&2; exit 1 ;;
esac

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

# ---------------------------------------------------------------------------
# THE INSTALL ROOT MUST NOT BE THE FILESYSTEM ROOT.
#
# The uninstall branch refused "/" but the INSTALL branch did not — so
# `CMESH_INSTALL=/ sh install.sh` wrote /versions, /current, /cmesh and
# /.cmesh-install-root at the filesystem root, and reported only
# "installed: /current/cmesh".
#
# The rule is the general one: a path the tool refuses to REMOVE must
# also be a path it refuses to install INTO. Otherwise a stray
# CMESH_INSTALL=/ litters the root of the machine.
#
# The test cannot install into "/" (and must not try), so it asserts the
# refusal: the installer must exit non-zero AND say why.
if CMESH_INSTALL=/ CMESH_BASE_URL=https://fixture.invalid/releases \
    sh "$ROOT/packaging/install.sh" >"$TMP/rootinstall.out" 2>&1; then
    printf '%s\n' 'the installer accepted the filesystem root as its install root' >&2
    exit 1
fi
grep -q 'filesystem root' "$TMP/rootinstall.out" || {
    printf '%s\n' 'the installer refused the filesystem root without saying why' >&2
    cat "$TMP/rootinstall.out" >&2
    exit 1
}

# ---------------------------------------------------------------------------
# THE STATE DIRECTORY MUST NOT BECOME THE FILESYSTEM ROOT.
#
# The state directory used to default to "${HOME:-}/.cmesh", so with
# HOME unset it expanded to "/.cmesh" — and `--purge` then ran
# `rm -rf -- /.cmesh` with no confirmation.
#
# The default is now /var/lib/cmesh (where the systemd units really
# keep their state), so HOME no longer shapes it. But the SAFETY NET
# must not go with the shape it used to be reached by: the check is on
# the VALUE now, so a root-shaped state path warns whoever supplies it,
# from wherever it comes.
#
# The INSTALL must not fail over it (an install never touches the state
# directory), but the operator must hear about it NOW rather than at
# purge time. So the install WARNS.
env CMESH_BASE_URL=https://fixture.invalid/releases CMESH_INSTALL="$TMP/home-unset" \
    CMESH_STATE=/.cmesh \
    PATH="$TMP/bin:$PATH" sh "$ROOT/packaging/install.sh" >"$TMP/nohome.out" 2>&1 || true
grep -q 'state directory' "$TMP/nohome.out" || {
    printf '%s\n' 'the installer did not warn about a state directory at the filesystem root' >&2
    cat "$TMP/nohome.out" >&2
    exit 1
}

# ★ AND THE DEFAULT ITSELF MUST NEVER BE ROOT-SHAPED. The default is
#   what a purge uses when the operator names no state directory, so a
#   root-shaped default would be a root-shaped `rm -rf`.
if env CMESH_BASE_URL=https://fixture.invalid/releases CMESH_INSTALL="$TMP/home-unset" \
    PATH="$TMP/bin:$PATH" sh "$ROOT/packaging/install.sh" >"$TMP/default.out" 2>&1; then
    if grep -q 'state directory' "$TMP/default.out"; then
        printf '%s\n' 'the DEFAULT state directory is root-shaped' >&2
        cat "$TMP/default.out" >&2
        exit 1
    fi
fi

# And the PURGE must REFUSE outright — that is the branch that would
# do the damage.
if env -u HOME CMESH_INSTALL="$install_root" CMESH_STATE=/ \
    sh "$ROOT/packaging/install.sh" --uninstall --purge >"$TMP/nohome-purge.out" 2>&1; then
    printf '%s\n' 'the purge accepted a state directory at the filesystem root' >&2
    exit 1
fi
grep -q 'state directory' "$TMP/nohome-purge.out" || {
    printf '%s\n' 'the purge refused the state directory without saying why' >&2
    cat "$TMP/nohome-purge.out" >&2
    exit 1
}

# ---------------------------------------------------------------------------
# AN UNINSTALL MUST STOP THE SYSTEMD UNITS.
#
# The installer never touched systemd, so `--uninstall` left the units
# installed AND enabled with the process still running — and the next
# boot started a unit whose ExecStart pointed at a path that no longer
# existed: status=203/EXEC, then the restart loop. The uninstall
# IGNITES the restart loop.
#
# The test cannot stop a real systemd (and there is none on this
# machine), so it puts a STUB systemctl on the PATH and asserts the
# installer called it for the right units — and that it said which units
# it stopped, because an operator who is not told believes the machine
# is clean when it is not.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$FIXTURE_ROOT/systemctl.calls"
exit 0
EOF
chmod 0755 "$TMP/bin/systemctl"
: > "$TMP/systemctl.calls"

# Reinstall so there is an install root to uninstall from.
CMESH_BASE_URL=https://fixture.invalid/releases CMESH_INSTALL="$install_root" \
    PATH="$TMP/bin:$PATH" sh "$ROOT/packaging/install.sh" >/dev/null 2>&1

CMESH_INSTALL="$install_root" CMESH_STATE="$TMP/state" \
    PATH="$TMP/bin:$PATH" sh "$ROOT/packaging/install.sh" --uninstall > "$TMP/uninstall.out" 2>&1

grep -q 'stop cmesh-discovery.service' "$TMP/systemctl.calls" || {
    printf '%s\n' 'the uninstall did not stop the discovery unit' >&2
    cat "$TMP/systemctl.calls" >&2
    exit 1
}
grep -q 'disable cmesh-discovery.service' "$TMP/systemctl.calls" || {
    printf '%s\n' 'the uninstall did not disable the discovery unit' >&2
    cat "$TMP/systemctl.calls" >&2
    exit 1
}
grep -q 'daemon-reload' "$TMP/systemctl.calls" || {
    printf '%s\n' 'the uninstall did not reload the unit files' >&2
    cat "$TMP/systemctl.calls" >&2
    exit 1
}
# The operator must be told which units were stopped.
grep -q 'stopped and disabled' "$TMP/uninstall.out" || {
    printf '%s\n' 'the uninstall did not say which units it stopped' >&2
    cat "$TMP/uninstall.out" >&2
    exit 1
}

# ---------------------------------------------------------------------------
# ★ THE PATH GUARDS MUST RESOLVE, NOT COMPARE STRINGS. (Round 98.)
#
# The guards above compared the path STRING against "/". That let
# "//" through (because "${STATE_DIR#/}" still contains a "/"), and on
# busybox/Alpine `rm -rf -- //` wipes the whole filesystem. "/." and
# "/.." and "/tmp/.." got through the same way.
#
# The installer now NORMALISES the path before comparing it. The test
# asserts the refusal for every shape that resolves to the root.
#
# ★ NOTHING HERE CAN DELETE ANYTHING: every one of these assertions is
# a REFUSAL, and a refusal happens before any removal is attempted.
# The paths do not exist, and the install root is a temp directory.
#
# The install root is REINSTALLED before each case, because the refusal
# path checks for the marker file — and an earlier successful purge in
# this script removed the whole root.
for bad in "//" "/." "/.." "/tmp/.." "/../"; do
    CMESH_BASE_URL=https://fixture.invalid/releases CMESH_INSTALL="$install_root" \
        PATH="$TMP/bin:$PATH" sh "$ROOT/packaging/install.sh" >/dev/null 2>&1
    if CMESH_INSTALL="$install_root" CMESH_STATE="$bad" \
        sh "$ROOT/packaging/install.sh" --uninstall --purge >"$TMP/resolve-purge.out" 2>&1; then
        printf 'the purge accepted the state path %s: it resolves to the filesystem root\n' "$bad" >&2
        exit 1
    fi
    grep -q 'state directory' "$TMP/resolve-purge.out" || {
        printf 'the purge refused %s without saying why\n' "$bad" >&2
        cat "$TMP/resolve-purge.out" >&2
        exit 1
    }
    # The install root must be intact: the refusal came first.
    test -e "$install_root/.cmesh-install-root" || {
        printf 'the install root was damaged by a refused purge of %s\n' "$bad" >&2
        exit 1
    }
done

# The same for the INSTALL root: "/.." and "/." resolve to the root, and
# the install branch used to write versions/, current, cmesh and
# .cmesh-install-root AT THE FILESYSTEM ROOT.
for bad in "//" "/." "/.." "/tmp/.."; do
    if CMESH_INSTALL="$bad" CMESH_BASE_URL=https://fixture.invalid/releases \
        sh "$ROOT/packaging/install.sh" >"$TMP/resolve-install.out" 2>&1; then
        printf 'the installer accepted %s as its install root: it resolves to the filesystem root\n' "$bad" >&2
        exit 1
    fi
    grep -q 'filesystem root' "$TMP/resolve-install.out" || {
        printf 'the installer refused %s without saying why\n' "$bad" >&2
        cat "$TMP/resolve-install.out" >&2
        exit 1
    }
done

# ---------------------------------------------------------------------------
# ★ THE versions/ DIRECTORY MUST BE TRAVERSABLE.
#
# `mkdir -p` creates it with the process's umask, and the systemd units
# run the node as an unprivileged "cmesh" user. Under a umask of 077 the
# directory comes out 0700, so only the installing user can enter it and
# the unit that is supposed to execute $INSTALL_DIR/current/cmesh gets
# "Permission denied".
#
# The version directory's own mode was already checked above; this is the
# directory AROUND it — the same class of bug, one level up.
versions_dir_mode=$(ls -ld "$install_root/versions" | cut -c1-10)
case "$versions_dir_mode" in
    d???------)
        printf 'the versions directory mode is %s: an unprivileged service user cannot traverse into it\n' "$versions_dir_mode" >&2
        exit 1
        ;;
esac

printf '%s\n' 'packaging install tests: ok'
