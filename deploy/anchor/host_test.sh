#!/usr/bin/env bash
# Offline regression for deploy/anchor/host.sh.
#
# ★ WHAT THIS PROVES.
#   The payload is IDEMPOTENT: running it twice over the same host
#   changes nothing the second time. That is the property that makes a
#   reinstall a command instead of an investigation — and it is the
#   property that is easy to lose: one extra `cmesh init --force` and
#   the network's account key is a new one, every member's certificate
#   is worthless, and nobody notices until the joins start failing.
#
#   So the assertions are:
#     * both runs exit 0
#     * the identity files are byte-identical after the second run
#     * the binary is not replaced when its content is unchanged
#     * the previous binary was kept as cmesh.bak.<epoch>
#     * the panel token is not printed, and the passphrase is not printed
#
#   It runs in a disposable prefix, so it touches nothing outside $TMP
#   and it needs no network and no systemd.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

# digest prints "<sha256>  <path>" for a file, on either platform.
digest() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	else
		shasum -a 256 "$1"
	fi
}

# stat_fmt prints "<mode> <owner> <group>" for a path, on either platform.
stat_fmt() {
	if stat -c '%a %U %G' "$1" >/dev/null 2>&1; then
		stat -c '%a %U %G' "$1"
	else
		stat -f '%Lp %Su %Sg' "$1"
	fi
}

# stat_inode prints the inode number of a path, on either platform.
stat_inode() {
	if stat -c '%i' "$1" >/dev/null 2>&1; then
		stat -c '%i' "$1"
	else
		stat -f '%i' "$1"
	fi
}

# The binary the payload installs. The container run mounts one that was
# cross-compiled for the container's platform.
BIN_SRC="${CMESH_BIN_SRC:-}"
if [ -z "$BIN_SRC" ]; then
	if command -v cmesh >/dev/null 2>&1; then
		BIN_SRC="$(command -v cmesh)"
	elif command -v go >/dev/null 2>&1; then
		BIN_SRC="$WORK/cmesh"
		CGO_ENABLED=0 go build -o "$BIN_SRC" "$HERE/../../cmd/cm"
	else
		fail "no binary to install: set CMESH_BIN_SRC, or put a cmesh on PATH, or install go"
	fi
fi
[ -f "$BIN_SRC" ] || fail "the binary $BIN_SRC does not exist"

PREFIX="$WORK/prefix"
mkdir -p "$PREFIX/usr/local/bin"
# The service user has to be able to walk into the prefix: mktemp -d
# creates 0700, and the identity is created AS the service user.
chmod 0755 "$WORK" "$PREFIX"

STATE="$PREFIX/var/lib/cmesh"
ETC="$PREFIX/etc/cmesh"
BIN="$PREFIX/usr/local/bin/cmesh"
UNIT="$PREFIX/etc/systemd/system/cmesh-serve-discovery.service"

PASSPHRASE="anchor-regression-passphrase-9f3a2c"

# The environment file the payload installs. It carries the passphrase
# and NOT the account public key: the payload has to fill that in from
# the account it finds (or creates).
ENV_SRC="$WORK/discovery.env"
cat >"$ENV_SRC" <<EOF
CMESH_STATE=$STATE
CMESH_ADDR=:18080
CMESH_ANNOUNCE=127.0.0.1:18080
CMESH_PASSPHRASE=$PASSPHRASE
EOF

# A previous binary, so the first run has something to keep. It is not
# the binary under test, so the payload must replace it and keep it.
printf '#!/bin/sh\necho the previous binary\n' >"$BIN"
chmod 0755 "$BIN"

run_payload() {
	local out="$1"
	shift
	set +e
	CMESH_PREFIX="$PREFIX" \
		CMESH_DISCOVERY_ENV_SOURCE="$ENV_SRC" \
		CMESH_BIN_SRC="$BIN_SRC" \
		CMESH_UNIT_SRC="$HERE/systemd/cmesh-serve-discovery.service" \
		"$@" \
		bash "$HERE/host.sh" >"$out" 2>&1
	echo $?
}

rc1="$(run_payload "$WORK/run1.out")"
printf '=== first run (exit %s) ===\n' "$rc1"
cat "$WORK/run1.out"
[ "$rc1" = "0" ] || fail "the first run exited $rc1"

# --- the identity the first run created -------------------------------
[ -f "$STATE/device.json" ] || fail "the first run did not create a device identity"
[ -f "$STATE/account.json" ] || fail "the first run did not create an account key"
cert_count="$(find "$STATE" -name '*.cert.json' -type f | wc -l | tr -d ' ')"
[ "$cert_count" -ge 1 ] || fail "the first run did not sign the device into the account"

sums1="$(cd "$STATE" && find . -name '*.json' -type f | sort | while read -r f; do digest "$f"; done)"
env1="$(digest "$ETC/discovery.env")"
inode1="$(stat_inode "$BIN")"
backups1="$(find "$PREFIX/usr/local/bin" -name 'cmesh.bak.*' | wc -l | tr -d ' ')"

[ "$backups1" = "1" ] || fail "the previous binary was not kept as cmesh.bak.<epoch> (found $backups1)"
cmp -s "$BIN" "$BIN_SRC" || fail "the installed binary is not the one that was handed in"

# --- the second run ---------------------------------------------------
rc2="$(run_payload "$WORK/run2.out")"
printf '\n=== second run (exit %s) ===\n' "$rc2"
cat "$WORK/run2.out"
[ "$rc2" = "0" ] || fail "the second run exited $rc2"

sums2="$(cd "$STATE" && find . -name '*.json' -type f | sort | while read -r f; do digest "$f"; done)"
env2="$(digest "$ETC/discovery.env")"
inode2="$(stat_inode "$BIN")"
backups2="$(find "$PREFIX/usr/local/bin" -name 'cmesh.bak.*' | wc -l | tr -d ' ')"

# ★ THE IDENTITY MUST NOT MOVE.
if [ "$sums1" != "$sums2" ]; then
	printf 'the identity changed on the second run:\n%s\n---\n%s\n' "$sums1" "$sums2" >&2
	fail "the second run touched the identity files"
fi
printf 'the identity after the FIRST run:\n%s\n' "$sums1"
printf 'the identity after the SECOND run:\n%s\n' "$sums2"
printf 'the environment file after the first run:  %s\n' "$env1"
printf 'the environment file after the second run: %s\n' "$env2"
printf 'the binary inode after the first run:  %s\n' "$inode1"
printf 'the binary inode after the second run: %s\n' "$inode2"
printf 'the previous binaries kept after the first run:  %s\n' "$backups1"
printf 'the previous binaries kept after the second run: %s\n' "$backups2"

# ★ THE ENVIRONMENT FILE MUST NOT MOVE EITHER: the account public key
# was filled in by the first run and it has to stay.
[ "$env1" = "$env2" ] || fail "the environment file changed on the second run"

# ★ AN IDENTICAL BINARY IS NOT REPLACED.
[ "$inode1" = "$inode2" ] || fail "the binary was replaced even though its content did not change"
[ "$backups1" = "$backups2" ] || fail "the second run made another backup of the binary"

# ★ THE REBUILD IS ANNOUNCED ONCE, NOT EVERY TIME.
grep -q 'THE ANCHOR IDENTITY WAS REBUILT' "$WORK/run1.out" ||
	fail "the first run did not announce the rebuilt identity"
grep -q 'THE ANCHOR IDENTITY WAS REBUILT' "$WORK/run2.out" &&
	fail "the second run announced a rebuilt identity it did not rebuild"
grep -q 'the account identity is already there' "$WORK/run2.out" ||
	fail "the second run did not say that the identity was left alone"

# ★ THE SCRIPT SAYS WHAT IT SKIPPED.
grep -qE 'skipping daemon-reload|is active' "$WORK/run2.out" ||
	fail "the script said nothing about systemd"

# ★ NO SECRET EVER REACHES THE OUTPUT.
if grep -Fq "$PASSPHRASE" "$WORK/run1.out" "$WORK/run2.out"; then
	fail "the passphrase appears in the output"
fi
TOKEN_VALUE="$(cat "$ETC/panel-token" 2>/dev/null || true)"
[ -n "$TOKEN_VALUE" ] || fail "no panel token was generated"
if grep -Fq "$TOKEN_VALUE" "$WORK/run1.out" "$WORK/run2.out"; then
	fail "the panel token appears in the output"
fi

# ★ THE LAYOUT IS THE DEPLOYED ONE.
[ "$(stat_fmt "$ETC/discovery.env")" = "600 root cmesh" ] ||
	fail "the environment file is $(stat_fmt "$ETC/discovery.env"), want 600 root cmesh"
[ "$(stat_fmt "$ETC/panel-token")" = "640 root cmesh" ] ||
	fail "the panel token file is $(stat_fmt "$ETC/panel-token"), want 640 root cmesh"
case "$(stat_fmt "$STATE")" in
"750 cmesh cmesh") ;;
*) fail "the state directory is $(stat_fmt "$STATE"), want 750 cmesh cmesh" ;;
esac

# ★ THE UNIT ON THE HOST IS THE UNIT IN THE REPOSITORY.
cmp -s "$UNIT" "$HERE/systemd/cmesh-serve-discovery.service" ||
	fail "the installed unit is not the one in deploy/anchor/systemd/"

# ★ THE START LIMIT IS A [Unit] DIRECTIVE. (Round 104.)
#
# systemd has no Service.StartLimit* alias, so the same directives under
# [Service] are silently ignored — and the limit the operator believes
# in is not the one systemd applies.
unit_section() {
	awk -v want="$1" '
		/^\[/ { section = substr($0, 2, length($0) - 2); next }
		{
			eq = index($0, "=")
			if (eq == 0) next
			k = substr($0, 1, eq - 1)
			sub(/[ \t]+$/, "", k)
			if (k == want) print section
		}
	' "$HERE/systemd/cmesh-serve-discovery.service"
}
[ "$(unit_section StartLimitIntervalSec)" = "Unit" ] ||
	fail "StartLimitIntervalSec is not in the [Unit] section"
[ "$(unit_section StartLimitBurst)" = "Unit" ] ||
	fail "StartLimitBurst is not in the [Unit] section"
[ "$(unit_section ExecStartPre)" = "" ] ||
	fail "the unit uses ExecStartPre: a precondition written as ExecStartPre is a start failure and restarts forever"

# ---------------------------------------------------------------------------
# ★ THE `su` BRANCH MUST ACTUALLY DELIVER THE PASSPHRASE. (Review.)
#
# `su` does not reliably preserve the environment: the child's environ
# is rebuilt, and CMESH_PASSPHRASE can go with it. The passphrase is the
# ONE variable that must survive — without it readPassphrase falls
# through to the interactive prompt, stdin is /dev/null, and the command
# fails instantly with EOF. A rebuild that fails on the OLDEST machines
# is exactly the failure this script exists to remove.
#
# So the su branch is chosen by MEASUREMENT (a probe process under each
# variant), and this asserts the CONSEQUENCE: the identity really was
# created, by the su branch, with the passphrase delivered through the
# environment. The assertion is on the ARTIFACT (account.json exists and
# is a real key), not on a log line — a test that only checked output
# would pass even if the passphrase had not arrived.
#
# It runs in its own prefix, so it cannot disturb the two runs above.
# ---------------------------------------------------------------------------
SU_PREFIX="$WORK/su-prefix"
mkdir -p "$SU_PREFIX/usr/local/bin"
chmod 0755 "$WORK" "$SU_PREFIX"
SU_STATE="$SU_PREFIX/var/lib/cmesh"
SU_ENV="$WORK/su-discovery.env"
cat >"$SU_ENV" <<EOF
CMESH_STATE=$SU_STATE
CMESH_ADDR=:18081
CMESH_ANNOUNCE=127.0.0.1:18081
CMESH_PASSPHRASE=$PASSPHRASE
EOF

if command -v su >/dev/null 2>&1; then
	set +e
	CMESH_PREFIX="$SU_PREFIX" \
		CMESH_DISCOVERY_ENV_SOURCE="$SU_ENV" \
		CMESH_BIN_SRC="$BIN_SRC" \
		CMESH_UNIT_SRC="$HERE/systemd/cmesh-serve-discovery.service" \
		CMESH_FORCE_SU=1 \
		bash "$HERE/host.sh" >"$WORK/run-su.out" 2>&1
	su_rc=$?
	set -e

	if [ "$su_rc" = "0" ]; then
		# The su branch ran and the passphrase arrived: the account key
		# exists and it is a real, loadable key.
		[ -f "$SU_STATE/account.json" ] ||
			fail "the su branch exited 0 but created no account key"
		[ -f "$SU_STATE/device.json" ] ||
			fail "the su branch exited 0 but created no device identity"
		# The account key is ENCRYPTED with the passphrase, so reading it
		# back needs the passphrase too — in the ENVIRONMENT, the same
		# way the payload delivered it. If this call fails, the passphrase
		# did NOT arrive, which is the whole point of the assertion.
		su_pub="$(CMESH_PASSPHRASE="$PASSPHRASE" \
			"$SU_PREFIX/usr/local/bin/cmesh" account show --state "$SU_STATE" 2>/dev/null |
			awk '/^pubkey\.b64 /{print $2; exit}')"
		[ -n "$su_pub" ] ||
			fail "the su branch created an account key the CLI cannot read: the passphrase did not arrive"
		if grep -Fq "$PASSPHRASE" "$WORK/run-su.out"; then
			fail "the passphrase appears in the su branch output"
		fi
		printf 'the su branch delivered the passphrase: the account key loads and prints a public key\n'
	else
		# The su branch REFUSED. That is the other acceptable outcome, and
		# it has to be a LOUD, USEFUL refusal — not a silent failure and
		# not a command that failed for an unexplained reason.
		grep -q 'Run these three commands BY HAND' "$WORK/run-su.out" ||
			fail "the su branch failed without printing the manual steps: $(tail -5 "$WORK/run-su.out")"
		grep -q 'no runuser, no setpriv' "$WORK/run-su.out" ||
			fail "the su branch failed without saying which tools are missing"
		printf 'the su branch refused (no su variant preserves the environment) and printed the manual steps\n'
	fi
else
	printf 'su is not on this machine: the su branch is not exercised here\n'
fi

# ---------------------------------------------------------------------------
# ★ AND THE REFUSAL PATH MUST BE EXERCISED TOO.
#
# The other acceptable outcome is a LOUD refusal: a machine whose `su`
# resets the environment and which has neither runuser nor setpriv. The
# script must then print the manual steps and FAIL — it must not run a
# command that is guaranteed to fail, and it must not put the passphrase
# on a command line to make it work.
#
# The machine is simulated with a stub `su` that drops the environment
# (the way a real environment-resetting su does) and a stub PATH that
# hides runuser and setpriv. The assertion is on the OUTCOME: a
# non-zero exit, the manual steps in the output, and NO identity file
# created — a half-built identity would be worse than none.
# ---------------------------------------------------------------------------
STUB_BIN="$WORK/stub-bin"
mkdir -p "$STUB_BIN"
cat >"$STUB_BIN/su" <<'STUBEOF'
#!/bin/sh
# A stub su that behaves like the environment-resetting kind: it keeps
# only the variables it was told to keep, and CMESH_PASSPHRASE is not
# one of them. This is the failure mode the su branch has to detect.
for arg in "$@"; do
	case "$arg" in
	-c) break ;;
	esac
done
shift || true
exec env -i PATH=/usr/bin:/bin HOME=/root /bin/sh -c "$1"
STUBEOF
chmod 0755 "$STUB_BIN/su"

REFUSE_PREFIX="$WORK/refuse-prefix"
mkdir -p "$REFUSE_PREFIX/usr/local/bin"
chmod 0755 "$WORK" "$REFUSE_PREFIX"
REFUSE_STATE="$REFUSE_PREFIX/var/lib/cmesh"
REFUSE_ENV="$WORK/refuse-discovery.env"
cat >"$REFUSE_ENV" <<EOF
CMESH_STATE=$REFUSE_STATE
CMESH_ADDR=:18082
CMESH_ANNOUNCE=127.0.0.1:18082
CMESH_PASSPHRASE=$PASSPHRASE
EOF

set +e
CMESH_PREFIX="$REFUSE_PREFIX" \
	CMESH_DISCOVERY_ENV_SOURCE="$REFUSE_ENV" \
	CMESH_BIN_SRC="$BIN_SRC" \
	CMESH_UNIT_SRC="$HERE/systemd/cmesh-serve-discovery.service" \
	CMESH_FORCE_SU=1 \
	PATH="$STUB_BIN:/usr/bin:/bin" \
	bash "$HERE/host.sh" >"$WORK/run-refuse.out" 2>&1
refuse_rc=$?
set -e

[ "$refuse_rc" != "0" ] ||
	fail "the su branch used a su that resets the environment and still exited 0"
grep -q 'Run these three commands BY HAND' "$WORK/run-refuse.out" ||
	fail "the su branch did not print the manual steps: $(tail -8 "$WORK/run-refuse.out")"
grep -q 'no runuser, no setpriv' "$WORK/run-refuse.out" ||
	fail "the su branch did not say which tools are missing"
[ ! -f "$REFUSE_STATE/account.json" ] ||
	fail "the refused run created an account key anyway: a half-built identity is worse than none"
if grep -Fq "$PASSPHRASE" "$WORK/run-refuse.out"; then
	fail "the passphrase appears in the output of the refused run"
fi
printf 'the su branch refused a su that resets the environment, printed the manual steps, and built no identity\n'

printf '\n%s\n' "deploy/anchor host regression: ok"
