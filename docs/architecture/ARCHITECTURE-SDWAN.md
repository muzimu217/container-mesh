# container-mesh Overlay SD-WAN 总体架构设计

- 版本：v1.0（设计稿，不含实现代码）
- 日期：2026-10-20
- 代码基线：`container-mesh@5226d96`
- 输入材料：
  - 研究一：`research-userspace-netstack.md`（无 TUN 权限容器的用户态网络栈研究）
  - 研究二：`research-mux-and-portforward.md`（多路复用与端口转发研究）
  - 既有决策：`container-mesh/docs/decisions.md`（D1–D72）
- 证据标注约定：
  - **`[实测]`** = 两份研究或项目历史中实际跑出的数字/行为
  - **`[查证]`** = 读源码或官方文档确认
  - **`[推断]`** = 基于已知事实的工程判断，未实测

---

## 0. 摘要

本设计把 cmesh 从「点对点传文件」升级为「混合权限 Overlay SD-WAN」，交付三种接入模式：

| 模式 | 一句话 | 权限要求 | 状态 |
|---|---|---|---|
| **L3 TUN** | `ping 100.64.0.5`、`ssh user@100.64.0.5` 直接可用 | `CAP_NET_ADMIN` + `/dev/net/tun` | 目标容器**不可用**；特权环境可用；**无实测**（研究未做 TUN 模式） |
| **SOCKS5** | `ALL_PROXY=socks5://127.0.0.1:1080` | 无 | netstack 路径**研究已实测全通**（CONNECT/UDP/SSH）；直通路径未实测 |
| **端口转发** | `cmesh forward -L 8080:db:5432` | 无 | 原型**已实测 10/10**；cmesh 集成未实测 |

五个核心结论：

1. **无 TUN 时透明 L3 物理上不可能。** 不是实现缺陷：内核没有接口承载 `100.64.0.0/10` 路由，`ping` 实测 100% 丢包（研究一 §3.2、§7.1）。SOCKS5 和端口转发是「应用显式走代理」，不是「内核认识虚拟 IP」。
2. **复用层用 `hashicorp/yamux`，放在加密通道内部。** 研究二的结论是「mux 在 secure 之下」，但其层次画法存在一个顺序性矛盾（详见 §5.A1）：secure 要求严格递增计数器和严格有序，而 yamux 在应用读取前就按流分发。本设计采纳其**意图**（整条连接一组密钥、不做每流加密），把顺序修正为 **传输 → secure（帧级 AEAD）→ 记录泵 → mux → 业务流**，即 SSH 的 transport/channel 分层。
3. **SOCKS5 默认走「流直通」：由对端拨号，而不是本地 netstack 拨虚拟 IP。** 这把 gVisor netstack 从「必需」降级为「可选增强」，同时消除四个风险：二进制 +25%、Go 工具链被迫升级、MTU 黑洞、每活跃流 1.1 MiB 内存。研究一已证明 netstack 可行，本设计只是让它不再位于热路径。
4. **MTU 必须固定 1200。** 研究一实测：netstack MTU 大于隧道容量时 TCP **完全卡死**（1 MiB 后 write timeout），不是降速。且 IP 包必须聚合——项目 D28 实测（1 GiB 回环）1177 字节/包只有 13 MiB/s，512 KiB/帧约 512 MiB/s，固定开销差约 40 倍。
5. **先做端口转发。** 零传递依赖（仅 yamux 一个模块，+33 KB），覆盖 SSH/数据库/HTTP 等绝大多数场景，约 8 个工程日分 3 个 PR 验收（原估 3–5 天低估了记录泵与会话重构），并顺带验证 mux + secure + 传输切换的地基。

**建议起点：阶段 1（端口转发）。**

---

## 1. 证据口径与现状基线

### 1.1 已实测基线（本设计反复引用的数字）

| 指标 | 数值 | 来源 |
|---|---|---|
| 无 TUN 提供完整 TCP/IP（ICMP/TCP/UDP） | ✅ 全通，数据面 0 syscall、0 fd | 研究一 §1.1、§1.2 `[实测]` |
| netstack 空载 RSS / 单 stack 堆 / 建栈耗时 | 9.1 MiB / 53 KiB / 35–48 µs | 研究一 §1.3 `[实测]` |
| netstack 200 空闲流 / 200 活跃流 RSS | 24.5 MiB / 238.4 MiB（≈1.1 MiB/活跃流） | 研究一 §1.3 `[实测]` |
| netstack 单进程 TCP，MTU 1500 / 1280 / 1200 | 349.2 / 282.2 / 261.4 MiB/s | 研究一 §3.3 `[实测]` |
| netstack 双进程经真实 TCP 隧道，单连接持续 | 73–88 MiB/s | 研究一 §3.4 `[实测]` |
| SOCKS5 CONNECT 额外延迟 | +0.24 ms（p50 0.30 ms，p95 0.52 ms） | 研究一 §3.4 `[实测]` |
| MTU 黑洞 | 1500/1200 → write timeout；1200/1200 → 正常 | 研究一 §3.3 `[实测]` |
| gVisor 二进制增量（linux/amd64，`-s -w`） | 9,457,824 → 11,813,024 B（+2.35 MB，+24.9%） | 研究一 §5.1 `[实测]` |
| gVisor 实际新增链接模块 / 包 | 2 个（gvisor + btree）/ 41 个包 | 研究一 §5.2 `[实测]` |
| gVisor 快照 Go 要求 | 2025-05 → go 1.23.1（cmesh 保持 1.24.0）；2026-09 → go 1.26.3 | 研究一 §5.3 `[实测]` |
| yamux 吞吐（1/10/50 流，1 MiB 窗口） | 3211 / 2109 / 2771 MiB/s | 研究二 §4.7 `[实测]` |
| yamux + 消息适配 + 每消息 AES-GCM | 1957 / 1962 / 2748 MiB/s（1/10/50 流） | 研究二 §4.7 `[实测]` |
| yamux 每活跃流内存 @256 KiB 窗口 | 514.8 KiB（≈2×窗口） | 研究二 §4.5 `[实测]` |
| yamux 20000 并发流 | 177–254 ms 打开，33.6 MiB 堆，稳定 | 研究二 §4.6 `[实测]` |
| yamux 延迟（空闲 / 9 条满载） | p50 18 µs / 191 µs | 研究二 §4.8 `[实测]` |
| smux v1 数据丢失 | 15 次 2 次复现，最多丢 22.0 MiB，`Write` 返回 nil | 研究二 §4.4 `[实测]` |
| QUIC 原生多流对照 | 139–193 MiB/s（比 yamux 低一个数量级） | 研究二 §4.7 `[实测]` |
| 端口转发原型 | 10/10 并发字节精确，2 ms；白名单拒绝 status=1 | 研究二 §4.1、§3.4 `[实测]` |
| cmesh 文件传输回环（512 MiB，512 KiB 分块） | 539.3 MiB/s | README / D11 `[实测]` |
| cmesh 1 GiB 回环传输 | 2 s 完成，SHA-256 一致 | README `[实测]` |
| cmesh P2P legacy（1177 B/包） | 13 MiB/s（relay 同场景 512 MiB/s） | D28 `[实测]` |
| cmesh QUIC 直连 | 回环 219.5 MiB/s；跨公网 20 MiB / 7 s | D43 `[实测]` |
| 跨公网 relay 传输 | **11–12 MiB/s**（项目记录，用户提供；两份研究未包含该口径） | 项目现状 |
| 生产代码 / 测试代码 | 11,964 / 9,502 行 | 仓库统计 `[实测]` |
| `internal/p2p2` | 4,710 行生产 + 4,806 行测试，**零外部导入** | `go list -deps` 计数 0 `[实测]` |

### 1.2 现有分层（代码事实）

```
cmd/cm
 └── internal/cli          probe / relay / send / recv；Node = 常驻节点
      └── internal/peer    hello、消息、双向文件传输；单一读泵按帧类型分发
           ├── internal/secure   SPAKE2(p256) + HKDF + AES-256-GCM
           └── internal/peer/switchable
                 ├── internal/wsconn   WebSocket（relay，默认）
                 ├── internal/p2pquic  QUIC（打洞成功后）
                 └── internal/p2p      自研 UDP（legacy，--p2p-legacy）
```

关键约束（`[查证]`）：

- `secure.Session`：每方向一个 AES-256-GCM 密钥；64 位严格递增计数器；`OpenFrame` 要求帧**严格按发送顺序**到达，否则视为协议违规（`secure.go:399-417`）。
- `peer.Session`：**只有一个 goroutine 读**（`readPump`），`SendFrame` 用 `sendMu` 保护“密封 + 发送”的原子性（`session.go:423-431`）。
- `wire.Frame`：10 字节头（type 1 + flags 1 + stream 4 + length 4），`MaxPayload = 1 MiB`；`Stream` 字段**当前完全未使用**，README 标注“预留多路复用”。
- `p2p.MaxDatagram = 1200`（`packet.go:39-42`）。
- `wsconn` 单消息读上限 16 MiB（`wsconn.go:140`）；relay 单消息上限 16 MiB、MaxRooms 1024、每 IP 16 连接、120 连接/分钟。
- `go.mod`：`go 1.24.0`；quic-go `v0.55.0`（为保持 Go 1.24 而 pin，见提交 `b998803`）。

---

## 2. 目标与非目标

### 2.1 目标

| 编号 | 目标 | 可验证的判据 |
|---|---|---|
| G1 | **双权限模式**：有 TUN 走 L3，无 TUN 走用户态代理 | 同一二进制在 `probe` 退出码 0 / 10 两种环境下分别给出 L3 / SOCKS5 能力 |
| G2 | **免权限 SSH 接入**：容器内无 root 也能被 SSH 进来 | 无特权容器内 `ssh -p 2222 127.0.0.1` 或 `ssh -o ProxyCommand=... user@100.64.0.5` 拿到 banner |
| G3 | **任意 TCP/UDP 端口访问**，无需预先映射 | `curl --socks5-hostname` 访问对端任意端口；UDP ASSOCIATE echo 通过 |
| G4 | **保留大文件低延迟能力** | 现有 512 MiB 回环 ≥ 500 MiB/s 不退化；跨公网不低于现状 11–12 MiB/s 的 80% |
| G5 | **可选的中继监控与智能选路** | 本地拓扑/RTT 矩阵可查询；relay 只暴露聚合计数；路径切换有迟滞、不抖动 |

### 2.2 硬边界（明确做不到）

| 编号 | 做不到 | 原因与实测证据 |
|---|---|---|
| N1 | **无 TUN 时 `ping 100.64.0.5`** | 内核没有接口承载该路由。研究一实测 `ping` 2 包 0 收、`nc`/`curl` 直连失败（§3.2）；`cap_eff=0`、无 `/dev/net/tun`，`TUNSETIFF` 必然 `EPERM`（§7.1）。没有 workaround。 |
| N2 | **无 TUN 时 `ssh user@100.64.0.5` 不加参数直连** | 同上。必须写成 `ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@100.64.0.5` 或 `ssh -p 2222 user@127.0.0.1`。 |
| N3 | **无 TUN 时让不感知代理的进程透明走 mesh** | `LD_PRELOAD` 只能影响本进程及子进程，对已运行进程和静态链接无效（研究一 §7.1）。 |
| N4 | **SCTP / 任意 IP 协议** | gVisor 不实现 SCTP（研究一 §1.5）；TUN 模式下内核支持，但目标容器无 TUN。 |
| N5 | **无 TUN 时 mesh 内 ICMP 诊断（`ping`）** | 可以另做一个 `cmesh ping` 子命令（自定义 echo 帧）来测 RTT，但这不是内核 ICMP，`ping`/`traceroute` 等第三方工具仍不可用。该子命令**不在本设计的阶段 1–4 内**。 |
| N6 | **不修改 yamux 源码来修 bug** | MPL-2.0 文件级 copyleft；需要 patch 时单独发布 patch，不并入 AGPL 代码库（研究二 §5.1）。 |

### 2.3 成功判据（整体）

1. 无特权容器：`cmesh forward -L` 与 `cmesh proxy --socks5` 在**不引入 root、不创建任何设备**的前提下工作。
2. 有特权环境：`cmesh up` 后 `ping`/`ssh`/任意 IP 协议透明可用。
3. 现有文件传输、断点续传、多 relay、双向并发**全部不回归**。
4. 默认构建（不含 netstack 与 TUN 代码）的二进制增量 ≤ 100 KB；启用 netstack 的构建增量 ≤ 2.5 MB。

---

## 3. 分层架构

### 3.1 分层图

```
┌──────────────────────────────────────────────────────────────────────────┐
│ 应用层                                                                   │
│ curl / ssh / psql / redis-cli / 浏览器 / 任意 TCP、UDP 客户端            │
└──────────────────────────────────────────────────────────────────────────┘
     模式 A：L3 TUN               模式 B：SOCKS5              模式 C：端口转发
     ping / ssh 直连          ALL_PROXY=socks5://…        cmesh forward -L
            ▼                         ▼                         ▼
┌──────────────────────┐  ┌──────────────────────┐  ┌──────────────────────┐
│ Linux 内核 TCP/IP    │  │ internal/proxy       │  │ internal/forward     │
│ tun0 + 100.64/10     │  │ SOCKS5 CONNECT       │  │ -L / -R listener     │
│ 需 CAP_NET_ADMIN     │  │ UDP ASSOCIATE        │  │ 前置头 + AllowList   │
└──────────────────────┘  │ mesh 名表→peer       │  │ 目标来自本地配置     │
                          └──────────────────────┘  └──────────────────────┘
  │ 原始 IP 包（聚合）             │ 字节流（TCP 直通）                   │ 字节流
                     [可选] netstack 仅用于 IP 语义场景
            └───────────────────────────────────────────────────┘
                                      ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ internal/mux — hashicorp/yamux v0.1.2                                    │
│ 一条 stream = 一条被转发的连接；每流独立流控、半关闭、keepalive          │
│ 每条 stream 首字节 kind：0 控制/文件、1 forward、2 proxy、3 packet       │
│ （yamux stream ID 从 1/2 起按 2 递增，没有 stream 0）                    │
└──────────────────────────────────────────────────────────────────────────┘
                                      │ 一条有序字节流（io.Pipe）
                                      ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ 记录泵 record pump — 单读者 / 单写者（新增，约 60–120 行）               │
│ 读：transport.Recv → secure.OpenFrame → 管道                             │
│ 写：管道 → secure.SealFrame(FrameStreamData) → transport.Send            │
└──────────────────────────────────────────────────────────────────────────┘
                                      ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ internal/secure — SPAKE2(p256) + HKDF + AES-256-GCM 【代码不改】         │
│ 整条连接一组密钥；严格递增计数器；严格有序；每帧一次 AEAD                │
└──────────────────────────────────────────────────────────────────────────┘
                                      ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ internal/peer — MessageConn + switchable（传输切换只在其下）             │
│ p2pquic：QUIC 直连（打洞成功后）   |   wsconn：WebSocket（relay）        │
└──────────────────────────────────────────────────────────────────────────┘
                                      ▼
relay（盲转发，只能看到密文）   /   对端
```

### 3.2 每层职责与选型

| 层 | 组件 | 库 / 技术 | 负责 | 明确不负责 |
|---|---|---|---|---|
| 接入 A | `internal/tun` | `golang.org/x/sys/unix` | 创建 TUN、配置 IP/MTU/路由、读写 IP 包、聚合 | TCP/IP 处理（交给内核） |
| 接入 B | `internal/proxy` | 自研 SOCKS5（RFC 1928） | CONNECT、UDP ASSOCIATE、域名解析、mesh 名表 | 目标可达性判断 |
| 接入 C | `internal/forward` | 自研 | `-L`/`-R` 监听、流前置头、AllowList、Limits、双向 pipe | 加密、路由 |
| 可选 | `internal/netstack` | `gvisor.dev/gvisor/pkg/tcpip` | 用户态 TCP/UDP/ICMP、`gonet` 拨号、`tcp.NewForwarder` | DNS、路由协议、SCTP |
| 复用 | `internal/mux` | `github.com/hashicorp/yamux` v0.1.2 | 多流、每流流控、半关闭、keepalive、流数上限 | 加密、目标策略 |
| 记录泵 | `internal/mux/record`（或 `internal/peer`） | 自研适配器 | 把 yamux 的有序字节流 ↔ `secure` 的帧 API 对接；单读者单写者 | 密钥管理、帧格式 |
| 加密 | `internal/secure` | SPAKE2 + HKDF + AES-256-GCM | 认证、密钥派生、AEAD、防重放、SAS | 多流、流控 |
| 会话 | `internal/peer` | 自研 | 帧分发、消息、双向文件传输、控制流 | 传输细节、多流调度 |
| 传输 | `p2pquic` / `wsconn` / `p2p` | quic-go / gorilla / 自研 | 可靠消息/字节传输 | 加密、复用 |

### 3.3 数据面：两条传输、三种接入

传输层保持现有二选一，不新增第三种：

- **relay（默认）**：`wsconn` → WebSocket over TCP/TLS → relay 盲转发。穿透 HTTP 代理与 CDN，目标容器已验证同形态可行（croc 与 cmesh 同构）。
- **direct（可选）**：`p2p` 打洞 → `p2pquic` QUIC。失败自动留在 relay，不影响功能。

三种接入模式共享同一个 mux + secure + 传输栈，区别只在「谁产生字节」：

| 模式 | 进入 mux 之前的形态 | 每条 stream 承载 |
|---|---|---|
| L3 TUN | 原始 IP 包（按批聚合） | 一条 packet stream，内含多个 length-prefixed IP 包 |
| SOCKS5 | 一条 TCP 字节流 / 一组 UDP 数据报 | 一条 TCP 连接；UDP 走独立 datagram 流 |
| 端口转发 | 一条 TCP 字节流 | 一条 TCP 连接 |

### 3.4 控制面

v1 不做中心控制器。控制面是**会话级**的，复用现有加密通道：

1. **会合**：relay 按 `RoomID(code)` 配对（现有）。
2. **认证**：SPAKE2，双方派生同一 session key（现有）。
3. **能力交换**：`FrameHello` 增加 `mesh_ip`、`caps`（TUN/SOCKS5/forward/packet）、`mux` 版本。
4. **直连协商**：`FrameP2POffer/Answer`（现有），在 mux 建立**之前**完成。
5. **mux 启动屏障**：双方各发一个 `FrameMuxStart`，收到对方的后统一切换到 `FrameStreamData`。
6. **运行期**：第一条 stream（由 initiator 打开，ID 为 1）承载现有 `peer.Session`（消息/文件）；其余 stream（initiator 用奇数、responder 用偶数）承载转发与代理。注意 yamux 的 stream ID 从 1/2 开始按 2 递增（`session.go:115-117,178-182` `[查证]`），**不存在 stream 0**，因此控制流靠“谁先打开 + 首字节 kind”识别，而不是靠 ID。
7. **监控（阶段 4）**：每节点本地 `cmesh status`；relay 只提供聚合计数；不做全局拓扑数据库。

### 3.5 三种模式的端到端路径

**模式 A（L3 TUN）**

```
应用 connect(100.64.0.6:22)
  → 内核路由表：100.64.0.0/10 dev tun0
  → cmesh 从 tun0 读 IP 包
  → 按批聚合（目标 64 KiB/帧，上限 1 MiB）
  → packet stream → 记录泵 → secure 密封 → 传输
  → 对端 secure 解密 → 记录泵 → 拆包 → 写回自己的 tun0
  → 对端内核收到 SYN，sshd 应答
```

**模式 B（SOCKS5，TCP 直通）**

```
curl --socks5-hostname 127.0.0.1:1080 http://peer.mesh:8080/
  → SOCKS5 CONNECT，ATYP=0x03，域名 peer.mesh
  → 本地 mesh 名表：peer.mesh → 100.64.0.6
  → 找到与该 peer 的会话，OpenStream(kind=proxy)
  → 前置头 {target=100.64.0.6:8080}
  → 对端校验：目标是否属于自己（mesh IP 匹配）→ 按目标映射规则拨真实地址
  → 回 StatusOK，双向字节 pipe
```

**模式 C（端口转发）**

```
cmesh forward -L 127.0.0.1:2222:peer:22
  → 本地 listener 接受连接
  → OpenStream(kind=forward) + Preamble{Kind=direct, Addr="peer:22"}
  → 对端 AllowList 检查 → 拨号 → Status → pipe
```

---

## 4. 三种接入模式

### 4.1 模式 A：L3 TUN

**前置条件**

- Linux；`cmesh probe` 退出码 0（`/dev/net/tun` 存在、可 open、`TUNSETIFF` 成功）。
- `CAP_NET_ADMIN`（创建接口、配 IP、加路由）。
- 目标容器 `cap_eff=0`、无 `/dev/net/tun` → **不可用**（研究一 §7.1 `[实测]`）。

**用户怎么用**

```bash
cmesh up --mesh-ip 100.64.0.5 --cidr 100.64.0.0/10 --code CODE
ping 100.64.0.6
ssh user@100.64.0.6
psql -h 100.64.0.6 -p 5432
```

**实现要点**

- 只创建**一个** TUN 接口，只向路由表添加 mesh CIDR；**绝不改默认路由**，避免把宿主流量吸进隧道。
- MTU 固定 1200（理由见 §5.A3）。不依赖 PMTUD，因为隧道是静默丢包，没有 ICMP Fragmentation Needed 反馈（研究一 §3.3 `[实测]`）。
- 读 TUN → 聚合 → packet stream；从 packet stream 收到一批 → 拆包 → 写 TUN。
- 聚合格式：每帧 payload = 若干 `[len u16][IP 包]`；单帧目标 64 KiB，上限 1 MiB（`wire.MaxPayload`）。
- 断线：TUN 接口与路由保留；已建立的 TCP 连接断开（符合预期），应用重连即可。
- 启动时探测 CIDR 冲突：若 `100.64.0.0/10` 已被宿主路由或与容器 CNI 重叠，拒绝启动并提示换 CIDR。

**实测性能**

- **无直接实测**（研究一明确目标容器不可用，lab 未做 TUN 模式）。
- 参照上限：netstack 单进程 MTU 1200 = 261.4 MiB/s；双 netstack 经 TCP 隧道 = 73–88 MiB/s；cmesh 大帧回环 = 539 MiB/s。
- `[推断]` TUN 模式省掉两遍用户态 TCP 处理，回环应高于 73–88 MiB/s；但瓶颈会转移到**包率**（每包一次聚合编码 + AEAD + 传输），因此聚合是必需项。验收门槛取 **40 MiB/s（回环单流）**，跨公网取 **5 MiB/s**。

**复杂度 / 依赖 / 适用场景**

- 复杂度：**高**。平台相关（Linux only）、需要特权测试环境、路由配置有宿主风险。
- 依赖：无新第三方库（`x/sys/unix` 已在依赖中）。
- 适用：特权容器、VM、裸机、家庭/办公 LAN 互联；需要 ICMP、任意 IP 协议、透明访问、无法改应用代理配置的场景。

**边界**

- 目标容器（K8s，`cap_eff=0`）**永远走不到这个模式**。文档与 CLI 必须明确这一点，不能让用户误以为“装上就能 ping”。

### 4.2 模式 B：SOCKS5

**前置条件**

- 无。只要出站 TCP/UDP 可用（目标容器已验证）。
- 不需要 root、不需要 TUN、不需要 netstack（TCP 直通路径）。

**用户怎么用**

```bash
cmesh proxy --socks5 127.0.0.1:1080 --code CODE

# 任意支持 SOCKS5 的客户端
export ALL_PROXY=socks5://127.0.0.1:1080
curl http://peer.mesh:8080/
curl --socks5-hostname 127.0.0.1:1080 http://100.64.0.6:8080/

# SSH（ProxyCommand）
ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@100.64.0.6

# 或 ncat（nmap 发行版，-X 5 的替代）
ssh -o ProxyCommand='ncat --proxy 127.0.0.1:1080 --proxy-type socks5 %h %p' user@peer.mesh
```

**实现要点**

- 支持 RFC 1928 的 `CONNECT` 与 `UDP ASSOCIATE`；`ATYP=0x03`（域名）必须支持。
- **DNS 在代理层解析**，不在 netstack 里跑 DNS 服务器（研究一 §2.3、§7.4）：维护一张 `mesh 名字 → 100.64.x.y` 表；未命中再走系统 DNS（可选，默认只解析 mesh 名，避免把公网 DNS 查询也代理出去）。
- **TCP 直通**：目标属于某个 peer 时，不拨虚拟 IP，而是向该 peer 开一条 `kind=proxy` 的 stream，前置头带原始目标；对端校验目标归属后拨本地地址（`127.0.0.1:port` 或配置的映射地址）。这避免了 IP 包传输、MTU 和 netstack 的全部成本。
- **UDP**：两种实现二选一：
  - 直通（默认）：每个客户端 UDP 关联建立一条 datagram stream，数据报带 `[target addr][payload]`；对端用自己的 UDP socket 发往目标，回包按关联回送。
  - netstack（可选，2b）：每个目的地址一条 netstack UDP socket（研究一 §2.4 已实测）。
- 认证：默认只监听 `127.0.0.1`；如需监听非 loopback，必须显式 `--listen` 并打印警告（与 `-R` 的 `--listen-all` 同一策略）。
- 并发上限：`--max-conns` 默认 256；超出直接拒绝并记录。

**实测性能**

- netstack 路径（研究一 lab，双进程经真实 TCP 隧道）：单连接持续 **73–88 MiB/s**；200 并发 × 1 MiB 聚合 **3773.6 MiB/s**；CONNECT **+0.24 ms**；200 空闲流 RSS **24.5 MiB** `[实测]`。
- 直通路径：**未直接实测**。`[推断]` 与端口转发同量级（yamux + AEAD 库层 1957–2748 MiB/s，cmesh 全栈回环参照 539 MiB/s），且省掉两遍用户态 TCP，应显著高于 netstack 路径。阶段 2 必须实测并记录，不能引用库层数字冒充全栈数字。

**复杂度 / 依赖 / 适用场景**

- 复杂度：**中**（直通）/ **中高**（netstack 增强）。
- 依赖：直通零新依赖；netstack 需 gVisor（+2.35 MB，可选编译）。
- 适用：K8s 无特权容器、任意 TCP 客户端、需要“任意端口、无需预映射”的场景。

**目标映射规则（必须在阶段 2 之前定死）**

SOCKS5 收到 `100.64.0.6:8080` 时，对端不能直接“拨 100.64.0.6:8080”——那是虚拟地址，对端内核不认识它。规则是：

| 前置头里的目标 | 对端实际拨号 | 说明 |
|---|---|---|
| 对端自己的 mesh IP + 端口 | `127.0.0.1:端口` | 覆盖“服务监听 loopback”和“监听 0.0.0.0”两种最常见情况 |
| 对端自己的 mesh IP + 端口，且配置了 `--map` | `--map` 指定的真实地址 | 服务只监听某个具体 LAN IP 时用 |
| 其他 mesh IP | **拒绝**（`StatusDenied`） | v1 不做多跳；避免把节点变成路由器 |
| 非 mesh 地址（公网/其他内网） | **拒绝** | 防止把容器变成开放代理 |

`127.0.0.1` 的语义要特别说明：对端拨的是**它自己的** loopback，不是客户端的。这正是“免权限 SSH 接入”能成立的原因——容器里的 sshd 监听 `0.0.0.0:22` 或 `127.0.0.1:22` 都可以被访问到。

**边界**

- 应用必须显式配代理（`ALL_PROXY`、`--socks5-hostname`、`ProxyCommand`）。
- `ping` 仍不可用（SOCKS5 没有 ICMP 命令）。
- 目标不属于任何已连接 peer 时默认拒绝；**v1 不做多跳路由**，也不做 exit node。
- 服务若只监听一个具体的非 loopback 地址（例如 `172.17.0.5:5432`），必须用 `--map` 显式声明；否则拒绝。

### 4.3 模式 C：端口转发

**前置条件**

- 无。只要出站 TCP 可用。

**用户怎么用**

```bash
# ssh -L 等价：本地 8080 → 对端拨 db:5432
cmesh forward --code CODE -L 127.0.0.1:8080:db:5432

# 多条转发，一条隧道
cmesh forward --code CODE \
    -L 127.0.0.1:8080:db:5432 \
    -L 127.0.0.1:9090:api:80

# ssh -R 等价：对端监听 9000，回连本地 127.0.0.1:3000
cmesh forward --code CODE -R 127.0.0.1:9000:127.0.0.1:3000

# 接受端显式声明允许的目标（默认拒绝一切）
cmesh forward --code CODE --accept \
    --allow-host db.internal --allow-host 10.0.0.0/8 \
    --allow-port 5432 --allow-port 6379
```

**实现要点**

- 线协议：每条 stream 一个前置头 `kind(1) + addrLen(1) + addr`，接受端回 `Status uint32`（0=OK，1=Denied，2=DialFail，3=TooMany）。显式状态码让客户端能区分“策略拒绝”与“目标挂了”（研究二 §3.2 `[实测]` 的教训）。
- **策略永远在“实际发起连接的那一侧”执行**：`-L` 由远端检查，`-R` 由本地检查。
- `-R` 的目标地址必须来自**本地** `-R` 配置，不接受远端在 preamble 里随意指定（研究二 §3.4）。
- 零值 `AllowList` 拒绝一切；空 Hosts 不等于“允许全部”。
- `-R` 默认绑 `127.0.0.1`；`--listen-all` 必须显式且打印警告。
- 流数上限默认 256（信号量在 Open 与 Accept 两侧都限）；拨号超时 5 s。
- 重连：本地 listener 只建一次；隧道断开时关闭所有转发连接并让应用看到 EOF；不做 stream 级续传（研究二 §3.6）。

**实测性能**

- 原型：10/10 并发连接字节精确，2 ms 完成；白名单拒绝可见（status=1）`[实测]`。
- 库层：yamux + 消息适配 + 每消息 AES-GCM，1/10/50 流 = **1957 / 1962 / 2748 MiB/s**（回环）`[实测]`。
- cmesh 全栈参照：文件传输回环 **539.3 MiB/s**；跨公网 **11–12 MiB/s**（项目记录）。
- `[推断]` 端口转发全栈回环应在数百 MiB/s 量级，跨公网受链路限制。

**复杂度 / 依赖 / 适用场景**

- 复杂度：**低**。复用、流控、半关闭、keepalive 全部由 yamux 提供；cmesh 只写“适配 + 策略 + CLI”。
- 依赖：yamux 一个模块（+33 KB，零传递依赖，MPL-2.0）。
- 适用：固定端口映射、SSH、数据库、HTTP、CI 调试；**最小可用版本**。

### 4.4 三种模式对比总表

| 维度 | L3 TUN | SOCKS5 | 端口转发 |
|---|---|---|---|
| 前置条件 | `CAP_NET_ADMIN` + `/dev/net/tun` | 无 | 无 |
| 用户用法 | `ping 100.64.0.6`、`ssh user@100.64.0.6` | `ALL_PROXY=socks5://127.0.0.1:1080` | `cmesh forward -L 8080:db:5432` |
| 是否需要改应用 | 否（透明） | 是（配代理） | 是（改目标地址/端口） |
| 支持 TCP | ✅（内核栈） | ✅ | ✅ |
| 支持 UDP | ✅ | ✅（UDP ASSOCIATE） | ❌（v1 仅 TCP） |
| 支持 ICMP | ✅ | ❌ | ❌ |
| 任意端口、无需预映射 | ✅ | ✅ | ❌（每个端口一条 `-L`） |
| 入站（别人访问我） | ✅（监听 mesh IP） | ⚠️ 需在对端配 `-R`，不是 SOCKS5 本身的能力 | ✅（`-R`） |
| 实测性能 | 无直接实测；参照 73–88 MiB/s（双 netstack）/ 539 MiB/s（大帧） | 73–88 MiB/s（netstack 路径）；直通未实测 | 1957–2748 MiB/s（库层）；全栈参照 539 MiB/s |
| 实现复杂度 | 高 | 中 | 低 |
| 新依赖 | 无（x/sys 已有） | 直通 0；netstack +2.35 MB | yamux +33 KB |
| 目标容器可用 | ❌ | ✅ | ✅ |
| 适用场景 | 特权容器、VM、裸机、需要透明 IP | K8s 无特权容器、任意 TCP 客户端 | SSH/DB/HTTP、最小可用 |

### 4.5 能力矩阵（哪些应用能用）

| 应用 / 协议 | L3 TUN | SOCKS5 | 端口转发 |
|---|---|---|---|
| `curl` / `wget` | ✅ 透明 | ✅ `ALL_PROXY` / `--socks5-hostname` | ✅ 改 `127.0.0.1:PORT` |
| `ssh` | ✅ 直连 | ✅ `ProxyCommand` | ✅ `-p 2222` |
| `psql` / `mysql` / `redis-cli` | ✅ 透明 | ✅ 支持 SOCKS5 的客户端；否则 `ProxyCommand` | ✅ 改 host/port |
| 浏览器 | ✅ | ✅（系统代理） | ✅（改 URL） |
| `ping` / `traceroute` | ✅ | ❌ | ❌ |
| 任意 UDP（游戏、DNS、QUIC） | ✅ | ✅（UDP ASSOCIATE） | ❌ |
| 不感知代理的旧程序 | ✅ | ❌ | ❌（除非改目标地址） |
| SCTP | ✅（内核） | ❌ | ❌ |
| 静态链接、`LD_PRELOAD` 无效的程序 | ✅ | ❌ | ❌ |

---

## 5. 关键设计决策

### A1. mux 的位置：加密在外、复用在内（修正研究二的层次画法）

**研究二的结论**：`hashicorp/yamux`，插在 `secure` 之下、`wsconn`/`p2pquic` 之上，「每个 stream 承载一串已加密帧」。

**本设计的决定**：采纳其**意图**（整条连接一组密钥、不做每流加密），修正其**层次**。可实施的顺序是：

```
传输（wsconn / p2pquic，MessageConn）
  │
  ├─ 出站：yamux sendLoop → 记录泵 → secure.SealFrame(FrameStreamData) → transport.Send
  │
  └─ 入站：transport.Recv → secure.OpenFrame → 记录泵 → yamux recvLoop
        │
        └─ yamux（跑在记录泵提供的 io.ReadWriteCloser 上）
              ├── 控制流（initiator 先开，首字节 kind=0）：消息/文件
              └── 其余 stream：forward / proxy / packet
                  （ID 按 yamux 规则分配：initiator 用奇数、responder 用偶数）
```

也就是 **`transport → secure（帧级 AEAD）→ 记录泵 → mux → 业务流`**，与 SSH 的 transport/channel 分层一致。注意方向：加密在复用**之外**，所以 yamux 看到的是明文，但它在 secure 的保护范围之内，relay 仍然只看到密文。

**record pump 是什么**：一个约 60–120 行的适配器，是唯一接触 `secure.Session` 的组件。它有两个模式：

- **引导模式**（`FrameMuxStart` 之前）：`SendFrame`/`RecvFrame` 直通，行为与现在完全一致；hello 与 p2p 协商都跑在这里。
- **复用模式**（`FrameMuxStart` 之后）：下面的读/写两侧。

**复用模式的读侧（单 goroutine）**：`transport.Recv()` → `secure.OpenFrame()` → **按帧类型分流**。`FrameStreamData` 的 payload 追加到一条有界管道，yamux 的 `recvLoop` 从管道读。因为只有一个 goroutine 调 `OpenFrame`，全局计数器**天然按到达顺序递增**。复用模式下不应再出现控制帧；若出现（例如对端在屏障竞态里发了 hello），记录一条警告并丢弃——它们已经在屏障前被处理过。

**复用模式的写侧（单 goroutine）**：yamux 的 `sendLoop` 写管道 → pump 把字节块封成 `FrameStreamData` → `secure.SealFrame()` → `transport.Send()`。引导阶段的 `FrameHello`/`FrameMuxStart` 也走同一个写侧（同一个 `sendMu`），因此计数器顺序不会被打乱。`secure` 自己的 `sendMu` 仍然保证密封与发送的原子性。

**为什么不能按研究二 §2.2 的字面画法（mux 在 secure 之下、每流承载密文）**：

1. `secure.Session` 的计数器是**整条连接单一递增**的，`OpenFrame` 要求严格按发送顺序到达（`secure.go:399-417`）。
2. yamux 只有一个 `sendLoop` 和一个 `recvLoop`（研究二 §1.2 查证 `session.go:488,514`），`recvLoop` 在应用读取之前就按 streamID 把帧分发到各 stream 的接收缓冲区，**不保留跨 stream 的到达顺序**。
3. 如果多个 goroutine 并发读不同 stream 并各自调用 `OpenFrame`，它们会看到**乱序的全局计数器**，解密直接失败。
4. 若要让「mux 在 secure 之下」成立，secure 必须知道 streamID 并按流派生密钥——这正是研究二 §2.2 自己论证为「安全增益为零、改动更大」的方案，也与本设计 A2 冲突。

**为什么 record pump 比“把 secure 改成记录层”更好**：

- `secure.go` **一行都不用改**（研究二的核心诉求），风险最低；`SealFrame`/`OpenFrame` 的现有测试全部继续有效。
- 新增的只是帧类型 `FrameStreamData` 与屏障 `FrameMuxStart`，以及一个适配器。
- 代价是每条记录多 10 字节 wire 头；yamux 的单次写入通常远大于此（实测平均 81.8 KiB/条，研究二 §4.9），开销可忽略。

**一处需要说明的读法差异**：研究二 §2.5 的适配器草案是 `mux.NewClient(c peer.MessageConn, cfg)`，即 **mux 直接包在传输上、secure 在其上**——与它 §2.2 的图一致，但与它「每个 stream 承载已加密帧」的表述矛盾。本设计的 record pump 正是把这个可实施的方向补全：secure 仍然在 mux 之外，只是它作用于**一条有序的字节流**，而不是被拆到多条 stream 上。

**代价**：需要把 `peer.Session` 的传输从「直接持有 `secure.Session` + `MessageConn`」改为「在控制流上收发 `wire.Frame`」。改动约 100–200 行，属于阶段 1 的前置重构。

**一个必须处理的生命周期细节**：`peer.Session` 现在把 `secure.Session` 的 `ShortAuth`/`Fingerprint` 暴露给 `Node`，而 `Node` 在 `dialOne` 里先建 `peer.Session`、再打印 SAS。引入记录泵后，SAS 仍来自同一个 `secure.Session`（泵持有它），所以只需把访问器从 `peer.Session` 转挂到泵或 `Node`，不需要改密钥派生。

**协议版本：保持 1，不升级**（修正自阶段 1 详细设计）。帧格式未变，新帧类型是增量，
`readPump` 的 `default` 分支本来就忽略未知类型，所以旧二进制遇到新帧只会忽略而非
崩溃。而升到 2 会让 `exchangeHello` 直接返回 `ErrProtocol`（`session.go:218-253`
`[查证]`，无降级路径），**使现有的 `send`/`recv` 在新旧二进制之间立刻不可用** ——
为并不存在的不兼容制造了真实的功能倒退。

灰度改用 hello 里的独立 `caps` 字段（不参与版本校验），双方取交集决定是否启用
mux。版本号本身不做“降级协商”。

**证据**：`secure.go:399-417`、`session.go:423-431`、`session.go:218-253` `[查证]`；yamux `recvLoop`/`sendLoop` `[查证]`；研究二 §2.2–§2.5 `[实测/查证]`。

### A2. 加密边界：整条连接一组密钥，不做每流加密

**决定**：一次 SPAKE2，一个 session key，HKDF 派生出方向密钥；所有 stream 共享这一组密钥。不派生每流子密钥，不跑每流 SPAKE2。

**理由**：

- 每流 SPAKE2 = N 次 PAKE、N 个 RTT，代价大且要改握手。
- 每流子密钥 = 必须保证 nonce 空间不重叠，否则 AES-GCM nonce 重用是灾难性的（研究二 §2.2）。
- 安全增益为零：攻击者能看到的仍然只是“一条加密连接”。
- 现有 `sendMu` 已保证“密封 + 发送”的原子性；记录泵只需要保持**单写者**（yamux `sendLoop`）和**单读者**（记录泵的读 goroutine）。这也是 record pump 存在的根本原因：它把“多流并发”收敛回 secure 要求的“单序列”。

**证据**：研究二 §2.3 表；`secure.go` 的计数器与 HKDF 实现 `[查证]`。

### A3. MTU 固定 1200，禁止依赖 PMTUD

**决定**：所有 IP 包路径（TUN、netstack、packet stream）的 link MTU 固定 **1200**，不提供“自动探测”开关。TCP MSS 由 MTU 推导（约 1160）。

**理由（研究一 §3.3 `[实测]`）**：

```
netstack_mtu=1500 tunnel_capacity=1500 → 4 MiB OK
netstack_mtu=1500 tunnel_capacity=1200 → 1 MiB write timeout
netstack_mtu=1200 tunnel_capacity=1200 → 4 MiB OK
netstack_mtu=1500 tunnel_capacity=1400 → 1 MiB write timeout
netstack_mtu=1280 tunnel_capacity=1200 → 1 MiB write timeout
```

MTU 只要**大于**隧道实际可承载的值，TCP 在第一个满尺寸段就卡死，**不会**降速或重传小包——因为隧道是静默丢包，没有 ICMP Fragmentation Needed 反馈。`p2p.MaxDatagram = 1200`（`packet.go:39-42`），所以 1200 是硬上限。

**代价（已实测）**：MTU 1500 → 1200，单进程 netstack 吞吐 349.2 → 261.4 MiB/s（−25%），但换来“不黑洞”。

**验收**：必须有一个回归测试构造“隧道容量 < MTU”的场景，断言不挂死（研究一 §8.3 明确要求）。

### A4. IP 包必须聚合，不能一包一帧

**决定**：packet stream 上每帧承载**多个** IP 包，格式 `[len u16][IP 包]...`，目标 64 KiB/帧，上限 1 MiB。禁止“一个 IP 包一个 `wire.Frame`”。

**理由（项目自身实测）**：

- D28（1 GiB 回环）：1177 字节/包、每包一次 syscall + 一次 AEAD + 一次锁 → **13 MiB/s**；同场景 512 KiB/帧 → **约 512 MiB/s**。固定开销差约 40 倍。
- 研究一 §4.1 只写了“`wire.Frame` 增加 `FramePacket` 类型”，没有规定聚合粒度。如果按“一个 IP 包一帧”实现，会精确复现 D28：1200 字节 payload + 10 字节 wire 头 + 16 字节 AEAD + WS 头，固定开销占比超过 3%。
- 聚合 64 KiB/帧时，每包摊销的固定开销降到约 1/53。

**`[推断]`**：聚合能把 packet 路径从“十几 MiB/s”拉回“接近隧道大帧上限”。阶段 3 必须实测并记录，不能只引用推断。

### A5. 端口转发的安全边界：默认拒绝，四层防御

**决定**：转发能力默认关闭；接受端默认拒绝一切目标；`-R` 默认只绑 loopback；流数与速率有硬上限。

**威胁模型**：cmesh 的信任模型是“知道 code 的人就是合法对端”。但 code 可能泄露（截图、日志、共享屏幕），而**转发的破坏力远大于传文件**：拿到 code 的人可以把容器当作进入内网的跳板，扫描并访问容器能到达的任何地址（研究二 §3.4）。

**四层防御（全部默认拒绝）**：

1. **默认不启用**：不接受转发，除非显式 `--accept`。
2. **目标白名单**：`AllowList` 零值拒绝一切；空 Hosts 不等于“允许全部”；`0.0.0.0/0` 与 `::/0` 需要显式确认并打警告。
3. **监听地址约束**：`-R` 默认 `127.0.0.1`；`--listen-all` 显式且警告。
4. **流数 / 速率 / 超时**：`MaxStreams=256`、`MaxStreamsPerSec=32`、`DialTimeout=5s`。

**额外要求**：启用转发时**强烈建议** `--expect-sas`；SAS 不一致直接中止（现有 D38 已实现）。`-R` 的目标地址必须来自本地配置，不接受远端指定。

**绝对不做**：`--allow-any` 这类“一键放开”开关。

**证据**：研究二 §3.4 `[实测]`（白名单拒绝生效，status=1）；`docs/decisions.md` D38 `[查证]`。

### A6. 虚拟网段：默认 `100.64.0.0/10`，启动探测冲突

**决定**：默认 mesh CIDR 为 `100.64.0.0/10`（CGNAT），每个节点一个 `/32`；启动时探测冲突，冲突则拒绝启动并提示换 CIDR。

**理由（研究一 §3.2 `[实测]`）**：测试机上 `ping 10.99.0.5` 一度“成功”（72–189 ms，走 `10.178.117.146` 网关），查证后确认是**上游网络的地址冲突**，不是 netstack 的功劳。10.99.0.0/24 在真实链路上可能被路由，必须避开。研究一已实测 `100.64.0.0/10` 与 `240.0.0.0/4` 在本机不可达。

**分配**：无中心控制器，节点 ID 由名字派生（哈希取低 16 位），冲突在 hello 阶段检测：双方 mesh IP 相同则中止并提示显式 `--mesh-ip`。`[推断]` 哈希冲突概率在百节点规模下可忽略，但必须有检测。

### A7. DNS 与名字解析：在代理层做，不在 netstack 里跑 DNS 服务器

**决定**：SOCKS5 支持 `ATYP=0x03`；维护 `mesh 名字 → mesh IP` 表；未命中走系统 DNS（可关闭）。不在 netstack 里实现 DNS 服务端。

**理由**：

- gVisor **没有 DNS 解析器**（研究一 §1.5 `[查证]`：`grep -r "resolv.conf\|dnsmessage" pkg/tcpip/` 无命中）。
- 在 netstack 里跑 DNS 服务器需要额外实现报文编解码，而且无 TUN 时应用根本连不到那个虚拟 IP（研究一 §7.4）。
- 在代理层解析更简单，且已实测可行：`curl --socks5-hostname 127.0.0.1:1080 http://peer.mesh:8080/` 返回 200（研究一 §2.3）。

### A8. 传输切换只发生在 mux 建立之前

**决定**：p2p 打洞与 `UpgradeToDirect` 必须在 mux 建立**之前**完成；mux 建立后不再切换传输。

**理由**：

- 研究二 §2.4：如果 yamux 已在旧 socket 上建立状态，换 socket 后 `recvLoop` 会读到半个帧。
- 现有代码本来就只支持切换一次（`switchable.Switched()`），且 `StartWithNegotiation` 已经在 hello 之后、readPump 之前留了钩子。
- 顺序：握手 → hello → p2p 协商 + 切换 → mux 屏障 → 启动 readPump。

**代价**：直连升级失败后，本次会话只能留在 relay；下次重连再试。这与现有行为一致。

**实测补强（阶段 1 细化时发现）**：这条不只是「避免读到半个帧」，而是**必须**。
若切换发生在一条记录的中途，已读到的半条记录永久损坏（`TestPartialRecordAcrossSwitchIsLost`
实测读到 24/49 字节后连接关闭，剩余 25 字节再也不会到达），下一个帧与计数器/字节
边界都不对齐，`secure.OpenFrame` 必然失败，`fail()` 关掉两端——**所有 yamux 流同时断**。
所以 mux 模式下 `Switch` 直接返回错误，而不是尝试热切换。

### A9. 内存与并发上限：两套预算，各自硬顶

**决定**：

| 资源 | 默认上限 | 单条成本 | 预算 |
|---|---|---|---|
| yamux 活跃 stream | 256 | 514.8 KiB（@256 KiB 窗口） | `256 × 0.5 MiB ≈ 128 MiB` |
| netstack 活跃流（仅 2b） | 200 | ~1.1 MiB | `200 × 1.1 MiB ≈ 220 MiB` |
| 记录泵缓冲 | 每方向 1 MiB（32 × 32 KiB） | 1 MiB | 2 MiB |
| packet 聚合缓冲（仅阶段 3） | 每方向 1 帧上限 | 1 MiB | 2 MiB |

两套预算**不叠加使用**：2a 直通路径只有 yamux + 记录泵（约 130 MiB 上限）；只有启用 2b 的 netstack 路径才会同时出现 220 MiB 的 netstack 预算，此时应把 yamux stream 上限降到 64。

**理由**：

- yamux 每活跃流 ≈ 2 × `MaxStreamWindowSize`（研究二 §4.5 `[实测]`：256 KiB → 514.8 KiB；1 MiB → 2.0 MiB）。
- netstack 200 活跃流 RSS 238.4 MiB（研究一 §1.3 `[实测]`），来自 gVisor 默认 1 MiB 收发缓冲。
- K8s 256 MiB limit 下，netstack 安全并发约 150–200 条；必须设上限，否则 OOM。

**窗口选择**：默认 256 KiB（内存安全）；跨公网 RTT > 30 ms 时建议 1 MiB（BDP：256 KiB / 50 ms ≈ 40 Mbit/s；研究二 §4.7 `[实测]` 回环上 256 KiB 只比 1 MiB 慢 13%，但广域网上窗口是硬约束）。

### A10. 与现有文件传输共存：控制流承载现有帧

**决定**：mux 建立后，现有 `peer.Session` 的全部帧走一条专用的控制流（首字节 `kind=0`，由 initiator 在 mux 建立后立即打开）；转发/代理走其他 stream。文件传输的 512 KiB 分块语义不变。

**控制流的编码**：控制流是字节流，`peer.Session` 用现有的 `wire.DecodeFrame(io.Reader)` 从中逐帧解码（`frame.go:133-154` `[查证]`），不再直接接触 `secure.Session`。因此 `wire` 的帧格式完全复用，`ChunkSize=512 KiB`、续传、双向并发都不变；变化的只是“帧从哪来/往哪去”。

**理由**：

- 不破坏现有消息、双向文件传输、断点续传、SAS、重连逻辑。
- yamux 的每流流控保证一条慢速转发流不会饿死文件传输；单 `sendLoop` 的串行化会增加满载延迟（研究二 §4.8 `[实测]`：p50 18 µs → 191 µs），但绝对值可接受。
- 不需要给 `peer.Session` 增加 stream 概念；`wire.Frame.Stream` 字段继续空着（研究二 §6.3 明确不要用它做复用）。

**回归门槛**：阶段 1 完成后，现有文件传输回环吞吐下降不得超过 10%（以 539.3 MiB/s 为基线），全部现有测试与 `-race` 必须通过。

### A11. 重连语义：连接级重试，不做 stream 级续传

**决定**：隧道断开 → 所有 stream 关闭 → 应用看到 EOF；本地 listener 保留；重连后新连接走新 session。文件传输靠现有 `.part` + offset 续传；转发连接不续传。

**理由**：

- `Node.Run` 断线后会重建 `peer.Session`（`node.go:136-246`），所有 yamux stream 必然失效。
- 跨 session 恢复一条半开 TCP 流需要缓存全部未确认数据，复杂度远超收益（研究二 §3.6、§6.3）。
- listener 只建一次，避免端口短暂不可用与并发 bind 失败。

### A12. `p2p2` 的处置：冻结、不接入、保留复审

**决定**：

- **不**把 `p2p2` 接入 SD-WAN 的任何路径。
- 保留包与测试，在包注释、README、`decisions.md` 标注“experimental / not wired”。
- CI 继续跑它的测试，防止 bit rot。
- 6 个月内仍无接入计划则删除；删除前单独提交，不混在功能 PR 里。

**理由**：

- `p2p2` 4,710 行生产 + 4,806 行测试，**零外部导入**（`go list -deps ./cmd/cm | grep -c p2p2` = 0）`[实测]`。
- 它解决的是“UDP 上的可靠传输”，与“应用层多路复用”正交；包注释明确写 `Multiplexing ... are out of scope`（研究二 §5.3）。
- QUIC 已是默认直连传输，回环 219.5 MiB/s、跨公网 20 MiB / 7 s、丢包下正确（D43）；`p2p2` 的最好成绩是 145–165 MiB/s（0% 丢包）`[实测]`，且多一个需要维护的传输。
- 死代码不进二进制（未被链接），所以保留的代价是仓库维护，不是体积。

---

## 6. 分阶段落地路线

> 说明：阶段 1–3 是主线（“三阶段路线”），阶段 4 是可选的增强。每个阶段都以“可验收指标”结束，不达标不进入下一阶段。

### 阶段 1：端口转发（最小可用）

**目标**：用最小改动交付 `ssh -L`/`ssh -R` 等价能力，同时把 mux + secure 的地基打好。

**交付物**

1. `internal/mux`
   - yamux 包装：`Client`/`Server`、`Open`/`Accept`、流数信号量、`CloseWrite` 映射、`LogOutput` 显式设置（默认 `os.Stderr` 会污染 CLI，研究二 §6.2）。
   - 流前置头：`kind(1)`；forward 流再带 `Preamble{Kind, Addr}`。
   - 适配器：`MessageConn` ↔ `io.ReadWriteCloser`（yamux 需要字节流）。
2. **记录泵（record pump）**
   - 新增 `FrameStreamData`（payload = 任意字节块，≤32 KiB；写侧最多 16 条合并成一次 transport 写）与 `FrameMuxStart`（双向屏障）。
   - 单读者：`transport.Recv → secure.OpenFrame → 缓冲管道`；单写者：`缓冲管道 → secure.SealFrame → transport.Send`。
   - **引导阶段**：`FrameMuxStart` 屏障之前，hello 与 p2p 协商仍走现有的“帧直发”路径（`peer.Session.SendFrame/RecvFrame`），记录泵尚未启动；屏障之后所有帧都包在 `FrameStreamData` 里。这样 p2p 切换仍然发生在 mux 之前（A8），且引导阶段不需要 mux 存在。
   - `secure.go` **不改**；`peer.Session` 改为在控制流上收发 `wire.Frame`；`ShortAuth`/`Fingerprint` 上移到 Node。
   - **协议版本**：保持 1。新帧类型是增量，旧版 `readPump` 忽略未知类型；升版本反而会让现有 `send`/`recv` 在新旧二进制间不可用。灰度用 hello 的 `caps` 字段取交集。
3. `internal/forward`
   - `Preamble`/`Status`、`AllowList`（零值拒绝一切）、`Limits`、双向 pipe、半关闭。
4. `internal/cli/forward.go`
   - `-L`/`-R`（可重复）、`--accept`、`--allow-host`/`--allow-port`、`--listen-all`、`--max-streams`、`--dial-timeout`、`--expect-sas`。
   - `-L` 解析歧义：`8080:h:1`、`1.2.3.4:8080:h:1`、`[::1]:8080:h:1`、`8080:[::1]:1` 全部表驱动测试。
5. 文档：README 增加 `forward` 一节；`decisions.md` 新增 A1（mux 位置）、A2（加密边界）、A5（转发安全）、A10（控制流）、A11（重连语义）对应条目。

**验收标准**

| 编号 | 指标 | 门槛 |
|---|---|---|
| 1.1 | 并发正确性 | 10/10 连接字节精确（研究二原型已 10/10） |
| 1.2 | 半关闭 | HTTP/1.0 请求-响应不挂起；`Close` 后仍能读对端响应 |
| 1.3 | 白名单 | 未授权目标返回 `StatusDenied=1`，且拒绝发生在 dial 之前 |
| 1.4 | 默认安全 | 不传 `--accept` 时拒绝一切入站转发；`-R` 默认只绑 `127.0.0.1` |
| 1.5 | 流数上限 | 超过 `--max-streams` 返回 `StatusTooMany=3`，进程不崩 |
| 1.6 | 重连 | 杀掉 relay 后 listener 仍在；旧连接 EOF；重连后新连接可用 |
| 1.7 | 回环吞吐 | 单流 ≥ 200 MiB/s；10 流聚合 ≥ 500 MiB/s（`[推断]` 门槛，按实测调整） |
| 1.8 | 现有功能不回归 | 全部单测 + `-race` + e2e 通过；文件传输回环下降 ≤ 10% |
| 1.9 | 内存 | 100 条活跃转发 < 80 MiB RSS |

**风险**

- **yamux v0.x API 变动**：适配器隔离，`internal/mux` 只暴露自己的类型。
- **`StreamOpenTimeout` 失败会关整个 session**（研究二 §1.2 查证）：设长（120 s），并在 `Open` 前检查 `NumStreams`。
- **开放代理**：A5 四层防御；任何一层缺失都视为阻断性问题。
- **`secure` 改造风险**：本设计**不改 `secure.go`**，只加记录泵；`SealFrame`/`OpenFrame` 的现有测试全部继续有效。
- **记录泵的背压**：裸 `io.Pipe` 无缓冲，写侧会阻塞 yamux 的 `sendLoop`。用显式的有界缓冲（每方向 1 MiB，即 32 条 32 KiB 记录）并在缓冲满时阻塞写侧——这是正确的背压，不是 bug；但必须保证读侧 goroutine 永远在跑，且 `Close` 时同时关闭管道两端，否则整个会话停摆。缓冲之所以是 1 MiB 而不是几条记录，是因为写侧要凑够 16 条才能做一次批量写。
- **重连断流**：预期行为，文档写清楚，不做 stream 续传。

**工作量**：约 8 工程日，分 3 个 PR（研究二估 880 生产 + 900 测试行偏低：记录泵、控制流重构、`-R` 控制面与安全加固都会超；详细拆分见 [`STAGE1-PORTFORWARD.md`](STAGE1-PORTFORWARD.md) §7–§8）。

**依赖**：`hashicorp/yamux v0.1.2`（+33 KB，零传递依赖，MPL-2.0）。

---

### 阶段 2：SOCKS5 接入（直通为主，netstack 可选）

**目标**：任意 TCP 客户端用 `ALL_PROXY` 访问对端任意端口；UDP 可用；不要求应用预映射端口。

**交付物**

1. **2a 流直通（必做，阶段完成的门槛）**
   - `internal/proxy`：SOCKS5 `CONNECT`（IPv4/IPv6/域名）、`UDP ASSOCIATE`。
   - mesh 名表：`名字 → mesh IP → 会话`；未命中默认拒绝（可配系统 DNS 回退）。
   - TCP 直通：`kind=proxy` stream + 前置头（原始目标）；对端校验目标归属后拨本地地址。
   - UDP 直通：`kind=proxy-udp` datagram stream，`[target][payload]` 分帧。
   - CLI：`cmesh proxy --socks5 127.0.0.1:1080`。
2. **2b netstack 增强（可选，不阻塞阶段完成）**
   - `internal/netstack`（build tag `netstack`）：gVisor 封装、`gonet` 拨号、`tcp.NewForwarder`、UDP/ICMP。
   - 仅在“需要真实 IP 语义”的场景启用：ICMP echo、mesh 内 listener、未来 packet 路径复用。
   - 版本策略：pin `gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c`（保持 `go 1.24.0`，下载 4.8 MiB）；CUBIC 有溢出 bug（[google/gvisor#11632]），使用 `reno` 而非 `cubic`。
3. 文档：SOCKS5 使用示例、`ALL_PROXY`/`ProxyCommand`、DNS 行为、安全边界。

**验收标准**

| 编号 | 指标 | 门槛 |
|---|---|---|
| 2.1 | HTTP | `curl --socks5-hostname 127.0.0.1:1080 http://peer.mesh:8080/` 返回 200（研究一已实测同形态） |
| 2.2 | SSH | `ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@100.64.0.6` 拿到 banner（研究一已实测同形态） |
| 2.3 | 任意端口 | 不预映射，访问对端 3 个不同端口全部成功（含 loopback-only 服务） |
| 2.3b | 目标映射 | mesh IP → `127.0.0.1`、`--map`、拒绝其他 mesh IP、拒绝非 mesh 地址，四条规则各有测试 |
| 2.4 | UDP | UDP ASSOCIATE echo 通过（研究一已实测） |
| 2.5 | CONNECT 延迟 | p95 < 2 ms（研究一实测 0.52 ms） |
| 2.6 | 直通吞吐 | 单流回环 ≥ 200 MiB/s；跨公网不低于现状 80% |
| 2.7 | 内存 | 200 条空闲连接 < 40 MiB（netstack 路径实测 24.5 MiB）；活跃流上限生效 |
| 2.8 | 默认安全 | SOCKS5 只监听 `127.0.0.1`；非 mesh 目标默认拒绝；跨 peer 跳转拒绝；无 `--allow-any` |
| 2.9 | 构建可选 | 默认构建不含 gVisor；`-tags netstack` 构建增量 ≤ 2.5 MB |

**风险**

- **netstack 版本/Go 版本**：pin 2025-05 快照可保持 Go 1.24.0，但该快照已 18 个月未更新（安全/正确性风险）；升级到 2026-09 会强制 `go 1.26.3`。二选一必须在阶段 2 开始前决定。
- **MTU 黑洞**：如果走 netstack IP 路径，A3 固定 1200；必须有“隧道容量 < MTU”回归测试。
- **每流 1.1 MiB 内存**：A9 硬上限；K8s 256 MiB limit 下约 150–200 条。
- **`syscall.Conn` 缺失**：部分 gRPC/HTTP2 调优路径退化（研究一 §1.5）；SOCKS5 代理场景不涉及。
- **CUBIC 溢出**：旧快照用 `reno`。
- **DNS 泄露**：默认只解析 mesh 名；系统 DNS 回退必须显式开启。

**工作量**：2a 约 3–4 工程日（SOCKS5 服务端 ~240 行 + 直通 ~150 行 + CLI/测试）；2b 约 1.5–2 周（研究一估算 1,200–1,600 生产 + 800–1,200 测试行）。

**依赖**：2a 零新依赖；2b gVisor（Apache-2.0）。

---

### 阶段 3：L3 TUN

**目标**：在**有权限**的环境里提供真正的透明 L3：`ping`、`ssh user@mesh-ip`、任意 IP 协议。

**前置条件（硬门）**

- Linux；`cmesh probe` 退出码 0；`CAP_NET_ADMIN`；`/dev/net/tun`。
- 目标容器（`cap_eff=0`）**不满足**，`cmesh up` 必须拒绝并打印 `probe` 指引，不得 panic。

**交付物**

1. `internal/tun`
   - 设备创建（`TUNSETIFF`）、IP/MTU/路由配置；只加 mesh CIDR，不改默认路由。
   - 包泵：读 TUN → 聚合 → packet stream；反向拆包 → 写 TUN。
   - 聚合格式：`[len u16][IP 包]...`，目标 64 KiB/帧，上限 1 MiB。
   - 冲突探测：CIDR 与宿主路由/CNI 重叠时拒绝启动。
2. 协议：`kind=packet` stream；`FrameStreamData` 复用阶段 1 的记录泵。
3. CLI：`cmesh up --mesh-ip 100.64.0.5 --cidr 100.64.0.0/10`。
4. 测试：Linux 特权容器/VM 内的 e2e；macOS 上用假 TUN（socketpair/channel）做单元测试。

**验收标准**

| 编号 | 指标 | 门槛 |
|---|---|---|
| 3.1 | ICMP | `ping 100.64.0.6` 通，RTT 与物理路径同量级 |
| 3.2 | SSH 透明 | `ssh user@100.64.0.6` 无需 ProxyCommand |
| 3.3 | 任意 IP 协议 | TCP/UDP/ICMP 全部可用；SCTP 除外（内核支持但不在验收） |
| 3.4 | MTU 回归 | “隧道容量 < MTU”用例不挂死；MTU 固定 1200 |
| 3.5 | 吞吐 | 单流回环 ≥ 40 MiB/s；跨公网 ≥ 5 MiB/s（`[推断]` 门槛，按实测调整） |
| 3.6 | 断线 | TUN 与路由保留；重连后恢复；已建立 TCP 连接断开（预期） |
| 3.7 | 非特权 | `cmesh up` 在 `probe` 退出码 10 时拒绝启动，退出码明确，不 panic |
| 3.8 | 宿主安全 | 默认路由不变；只添加 mesh CIDR；冲突探测生效 |

**风险**

- **无法在开发机（macOS）验证真实 TUN**：必须有 Linux 特权测试环境；否则阶段 3 不能宣布完成。
- **包率与聚合实现**：聚合是性能关键；先用 64 KiB 目标，实测后调。
- **路由误配影响宿主网络**：只加 mesh CIDR、启动前打印将执行的路由变更、失败回滚。
- **地址冲突**：A6 的启动探测；`100.64.0.0/10` 也可能与运营商 CGNAT 重叠，必须可配。
- **TUN 模式的安全模型变化**：L3 等于把 peer 放进同一网段；需要文档明确“这是信任域决策”，并建议配合防火墙/AllowList。

**工作量**：1–1.5 周 + 特权测试环境准备。

**依赖**：无新第三方库。

---

### 阶段 4：监控与智能选路（可选）

**目标**：可观测 + 不抖动的路径选择；不做中心调度。

**交付物**

1. `internal/mesh`
   - 节点信息（mesh IP、名字、能力、relay、直连地址）。
   - 心跳与 RTT 采样（复用现有 P2P 测量，D42 的“没探测过的地址不能算 RTT”必须遵守）。
   - 本地拓扑快照与 RTT 矩阵。
2. `cmesh status --json`：本地会话、路径（relay/direct）、RTT、字节数、流数。
3. relay 只读状态端点 `/statusz`：房间数、连接数、转发字节数；**不暴露 room/code**（最多显示哈希前缀）。
4. 路径选择：relay vs direct，带迟滞（连续 N 次优/劣才切换）；不做按 RTT 选 relay（D12 已否决：两端可能选到不同 relay 永不相遇）。
5. 可选上报：`--report-to URL`，显式同意，默认关闭。

**验收标准**

| 编号 | 指标 | 门槛 |
|---|---|---|
| 4.1 | RTT 准确性 | 与独立 ping/QUIC RTT 对照，误差 < 10% |
| 4.2 | 不抖动 | 稳定链路 30 分钟内路径切换 ≤ 2 次 |
| 4.3 | 监控开销 | CPU < 1%，RSS < 2 MiB |
| 4.4 | 隐私 | relay `/statusz` 不含 code/room 明文；默认不上报 |
| 4.5 | 故障 | relay 状态端点不可用时，数据面不受影响 |

**风险**

- **全局调度需要中心**：v1 不做；任何“智能选路”都限制在**已建立会话内的路径选择**。
- **测量本身影响性能**：采样频率可配，默认低频率。
- **RTT 选择 relay 的陷阱**：D12 已明确否决，不要重新引入。
- **拓扑泄露**：上报默认关闭；relay 只给聚合。

**工作量**：1–2 周。

**依赖**：无新第三方库。

---

## 7. 风险清单

| 编号 | 风险 | 严重度 | 触发条件 | 证据 | 缓解 |
|---|---|---|---|---|---|
| R1 | **二进制体积 +25%**（gVisor） | 中 | 启用 2b / netstack 构建 | 研究一 §5.1 `[实测]`：+2.35 MB，+24.9% | build tag `netstack`，默认构建不含；阶段 2 的 2a 路径零 gVisor |
| R2 | **Go 版本被迫升级** | 中 | 使用 2026-09 及以后的 gVisor 快照 | 研究一 §5.3 `[实测]`：`go mod tidy` 后变成 `go 1.26.3` | pin 2025-05 快照保持 `go 1.24.0`；代价是 18 个月未更新 + CUBIC 溢出，用 `reno` |
| R3 | **开放代理** | **高** | 启用 `--accept` 且白名单配置错误 | 研究二 §3.4 `[实测]` | A5 四层防御；零值 AllowList 拒绝一切；`-R` 默认 loopback；`--expect-sas` |
| R4 | **MTU 黑洞** | **高** | IP 包路径 MTU > 隧道容量 | 研究一 §3.3 `[实测]`：write timeout，不降速 | A3 固定 1200；禁止 PMTUD；回归测试 |
| R5 | **内存耗尽** | 中 | 对端开大量流 / netstack 活跃流过多 | 研究二 §4.5、研究一 §1.3 `[实测]` | A9 双预算硬上限；信号量在 Open/Accept 两侧 |
| R6 | **虚拟网段冲突** | 中 | 默认 CIDR 与上游/CNI 重叠 | 研究一 §3.2 `[实测]`：10.99.0.5 被上游路由 | A6 默认 `100.64.0.0/10` + 启动探测；可配 |
| R7 | **许可证兼容性** | 低 | 引入 gVisor / yamux | gVisor Apache-2.0、yamux MPL-2.0；cmesh AGPL-3.0 | Apache-2.0 可被 AGPL 包含，保留 NOTICE；MPL-2.0 文件级 copyleft，不修改 yamux 源码；需 patch 时单独发布 |
| R8 | **重连导致所有转发断开** | 中 | relay 抖动 / 网络切换 | 研究二 §3.6 `[查证]` | listener 只建一次；断开时关闭所有转发连接；不做 stream 续传 |
| R9 | **relay 消息数增加约 6 倍** | 低 | 启用 mux 后 | 研究二 §4.9 `[实测]`：13 条/MiB，平均 81.8 KiB | 观察 relay CPU；必要时在适配器做 coalescing（研究二未跑通，需重验） |
| R10 | **yamux v0.x API 变动** | 低 | 升级 yamux | 研究二 §1.2 `[查证]` | 适配器隔离；协议 spec 稳定；pin 版本 |
| R11 | **`StreamOpenTimeout` 关整个 session** | 低 | 单条流 SYN 未被 ACK | 研究二 §1.2 `[查证]` | 设 120 s；Open 前检查 `NumStreams` |
| R12 | **旧 gVisor 快照安全/正确性** | 中 | 使用 2025-05 快照 | 研究一 §5.3–§5.4 `[查证]` | `govulncheck`；只用 `reno`；把“升级 Go”作为独立决策 |
| R13 | **无法在目标容器验证 TUN** | 高（阶段 3） | 目标容器无权限 | 研究一 §7.1 `[实测]` | 明确不可用；阶段 3 只在特权环境交付；CLI 拒绝并给 probe 指引 |
| R14 | **`p2p2` 维护负担** | 低 | 长期保留未接入代码 | `go list` 计数 0 `[实测]` | A12 冻结 + 6 个月复审；不投入新功能 |
| R15 | **SOCKS5 直通路径未实测** | 中 | 阶段 2 直接引用库层数字 | 本设计 `[推断]` | 阶段 2 必须实测全栈吞吐并记录；不达标则回退 netstack 路径 |

---

## 8. 不做的事

| 不做 | 原因 |
|---|---|
| **无 TUN 的透明 L3** | 物理不可能。内核没有接口承载 mesh 路由；`TUNSETIFF` 需要 `CAP_NET_ADMIN`。研究一 §7.1 `[实测]`。任何声称“无 TUN 也能 ping”的方案都是在骗人。 |
| **WireGuard / wireguard-go** | 它内部就是 gVisor 封装（研究一 §2.1 `[查证]`），会引入重复的 Noise 协议栈；cmesh 已有 SPAKE2 + AES-GCM。二进制还更大（11.84 vs 11.81 MB）。 |
| **smux** | v1 在 `Write` 返回成功的情况下静默丢数据（15 次 2 次，最多 22.0 MiB，研究二 §4.4 `[实测]`）。对 HTTP/DB/SSH 意味着随机字节损坏。 |
| **QUIC 原生多流作为复用基础** | 只在 p2p 直连时可用，relay（WebSocket）无法承载 QUIC；且吞吐低一个数量级（139–193 vs 1962–2748 MiB/s）。研究二 §1.4 `[实测]`。 |
| **每流加密 / 每流 SPAKE2** | 安全增益为零，改动大，nonce 空间风险高。A2。 |
| **用 `wire.Frame.Stream` 做复用** | 会要求 secure 从“严格有序帧通道”变成“可按流重排”，破坏防重放语义。让 yamux 在加密通道内部做。研究二 §6.3。 |
| **stream 级断线续传** | 需要缓存全部未确认数据，复杂度远超收益；TCP 连接本身有状态。研究二 §6.3。 |
| **在 netstack 里跑 DNS 服务器** | gVisor 没有 DNS 解析器；无 TUN 时应用连不到虚拟 DNS IP；代理层解析更简单且已实测。研究一 §7.4。 |
| **多跳路由 / exit node（v1）** | 需要路由协议、环路避免、策略传播；v1 只做成对隧道。目标不属于已连接 peer 时默认拒绝。 |
| **按 RTT 自动选 relay** | 两端可能选到不同 relay，永不相遇（D12 已否决）。智能选路只限“已建立会话内的 relay vs direct”。 |
| **SCTP** | gVisor 不实现；TUN 模式下内核支持但目标容器无 TUN。 |
| **Windows TUN（wintun）** | 不在目标场景；会引入 `golang.zx2c4.com/wintun` 依赖。 |
| **`--allow-any` 一键放开** | 开放代理的经典来源。需要放宽时写具体 CIDR。 |
| **修改 yamux 源码** | MPL-2.0 文件级 copyleft；需 patch 时单独发布 patch。 |
| **中心控制器 / 全局拓扑数据库** | 与 cmesh 的去中心化模型冲突；阶段 4 只做本地视图 + relay 聚合计数。 |
| **把 gVisor 作为默认依赖** | 2a 直通路径不需要它；默认构建保持精简。只有 2b/特殊 IP 语义场景才启用。 |

---

## 9. 里程碑与决策门

| 门 | 时点 | 通过条件 | 不通过的处理 |
|---|---|---|---|
| G0 | 阶段 1 开始前 | 确认 A1 的记录泵方案；确认 yamux 版本 pin | 回退到“帧级逻辑复用”（`Frame.Stream` + 自研 credit），但这会重新引入流控风险，需重新评估 |
| G1 | 阶段 1 结束 | 1.1–1.9 全部通过；文件传输回环下降 ≤ 10% | 不进入阶段 2；先修回归 |
| G2 | 阶段 2 开始前 | 决定 gVisor 快照与 Go 版本（R2）；确认 2a 直通路径的目标映射规则 | 若不能接受 Go 升级，2b 延后，只做 2a |
| G3 | 阶段 2 结束 | 2.1–2.9 通过（2b 可延后） | 不进入阶段 3；先补 SOCKS5 正确性 |
| G4 | 阶段 3 开始前 | 有 Linux 特权测试环境；`probe` 退出码 0 可复现 | 阶段 3 不启动（无法验收） |
| G5 | 阶段 3 结束 | 3.1–3.8 通过，尤其是 3.4 MTU 回归 | 不发布 TUN 模式 |
| G6 | 阶段 4 开始前 | 阶段 1–3 稳定运行；有真实跨公网数据 | 阶段 4 保持可选 |

**建议起点**：**阶段 1**。理由：

1. 零传递依赖（仅 yamux +33 KB），不引入 gVisor、不改 Go 版本。
2. 约 8 个工程日、3 个 PR 可验收，覆盖 SSH/数据库/HTTP 等绝大多数实际需求。
3. 顺带验证 mux + secure + 记录泵 + 传输切换的地基；阶段 2/3 都建立在这之上。
4. 风险最低：不涉及特权、不涉及 IP 包、不涉及 MTU；最坏情况是回退到现有单流实现。

---

## 10. 附录：证据索引

### 10.1 研究一（userspace-netstack）关键条目

| 主题 | 章节 |
|---|---|
| gVisor 无 TUN 提供完整 TCP/IP | §1.1–§1.4 |
| 内存/启动开销 | §1.3 |
| gVisor 能力边界（无 SCTP、无 DNS、无 `syscall.Conn`） | §1.5 |
| Tailscale 的 userspace 做法 | §2.1–§2.4 |
| 透明 L3 不可行 | §3.2、§7.1 |
| MTU 黑洞 | §3.3 |
| 吞吐与 CPU | §3.4 |
| 集成分层建议 | §4.1 |
| 三种方案（A 端口转发 / B SOCKS5+netstack / C L3） | §4.2–§4.3 |
| 二进制/依赖/Go 版本/代码量 | §5.1–§5.6 |
| 不可行方案（wireguard-go、runsc、netstack DNS） | §7.2–§7.4 |

### 10.2 研究二（mux-and-portforward）关键条目

| 主题 | 章节 |
|---|---|
| yamux / smux / QUIC 选型 | §1 |
| smux v1 静默丢数据 | §1.3、§4.4 |
| QUIC 原生多流不可用 | §1.4 |
| 内存与并发上限 | §1.5 |
| 插入层次与加密边界 | §2.2–§2.3 |
| 传输切换的影响 | §2.4 |
| 适配器设计 | §2.5 |
| 端口转发线协议与状态码 | §3.2–§3.3 |
| 开放代理四层防御 | §3.4 |
| CLI 设计 | §3.5 |
| 与 Node 的集成/重连 | §3.6 |
| 实测（隔离、半关闭、流控、完整性、内存、吞吐、延迟、消息数） | §4 |
| 成本（体积/代码量） | §5 |
| 与 p2p2 / p2pquic 的关系 | §5.3 |
| 实施建议与四件必须做对的事 | §6 |

### 10.3 项目内决策索引

| 决策 | 与本设计的关系 |
|---|---|
| D11 文件分块 512 KiB | 阶段 1 的回归基线（539.3 MiB/s） |
| D12 多 relay 有序列表 | 阶段 4 明确不做按 RTT 选 relay |
| D23/D28 P2P 直连默认关闭 | 传输层保持“relay 默认、direct 可选” |
| D38 SAS 强制比对 | 端口转发建议强制 `--expect-sas` |
| D43/D44 QUIC 为默认直连 | 不接入 p2p2；A12 |
| D47–D72 p2p2 的 KPI | 作为“自研可靠传输”的参考，不作为 SD-WAN 数据面 |
| D55 p2p2 未接入 CLI | A12 冻结策略的依据 |

---

*本文档只做设计与路线，不含实现代码。所有数字均标注了 `[实测]` / `[查证]` / `[推断]`；阶段验收门槛中的 `[推断]` 项必须在对应阶段用实测替换，不能沿用。*

---

## 阶段 1 详细设计对本文档的修正

阶段 1 的详细设计（[`STAGE1-PORTFORWARD.md`](STAGE1-PORTFORWARD.md)）在细化过程中
发现本文档有八处需要修正，其中三条是实质性的。这些修正已回写到上文，此处集中
记录以免混淆。

### 实质性修正

**1. `wire.Version` 保持 1，不升到 2。**
帧格式没有变化，新帧类型是纯增量，`readPump` 的 `default` 分支本来就忽略未知
类型。升版本会让 `exchangeHello` 直接返回 `ErrProtocol`（无降级路径），使现有的
`send`/`recv` 在新旧二进制之间立刻不可用——为并不存在的不兼容制造真实的功能
倒退。灰度改用 hello 的独立 `caps` 字段取交集。

**2. 记录大小是 32 KiB，配 16 条批量写（初版写 512 KiB，已推翻）。**
512 KiB 的依据是回环吞吐（512 KiB 分块 539.3 MiB/s vs 64 KiB 的 211.9 MiB/s），
但那把优化目标选错了：AEAD 在任何真实链路上都不是瓶颈（32 KiB 实测仍有
1408 MB/s），而一条 512 KiB 记录在 2 MiB/s 链路上要传 250 ms——这期间所有共享
连接的流都得等，等于给 SSH 会话加一个 250 ms 的队头阻塞。32 KiB 把这个下限降到
15.6 ms，同时用「16 条合并成一次 transport 写」把写次数拉回与 512 KiB 相同。
记录泵的缓冲因此是 1 MiB/方向（32 条），而不是 4×64 KiB。

**3. `-R` 需要独立授权与端口白名单。**
`-L` 和 `-R` 的风险量级不同：`-L` 由本端决定拨往何处，`-R` 让**对端**在本端
开监听器。因此需要独立的 `--accept-reverse` 开关，以及 `--allow-listen-port`
白名单（零值拒绝一切），否则对端可以抢占本端端口（例如绑掉本地 Redis 的 6379）。

### 其余修正

4. `Preamble` 类型放在 `wire` 包，避免 `peer` 与 `forward` 之间的导入环。
5. `Permits` 返回**要拨的 IP** 而不是 bool，封死 check-then-dial 的 TOCTOU 窗口。
6. `-R` 的 `Addr` 字段改为**监听器 ID**，多监听器按 ID 匹配，重连不重复 bind。
7. 记录泵的队列必须是**有界阻塞**而非丢弃：丢字节对数据库协议意味着静默损坏。
8. 泵的 `readLoop` 必须保留 `ErrTransportSwitched` 分支，否则 `--p2p` 升级会
   瞬间杀死会话——这是最可能的回归点。

### 验收口径

`bash tests/run-tests.sh` 全量运行实测 **39 项通过**（`--quick` 模式 35 项，跳过
4 项单测断言）。阶段 1 的验收基线是这 39 项全绿，且性能不低于 485 MiB/s
（现有 539.3 MiB/s 的 −10%）。

