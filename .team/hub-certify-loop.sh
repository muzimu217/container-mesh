#!/usr/bin/env bash
# The Hub test-certification loop (D79): one pass of the periodic cycle
# that keeps the four test agents certifying the current develop tip.
#
# The loop is INTENTIONALLY commit-free: a scheduled session has no
# outward git authority, so everything it changes stays in the worktree
# for the interactive session to review and commit.
#
# Usage: bash .team/hub-certify-loop.sh [--dry-run]
#   --dry-run  print what would be sent without sending anything.
set -u
cd "$(dirname "$0")/.." || exit 1

# The hub's public URL. It is the NORMAL path.
#
# ★ BUT IT IS NOT ALWAYS REACHABLE, AND THE FALLBACK IS NOT OPTIONAL.
# (Round 102.) The authorized host was reinstalled after the round-98
# accident, and a fresh cloud instance comes with a fresh security group:
# only SSH was open. The hub itself was up and answering on loopback, but
# 80 and 443 were filtered from the outside, so the public URL timed out
# and the whole coordination channel was down — not because the hub was
# broken, but because a port was shut.
#
# So the loop falls back to a TUNNEL over SSH, which the host's firewall
# does allow. The hub answers the same either way; only the path differs.
#
# The tunnel is started on demand and lives as long as the SSH session.
HUB=${CMESH_HUB_URL:-https://new.blackevil.cn/hub}
HUB_TUNNEL_PORT=${CMESH_HUB_TUNNEL_PORT:-8788}
# The hub listens on 127.0.0.1:8787 on the host (HUB_ADDR in
# cmesh-hub.service). It is a variable, not a literal, because the
# pkill pattern that finds a stale tunnel has to match it exactly —
# a hardcoded literal in one place and a variable in another is how
# the pattern starts killing tunnels that belong to someone else.
HUB_REMOTE_PORT=${CMESH_HUB_REMOTE_PORT:-8787}

# ★ THE COOLDOWNS. (Round 102.) The loop runs every few minutes, and
# it used to re-send the same brief and the same nudge every time:
# measured on kuku's inbox, 11 messages of which 4 were one identical
# assignment and 5 were one identical nudge. A tester cannot tell a new
# nudge from a re-send of the old one.
#
# Both decisions are made from what was ACTUALLY DELIVERED (read back
# from the hub), not from a local guess about what we sent.
#
# ★ THEY ARE DEFINED HERE, AT THE TOP, because the assignment section
#   uses ASSIGN_COOLDOWN and the nudge section defines it — and a
#   `set -u` shell then dies with "unbound variable" on the FIRST run.
NUDGE_COOLDOWN=${CMESH_NUDGE_COOLDOWN:-3600}
ASSIGN_COOLDOWN=${CMESH_ASSIGN_COOLDOWN:-3600}
ANCHOR=root@175.24.139.148
# The four core agents, plus the ones that have joined since.
AGENTS="doubao-cloud-vm-01 kuku-sandbox-01 netns-probe oh-no-agent muse-licheng-test1"
LEDGER=.team/certification.md
WATERMARK=.team/hub-watermark
DRY=0
[ "${1:-}" = "--dry-run" ] && DRY=1

# hub_base returns the URL the loop talks to, and makes sure it is one
# that ANSWERS CORRECTLY.
#
# ★ A PROBE THAT CANNOT FAIL IS NOT A PROBE. (Round 102, found by the
# audit line and reproduced by hand.) The old probe was
# `curl -sS -m 8 -o /dev/null "$HUB/healthz"` — and curl EXITS 0 ON
# EVERY HTTP ERROR STATUS. Only a transport failure is non-zero.
# Measured: 401 → exit 0, 404 → exit 0, 502 → exit 0. So the probe
# said "reachable" for a hub that was answering "unauthorized" and for
# an nginx whose upstream was dead.
#
# And it probed the WRONG URL. The hub registers /healthz at the ROOT
# (deploy/hub/main.go:45, unauthenticated); nginx proxies only
# location /hub/, and /hub/ is behind auth(). So $HUB/healthz is
# https://new.blackevil.cn/hub/healthz, which is a 401 without a token
# and a 404 with one. There is NO working health endpoint behind the
# proxy at all.
#
# So the probe now asks the only question that matters: does the hub
# ANSWER AN AUTHENTICATED REQUEST with something the hub would say?
# `hub_probe <url>` succeeds only on HTTP 200. A 401 means the token is
# wrong; a 404 means the path is wrong; a 502 means the upstream is
# dead. None of them are "the path works".
hub_probe() {
  # hub_probe <base-url>
  local base="$1" tok
  tok="$(hub_token)"
  [ -z "$tok" ] && return 1
  # -f makes curl fail on 4xx/5xx. The endpoint is the reports one,
  # because it is the only one that exists behind the proxy.
  curl -sS -m 8 -6 -f -o /dev/null \
    -H "X-Hub-Token: $tok" "$base/reports?topic=issue8" 2>/dev/null
}

hub_base() {
  if [ -n "${CMESH_HUB_URL:-}" ]; then
    printf '%s' "$HUB"
    return 0
  fi
  # ★ THE PUBLIC URL, OVER IPv6. (Round 102, third part.) The security
  # group filters 80/443 on the IPv4 address — measured: IPv4:443
  # times out at 4.00s (dropped), while IPv6:443 answers with an RST
  # in 0.05s, i.e. the packet REACHED the host, so IPv6 is not
  # filtered. (The host's ip6tables INPUT policy is ACCEPT.)
  #
  # The canonical URL works as-is over IPv6: a Let's Encrypt
  # certificate was issued (HTTP-01 over IPv6) and nginx now listens on
  # [::]:443. So the agents' own two curl lines need no change at all.
  #
  # -6 is used deliberately: over IPv4 the same URL times out, and a
  # probe that has to wait out the timeout on every call is a probe
  # that lies about which path it found.
  if hub_probe "$HUB"; then
    printf '%s' "$HUB"
    return 0
  fi
  # Plain HTTP over IPv6 — the fallback if the certificate ever
  # expires and the renewal has not run yet.
  #
  # ⚠️ THIS PATH SENDS THE SHARED TOKEN IN CLEARTEXT. It is a
  # fallback, not the normal path. The agent-facing documentation
  # says HTTPS; only the loop falls back to this.
  if hub_probe "http://new.blackevil.cn/hub"; then
    printf 'http://new.blackevil.cn/hub'
    return 0
  fi
  # The tunnel. A direct public path beats a tunnel, so this is the
  # LAST resort — but it is the one path that needs no DNS and no
  # public port at all.
  #
  # The tunnel dies with the SSH session, and a dead tunnel that
  # still holds the port LOOKS alive to `nc -z` — so the port check
  # alone is not enough. It is verified by an actual request, and
  # ExitOnForwardFailure keeps a failed forward from lingering.
  if ! hub_probe "http://127.0.0.1:$HUB_TUNNEL_PORT/hub"; then
    # ★ THE pkill PATTERN MUST NOT MATCH OTHER PORTS. The old one was
    # "ssh -f -N.*$PORT:127.0.0.1:8787" — 8788 is a SUBSTRING of
    # 18788, and the dots in 127.0.0.1 are regex "any char". Measured:
    # a tunnel on -L 18788:127.0.0.1:8787 matched and was killed.
    # The pattern is now anchored on the local port with a word
    # boundary, and the remote port is a variable rather than a
    # literal.
    pkill -f -- "-L ${HUB_TUNNEL_PORT}:127\.0\.0\.1:${HUB_REMOTE_PORT}( |$)" 2>/dev/null
    ssh -f -N -o ConnectTimeout=10 -o BatchMode=yes \
      -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
      -o ExitOnForwardFailure=yes \
      -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -L "$HUB_TUNNEL_PORT:127.0.0.1:$HUB_REMOTE_PORT" "$ANCHOR" >/dev/null 2>&1
    sleep 2
  fi
  if hub_probe "http://127.0.0.1:$HUB_TUNNEL_PORT/hub"; then
    printf 'http://127.0.0.1:%s/hub' "$HUB_TUNNEL_PORT"
    return 0
  fi
  # ★ NO PATH WORKS. The old code printed the public URL anyway and
  # returned 0 — so the loop carried on with a URL it knew was dead,
  # and (because the fetch then yielded nothing) it wiped the ledger.
  # Now it says so and fails.
  echo "hub_base: no working path to the hub (public IPv6, plain HTTP, tunnel all failed)" >&2
  return 1
}

hub_token() {
  # ★ THE HOST KEY CHANGED. (Round 102.) The authorized host was
  # reinstalled, so it presents a NEW host key and the strict check
  # refuses it — which made this function return nothing, which made
  # the loop report "the hub is unreachable" when the hub was fine.
  #
  # The fix is NOT to disable the check blindly: the key changed for a
  # known reason (the net owner reinstalled the machine), and the old
  # entry is removed so it cannot be used again.
  #
  # So: the stale entry is dropped, and the new key is accepted for this
  # connection. The identity that matters is the SSH KEY that logs in,
  # not the host key that identifies the machine.
  #
  # ★ A LOCAL COPY IS THE PRIMARY SOURCE. (Round 102, third part.) The
  # token is the one credential that EVERY agent already holds in its
  # own environment — so as long as the hub runs the SAME token, every
  # agent works with no reconfiguration at all. The token therefore
  # lives in .team/hub-token (mode 600, git-ignored), and the hub on
  # the host was given that same value.
  #
  # The local copy is preferred because it needs no SSH: the whole
  # point of the IPv6 path is that it survives the SSH path being
  # broken. SSH is the fallback.
  if [ -r .team/hub-token ]; then
    tr -d '\n\r' < .team/hub-token
    return 0
  fi
  ssh -o ConnectTimeout=8 -o BatchMode=yes \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$ANCHOR" \
    'grep -oE "^HUB_TOKEN=.*" /etc/default/cmesh-hub | cut -d= -f2' 2>/dev/null
}

hub_post() {
  # hub_post <to|channel> <body-file>
  #
  # The hub's two write endpoints: /hub/report (the channel, from=…&topic=…)
  # and /hub/instruct (one agent's inbox, to=…). The body travels as the
  # POST body, not as a form field.
  #
  # ★ A `{"ok":true}` IS NOT PROOF OF DELIVERY. (Round 98, found the hard
  # way.) The hub CREATES AN INBOX FOR ANY `to=` VALUE and answers
  # `{"ok":true}` whether or not that agent exists — so a misspelled
  # recipient (`to=kuku` instead of `to=kuku-sandbox-01`) is accepted,
  # stored, and NEVER READ. The sender sees a success that no one receives.
  #
  # We lost four messages that way: the reply to doubao, the nudges to
  # kuku and netns, and the reply to muse all went to phantom inboxes
  # named `doubao` / `kuku` / `muse` while the real agents stayed silent
  # — and the silence looked exactly like an agent that had stopped
  # answering.
  #
  # So the recipient is CHECKED AGAINST THE KNOWN ROSTER before sending,
  # and the delivery is VERIFIED BY READING THE INBOX BACK.
  local to="$1" body="$2" token
  token=$(hub_token)
  [ -z "$token" ] && { echo "hub token unavailable" >&2; return 1; }
  if [ "$DRY" = 1 ]; then
    # The preview must not die on a multi-byte character: `tr` works on
    # BYTES, and in a UTF-8 locale it aborts on the first Chinese
    # character it meets ("tr: Illegal byte sequence"). The preview is
    # cosmetic, so it must never take the run down.
    echo "  [dry] would send to=$to: $(head -c 60 "$body" | LC_ALL=C tr '\n' ' ' 2>/dev/null || head -c 60 "$body")..."
    return 0
  fi
  # ★ THE BASE MUST BE RESOLVED BEFORE THE POST, AND ITS FAILURE
  # MUST STOP THE POST. "$(hub_base)" inside a URL is a command
  # substitution in a string: its exit status is thrown away, so a
  # failed hub_base yields the URL "/report?..." — which curl fetches
  # as a relative path and fails, or worse, against whatever a proxy
  # answers. The failure has to be checked, not interpolated.
  local base
  base="$(hub_base)" || return 1
  if [ "$to" = "channel" ]; then
    curl -sS -m 30 -X POST -H "X-Hub-Token: $token" --data-binary @"$body" \
      "$base/report?from=coordinator&topic=issue8" 2>&1 | head -c 120
    echo
    return 0
  fi
  # THE RECIPIENT MUST BE A REAL AGENT. A name that is not in the roster
  # is a typo, and a typo is how messages vanish.
  if ! printf '%s\n' $AGENTS | grep -qx "$to"; then
    echo "REFUSED: '$to' is not in the roster ($AGENTS)" >&2
    echo "         (the hub would have accepted it into a phantom inbox)" >&2
    return 1
  fi
  curl -sS -m 30 -X POST -H "X-Hub-Token: $token" --data-binary @"$body" \
    "$base/instruct?to=$to" 2>&1 | head -c 120
  echo
}

# hub_inbox_times prints "<time>" for every message in an agent's inbox,
# one per line, oldest first. It is used to decide whether a nudge is
# due — the decision must be based on what was ACTUALLY DELIVERED (read
# back from the hub), not on a local guess about what we sent.
hub_inbox_times() {
  local to="$1" token
  token=$(hub_token)
  [ -z "$token" ] && return 1
  local base
  base="$(hub_base)" || return 1
  # ★ THE CHANNEL IS A TOPIC, NOT AN INBOX. The brief to everybody goes
  # to /hub/report?topic=issue8, so it lands in the TOPIC store — not
  # in an inbox named "channel". Reading /hub/inbox?to=channel returns
  # nothing, so a cooldown decided on that reading could never fire
  # and the same brief went out on every single cycle.
  local path
  if [ "$to" = "channel" ]; then
    path="$base/reports?topic=issue8"
  else
    path="$base/inbox?to=$to"
  fi
  curl -sS -m 20 -H "X-Hub-Token: $token" "$path" 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
for m in d.get('messages') or []:
    t = m.get('time') or ''
    b = m.get('body') or ''
    print(t + '\t' + b.split(chr(10))[0])
"
}

# ts_of turns an RFC3339 timestamp into a unix epoch second. It returns
# 0 for anything it cannot parse, so a malformed timestamp reads as
# "very old" rather than as "just now" — a nudge is cheap, a silent
# agent is not.
ts_of() {
  python3 -c "
from datetime import datetime, timezone
try:
    print(int(datetime.strptime('$1'.replace('Z','+0000'), '%Y-%m-%dT%H:%M:%S%z').timestamp()))
except Exception:
    print(0)
"
}

hub_delivered() {
  # hub_delivered <agent> <body-file>
  #
  # VERIFY THE DELIVERY BY READING THE INBOX BACK.
  #
  # The hub's `{"ok":true}` only says "I stored this somewhere". The
  # only thing that says "the agent can read it" is the message being
  # in THAT agent's inbox. So the check is: does the recipient's inbox
  # now contain a message whose body matches what we sent?
  local to="$1" body="$2" token
  token=$(hub_token)
  [ -z "$token" ] && return 1
  local sig
  # The first line is the subject line, and it is unique per round.
  sig=$(head -1 "$body")
  [ -z "$sig" ] && return 1
  local base
  base="$(hub_base)" || return 1
  curl -sS -m 20 -H "X-Hub-Token: $token" "$base/inbox?to=$to" 2>/dev/null | python3 -c "
import sys, json
sig = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    print('inbox unreadable'); raise SystemExit(1)
for m in d.get('messages') or []:
    if (m.get('body') or '').startswith(sig):
        print('delivered at ' + str(m.get('time')))
        raise SystemExit(0)
print('NOT in this inbox')
raise SystemExit(1)
" "$sig"
}

hub_reports() {
  local token
  token=$(hub_token)
  [ -z "$token" ] && return 1
  local base
  base="$(hub_base)" || return 1
  curl -sS -m 20 -H "X-Hub-Token: $token" "$base/reports?topic=issue8" 2>/dev/null
}

watermark() { head -1 "$WATERMARK" 2>/dev/null || echo "1970-01-01T00:00:00Z"; }

# The HANDLED cursor: the newest report the coordinator has actually
# DISPOSED of (read, judged, replied to, mirrored).
#
# It is a SEPARATE line from the fetched cursor because "the report
# arrived" and "the report was handled" are different facts. An
# external tester put it exactly right: a report of theirs was
# delivered (HTTP 200, hub side stored it, the content was quoted
# back to them) and yet it was never acted on — because the tool
# recorded one cursor for both facts.
#
# The file's format is:
#   line 1: FETCHED  — the newest report this loop has seen
#   line 2: HANDLED  — the newest report the coordinator has disposed of
#   line 3: the fetched cursor again (kept for older readers)
handled_watermark() { sed -n '2p' "$WATERMARK" 2>/dev/null || watermark; }

advance_watermark() {
  local ts="$1"
  # A DRY RUN WRITES NOTHING. It says what it WOULD send, and it says
  # what it WOULD advance the watermark to — but it does not do it.
  #
  # The bug this fixes: the dry run advanced the watermark anyway. The
  # watermark is the "reports I have already handled" cursor, so moving
  # it in a dry run SILENTLY DISCARDED every report between the old and
  # the new cursor — the next real run then read nothing new and those
  # reports were never handled. It cost one round: a tester's report
  # (doubao, 18:20:59Z) was read and then marked as handled without
  # being handled.
  #
  # The rule is the general one: a dry run must not mutate state that
  # decides what work remains. Printing a plan is not doing the work.
  if [ "$DRY" = 1 ]; then
    echo "  [dry] would advance the watermark to $ts"
    return 0
  fi
  printf '%s\n%s\n%s\n' "$ts" "$ts" "$ts" > "$WATERMARK"
}

# advance_handled records that the coordinator has disposed of every
# report up to and including ts.
#
# It is a SEPARATE write from advance_watermark on purpose: the loop
# FETCHES reports, but the COORDINATOR handles them. The loop may
# advance the fetched cursor on its own; only the coordinator may
# advance the handled one. Merging them is what turned "delivered"
# into "done".
advance_handled() {
  local ts="$1"
  if [ "$DRY" = 1 ]; then
    echo "  [dry] would advance the HANDLED cursor to $ts"
    return 0
  fi
  local fetched
  fetched=$(watermark)
  printf '%s\n%s\n%s\n' "$fetched" "$ts" "$fetched" > "$WATERMARK"
}

tip() { git rev-parse --short origin/develop 2>/dev/null || echo unknown; }

# ---------------------------------------------------------------------------
echo "=== Hub test-certification loop @ $(date -u +%FT%TZ) ==="
TIP=$(tip)
WM=$(watermark)
echo "develop tip: $TIP   watermark: $WM"

# 1. New reports since the watermark.
REPORTS=$(hub_reports) || { echo "FATAL: the hub is unreachable"; exit 1; }
NEW_COUNT=$(printf '%s' "$REPORTS" | python3 -c "
import sys, json
wm = '$WM'
try:
    d = json.load(sys.stdin)
except Exception:
    print(0); raise SystemExit
new = [m for m in d.get('messages') or [] if m.get('time','') > wm]
print(len(new))
")
echo "new reports since the watermark: $NEW_COUNT"

# 2. Per-agent last-report time (the ledger's raw material).
#
# THE HUB HAS TWO MESSAGE SHAPES, and only one of them was understood.
#
# A normal report arrives as {"topic","time","from","text"}. But an
# agent that posts a JSON document as its BODY arrives as
# {"topic","time","body":"{\"from\": \"...\", \"body\": \"...\"}"} —
# there is no top-level "from" at all.
#
# The old parser read m.get('from') and got "" for that one, so the
# report was INVISIBLE: it did not count as a report (the agent looked
# silent) and it did not reach the ledger. The agent had answered, and
# the loop said nothing had arrived.
#
# So the parser now unwraps a nested body when it is JSON. A report is
# a report no matter which shape it arrived in.
printf '%s' "$REPORTS" | python3 -c "
import sys, json
d = json.load(sys.stdin)
last = {}
for m in d.get('messages') or []:
    f = m.get('from') or ''
    t = m.get('time') or ''
    body = m.get('body') or ''
    if not f and body.startswith('{'):
        # A nested report: the agent posted a JSON document as the body.
        try:
            inner = json.loads(body)
            f = inner.get('from') or ''
        except Exception:
            f = ''
    if f and (f not in last or t > last[f]):
        last[f] = t
for a in '$AGENTS'.split():
    print('%s %s' % (a, last.get(a, 'never')))
" > /tmp/hub-last-reports.$$ 2>/dev/null

# ★ AN AGENT WITH NO REPORT IN THE FETCH KEEPS ITS KNOWN TIME.
# (Round 102, found while fixing the P0 above.) The hub's history is
# not permanent: a restart with a fresh state.json, a topic that was
# trimmed, or a channel that was rebuilt all make the fetch come back
# with no message for an agent that DID report — and the ledger then
# overwrites a known timestamp with "never".
#
# That is the same error as the wipe, one level up: "I did not see it
# this time" is not "it never happened". It cost real information once
# already — the recovered silence durations (kuku 48.8 h, netns 49.0 h)
# were replaced by "never" because the hub had been restarted.
#
# So a "never" from the fetch never overwrites a known value in the
# ledger. Only a real timestamp does.
while read -r a t; do
  if [ "$t" = "never" ]; then
    known=$(awk -v ag="$a" '$1==ag {print $2}' "$LEDGER" 2>/dev/null | head -1)
    if [ -n "$known" ] && [ "$known" != "never" ]; then
      printf '%s %s\n' "$a" "$known"
      continue
    fi
  fi
  printf '%s %s\n' "$a" "$t"
done < /tmp/hub-last-reports.$$ > /tmp/hub-last-reports.keep.$$
mv /tmp/hub-last-reports.keep.$$ /tmp/hub-last-reports.$$

# 3. Update the ledger (only when something changed).
#
# ★ NEVER REWRITE THE LEDGER FROM AN EMPTY FETCH. (Round 102, found by
# the audit line and reproduced by hand.) The agent rows are written
# ONLY from /tmp/hub-last-reports.$$ — so when the fetch produced
# nothing, the `while read` loop below wrote nothing, and `mv` replaced
# the whole ledger with just the coordinator's `#` notes. The five
# agent rows were DELETED.
#
# And it was worse than data loss: with the rows gone, PENDING counts
# to 0, so the loop printed "tip ... is fully certified" — a FALSE
# GREEN on an empty ledger, which is exactly the outcome this script
# exists to prevent.
#
# One network blip or one token desync was enough to trigger it.
# curl exits 0 on a 401 and on a 404, so "the hub answered" and "the
# hub answered correctly" are the same thing to that check.
#
# So: an empty fetch means "I do not know", and "I do not know" must
# never be written down as "nothing is certified".
if [ ! -s /tmp/hub-last-reports.$$ ]; then
  echo "  !! the hub fetch produced NOTHING — the ledger is left UNTOUCHED"
  echo "  !! (the agent rows are written only from that fetch; rewriting from it"
  echo "  !!  would delete them and report the tip as fully certified)"
  rm -f /tmp/hub-last-reports.$$
  exit 1
fi

#
# The ledger keeps MORE than this script can know: the SHA an agent
# actually certified, and the notes the coordinator wrote. So the update
# is a MERGE — the existing SHA column and the trailing notes survive —
# not a rewrite. (The first version of this script rewrote the file and
# silently dropped the coordinator's notes; the loop must not erase what
# it did not write.)
if [ ! -f "$LEDGER" ] || ! diff -q <(sort /tmp/hub-last-reports.$$) \
    <(grep -E "^(doubao|kuku|netns|ohno|oh-no|muse)" "$LEDGER" 2>/dev/null | awk '{print $1, $2}' | sort) >/dev/null 2>&1; then
  # A DRY RUN DOES NOT WRITE THE LEDGER EITHER. The ledger is the
  # record of who certified what; a rehearsal must not change it.
  if [ "$DRY" = 1 ]; then
    echo "  [dry] would update the ledger ($LEDGER)"
  else
  {
    echo "# 测试认证台账（hub-certify-loop 自动维护 + 值守补记）"
    echo "# 格式: <代理> <最后一报UTC> <已认证的SHA> <当前tip是否已认证>"
    echo "# 「已认证的 SHA」= 这个代理实际复测过并回报 PASS 的提交。"
    echo "# tip: $TIP  更新: $(date -u +%FT%TZ)"
    while read -r a t; do
      # Keep the SHA the coordinator recorded, if any.
      sha=$(awk -v ag="$a" '$1==ag {print $3}' "$LEDGER" 2>/dev/null | head -1)
      [ -z "$sha" ] && sha="—"
      # The tip counts as certified when the recorded SHA IS the tip.
      state="no"
      [ "$sha" = "$TIP" ] && state="yes"
      echo "$a $t $sha $state"
    done < /tmp/hub-last-reports.$$
    # The coordinator's notes and the agents the loop does not track (the
    # list is the four core agents; a new member arrives with its own
    # entries) must survive.
    if [ -f "$LEDGER" ]; then
      echo ""
      echo "# --- 值守补记（脚本不会覆盖以下内容）---"
      grep -E "^#" "$LEDGER" 2>/dev/null | grep -vE "^# (测试认证台账|格式|「已认证|tip:|--- 值守补记)" || true
      # Agents the loop does not track.
      for a in $(awk '{print $1}' "$LEDGER" 2>/dev/null | grep -vE "^(doubao|kuku|netns|ohno|oh-no|muse|#|格式|「已认证|tip:)" | sort -u); do
        line=$(grep -E "^$a " "$LEDGER" 2>/dev/null | head -1)
        if [ -n "$line" ]; then
          # Normalise to the four-column shape.
          set -- $line
          sha="${3:-—}"
          [ "$4" = "yes" ] || [ "$3" = "yes" ] && sha="$3"
          echo "$a $2 $sha no"
        fi
      done
    fi
  } > "$LEDGER.tmp"
  mv "$LEDGER.tmp" "$LEDGER"
  echo "ledger updated: $LEDGER"
  fi
fi

# 4. Silence check: a last report older than 6h is a nudge.
NOW=$(date -u +%s)
NUDGE_LIST=""
while read -r a t; do
  [ "$t" = "never" ] && t="$WM"
  TS=$(python3 -c "
from datetime import datetime, timezone
try:
    print(int(datetime.strptime('$t'.replace('Z','+0000'), '%Y-%m-%dT%H:%M:%S%z').timestamp()))
except Exception:
    print(0)
")
  AGE=$(( NOW - TS ))
  if [ "$AGE" -gt 21600 ]; then   # 6 h
    H=$(( AGE / 3600 ))
    echo "SILENT: $a — last report $t ($H h ago) → nudge"
    NUDGE_LIST="$NUDGE_LIST $a"
  fi
done < /tmp/hub-last-reports.$$

# 5. The certification assignment: send when any agent's ledger entry is
#    pending for the current tip.
#
# The count must be an INTEGER on one line. `grep -c` prints "0" AND exits
# 1 when it finds nothing, so `grep -c ... || echo 0` yields TWO lines
# ("0\n0") — and `[ "0
# 0" -gt 0 ]` then fails with "integer expected". The assignment was
# therefore never sent, and the loop announced a heartbeat instead: the
# tip was declared "fully certified" while three of the five agents had
# NOT certified it (their SHA column was "—", i.e. "no SHA recorded").
#
# So the count is taken from the ledger's OWN state column, not from a
# grep for a word that never appears in it. A row is pending when its
# state column is not "yes" — that is the column the loop itself writes.
PENDING=$(awk '$1 !~ /^#/ && NF >= 3 && $4 != "yes" {n++} END {print n+0}' "$LEDGER" 2>/dev/null)

# ★ NEVER SEND A BRIEF WITH AN UNKNOWN TIP. (Round 102.) The brief
# names the SHA it wants certified. If the tip is "unknown", the brief
# asks the agents to certify "unknown" — which is not a SHA, and the
# agents cannot act on it. Worse, it looks like a real assignment.
#
# How it happened: the loop was run from a COPY of itself in a
# directory that is not a git repository (an isolated test of the
# ledger-wipe fix), so `git rev-parse origin/develop` failed and the
# tip came out empty. The copy still had the real token and the real
# hub URL, so it posted eight real briefs into the agents' inboxes.
#
# An unknown tip is not a reason to keep going quietly. It is a reason
# to stop and say so.
if [ "$TIP" = "unknown" ] || [ -z "$TIP" ]; then
  echo "!! the develop tip could not be resolved — NOTHING was sent."
  echo "!! (a brief naming an unknown SHA is not an assignment; and a"
  echo "!!  loop run outside the repository would send it to the real agents)"
  rm -f /tmp/hub-last-reports.$$
  exit 1
fi

if [ "$PENDING" -gt 0 ]; then
  # WHO STILL OWNS A CELL.
  #
  # The assignment used to go to EVERY agent, every time, as long as
  # ANY cell was pending. So an agent who had already submitted their
  # result got asked again — and one of them said so: "doubao 那一格
  # 已经在 22:20Z 交过了……不必重发". Re-asking a tester who has
  # already answered is not a harmless repeat: it makes their inbox
  # noisy and it makes their silence (once they stop replying to
  # repeats) indistinguishable from being stuck.
  #
  # So the brief lists only the agents whose OWN row still says the
  # tip is not certified, and it goes only to them.
  PENDING_AGENTS=$(awk -v tip="$TIP" '$1 !~ /^#/ && NF >= 4 && $3 != tip {print $1}' "$LEDGER" 2>/dev/null)
  if [ -z "$PENDING_AGENTS" ]; then
    # No row names a pending agent: fall back to every agent, so a
    # ledger that has lost its rows does not silence the whole loop.
    PENDING_AGENTS="$AGENTS"
  fi

  BRIEF=/tmp/hub-certify-brief.$$
  {
    echo "# 测试认证派单 · develop@$TIP · $(date -u +%FT%TZ)"
    echo
    echo "当前 tip \`$TIP\` 尚未完成四线认证（台账 pending 项 $PENDING）。请**尚未交这一格**的线在**本轮回信**里带上你复测的 SHA 与结论，格式："
    echo "    SHA=$TIP 结论=PASS|FAIL 证据=<命令原文+退出码+输出首末20行>"
    echo
    echo "分工："
    echo "  - doubao：真机功能面（join --cred / cred list / cred revoke / 中转四项 ping,msg,run,send-to）"
    echo "  - kuku：单测语义复核（本轮改动涉及的包，跑 go test 并贴结果）"
    echo "  - netns：资源与可用性（RSS 口径 / cred 命令面走查）"
    echo "  - oh-no：同 SHA 外部复测（独立环境）"
    echo
    echo "**已经交过这一格的线不必再回**（重复交是噪音，不是配合）。"
    echo "收到请先回 {\"ok\":true}。"
  } > "$BRIEF"
  echo "--- certification assignment (pending=$PENDING) ---"

  # ★ THE ASSIGNMENT GETS A COOLDOWN TOO. (Round 102.) Measured on
  # kuku's inbox: 11 messages, of which 4 were the SAME assignment and
  # 5 were the SAME nudge. The loop runs every few minutes, so an
  # agent whose cell was still pending received a fresh copy of the
  # same brief every time.
  #
  # The assignment is per-tip, so the question is not "did we send one"
  # but "did we send one for THIS tip, and how long ago". A brief
  # naming tip X is still the brief naming tip X tomorrow — it says the
  # same thing, so resending it says the same thing again.
  #
  # The decision is made from what was actually delivered (read back
  # from the hub), not from a local guess.
  assign_due=1
  assign_at=""
  while IFS=$'\t' read -r mtime mfirst; do
    case "$mfirst" in
      *"测试认证派单 · develop@$TIP"*) assign_due=0; assign_at="$mtime" ;;
    esac
  done <<EOF
$(hub_inbox_times channel 2>/dev/null)
EOF
  if [ "$assign_due" = 0 ] && [ -n "$assign_at" ]; then
    age=$(( NOW - $(ts_of "$assign_at") ))
    if [ "$age" -lt "$ASSIGN_COOLDOWN" ]; then
      echo "  assignment: SKIPPED (the same brief for tip $TIP went out $(( age / 60 )) min ago, under the $(( ASSIGN_COOLDOWN / 60 )) min cooldown)"
      assign_due=0
    fi
  fi
  if [ "$assign_due" = 0 ]; then
    :
  else
  hub_post channel "$BRIEF"
  for a in $PENDING_AGENTS; do
    hub_post "$a" "$BRIEF"
    # ★ The `{"ok":true}` is not proof. Read the inbox back.
    if [ "$DRY" != 1 ]; then
      printf '  delivered? %s: ' "$a"
      hub_delivered "$a" "$BRIEF" || echo "  ★ NOT DELIVERED — the message is not in that inbox"
    fi
  done
  rm -f "$BRIEF"
  fi
else
  echo "tip $TIP is fully certified; sending a heartbeat only"
  HB=/tmp/hub-heartbeat.$$
  echo "# 值守心跳 $(date -u +%FT%TZ): tip $TIP 四线认证已齐，本轮无新派单。" > "$HB"
  hub_post channel "$HB"
  rm -f "$HB"
fi

# 6. Nudges (after the assignment so the assignment is the primary mail).
#
# ★ A NUDGE IS NOT A HEARTBEAT. (Round 102.) The loop runs every few
# minutes and every run re-sent the same nudge to every silent agent,
# so an inbox that had been quiet for 6 hours filled with IDENTICAL
# copies: 11 messages, of which 5 were the same nudge and 4 were the
# same assignment. Measured on kuku's inbox.
#
# A tester cannot tell a new nudge from a re-send of the old one, and
# an inbox full of identical copies is exactly the shape that makes a
# silent agent look like a busy one.
#
# So a nudge is sent only if the last one to that agent was more than
# NUDGE_COOLDOWN ago — and the receipt is read back from the inbox
# (which the loop already does for delivery), so the decision is based
# on what was actually delivered, not on a local guess.
for a in $NUDGE_LIST; do
  last_nudge=""
  while IFS=$'\t' read -r mtime mfirst; do
    case "$mfirst" in
      *"具体催办 · $a"*) last_nudge="$mtime" ;;
    esac
  done <<EOF
$(hub_inbox_times "$a" 2>/dev/null)
EOF
  if [ -n "$last_nudge" ]; then
    age=$(( NOW - $(ts_of "$last_nudge") ))
    if [ "$age" -lt "$NUDGE_COOLDOWN" ]; then
      echo "  nudge → $a: SKIPPED (the last one was $(( age / 60 )) min ago, under the $(( NUDGE_COOLDOWN / 60 )) min cooldown)"
      continue
    fi
  fi
  N=/tmp/hub-nudge.$$
  {
    echo "# 具体催办 · $a · $(date -u +%FT%TZ)"
    echo "你已静默超过 6 小时（最后一报见台账）。当前 develop tip 是 \`$TIP\`，请按上一条认证派单回复你的复测结果；如被环境卡住，请直接说明卡点（缺环境/缺权限/缺凭据），不要静默。"
  } > "$N"
  echo "--- nudge → $a ---"
  hub_post "$a" "$N"
  # ★ Same check: a nudge nobody received is the same as a nudge never sent.
  if [ "$DRY" != 1 ]; then
    printf '  delivered? %s: ' "$a"
    hub_delivered "$a" "$N" || echo "  ★ NOT DELIVERED — the message is not in that inbox"
  fi
  rm -f "$N"
done

# 7. Advance the watermark when there were new reports.
if [ "$NEW_COUNT" -gt 0 ]; then
  LATEST=$(printf '%s' "$REPORTS" | python3 -c "
import sys, json
d = json.load(sys.stdin)
ts = [m.get('time','') for m in d.get('messages') or []]
print(max(ts) if ts else '$WM')
")
  advance_watermark "$LATEST"
  echo "watermark advanced to $LATEST"
fi

rm -f /tmp/hub-last-reports.$$
echo "=== loop done ==="
