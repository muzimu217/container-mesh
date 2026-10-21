#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 muzimu217
#
# start.sh — bring up a container-mesh relay on this host.
#
# Designed for "extract and run": no config file to edit, no dependencies
# beyond a POSIX shell. It picks the right binary for this architecture,
# picks a free port if the default is taken, and prints exactly what the
# other side should pass to --relay.
#
#   ./start.sh              # foreground, Ctrl-C to stop
#   ./start.sh -d           # background, writes relay.pid and relay.log
#   ./start.sh -p 9000      # listen on a specific port
#   ./start.sh --tls CERT KEY   # serve wss:// directly (see README)

set -eu

PORT=8080
PORT_EXPLICIT=0
BACKGROUND=0
TLS_CERT=""
TLS_KEY=""
BIND=":8080"

usage() {
    sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

while [ $# -gt 0 ]; do
    case "$1" in
    -d | --daemon)
        BACKGROUND=1
        shift
        ;;
    -p | --port)
        PORT="$2"
        BIND=":$2"
        PORT_EXPLICIT=1
        shift 2
        ;;
    --tls)
        TLS_CERT="$2"
        TLS_KEY="$3"
        shift 3
        ;;
    -h | --help) usage 0 ;;
    *)
        echo "unknown option: $1" >&2
        usage 2
        ;;
    esac
done

# ---- pick the binary for this machine -------------------------------------

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) bin=./cmesh-linux-amd64 ;;
aarch64 | arm64) bin=./cmesh-linux-arm64 ;;
*)
    echo "unsupported architecture: $arch" >&2
    echo "this package ships linux/amd64 and linux/arm64 binaries" >&2
    exit 1
    ;;
esac

if [ ! -x "$bin" ]; then
    chmod +x "$bin" 2>/dev/null || true
fi
if [ ! -x "$bin" ]; then
    echo "cannot execute $bin; check that the filesystem allows execution" >&2
    echo "(a noexec mount is the usual cause: try copying the directory to /tmp)" >&2
    exit 1
fi

# ---- make sure the port is actually ours ----------------------------------

# A demo that dies with "address already in use" is a bad first impression, and
# on a shared host 8080 is very often taken (a reverse proxy, another demo).
# Unless the operator named a port, walk upward until one is free.
if [ "$PORT_EXPLICIT" -eq 0 ]; then
    tries=0
    while [ $tries -lt 20 ]; do
        if ! (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
            break
        fi
        exec 3>&- 2>/dev/null || true
        echo "port $PORT is in use; trying $((PORT + 1))" >&2
        PORT=$((PORT + 1))
        BIND=":$PORT"
        tries=$((tries + 1))
    done
    if [ $tries -ge 20 ]; then
        echo "could not find a free port between 8080 and 8099" >&2
        echo "pick one explicitly with: ./start.sh -p 9000" >&2
        exit 1
    fi
fi

# ---- work out how the other side should reach us --------------------------

# Best effort: the public address is only a hint. If this host sits behind NAT
# or a proxy the operator must supply the real hostname, so the banner says
# both what we guessed and what to override.
public_ip=$(
    (curl -fsS --max-time 3 https://api.ipify.org 2>/dev/null ||
        curl -fsS --max-time 3 https://ifconfig.me 2>/dev/null) | tr -d '\r\n'
) || public_ip=""
[ -n "$public_ip" ] || public_ip="<this-host>"

scheme=ws
if [ -n "$TLS_CERT" ]; then
    scheme=wss
fi
relay_url="$scheme://$public_ip:$PORT/ws"

# ---- banner ---------------------------------------------------------------

cat <<BANNER
container-mesh relay
  listening   $BIND
  version     $("$bin" version | awk '{print $2}')

  From another machine, connect with:
      --relay $relay_url

  If this host is behind NAT or a domain points at it, use the domain instead:
      --relay ${scheme}://your.domain:$PORT/ws

  Quick check from this host:
      $bin send --relay ws://127.0.0.1:$PORT/ws --code demo1234 --message hi

BANNER

# ---- run ------------------------------------------------------------------

if [ "$BACKGROUND" -eq 1 ]; then
    if [ -f relay.pid ] && kill -0 "$(cat relay.pid)" 2>/dev/null; then
        echo "already running (pid $(cat relay.pid)); stop it first with ./stop.sh" >&2
        exit 1
    fi
    if [ -n "$TLS_CERT" ]; then
        "$bin" relay --addr "$BIND" --tls-cert "$TLS_CERT" --tls-key "$TLS_KEY" \
            >relay.log 2>&1 &
    else
        "$bin" relay --addr "$BIND" >relay.log 2>&1 &
    fi
    echo $! >relay.pid
    sleep 1
    if kill -0 "$(cat relay.pid)" 2>/dev/null; then
        echo "running in background (pid $(cat relay.pid)); log: ./relay.log"
        echo "stop with: ./stop.sh"
    else
        echo "relay exited immediately; last lines of relay.log:" >&2
        tail -n 20 relay.log >&2
        if grep -q "address already in use" relay.log 2>/dev/null; then
            echo >&2
            echo "port $PORT is already taken on this host. Either pick another:" >&2
            echo "    ./start.sh -d -p 9000" >&2
            echo "or find out what holds it:" >&2
            echo "    ss -ltnp | grep :$PORT" >&2
        fi
        rm -f relay.pid
        exit 1
    fi
    exit 0
fi

if [ -n "$TLS_CERT" ]; then
    exec "$bin" relay --addr "$BIND" --tls-cert "$TLS_CERT" --tls-key "$TLS_KEY"
fi
exec "$bin" relay --addr "$BIND"
