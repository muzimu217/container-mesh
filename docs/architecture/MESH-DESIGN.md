# 阶段 2 详细设计：组网（Mesh）

> 三层文档的第三层。上层：[`../REQUIREMENTS.md`](../REQUIREMENTS.md)（要什么）、
> [`../DESIGN.md`](../DESIGN.md)（用什么结构）。
> 本文：**具体怎么写代码**——包结构、Go 接口、线协议、CLI、测试计划、按 PR 拆分的实施路线。
>
> 参照物：本文之于阶段 2，相当于 [`STAGE1-PORTFORWARD.md`](STAGE1-PORTFORWARD.md) 之于阶段 1。

**版本**：M1（2026-10-23）— 设计稿，未实现
**前置决策**（用户已确认）：
- 身份 = 密钥对（自建，不接第三方账号）
- 允许经第三方节点转发，但**只在直连失败时**
- 先 CLI 打通 N 台互传，界面后置
- 阶段 1（端口转发）冻结，能力保留复用

---

## 0. 摘要

五个核心结论，评审时先看这里：

1. **身份是密钥对，账号是设备证书的签发者。** 数据面按设备认证（每台独立，可单独撤销），
   管理层按账号聚合（`cmesh status` 显示「我的 N 台设备」）。撤销 = 从成员列表删公钥，
   不需要换任何密钥。

2. **发现节点不是中心。** 任何成员可兼任，客户端可配多个做故障切换。它只转发成员信息
   （deviceID、地址、公钥），**从不转发用户数据**。没有发现节点也能工作——只要节点间
   能通过某种渠道交换一次地址。

3. **两层加密是组网的本质区别。** 端到端层（新增，X25519+AES-GCM）只有通信双方能解；
   会话层（复用阶段 1 的 SPAKE2/TLS）每一跳独立。中转节点 B 转发 A→C 时，
   只能解开 A↔B 那跳，**解不开 A↔C 的端到端层**。

4. **路由是距离向量 + 实测 RTT，不做全局最优。** 几十台规模下全局最优（链路状态协议）
   的复杂度不值。直连永远优先；失败才选「RTT 最小且跳数最少」的中转；跳数上限 8 防环。

5. **中转有配额，防止被当免费跳板。** 单节点默认 ≤50 MiB/s、≤20 条流。超出只拒新请求，
   不影响自身业务。

**新增代码量预估**：约 1,900 行生产 + 2,000 行测试，分 5 个 PR。

---

## 1. 包结构

### 1.1 依赖方向（无环）

```
cmd/cm
 └── internal/cli
      ├── internal/identity    【新】不依赖任何项目内包
      ├── internal/discovery   【新】依赖 identity + wire
      ├── internal/mesh        【新】依赖 identity + discovery + wire
      ├── internal/e2e         【新】依赖 identity + wire
      ├── internal/forward     【阶段1 冻结】
      ├── internal/peer        【阶段1，小改】插入 e2e 层
      ├── internal/mux         【阶段1 复用】
      ├── internal/secure      【阶段1 复用】
      ├── internal/relay       【阶段1，演化】→ 发现节点
      ├── internal/p2p         【阶段1 复用】
      ├── internal/p2pquic     【阶段1 复用】
      └── internal/wire        【扩展】新帧类型
```

**约束**：`internal/identity` 不导入任何项目内包（它是地基）。
`internal/e2e` 不导入 `mesh`（加密不该知道路由的存在）。

### 1.2 新建文件

| 文件 | 职责 | 预估行数（生产/测试） |
|---|---|---|
| `internal/identity/identity.go` | 密钥对生成/加载/存储、deviceID 派生 | 200 / 180 |
| `internal/identity/account.go` | 账号密钥、设备证书签发与验证、撤销列表 | 250 / 220 |
| `internal/discovery/member.go` | 成员记录、序列化、验证 | 150 / 130 |
| `internal/discovery/node.go` | 发现节点服务端（成员注册、心跳、广播） | 300 / 280 |
| `internal/discovery/client.go` | 客户端（注册、心跳、接收广播） | 200 / 180 |
| `internal/mesh/route.go` | 路由表、距离向量、防环、选路 | 300 / 280 |
| `internal/mesh/relay.go` | 中转转发、配额执行 | 250 / 220 |
| `internal/mesh/agent.go` | 节点主循环：维护连接、响应拓扑变化 | 300 / 280 |
| `internal/e2e/e2e.go` | 端到端加密：X25519 ECDH + AES-GCM 封装 | 250 / 230 |
| `internal/cli/mesh.go` | `init` / `join` / `status` / `dial` / `serve-discovery` | 350 / 250 |

合计约 2,550 生产 / 2,260 测试。（DESIGN.md 写 1,900 是不含 CLI 的核心库。）

### 1.3 既有包的改动

| 包 | 改动 | 不改的部分 |
|---|---|---|
| `internal/wire` | 新增 8 个帧类型（§3.1） | 既有帧一字不动 |
| `internal/peer` | `Session` 之上插入可选的 e2e 层 | 记录泵、文件传输不动 |
| `internal/relay` | 新增 `/mesh/discover` HTTP 端点（发现服务） | 既有 `/ws` 配对不动（阶段 1 兼容） |
| `internal/cli` | 新增 5 个子命令 | 既有命令不动 |

---

## 2. 核心接口

### 2.1 身份层（`internal/identity`）

```go
package identity

// Device is one machine's long-term identity.
//
// The private key never leaves this machine. The device ID is derived from
// the public key, so it is self-certifying: given the ID and the public key,
// anyone can verify they match without asking a server.
type Device struct {
    priv    ed25519.PrivateKey
    pub     ed25519.PublicKey
    // ID is base32(SHA-256(pub))[:16] — that is 16 base32 CHARACTERS
    // (= 80 bits of the digest), NOT 16 bytes and NOT hex. Written out
    // for the implementer because "[:16]" on a byte slice versus on a
    // hex string versus on a base32 string are three different lengths,
    // and PR-M1 (identity) and PR-M4 (mesh) must agree byte-for-byte.
    //
    // Canonical form: base32.StdEncoding.WithPadding(NoPadding),
    // lowercase. Example length: exactly 16 runes.
    ID      DeviceID
}

// DeviceID is exactly 16 lowercase base32 characters (80 bits).
// Not hex, not raw bytes — see the field comment above.
type DeviceID string

// Generate creates a fresh identity. Called once per machine, by `cmesh init`.
func Generate() (*Device, error)

// Load reads an encrypted identity file. The key is encrypted with a key
// derived from a passphrase via Argon2id, because containers have no keychain
// and an unencrypted private key on disk means anyone with file access owns
// the device.
func Load(path, passphrase string) (*Device, error)

// Save writes the encrypted identity file.
func (d *Device) Save(path, passphrase string) error

// Sign / Verify are the primitives everything above builds on.
func (d *Device) Sign(msg []byte) []byte
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool


// Account is the "same identity, many devices" anchor.
//
// An account key is just another Ed25519 key pair, kept offline or on the
// most trusted device. It signs device certificates. The data plane never
// uses the account key -- if it did, compromising one device would
// compromise all of them, which is exactly what per-device keys prevent.
type Account struct {
    priv ed25519.PrivateKey
}

// DeviceCert binds a device to an account. Any member can verify it with the
// account public key alone; no server is involved.
type DeviceCert struct {
    DeviceID  DeviceID
    DevicePub ed25519.PublicKey
    AccountID string           // base32(sha256(accountPub))[:16]
    IssuedAt  time.Time
    ExpiresAt time.Time         // default 1 year; renewal re-signs
    Sig       []byte            // account's signature over the above
}

func (a *Account) IssueCert(dev *Device, ttl time.Duration) (*DeviceCert, error)
func (c *DeviceCert) Verify(accountPub ed25519.PublicKey) error


// Revocation. A mesh keeps a member list; removing a deviceID from it
// revokes that device. Because the data plane authenticates per device,
// no key rotation is needed -- the removed key simply stops being accepted.
//
// IMMEDIACY (this is the part a review caught missing): a removed key stops
// being accepted at the next handshake, but an ALREADY-ESTABLISHED e2e
// session holds a cached symmetric key and would keep working. So removal
// must also broadcast: the MemberList version bumps, the discovery node
// pushes it on the Subscribe stream (push, not poll -- that is why
// Subscribe exists), and every node that receives a higher version with
// deviceID X removed tears down X's active e2e.Session and drops X's
// route-table entries. Target: a revoked device loses its live sessions
// within one push round-trip, not within a heartbeat period.
//
// Partitions: a revoked node that never hears the broadcast keeps its cached
// sessions until reconnect. Mitigation: every e2e frame carries the member-
// list version it was sealed under; on receipt the receiver compares, and a
// session whose version is older than a revocation it knows about is dropped.
// The revoked side cannot forge a newer version -- it cannot sign as a member
// it no longer is.
type MemberList struct {
    AccountID string
    Members   map[DeviceID]Member
    Version   uint64          // bumps on every change; members gossip it
}

type Member struct {
    Cert     DeviceCert
    AddedAt  time.Time
    Endpoints []string        // candidate addresses, updated by heartbeats
    LastSeen time.Time
}
```

**为什么 deviceID 是 `sha256(pub)[:16]` 而不是直接用公钥**：
公钥 32 字节太长没法念、没法手输。16 字符 base32 有 80 bit 熵，
撞名概率在几十台规模下是零，且从 ID 找公钥靠成员列表，不需要 ID 承载全部信息。

### 2.2 发现层（`internal/discovery`）

```go
package discovery

// Node is the rendezvous service. Any member can host one; it is not a
// centre because (a) clients hold a list and fail over, and (b) it carries
// member information only -- never user data.
type Node struct {
    // ... config, members, logger
}

// Register adds or refreshes a member. The heartbeat is a Register with the
// same deviceID: it updates Endpoints and LastSeen.
//
// Authentication: the payload is signed by the device key. The node verifies
// it against the MemberList before accepting, so a stranger cannot inject
// itself by knowing the node's address (R-2.4).
func (n *Node) Register(m MemberAnnounce, sig []byte) error

// MemberAnnounce is what a device sends on heartbeat.
type MemberAnnounce struct {
    DeviceID  identity.DeviceID
    Endpoints []string      // public + local candidates
    Timestamp time.Time
}

// Subscribe streams membership changes to a connected member.
// Push, not poll: topology changes must converge in seconds (R-2.2), and a
// 30 s poll would make the convergence bound 30 s by itself.
func (n *Node) Subscribe(deviceID DeviceID) (<-chan MemberList, error)

// Sweep drops members whose heartbeat timed out (default 90 s) and
// broadcasts the removal.
func (n *Node) Sweep()


// Client is what every node runs.
type Client struct {
    // ...
}

// Announce sends a heartbeat. Called every HeartbeatInterval (30 s default).
func (c *Client) Announce(ctx context.Context) error

// Updates returns the membership-change stream, merged across all configured
// discovery nodes. The mesh layer consumes this.
func (c *Client) Updates() <-chan MemberList
```

**传输**：复用 `wsconn`（WebSocket + TLS）。发现节点就是 relay 进程上的一个新端点
`/mesh/discover`，不新起进程。

**成员信息加密**（R-2.4）：`MemberList` 用网络密钥（account 公钥协商出的对称密钥）加密。
发现节点本身存的是密文？**不**——发现节点是成员之一才能解密吗？

这是设计里最微妙的一点，明确写死：

> **发现节点以明文持有成员列表，但只在内存里，且它自己必须是成员。**
> 理由：发现节点要验证新成员的签名、要向成员推送列表，它必须能读。
> 约束「必须是成员」意味着：网络里至少有一台可信机器跑发现服务。
> 如果连这个都不想给，就不部署发现节点——节点间手工交换一次地址也能组网（§6.2）。

### 2.3 端到端加密层（`internal/e2e`）

```go
package e2e

// Session encrypts between two deviceIDs, regardless of how many hops
// carry the bytes. A relay in the middle sees ciphertext and nothing else.
//
// STREAMS INSIDE THE SESSION (a review caught this undefined): the per-hop
// yamux cannot give A and C end-to-end streams -- B terminates it. So the
// e2e.Session itself is a byte stream, and a SECOND yamux runs INSIDE it:
//
//   direct:   [app streams] -> yamux(e2e) -> E2E AEAD -> QUIC stream
//   relayed:  [app streams] -> yamux(e2e) -> E2E AEAD -> yamux(trunk,A-B)
//                                          -> hop conn -> yamux(trunk,B-C)
//
// B sees one trunk stream carrying opaque bytes; the A<->C stream structure
// is inside the ciphertext. The cost is one extra mux header layer, which
// we accept because it reuses the stage-1 machinery unchanged (P-3 must
// still be measured; if the double header costs too much, the fallback is
// an explicit StreamID+window field in FrameMeshData, documented in X-risk
// table X8).
type Session struct {
    // ...
}

// Streams returns the end-to-end multiplexer: many streams between the two
// endpoints, invisible to any relay. This is what forward.Manager's
// Provider hands out, so stage-1 -L/-R mounts on it unchanged (see PR-M5).
func (s *Session) Streams() *mux.Session

// Handshake performs an X25519 exchange, authenticated by both devices'
// Ed25519 identity keys (the signatures prove who did the ECDH, which is
// what stops a relay from inserting itself as a man in the middle).
//
// The handshake messages travel over whatever path exists -- direct or
// relayed. A relay forwards them opaquely; it cannot rewrite them without
// invalidating the signatures.
func Handshake(who *identity.Device, peer identity.DeviceID,
    peerPub ed25519.PublicKey, io io.ReadWriter) (*Session, error)

// Seal / Open handle one frame. Frame format is the existing wire.Frame,
// wrapped so the relay sees only opaque bytes plus the next-hop address.
func (s *Session) Seal(f *wire.Frame) ([]byte, error)
func (s *Session) Open(b []byte) (*wire.Frame, error)

// CipherSuite is negotiated in the handshake, one byte:
//   0x01 = AES-256-GCM        (default; hardware-accelerated with AES-NI)
//   0x02 = ChaCha20-Poly1305  (for low-power ARM without AES-NI)
// The byte exists so the choice is explicit on the wire rather than implied
// by the build; unknown values fall back to refusing the handshake rather
// than guessing, so a future suite can be added without a version bump.
func (s *Session) CipherSuite() byte
```

**为什么不用 SPAKE2**（阶段 1 的选择）：SPAKE2 的前提是双方共享一个口令。
组网里双方共享的是**公钥**，正确原语是签名认证的 ECDH（SIGMA 模式）。

**组网会话从建立起只有一层用户态 AEAD（E2E），不再叠加逐跳 SPAKE2。**
早一版图示曾把两者叠加，加上 QUIC 自带的 TLS 共三重 AEAD，用户态跑两遍
AES-GCM，直接威胁 P-3 的 200 MiB/s。E2E 的签名认证已提供 SPAKE2 的全部
功能，逐跳再加密一次不增加攻击者需要攻破的任何东西。SPAKE2 只保留在
阶段 1 兼容路径上，两条路径不混用。

### 2.4 路由层（`internal/mesh`）

```go
package mesh

// Agent is the per-node mesh brain. One per process.
type Agent struct {
    // ...
}

// RouteTable maps every known deviceID to how we would reach it.
type RouteTable struct {
    // ...
}

type Route struct {
    Peer     identity.DeviceID
    Direct   bool             // true = we have a live direct connection
    Via      identity.DeviceID // set when !Direct: who forwards for us
    RTT      time.Duration     // measured, not configured
    Hops     int               // ≤ MaxHops (8)
    Updated  time.Time
}

// Best returns the preferred route. Direct beats everything; among relayed
// routes, lower RTT wins, ties broken by fewer hops.
//
// Deliberately simple. Global optimality (link-state) needs full-topology
// sync whose cost is not worth it below ~100 nodes (N-6).
func (t *RouteTable) Best(peer identity.DeviceID) (Route, bool)

// Advertise produces this node's distance vector for gossiping. Includes
// split-horizon filtering: entries learned from X are not advertised to X.
func (t *RouteTable) Advertise(to identity.DeviceID) []RouteEntry


// RelayQuota caps how much forwarding one node does (R-3.6, P-7).
type RelayQuota struct {
    MaxBytesPerSec int64  // default 50 MiB
    MaxStreams     int    // default 20
}

// Forwarder carries bytes for a pair that cannot reach each other directly.
//
// What a forwarder can see: the next hop for each direction, and byte
// counts (unavoidable metadata). What it cannot see: the plaintext, or the
// end-to-end keys -- those live in the e2e layer between the two endpoints.
type Forwarder struct {
    // ...
}

// AcceptRelay decides whether to carry a new relayed stream. Refusing is
// normal and costs nothing; the requester tries the next candidate route.
func (f *Forwarder) AcceptRelay(from, to identity.DeviceID) error
```

**防环**：`MaxHops = 8`。每个转发的帧带 TTL，每过一跳减一，归零丢弃。
加上 split-horizon，足够防止几十台规模内的环路。

### 2.5 会话组装（改动 `internal/peer`）

```go
// Option to attach the end-to-end layer above the existing session.
//
// Layer order in a relayed path (A → B → C):
//
//   [e2e: A↔C]            only A and C hold these keys
//   [session: A↔B]        hop 1, reused stage-1 machinery
//   [record pump + yamux] unchanged
//   [session: B↔C]        hop 2, B↔C's own keys
//
// B sits between two session layers. It forwards frames between them. The
// e2e layer is opaque to B because B lacks A↔C's shared secret.
type WithE2E(*e2e.Session) peer.Option
```

---

## 3. 协议扩展

### 3.1 新增帧类型（`internal/wire`）

```go
const (
    // ... 既有帧类型不变 ...

    // --- 组网（阶段 2）---

    // FrameMeshAnnounce is a discovery heartbeat or a pushed member list.
    // Payload: encrypted MemberAnnounce / MemberList JSON.
    FrameMeshAnnounce

    // FrameMeshRoute is a distance-vector advertisement between peers.
    // Payload: encrypted []RouteEntry JSON.
    FrameMeshRoute

    // FrameMeshRelayReq asks the peer to forward to a third deviceID.
    // Payload: encrypted {to DeviceID, ttl int}.
    FrameMeshRelayReq

    // FrameMeshMembers carries a versioned MemberList push: joins, leaves,
    // and REVOCATIONS. Every e2e frame also carries the member-list version
    // it was sealed under, so a node that knows about a revocation drops a
    // session still running on an older version (see identity.MemberList).
    FrameMeshMembers

    // FrameMeshRelayAns answers it: ok / refused (quota) / unreachable.
    FrameMeshRelayAns

    // FrameE2EHello / FrameE2EResp carry the X25519 handshake, signed.
    FrameE2EHello
    FrameE2EResp
)
```

**版本策略不变**（C-4）：`wire.Version` 保持 1，`caps` 数组新增 `"mesh"`。
旧二进制忽略未知帧类型，文件传输不受影响——阶段 1 的 e2e 兼容性因此自动保留。

### 3.2 发现节点 HTTP API

```
POST /mesh/register    身体: MemberAnnounce + 签名       → 200 / 401
GET  /mesh/members     WebSocket 升级，推送 MemberList 流  → 101
GET  /healthz          存活（已有）
```

发现节点与阶段 1 的 relay 跑在**同一个进程**（`cmesh relay` 加参数即启用发现），
复用它的限流、看板与部署方式。

---

## 4. CLI 设计

### 4.1 新命令

```
cmesh init [--path ~/.cmesh]                生成设备身份（交互输口令）
cmesh account create                        生成账号密钥（最可信的机器上做一次）
cmesh account device-add [--device PATH]    给本设备签发证书
cmesh join <invite>                         用邀请加入网络
cmesh invite                                生成邀请（成员机器上）
cmesh status                                列出成员、在线状态、RTT、路由
cmesh dial <deviceID|name> [--cmd ARGS]     与某设备建立交互
cmesh send-to <deviceID> --file PATH        点对点传文件（按设备名）
cmesh serve-discovery                       本机兼任发现节点
cmesh mesh ping <deviceID>                  自定义 RTT 测量（非 ICMP）
```

### 4.2 与既有命令的关系

`send` / `recv` / `forward` **原样保留**（阶段 1 兼容）。组网命令是**新增**，
不是替换。用户可以只用阶段 1 的两人模式，也可以升级到组网。

### 4.3 邀请格式

```
cmesh://join?acct=<accountID>&disc=<discovery-addr>&k=<account-pub-b32>
```

一段可复制/可扫的字符串。含账号公钥与发现节点地址，**不含任何秘密**。
加入仍需成员批准（`R-1.5`：不能靠拿到邀请就进网）——批准动作 = 现有成员把新设备的
证书加进 MemberList 并广播。

---

## 5. 安全设计

### 5.1 威胁模型

| 攻击者 | 能做什么 | 防线 |
|---|---|---|
| 被动监听（ISP、relay 运营方） | 看到密文与元数据（大小、时间） | 端到端加密；流量大小不隐藏（明确不目标） |
| 主动的发现节点 | 拒绝服务、注入假成员 | 成员签名验证：假成员进不了 MemberList |
| 恶意成员 | 尝试读别人的流量 | e2e 层密钥只有通信双方有 |
| 恶意成员 | 尝试成为 MITM | 握手签名绑定身份密钥 |
| 恶意成员 | 大量中转请求拖垮某节点 | 配额（R-3.6） |
| 拿到一台设备私钥 | 冒充该设备 | 撤销：删公钥即出局；其他设备不受影响 |
| 拿到发现节点全部内存 | 成员列表（ID+地址+公钥） | 无秘密；私钥永不离开各自设备 |

### 5.2 与阶段 1 安全模型的差异

| | 阶段 1 | 阶段 2 |
|---|---|---|
| 信任前提 | 共享 code 即对端 | 成员列表里的公钥即对端 |
| 中继可见 | 密文（看不到明文）✓ | **密文**（同样）✓ |
| 中继可见 | 房间号（code 的派生） | **无**（发现节点持成员列表，但那是成员信息不是口令） |
| MITM 检测 | SAS 人工比对 | 握手签名自动验证（更严格） |
| 撤销 | 换 code（全网重配） | 删一个公钥 |

**注意**：阶段 2 的身份模型严格强于阶段 1。SAS 人工核对在组网里不再必要，
因为握手由密钥签名认证——但保留 `--expect-sas` 于阶段 1 路径。

### 5.3 明确不防

- 流量分析（谁在什么时候传了多少字节）——元数据不隐藏
- 恶意成员对它**自己**的流量做坏事——它本来就能看自己的
- 发现节点 DoS——只能靠多发现节点缓解
- 全球被动攻击者关联设备行为——那需要流量混淆，明确超出范围

---

## 6. 部署形态

### 6.1 典型（一台云服务器 + N 台内网机器）

```
云服务器（有公网 IP）
  cmesh relay --mesh-discovery   ← 阶段1 relay + 发现节点，同一进程

笔记本 / NAS / K8s Pod（全部 NAT 后）
  cmesh join cmesh://join?...
  cmesh status                   ← 看到彼此
  cmesh send-to nas --file big.bin
```

### 6.2 无任何公网节点

发现节点不可用时，`cmesh join` 支持从**文件**导入成员列表
（在能互通的渠道里手工传一次），之后靠成员间直连与 gossip 维护拓扑。
这覆盖「两台都在内网、有办法交换一个文件」的场景。

### 6.3 与阶段 1 部署共存

已有的 relay 二进制升级后自动支持 `/mesh/*` 端点；不升级的 relay 继续做阶段 1
配对，组网客户端不使用它们。**不破坏任何现有部署。**

---

## 7. 测试计划

### 7.1 单元测试（每包）

| 包 | 关键用例 |
|---|---|
| `identity` | 生成→加载往返；口令错误拒绝；证书签发→验证→过期→拒绝；deviceID 派生稳定 |
| `discovery` | 注册→心跳→超时剔除；签名伪造拒绝；订阅推送在 1 s 内到达 |
| `e2e` | 握手往返；篡改握手→拒绝；中转路径上握手仍成立（模拟 B 转发） |
| `mesh` | 距离向量收敛；split-horizon（不回灌）；TTL 归零丢弃；配额满→拒新流不杀旧流 |
| `wire` | 8 个新帧的往返 + 拒绝表 |

### 7.2 集成测试（真实多进程）

| 编号 | 场景 | 通过条件 |
|---|---|---|
| M1 | 3 节点、1 发现节点，全部直连 | `status` 互相可见 ≤10 s；两两 `mesh ping` < 5 ms（同机） |
| M2 | 阻断 A↔C 直连（iptables DROP UDP） | 10 s 内 A→C 自动经 B；仍可传文件且哈希一致 |
| M3 | 撤销 B | B 的活跃 e2e 会话在一个推送往返内被销毁；B 无法重连；A↔C 不受影响 |
| M3b | `kill -9` 一个节点 | 其余节点 ≤10 s 内感知（传输层断连检测，非心跳等待） |
| M4 | 中转配额 | 打满 20 条流后第 21 条被拒；B 自身吞吐不受影响 |
| M5 | 发现节点下线 | 成员拓扑靠既有连接维持 ≥60 s；发现节点回来后收敛 |
| M6 | 中转节点抓包 | 只有密文（`tcpdump` + 人工检查） |
| M7 | 旧二进制 + 新二进制混布 | 阶段 1 的 send/recv 仍工作 |

### 7.3 性能门槛

| 编号 | 指标 | 门槛 |
|---|---|---|
| MP-1 | 同机 3 节点两两 `mesh ping` | p95 < 5 ms |
| MP-2 | 直连文件吞吐（512 MiB） | ≥ 200 MiB/s |
| MP-3 | 经 1 跳中转的吞吐 | ≥ 50 MiB/s |
| MP-4 | 拓扑收敛（节点下线→其余感知） | < 10 s |
| MP-5 | 20 节点（模拟）时单节点路由表内存 | < 10 MiB |

---

## 8. 分步实施（按 PR 拆分）

每个 PR 独立可合并、可回退，顺序保证「每步之后主干都是可发布的」。

### PR-M1 —— 身份层（约 450 生产 + 400 测试）

| 交付 | 内容 |
|---|---|
| `internal/identity` | `Device`、`Account`、`DeviceCert`、`MemberList` |
| `cmesh init` / `account create` / `account device-add` | 本地可用，无网络 |

**验收**：两个身份文件能互相验证证书；错误口令加载失败；撤销后 `Verify` 拒绝。
**回滚**：纯新增包 + 新命令，不影响任何既有功能。

### PR-M2 —— 发现层（约 650 生产 + 590 测试）

| 交付 | 内容 |
|---|---|
| `internal/discovery` | Node（服务端）+ Client |
| relay 集成 | `/mesh/register`、`/mesh/members` |
| `cmesh serve-discovery` / `join` / `invite` | |

**验收**：M1 集成场景（3 节点互相可见 ≤10 s）。
**依赖**：PR-M1。

### PR-M3 —— 直连（约 400 生产 + 350 测试）

| 交付 | 内容 |
|---|---|
| 打洞接入组网 | 复用 `p2p` + `p2pquic`，候选地址来自成员列表而非手工配置 |
| `cmesh dial` / `mesh ping` | |

**验收**：任意两台 `dial` 直连成功；`mesh ping` p95 < 5 ms（同机）。
**依赖**：PR-M2。

### PR-M4 —— 端到端加密 + 中转路由（约 800 生产 + 800 测试）

| 交付 | 内容 |
|---|---|
| `internal/e2e` | SIGMA 握手 + 帧封装 |
| `internal/mesh` | 路由表、距离向量、Forwarder、配额 |
| peer 集成 | `WithE2E` 选项 |
| `cmesh send-to` | 按设备名传文件 |

**验收**：M2（阻断直连仍可通）、M4（配额）、M6（抓包无明文）。
**依赖**：PR-M3。这是风险最高的 PR，单独提交便于 bisect。
**回滚**：`WithE2E` 不启用时行为与 PR-M3 完全一致。

### PR-M5 —— CLI 完善 + 文档（约 250 生产 + 120 测试）

| 交付 | 内容 |
|---|---|
| `cmesh status` 完整输出 | 成员、RTT、路由路径、配额状态 |
| **阶段 1 转发挂载到组网** | `e2e.Session.Streams()` 暴露为 `*mux.Session`；`forward.Manager` 的 `Provider` 在组网模式下返回它，于是 `-L`/`-R` 按设备名工作（`cmesh forward -L 8080:nas:5432`），**阶段 1 的 forward 代码零改动**——它只认 `Mux` 接口，不关心底下是直连 yamux 还是 e2e 内的 yamux |
| README / MESH.md 更新 | 组网上手指南 |
| e2e 脚本扩展 | M1–M7 全部进 `tests/run-tests.sh --mesh` |

**验收**：M1–M7 全绿；阶段 1 的 35 项 e2e 仍然全绿。

---

## 9. 主要风险

| 编号 | 风险 | 严重度 | 缓解 |
|---|---|---|---|
| X1 | 距离向量在拓扑抖动时路由震荡 | 高 | 水平分割 + 抑制计时器（路由毒化后暂不更新） |
| X2 | 打洞在对称 NAT × 对称 NAT 下必然失败 | 高 | 这正是中转路径存在的理由（N-4）；测试 M2 覆盖 |
| X3 | e2e 层与记录泵的层叠顺序出错（重蹈阶段 1 覆辙） | 高 | PR-M4 单独提交；`WithE2E` 关闭时逐字节等价测试 |
| X4 | 发现节点单点 | 中 | 多发现节点 + 无发现节点的文件导入路径（§6.2） |
| X5 | 撤销广播丢失（分区时） | 中 | MemberList 带版本号，重连时比较并同步 |
| X6 | 恶意成员谎报 RTT 抢中转流量 | 低 | 实测 RTT 自己算，不信广播值 |
| X7 | 旧 relay 不支持 `/mesh/*` | 低 | 组网客户端探测失败即跳过该节点（caps 协商） |
| X8 | e2e 内再跑一层 yamux 的双重复用开销超出预算 | 中 | PR-M4 验收含 MP-2/MP-3；不达标则改为 `FrameMeshData` 帧头显式 StreamID+滑动窗口（实现更少复用、更多新代码，约 +200 行） |

---

## 10. 对 DESIGN.md 的修正清单

本文细化过程中发现并修正的（按 T-0.2，先改这里，DESIGN.md 随后同步）：

| # | DESIGN.md 原文 | 修正 | 理由 |
|---|---|---|---|
| 1 | 发现节点「拿不到明文：成员信息加密」 | **发现节点以明文持有成员列表（内存中），但它必须是成员** | 它要验证签名、推送列表，必须能读；换来的约束是「发现节点=成员」，不接受就别部署 |
| 2 | 预估 1,900 行 | **2,550 行（含 CLI）** | DESIGN.md 没算 CLI 的 350 行 |
| 3 | 「两层加密」未指明原语 | **SIGMA 模式（签名认证的 X25519）**，不是 SPAKE2 | SPAKE2 前提是共享口令；组网共享的是公钥 |
