#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 muzimu217
#
# stop.sh — stop the relay started by ./start.sh -d.

set -eu

if [ ! -f relay.pid ]; then
    echo "no relay.pid here; nothing started by ./start.sh -d" >&2
    exit 1
fi

pid=$(cat relay.pid)
if ! kill -0 "$pid" 2>/dev/null; then
    echo "process $pid is already gone; removing stale relay.pid"
    rm -f relay.pid
    exit 0
fi

kill "$pid"

# Wait up to 5 s for a clean exit, then insist. A relay holds no state worth a
# long shutdown, but giving it a moment lets in-flight handshakes fail cleanly
# instead of being cut off mid-frame.
i=0
while [ $i -lt 50 ]; do
    if ! kill -0 "$pid" 2>/dev/null; then
        rm -f relay.pid
        echo "stopped"
        exit 0
    fi
    sleep 0.1
    i=$((i + 1))
done

kill -9 "$pid" 2>/dev/null || true
rm -f relay.pid
echo "stopped (forced)"
