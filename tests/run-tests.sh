#!/usr/bin/env bash
# ============================================================
# container-mesh — 本地测试套件
# ============================================================
# 全程不需要外网：relay 在本机随机端口启动，peer 用真实
# WebSocket + SPAKE2 + AES-GCM 走完整个链路。
#
# 用法:
#   bash tests/run-tests.sh            # 全部
#   bash tests/run-tests.sh --verbose  # 显示每个用例
#   bash tests/run-tests.sh --quick    # 跳过 Go 单测, 只跑端到端
#   bash tests/run-tests.sh --large    # 追加大文件传输测试 (默认 1GiB,
#                                      # 用 LARGE_MB=2048 调整)
#
# 退出码: 0 全部通过 | 1 有失败
# ============================================================
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERBOSE="no"
QUICK="no"
LARGE="no"
for arg in "$@"; do
    case "$arg" in
        --verbose) VERBOSE="yes" ;;
        --quick)   QUICK="yes" ;;
        --large)   LARGE="yes" ;;
    esac
done

PASS=0
FAIL=0
FAILED_NAMES=""

ok() {
    PASS=$((PASS + 1))
    [ "$VERBOSE" = "yes" ] && printf '  \033[32m✓\033[0m %s\n' "$1"
    return 0
}

bad() {
    FAIL=$((FAIL + 1))
    FAILED_NAMES="$FAILED_NAMES
  - $1: $2"
    printf '  \033[31m✗\033[0m %s — %s\n' "$1" "$2"
}

assert_eq() {
    local name="$1" expect="$2" actual="$3"
    if [ "$expect" = "$actual" ]; then ok "$name"; else bad "$name" "期望 [$expect], 实际 [$actual]"; fi
}

assert_ok() {
    local name="$1"; shift
    if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name" "命令失败: $*"; fi
}

assert_file_eq() {
    local name="$1" a="$2" b="$3"
    if [ ! -f "$a" ]; then bad "$name" "缺少文件 $a"; return; fi
    if [ ! -f "$b" ]; then bad "$name" "缺少文件 $b"; return; fi
    if cmp -s "$a" "$b"; then ok "$name"; else bad "$name" "内容不一致: $a vs $b"; fi
}

section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# wait_log FILE PATTERN SECONDS — 轮询日志直到出现模式或超时
wait_log() {
    local file="$1" pattern="$2" seconds="$3"
    local iterations=$((seconds * 4))
    local i
    for i in $(seq 1 "$iterations"); do
        if grep -q "$pattern" "$file" 2>/dev/null; then return 0; fi
        sleep 0.25
    done
    return 1
}

TMP="$(mktemp -d)"
RELAY_PID=""
cleanup() {
    if [ -n "$RELAY_PID" ]; then
        kill "$RELAY_PID" 2>/dev/null
        wait "$RELAY_PID" 2>/dev/null
    fi
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

BIN="$TMP/cmesh"

# ---------------------------------------------------------------
section "0. 构建"
if ( cd "$ROOT" && go build -o "$BIN" ./cmd/cm ); then
    ok "go build"
else
    bad "go build" "编译失败"
    printf '\n无法继续, 构建失败\n'
    exit 1
fi

# ---------------------------------------------------------------
if [ "$QUICK" = "no" ]; then
    section "1. Go 单元测试"
    if ( cd "$ROOT" && go test ./... ); then
        ok "go test ./..."
    else
        bad "go test ./..." "单元测试失败"
    fi

    section "2. 静态检查"
    if ( cd "$ROOT" && go vet ./... ); then ok "go vet"; else bad "go vet" "vet 报告问题"; fi
    UNFMT="$( cd "$ROOT" && gofmt -l . )"
    if [ -z "$UNFMT" ]; then ok "gofmt"; else bad "gofmt" "未格式化: $UNFMT"; fi
fi

# ---------------------------------------------------------------
section "3. probe 退出码契约"
"$BIN" probe --skip-network --device "$TMP/no-such-tun" --json > "$TMP/probe.json" 2>/dev/null
code=$?
assert_eq "无 TUN 且跳过网络 → exit 20" "20" "$code"
if python3 -c "import json,sys; json.load(open('$TMP/probe.json'))" 2>/dev/null; then
    ok "probe 输出是合法 JSON"
else
    bad "probe 输出是合法 JSON" "无法解析"
fi

# ---------------------------------------------------------------
section "4. 端到端: 消息 + 文件"
PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
"$BIN" relay --addr "127.0.0.1:$PORT" > "$TMP/relay.log" 2>&1 &
RELAY_PID=$!

ready="no"
for _ in $(seq 1 50); do
    if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then ready="yes"; break; fi
    sleep 0.1
done
if [ "$ready" = "yes" ]; then ok "relay 启动"; else
    bad "relay 启动" "健康检查未通过"
    cat "$TMP/relay.log"
    exit 1
fi

WS="ws://127.0.0.1:$PORT/ws"
CODE="test-code-$$"

mkdir -p "$TMP/src" "$TMP/dst"
# 1 MiB + 37 字节: 跨越多个 chunk 且不是 chunk 的整数倍
head -c 1048613 /dev/urandom > "$TMP/src/payload.bin"
SRC_SUM=$(shasum -a 256 "$TMP/src/payload.bin" | awk '{print $1}')

"$BIN" recv --relay "$WS" --code "$CODE" --name beta --out "$TMP/dst" --stay=false \
    > "$TMP/recv.log" 2>&1 &
RECV_PID=$!
sleep 0.4
"$BIN" send --relay "$WS" --code "$CODE" --name alpha \
    --file "$TMP/src/payload.bin" --message "hello-e2e" --stay=false \
    > "$TMP/send.log" 2>&1
SEND_RC=$?
wait "$RECV_PID" 2>/dev/null
RECV_RC=$?

assert_eq "send 退出码" "0" "$SEND_RC"
assert_eq "recv 退出码" "0" "$RECV_RC"
assert_file_eq "接收文件与源文件一致" "$TMP/src/payload.bin" "$TMP/dst/payload.bin"

DST_SUM=""
[ -f "$TMP/dst/payload.bin" ] && DST_SUM=$(shasum -a 256 "$TMP/dst/payload.bin" | awk '{print $1}')
assert_eq "SHA-256 一致" "$SRC_SUM" "$DST_SUM"

if [ -f "$TMP/dst/payload.bin.part" ]; then
    bad "无残留 .part 文件" "发现 payload.bin.part"
else
    ok "无残留 .part 文件"
fi

# ---------------------------------------------------------------
section "5. 端到端: 断点续传"
CODE2="resume-code-$$"
mkdir -p "$TMP/dst2"
# 预置 400000 字节前缀, 模拟上次中断
head -c 400000 "$TMP/src/payload.bin" > "$TMP/dst2/payload.bin.part"

"$BIN" recv --relay "$WS" --code "$CODE2" --name beta --out "$TMP/dst2" --stay=false \
    > "$TMP/recv2.log" 2>&1 &
RECV2_PID=$!
sleep 0.4
"$BIN" send --relay "$WS" --code "$CODE2" --name alpha \
    --file "$TMP/src/payload.bin" --stay=false > "$TMP/send2.log" 2>&1
SEND2_RC=$?
wait "$RECV2_PID" 2>/dev/null
RECV2_RC=$?

assert_eq "续传 send 退出码" "0" "$SEND2_RC"
assert_eq "续传 recv 退出码" "0" "$RECV2_RC"
assert_file_eq "续传后文件一致" "$TMP/src/payload.bin" "$TMP/dst2/payload.bin"

if grep -q "resuming transfer.*from=400000" "$TMP/send2.log"; then
    ok "从 400000 字节处续传"
else
    bad "从 400000 字节处续传" "日志中没有续传记录"
fi

# ---------------------------------------------------------------
section "6. 端到端: 错误码"
# 两端 code 不一致必须失败, 不能静默传输出错数据
CODE3="right-code-$$"
mkdir -p "$TMP/dst3"
"$BIN" recv --relay "$WS" --code "wrong-code-$$" --name beta --out "$TMP/dst3" --stay=false \
    > "$TMP/recv3.log" 2>&1 &
RECV3_PID=$!
sleep 0.4
"$BIN" send --relay "$WS" --code "$CODE3" --name alpha \
    --file "$TMP/src/payload.bin" --stay=false > "$TMP/send3.log" 2>&1
SEND3_RC=$?
wait "$RECV3_PID" 2>/dev/null
RECV3_RC=$?

if [ "$RECV3_RC" -ne 0 ]; then
    ok "错误 code 被拒绝 (recv 退出码 $RECV3_RC)"
else
    bad "错误 code 被拒绝" "recv 竟然成功退出"
fi
if [ -f "$TMP/dst3/payload.bin" ]; then
    bad "错误 code 未落盘文件" "不应产生 payload.bin"
else
    ok "错误 code 未落盘文件"
fi

# ---------------------------------------------------------------
section "7. 单元测试覆盖的安全性质"
if [ "$QUICK" = "no" ]; then
    if ( cd "$ROOT" && go test ./internal/secure/ -run 'TestSessionRejectsReplay|TestSessionRejectsTampered' -v 2>&1 | grep -q '^--- PASS' ); then
        ok "重放/篡改被拒绝"
    else
        bad "重放/篡改被拒绝" "测试未通过"
    fi
    if ( cd "$ROOT" && go test ./internal/peer/ -run 'TestCorruptPartialIsRejectedAndRemoved' >/dev/null 2>&1 ); then
        ok "损坏的 .part 不会被当作成功"
    else
        bad "损坏的 .part 不会被当作成功" "测试未通过"
    fi
else
    ok "跳过 (--quick)"
fi

# ---------------------------------------------------------------
section "8. 端到端: 断线重连 (杀掉 relay 后自动恢复会话)"
# 场景: 两端 --stay 常驻, relay 进程被杀并重启, 两端必须自动
# 重连、重新握手, 并且后续消息继续可达。
CODE8="reconnect-code-$$"
FIFO="$TMP/send8.in"
mkfifo "$FIFO"

"$BIN" recv --relay "$WS" --code "$CODE8" --name beta8 --out "$TMP" --stay \
    < /dev/null > "$TMP/recv8.log" 2>&1 &
RECV8_PID=$!
# 写入端先 sleep 再发第二条消息; 打开 fifo 会阻塞到 send 读端就绪
( sleep 8; echo "m2-after-reconnect" ) > "$FIFO" &
"$BIN" send --relay "$WS" --code "$CODE8" --name alpha8 --message "m1-before-kill" --stay \
    < "$FIFO" > "$TMP/send8.log" 2>&1 &
SEND8_PID=$!

if wait_log "$TMP/recv8.log" "m1-before-kill" 20; then
    ok "重连前消息可达 (m1)"
else
    bad "重连前消息可达 (m1)" "recv 日志中未见 m1"
fi

# 杀掉 relay, 立即在同一个端口重启一个新 relay
kill "$RELAY_PID" 2>/dev/null
wait "$RELAY_PID" 2>/dev/null
"$BIN" relay --addr "127.0.0.1:$PORT" > "$TMP/relay8.log" 2>&1 &
RELAY_PID=$!
ready="no"
for _ in $(seq 1 50); do
    if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then ready="yes"; break; fi
    sleep 0.1
done
if [ "$ready" = "yes" ]; then ok "relay 重启"; else bad "relay 重启" "健康检查未通过"; fi

if wait_log "$TMP/recv8.log" "m2-after-reconnect" 45; then
    ok "重连后消息可达 (m2)"
else
    bad "重连后消息可达 (m2)" "45s 内未在 recv 日志看到 m2"
    printf -- '--- recv8.log ---\n'; cat "$TMP/recv8.log"
    printf -- '--- send8.log ---\n'; cat "$TMP/send8.log"
fi

kill "$RECV8_PID" "$SEND8_PID" 2>/dev/null
wait "$RECV8_PID" "$SEND8_PID" 2>/dev/null

# ---------------------------------------------------------------
section "9. 端到端: 多 relay 故障切换"
# 第一个 relay 端口没有进程 (连接立即被拒), 客户端必须自动
# 换到列表中的第二个 relay 并完成传输。
DEADPORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
PORT2=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
"$BIN" relay --addr "127.0.0.1:$PORT2" > "$TMP/relay9.log" 2>&1 &
RELAY9_PID=$!
ready="no"
for _ in $(seq 1 50); do
    if curl -sf "http://127.0.0.1:$PORT2/healthz" >/dev/null 2>&1; then ready="yes"; break; fi
    sleep 0.1
done
if [ "$ready" = "yes" ]; then ok "备用 relay 启动"; else bad "备用 relay 启动" "健康检查未通过"; fi

RELAY_LIST="ws://127.0.0.1:$DEADPORT/ws,ws://127.0.0.1:$PORT2/ws"
CODE9="fallback-code-$$"
mkdir -p "$TMP/dst9"
"$BIN" recv --relay "$RELAY_LIST" --code "$CODE9" --name beta9 --out "$TMP/dst9" --stay=false \
    > "$TMP/recv9.log" 2>&1 &
RECV9_PID=$!
sleep 0.4
"$BIN" send --relay "$RELAY_LIST" --code "$CODE9" --name alpha9 \
    --file "$TMP/src/payload.bin" --stay=false > "$TMP/send9.log" 2>&1
SEND9_RC=$?
wait "$RECV9_PID" 2>/dev/null
RECV9_RC=$?
kill "$RELAY9_PID" 2>/dev/null
wait "$RELAY9_PID" 2>/dev/null

assert_eq "故障切换 send 退出码" "0" "$SEND9_RC"
assert_eq "故障切换 recv 退出码" "0" "$RECV9_RC"
assert_file_eq "故障切换后文件一致" "$TMP/src/payload.bin" "$TMP/dst9/payload.bin"

# ---------------------------------------------------------------
if [ "$LARGE" = "yes" ]; then
    section "10. 端到端: 大文件 (${LARGE_MB:-1024} MiB)"
    MB="${LARGE_MB:-1024}"
    CODE10="large-code-$$"
    mkdir -p "$TMP/dst10"
    echo "生成 ${MB} MiB 随机数据…"
    head -c $((MB * 1048576)) /dev/urandom > "$TMP/src/big.bin"
    BIG_SUM=$(shasum -a 256 "$TMP/src/big.bin" | awk '{print $1}')

    START=$(date +%s)
    "$BIN" recv --relay "$WS" --code "$CODE10" --name beta10 --out "$TMP/dst10" --stay=false \
        > "$TMP/recv10.log" 2>&1 &
    RECV10_PID=$!
    sleep 0.4
    "$BIN" send --relay "$WS" --code "$CODE10" --name alpha10 \
        --file "$TMP/src/big.bin" --stay=false > "$TMP/send10.log" 2>&1
    SEND10_RC=$?
    wait "$RECV10_PID" 2>/dev/null
    RECV10_RC=$?
    END=$(date +%s)

    assert_eq "大文件 send 退出码" "0" "$SEND10_RC"
    assert_eq "大文件 recv 退出码" "0" "$RECV10_RC"
    BIG_DST_SUM=""
    [ -f "$TMP/dst10/big.bin" ] && BIG_DST_SUM=$(shasum -a 256 "$TMP/dst10/big.bin" | awk '{print $1}')
    assert_eq "大文件 SHA-256 一致" "$BIG_SUM" "$BIG_DST_SUM"
    printf '  用时 %ss\n' "$((END - START))"
else
    section "10. 端到端: 大文件 (跳过, 用 --large 启用)"
    ok "跳过 (--large 未指定)"
fi

# ---------------------------------------------------------------
section "结果"
printf '通过 %d, 失败 %d\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
    printf '\n失败项:%s\n' "$FAILED_NAMES"
    exit 1
fi
printf '\033[32m全部通过\033[0m\n'
exit 0
