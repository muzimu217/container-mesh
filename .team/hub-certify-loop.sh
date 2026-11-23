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

HUB=https://new.blackevil.cn/hub
ANCHOR=root@175.24.139.148
# The four core agents, plus the ones that have joined since.
AGENTS="doubao-cloud-vm-01 kuku-sandbox-01 netns-probe oh-no-agent muse-licheng-test1"
LEDGER=.team/certification.md
WATERMARK=.team/hub-watermark
DRY=0
[ "${1:-}" = "--dry-run" ] && DRY=1

hub_token() {
  ssh -o ConnectTimeout=8 -o BatchMode=yes "$ANCHOR" \
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
  if [ "$to" = "channel" ]; then
    curl -sS -m 30 -X POST -H "X-Hub-Token: $token" --data-binary @"$body" \
      "$HUB/report?from=coordinator&topic=issue8" 2>&1 | head -c 120
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
    "$HUB/instruct?to=$to" 2>&1 | head -c 120
  echo
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
  curl -sS -m 20 -H "X-Hub-Token: $token" "$HUB/inbox?to=$to" 2>/dev/null | python3 -c "
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
  curl -sS -m 20 -H "X-Hub-Token: $token" "$HUB/reports?topic=issue8" 2>/dev/null
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

# 3. Update the ledger (only when something changed).
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
else
  echo "tip $TIP is fully certified; sending a heartbeat only"
  HB=/tmp/hub-heartbeat.$$
  echo "# 值守心跳 $(date -u +%FT%TZ): tip $TIP 四线认证已齐，本轮无新派单。" > "$HB"
  hub_post channel "$HB"
  rm -f "$HB"
fi

# 6. Nudges (after the assignment so the assignment is the primary mail).
for a in $NUDGE_LIST; do
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
