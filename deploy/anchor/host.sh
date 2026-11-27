#!/usr/bin/env bash
# Rebuild the container-mesh discovery anchor on a host.
#
# ★ WHY THIS EXISTS.
#   The anchor is the node the whole network leans on: it holds the
#   account key, the roster and the panel token. When its host is
#   reinstalled, all of that is gone, and putting it back was a page of
#   notes plus a session of guessing ("which user owned what, which
#   file held the token, what was the unit called again?").
#
#   This script makes the rebuild one command. It is the payload half
#   of deploy/anchor/install.sh and it runs on the TARGET host as root.
#
# ★ IT IS IDEMPOTENT. Running it twice must change nothing the second
#   time:
#     * the identity is created ONLY when it is missing — a regenerated
#       account key invalidates every member's certificate, so an
#       existing account.json is never touched, not even re-encrypted;
#     * the binary is replaced ONLY when its content differs from the
#       installed one, and the previous binary is kept as
#       cmesh.bak.<epoch>;
#     * the panel token is generated ONLY when the file is missing.
#
# ★ WHAT IT DELIBERATELY DOES NOT DO.
#     * it does not invent /etc/cmesh/discovery.env. The listen address
#       and the announced endpoint are deployment facts, so the file
#       must already exist or must be handed in with
#       CMESH_DISCOVERY_ENV_SOURCE;
#     * it does not print the panel token, and it does not rotate one
#       that already exists;
#     * it does not delete anything except the fixed-name staging files
#       it creates itself;
#     * it does not move the anchor onto the managed installation
#       layout (/opt/container-mesh/current/cmesh) — that is a later
#       milestone, and the deployed unit still runs /usr/local/bin/cmesh.
#
# ★ THE PASSPHRASE NEVER REACHES A COMMAND LINE.
#   `cmesh init` and `cmesh account create` read the passphrase from
#   --passphrase, then from $CMESH_PASSPHRASE, then from an interactive
#   prompt (internal/cli/mesh.go, readPassphrase). This script uses the
#   ENVIRONMENT: the value is read out of the environment file and
#   exported into the child process, so it never appears in ps output
#   and never in this script's own output.
#
# Usage (on the target host, as root):
#   CMESH_BIN_SRC=/tmp/cmesh.anchor.install \
#   CMESH_DISCOVERY_ENV_SOURCE=/tmp/discovery.env \
#   bash host.sh
#
# Environment variables (all optional):
#   CMESH_PREFIX                 root every path below this prefix
#                                (the offline regression uses it)
#   CMESH_STATE_DIR              default /var/lib/cmesh
#   CMESH_ETC_DIR                default /etc/cmesh
#   CMESH_BIN_PATH               default /usr/local/bin/cmesh
#   CMESH_UNIT_DIR               default /etc/systemd/system
#   CMESH_UNIT_PATH              default <unit dir>/cmesh-serve-discovery.service
#   CMESH_DISCOVERY_ENV          default <etc dir>/discovery.env
#   CMESH_PANEL_TOKEN_FILE       default <etc dir>/panel-token
#   CMESH_DISCOVERY_ENV_SOURCE   a local file to install as the env file
#   CMESH_BIN_SRC                a local binary to install
#   CMESH_UNIT_SRC               a local unit file to install
#   CMESH_SERVICE_USER/GROUP     default cmesh / cmesh
#   CMESH_SKIP_IDENTITY=1        do not create the identity at all
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

say() { printf '  %s\n' "$*"; }
warn() { printf '  !! %s\n' "$*" >&2; }
die() {
	printf '  !! %s\n' "$*" >&2
	exit 1
}
banner() {
	printf '\n'
	printf '############################################################\n'
	printf '# %s\n' "$*"
	printf '############################################################\n'
	printf '\n'
}

# ---------------------------------------------------------------------------
# ★ THE PATH GUARDS RESOLVE, THEY DO NOT COMPARE STRINGS. (Round 98.)
#
# The purge that deleted a host's filesystem root compared the path
# STRING against "/", so "//", "/." and "/tmp/.." all got through. Every
# path this script writes to is normalised first, and a path that
# resolves to the root is refused before anything is touched.
# ---------------------------------------------------------------------------
resolve_path() {
	local p="$1" rest seg out=""
	case "$p" in
		/*) ;;
		*) p="$PWD/$p" ;;
	esac
	rest="$p"
	while [ -n "$rest" ]; do
		case "$rest" in
			*/*) seg="${rest%%/*}"; rest="${rest#*/}" ;;
			*) seg="$rest"; rest="" ;;
		esac
		case "$seg" in
			'' | '.') ;;
			'..') out="${out%/*}" ;;
			*) out="$out/$seg" ;;
		esac
	done
	printf '%s' "${out:-/}"
}

guard_path() {
	local p="$1" what="$2" r
	case "$p" in
		/*) ;;
		*) die "$what must be an absolute path: $p" ;;
	esac
	r="$(resolve_path "$p")"
	if [ "$r" = "/" ]; then
		die "$what resolves to the filesystem root and is refused: $p"
	fi
	printf '%s' "$r"
}

# ---------------------------------------------------------------------------
# THE LAYOUT. Every path below is named by a variable and resolved before
# it is used.
# ---------------------------------------------------------------------------
SERVICE_USER="${CMESH_SERVICE_USER:-cmesh}"
SERVICE_GROUP="${CMESH_SERVICE_GROUP:-cmesh}"
PREFIX="${CMESH_PREFIX:-}"

if [ -n "$PREFIX" ]; then
	STATE_DIR="$PREFIX/var/lib/cmesh"
	ETC_DIR="$PREFIX/etc/cmesh"
	BIN_PATH="$PREFIX/usr/local/bin/cmesh"
	UNIT_DIR="$PREFIX/etc/systemd/system"
else
	STATE_DIR="${CMESH_STATE_DIR:-/var/lib/cmesh}"
	ETC_DIR="${CMESH_ETC_DIR:-/etc/cmesh}"
	BIN_PATH="${CMESH_BIN_PATH:-/usr/local/bin/cmesh}"
	UNIT_DIR="${CMESH_UNIT_DIR:-/etc/systemd/system}"
fi

UNIT_NAME="cmesh-serve-discovery.service"
UNIT_PATH="${CMESH_UNIT_PATH:-$UNIT_DIR/$UNIT_NAME}"
ENV_PATH="${CMESH_DISCOVERY_ENV:-$ETC_DIR/discovery.env}"
TOKEN_PATH="${CMESH_PANEL_TOKEN_FILE:-$ETC_DIR/panel-token}"
UNIT_SRC="${CMESH_UNIT_SRC:-$HERE/systemd/$UNIT_NAME}"
BIN_SRC="${CMESH_BIN_SRC:-}"
ENV_SRC="${CMESH_DISCOVERY_ENV_SOURCE:-}"
SKIP_IDENTITY="${CMESH_SKIP_IDENTITY:-0}"

STATE_DIR="$(guard_path "$STATE_DIR" "the state directory")"
ETC_DIR="$(guard_path "$ETC_DIR" "the configuration directory")"
BIN_PATH="$(guard_path "$BIN_PATH" "the binary path")"
UNIT_PATH="$(guard_path "$UNIT_PATH" "the unit path")"
ENV_PATH="$(guard_path "$ENV_PATH" "the environment file")"
TOKEN_PATH="$(guard_path "$TOKEN_PATH" "the panel token file")"

IDENTITY_CREATED=0
UNIT_CHANGED=0

# ---------------------------------------------------------------------------
# ★ THE ENVIRONMENT FILE IS READ, NOT SOURCED.
#
# `source /etc/cmesh/discovery.env` would execute whatever is in it as
# root. The file is a systemd EnvironmentFile, and this reads it the way
# systemd does: KEY=value, optional surrounding quotes, no expansion.
# ---------------------------------------------------------------------------
env_get() {
	local file="$1" key="$2"
	[ -f "$file" ] || return 0
	awk -v want="$key" '
		{
			line = $0
			sub(/\r$/, "", line)
			sub(/^[ \t]+/, "", line)
			if (line == "" || substr(line, 1, 1) == "#") next
			eq = index(line, "=")
			if (eq == 0) next
			k = substr(line, 1, eq - 1)
			v = substr(line, eq + 1)
			sub(/[ \t]+$/, "", k)
			if (k != want) next
			if (length(v) >= 2 && substr(v, 1, 1) == "\"" && substr(v, length(v), 1) == "\"")
				v = substr(v, 2, length(v) - 2)
			else if (length(v) >= 2 && substr(v, 1, 1) == sprintf("%c", 39) && substr(v, length(v), 1) == sprintf("%c", 39))
				v = substr(v, 2, length(v) - 2)
			print v
			exit
		}
	' "$file"
}

# env_set rewrites ONE key in the environment file, in place, atomically.
#
# It is used for exactly one key: CMESH_ACCOUNT_PUB. The script may have
# just regenerated the account key, and a public key that does not match
# the account on disk makes the anchor refuse every enrollment while it
# looks perfectly healthy.
env_set() {
	local file="$1" key="$2" value="$3" tmp="$1.new"
	umask 077
	awk -v want="$key" -v val="$value" '
		BEGIN { done = 0 }
		{
			raw = $0
			line = raw
			sub(/\r$/, "", line)
			probe = line
			sub(/^[ \t]+/, "", probe)
			eq = index(probe, "=")
			if (eq > 0) {
				k = substr(probe, 1, eq - 1)
				sub(/[ \t]+$/, "", k)
				if (k == want) {
					print want "=" val
					done = 1
					next
				}
			}
			print raw
		}
		END { if (!done) print want "=" val }
	' "$file" >"$tmp"
	chmod 0600 "$tmp"
	chown root:"$SERVICE_GROUP" "$tmp" 2>/dev/null || true
	mv "$tmp" "$file"
}

have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
# ★ RUN AS THE SERVICE USER.
#
# The identity files belong to cmesh, and the CLI writes them 0600. Run
# as root and the files would be root-owned, and the service (which runs
# as cmesh) could not read them — a failure that looks like "the node
# came up but cannot find its identity".
#
# runuser/setpriv are tried in that order: both pass the environment
# through, which is how CMESH_PASSPHRASE reaches the CLI without ever
# being a command-line argument.
#
# ★ `su` IS NOT EQUIVALENT, AND IT IS NOT USED BLINDLY.
#
# `su` does NOT reliably preserve the environment: the child's environ
# is rebuilt (PATH, HOME, SHELL, USER, LOGNAME at minimum, and on some
# distributions much more), and CMESH_PASSPHRASE can go with it. The
# passphrase is the ONE variable that must survive — without it
# readPassphrase falls through to the interactive prompt, stdin is
# /dev/null, and the command fails instantly with EOF. A rebuild that
# fails on the OLDEST machines — the ones most likely to be the
# reinstalled ones — is exactly the failure this script exists to
# remove.
#
# So the su branch is chosen by MEASUREMENT, not by assumption: a probe
# process runs under each su variant and reports whether the variable
# survived. The first variant that preserves it is used. The probe
# prints WHETHER the variable is set, never what it is.
#
# If NO variant preserves it, the script refuses with the manual steps.
# It does not run a command that is guaranteed to fail, and it does not
# put the passphrase on a command line to make it work.
# ---------------------------------------------------------------------------
SU_VARIANT=""

# su_probe asks the candidate variant whether the environment survived.
#
# The single quotes are LOAD-BEARING: the expansion has to happen in the
# CHILD shell, under su, where the environment either survived or did
# not. Expanding it here would test the parent's environment and always
# answer "yes".
# shellcheck disable=SC2016
su_probe() {
	local variant="$1" out=""
	case "$variant" in
	plain) out="$(su -s /bin/sh "$SERVICE_USER" -c 'test -n "${CMESH_PASSPHRASE:-}" && echo yes' </dev/null 2>/dev/null || true)" ;;
	p) out="$(su -p -s /bin/sh "$SERVICE_USER" -c 'test -n "${CMESH_PASSPHRASE:-}" && echo yes' </dev/null 2>/dev/null || true)" ;;
	m) out="$(su -m -s /bin/sh "$SERVICE_USER" -c 'test -n "${CMESH_PASSPHRASE:-}" && echo yes' </dev/null 2>/dev/null || true)" ;;
	esac
	[ "$out" = "yes" ]
}

# su_select picks the first su variant that preserves the environment.
su_select() {
	local v
	for v in plain p m; do
		if su_probe "$v"; then
			SU_VARIANT="$v"
			return 0
		fi
	done
	return 1
}

su_run() {
	local cmd="$1"
	case "$SU_VARIANT" in
	plain) su -s /bin/sh "$SERVICE_USER" -c "$cmd" </dev/null ;;
	p) su -p -s /bin/sh "$SERVICE_USER" -c "$cmd" </dev/null ;;
	m) su -m -s /bin/sh "$SERVICE_USER" -c "$cmd" </dev/null ;;
	*) return 1 ;;
	esac
}

# manual_identity_steps prints what a human has to run by hand when this
# machine cannot run the CLI as the service user.
manual_identity_steps() {
	cat >&2 <<EOF
  This machine has no runuser, no setpriv, and no su variant here that
  preserves the environment — so the passphrase cannot reach the CLI
  without landing in a command line, which this script will not do.

  Run these three commands BY HAND as the service user, with the
  passphrase in the ENVIRONMENT (not in the command line):

    export CMESH_PASSPHRASE='<the passphrase from $ENV_PATH>'
    sudo -u $SERVICE_USER $BIN_PATH init --state $STATE_DIR
    sudo -u $SERVICE_USER $BIN_PATH account create --state $STATE_DIR
    sudo -u $SERVICE_USER $BIN_PATH account device-add --state $STATE_DIR

  Then run this script again: it will find the identity and leave it
  alone.

  (Installing util-linux's runuser, or shadow's setpriv, makes this
  script do it on its own next time.)
EOF
}

run_as_service_user() {
	# stdin is closed: the passphrase comes from the ENVIRONMENT, and a
	# service rebuild must never block waiting for a human to type one.
	#
	# CMESH_FORCE_SU=1 skips runuser/setpriv and goes straight to the su
	# branch. It exists so the offline regression can cover the oldest
	# branch on a machine that happens to have the newer tools.
	if [ "${CMESH_FORCE_SU:-0}" != "1" ]; then
		if have runuser; then
			runuser -u "$SERVICE_USER" -- "$@" </dev/null
			return 0
		fi
		if have setpriv; then
			setpriv --reuid="$SERVICE_USER" --regid="$SERVICE_GROUP" --init-groups -- "$@" </dev/null
			return 0
		fi
	fi
	if have su; then
		if su_select; then
			local cmd
			cmd="$(printf '%q ' "$@")"
			su_run "$cmd"
			return 0
		fi
		warn "no su variant on this machine preserves the environment (tried: plain, -p, -m)"
	fi
	manual_identity_steps
	die "cannot run the CLI as $SERVICE_USER on this machine"
}

# ---------------------------------------------------------------------------
say "=== the anchor rebuild ==="
say "state   $STATE_DIR"
say "config  $ETC_DIR"
say "binary  $BIN_PATH"
say "unit    $UNIT_PATH"

if [ "$(id -u)" != "0" ]; then
	die "this script must run as root"
fi

# ---------------------------------------------------------------------------
# 1. THE SERVICE ACCOUNT. Created only when it is missing.
# ---------------------------------------------------------------------------
if getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
	say "the group $SERVICE_GROUP already exists"
else
	if have groupadd; then
		groupadd --system "$SERVICE_GROUP"
		say "created the group $SERVICE_GROUP"
	else
		die "groupadd is not available and the group $SERVICE_GROUP does not exist"
	fi
fi

if id -u "$SERVICE_USER" >/dev/null 2>&1; then
	say "the user $SERVICE_USER already exists (uid $(id -u "$SERVICE_USER"))"
else
	if have useradd; then
		useradd --system --gid "$SERVICE_GROUP" --home-dir "$STATE_DIR" \
			--shell /usr/sbin/nologin --no-create-home "$SERVICE_USER"
		say "created the user $SERVICE_USER (uid $(id -u "$SERVICE_USER"))"
	else
		die "useradd is not available and the user $SERVICE_USER does not exist"
	fi
fi

# ---------------------------------------------------------------------------
# 2. THE DIRECTORIES. Owner and mode follow the deployed layout.
#
#    /var/lib/cmesh   cmesh:cmesh 0750   the identity and the roster
#    /etc/cmesh       root:cmesh   0750   the environment file and the token
# ---------------------------------------------------------------------------
install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$STATE_DIR"
install -d -m 0750 -o root -g "$SERVICE_GROUP" "$ETC_DIR"
say "the state and configuration directories are in place"

# ---------------------------------------------------------------------------
# 3. THE BINARY. Written to a staging file in the SAME directory and then
#    renamed, so a reader never sees a half-written executable. An
#    identical binary is left alone; a different one replaces the
#    installed one and the previous one is kept as cmesh.bak.<epoch>.
# ---------------------------------------------------------------------------
if [ -z "$BIN_SRC" ]; then
	say "no source binary given: keeping the installed one"
elif [ ! -f "$BIN_SRC" ]; then
	die "the source binary $BIN_SRC does not exist"
elif [ -f "$BIN_PATH" ] && cmp -s "$BIN_SRC" "$BIN_PATH"; then
	say "the installed binary is identical: not replacing it"
else
	install -d -m 0755 -o root -g root "$(dirname "$BIN_PATH")"
	cp "$BIN_SRC" "$BIN_PATH.new"
	chmod 0755 "$BIN_PATH.new"
	if [ -f "$BIN_PATH" ]; then
		backup="$BIN_PATH.bak.$(date +%s)"
		n=1
		while [ -e "$backup" ]; do
			backup="$BIN_PATH.bak.$(date +%s).$n"
			n=$((n + 1))
		done
		mv "$BIN_PATH" "$backup"
		say "the previous binary was kept at $backup"
	fi
	mv "$BIN_PATH.new" "$BIN_PATH"
	say "installed $BIN_PATH"
fi

# ---------------------------------------------------------------------------
# 4. THE ENVIRONMENT FILE.
#
#    The script does not fabricate it: the listen address and the
#    announced endpoint are facts about this deployment, so the file must
#    already be there or must be handed in. What the script owns is the
#    OWNER and the MODE: root:cmesh, 0600 — it carries a passphrase.
# ---------------------------------------------------------------------------
if [ -n "$ENV_SRC" ]; then
	if [ ! -f "$ENV_SRC" ]; then
		die "the environment file source $ENV_SRC does not exist"
	fi
	if [ -f "$ENV_PATH" ]; then
		# ★ THE SOURCE IS A BOOTSTRAP, NOT A SYNC.
		#
		# The installed file is the LIVE configuration of this anchor: it
		# holds the passphrase AND the account public key, and the public
		# key is filled in by this script from the account on the disk.
		# Overwriting it on every run would wipe that key on the second
		# run, and an anchor whose CMESH_ACCOUNT_PUB does not match its
		# account refuses every enrollment while looking healthy.
		#
		# So the source is installed only when there is nothing there.
		# To change the live configuration, edit it on the host.
		chown root:"$SERVICE_GROUP" "$ENV_PATH"
		chmod 0600 "$ENV_PATH"
		say "the environment file is already there: its content was not touched"
		if ! cmp -s "$ENV_SRC" "$ENV_PATH"; then
			warn "the source $ENV_SRC and the installed $ENV_PATH differ."
			warn "The installed one was kept: it is the live configuration of this anchor."
		fi
	else
		install -m 0600 -o root -g "$SERVICE_GROUP" "$ENV_SRC" "$ENV_PATH"
		say "installed the environment file from $ENV_SRC"
	fi
elif [ -f "$ENV_PATH" ]; then
	chown root:"$SERVICE_GROUP" "$ENV_PATH"
	chmod 0600 "$ENV_PATH"
	say "the environment file is already there: its content was not touched"
else
	die "there is no environment file at $ENV_PATH and CMESH_DISCOVERY_ENV_SOURCE is not set.
     The file carries CMESH_STATE, CMESH_ADDR, CMESH_ANNOUNCE and CMESH_PASSPHRASE,
     and those are facts about this deployment: copy the old one onto the host and
     point CMESH_DISCOVERY_ENV_SOURCE at it, or put it at $ENV_PATH by hand."
fi

# ---------------------------------------------------------------------------
# 5. THE IDENTITY. Created ONLY when it is missing.
#
#    account.json is the criterion: `cmesh account create` is the only
#    thing that writes it, and it is what makes this host the ACCOUNT
#    OWNER (a member device never has one). An existing account.json is
#    not touched — not re-encrypted, not re-created — because a new
#    account key invalidates every certificate the network holds.
#
#    The sequence is the one in deploy/MESH-NET.md: init, account create,
#    account device-add. The last step signs THIS machine's device into
#    the account, which is what puts the anchor on its own roster.
# ---------------------------------------------------------------------------
if [ -f "$STATE_DIR/account.json" ]; then
	say "the account identity is already there: not touching it"
elif [ "$SKIP_IDENTITY" = "1" ]; then
	warn "CMESH_SKIP_IDENTITY=1: the anchor gets NO identity."
	warn "The node will not start without a device identity (serve-discovery refuses"
	warn "to start without device.json), and no member can enroll without the"
	warn "account public key."
else
	PASSPHRASE="$(env_get "$ENV_PATH" CMESH_PASSPHRASE)"
	if [ -z "$PASSPHRASE" ]; then
		PASSPHRASE="${CMESH_PASSPHRASE:-}"
		[ -n "$PASSPHRASE" ] && say "the passphrase came from the environment, not from the file"
	fi
	if [ -z "$PASSPHRASE" ]; then
		die "no CMESH_PASSPHRASE in $ENV_PATH and none in the environment.
     The passphrase unlocks the device identity the service reads at start
     (serve-discovery loads device.json), so it has to be the same value
     the service will use."
	fi
	# The passphrase goes into the ENVIRONMENT of the child processes
	# and nowhere else. It is never an argument and never printed.
	export CMESH_PASSPHRASE="$PASSPHRASE"

	if [ ! -f "$STATE_DIR/device.json" ]; then
		run_as_service_user "$BIN_PATH" init --state "$STATE_DIR"
		say "created the device identity"
	else
		say "the device identity is already there: not touching it"
	fi
	run_as_service_user "$BIN_PATH" account create --state "$STATE_DIR"
	say "created the account key"
	run_as_service_user "$BIN_PATH" account device-add --state "$STATE_DIR"
	say "signed this device into the account"

	IDENTITY_CREATED=1
	unset PASSPHRASE
fi

# ---------------------------------------------------------------------------
# 6. THE ACCOUNT PUBLIC KEY IN THE ENVIRONMENT FILE.
#
#    The unit passes ${CMESH_ACCOUNT_PUB} to serve-discovery. Without it
#    the enrollment endpoint is NOT installed at all — the node serves,
#    the panel works, and not one member can ever join. A public key that
#    does not match the account on disk is the same failure, silently.
#
#    So the key is checked against the account that is actually on the
#    host, and corrected when the script itself just regenerated it.
# ---------------------------------------------------------------------------
ACCOUNT_PUB=""
if [ -f "$STATE_DIR/account.json" ]; then
	if [ -n "${CMESH_PASSPHRASE:-}" ] || [ -n "$(env_get "$ENV_PATH" CMESH_PASSPHRASE)" ]; then
		if [ -z "${CMESH_PASSPHRASE:-}" ]; then
			account_pass="$(env_get "$ENV_PATH" CMESH_PASSPHRASE)"
			export CMESH_PASSPHRASE="$account_pass"
			unset account_pass
		fi
		ACCOUNT_PUB="$("$BIN_PATH" account show --state "$STATE_DIR" 2>/dev/null |
			awk '/^pubkey\.b64 /{print $2; exit}')"
	fi
fi

if [ -n "$ACCOUNT_PUB" ]; then
	INSTALLED_PUB="$(env_get "$ENV_PATH" CMESH_ACCOUNT_PUB)"
	if [ "$INSTALLED_PUB" = "$ACCOUNT_PUB" ]; then
		say "the account public key in the environment file matches the account on disk"
	elif [ "$IDENTITY_CREATED" = "1" ]; then
		backup="$ENV_PATH.bak.$(date +%s)"
		n=1
		while [ -e "$backup" ]; do
			backup="$ENV_PATH.bak.$(date +%s).$n"
			n=$((n + 1))
		done
		cp -p "$ENV_PATH" "$backup"
		env_set "$ENV_PATH" CMESH_ACCOUNT_PUB "$ACCOUNT_PUB"
		say "the account key was regenerated: CMESH_ACCOUNT_PUB in $ENV_PATH was updated"
		say "  the previous file was kept at $backup"
	else
		warn "CMESH_ACCOUNT_PUB in $ENV_PATH does NOT match the account key on this host."
		warn "The anchor will refuse every enrollment until they match. Not changed by"
		warn "this script: the account key on disk is the one that counts."
	fi
else
	warn "the account public key could not be read from $STATE_DIR/account.json"
	warn "(the account passphrase is needed for it). Check CMESH_ACCOUNT_PUB in $ENV_PATH by hand."
fi

# ---------------------------------------------------------------------------
# 7. THE PANEL TOKEN. Generated only when the file is missing, and never
#    printed: the data endpoint hands out the WHOLE roster.
# ---------------------------------------------------------------------------
if [ -f "$TOKEN_PATH" ]; then
	say "the panel token file is already there: not touching it"
else
	if have openssl; then
		token="$(openssl rand -hex 32)"
	else
		token="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
	fi
	if [ -z "$token" ]; then
		die "could not generate a panel token"
	fi
	umask 077
	printf '%s\n' "$token" >"$TOKEN_PATH.new"
	install -m 0640 -o root -g "$SERVICE_GROUP" "$TOKEN_PATH.new" "$TOKEN_PATH"
	rm -f "$TOKEN_PATH.new"
	unset token
	say "a panel token was generated and written to $TOKEN_PATH (its value is not printed)"
fi

# ---------------------------------------------------------------------------
# 8. THE UNIT AND SYSTEMD.
#
#    The unit file is installed either way — it is an inert file without
#    systemd. The systemctl steps are skipped when there is no systemd on
#    the machine, and the script says so instead of failing.
# ---------------------------------------------------------------------------
if [ ! -f "$UNIT_SRC" ]; then
	die "the unit file $UNIT_SRC does not exist"
fi

install -d -m 0755 -o root -g root "$UNIT_DIR"

if [ -f "$UNIT_PATH" ] && cmp -s "$UNIT_SRC" "$UNIT_PATH"; then
	say "the installed unit is identical: not replacing it"
else
	install -m 0644 -o root -g root "$UNIT_SRC" "$UNIT_PATH"
	UNIT_CHANGED=1
	say "installed $UNIT_PATH"
fi

if [ -d /run/systemd/system ] && have systemctl; then
	systemctl daemon-reload
	systemctl enable --now "$UNIT_NAME"
	if [ "$UNIT_CHANGED" = "1" ]; then
		systemctl restart "$UNIT_NAME"
		say "restarted $UNIT_NAME (the unit file changed)"
	fi
	sleep 1
	if systemctl is-active --quiet "$UNIT_NAME"; then
		say "$UNIT_NAME is active"
	else
		warn "$UNIT_NAME is NOT active. The journal says why: journalctl -u $UNIT_NAME -n 50"
	fi
else
	say "no systemd on this machine (/run/systemd/system is missing):"
	say "skipping daemon-reload, enable and the service start"
fi

# ---------------------------------------------------------------------------
# 9. THE SUMMARY.
# ---------------------------------------------------------------------------
if [ "$IDENTITY_CREATED" = "1" ]; then
	banner "THE ANCHOR IDENTITY WAS REBUILT — EVERY MEMBER MUST JOIN AGAIN"
	cat >&2 <<EOF
  A new account key and a new device identity were created in $STATE_DIR.

  The old ones are gone. Every member of the network has to be enrolled
  again against this new account, and the certificates the old account
  signed are worthless now. The roster in $STATE_DIR/members.json starts
  empty.

  If this host was reinstalled and the OLD account key still exists
  somewhere, restore it instead of creating a new one: the members would
  not have to rejoin.

  The panel token in $TOKEN_PATH was generated (or kept); its value is
  never printed by this script.
EOF
fi

say "=== done ==="
say "the anchor's identity, the binary, the unit and the token are in place"
say "read back the evidence with: systemctl status $UNIT_NAME · cmesh version"
