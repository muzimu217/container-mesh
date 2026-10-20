# container-mesh

两台 NAT 后、低权限容器之间的**用户态**双向通道：小消息 + 大文件（断点续传），
**断线自动重连、多 relay 自动切换、心跳保活**。

不依赖 TUN、不依赖 `CAP_NET_ADMIN`、不依赖入站端口。只要求容器能**出站**连到一个中继。

```
 ┌─────────────┐        ┌──────────────┐        ┌─────────────┐
 │  container A│        │    relay     │        │  container B│
 │  (role A)   │◀──WS──▶│  (dumb pipe) │◀──WS──▶│  (role B)   │
 └─────────────┘        └──────────────┘        └─────────────┘
        └────────── SPAKE2 + AES-256-GCM ──────────┘
                   relay 看不到明文
```

网络鲁棒性（全球/跨运营商可用性的关键）：

- **心跳保活**：WS 层 20s ping / 60s 超时。NAT 映射过期或透明代理静默掐断连接后，
  约 1 分钟内自动发现，而不是永远挂死。
- **断线自动重连**：relay 或网络一抖，两端自动重新拨号 + 重新跑 SPAKE2 握手，
  消息和文件通道恢复；传输中的文件从接收端 `.part` **断点续传**。
- **多 relay 有序切换**：`--relay A,B,C` 按顺序尝试，连不上或配对窗口内对方没来
  就换下一台。国内 + 海外各部署一台，列表顺序即路由策略（见 `deploy/README.md`）。
- **P2P 直连（实验性，默认关闭）**：加 `--p2p` 后双方尝试 UDP 打洞，成功后数据
  不再经过 relay，延迟从 2×RTT 降到 1×RTT，也不消耗 relay 带宽。打洞失败自动
  保持 relay。

  **默认关闭的原因**：回环实测该用户态 UDP 路径比 relay 慢约 16 倍（13 MiB/s vs
  256 MiB/s），每 1177 字节一个系统调用 + 一次 AEAD，而 relay 一次处理 512 KiB。
  它适合「relay 带宽受限或额外 RTT 是瓶颈」的场景，而这需要真实广域网验证；在
  快速链路上不要开。

  **已知限制**（外部评审实测）：只有累积 ACK，没有 SACK 或快重传，丢包恢复要等
  一个 RTO；`Send` 在重传 12 次后会静默放弃该帧。补齐这些属于设计级改动，在
  「直连是否值得」被真实广域网数据回答之前不做。

---

## 1. 状态：哪些已验证，哪些没有

### ✅ 已在本机实测（macOS arm64, Go 1.27.2，2026-10-18）

| 项目 | 结果 |
|---|---|
| `go build ./...` / `go vet ./...` / `gofmt` | 通过 |
| 单元测试（wire / secure / probe / peer / relay / wsconn） | 全部通过 |
| `go test -race ./...` | 通过（含并发发送顺序、双向并发传输、传输中插 Ping） |
| 端到端：消息双向收发、文件传输 + SHA-256 一致 | 通过 |
| 端到端：断点续传（预置 `.part` 前缀，从正确 offset 续传） | 通过 |
| 端到端：**1 GiB 文件**传输 + SHA-256 一致 | 通过（回环 2s 完成） |
| 端到端：错误 code 被拒绝，不落盘 | 通过 |
| **断线重连**：杀掉 relay → 两端自动重连 → 后续消息可达 | 通过（e2e 用例 8） |
| **多 relay 故障切换**：第一个 relay 不可达 → 自动用第二个完成传输 | 通过（e2e 用例 9） |
| **双向同时传文件**（一个上行一个下行并发） | 通过（单测 TestDuplexTransfers） |
| 安全性质：重放拒绝、篡改拒绝、坏 `.part` 不误判成功 | 通过 |
| 吞吐（本机回环，含进程启动与握手；512 KiB 分块） | 1 MiB → 21.3 MiB/s；10 MiB → 166 MiB/s；100 MiB → 468.7 MiB/s；512 MiB → **539.3 MiB/s**（64 KiB 分块时代为 211.9） |
| `cmesh probe` 退出码契约 | 0 / 10 / 20 三档均已触发验证 |
| STUN 多服务器探测（国内+海外并行） | 实测：移动网络下 `stun.miwifi.com` 138ms 最快、`stun.qq.com` 超时、Cloudflare/Google 可达但端口映射不同 |

### ❌ 未验证 / 仅设计假设

| 项目 | 说明 |
|---|---|
| **在目标容器内运行 `cmesh`** | 本机无 Docker/Podman，**没有在真实容器里跑过**。目标容器已确认 croc 可用，而 `cmesh` 的网络形态与 croc 相同（出站 TCP/WS 到 relay），所以可行性高，但**这是推断，不是实测**。`dist/cmesh-linux-amd64` 可直接拷入目标容器跑 `cmesh probe` 验证。 |
| **公网 relay 部署** | 只在 `127.0.0.1` 验证过。部署路径（Dockerfile / compose + Caddy TLS / 免费资源矩阵）已备好，见 [`deploy/README.md`](deploy/README.md)。 |
| **跨国真实链路** | 多 relay 逻辑已在回环验证；真实跨境延迟/丢包下的重连表现未实测。 |
| **同方向并发传多个文件** | 会话层支持双向各一个并发传输；同一方向多个文件仍串行（协议单流，`wire.Frame.Stream` 字段预留多路复用）。 |
| **透明代理下的长连接行为** | 心跳机制正是为此设计（20s ping 保 NAT 映射、60s 检测死链），但真实透明代理环境未实测。 |
| **TUN 探测在 Linux 容器内的真实表现** | 探测逻辑已实现并交叉编译通过，未在无 `CAP_NET_ADMIN` 的 Linux 容器里实跑。 |

诚实说明：`cmesh` 的**协议与实现**是完整跑通的，**部署环境**是未验证的。

---

## 2. 快速开始

```bash
# 构建
go build -o cmesh ./cmd/cm

# 1) 先探测目标容器到底能干什么
./cmesh probe
#    exit 0  = 能建 TUN（本方案非必需，但有说明权限充足）
#    exit 10 = 仅用户态（预期结果）
#    exit 20 = 连出站都不通，本方案不可用

./cmesh probe --json > probe.json   # 结构化输出
```

### 起一个中继

```bash
./cmesh relay --addr :8080 --path /ws
# 健康检查: curl http://localhost:8080/healthz
```

生产环境建议放在 TLS 后面（Caddy/nginx），客户端用 `wss://`。

### 传文件

```bash
# B 端（接收方，role B）
./cmesh recv --relay wss://relay.example.com/ws --code "correct-horse-battery" --out ./downloads --stay=false

# A 端（发送方，role A）
./cmesh send --relay wss://relay.example.com/ws --code "correct-horse-battery" --file ./big.iso --stay=false
```

`--stay=false` 表示传完就退出；默认 `true` 会保持会话，可以继续收发消息。

**连上后两端都会打印一行 SAS**，请核对两边是否一致：

```
[cmesh] connected: wss://relay.example.com/ws  peer=beta  SAS=418302  fingerprint=9f3c1d8a77e0b245
```

连接建立后会多打一行路径信息：

```
[cmesh] p2p-direct: p2p: direct path to 10.0.0.5:51234 (rtt 1ms)
# 或
[cmesh] p2p-relay: p2p: direct path unavailable (hole punching failed: ...); using relay
```

**建议用 `--expect-sas` 强制核对**（防御主动 relay 的唯一真正手段）：

```bash
# 第一次连接记下对方打印的 SAS，之后每次都强制比对
./cmesh recv --code "..." --expect-sas 418302
```

不一致会直接中止会话并提示 `possible active relay or wrong code`。

`SAS` 相同时，说明中间没有第三方——即使 relay 被入侵或被要求配合，它也
无法让两端派生出相同的 SAS（它不知道 code）。这是唯一需要人工确认的一步，
每次会话一次。详细威胁模型见 [§3.3](#33-安全模型)。

接收端默认限制单文件 16 GiB 并在写入前检查可用空间：

```bash
./cmesh recv --code "..." --out ./in --max-file-size 34359738368  # 32 GiB
./cmesh recv --code "..." --out ./in --max-file-size -1           # 不限制
```

### 聊天 / 双向消息 / 会话中随时传文件

```bash
# 两端都 --stay（默认），然后直接在终端输入；/file 随时发文件
./cmesh recv --relay ws://127.0.0.1:8080/ws --code "my-secret-code" --out ./in
./cmesh send --relay ws://127.0.0.1:8080/ws --code "my-secret-code"
# 对端消息以  << text  打印；输入 /file ./report.pdf 立即发送
```

常驻会话下两端**完全对等**：任何一端都可以随时发消息（直接打字）或发文件
（`/file PATH`），对端自动接收并校验。

### 多 relay 与自动重连

```bash
# 逗号分隔列表，按顺序尝试；两端用同一份列表即可自动在同一台 relay 相遇。
# 例：国内机器把国内 relay 放前面，海外机器反之（路由策略见 deploy/README.md）
export CMESH_RELAY=wss://cn-relay.example.com/ws,wss://intl-relay.example.com/ws

./cmesh recv --code "my-secret-code" --out ./in   # 常驻，断线自动重连
```

- relay 进程被杀/重启、网络切换、NAT 映射过期：两端在几秒内自动重新配对。
- 传输中断线：重连后自动重新 offer，接收端从 `.part` 续传，已传字节不浪费。
- `--pair-window 60s`：单台 relay 上等待对方出现的窗口，超时换下一台。

### 断点续传怎么用

什么都不用做。接收方如果发现 `文件名.part` 存在，会自动把已有字节数告诉发送方，发送方从该 offset 继续。

```bash
# 模拟：第一次传输中断，留下 2MB 的 .part
ls -l downloads/big.iso.part   # 2000000 bytes
# 重跑同样的 recv + send，日志会显示：
#   level=INFO msg="resuming transfer" file=big.iso from=2000000 total=5000000
```

如果 `.part` 内容是坏的，最终 SHA-256 校验会失败，坏文件被删除，下一次从头开始。**不会出现"校验通过但内容错"的情况**。

### 环境变量

| 变量 | 等价参数 |
|---|---|
| `CMESH_RELAY` | `--relay` |
| `CMESH_CODE` | `--code`（未设时自动生成高熵 code） |

---

## 3. 架构

### 3.1 层次

```
cmd/cm              CLI 入口
internal/cli        probe / relay / send / recv 子命令；Node = 常驻节点
                    （多 relay 轮换、自动重连、断点续传重试）
internal/peer       会话层：单一读泵按帧类型分发（消息、双向文件传输互不阻塞）
internal/secure     SPAKE2 握手 + HKDF + AES-256-GCM（含防重放）
internal/wire       二进制帧格式（10 字节头 + payload）
internal/relay      中继服务：房间配对 + 双向转发 + 心跳 + 同角色重连踢占位
internal/wsconn     WebSocket 传输封装 + TCP/WS 双层 keepalive
internal/probe      权限/网络探测（TUN、CapEff、UDP/TCP、多 STUN 含国内服务器）
```

### 3.2 连接流程

```
A                              relay                              B
│── WS connect ?room=X&role=A ──▶│                                │
│                                │◀── WS connect ?room=X&role=B ──│
│                                │  (配对，两连接 staple 起来)      │
│── SPAKE2 msg1 ─────────────────┼───────────────────────────────▶│
│◀───────────────────────────────┼──────────────── SPAKE2 msg2 ───│
│  双方独立算出同一 session key（relay 看不到）                       │
│── Hello ───────────────────────┼───────────────────────────────▶│
│◀───────────────────────────────┼────────────────────── Hello ───│
│── FileOffer(name,size,sha256) ─┼───────────────────────────────▶│
│◀───────────────────────────────┼──────── FileAccept(resume_from)│
│── FileChunk(offset,data) × N ──┼───────────────────────────────▶│
│── FileDone(sha256) ────────────┼───────────────────────────────▶│
│◀───────────────────────────────┼─────────── FileResult(ok,msg) ─│
```

### 3.3 安全模型

- **威胁模型**：relay 不可信（可丢包、延迟、注入、记录流量，**也可以主动做中间人**）；攻击者可能知道协议但不知道 code。
- **握手**：SPAKE2（`github.com/schollz/pake/v3`，P-256）。这是显式认证的 PAKE，被动录制者无法离线爆破短码。
- **relay 拿不到 code**：客户端发给 relay 的 `room` 是 `HMAC-SHA256(code, "container-mesh/room/v1")` 的前 16 字节 hex（`secure.RoomID`），不是 code 本身。relay 只看到一个不可逆标签，无法直接参与握手。
- **SAS 带外核对（防主动 MITM）**：连接建立后两端各打印同一个 **6 位短认证串**（`secure.Session.ShortAuth`，由 session key 经独立 HKDF 派生），并附带 16 位十六进制指纹。**双方口头/截图比对一次**即可确认中间没有第三方。恶意 relay 虽然能路由（它知道 room 标签），但不知道 code，无法让两端派生出相同的 SAS。
- **密钥分离**：`HKDF-SHA256(sessionKey, info="container-mesh/v1 A->B" / "B->A")`，两个方向独立；SAS 用独立的 `v1 SAS` 标签派生，保证两端一致。
- **加密**：AES-256-GCM，12 字节 nonce = 8 字节大端计数器 + 4 字节零。
- **防重放**：接收方要求严格单调递增的计数器，任何乱序/重放/篡改都直接失败并断开。
- **文件完整性**：全文件 SHA-256，发送方在 offer 里声明、在 done 里再确认一次，接收方校验后才发布最终文件。
- **接收端保护**：
  - 文件名走白名单（字母/数字/`.`/`-`/`_`/`+`/空格/`()`/`[]`/`@`/`#`），拒绝路径分隔符、隐藏文件、Windows 设备名、`.part` 后缀、超长名；
  - 默认拒绝覆盖已存在的同名文件（link-then-remove 原子发布，不支持硬链接时回退为存在性检查）；
  - `--max-file-size` 限制单文件大小（默认 16 GiB，负数表示不限制）；
  - 写入前用 `statfs(Bavail)` 预检可用空间，不足直接拒绝，避免写满磁盘。
- **relay 侧防护**：按源 IP 的并发连接数（默认 16）与新建速率（默认 120/分钟）限流；`X-Forwarded-For` / `X-Real-IP` 参与判定，适配 nginx/Caddy 反代；日志只记录 room 前 8 字符。

**未做的**（诚实标注）：
- 没有前向保密轮换（单次会话内密钥固定）。
- **SAS 需要人工比对**：不比对时，主动 relay 仍可 MITM。这是刻意的取舍——自动比对需要预共享身份（证书/公钥），那会引入另一套密钥分发问题。
- `RoomID` 是 code 的确定性函数，所以拿到它的攻击者可以对**弱 code** 做离线字典攻击。请使用高熵 code（建议 4 个以上随机单词）。
- code 强度依赖用户。代码强制规范化后至少 6 字符，但没有熵检查。
- 没有内建限流之外的反滥用手段（如 captcha、账户体系）。

### 3.4 帧格式

```
 0        1        2        6        10
 +--------+--------+--------+--------+----------------+
 | type   | flags  | stream | length |    payload     |
 | 1 byte | 1 byte | 4 byte | 4 byte |   0..1 MiB     |
 +--------+--------+--------+--------+----------------+
```

`stream` 字段当前未使用，预留给未来的多路复用（双向并发传输）。

帧类型：`hello`、`msg`、`file-offer`、`file-accept`、`file-reject`、`file-chunk`、`file-done`、`file-result`、`error`、`ping`、`pong`。

### 3.5 为什么不用 TUN / EasyTier / WebRTC

见 [`docs/decisions.md`](docs/decisions.md)。一句话：目标容器无 `CAP_NET_ADMIN`、无 `/dev/net/tun`，TUN 类方案全部出局；EasyTier `--no-tun` 在该容器有可复现异常；WebRTC 自建的本质是自建 TURN，工作量大而收益与本方案相同。

---

## 4. 测试

```bash
# 全部（Go 单测 + 静态检查 + 端到端），不需要外网
bash tests/run-tests.sh

# 只看每个用例
bash tests/run-tests.sh --verbose

# 跳过 Go 单测，只跑端到端（快）
bash tests/run-tests.sh --quick

# 追加大文件用例（默认 1 GiB，LARGE_MB=2048 可调）
bash tests/run-tests.sh --quick --large
```

测试脚本做的事：
1. `go build`
2. `go test ./...`、`go vet ./...`、`gofmt -l`
3. `probe` 退出码契约（20 = 无 TUN 且跳过网络）
4. 端到端：relay 随机端口 → 1 MiB 文件 + 消息 → 字节比对 + SHA-256
5. 端到端：预置 `.part` → 验证从正确 offset 续传
6. 端到端：错误 code → 必须失败且不落盘
7. 安全性质：重放/篡改/坏 `.part`
8. **断线重连**：杀掉 relay 重启 → 两端自动恢复 → 后续消息可达
9. **多 relay 切换**：第一台不可达 → 自动落到第二台完成传输
10. **大文件**（`--large`）：1 GiB 传输 + SHA-256

当前结果：**30 项全部通过**（含 `--large` 的 1 GiB 用例；另有 `go test -race ./...` 全包通过；连续多次运行稳定）。

---

## 5. 部署

完整部署指南（含免费资源矩阵：Oracle Cloud Always Free、Cloudflare Tunnel、
国内轻量服务器、双 relay 路由策略等）见 **[`deploy/README.md`](deploy/README.md)**。
仓库根目录提供 `Dockerfile`，`deploy/` 下有 compose + Caddy 自动 HTTPS 的现成配置。

### 5.1 中继

单二进制，无状态（房间信息在内存，配对后即删除）。

```bash
./cmesh relay --addr :8080 --path /ws --pair-timeout 10m --max-rooms 1024
```

- `--pair-timeout`：第一个 peer 等待配对的最长时间，超时断开。
- `--max-rooms`：并发房间上限，限制内存。
- 心跳：relay 对每个连接 20s ping / 60s 超时，静默死链自动清理。

### 5.2 TLS

`cmesh relay` 只说 `ws://`。生产用反向代理：

```nginx
location /ws {
    proxy_pass http://127.0.0.1:8080/ws;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 3600s;      # 大文件传输别被掐断
    proxy_send_timeout 3600s;
    client_max_body_size 0;        # WS 帧不受此限，但别让别的规则误伤
}
```

客户端用 `--relay wss://relay.example.com/ws`。

### 5.3 带宽成本

所有流量都过 relay。100 GB 文件 ≈ 200 GB 双向计费流量。

| 方式 | 100 GB 文件估算 |
|---|---|
| 云厂商按流量 (¥0.5-0.8/GB) | ¥100-160 |
| Hetzner/含大流量 VPS | 通常无额外费用 |
| Twilio TURN 亚太（对比） | ~$120 |

---

## 6. 已知限制

1. **同方向并发传输**：一个会话内双向各支持一个并发文件传输（A 上行 + B 下行同时进行）；同一方向多文件仍串行。多路复用需扩展协议（`wire.Frame.Stream` 字段已预留）。
2. **目录**：直接传，无需手动 `tar`。发送端把目录打包成临时 tar 流式发送，接收端自动解包成目录树；归档中的符号链接/设备文件被跳过（防止越界写入），路径穿越被拒绝。
3. **无压缩**：直接传原始字节。压缩过的文件（zip/mp4）本来也不需要。
4. **无速率限制**：`cmesh` 不限制带宽，可能打满链路。
5. **中继无持久化**：relay 重启后所有会话断开；两端会在几秒内自动重连恢复，传输中的文件断点续传。
6. **重连语义**：消息是即发即弃的——断线期间对方发的消息不会补投；文件靠断点续传恢复。
7. **文件名字符集**：拒绝路径分隔符、`/`、`\`、`.`/`..`、Windows 设备名（`CON`/`NUL`/`COM1`…）、`.part` 后缀、控制字符、超长名（>255 字节）。
8. **隐藏文件（点开头）默认拒收**：这是**有意的策略**，不是缺陷。接收端会把文件写进工作目录，允许 `.bashrc`、`.ssh/` 之类会带来代码执行风险。需要时用 `--allow-hidden` 显式放开（遍历/分隔符/设备名的拦截不受影响）。
9. **同名文件默认不覆盖**：目标已存在时拒绝并提示 `rerun with --force to replace it`，避免对端静默销毁本地数据。用 `--force` 显式允许替换。
10. **code 未指定时自动生成**：`--code` 省略（且未设 `CMESH_CODE`）时，程序用 `crypto/rand` 生成一个 26 字符、约 128 位熵的 code 并打印一次。这比让用户自拟更安全——room 标签是 code 的确定性函数，弱 code 可被记录房间号的 relay 离线枚举。
8. **慢链路下限**：512 KiB 分块要求单帧在心跳超时（60s）内送达，即持续速率需高于 ~10 KB/s；拨号级链路不适用。

---

## 7. 开发

```bash
go build ./...          # 构建
go test ./...           # 单测
go vet ./...            # 静态检查
gofmt -l .              # 格式检查
bash tests/run-tests.sh # 全量
```

依赖（全部为纯 Go，无 cgo）：

| 模块 | 用途 |
|---|---|
| `github.com/gorilla/websocket` | WebSocket |
| `github.com/schollz/pake/v3` | SPAKE2 |
| `golang.org/x/crypto` | HKDF |
| `golang.org/x/sys` | `ioctl(TUNSETIFF)` |

> 中国大陆构建如遇 `proxy.golang.org` 超时，用 `GOPROXY=https://goproxy.cn,direct`。

---

## 8. 相关项目

本仓库是**双向组网**这一侧。配套的姊妹项目负责**单向远程运维入口**，两者定位互补、不重叠：

| 项目 | 定位 | 何时用 |
|---|---|---|
| **container-mesh**（本项目） | 机器 ↔ 机器：双向消息、大文件、断点续传。两端对等，各有身份，可互相主动发送。 | 需要在两台机器之间持续交换数据、同步产物、跑双向 RPC。 |
| **Cloud Remote Tunnel**<br>`../cloud-remote-tunnel` | 人 → 机器：单向远程终端 / 文件通道。把容器内的 shell 或 HTTP 服务暴露出来，供外部访问。 | 需要从本地终端登进远端容器敲命令、临时看一个 Web 服务。 |

一句话区分：**要「两台机器互相说话」用 container-mesh；要「我连进某台机器」用 Cloud Remote Tunnel。**

两者可以叠加使用：先用 Cloud Remote Tunnel 拿到远端容器的运维入口做部署与排障，再用 container-mesh 在业务层面做机器间的数据交换。

> 互链说明：本项目 README 已指向 Cloud Remote Tunnel。反向链接需要在 `cloud-remote-tunnel` 仓库侧添加（本任务红线禁止修改该目录）。建议在对方的 README 中加一段：
>
> ```markdown
> ## 相关项目
>
> - [container-mesh](../container-mesh) — 机器间双向消息与大文件传输（用户态、无 TUN、断点续传）。
>   本项目的定位是单向远程运维入口，两者互补。
> ```

---

## 9. 许可

Copyright (C) 2026 muzimu217

本项目采用 **GNU Affero General Public License v3.0（AGPL-3.0）**，全文见 [`LICENSE`](LICENSE)。

这是一个**强著佐权（copyleft）**协议，要点：

| 你可以 | 但必须 |
|---|---|
| 自由使用、修改、分发本软件 | 分发时附上完整源代码，并以 AGPL-3.0 授权 |
| 用于商业目的 | 若修改后对外提供**网络服务**（SaaS），也必须向该服务的用户提供完整源码 |
| 私有部署自用 | 仅内部使用、不对外分发或提供服务时，没有开源义务 |

**不允许**：把本项目（或其修改版）拿去做闭源分发，或做成闭源 SaaS 而不公开改动。

商业闭源授权需另行联系著作权人。若你只需要宽松许可（如 MIT/Apache-2.0）用于非著佐权项目，同样需要单独授权。
