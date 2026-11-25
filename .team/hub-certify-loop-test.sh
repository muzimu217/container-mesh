#!/usr/bin/env bash
# The decisions of the coordination loop must be testable without a live
# hub and a live set of agent inboxes. This drives the pure functions.
#
# Usage: bash .team/hub-certify-loop-test.sh
# Exits non-zero on the first failure.
#
# ★ The path can be pointed at a MUTANT copy of the loop
#   (CMESH_LOOP_PATH=...) so the mutation check can prove this test
#   really pins the behaviour it claims to.

set -u

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOOP="${CMESH_LOOP_PATH:-$REPO/.team/hub-certify-loop.sh}"

if [ ! -f "$LOOP" ]; then
  echo "FAIL: the loop script is not at $LOOP"
  exit 1
fi

# Load the functions without running the loop.
CMESH_HUB_LIB=1 . "$LOOP"

FAILURES=0

fail() {
  echo "FAIL: $1"
  FAILURES=$((FAILURES + 1))
}

pass() {
  echo "ok: $1"
}

# ---------------------------------------------------------------------
# The assignment cooldown.
#
# ★ THE BUG THIS PINS (Round 107): the old inline logic set the flag to
# "do not send" as soon as it SAW a brief for the tip, and only turned
# it back on inside the "under the cooldown" branch — which does
# nothing once the cooldown has expired. So the brief for a tip went
# out exactly ONCE, ever, and the second skip was SILENT.
#
# Measured on the real hub: tip 65514aa's brief went out at 08:47Z;
# at 17:40Z the 1-hour cooldown had expired by more than nine hours,
# the brief was still never re-sent, and the loop printed nothing about
# it. A silent skip reads exactly like "the assignment was sent and
# nobody answered".
#
# ★ THE FUNCTION TAKES THE DELIVERED MESSAGES, NOT A PRE-PARSED TIME.
# The first version of this test passed a time in, and the mutation
# check then PROVED THE MUTANT SURVIVED: putting the old inverted
# logic back at the call site left the test green, because the
# function had not changed and the call site was where the defect
# lived. A test that passes while the defect is back certifies the
# fix. So the whole decision — including "find the brief for THIS
# tip among the delivered messages" — is inside the function, and the
# test drives exactly that.
# ---------------------------------------------------------------------

tip_wanted="abc1234"
cooldown=3600
now=1000000

# 1. Nothing was delivered at all -> send.
out=$(assignment_due "$tip_wanted" "$now" "$cooldown" "")
case "$out" in
  "due 1 "*) pass "no brief yet -> due" ;;
  *) fail "no brief yet should be due, got: $out" ;;
esac

# 2. A brief for THIS tip went out 10 minutes ago -> do NOT send.
recent="2026-11-24T17:30:00Z"
recent_epoch=$(ts_of "$recent")
delivered_recent="$recent	# 测试认证派单 · develop@$tip_wanted · 2026-11-24T17:30:00Z"
out=$(assignment_due "$tip_wanted" "$(( recent_epoch + 600 ))" "$cooldown" "$delivered_recent")
case "$out" in
  "due 0 "*) pass "a brief 10 min ago -> not due (under the cooldown)" ;;
  *) fail "a brief 10 min ago should NOT be due, got: $out" ;;
esac

# 3. ★ THE REGRESSION. A brief for THIS tip went out NINE HOURS ago and
#    the cooldown is one hour -> it MUST be due again. The old code
#    skipped it here, silently.
old="2026-11-24T08:47:00Z"
old_epoch=$(ts_of "$old")
delivered_old="$old	# 测试认证派单 · develop@$tip_wanted · 2026-11-24T08:47:00Z"
out=$(assignment_due "$tip_wanted" "$(( old_epoch + 9 * 3600 ))" "$cooldown" "$delivered_old")
case "$out" in
  "due 1 "*) pass "a brief 9 h ago with a 1 h cooldown -> due again (the regression)" ;;
  *) fail "a brief 9 h ago with a 1 h cooldown MUST be due again, got: $out" ;;
esac

# 4. The skip must SAY WHY. A skip that prints nothing is the second
#    half of the same bug: it reads like "sent, nobody answered".
out=$(assignment_due "$tip_wanted" "$(( recent_epoch + 600 ))" "$cooldown" "$delivered_recent")
case "$out" in
  *"cooldown"*) pass "the skip states the reason" ;;
  *) fail "the skip must state why it skipped, got: $out" ;;
esac

# 5. A clock that went backwards must not become a licence to skip.
out=$(assignment_due "$tip_wanted" "$(( recent_epoch - 500 ))" "$cooldown" "$delivered_recent")
case "$out" in
  "due 1 "*) pass "a future-stamped brief does not block the send" ;;
  *) fail "a future-stamped brief should not block the send, got: $out" ;;
esac

# 6. ★ A BRIEF FOR A DIFFERENT TIP MUST NOT SUPPRESS THIS ONE. The tip
#    is the key: the brief names the tip it wants certified, so a brief
#    for tip X says nothing about tip Y.
other_tip="def5678"
delivered_other="$recent	# 测试认证派单 · develop@$other_tip · 2026-11-24T17:30:00Z"
out=$(assignment_due "$tip_wanted" "$(( recent_epoch + 600 ))" "$cooldown" "$delivered_other")
case "$out" in
  "due 1 "*) pass "a brief for another tip does not suppress this one" ;;
  *) fail "a brief for another tip must not suppress this one, got: $out" ;;
esac

# 7. The newest brief for the tip wins when there are several.
delivered_two="$old	# 测试认证派单 · develop@$tip_wanted · 2026-11-24T08:47:00Z
$recent	# 测试认证派单 · develop@$tip_wanted · 2026-11-24T17:30:00Z"
out=$(assignment_due "$tip_wanted" "$(( recent_epoch + 600 ))" "$cooldown" "$delivered_two")
case "$out" in
  "due 0 "*) pass "with several briefs, the newest one decides" ;;
  *) fail "with several briefs, the newest one must decide, got: $out" ;;
esac

# ---------------------------------------------------------------------
# THE NUDGE MUST CARRY A COMMAND THAT WORKS. (Round 107.)
#
# The nudge tells a silent agent what to do, and one of the steps is a
# curl line. The first version of that line ended in
# `-w '%{http_code}\n'` — and the `\n` was expanded by `echo`, so the
# command was SPLIT ACROSS TWO LINES. A tester who pasted it would run
# a truncated curl and get a confusing result, and the coordinator
# would read that as "their network is broken".
#
# A nudge carrying a broken command is worse than no nudge: it sends
# the reader off to debug the wrong thing.
#
# So the nudge's command line must be a SINGLE line, and it must still
# contain the pieces that make it diagnosable.
# ---------------------------------------------------------------------

TIP=65514aa
N=/tmp/hub-nudge-body.$$
{
  echo "# 具体催办 · doubao-cloud-vm-01 · $(date -u +%FT%TZ)"
  echo "你已静默超过 6 小时。"
} > "$N"

# Reproduce the nudge's command line exactly the way the loop builds it.
{
  echo "    1) curl -6 -sS -m 20 -o /dev/null -w '%{http_code}' -H \"X-Hub-Token: \$CMESH_HUB_TOKEN\" 'https://new.blackevil.cn/hub/reports?topic=issue8'"
} >> "$N"

nudge_cmd_lines=$(grep -c "curl -6" "$N")
if [ "$nudge_cmd_lines" = "1" ]; then
  pass "the nudge's command is on ONE line"
else
  fail "the nudge's command spans $nudge_cmd_lines lines — pasting it would run a truncated curl"
fi

# And the command must still carry the parts that make it diagnosable.
if grep -q "http_code" "$N" && grep -q "X-Hub-Token" "$N" && grep -q "new.blackevil.cn" "$N"; then
  pass "the nudge's command still carries the code, the header and the URL"
else
  fail "the nudge's command lost a piece that makes it diagnosable"
fi

# ★ AND: the loop script itself must not contain the escaped-newline
# form. This is the actual regression — the body is built by `echo`,
# so a literal backslash-n in the source becomes a real newline.
#
# ★ TWO TRAPS, AND THE TEST FELL INTO BOTH BEFORE IT WORKED.
#
# (a) THE PATTERN MUST BE LITERAL. `grep -q '%{http_code}\n'` treats
#     `\n` as "newline", which matches nothing in a text file. Use
#     `grep -F`.
# (b) THE SOURCE HOLDS **TWO** BACKSLASHES. The echo line is written
#     as `... '%{http_code}\\n' ...`, so the bytes on disk are
#     backslash, backslash, n — and `echo` turns `\\` into one
#     backslash and then `n` into a newline. So the thing to search
#     for is TWO backslashes followed by 'n'.
#
# The first version searched for ONE backslash and matched nothing,
# so the mutant SURVIVED — a test that cannot fail.
if grep -qF '%{http_code}\\n' "$LOOP"; then
  fail "★ THE DEFECT: the loop's nudge command contains an escaped newline, so echo splits it across two lines and the reader pastes a truncated curl"
else
  pass "the loop's nudge command has no escaped newline"
fi

# ★ AND THE CHECK MUST BE ABLE TO FAIL. Prove it on a file that
# really does contain the bad form.
if printf 'x %%{http_code}\\\\n y\n' > /tmp/hub-nudge-mutant.$$ && \
   grep -qF '%{http_code}\\n' /tmp/hub-nudge-mutant.$$; then
  pass "the check really detects the bad form (it is not a check that cannot fail)"
else
  fail "★ the check CANNOT DETECT THE BAD FORM — it would survive a mutant"
fi
rm -f /tmp/hub-nudge-mutant.$$

rm -f "$N"

echo
if [ "$FAILURES" -gt 0 ]; then
  echo "$FAILURES failure(s)"
  exit 1
fi
echo "all good"
