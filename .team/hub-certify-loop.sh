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
  local to="$1" body="$2" token
  token=$(hub_token)
  [ -z "$token" ] && { echo "hub token unavailable" >&2; return 1; }
  if [ "$DRY" = 1 ]; then
    echo "  [dry] would send to=$to: $(head -c 60 "$body" | tr '\n' ' ')..."
    return 0
  fi
  if [ "$to" = "channel" ]; then
    curl -sS -m 30 -X POST -H "X-Hub-Token: $token" --data-binary @"$body" \
      "$HUB/report?from=coordinator&topic=issue8" 2>&1 | head -c 120
  else
    curl -sS -m 30 -X POST -H "X-Hub-Token: $token" --data-binary @"$body" \
      "$HUB/instruct?to=$to" 2>&1 | head -c 120
  fi
  echo
}

hub_reports() {
  local token
  token=$(hub_token)
  [ -z "$token" ] && return 1
  curl -sS -m 20 -H "X-Hub-Token: $token" "$HUB/reports?topic=issue8" 2>/dev/null
}

watermark() { head -1 "$WATERMARK" 2>/dev/null || echo "1970-01-01T00:00:00Z"; }

advance_watermark() {
  local ts="$1"
  printf '%s\n%s\n%s\n' "$ts" "$ts" "$ts" > "$WATERMARK"
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
printf '%s' "$REPORTS" | python3 -c "
import sys, json
d = json.load(sys.stdin)
last = {}
for m in d.get('messages') or []:
    f = m.get('from') or ''
    t = m.get('time') or ''
    if f and (f not in last or t > last[f]):
        last[f] = t
import os
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
PENDING=$(grep -c "pending" "$LEDGER" 2>/dev/null || echo 0)
if [ "$PENDING" -gt 0 ]; then
  BRIEF=/tmp/hub-certify-brief.$$
  {
    echo "# 测试认证派单 · develop@$TIP · $(date -u +%FT%TZ)"
    echo
    echo "当前 tip \`$TIP\` 尚未完成四线认证（台账 pending 项 $PENDING）。请各线在**本轮回信**里带上你复测的 SHA 与结论，格式："
    echo "    SHA=$TIP 结论=PASS|FAIL 证据=<命令原文+退出码+输出首末20行>"
    echo
    echo "分工："
    echo "  - doubao：真机功能面（join --cred / cred list / cred revoke / 中转四项 ping,msg,run,send-to）"
    echo "  - kuku：单测语义复核（本轮改动涉及的包，跑 go test 并贴结果）"
    echo "  - netns：资源与可用性（RSS 口径 / cred 命令面走查）"
    echo "  - oh-no：同 SHA 外部复测（独立环境）"
    echo
    echo "收到请先回 {\"ok\":true}。"
  } > "$BRIEF"
  echo "--- certification assignment (pending=$PENDING) ---"
  hub_post channel "$BRIEF"
  for a in $AGENTS; do
    hub_post "$a" "$BRIEF"
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
