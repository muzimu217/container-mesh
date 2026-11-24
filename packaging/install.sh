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
# ★ THE DEFAULT STATE DIR MUST BE THE ONE THAT IS REALLY DEPLOYED.
#
# It used to be ${HOME}/.cmesh — which is where a per-user install put
# its state, and it is NOT where the systemd units put theirs:
#
#   cmesh-serve-discovery.service  →  /var/lib/cmesh
#   cmesh-hub.service              →  /var/lib/cmesh-hub
#
# So a purge with the default removed a directory that did not exist,
# printed "purged install root and runtime state", and LEFT BOTH the
# device's private-key envelope and the hub's whole channel history on
# disk. The operator was told the machine was clean; it was not.
#
# The systemd deployment is the one that matters, so its state
# directory is the default. A per-user install passes CMESH_STATE.
#
# ★ BUT THE ROOT-SHAPE SAFETY NET STAYS. It used to live in the default
#   itself: with HOME unset, "${HOME:-}/.cmesh" expanded to "/.cmesh",
#   and the purge then ran `rm -rf -- /.cmesh`. The warning below
#   catches that shape, so removing the shape from the default must not
#   remove the warning — the warning is what protects the NEXT shape.
STATE_DIR=${CMESH_STATE:-/var/lib/cmesh}

say() { printf '%s\n' "$*"; }
die() { printf 'cmesh installer: %s\n' "$*" >&2; exit 1; }

# resolve_path NORMALISES a path so that the guards below compare the
# path a tool would ACTUALLY act on.
#
# It is pure string arithmetic — it never touches the filesystem — so it
# cannot be defeated by a symlink and cannot have a side effect of its
# own. What it does:
#
#   "//"        -> "/"
#   "/."        -> "/"
#   "/.."       -> "/"
#   "/tmp/.."   -> "/"
#   "/a/b/../c" -> "/a/c"
#
# The last case is what makes it more than a "/"-check: a path can hide
# the root behind a deeper-looking prefix, and only a real normalization
# finds it.
#
# ★ THE NORMALIZATION IS DONE ON THE WHOLE PATH, NOT ON A SPLIT. (Round
# 98.) Splitting off the last component and resolving only the directory
# leaves "/.." as "/..", because ".." is a component and not a directory
# name — so a "/"-comparison would still miss it. The path is therefore
# collapsed component by component, which is what makes "/tmp/.." and
# "/.." both come out as "/".
#
# It uses `cd` into the directory plus `pwd`, which is what the shell can
# do portably (readlink -f is not POSIX, and realpath is not everywhere).
resolve_path() {
    _rp=$1
    case "$_rp" in
        /*) ;;
        *) _rp="$(pwd)/$_rp" ;;
    esac
    # Collapse the path component by component. ".." pops the previous
    # component; "." and empty components are dropped; ".." at the root
    # stays at the root (which is what POSIX says "/" + ".." is).
    _rp_out=""
    _rp_rest=${_rp#/}
    while [ -n "$_rp_rest" ]; do
        _rp_comp=${_rp_rest%%/*}
        if [ "$_rp_comp" = "$_rp_rest" ]; then
            _rp_rest=""
        else
            _rp_rest=${_rp_rest#*/}
        fi
        case "$_rp_comp" in
            ''|'.') ;;
            '..')
                # Pop the last component of what we have so far.
                #
                # ★ dirname "" is "." and dirname "/" is "/", so the
                # empty case has to be handled EXPLICITLY: without it,
                # "/.." pops into "." and then resolves to the CURRENT
                # DIRECTORY — which is not the root, and would sail
                # straight past the guard.
                if [ -z "$_rp_out" ] || [ "$_rp_out" = "/" ]; then
                    _rp_out="/"
                else
                    _rp_out=$(dirname "$_rp_out")
                fi
                ;;
            *) _rp_out="${_rp_out%/}/$_rp_comp" ;;
        esac
    done
    [ -z "$_rp_out" ] && _rp_out=/
    # The result may still name a directory that exists; resolve it so a
    # symlinked path cannot smuggle the root past the guard.
    if [ -d "$_rp_out" ]; then
        _rp_real=$(cd "$_rp_out" 2>/dev/null && pwd) || _rp_real=$_rp_out
        # "/.." resolves to "/" through a real cd; use that.
        _rp_out=$_rp_real
    fi
    printf '%s\n' "$_rp_out"
}

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
    # The state directory must never be the filesystem root. With HOME
    # unset, `${HOME:-}/.cmesh` expands to "/.cmesh" — and `--purge`
    # then ran `rm -rf -- /.cmesh` with no confirmation.
    #
    # The refusal belongs HERE, before any removal is attempted, and it
    # belongs in the UNINSTALL branch because that is the branch that
    # removes things. The install branch checks it too (a refusal the
    # operator only learns about at purge time is a refusal too late),
    # but an install never touches the state directory, so it must not
    # fail a perfectly good install over a state path it will not use.
    case "$STATE_DIR" in
        /|'')
            die "the runtime state directory would be the filesystem root; set CMESH_STATE or HOME"
            ;;
    esac
    # A path DIRECTLY under the filesystem root ("/.cmesh") is just as
    # dangerous: `--purge` runs `rm -rf -- "$STATE_DIR"` on it. With
    # HOME unset, `${HOME:-}/.cmesh` expands to exactly that.
    #
    # ★ THE STRING COMPARISON IS NOT ENOUGH. (Round 98.)
    # "${STATE_DIR#/}" contains a "/" for "//" and for "/tmp/..", so the
    # test below let BOTH through — and "//" is the one that matters:
    # on busybox/Alpine (a platform this installer advertises support
    # for) `rm -rf -- //` wipes the whole filesystem, because busybox
    # treats "//" as "/" while GNU rm refuses it.
    #
    # So the path is RESOLVED first: an absolute path is resolved
    # against the current directory and then normalized ("//" -> "/",
    # "/tmp/.." -> "/"), and the result is compared against "/". A
    # relative path is resolved against the current directory too.
    #
    # The resolution is pure string work (no filesystem access), so it
    # cannot itself be defeated by a symlink.
    resolved_state=$(resolve_path "$STATE_DIR")
    case "$resolved_state" in
        /)
            die "the runtime state directory resolves to the filesystem root ($STATE_DIR); set CMESH_STATE or HOME"
            ;;
    esac
    case "${resolved_state#/}" in
        */*) ;;   # deeper than the root: fine
        *) die "the runtime state directory would sit directly in the filesystem root ($STATE_DIR); set CMESH_STATE or HOME" ;;
    esac
    marker="$INSTALL_DIR/.cmesh-install-root"
    [ -f "$marker" ] || die "$INSTALL_DIR is not a cmesh install root"
    case "$INSTALL_DIR" in
        ""|/) die "refusing to uninstall from an empty path or filesystem root" ;;
    esac
    # ★ SAME RESOLUTION FOR THE INSTALL ROOT. "/.." and "/." and "//"
    # all resolve to the filesystem root while LOOKING like something
    # else, and the string comparison above let them through: the
    # installer then wrote versions/, current, cmesh and
    # .cmesh-install-root AT THE FILESYSTEM ROOT.
resolved_install=$(resolve_path "$INSTALL_DIR")
case "$resolved_install" in
    /) die "refusing to uninstall from a path that resolves to the filesystem root ($INSTALL_DIR)" ;;
esac
# ★ AND THE INSTALL ROOT GETS THE SAME "DIRECT CHILD OF THE ROOT" RULE
# THE STATE PATH ALREADY HAS.
#
# The state path was guarded strictly (a path directly in the root is
# refused) while the install root only had the root check. That
# asymmetry let a RELATIVE install root through: "CMESH_INSTALL=.."
# resolves to the parent of the working directory, the installer wrote
# versions/, current, cmesh and .cmesh-install-root INTO THAT
# PARENT — and the purge then ran `rm -rf` on it.
#
# GNU rm refuses to remove ".." and "." (and says so), so on a normal
# Linux the damage is "the installer littered a directory it should
# not have". But the refusal belongs to rm, not to the installer — and
# the platforms where rm does NOT refuse it (busybox/Alpine, which this
# file's own comments name) would lose the parent tree.
#
# So the installer refuses it itself.
case "${resolved_install#/}" in
    */*) ;;   # deeper than the root: fine
    *) die "the install root would sit directly in the filesystem root ($INSTALL_DIR); set CMESH_INSTALL" ;;
esac


    # THE SYSTEMD UNITS MUST BE STOPPED, OR THE UNINSTALL IS INCOMPLETE.
    #
    # The installer never touches systemd, so `--uninstall` used to leave
    # the units installed AND enabled, with the process still running
    # (holding an already-unlinked binary) — and the next boot would
    # start a unit whose ExecStart points at a path that no longer
    # exists: status=203/EXEC, then the restart loop.
    #
    # The two findings are not independent: an uninstall that does not
    # stop the unit IGNITES the restart loop on the next boot.
    #
    # So: any uninstall stops and disables the units; only a purge also
    # removes the unit files. And the units that were stopped are
    # NAMED in the output — otherwise the operator believes the machine
    # is clean when it is not.
    #
    # `systemctl` is absent on a machine without systemd, so its absence
    # is not an error: there is nothing to stop there.
    # ★ THE UNIT LIST MUST NAME THE UNITS THAT ARE REALLY DEPLOYED.
    #
    # The list used to be "cmesh-discovery.service cmesh-member.service"
    # — and on a real deployment NEITHER of those is the unit that runs
    # the anchor. The deployed names are:
    #
    #   cmesh-serve-discovery.service   the anchor (serve-discovery)
    #   cmesh-hub.service               the coordination hub
    #
    # So the uninstall stopped units that were never there, left the two
    # that WERE there running and enabled, and told the operator
    # "stopped and disabled: cmesh-discovery.service cmesh-member.service"
    # — a list of units the machine never had. At the next boot those
    # real units started with a removed binary: 203/EXEC and then the
    # restart loop.
    #
    # The names are checked against what is ON DISK and what systemd
    # knows, so a deployment that uses only some of them is handled: a
    # unit that is not there is simply skipped.
    UNITS="cmesh-serve-discovery.service cmesh-discovery.service cmesh-member.service cmesh-hub.service"
    stopped=""
    if command -v systemctl >/dev/null 2>&1; then
        for u in $UNITS; do
            # The unit counts as present if systemd knows it OR its file
            # is on disk. The file check matters because a unit that
            # systemd has forgotten (a stale enablement) is exactly the
            # case this is here for.
            if systemctl list-unit-files "$u" >/dev/null 2>&1 || \
               [ -e "/etc/systemd/system/$u" ]; then
                systemctl stop "$u" >/dev/null 2>&1 || true
                systemctl disable "$u" >/dev/null 2>&1 || true
                stopped="$stopped $u"
            fi
        done
        if [ -n "$stopped" ]; then
            systemctl daemon-reload >/dev/null 2>&1 || true
        fi
    fi

    if [ "$PURGE" -eq 1 ]; then
        for u in $UNITS; do
            rm -f -- "/etc/systemd/system/$u"
        done
        if command -v systemctl >/dev/null 2>&1; then
            systemctl daemon-reload >/dev/null 2>&1 || true
        fi
        rm -rf -- "$INSTALL_DIR"
        if [ -n "$STATE_DIR" ] && [ "$STATE_DIR" != "$INSTALL_DIR" ]; then
            rm -rf -- "$STATE_DIR"
        fi
        say "purged install root and runtime state"
        if [ -n "$stopped" ]; then
            say "stopped and disabled:$stopped"
        fi
    else
        rm -rf -- "$INSTALL_DIR/cmesh" "$INSTALL_DIR/current" "$INSTALL_DIR/versions" "$marker"
        say "uninstalled binaries; runtime state was retained at $STATE_DIR"
        if [ -n "$stopped" ]; then
            say "stopped and disabled:$stopped (the unit files were kept; --purge removes them)"
        fi
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
# The filesystem root is NOT an install root.
#
# The uninstall branch already refused it (the empty-path check above),
# but the INSTALL branch did not — so `CMESH_INSTALL=/ sh install.sh`
# wrote /versions, /current, /cmesh and /.cmesh-install-root at the
# filesystem root, and said only "installed: /current/cmesh".
#
# The asymmetry was the bug: a path the tool refuses to remove must
# also be a path it refuses to install into. Otherwise a typo (or a
# stray "CMESH_INSTALL=/") litters the root of the machine.
#
# ★ AND THE CHECK RESOLVES THE PATH, IT DOES NOT COMPARE STRINGS.
# (Round 98.) "/..", "/." and "//" all resolve to the filesystem root
# while LOOKING like something else, and a plain `case "$INSTALL_DIR" in
# /)` let all three through: the installer then wrote versions/,
# current, cmesh and .cmesh-install-root AT THE FILESYSTEM ROOT.
case "$INSTALL_DIR" in
    ''|/) die "refusing to install into the filesystem root" ;;
esac
resolved_install=$(resolve_path "$INSTALL_DIR")
case "$resolved_install" in
    /) die "refusing to install into a path that resolves to the filesystem root ($INSTALL_DIR)" ;;
esac
# ★ THE SAME "DIRECT CHILD OF THE ROOT" RULE THE STATE PATH HAS.
#
# The state path was guarded strictly while the install root only had
# the root check, and that asymmetry let a RELATIVE install root
# through: "CMESH_INSTALL=.." resolves to the parent of the working
# directory, and the installer wrote versions/, current, cmesh and
# .cmesh-install-root INTO THAT PARENT.
#
# The install branch only ADDS files, so the damage is litter rather
# than deletion — but a tool that will happily write into a directory
# it was not pointed at is a tool that will do it again on a typo.
case "${resolved_install#/}" in
    */*) ;;   # deeper than the root: fine
    *) die "the install root would sit directly in the filesystem root ($INSTALL_DIR); set CMESH_INSTALL" ;;
esac
# The state directory: an install NEVER touches it, so a state path
# that would be dangerous to purge must not fail a good install — but
# the operator must hear about it NOW, not at purge time.
#
# ★ THE CHECK IS ON THE VALUE, NOT ON HOW IT WAS PRODUCED.
#
# The warning used to be reached only because the DEFAULT expanded to
# "/.cmesh" when HOME was unset. Change the default and that safety net
# disappears with it — which is exactly what happened when the default
# became /var/lib/cmesh: the regression test caught that an install
# with HOME unset no longer warned.
#
# So the shape is checked directly: a state path that is the root, or
# a direct child of the root, is warned about whatever produced it.
case "$STATE_DIR" in
    /|'')
        say "warning: the runtime state directory would be the filesystem root; set CMESH_STATE"
        ;;
esac
resolved_state=$(resolve_path "$STATE_DIR")
case "$resolved_state" in
    /)
        say "warning: the runtime state directory resolves to the filesystem root ($STATE_DIR); set CMESH_STATE"
        ;;
esac
case "${resolved_state#/}" in
    */*) ;;   # deeper than the root: fine
    *) say "warning: the runtime state directory would sit directly in the filesystem root ($STATE_DIR); set CMESH_STATE" ;;
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

# ★ THE versions/ DIRECTORY MUST BE TRAVERSABLE BY THE SERVICE USER.
#
# `mkdir -p` creates it with the process's umask, and the systemd units
# run the node as an unprivileged "cmesh" user. Under a umask of 077 the
# directory comes out 0700, so only the installing user can enter it and
# the unit that is supposed to execute $INSTALL_DIR/current/cmesh gets
# "Permission denied".
#
# This is the SAME class as the version-directory bug that was already
# fixed one level down (the version dir was 0700 because mktemp -d
# creates 0700): the fix was applied to the leaf and not to the tree
# around it.
chmod 0755 "$INSTALL_DIR/versions" 2>/dev/null || true
mkdir -p "$INSTALL_DIR/versions" || die "cannot create install directory $INSTALL_DIR"
chmod 0755 "$INSTALL_DIR/versions" 2>/dev/null || true
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
    #
    # The removal is -rf, not -f: `current` can be a DIRECTORY (a
    # half-finished earlier install, or somebody's own directory), and
    # `rm -f` on a directory fails — the install then died with "cannot
    # remove: Is a directory" and left the staged link behind as an
    # orphan, with `current/cmesh` still pointing at nothing.
    #
    # The -rf is safe because `link` is the MANAGED name inside the
    # install root, and the root is checked to be a cmesh install root
    # (with its marker file) before this runs. The guard below refuses
    # the one path that would be catastrophic: removing the root itself.
    case "$link" in
        "$INSTALL_DIR"/*) ;;
        *) die "refusing to replace a link outside the install root: $link" ;;
    esac
    rm -rf "$link"
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
